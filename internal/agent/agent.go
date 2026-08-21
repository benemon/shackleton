package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/openai/openai-go/v3"
)

// SystemPrompt assembles the system prompt from the operator-authored
// preamble (role + judgement, from agent.prompt in the config), the
// environment section generated from inventory data, and the fixed
// behavioral contract, with tool names drawn from what is actually
// registered. The denial sentence is a hard requirement: the model must
// report an operator deny as a decision, not invent a rationale for it.
func SystemPrompt(preamble, environment string, metricsTools, logsTools, gatedTools []string) string {
	var b strings.Builder
	if preamble == "" {
		preamble = "You are an infrastructure investigation agent."
	}
	b.WriteString(preamble)
	if environment != "" {
		b.WriteString("\n" + environment + "\n")
	}
	if len(metricsTools) > 0 {
		b.WriteString(" " + strings.Join(metricsTools, " and ") + " are the ONLY way to read metrics.")
	}
	if len(logsTools) > 0 {
		b.WriteString(" Use " + strings.Join(logsTools, " and ") + " to search logs when corroborating a finding.")
	}
	if len(gatedTools) == 1 {
		b.WriteString(" The gated tool " + gatedTools[0] + " is for APPLYING an approved change, never for lookups; prefer auto-approved read tools.")
	} else if len(gatedTools) > 1 {
		b.WriteString(" The gated tools " + strings.Join(gatedTools, " and ") + " are for APPLYING an approved change, never for lookups; prefer auto-approved read tools.")
	}
	b.WriteString(" Do not repeat a lookup you already ran. If the operator denies a proposed action, report the denial as an operator decision and do not invent a reason for it. If an approved action executes, re-run the check that motivated it and state whether the symptom cleared. After a few tool calls, stop and give your best concise answer.")
	b.WriteString(" End your final answer with a fenced json block of exactly this shape:\n```json\n{\"verdict\":\"healthy\",\"summary\":\"<one line>\",\"evidence\":[\"<item>\"]}\n```\nverdict must be healthy, attention, or action. Use healthy only when nothing needs attention. If an approved action was executed, also include \"resolution\":\"cleared\" or \"resolution\":\"persisting\" in the block, based on re-checking the signal that motivated the action.")
	return b.String()
}

type ToolCall struct {
	Name     string         `json:"name"`
	ArgsJSON string         `json:"args_json"`
	Args     map[string]any `json:"args"`
	ID       string         `json:"id"`
	Human    string         `json:"human"`
}

type Decision struct {
	Approved bool
	Via      string
}

type Approver interface {
	RequestApproval(ctx context.Context, call ToolCall) (Decision, error)
}

// TargetResolver validates a gated call's host target against the inventory
// before any approval is requested, and maps aliases to the canonical name.
type TargetResolver interface {
	ResolveTarget(target string) (string, bool)
	KnownTargets() []string
}

type Notifier interface {
	Send(ctx context.Context, text string) error
}

type EventSink interface {
	Emit(eventType string, payload any)
}

type Metrics struct {
	Rounds         int           `json:"rounds"`
	ToolCallsTotal int           `json:"tool_calls_total"`
	MalformedJSON  int           `json:"malformed_json"`
	SchemaInvalid  int           `json:"schema_invalid"`
	UnknownTool    int           `json:"unknown_tool"`
	ToolErrors     int           `json:"tool_errors"`
	WrongFirstTool bool          `json:"wrong_first_tool"`
	Denied         int           `json:"denied"`
	Recovered      int           `json:"recovered"`
	Duration       time.Duration `json:"duration"`
	Completed      bool          `json:"completed"`
	Answer         string        `json:"answer"`
}

type ModelToolCall struct {
	Name      string
	Arguments string
	ID        string
}

type ModelMessage struct {
	Content   string
	ToolCalls []ModelToolCall
}

type CompletionFunc func(context.Context, []openai.ChatCompletionMessageParamUnion, []openai.ChatCompletionToolUnionParam) (ModelMessage, error)

type Runner struct {
	Complete             CompletionFunc
	Tools                *Registry
	Approver             Approver
	Targets              TargetResolver
	Notifier             Notifier
	Prompt               string
	MaxRounds            int
	MaxMalformedRetries  int
	MaxToolResult        int
	CallTimeout          time.Duration
	InvestigationTimeout time.Duration
	Events               EventSink
}

