package telegram

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/benemon/shackleton/internal/agent"
	"github.com/benemon/shackleton/internal/service"
	"github.com/benemon/shackleton/internal/store"
)

type pendingApproval struct {
	decision  chan bool
	messageID int64
	human     string
}

type bot struct {
	client  *http.Client
	baseURL string
}

// Cred is one configured channel's credentials, carried from the config to
// Start with the channel name for error context.
type Cred struct {
	Name   string
	Token  string
	ChatID string
}

type chatRole struct {
	qa        bool
	approvals bool
}

// Start wires serve-mode Telegram: notification chats take the Q&A trigger
// and receive investigation answers; approval chats receive Approve/Deny
// buttons and settlements. Telegram allows one getUpdates consumer per bot
// token, so chats are pooled into one poller per unique token — which is how
// the same token may back both lists. Returns a Notifier per notifications
// channel for the sweep fan-out.
func Start(ctx context.Context, svc *service.Service, notifications, approvals []Cred) ([]agent.Notifier, error) {
	pollers := make(map[string]map[int64]*chatRole)
	ensure := func(cred Cred) (*chatRole, error) {
		id, err := parseChatID(cred.Name, cred.ChatID)
		if err != nil {
			return nil, err
		}
		if cred.Token == "" {
			return nil, fmt.Errorf("%s: bot token is empty", cred.Name)
		}
		chats := pollers[cred.Token]
		if chats == nil {
			chats = make(map[int64]*chatRole)
			pollers[cred.Token] = chats
		}
		role := chats[id]
		if role == nil {
			role = &chatRole{}
			chats[id] = role
		}
		return role, nil
	}
	var notifiers []agent.Notifier
	for _, cred := range notifications {
		role, err := ensure(cred)
		if err != nil {
			return nil, err
		}
		role.qa = true
		notifier, err := NewNotifier(cred.Token, cred.ChatID)
		if err != nil {
			return nil, err
		}
		notifiers = append(notifiers, notifier)
	}
	for _, cred := range approvals {
		role, err := ensure(cred)
		if err != nil {
			return nil, err
		}
		role.approvals = true
	}
	for token, chats := range pollers {
		newTrigger(ctx, &bot{client: &http.Client{Timeout: 35 * time.Second}, baseURL: "https://api.telegram.org/bot" + token}, chats, svc)
	}
	return notifiers, nil
}

type Adapter struct {
	timeout time.Duration
	bot     *bot
	chatID  int64
	mu      sync.Mutex
	pending map[string]*pendingApproval
}

func New(ctx context.Context, token, chatID string, timeout time.Duration) (*Adapter, error) {
	client, id, err := newBot(token, chatID)
	if err != nil {
		return nil, err
	}
	if timeout == 0 {
		timeout = 10 * time.Minute
	}
	a := &Adapter{timeout: timeout, bot: client, chatID: id, pending: make(map[string]*pendingApproval)}
	go a.poll(ctx)
	return a, nil
}

func (a *Adapter) Send(ctx context.Context, text string) error {
	_, err := a.bot.sendMessage(ctx, a.chatID, truncate(text, 3800), "", nil)
	return err
}

func (a *Adapter) RequestApproval(ctx context.Context, call agent.ToolCall) (agent.Decision, error) {
	nonceBytes := make([]byte, 12)
	if _, err := rand.Read(nonceBytes); err != nil {
		return agent.Decision{}, err
	}
	nonce := base64.RawURLEncoding.EncodeToString(nonceBytes)
	keyboard := approvalKeyboard(nonce)
	text := "Proposed action:\n\n" + call.Human + "\n\nApprove to execute."
	messageID, err := a.bot.sendMessage(ctx, a.chatID, text, "", keyboard)
	if err != nil {
		return agent.Decision{}, err
	}
	p := &pendingApproval{decision: make(chan bool, 1), messageID: messageID, human: call.Human}
	a.mu.Lock()
	a.pending[nonce] = p
	a.mu.Unlock()

	timer := time.NewTimer(a.timeout)
	defer timer.Stop()
	select {
	case approved := <-p.decision:
		return agent.Decision{Approved: approved, Via: "telegram"}, nil
	case <-ctx.Done():
		if a.removePending(nonce, p) {
			return agent.Decision{}, ctx.Err()
		}
		return agent.Decision{Approved: <-p.decision, Via: "telegram"}, nil
	case <-timer.C:
		if !a.removePending(nonce, p) {
			return agent.Decision{Approved: <-p.decision, Via: "telegram"}, nil
		}
		_ = a.bot.editMessage(context.Background(), a.chatID, messageID, "Expired (no response)")
		return agent.Decision{}, fmt.Errorf("approval timed out after %s", a.timeout)
	}
}

