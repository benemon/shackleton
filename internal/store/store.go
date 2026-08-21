package store

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/benemon/shackleton/internal/agent"
)

const (
	EventCreated           = "created"
	EventToolCall          = "tool_call"
	EventApprovalRequested = "approval_requested"
	EventApprovalDecided   = "approval_decided"
	EventCompleted         = "completed"
	EventFailed            = "failed"
)

type CreatedPayload struct {
	Question string `json:"question"`
	Trigger  string `json:"trigger"`
}

type ToolCallPayload struct {
	Round         int    `json:"round"`
	Name          string `json:"name"`
	Args          any    `json:"args"`
	ResultSnippet string `json:"result_snippet"`
	Error         bool   `json:"error"`
}

type ApprovalRequestedPayload struct {
	CallID string `json:"call_id"`
	Name   string `json:"name"`
	Human  string `json:"human"`
}

type ApprovalDecidedPayload struct {
	CallID   string `json:"call_id"`
	Approved bool   `json:"approved"`
	Via      string `json:"via"`
}

type Verdict struct {
	Verdict  string   `json:"verdict"`
	Summary  string   `json:"summary"`
	Evidence []string `json:"evidence"`
	// Resolution is the model's post-action re-check outcome: cleared or
	// persisting. Present only when an approved action executed.
	Resolution string `json:"resolution,omitempty"`
}

type CompletedPayload struct {
	Answer  string        `json:"answer"`
	Verdict *Verdict      `json:"verdict,omitempty"`
	Metrics agent.Metrics `json:"metrics"`
}

type FailedPayload struct {
	Reason  string        `json:"reason"`
	Metrics agent.Metrics `json:"metrics"`
}

// ParseVerdict extracts the structured verdict from the trailing fenced json
// block the behavioral contract asks for. Returns nil when the answer carries
// no valid block — callers decide whether absence matters.
func ParseVerdict(answer string) *Verdict {
	start := strings.LastIndex(answer, "```json")
	if start < 0 {
		return nil
	}
	body, _, ok := strings.Cut(answer[start+len("```json"):], "```")
	if !ok {
		return nil
	}
	var parsed Verdict
	if err := json.Unmarshal([]byte(strings.TrimSpace(body)), &parsed); err != nil {
		return nil
	}
	switch parsed.Verdict {
	case "healthy", "attention", "action":
		if parsed.Resolution != "cleared" && parsed.Resolution != "persisting" {
			parsed.Resolution = ""
		}
		return &parsed
	}
	return nil
}

type Event struct {
	TS      time.Time       `json:"ts"`
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

type Summary struct {
	ID        string    `json:"id"`
	Question  string    `json:"question"`
	Trigger   string    `json:"trigger"`
	Status    string    `json:"status"`
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at,omitempty"`
	Answer    string    `json:"answer,omitempty"`
	Verdict   *Verdict  `json:"verdict,omitempty"`
}

type Store struct {
	dir         string
	mu          sync.Mutex
	summaries   map[string]Summary
	buffers     map[string][]Event
	subscribers map[string]map[chan Event]struct{}
}

type Investigation struct {
	ID     string
	store  *Store
	file   *os.File
	writer *bufio.Writer
	mu     sync.Mutex
	closed bool
}

func Open(dir string) (*Store, error) {
	investigationsDir := filepath.Join(dir, "investigations")
	if err := os.MkdirAll(investigationsDir, 0o755); err != nil {
		return nil, err
	}
	s := &Store{
		dir: investigationsDir, summaries: make(map[string]Summary),
		buffers:     make(map[string][]Event),
		subscribers: make(map[string]map[chan Event]struct{}),
	}
	entries, err := os.ReadDir(investigationsDir)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".jsonl" {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".jsonl")
		events, err := readEvents(filepath.Join(investigationsDir, entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("investigation %s: %w", id, err)
		}
		if len(events) > 0 {
			s.summaries[id] = summarize(id, events)
		}
	}
	return s, nil
}

func (s *Store) Begin(question, trigger string) (*Investigation, error) {
	for range 4 {
		id, err := newID()
		if err != nil {
			return nil, err
		}
		file, err := os.OpenFile(s.path(id), os.O_CREATE|os.O_EXCL|os.O_WRONLY|os.O_APPEND, 0o600)
		if os.IsExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		s.mu.Lock()
		s.buffers[id] = nil
		s.mu.Unlock()
		investigation := &Investigation{ID: id, store: s, file: file, writer: bufio.NewWriter(file)}
		if err := investigation.Append(EventCreated, CreatedPayload{Question: question, Trigger: trigger}); err != nil {
			_ = investigation.Close()
			s.mu.Lock()
			delete(s.buffers, id)
			s.mu.Unlock()
			return nil, err
		}
		return investigation, nil
	}
	return nil, fmt.Errorf("could not allocate investigation id")
}

func (i *Investigation) Append(eventType string, payload any) error {
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	event := Event{TS: time.Now().UTC(), Type: eventType, Payload: payloadJSON}
	line, err := json.Marshal(event)
	if err != nil {
		return err
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.closed {
		return fmt.Errorf("investigation %s is closed", i.ID)
	}
	if _, err := i.writer.Write(append(line, '\n')); err != nil {
		return err
	}
	if err := i.writer.Flush(); err != nil {
		return err
	}
	i.store.record(i.ID, event)
	return nil
}

func (i *Investigation) Close() error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.closed {
		return nil
	}
	i.closed = true
	if err := i.writer.Flush(); err != nil {
		_ = i.file.Close()
		return err
	}
	return i.file.Close()
}

func (s *Store) List() []Summary {
	s.mu.Lock()
	result := make([]Summary, 0, len(s.summaries))
	for _, summary := range s.summaries {
		result = append(result, summary)
	}
	s.mu.Unlock()
	sort.Slice(result, func(a, b int) bool {
		if result[a].StartedAt.Equal(result[b].StartedAt) {
			return result[a].ID > result[b].ID
		}
		return result[a].StartedAt.After(result[b].StartedAt)
	})
	return result
}

func (s *Store) Get(id string) ([]Event, error) {
	if err := validateID(id); err != nil {
		return nil, err
	}
	return readEvents(s.path(id))
}

// Subscribe drops the oldest buffered event if a slow subscriber fills its channel.
func (s *Store) Subscribe(id string) (<-chan Event, func()) {
	s.mu.Lock()
	ch, cancel := s.subscribeLocked(id)
	s.mu.Unlock()
	return ch, cancel
}

func (s *Store) Follow(id string) ([]Event, <-chan Event, func(), error) {
	if err := validateID(id); err != nil {
		return nil, nil, func() {}, err
	}
	s.mu.Lock()
	if events, ok := s.buffers[id]; ok {
		snapshot := append([]Event(nil), events...)
		ch, cancel := s.subscribeLocked(id)
		s.mu.Unlock()
		return snapshot, ch, cancel, nil
	}
	s.mu.Unlock()
	events, err := readEvents(s.path(id))
	return events, nil, func() {}, err
}

func (s *Store) subscribeLocked(id string) (chan Event, func()) {
	ch := make(chan Event, 32)
	if s.subscribers[id] == nil {
		s.subscribers[id] = make(map[chan Event]struct{})
	}
	s.subscribers[id][ch] = struct{}{}
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			s.mu.Lock()
			delete(s.subscribers[id], ch)
			if len(s.subscribers[id]) == 0 {
				delete(s.subscribers, id)
			}
			close(ch)
			s.mu.Unlock()
		})
	}
}

