package service

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/benemon/shackleton/internal/agent"
	"github.com/benemon/shackleton/internal/config"
	"github.com/benemon/shackleton/internal/inventory"
	"github.com/benemon/shackleton/internal/kb"
	"github.com/benemon/shackleton/internal/store"
)

var (
	ErrApprovalNotFound       = errors.New("approval not found")
	ErrApprovalAlreadyDecided = errors.New("approval already decided")
)

type RunnerFactory func(events agent.EventSink, approver agent.Approver) *agent.Runner

type PendingApproval struct {
	ID              string    `json:"id"`
	InvestigationID string    `json:"investigation_id"`
	CallID          string    `json:"call_id"`
	Name            string    `json:"name"`
	Human           string    `json:"human"`
	ArgsJSON        string    `json:"args_json"`
	RequestedAt     time.Time `json:"requested_at"`
}

type ApprovalEvent struct {
	Type     string          `json:"type"`
	Approval PendingApproval `json:"approval"`
	Approved bool            `json:"approved"`
	Via      string          `json:"via"`
}

type ConfigView struct {
	Listen         string           `json:"listen"`
	StateDir       string           `json:"state_dir"`
	KBDir          string           `json:"kb_dir"`
	InventoryDir   string           `json:"inventory_dir"`
	EnvFiles       []string         `json:"env_files"`
	Model          ModelView        `json:"model"`
	MCPServers     []MCPServerView  `json:"mcp_servers"`
	MetricsSources []SourceView     `json:"metrics_sources"`
	LogsSources    []SourceView     `json:"logs_sources"`
	GatedTools     []string         `json:"gated_tools"`
	Notifications  []ChannelView    `json:"notifications"`
	Approvals      []ChannelView    `json:"approvals"`
	Agent          AgentView        `json:"agent"`
	Sweeps         []SweepView      `json:"sweeps"`
	APIToken       config.SecretRef `json:"api_token"`
	TLS            TLSView          `json:"tls"`
}

type TLSView struct {
	CertFile string `json:"cert_file"`
	KeyFile  string `json:"key_file"`
}

type SweepView struct {
	Name     string `json:"name"`
	Schedule string `json:"schedule"`
	Question string `json:"question"`
}

type ModelView struct {
	BaseURL string           `json:"base_url"`
	Name    string           `json:"name"`
	APIKey  config.SecretRef `json:"api_key"`
}

type MCPServerView struct {
	Name       string            `json:"name"`
	URL        string            `json:"url"`
	AuthHeader *config.SecretRef `json:"auth_header,omitempty"`
}

type SourceView struct {
	Name       string            `json:"name"`
	Type       string            `json:"type"`
	URL        string            `json:"url"`
	AuthHeader *config.SecretRef `json:"auth_header,omitempty"`
}

type ChannelView struct {
	Name     string           `json:"name"`
	Type     string           `json:"type"`
	BotToken config.SecretRef `json:"bot_token"`
	ChatID   config.SecretRef `json:"chat_id"`
}

type AgentView struct {
	Prompt               string `json:"prompt"`
	MaxRounds            int    `json:"max_rounds"`
	MaxToolResultChars   int    `json:"max_tool_result_chars"`
	CallTimeout          string `json:"call_timeout"`
	InvestigationTimeout string `json:"investigation_timeout"`
}

type HealthStatus struct {
	Status string `json:"status"`
}

type pendingApproval struct {
	PendingApproval
	decision chan agent.Decision
}

type Service struct {
	ctx       context.Context
	store     *store.Store
	config    *config.Config
	newRunner RunnerFactory
	// Notifier, when set, receives outcomes of alert- and API-triggered
	// investigations that need eyes (attention/action/failed). Sweeps notify
	// through their own follower; Q&A answers return on their own channel.
	Notifier agent.Notifier
	// KB, when set, receives resolution records: symptomatic triggers
	// (alerts, sweeps) on a non-healthy verdict or approved action; ad-hoc
	// questions only when an approved action ran.
	KB *kb.Store
	// Inventory, when set, backs the /v1/inventory projection and the
	// environment facts recorded into KB articles.
	Inventory *inventory.Inventory
	wg        sync.WaitGroup

	mu           sync.Mutex
	pending      map[string]*pendingApproval
	decided      map[string]struct{}
	decidedOrder []string
	subscribers  map[chan ApprovalEvent]struct{}
}

