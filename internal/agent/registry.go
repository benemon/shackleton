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

// NativeSource is a metrics or logs query API instance backed by an HTTP
// client that already carries its auth.
type NativeSource struct {
	Name    string
	Client  *http.Client
	BaseURL string
}

type reconnectingSession struct {
	mu      sync.Mutex
	connect MCPConnectFunc
	session MCPSession
}

func NewRegistry(ctx context.Context, servers []MCPServer, gatedTools map[string]bool, metrics, logs []NativeSource) (*Registry, error) {
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
				return nil, fmt.Errorf("%s: %w", server.Name, err)
			}
		}
		r.sessions = append(r.sessions, reconnecting)
	}
	for _, source := range metrics {
		if err := r.addNativePrometheus(source); err != nil {
			r.Close()
			return nil, err
		}
	}
	for _, source := range logs {
		if err := r.addNativeLoki(source); err != nil {
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
	// A gated call gets exactly one submission (mutations must never run
	// twice), so it must not spend that submission on a session that idled
	// out while the operator decided: prove the session alive and replace a
	// dead one before submitting. Ungated calls skip the ping — their
	// post-failure retry below already covers a dead session.
	if !retry && s.session.Ping(ctx, nil) != nil {
		if replacement, err := s.connect(ctx); err == nil {
			_ = s.session.Close()
			s.session = replacement
		}
	}
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
	// Last-wins would silently shadow one server's tool with another's and
	// make gating by name ambiguous.
	if _, exists := r.tools[name]; exists {
		return fmt.Errorf("duplicate tool %q", name)
	}
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

func (r *Registry) addNativePrometheus(source NativeSource) error {
	instant := map[string]any{
		"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}},
		"required": []string{"query"}, "additionalProperties": false,
	}
	if err := r.add("query_"+source.Name+"_instant", "Run an instant PromQL query.", instant, false, func(ctx context.Context, args map[string]any) (string, error) {
		values := url.Values{}
		for key, value := range args {
			values.Set(key, fmt.Sprint(value))
		}
		return doGet(ctx, source, "/api/v1/query", values)
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
	return r.add("query_"+source.Name+"_range", "Run a range PromQL query. start and end must be RFC3339 timestamps or unix seconds; relative expressions like now-6h are NOT accepted. step is a duration such as 5m.", rangeSchema, false, func(ctx context.Context, args map[string]any) (string, error) {
		values := url.Values{}
		for key, value := range args {
			values.Set(key, fmt.Sprint(value))
		}
		return doGet(ctx, source, "/api/v1/query_range", values)
	})
}

func (r *Registry) addNativeLoki(source NativeSource) error {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{"type": "string"}, "start": map[string]any{"type": "string"},
			"end": map[string]any{"type": "string"}, "limit": map[string]any{"type": "integer"},
		},
		"required": []string{"query", "start", "end"}, "additionalProperties": false,
	}
	return r.add("query_"+source.Name+"_logs", "Run a LogQL range query and return matching log lines, newest first. start and end must be RFC3339 timestamps or unix seconds; relative expressions like now-6h are NOT accepted. limit caps returned lines (default 100).", schema, false, func(ctx context.Context, args map[string]any) (string, error) {
		values := url.Values{"direction": {"backward"}, "limit": {"100"}}
		for key, value := range args {
			values.Set(key, fmt.Sprint(value))
		}
		return doGet(ctx, source, "/loki/api/v1/query_range", values)
	})
}

func doGet(ctx context.Context, source NativeSource, path string, values url.Values) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, source.BaseURL+path+"?"+values.Encode(), nil)
	if err != nil {
		return "", err
	}
	resp, err := source.Client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s: %s: %s", source.Name, resp.Status, body)
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
