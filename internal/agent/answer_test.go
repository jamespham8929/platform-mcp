package agent

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseAnswer(t *testing.T) {
	a, err := parseAnswer(`{"status":"insufficient_evidence","causes":[],"summary":"s","next_action":"n"}`)
	require.NoError(t, err)
	assert.Equal(t, StatusInsufficient, a.Status)

	_, err = parseAnswer(`{"status":"sure","causes":[]}`)
	assert.Error(t, err, "unknown status")
	_, err = parseAnswer(`{"status":"identified","causes":[]}`)
	assert.Error(t, err, "identified needs a cause")
	_, err = parseAnswer(`not json`)
	assert.Error(t, err)

	many := `{"status":"identified","summary":"s","next_action":"n","causes":[
		{"evidence_id":"event:a"},{"evidence_id":"event:b"},{"evidence_id":"event:c"},{"evidence_id":"event:d"}]}`
	a, err = parseAnswer(many)
	require.NoError(t, err)
	assert.Len(t, a.Causes, MaxCauses, "extra causes are dropped, not graded")
}

func TestCheckCitations(t *testing.T) {
	seen := map[string]bool{"event:a": true, "slo:checkout/availability": true}
	a := Answer{Causes: []Claim{
		{EvidenceID: "event:a", SupportingEvidence: []string{"slo:checkout/availability", "event:zzz"}},
		{EvidenceID: "slo:checkout/availability"},
	}}
	c := CheckCitations(a, seen)
	assert.Equal(t, 4, c.Total)
	assert.Equal(t, 3, c.Valid)
	assert.Equal(t, []string{"event:zzz"}, c.Unknown)
	assert.Equal(t, []string{"slo:checkout/availability"}, c.BadCauses, "an SLO is a symptom")
	assert.InDelta(t, 0.75, c.Share(), 1e-9)
	assert.Equal(t, 1.0, Citations{}.Share())
}

func TestUnderlyingID(t *testing.T) {
	assert.Equal(t, "payments/nightly/20260115T114501Z", UnderlyingID("chaos:payments/nightly/20260115T114501Z"))
	assert.Equal(t, "payments/nightly/20260115T114501Z", UnderlyingID("event:payments/nightly/20260115T114501Z"))
	assert.Equal(t, "plain", UnderlyingID("plain"))
}

func TestCollectEvidence(t *testing.T) {
	var v any
	require.NoError(t, json.Unmarshal([]byte(`{
		"causes": [{"evidence_id": "event:a"}],
		"events": [
			{"evidence_id": "event:a", "source": "metric_anomaly", "service": "inventory",
			 "metadata": {"query": "upstream_p99_latency"}},
			{"evidence_id": "event:b", "source": "deploy", "service": "checkout", "metadata": {"evidence_id": "event:c"}}
		],
		"note": "evidence_id"
	}`), &v))
	seen, info := map[string]bool{}, map[string]Evidence{}
	collectEvidence(v, seen, info)
	assert.Equal(t, map[string]bool{"event:a": true, "event:b": true, "event:c": true}, seen)
	assert.Equal(t, map[string]Evidence{
		// The bare cause entry has no attributes and must not wipe out the
		// event entry that has them.
		"event:a": {Source: "metric_anomaly", Service: "inventory", Query: "upstream_p99_latency"},
		"event:b": {Source: "deploy", Service: "checkout"},
	}, info)
}

func TestCost(t *testing.T) {
	cost, ok := Cost("claude-opus-5-5", Usage{
		InputTokens: 1_000_000, OutputTokens: 1_000_000,
		CacheWriteTokens: 1_000_000, CacheReadTokens: 1_000_000,
	})
	require.True(t, ok)
	assert.InDelta(t, 4+20+5+0.20, cost, 1e-9)

	_, ok = Cost("some-future-model", Usage{InputTokens: 1})
	assert.False(t, ok)
}
