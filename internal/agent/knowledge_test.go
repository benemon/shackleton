package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGenericKnowledgeFetchEnforcesTheSiteAllowlist(t *testing.T) {
	docs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<html><head><style>.x{}</style><script>alert(1)</script></head><body><h1>ACL Bootstrap</h1><p>Run &amp; verify.</p></body></html>`)
	}))
	defer docs.Close()
	registry, err := NewRegistry(context.Background(), nil, nil, nil, nil,
		[]KnowledgeSource{{Name: "hashicorp", Type: "generic", Sites: []string{docs.URL}, Client: docs.Client()}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	if _, ok := registry.tools["search_hashicorp_docs"]; ok {
		t.Fatal("search tool registered without a search backend")
	}
	text, err := registry.tools["get_hashicorp_doc"].call(context.Background(), map[string]any{"url": docs.URL + "/consul/acl"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "ACL Bootstrap Run & verify.") {
		t.Fatalf("extracted text = %q", text)
	}
	if strings.Contains(text, "alert(1)") || strings.Contains(text, ".x{}") {
		t.Fatalf("script/style leaked: %q", text)
	}
	_, err = registry.tools["get_hashicorp_doc"].call(context.Background(), map[string]any{"url": "https://evil.example/consul"})
	if err == nil || !strings.Contains(err.Error(), "outside this source's sites") {
		t.Fatalf("off-list fetch error = %v", err)
	}
}

func TestGenericKnowledgeSearchScopesQueriesAndFiltersResults(t *testing.T) {
	docsSite := "https://developer.hashicorp.com"
	var seenQuery string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenQuery = r.URL.Query().Get("q")
		json.NewEncoder(w).Encode(map[string]any{"results": []map[string]string{
			{"url": docsSite + "/consul/acl", "title": "ACL system", "content": "bootstrap the acl system"},
			{"url": "https://blogspam.example/acl", "title": "spam", "content": "spam"},
		}})
	}))
	defer backend.Close()
	registry, err := NewRegistry(context.Background(), nil, nil, nil, nil,
		[]KnowledgeSource{{Name: "hashicorp", Type: "generic", Sites: []string{docsSite}, Client: backend.Client()}},
		&KnowledgeSearch{URL: backend.URL, Client: backend.Client()})
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	text, err := registry.tools["search_hashicorp_docs"].call(context.Background(), map[string]any{"query": "acl bootstrap"})
	if err != nil {
		t.Fatal(err)
	}
	if seenQuery != "site:developer.hashicorp.com acl bootstrap" {
		t.Fatalf("backend query = %q", seenQuery)
	}
	if !strings.Contains(text, docsSite+"/consul/acl") {
		t.Fatalf("on-site result missing: %q", text)
	}
	// The backend is untrusted metasearch: strays never reach the model.
	if strings.Contains(text, "blogspam") {
		t.Fatalf("off-site result leaked: %q", text)
	}
}

func TestRedhatKnowledgeExchangesOnceAndSpeaksKCS(t *testing.T) {
	ssoCalls := 0
	sso := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ssoCalls++
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("client_id") != "rhsm-api" || r.Form.Get("refresh_token") != "offline-token" {
			t.Fatalf("token exchange form = %v", r.Form)
		}
		json.NewEncoder(w).Encode(map[string]any{"access_token": "short-lived", "expires_in": 900})
	}))
	defer sso.Close()
	var lastQuery map[string][]string
	kb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer short-lived" {
			t.Fatalf("kb auth = %q", r.Header.Get("Authorization"))
		}
		lastQuery = r.URL.Query()
		json.NewEncoder(w).Encode(map[string]any{"response": map[string]any{"docs": []map[string]any{{
			"id": "12345", "documentKind": "Solution", "title": "Stuck CSV",
			"abstract": []any{"multi", "valued"}, "view_uri": "https://access.redhat.com/solutions/12345",
		}}}})
	}))
	defer kb.Close()
	registry := &Registry{tools: make(map[string]toolEntry)}
	err := registry.addRedhatKnowledge(KnowledgeSource{
		Name: "redhat", Type: "redhat", Auth: func() string { return "offline-token" }, Client: kb.Client(),
	}, sso.URL, kb.URL)
	if err != nil {
		t.Fatal(err)
	}
	text, err := registry.tools["search_redhat_kb"].call(context.Background(), map[string]any{"query": "csv stuck pending"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "id: 12345") || !strings.Contains(text, "abstract: multi valued") {
		t.Fatalf("kb result = %q", text)
	}
	if got := lastQuery["q"][0]; got != "csv stuck pending" {
		t.Fatalf("kb query = %q", got)
	}
	if _, err := registry.tools["get_redhat_kb"].call(context.Background(), map[string]any{"id": "12345"}); err != nil {
		t.Fatal(err)
	}
	if got := lastQuery["q"][0]; got != "id:12345" {
		t.Fatalf("detail query = %q", got)
	}
	if !strings.Contains(lastQuery["fl"][0], "solution_resolution") {
		t.Fatalf("detail fields = %q", lastQuery["fl"][0])
	}
	if _, err := registry.tools["search_redhat_docs"].call(context.Background(), map[string]any{"query": "upgrade path"}); err != nil {
		t.Fatal(err)
	}
	if got := lastQuery["fq"][0]; got != `documentKind:"Documentation"` {
		t.Fatalf("docs filter = %q", got)
	}
	if ssoCalls != 1 {
		t.Fatalf("token exchanged %d times, want 1 (cached)", ssoCalls)
	}
}