func (r *Runner) Run(ctx context.Context, question, expectFirstTool string) (metrics Metrics, err error) {
	started := time.Now()
	defer func() { metrics.Duration = time.Since(started) }()
	parentCtx := ctx
	var investigationDeadline time.Time
	if r.InvestigationTimeout > 0 {
		investigationDeadline = time.Now().Add(r.InvestigationTimeout)
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, investigationDeadline)
		defer cancel()
	}
	wallClockExceeded := func() bool {
		return !investigationDeadline.IsZero() && !time.Now().Before(investigationDeadline) &&
			errors.Is(ctx.Err(), context.DeadlineExceeded) && parentCtx.Err() == nil
	}
	finishWallClock := func() (Metrics, error) {
		metrics.Completed = false
		metrics.Answer = "wall clock exceeded"
		return metrics, nil
	}
	maxRounds := r.MaxRounds
	if maxRounds == 0 {
		maxRounds = 8
	}
	maxMalformed := r.MaxMalformedRetries
	if maxMalformed == 0 {
		maxMalformed = 3
	}
	maxToolResult := r.MaxToolResult
	if maxToolResult == 0 {
		maxToolResult = 30000
	}
	callTimeout := r.CallTimeout
	if callTimeout == 0 {
		callTimeout = 30 * time.Second
	}
	prompt := r.Prompt
	if prompt == "" {
		prompt = SystemPrompt("", "", nil, nil, nil)
	}
	messages := []openai.ChatCompletionMessageParamUnion{
		openai.SystemMessage(prompt + " The current time is " + time.Now().UTC().Format(time.RFC3339) + "."),
		openai.UserMessage(question),
	}
	malformed := make(map[string]int)
	first := true

	for metrics.Rounds < maxRounds {
		metrics.Rounds++
		if metrics.Rounds == maxRounds {
			messages = append(messages, openai.UserMessage("Budget check: this is your final round. Do not call more tools unless strictly necessary - give your best concise answer from what you already know."))
		}
		msg, completeErr := r.Complete(ctx, messages, r.Tools.OpenAITools())
		if completeErr != nil {
			if wallClockExceeded() {
				return finishWallClock()
			}
			return metrics, completeErr
		}
		messages = append(messages, assistantMessage(msg))
		if len(msg.ToolCalls) == 0 {
			metrics.Completed = true
			metrics.Answer = msg.Content
			if r.Notifier != nil {
				if notifyErr := r.Notifier.Send(ctx, msg.Content); notifyErr != nil {
					if wallClockExceeded() {
						return finishWallClock()
					}
					return metrics, notifyErr
				}
			}
			return metrics, nil
		}

		for _, raw := range msg.ToolCalls {
			metrics.ToolCallsTotal++
			if first {
				metrics.WrongFirstTool = expectFirstTool != "" && raw.Name != expectFirstTool
				first = false
			}
			result := r.handleCall(ctx, raw, callTimeout, &metrics)
			r.emit("tool_call", struct {
				Round         int    `json:"round"`
				Name          string `json:"name"`
				Args          any    `json:"args"`
				ResultSnippet string `json:"result_snippet"`
				Error         bool   `json:"error"`
			}{metrics.Rounds, raw.Name, result.args, truncateRunes(result.text, 2000), result.errored})
			if wallClockExceeded() {
				return finishWallClock()
			}
			if result.malformed {
				malformed[raw.Name]++
				if malformed[raw.Name] > maxMalformed {
					return metrics, fmt.Errorf("tool %s exceeded malformed retry limit", raw.Name)
				}
			} else if malformed[raw.Name] > 0 {
				metrics.Recovered += malformed[raw.Name]
				delete(malformed, raw.Name)
			}
			// An unbounded result (a broad PromQL query can return megabytes)
			// would blow the model's context window and kill the whole
			// investigation; cap it and tell the model to narrow.
			text := result.text
			if runes := []rune(text); len(runes) > maxToolResult {
				text = string(runes[:maxToolResult]) +
					fmt.Sprintf("\n…[truncated: the result was %d characters, showing the first %d. Narrow the query.]", len(runes), maxToolResult)
			}
			messages = append(messages, openai.ToolMessage(text, raw.ID))
		}
	}
	metrics.Answer = "round limit reached"
	return metrics, nil
}

type callResult struct {
	text      string
	args      any
	malformed bool
	errored   bool
}

