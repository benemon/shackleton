package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// KnowledgeSource is a vendor documentation or knowledge-base source. Type
// generic fetches pages from its allowlisted sites and searches them through
// the shared backend; type redhat speaks the Customer Portal KB API.
type KnowledgeSource struct {
	Name   string
	Type   string
	Sites  []string
	Auth   func() string
	Client *http.Client
}

// KnowledgeSearch is the shared metasearch backend (searxng JSON API).
type KnowledgeSearch struct {
	URL    string
	Client *http.Client
}

const (
	redhatSSOURL = "https://sso.redhat.com/auth/realms/redhat-external/protocol/openid-connect/token"
	redhatKBURL  = "https://access.redhat.com/hydra/rest/search/kcs"

	redhatSearchFields = "id,title,abstract,documentKind,view_uri,product,lastModifiedDate"
	redhatDetailFields = redhatSearchFields + ",issue,solution_environment,solution_rootcause,solution_resolution,solution_diagnosticsteps,createdDate"

	// A docs page beyond this is a download, not documentation; the runner's
	// tool-result cap truncates further for the model.
	maxDocBytes = 2 << 20
)

func (r *Registry) addKnowledgeSources(sources []KnowledgeSource, search *KnowledgeSearch) error {
	for _, source := range sources {
		var err error
		switch source.Type {
		case "generic":
			err = r.addGenericKnowledge(source, search)
		case "redhat":
			err = r.addRedhatKnowledge(source, redhatSSOURL, redhatKBURL)
		default:
			err = fmt.Errorf("unsupported type %q", source.Type)
		}
		if err != nil {
			return fmt.Errorf("knowledge source %s: %w", source.Name, err)
		}
	}
	return nil
}

func (r *Registry) addGenericKnowledge(source KnowledgeSource, search *KnowledgeSearch) error {
	sites := strings.Join(source.Sites, ", ")
	urlSchema := map[string]any{
		"type": "object", "properties": map[string]any{"url": map[string]any{"type": "string"}},
		"required": []string{"url"}, "additionalProperties": false,
	}
	if err := r.add("get_"+source.Name+"_doc",
		"Fetch a documentation page from "+sites+" and return its text.",
		urlSchema, false, func(ctx context.Context, args map[string]any) (string, error) {
			raw := fmt.Sprint(args["url"])
			if !urlAllowed(raw, source.Sites) {
				return "", fmt.Errorf("url is outside this source's sites (%s)", sites)
			}
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
			if err != nil {
				return "", err
			}
			if source.Auth != nil {
				if value := source.Auth(); value != "" {
					req.Header.Set("Authorization", value)
				}
			}
			resp, err := source.Client.Do(req)
			if err != nil {
				return "", err
			}
			defer resp.Body.Close()
			body, err := io.ReadAll(io.LimitReader(resp.Body, maxDocBytes))
			if err != nil {
				return "", err
			}
			if resp.StatusCode != http.StatusOK {
				return "", fmt.Errorf("%s: %s", source.Name, resp.Status)
			}
			return htmlToText(string(body)), nil
		}); err != nil {
		return err
	}
	if search == nil {
		return nil
	}
	querySchema := map[string]any{
		"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}},
		"required": []string{"query"}, "additionalProperties": false,
	}
	scope := make([]string, 0, len(source.Sites))
	for _, site := range source.Sites {
		if u, err := url.Parse(site); err == nil {
			scope = append(scope, "site:"+u.Host)
		}
	}
	return r.add("search_"+source.Name+"_docs",
		"Search documentation on "+sites+". Returns titles, URLs and snippets; fetch a result with get_"+source.Name+"_doc.",
		querySchema, false, func(ctx context.Context, args map[string]any) (string, error) {
			query := strings.Join(scope, " OR ") + " " + fmt.Sprint(args["query"])
			values := url.Values{"q": {query}, "format": {"json"}}
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, search.URL+"/search?"+values.Encode(), nil)
			if err != nil {
				return "", err
			}
			resp, err := search.Client.Do(req)
			if err != nil {
				return "", err
			}
			defer resp.Body.Close()
			body, err := io.ReadAll(io.LimitReader(resp.Body, maxDocBytes))
			if err != nil {
				return "", err
			}
			if resp.StatusCode != http.StatusOK {
				return "", fmt.Errorf("search: %s", resp.Status)
			}
			var decoded struct {
				Results []struct {
					URL     string `json:"url"`
					Title   string `json:"title"`
					Content string `json:"content"`
				} `json:"results"`
			}
			if err := json.Unmarshal(body, &decoded); err != nil {
				return "", fmt.Errorf("search: %w", err)
			}
			var b strings.Builder
			// The backend is a metasearch over public engines: results outside
			// the source's sites are dropped, never surfaced.
			for _, result := range decoded.Results {
				if !urlAllowed(result.URL, source.Sites) {
					continue
				}
				fmt.Fprintf(&b, "%s\n%s\n%s\n\n", result.Title, result.URL, result.Content)
			}
			if b.Len() == 0 {
				return "no results", nil
			}
			return strings.TrimSpace(b.String()), nil
		})
}

type redhatSession struct {
	mu      sync.Mutex
	token   string
	expires time.Time
}