func (a *Adapter) removePending(nonce string, want *pendingApproval) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.pending[nonce] != want {
		return false
	}
	delete(a.pending, nonce)
	return true
}

func (a *Adapter) poll(ctx context.Context) {
	var offset int64
	for ctx.Err() == nil {
		updates, err := a.bot.getUpdates(ctx, offset, []string{"callback_query"})
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("telegram getUpdates: %v", err)
				time.Sleep(time.Second)
			}
			continue
		}
		for _, item := range updates {
			if item.UpdateID >= offset {
				offset = item.UpdateID + 1
			}
			if item.CallbackQuery != nil {
				a.handleCallback(ctx, *item.CallbackQuery)
			}
		}
	}
}

func (a *Adapter) handleCallback(ctx context.Context, callback callbackQuery) {
	if callback.Message.Chat.ID != a.chatID || callback.From.ID != a.chatID {
		_ = a.bot.answerCallback(ctx, callback.ID, "Unauthorized")
		return
	}
	prefix, nonce, ok := strings.Cut(callback.Data, ":")
	if !ok || nonce == "" || (prefix != "a" && prefix != "d") {
		log.Printf("telegram callback has invalid data %q", callback.Data)
		return
	}
	a.mu.Lock()
	p := a.pending[nonce]
	if p == nil || p.messageID != callback.Message.MessageID {
		a.mu.Unlock()
		log.Printf("telegram callback ignored for unknown, settled, or stale nonce %q", nonce)
		return
	}
	delete(a.pending, nonce)
	a.mu.Unlock()

	approved := prefix == "a"
	verdict := "Denied"
	if approved {
		verdict = "Approved"
	}
	_ = a.bot.answerCallback(ctx, callback.ID, verdict)
	_ = a.bot.editMessage(ctx, a.chatID, p.messageID, verdict+":\n\n"+p.human)
	p.decision <- approved
}

// Notifier sends without polling: it can run alongside another getUpdates
// consumer on the same bot token, which the Adapter and Trigger cannot.
type Notifier struct {
	bot    *bot
	chatID int64
}

func NewNotifier(token, chatID string) (*Notifier, error) {
	client, id, err := newBot(token, chatID)
	if err != nil {
		return nil, err
	}
	return &Notifier{bot: client, chatID: id}, nil
}

func (n *Notifier) Send(ctx context.Context, text string) error {
	_, err := n.bot.sendMessage(ctx, n.chatID, truncate(text, 3800), "", nil)
	return err
}

type postedApproval struct {
	chatID    int64
	messageID int64
}

type Trigger struct {
	bot      *bot
	service  *service.Service
	chats    map[int64]*chatRole
	mu       sync.Mutex
	messages map[string][]postedApproval
}

func newTrigger(ctx context.Context, client *bot, chats map[int64]*chatRole, svc *service.Service) *Trigger {
	t := &Trigger{bot: client, service: svc, chats: chats, messages: make(map[string][]postedApproval)}
	if len(t.approvalChats()) > 0 {
		events, cancel := svc.SubscribeApprovals()
		go func() {
			defer cancel()
			t.followApprovals(ctx, events)
		}()
	}
	go t.poll(ctx)
	return t
}

