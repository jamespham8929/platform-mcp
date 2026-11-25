package eval

import (
	"fmt"
	"io"
	"sort"
	"text/tabwriter"
	"time"
)

// Score sums up one setup over a suite.
type Score struct {
	Setup     string `json:"setup"`
	Model     string `json:"model,omitempty"`
	Scenarios int    `json:"scenarios"`
	Errors    int    `json:"errors"`
	// Top1 and Top3 are shares of all scenarios, abstentions included.
	Top1 float64 `json:"top1"`
	Top3 float64 `json:"top3"`
	MRR  float64 `json:"mrr"`
	// AbstainRate is how often the investigator said the evidence was not
	// enough.
	AbstainRate float64 `json:"abstain_rate"`
	// CitationValidity is the share of cited evidence IDs that a tool really
	// returned during that investigation.
	CitationValidity float64 `json:"citation_validity"`
	// BadCauseRate is how often an answer blamed a symptom, like an SLO,
	// instead of an event.
	BadCauseRate float64 `json:"bad_cause_rate"`
	// DistractedRate is how often the top cause was a planted distraction.
	DistractedRate   float64        `json:"distracted_rate"`
	UpstreamTop1     float64        `json:"upstream_top1"`
	SameServiceTop1  float64        `json:"same_service_top1"`
	CostUSD          float64        `json:"cost_usd"`
	CostPerIncident  float64        `json:"cost_per_incident_usd"`
	MedianDuration   time.Duration  `json:"median_duration_ns"`
	MeanToolCalls    float64        `json:"mean_tool_calls"`
	StopReasons      map[string]int `json:"stop_reasons"`
	AgreesCorrelator float64        `json:"agrees_with_correlator"`
}

func share(n, d int) float64 {
	if d == 0 {
		return 0
	}
	return float64(n) / float64(d)
}

// Summarize scores graded scenarios for one setup.
func Summarize(setup, model string, graded []Graded) Score {
	s := Score{Setup: setup, Model: model, Scenarios: len(graded), StopReasons: map[string]int{}}
	var top1, top3, abstain, bad, distracted, agree, upN, upHit, sameN, sameHit, cited, valid, calls int
	var mrr float64
	var durations []time.Duration
	for _, g := range graded {
		if g.Error != "" {
			s.Errors++
		}
		if g.Rank == 1 {
			top1++
		}
		if g.Rank >= 1 && g.Rank <= 3 {
			top3++
			mrr += 1 / float64(g.Rank)
		}
		if g.Abstained {
			abstain++
		}
		if len(g.Citations.BadCauses) > 0 {
			bad++
		}
		if g.Distracted {
			distracted++
		}
		cr := g.CorrelatorRank
		if cr > 3 {
			cr = 0
		}
		if g.Rank == cr {
			agree++
		}
		if g.Upstream {
			upN++
			if g.Rank == 1 {
				upHit++
			}
		} else {
			sameN++
			if g.Rank == 1 {
				sameHit++
			}
		}
		cited += g.Citations.Total
		valid += g.Citations.Valid
		if g.Result != nil {
			s.CostUSD += g.Result.CostUSD
			durations = append(durations, g.Result.Duration)
			calls += g.Result.ToolCalls
			s.StopReasons[g.Result.Stop]++
		}
	}
	n := len(graded)
	s.Top1, s.Top3, s.MRR = share(top1, n), share(top3, n), 0
	if n > 0 {
		s.MRR = mrr / float64(n)
	}
	s.AbstainRate = share(abstain, n)
	s.BadCauseRate = share(bad, n)
	s.DistractedRate = share(distracted, n)
	s.AgreesCorrelator = share(agree, n)
	s.UpstreamTop1 = share(upHit, upN)
	s.SameServiceTop1 = share(sameHit, sameN)
	s.CitationValidity = 1
	if cited > 0 {
		s.CitationValidity = share(valid, cited)
	}
	if n > 0 {
		s.CostPerIncident = s.CostUSD / float64(n)
		s.MeanToolCalls = float64(calls) / float64(n)
	}
	if len(durations) > 0 {
		sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
		s.MedianDuration = durations[len(durations)/2]
	}
	return s
}

// Print writes a scorecard table.
func Print(w io.Writer, scores []Score) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(tw, "setup\tmodel\tn\ttop-1\ttop-3\tMRR\tupstream top-1\tabstain\tcitations ok\tcost/incident\tmedian time\t")
	for _, s := range scores {
		model := s.Model
		if model == "" {
			model = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%.3f\t%.3f\t%.3f\t%.3f\t%.3f\t%.3f\t$%.4f\t%s\t\n",
			s.Setup, model, s.Scenarios, s.Top1, s.Top3, s.MRR, s.UpstreamTop1,
			s.AbstainRate, s.CitationValidity, s.CostPerIncident, s.MedianDuration.Round(time.Millisecond))
	}
	_ = tw.Flush()
}
