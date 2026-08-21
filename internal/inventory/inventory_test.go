package inventory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, dir, name, contents string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLoadMergesFilesAndAppliesDefaults(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "hosts.yaml", `
hosts:
  - name: nas
    hostname: nas.lab.example
    aliases: [storage]
  - name: winbox
    connection: winrm
`)
	write(t, dir, "clusters.yml", `
clusters:
  - name: ocp
    api: https://api.ocp.lab.example:6443
    type: openshift
`)
	write(t, dir, "ignored.txt", "not yaml")
	inv, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.Hosts) != 2 || len(inv.Clusters) != 1 {
		t.Fatalf("unexpected inventory: %+v", inv)
	}
	if inv.Hosts[0].Connection != "ssh" {
		t.Errorf("connection default not applied: %+v", inv.Hosts[0])
	}
	if inv.Hosts[1].Connection != "winrm" {
		t.Errorf("declared connection lost: %+v", inv.Hosts[1])
	}
}

func TestLoadMissingDirIsEmptyInventory(t *testing.T) {
	inv, err := Load(filepath.Join(t.TempDir(), "absent"))
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.Hosts) != 0 || len(inv.Clusters) != 0 || inv.Environment() != "" {
		t.Fatalf("expected empty inventory: %+v", inv)
	}
	if _, ok := inv.ResolveTarget("anything"); ok {
		t.Error("empty inventory resolved a target")
	}
}

func TestLoadValidation(t *testing.T) {
	cases := []struct {
		name     string
		contents string
		want     string
	}{
		{"missing host name", "hosts:\n  - hostname: a.example\n", "hosts[0].name is required"},
		{"duplicate host name", "hosts:\n  - name: nas\n  - name: nas\n", `hosts[1].name "nas" is duplicated`},
		{"bad connection", "hosts:\n  - name: nas\n    connection: telnet\n", "want ssh or winrm"},
		{"identity collision", "hosts:\n  - name: nas\n    aliases: [shared]\n  - name: mini\n    aliases: [shared]\n", `identity "shared" already belongs to host "nas"`},
		{"missing cluster api", "clusters:\n  - name: ocp\n    type: openshift\n", "clusters[0].api is required"},
		{"bad cluster type", "clusters:\n  - name: ocp\n    api: https://api.example\n    type: nomad\n", "want kubernetes or openshift"},
		{"unknown key", "fleet:\n  - name: nas\n", "field fleet not found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			write(t, dir, "inventory.yaml", tc.contents)
			_, err := Load(dir)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestResolveTarget(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "inventory.yaml", `
hosts:
  - name: nas
    hostname: nas.lab.example
    aliases: [storage]
  - name: mini
  - name: ignored-node
    hostname: ignored.example
    status: ignored
  - name: worker-node
    cluster: ocp
`)
	inv, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"nas", "nas.lab.example", "storage", "nas.lab.example:9100", "nas:22"} {
		got, ok := inv.ResolveTarget(target)
		if !ok || got != "nas.lab.example" {
			t.Errorf("ResolveTarget(%q) = %q, %v; want nas.lab.example, true", target, got, ok)
		}
	}
	if got, ok := inv.ResolveTarget("mini"); !ok || got != "mini" {
		t.Errorf("ResolveTarget(mini) = %q, %v; want mini, true", got, ok)
	}
	if _, ok := inv.ResolveTarget("oddjob"); ok {
		t.Error("unknown target resolved")
	}
	for _, target := range []string{"ignored-node", "ignored.example", "worker-node"} {
		if _, ok := inv.ResolveTarget(target); ok {
			t.Errorf("inert host %q resolved", target)
		}
	}
	if got := inv.KnownTargets(); len(got) != 2 || got[0] != "nas.lab.example" || got[1] != "mini" {
		t.Errorf("KnownTargets() = %v", got)
	}
}

func TestEnvironmentRendersFacts(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "inventory.yaml", `
hosts:
  - name: nas
    hostname: nas.lab.example
    aliases: [storage]
  - name: winbox
    connection: winrm
  - name: pending-node
    status: draft
  - name: ignored-node
    status: ignored
  - name: node-z
    cluster: ocp
  - name: node-a
    cluster: ocp
clusters:
  - name: ocp
    api: https://api.ocp.lab.example:6443
    type: openshift
`)
	inv, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := inv.Environment()
	for _, want := range []string{
		"Inventory:",
		"may target any identity listed",
		"- nas (aka nas.lab.example, storage)",
		"- winbox (winrm)",
		"- ocp: openshift, API https://api.ocp.lab.example:6443 (nodes: node-a, node-z)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("Environment() missing %q:\n%s", want, got)
		}
	}
	for _, unwanted := range []string{"pending-node", "ignored-node"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("Environment() contains inert standalone host %q:\n%s", unwanted, got)
		}
	}
}
