package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jamespham8929/platform-mcp/internal/incidentcli"
	"github.com/jamespham8929/platform-mcp/internal/tools"
)

var incidentTime = time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)

// session serves the investigate tool from a recorded incident-cli output.
func session(t *testing.T) *mcp.ClientSession {
	t.Helper()
	out, err := os.ReadFile(filepath.Join("..", "..", "testdata", "investigate-v1.json"))
	require.NoError(t, err)
	server := tools.NewServer(tools.Config{
		Investigator: &incidentcli.Runner{
			Sources: incidentcli.Sources{EventFiles: []string{"events.json"}},
			Exec: func(context.Context, string, ...string) ([]byte, []byte, error) {
				return out, nil, nil
			},
		},
		Now: func() time.Time { return incidentTime },
	}, "test")
	st, ct := mcp.NewInMemoryTransports()
	_, err = server.Connect(context.Background(), st, nil)
	require.NoError(t, err)
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "t"}, nil).
		Connect(context.Background(), ct, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// fakeAPI plays back Messages API responses in order and keeps each request
// it received. The real SDK builds the requests and parses the responses,
// so a shape the SDK does not accept fails here.
type fakeAPI struct {
	mu        sync.Mutex
	responses []string
	requests  []map[string]any
	headers   []http.Header
}

func (f *fakeAPI) server(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/messages", r.URL.Path)
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		require.NoError(t, json.Unmarshal(body, &req))
		f.mu.Lock()
		defer f.mu.Unlock()
		f.requests = append(f.requests, req)
		f.headers = append(f.headers, r.Header.Clone())
		require.NotEmpty(t, f.responses, "the agent sent more requests than the test expected")
		resp := f.responses[0]
		f.responses = f.responses[1:]
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(resp))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func claude(t *testing.T, api *fakeAPI, cfg Config) *Claude {
	srv := api.server(t)
	client := anthropic.NewClient(
		option.WithBaseURL(srv.URL),
		option.WithAPIKey("test-key"),
		option.WithMaxRetries(0),
	)
	return &Claude{Msgs: &client.Beta.Messages, Cfg: cfg}
}

func message(model, stop string, content []map[string]any, in, out, cacheWrite, cacheRead int) string {
	b, _ := json.Marshal(map[string]any{
		"id": "msg_test", "type": "message", "role": "assistant", "model": model,
		"content": content, "stop_reason": stop, "stop_sequence": nil,
		"usage": map[string]any{
			"input_tokens": in, "output_tokens": out,
			"cache_creation_input_tokens": cacheWrite, "cache_read_input_tokens": cacheRead,
		},
	})
	return string(b)
}

// Opus 5.5 returns thinking blocks with the text left out by default. They
// must survive the round trip into the next request.
var thinking = map[string]any{"type": "thinking", "thinking": "", "signature": "c2lnbmF0dXJl"}

func toolUse(id, name string, input any) map[string]any {
	return map[string]any{"type": "tool_use", "id": id, "name": name, "input": input}
}

func validAnswer(cause string, support ...string) map[string]any {
	if support == nil {
		support = []string{}
	}
	return map[string]any{
		"status": "identified",
		"causes": []any{map[string]any{
			"evidence_id": cause, "confidence": "high",
			"reasoning": "deployed 8 minutes before the page", "supporting_evidence": support,
		}},
		"summary":     "the checkout deploy broke payments",
		"next_action": "roll back checkout v4821",
	}
}

var inc = Incident{Service: "checkout", DetectedAt: incidentTime}

func TestClaudeInvestigatesAndCites(t *testing.T) {
	api := &fakeAPI{responses: []string{
		message("claude-opus-5-5", "tool_use", []map[string]any{
			thinking,
			toolUse("toolu_1", "investigate", map[string]any{"service": "checkout", "at": "2026-01-15T12:00:00Z"}),
		}, 900, 120, 4000, 0),
		message("claude-opus-5-5", "tool_use", []map[string]any{
			thinking,
			toolUse("toolu_2", "submit_answer", validAnswer("event:dep-checkout-4821", "event:made-up-id")),
		}, 300, 200, 0, 4900),
	}}
	c := claude(t, api, DefaultConfig())

	res, err := c.Investigate(context.Background(), session(t), inc)
	require.NoError(t, err)

	assert.Equal(t, "answered", res.Stop)
	assert.Equal(t, 2, res.Turns)
	assert.Equal(t, 1, res.ToolCalls)
	require.NotNil(t, res.Answer)
	assert.Equal(t, "event:dep-checkout-4821", res.Answer.Causes[0].EvidenceID)
	assert.Equal(t, []string{"claude-opus-5-5", "claude-opus-5-5"}, res.ModelsUsed)

	// The agent invented one supporting ID. The check catches it.
	cites := CheckCitations(*res.Answer, res.Seen)
	assert.Equal(t, 2, cites.Total)
	assert.Equal(t, 1, cites.Valid)
	assert.Equal(t, []string{"event:made-up-id"}, cites.Unknown)

	assert.Equal(t, Usage{InputTokens: 1200, OutputTokens: 320, CacheWriteTokens: 4000, CacheReadTokens: 4900}, res.Usage)
	want, _ := Cost("claude-opus-5-5", res.Usage)
	assert.InDelta(t, want, res.CostUSD, 1e-12)

	require.Len(t, res.Steps, 1)
	assert.Equal(t, "investigate", res.Steps[0].Tool)
	assert.Contains(t, string(res.Steps[0].Output), "dep-checkout-4821")
}

// The request has to match what the API documents. These are the fields a
// typo would silently break: caching, effort, fallbacks and the strict tool.
func TestClaudeRequestShape(t *testing.T) {
	api := &fakeAPI{responses: []string{
		message("claude-opus-5-5", "tool_use", []map[string]any{
			toolUse("toolu_1", "submit_answer", map[string]any{
				"status": "insufficient_evidence", "causes": []any{},
				"summary": "nothing stands out", "next_action": "check the dashboards",
			}),
		}, 10, 10, 0, 0),
	}}
	_, err := claude(t, api, DefaultConfig()).Investigate(context.Background(), session(t), inc)
	require.NoError(t, err)

	req := api.requests[0]
	assert.Equal(t, "claude-opus-5-5", req["model"])
	assert.Equal(t, "default", req["fallbacks"])
	assert.Equal(t, map[string]any{"effort": "high"}, req["output_config"])
	assert.Equal(t, map[string]any{"type": "ephemeral"}, req["cache_control"])
	assert.NotContains(t, req, "thinking", "Opus 5.5 always thinks; sending a thinking config is not needed")
	assert.NotContains(t, req, "tool_choice", "forced tool choice is rejected on this model")
	assert.Contains(t, api.headers[0].Get("anthropic-beta"), "server-side-fallback-2026-07-01")

	system := req["system"].([]any)[0].(map[string]any)
	assert.Equal(t, map[string]any{"type": "ephemeral"}, system["cache_control"])
	assert.NotContains(t, system["text"], "checkout", "per-incident text in the system prompt breaks the cache")

	var names []string
	var submit map[string]any
	for _, raw := range req["tools"].([]any) {
		tool := raw.(map[string]any)
		names = append(names, tool["name"].(string))
		if tool["name"] == "submit_answer" {
			submit = tool
		}
	}
	assert.Equal(t, []string{"investigate", "submit_answer"}, names)
	require.NotNil(t, submit)
	assert.Equal(t, true, submit["strict"])
	schema := submit["input_schema"].(map[string]any)
	assert.Equal(t, false, schema["additionalProperties"])
	assert.ElementsMatch(t, []any{"status", "causes", "summary", "next_action"}, schema["required"])

	user := req["messages"].([]any)[0].(map[string]any)
	assert.Equal(t, "user", user["role"])
	assert.Contains(t, mustJSON(t, user), "2026-01-15T12:00:00Z")
}

func TestClaudeSendsThinkingBlocksBack(t *testing.T) {
	api := &fakeAPI{responses: []string{
		message("claude-opus-5-5", "tool_use", []map[string]any{
			thinking, toolUse("toolu_1", "investigate", map[string]any{"service": "checkout"}),
		}, 1, 1, 0, 0),
		message("claude-opus-5-5", "tool_use", []map[string]any{
			toolUse("toolu_2", "submit_answer", validAnswer("event:dep-checkout-4821")),
		}, 1, 1, 0, 0),
	}}
	_, err := claude(t, api, DefaultConfig()).Investigate(context.Background(), session(t), inc)
	require.NoError(t, err)

	msgs := api.requests[1]["messages"].([]any)
	require.Len(t, msgs, 3)
	assistant := msgs[1].(map[string]any)
	assert.Equal(t, "assistant", assistant["role"])
	first := assistant["content"].([]any)[0].(map[string]any)
	assert.Equal(t, "thinking", first["type"])
	assert.Equal(t, "c2lnbmF0dXJl", first["signature"])

	results := msgs[2].(map[string]any)["content"].([]any)
	require.Len(t, results, 1)
	assert.Equal(t, "tool_result", results[0].(map[string]any)["type"])
	assert.Equal(t, "toolu_1", results[0].(map[string]any)["tool_use_id"])
}

func TestClaudeGetsABadAnswerBackAsAnError(t *testing.T) {
	bad := map[string]any{"status": "identified", "causes": []any{}, "summary": "x", "next_action": "y"}
	api := &fakeAPI{responses: []string{
		message("claude-opus-5-5", "tool_use", []map[string]any{toolUse("toolu_1", "submit_answer", bad)}, 1, 1, 0, 0),
		message("claude-opus-5-5", "tool_use", []map[string]any{
			toolUse("toolu_2", "submit_answer", validAnswer("event:dep-checkout-4821")),
		}, 1, 1, 0, 0),
	}}
	res, err := claude(t, api, DefaultConfig()).Investigate(context.Background(), session(t), inc)
	require.NoError(t, err)
	assert.Equal(t, "answered", res.Stop)

	results := api.requests[1]["messages"].([]any)[2].(map[string]any)["content"].([]any)
	assert.Equal(t, true, results[0].(map[string]any)["is_error"])
}

func TestClaudeStops(t *testing.T) {
	refusal := message("claude-opus-5-5", "refusal", []map[string]any{}, 1, 1, 0, 0)
	text := message("claude-opus-5-5", "end_turn", []map[string]any{{"type": "text", "text": "I think it was the deploy."}}, 1, 1, 0, 0)
	loop := message("claude-opus-5-5", "tool_use", []map[string]any{
		toolUse("toolu_x", "investigate", map[string]any{"service": "checkout"}),
	}, 1, 1, 0, 0)
	expensive := message("claude-opus-5-5", "tool_use", []map[string]any{
		toolUse("toolu_x", "investigate", map[string]any{"service": "checkout"}),
	}, 1_000_000, 0, 0, 0)

	cases := []struct {
		name      string
		responses []string
		cfg       func(*Config)
		stop      string
		turns     int
	}{
		{"refusal", []string{refusal}, nil, "refusal", 1},
		{"no answer after a nudge", []string{text, text}, nil, "no_answer", 2},
		{"turn limit", []string{loop, loop, loop}, func(c *Config) { c.MaxTurns = 3 }, "max_turns", 3},
		{"cost limit", []string{expensive}, func(c *Config) { c.MaxCostUSD = 1 }, "cost_cap", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultConfig()
			if tc.cfg != nil {
				tc.cfg(&cfg)
			}
			res, err := claude(t, &fakeAPI{responses: tc.responses}, cfg).
				Investigate(context.Background(), session(t), inc)
			require.NoError(t, err)
			assert.Equal(t, tc.stop, res.Stop)
			assert.Equal(t, tc.turns, res.Turns)
			assert.Nil(t, res.Answer)
		})
	}
}

