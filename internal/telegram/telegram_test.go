package telegram

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type telegramRecorder struct {
	mu      sync.Mutex
	methods []string
	texts   []string
}

func testAdapter(t *testing.T) (*Adapter, *telegramRecorder) {
	t.Helper()
	recorder := &telegramRecorder{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Text string `json:"text"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		recorder.mu.Lock()
		recorder.methods = append(recorder.methods, r.URL.Path)
		recorder.texts = append(recorder.texts, body.Text)
		recorder.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":42}}`))
	}))
	t.Cleanup(server.Close)
	return &Adapter{chatID: 7, client: server.Client(), baseURL: server.URL, pending: make(map[string]*pendingApproval)}, recorder
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
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	answers := 0
	for _, method := range recorder.methods {
		if strings.HasSuffix(method, "/answerCallbackQuery") {
			answers++
		}
	}
	if answers != 1 {
		t.Fatalf("replay was answered %d times, want 1", answers)
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
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if got := []rune(recorder.texts[0]); len(got) != 3800 {
		t.Fatalf("sent %d characters, want 3800", len(got))
	}
}
