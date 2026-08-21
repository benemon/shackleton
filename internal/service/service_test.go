package service

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/benemon/shackleton/internal/agent"
	"github.com/benemon/shackleton/internal/config"
	"github.com/benemon/shackleton/internal/kb"
	"github.com/benemon/shackleton/internal/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/openai/openai-go/v3"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

type approvalSession struct{}

func (approvalSession) ListTools(context.Context, *mcp.ListToolsParams) (*mcp.ListToolsResult, error) {
	return &mcp.ListToolsResult{Tools: []*mcp.Tool{{
		Name: "repair", Description: "repair", InputSchema: map[string]any{"type": "object"},
	}}}, nil
}

func (approvalSession) CallTool(context.Context, *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "repaired"}}}, nil
}

func (approvalSession) Ping(context.Context, *mcp.PingParams) error { return nil }
func (approvalSession) Close() error                                { return nil }

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	t.Setenv("SERVICE_MODEL_SECRET", "known-model-secret")
	t.Setenv("SERVICE_MCP_SECRET", "known-mcp-secret")
	t.Setenv("SERVICE_PROM_SECRET", "known-prometheus-secret")
	t.Setenv("SERVICE_API_SECRET", "known-api-secret")
	path := filepath.Join(t.TempDir(), "shackleton.yaml")
	contents := `
listen: "127.0.0.1:8420"
state_dir: /tmp/shackleton
model:
  base_url: https://model.example/v1
  name: test-model
  api_key: {env: SERVICE_MODEL_SECRET}
mcp_servers:
  - name: remediation
    url: http://127.0.0.1:8100/mcp
    auth_header: {env: SERVICE_MCP_SECRET}
metrics_sources:
  - name: prometheus
    type: prometheus
    url: https://prometheus.example
    auth_header: {env: SERVICE_PROM_SECRET}
gated_tools: [run_host_command]
agent:
  max_rounds: 4
  call_timeout: 17s
  investigation_timeout: 3m
api_token: {env: SERVICE_API_SECRET}
`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func completedRunnerFactory(t *testing.T, answer string) RunnerFactory {
	t.Helper()
	registry, err := agent.NewRegistry(context.Background(), nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return func(events agent.EventSink, approver agent.Approver) *agent.Runner {
		if events != nil {
			events.Emit(store.EventToolCall, store.ToolCallPayload{Round: 1, Name: "fake", ResultSnippet: "result"})
		}
		return &agent.Runner{
			Tools: registry, Events: events, Approver: approver,
			Complete: func(context.Context, []openai.ChatCompletionMessageParamUnion, []openai.ChatCompletionToolUnionParam) (agent.ModelMessage, error) {
				return agent.ModelMessage{Content: answer}, nil
			},
		}
	}
}

func approvalRunnerFactory(t *testing.T) RunnerFactory {
	t.Helper()
	registry, err := agent.NewRegistry(context.Background(), []agent.MCPServer{{
		Name: "fake", Connect: func(context.Context) (agent.MCPSession, error) { return approvalSession{}, nil },
	}}, map[string]bool{"repair": true}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = registry.Close() })
	return func(events agent.EventSink, approver agent.Approver) *agent.Runner {
		completion := 0
		return &agent.Runner{
			Tools: registry, Events: events, Approver: approver,
			Complete: func(context.Context, []openai.ChatCompletionMessageParamUnion, []openai.ChatCompletionToolUnionParam) (agent.ModelMessage, error) {
				completion++
				if completion == 1 {
					return agent.ModelMessage{ToolCalls: []agent.ModelToolCall{{Name: "repair", Arguments: `{}`, ID: "call"}}}, nil
				}
				return agent.ModelMessage{Content: "done"}, nil
			},
		}
	}
}

