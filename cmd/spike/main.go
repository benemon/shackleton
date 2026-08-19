// Command spike is the shk-b1l go/no-go probe harness. It runs on the NAS
// (the MCP servers are localhost-only, firewalled to uid 0) with
// /opt/holmes/env sourced; credentials never leave that host.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/benemon/shackleton/internal/agent"
	"github.com/benemon/shackleton/internal/telegram"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"
)

const defaultBase = "https://litellm.apps.ocp.lab.orbital.home/v1"

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: spike <llm|mcp|thanos|agent|bench>")
		os.Exit(2)
	}
	if f := os.Getenv("SPIKE_ENV_FILE"); f != "" {
		if err := loadEnvFile(f); err != nil {
			fmt.Fprintf(os.Stderr, "FAIL: env file: %v\n", err)
			os.Exit(1)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	var err error
	probe := true
	switch os.Args[1] {
	case "llm":
		err = probeLLM(ctx)
	case "mcp":
		err = probeMCP(ctx)
	case "thanos":
		err = probeThanos(ctx)
	case "agent":
		probe = false
		err = runAgent(context.Background(), os.Args[2:])
	case "bench":
		probe = false
		err = runBench(context.Background(), os.Args[2:])
	default:
		err = fmt.Errorf("unknown probe %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: %v\n", err)
		os.Exit(1)
	}
	if probe {
		fmt.Println("PASS")
	}
}

// headerRoundTripper injects a static header set into every request — the
// mechanism Shackleton will use for auth against gated MCP servers.
type headerRoundTripper struct {
	base    http.RoundTripper
	headers map[string]string
}

func (h *headerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	for k, v := range h.headers {
		req.Header.Set(k, v)
	}
	return h.base.RoundTrip(req)
}

// loadEnvFile parses a dotenv-style file (KEY=value, # comments; values may
// contain spaces unquoted — /opt/holmes/env is written for python-dotenv and
// is NOT shell-sourceable). Existing environment wins.
func loadEnvFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	for line := range strings.Lines(string(data)) {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok || os.Getenv(k) != "" {
			continue
		}
		v = strings.Trim(v, `"'`)
		os.Setenv(k, v)
	}
	return nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func probeLLM(ctx context.Context) error {
	client := openai.NewClient(
		option.WithBaseURL(envOr("OPENAI_API_BASE", defaultBase)),
		option.WithAPIKey(os.Getenv("OPENAI_API_KEY")),
	)

	tools := []openai.ChatCompletionToolUnionParam{
		openai.ChatCompletionFunctionTool(shared.FunctionDefinitionParam{
			Name:        "query_prometheus",
			Description: openai.String("Run a PromQL query against the lab's Prometheus and return the result."),
			Parameters: shared.FunctionParameters{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{"type": "string", "description": "PromQL expression"},
				},
				"required": []string{"query"},
			},
		}),
		openai.ChatCompletionFunctionTool(shared.FunctionDefinitionParam{
			Name:        "run_host_command",
			Description: openai.String("Run a read-only diagnostic command on a lab host over SSH."),
			Parameters: shared.FunctionParameters{
				"type": "object",
				"properties": map[string]any{
					"host":    map[string]any{"type": "string", "enum": []string{"nas", "oddjob", "mini"}},
					"command": map[string]any{"type": "string"},
				},
				"required": []string{"host", "command"},
			},
		}),
	}

	params := openai.ChatCompletionNewParams{
		Model: envOr("SPIKE_MODEL", "qwen-a3b-thinking"),
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.SystemMessage("You are an infrastructure investigation agent. Use the provided tools to answer; do not guess."),
			openai.UserMessage("What is the current 5-minute load average on host nas?"),
		},
		Tools: tools,
	}

	// Streaming with accumulator — the mode Shackleton will actually use.
	stream := client.Chat.Completions.NewStreaming(ctx, params)
	acc := openai.ChatCompletionAccumulator{}
	for stream.Next() {
		acc.AddChunk(stream.Current())
		if tc, ok := acc.JustFinishedToolCall(); ok {
			fmt.Printf("stream: finished tool call: %s(%s)\n", tc.Name, tc.Arguments)
		}
	}
	if err := stream.Err(); err != nil {
		return fmt.Errorf("stream: %w", err)
	}
	if len(acc.Choices) == 0 {
		return fmt.Errorf("stream: no choices accumulated")
	}
	msg := acc.Choices[0].Message
	if len(msg.ToolCalls) == 0 {
		return fmt.Errorf("stream: model made no tool call; content=%q", msg.Content)
	}
	for _, tc := range msg.ToolCalls {
		var args map[string]any
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
			return fmt.Errorf("tool call %s: arguments are not valid JSON: %w (raw=%q)", tc.Function.Name, err, tc.Function.Arguments)
		}
		fmt.Printf("accumulated: %s args=%v id=%s\n", tc.Function.Name, args, tc.ID)
	}
	return nil
}

