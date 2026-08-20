package sweep

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/benemon/shackleton/internal/agent"
	"github.com/benemon/shackleton/internal/config"
	"github.com/benemon/shackleton/internal/service"
	"github.com/benemon/shackleton/internal/store"
	"github.com/openai/openai-go/v3"
)

type fakeNotifier struct {
	mu   sync.Mutex
	sent []string
	err  error
}

func (f *fakeNotifier) Send(_ context.Context, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, text)
	return f.err
}

func (f *fakeNotifier) messages() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sent...)
}

func testService(t *testing.T, answer string, fail error) *service.Service {
	t.Helper()
	audit, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	registry, err := agent.NewRegistry(context.Background(), nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = registry.Close() })
	factory := func(events agent.EventSink, approver agent.Approver) *agent.Runner {
		return &agent.Runner{
			Tools: registry, Events: events, Approver: approver,
			Complete: func(context.Context, []openai.ChatCompletionMessageParamUnion, []openai.ChatCompletionToolUnionParam) (agent.ModelMessage, error) {
				if fail != nil {
					return agent.ModelMessage{}, fail
				}
				return agent.ModelMessage{Content: answer}, nil
			},
		}
	}
	return service.New(context.Background(), audit, nil, factory)
}

func fenced(verdict, summary string, evidence string) string {
	return "prose before\n```json\n{\"verdict\":\"" + verdict + "\",\"summary\":\"" + summary + "\",\"evidence\":[" + evidence + "]}\n```\n"
}

func TestHealthyVerdictIsSilent(t *testing.T) {
	svc := testService(t, fenced("healthy", "all fine", ""), nil)
	notifier := &fakeNotifier{}
	runSweep(context.Background(), svc, notifier, config.Sweep{Name: "node-fs", Question: "check"})
	if sent := notifier.messages(); len(sent) != 0 {
		t.Fatalf("healthy sweep notified: %q", sent)
	}
	investigations := svc.ListInvestigations()
	if len(investigations) != 1 || investigations[0].Trigger != "sweep:node-fs" {
		t.Fatalf("investigations = %+v", investigations)
	}
	if !strings.Contains(investigations[0].Question, "check") || !strings.Contains(investigations[0].Question, "fenced json block") {
		t.Fatalf("scaffold not appended: %q", investigations[0].Question)
	}
}

func TestAttentionVerdictNotifiesWithEvidence(t *testing.T) {
	svc := testService(t, fenced("attention", "disk filling", "\"nas / 87%\""), nil)
	notifier := &fakeNotifier{}
	runSweep(context.Background(), svc, notifier, config.Sweep{Name: "node-fs", Question: "check"})
	sent := notifier.messages()
	if len(sent) != 1 {
		t.Fatalf("sends = %q", sent)
	}
	for _, want := range []string{"node-fs", "ATTENTION", "disk filling", "- nas / 87%"} {
		if !strings.Contains(sent[0], want) {
			t.Fatalf("notification %q missing %q", sent[0], want)
		}
	}
}

func TestUnparseableVerdictNotifiesAsAttention(t *testing.T) {
	svc := testService(t, "no verdict block here", nil)
	notifier := &fakeNotifier{}
	runSweep(context.Background(), svc, notifier, config.Sweep{Name: "s", Question: "q"})
	sent := notifier.messages()
	if len(sent) != 1 || !strings.Contains(sent[0], "verdict unparseable") {
		t.Fatalf("sends = %q", sent)
	}
}

func TestFailedInvestigationNotifies(t *testing.T) {
	svc := testService(t, "", errors.New("model exploded"))
	notifier := &fakeNotifier{}
	runSweep(context.Background(), svc, notifier, config.Sweep{Name: "s", Question: "q"})
	sent := notifier.messages()
	if len(sent) != 1 || !strings.Contains(sent[0], "investigation failed: model exploded") {
		t.Fatalf("sends = %q", sent)
	}
}

func TestNotifierErrorAndNilNotifierDoNotEscape(t *testing.T) {
	svc := testService(t, fenced("action", "restart it", ""), nil)
	runSweep(context.Background(), svc, &fakeNotifier{err: errors.New("telegram down")}, config.Sweep{Name: "s", Question: "q"})
	svc = testService(t, fenced("action", "restart it", ""), nil)
	runSweep(context.Background(), svc, nil, config.Sweep{Name: "s", Question: "q"})
}

func TestParseVerdict(t *testing.T) {
	cases := []struct {
		name    string
		answer  string
		verdict string
		summary string
	}{
		{"last block wins", fenced("healthy", "first", "") + fenced("action", "second", ""), "action", "second"},
		{"unknown verdict", fenced("fine", "x", ""), "attention", "verdict unparseable"},
		{"uppercase rejected", fenced("HEALTHY", "x", ""), "attention", "verdict unparseable"},
		{"unterminated block", "```json\n{\"verdict\":\"healthy\"", "attention", "verdict unparseable"},
		{"bad json", "```json\nnot json\n```", "attention", "verdict unparseable"},
		{"healthy", fenced("healthy", "ok", ""), "healthy", "ok"},
	}
	for _, test := range cases {
		got := parseVerdict(test.answer)
		if got.Verdict != test.verdict || got.Summary != test.summary {
			t.Fatalf("%s: got %+v", test.name, got)
		}
	}
}
