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

// draftsFile is the one file discovery owns inside inventory_dir. Everything
// else in the directory is operator custody; entries in this file whose
// status an operator flipped away from draft are custody transfers and are
// never rewritten or re-drafted.
const draftsFile = "drafts.yaml"

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

// Discover queries each source for node_uname_info series and proposes any
// instance that resolves to no declared member as a draft host in
// drafts.yaml. It returns only the drafts new in this pass. Estates with no
// declared hosts are left untouched: discovery appends to an inventory the
// operator opted into, it never starts one.
func Discover(ctx context.Context, dir string, sources []Source) ([]Host, error) {
	inv, err := Load(dir)
	if err != nil {
		return nil, err
	}
	declared := 0
	drafted := map[string]Host{}
	takenNames := map[string]bool{}
	for _, host := range inv.Hosts {
		takenNames[host.Name] = true
		if host.draft() {
			drafted[host.connectionTarget()] = host
			continue
		}
		declared++
	}
	if declared == 0 {
		return nil, nil
	}

	var news []Host
	for _, source := range sources {
		instances, err := hostInstances(ctx, source)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", source.Name, err)
		}
		for instance, nodename := range instances {
			hostPart := instance
			if host, _, err := net.SplitHostPort(instance); err == nil {
				hostPart = host
			}
			if _, known := inv.ResolveTarget(hostPart); known {
				continue
			}
			if _, already := drafted[hostPart]; already {
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
				Source: source.Name, FirstSeen: time.Now().UTC().Format(time.RFC3339)}
			if hostPart != name {
				draft.Hostname = hostPart
			}
			drafted[hostPart] = draft
			news = append(news, draft)
		}
	}

	sort.Slice(news, func(i, j int) bool { return news[i].Name < news[j].Name })
	if err := writeDrafts(dir, inv, drafted); err != nil {
		return nil, err
	}
	return news, nil
}

// writeDrafts rewrites drafts.yaml: operator-approved entries verbatim,
// then drafts that still resolve to nothing declared (a draft whose
// identity the operator has since declared elsewhere self-heals away).
func writeDrafts(dir string, inv *Inventory, drafted map[string]Host) error {
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
				if !host.draft() {
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

func hostInstances(ctx context.Context, source Source) (map[string]string, error) {
	values := url.Values{"query": {"node_uname_info"}}
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
	instances := make(map[string]string, len(payload.Data.Result))
	for _, series := range payload.Data.Result {
		if instance := series.Metric["instance"]; instance != "" {
			instances[instance] = series.Metric["nodename"]
		}
	}
	return instances, nil
}
