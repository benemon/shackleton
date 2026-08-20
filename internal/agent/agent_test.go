package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/openai/openai-go/v3"
)

type recordedEvent struct {
	typeName string
	payload  any
}

type eventRecorder struct {
	events []recordedEvent
}

func (r *eventRecorder) Emit(eventType string, payload any) {
	r.events = append(r.events, recordedEvent{eventType, payload})
}

func testRegistry(t *testing.T, called *int) *Registry {
	t.Helper()
	r := &Registry{tools: make(map[string]toolEntry)}
	schema := map[string]any{
		"type":                 "object",
		"properties":           map[string]any{"query": map[string]any{"type": "string"}},
		"required":             []string{"query"},
		"additionalProperties": false,
	}
	err := r.add("query_prometheus_instant", "test", schema, false, func(context.Context, map[string]any) (string, error) {
		*called++
		return "result", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestSystemPrompt(t *testing.T) {
	got := SystemPrompt("You investigate the ACME estate.", []string{"run_host_command", "run_kubectl_command"})
	if !strings.HasPrefix(got, "You investigate the ACME estate. ") {
		t.Errorf("preamble missing: %q", got)
	}
	if !strings.Contains(got, "The gated tools run_host_command and run_kubectl_command are for APPLYING an approved change") {
		t.Errorf("gated tool sentence missing: %q", got)
	}
	got = SystemPrompt("", nil)
	if !strings.HasPrefix(got, "You are an infrastructure investigation agent. ") {
		t.Errorf("default preamble missing: %q", got)
	}
	if strings.Contains(got, "gated tools") {
		t.Errorf("gated tool sentence present with no gated tools: %q", got)
	}
}

func TestSchemaValidationAndMalformedRecovery(t *testing.T) {
	called := 0
	messages := []ModelMessage{
		{ToolCalls: []ModelToolCall{{Name: "query_prometheus_instant", Arguments: `{"wrong":"node_load5"}`, ID: "1"}}},
		{ToolCalls: []ModelToolCall{{Name: "query_prometheus_instant", Arguments: `{"query":`, ID: "2"}}},
		{ToolCalls: []ModelToolCall{{Name: "query_prometheus_instant", Arguments: `{"query":"node_load5"}`, ID: "3"}}},
		{Content: "done"},
	}
	index := 0
	runner := Runner{
		Tools: testRegistry(t, &called),
		Complete: func(_ context.Context, history []openai.ChatCompletionMessageParamUnion, _ []openai.ChatCompletionToolUnionParam) (ModelMessage, error) {
			if index == 1 {
				toolText := history[len(history)-1].OfTool.Content.OfString.Value
				if !strings.Contains(toolText, "schema validation failed") {
					t.Fatalf("schema failure was not returned to model: %q", toolText)
				}
			}
			result := messages[index]
			index++
			return result, nil
		},
	}
	metrics, err := runner.Run(context.Background(), "question", "query_prometheus_instant")
	if err != nil {
		t.Fatal(err)
	}
	if called != 1 {
		t.Fatalf("tool executed %d times, want 1", called)
	}
	if metrics.SchemaInvalid != 1 || metrics.MalformedJSON != 1 || metrics.Recovered != 2 {
		t.Fatalf("unexpected malformed metrics: %+v", metrics)
	}
	if metrics.ToolCallsTotal != 3 || metrics.Rounds != 4 || !metrics.Completed || metrics.Answer != "done" {
		t.Fatalf("unexpected run metrics: %+v", metrics)
	}
}

func TestRoundCapTerminatesToolSequence(t *testing.T) {
	called := 0
	runner := Runner{
		Tools:     testRegistry(t, &called),
		MaxRounds: 2,
		Complete: func(context.Context, []openai.ChatCompletionMessageParamUnion, []openai.ChatCompletionToolUnionParam) (ModelMessage, error) {
			return ModelMessage{ToolCalls: []ModelToolCall{{Name: "query_prometheus_instant", Arguments: `{"query":"up"}`, ID: "call"}}}, nil
		},
	}
	metrics, err := runner.Run(context.Background(), "question", "other_tool")
	if err != nil {
		t.Fatal(err)
	}
	if metrics.Rounds != 2 || metrics.ToolCallsTotal != 2 || metrics.Completed || metrics.Answer != "round limit reached" {
		t.Fatalf("round cap did not terminate correctly: %+v", metrics)
	}
	if !metrics.WrongFirstTool || called != 2 {
		t.Fatalf("unexpected first-tool/execution metrics: %+v, called=%d", metrics, called)
	}
}

func TestPerCallTimeout(t *testing.T) {
	r := &Registry{tools: make(map[string]toolEntry)}
	err := r.add("slow", "test", map[string]any{"type": "object"}, false, func(ctx context.Context, _ map[string]any) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	index := 0
	runner := Runner{
		Tools: r, CallTimeout: time.Millisecond,
		Complete: func(_ context.Context, history []openai.ChatCompletionMessageParamUnion, _ []openai.ChatCompletionToolUnionParam) (ModelMessage, error) {
			index++
			if index == 1 {
				return ModelMessage{ToolCalls: []ModelToolCall{{Name: "slow", Arguments: `{}`, ID: "1"}}}, nil
			}
			text := history[len(history)-1].OfTool.Content.OfString.Value
			if !strings.Contains(text, "deadline exceeded") {
				t.Fatalf("timeout not reflected to model: %q", text)
			}
			return ModelMessage{Content: "finished"}, nil
		},
	}
	metrics, err := runner.Run(context.Background(), "question", "")
	if err != nil || !metrics.Completed {
		t.Fatalf("run failed: metrics=%+v err=%v", metrics, err)
	}
}

func TestInvestigationTimeoutReturnsOutcomeWithPartialMetrics(t *testing.T) {
	runner := Runner{
		Tools:                testRegistry(t, new(int)),
		InvestigationTimeout: 10 * time.Millisecond,
		Complete: func(ctx context.Context, _ []openai.ChatCompletionMessageParamUnion, _ []openai.ChatCompletionToolUnionParam) (ModelMessage, error) {
			<-ctx.Done()
			return ModelMessage{}, ctx.Err()
		},
	}
	metrics, err := runner.Run(context.Background(), "question", "")
	if err != nil {
		t.Fatal(err)
	}
	if metrics.Rounds != 1 || metrics.Completed || metrics.Answer != "wall clock exceeded" {
		t.Fatalf("timeout metrics = %+v", metrics)
	}
}

func TestCLIApprovalDecisionRecordsVia(t *testing.T) {
	registry := &Registry{tools: make(map[string]toolEntry)}
	if err := registry.add("repair", "test", map[string]any{"type": "object"}, true,
		func(context.Context, map[string]any) (string, error) { return "repaired", nil }); err != nil {
		t.Fatal(err)
	}
	recorder := &eventRecorder{}
	completion := 0
	runner := Runner{
		Tools: registry, Approver: NewCLIApprover(false), Events: recorder,
		Complete: func(context.Context, []openai.ChatCompletionMessageParamUnion, []openai.ChatCompletionToolUnionParam) (ModelMessage, error) {
			completion++
			if completion == 1 {
				return ModelMessage{ToolCalls: []ModelToolCall{{Name: "repair", Arguments: `{}`, ID: "call"}}}, nil
			}
			return ModelMessage{Content: "done"}, nil
		},
	}
	if _, err := runner.Run(context.Background(), "question", ""); err != nil {
		t.Fatal(err)
	}
	for _, event := range recorder.events {
		if event.typeName != "approval_decided" {
			continue
		}
		encoded, err := json.Marshal(event.payload)
		if err != nil {
			t.Fatal(err)
		}
		var payload struct {
			Approved bool   `json:"approved"`
			Via      string `json:"via"`
		}
		if err := json.Unmarshal(encoded, &payload); err != nil {
			t.Fatal(err)
		}
		if payload.Approved || payload.Via != "cli" {
			t.Fatalf("approval decision = %+v", payload)
		}
		return
	}
	t.Fatal("approval_decided event not emitted")
}

func TestFinalRoundNudgeAppearsExactlyOnceAtCap(t *testing.T) {
	called := 0
	completion := 0
	runner := Runner{
		Tools: testRegistry(t, &called), MaxRounds: 2,
		Complete: func(_ context.Context, history []openai.ChatCompletionMessageParamUnion, _ []openai.ChatCompletionToolUnionParam) (ModelMessage, error) {
			completion++
			count := 0
			for _, message := range history {
				if message.OfUser != nil && message.OfUser.Content.OfString.Value == "Budget check: this is your final round. Do not call more tools unless strictly necessary - give your best concise answer from what you already know." {
					count++
				}
			}
			want := 0
			if completion == 2 {
				want = 1
			}
			if count != want {
				t.Fatalf("completion %d nudge count = %d, want %d", completion, count, want)
			}
			if completion == 1 {
				return ModelMessage{ToolCalls: []ModelToolCall{{Name: "query_prometheus_instant", Arguments: `{"query":"up"}`, ID: "one"}}}, nil
			}
			return ModelMessage{Content: "done"}, nil
		},
	}
	metrics, err := runner.Run(context.Background(), "question", "")
	if err != nil || !metrics.Completed || metrics.Answer != "done" {
		t.Fatalf("metrics = %+v, error = %v", metrics, err)
	}
}

type fakeMCPSession struct {
	callErr error
	pingErr error
	result  string
	calls   int
}

func (s *fakeMCPSession) ListTools(context.Context, *mcp.ListToolsParams) (*mcp.ListToolsResult, error) {
	return &mcp.ListToolsResult{Tools: []*mcp.Tool{{
		Name: "repair", Description: "repair", InputSchema: map[string]any{"type": "object"},
	}}}, nil
}

func (s *fakeMCPSession) CallTool(context.Context, *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	s.calls++
	if s.callErr != nil {
		return nil, s.callErr
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s.result}}}, nil
}

func (s *fakeMCPSession) Ping(context.Context, *mcp.PingParams) error { return s.pingErr }
func (s *fakeMCPSession) Close() error                                { return nil }

func TestRegistryReconnectsAndRetriesFailedToolCall(t *testing.T) {
	connects := 0
	sessions := []*fakeMCPSession{
		{callErr: errors.New("connection reset"), pingErr: errors.New("dead session")},
		{result: "recovered result"},
	}
	connect := func(context.Context) (MCPSession, error) {
		session := sessions[connects]
		connects++
		return session, nil
	}
	registry, err := NewRegistry(context.Background(), []MCPServer{{Name: "fake", Connect: connect}}, nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	result, err := registry.tools["repair"].call(context.Background(), map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if result != "recovered result" || connects != 2 {
		t.Fatalf("result = %q, connects = %d", result, connects)
	}
}

func TestRegistryNeverRetriesGatedToolCall(t *testing.T) {
	connects := 0
	sessions := []*fakeMCPSession{
		{callErr: errors.New("connection reset"), pingErr: errors.New("dead session")},
		{result: "would be a double execution"},
	}
	connect := func(context.Context) (MCPSession, error) {
		session := sessions[connects]
		connects++
		return session, nil
	}
	registry, err := NewRegistry(context.Background(), []MCPServer{{Name: "fake", Connect: connect}},
		map[string]bool{"repair": true}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	_, err = registry.tools["repair"].call(context.Background(), map[string]any{})
	if err == nil || err.Error() != "connection reset" {
		t.Fatalf("gated call error = %v, want original transport error", err)
	}
	if connects != 2 {
		t.Fatalf("connects = %d, want 2 (session refreshed for later calls)", connects)
	}
	if sessions[1].calls != 0 {
		t.Fatalf("gated tool was re-executed on the fresh session (%d calls)", sessions[1].calls)
	}
}
