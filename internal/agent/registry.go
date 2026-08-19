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

type Registry struct{ tools map[string]toolEntry }

func NewRegistry(ctx context.Context, sessions []*mcp.ClientSession, thanosClient *http.Client, thanosURL string) (*Registry, error) {
	r := &Registry{tools: make(map[string]toolEntry)}
	for _, session := range sessions {
		listed, err := session.ListTools(ctx, nil)
		if err != nil {
			return nil, fmt.Errorf("list MCP tools: %w", err)
		}
		for _, tool := range listed.Tools {
			tool := tool
			if err := r.add(tool.Name, tool.Description, tool.InputSchema, tool.Name == "run_host_command" || tool.Name == "run_kubectl_command", func(ctx context.Context, args map[string]any) (string, error) {
				result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: tool.Name, Arguments: args})
				if err != nil {
					return "", err
				}
				return mcpResultText(result), nil
			}); err != nil {
				return nil, err
			}
		}
	}
	if thanosClient != nil {
		if err := r.addNativeThanos(thanosClient, strings.TrimRight(thanosURL, "/")); err != nil {
			return nil, err
		}
	}
	return r, nil
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

func (r *Registry) addNativeThanos(client *http.Client, baseURL string) error {
	instant := map[string]any{
		"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}},
		"required": []string{"query"}, "additionalProperties": false,
	}
	if err := r.add("query_prometheus_instant", "Run an instant PromQL query against lab metrics.", instant, false, func(ctx context.Context, args map[string]any) (string, error) {
		return queryThanos(ctx, client, baseURL+"/api/v1/query", args)
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
	return r.add("query_prometheus_range", "Run a range PromQL query against lab metrics. start and end must be RFC3339 timestamps or unix seconds; relative expressions like now-6h are NOT accepted. step is a duration such as 5m.", rangeSchema, false, func(ctx context.Context, args map[string]any) (string, error) {
		return queryThanos(ctx, client, baseURL+"/api/v1/query_range", args)
	})
}

func queryThanos(ctx context.Context, client *http.Client, endpoint string, args map[string]any) (string, error) {
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
		return "", fmt.Errorf("Thanos: %s: %s", resp.Status, body)
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
