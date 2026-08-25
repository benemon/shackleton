package telegram

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/benemon/shackleton/internal/agent"
	"github.com/benemon/shackleton/internal/kb"
	"github.com/benemon/shackleton/internal/service"
	"github.com/benemon/shackleton/internal/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/openai/openai-go/v3"
)

type telegramCall struct {
	method    string
	payload   map[string]any
	messageID int64
}

type telegramRecorder struct {
	mu            sync.Mutex
	calls         []telegramCall
	nextMessageID int64
	updates       []update
	rejectHTML    bool
}

func testBot(t *testing.T) (*bot, *telegramRecorder) {
	t.Helper()
	recorder := &telegramRecorder{nextMessageID: 41}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		_ = json.NewDecoder(r.Body).Decode(&payload)
		recorder.mu.Lock()
		if strings.HasSuffix(r.URL.Path, "/getUpdates") {
			updates := recorder.updates
			recorder.updates = nil
			recorder.calls = append(recorder.calls, telegramCall{method: r.URL.Path, payload: payload})
			recorder.mu.Unlock()
			if len(updates) == 0 {
				select {
				case <-r.Context().Done():
					return
				case <-time.After(10 * time.Millisecond):
				}
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": updates})
			return
		}
		messageID := int64(0)
		if strings.HasSuffix(r.URL.Path, "/sendMessage") {
			recorder.nextMessageID++
			messageID = recorder.nextMessageID
		}
		recorder.calls = append(recorder.calls, telegramCall{r.URL.Path, payload, messageID})
		rejectHTML := recorder.rejectHTML && payload["parse_mode"] == "HTML"
		recorder.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if rejectHTML {
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "description": "can't parse entities"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"message_id": messageID}})
	}))
	t.Cleanup(server.Close)
	return &bot{client: server.Client(), baseURL: server.URL}, recorder
}

func testAdapter(t *testing.T) (*Adapter, *telegramRecorder) {
	t.Helper()
	client, recorder := testBot(t)
	return &Adapter{bot: client, chatID: 7, pending: make(map[string]*pendingApproval)}, recorder
}

func (r *telegramRecorder) matching(method string) []telegramCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	var result []telegramCall
	for _, call := range r.calls {
		if strings.HasSuffix(call.method, "/"+method) {
			result = append(result, call)
		}
	}
	return result
}

