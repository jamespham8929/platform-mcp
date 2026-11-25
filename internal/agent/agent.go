// Package agent holds the investigators that platform-mcp's eval compares.
// The Claude investigator calls the MCP tools in a loop, forms hypotheses,
// and submits ranked causes that must cite evidence the tools returned. The
// baseline returns incident-cli's ranking unchanged.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Incident is what a page tells the investigator.
type Incident struct {
	Service    string    `json:"service"`
	DetectedAt time.Time `json:"detected_at"`
	// Summary is the alert text, if there is one.
	Summary string `json:"summary,omitempty"`
}

// Step is one tool call, kept so a run can be read back later.
type Step struct {
	Tool    string          `json:"tool"`
	Input   json.RawMessage `json:"input"`
	Output  json.RawMessage `json:"output"`
	IsError bool            `json:"is_error,omitempty"`
}

// Result is one investigation.
type Result struct {
	Investigator string `json:"investigator"`
	// Model is the requested model. ModelsUsed lists what served each turn,
	// which differs when a refusal fell back to another model.
	Model      string   `json:"model,omitempty"`
	ModelsUsed []string `json:"models_used,omitempty"`
	Answer     *Answer  `json:"answer"`
	// Stop says why the run ended: answered, max_turns, cost_cap, refusal,
	// max_tokens or no_answer.
	Stop      string        `json:"stop"`
	Turns     int           `json:"turns"`
	ToolCalls int           `json:"tool_calls"`
	Usage     Usage         `json:"usage"`
	CostUSD   float64       `json:"cost_usd"`
	Duration  time.Duration `json:"duration_ns"`
	Steps     []Step        `json:"steps,omitempty"`
	// Seen holds every evidence ID a tool returned during the run.
	Seen map[string]bool `json:"-"`
}

// Investigator investigates one incident through an MCP session.
type Investigator interface {
	Name() string
	Investigate(ctx context.Context, session *mcp.ClientSession, inc Incident) (*Result, error)
}

// Messages is the part of the Anthropic client the agent uses.
type Messages interface {
	New(ctx context.Context, params anthropic.BetaMessageNewParams, opts ...option.RequestOption) (*anthropic.BetaMessage, error)
}

type Config struct {
	Model  string
	Effort string
	// MaxTurns bounds the requests in one investigation.
	MaxTurns int
	// MaxCostUSD stops an investigation once its estimated cost passes this.
	MaxCostUSD float64
	MaxTokens  int64
}

func DefaultConfig() Config {
	return Config{
		Model:      "claude-opus-5-5",
		Effort:     "high",
		MaxTurns:   12,
		MaxCostUSD: 1.00,
		MaxTokens:  16000,
	}
}

// Claude is the agent investigator.
type Claude struct {
	Msgs Messages
	Cfg  Config
}

func (c *Claude) Name() string { return "claude" }

const submitTool = "submit_answer"

// systemPrompt stays byte for byte the same across runs so the prompt cache
// hits. Anything that changes per incident goes in the user message.
const systemPrompt = `You investigate production incidents on a Kubernetes platform for the on-call engineer.

You have read-only tools. Every item they return has an evidence_id. Base every claim on those items, and cite their IDs. Do not cite an ID that no tool returned.

The investigate tool ranks candidate causes with a simple statistical model: time decay, service relevance, a base rate per event type, and magnitude. Treat that ranking as a starting point. Check it against what the events say. Timing matters, and so does the dependency relation between the event's service and the paged service. A change to a service the paged service depends on can cause the page. A change to a service that only calls the paged service usually cannot.

A cause must be an event or a chaos run (an ID starting with event: or chaos:). An SLO state or an alert is a symptom. It can support a cause, but it is not one.

If the evidence does not single out a cause, answer insufficient_evidence. That is better than a confident guess, because a wrong answer sends the engineer the wrong way at the worst time.

You suggest the next action. You never take it, and you never say you did.

Finish by calling submit_answer once, with at most three causes, most likely first.`

