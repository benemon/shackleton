package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/benemon/shackleton/internal/kb"
	"github.com/benemon/shackleton/internal/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type bearerTransport struct{ token string }

func (t bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req.Header.Set("Authorization", "Bearer "+t.token)
	return http.DefaultTransport.RoundTrip(req)
}

func mcpSession(t *testing.T, url, token string) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint:   url,
		HTTPClient: &http.Client{Transport: bearerTransport{token}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func decodeStructured(t *testing.T, result *mcp.CallToolResult, target any) {
	t.Helper()
	encoded, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, target); err != nil {
		t.Fatal(err)
	}
}

func TestMCPRequiresBearerToken(t *testing.T) {
	svc := New(context.Background(), nil, nil, nil)
	server := httptest.NewServer(NewMCP(svc, "correct-token"))
	t.Cleanup(server.Close)
	response, err := http.Post(server.URL, "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated MCP request returned %d", response.StatusCode)
	}
}

func TestMCPExposesAskAndReadToolsOnly(t *testing.T) {
	svc := New(context.Background(), nil, nil, nil)
	server := httptest.NewServer(NewMCP(svc, "token"))
	t.Cleanup(server.Close)
	session := mcpSession(t, server.URL, "token")
	listed, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(listed.Tools))
	for _, tool := range listed.Tools {
		names = append(names, tool.Name)
	}
	sort.Strings(names)
	want := []string{"get_investigation", "investigate", "read_kb", "search_kb", "wait_for_verdict"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("tool list = %v, want %v", names, want)
	}
}

func TestMCPInvestigateAndWaitForVerdict(t *testing.T) {
	audit, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	answer := "all clear\n```json\n{\"verdict\":\"healthy\",\"summary\":\"nothing wrong\",\"evidence\":[\"checked\"]}\n```"
	svc := New(context.Background(), audit, testConfig(t), completedRunnerFactory(t, answer))
	server := httptest.NewServer(NewMCP(svc, "token"))
	t.Cleanup(server.Close)
	session := mcpSession(t, server.URL, "token")

	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "investigate", Arguments: map[string]any{"question": "is the estate healthy?"},
	})
	if err != nil || result.IsError {
		t.Fatalf("investigate failed: %v %+v", err, result)
	}
	var created store.Summary
	decodeStructured(t, result, &created)
	if created.ID == "" || created.Trigger != "mcp" {
		t.Fatalf("created = %+v", created)
	}

	result, err = session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "wait_for_verdict", Arguments: map[string]any{"id": created.ID, "timeout_seconds": 30},
	})
	if err != nil || result.IsError {
		t.Fatalf("wait_for_verdict failed: %v %+v", err, result)
	}
	var completed store.Summary
	decodeStructured(t, result, &completed)
	if completed.Status != "completed" || completed.Verdict == nil || completed.Verdict.Verdict != "healthy" {
		t.Fatalf("completed = %+v", completed)
	}

	result, err = session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "get_investigation", Arguments: map[string]any{"id": "missing"},
	})
	if err != nil || !result.IsError {
		t.Fatalf("get_investigation(missing) = %v %+v", err, result)
	}
}

func TestMCPKBSearchAndRead(t *testing.T) {
	kbStore, err := kb.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := kbStore.Record(kb.Article{FrontMatter: kb.FrontMatter{
		Slug: "alert-diskfull", Title: "DiskFull (alert)", Verdict: "action",
		Symptom: kb.Symptom{Trigger: "alert", Alertname: "DiskFull"},
	}, Body: "# DiskFull\nfix it"}); err != nil {
		t.Fatal(err)
	}
	svc := New(context.Background(), nil, nil, nil)
	svc.KB = kbStore
	server := httptest.NewServer(NewMCP(svc, "token"))
	t.Cleanup(server.Close)
	session := mcpSession(t, server.URL, "token")

	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "search_kb", Arguments: map[string]any{"query": "diskfull"},
	})
	if err != nil || result.IsError {
		t.Fatalf("search_kb failed: %v %+v", err, result)
	}
	var search kbSearchOutput
	decodeStructured(t, result, &search)
	if len(search.Articles) != 1 || search.Articles[0].Slug != "alert-diskfull" {
		t.Fatalf("search = %+v", search)
	}

	result, err = session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "search_kb", Arguments: map[string]any{"query": "unrelated"},
	})
	if err != nil || result.IsError {
		t.Fatal(err)
	}
	decodeStructured(t, result, &search)
	if len(search.Articles) != 0 {
		t.Fatalf("search(unrelated) = %+v", search)
	}

	result, err = session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "read_kb", Arguments: map[string]any{"slug": "alert-diskfull"},
	})
	if err != nil || result.IsError {
		t.Fatalf("read_kb failed: %v %+v", err, result)
	}
	var article kbArticleOutput
	decodeStructured(t, result, &article)
	if !strings.Contains(article.Markdown, "fix it") {
		t.Fatalf("article = %+v", article)
	}
}
