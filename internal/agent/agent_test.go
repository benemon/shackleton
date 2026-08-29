package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
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
	got := SystemPrompt("You investigate the ACME estate.",
		"Inventory:\nHosts:\n- alpha",
		[]string{"query_prometheus_instant", "query_prometheus_range"},
		[]string{"query_loki_logs"},
		[]string{"search_redhat_kb", "get_redhat_kb"},
		[]string{"run_host_command", "run_kubectl_command"})
	if !strings.Contains(got, "You investigate the ACME estate.\nInventory:\nHosts:\n- alpha\n") {
		t.Errorf("environment section missing after preamble: %q", got)
	}
	if !strings.HasPrefix(got, "You investigate the ACME estate.\n") {
		t.Errorf("preamble missing: %q", got)
	}
	if !strings.Contains(got, "query_prometheus_instant and query_prometheus_range are the ONLY way to read metrics.") {
		t.Errorf("metrics sentence missing: %q", got)
	}
	if !strings.Contains(got, "Use query_loki_logs to search logs") {
		t.Errorf("logs sentence missing: %q", got)
	}
	if !strings.Contains(got, "The gated tools run_host_command and run_kubectl_command are for APPLYING an approved change") {
		t.Errorf("gated tool sentence missing: %q", got)
	}
	if !strings.Contains(got, "re-run the check that motivated it and state whether the symptom cleared") {
		t.Errorf("verification sentence missing: %q", got)
	}
	if !strings.Contains(got, "executed without error is done") ||
		!strings.Contains(got, "never propose the same action again") {
		t.Errorf("accepted-vs-achieved sentence missing: %q", got)
	}
	if !strings.Contains(got, "Evidence must quote values you actually read from tool results") ||
		!strings.Contains(got, "never assert a number or a state you did not observe") {
		t.Errorf("anti-fabrication sentence missing: %q", got)
	}
	if !strings.Contains(got, "Use search_redhat_kb, get_redhat_kb to consult vendor documentation and knowledge bases.") {
		t.Errorf("knowledge sentence missing: %q", got)
	}
	if !strings.Contains(got, "A procedural recommendation") ||
		!strings.Contains(got, "otherwise state plainly that it comes from general knowledge and is unverified") {
		t.Errorf("grounding sentence missing: %q", got)
	}
	if !strings.Contains(got, "read its current state first") ||
		!strings.Contains(got, "anchored to the versions and names you observed, never a generic example") {
		t.Errorf("estate-anchoring sentence missing: %q", got)
	}
	if !strings.Contains(got, "Never include estate hostnames or identifiers in documentation search queries.") {
		t.Errorf("query-privacy sentence missing: %q", got)
	}
	if !strings.Contains(got, "End your final answer with a fenced json block") || !strings.Contains(got, `{"verdict":"healthy","summary":"<one line>","evidence":["<item>"]}`) {
		t.Errorf("verdict contract missing: %q", got)
	}
	got = SystemPrompt("", "", nil, nil, nil, []string{"run_host_command"})
	if !strings.Contains(got, "The gated tool run_host_command is for APPLYING an approved change") {
		t.Errorf("singular gated sentence wrong: %q", got)
	}
	if !strings.Contains(got, "CALL the gated tool — the call itself is the proposal") {
		t.Errorf("proposal-means-calling sentence missing: %q", got)
	}
	got = SystemPrompt("", "", nil, nil, nil, nil)
	if !strings.HasPrefix(got, "You are an infrastructure investigation agent. ") {
		t.Errorf("default preamble missing: %q", got)
	}
	for _, absent := range []string{"ONLY way to read metrics", "search logs", "gated tools", "vendor documentation", "procedural recommendation"} {
		if strings.Contains(got, absent) {
			t.Errorf("%q present with nothing registered: %q", absent, got)
		}
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

func TestOversizedToolResultIsTruncatedWithMarker(t *testing.T) {
	r := &Registry{tools: make(map[string]toolEntry)}
	huge := strings.Repeat("x", 500)
	if err := r.add("big", "test", map[string]any{"type": "object"}, false,
		func(context.Context, map[string]any) (string, error) { return huge, nil }); err != nil {
		t.Fatal(err)
	}
	index := 0
	runner := Runner{
		Tools: r, MaxToolResult: 100,
		Complete: func(_ context.Context, history []openai.ChatCompletionMessageParamUnion, _ []openai.ChatCompletionToolUnionParam) (ModelMessage, error) {
			index++
			if index == 1 {
				return ModelMessage{ToolCalls: []ModelToolCall{{Name: "big", Arguments: `{}`, ID: "1"}}}, nil
			}
			text := history[len(history)-1].OfTool.Content.OfString.Value
			if !strings.HasPrefix(text, strings.Repeat("x", 100)+"\n…[truncated: the result was 500 characters, showing the first 100.") {
				t.Fatalf("truncated tool message = %.160q", text)
			}
			if strings.Contains(text[100:], "x") {
				t.Fatalf("payload leaked past the cap: %.200q", text)
			}
			return ModelMessage{Content: "done"}, nil
		},
	}
	metrics, err := runner.Run(context.Background(), "question", "")
	if err != nil || !metrics.Completed {
		t.Fatalf("run failed: metrics=%+v err=%v", metrics, err)
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

func TestEmitVerdictAsksWithoutToolsAndCarriesTheAnswer(t *testing.T) {
	var seenMessages []openai.ChatCompletionMessageParamUnion
	var seenTools []openai.ChatCompletionToolUnionParam
	runner := Runner{
		Complete: func(_ context.Context, messages []openai.ChatCompletionMessageParamUnion, tools []openai.ChatCompletionToolUnionParam) (ModelMessage, error) {
			seenMessages, seenTools = messages, tools
			return ModelMessage{Content: "```json\n{\"verdict\":\"healthy\",\"summary\":\"s\",\"evidence\":[]}\n```"}, nil
		},
	}
	block, err := runner.EmitVerdict(context.Background(), "the question", "the final answer")
	if err != nil || !strings.Contains(block, `"verdict":"healthy"`) {
		t.Fatalf("block = %q, err = %v", block, err)
	}
	if len(seenTools) != 0 {
		t.Fatalf("extraction call carried %d tools", len(seenTools))
	}
	user := seenMessages[len(seenMessages)-1].OfUser.Content.OfString.Value
	if !strings.Contains(user, "the question") || !strings.Contains(user, "the final answer") {
		t.Fatalf("extraction user message = %q", user)
	}
}

type fakeMCPSession struct {
	callErr  error
	pingErr  error
	pingErrs []error
	result   string
	calls    int
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

func (s *fakeMCPSession) Ping(context.Context, *mcp.PingParams) error {
	if len(s.pingErrs) > 0 {
		err := s.pingErrs[0]
		s.pingErrs = s.pingErrs[1:]
		return err
	}
	return s.pingErr
}
func (s *fakeMCPSession) Close() error { return nil }

func TestDuplicateToolNamesAcrossServersFailStartup(t *testing.T) {
	connect := func(context.Context) (MCPSession, error) { return &fakeMCPSession{}, nil }
	_, err := NewRegistry(context.Background(),
		[]MCPServer{{Name: "first", Connect: connect}, {Name: "second", Connect: connect}}, nil, nil, nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), `second: duplicate tool "repair"`) {
		t.Fatalf("collision error = %v", err)
	}
}

func TestNativeSourcesRegisterNamedTools(t *testing.T) {
	var request *http.Request
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request = r.Clone(context.Background())
		fmt.Fprint(w, "payload")
	}))
	defer server.Close()
	source := func(name string) NativeSource {
		return NativeSource{Name: name, Client: server.Client(), BaseURL: server.URL}
	}
	registry, err := NewRegistry(context.Background(), nil, nil,
		[]NativeSource{source("prometheus"), source("vm")}, []NativeSource{source("loki")}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	for _, name := range []string{"query_prometheus_instant", "query_prometheus_range",
		"query_vm_instant", "query_vm_range", "query_loki_logs"} {
		if _, ok := registry.tools[name]; !ok {
			t.Errorf("tool %s not registered", name)
		}
	}
	result, err := registry.tools["query_loki_logs"].call(context.Background(),
		map[string]any{"query": `{app="x"}`, "start": "s", "end": "e"})
	if err != nil || result != "payload" {
		t.Fatalf("loki call = %q, %v", result, err)
	}
	if request.URL.Path != "/loki/api/v1/query_range" {
		t.Errorf("loki path = %s", request.URL.Path)
	}
	q := request.URL.Query()
	if q.Get("direction") != "backward" || q.Get("limit") != "100" || q.Get("query") != `{app="x"}` {
		t.Errorf("loki params = %v", q)
	}
	if _, err := registry.tools["query_vm_instant"].call(context.Background(), map[string]any{"query": "up"}); err != nil {
		t.Fatal(err)
	}
	if request.URL.Path != "/api/v1/query" {
		t.Errorf("prometheus path = %s", request.URL.Path)
	}
}

func TestNativeSourceErrorsCarrySourceName(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "denied", http.StatusUnauthorized)
	}))
	defer server.Close()
	registry, err := NewRegistry(context.Background(), nil, nil, nil,
		[]NativeSource{{Name: "loki", Client: server.Client(), BaseURL: server.URL}}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	_, err = registry.tools["query_loki_logs"].call(context.Background(),
		map[string]any{"query": "q", "start": "s", "end": "e"})
	if err == nil || !strings.HasPrefix(err.Error(), "loki: 401") {
		t.Fatalf("error = %v", err)
	}
}

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
	registry, err := NewRegistry(context.Background(), []MCPServer{{Name: "fake", Connect: connect}}, nil, nil, nil, nil, nil)
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
	// Alive at submission (first ping passes), dies mid-flight: the one
	// permitted submission is spent, so the fresh session must not re-run it.
	sessions := []*fakeMCPSession{
		{callErr: errors.New("connection reset"), pingErrs: []error{nil}, pingErr: errors.New("dead session")},
		{result: "would be a double execution"},
	}
	connect := func(context.Context) (MCPSession, error) {
		session := sessions[connects]
		connects++
		return session, nil
	}
	registry, err := NewRegistry(context.Background(), []MCPServer{{Name: "fake", Connect: connect}},
		map[string]bool{"repair": true}, nil, nil, nil, nil)
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

