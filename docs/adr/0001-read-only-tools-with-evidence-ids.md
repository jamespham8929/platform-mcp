# 1. Read-only tools that return evidence IDs

Date: 2026-11-24

## Status

Accepted

## Context

An investigator needs the platform's signals: the ranked causes from
incident-cli, the chaos runs, and the SLO decisions. MCP is how agents get
tools in 2026. Claude Code, Claude Desktop and most agent frameworks speak it.

Two risks shaped the design. An agent with write access can make an incident
worse. An agent that writes fluent text can make up a cause that sounds right.

## Decision

- Every tool is read only, and the server marks each one with the MCP read
  only hint. No tool can write to the cluster, GitHub, PagerDuty or Slack.
- The server operator fixes the sources: event files, repos, the cluster
  context and the Prometheus URL. The agent chooses only the service, the time,
  the window and how many results to return.
- `investigate` runs the incident-cli binary and reads its versioned JSON
  output (`incident-cli/investigate/v1`). It does not import incident-cli's
  code. The JSON is the contract, a golden file in incident-cli pins it, and a
  test here runs the real binary against it. A new schema version is refused,
  not guessed at.
- `chaos_runs` uses kubectl, like incident-cli does. It reads the records
  chaos-operator keeps in `status.runs`.
- `slo_state` reads the metrics k8s-slo-tracker's daemon exports. It runs a
  fixed set of queries with a time limit and a series cap. There is no free
  PromQL tool yet. That comes later, with the same limits.
- Every item a tool returns has an `evidence_id`. Its kind comes first:
  `event:`, `chaos:` or `slo:`. The eval checks each citation in an answer
  against the IDs the tools returned in that investigation.
- A tool is registered only when its backend is configured.

## Consequences

- A made-up citation is caught and counted, not trusted.
- A chaos run can appear twice, as an `event:` through incident-cli and as a
  `chaos:` from `chaos_runs`. Both name the same run ID, and the grader treats
  them as the same thing.
- The server needs the incident-cli and kubectl binaries next to it. For a
  local MCP server that is normal. A server inside the cluster would want
  client-go and in-cluster auth instead.
