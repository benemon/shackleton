package telegram

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
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
	chatID  int64
	client  *http.Client
	baseURL string
}

type Adapter struct {
	timeout time.Duration
	bot     *bot
	mu      sync.Mutex
	pending map[string]*pendingApproval
}

func New(ctx context.Context, token, chatID string, timeout time.Duration) (*Adapter, error) {
	client, err := newBot(token, chatID)
	if err != nil {
		return nil, err
	}
	if timeout == 0 {
		timeout = 10 * time.Minute
	}
	a := &Adapter{timeout: timeout, bot: client, pending: make(map[string]*pendingApproval)}
	go a.poll(ctx)
	return a, nil
}

func (a *Adapter) Send(ctx context.Context, text string) error {
	_, err := a.bot.sendMessage(ctx, truncate(text, 3800), nil)
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
	messageID, err := a.bot.sendMessage(ctx, text, keyboard)
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
		_ = a.bot.editMessage(context.Background(), messageID, "Expired (no response)")
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
	if callback.Message.Chat.ID != a.bot.chatID || callback.From.ID != a.bot.chatID {
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
	_ = a.bot.editMessage(ctx, p.messageID, verdict+":\n\n"+p.human)
	p.decision <- approved
}

// Notifier sends without polling: it can run alongside another getUpdates
// consumer on the same bot token, which the Adapter and Trigger cannot.
type Notifier struct {
	bot *bot
}

func NewNotifier(token, chatID string) (*Notifier, error) {
	client, err := newBot(token, chatID)
	if err != nil {
		return nil, err
	}
	return &Notifier{bot: client}, nil
}

func (n *Notifier) Send(ctx context.Context, text string) error {
	_, err := n.bot.sendMessage(ctx, truncate(text, 3800), nil)
	return err
}

type Trigger struct {
	bot      *bot
	service  *service.Service
	mu       sync.Mutex
	messages map[string]int64
}

func NewTrigger(ctx context.Context, token, chatID string, svc *service.Service) (*Trigger, error) {
	client, err := newBot(token, chatID)
	if err != nil {
		return nil, err
	}
	return newTrigger(ctx, client, svc), nil
}

func newTrigger(ctx context.Context, client *bot, svc *service.Service) *Trigger {
	t := &Trigger{bot: client, service: svc, messages: make(map[string]int64)}
	events, cancel := svc.SubscribeApprovals()
	go func() {
		defer cancel()
		t.followApprovals(ctx, events)
	}()
	go t.poll(ctx)
	return t
}

func (t *Trigger) poll(ctx context.Context) {
	var offset int64
	for ctx.Err() == nil {
		updates, err := t.bot.getUpdates(ctx, offset, []string{"message", "callback_query"})
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
	if message.Chat.ID != t.bot.chatID || message.From.ID != t.bot.chatID {
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
	if _, err := t.bot.sendMessage(ctx, "Investigating…", nil); err != nil {
		log.Printf("telegram send acknowledgement: %v", err)
	}
	go t.followInvestigation(ctx, summary.ID)
}

func (t *Trigger) followInvestigation(ctx context.Context, id string) {
	snapshot, live, cancel, err := t.service.FollowInvestigation(id)
	if err != nil {
		log.Printf("telegram follow investigation %s: %v", id, err)
		return
	}
	defer cancel()
	for _, event := range snapshot {
		if t.sendTerminal(ctx, event) {
			return
		}
	}
	for live != nil {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-live:
			if !ok || t.sendTerminal(ctx, event) {
				return
			}
		}
	}
}

func (t *Trigger) sendTerminal(ctx context.Context, event store.Event) bool {
	var text string
	switch event.Type {
	case store.EventCompleted:
		var payload store.CompletedPayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			log.Printf("telegram decode completed event: %v", err)
			return true
		}
		text = payload.Answer
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
	if _, err := t.bot.sendMessage(ctx, truncate(text, 3800), nil); err != nil {
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
	messageID, err := t.bot.sendMessage(ctx, text, approvalKeyboard(approval.ID))
	if err != nil {
		log.Printf("telegram send approval: %v", err)
		return
	}
	t.mu.Lock()
	t.messages[approval.ID] = messageID
	t.mu.Unlock()
}

func (t *Trigger) settleApproval(ctx context.Context, event service.ApprovalEvent) {
	t.mu.Lock()
	messageID, ok := t.messages[event.Approval.ID]
	t.mu.Unlock()
	if !ok {
		return
	}
	verdict := "Denied"
	if event.Approved {
		verdict = "Approved"
	}
	if err := t.bot.editMessage(ctx, messageID, verdict+" via "+event.Via+":\n\n"+event.Approval.Human); err != nil {
		log.Printf("telegram edit settled approval: %v", err)
	}
	t.mu.Lock()
	if t.messages[event.Approval.ID] == messageID {
		delete(t.messages, event.Approval.ID)
	}
	t.mu.Unlock()
}

func (t *Trigger) handleCallback(ctx context.Context, callback callbackQuery) {
	if callback.Message.Chat.ID != t.bot.chatID || callback.From.ID != t.bot.chatID {
		_ = t.bot.answerCallback(ctx, callback.ID, "Unauthorized")
		return
	}
	prefix, approvalID, ok := strings.Cut(callback.Data, ":")
	if !ok || approvalID == "" || (prefix != "a" && prefix != "d") {
		log.Printf("telegram callback has invalid data %q", callback.Data)
		return
	}
	t.mu.Lock()
	messageID, exists := t.messages[approvalID]
	t.mu.Unlock()
	if !exists || messageID != callback.Message.MessageID {
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

func newBot(token, chatID string) (*bot, error) {
	if token == "" {
		return nil, fmt.Errorf("TG_BOT_TOKEN is not set")
	}
	id, err := strconv.ParseInt(chatID, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("TG_CHAT_ID: %w", err)
	}
	return &bot{
		chatID: id, client: &http.Client{Timeout: 35 * time.Second},
		baseURL: "https://api.telegram.org/bot" + token,
	}, nil
}

func (b *bot) getUpdates(ctx context.Context, offset int64, allowed []string) ([]update, error) {
	var response updateResponse
	err := b.post(ctx, "getUpdates", map[string]any{
		"timeout": 25, "allowed_updates": allowed, "offset": offset,
	}, &response)
	return response.Result, err
}

func (b *bot) sendMessage(ctx context.Context, text string, markup any) (int64, error) {
	payload := map[string]any{"chat_id": b.chatID, "text": text}
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

func (b *bot) editMessage(ctx context.Context, messageID int64, text string) error {
	return b.post(ctx, "editMessageText", map[string]any{"chat_id": b.chatID, "message_id": messageID, "text": text}, nil)
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