func TestGatedCallOnDeadSessionReconnectsBeforeSubmitting(t *testing.T) {
	connects := 0
	sessions := []*fakeMCPSession{
		{callErr: errors.New("must never be submitted"), pingErr: errors.New("session idled out")},
		{result: "executed once"},
	}
	connect := func(context.Context) (MCPSession, error) {
		session := sessions[connects]
		connects++
		return session, nil
	}
	registry, err := NewRegistry(context.Background(), []MCPServer{{Name: "fake", Connect: connect}},
		map[string]bool{"repair": true}, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	result, err := registry.tools["repair"].call(context.Background(), map[string]any{})
	if err != nil || result != "executed once" {
		t.Fatalf("gated call on dead session = %q, %v", result, err)
	}
	if sessions[0].calls != 0 {
		t.Fatalf("gated call was submitted to the dead session (%d calls)", sessions[0].calls)
	}
	if sessions[1].calls != 1 || connects != 2 {
		t.Fatalf("fresh session calls = %d, connects = %d", sessions[1].calls, connects)
	}
}

type fakeResolver struct{}

func (fakeResolver) ResolveTarget(target string) (string, bool) {
	if target == "nas" || target == "nas.lab.example" || target == "nas.lab.example:9100" {
		return "nas", true
	}
	return "", false
}

func (fakeResolver) KnownTargets() []string { return []string{"nas", "mini"} }

type recordingApprover struct{ calls []ToolCall }

func (a *recordingApprover) RequestApproval(_ context.Context, call ToolCall) (Decision, error) {
	a.calls = append(a.calls, call)
	return Decision{Approved: true, Via: "test"}, nil
}

func gatedHostRegistry(t *testing.T, executed *[]string) *Registry {
	t.Helper()
	registry := &Registry{tools: make(map[string]toolEntry)}
	schema := map[string]any{
		"type":                 "object",
		"properties":           map[string]any{"host": map[string]any{"type": "string"}, "command": map[string]any{"type": "array", "items": map[string]any{"type": "string"}}},
		"required":             []string{"host", "command"},
		"additionalProperties": false,
	}
	err := registry.add("run_host_command", "test", schema, true, func(_ context.Context, args map[string]any) (string, error) {
		*executed = append(*executed, fmt.Sprint(args["host"]))
		return "ran", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

func TestPreflightRejectsUnknownTargetBeforeApproval(t *testing.T) {
	var executed []string
	approver := &recordingApprover{}
	completion := 0
	runner := Runner{
		Tools: gatedHostRegistry(t, &executed), Approver: approver, Targets: fakeResolver{},
		Complete: func(_ context.Context, history []openai.ChatCompletionMessageParamUnion, _ []openai.ChatCompletionToolUnionParam) (ModelMessage, error) {
			completion++
			if completion == 1 {
				return ModelMessage{ToolCalls: []ModelToolCall{{Name: "run_host_command", Arguments: `{"host":"oddjob","command":["uptime"]}`, ID: "one"}}}, nil
			}
			toolText := history[len(history)-1].OfTool.Content.OfString.Value
			if !strings.Contains(toolText, `host "oddjob" is not in the inventory`) || !strings.Contains(toolText, "known hosts: nas, mini") {
				t.Fatalf("pre-flight rejection not returned to model: %q", toolText)
			}
			return ModelMessage{Content: "done"}, nil
		},
	}
	metrics, err := runner.Run(context.Background(), "question", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(approver.calls) != 0 {
		t.Fatalf("approval was requested for an unknown target: %+v", approver.calls)
	}
	if len(executed) != 0 {
		t.Fatalf("gated tool executed for an unknown target: %v", executed)
	}
	if metrics.ToolErrors != 1 {
		t.Fatalf("metrics = %+v", metrics)
	}
}

func TestReproposedGatedCallShortCircuitsAfterApply(t *testing.T) {
	var executed []string
	approver := &recordingApprover{}
	completion := 0
	runner := Runner{
		Tools: gatedHostRegistry(t, &executed), Approver: approver, Targets: fakeResolver{},
		Complete: func(_ context.Context, history []openai.ChatCompletionMessageParamUnion, _ []openai.ChatCompletionToolUnionParam) (ModelMessage, error) {
			completion++
			// Propose the identical gated call twice, as the model does when
			// an async mutation's effect is not yet observable.
			if completion <= 2 {
				return ModelMessage{ToolCalls: []ModelToolCall{{Name: "run_host_command", Arguments: `{"host":"nas","command":["patch"]}`, ID: fmt.Sprint(completion)}}}, nil
			}
			toolText := history[len(history)-1].OfTool.Content.OfString.Value
			if !strings.Contains(toolText, "Already applied this investigation") {
				t.Fatalf("second proposal was not short-circuited: %q", toolText)
			}
			return ModelMessage{Content: "done"}, nil
		},
	}
	if _, err := runner.Run(context.Background(), "question", ""); err != nil {
		t.Fatal(err)
	}
	if len(approver.calls) != 1 {
		t.Fatalf("identical gated call raised %d approvals, want 1", len(approver.calls))
	}
	if len(executed) != 1 {
		t.Fatalf("gated tool executed %d times, want 1", len(executed))
	}
}

func TestPreflightRewritesAliasToCanonicalTarget(t *testing.T) {
	var executed []string
	approver := &recordingApprover{}
	completion := 0
	runner := Runner{
		Tools: gatedHostRegistry(t, &executed), Approver: approver, Targets: fakeResolver{},
		Complete: func(context.Context, []openai.ChatCompletionMessageParamUnion, []openai.ChatCompletionToolUnionParam) (ModelMessage, error) {
			completion++
			if completion == 1 {
				return ModelMessage{ToolCalls: []ModelToolCall{{Name: "run_host_command", Arguments: `{"host":"nas.lab.example:9100","command":["uptime"]}`, ID: "one"}}}, nil
			}
			return ModelMessage{Content: "done"}, nil
		},
	}
	if _, err := runner.Run(context.Background(), "question", ""); err != nil {
		t.Fatal(err)
	}
	if len(approver.calls) != 1 {
		t.Fatalf("approval calls = %+v", approver.calls)
	}
	call := approver.calls[0]
	if call.Args["host"] != "nas" || !strings.Contains(call.Human, "nas:") || !strings.Contains(call.ArgsJSON, `"host":"nas"`) {
		t.Fatalf("alias not rewritten to canonical: %+v", call)
	}
	if len(executed) != 1 || executed[0] != "nas" {
		t.Fatalf("executed hosts = %v", executed)
	}
}

func TestEmptyFinalAnswerNudgedThenFailsLoudly(t *testing.T) {
	called := 0
	completion := 0
	runner := Runner{
		Tools: testRegistry(t, &called),
		Complete: func(_ context.Context, history []openai.ChatCompletionMessageParamUnion, _ []openai.ChatCompletionToolUnionParam) (ModelMessage, error) {
			completion++
			if completion == 1 {
				return ModelMessage{Content: ""}, nil
			}
			last := history[len(history)-1]
			if last.OfUser == nil || last.OfUser.Content.OfString.Value != "Your final message was empty. State your answer now." {
				t.Fatalf("nudge not injected before retry: %+v", last)
			}
			return ModelMessage{Content: "the real answer"}, nil
		},
	}
	metrics, err := runner.Run(context.Background(), "question", "")
	if err != nil {
		t.Fatal(err)
	}
	if !metrics.Completed || metrics.Answer != "the real answer" {
		t.Fatalf("metrics = %+v", metrics)
	}

	runner.Complete = func(context.Context, []openai.ChatCompletionMessageParamUnion, []openai.ChatCompletionToolUnionParam) (ModelMessage, error) {
		return ModelMessage{Content: "  "}, nil
	}
	if _, err := runner.Run(context.Background(), "question", ""); err == nil || !strings.Contains(err.Error(), "empty final answer") {
		t.Fatalf("repeated empty answer did not fail loudly: %v", err)
	}
}
