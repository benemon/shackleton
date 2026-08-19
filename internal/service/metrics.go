package service

import (
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	investigationsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "shackleton_investigations_total",
		Help: "Investigations by trigger class and terminal status.",
	}, []string{"trigger", "status"})
	investigationSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "shackleton_investigation_duration_seconds",
		Help:    "Investigation wall-clock duration by trigger class.",
		Buckets: prometheus.ExponentialBuckets(1, 2, 12),
	}, []string{"trigger"})
	toolCallsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "shackleton_tool_calls_total",
		Help: "Model tool calls across all investigations.",
	})
	toolCallErrors = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "shackleton_tool_call_errors_total",
		Help: "Tool-call fidelity failures by kind.",
	}, []string{"kind"})
	toolCallsRecovered = promauto.NewCounter(prometheus.CounterOpts{
		Name: "shackleton_tool_calls_recovered_total",
		Help: "Malformed tool calls the model subsequently corrected.",
	})
	approvalDecisions = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "shackleton_approval_decisions_total",
		Help: "Approval decisions by channel and outcome.",
	}, []string{"via", "approved"})
)

// triggerClass strips per-instance suffixes (alert fingerprints, sweep
// names) so metric label cardinality stays bounded.
func triggerClass(trigger string) string {
	class, _, _ := strings.Cut(trigger, ":")
	return class
}