// decidedCap bounds the tombstones that distinguish an already-decided
// approval (409) from an unknown one (404); evicted ids report unknown.
const decidedCap = 1024

func New(ctx context.Context, audit *store.Store, cfg *config.Config, newRunner RunnerFactory) *Service {
	return &Service{
		ctx: ctx, store: audit, config: cfg, newRunner: newRunner,
		pending: make(map[string]*pendingApproval), decided: make(map[string]struct{}),
		subscribers: make(map[chan ApprovalEvent]struct{}),
	}
}

func (s *Service) CreateInvestigation(_ context.Context, question, trigger string) (store.Summary, error) {
	investigation, err := s.store.Begin(question, trigger)
	if err != nil {
		return store.Summary{}, err
	}
	summary := s.summary(investigation.ID)
	s.wg.Go(func() {
		sink := &investigationSink{investigation: investigation}
		runner := s.newRunner(sink, &investigationApprover{service: s, investigationID: investigation.ID})
		metrics, runErr := runner.Run(s.ctx, question, "")
		if eventErr := sink.Err(); eventErr != nil {
			runErr = eventErr
		}
		status := "completed"
		verdict := store.ParseVerdict(metrics.Answer)
		if runErr != nil {
			status = "failed"
			_ = investigation.Append(store.EventFailed, store.FailedPayload{Reason: runErr.Error(), Metrics: metrics})
		} else {
			_ = investigation.Append(store.EventCompleted, store.CompletedPayload{Answer: metrics.Answer, Verdict: verdict, Metrics: metrics})
		}
		_ = investigation.Close()
		s.notifyOutcome(investigation.ID, question, trigger, verdict, runErr)
		s.recordResolution(investigation.ID, question, trigger, verdict, metrics.Answer, runErr)
		class := triggerClass(trigger)
		investigationsTotal.WithLabelValues(class, status).Inc()
		investigationSeconds.WithLabelValues(class).Observe(metrics.Duration.Seconds())
		toolCallsTotal.Add(float64(metrics.ToolCallsTotal))
		toolCallErrors.WithLabelValues("malformed_json").Add(float64(metrics.MalformedJSON))
		toolCallErrors.WithLabelValues("schema_invalid").Add(float64(metrics.SchemaInvalid))
		toolCallErrors.WithLabelValues("unknown_tool").Add(float64(metrics.UnknownTool))
		toolCallErrors.WithLabelValues("tool_error").Add(float64(metrics.ToolErrors))
		toolCallsRecovered.Add(float64(metrics.Recovered))
	})
	return summary, nil
}

func (s *Service) notifyOutcome(id, question, trigger string, verdict *store.Verdict, runErr error) {
	if s.Notifier == nil || (!strings.HasPrefix(trigger, "alert:") && trigger != "api") {
		return
	}
	headline, _, _ := strings.Cut(question, "\n")
	var b strings.Builder
	b.WriteString(headline + "\n")
	switch {
	case runErr != nil:
		b.WriteString("Investigation failed: " + runErr.Error())
	case verdict == nil:
		b.WriteString("ATTENTION: completed without a structured verdict")
	case verdict.Verdict == "healthy":
		return
	default:
		b.WriteString(strings.ToUpper(verdict.Verdict) + ": " + verdict.Summary)
		for _, item := range verdict.Evidence {
			b.WriteString("\n- " + item)
		}
	}
	b.WriteString("\n(" + id + ")")
	if err := s.Notifier.Send(s.ctx, b.String()); err != nil {
		log.Printf("notify outcome of %s: %v", id, err)
	}
}