func (t *Trigger) approvalChats() []int64 {
	var ids []int64
	for id, role := range t.chats {
		if role.approvals {
			ids = append(ids, id)
		}
	}
	return ids
}

func (t *Trigger) poll(ctx context.Context) {
	allowed := []string{}
	for _, role := range t.chats {
		if role.qa {
			allowed = append(allowed, "message")
			break
		}
	}
	if len(t.approvalChats()) > 0 {
		allowed = append(allowed, "callback_query")
	}
	var offset int64
	for ctx.Err() == nil {
		updates, err := t.bot.getUpdates(ctx, offset, allowed)
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("telegram getUpdates: %v", err)
				time.Sleep(time.Second)
			}
			continue
		}
		for _, item := range updates {
			if item.UpdateID >= offset {
				offset = item.UpdateID + 1
			}
			if item.Message != nil {
				t.handleMessage(ctx, *item.Message)
			}
			if item.CallbackQuery != nil {
				t.handleCallback(ctx, *item.CallbackQuery)
			}
		}
	}
}

func (t *Trigger) handleMessage(ctx context.Context, message message) {
	role := t.chats[message.Chat.ID]
	if role == nil || !role.qa || message.From.ID != message.Chat.ID {
		return
	}
	if strings.TrimSpace(message.Text) == "" || strings.HasPrefix(message.Text, "/") {
		return
	}
	summary, err := t.service.CreateInvestigation(ctx, message.Text, "telegram")
	if err != nil {
		log.Printf("telegram create investigation: %v", err)
		return
	}
	if _, err := t.bot.sendMessage(ctx, message.Chat.ID, "Investigating…", "", nil); err != nil {
		log.Printf("telegram send acknowledgement: %v", err)
	}
	go t.followInvestigation(ctx, message.Chat.ID, summary.ID)
}

func (t *Trigger) followInvestigation(ctx context.Context, chatID int64, id string) {
	snapshot, live, cancel, err := t.service.FollowInvestigation(id)
	if err != nil {
		log.Printf("telegram follow investigation %s: %v", id, err)
		return
	}
	defer cancel()
	for _, event := range snapshot {
		if t.sendTerminal(ctx, chatID, id, event) {
			return
		}
	}
	for live != nil {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-live:
			if !ok || t.sendTerminal(ctx, chatID, id, event) {
				return
			}
		}
	}
}

func (t *Trigger) sendTerminal(ctx context.Context, chatID int64, id string, event store.Event) bool {
	var text, formatted, parseMode string
	switch event.Type {
	case store.EventCompleted:
		var payload store.CompletedPayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			log.Printf("telegram decode completed event: %v", err)
			return true
		}
		text = store.StripVerdictBlock(payload.Answer)
		verdictLine := ""
		if payload.Verdict != nil {
			verdictLine = "verdict: " + payload.Verdict.Verdict + " — " + payload.Verdict.Summary
		}
		if strings.TrimSpace(text) == "" && verdictLine == "" {
			text = "Investigation " + id + " ended without an answer — check the console."
		}
		// The verdict line is bounded hard so the remaining answer budget can
		// never go negative — truncate panics on a negative limit.
		verdictLine = truncate(verdictLine, 500)
		limit := 3800
		if verdictLine != "" {
			limit -= utf8.RuneCountInString(verdictLine) + 2
		}
		text = strings.TrimRight(truncate(text, limit), "\n")
		formatted = markdownToTelegramHTML(text)
		if verdictLine != "" {
			if text != "" {
				text += "\n"
				formatted += "\n"
			}
			text += verdictLine
			formatted += html.EscapeString(verdictLine)
		}
		parseMode = "HTML"
	case store.EventFailed:
		var payload store.FailedPayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			log.Printf("telegram decode failed event: %v", err)
			return true
		}
		text = payload.Reason
	default:
		return false
	}
	// Telegram rejects empty messages; an empty terminal must still reach
	// the operator rather than degrade to silence.
	if strings.TrimSpace(text) == "" {
		text = "Investigation " + id + " ended without an answer — check the console."
	}
	if parseMode == "" {
		formatted = truncate(text, 3800)
	}
	if _, err := t.bot.sendMessage(ctx, chatID, formatted, parseMode, nil); err != nil {
		if parseMode != "" {
			if _, retryErr := t.bot.sendMessage(ctx, chatID, text, "", nil); retryErr == nil {
				return true
			} else {
				log.Printf("telegram send investigation result retry: %v", retryErr)
			}
		}
		log.Printf("telegram send investigation result: %v", err)
	}
	return true
}

