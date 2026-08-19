package service

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io/fs"
	"sort"
	"sync"
	"time"

	"github.com/benemon/shackleton/internal/agent"
	"github.com/benemon/shackleton/internal/config"
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
	RequestedAt     time.Time `json:"requested_at"`
}

type ConfigView struct {
	Listen     string           `json:"listen"`
	StateDir   string           `json:"state_dir"`
	EnvFiles   []string         `json:"env_files"`
	Model      ModelView        `json:"model"`
	MCPServers []MCPServerView  `json:"mcp_servers"`
	Prometheus PrometheusView   `json:"prometheus"`
	GatedTools []string         `json:"gated_tools"`
	Telegram   TelegramView     `json:"telegram"`
	Agent      AgentView        `json:"agent"`
	APIToken   config.SecretRef `json:"api_token"`
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

type PrometheusView struct {
	URL        string           `json:"url"`
	AuthHeader config.SecretRef `json:"auth_header"`
}

type TelegramView struct {
	EnvFile string `json:"env_file"`
}

type AgentView struct {
	MaxRounds            int    `json:"max_rounds"`
	CallTimeout          string `json:"call_timeout"`
	InvestigationTimeout string `json:"investigation_timeout"`
}

type HealthStatus struct {
	Status string `json:"status"`
}

type pendingApproval struct {
	PendingApproval
	decision chan bool
}

type Service struct {
	ctx       context.Context
	store     *store.Store
	config    *config.Config
	newRunner RunnerFactory
	wg        sync.WaitGroup

	mu      sync.Mutex
	pending map[string]*pendingApproval
	decided map[string]struct{}
}

func New(ctx context.Context, audit *store.Store, cfg *config.Config, newRunner RunnerFactory) *Service {
	return &Service{
		ctx: ctx, store: audit, config: cfg, newRunner: newRunner,
		pending: make(map[string]*pendingApproval), decided: make(map[string]struct{}),
	}
}

func (s *Service) CreateInvestigation(_ context.Context, question, trigger string) (store.Summary, error) {
	investigation, err := s.store.Begin(question, trigger)
	if err != nil {
		return store.Summary{}, err
	}
	summary := s.summary(investigation.ID)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		sink := &investigationSink{investigation: investigation}
		runner := s.newRunner(sink, &investigationApprover{service: s, investigationID: investigation.ID})
		metrics, runErr := runner.Run(s.ctx, question, "")
		if eventErr := sink.Err(); eventErr != nil {
			runErr = eventErr
		}
		if runErr != nil {
			_ = investigation.Append(store.EventFailed, store.FailedPayload{Reason: runErr.Error(), Metrics: metrics})
		} else {
			_ = investigation.Append(store.EventCompleted, store.CompletedPayload{Answer: metrics.Answer, Metrics: metrics})
		}
		_ = investigation.Close()
	}()
	return summary, nil
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

func (s *Service) DecideApproval(id string, approved bool) error {
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
	s.mu.Unlock()
	pending.decision <- approved
	return nil
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
	return ConfigView{
		Listen: cfg.Listen, StateDir: cfg.StateDir, EnvFiles: append([]string{}, cfg.EnvFiles...),
		Model:      ModelView{BaseURL: cfg.Model.BaseURL, Name: cfg.Model.Name, APIKey: cfg.Model.APIKey.Ref()},
		MCPServers: servers,
		Prometheus: PrometheusView{URL: cfg.Prometheus.URL, AuthHeader: cfg.Prometheus.AuthHeader.Ref()},
		GatedTools: append([]string{}, cfg.GatedTools...), Telegram: TelegramView{EnvFile: cfg.Telegram.EnvFile},
		Agent:    AgentView{MaxRounds: cfg.Agent.MaxRounds, CallTimeout: cfg.Agent.CallTimeout.Duration().String(), InvestigationTimeout: cfg.Agent.InvestigationTimeout.Duration().String()},
		APIToken: cfg.APIToken.Ref(),
	}
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

func (a *investigationApprover) RequestApproval(ctx context.Context, call agent.ToolCall) (bool, error) {
	pending, err := a.service.addPending(a.investigationID, call)
	if err != nil {
		return false, err
	}
	select {
	case approved := <-pending.decision:
		return approved, nil
	case <-ctx.Done():
		if a.service.removePending(pending.ID, pending) {
			return false, ctx.Err()
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
			Human: call.Human, RequestedAt: time.Now().UTC(),
		}, decision: make(chan bool, 1)}
		s.mu.Lock()
		_, wasDecided := s.decided[id]
		if s.pending[id] == nil && !wasDecided {
			s.pending[id] = pending
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
	return true
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