func waitForTelegramCalls(t *testing.T, recorder *telegramRecorder, method string, count int) []telegramCall {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		calls := recorder.matching(method)
		if len(calls) >= count {
			return calls
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d %s calls", count, method)
	return nil
}

func callback(id, data string, chatID, fromID, messageID int64) callbackQuery {
	result := callbackQuery{ID: id, Data: data, From: user{ID: fromID}}
	result.Message.Chat.ID = chatID
	result.Message.MessageID = messageID
	return result
}

func assertNoDecision(t *testing.T, decisions <-chan bool) {
	t.Helper()
	select {
	case decision := <-decisions:
		t.Fatalf("unexpected decision: %v", decision)
	default:
	}
}

func TestReplayedTapSettlesExactlyOnce(t *testing.T) {
	a, recorder := testAdapter(t)
	p := &pendingApproval{decision: make(chan bool, 2), messageID: 42, human: "command"}
	a.pending["nonce"] = p
	cb := callback("callback", "a:nonce", 7, 7, 42)
	a.handleCallback(context.Background(), cb)
	a.handleCallback(context.Background(), cb)
	if approved := <-p.decision; !approved {
		t.Fatal("first tap did not approve")
	}
	assertNoDecision(t, p.decision)
	if answers := recorder.matching("answerCallbackQuery"); len(answers) != 1 {
		t.Fatalf("replay was answered %d times, want 1", len(answers))
	}
}

func TestUnauthorizedTapCannotSettle(t *testing.T) {
	a, _ := testAdapter(t)
	p := &pendingApproval{decision: make(chan bool, 1), messageID: 42, human: "command"}
	a.pending["nonce"] = p
	a.handleCallback(context.Background(), callback("wrong-user", "a:nonce", 7, 8, 42))
	assertNoDecision(t, p.decision)
	a.handleCallback(context.Background(), callback("wrong-chat", "a:nonce", 8, 7, 42))
	assertNoDecision(t, p.decision)
	a.handleCallback(context.Background(), callback("authorized", "d:nonce", 7, 7, 42))
	if approved := <-p.decision; approved {
		t.Fatal("authorized denial returned approval")
	}
}

func TestStaleButtonCannotSettle(t *testing.T) {
	a, _ := testAdapter(t)
	p := &pendingApproval{decision: make(chan bool, 1), messageID: 42, human: "command"}
	a.pending["nonce"] = p
	a.handleCallback(context.Background(), callback("stale", "a:nonce", 7, 7, 41))
	assertNoDecision(t, p.decision)
	a.handleCallback(context.Background(), callback("current", "a:nonce", 7, 7, 42))
	if approved := <-p.decision; !approved {
		t.Fatal("current button did not approve")
	}
}

func TestSendTruncatesTo3800Characters(t *testing.T) {
	a, recorder := testAdapter(t)
	if err := a.Send(context.Background(), strings.Repeat("界", 3801)); err != nil {
		t.Fatal(err)
	}
	calls := recorder.matching("sendMessage")
	if got := []rune(calls[0].payload["text"].(string)); len(got) != 3800 {
		t.Fatalf("sent %d characters, want 3800", len(got))
	}
}

func TestMarkdownToTelegramHTML(t *testing.T) {
	for _, test := range []struct {
		name     string
		markdown string
		want     string
	}{
		{name: "bold", markdown: "**ready**", want: "<b>ready</b>"},
		{name: "snake_case survives", markdown: "run_kubectl_mutation restarted node_exporter", want: "run_kubectl_mutation restarted node_exporter"},
		{name: "arithmetic survives", markdown: "rate(x[5m]) * 60 * 24", want: "rate(x[5m]) * 60 * 24"},
		{name: "inline code", markdown: "run `status`", want: "run <code>status</code>"},
		{name: "fenced block", markdown: "```go\nif a < b {\n}\n```", want: "<pre>if a &lt; b {\n}</pre>"},
		{name: "table", markdown: "| Host | State |\n| --- | --- |\n| nas | up |", want: "<pre>Host | State\nnas  | up</pre>"},
		{name: "literal HTML", markdown: "literal <b> & </b>", want: "literal &lt;b&gt; &amp; &lt;/b&gt;"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := markdownToTelegramHTML(test.markdown); got != test.want {
				t.Fatalf("markdownToTelegramHTML() = %q, want %q", got, test.want)
			}
		})
	}
}

type triggerApprovalSession struct{}

func (triggerApprovalSession) ListTools(context.Context, *mcp.ListToolsParams) (*mcp.ListToolsResult, error) {
	return &mcp.ListToolsResult{Tools: []*mcp.Tool{{
		Name: "repair", Description: "repair", InputSchema: map[string]any{"type": "object"},
	}}}, nil
}

func (triggerApprovalSession) CallTool(context.Context, *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "repaired"}}}, nil
}

func (triggerApprovalSession) Ping(context.Context, *mcp.PingParams) error { return nil }
func (triggerApprovalSession) Close() error                                { return nil }

func completedFactory(t *testing.T, answer string) service.RunnerFactory {
	t.Helper()
	registry, err := agent.NewRegistry(context.Background(), nil, nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = registry.Close() })
	return func(events agent.EventSink, approver agent.Approver) *agent.Runner {
		return &agent.Runner{
			Tools: registry, Events: events, Approver: approver,
			Complete: func(context.Context, []openai.ChatCompletionMessageParamUnion, []openai.ChatCompletionToolUnionParam) (agent.ModelMessage, error) {
				return agent.ModelMessage{Content: answer}, nil
			},
		}
	}
}

func triggerApprovalFactory(t *testing.T) service.RunnerFactory {
	t.Helper()
	registry, err := agent.NewRegistry(context.Background(), []agent.MCPServer{{
		Name: "fake", Connect: func(context.Context) (agent.MCPSession, error) { return triggerApprovalSession{}, nil },
	}}, map[string]bool{"repair": true}, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = registry.Close() })
	return func(events agent.EventSink, approver agent.Approver) *agent.Runner {
		completion := 0
		return &agent.Runner{
			Tools: registry, Events: events, Approver: approver,
			Complete: func(context.Context, []openai.ChatCompletionMessageParamUnion, []openai.ChatCompletionToolUnionParam) (agent.ModelMessage, error) {
				completion++
				if completion == 1 {
					return agent.ModelMessage{ToolCalls: []agent.ModelToolCall{{Name: "repair", Arguments: `{}`, ID: "call"}}}, nil
				}
				return agent.ModelMessage{Content: "done"}, nil
			},
		}
	}
}