func (t *Trigger) followApprovals(ctx context.Context, events <-chan service.ApprovalEvent) {
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-events:
			if !ok {
				return
			}
			switch event.Type {
			case "requested":
				t.requestApproval(ctx, event.Approval)
			case "settled":
				t.settleApproval(ctx, event)
			}
		}
	}
}

func (t *Trigger) requestApproval(ctx context.Context, approval service.PendingApproval) {
	text := "Proposed action:\n\n" + approval.Human + "\n\nApprove to execute."
	var posted []postedApproval
	for _, chatID := range t.approvalChats() {
		messageID, err := t.bot.sendMessage(ctx, chatID, text, "", approvalKeyboard(approval.ID))
		if err != nil {
			log.Printf("telegram send approval: %v", err)
			continue
		}
		posted = append(posted, postedApproval{chatID: chatID, messageID: messageID})
	}
	if len(posted) == 0 {
		return
	}
	t.mu.Lock()
	t.messages[approval.ID] = posted
	t.mu.Unlock()
}

func (t *Trigger) settleApproval(ctx context.Context, event service.ApprovalEvent) {
	t.mu.Lock()
	posted := t.messages[event.Approval.ID]
	delete(t.messages, event.Approval.ID)
	t.mu.Unlock()
	verdict := "Denied"
	if event.Approved {
		verdict = "Approved"
	}
	for _, msg := range posted {
		if err := t.bot.editMessage(ctx, msg.chatID, msg.messageID, verdict+" via "+event.Via+":\n\n"+event.Approval.Human); err != nil {
			log.Printf("telegram edit settled approval: %v", err)
		}
	}
}

func (t *Trigger) handleCallback(ctx context.Context, callback callbackQuery) {
	role := t.chats[callback.Message.Chat.ID]
	if role == nil || !role.approvals || callback.From.ID != callback.Message.Chat.ID {
		_ = t.bot.answerCallback(ctx, callback.ID, "Unauthorized")
		return
	}
	prefix, approvalID, ok := strings.Cut(callback.Data, ":")
	if !ok || approvalID == "" || (prefix != "a" && prefix != "d") {
		log.Printf("telegram callback has invalid data %q", callback.Data)
		return
	}
	t.mu.Lock()
	known := false
	for _, msg := range t.messages[approvalID] {
		if msg.chatID == callback.Message.Chat.ID && msg.messageID == callback.Message.MessageID {
			known = true
			break
		}
	}
	t.mu.Unlock()
	if !known {
		// A tap on a button from before a restart, or on an approval that
		// already settled, must not be silent — the operator deserves to know
		// the tap did nothing.
		_ = t.bot.answerCallback(ctx, callback.ID, "Expired or already settled")
		log.Printf("telegram callback ignored for unknown, settled, or stale approval %q", approvalID)
		return
	}
	approved := prefix == "a"
	if err := t.service.DecideApproval(approvalID, approved, "telegram"); err != nil {
		switch {
		case errors.Is(err, service.ErrApprovalNotFound):
			_ = t.bot.answerCallback(ctx, callback.ID, "Expired")
		case errors.Is(err, service.ErrApprovalAlreadyDecided):
			_ = t.bot.answerCallback(ctx, callback.ID, "Already decided")
		default:
			log.Printf("telegram decide approval: %v", err)
		}
		return
	}
	verdict := "Denied"
	if approved {
		verdict = "Approved"
	}
	_ = t.bot.answerCallback(ctx, callback.ID, verdict)
}

