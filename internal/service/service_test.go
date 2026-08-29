package service

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

type approvalSession struct{ result string }

func (approvalSession) ListTools(context.Context, *mcp.ListToolsParams) (*mcp.ListToolsResult, error) {
	return &mcp.ListToolsResult{Tools: []*mcp.Tool{{
		Name: "repair", Description: "repair", InputSchema: map[string]any{"type": "object"},
	}}}, nil
}

func (s approvalSession) CallTool(context.Context, *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s.result}}}, nil
}

func (approvalSession) Ping(context.Context, *mcp.PingParams) error { return nil }
func (approvalSession) Close() error                                { return nil }

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	t.Setenv("SERVICE_MODEL_SECRET", "known-model-secret")
	t.Setenv("SERVICE_MCP_SECRET", "known-mcp-secret")
	t.Setenv("SERVICE_PROM_SECRET", "known-prometheus-secret")
	t.Setenv("SERVICE_API_SECRET", "known-api-secret")
	t.Setenv("SERVICE_BOT_SECRET", "known-bot-secret")
	t.Setenv("SERVICE_CHAT_SECRET", "known-chat-secret")
	path := filepath.Join(t.TempDir(), "shackleton.yaml")
	contents := `
listen: "127.0.0.1:8420"
tls:
  cert_file: /etc/shackleton/server.pem
  key_file: /etc/shackleton/server.pem
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
notifications:
  - name: notifications
    type: telegram
    bot_token: {env: SERVICE_BOT_SECRET}
    chat_id: {env: SERVICE_CHAT_SECRET}
approvals:
  - name: approvals
    type: telegram
    bot_token: {env: SERVICE_BOT_SECRET}
    chat_id: {env: SERVICE_CHAT_SECRET}
gated_tools: [run_host_command]
agent:
  prompt: Test operator prompt
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
	registry, err := agent.NewRegistry(context.Background(), nil, nil, nil, nil, nil, nil)
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

func approvalRunnerFactory(t *testing.T, result string) RunnerFactory {
	t.Helper()
	registry, err := agent.NewRegistry(context.Background(), []agent.MCPServer{{
		Name: "fake", Connect: func(context.Context) (agent.MCPSession, error) { return approvalSession{result}, nil },
	}}, map[string]bool{"repair": true}, nil, nil, nil, nil)
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
					return agent.ModelMessage{ToolCalls: []agent.ModelToolCall{{Name: "repair", Arguments: `{"target":"node1"}`, ID: "call"}}}, nil
				}
				return agent.ModelMessage{Content: "done\n```json\n{\"verdict\":\"action\",\"summary\":\"csv stuck\",\"evidence\":[\"phase Pending\"]}\n```\n"}, nil
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