func TestCreateInvestigationRunsAndRecordsTerminalEvent(t *testing.T) {
	audit, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	service := New(context.Background(), audit, testConfig(t), completedRunnerFactory(t, "answer"))
	summary, err := service.CreateInvestigation(context.Background(), "question", "api")
	if err != nil {
		t.Fatal(err)
	}
	if summary.Status != "running" || summary.Question != "question" || summary.Trigger != "api" {
		t.Fatalf("initial summary = %+v", summary)
	}
	service.Wait()
	summary, events, err := service.GetInvestigation(summary.ID)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Status != "completed" || summary.Answer != "answer" || summary.EndedAt.IsZero() {
		t.Fatalf("completed summary = %+v", summary)
	}
	want := []string{store.EventCreated, store.EventToolCall, store.EventCompleted}
	if len(events) != len(want) {
		t.Fatalf("event count = %d, want %d", len(events), len(want))
	}
	for i, eventType := range want {
		if events[i].Type != eventType {
			t.Fatalf("event %d type = %q, want %q", i, events[i].Type, eventType)
		}
	}
}

func TestApprovalDecisionsAreRoutedExactlyOnce(t *testing.T) {
	service := New(context.Background(), nil, nil, nil)
	type result struct {
		investigationID string
		decision        agent.Decision
		err             error
	}
	results := make(chan result, 2)
	for _, investigationID := range []string{"first", "second"} {
		investigationID := investigationID
		go func() {
			decision, err := (&investigationApprover{service: service, investigationID: investigationID}).RequestApproval(
				context.Background(), agent.ToolCall{ID: "shared-model-call-id", Name: "repair", Human: investigationID},
			)
			results <- result{investigationID, decision, err}
		}()
	}
	var pending []PendingApproval
	deadline := time.Now().Add(time.Second)
	for len(pending) != 2 && time.Now().Before(deadline) {
		pending = service.ListPendingApprovals()
		time.Sleep(time.Millisecond)
	}
	if len(pending) != 2 || pending[0].ID == pending[1].ID {
		t.Fatalf("pending approvals = %+v", pending)
	}
	ids := make(map[string]string)
	for _, approval := range pending {
		if approval.CallID != "shared-model-call-id" {
			t.Fatalf("call ID = %q", approval.CallID)
		}
		ids[approval.InvestigationID] = approval.ID
	}
	if err := service.DecideApproval(ids["first"], false, "api"); err != nil {
		t.Fatal(err)
	}
	if err := service.DecideApproval(ids["second"], true, "telegram"); err != nil {
		t.Fatal(err)
	}
	verdicts := make(map[string]bool)
	for range 2 {
		result := <-results
		if result.err != nil {
			t.Fatal(result.err)
		}
		verdicts[result.investigationID] = result.decision.Approved
		wantVia := "api"
		if result.investigationID == "second" {
			wantVia = "telegram"
		}
		if result.decision.Via != wantVia {
			t.Fatalf("%s decision via = %q, want %q", result.investigationID, result.decision.Via, wantVia)
		}
	}
	if verdicts["first"] || !verdicts["second"] {
		t.Fatalf("verdicts = %+v", verdicts)
	}
	if err := service.DecideApproval(ids["first"], true, "api"); !errors.Is(err, ErrApprovalAlreadyDecided) {
		t.Fatalf("second decision error = %v", err)
	}

	handler := NewHTTP(service, "token")
	request := httptest.NewRequest(http.MethodPost, "/v1/approvals/"+ids["first"]+"/decision", strings.NewReader(`{"approved":true}`))
	request.Header.Set("Authorization", "Bearer token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusConflict {
		t.Fatalf("second HTTP decision status = %d, body = %s", response.Code, response.Body.String())
	}
	request = httptest.NewRequest(http.MethodPost, "/v1/approvals/unknown/decision", strings.NewReader(`{"approved":true}`))
	request.Header.Set("Authorization", "Bearer token")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("unknown HTTP decision status = %d, body = %s", response.Code, response.Body.String())
	}
}

