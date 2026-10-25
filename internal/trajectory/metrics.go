// SPDX-License-Identifier: Apache-2.0
package trajectory

import (
	"encoding/json"
	"math"
	"strings"

	"github.com/xxyyue/llm-observer-proxy-go/internal/observer"
)

func number(value any) (float64, bool) {
	switch v := value.(type) {
	case float64:
		return v, true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case json.Number:
		f, e := v.Float64()
		return f, e == nil
	}
	return 0, false
}
func (b *builder) token(value any) (int64, bool) {
	if value == nil {
		return 0, false
	}
	n, ok := number(value)
	if !ok || math.IsNaN(n) || math.IsInf(n, 0) || n < 0 || n >= math.MaxInt64 || math.Trunc(n) != n {
		b.issue("invalid_usage", "Token usage is not a nonnegative integer.")
		return 0, false
	}
	return int64(n), true
}
func (b *builder) metrics(event observer.Event, usage map[string]any) map[string]any {
	result := map[string]any{}
	extra := map[string]any{}
	input, inputOK := b.token(first(usage["prompt_tokens"], usage["input_tokens"]))
	output, outputOK := b.token(first(usage["completion_tokens"], usage["output_tokens"]))
	cached, cachedOK := b.token(first(usage["cache_read_input_tokens"], object(usage["prompt_tokens_details"])["cached_tokens"], object(usage["input_tokens_details"])["cached_tokens"]))
	creation, creationOK := b.token(usage["cache_creation_input_tokens"])
	// Compatibility gateways may return OpenAI usage even on /v1/messages.
	// prompt_tokens is already an inclusive total and must not gain caches twice.
	if strings.HasSuffix(event.Path, "/messages") && usage["prompt_tokens"] == nil && inputOK {
		// Anthropic input_tokens excludes cache creation/read; ATIF includes both.
		input += cached + creation
	}
	if inputOK {
		result["prompt_tokens"] = input
		b.sumInt("total_prompt_tokens", input)
	}
	if outputOK {
		result["completion_tokens"] = output
		b.sumInt("total_completion_tokens", output)
	}
	if cachedOK {
		result["cached_tokens"] = cached
		b.sumInt("total_cached_tokens", cached)
	}
	if creationOK {
		extra["cache_creation_input_tokens"] = creation
	}
	if inputOK && cachedOK && cached > input {
		b.issue("invalid_usage", "Cached tokens exceed total prompt tokens.")
	}
	if inputOK && outputOK {
		b.metricsComplete++
	}
	if inputOK || outputOK || cachedOK {
		b.metricsCount++
	}
	if len(usage) > 0 {
		extra["provider_usage"] = usage
	}
	cost := object(event.Normalized["cost"])
	if amount, ok := number(cost["total_cost_usd"]); ok {
		if amount >= 0 && !math.IsNaN(amount) && !math.IsInf(amount, 0) {
			result["cost_usd"] = amount
			b.costCount++
			total, _ := number(b.totals["total_cost_usd"])
			b.totals["total_cost_usd"] = total + amount
			extra["cost_source"] = "recorded_local_estimate"
			extra["cost_note"] = "Preserved from source log without repricing; not a verified provider bill. Legacy cache pricing may differ."
		} else {
			b.issue("invalid_cost", "Recorded cost is not a nonnegative finite amount.")
		}
	}
	if len(result) == 0 {
		return nil
	}
	if len(extra) > 0 {
		result["extra"] = map[string]any{Namespace: extra}
	}
	return result
}
func (b *builder) sumInt(key string, value int64) {
	total, _ := b.totals[key].(int64)
	b.totals[key] = total + value
}