func (s *Service) recordResolution(id, question, trigger string, verdict *store.Verdict, answer string, runErr error) {
	if s.KB == nil || runErr != nil {
		return
	}
	events, err := s.store.Get(id)
	if err != nil {
		log.Printf("kb: read %s: %v", id, err)
		return
	}
	actions := approvedActions(events)
	// The KB records resolutions, never current state. Alerts and sweeps
	// carry stable symptom identities, so a non-healthy verdict is a
	// recurrence worth remembering; an ad-hoc question earns an article only
	// when an approved remediation actually ran.
	symptomatic := strings.HasPrefix(trigger, "alert:") || strings.HasPrefix(trigger, "sweep:")
	if len(actions) == 0 && (!symptomatic || verdict == nil || verdict.Verdict == "healthy") {
		return
	}
	article := buildArticle(id, question, trigger, verdict, answer, s.environmentText(), events, actions)
	front, err := s.KB.Record(article)
	if err != nil {
		log.Printf("kb: record %s: %v", article.Slug, err)
		return
	}
	// Nomination, never promotion: three verified resolutions earn the draft a
	// review request; the status transition stays a human edit.
	if front.Status == "draft" && !front.Nominated && front.ClearedCount() >= 3 && s.Notifier != nil {
		if err := s.KB.MarkNominated(front.Slug); err != nil {
			log.Printf("kb: nominate %s: %v", front.Slug, err)
			return
		}
		text := fmt.Sprintf("KB nomination: draft article %s has a verified resolution for this symptom %d times. Review it and set status: approved to let it guide future investigations.", front.Slug, front.ClearedCount())
		if err := s.Notifier.Send(s.ctx, text); err != nil {
			log.Printf("kb: nominate %s: %v", front.Slug, err)
		}
	}
}

func (s *Service) environmentText() string {
	var parts []string
	if s.config != nil && s.config.Agent.Prompt != "" {
		parts = append(parts, s.config.Agent.Prompt)
	}
	if s.Inventory != nil {
		if env := s.Inventory.Environment(); env != "" {
			parts = append(parts, env)
		}
	}
	return strings.Join(parts, "\n\n")
}

func approvedActions(events []store.Event) []kb.Action {
	type request struct{ name, human string }
	requests := make(map[string]request)
	approvedByName := make(map[string][]string)
	var actions []kb.Action
	for _, event := range events {
		switch event.Type {
		case store.EventApprovalRequested:
			var payload struct {
				CallID string `json:"call_id"`
				Name   string `json:"name"`
				Human  string `json:"human"`
			}
			if json.Unmarshal(event.Payload, &payload) == nil {
				requests[payload.CallID] = request{payload.Name, payload.Human}
			}
		case store.EventApprovalDecided:
			var payload struct {
				CallID   string `json:"call_id"`
				Approved bool   `json:"approved"`
			}
			if json.Unmarshal(event.Payload, &payload) == nil && payload.Approved {
				if req, ok := requests[payload.CallID]; ok {
					approvedByName[req.name] = append(approvedByName[req.name], req.human)
				}
			}
		case store.EventToolCall:
			var payload store.ToolCallPayload
			if json.Unmarshal(event.Payload, &payload) != nil {
				continue
			}
			// The first tool call for a name after its approved decision is
			// the execution; correlate by arrival order.
			if queue := approvedByName[payload.Name]; len(queue) > 0 {
				actions = append(actions, kb.Action{Human: queue[0], Outcome: truncate(payload.ResultSnippet, 200)})
				approvedByName[payload.Name] = queue[1:]
			}
		}
	}
	return actions
}