func TestEveryRouteRequiresBearerToken(t *testing.T) {
	service := New(context.Background(), nil, nil, nil)
	handler := NewHTTP(service, "correct-token")
	routes := []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodPost, "/v1/investigations", `{"question":"q"}`},
		{http.MethodGet, "/v1/investigations", ""},
		{http.MethodGet, "/v1/investigations/missing", ""},
		{http.MethodGet, "/v1/investigations/missing/events", ""},
		{http.MethodPost, "/v1/alerts", `{"alerts":[]}`},
		{http.MethodGet, "/v1/approvals", ""},
		{http.MethodGet, "/v1/approvals/events", ""},
		{http.MethodPost, "/v1/approvals/missing/decision", `{"approved":true}`},
		{http.MethodGet, "/v1/audit", ""},
		{http.MethodGet, "/metrics", ""},
		{http.MethodGet, "/v1/config", ""},
		{http.MethodGet, "/v1/health", ""},
	}
	for _, route := range routes {
		for _, authorization := range []string{"", "Bearer wrong-token"} {
			request := httptest.NewRequest(route.method, route.path, strings.NewReader(route.body))
			if authorization != "" {
				request.Header.Set("Authorization", authorization)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusUnauthorized {
				t.Fatalf("%s %s with %q returned %d", route.method, route.path, authorization, response.Code)
			}
		}
	}
}

func TestHTTPApprovalDecisionRecordsAPIVia(t *testing.T) {
	audit, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	service := New(context.Background(), audit, testConfig(t), approvalRunnerFactory(t))
	summary, err := service.CreateInvestigation(context.Background(), "repair", "api")
	if err != nil {
		t.Fatal(err)
	}
	var pending []PendingApproval
	deadline := time.Now().Add(time.Second)
	for len(pending) != 1 && time.Now().Before(deadline) {
		pending = service.ListPendingApprovals()
		time.Sleep(time.Millisecond)
	}
	if len(pending) != 1 {
		t.Fatalf("pending approvals = %+v", pending)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/approvals/"+pending[0].ID+"/decision", strings.NewReader(`{"approved":true,"via":"telegram"}`))
	request.Header.Set("Authorization", "Bearer token")
	response := httptest.NewRecorder()
	NewHTTP(service, "token").ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || len(service.ListPendingApprovals()) != 1 {
		t.Fatalf("client-supplied via status = %d, body = %s", response.Code, response.Body.String())
	}
	request = httptest.NewRequest(http.MethodPost, "/v1/approvals/"+pending[0].ID+"/decision", strings.NewReader(`{"approved":true}`))
	request.Header.Set("Authorization", "Bearer token")
	response = httptest.NewRecorder()
	NewHTTP(service, "token").ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("decision status = %d, body = %s", response.Code, response.Body.String())
	}
	service.Wait()
	_, events, err := service.GetInvestigation(summary.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type != store.EventApprovalDecided {
			continue
		}
		var payload store.ApprovalDecidedPayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if !payload.Approved || payload.Via != "api" {
			t.Fatalf("approval decision = %+v", payload)
		}
		return
	}
	t.Fatal("approval_decided event not recorded")
}

func TestConfigHTTPResponseContainsRefsAndNoSecretValues(t *testing.T) {
	cfg := testConfig(t)
	service := New(context.Background(), nil, cfg, nil)
	request := httptest.NewRequest(http.MethodGet, "/v1/config", nil)
	request.Header.Set("Authorization", "Bearer token")
	response := httptest.NewRecorder()
	NewHTTP(service, "token").ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	for _, secret := range []string{"known-model-secret", "known-mcp-secret", "known-prometheus-secret", "known-api-secret"} {
		if strings.Contains(body, secret) {
			t.Fatalf("config response leaked %q: %s", secret, body)
		}
	}
	for _, ref := range []string{"SERVICE_MODEL_SECRET", "SERVICE_MCP_SECRET", "SERVICE_PROM_SECRET", "SERVICE_API_SECRET"} {
		if !strings.Contains(body, ref) {
			t.Fatalf("config response omitted ref %q: %s", ref, body)
		}
	}
}

