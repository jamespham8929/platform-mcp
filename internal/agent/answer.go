package agent

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Answer is what an investigator hands to a human. It names causes and
// suggests a next step. It never claims to have done anything.
type Answer struct {
	// Status is "identified" or "insufficient_evidence". Saying the evidence
	// is not enough is a valid answer, and often the right one.
	Status     string  `json:"status"`
	Causes     []Claim `json:"causes"`
	Summary    string  `json:"summary"`
	NextAction string  `json:"next_action"`
}

// Claim is one ranked cause.
type Claim struct {
	// EvidenceID is the event or chaos run blamed, as a tool returned it.
	EvidenceID string `json:"evidence_id"`
	// Confidence is "high", "medium" or "low".
	Confidence         string   `json:"confidence"`
	Reasoning          string   `json:"reasoning"`
	SupportingEvidence []string `json:"supporting_evidence"`
}

const (
	StatusIdentified   = "identified"
	StatusInsufficient = "insufficient_evidence"
	// MaxCauses is how many causes an answer may rank.
	MaxCauses = 3
)

// answerSchema is the submit_answer input schema. It is strict, so every
// property is required and no others are allowed.
var answerSchema = map[string]any{
	"properties": map[string]any{
		"status": map[string]any{
			"type": "string",
			"enum": []string{StatusIdentified, StatusInsufficient},
			"description": "identified when the evidence points at specific causes. " +
				"insufficient_evidence when it does not. Then causes may be empty.",
		},
		"causes": map[string]any{
			"type":        "array",
			"description": "Up to 3 causes, most likely first.",
			"items": map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"required":             []string{"evidence_id", "confidence", "reasoning", "supporting_evidence"},
				"properties": map[string]any{
					"evidence_id": map[string]any{
						"type": "string",
						"description": "The evidence_id of the event or chaos run you blame, exactly as " +
							"a tool returned it. It starts with event: or chaos:.",
					},
					"confidence": map[string]any{"type": "string", "enum": []string{"high", "medium", "low"}},
					"reasoning":  map[string]any{"type": "string", "description": "Why, in two or three sentences."},
					"supporting_evidence": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "Other evidence_ids you relied on, such as an SLO state.",
					},
				},
			},
		},
		"summary": map[string]any{"type": "string", "description": "What happened, for the on-call engineer."},
		"next_action": map[string]any{
			"type":        "string",
			"description": "One step a human should take next. You cannot take it yourself.",
		},
	},
	"required": []string{"status", "causes", "summary", "next_action"},
}

// parseAnswer decodes and checks a submit_answer input.
func parseAnswer(raw string) (Answer, error) {
	var a Answer
	if err := json.Unmarshal([]byte(raw), &a); err != nil {
		return a, fmt.Errorf("submit_answer input is not valid JSON: %w", err)
	}
	if a.Status != StatusIdentified && a.Status != StatusInsufficient {
		return a, fmt.Errorf("status must be %s or %s", StatusIdentified, StatusInsufficient)
	}
	if a.Status == StatusIdentified && len(a.Causes) == 0 {
		return a, fmt.Errorf("status identified needs at least one cause")
	}
	if len(a.Causes) > MaxCauses {
		a.Causes = a.Causes[:MaxCauses]
	}
	return a, nil
}

// Citations checks an answer's evidence IDs against the IDs the tools really
// returned in this investigation.
type Citations struct {
	Total   int      `json:"total"`
	Valid   int      `json:"valid"`
	Unknown []string `json:"unknown,omitempty"`
	// BadCauses are causes that point at something other than an event or a
	// chaos run, such as an SLO. An SLO state is a symptom, not a cause.
	BadCauses []string `json:"bad_causes,omitempty"`
}

// Share is the fraction of citations that are real. No citations counts as 1.
func (c Citations) Share() float64 {
	if c.Total == 0 {
		return 1
	}
	return float64(c.Valid) / float64(c.Total)
}

// CheckCitations compares every cited ID with the set the tools returned.
func CheckCitations(a Answer, seen map[string]bool) Citations {
	var c Citations
	check := func(id string) {
		c.Total++
		if seen[id] {
			c.Valid++
		} else {
			c.Unknown = append(c.Unknown, id)
		}
	}
	for _, claim := range a.Causes {
		check(claim.EvidenceID)
		if !strings.HasPrefix(claim.EvidenceID, "event:") && !strings.HasPrefix(claim.EvidenceID, "chaos:") {
			c.BadCauses = append(c.BadCauses, claim.EvidenceID)
		}
		for _, id := range claim.SupportingEvidence {
			check(id)
		}
	}
	return c
}

// UnderlyingID strips the evidence kind, so event:X and chaos:X both give X.
// incident-cli lists a chaos run as an event with the run ID, so the two
// kinds can name the same thing.
func UnderlyingID(evidenceID string) string {
	if _, rest, ok := strings.Cut(evidenceID, ":"); ok {
		return rest
	}
	return evidenceID
}

// collectEvidenceIDs finds every evidence_id value anywhere in a tool result.
func collectEvidenceIDs(v any, into map[string]bool) {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			if s, ok := val.(string); ok && k == "evidence_id" {
				into[s] = true
				continue
			}
			collectEvidenceIDs(val, into)
		}
	case []any:
		for _, val := range x {
			collectEvidenceIDs(val, into)
		}
	}
}
