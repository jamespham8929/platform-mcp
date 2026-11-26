package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGymCommandErrors(t *testing.T) {
	ctx := context.Background()
	var out, errOut bytes.Buffer
	cases := map[string]struct {
		args []string
		want string
	}{
		"no subcommand":        {[]string{"gym"}, "record|replay"},
		"unknown subcommand":   {[]string{"gym", "train"}, `unknown gym command "train"`},
		"no scenarios":         {[]string{"gym", "record", "--scenarios", t.TempDir()}, "no scenarios"},
		"unknown scenario":     {[]string{"gym", "record", "--scenarios", "../../gym/scenarios", "--only", "nope"}, `no scenario named "nope"`},
		"no recordings":        {[]string{"gym", "replay", "--recordings", t.TempDir()}, "no recordings"},
		"bad flag":             {[]string{"gym", "replay", "--frobnicate"}, "flag provided but not defined"},
		"unknown investigator": {[]string{"gym", "replay", "--recordings", "../../gym/recordings", "--investigator", "oracle"}, `unknown investigator "oracle"`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if name == "unknown investigator" {
				if _, err := os.Stat("../../gym/recordings"); err != nil {
					t.Skip("no recordings yet")
				}
			}
			assert.ErrorContains(t, run(ctx, tc.args, &out, &errOut), tc.want)
		})
	}
}

// TestGymReplayOfTheCommittedRecordings is the check CI shows as the gym
// scorecard. It runs the real incident-cli and a real Prometheus.
func TestGymReplayOfTheCommittedRecordings(t *testing.T) {
	incident := os.Getenv("INCIDENT_CLI")
	if incident == "" {
		t.Skip("set INCIDENT_CLI to replay the gym recordings")
	}
	for _, bin := range []string{"prometheus", "promtool"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s is not on PATH", bin)
		}
	}
	if _, err := os.Stat("../../gym/recordings"); err != nil {
		t.Skip("no recordings yet")
	}
	results := filepath.Join(t.TempDir(), "results.json")
	var out, errOut bytes.Buffer
	err := run(context.Background(), []string{"gym", "replay",
		"--recordings", "../../gym/recordings", "--gym", "../../gym",
		"--incident-cli", incident, "--out", results}, &out, &errOut)
	require.NoError(t, err, errOut.String())
	assert.Contains(t, out.String(), "distracted")
	assert.Contains(t, out.String(), "inventory-latency")

	var doc struct {
		Score struct {
			Scenarios        int     `json:"scenarios"`
			Errors           int     `json:"errors"`
			CitationValidity float64 `json:"citation_validity"`
		} `json:"score"`
		Graded []struct {
			Scenario string `json:"scenario"`
			Rank     int    `json:"rank"`
		} `json:"graded"`
	}
	b, err := os.ReadFile(results)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(b, &doc))

	// The README scorecard and ADR 0004 report these ranks. The tuning
	// scenarios shaped incident-cli's cascade rules (its ADR 0006), so they
	// should stay at 1. The held-out ranks are what the rules got on
	// incidents they were not written for. If a change to incident-cli moves
	// any rank, update the scorecard and the ADR with it.
	got := map[string]int{}
	for _, g := range doc.Graded {
		got[g.Scenario] = g.Rank
	}
	assert.Equal(t, gymRanks, got, "the baseline's ranks moved (0 is a miss)")

	// The reason each symptom was scaled down must reach the tool output,
	// because that is what an agent reads.
	assert.True(t, bytes.Contains(b, []byte("explained by anomalies on")), "cascade reasons must reach the MCP tool output")
	dirs, err := os.ReadDir("../../gym/recordings")
	require.NoError(t, err)
	assert.Equal(t, len(dirs), doc.Score.Scenarios, "every recording was replayed")
	assert.Zero(t, doc.Score.Errors)
	assert.Equal(t, 1.0, doc.Score.CitationValidity, "the baseline cites only what the tool returned")
}

// gymRanks is the rank of the planted cause in the baseline's answer for
// each committed recording. 0 means the cause was not in the top three.
var gymRanks = map[string]int{
	// Tuning.
	"checkout-bad-deploy":                1,
	"checkout-pod-kill":                  1,
	"inventory-latency":                  1,
	"inventory-latency-deploy-after":     1,
	"inventory-pod-kill-frontend-deploy": 1,
	// Held-out.
	"checkout-latency":                         0,
	"checkout-latency-inventory-deploy":        0,
	"checkout-pod-kill-inventory-deploy-after": 1,
	"checkout-timeout-config":                  0,
	"frontend-bad-deploy":                      1,
	"frontend-latency":                         2,
	"inventory-bad-deploy":                     1,
	"inventory-latency-frontend-deploy-after":  1,
	"inventory-latency-inventory-flag":         3,
	"inventory-pod-kill-checkout-flag":         1,
}