type updateResponse struct {
	OK          bool     `json:"ok"`
	Description string   `json:"description"`
	Result      []update `json:"result"`
}

type update struct {
	UpdateID      int64          `json:"update_id"`
	Message       *message       `json:"message"`
	CallbackQuery *callbackQuery `json:"callback_query"`
}

type message struct {
	MessageID int64  `json:"message_id"`
	Text      string `json:"text"`
	From      user   `json:"from"`
	Chat      chat   `json:"chat"`
}

type callbackQuery struct {
	ID      string  `json:"id"`
	Data    string  `json:"data"`
	From    user    `json:"from"`
	Message message `json:"message"`
}

type user struct {
	ID int64 `json:"id"`
}

type chat struct {
	ID int64 `json:"id"`
}

func newBot(token, chatID string) (*bot, int64, error) {
	if token == "" {
		return nil, 0, fmt.Errorf("bot token is not set")
	}
	id, err := parseChatID("chat_id", chatID)
	if err != nil {
		return nil, 0, err
	}
	return &bot{
		client:  &http.Client{Timeout: 35 * time.Second},
		baseURL: "https://api.telegram.org/bot" + token,
	}, id, nil
}

func parseChatID(name, chatID string) (int64, error) {
	id, err := strconv.ParseInt(chatID, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: chat id %q is not numeric", name, chatID)
	}
	return id, nil
}

func (b *bot) getUpdates(ctx context.Context, offset int64, allowed []string) ([]update, error) {
	var response updateResponse
	err := b.post(ctx, "getUpdates", map[string]any{
		"timeout": 25, "allowed_updates": allowed, "offset": offset,
	}, &response)
	return response.Result, err
}

func (b *bot) sendMessage(ctx context.Context, chatID int64, text, parseMode string, markup any) (int64, error) {
	payload := map[string]any{"chat_id": chatID, "text": text}
	if parseMode != "" {
		payload["parse_mode"] = parseMode
	}
	if markup != nil {
		payload["reply_markup"] = markup
	}
	var response struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
		Result      struct {
			MessageID int64 `json:"message_id"`
		} `json:"result"`
	}
	if err := b.post(ctx, "sendMessage", payload, &response); err != nil {
		return 0, err
	}
	return response.Result.MessageID, nil
}

