package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Baseline returns incident-cli's top three causes as its answer. It goes
// through the same MCP tool as the agent, so the comparison is fair, and it
// is the number the agent has to beat.
type Baseline struct{}

func (Baseline) Name() string { return "baseline" }

func (Baseline) Investigate(ctx context.Context, session *mcp.ClientSession, inc Incident) (*Result, error) {
	start := time.Now()
	res := &Result{Investigator: "baseline", Seen: map[string]bool{}}
	defer func() { res.Duration = time.Since(start) }()

	args := map[string]any{
		"service": inc.Service,
		"at":      inc.DetectedAt.UTC().Format(time.RFC3339),
		"top":     MaxCauses,
	}
	out, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "investigate", Arguments: args})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNoInvestigateTool, err)
	}
	res.Turns, res.ToolCalls = 1, 1
	if out.IsError {
		return nil, fmt.Errorf("investigate failed: %s", resultText(out))
	}

	b, err := json.Marshal(out.StructuredContent)
	if err != nil {
		return nil, err
	}
	var generic any
	_ = json.Unmarshal(b, &generic)
	collectEvidenceIDs(generic, res.Seen)

	var parsed struct {
		Causes []struct {
			EvidenceID string  `json:"evidence_id"`
			Score      float64 `json:"score"`
		} `json:"causes"`
	}
	if err := json.Unmarshal(b, &parsed); err != nil {
		return nil, err
	}
	answer := Answer{Status: StatusInsufficient, Summary: "incident-cli ranking", NextAction: "review the top cause"}
	for i, c := range parsed.Causes {
		if i == MaxCauses {
			break
		}
		answer.Status = StatusIdentified
		answer.Causes = append(answer.Causes, Claim{
			EvidenceID: c.EvidenceID,
			Confidence: []string{"high", "medium", "low"}[i],
			Reasoning:  fmt.Sprintf("ranked %d by incident-cli with score %.3f", i+1, c.Score),
		})
	}
	res.Answer = &answer
	res.Stop = "answered"
	return res, nil
}
