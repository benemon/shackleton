package store

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/benemon/shackleton/internal/agent"
)

func TestScanRebuildRoundTripAndTornFinalLine(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	investigation, err := s.Begin("why is nas slow?", "cli")
	if err != nil {
		t.Fatal(err)
	}
	metrics := agent.Metrics{Rounds: 2, Completed: true, Answer: "disk pressure"}
	if err := investigation.Append(EventToolCall, ToolCallPayload{Round: 1, Name: "query", Args: map[string]any{"q": "up"}, ResultSnippet: "result"}); err != nil {
		t.Fatal(err)
	}
	if err := investigation.Append(EventCompleted, CompletedPayload{Answer: "disk pressure", Metrics: metrics}); err != nil {
		t.Fatal(err)
	}
	if err := investigation.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := s.Get(investigation.ID)
	if err != nil {
		t.Fatal(err)
	}
	listBefore := s.List()
	file, err := os.OpenFile(s.path(investigation.ID), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(`{"ts":"torn`); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	after, err := reopened.Get(investigation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("events after reopen = %#v, want %#v", after, before)
	}
	if !reflect.DeepEqual(reopened.List(), listBefore) {
		t.Fatalf("list after reopen = %#v, want %#v", reopened.List(), listBefore)
	}
	summary := reopened.List()[0]
	if summary.ID != investigation.ID || summary.Question != "why is nas slow?" || summary.Trigger != "cli" || summary.Status != "completed" || summary.Answer != "disk pressure" || summary.EndedAt.IsZero() {
		t.Fatalf("rebuilt summary = %+v", summary)
	}
}

func TestSubscribeDeliversAppendedEventsInOrder(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	investigation, err := s.Begin("question", "cli")
	if err != nil {
		t.Fatal(err)
	}
	defer investigation.Close()
	events, unsubscribe := s.Subscribe(investigation.ID)
	defer unsubscribe()
	if err := investigation.Append(EventApprovalRequested, ApprovalRequestedPayload{CallID: "one", Name: "tool", Human: "do it"}); err != nil {
		t.Fatal(err)
	}
	if err := investigation.Append(EventApprovalDecided, ApprovalDecidedPayload{CallID: "one", Approved: true, Via: "cli-approve"}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{EventApprovalRequested, EventApprovalDecided} {
		select {
		case event := <-events:
			if event.Type != want {
				t.Fatalf("event type = %q, want %q", event.Type, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for %s", want)
		}
	}
}

func TestFollowHasExactSnapshotAndLiveBoundary(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	investigation, err := s.Begin("question", "api")
	if err != nil {
		t.Fatal(err)
	}
	for round := 1; round <= 3; round++ {
		if err := investigation.Append(EventToolCall, ToolCallPayload{Round: round, Name: "tool"}); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, live, cancel, err := s.Follow(investigation.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	if len(snapshot) != 4 {
		t.Fatalf("snapshot has %d events, want 4", len(snapshot))
	}
	for round := 4; round <= 6; round++ {
		if err := investigation.Append(EventToolCall, ToolCallPayload{Round: round, Name: "tool"}); err != nil {
			t.Fatal(err)
		}
	}
	for round := 4; round <= 6; round++ {
		select {
		case event := <-live:
			var payload ToolCallPayload
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				t.Fatal(err)
			}
			if event.Type != EventToolCall || payload.Round != round {
				t.Fatalf("live event = %s round %d, want %s round %d", event.Type, payload.Round, EventToolCall, round)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for round %d", round)
		}
	}
	select {
	case event := <-live:
		t.Fatalf("unexpected duplicate live event: %+v", event)
	default:
	}
	if err := investigation.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	replayed, reopenedLive, reopenedCancel, err := reopened.Follow(investigation.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer reopenedCancel()
	if len(replayed) != 7 {
		t.Fatalf("reopened snapshot has %d events, want 7", len(replayed))
	}
	if reopenedLive != nil {
		t.Fatal("reopened investigation returned a live channel")
	}
}

func TestStatusIsDerivedFromLastEvent(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		terminal string
		payload  any
		status   string
	}{
		{EventToolCall, ToolCallPayload{}, "running"},
		{EventCompleted, CompletedPayload{}, "completed"},
		{EventFailed, FailedPayload{Reason: "failure"}, "failed"},
	}
	for _, test := range tests {
		investigation, err := s.Begin(test.status, "cli")
		if err != nil {
			t.Fatal(err)
		}
		if err := investigation.Append(test.terminal, test.payload); err != nil {
			t.Fatal(err)
		}
		if err := investigation.Close(); err != nil {
			t.Fatal(err)
		}
		found := false
		for _, summary := range s.List() {
			if summary.ID == investigation.ID {
				found = true
				if summary.Status != test.status {
					t.Fatalf("last event %s produced status %q", test.terminal, summary.Status)
				}
			}
		}
		if !found {
			t.Fatalf("missing investigation %s", investigation.ID)
		}
	}

	// A nonterminal event after a terminal event returns the investigation to running.
	investigation, err := s.Begin("last wins", "cli")
	if err != nil {
		t.Fatal(err)
	}
	if err := investigation.Append(EventCompleted, CompletedPayload{}); err != nil {
		t.Fatal(err)
	}
	if err := investigation.Append(EventToolCall, ToolCallPayload{}); err != nil {
		t.Fatal(err)
	}
	if err := investigation.Close(); err != nil {
		t.Fatal(err)
	}
	events, err := s.Get(investigation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if events[len(events)-1].Type != EventToolCall {
		last, _ := json.Marshal(events[len(events)-1])
		t.Fatalf("last event = %s", last)
	}
	for _, summary := range s.List() {
		if summary.ID == investigation.ID && summary.Status != "running" {
			t.Fatalf("last-event status = %q", summary.Status)
		}
	}
}
