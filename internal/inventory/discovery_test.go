package inventory

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fakePrometheus(t *testing.T, instances map[string]string, kubeNodes []string) Source {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/query" {
			http.NotFound(w, r)
			return
		}
		var results []string
		switch r.URL.Query().Get("query") {
		case "node_uname_info":
			for instance, nodename := range instances {
				results = append(results, fmt.Sprintf(`{"metric":{"instance":%q,"nodename":%q}}`, instance, nodename))
			}
		case "kube_node_info":
			for _, node := range kubeNodes {
				results = append(results, fmt.Sprintf(`{"metric":{"instance":"kube-state-metrics:8080","node":%q}}`, node))
			}
		default:
			http.NotFound(w, r)
			return
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
	}, nil)

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
	source := fakePrometheus(t, map[string]string{"node1.lab.example:9100": "node1"}, nil)
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
	}, nil)
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

func TestDiscoverRecognizesClusterNodes(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "declared.yaml", `
clusters:
  - name: ocp
    api: https://api.ocp.example
    type: openshift
`)
	source := fakePrometheus(t, map[string]string{
		"node1.example:9100": "node1",
		"node2:9100":         "different-uname",
	}, []string{"node1", "node2"})
	var output bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&output)
	defer log.SetOutput(previous)

	news, err := Discover(context.Background(), dir, []Source{source})
	if err != nil {
		t.Fatal(err)
	}
	if len(news) != 0 {
		t.Fatalf("cluster node proposed as draft: %+v", news)
	}
	data, err := os.ReadFile(filepath.Join(dir, clusterNodesFile))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "Fully machine-owned") || strings.Contains(string(data), "connection:") {
		t.Fatalf("cluster nodes file has wrong custody or fields:\n%s", data)
	}
	inv, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.Hosts) != 2 {
		t.Fatalf("hosts = %+v", inv.Hosts)
	}
	byName := map[string]Host{}
	for _, host := range inv.Hosts {
		byName[host.Name] = host
	}
	host := byName["node1"]
	if host.Name != "node1" || host.Cluster != "ocp" || host.Source != "prometheus" || host.Status != "" {
		t.Fatalf("cluster member = %+v", host)
	}
	if host := byName["node2"]; host.Cluster != "ocp" {
		t.Fatalf("port-stripped host match = %+v", host)
	}
	if _, err := time.Parse(time.RFC3339, host.FirstSeen); err != nil {
		t.Fatalf("first_seen = %q: %v", host.FirstSeen, err)
	}
	if _, err := os.Stat(filepath.Join(dir, draftsFile)); !os.IsNotExist(err) {
		t.Fatal("cluster node created a draft file")
	}
	if strings.Count(output.String(), "recognized cluster members") != 1 || !strings.Contains(output.String(), "node1, node2") {
		t.Fatalf("recognition log = %q", output.String())
	}
	output.Reset()
	if _, err := Discover(context.Background(), dir, []Source{source}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), "recognized cluster members") {
		t.Fatalf("persistent members logged as new: %q", output.String())
	}
}

func TestDiscoverExcludesUnattributableClusterNodes(t *testing.T) {
	tests := []struct {
		name     string
		declared string
		reason   string
	}{
		{"no clusters", "hosts:\n  - name: nas\n", "no declared cluster"},
		{"two clusters", `
clusters:
  - name: east
    api: https://api.east.example
    type: kubernetes
  - name: west
    api: https://api.west.example
    type: kubernetes
`, "ambiguous attribution across 2 declared clusters"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			write(t, dir, "declared.yaml", test.declared)
			source := fakePrometheus(t, map[string]string{"node1:9100": "node1"}, []string{"node1"})
			var output bytes.Buffer
			previous := log.Writer()
			log.SetOutput(&output)
			defer log.SetOutput(previous)
			news, err := Discover(context.Background(), dir, []Source{source})
			if err != nil {
				t.Fatal(err)
			}
			if len(news) != 0 {
				t.Fatalf("cluster node proposed as draft: %+v", news)
			}
			for _, name := range []string{draftsFile, clusterNodesFile} {
				if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
					t.Fatalf("%s created for unattributable node", name)
				}
			}
			if strings.Count(output.String(), "excluded cluster nodes") != 1 || !strings.Contains(output.String(), "node1") || !strings.Contains(output.String(), test.reason) {
				t.Fatalf("exclusion log = %q", output.String())
			}
		})
	}
}

