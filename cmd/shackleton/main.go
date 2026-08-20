// Command shackleton is a self-hosted AI ops investigation daemon; the
// agent/bench subcommands are its development harness. Deployments run it on
// the host that can reach the MCP servers; credentials never leave that host.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/benemon/shackleton/internal/agent"
	"github.com/benemon/shackleton/internal/config"
	"github.com/benemon/shackleton/internal/service"
	"github.com/benemon/shackleton/internal/store"
	"github.com/benemon/shackleton/internal/sweep"
	"github.com/benemon/shackleton/internal/telegram"
	"github.com/benemon/shackleton/ui"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

// version is stamped by the Makefile via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: shackleton <agent|bench|serve|version>")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "agent":
		err = runAgent(context.Background(), os.Args[2:])
	case "bench":
		err = runBench(context.Background(), os.Args[2:])
	case "serve":
		err = runServe(context.Background(), os.Args[2:])
	case "version":
		fmt.Println(version)
	default:
		err = fmt.Errorf("unknown subcommand %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: %v\n", err)
		os.Exit(1)
	}
}

// headerRoundTripper injects headers into every request — the auth mechanism
// for gated MCP servers and the metrics endpoint. Values are fetched per
// request because file-backed secrets rotate under a running daemon.
type headerRoundTripper struct {
	base    http.RoundTripper
	headers map[string]func() string
}

func (h *headerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	for k, v := range h.headers {
		req.Header.Set(k, v())
	}
	return h.base.RoundTrip(req)
}

