// Package slo reads the gated decisions k8s-slo-tracker's daemon exports and
// groups them per SLO. The metric names come from the tracker's daemon.py and
// its ADR 0005.
package slo

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/jamespham8929/platform-mcp/internal/promapi"
)

// StaleAfter matches the tracker's SLOTrackerStale alert. A decision older
// than this may no longer describe the service.
const StaleAfter = 10 * time.Minute

// Querier runs an instant query.
type Querier interface {
	Query(ctx context.Context, query string, at time.Time) ([]promapi.Sample, error)
}

type Window struct {
	Window         string  `json:"window"`
	BurnLowerBound float64 `json:"burn_rate_lower_bound"`
	BurnObserved   float64 `json:"burn_rate_observed"`
	Samples        float64 `json:"samples"`
	SufficientData bool    `json:"sufficient_data"`
}

type State struct {
	EvidenceID string `json:"evidence_id"`
	Service    string `json:"service"`
	SLO        string `json:"slo"`
	// Severity is the gated decision: critical, warning, none or
	// insufficient_data. Empty means the daemon has not exported one.
	Severity string `json:"severity"`
	// LastEvaluated is the last successful evaluation. Zero means never.
	LastEvaluated    time.Time `json:"last_evaluated,omitempty"`
	SecondsSinceEval float64   `json:"seconds_since_evaluation"`
	Stale            bool      `json:"stale"`
	EvaluationErrors float64   `json:"evaluation_errors_total"`
	Windows          []Window  `json:"windows"`
}

func selector(metric, service string) string {
	// The service name is checked against a strict pattern before it gets
	// here, so it cannot break out of the quotes.
	return fmt.Sprintf(`%s{service=%q}`, metric, service)
}

// Read returns every SLO the daemon reports for a service, at a time.
func Read(ctx context.Context, q Querier, service string, at time.Time) ([]State, error) {
	states := map[string]*State{}
	get := func(slo string) *State {
		if s, ok := states[slo]; ok {
			return s
		}
		s := &State{EvidenceID: "slo:" + service + "/" + slo, Service: service, SLO: slo}
		states[slo] = s
		return s
	}
	windows := map[string]map[string]*Window{}
	window := func(slo, name string) *Window {
		if windows[slo] == nil {
			windows[slo] = map[string]*Window{}
		}
		if w, ok := windows[slo][name]; ok {
			return w
		}
		w := &Window{Window: name}
		windows[slo][name] = w
		return w
	}

	run := func(metric string, apply func(promapi.Sample)) error {
		samples, err := q.Query(ctx, selector(metric, service), at)
		if err != nil {
			return fmt.Errorf("%s: %w", metric, err)
		}
		for _, s := range samples {
			if s.Labels["slo"] == "" {
				continue
			}
			apply(s)
		}
		return nil
	}

	steps := []struct {
		metric string
		apply  func(promapi.Sample)
	}{
		{"slo_tracker_alert_state", func(s promapi.Sample) {
			st := get(s.Labels["slo"])
			if s.Value == 1 {
				st.Severity = s.Labels["severity"]
			}
		}},
		{"slo_tracker_last_evaluation_timestamp_seconds", func(s promapi.Sample) {
			st := get(s.Labels["slo"])
			t := time.UnixMilli(int64(s.Value * 1000)).UTC()
			// Two daemon replicas would both export. The newest one wins.
			if t.After(st.LastEvaluated) {
				st.LastEvaluated = t
			}
		}},
		{"slo_tracker_evaluation_errors_total", func(s promapi.Sample) {
			get(s.Labels["slo"]).EvaluationErrors += s.Value
		}},
		{"slo_tracker_burn_rate_lower_bound", func(s promapi.Sample) {
			get(s.Labels["slo"])
			window(s.Labels["slo"], s.Labels["window"]).BurnLowerBound = s.Value
		}},
		{"slo_tracker_burn_rate_observed", func(s promapi.Sample) {
			get(s.Labels["slo"])
			window(s.Labels["slo"], s.Labels["window"]).BurnObserved = s.Value
		}},
		{"slo_tracker_window_samples", func(s promapi.Sample) {
			get(s.Labels["slo"])
			window(s.Labels["slo"], s.Labels["window"]).Samples = s.Value
		}},
		{"slo_tracker_window_sufficient_data", func(s promapi.Sample) {
			get(s.Labels["slo"])
			window(s.Labels["slo"], s.Labels["window"]).SufficientData = s.Value == 1
		}},
	}
	for _, step := range steps {
		if err := run(step.metric, step.apply); err != nil {
			return nil, err
		}
	}

	out := make([]State, 0, len(states))
	for name, st := range states {
		for _, w := range windows[name] {
			st.Windows = append(st.Windows, *w)
		}
		sort.Slice(st.Windows, func(i, j int) bool {
			return windowSeconds(st.Windows[i].Window) < windowSeconds(st.Windows[j].Window)
		})
		if st.Windows == nil {
			st.Windows = []Window{}
		}
		if st.LastEvaluated.IsZero() {
			st.Stale = true
		} else {
			st.SecondsSinceEval = at.Sub(st.LastEvaluated).Seconds()
			st.Stale = at.Sub(st.LastEvaluated) > StaleAfter
		}
		out = append(out, *st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SLO < out[j].SLO })
	return out, nil
}

// windowSeconds orders windows like 5m, 30m, 1h, 6h, 3d. An unknown format
// sorts last.
func windowSeconds(w string) float64 {
	if len(w) < 2 {
		return 1e18
	}
	unit := w[len(w)-1]
	var n float64
	if _, err := fmt.Sscanf(w[:len(w)-1], "%g", &n); err != nil {
		return 1e18
	}
	switch unit {
	case 's':
		return n
	case 'm':
		return n * 60
	case 'h':
		return n * 3600
	case 'd':
		return n * 86400
	}
	return 1e18
}