func buildArticle(id, question, trigger string, verdict *store.Verdict, answer, environment string, events []store.Event, actions []kb.Action) kb.Article {
	slug, title, symptom := symptomIdentity(id, question, trigger)
	var b strings.Builder
	b.WriteString("# " + title + "\n\n## Environment\n" + orNone(environment, "(no operator preamble configured)"))
	b.WriteString("\n\n## Issue\nTrigger: " + trigger + "\n\n" + question)
	b.WriteString("\n\n## Diagnosis\nInvestigation " + id + ":\n")
	listed := 0
	for _, event := range events {
		if event.Type != store.EventToolCall || listed >= 30 {
			continue
		}
		var payload store.ToolCallPayload
		if json.Unmarshal(event.Payload, &payload) != nil {
			continue
		}
		status := "ok"
		if payload.Error {
			status = "error"
		}
		args, _ := json.Marshal(payload.Args)
		b.WriteString("- " + payload.Name + " " + truncate(string(args), 120) + " → " + status + "\n")
		listed++
	}
	b.WriteString("\n## Root cause\n" + strings.TrimSpace(stripVerdictBlock(answer)))
	b.WriteString("\n\n## Resolution\n")
	if len(actions) == 0 {
		b.WriteString("No remediation applied.\n")
	}
	for _, action := range actions {
		b.WriteString("- Approved: " + action.Human + " → " + action.Outcome + "\n")
	}
	verified := ""
	if len(actions) > 0 && verdict != nil && verdict.Resolution != "" {
		verified = verdict.Resolution
	}
	front := kb.FrontMatter{Slug: slug, Title: title, Symptom: symptom,
		Occurrences: []kb.Occurrence{{Investigation: id, At: time.Now().UTC(), Verified: verified}},
		Resolution:  kb.Resolution{Actions: actions, Verified: orNone(verified, "none")}}
	if verdict != nil {
		front.Verdict = verdict.Verdict
		b.WriteString("\n## Verdict\n" + verdict.Verdict + ": " + verdict.Summary + "\n")
		for _, item := range verdict.Evidence {
			b.WriteString("- " + item + "\n")
		}
	}
	return kb.Article{FrontMatter: front, Body: b.String()}
}

func symptomIdentity(id, question, trigger string) (string, string, kb.Symptom) {
	switch {
	case strings.HasPrefix(trigger, "alert:"):
		fingerprint := strings.TrimPrefix(trigger, "alert:")
		alertname := "unknown-alert"
		if rest, ok := strings.CutPrefix(question, "Alertmanager alert firing: "); ok {
			if name, _, ok := strings.Cut(rest, "."); ok {
				alertname = name
			}
		}
		return "alert-" + slugify(alertname), alertname + " (alert)",
			kb.Symptom{Trigger: "alert", Alertname: alertname, Fingerprints: []string{fingerprint}}
	case strings.HasPrefix(trigger, "sweep:"):
		name := strings.TrimPrefix(trigger, "sweep:")
		return "sweep-" + slugify(name), "Sweep " + name, kb.Symptom{Trigger: "sweep", Sweep: name}
	default:
		headline, _, _ := strings.Cut(question, "\n")
		return "adhoc-" + slugify(id), truncate(headline, 80), kb.Symptom{Trigger: trigger}
	}
}

func slugify(value string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(value) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

func stripVerdictBlock(answer string) string {
	start := strings.LastIndex(answer, "```json")
	if start < 0 || store.ParseVerdict(answer) == nil {
		return answer
	}
	return answer[:start]
}

func truncate(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit]) + "…"
}