func runAgent(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("agent", flag.ContinueOnError)
	configPath := flags.String("config", "", "configuration YAML file")
	approverName := flags.String("approver", "cli-deny", "cli-deny, cli-approve, or telegram")
	verbose := flags.Bool("v", false, "trace each tool call and result to stderr")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *configPath == "" || flags.NArg() != 1 {
		return fmt.Errorf("usage: shackleton agent -config=<path> [-approver=cli-deny|cli-approve|telegram] [-v] \"<question>\"")
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	runner, closeSessions, err := newRunner(ctx, *approverName, cfg)
	if err != nil {
		return err
	}
	defer closeSessions()
	audit, err := store.Open(cfg.StateDir)
	if err != nil {
		return err
	}
	investigation, err := audit.Begin(flags.Arg(0), "cli")
	if err != nil {
		return err
	}
	sink := &investigationSink{investigation: investigation, verbose: *verbose}
	runner.Events = sink
	metrics, runErr := runner.Run(ctx, flags.Arg(0), "")
	if eventErr := sink.Err(); eventErr != nil {
		runErr = eventErr
	}
	if runErr != nil {
		err = investigation.Append(store.EventFailed, store.FailedPayload{Reason: runErr.Error(), Metrics: metrics})
	} else {
		err = investigation.Append(store.EventCompleted, store.CompletedPayload{Answer: metrics.Answer, Metrics: metrics})
	}
	if closeErr := investigation.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if metrics.Answer != "" {
		fmt.Println(metrics.Answer)
	}
	encoded, _ := json.Marshal(metrics)
	fmt.Fprintln(os.Stderr, string(encoded))
	return runErr
}

func runServe(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	configPath := flags.String("config", "", "configuration YAML file")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *configPath == "" || flags.NArg() != 0 {
		return fmt.Errorf("usage: shackleton serve -config=<path>")
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	if err := validateServeConfig(cfg); err != nil {
		return err
	}

	signalCtx, stopSignals := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	investigationCtx, cancelInvestigations := context.WithCancel(ctx)
	factory, closeSessions, err := newRunnerFactory(investigationCtx, cfg)
	if err != nil {
		cancelInvestigations()
		return err
	}
	audit, err := store.Open(cfg.StateDir)
	if err != nil {
		cancelInvestigations()
		closeSessions()
		return err
	}
	core := service.New(investigationCtx, audit, cfg, factory)
	var notifier agent.Notifier
	telegramToken, telegramChatID := os.Getenv("TG_BOT_TOKEN"), os.Getenv("TG_CHAT_ID")
	if telegramToken != "" && telegramChatID != "" {
		sender, err := telegram.NewNotifier(telegramToken, telegramChatID)
		if err != nil {
			cancelInvestigations()
			closeSessions()
			return err
		}
		notifier = sender
		if _, err := telegram.NewTrigger(investigationCtx, telegramToken, telegramChatID, core); err != nil {
			cancelInvestigations()
			closeSessions()
			return err
		}
	} else {
		fmt.Fprintln(os.Stderr, "telegram trigger disabled: TG_BOT_TOKEN and TG_CHAT_ID are required")
	}
	if len(cfg.Sweeps) > 0 {
		sweep.Run(investigationCtx, cfg.Sweeps, core, notifier)
	}
	api := service.NewHTTP(core, cfg.APIToken.Value())
	root := http.NewServeMux()
	root.Handle("/v1/", api)
	root.Handle("/metrics", api)
	root.Handle("/", ui.Handler())
	server := &http.Server{Addr: cfg.Listen, Handler: root}
	serverErrors := make(chan error, 1)
	go func() {
		if cfg.TLS.CertFile != "" {
			serverErrors <- server.ListenAndServeTLS(cfg.TLS.CertFile, cfg.TLS.KeyFile)
		} else {
			serverErrors <- server.ListenAndServe()
		}
	}()

	var serveErr error
	select {
	case err := <-serverErrors:
		if !errors.Is(err, http.ErrServerClosed) {
			serveErr = err
		}
	case <-signalCtx.Done():
		shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 30*time.Second)
		serveErr = server.Shutdown(shutdownCtx)
		cancelShutdown()
		if serveErr != nil {
			_ = server.Close()
		}
	}
	cancelInvestigations()
	core.Wait()
	closeSessions()
	return serveErr
}

func validateServeConfig(cfg *config.Config) error {
	if cfg.Listen == "" {
		return fmt.Errorf("listen is required for serve")
	}
	if cfg.APIToken.Value() == "" {
		return fmt.Errorf("api_token is required for serve")
	}
	return nil
}

type scenario struct {
	ID              string `json:"id"`
	Question        string `json:"question"`
	ExpectFirstTool string `json:"expect_first_tool"`
}

type benchResult struct {
	ScenarioID string `json:"scenario_id"`
	Run        int    `json:"run"`
	Error      string `json:"error,omitempty"`
	agent.Metrics
}

type benchFailure struct {
	scenarioID string
	run        int
	err        string
}

type aggregate struct {
	runs, toolCalls, malformed, wrongFirst, rounds, recovered, completed int
}

func runBench(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("bench", flag.ContinueOnError)
	configPath := flags.String("config", "", "configuration YAML file")
	scenariosPath := flags.String("scenarios", "scenarios.json", "scenario JSON file")
	n := flags.Int("n", 5, "runs per scenario")
	outPath := flags.String("out", "results.jsonl", "append-only JSONL output")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *configPath == "" {
		return fmt.Errorf("-config is required")
	}
	if *n < 1 {
		return fmt.Errorf("-n must be at least 1")
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(*scenariosPath)
	if err != nil {
		return err
	}
	var scenarios []scenario
	if err := json.Unmarshal(data, &scenarios); err != nil {
		return fmt.Errorf("scenarios: %w", err)
	}
	out, err := os.OpenFile(*outPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()

	runner, closeSessions, err := newRunner(ctx, "cli-deny", cfg)
	if err != nil {
		return err
	}
	defer closeSessions()
	audit, err := store.Open(cfg.StateDir)
	if err != nil {
		return err
	}
	byScenario := make(map[string]*aggregate)
	overall := &aggregate{}
	var failures []benchFailure
	encoder := json.NewEncoder(out)
	for _, scenario := range scenarios {
		total := &aggregate{}
		byScenario[scenario.ID] = total
		for run := 1; run <= *n; run++ {
			investigation, err := audit.Begin(scenario.Question, "cli")
			if err != nil {
				return err
			}
			sink := &investigationSink{investigation: investigation}
			runner.Events = sink
			metrics, runErr := runner.Run(ctx, scenario.Question, scenario.ExpectFirstTool)
			if eventErr := sink.Err(); eventErr != nil {
				runErr = eventErr
			}
			result := benchResult{ScenarioID: scenario.ID, Run: run, Metrics: metrics}
			if runErr != nil {
				result.Error = runErr.Error()
				err = investigation.Append(store.EventFailed, store.FailedPayload{Reason: runErr.Error(), Metrics: metrics})
				failures = append(failures, benchFailure{scenario.ID, run, runErr.Error()})
			} else {
				err = investigation.Append(store.EventCompleted, store.CompletedPayload{Answer: metrics.Answer, Metrics: metrics})
			}
			if closeErr := investigation.Close(); err == nil {
				err = closeErr
			}
			if err != nil {
				return err
			}
			if err := encoder.Encode(result); err != nil {
				return err
			}
			addMetrics(total, metrics, runErr == nil)
			addMetrics(overall, metrics, runErr == nil)
		}
	}
	fmt.Println("scenario\truns\ttool_calls\tmalformed_rate\twrong_first_rate\tmean_rounds\trecovery_rate\tcompletion_rate")
	for _, scenario := range scenarios {
		printAggregate(scenario.ID, byScenario[scenario.ID])
	}
	printAggregate("OVERALL", overall)
	if len(failures) > 0 {
		fmt.Println("failures:")
		for _, failure := range failures {
			fmt.Printf("%s run %d: %s\n", failure.scenarioID, failure.run, failure.err)
		}
	}
	return nil
}

func addMetrics(total *aggregate, metrics agent.Metrics, runSucceeded bool) {
	total.runs++
	total.toolCalls += metrics.ToolCallsTotal
	total.malformed += metrics.MalformedJSON + metrics.SchemaInvalid + metrics.UnknownTool
	if metrics.WrongFirstTool {
		total.wrongFirst++
	}
	total.rounds += metrics.Rounds
	total.recovered += metrics.Recovered
	if runSucceeded && metrics.Completed {
		total.completed++
	}
}

func printAggregate(name string, total *aggregate) {
	fmt.Printf("%s\t%d\t%d\t%.3f\t%.3f\t%.2f\t%.3f\t%.3f\n", name, total.runs, total.toolCalls,
		ratio(total.malformed, total.toolCalls), ratio(total.wrongFirst, total.runs), ratio(total.rounds, total.runs),
		ratio(total.recovered, total.malformed), ratio(total.completed, total.runs))
}

func ratio(numerator, denominator int) float64 {
	if denominator == 0 {
		return 0
	}
	return float64(numerator) / float64(denominator)
}

type investigationSink struct {
	investigation *store.Investigation
	verbose       bool
	mu            sync.Mutex
	err           error
}

func (s *investigationSink) Emit(eventType string, payload any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return
	}
	if err := s.investigation.Append(eventType, payload); err != nil {
		s.err = err
		return
	}
	if s.verbose && eventType == store.EventToolCall {
		encoded, _ := json.Marshal(payload)
		var event store.ToolCallPayload
		if json.Unmarshal(encoded, &event) == nil {
			args, _ := json.Marshal(event.Args)
			fmt.Fprintf(os.Stderr, "[round %d] %s %s -> %.300s\n", event.Round, event.Name, args, event.ResultSnippet)
		}
	}
}

func (s *investigationSink) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

func newRunner(ctx context.Context, approverName string, cfg *config.Config) (*agent.Runner, func(), error) {
	if approverName != "cli-deny" && approverName != "cli-approve" && approverName != "telegram" {
		return nil, func() {}, fmt.Errorf("unknown approver %q", approverName)
	}
	factory, closeSessions, err := newRunnerFactory(ctx, cfg)
	if err != nil {
		return nil, func() {}, err
	}
	runner := factory(nil, nil)
	switch approverName {
	case "cli-deny":
		runner.Approver = agent.NewCLIApprover(false)
	case "cli-approve":
		runner.Approver = agent.NewCLIApprover(true)
	case "telegram":
		adapter, err := telegram.New(ctx, os.Getenv("TG_BOT_TOKEN"), os.Getenv("TG_CHAT_ID"), 0)
		if err != nil {
			closeSessions()
			return nil, func() {}, err
		}
		runner.Approver = adapter
		runner.Notifier = adapter
	}
	return runner, closeSessions, nil
}

func newRunnerFactory(ctx context.Context, cfg *config.Config) (service.RunnerFactory, func(), error) {
	gatedTools := make(map[string]bool, len(cfg.GatedTools))
	for _, name := range cfg.GatedTools {
		gatedTools[name] = true
	}
	servers := make([]agent.MCPServer, 0, len(cfg.MCPServers))
	for _, configured := range cfg.MCPServers {
		servers = append(servers, agent.MCPServer{Name: configured.Name, Connect: func(connectCtx context.Context) (agent.MCPSession, error) {
			var transport http.RoundTripper = http.DefaultTransport
			if configured.AuthHeader.IsSet() {
				transport = &headerRoundTripper{base: http.DefaultTransport, headers: map[string]func() string{"Authorization": configured.AuthHeader.Fresh}}
			}
			client := mcp.NewClient(&mcp.Implementation{Name: "shackleton", Version: "0.0.1"}, nil)
			return client.Connect(connectCtx, &mcp.StreamableClientTransport{
				Endpoint:   configured.URL,
				HTTPClient: &http.Client{Transport: transport, Timeout: cfg.Agent.CallTimeout.Duration()},
			}, nil)
		}})
	}
	promClient := &http.Client{Transport: &headerRoundTripper{
		base: http.DefaultTransport, headers: map[string]func() string{"Authorization": cfg.Prometheus.AuthHeader.Fresh},
	}, Timeout: cfg.Agent.CallTimeout.Duration()}
	registry, err := agent.NewRegistry(ctx, servers, gatedTools, promClient, cfg.Prometheus.URL)
	if err != nil {
		return nil, func() {}, err
	}
	closeSessions := func() { _ = registry.Close() }
	openAIClient := openai.NewClient(option.WithBaseURL(cfg.Model.BaseURL), option.WithAPIKey(cfg.Model.APIKey.Value()))
	complete := agent.StreamCompleter(openAIClient, cfg.Model.Name)
	prompt := agent.SystemPrompt(cfg.Agent.Prompt, cfg.GatedTools)
	return func(events agent.EventSink, approver agent.Approver) *agent.Runner {
		return &agent.Runner{
			Complete: complete, Tools: registry, Approver: approver, Events: events,
			Prompt: prompt, MaxRounds: cfg.Agent.MaxRounds, CallTimeout: cfg.Agent.CallTimeout.Duration(),
			InvestigationTimeout: cfg.Agent.InvestigationTimeout.Duration(),
		}
	}, closeSessions, nil
}