func TestHTTPCreateGetListRoundTrip(t *testing.T) {
	audit, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	service := New(context.Background(), audit, testConfig(t), completedRunnerFactory(t, "round-trip answer"))
	handler := NewHTTP(service, "token")
	request := httptest.NewRequest(http.MethodPost, "/v1/investigations", strings.NewReader(`{"question":"round trip"}`))
	request.Header.Set("Authorization", "Bearer token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", response.Code, response.Body.String())
	}
	var created store.Summary
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Status != "running" || created.Trigger != "api" {
		t.Fatalf("created summary = %+v", created)
	}
	service.Wait()

	request = httptest.NewRequest(http.MethodGet, "/v1/investigations/"+created.ID, nil)
	request.Header.Set("Authorization", "Bearer token")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !bytes.Contains(response.Body.Bytes(), []byte("round-trip answer")) {
		t.Fatalf("get status = %d, body = %s", response.Code, response.Body.String())
	}
	request = httptest.NewRequest(http.MethodGet, "/v1/investigations", nil)
	request.Header.Set("Authorization", "Bearer token")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	var listed []store.Summary
	if err := json.Unmarshal(response.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].ID != created.ID || listed[0].Status != "completed" {
		t.Fatalf("listed summaries = %+v", listed)
	}
}

func TestSSEFlushesLiveEventBeforeTerminal(t *testing.T) {
	audit, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	investigation, err := audit.Begin("question", "api")
	if err != nil {
		t.Fatal(err)
	}
	defer investigation.Close()
	service := New(context.Background(), audit, nil, nil)
	server := httptest.NewServer(NewHTTP(service, "token"))
	defer server.Close()
	request, err := http.NewRequest(http.MethodGet, server.URL+"/v1/investigations/"+investigation.ID+"/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer token")
	client := server.Client()
	client.Timeout = 2 * time.Second
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	events := make(chan string)
	scanErrors := make(chan error, 1)
	go scanSSE(response.Body, events, scanErrors)
	waitForSSE(t, events, scanErrors, store.EventCreated)
	if err := investigation.Append(store.EventToolCall, store.ToolCallPayload{Round: 1, Name: "live"}); err != nil {
		t.Fatal(err)
	}
	waitForSSE(t, events, scanErrors, store.EventToolCall)
	if err := investigation.Append(store.EventCompleted, store.CompletedPayload{Answer: "done"}); err != nil {
		t.Fatal(err)
	}
	waitForSSE(t, events, scanErrors, store.EventCompleted)
}

func TestApprovalSSEFlushesRequestedAndSettled(t *testing.T) {
	service := New(context.Background(), nil, nil, nil)
	server := httptest.NewServer(NewHTTP(service, "token"))
	defer server.Close()
	request, err := http.NewRequest(http.MethodGet, server.URL+"/v1/approvals/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer token")
	client := server.Client()
	client.Timeout = 2 * time.Second
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	events := make(chan string)
	scanErrors := make(chan error, 1)
	go scanSSE(response.Body, events, scanErrors)
	result := make(chan agent.Decision, 1)
	go func() {
		decision, _ := (&investigationApprover{service: service, investigationID: "investigation"}).RequestApproval(
			context.Background(), agent.ToolCall{ID: "call", Name: "repair", Human: "repair"},
		)
		result <- decision
	}()
	waitForSSE(t, events, scanErrors, "requested")
	pending := service.ListPendingApprovals()
	if len(pending) != 1 {
		t.Fatalf("pending approvals = %+v", pending)
	}
	if err := service.DecideApproval(pending[0].ID, false, "api"); err != nil {
		t.Fatal(err)
	}
	waitForSSE(t, events, scanErrors, "settled")
	if decision := <-result; decision.Approved || decision.Via != "api" {
		t.Fatalf("decision = %+v", decision)
	}
}

