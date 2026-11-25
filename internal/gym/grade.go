package gym

import (
	"fmt"
	"io"
	"slices"
	"text/tabwriter"
	"time"

	"github.com/jamespham8929/platform-mcp/internal/agent"
	"github.com/jamespham8929/platform-mcp/internal/eval"
)

// Matches reports whether a cited cause names the planted cause.
//
// A metric anomaly's ID carries its onset time, and that can move with the
// window an investigator picks. So a rule matches on what the tool said about
// the evidence, its source and service, and not on the ID. A change is
// matched by its exact ID, because the recording wrote that ID.
func (t Truth) Matches(evidenceID string, ev agent.Evidence) bool {
	for _, r := range t.Accept {
		if r.matches(evidenceID, ev) {
			return true
		}
	}
	return slices.Contains(t.ChaosRuns, agent.UnderlyingID(evidenceID))
}

func (r Rule) matches(evidenceID string, ev agent.Evidence) bool {
	if r.EvidenceID != "" {
		return evidenceID == r.EvidenceID
	}
	if r.Change != "" {
		// A change that never got an ID cannot match anything.
		return false
	}
	if r.Source == "" && r.Service == "" && r.Query == "" {
		return false
	}
	return (r.Source == "" || ev.Source == r.Source) &&
		(r.Service == "" || ev.Service == r.Service) &&
		(r.Query == "" || ev.Query == r.Query)
}

// Grade scores one answer against the recording's truth.
func Grade(g *eval.Graded, rec Recording, res *agent.Result) {
	if res == nil || res.Answer == nil {
		g.Abstained = true
		return
	}
	g.Citations = agent.CheckCitations(*res.Answer, res.Seen)
	if res.Answer.Status == agent.StatusInsufficient {
		g.Abstained = true
	}
	for i, c := range res.Answer.Causes {
		if rec.Truth.Matches(c.EvidenceID, res.Evidence[c.EvidenceID]) {
			g.Rank = i + 1
			break
		}
	}
	if len(res.Answer.Causes) > 0 && slices.Contains(rec.Truth.Distractors, res.Answer.Causes[0].EvidenceID) {
		g.Distracted = true
	}
}

// PrintScenarios writes one line per scenario: the planted cause and what the
// investigator put first. Five incidents are few enough to read one by one.
func PrintScenarios(w io.Writer, recs []Loaded, graded []eval.Graded) {
	byName := map[string]Loaded{}
	for _, r := range recs {
		byName[r.Definition.Name] = r
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "scenario\tcause\trank\ttop answer\tnote")
	for _, g := range graded {
		rec := byName[g.Scenario]
		cause := rec.Truth.Service
		if rec.Definition.Fault != nil {
			cause = rec.Definition.Fault.Type + " on " + cause
		} else {
			cause = "deploy on " + cause
		}
		top, note := "-", ""
		if g.Result != nil && g.Result.Answer != nil && len(g.Result.Answer.Causes) > 0 {
			id := g.Result.Answer.Causes[0].EvidenceID
			top = id
			if ev, ok := g.Result.Evidence[id]; ok {
				top = fmt.Sprintf("%s (%s, %s)", id, ev.Source, ev.Service)
			}
		}
		switch {
		case g.Error != "":
			note = "error: " + g.Error
		case g.Distracted:
			note = "fell for the distraction"
		case g.Abstained:
			note = "said the evidence was not enough"
		}
		rank := "miss"
		if g.Rank > 0 {
			rank = fmt.Sprint(g.Rank)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", g.Scenario, cause, rank, top, note)
	}
	_ = tw.Flush()
}

// PrintScore writes the summary line for one setup.
func PrintScore(w io.Writer, s eval.Score) {
	model := s.Model
	if model == "" {
		model = "-"
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(tw, "setup\tmodel\tn\ttop-1\ttop-3\tMRR\tdistracted\tabstain\tcitations ok\tcost/incident\tmedian time\t")
	fmt.Fprintf(tw, "%s\t%s\t%d\t%.3f\t%.3f\t%.3f\t%.3f\t%.3f\t%.3f\t$%.4f\t%s\t\n",
		s.Setup, model, s.Scenarios, s.Top1, s.Top3, s.MRR, s.DistractedRate, s.AbstainRate,
		s.CitationValidity, s.CostPerIncident, s.MedianDuration.Round(time.Millisecond))
	_ = tw.Flush()
}