func orNone(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

type Alert struct {
	Status      string            `json:"status"`
	Fingerprint string            `json:"fingerprint"`
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
}

// IngestAlerts creates one investigation per firing alert, skipping alerts
// whose fingerprint already has a running investigation and alerts that are
// not firing (resolved alerts are Alertmanager bookkeeping, not new work).
func (s *Service) IngestAlerts(ctx context.Context, alerts []Alert) (created, skipped int, err error) {
	running := make(map[string]bool)
	for _, summary := range s.store.List() {
		if summary.Status == "running" {
			running[summary.Trigger] = true
		}
	}
	for _, alert := range alerts {
		trigger := "alert:" + alert.Fingerprint
		if alert.Status != "firing" || alert.Fingerprint == "" || running[trigger] {
			skipped++
			continue
		}
		if _, err := s.CreateInvestigation(ctx, triageQuestion(alert)+s.recurrenceContext(alert), trigger); err != nil {
			return created, skipped, err
		}
		running[trigger] = true
		created++
	}
	return created, skipped, nil
}

// recurrenceContext surfaces the symptom's history as one mention-only line:
// prior investigations of the same alertname and any knowledge-base article.
// It asserts nothing — the model is told to verify current state itself.
func (s *Service) recurrenceContext(alert Alert) string {
	name := alert.Labels["alertname"]
	if name == "" {
		return ""
	}
	headline := "Alertmanager alert firing: " + name + "."
	count := 0
	var last store.Summary
	for _, summary := range s.store.List() {
		if summary.Status == "running" || !strings.HasPrefix(summary.Trigger, "alert:") || !strings.HasPrefix(summary.Question, headline) {
			continue
		}
		count++
		if summary.StartedAt.After(last.StartedAt) {
			last = summary
		}
	}
	if count == 0 {
		return ""
	}
	var line strings.Builder
	fmt.Fprintf(&line, "\nPrior history: this alert has been investigated %d time(s); most recently %s", count, last.StartedAt.UTC().Format("2006-01-02 15:04"))
	if events, err := s.store.Get(last.ID); err == nil {
		for _, event := range events {
			if event.Type != store.EventCompleted {
				continue
			}
			var payload store.CompletedPayload
			if json.Unmarshal(event.Payload, &payload) == nil && payload.Verdict != nil {
				fmt.Fprintf(&line, " with verdict %s: %s", payload.Verdict.Verdict, payload.Verdict.Summary)
			}
		}
	}
	line.WriteString(".")
	// Only operator-approved articles feed resolution context; drafts are
	// machine prose no human has vouched for.
	if s.KB != nil {
		if articles, err := s.KB.List(); err == nil {
			for _, article := range articles {
				if article.Symptom.Alertname == name && article.Status == "approved" {
					fmt.Fprintf(&line, " An approved knowledge-base article exists for this symptom (%s).", article.Slug)
					break
				}
			}
		}
	}
	return line.String() + " Treat this as context only; verify the current state independently."
}

func triageQuestion(alert Alert) string {
	var b strings.Builder
	b.WriteString("Alertmanager alert firing")
	if name := alert.Labels["alertname"]; name != "" {
		b.WriteString(": " + name)
	}
	b.WriteString(".")
	for _, section := range []struct {
		title  string
		values map[string]string
	}{{"Labels", alert.Labels}, {"Annotations", alert.Annotations}} {
		if len(section.values) == 0 {
			continue
		}
		keys := make([]string, 0, len(section.values))
		for key := range section.values {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		b.WriteString("\n" + section.title + ":")
		for _, key := range keys {
			b.WriteString("\n  " + key + ": " + section.values[key])
		}
	}
	b.WriteString("\nInvestigate the cause. If a remediation is warranted, propose it through the gated tools; otherwise report findings.")
	return b.String()
}

func (s *Service) GetInvestigation(id string) (store.Summary, []store.Event, error) {
	events, err := s.store.Get(id)
	if err != nil {
		return store.Summary{}, nil, err
	}
	summary := s.summary(id)
	if summary.ID == "" {
		return store.Summary{}, nil, fs.ErrNotExist
	}
	return summary, events, nil
}

func (s *Service) ListInvestigations() []store.Summary {
	return s.store.List()
}

func (s *Service) FollowInvestigation(id string) ([]store.Event, <-chan store.Event, func(), error) {
	return s.store.Follow(id)
}

func (s *Service) ListPendingApprovals() []PendingApproval {
	s.mu.Lock()
	result := make([]PendingApproval, 0, len(s.pending))
	for _, pending := range s.pending {
		result = append(result, pending.PendingApproval)
	}
	s.mu.Unlock()
	sort.Slice(result, func(i, j int) bool {
		if result[i].RequestedAt.Equal(result[j].RequestedAt) {
			return result[i].ID < result[j].ID
		}
		return result[i].RequestedAt.Before(result[j].RequestedAt)
	})
	return result
}

func (s *Service) SubscribeApprovals() (<-chan ApprovalEvent, func()) {
	s.mu.Lock()
	ch := make(chan ApprovalEvent, 32)
	s.subscribers[ch] = struct{}{}
	s.mu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			s.mu.Lock()
			delete(s.subscribers, ch)
			close(ch)
			s.mu.Unlock()
		})
	}
}

