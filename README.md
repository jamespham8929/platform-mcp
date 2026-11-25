# platform-mcp

An MCP server with read-only tools over a Kubernetes platform, and an eval
that grades AI incident investigators on incidents with a known cause.

## The problem

AI agents that investigate incidents are everywhere in 2026. Few of them can
say how often they are right. Real incidents have no answer key, so a demo
shows the cases that worked.

This portfolio can make the answer key. chaos-operator breaks a known service
in a known way at a known time, and it records the run. incident-cli already
ranks candidate causes with a simple model. So an agent can be measured
against that model, on incidents where the right answer is known.

This repo is phase 1 of that plan. It has the tools, an investigator on
Claude, a baseline, and an offline eval on synthetic incidents.

## Tools

Every tool is read only. The server registers a tool only when its backend is
configured, so an agent never sees a tool that can only fail.

| Tool | Reads | Returns |
|------|-------|---------|
| `investigate` | incident-cli, with the event sources the operator set | ranked causes with score parts, and the timeline |
| `chaos_runs` | chaos-operator run records, through kubectl | runs in a time range, with outcome and target |
| `slo_state` | k8s-slo-tracker's gated decisions, from Prometheus | severity per SLO, burn rate bounds per window, staleness |

Each item in a result has an `evidence_id`, such as
`event:dep-checkout-4821`, `chaos:payments/nightly/20261124T030005Z` or
`slo:checkout/availability`. An investigator must cite these IDs, and the eval
checks every citation against what the tools really returned.

The agent picks only the service, the time and the window. Files, repos,
clusters and URLs are fixed by whoever starts the server. A service name must
match a strict pattern, and it is passed to incident-cli as `--service=<name>`,
so it can never become a flag. Prometheus queries have a 10 second limit and a
cap of 500 series.

## Use it from Claude Code

```bash
go install github.com/jamespham8929/platform-mcp/cmd/platform-mcp@latest
```

Add it to `.mcp.json`:

```json
{
  "mcpServers": {
    "platform": {
      "command": "platform-mcp",
      "args": [
        "serve",
        "--incident-cli", "incident",
        "--github-repo", "jamespham8929/gitops-platform-demo=sample-service",
        "--chaos-from-cluster", "--kube-context", "kind-platform",
        "--prometheus-url", "http://localhost:9090",
        "--graph", "deps.json"
      ]
    }
  }
}
```

`serve` speaks MCP on stdin and stdout and logs each call on stderr.
`--now 2026-01-15T12:00:00Z` fixes the clock, which helps when you replay an
old incident.

## The investigator

`internal/agent` holds two investigators behind one interface.

- `baseline` calls `investigate` and returns incident-cli's top three causes.
  It is the number to beat.
- `claude` runs Claude Opus 5.5 in a tool loop. It lists the server's tools,
  forms a view, and finishes by calling `submit_answer` with at most three
  causes. Each cause must be an `event:` or `chaos:` ID. An SLO state is a
  symptom, so it can support a cause but cannot be one. "Not enough evidence"
  is a valid answer. The agent suggests a next step and never takes one.

The loop runs on the Messages API in this repo, so the eval controls and
records every tool call. The system prompt and tool list never change between
runs, and both are cached. Each investigation has a turn limit (12) and a cost
limit ($1.00). A whole eval run has its own limit ($5 by default). A refusal
falls back to another model inside the same call, and each result records
which model served each turn.

## Eval

```bash
go run ./cmd/platform-mcp eval --incident-cli incident                  # baseline
go run ./cmd/platform-mcp eval --incident-cli incident --no-graph       # baseline without the graph
ANTHROPIC_API_KEY=... go run ./cmd/platform-mcp eval --investigator claude --limit 20 --out results/claude.json
```

The scenarios in `testdata/synthetic` come from incident-cli's benchmark
(seed 7, 100 incidents, 12 noise events each, 35% upstream causes). The
export gives every event a random ID and a title drawn the same way for the
cause and the noise, and it shuffles the order. So nothing in an event says
which one is the answer. The grader reads the answer from a truth block the
investigator never sees.

Scorecard from CI:

| setup | top-1 | top-3 | MRR | upstream top-1 | citations ok | cost per incident |
|-------|-------|-------|-----|----------------|--------------|-------------------|
| baseline | 0.810 | 0.990 | 0.892 | 0.750 | 1.000 | $0 |
| baseline, no graph | 0.790 | 0.980 | 0.878 | 0.458 | 1.000 | $0 |
| claude-opus-5-5 | not run yet | | | | | |

The Claude row is empty because no API key was available when this was
built. The `Agent eval` workflow runs it by hand with the
`ANTHROPIC_API_KEY` secret, and it stops at a cost limit you set.

Two checks keep these numbers honest.

- The baseline goes through MCP and the real incident-cli binary. It must rank
  every planted cause exactly where the benchmark ranked it, in all 100
  scenarios, with and without the graph. It does. So the server loses nothing
  on the way.
- CI re-exports the scenarios from incident-cli and fails if they differ from
  the committed ones.

Read the synthetic results with care. The benchmark plants causes with the
same assumptions the correlator scores with: recent changes, deploys and
config changes, upstream services. On this data the correlator is close to
the best possible ranking. An agent will have a hard time beating it here,
and that is fine. Phase 2 moves to real incidents on kind, where the signals
are messier. See [ADR 0002](docs/adr/0002-grading-investigators.md).

## Layout

```
cmd/platform-mcp/       serve and eval
internal/tools/         the MCP tools and their evidence IDs
internal/incidentcli/   runs incident-cli and reads its v1 JSON
internal/chaos/         reads chaos-operator run records
internal/slo/           reads k8s-slo-tracker decisions from Prometheus
internal/promapi/       instant queries with a time limit and a series cap
internal/agent/         the Claude investigator, the baseline, citations, cost
internal/eval/          scenarios, grading, the scorecard
testdata/               fixtures written by the other repos' own code
docs/adr/               why the server looks like this, and how grading works
```

## Tests

```bash
go test ./...
INCIDENT_CLI=$(which incident) go test ./...   # adds the contract tests
```

Every tool test goes through a real MCP client session. One test builds the
binary and talks to `serve` over stdio, the way Claude Code would. The agent
tests point the real Anthropic SDK at a fake Messages endpoint, so a request
or response shape the SDK does not accept fails the test. They also check the
fields that matter: caching, effort, fallbacks, the strict answer tool, and
thinking blocks sent back unchanged.

The fixtures come from the other repos' code, not from copies of it.

- `chaos-experiments.json` was written by chaos-operator's `history` module.
- `slo-tracker-series.json` was written by k8s-slo-tracker's
  `DecisionExporter`, through [scripts/slo-fixture.py](scripts/slo-fixture.py).
- `investigate-v1.json` is incident-cli's own golden file.

## Limitations

- The synthetic scenarios only exercise `investigate`. `chaos_runs` and
  `slo_state` matter once incidents come from a real cluster in phase 2.
- In the phase 2 gym, the investigator must not see chaos runs. They are the
  answer key there. The gym will turn the chaos source and `chaos_runs` off
  for the investigator, and use the run IDs only for grading.
- Cost is estimated from list prices in `internal/agent/pricing.go`. It is
  for comparing runs, not a bill.
- `slo_state` reads the `service` label only. The ServiceMonitor renames the
  daemon's `namespace` label, so the tool does not use it.

## License

MIT
