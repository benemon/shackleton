package sweep

import (
	"context"
	"encoding/json"
	"log"
	"strings"

	"github.com/benemon/shackleton/internal/agent"
	"github.com/benemon/shackleton/internal/config"
	"github.com/benemon/shackleton/internal/service"
	"github.com/benemon/shackleton/internal/store"
	"github.com/robfig/cron/v3"
)

const scaffold = "\n\nEnd your final answer with a fenced json block of exactly this shape:\n```json\n{\"verdict\":\"healthy\",\"summary\":\"<one line>\",\"evidence\":[\"<item>\"]}\n```\nverdict must be healthy, attention, or action. Use healthy only when nothing needs attention."

type verdict struct {
	Verdict  string   `json:"verdict"`
	Summary  string   `json:"summary"`
	Evidence []string `json:"evidence"`
}

func Run(ctx context.Context, sweeps []config.Sweep, svc *service.Service, notifier agent.Notifier) {
	engine := cron.New()
	for _, sw := range sweeps {
		sw := sw
		engine.Schedule(sw.Parsed(), cron.FuncJob(func() { runSweep(ctx, svc, notifier, sw) }))
	}
	engine.Start()
	go func() {
		<-ctx.Done()
		engine.Stop()
	}()
}

func runSweep(ctx context.Context, svc *service.Service, notifier agent.Notifier, sw config.Sweep) {
	summary, err := svc.CreateInvestigation(ctx, sw.Question+scaffold, "sweep:"+sw.Name)
	if err != nil {
		log.Printf("sweep %s: create investigation: %v", sw.Name, err)
		return
	}
	snapshot, live, cancel, err := svc.FollowInvestigation(summary.ID)
	if err != nil {
		log.Printf("sweep %s: follow investigation %s: %v", sw.Name, summary.ID, err)
		return
	}
	defer cancel()
	result, done := verdict{}, false
	for _, event := range snapshot {
		if result, done = terminalVerdict(event); done {
			break
		}
	}
	for !done && live != nil {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-live:
			if !ok {
				return
			}
			result, done = terminalVerdict(event)
		}
	}
	if !done || result.Verdict == "healthy" {
		return
	}
	text := "Sweep " + sw.Name + ": " + strings.ToUpper(result.Verdict) + "\n" + result.Summary
	for _, item := range result.Evidence {
		text += "\n- " + item
	}
	if notifier == nil {
		log.Printf("sweep %s: no notifier configured:\n%s", sw.Name, text)
		return
	}
	if err := notifier.Send(ctx, text); err != nil {
		log.Printf("sweep %s: notify: %v", sw.Name, err)
	}
}

func terminalVerdict(event store.Event) (verdict, bool) {
	switch event.Type {
	case store.EventCompleted:
		var payload store.CompletedPayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return verdict{Verdict: "attention", Summary: "verdict unparseable"}, true
		}
		return parseVerdict(payload.Answer), true
	case store.EventFailed:
		var payload store.FailedPayload
		_ = json.Unmarshal(event.Payload, &payload)
		return verdict{Verdict: "attention", Summary: "investigation failed: " + payload.Reason}, true
	}
	return verdict{}, false
}

func parseVerdict(answer string) verdict {
	unparseable := verdict{Verdict: "attention", Summary: "verdict unparseable"}
	start := strings.LastIndex(answer, "```json")
	if start < 0 {
		return unparseable
	}
	body := answer[start+len("```json"):]
	end := strings.Index(body, "```")
	if end < 0 {
		return unparseable
	}
	var parsed verdict
	if err := json.Unmarshal([]byte(strings.TrimSpace(body[:end])), &parsed); err != nil {
		return unparseable
	}
	switch parsed.Verdict {
	case "healthy", "attention", "action":
		return parsed
	}
	return unparseable
}
