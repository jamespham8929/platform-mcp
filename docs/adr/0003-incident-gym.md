# 3. The incident gym: record on kind, replay without it

Date: 2026-11-25

## Status

Accepted

## Context

Phase 1 graded investigators on synthetic incidents. Those incidents come
from incident-cli's benchmark, which plants causes with the same assumptions
the correlator scores with. On that data the correlator is close to the best
ranking, so it says little about an agent.

Phase 2 needs incidents where a real fault hits a real service, and the
signals are what Prometheus really saw. chaos-operator can cause the fault
and record when. But a run on kind takes about two hours, and it is never
exactly the same twice. An eval of a language model needs the same inputs on
every run, and CI needs an answer in minutes.

## Decision

The gym records on kind and replays without it.

### The shop

- A shop is four copies of gitops-platform-demo's sample service. frontend
  calls checkout, checkout calls inventory, and recommendations stands
  alone. A load generator sends orders to frontend and recommendations.
- The sample service gained an optional upstream call. A caller counts and
  times its calls in `upstream_requests_total` and
  `upstream_request_duration_seconds`. This matters for latency. netem delays
  inventory's packets after its handler returns, so inventory's own latency
  metric stays flat. Only checkout's upstream metric shows the fault.
- Every scenario gets its own shop in its own namespace. A fault in one
  scenario can never show up in another scenario's recording.

### Faults and distractions

- chaos-operator injects every fault. Latency runs `tc` in a chaos-agent
  sidecar, through the new `spec.container` field, because the app image is
  distroless. A pod kill repeats every minute, like a crash loop, so it lasts
  long enough to page.
- chaos-operator now checks the exit status of each command it runs in a pod.
  Before that, a fault that never ran was still recorded as injected. The gym
  reads those records as the answer key, so this had to be fixed first.
- Each scenario also makes changes that a real incident has: deploys (real
  Helm upgrades) and flag flips. They become incident-cli events with the same
  magnitude, so magnitude does not hint at the cause. A scenario marks which
  changes are distractions.
- One scenario is a control with no chaos. A bad deploy is the cause, so an
  investigator that learns to never blame deploys gets caught.

### The page

A PrometheusRule pages on frontend when more than 5% of orders fail or p99
latency passes 300ms, for three minutes. The three minutes come from
incident-cli's anomaly detector. It needs three one-minute points in a row
before it calls a shift real. A page that fires sooner would come before any
anomaly exists to explain it.

### What a recording holds

The gym records the inputs to the tools, not the tools' answers.

- The shop's raw metric samples, read with a range vector query and written as
  gzipped OpenMetrics.
- The changes, in incident-cli's event file format.
- The ChaosExperiment list, with chaos-operator's run records.
- The page time and the resolved answer key.

A replay loads the samples into a fresh Prometheus with `promtool tsdb
create-blocks-from openmetrics`. Then the real incident-cli and the real MCP
tools run on top, with the clock set to the page time. A recording of tool
answers would only answer the calls that were made while recording. A real
Prometheus answers any query an investigator thinks of.

### The answer key

- The investigator never sees the chaos runs. They are the answer key. A
  `--show-chaos` replay exists only to check the pipe.
- A cause counts as right when it names the faulted service through a metric
  anomaly, or cites the exact change that was the cause, or a chaos run. The
  grader matches anomalies by what the tool said about them, source and
  service, because an anomaly's ID includes its onset time, and that can move
  with the window an investigator picks.
- A symptom on the paged service is not the cause. Neither is a distraction.
  The scorecard counts how often the top answer was a distraction.

### History

A shop warms up for 95 minutes before its fault. The anomaly detector needs 30
minutes of baseline before the window it searches. So replays default the
investigate window to 30 minutes, and windows up to an hour still work.

### Cadence

CI replays every recording on every push, through the pinned Prometheus and
incident-cli from main. The `Gym` workflow records from scratch weekly and on
demand, and uploads the new recordings. They are committed by hand, after
reading the scorecard, when the shop, the faults or the queries change.

## Consequences

- A replay takes seconds and gives the same inputs every time. The faults,
  metrics and page behind it were real.
- Five incidents are a small sample. The scorecard is read one scenario at a
  time, not as a rate. Phase 3 grows the set to 15.
- The shop is small, and its metrics are clean. Real systems have more
  services and more noise.
- For a chaos fault, the evidence is a metric anomaly. The gym does not give
  the investigator Kubernetes events or logs yet, which a person would read
  after a pod kill.
- `slo_state` is not used yet, because k8s-slo-tracker's daemon is not in the
  gym. That comes with phase 3.
- The recordings come from one cluster on one day. A change in the shop, the
  faults or the anomaly queries needs a new recording.