func (r *Runner) handleCall(ctx context.Context, raw ModelToolCall, timeout time.Duration, metrics *Metrics) callResult {
	entry, ok := r.Tools.tools[raw.Name]
	if !ok {
		metrics.UnknownTool++
		return callResult{fmt.Sprintf("Tool error: unknown tool %q. Use one of the declared tools and retry.", raw.Name), raw.Arguments, true, true}
	}
	var decoded any
	if err := json.Unmarshal([]byte(raw.Arguments), &decoded); err != nil {
		metrics.MalformedJSON++
		return callResult{fmt.Sprintf("Tool argument error for %s: invalid JSON: %v. Correct the JSON and retry.", raw.Name, err), raw.Arguments, true, true}
	}
	if err := entry.schema.Validate(decoded); err != nil {
		metrics.SchemaInvalid++
		return callResult{fmt.Sprintf("Tool argument error for %s: JSON schema validation failed: %v. Correct the arguments and retry.", raw.Name, err), decoded, true, true}
	}
	args, ok := decoded.(map[string]any)
	if !ok {
		metrics.SchemaInvalid++
		return callResult{fmt.Sprintf("Tool argument error for %s: JSON schema validation failed: arguments must be an object. Correct the arguments and retry.", raw.Name), decoded, true, true}
	}
	// Pre-flight target validation: a gated call against a host outside the
	// inventory is rejected before any approval is minted, and an alias is
	// rewritten to the canonical name so the approver and the executor see
	// the same target.
	if entry.gated && r.Targets != nil {
		if target, ok := args["host"].(string); ok {
			canonical, known := r.Targets.ResolveTarget(target)
			if !known {
				metrics.ToolErrors++
				return callResult{fmt.Sprintf("Tool error: host %q is not in the inventory; known hosts: %s.", target, strings.Join(r.Targets.KnownTargets(), ", ")), args, false, true}
			}
			if canonical != target {
				args["host"] = canonical
				if encoded, err := json.Marshal(args); err == nil {
					raw.Arguments = string(encoded)
				}
			}
		}
	}
	call := ToolCall{Name: raw.Name, ArgsJSON: raw.Arguments, Args: args, ID: raw.ID, Human: humanRendering(raw.Name, args)}
	if entry.gated {
		if r.Approver == nil {
			return callResult{"Tool error: approval is required but no approver is configured.", args, false, true}
		}
		r.emit("approval_requested", struct {
			CallID string `json:"call_id"`
			Name   string `json:"name"`
			Human  string `json:"human"`
		}{call.ID, call.Name, call.Human})
		decision, err := r.Approver.RequestApproval(ctx, call)
		if err != nil {
			return callResult{"Tool error: approval failed: " + err.Error(), args, false, true}
		}
		r.emit("approval_decided", struct {
			CallID   string `json:"call_id"`
			Approved bool   `json:"approved"`
			Via      string `json:"via"`
		}{call.ID, decision.Approved, decision.Via})
		if !decision.Approved {
			metrics.Denied++
			return callResult{"denied by operator", args, false, false}
		}
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	result, err := entry.call(callCtx, args)
	if err != nil {
		metrics.ToolErrors++
		return callResult{"Tool error: " + err.Error(), args, false, true}
	}
	return callResult{result, args, false, false}
}

func (r *Runner) emit(eventType string, payload any) {
	if r.Events != nil {
		r.Events.Emit(eventType, payload)
	}
}

func truncateRunes(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}

func assistantMessage(msg ModelMessage) openai.ChatCompletionMessageParamUnion {
	p := openai.ChatCompletionAssistantMessageParam{}
	if msg.Content != "" {
		p.Content.OfString = openai.String(msg.Content)
	}
	for _, tc := range msg.ToolCalls {
		call := openai.ChatCompletionMessageFunctionToolCallParam{
			ID: tc.ID,
			Function: openai.ChatCompletionMessageFunctionToolCallFunctionParam{
				Name: tc.Name, Arguments: tc.Arguments,
			},
		}
		p.ToolCalls = append(p.ToolCalls, openai.ChatCompletionMessageToolCallUnionParam{OfFunction: &call})
	}
	return openai.ChatCompletionMessageParamUnion{OfAssistant: &p}
}

func StreamCompleter(client openai.Client, model string) CompletionFunc {
	return func(ctx context.Context, messages []openai.ChatCompletionMessageParamUnion, tools []openai.ChatCompletionToolUnionParam) (ModelMessage, error) {
		stream := client.Chat.Completions.NewStreaming(ctx, openai.ChatCompletionNewParams{Model: model, Messages: messages, Tools: tools})
		acc := openai.ChatCompletionAccumulator{}
		for stream.Next() {
			acc.AddChunk(stream.Current())
		}
		if err := stream.Err(); err != nil {
			return ModelMessage{}, fmt.Errorf("stream: %w", err)
		}
		if len(acc.Choices) == 0 {
			return ModelMessage{}, fmt.Errorf("stream: no choices accumulated")
		}
		message := acc.Choices[0].Message
		result := ModelMessage{Content: message.Content}
		for _, tc := range message.ToolCalls {
			if tc.Function.Name != "" {
				result.ToolCalls = append(result.ToolCalls, ModelToolCall{Name: tc.Function.Name, Arguments: tc.Function.Arguments, ID: tc.ID})
			}
		}
		return result, nil
	}
}

type CLIApprover struct{ approve bool }

func NewCLIApprover(approve bool) *CLIApprover { return &CLIApprover{approve: approve} }

func (a *CLIApprover) RequestApproval(context.Context, ToolCall) (Decision, error) {
	return Decision{Approved: a.approve, Via: "cli"}, nil
}

func humanRendering(name string, args map[string]any) string {
	switch name {
	case "run_kubectl_command":
		if values, ok := args["args"].([]any); ok {
			argv := make([]string, 0, len(values))
			for _, value := range values {
				argv = append(argv, fmt.Sprint(value))
			}
			return "kubectl " + strings.Join(argv, " ")
		}
	case "run_host_command":
		if values, ok := args["command"].([]any); ok {
			argv := make([]string, 0, len(values))
			for _, value := range values {
				argv = append(argv, fmt.Sprint(value))
			}
			return fmt.Sprintf("%v: %s", args["host"], strings.Join(argv, " "))
		}
		return fmt.Sprintf("%v: %v", args["host"], args["command"])
	}
	b, _ := json.Marshal(args)
	return name + " " + string(b)
}