func (s *Service) DecideApproval(id string, approved bool, via string) error {
	s.mu.Lock()
	pending := s.pending[id]
	if pending == nil {
		_, decided := s.decided[id]
		s.mu.Unlock()
		if decided {
			return ErrApprovalAlreadyDecided
		}
		return ErrApprovalNotFound
	}
	delete(s.pending, id)
	s.decided[id] = struct{}{}
	s.decidedOrder = append(s.decidedOrder, id)
	if len(s.decidedOrder) > decidedCap {
		delete(s.decided, s.decidedOrder[0])
		s.decidedOrder = s.decidedOrder[1:]
	}
	decision := agent.Decision{Approved: approved, Via: via}
	s.publishApprovalLocked(ApprovalEvent{Type: "settled", Approval: pending.PendingApproval, Approved: approved, Via: via})
	s.mu.Unlock()
	approvalDecisions.WithLabelValues(via, strconv.FormatBool(approved)).Inc()
	pending.decision <- decision
	return nil
}

type AuditEntry struct {
	InvestigationID string          `json:"investigation_id"`
	TS              time.Time       `json:"ts"`
	Type            string          `json:"type"`
	Payload         json.RawMessage `json:"payload"`
}

// AuditTrail projects the mutating operations across all investigations,
// newest first, from the store's append-only records.
func (s *Service) AuditTrail() ([]AuditEntry, error) {
	entries := []AuditEntry{}
	for _, summary := range s.store.List() {
		events, err := s.store.Get(summary.ID)
		if err != nil {
			return nil, err
		}
		for _, event := range events {
			switch event.Type {
			case store.EventCreated, store.EventApprovalRequested, store.EventApprovalDecided:
				entries = append(entries, AuditEntry{InvestigationID: summary.ID, TS: event.TS, Type: event.Type, Payload: event.Payload})
			}
		}
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].TS.Equal(entries[j].TS) {
			return entries[i].InvestigationID > entries[j].InvestigationID
		}
		return entries[i].TS.After(entries[j].TS)
	})
	return entries, nil
}

func (s *Service) ConfigView() ConfigView {
	cfg := s.config
	servers := make([]MCPServerView, 0, len(cfg.MCPServers))
	for _, server := range cfg.MCPServers {
		view := MCPServerView{Name: server.Name, URL: server.URL}
		if server.AuthHeader.IsSet() {
			ref := server.AuthHeader.Ref()
			view.AuthHeader = &ref
		}
		servers = append(servers, view)
	}
	sweeps := make([]SweepView, 0, len(cfg.Sweeps))
	for _, sweep := range cfg.Sweeps {
		sweeps = append(sweeps, SweepView{Name: sweep.Name, Schedule: sweep.Schedule, Question: sweep.Question})
	}
	sourceView := func(name, sourceType, url string, auth config.Secret) SourceView {
		view := SourceView{Name: name, Type: sourceType, URL: url}
		if auth.IsSet() {
			ref := auth.Ref()
			view.AuthHeader = &ref
		}
		return view
	}
	metrics := make([]SourceView, 0, len(cfg.MetricsSources))
	for _, source := range cfg.MetricsSources {
		metrics = append(metrics, sourceView(source.Name, source.Type, source.URL, source.AuthHeader))
	}
	logs := make([]SourceView, 0, len(cfg.LogsSources))
	for _, source := range cfg.LogsSources {
		logs = append(logs, sourceView(source.Name, source.Type, source.URL, source.AuthHeader))
	}
	channelViews := func(channels []config.Channel) []ChannelView {
		views := make([]ChannelView, 0, len(channels))
		for _, channel := range channels {
			views = append(views, ChannelView{
				Name: channel.Name, Type: channel.Type,
				BotToken: channel.BotToken.Ref(), ChatID: channel.ChatID.Ref(),
			})
		}
		return views
	}
	return ConfigView{
		Listen: cfg.Listen, StateDir: cfg.StateDir, KBDir: cfg.KBDir, InventoryDir: cfg.InventoryDir, EnvFiles: append([]string{}, cfg.EnvFiles...), Sweeps: sweeps,
		Model:          ModelView{BaseURL: cfg.Model.BaseURL, Name: cfg.Model.Name, APIKey: cfg.Model.APIKey.Ref()},
		MCPServers:     servers,
		MetricsSources: metrics, LogsSources: logs,
		Notifications: channelViews(cfg.Notifications), Approvals: channelViews(cfg.Approvals),
		GatedTools: append([]string{}, cfg.GatedTools...),
		Agent: AgentView{Prompt: cfg.Agent.Prompt, MaxRounds: cfg.Agent.MaxRounds, MaxToolResultChars: cfg.Agent.MaxToolResultChars,
			CallTimeout: cfg.Agent.CallTimeout.Duration().String(), InvestigationTimeout: cfg.Agent.InvestigationTimeout.Duration().String()},
		APIToken: cfg.APIToken.Ref(),
		TLS:      TLSView{CertFile: cfg.TLS.CertFile, KeyFile: cfg.TLS.KeyFile},
	}
}