func TestMissingVerdictTriggersOneExtractionCall(t *testing.T) {
	audit, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	registry, err := agent.NewRegistry(context.Background(), nil, nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	factory := func(events agent.EventSink, approver agent.Approver) *agent.Runner {
		return &agent.Runner{
			Tools: registry, Events: events, Approver: approver,
			Complete: func(context.Context, []openai.ChatCompletionMessageParamUnion, []openai.ChatCompletionToolUnionParam) (agent.ModelMessage, error) {
				calls++
				if calls == 1 {
					return agent.ModelMessage{Content: "prose answer with no block"}, nil
				}
				// The extraction reply arrives without fencing; the wrap
				// fallback must still parse it.
				return agent.ModelMessage{Content: `{"verdict":"attention","summary":"s","evidence":["e"]}`}, nil
			},
		}
	}
	service := New(context.Background(), audit, testConfig(t), factory)
	summary, err := service.CreateInvestigation(context.Background(), "question", "api")
	if err != nil {
		t.Fatal(err)
	}
	service.Wait()
	summary, _, err = service.GetInvestigation(summary.ID)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("completion calls = %d, want run + one extraction", calls)
	}
	if summary.Verdict == nil || summary.Verdict.Verdict != "attention" {
		t.Fatalf("verdict = %+v, want extracted attention", summary.Verdict)
	}
	if summary.Answer != "prose answer with no block" {
		t.Fatalf("answer was rewritten: %q", summary.Answer)
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
		{http.MethodPost, "/v1/session", ""},
		{http.MethodPost, "/v1/investigations", `{"question":"q"}`},
		{http.MethodGet, "/v1/investigations", ""},
		{http.MethodGet, "/v1/investigations/missing", ""},
		{http.MethodGet, "/v1/investigations/missing/events", ""},
		{http.MethodPost, "/v1/alerts", `{"alerts":[]}`},
		{http.MethodGet, "/v1/approvals", ""},
		{http.MethodGet, "/v1/approvals/events", ""},
		{http.MethodPost, "/v1/approvals/missing/decision", `{"approved":true}`},
		{http.MethodGet, "/v1/audit", ""},
		{http.MethodGet, "/v1/inventory", ""},
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

func TestSessionCookieLifecycle(t *testing.T) {
	service := New(context.Background(), nil, nil, nil)
	handler := NewHTTP(service, "correct-token")

	request := httptest.NewRequest(http.MethodPost, "/v1/session", nil)
	request.Header.Set("Authorization", "Bearer correct-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("create session returned %d", response.Code)
	}
	cookies := response.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("create session set %d cookies", len(cookies))
	}
	session := cookies[0]
	if session.Name != "shackleton_session" || session.Value != "correct-token" {
		t.Fatalf("unexpected cookie %s=%s", session.Name, session.Value)
	}
	if !session.HttpOnly || session.SameSite != http.SameSiteStrictMode || session.Path != "/v1/session" {
		t.Fatalf("cookie attributes: httpOnly=%v sameSite=%v path=%s", session.HttpOnly, session.SameSite, session.Path)
	}
	if session.MaxAge != 0 {
		t.Fatalf("session cookie must die with the browser, got MaxAge %d", session.MaxAge)
	}

	request = httptest.NewRequest(http.MethodGet, "/v1/session", nil)
	request.AddCookie(session)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("read session returned %d", response.Code)
	}
	var body struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil || body.Token != "correct-token" {
		t.Fatalf("read session body %q err %v", body.Token, err)
	}

	request = httptest.NewRequest(http.MethodGet, "/v1/session", nil)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent || len(response.Result().Cookies()) != 0 {
		t.Fatalf("read without cookie returned %d with %d cookies", response.Code, len(response.Result().Cookies()))
	}

	request = httptest.NewRequest(http.MethodGet, "/v1/session", nil)
	request.AddCookie(&http.Cookie{Name: "shackleton_session", Value: "rotated-away"})
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("read with stale cookie returned %d", response.Code)
	}
	cleared := response.Result().Cookies()
	if len(cleared) != 1 || cleared[0].Value != "" || cleared[0].MaxAge != -1 {
		t.Fatalf("stale cookie was not cleared: %+v", cleared)
	}

	// Ending a session must never require a live credential.
	request = httptest.NewRequest(http.MethodDelete, "/v1/session", nil)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("delete session returned %d", response.Code)
	}
	cleared = response.Result().Cookies()
	if len(cleared) != 1 || cleared[0].Value != "" || cleared[0].MaxAge != -1 {
		t.Fatalf("delete did not clear the cookie: %+v", cleared)
	}
}

