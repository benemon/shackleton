package inventory

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Source is a Prometheus-compatible query API discovery reads host-shaped
// series from, carrying an HTTP client that already holds its auth.
type Source struct {
	Name    string
	Client  *http.Client
	BaseURL string
}

const (
	draftsFile       = "drafts.yaml"
	clusterNodesFile = "cluster-nodes.yaml"
)

// Run performs a discovery pass at startup and then hourly until ctx ends,
// reporting each newly proposed draft through notify.
func Run(ctx context.Context, dir string, sources []Source, notify func(Host)) {
	go func() {
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for {
			drafts, err := Discover(ctx, dir, sources)
			if err != nil {
				log.Printf("inventory discovery: %v", err)
			}
			for _, draft := range drafts {
				notify(draft)
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

// Discover observes standalone hosts and cluster members from metrics.
func Discover(ctx context.Context, dir string, sources []Source) ([]Host, error) {
	inv, err := Load(dir)
	if err != nil {
		return nil, err
	}
	declared := 0
	drafted := map[string]Host{}
	suppressed := map[string]bool{}
	takenNames := map[string]bool{}
	hasMembers := false
	for _, host := range inv.Hosts {
		if host.Cluster != "" {
			hasMembers = true
			continue
		}
		takenNames[host.Name] = true
		if host.Status == "draft" {
			drafted[host.connectionTarget()] = host
		}
		if host.Status == "draft" || host.Status == "ignored" {
			suppressed[host.Name] = true
			suppressed[host.connectionTarget()] = true
			for _, alias := range host.Aliases {
				suppressed[alias] = true
			}
			continue
		}
		declared++
	}
	if declared == 0 && len(inv.Clusters) == 0 {
		if hasMembers {
			_, err := writeClusterNodes(dir, map[string]Host{})
			return nil, err
		}
		return nil, nil
	}

	now := time.Now().UTC().Format(time.RFC3339)
	var news []Host
	members := map[string]Host{}
	recognized := map[string]bool{}
	excluded := map[string]bool{}
	for _, source := range sources {
		instances, kubeNodes, err := sourceNodes(ctx, source)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", source.Name, err)
		}
		for instance, nodename := range instances {
			hostPart := instance
			if host, _, err := net.SplitHostPort(instance); err == nil {
				hostPart = host
			}
			node := ""
			if kubeNodes[nodename] && nodename != "" {
				node = nodename
			} else if kubeNodes[hostPart] {
				node = hostPart
			}
			if node != "" {
				recognized[node] = true
				recognized[nodename] = true
				recognized[hostPart] = true
				if len(inv.Clusters) == 1 {
					members[node] = Host{Name: node, Cluster: inv.Clusters[0].Name, Source: source.Name, FirstSeen: now}
				} else {
					excluded[node] = true
				}
				continue
			}
			if _, known := inv.ResolveTarget(hostPart); known {
				continue
			}
			if suppressed[hostPart] || suppressed[nodename] {
				continue
			}
			name := nodename
			if name == "" || takenNames[name] {
				name = hostPart
			}
			if takenNames[name] {
				continue
			}
			takenNames[name] = true
			draft := Host{Name: name, Connection: "ssh", Status: "draft",
				Source: source.Name, FirstSeen: now}
			if hostPart != name {
				draft.Hostname = hostPart
			}
			drafted[hostPart] = draft
			news = append(news, draft)
		}
	}

	sort.Slice(news, func(i, j int) bool { return news[i].Name < news[j].Name })
	if err := writeDrafts(dir, inv, drafted, recognized); err != nil {
		return nil, err
	}
	newMembers, err := writeClusterNodes(dir, members)
	if err != nil {
		return nil, err
	}
	if len(excluded) > 0 {
		nodes := make([]string, 0, len(excluded))
		for node := range excluded {
			nodes = append(nodes, node)
		}
		sort.Strings(nodes)
		reason := "no declared cluster"
		if len(inv.Clusters) > 1 {
			reason = fmt.Sprintf("ambiguous attribution across %d declared clusters", len(inv.Clusters))
		}
		log.Printf("inventory discovery: excluded cluster nodes %s: %s", strings.Join(nodes, ", "), reason)
	}
	if len(newMembers) > 0 {
		log.Printf("inventory discovery: recognized cluster members %s", strings.Join(newMembers, ", "))
	}
	return news, nil
}

// writeDrafts rewrites drafts.yaml: operator-approved entries verbatim,
// then drafts that still resolve to nothing declared (a draft whose
// identity the operator has since declared elsewhere self-heals away).
func writeDrafts(dir string, inv *Inventory, drafted map[string]Host, recognized map[string]bool) error {
	var file struct {
		Hosts []Host `yaml:"hosts"`
	}
	existing, err := os.ReadFile(filepath.Join(dir, draftsFile))
	if err == nil {
		var current struct {
			Hosts []Host `yaml:"hosts"`
		}
		if err := yaml.Unmarshal(existing, &current); err == nil {
			for _, host := range current.Hosts {
				if host.Status != "draft" {
					file.Hosts = append(file.Hosts, host)
				}
			}
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	targets := make([]string, 0, len(drafted))
	for target := range drafted {
		targets = append(targets, target)
	}
	sort.Strings(targets)
	for _, target := range targets {
		host := drafted[target]
		if _, known := inv.ResolveTarget(host.connectionTarget()); known {
			continue
		}
		if recognized[host.Name] || recognized[host.connectionTarget()] {
			continue
		}
		healed := false
		for _, alias := range host.Aliases {
			if recognized[alias] {
				healed = true
				break
			}
		}
		if healed {
			continue
		}
		file.Hosts = append(file.Hosts, host)
	}
	if len(file.Hosts) == 0 {
		err := os.Remove(filepath.Join(dir, draftsFile))
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	encoded, err := yaml.Marshal(file)
	if err != nil {
		return err
	}
	header := "# Machine-owned: discovery proposes draft hosts here. Approve a member by\n# setting status: approved (or moving it to an operator file); discovery\n# never rewrites a non-draft entry.\n"
	path := filepath.Join(dir, draftsFile)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append([]byte(header), encoded...), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func writeClusterNodes(dir string, observed map[string]Host) ([]string, error) {
	path := filepath.Join(dir, clusterNodesFile)
	existing := map[string]Host{}
	data, err := os.ReadFile(path)
	if err == nil {
		var current struct {
			Hosts []Host `yaml:"hosts"`
		}
		if err := yaml.Unmarshal(data, &current); err != nil {
			return nil, err
		}
		for _, host := range current.Hosts {
			existing[host.Name] = host
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if len(observed) == 0 {
		err := os.Remove(path)
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	names := make([]string, 0, len(observed))
	for name := range observed {
		names = append(names, name)
	}
	sort.Strings(names)
	var file struct {
		Hosts []Host `yaml:"hosts"`
	}
	var news []string
	for _, name := range names {
		host := observed[name]
		if previous, ok := existing[name]; ok {
			if previous.FirstSeen != "" {
				host.FirstSeen = previous.FirstSeen
			}
		} else {
			news = append(news, name)
		}
		file.Hosts = append(file.Hosts, host)
	}
	encoded, err := yaml.Marshal(file)
	if err != nil {
		return nil, err
	}
	header := "# Fully machine-owned: cluster nodes are rewritten from observation.\n# Operator edits will be overwritten.\n"
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append([]byte(header), encoded...), 0o644); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, path); err != nil {
		return nil, err
	}
	return news, nil
}

func sourceNodes(ctx context.Context, source Source) (map[string]string, map[string]bool, error) {
	hostSeries, err := querySeries(ctx, source, "node_uname_info")
	if err != nil {
		return nil, nil, err
	}
	kubeSeries, err := querySeries(ctx, source, "kube_node_info")
	if err != nil {
		return nil, nil, err
	}
	instances := make(map[string]string, len(hostSeries))
	for _, series := range hostSeries {
		if instance := series["instance"]; instance != "" {
			instances[instance] = series["nodename"]
		}
	}
	kubeNodes := make(map[string]bool, len(kubeSeries))
	for _, series := range kubeSeries {
		if node := series["node"]; node != "" {
			kubeNodes[node] = true
		}
	}
	return instances, kubeNodes, nil
}

func querySeries(ctx context.Context, source Source, query string) ([]map[string]string, error) {
	values := url.Values{"query": {query}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, source.BaseURL+"/api/v1/query?"+values.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := source.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", resp.Status, body)
	}
	var payload struct {
		Data struct {
			Result []struct {
				Metric map[string]string `json:"metric"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	result := make([]map[string]string, 0, len(payload.Data.Result))
	for _, series := range payload.Data.Result {
		result = append(result, series.Metric)
	}
	return result, nil
}