func testTrigger(t *testing.T, svc *service.Service) (*Trigger, *telegramRecorder, context.Context) {
	t.Helper()
	client, recorder := testBot(t)
	ctx := t.Context()
	trigger := &Trigger{bot: client, service: svc, chats: map[int64]*chatRole{7: {qa: true, approvals: true}}, messages: make(map[string][]postedApproval)}
	events, unsubscribe := svc.SubscribeApprovals()
	go func() {
		defer unsubscribe()
		trigger.followApprovals(ctx, events)
	}()
	return trigger, recorder, ctx
}

func waitForPending(t *testing.T, svc *service.Service) service.PendingApproval {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		pending := svc.ListPendingApprovals()
		if len(pending) == 1 {
			return pending[0]
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("timed out waiting for pending approval")
	return service.PendingApproval{}
}

func TestTriggerQuestionAuthorizationAndHappyPath(t *testing.T) {
	audit, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	svc := service.New(ctx, audit, nil, completedFactory(t, "answer"))
	client, recorder := testBot(t)
	recorder.mu.Lock()
	recorder.updates = []update{
		{UpdateID: 1, Message: &message{Text: "wrong chat", Chat: chat{ID: 8}, From: user{ID: 7}}},
		{UpdateID: 2, Message: &message{Text: "wrong user", Chat: chat{ID: 7}, From: user{ID: 8}}},
		{UpdateID: 3, Message: &message{Text: "/command", Chat: chat{ID: 7}, From: user{ID: 7}}},
		{UpdateID: 4, Message: &message{Text: "question", Chat: chat{ID: 7}, From: user{ID: 7}}},
	}
	recorder.mu.Unlock()
	newTrigger(ctx, client, map[int64]*chatRole{7: {qa: true, approvals: true}}, svc)
	deadline := time.Now().Add(time.Second)
	for len(svc.ListInvestigations()) != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	calls := waitForTelegramCalls(t, recorder, "sendMessage", 2)
	svc.Wait()
	if investigations := svc.ListInvestigations(); len(investigations) != 1 || investigations[0].Question != "question" || investigations[0].Trigger != "telegram" {
		t.Fatalf("investigations = %+v", investigations)
	}
	if calls[0].payload["text"] != "Investigating…" || calls[1].payload["text"] != "answer" {
		t.Fatalf("sent texts = %q, %q", calls[0].payload["text"], calls[1].payload["text"])
	}
	for _, call := range calls {
		if call.payload["chat_id"] != float64(7) {
			t.Fatalf("sendMessage chat_id = %v", call.payload["chat_id"])
		}
	}
	polls := recorder.matching("getUpdates")
	allowed, ok := polls[0].payload["allowed_updates"].([]any)
	if !ok || len(allowed) != 2 || allowed[0] != "message" || allowed[1] != "callback_query" {
		t.Fatalf("allowed_updates = %#v", polls[0].payload["allowed_updates"])
	}
}

func TestTriggerCallbackAuthorizationStalenessDenialAndVia(t *testing.T) {
	audit, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc := service.New(context.Background(), audit, nil, triggerApprovalFactory(t))
	trigger, recorder, ctx := testTrigger(t, svc)
	summary, err := svc.CreateInvestigation(ctx, "repair", "telegram")
	if err != nil {
		t.Fatal(err)
	}
	pending := waitForPending(t, svc)
	approvalMessages := waitForTelegramCalls(t, recorder, "sendMessage", 1)
	messageID := approvalMessages[0].messageID
	encoded, _ := json.Marshal(approvalMessages[0].payload["reply_markup"])
	if !strings.Contains(string(encoded), `"callback_data":"d:`+pending.ID+`"`) {
		t.Fatalf("approval keyboard = %s", encoded)
	}
	trigger.handleCallback(ctx, callback("wrong-chat", "d:"+pending.ID, 8, 7, messageID))
	trigger.handleCallback(ctx, callback("wrong-user", "d:"+pending.ID, 7, 8, messageID))
	for _, answer := range waitForTelegramCalls(t, recorder, "answerCallbackQuery", 2)[:2] {
		if answer.payload["text"] != "Unauthorized" {
			t.Fatalf("unauthorized callback answer = %v", answer.payload["text"])
		}
	}
	if len(svc.ListPendingApprovals()) != 1 {
		t.Fatal("unauthorized callback settled approval")
	}
	trigger.handleCallback(ctx, callback("stale", "d:"+pending.ID, 7, 7, messageID+1))
	if len(svc.ListPendingApprovals()) != 1 {
		t.Fatal("stale callback settled approval")
	}
	trigger.handleCallback(ctx, callback("deny", "d:"+pending.ID, 7, 7, messageID))
	svc.Wait()
	edits := waitForTelegramCalls(t, recorder, "editMessageText", 1)
	if edits[0].payload["message_id"] != float64(messageID) || edits[0].payload["text"] != "Denied via telegram:\n\nrepair {}" {
		t.Fatalf("settled edit = %+v", edits[0].payload)
	}
	_, events, err := svc.GetInvestigation(summary.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type != store.EventApprovalDecided {
			continue
		}
		var payload store.ApprovalDecidedPayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if payload.Approved || payload.Via != "telegram" {
			t.Fatalf("approval decision = %+v", payload)
		}
		return
	}
	t.Fatal("approval_decided event not recorded")
}

func TestChatRolesSeparateQAFromApprovals(t *testing.T) {
	audit, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc := service.New(context.Background(), audit, nil, triggerApprovalFactory(t))
	client, recorder := testBot(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	// Chat 7 is notifications-only, chat 9 approvals-only.
	trigger := &Trigger{bot: client, service: svc,
		chats:    map[int64]*chatRole{7: {qa: true}, 9: {approvals: true}},
		messages: make(map[string][]postedApproval)}
	events, unsubscribe := svc.SubscribeApprovals()
	go func() {
		defer unsubscribe()
		trigger.followApprovals(ctx, events)
	}()

	msg := message{Text: "repair"}
	msg.Chat.ID, msg.From.ID = 9, 9
	trigger.handleMessage(ctx, msg)
	if got := len(svc.ListInvestigations()); got != 0 {
		t.Fatalf("approvals-only chat created %d investigations", got)
	}
	msg.Chat.ID, msg.From.ID = 7, 7
	trigger.handleMessage(ctx, msg)
	pending := waitForPending(t, svc)
	// Two sends land: the Q&A acknowledgement in chat 7, then the approval
	// buttons in chat 9.
	approvalMessages := waitForTelegramCalls(t, recorder, "sendMessage", 2)
	var buttons []telegramCall
	for _, call := range approvalMessages {
		if call.payload["reply_markup"] != nil {
			buttons = append(buttons, call)
		}
	}
	if len(buttons) != 1 || buttons[0].payload["chat_id"] != float64(9) {
		t.Fatalf("approval buttons = %+v", buttons)
	}
	trigger.handleCallback(ctx, callback("qa-chat", "d:"+pending.ID, 7, 7, buttons[0].messageID))
	if len(svc.ListPendingApprovals()) != 1 {
		t.Fatal("notifications-only chat settled an approval")
	}
	trigger.handleCallback(ctx, callback("ok", "d:"+pending.ID, 9, 9, buttons[0].messageID))
	svc.Wait()
	if len(svc.ListPendingApprovals()) != 0 {
		t.Fatal("approvals chat could not settle")
	}
}

func TestTriggerEditsApprovalSettledViaAPI(t *testing.T) {
	audit, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc := service.New(context.Background(), audit, nil, triggerApprovalFactory(t))
	_, recorder, ctx := testTrigger(t, svc)
	if _, err := svc.CreateInvestigation(ctx, "repair", "telegram"); err != nil {
		t.Fatal(err)
	}
	pending := waitForPending(t, svc)
	approvalMessages := waitForTelegramCalls(t, recorder, "sendMessage", 1)
	if err := svc.DecideApproval(pending.ID, true, "api"); err != nil {
		t.Fatal(err)
	}
	svc.Wait()
	edits := waitForTelegramCalls(t, recorder, "editMessageText", 1)
	if edits[0].payload["message_id"] != float64(approvalMessages[0].messageID) || edits[0].payload["text"] != "Approved via api:\n\nrepair {}" {
		t.Fatalf("settled edit = %+v", edits[0].payload)
	}
}

func TestNotifierSendsTruncatedWithoutPolling(t *testing.T) {
	client, recorder := testBot(t)
	notifier := &Notifier{bot: client}
	if err := notifier.Send(context.Background(), strings.Repeat("界", 3801)); err != nil {
		t.Fatal(err)
	}
	sends := recorder.matching("sendMessage")
	if len(sends) != 1 {
		t.Fatalf("sendMessage calls = %d", len(sends))
	}
	if got := []rune(sends[0].payload["text"].(string)); len(got) != 3800 {
		t.Fatalf("sent %d characters, want 3800", len(got))
	}
	if polls := recorder.matching("getUpdates"); len(polls) != 0 {
		t.Fatalf("notifier polled getUpdates %d times", len(polls))
	}
}

func TestEmptyTerminalAnswerSendsFallback(t *testing.T) {
	client, recorder := testBot(t)
	trigger := &Trigger{bot: client, chats: map[int64]*chatRole{7: {qa: true}}, messages: make(map[string][]postedApproval)}
	payload, err := json.Marshal(store.CompletedPayload{Answer: ""})
	if err != nil {
		t.Fatal(err)
	}
	done := trigger.sendTerminal(t.Context(), 7, "inv-empty", store.Event{Type: store.EventCompleted, Payload: payload})
	if !done {
		t.Fatal("completed event not treated as terminal")
	}
	calls := waitForTelegramCalls(t, recorder, "sendMessage", 1)
	text, _ := calls[0].payload["text"].(string)
	if !strings.Contains(text, "inv-empty") || !strings.Contains(text, "without an answer") {
		t.Fatalf("fallback text = %q", text)
	}
}

func TestCompletedTerminalStripsVerdictAndAppendsSummary(t *testing.T) {
	client, recorder := testBot(t)
	trigger := &Trigger{bot: client}
	verdict := &store.Verdict{Verdict: "attention", Summary: "clock is drifting"}
	payload, err := json.Marshal(store.CompletedPayload{
		Answer:  "# Result\n\n**drift** detected\n```json\n{\"verdict\":\"attention\",\"summary\":\"clock is drifting\",\"evidence\":[]}\n```\n",
		Verdict: verdict,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !trigger.sendTerminal(t.Context(), 7, "inv-1", store.Event{Type: store.EventCompleted, Payload: payload}) {
		t.Fatal("completed event not treated as terminal")
	}
	calls := waitForTelegramCalls(t, recorder, "sendMessage", 1)
	text := calls[0].payload["text"].(string)
	if strings.Contains(text, "```json") {
		t.Fatalf("message retained verdict block: %q", text)
	}
	if !strings.Contains(text, "verdict: attention — clock is drifting") {
		t.Fatalf("message omitted verdict line: %q", text)
	}
	if calls[0].payload["parse_mode"] != "HTML" || !strings.Contains(text, "<b>Result</b>") || !strings.Contains(text, "<b>drift</b>") {
		t.Fatalf("formatted message = %+v", calls[0].payload)
	}
}

func TestCompletedTerminalRetriesPlainTextAfterHTMLRejection(t *testing.T) {
	client, recorder := testBot(t)
	recorder.rejectHTML = true
	trigger := &Trigger{bot: client}
	payload, err := json.Marshal(store.CompletedPayload{Answer: "**answer**"})
	if err != nil {
		t.Fatal(err)
	}
	trigger.sendTerminal(t.Context(), 7, "inv-1", store.Event{Type: store.EventCompleted, Payload: payload})
	calls := waitForTelegramCalls(t, recorder, "sendMessage", 2)
	if len(calls) != 2 {
		t.Fatalf("sendMessage calls = %d, want 2", len(calls))
	}
	if calls[0].payload["parse_mode"] != "HTML" || calls[0].payload["text"] != "<b>answer</b>" {
		t.Fatalf("first send = %+v", calls[0].payload)
	}
	if _, ok := calls[1].payload["parse_mode"]; ok || calls[1].payload["text"] != "**answer**" {
		t.Fatalf("fallback send = %+v", calls[1].payload)
	}
	trigger.mu.Lock()
	mapped := trigger.answers[calls[1].messageID]
	_, rejectedMapped := trigger.answers[calls[0].messageID]
	trigger.mu.Unlock()
	if mapped != "inv-1" || rejectedMapped {
		t.Fatalf("answer mappings = %+v", trigger.answers)
	}
}

func TestTriggerReplySavesKnownAnswerToKB(t *testing.T) {
	audit, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc := service.New(t.Context(), audit, nil, completedFactory(t, "Restart it."))
	svc.KB, err = kb.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	summary, err := svc.CreateInvestigation(t.Context(), "Repair exporter", "telegram")
	if err != nil {
		t.Fatal(err)
	}
	svc.Wait()
	_, events, err := svc.GetInvestigation(summary.ID)
	if err != nil {
		t.Fatal(err)
	}
	trigger, recorder, ctx := testTrigger(t, svc)
	if !trigger.sendTerminal(ctx, 7, summary.ID, events[len(events)-1]) {
		t.Fatal("completed event not treated as terminal")
	}
	answer := waitForTelegramCalls(t, recorder, "sendMessage", 1)[0]
	reply := message{Text: "Make this a knowledgebase article", ReplyTo: &message{MessageID: answer.messageID}, Chat: chat{ID: 7}, From: user{ID: 7}}
	trigger.handleMessage(ctx, reply)
	calls := waitForTelegramCalls(t, recorder, "sendMessage", 2)
	if calls[1].payload["text"] != "Saved as draft article repair-exporter -- review and approve it in the console." {
		t.Fatalf("confirmation = %q", calls[1].payload["text"])
	}
	articles, err := svc.KB.List()
	if err != nil || len(articles) != 1 || articles[0].Slug != "repair-exporter" {
		t.Fatalf("articles = %+v, %v", articles, err)
	}
	if got := len(svc.ListInvestigations()); got != 1 {
		t.Fatalf("save reply created %d investigations", got)
	}
}

func TestTriggerNonSaveAndUnknownRepliesCreateInvestigations(t *testing.T) {
	audit, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc := service.New(t.Context(), audit, nil, completedFactory(t, "answer"))
	client, _ := testBot(t)
	trigger := &Trigger{bot: client, service: svc, chats: map[int64]*chatRole{7: {qa: true}}, answers: map[int64]string{42: "known"}}
	trigger.handleMessage(t.Context(), message{Text: "What about logs?", ReplyTo: &message{MessageID: 42}, Chat: chat{ID: 7}, From: user{ID: 7}})
	// Bare "kb" stays a follow-up question even on a known answer.
	trigger.handleMessage(t.Context(), message{Text: "check the redhat kb for this", ReplyTo: &message{MessageID: 42}, Chat: chat{ID: 7}, From: user{ID: 7}})
	trigger.handleMessage(t.Context(), message{Text: "save this", ReplyTo: &message{MessageID: 99}, Chat: chat{ID: 7}, From: user{ID: 7}})
	svc.Wait()
	investigations := svc.ListInvestigations()
	if len(investigations) != 3 {
		t.Fatalf("investigations = %+v", investigations)
	}
	questions := map[string]bool{}
	for _, investigation := range investigations {
		questions[investigation.Question] = true
	}
	if !questions["What about logs?"] || !questions["check the redhat kb for this"] || !questions["save this"] {
		t.Fatalf("questions = %+v", questions)
	}
}

func TestTriggerAnswerMappingIsBounded(t *testing.T) {
	trigger := &Trigger{}
	for i := int64(1); i <= answerCap+1; i++ {
		trigger.rememberAnswer(i, "investigation")
	}
	if len(trigger.answers) != answerCap || len(trigger.answerOrder) != answerCap {
		t.Fatalf("mapping sizes = %d, %d", len(trigger.answers), len(trigger.answerOrder))
	}
	if _, ok := trigger.answers[1]; ok {
		t.Fatal("oldest answer mapping was not evicted")
	}
	if trigger.answers[answerCap+1] != "investigation" {
		t.Fatal("newest answer mapping was not retained")
	}
}
