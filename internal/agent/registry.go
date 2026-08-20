package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/shared"
	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
)

type toolEntry struct {
	description string
	parameters  shared.FunctionParameters
	schema      *jsonschema.Schema
	gated       bool
	call        func(context.Context, map[string]any) (string, error)
}

type Registry struct {
	tools    map[string]toolEntry
	sessions []*reconnectingSession
}

type MCPSession interface {
	ListTools(context.Context, *mcp.ListToolsParams) (*mcp.ListToolsResult, error)
	CallTool(context.Context, *mcp.CallToolParams) (*mcp.CallToolResult, error)
	Ping(context.Context, *mcp.PingParams) error
	Close() error
}

type MCPConnectFunc func(context.Context) (MCPSession, error)

type MCPServer struct {
	Name    string
	Connect MCPConnectFunc
}

type reconnectingSession struct {
	mu      sync.Mutex
	connect MCPConnectFunc
	session MCPSession
}

func NewRegistry(ctx context.Context, servers []MCPServer, gatedTools map[string]bool, promClient *http.Client, promURL string) (*Registry, error) {
	r := &Registry{tools: make(map[string]toolEntry)}
	for _, server := range servers {
		session, err := server.Connect(ctx)
		if err != nil {
			r.Close()
			return nil, fmt.Errorf("%s: connect: %w", server.Name, err)
		}
		reconnecting := &reconnectingSession{connect: server.Connect, session: session}
		listed, err := reconnecting.listTools(ctx)
		if err != nil {
			reconnecting.close()
			r.Close()
			return nil, fmt.Errorf("%s: list MCP tools: %w", server.Name, err)
		}
		for _, tool := range listed.Tools {
			// A gated call is never re-executed: after an ambiguous transport
			// death the original may already have run, and mutations must not
			// run twice. The session still reconnects for subsequent calls.
			retry := !gatedTools[tool.Name]
			if err := r.add(tool.Name, tool.Description, tool.InputSchema, gatedTools[tool.Name], func(ctx context.Context, args map[string]any) (string, error) {
				result, err := reconnecting.callTool(ctx, &mcp.CallToolParams{Name: tool.Name, Arguments: args}, retry)
				if err != nil {
					return "", err
				}
				return mcpResultText(result), nil
			}); err != nil {
				reconnecting.close()
				r.Close()
				return nil, err
			}
		}
		r.sessions = append(r.sessions, reconnecting)
	}
	if promClient != nil {
		if err := r.addNativePrometheus(promClient, strings.TrimRight(promURL, "/")); err != nil {
			r.Close()
			return nil, err
		}
	}
	return r, nil
}

func (r *Registry) Close() error {
	var first error
	for _, session := range r.sessions {
		if err := session.close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func (s *reconnectingSession) listTools(ctx context.Context) (*mcp.ListToolsResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.session.ListTools(ctx, nil)
}

func (s *reconnectingSession) callTool(ctx context.Context, params *mcp.CallToolParams, retry bool) (*mcp.CallToolResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	result, originalErr := s.session.CallTool(ctx, params)
	if originalErr == nil {
		return result, nil
	}
	if s.session.Ping(ctx, nil) == nil {
		return nil, originalErr
	}
	replacement, err := s.connect(ctx)
	if err != nil {
		return nil, originalErr
	}
	_ = s.session.Close()
	s.session = replacement
	if !retry {
		return nil, originalErr
	}
	result, err = s.session.CallTool(ctx, params)
	if err != nil {
		return nil, originalErr
	}
	return result, nil
}

func (s *reconnectingSession) close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.session.Close()
}

func (r *Registry) OpenAITools() []openai.ChatCompletionToolUnionParam {
	tools := make([]openai.ChatCompletionToolUnionParam, 0, len(r.tools))
	names := make([]string, 0, len(r.tools))
	for name := range r.tools {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		entry := r.tools[name]
		tools = append(tools, openai.ChatCompletionFunctionTool(shared.FunctionDefinitionParam{
			Name: name, Description: openai.String(entry.description), Parameters: entry.parameters,
		}))
	}
	return tools
}

func (r *Registry) add(name, description string, inputSchema any, gated bool, call func(context.Context, map[string]any) (string, error)) error {
	b, err := json.Marshal(inputSchema)
	if err != nil {
		return fmt.Errorf("tool %s schema: %w", name, err)
	}
	var parameters shared.FunctionParameters
	if err := json.Unmarshal(b, &parameters); err != nil {
		return fmt.Errorf("tool %s OpenAI schema: %w", name, err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
	if err != nil {
		return fmt.Errorf("tool %s schema: %w", name, err)
	}
	compiler := jsonschema.NewCompiler()
	location := "mem://" + url.PathEscape(name) + ".json"
	if err := compiler.AddResource(location, doc); err != nil {
		return fmt.Errorf("tool %s schema: %w", name, err)
	}
	compiled, err := compiler.Compile(location)
	if err != nil {
		return fmt.Errorf("tool %s schema: %w", name, err)
	}
	r.tools[name] = toolEntry{description: description, parameters: parameters, schema: compiled, gated: gated, call: call}
	return nil
}

func (r *Registry) addNativePrometheus(client *http.Client, baseURL string) error {
	instant := map[string]any{
		"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}},
		"required": []string{"query"}, "additionalProperties": false,
	}
	if err := r.add("query_prometheus_instant", "Run an instant PromQL query.", instant, false, func(ctx context.Context, args map[string]any) (string, error) {
		return queryPrometheus(ctx, client, baseURL+"/api/v1/query", args)
	}); err != nil {
		return err
	}
	rangeSchema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{"type": "string"}, "start": map[string]any{"type": "string"},
			"end": map[string]any{"type": "string"}, "step": map[string]any{"type": "string"},
		},
		"required": []string{"query", "start", "end", "step"}, "additionalProperties": false,
	}
	return r.add("query_prometheus_range", "Run a range PromQL query. start and end must be RFC3339 timestamps or unix seconds; relative expressions like now-6h are NOT accepted. step is a duration such as 5m.", rangeSchema, false, func(ctx context.Context, args map[string]any) (string, error) {
		return queryPrometheus(ctx, client, baseURL+"/api/v1/query_range", args)
	})
}

func queryPrometheus(ctx context.Context, client *http.Client, endpoint string, args map[string]any) (string, error) {
	values := url.Values{}
	for key, value := range args {
		values.Set(key, fmt.Sprint(value))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?"+values.Encode(), nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("prometheus: %s: %s", resp.Status, body)
	}
	return string(body), nil
}

func mcpResultText(result *mcp.CallToolResult) string {
	var parts []string
	for _, content := range result.Content {
		if text, ok := content.(*mcp.TextContent); ok {
			parts = append(parts, text.Text)
			continue
		}
		if b, err := content.MarshalJSON(); err == nil {
			parts = append(parts, string(b))
		}
	}
	if len(parts) == 0 && result.StructuredContent != nil {
		b, _ := json.Marshal(result.StructuredContent)
		return string(b)
	}
	return strings.Join(parts, "\n")
}
