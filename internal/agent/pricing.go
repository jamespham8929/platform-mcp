package agent

// Price is a model's list price in US dollars per million tokens. Writing to
// the five minute cache costs 1.25 times the input price.
type Price struct {
	Input, Output, CacheRead float64
}

// prices are Anthropic's first-party list prices. Costs computed from them are
// estimates for comparing runs, not a bill. Update this table when prices
// change, and keep the model ID in every result so old runs stay comparable.
var prices = map[string]Price{
	"claude-fable-5-2":  {Input: 8, Output: 40, CacheRead: 0.20},
	"claude-opus-5-5":   {Input: 4, Output: 20, CacheRead: 0.20},
	"claude-opus-5":     {Input: 5, Output: 25, CacheRead: 0.50},
	"claude-opus-4-8":   {Input: 5, Output: 25, CacheRead: 0.50},
	"claude-sonnet-5-5": {Input: 2, Output: 10, CacheRead: 0.10},
	"claude-sonnet-5":   {Input: 2, Output: 10, CacheRead: 0.20},
	"claude-haiku-5-5":  {Input: 0.10, Output: 0.50, CacheRead: 0.01},
}

// Usage counts tokens across every request in one investigation.
type Usage struct {
	InputTokens      int64 `json:"input_tokens"`
	OutputTokens     int64 `json:"output_tokens"`
	CacheWriteTokens int64 `json:"cache_write_tokens"`
	CacheReadTokens  int64 `json:"cache_read_tokens"`
}

func (u *Usage) add(o Usage) {
	u.InputTokens += o.InputTokens
	u.OutputTokens += o.OutputTokens
	u.CacheWriteTokens += o.CacheWriteTokens
	u.CacheReadTokens += o.CacheReadTokens
}

// Cost returns the estimated cost of some usage on a model, and false when
// the model has no price in the table.
func Cost(model string, u Usage) (float64, bool) {
	p, ok := prices[model]
	if !ok {
		return 0, false
	}
	const perToken = 1e-6
	return perToken * (float64(u.InputTokens)*p.Input +
		float64(u.CacheWriteTokens)*p.Input*1.25 +
		float64(u.CacheReadTokens)*p.CacheRead +
		float64(u.OutputTokens)*p.Output), true
}
