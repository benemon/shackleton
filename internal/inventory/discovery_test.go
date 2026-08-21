package inventory

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fakePrometheus(t *testing.T, instances map[string]string) Source {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/query" || r.URL.Query().Get("query") != "node_uname_info" {
			http.NotFound(w, r)
			return
		}
		var results []string
		for instance, nodename := range instances {
			results = append(results, fmt.Sprintf(`{"metric":{"instance":%q,"nodename":%q}}`, instance, nodename))
		}
		fmt.Fprintf(w, `{"status":"success","data":{"result":[%s]}}`, strings.Join(results, ","))
	}))
	t.Cleanup(server.Close)
	return Source{Name: "prometheus", Client: server.Client(), BaseURL: server.URL}
}

func TestDiscoverProposesDraftsOnce(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "declared.yaml", "hosts:\n  - name: nas\n    hostname: nas.lab.example\n")
	source := fakePrometheus(t, map[string]string{
		"nas.lab.example:9100":   "nas",
		"node1.lab.example:9100": "node1",
	})

	news, err := Discover(context.Background(), dir, []Source{source})
	if err != nil {
		t.Fatal(err)
	}
	if len(news) != 1 || news[0].Name != "node1" || news[0].Hostname != "node1.lab.example" || news[0].Status != "draft" || news[0].Source != "prometheus" {
		t.Fatalf("news = %+v", news)
	}

	inv, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.Hosts) != 2 {
		t.Fatalf("hosts after discovery = %+v", inv.Hosts)
	}
	if _, ok := inv.ResolveTarget("node1"); ok {
		t.Error("draft resolved as a target")
	}
	if _, ok := inv.ResolveTarget("node1.lab.example"); ok {
		t.Error("draft hostname resolved as a target")
	}
	if env := inv.Environment(); strings.Contains(env, "node1") {
		t.Errorf("draft leaked into environment: %s", env)
	}
	if targets := inv.KnownTargets(); len(targets) != 1 || targets[0] != "nas.lab.example" {
		t.Errorf("KnownTargets = %v", targets)
	}

	news, err = Discover(context.Background(), dir, []Source{source})
	if err != nil {
		t.Fatal(err)
	}
	if len(news) != 0 {
		t.Fatalf("second pass re-proposed: %+v", news)
	}
}

func TestDiscoverRequiresDeclaredMembers(t *testing.T) {
	dir := t.TempDir()
	source := fakePrometheus(t, map[string]string{"node1.lab.example:9100": "node1"})
	news, err := Discover(context.Background(), dir, []Source{source})
	if err != nil || len(news) != 0 {
		t.Fatalf("discovery on an empty inventory: %v %+v", err, news)
	}
	if _, err := os.Stat(filepath.Join(dir, draftsFile)); !os.IsNotExist(err) {
		t.Fatal("drafts file created on an estate with no declared inventory")
	}
}

func TestDiscoverSelfHealsAndPreservesApprovals(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "declared.yaml", "hosts:\n  - name: nas\n    hostname: nas.lab.example\n")
	source := fakePrometheus(t, map[string]string{
		"node1.lab.example:9100": "node1",
		"node2.lab.example:9100": "node2",
	})
	if _, err := Discover(context.Background(), dir, []Source{source}); err != nil {
		t.Fatal(err)
	}

	// Operator approves node1 in place and declares node2 in their own file.
	drafts, err := os.ReadFile(filepath.Join(dir, draftsFile))
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(drafts), "status: draft", "status: approved", 1)
	if err := os.WriteFile(filepath.Join(dir, draftsFile), []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	write(t, dir, "more.yaml", "hosts:\n  - name: node2\n    hostname: node2.lab.example\n")

	news, err := Discover(context.Background(), dir, []Source{source})
	if err != nil {
		t.Fatal(err)
	}
	if len(news) != 0 {
		t.Fatalf("re-proposed after approval/declaration: %+v", news)
	}
	inv, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]Host{}
	for _, host := range inv.Hosts {
		byName[host.Name] = host
	}
	if byName["node1"].Status != "approved" {
		t.Fatalf("in-place approval lost: %+v", byName["node1"])
	}
	if _, ok := inv.ResolveTarget("node1.lab.example"); !ok {
		t.Error("approved member is not actionable")
	}
	if drafts, err := os.ReadFile(filepath.Join(dir, draftsFile)); err != nil || strings.Contains(string(drafts), "node2") {
		t.Fatalf("declared host did not self-heal out of drafts: %v %s", err, drafts)
	}
}
