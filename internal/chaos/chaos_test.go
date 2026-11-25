package chaos

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The fixture was written by chaos-operator's history module.
var fixture = filepath.Join("..", "..", "testdata", "chaos-experiments.json")

func TestParse(t *testing.T) {
	raw, err := File{Path: fixture}.List(context.Background())
	require.NoError(t, err)
	runs, err := Parse(raw)
	require.NoError(t, err)
	require.Len(t, runs, 4)
	for i := 1; i < len(runs); i++ {
		assert.False(t, runs[i].StartedAt.After(runs[i-1].StartedAt), "newest first")
	}
	assert.Equal(t, "identity/auth-latency/20260115T115000Z", runs[0].ID)
	assert.Equal(t, "auth-latency", runs[0].Experiment)
	assert.Equal(t, "identity", runs[0].Namespace)
	assert.True(t, runs[0].Injected)

	_, err = Parse([]byte("{"))
	assert.Error(t, err)

	// A record from before the injected field still parses.
	old, err := Parse([]byte(`{"items":[{"metadata":{"name":"x"},"status":{"runs":[{"id":"a","outcome":"refused"}]}}]}`))
	require.NoError(t, err)
	assert.False(t, old[0].Injected)
}

func TestFilter(t *testing.T) {
	raw, _ := File{Path: fixture}.List(context.Background())
	runs, _ := Parse(raw)
	until := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)

	assert.Len(t, Filter(runs, "", until.Add(-2*time.Hour), until, false), 2)
	assert.Len(t, Filter(runs, "", until.Add(-2*time.Hour), until, true), 3)
	assert.Len(t, Filter(runs, "checkout", until.Add(-24*time.Hour), until, true), 3)
	assert.Empty(t, Filter(runs, "search", until.Add(-24*time.Hour), until, true))
	assert.Empty(t, Filter(runs, "", until.Add(-time.Hour), until.Add(-30*time.Minute), true))
}

func TestKubectl(t *testing.T) {
	var got []string
	k := Kubectl{Context: "kind-platform", Run: func(_ context.Context, name string, args ...string) ([]byte, error) {
		got = append([]string{name}, args...)
		return []byte(`{"items":[]}`), nil
	}}
	_, err := k.List(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []string{"kubectl", "get", "chaosexperiments.chaos.jamespham.io",
		"--all-namespaces", "--output", "json", "--context", "kind-platform"}, got)
}