func TestApprovalCancellationPublishesTimeoutSettlement(t *testing.T) {
	service := New(context.Background(), nil, nil, nil)
	events, unsubscribe := service.SubscribeApprovals()
	defer unsubscribe()
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := (&investigationApprover{service: service, investigationID: "investigation"}).RequestApproval(
			ctx, agent.ToolCall{ID: "call", Name: "repair", Human: "repair"},
		)
		result <- err
	}()
	requested := <-events
	if requested.Type != "requested" {
		t.Fatalf("first approval event = %+v", requested)
	}
	cancel()
	settled := <-events
	if settled.Type != "settled" || settled.Approved || settled.Via != "timeout" || settled.Approval.ID != requested.Approval.ID {
		t.Fatalf("settled approval event = %+v", settled)
	}
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("approval error = %v", err)
	}
}

func scanSSE(reader io.Reader, events chan<- string, scanErrors chan<- error) {
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		if eventType, ok := strings.CutPrefix(scanner.Text(), "event: "); ok {
			events <- eventType
		}
	}
	scanErrors <- scanner.Err()
}

func waitForSSE(t *testing.T, events <-chan string, scanErrors <-chan error, want string) {
	t.Helper()
	select {
	case eventType := <-events:
		if eventType != want {
			t.Fatalf("SSE event = %q, want %q", eventType, want)
		}
	case err := <-scanErrors:
		t.Fatalf("SSE stream ended before %q: %v", want, err)
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for SSE event %q", want)
	}
}

func blockingRunnerFactory(t *testing.T) RunnerFactory {
	t.Helper()
	registry, err := agent.NewRegistry(context.Background(), nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = registry.Close() })
	return func(events agent.EventSink, approver agent.Approver) *agent.Runner {
		return &agent.Runner{
			Tools: registry, Events: events, Approver: approver,
			Complete: func(ctx context.Context, _ []openai.ChatCompletionMessageParamUnion, _ []openai.ChatCompletionToolUnionParam) (agent.ModelMessage, error) {
				<-ctx.Done()
				return agent.ModelMessage{}, ctx.Err()
			},
		}
	}
}

func TestIngestAlertsFiltersAndDedupes(t *testing.T) {
	audit, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	service := New(ctx, audit, nil, blockingRunnerFactory(t))
	t.Cleanup(func() { cancel(); service.Wait() })
	alerts := []Alert{
		{Status: "firing", Fingerprint: "aaa", Labels: map[string]string{"alertname": "HostDown", "instance": "nas"}, Annotations: map[string]string{"summary": "nas is down"}},
		{Status: "firing", Fingerprint: "aaa"},
		{Status: "resolved", Fingerprint: "bbb"},
		{Status: "firing", Fingerprint: ""},
		{Status: "firing", Fingerprint: "ccc", Labels: map[string]string{"alertname": "DiskFull"}},
	}
	created, skipped, err := service.IngestAlerts(ctx, alerts)
	if err != nil {
		t.Fatal(err)
	}
	if created != 2 || skipped != 3 {
		t.Fatalf("created=%d skipped=%d", created, skipped)
	}
	triggers := make(map[string]string)
	for _, summary := range service.ListInvestigations() {
		triggers[summary.Trigger] = summary.Question
	}
	if len(triggers) != 2 {
		t.Fatalf("investigations = %v", triggers)
	}
	question := triggers["alert:aaa"]
	for _, want := range []string{"HostDown", "instance: nas", "summary: nas is down", "Investigate the cause"} {
		if !strings.Contains(question, want) {
			t.Fatalf("question %q missing %q", question, want)
		}
	}
	created, skipped, err = service.IngestAlerts(ctx, alerts[:1])
	if err != nil || created != 0 || skipped != 1 {
		t.Fatalf("re-ingest while running: created=%d skipped=%d err=%v", created, skipped, err)
	}
}