func (r *Registry) addRedhatKnowledge(source KnowledgeSource, ssoURL, kbURL string) error {
	session := &redhatSession{}
	accessToken := func(ctx context.Context) (string, error) {
		session.mu.Lock()
		defer session.mu.Unlock()
		if session.token != "" && time.Now().Before(session.expires) {
			return session.token, nil
		}
		form := url.Values{
			"grant_type":    {"refresh_token"},
			"client_id":     {"rhsm-api"},
			"refresh_token": {source.Auth()},
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, ssoURL, strings.NewReader(form.Encode()))
		if err != nil {
			return "", err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		resp, err := source.Client.Do(req)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxDocBytes))
		if err != nil {
			return "", err
		}
		if resp.StatusCode != http.StatusOK {
			return "", fmt.Errorf("token exchange: %s", resp.Status)
		}
		var token struct {
			AccessToken string `json:"access_token"`
			ExpiresIn   int    `json:"expires_in"`
		}
		if err := json.Unmarshal(body, &token); err != nil || token.AccessToken == "" {
			return "", fmt.Errorf("token exchange: unexpected response")
		}
		session.token = token.AccessToken
		session.expires = time.Now().Add(time.Duration(token.ExpiresIn)*time.Second - time.Minute)
		return session.token, nil
	}
	kcsQuery := func(ctx context.Context, values url.Values) (string, error) {
		token, err := accessToken(ctx)
		if err != nil {
			return "", err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, kbURL+"?"+values.Encode(), nil)
		if err != nil {
			return "", err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := source.Client.Do(req)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxDocBytes))
		if err != nil {
			return "", err
		}
		if resp.StatusCode != http.StatusOK {
			return "", fmt.Errorf("%s: %s", source.Name, resp.Status)
		}
		var decoded struct {
			Response struct {
				Docs []map[string]any `json:"docs"`
			} `json:"response"`
		}
		if err := json.Unmarshal(body, &decoded); err != nil {
			return "", fmt.Errorf("%s: %w", source.Name, err)
		}
		if len(decoded.Response.Docs) == 0 {
			return "no results", nil
		}
		var b strings.Builder
		for _, doc := range decoded.Response.Docs {
			for _, key := range []string{"id", "documentKind", "title", "view_uri", "abstract",
				"issue", "solution_environment", "solution_rootcause", "solution_resolution", "solution_diagnosticsteps"} {
				if value := kcsField(doc, key); value != "" {
					fmt.Fprintf(&b, "%s: %s\n", key, value)
				}
			}
			b.WriteString("\n")
		}
		return strings.TrimSpace(b.String()), nil
	}
	querySchema := map[string]any{
		"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}},
		"required": []string{"query"}, "additionalProperties": false,
	}
	idSchema := map[string]any{
		"type": "object", "properties": map[string]any{"id": map[string]any{"type": "string"}},
		"required": []string{"id"}, "additionalProperties": false,
	}
	if err := r.add("search_"+source.Name+"_kb",
		"Search the Red Hat Knowledge Base for solutions and articles. Fetch full content with get_"+source.Name+"_kb.",
		querySchema, false, func(ctx context.Context, args map[string]any) (string, error) {
			return kcsQuery(ctx, url.Values{"q": {fmt.Sprint(args["query"])}, "rows": {"10"}, "fl": {redhatSearchFields}})
		}); err != nil {
		return err
	}
	if err := r.add("get_"+source.Name+"_kb",
		"Get a Red Hat Knowledge Base article by id: environment, issue, root cause, resolution, diagnostic steps.",
		idSchema, false, func(ctx context.Context, args map[string]any) (string, error) {
			return kcsQuery(ctx, url.Values{"q": {"id:" + fmt.Sprint(args["id"])}, "fl": {redhatDetailFields}})
		}); err != nil {
		return err
	}
	return r.add("search_"+source.Name+"_docs",
		"Search Red Hat product documentation.",
		querySchema, false, func(ctx context.Context, args map[string]any) (string, error) {
			return kcsQuery(ctx, url.Values{
				"q": {fmt.Sprint(args["query"])}, "rows": {"10"}, "fl": {redhatSearchFields},
				"fq": {`documentKind:"Documentation"`},
			})
		})
}

// kcsField renders a Solr field that may be a string or a multi-valued list.
func kcsField(doc map[string]any, key string) string {
	switch value := doc[key].(type) {
	case string:
		return value
	case []any:
		parts := make([]string, 0, len(value))
		for _, item := range value {
			parts = append(parts, fmt.Sprint(item))
		}
		return strings.Join(parts, " ")
	case nil:
		return ""
	default:
		return fmt.Sprint(value)
	}
}

func urlAllowed(raw string, sites []string) bool {
	for _, site := range sites {
		site = strings.TrimRight(site, "/")
		if raw == site || strings.HasPrefix(raw, site+"/") {
			return true
		}
	}
	return false
}

// htmlToText is deliberately crude: tags become whitespace, script and style
// bodies are dropped, entities decode, whitespace collapses. Docs pages come
// out as readable running text; anything fancier would be a dependency.
func htmlToText(raw string) string {
	var b strings.Builder
	for i := 0; i < len(raw); {
		if raw[i] != '<' {
			next := strings.IndexByte(raw[i:], '<')
			if next < 0 {
				b.WriteString(raw[i:])
				break
			}
			b.WriteString(raw[i : i+next])
			i += next
			continue
		}
		end := strings.IndexByte(raw[i:], '>')
		if end < 0 {
			break
		}
		tag := strings.ToLower(strings.TrimSpace(raw[i+1 : i+end]))
		i += end + 1
		name, _, _ := strings.Cut(tag, " ")
		if name == "script" || name == "style" {
			close := strings.Index(strings.ToLower(raw[i:]), "</"+name)
			if close < 0 {
				break
			}
			i += close
			continue
		}
		b.WriteByte(' ')
	}
	return strings.Join(strings.Fields(html.UnescapeString(b.String())), " ")
}