func TestClaudeRecordsAFallbackModel(t *testing.T) {
	api := &fakeAPI{responses: []string{
		message("claude-opus-4-8", "tool_use", []map[string]any{
			toolUse("toolu_1", "submit_answer", validAnswer("event:dep-checkout-4821")),
		}, 1000, 100, 0, 0),
	}}
	res, err := claude(t, api, DefaultConfig()).Investigate(context.Background(), session(t), inc)
	require.NoError(t, err)
	assert.Equal(t, []string{"claude-opus-4-8"}, res.ModelsUsed)
	want, _ := Cost("claude-opus-4-8", Usage{InputTokens: 1000, OutputTokens: 100})
	assert.InDelta(t, want, res.CostUSD, 1e-12, "the served model's price applies")
}

func TestBaselineReturnsTheRanking(t *testing.T) {
	res, err := Baseline{}.Investigate(context.Background(), session(t), inc)
	require.NoError(t, err)
	require.NotNil(t, res.Answer)
	require.Len(t, res.Answer.Causes, 3)
	assert.Equal(t, "event:dep-checkout-4821", res.Answer.Causes[0].EvidenceID)
	assert.Equal(t, 1.0, CheckCitations(*res.Answer, res.Seen).Share())
}

func mustJSON(t *testing.T, v any) string {
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return string(b)
}