func TestHTTPIngestAlertsAcceptsRealPayloadShape(t *testing.T) {
	audit, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	service := New(ctx, audit, nil, blockingRunnerFactory(t))
	t.Cleanup(func() { cancel(); service.Wait() })
	handler := NewHTTP(service, "token")
	payload := `{"version":"4","groupKey":"{}:{alertname=\"HostDown\"}","truncatedAlerts":0,"status":"firing","receiver":"shackleton","externalURL":"http://alertmanager.example","alerts":[{"status":"firing","fingerprint":"deadbeef","startsAt":"2026-08-19T20:00:00Z","endsAt":"0001-01-01T00:00:00Z","generatorURL":"http://prom.example","labels":{"alertname":"HostDown"},"annotations":{"summary":"down"}}]}`
	request := httptest.NewRequest(http.MethodPost, "/v1/alerts", strings.NewReader(payload))
	request.Header.Set("Authorization", "Bearer token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted || !strings.Contains(response.Body.String(), `"created":1`) {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	request = httptest.NewRequest(http.MethodPost, "/v1/alerts", strings.NewReader("{"))
	request.Header.Set("Authorization", "Bearer token")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("malformed payload status = %d", response.Code)
	}
}

func TestInvalidInvestigationIDReturns400(t *testing.T) {
	audit, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	handler := NewHTTP(New(context.Background(), audit, nil, nil), "token")
	for _, path := range []string{"/v1/investigations/..%2Fescape", "/v1/investigations/..%2Fescape/events"} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.Header.Set("Authorization", "Bearer token")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("%s status = %d, body = %s", path, response.Code, response.Body.String())
		}
	}
}

func TestDecidedTombstonesAreBounded(t *testing.T) {
	service := New(context.Background(), nil, nil, nil)
	var first, last string
	for i := 0; i <= decidedCap; i++ {
		pending, err := service.addPending("inv", agent.ToolCall{ID: "call", Name: "repair"})
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = pending.ID
		}
		last = pending.ID
		if err := service.DecideApproval(pending.ID, true, "test"); err != nil {
			t.Fatal(err)
		}
	}
	if err := service.DecideApproval(first, true, "test"); !errors.Is(err, ErrApprovalNotFound) {
		t.Fatalf("evicted tombstone error = %v", err)
	}
	if err := service.DecideApproval(last, true, "test"); !errors.Is(err, ErrApprovalAlreadyDecided) {
		t.Fatalf("recent tombstone error = %v", err)
	}
}

func TestAuditTrailProjectsMutatingEventsNewestFirst(t *testing.T) {
	audit, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	first, err := audit.Begin("q1", "api")
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Append(store.EventToolCall, store.ToolCallPayload{Round: 1, Name: "lookup"}); err != nil {
		t.Fatal(err)
	}
	if err := first.Append(store.EventApprovalRequested, store.ApprovalRequestedPayload{CallID: "c1", Name: "repair", Human: "fix"}); err != nil {
		t.Fatal(err)
	}
	if err := first.Append(store.EventApprovalDecided, store.ApprovalDecidedPayload{CallID: "c1", Approved: true, Via: "api"}); err != nil {
		t.Fatal(err)
	}
	if err := first.Append(store.EventCompleted, store.CompletedPayload{Answer: "done"}); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := audit.Begin("q2", "telegram")
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	service := New(context.Background(), audit, nil, nil)
	entries, err := service.AuditTrail()
	if err != nil {
		t.Fatal(err)
	}
	types := make([]string, 0, len(entries))
	for _, entry := range entries {
		types = append(types, entry.Type)
	}
	want := []string{store.EventCreated, store.EventApprovalDecided, store.EventApprovalRequested, store.EventCreated}
	if len(types) != len(want) {
		t.Fatalf("audit types = %v", types)
	}
	for i := range want {
		if types[i] != want[i] {
			t.Fatalf("audit types = %v, want %v", types, want)
		}
	}
	if entries[0].InvestigationID != second.ID {
		t.Fatalf("newest entry from %s, want %s", entries[0].InvestigationID, second.ID)
	}
}

func TestInvestigationMetricsUseTriggerClass(t *testing.T) {
	audit, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	service := New(context.Background(), audit, testConfig(t), completedRunnerFactory(t, "answer"))
	before := testutil.ToFloat64(investigationsTotal.WithLabelValues("alert", "completed"))
	if _, err := service.CreateInvestigation(context.Background(), "q", "alert:deadbeef99"); err != nil {
		t.Fatal(err)
	}
	service.Wait()
	if got := testutil.ToFloat64(investigationsTotal.WithLabelValues("alert", "completed")) - before; got != 1 {
		t.Fatalf("alert/completed delta = %v, want 1", got)
	}
}