func (c *Claude) tools(ctx context.Context, session *mcp.ClientSession) ([]anthropic.BetaToolUnionParam, error) {
	list, err := session.ListTools(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("listing MCP tools: %w", err)
	}
	// A fixed order keeps the tool definitions identical between runs, which
	// the prompt cache needs.
	sort.Slice(list.Tools, func(i, j int) bool { return list.Tools[i].Name < list.Tools[j].Name })

	var out []anthropic.BetaToolUnionParam
	for _, t := range list.Tools {
		schema, _ := t.InputSchema.(map[string]any)
		params := anthropic.BetaToolInputSchemaParam{ExtraFields: map[string]any{}}
		for k, v := range schema {
			switch k {
			case "type":
			case "properties":
				params.Properties = v
			case "required":
				for _, r := range v.([]any) {
					params.Required = append(params.Required, fmt.Sprint(r))
				}
			default:
				params.ExtraFields[k] = v
			}
		}
		tool := anthropic.BetaToolParam{
			Name:        t.Name,
			Description: anthropic.String(t.Description),
			InputSchema: params,
		}
		out = append(out, anthropic.BetaToolUnionParam{OfTool: &tool})
	}
	submit := anthropic.BetaToolParam{
		Name: submitTool,
		Description: anthropic.String("Submit your conclusion. Call this once, when you are done. " +
			"The run ends after it."),
		Strict: anthropic.Bool(true),
		InputSchema: anthropic.BetaToolInputSchemaParam{
			Properties:  answerSchema["properties"],
			Required:    answerSchema["required"].([]string),
			ExtraFields: map[string]any{"additionalProperties": false},
		},
	}
	return append(out, anthropic.BetaToolUnionParam{OfTool: &submit}), nil
}

func userPrompt(inc Incident) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Service %s paged at %s.", inc.Service, inc.DetectedAt.UTC().Format(time.RFC3339))
	if inc.Summary != "" {
		fmt.Fprintf(&b, " Alert: %s", inc.Summary)
	}
	b.WriteString(" Find the most likely cause. Use that time as the at argument for the tools.")
	return b.String()
}

