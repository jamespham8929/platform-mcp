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
			Top1             float64 `json:"top1"`
		} `json:"score"`
	}
	b, err := os.ReadFile(results)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(b, &doc))

	// The README says the baseline names every cause first, since
	// incident-cli learned to trace cascades (its ADR 0006). This holds it
	// to that. The reason each symptom was scaled down must reach the tool
	// output, because that is what an agent reads.
	assert.Equal(t, 1.0, doc.Score.Top1, "the baseline no longer names every gym cause first")
	assert.True(t, bytes.Contains(b, []byte("explained by anomalies on")), "cascade reasons must reach the MCP tool output")
	dirs, err := os.ReadDir("../../gym/recordings")
	require.NoError(t, err)
	assert.Equal(t, len(dirs), doc.Score.Scenarios, "every recording was replayed")
	assert.Zero(t, doc.Score.Errors)
	assert.Equal(t, 1.0, doc.Score.CitationValidity, "the baseline cites only what the tool returned")
}