func TestApprovalDecisionMetricByViaAndOutcome(t *testing.T) {
	service := New(context.Background(), nil, nil, nil)
	pending, err := service.addPending("inv", agent.ToolCall{ID: "c", Name: "repair"})
	if err != nil {
		t.Fatal(err)
	}
	before := testutil.ToFloat64(approvalDecisions.WithLabelValues("telegram", "false"))
	if err := service.DecideApproval(pending.ID, false, "telegram"); err != nil {
		t.Fatal(err)
	}
	if got := testutil.ToFloat64(approvalDecisions.WithLabelValues("telegram", "false")) - before; got != 1 {
		t.Fatalf("telegram/false delta = %v, want 1", got)
	}
}

type recordingNotifier struct {
	mu   sync.Mutex
	sent []string
}

func (n *recordingNotifier) Send(_ context.Context, text string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.sent = append(n.sent, text)
	return nil
}

func (n *recordingNotifier) messages() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string{}, n.sent...)
}

func TestOutcomeNotificationRouting(t *testing.T) {
	answer := func(verdict string) string {
		return "analysis prose\n```json\n{\"verdict\":\"" + verdict + "\",\"summary\":\"the summary\",\"evidence\":[\"item one\"]}\n```\n"
	}
	cases := []struct {
		name    string
		trigger string
		answer  string
		runErr  error
		want    int
	}{
		{"alert action notifies", "alert:abc", answer("action"), nil, 1},
		{"alert healthy silent", "alert:abc", answer("healthy"), nil, 0},
		{"alert no verdict notifies", "alert:abc", "prose only", nil, 1},
		{"api attention notifies", "api", answer("attention"), nil, 1},
		{"telegram excluded", "telegram", answer("action"), nil, 0},
		{"sweep excluded", "sweep:node-fs", answer("action"), nil, 0},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			audit, err := store.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			svc := New(context.Background(), audit, nil, func(events agent.EventSink, approver agent.Approver) *agent.Runner {
				return &agent.Runner{Complete: func(context.Context, []openai.ChatCompletionMessageParamUnion, []openai.ChatCompletionToolUnionParam) (agent.ModelMessage, error) {
					return agent.ModelMessage{Content: test.answer}, nil
				}, Tools: emptyRegistry(t)}
			})
			notifier := &recordingNotifier{}
			svc.Notifier = notifier
			if _, err := svc.CreateInvestigation(context.Background(), "Alertmanager alert firing: TestAlert.\ndetails", test.trigger); err != nil {
				t.Fatal(err)
			}
			svc.Wait()
			got := notifier.messages()
			if len(got) != test.want {
				t.Fatalf("notifications = %q, want %d", got, test.want)
			}
			if test.want == 1 {
				if !strings.Contains(got[0], "Alertmanager alert firing: TestAlert.") || !strings.Contains(got[0], "(20") {
					t.Fatalf("notification missing headline or id: %q", got[0])
				}
				if test.answer != "prose only" && !strings.Contains(got[0], "the summary") {
					t.Fatalf("notification missing summary: %q", got[0])
				}
			}
		})
	}
}

func emptyRegistry(t *testing.T) *agent.Registry {
	t.Helper()
	registry, err := agent.NewRegistry(context.Background(), nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = registry.Close() })
	return registry
}

