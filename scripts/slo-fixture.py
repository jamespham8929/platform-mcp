"""Write testdata/slo-tracker-series.json from k8s-slo-tracker's real exporter.

Run it from a k8s-slo-tracker checkout with its dependencies installed:

    cd ../k8s-slo-tracker && python ../platform-mcp/scripts/slo-fixture.py

The series come from DecisionExporter, so the slo_state tests read the metric
names and labels the daemon really exports. The script adds the job, instance
and pod labels a scrape through the ServiceMonitor would add.
"""

import json
from datetime import datetime, timezone

from prometheus_client import CollectorRegistry, generate_latest
from prometheus_client.parser import text_string_to_metric_families

from slo_tracker.adaptive import AlertDecision, BurnWindow, Severity, WindowVerdict
from slo_tracker.confidence import Interval
from slo_tracker.daemon import DecisionExporter
from slo_tracker.evaluator import EvaluatedSLO
from slo_tracker.models import SLODefinition, SLOType

NOW = datetime(2026, 1, 15, 11, 58, tzinfo=timezone.utc).timestamp()
OUT = "../platform-mcp/testdata/slo-tracker-series.json"


def verdict(window, observed, lower, samples, sufficient, breach):
    interval = Interval(observed, lower, observed * 1.2)
    return WindowVerdict(window, observed, lower, interval, samples, 347.0, sufficient, breach)


def main():
    availability = SLODefinition("availability", "checkout", "payments", SLOType.AVAILABILITY,
                                 0.999, 30, team="payments")
    latency = SLODefinition("latency-p99", "checkout", "payments", SLOType.LATENCY,
                            0.99, 30, team="payments")
    search = SLODefinition("availability", "search", "search", SLOType.AVAILABILITY, 0.999, 30)

    exporter = DecisionExporter(CollectorRegistry())
    exporter.record(EvaluatedSLO(availability, AlertDecision(
        "checkout", "availability", Severity.CRITICAL,
        BurnWindow("1h", "5m", 14.4, Severity.CRITICAL), "1h and 5m both burn fast",
        verdicts=[verdict("1h", 19.2, 16.1, 41000, True, True),
                  verdict("5m", 22.5, 15.3, 3400, True, True),
                  verdict("6h", 4.1, 3.6, 250000, True, False)],
    )), now=NOW)
    # The last good evaluation was 15 minutes ago, so this SLO is stale.
    exporter.record(EvaluatedSLO(latency, AlertDecision(
        "checkout", "latency-p99", Severity.INSUFFICIENT_DATA, None, "5m window too sparse",
        verdicts=[verdict("5m", 30.0, 0.0, 12, False, False)],
    )), now=NOW - 900)
    exporter.record(EvaluatedSLO(search, AlertDecision(
        "search", "availability", Severity.NONE, None, "within budget",
        verdicts=[verdict("1h", 0.2, 0.0, 9000, True, False)],
    )), now=NOW)
    exporter.record_error(latency)

    series = []
    text = generate_latest(exporter.registry).decode()
    for family in text_string_to_metric_families(text):
        for s in family.samples:
            if s.name.endswith("_created"):
                continue
            labels = {"__name__": s.name, **s.labels, "job": "slo-tracker",
                      "instance": "10.0.3.7:9105", "pod": "slo-tracker-6d9f-abcde"}
            series.append({"metric": labels, "value": [NOW, repr(float(s.value))]})

    doc = {
        "generated_by": "k8s-slo-tracker DecisionExporter, see platform-mcp/scripts/slo-fixture.py",
        "evaluated_at": NOW,
        "series": series,
    }
    with open(OUT, "w") as f:
        f.write(json.dumps(doc, indent=1) + "\n")
    print(f"wrote {len(series)} series to {OUT}")


if __name__ == "__main__":
    main()
