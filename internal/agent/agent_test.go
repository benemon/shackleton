package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/openai/openai-go/v3"
)

func testRegistry(t *testing.T, called *int) *Registry {
	t.Helper()
	r := &Registry{tools: make(map[string]toolEntry)}
	schema := map[string]any{
		"type":                 "object",
		"properties":           map[string]any{"query": map[string]any{"type": "string"}},
		"required":             []string{"query"},
		"additionalProperties": false,
	}
	err := r.add("query_prometheus_instant", "test", schema, false, func(context.Context, map[string]any) (string, error) {
		*called++
		return "result", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestSchemaValidationAndMalformedRecovery(t *testing.T) {
	called := 0
	messages := []ModelMessage{
		{ToolCalls: []ModelToolCall{{Name: "query_prometheus_instant", Arguments: `{"wrong":"node_load5"}`, ID: "1"}}},
		{ToolCalls: []ModelToolCall{{Name: "query_prometheus_instant", Arguments: `{"query":`, ID: "2"}}},
		{ToolCalls: []ModelToolCall{{Name: "query_prometheus_instant", Arguments: `{"query":"node_load5"}`, ID: "3"}}},
		{Content: "done"},
	}
	index := 0
	runner := Runner{
		Tools: testRegistry(t, &called),
		Complete: func(_ context.Context, history []openai.ChatCompletionMessageParamUnion, _ []openai.ChatCompletionToolUnionParam) (ModelMessage, error) {
			if index == 1 {
				toolText := history[len(history)-1].OfTool.Content.OfString.Value
				if !strings.Contains(toolText, "schema validation failed") {
					t.Fatalf("schema failure was not returned to model: %q", toolText)
				}
			}
			result := messages[index]
			index++
			return result, nil
		},
	}
	metrics, err := runner.Run(context.Background(), "question", "query_prometheus_instant")
	if err != nil {
		t.Fatal(err)
	}
	if called != 1 {
		t.Fatalf("tool executed %d times, want 1", called)
	}
	if metrics.SchemaInvalid != 1 || metrics.MalformedJSON != 1 || metrics.Recovered != 2 {
		t.Fatalf("unexpected malformed metrics: %+v", metrics)
	}
	if metrics.ToolCallsTotal != 3 || metrics.Rounds != 4 || !metrics.Completed || metrics.Answer != "done" {
		t.Fatalf("unexpected run metrics: %+v", metrics)
	}
}

func TestRoundCapTerminatesToolSequence(t *testing.T) {
	called := 0
	runner := Runner{
		Tools:     testRegistry(t, &called),
		MaxRounds: 2,
		Complete: func(context.Context, []openai.ChatCompletionMessageParamUnion, []openai.ChatCompletionToolUnionParam) (ModelMessage, error) {
			return ModelMessage{ToolCalls: []ModelToolCall{{Name: "query_prometheus_instant", Arguments: `{"query":"up"}`, ID: "call"}}}, nil
		},
	}
	metrics, err := runner.Run(context.Background(), "question", "other_tool")
	if err != nil {
		t.Fatal(err)
	}
	if metrics.Rounds != 2 || metrics.ToolCallsTotal != 2 || metrics.Completed || metrics.Answer != "round limit reached" {
		t.Fatalf("round cap did not terminate correctly: %+v", metrics)
	}
	if !metrics.WrongFirstTool || called != 2 {
		t.Fatalf("unexpected first-tool/execution metrics: %+v, called=%d", metrics, called)
	}
}

func TestPerCallTimeout(t *testing.T) {
	r := &Registry{tools: make(map[string]toolEntry)}
	err := r.add("slow", "test", map[string]any{"type": "object"}, false, func(ctx context.Context, _ map[string]any) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	index := 0
	runner := Runner{
		Tools: r, CallTimeout: time.Millisecond,
		Complete: func(_ context.Context, history []openai.ChatCompletionMessageParamUnion, _ []openai.ChatCompletionToolUnionParam) (ModelMessage, error) {
			index++
			if index == 1 {
				return ModelMessage{ToolCalls: []ModelToolCall{{Name: "slow", Arguments: `{}`, ID: "1"}}}, nil
			}
			text := history[len(history)-1].OfTool.Content.OfString.Value
			if !strings.Contains(text, "deadline exceeded") {
				t.Fatalf("timeout not reflected to model: %q", text)
			}
			return ModelMessage{Content: "finished"}, nil
		},
	}
	metrics, err := runner.Run(context.Background(), "question", "")
	if err != nil || !metrics.Completed {
		t.Fatalf("run failed: metrics=%+v err=%v", metrics, err)
	}
}
