# 4. A held-out test for the cascade rules

Date: 2026-11-25

## Status

Accepted

## Context

The first five gym recordings found that incident-cli ranked the page's own
symptoms above the cause. incident-cli then learned to trace cascades
(its ADR 0006), and the baseline went from 0 of 5 to 5 of 5. But those five
incidents shaped the rules. A score on the data a rule was written for says
the rule fits that data. It does not say the rule works.

## Decision

- The first five scenarios are the tuning set. Ten new scenarios are held
  out. Each scenario file says which group it is in, and replays score the
  groups apart.
- The ten were written and pushed (commit a4ef43b) before any of them was
  recorded or replayed. The commit message holds a prediction for each one.
- The held-out set aims at what the rules might not cover: a fault on the
  paged service itself, a fault in the middle of the chain, a bad deploy at
  each end, a bad config change, and a harmless change on the faulty service
  just before the fault.
- incident-cli is not changed while the held-out set is scored. A fix that
  comes after this is tuned on the held-out set too, and the next held-out
  set has to judge it.
- A replay with a graph that is missing one edge measures what a wrong graph
  costs.

## Results

Replayed with incident-cli e119579, the commit that added the cascade
pass. Rank is where the planted cause sits in the baseline's answer, and
"miss" means it is not in the top three.

| scenario | predicted | rank | top answer |
|----------|-----------|------|------------|
| frontend-latency | not first | 2 | the checkout deploy, a distraction |
| checkout-latency | not first | miss | inventory's latency, as checkout measures it |
| checkout-latency-inventory-deploy | not first | miss | the inventory deploy, a distraction |
| inventory-latency-inventory-flag | not first | 3 | the inventory flag, a distraction |
| inventory-latency-frontend-deploy-after | first | 1 | the cause |
| inventory-bad-deploy | first | 1 | the cause |
| frontend-bad-deploy | first | 1 | the cause |
| checkout-timeout-config | not first | miss | the inventory flag, a distraction |
| checkout-pod-kill-inventory-deploy-after | first | 1 | the cause |
| inventory-pod-kill-checkout-flag | first | 1 | the cause |

Every prediction held. Five passed, and five missed for the reason the
commit message gave.

| setup | tuning top-1 | held-out top-1 | held-out top-3 | held-out distracted |
|-------|--------------|----------------|----------------|---------------------|
| baseline | 1.000 | 0.500 | 0.700 | 0.400 |
| baseline, no graph | 0.000 | 0.100 | 0.400 | 0.100 |
| baseline, graph missing one edge | 0.400 | 0.500 | 0.900 | 0.200 |
| baseline, chaos runs shown | 1.000 | 0.600 | 0.700 | 0.300 |

The first try at checkout-timeout-config never paged. A 1ms timeout failed
only 2.6% of checkout's calls, because most calls to inventory take less
than 1ms. Commit ea043a2 changed it to 100us, which fails about 70% of
orders, and the scenario was recorded again. That commit went in before any
held-out replay ran.

Four of the misses have one cause. The caller measures each call, both its
latency and its errors, and the gym's queries file them under the service
being called. tc on checkout delays every packet checkout sends, and that
includes its requests to inventory. So checkout's calls to inventory look
slow, and the walk goes on to inventory. The failure seems to start one
service too deep. Then a change on that wrong service ranks first, because
the rules put a change above the anomaly it explains. frontend-latency is the
same thing one hop up. In checkout-timeout-config, checkout's own timeout
fails its calls, and the errors are filed under inventory. So the inventory
flag ranks first.

The mean latency of each hop at the page shows it:

| scenario | frontend to checkout | checkout to inventory |
|----------|----------------------|-----------------------|
| inventory-latency-inventory-flag | +420ms | +420ms |
| checkout-latency | +940ms | +510ms |
| frontend-latency | +440ms | +0ms |

inventory's own handlers stayed under 1ms in all three. With the fault on
inventory, the delay passes through checkout unchanged. With the fault on
checkout, frontend sees two delays. One is on checkout's request to
inventory, and one is on its reply.

The fifth miss is the flag. The rules put a change on the frontier above the
anomaly it explains, and that is what gets the bad deploys right. Here the
change was a harmless flag one minute before the fault, and it ranked first.

A graph without the checkout to inventory edge also named 5 of 10 first, but
a different five. The walk could not go past checkout, so it passed every
checkout scenario. It failed every inventory scenario instead. The right
graph's misses are one service too deep. The wrong graph's misses are one
service too shallow.

## Consequences

- The tuning score overstated what the rules can do. On incidents the rules
  were not written for, the baseline names the cause first in 5 of 10, and
  in the top three in 7 of 10. The README now reports the two groups apart.
- Every prediction held. So the misses come from known gaps in the rules,
  and none of them was a surprise.
- A fix for the latency misses could compare the hops. A service that adds
  delay on both sides of itself is the likely cause. That would fix both
  checkout-latency scenarios. It would not fix frontend-latency or
  checkout-timeout-config. In those, one hop breaks, and nothing in these
  metrics says which end of it is at fault. A second caller of the service
  below would tell.
- We keep the rule that puts an earlier change above its anomaly. It is right
  for every bad deploy in the gym, and wrong for one harmless flag. A person
  with the same data would check that flag first too. The cause still ranks
  third.
- Showing the chaos runs to the baseline fixed only the flag scenario. In the
  latency misses, the chaos run is on a service the walk went past, so the
  pass scales it down like any other symptom. So the cascade pass can bury
  a direct signal too.
- The wrong graph tied the right one on held-out top-1, and each fails where
  the other passes. So on this set, the walk's habit of going one service
  too deep costs as much as a missing edge.
- Any change to incident-cli from here is tuned on these ten too. It needs a
  new held-out set, written and pushed before it is recorded. The gym test
  pins every scenario's rank. So a change that moves one fails CI until the
  README and this ADR say what moved.
- The gym now records every night. The nightly scorecards show how much the
  ranks move between two recordings of the same scenario. One recording per
  scenario cannot show that.