func probeMCP(ctx context.Context) error {
	for _, ep := range []string{"http://127.0.0.1:8100/mcp", "http://127.0.0.1:8000/mcp"} {
		client := mcp.NewClient(&mcp.Implementation{Name: "shackleton-spike", Version: "0.0.1"}, nil)
		transport := &mcp.StreamableClientTransport{
			Endpoint: ep,
			// Header injection through a custom client: the servers ignore the
			// header today, but this proves the transport carries it (the thanos
			// probe proves end-to-end acceptance by a gated endpoint).
			HTTPClient: &http.Client{
				Transport: &headerRoundTripper{
					base:    http.DefaultTransport,
					headers: map[string]string{"Authorization": "Bearer spike-injection-test"},
				},
				Timeout: 30 * time.Second,
			},
		}
		session, err := client.Connect(ctx, transport, nil)
		if err != nil {
			return fmt.Errorf("%s: connect: %w", ep, err)
		}
		init := session.InitializeResult()
		fmt.Printf("%s: server=%s %s\n", ep, init.ServerInfo.Name, init.ServerInfo.Version)
		tools, err := session.ListTools(ctx, nil)
		if err != nil {
			return fmt.Errorf("%s: list tools: %w", ep, err)
		}
		for _, t := range tools.Tools {
			schema, _ := json.Marshal(t.InputSchema)
			fmt.Printf("  tool %s: %s\n    schema: %s\n", t.Name, t.Description, schema)
		}
		session.Close()
	}
	return nil
}

func probeThanos(ctx context.Context) error {
	auth := os.Getenv("PROMETHEUS_AUTH_HEADER")
	if auth == "" {
		return fmt.Errorf("PROMETHEUS_AUTH_HEADER not set")
	}
	hc := &http.Client{
		Transport: &headerRoundTripper{
			base:    http.DefaultTransport,
			headers: map[string]string{"Authorization": auth},
		},
		Timeout: 30 * time.Second,
	}
	url := envOr("THANOS_URL", "https://thanos-querier-openshift-monitoring.apps.ocp.lab.orbital.home") +
		"/api/v1/query?query=node_load5"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("thanos: %s: %s", resp.Status, body)
	}
	fmt.Printf("thanos: 200 OK via injected header; body starts: %.200s\n", body)
	return nil
}

func runAgent(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("agent", flag.ContinueOnError)
	approverName := flags.String("approver", "cli-deny", "cli-deny, cli-approve, or telegram")
	verbose := flags.Bool("v", false, "trace each tool call and result to stderr")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return fmt.Errorf("usage: spike agent [-approver=cli-deny|cli-approve|telegram] [-v] \"<question>\"")
	}
	runner, closeSessions, err := newRunner(ctx, *approverName)
	if err != nil {
		return err
	}
	defer closeSessions()
	if *verbose {
		runner.Trace = func(round int, name, args, result string) {
			fmt.Fprintf(os.Stderr, "[round %d] %s %s -> %.300s\n", round, name, args, result)
		}
	}
	metrics, runErr := runner.Run(ctx, flags.Arg(0), "")
	if metrics.Answer != "" {
		fmt.Println(metrics.Answer)
	}
	encoded, _ := json.Marshal(metrics)
	fmt.Fprintln(os.Stderr, string(encoded))
	return runErr
}

type scenario struct {
	ID              string `json:"id"`
	Question        string `json:"question"`
	ExpectFirstTool string `json:"expect_first_tool"`
}

type benchResult struct {
	ScenarioID string `json:"scenario_id"`
	Run        int    `json:"run"`
	agent.Metrics
}

type aggregate struct {
	runs, toolCalls, malformed, wrongFirst, rounds, recovered, completed int
}

