package slo

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jamespham8929/platform-mcp/internal/promapi"
)

func TestWindowOrder(t *testing.T) {
	assert.Less(t, windowSeconds("30s"), windowSeconds("5m"))
	assert.Less(t, windowSeconds("30m"), windowSeconds("1h"))
	assert.Less(t, windowSeconds("6h"), windowSeconds("3d"))
	assert.Equal(t, 1e18, windowSeconds("weird"))
	assert.Equal(t, 1e18, windowSeconds("5w"))
	assert.Equal(t, 1e18, windowSeconds("x"))
}

type fake map[string][]promapi.Sample

func (f fake) Query(_ context.Context, q string, _ time.Time) ([]promapi.Sample, error) {
	if s, ok := f[q]; ok {
		return s, nil
	}
	return nil, nil
}

func TestReplicasAndNeverEvaluated(t *testing.T) {
	at := time.Unix(10_000, 0).UTC()
	q := fake{
		`slo_tracker_alert_state{service="api"}`: {
			{Labels: map[string]string{"slo": "avail", "severity": "warning"}, Value: 1},
			{Labels: map[string]string{"slo": "avail", "severity": "none"}, Value: 0},
			{Labels: map[string]string{"severity": "none"}, Value: 1}, // no slo label, ignored
		},
		`slo_tracker_last_evaluation_timestamp_seconds{service="api"}`: {
			{Labels: map[string]string{"slo": "avail", "pod": "a"}, Value: 9_000},
			{Labels: map[string]string{"slo": "avail", "pod": "b"}, Value: 9_900},
		},
		`slo_tracker_window_sufficient_data{service="api"}`: {
			{Labels: map[string]string{"slo": "never", "window": "1h"}, Value: 0},
		},
	}
	states, err := Read(context.Background(), q, "api", at)
	require.NoError(t, err)
	require.Len(t, states, 2)
	assert.Equal(t, "warning", states[0].Severity)
	assert.InDelta(t, 100, states[0].SecondsSinceEval, 1e-9, "the newest replica wins")
	assert.False(t, states[0].Stale)
	assert.Equal(t, []Window{}, states[0].Windows)
	assert.Equal(t, "never", states[1].SLO)
	assert.True(t, states[1].Stale, "never evaluated counts as stale")
}

type failing struct{}

func (failing) Query(context.Context, string, time.Time) ([]promapi.Sample, error) {
	return nil, errors.New("connection refused")
}

func TestReadError(t *testing.T) {
	_, err := Read(context.Background(), failing{}, "api", time.Now())
	assert.ErrorContains(t, err, "slo_tracker_alert_state")
	assert.ErrorContains(t, err, "connection refused")
}
