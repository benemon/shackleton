package telegram

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
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
)

type pendingApproval struct {
	decision  chan bool
	messageID int64
	human     string
}

type Adapter struct {
	chatID  int64
	timeout time.Duration
	client  *http.Client
	baseURL string
	mu      sync.Mutex
	pending map[string]*pendingApproval
}

func New(ctx context.Context, token, chatID string, timeout time.Duration) (*Adapter, error) {
	if token == "" {
		return nil, fmt.Errorf("TG_BOT_TOKEN is not set")
	}
	id, err := strconv.ParseInt(chatID, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("TG_CHAT_ID: %w", err)
	}
	if timeout == 0 {
		timeout = 10 * time.Minute
	}
	a := &Adapter{
		chatID: id, timeout: timeout, client: &http.Client{Timeout: 35 * time.Second},
		baseURL: "https://api.telegram.org/bot" + token, pending: make(map[string]*pendingApproval),
	}
	go a.poll(ctx)
	return a, nil
}

func (a *Adapter) Send(ctx context.Context, text string) error {
	_, err := a.sendMessage(ctx, truncate(text, 3800), nil)
	return err
}

func (a *Adapter) RequestApproval(ctx context.Context, call agent.ToolCall) (bool, error) {
	nonceBytes := make([]byte, 12)
	if _, err := rand.Read(nonceBytes); err != nil {
		return false, err
	}
	nonce := base64.RawURLEncoding.EncodeToString(nonceBytes)
	keyboard := map[string]any{"inline_keyboard": [][]map[string]string{{
		{"text": "Approve", "callback_data": "a:" + nonce},
		{"text": "Deny", "callback_data": "d:" + nonce},
	}}}
	text := "Proposed action:\n\n" + call.Human + "\n\nApprove to execute."
	messageID, err := a.sendMessage(ctx, text, keyboard)
	if err != nil {
		return false, err
	}
	p := &pendingApproval{decision: make(chan bool, 1), messageID: messageID, human: call.Human}
	a.mu.Lock()
	a.pending[nonce] = p
	a.mu.Unlock()

	timer := time.NewTimer(a.timeout)
	defer timer.Stop()
	select {
	case approved := <-p.decision:
		return approved, nil
	case <-ctx.Done():
		if a.removePending(nonce, p) {
			return false, ctx.Err()
		}
		return <-p.decision, nil
	case <-timer.C:
		if !a.removePending(nonce, p) {
			return <-p.decision, nil
		}
		_ = a.editMessage(context.Background(), messageID, "Expired (no response)")
		return false, fmt.Errorf("approval timed out after %s", a.timeout)
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

type updateResponse struct {
	OK          bool     `json:"ok"`
	Description string   `json:"description"`
	Result      []update `json:"result"`
}

type update struct {
	UpdateID      int64          `json:"update_id"`
	CallbackQuery *callbackQuery `json:"callback_query"`
}

type callbackQuery struct {
	ID      string `json:"id"`
	Data    string `json:"data"`
	From    user   `json:"from"`
	Message struct {
		MessageID int64 `json:"message_id"`
		Chat      chat  `json:"chat"`
	} `json:"message"`
}

type user struct {
	ID int64 `json:"id"`
}
type chat struct {
	ID int64 `json:"id"`
}

func (a *Adapter) poll(ctx context.Context) {
	var offset int64
	for ctx.Err() == nil {
		var response updateResponse
		err := a.post(ctx, "getUpdates", map[string]any{
			"timeout": 25, "allowed_updates": []string{"callback_query"}, "offset": offset,
		}, &response)
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("telegram getUpdates: %v", err)
				time.Sleep(time.Second)
			}
			continue
		}
		for _, item := range response.Result {
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
		_ = a.answerCallback(ctx, callback.ID, "Unauthorized")
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
	_ = a.answerCallback(ctx, callback.ID, verdict)
	_ = a.editMessage(ctx, p.messageID, verdict+":\n\n"+p.human)
	p.decision <- approved
}

func (a *Adapter) sendMessage(ctx context.Context, text string, markup any) (int64, error) {
	payload := map[string]any{"chat_id": a.chatID, "text": text}
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
	if err := a.post(ctx, "sendMessage", payload, &response); err != nil {
		return 0, err
	}
	return response.Result.MessageID, nil
}

func (a *Adapter) editMessage(ctx context.Context, messageID int64, text string) error {
	return a.post(ctx, "editMessageText", map[string]any{"chat_id": a.chatID, "message_id": messageID, "text": text}, nil)
}

func (a *Adapter) answerCallback(ctx context.Context, callbackID, text string) error {
	return a.post(ctx, "answerCallbackQuery", map[string]any{"callback_query_id": callbackID, "text": text}, nil)
}

func (a *Adapter) post(ctx context.Context, method string, payload any, result any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+"/"+method, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.client.Do(req)
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

func truncate(text string, limit int) string {
	if utf8.RuneCountInString(text) <= limit {
		return text
	}
	runes := []rune(text)
	return string(runes[:limit])
}

var _ agent.Approver = (*Adapter)(nil)
var _ agent.Notifier = (*Adapter)(nil)
