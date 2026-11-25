# 2. Grade investigators on planted causes, against a baseline

Date: 2026-11-24

## Status

Accepted

## Context

The point of this repo is to say how often an investigator is right. That
needs incidents with a known cause, a fair baseline, and a grader that
cannot be fooled by confident text.

incident-cli's benchmark already makes synthetic incidents with a planted
cause. It was built for the correlator, which never reads event IDs or
titles. The benchmark named the cause `TRUE` and the noise `noise-N`. A
language model reads both.

## Decision

- incident-cli's benchmark exports its trials with `-export`. Every event gets
  a random ID and a title drawn the same way for the cause and the noise, and
  the order is shuffled. The truth block in each file is for the grader only.
  The eval writes the events to a separate file before the investigator runs.
- Each scenario records where the correlator ranked the cause on exactly
  those events. The baseline investigator runs through MCP and the real
  incident-cli. A test requires it to match that rank in every scenario. If
  the pipe in between loses anything, the test fails before any agent number
  is trusted.
- The grader counts these, per scenario:
  - the rank of the planted cause in the answer, so top-1, top-3 and MRR
  - whether the investigator abstained
  - the share of cited evidence IDs that a tool really returned
  - whether a cause was a symptom, such as an SLO, instead of an event
  - the estimated cost, the time, the tool calls, and why the run stopped
- An abstention counts as a miss in top-1. Its rate is reported next to it.
  On synthetic data every incident has a cause, so abstaining is always wrong
  here. On real incidents it will not be.
- Every result records the model that was asked for and the models that
  served each turn. Results from different models stay comparable.
- An eval run stops at a total cost limit. Each investigation stops at its own
  limit and at 12 turns.
- The comparison has three setups, as the plan asks: the correlator alone,
  the agent without the dependency graph, and the agent with every tool.

## Consequences

- The synthetic set favors the correlator. The benchmark plants causes with
  the same assumptions the correlator scores with. On this data an agent may
  only match it, and that will be reported as it is. The set is still useful.
  It checks the tool plumbing, the citation discipline, the cost per incident,
  and whether the agent drops a cause the ranking had right.
- Phase 2 moves to the incident gym on kind. There, chaos-operator causes real
  faults and the signals are messy. The chaos source must be hidden from the
  investigator, because it is the answer key.
- Grading needs no model in the loop. A cause is right when its ID matches.
  This only works because every answer must name an evidence ID.
