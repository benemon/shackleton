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

func Run(ctx context.Context, sweeps []config.Sweep, svc *service.Service, notifier agent.Notifier) {
	engine := cron.New()
	for _, sw := range sweeps {
		engine.Schedule(sw.Parsed(), cron.FuncJob(func() { runSweep(ctx, svc, notifier, sw) }))
	}
	engine.Start()
	go func() {
		<-ctx.Done()
		engine.Stop()
	}()
}

func runSweep(ctx context.Context, svc *service.Service, notifier agent.Notifier, sw config.Sweep) {
	summary, err := svc.CreateInvestigation(ctx, sw.Question, "sweep:"+sw.Name)
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
	result, done := store.Verdict{}, false
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

func terminalVerdict(event store.Event) (store.Verdict, bool) {
	switch event.Type {
	case store.EventCompleted:
		var payload store.CompletedPayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil || payload.Verdict == nil {
			return store.Verdict{Verdict: "attention", Summary: "verdict unparseable"}, true
		}
		return *payload.Verdict, true
	case store.EventFailed:
		var payload store.FailedPayload
		_ = json.Unmarshal(event.Payload, &payload)
		return store.Verdict{Verdict: "attention", Summary: "investigation failed: " + payload.Reason}, true
	}
	return store.Verdict{}, false
}
