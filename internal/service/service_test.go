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
	"testing"
	"time"

	"github.com/benemon/shackleton/internal/agent"
	"github.com/benemon/shackleton/internal/config"
	"github.com/benemon/shackleton/internal/store"
	"github.com/openai/openai-go/v3"
)

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
prometheus:
  url: https://prometheus.example
  auth_header: {env: SERVICE_PROM_SECRET}
gated_tools: [run_host_command]
telegram:
  env_file: ""
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
	registry, err := agent.NewRegistry(context.Background(), nil, nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	return func(events agent.EventSink, approver agent.Approver) *agent.Runner {
		if events != nil {
			events.Emit(store.EventToolCall, store.ToolCallPayload{Round: 1, Name: "fake", ResultSnippet: "result"})
		}
		return &agent.Runner{
			Tools: registry, Events: events, Approver: approver, ApprovalVia: "api",
			Complete: func(context.Context, []openai.ChatCompletionMessageParamUnion, []openai.ChatCompletionToolUnionParam) (agent.ModelMessage, error) {
				return agent.ModelMessage{Content: answer}, nil
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
		approved        bool
		err             error
	}
	results := make(chan result, 2)
	for _, investigationID := range []string{"first", "second"} {
		investigationID := investigationID
		go func() {
			approved, err := (&investigationApprover{service: service, investigationID: investigationID}).RequestApproval(
				context.Background(), agent.ToolCall{ID: "shared-model-call-id", Name: "repair", Human: investigationID},
			)
			results <- result{investigationID, approved, err}
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
	if err := service.DecideApproval(ids["first"], false); err != nil {
		t.Fatal(err)
	}
	if err := service.DecideApproval(ids["second"], true); err != nil {
		t.Fatal(err)
	}
	verdicts := make(map[string]bool)
	for range 2 {
		result := <-results
		if result.err != nil {
			t.Fatal(result.err)
		}
		verdicts[result.investigationID] = result.approved
	}
	if verdicts["first"] || !verdicts["second"] {
		t.Fatalf("verdicts = %+v", verdicts)
	}
	if err := service.DecideApproval(ids["first"], true); !errors.Is(err, ErrApprovalAlreadyDecided) {
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
		{http.MethodGet, "/v1/approvals", ""},
		{http.MethodPost, "/v1/approvals/missing/decision", `{"approved":true}`},
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
