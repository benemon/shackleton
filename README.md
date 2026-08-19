# Shackleton

A self-hosted, **infra-agnostic** AI ops/investigation agent that runs on a
**local model** and treats bare metal, traditional VMs, cloud instances, and
metrics as first-class citizens — not a Kubernetes afterthought.

## Why

Existing open-source agentic ops tooling (HolmesGPT, k8sgpt, kagent, kubectl-ai)
is overwhelmingly Kubernetes-first: the bias lives in the harness's own system
prompt and default toolset, so it discounts host/SSH/Prometheus tools even when
they're available. The commercial AI-SRE products that *are* infra-agnostic are
SaaS control planes that meter frontier-model inference — none is self-hostable
on a local model. For a lab (or an estate) that is as much bare metal and VMs as
it is a cluster, that leaves a gap. Shackleton fills it, lab-benefit-first.

## Shape

- **Language:** Go — single static, dependency-free binary; cross-compiles to any
  target; matches the infra ecosystem. (Distribution beats a Python venv even in
  one's own lab, and doubly so for air-gapped/heterogeneous targets.)
- **Model:** any OpenAI-compatible endpoint (e.g. litellm → a local Qwen). No
  mandatory cloud inference.
- **Tools:** MCP-native — host/SSH diagnostics, Prometheus, gated executors are
  all first-class peers behind a tool-agnostic, operator-authored system prompt.
- **Architecture:** ports-and-adapters (hexagonal). I/O channels
  (Telegram/Slack/Teams/…), triggers, model, tools, state, and secrets are all
  behind ports. The approval channel is bidirectional — "present an approval
  request and await a human decision" — with per-channel adapters.
- **Safety:** mutating actions are human-approved; the model never sees raw
  credentials (custody stays in the tool/adapter layer).
- **Modes:** proactive (scheduled, silent-when-healthy, structured verdicts) and
  reactive (streaming Q&A / alert triage).

## Status

Spike phase — proving local-model tool-call reliability on a heterogeneous tool
surface and the approval seam before building out.