func markdownToTelegramHTML(markdown string) string {
	lines := strings.Split(markdown, "\n")
	var output []string
	for i := 0; i < len(lines); {
		if strings.HasPrefix(strings.TrimSpace(lines[i]), "```") {
			var code []string
			i++
			for i < len(lines) && !strings.HasPrefix(strings.TrimSpace(lines[i]), "```") {
				code = append(code, lines[i])
				i++
			}
			if i < len(lines) {
				i++
			}
			output = append(output, "<pre>"+html.EscapeString(strings.Join(code, "\n"))+"</pre>")
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(lines[i]), "|") {
			end := i
			for end < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[end]), "|") {
				end++
			}
			tableLines := lines[i:end]
			if len(tableLines) >= 2 {
				separator := strings.Split(strings.Trim(strings.TrimSpace(tableLines[1]), "|"), "|")
				valid := len(separator) > 0
				for _, cell := range separator {
					cell = strings.TrimSpace(cell)
					cell = strings.TrimPrefix(strings.TrimSuffix(cell, ":"), ":")
					if len(cell) < 3 || strings.Trim(cell, "-") != "" {
						valid = false
					}
				}
				if valid {
					rows := make([][]string, 0, len(tableLines)-1)
					widths := make([]int, 0, len(separator))
					for rowIndex, line := range tableLines {
						if rowIndex == 1 {
							continue
						}
						cells := strings.Split(strings.Trim(strings.TrimSpace(line), "|"), "|")
						for cellIndex := range cells {
							cells[cellIndex] = strings.TrimSpace(cells[cellIndex])
							for len(widths) <= cellIndex {
								widths = append(widths, 0)
							}
							widths[cellIndex] = max(widths[cellIndex], utf8.RuneCountInString(cells[cellIndex]))
						}
						rows = append(rows, cells)
					}
					var rendered []string
					for _, row := range rows {
						cells := make([]string, len(widths))
						for cellIndex := range widths {
							if cellIndex < len(row) {
								cells[cellIndex] = row[cellIndex] + strings.Repeat(" ", widths[cellIndex]-utf8.RuneCountInString(row[cellIndex]))
							} else {
								cells[cellIndex] = strings.Repeat(" ", widths[cellIndex])
							}
						}
						rendered = append(rendered, strings.TrimRight(strings.Join(cells, " | "), " "))
					}
					output = append(output, "<pre>"+html.EscapeString(strings.Join(rendered, "\n"))+"</pre>")
					i = end
					continue
				}
			}
		}
		line := lines[i]
		trimmed := strings.TrimLeft(line, "#")
		if len(trimmed) < len(line) && strings.HasPrefix(trimmed, " ") {
			output = append(output, "<b>"+telegramInline(strings.TrimPrefix(trimmed, " "))+"</b>")
		} else {
			output = append(output, telegramInline(line))
		}
		i++
	}
	return strings.Join(output, "\n")
}

func telegramInline(text string) string {
	escaped := html.EscapeString(text)
	var output strings.Builder
	for len(escaped) > 0 {
		// Only ** and backticks convert: single * and _ are ambiguous with
		// snake_case identifiers and PromQL arithmetic, which this domain's
		// answers are full of — mangling those is worse than losing italics.
		marker, tag := "", ""
		switch {
		case strings.HasPrefix(escaped, "**"):
			marker, tag = "**", "b"
		case strings.HasPrefix(escaped, "`"):
			marker, tag = "`", "code"
		}
		if marker != "" {
			if end := strings.Index(escaped[len(marker):], marker); end >= 0 {
				end += len(marker)
				output.WriteString("<" + tag + ">" + escaped[len(marker):end] + "</" + tag + ">")
				escaped = escaped[end+len(marker):]
				continue
			}
		}
		output.WriteByte(escaped[0])
		escaped = escaped[1:]
	}
	return output.String()
}

func (b *bot) editMessage(ctx context.Context, chatID, messageID int64, text string) error {
	return b.post(ctx, "editMessageText", map[string]any{"chat_id": chatID, "message_id": messageID, "text": text}, nil)
}

func (b *bot) answerCallback(ctx context.Context, callbackID, text string) error {
	return b.post(ctx, "answerCallbackQuery", map[string]any{"callback_query_id": callbackID, "text": text}, nil)
}

func (b *bot) post(ctx context.Context, method string, payload any, result any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.baseURL+"/"+method, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("telegram %s: %s", method, resp.Status)
	}
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	var envelope struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(responseBody, &envelope); err != nil {
		return err
	}
	if !envelope.OK {
		return fmt.Errorf("telegram %s: %s", method, envelope.Description)
	}
	if result != nil {
		if err := json.Unmarshal(responseBody, result); err != nil {
			return err
		}
	}
	return nil
}

func approvalKeyboard(id string) map[string]any {
	return map[string]any{"inline_keyboard": [][]map[string]string{{
		{"text": "Approve", "callback_data": "a:" + id},
		{"text": "Deny", "callback_data": "d:" + id},
	}}}
}

func truncate(text string, limit int) string {
	if utf8.RuneCountInString(text) <= limit {
		return text
	}
	runes := []rune(text)
	return string(runes[:limit])
}

var _ agent.Approver = (*Adapter)(nil)
var _ agent.Notifier = (*Adapter)(nil)
var _ agent.Notifier = (*Notifier)(nil)
