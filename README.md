# Shackleton

A self-hosted, **infra-agnostic** AI ops/investigation agent that runs on a
**local model** and treats bare metal, traditional VMs, cloud instances, and
metrics as first-class citizens — not a Kubernetes afterthought.

The core binary gives you a baseline experience out of the box; the more
capability you inject through its ports, the more complete the intelligent
SRE experience becomes:

1. **Observe** — the binary, a config file, any OpenAI-compatible model, and
   a Prometheus-compatible endpoint. That alone buys investigations,
   scheduled sweeps, structured verdicts, a full audit trail, and the
   embedded console.
2. **Notify** — add a notification channel and verdicts, answers, and
   outcomes reach you; ask questions from your pocket.
3. **Act** — add gated MCP executors and a privileged approvals channel:
   remediation proposals with a human tap on every mutation, and honest
   settlement of every decision.
4. **Remember** — verified resolutions become knowledge-base articles, one
   per symptom; approve an article and it guides future investigations of
   the same symptom.
5. **Scale** — more metrics and log sources, more channels, more tools; the
   same core, progressively more capable.

Every rung is optional, additive, and declared in one config file.

## Why

Existing open-source agentic ops tooling (HolmesGPT, k8sgpt, kagent, kubectl-ai)
is overwhelmingly Kubernetes-first: the bias lives in the harness's own system
prompt and default toolset, so it discounts host/SSH/Prometheus tools even when
they're available. The commercial AI-SRE products that *are* infra-agnostic are
SaaS control planes that meter frontier-model inference — none is self-hostable
on a local model. For a lab (or an estate) that is as much bare metal and VMs as
it is a cluster, that leaves a gap. Shackleton fills it, lab-benefit-first.

## Shape

- **Language:** Go — the core is a single static binary that cross-compiles to
  any target; the system composes with the services you point it at (model
  endpoint, metrics, MCP tools). No runtime dependency management, ever.
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
- **Projections:** the same service core is exposed as the REST API + embedded
  console and as an MCP server on `/mcp` — ask-and-read only (start and read
  investigations, search the knowledge base); approval surfaces stay
  exclusively human. Other agents get estate answers without holding estate
  credentials: `claude mcp add --transport http shackleton
  https://<host>:8420/mcp --header "Authorization: Bearer <token>"`.
- **Modes:** proactive (scheduled, silent-when-healthy, structured verdicts) and
  reactive (streaming Q&A / alert triage).

## Status

In service against its first estate. The closed loop is running: alert triage,
scheduled sweeps, and Q&A end in structured verdicts; consequential outcomes
notify the operator's channels; approved actions are verified against the
signal that motivated them; resolutions accrue as knowledge-base articles that
inform recurrences once approved. The estate itself is declared: an
operator-owned inventory of hosts and clusters (`inventory.example.yaml`)
feeds the system prompt as generated facts and validates the target of every
gated host action before an approval is even requested. Ahead:
knowledge-driven investigation shortcuts and multi-model review.