func TestResolutionRecordedToKB(t *testing.T) {
	answer := "found it\n```json\n{\"verdict\":\"action\",\"summary\":\"csv stuck\",\"evidence\":[\"phase Pending\"]}\n```\n"
	audit, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc := New(context.Background(), audit, nil, func(events agent.EventSink, approver agent.Approver) *agent.Runner {
		return &agent.Runner{Complete: func(context.Context, []openai.ChatCompletionMessageParamUnion, []openai.ChatCompletionToolUnionParam) (agent.ModelMessage, error) {
			return agent.ModelMessage{Content: answer}, nil
		}, Tools: emptyRegistry(t)}
	})
	kbStore, err := kb.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc.KB = kbStore
	if _, err := svc.CreateInvestigation(context.Background(), "Alertmanager alert firing: CsvAbnormal.\nLabels:", "alert:fp123"); err != nil {
		t.Fatal(err)
	}
	svc.Wait()
	articles, err := kbStore.List()
	if err != nil || len(articles) != 1 {
		t.Fatalf("articles = %+v, %v", articles, err)
	}
	got := articles[0]
	if got.Slug != "alert-csvabnormal" || got.Symptom.Alertname != "CsvAbnormal" || got.Verdict != "action" ||
		got.Symptom.Fingerprints[0] != "fp123" || got.Status != "draft" {
		t.Fatalf("front-matter = %+v", got)
	}
	raw, err := kbStore.Get("alert-csvabnormal")
	if err != nil {
		t.Fatal(err)
	}
	content := string(raw)
	for _, want := range []string{"## Root cause", "found it", "No remediation applied", "action: csv stuck", "- phase Pending"} {
		if !strings.Contains(content, want) {
			t.Fatalf("article missing %q:\n%s", want, content)
		}
	}
	if strings.Contains(content, "```json") {
		t.Fatalf("verdict block should be stripped from root cause: %s", content)
	}
	// A healthy investigation with no actions must not create an article.
	answer = "fine\n```json\n{\"verdict\":\"healthy\",\"summary\":\"ok\",\"evidence\":[]}\n```\n"
	if _, err := svc.CreateInvestigation(context.Background(), "Alertmanager alert firing: OtherAlert.\n", "alert:fp999"); err != nil {
		t.Fatal(err)
	}
	svc.Wait()
	articles, _ = kbStore.List()
	if len(articles) != 1 {
		t.Fatalf("healthy investigation created an article: %+v", articles)
	}
}

func TestRecurrenceContextInjected(t *testing.T) {
	answer := "seen it\n```json\n{\"verdict\":\"attention\",\"summary\":\"csv is stuck\",\"evidence\":[]}\n```\n"
	audit, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc := New(context.Background(), audit, nil, func(events agent.EventSink, approver agent.Approver) *agent.Runner {
		return &agent.Runner{Complete: func(context.Context, []openai.ChatCompletionMessageParamUnion, []openai.ChatCompletionToolUnionParam) (agent.ModelMessage, error) {
			return agent.ModelMessage{Content: answer}, nil
		}, Tools: emptyRegistry(t)}
	})
	kbStore, err := kb.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc.KB = kbStore
	alert := Alert{Status: "firing", Fingerprint: "fp1", Labels: map[string]string{"alertname": "StuckCsv"}}
	if _, _, err := svc.IngestAlerts(context.Background(), []Alert{alert}); err != nil {
		t.Fatal(err)
	}
	svc.Wait()
	first := svc.ListInvestigations()
	if len(first) != 1 || strings.Contains(first[0].Question, "Prior history") {
		t.Fatalf("first occurrence should have no history: %+v", first)
	}
	alert.Fingerprint = "fp2"
	if _, _, err := svc.IngestAlerts(context.Background(), []Alert{alert}); err != nil {
		t.Fatal(err)
	}
	svc.Wait()
	var second store.Summary
	for _, summary := range svc.ListInvestigations() {
		if summary.Trigger == "alert:fp2" {
			second = summary
		}
	}
	for _, want := range []string{"Prior history: this alert has been investigated 1 time(s)",
		"verdict attention: csv is stuck",
		"knowledge-base article exists for this symptom (alert-stuckcsv, status draft)",
		"verify the current state independently"} {
		if !strings.Contains(second.Question, want) {
			t.Fatalf("question missing %q:\n%s", want, second.Question)
		}
	}
}