func TestDiscoverReclassifiesDraftAsClusterMember(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "declared.yaml", `
clusters:
  - name: ocp
    api: https://api.ocp.example
    type: openshift
`)
	write(t, dir, draftsFile, `
hosts:
  - name: node1
    hostname: node1.example
    status: draft
    source: prometheus
    first_seen: "2026-01-01T00:00:00Z"
`)
	source := fakePrometheus(t, map[string]string{"node1.example:9100": "node1"}, []string{"node1"})

	news, err := Discover(context.Background(), dir, []Source{source})
	if err != nil {
		t.Fatal(err)
	}
	if len(news) != 0 {
		t.Fatalf("reclassified draft reported as new: %+v", news)
	}
	if _, err := os.Stat(filepath.Join(dir, draftsFile)); !os.IsNotExist(err) {
		t.Fatal("reclassified draft remains in drafts.yaml")
	}
	inv, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.Hosts) != 1 || inv.Hosts[0].Name != "node1" || inv.Hosts[0].Cluster != "ocp" {
		t.Fatalf("hosts = %+v", inv.Hosts)
	}
}

func TestDiscoverPreservesAndSuppressesIgnoredHost(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "declared.yaml", "hosts:\n  - name: nas\n")
	write(t, dir, draftsFile, `
hosts:
  - name: node1
    hostname: node1.example
    aliases: [worker-one]
    status: ignored
    source: prometheus
`)
	source := fakePrometheus(t, map[string]string{"node1.example:9100": "node1"}, nil)

	news, err := Discover(context.Background(), dir, []Source{source})
	if err != nil {
		t.Fatal(err)
	}
	if len(news) != 0 {
		t.Fatalf("ignored host re-proposed: %+v", news)
	}
	data, err := os.ReadFile(filepath.Join(dir, draftsFile))
	if err != nil || !strings.Contains(string(data), "status: ignored") {
		t.Fatalf("ignored host not preserved: %v\n%s", err, data)
	}
	inv, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, identity := range []string{"node1", "node1.example", "worker-one"} {
		if _, ok := inv.ResolveTarget(identity); ok {
			t.Errorf("ignored identity %q resolved", identity)
		}
	}
	if got := inv.KnownTargets(); len(got) != 1 || got[0] != "nas" {
		t.Errorf("KnownTargets() = %v", got)
	}
	if env := inv.Environment(); strings.Contains(env, "node1") {
		t.Errorf("ignored host leaked into environment: %s", env)
	}
}

func TestDiscoverDropsMemberOfUndeclaredCluster(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "declared.yaml", "hosts:\n  - name: nas\n")
	write(t, dir, clusterNodesFile, `
hosts:
  - name: node1
    cluster: retired
    source: prometheus
    first_seen: "2026-01-01T00:00:00Z"
`)
	inv, err := Load(dir)
	if err != nil || len(inv.Hosts) != 2 {
		t.Fatalf("undeclared cluster member did not load: %v %+v", err, inv)
	}
	source := fakePrometheus(t, map[string]string{}, nil)
	if _, err := Discover(context.Background(), dir, []Source{source}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, clusterNodesFile)); !os.IsNotExist(err) {
		t.Fatal("member of undeclared cluster was not dropped")
	}
}

func TestDiscoverRewritesClusterNodeObservation(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "declared.yaml", `
clusters:
  - name: ocp
    api: https://api.ocp.example
    type: openshift
`)
	write(t, dir, clusterNodesFile, `
hosts:
  - name: node1
    cluster: ocp
    source: prometheus
    first_seen: "2026-01-01T00:00:00Z"
  - name: node2
    cluster: ocp
    source: prometheus
    first_seen: "2026-02-01T00:00:00Z"
`)
	source := fakePrometheus(t, map[string]string{
		"node1:9100": "node1",
		"node2:9100": "node2",
	}, []string{"node1"})

	if _, err := Discover(context.Background(), dir, []Source{source}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, clusterNodesFile))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "node2") || !strings.Contains(string(data), `first_seen: "2026-01-01T00:00:00Z"`) {
		t.Fatalf("cluster node rewrite did not drop node2 or preserve node1 first_seen:\n%s", data)
	}
}