// InventoryView reads the inventory directory fresh so operator edits and
// discovery drafts appear without a restart; the startup snapshot behind
// prompt assembly and gating (s.Inventory) is unaffected.
func (s *Service) InventoryView() (*inventory.Inventory, error) {
	if s.config == nil || s.config.InventoryDir == "" {
		return &inventory.Inventory{Hosts: []inventory.Host{}, Clusters: []inventory.Cluster{}}, nil
	}
	return inventory.Load(s.config.InventoryDir)
}

func (s *Service) KBList() ([]kb.FrontMatter, error) {
	if s.KB == nil {
		return []kb.FrontMatter{}, nil
	}
	return s.KB.List()
}

func (s *Service) KBGet(slug string) ([]byte, error) {
	if s.KB == nil {
		return nil, fs.ErrNotExist
	}
	return s.KB.Get(slug)
}

func (s *Service) Health() HealthStatus {
	return HealthStatus{Status: "ok"}
}

func (s *Service) Wait() {
	s.wg.Wait()
}

func (s *Service) summary(id string) store.Summary {
	for _, summary := range s.store.List() {
		if summary.ID == id {
			return summary
		}
	}
	return store.Summary{}
}

type investigationApprover struct {
	service         *Service
	investigationID string
}

func (a *investigationApprover) RequestApproval(ctx context.Context, call agent.ToolCall) (agent.Decision, error) {
	pending, err := a.service.addPending(a.investigationID, call)
	if err != nil {
		return agent.Decision{}, err
	}
	select {
	case decision := <-pending.decision:
		return decision, nil
	case <-ctx.Done():
		if a.service.removePending(pending.ID, pending) {
			return agent.Decision{}, ctx.Err()
		}
		return <-pending.decision, nil
	}
}

func (s *Service) addPending(investigationID string, call agent.ToolCall) (*pendingApproval, error) {
	for range 4 {
		id, err := newApprovalID()
		if err != nil {
			return nil, err
		}
		pending := &pendingApproval{PendingApproval: PendingApproval{
			ID: id, InvestigationID: investigationID, CallID: call.ID, Name: call.Name,
			Human: call.Human, ArgsJSON: call.ArgsJSON, RequestedAt: time.Now().UTC(),
		}, decision: make(chan agent.Decision, 1)}
		s.mu.Lock()
		_, wasDecided := s.decided[id]
		if s.pending[id] == nil && !wasDecided {
			s.pending[id] = pending
			s.publishApprovalLocked(ApprovalEvent{Type: "requested", Approval: pending.PendingApproval})
			s.mu.Unlock()
			return pending, nil
		}
		s.mu.Unlock()
	}
	return nil, errors.New("could not allocate approval id")
}

func (s *Service) removePending(id string, want *pendingApproval) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pending[id] != want {
		return false
	}
	delete(s.pending, id)
	s.publishApprovalLocked(ApprovalEvent{Type: "settled", Approval: want.PendingApproval, Approved: false, Via: "timeout"})
	approvalDecisions.WithLabelValues("timeout", "false").Inc()
	return true
}

func (s *Service) publishApprovalLocked(event ApprovalEvent) {
	for ch := range s.subscribers {
		select {
		case ch <- event:
		default:
			select {
			case <-ch:
			default:
			}
			select {
			case ch <- event:
			default:
			}
		}
	}
}

type investigationSink struct {
	investigation *store.Investigation
	mu            sync.Mutex
	err           error
}

func (s *investigationSink) Emit(eventType string, payload any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err == nil {
		s.err = s.investigation.Append(eventType, payload)
	}
}

func (s *investigationSink) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

func newApprovalID() (string, error) {
	random := make([]byte, 12)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(random), nil
}
