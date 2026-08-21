// Package inventory loads the operator-declared estate — hosts and clusters —
// and resolves the identities alerts and metrics use (name, hostname, alias)
// to the canonical connection target. The inventory is a security boundary:
// gated tool targets are validated against it before any approval is
// requested, so members are operator-declared only; it never holds
// credentials.
package inventory

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

type Host struct {
	Name       string   `yaml:"name" json:"name"`
	Hostname   string   `yaml:"hostname,omitempty" json:"hostname,omitempty"`
	Aliases    []string `yaml:"aliases,omitempty" json:"aliases,omitempty"`
	Connection string   `yaml:"connection,omitempty" json:"connection"`
}

// connectionTarget is the address executors are given: hostname when it
// differs from the friendly name, the name otherwise.
func (h Host) connectionTarget() string {
	if h.Hostname != "" {
		return h.Hostname
	}
	return h.Name
}

type Cluster struct {
	Name string `yaml:"name" json:"name"`
	API  string `yaml:"api" json:"api"`
	Type string `yaml:"type" json:"type"`
}

type Inventory struct {
	Hosts    []Host    `json:"hosts"`
	Clusters []Cluster `json:"clusters"`
	targets  map[string]string
}

// Load reads every *.yaml/*.yml file in dir, in name order, into one
// inventory. A missing directory is an empty inventory, not an error: the
// feature is opt-in and every consumer degrades to pre-inventory behaviour.
func Load(dir string) (*Inventory, error) {
	inv := &Inventory{Hosts: []Host{}, Clusters: []Cluster{}, targets: map[string]string{}}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return inv, nil
		}
		return nil, err
	}
	hostNames := map[string]bool{}
	clusterNames := map[string]bool{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || (!strings.HasSuffix(name, ".yaml") && !strings.HasSuffix(name, ".yml")) {
			continue
		}
		path := filepath.Join(dir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var file struct {
			Hosts    []Host    `yaml:"hosts"`
			Clusters []Cluster `yaml:"clusters"`
		}
		decoder := yaml.NewDecoder(bytes.NewReader(data))
		decoder.KnownFields(true)
		if err := decoder.Decode(&file); err != nil && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		for i := range file.Hosts {
			host := &file.Hosts[i]
			prefix := fmt.Sprintf("%s: hosts[%d]", path, i)
			if host.Name == "" {
				return nil, fmt.Errorf("%s.name is required", prefix)
			}
			if hostNames[host.Name] {
				return nil, fmt.Errorf("%s.name %q is duplicated", prefix, host.Name)
			}
			hostNames[host.Name] = true
			if host.Connection == "" {
				host.Connection = "ssh"
			}
			if host.Connection != "ssh" && host.Connection != "winrm" {
				return nil, fmt.Errorf("%s.connection %q is not supported (want ssh or winrm)", prefix, host.Connection)
			}
			identities := append([]string{host.Name}, host.Aliases...)
			if host.Hostname != "" {
				identities = append(identities, host.Hostname)
			}
			for _, identity := range identities {
				if owner, exists := inv.targets[identity]; exists && owner != host.connectionTarget() {
					return nil, fmt.Errorf("%s: identity %q already belongs to host %q", prefix, identity, owner)
				}
				inv.targets[identity] = host.connectionTarget()
			}
			inv.Hosts = append(inv.Hosts, *host)
		}
		for i, cluster := range file.Clusters {
			prefix := fmt.Sprintf("%s: clusters[%d]", path, i)
			if cluster.Name == "" {
				return nil, fmt.Errorf("%s.name is required", prefix)
			}
			if clusterNames[cluster.Name] {
				return nil, fmt.Errorf("%s.name %q is duplicated", prefix, cluster.Name)
			}
			clusterNames[cluster.Name] = true
			if cluster.API == "" {
				return nil, fmt.Errorf("%s.api is required", prefix)
			}
			if cluster.Type != "kubernetes" && cluster.Type != "openshift" {
				return nil, fmt.Errorf("%s.type %q is not supported (want kubernetes or openshift)", prefix, cluster.Type)
			}
			inv.Clusters = append(inv.Clusters, cluster)
		}
	}
	return inv, nil
}

// ResolveTarget maps a declared identity — name, hostname, or alias, with or
// without the :port suffix metric instance labels carry — to the host's
// connection target.
func (inv *Inventory) ResolveTarget(target string) (string, bool) {
	if canonical, ok := inv.targets[target]; ok {
		return canonical, true
	}
	if host, _, err := net.SplitHostPort(target); err == nil {
		if canonical, ok := inv.targets[host]; ok {
			return canonical, true
		}
	}
	return "", false
}

// KnownTargets lists connection targets for pre-flight error messages.
func (inv *Inventory) KnownTargets() []string {
	names := make([]string, 0, len(inv.Hosts))
	for _, host := range inv.Hosts {
		names = append(names, host.connectionTarget())
	}
	return names
}

// Environment renders the inventory as fact lines for the system prompt and
// KB articles; judgement prose stays in the operator preamble.
func (inv *Inventory) Environment() string {
	if len(inv.Hosts) == 0 && len(inv.Clusters) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Inventory:")
	if len(inv.Hosts) > 0 {
		b.WriteString("\nHosts (a gated host command may target any identity listed; nothing else):")
		for _, host := range inv.Hosts {
			b.WriteString("\n- " + host.Name)
			var detail []string
			if host.Connection != "ssh" {
				detail = append(detail, host.Connection)
			}
			var akas []string
			if host.Hostname != "" && host.Hostname != host.Name {
				akas = append(akas, host.Hostname)
			}
			akas = append(akas, host.Aliases...)
			if len(akas) > 0 {
				detail = append(detail, "aka "+strings.Join(akas, ", "))
			}
			if len(detail) > 0 {
				b.WriteString(" (" + strings.Join(detail, "; ") + ")")
			}
		}
	}
	if len(inv.Clusters) > 0 {
		b.WriteString("\nClusters:")
		for _, cluster := range inv.Clusters {
			b.WriteString("\n- " + cluster.Name + ": " + cluster.Type + ", API " + cluster.API)
		}
	}
	return b.String()
}