func (c *Claude) Investigate(ctx context.Context, session *mcp.ClientSession, inc Incident) (*Result, error) {
	start := time.Now()
	cfg := c.Cfg
	res := &Result{Investigator: c.Name(), Model: cfg.Model, Seen: map[string]bool{}}
	defer func() { res.Duration = time.Since(start) }()

	tools, err := c.tools(ctx, session)
	if err != nil {
		return nil, err
	}
	params := anthropic.BetaMessageNewParams{
		Model:     cfg.Model,
		MaxTokens: cfg.MaxTokens,
		System: []anthropic.BetaTextBlockParam{{
			Text: systemPrompt,
			// Caches the tool definitions and the system prompt together.
			CacheControl: anthropic.NewBetaCacheControlEphemeralParam(),
		}},
		// Caches the growing conversation, so each turn pays full price only
		// for what is new.
		CacheControl: anthropic.NewBetaCacheControlEphemeralParam(),
		Tools:        tools,
		OutputConfig: anthropic.BetaOutputConfigParam{Effort: anthropic.BetaOutputConfigEffort(cfg.Effort)},
		// A refusal on an investigation is retried on another model inside
		// the same call. The served model is recorded per turn.
		Fallbacks: anthropic.BetaFallbacksParamOfDefault(),
		Betas:     []anthropic.AnthropicBeta{anthropic.AnthropicBetaServerSideFallback2026_07_01},
		Messages: []anthropic.BetaMessageParam{
			anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(userPrompt(inc))),
		},
	}

	nudged := false
	for res.Turns < cfg.MaxTurns {
		resp, err := c.Msgs.New(ctx, params)
		if err != nil {
			return res, fmt.Errorf("turn %d: %w", res.Turns+1, err)
		}
		res.Turns++
		res.ModelsUsed = append(res.ModelsUsed, resp.Model)
		turn := Usage{
			InputTokens:      resp.Usage.InputTokens,
			OutputTokens:     resp.Usage.OutputTokens,
			CacheWriteTokens: resp.Usage.CacheCreationInputTokens,
			CacheReadTokens:  resp.Usage.CacheReadInputTokens,
		}
		res.Usage.add(turn)
		priceModel := resp.Model
		if _, ok := prices[priceModel]; !ok {
			priceModel = cfg.Model
		}
		if cost, ok := Cost(priceModel, turn); ok {
			res.CostUSD += cost
		}
		params.Messages = append(params.Messages, resp.ToParam())

		switch resp.StopReason {
		case anthropic.BetaStopReasonRefusal:
			res.Stop = "refusal"
			return res, nil
		case anthropic.BetaStopReasonMaxTokens:
			res.Stop = "max_tokens"
			return res, nil
		}

		var results []anthropic.BetaContentBlockParamUnion
		for _, block := range resp.Content {
			use, ok := block.AsAny().(anthropic.BetaToolUseBlock)
			if !ok {
				continue
			}
			raw := use.JSON.Input.Raw()
			if use.Name == submitTool {
				answer, err := parseAnswer(raw)
				if err != nil {
					results = append(results, anthropic.NewBetaToolResultBlock(use.ID, err.Error(), true))
					continue
				}
				res.Answer = &answer
				res.Stop = "answered"
				return res, nil
			}
			res.ToolCalls++
			text, isErr := c.callTool(ctx, session, use.Name, raw, res)
			results = append(results, anthropic.NewBetaToolResultBlock(use.ID, text, isErr))
		}

		if res.CostUSD >= cfg.MaxCostUSD {
			res.Stop = "cost_cap"
			return res, nil
		}
		if len(results) > 0 {
			params.Messages = append(params.Messages, anthropic.NewBetaUserMessage(results...))
			continue
		}
		// The model ended its turn without calling a tool. Ask once.
		if nudged {
			res.Stop = "no_answer"
			return res, nil
		}
		nudged = true
		params.Messages = append(params.Messages, anthropic.NewBetaUserMessage(
			anthropic.NewBetaTextBlock("Please finish by calling submit_answer.")))
	}
	res.Stop = "max_turns"
	return res, nil
}

// callTool runs one MCP tool and returns the text to hand back to the model.
func (c *Claude) callTool(ctx context.Context, session *mcp.ClientSession, name, rawInput string, res *Result) (string, bool) {
	var args map[string]any
	if err := json.Unmarshal([]byte(rawInput), &args); err != nil {
		return "tool input is not a JSON object: " + err.Error(), true
	}
	out, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	step := Step{Tool: name, Input: json.RawMessage(rawInput)}
	if err != nil {
		step.IsError = true
		step.Output, _ = json.Marshal(err.Error())
		res.Steps = append(res.Steps, step)
		return "the tool call failed: " + err.Error(), true
	}
	text := resultText(out)
	step.IsError = out.IsError
	step.Output = json.RawMessage(text)
	if !json.Valid(step.Output) {
		step.Output, _ = json.Marshal(text)
	}
	res.Steps = append(res.Steps, step)
	if !out.IsError && out.StructuredContent != nil {
		// Round trip through JSON so typed values become maps and slices.
		var generic any
		if b, err := json.Marshal(out.StructuredContent); err == nil && json.Unmarshal(b, &generic) == nil {
			collectEvidenceIDs(generic, res.Seen)
		}
	}
	return text, out.IsError
}

func resultText(out *mcp.CallToolResult) string {
	var parts []string
	for _, c := range out.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			parts = append(parts, tc.Text)
		}
	}
	if len(parts) == 0 && out.StructuredContent != nil {
		if b, err := json.Marshal(out.StructuredContent); err == nil {
			return string(b)
		}
	}
	return strings.Join(parts, "\n")
}

// ErrNoInvestigateTool is returned by the baseline when the server does not
// offer investigate.
var ErrNoInvestigateTool = errors.New("the server has no investigate tool")