func runBench(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("bench", flag.ContinueOnError)
	scenariosPath := flags.String("scenarios", "scenarios.json", "scenario JSON file")
	n := flags.Int("n", 5, "runs per scenario")
	outPath := flags.String("out", "results.jsonl", "append-only JSONL output")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *n < 1 {
		return fmt.Errorf("-n must be at least 1")
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

	runner, closeSessions, err := newRunner(ctx, "cli-deny")
	if err != nil {
		return err
	}
	defer closeSessions()
	byScenario := make(map[string]*aggregate)
	overall := &aggregate{}
	encoder := json.NewEncoder(out)
	for _, scenario := range scenarios {
		total := &aggregate{}
		byScenario[scenario.ID] = total
		for run := 1; run <= *n; run++ {
			metrics, runErr := runner.Run(ctx, scenario.Question, scenario.ExpectFirstTool)
			if err := encoder.Encode(benchResult{ScenarioID: scenario.ID, Run: run, Metrics: metrics}); err != nil {
				return err
			}
			addMetrics(total, metrics)
			addMetrics(overall, metrics)
			if runErr != nil {
				return fmt.Errorf("scenario %s run %d: %w", scenario.ID, run, runErr)
			}
		}
	}
	fmt.Println("scenario\truns\ttool_calls\tmalformed_rate\twrong_first_rate\tmean_rounds\trecovery_rate\tcompletion_rate")
	for _, scenario := range scenarios {
		printAggregate(scenario.ID, byScenario[scenario.ID])
	}
	printAggregate("OVERALL", overall)
	return nil
}

func addMetrics(total *aggregate, metrics agent.Metrics) {
	total.runs++
	total.toolCalls += metrics.ToolCallsTotal
	total.malformed += metrics.MalformedJSON + metrics.SchemaInvalid + metrics.UnknownTool
	if metrics.WrongFirstTool {
		total.wrongFirst++
	}
	total.rounds += metrics.Rounds
	total.recovered += metrics.Recovered
	if metrics.Completed {
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

func newRunner(ctx context.Context, approverName string) (*agent.Runner, func(), error) {
	if approverName != "cli-deny" && approverName != "cli-approve" && approverName != "telegram" {
		return nil, func() {}, fmt.Errorf("unknown approver %q", approverName)
	}
	if approverName == "telegram" {
		if f := os.Getenv("TELEGRAM_ENV_FILE"); f != "" {
			if err := loadEnvFile(f); err != nil {
				return nil, func() {}, fmt.Errorf("telegram env file: %w", err)
			}
		}
	}
	sessions := make([]*mcp.ClientSession, 0, 2)
	for _, endpoint := range []string{"http://127.0.0.1:8100/mcp", "http://127.0.0.1:8000/mcp"} {
		client := mcp.NewClient(&mcp.Implementation{Name: "shackleton-spike", Version: "0.0.1"}, nil)
		session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
			Endpoint:   endpoint,
			HTTPClient: &http.Client{Timeout: 30 * time.Second},
		}, nil)
		if err != nil {
			for _, opened := range sessions {
				opened.Close()
			}
			return nil, func() {}, fmt.Errorf("%s: connect: %w", endpoint, err)
		}
		sessions = append(sessions, session)
	}
	closeSessions := func() {
		for _, session := range sessions {
			session.Close()
		}
	}
	auth := os.Getenv("PROMETHEUS_AUTH_HEADER")
	thanosClient := &http.Client{Transport: &headerRoundTripper{
		base: http.DefaultTransport, headers: map[string]string{"Authorization": auth},
	}, Timeout: 30 * time.Second}
	registry, err := agent.NewRegistry(ctx, sessions, thanosClient, envOr("THANOS_URL", "https://thanos-querier-openshift-monitoring.apps.ocp.lab.orbital.home"))
	if err != nil {
		closeSessions()
		return nil, func() {}, err
	}
	openAIClient := openai.NewClient(option.WithBaseURL(envOr("OPENAI_API_BASE", defaultBase)), option.WithAPIKey(os.Getenv("OPENAI_API_KEY")))
	runner := &agent.Runner{
		Complete: agent.StreamCompleter(openAIClient, envOr("SPIKE_MODEL", "qwen-a3b-thinking")),
		Tools:    registry,
	}
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