func (s *Store) path(id string) string { return filepath.Join(s.dir, id+".jsonl") }

func (s *Store) record(id string, event Event) {
	s.mu.Lock()
	if events, ok := s.buffers[id]; ok {
		s.buffers[id] = append(events, event)
	}
	summary := s.summaries[id]
	applyEvent(&summary, id, event)
	s.summaries[id] = summary
	for ch := range s.subscribers[id] {
		select {
		case ch <- event:
		default:
			select {
			case <-ch:
			default:
			}
			select {
			case ch <- event:
			default:
			}
		}
	}
	if event.Type == EventCompleted || event.Type == EventFailed {
		delete(s.buffers, id)
	}
	s.mu.Unlock()
}

var ErrInvalidID = errors.New("invalid investigation id")

func validateID(id string) error {
	if filepath.Base(id) != id || id == "." || id == "" {
		return fmt.Errorf("%w %q", ErrInvalidID, id)
	}
	return nil
}

func readEvents(path string) ([]Event, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	lines := bytes.Split(data, []byte{'\n'})
	events := make([]Event, 0, len(lines))
	terminated := len(data) == 0 || data[len(data)-1] == '\n'
	for index, line := range lines {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var event Event
		if err := json.Unmarshal(line, &event); err != nil {
			if index == len(lines)-1 && !terminated {
				break
			}
			return nil, fmt.Errorf("line %d: %w", index+1, err)
		}
		events = append(events, event)
	}
	return events, nil
}

func summarize(id string, events []Event) Summary {
	var summary Summary
	for _, event := range events {
		applyEvent(&summary, id, event)
	}
	return summary
}

func applyEvent(summary *Summary, id string, event Event) {
	summary.ID = id
	switch event.Type {
	case EventCreated:
		var payload CreatedPayload
		_ = json.Unmarshal(event.Payload, &payload)
		summary.Question = payload.Question
		summary.Trigger = payload.Trigger
		summary.StartedAt = event.TS
		summary.EndedAt = time.Time{}
		summary.Answer = ""
		summary.Verdict = nil
		summary.Status = "running"
	case EventCompleted:
		var payload CompletedPayload
		_ = json.Unmarshal(event.Payload, &payload)
		summary.Status = "completed"
		summary.EndedAt = event.TS
		summary.Answer = payload.Answer
		summary.Verdict = payload.Verdict
	case EventFailed:
		summary.Status = "failed"
		summary.EndedAt = event.TS
		summary.Answer = ""
		summary.Verdict = nil
	default:
		summary.Status = "running"
		summary.EndedAt = time.Time{}
		summary.Answer = ""
	}
}

func newID() (string, error) {
	random := make([]byte, 8)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	return time.Now().UTC().Format("20060102-150405-") + base64.RawURLEncoding.EncodeToString(random), nil
}