func TestInventoryHTTPProjection(t *testing.T) {
	dir := t.TempDir()
	contents := `
hosts:
  - name: nas
    hostname: nas.lab.example
clusters:
  - name: ocp
    api: https://api.ocp.lab.example:6443
    type: openshift
`
	if err := os.WriteFile(filepath.Join(dir, "lab.yaml"), []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(t)
	cfg.InventoryDir = dir
	service := New(context.Background(), nil, cfg, nil)
	request := httptest.NewRequest(http.MethodGet, "/v1/inventory", nil)
	request.Header.Set("Authorization", "Bearer token")
	response := httptest.NewRecorder()
	NewHTTP(service, "token").ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var got struct {
		Hosts []struct {
			Name       string `json:"name"`
			Hostname   string `json:"hostname"`
			Connection string `json:"connection"`
		} `json:"hosts"`
		Clusters []struct {
			Name string `json:"name"`
			API  string `json:"api"`
			Type string `json:"type"`
		} `json:"clusters"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Hosts) != 1 || got.Hosts[0].Name != "nas" || got.Hosts[0].Connection != "ssh" {
		t.Fatalf("hosts = %+v", got.Hosts)
	}
	if len(got.Clusters) != 1 || got.Clusters[0].Type != "openshift" {
		t.Fatalf("clusters = %+v", got.Clusters)
	}

	// The view reads the directory fresh: a draft written after startup
	// appears without a restart.
	if err := os.WriteFile(filepath.Join(dir, "drafts.yaml"), []byte("hosts:\n  - name: node1\n    status: draft\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	response = httptest.NewRecorder()
	NewHTTP(service, "token").ServeHTTP(response, request.Clone(context.Background()))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"status":"draft"`) {
		t.Fatalf("draft not visible in fresh view: %d %s", response.Code, response.Body.String())
	}

	empty := New(context.Background(), nil, nil, nil)
	response = httptest.NewRecorder()
	NewHTTP(empty, "token").ServeHTTP(response, request.Clone(context.Background()))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"hosts":[]`) {
		t.Fatalf("empty inventory projection: %d %s", response.Code, response.Body.String())
	}
}

func TestHTTPApprovalDecisionRecordsAPIVia(t *testing.T) {
	audit, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	service := New(context.Background(), audit, testConfig(t), approvalRunnerFactory(t, "repaired"))
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
	request := httptest.NewRequest(http.MethodGet, "/v1/approvals", nil)
	request.Header.Set("Authorization", "Bearer token")
	response := httptest.NewRecorder()
	NewHTTP(service, "token").ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("list status = %d, body = %s", response.Code, response.Body.String())
	}
	var listed []PendingApproval
	if err := json.Unmarshal(response.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].ArgsJSON != `{"target":"node1"}` {
		t.Fatalf("listed approvals = %+v", listed)
	}
	request = httptest.NewRequest(http.MethodPost, "/v1/approvals/"+pending[0].ID+"/decision", strings.NewReader(`{"approved":true,"via":"telegram"}`))
	request.Header.Set("Authorization", "Bearer token")
	response = httptest.NewRecorder()
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
	if !strings.Contains(body, `"tls":{"cert_file":"/etc/shackleton/server.pem","key_file":"/etc/shackleton/server.pem"}`) {
		t.Fatalf("config response omitted TLS paths: %s", body)
	}
	if !strings.Contains(body, `"prompt":"Test operator prompt"`) {
		t.Fatalf("config response omitted agent prompt: %s", body)
	}
	for _, secret := range []string{"known-model-secret", "known-mcp-secret", "known-prometheus-secret", "known-api-secret", "known-bot-secret", "known-chat-secret"} {
		if strings.Contains(body, secret) {
			t.Fatalf("config response leaked %q: %s", secret, body)
		}
	}
	for _, ref := range []string{"SERVICE_MODEL_SECRET", "SERVICE_MCP_SECRET", "SERVICE_PROM_SECRET", "SERVICE_API_SECRET", "SERVICE_BOT_SECRET", "SERVICE_CHAT_SECRET"} {
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
	if listed[0].Title != "round trip" {
		t.Fatalf("listed title = %q", listed[0].Title)
	}
}

func TestHTTPSaveInvestigationToKB(t *testing.T) {
	audit, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	service := New(context.Background(), audit, testConfig(t), completedRunnerFactory(t, "procedure"))
	service.KB, err = kb.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	created, err := service.CreateInvestigation(context.Background(), "Repair the stuck CSV", "api")
	if err != nil {
		t.Fatal(err)
	}
	service.Wait()
	handler := NewHTTP(service, "token")

	request := httptest.NewRequest(http.MethodPost, "/v1/investigations/"+created.ID+"/kb", nil)
	request.Header.Set("Authorization", "Bearer token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated || response.Body.String() != "{\"slug\":\"repair-the-stuck-csv\"}\n" {
		t.Fatalf("save status = %d, body = %s", response.Code, response.Body.String())
	}

	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusConflict {
		t.Fatalf("repeat status = %d, body = %s", response.Code, response.Body.String())
	}
	request = httptest.NewRequest(http.MethodPost, "/v1/investigations/missing/kb", nil)
	request.Header.Set("Authorization", "Bearer token")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("missing status = %d, body = %s", response.Code, response.Body.String())
	}
}

func TestCreateFollowUpThreadsPriorContext(t *testing.T) {
	audit, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	answer := "node2 /var is filling\n```json\n{\"verdict\":\"attention\",\"summary\":\"/var pressure\",\"evidence\":[]}\n```\n"
	svc := New(context.Background(), audit, nil, completedRunnerFactory(t, answer))
	prior, err := svc.CreateInvestigation(context.Background(), "What is using the space on /var?\nsecond line", "telegram")
	if err != nil {
		t.Fatal(err)
	}
	svc.Wait()
	followUp, err := svc.CreateFollowUp(context.Background(), "Run oc debug on node2", "telegram", prior.ID)
	if err != nil {
		t.Fatal(err)
	}
	svc.Wait()
	got, _, err := svc.GetInvestigation(followUp.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got.Question, "Run oc debug on node2\n") {
		t.Fatalf("follow-up question must lead with the reply: %q", got.Question)
	}
	for _, want := range []string{
		"(Follow-up to investigation " + prior.ID + ".)",
		"Prior question: What is using the space on /var?",
		"Prior answer:\nnode2 /var is filling",
		"verify current state yourself",
	} {
		if !strings.Contains(got.Question, want) {
			t.Fatalf("follow-up question missing %q:\n%s", want, got.Question)
		}
	}
	if strings.Contains(got.Question, "```json") {
		t.Fatalf("verdict block leaked into follow-up context: %q", got.Question)
	}
	if got.Title != "Run oc debug on node2" {
		t.Fatalf("title = %q", got.Title)
	}

	if _, err := svc.CreateFollowUp(context.Background(), "anything", "telegram", "20990101-000000-missing"); !errors.Is(err, ErrInvestigationNotFound) {
		t.Fatalf("unknown prior error = %v", err)
	}
}

func TestDisplayTitle(t *testing.T) {
	long := strings.Repeat("x", 100)
	for _, tc := range []struct {
		name, question, trigger, want string
	}{
		{"alert carries the alertname", "Alertmanager alert firing: OdfNodeMtuLessThan9000.\nLabels:\n  severity: warning", "alert:abc123", "OdfNodeMtuLessThan9000"},
		{"unparseable alert question falls back to the headline", "something else entirely\nsecond line", "alert:abc123", "something else entirely"},
		{"sweep carries the sweep name", "Check every filesystem on every host…", "sweep:node-fs", "node-fs"},
		{"question keeps its first line", "Is the cluster healthy?\nCheck operators too.", "telegram", "Is the cluster healthy?"},
		{"long question is bounded", long + "\nrest", "api", strings.Repeat("x", 80) + "…"},
	} {
		if got := displayTitle(tc.question, tc.trigger); got != tc.want {
			t.Errorf("%s: displayTitle = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestHTTPFollowUpCreateAndUnknownPrior(t *testing.T) {
	audit, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	service := New(context.Background(), audit, testConfig(t), completedRunnerFactory(t, "prior answer"))
	handler := NewHTTP(service, "token")
	request := httptest.NewRequest(http.MethodPost, "/v1/investigations", strings.NewReader(`{"question":"first"}`))
	request.Header.Set("Authorization", "Bearer token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	var created store.Summary
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	service.Wait()

	request = httptest.NewRequest(http.MethodPost, "/v1/investigations", strings.NewReader(`{"question":"follow","follow_up_to":"`+created.ID+`"}`))
	request.Header.Set("Authorization", "Bearer token")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("follow-up status = %d, body = %s", response.Code, response.Body.String())
	}
	var followUp store.Summary
	if err := json.Unmarshal(response.Body.Bytes(), &followUp); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(followUp.Question, "(Follow-up to investigation "+created.ID+".)") {
		t.Fatalf("follow-up question = %q", followUp.Question)
	}
	service.Wait()

	request = httptest.NewRequest(http.MethodPost, "/v1/investigations", strings.NewReader(`{"question":"x","follow_up_to":"20990101-000000-missing"}`))
	request.Header.Set("Authorization", "Bearer token")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("unknown prior status = %d, body = %s", response.Code, response.Body.String())
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
	registry, err := agent.NewRegistry(context.Background(), nil, nil, nil, nil, nil, nil)
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

func TestNotificationHeadlineIsBounded(t *testing.T) {
	audit, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc := New(context.Background(), audit, nil, func(events agent.EventSink, approver agent.Approver) *agent.Runner {
		return &agent.Runner{Complete: func(context.Context, []openai.ChatCompletionMessageParamUnion, []openai.ChatCompletionToolUnionParam) (agent.ModelMessage, error) {
			return agent.ModelMessage{Content: "prose\n```json\n{\"verdict\":\"attention\",\"summary\":\"s\",\"evidence\":[]}\n```"}, nil
		}, Tools: emptyRegistry(t)}
	})
	notifier := &recordingNotifier{}
	svc.Notifier = notifier
	long := strings.Repeat("investigate the estate ", 20)
	if _, err := svc.CreateInvestigation(context.Background(), long, "api"); err != nil {
		t.Fatal(err)
	}
	svc.Wait()
	got := notifier.messages()
	if len(got) != 1 {
		t.Fatalf("notifications = %d", len(got))
	}
	headline, _, _ := strings.Cut(got[0], "\n")
	if runes := []rune(headline); len(runes) != 140 || !strings.HasSuffix(headline, "…") {
		t.Fatalf("headline length = %d, tail %q", len([]rune(headline)), headline[len(headline)-12:])
	}
}

func emptyRegistry(t *testing.T) *agent.Registry {
	t.Helper()
	registry, err := agent.NewRegistry(context.Background(), nil, nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = registry.Close() })
	return registry
}

func TestResolutionRecordedToKB(t *testing.T) {
	audit, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc := New(context.Background(), audit, nil, approvalRunnerFactory(t, "repaired"))
	kbStore, err := kb.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc.KB = kbStore
	if _, err := svc.CreateInvestigation(context.Background(), "Alertmanager alert firing: CsvAbnormal.\nLabels:", "alert:fp123"); err != nil {
		t.Fatal(err)
	}
	var pending []PendingApproval
	for i := 0; i < 200 && len(pending) == 0; i++ {
		pending = svc.ListPendingApprovals()
		time.Sleep(5 * time.Millisecond)
	}
	if len(pending) != 1 {
		t.Fatalf("pending = %+v", pending)
	}
	if err := svc.DecideApproval(pending[0].ID, true, "test"); err != nil {
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
	for _, want := range []string{
		"## Issue\nAlert CsvAbnormal firing.",
		"## Diagnosis\nInvestigation ",
		"- phase Pending",
		"## Root cause\ncsv stuck",
		"## Resolution\n- Approved: ",
		"→ repaired",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("article missing %q:\n%s", want, content)
		}
	}
	// The KCS sections carry structured fields only: no answer text, no
	// verdict block, no triage question, no prompt boilerplate.
	for _, reject := range []string{"```json", "Alertmanager alert firing", "## Verdict", "## Environment", "No remediation applied"} {
		if strings.Contains(content, reject) {
			t.Fatalf("article should not contain %q:\n%s", reject, content)
		}
	}
	// A verdict without a remediation is an observation, not an article —
	// the investigations list is its record.
	audit2, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc2 := New(context.Background(), audit2, nil, completedRunnerFactory(t, "found it\n```json\n{\"verdict\":\"action\",\"summary\":\"csv stuck\",\"evidence\":[\"phase Pending\"]}\n```\n"))
	kbStore2, err := kb.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc2.KB = kbStore2
	if _, err := svc2.CreateInvestigation(context.Background(), "Alertmanager alert firing: OtherAlert.\n", "alert:fp999"); err != nil {
		t.Fatal(err)
	}
	svc2.Wait()
	if articles, _ := kbStore2.List(); len(articles) != 0 {
		t.Fatalf("verdict-only investigation created an article: %+v", articles)
	}
}

func TestSaveInvestigationToKB(t *testing.T) {
	audit, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	answer := "Restart the exporter.\n\nThen verify metrics.\n```json\n{\"verdict\":\"action\",\"summary\":\"the exporter was stuck\",\"evidence\":[\"scrape target timed out\"]}\n```\n"
	svc := New(context.Background(), audit, nil, completedRunnerFactory(t, answer))
	kbStore, err := kb.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc.KB = kbStore
	summary, err := svc.CreateInvestigation(context.Background(), "How do I repair the stuck exporter?\nInclude verification.", "telegram")
	if err != nil {
		t.Fatal(err)
	}
	svc.Wait()

	slug, err := svc.SaveInvestigationToKB(summary.ID)
	if err != nil {
		t.Fatal(err)
	}
	if slug != "how-do-i-repair-the-stuck-exporter" {
		t.Fatalf("slug = %q", slug)
	}
	articles, err := kbStore.List()
	if err != nil || len(articles) != 1 {
		t.Fatalf("articles = %+v, %v", articles, err)
	}
	front := articles[0]
	if front.Status != "draft" || front.Title != "How do I repair the stuck exporter?" || front.Symptom.Trigger != "telegram" || front.Verdict != "action" ||
		len(front.Occurrences) != 1 || front.Occurrences[0].Investigation != summary.ID {
		t.Fatalf("front-matter = %+v", front)
	}
	raw, err := kbStore.Get(slug)
	if err != nil {
		t.Fatal(err)
	}
	content := string(raw)
	for _, want := range []string{
		"# How do I repair the stuck exporter?",
		"## Issue\nHow do I repair the stuck exporter?",
		"## Diagnosis\nInvestigation " + summary.ID + ":\n- scrape target timed out",
		"## Root cause\nthe exporter was stuck",
		"## Resolution\nRestart the exporter.\n\nThen verify metrics.",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("article missing %q:\n%s", want, content)
		}
	}
	if strings.Contains(content, "```json") {
		t.Fatalf("article retained verdict block:\n%s", content)
	}
	if _, err := svc.SaveInvestigationToKB(summary.ID); !errors.Is(err, ErrArticleExists) {
		t.Fatalf("second save error = %v", err)
	}
	if _, err := svc.SaveInvestigationToKB("missing"); !errors.Is(err, ErrInvestigationNotFound) {
		t.Fatalf("missing save error = %v", err)
	}
	running, err := audit.Begin("Still investigating", "api")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = running.Close() })
	if _, err := svc.SaveInvestigationToKB(running.ID); !errors.Is(err, ErrNotCurateable) {
		t.Fatalf("running save error = %v", err)
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
	kbDir := t.TempDir()
	kbStore, err := kb.Open(kbDir)
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
	// Articles only exist for remediated symptoms; seed one as a fixture so
	// the draft-vs-approved citation rule stays exercised.
	if _, err := kbStore.Record(kb.Article{FrontMatter: kb.FrontMatter{
		Slug: "alert-stuckcsv", Title: "StuckCsv (alert)",
		Symptom: kb.Symptom{Trigger: "alert", Alertname: "StuckCsv"},
	}, Body: "# StuckCsv (alert)\n"}); err != nil {
		t.Fatal(err)
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
		"verify the current state independently"} {
		if !strings.Contains(second.Question, want) {
			t.Fatalf("question missing %q:\n%s", want, second.Question)
		}
	}
	if strings.Contains(second.Question, "knowledge-base article") {
		t.Fatalf("draft article must not feed resolution context:\n%s", second.Question)
	}
	// Operator approves the article; the next occurrence may cite it.
	raw, err := kbStore.Get("alert-stuckcsv")
	if err != nil {
		t.Fatal(err)
	}
	approved := strings.Replace(string(raw), "status: draft", "status: approved", 1)
	if err := os.WriteFile(filepath.Join(kbDir, "alert-stuckcsv.md"), []byte(approved), 0o644); err != nil {
		t.Fatal(err)
	}
	alert.Fingerprint = "fp3"
	if _, _, err := svc.IngestAlerts(context.Background(), []Alert{alert}); err != nil {
		t.Fatal(err)
	}
	svc.Wait()
	for _, summary := range svc.ListInvestigations() {
		if summary.Trigger == "alert:fp3" && !strings.Contains(summary.Question, "An approved knowledge-base article exists for this symptom (alert-stuckcsv)") {
			t.Fatalf("approved article not cited:\n%s", summary.Question)
		}
	}
}

func TestVerifiedResolutionsNominateDraftOnce(t *testing.T) {
	answer := "fixed it\n```json\n{\"verdict\":\"attention\",\"summary\":\"repaired\",\"evidence\":[\"before/after\"],\"resolution\":\"cleared\"}\n```\n"
	audit, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	registry, err := agent.NewRegistry(context.Background(), []agent.MCPServer{{
		Name: "fake", Connect: func(context.Context) (agent.MCPSession, error) { return approvalSession{}, nil },
	}}, map[string]bool{"repair": true}, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = registry.Close() })
	svc := New(context.Background(), audit, nil, func(events agent.EventSink, approver agent.Approver) *agent.Runner {
		completion := 0
		return &agent.Runner{
			Tools: registry, Events: events, Approver: approver,
			Complete: func(context.Context, []openai.ChatCompletionMessageParamUnion, []openai.ChatCompletionToolUnionParam) (agent.ModelMessage, error) {
				completion++
				if completion == 1 {
					return agent.ModelMessage{ToolCalls: []agent.ModelToolCall{{Name: "repair", Arguments: `{}`, ID: "call"}}}, nil
				}
				return agent.ModelMessage{Content: answer}, nil
			},
		}
	})
	kbStore, err := kb.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc.KB = kbStore
	notifier := &recordingNotifier{}
	svc.Notifier = notifier
	approveAll := func() {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			for _, pending := range svc.ListPendingApprovals() {
				_ = svc.DecideApproval(pending.ID, true, "api")
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	for i := 1; i <= 4; i++ {
		alert := Alert{Status: "firing", Fingerprint: fmt.Sprintf("fp%d", i), Labels: map[string]string{"alertname": "Repairable"}}
		if _, _, err := svc.IngestAlerts(context.Background(), []Alert{alert}); err != nil {
			t.Fatal(err)
		}
		go approveAll()
		svc.Wait()
	}
	articles, err := kbStore.List()
	if err != nil || len(articles) != 1 {
		t.Fatalf("articles = %+v, %v", articles, err)
	}
	got := articles[0]
	if got.ClearedCount() != 4 || got.Resolution.Verified != "cleared" || !got.Nominated || got.Status != "draft" {
		t.Fatalf("front-matter = %+v", got)
	}
	nominations := 0
	for _, message := range notifier.messages() {
		if strings.HasPrefix(message, "KB nomination:") {
			nominations++
			if !strings.Contains(message, got.Slug) {
				t.Fatalf("nomination missing slug: %q", message)
			}
		}
	}
	if nominations != 1 {
		t.Fatalf("nominations = %d, want exactly 1", nominations)
	}
}

func TestAdHocQuestionsRecordOnlyRemediations(t *testing.T) {
	answer := "warm\n```json\n{\"verdict\":\"attention\",\"summary\":\"node1 is warmest\",\"evidence\":[]}\n```\n"
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
	for _, trigger := range []string{"api", "mcp", "cli"} {
		if _, err := svc.CreateInvestigation(context.Background(), "What is the warmest kubernetes host right now?", trigger); err != nil {
			t.Fatal(err)
		}
	}
	svc.Wait()
	if articles, _ := kbStore.List(); len(articles) != 0 {
		t.Fatalf("current-state question became an article: %+v", articles)
	}

	// The same ad-hoc trigger records once an approved remediation ran.
	audit2, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc2 := New(context.Background(), audit2, nil, approvalRunnerFactory(t, "repaired"))
	kbStore2, err := kb.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc2.KB = kbStore2
	if _, err := svc2.CreateInvestigation(context.Background(), "Fix the stuck exporter on nas", "api"); err != nil {
		t.Fatal(err)
	}
	var pending []PendingApproval
	for i := 0; i < 200 && len(pending) == 0; i++ {
		pending = svc2.ListPendingApprovals()
		time.Sleep(5 * time.Millisecond)
	}
	if len(pending) != 1 {
		t.Fatalf("pending = %+v", pending)
	}
	if err := svc2.DecideApproval(pending[0].ID, true, "test"); err != nil {
		t.Fatal(err)
	}
	svc2.Wait()
	articles, err := kbStore2.List()
	if err != nil || len(articles) != 1 {
		t.Fatalf("remediated question did not record: %+v, %v", articles, err)
	}
	if !strings.HasPrefix(articles[0].Slug, "adhoc-") {
		t.Fatalf("slug = %q", articles[0].Slug)
	}

	// An approved call the executor refused is a probe, not a remediation.
	audit3, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc3 := New(context.Background(), audit3, nil, approvalRunnerFactory(t, "error: command 'auth' is not allowed"))
	kbStore3, err := kb.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc3.KB = kbStore3
	if _, err := svc3.CreateInvestigation(context.Background(), "SYNTHETIC: gated-session check", "api"); err != nil {
		t.Fatal(err)
	}
	pending = nil
	for i := 0; i < 200 && len(pending) == 0; i++ {
		pending = svc3.ListPendingApprovals()
		time.Sleep(5 * time.Millisecond)
	}
	if len(pending) != 1 {
		t.Fatalf("pending = %+v", pending)
	}
	if err := svc3.DecideApproval(pending[0].ID, true, "test"); err != nil {
		t.Fatal(err)
	}
	svc3.Wait()
	if articles, _ := kbStore3.List(); len(articles) != 0 {
		t.Fatalf("refused execution became an article: %+v", articles)
	}
}
