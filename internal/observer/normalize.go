// SPDX-License-Identifier: Apache-2.0

package observer

import (
	"encoding/json"
	"math"
	"sort"
	"strings"
)

func Normalize(requestJSON, responseJSON any, responseBody, path string, stream bool, pricing func(string) map[string]float64) map[string]any {
	request, _ := requestJSON.(map[string]any)
	response, _ := responseJSON.(map[string]any)
	normalized := map[string]any{
		"path":                   path,
		"endpoint_type":          endpointType(path, request),
		"request_model":          stringValue(request["model"]),
		"request_stream":         boolValue(request["stream"]),
		"request_messages":       request["messages"],
		"request_input":          request["input"],
		"request_tools":          request["tools"],
		"request_system":         request["system"],
		"response_text":          "",
		"response_tool_calls":    []any{},
		"response_reasoning":     []any{},
		"response_finish_reason": nil,
	}
	if response != nil {
		normalizeResponse(response, normalized)
	}
	if stream && responseBody != "" {
		normalizeStream(responseBody, normalized)
	}
	usage := extractUsage(response, responseBody, stream)
	if len(usage) > 0 {
		normalized["usage"] = usage
		if pricing != nil {
			if cost := computeCost(stringValue(request["model"]), usage, pricing(stringValue(request["model"]))); cost != nil {
				normalized["cost"] = cost
			}
		}
	}
	return normalized
}

func endpointType(path string, request map[string]any) string {
	if path == "/v1/messages" || strings.HasSuffix(path, "/messages") {
		return "messages"
	}
	if strings.Contains(path, "/responses") || request["input"] != nil {
		return "responses"
	}
	if strings.Contains(path, "/chat/completions") || request["messages"] != nil {
		return "chat_completions"
	}
	return "unknown"
}

func normalizeResponse(response map[string]any, normalized map[string]any) {
	if choices, ok := response["choices"].([]any); ok && len(choices) > 0 {
		choice, _ := choices[0].(map[string]any)
		message, _ := choice["message"].(map[string]any)
		normalized["endpoint_type"] = "chat_completions"
		normalized["response_text"] = extractText(message["content"])
		if tools, ok := message["tool_calls"].([]any); ok {
			normalized["response_tool_calls"] = tools
		}
		if reasoning := message["reasoning"]; reasoning != nil {
			normalized["response_reasoning"] = asList(reasoning)
		} else if reasoning := message["reasoning_content"]; reasoning != nil {
			normalized["response_reasoning"] = asList(reasoning)
		}
		normalized["response_finish_reason"] = choice["finish_reason"]
		return
	}
	if output, ok := response["output"].([]any); ok {
		normalized["endpoint_type"] = "responses"
		text, tools, reasoning := normalizeOutput(output)
		normalized["response_text"] = text
		normalized["response_tool_calls"] = tools
		normalized["response_reasoning"] = reasoning
		normalized["response_finish_reason"] = response["status"]
		return
	}
	if content, ok := response["content"].([]any); ok {
		normalized["endpoint_type"] = "messages"
		var text []string
		var tools, reasoning []any
		for _, raw := range content {
			item, _ := raw.(map[string]any)
			switch stringValue(item["type"]) {
			case "text":
				text = append(text, stringValue(item["text"]))
			case "tool_use":
				tools = append(tools, item)
			case "thinking", "redacted_thinking":
				reasoning = append(reasoning, item)
			}
		}
		normalized["response_text"] = strings.Join(text, "")
		normalized["response_tool_calls"] = tools
		normalized["response_reasoning"] = reasoning
		normalized["response_finish_reason"] = response["stop_reason"]
	}
}

func normalizeOutput(output []any) (string, []any, []any) {
	var text []string
	var tools, reasoning []any
	for _, raw := range output {
		item, _ := raw.(map[string]any)
		switch stringValue(item["type"]) {
		case "message":
			text = append(text, extractText(item["content"]))
		case "function_call", "computer_call", "tool_call", "custom_tool_call":
			tools = append(tools, item)
		case "reasoning":
			reasoning = append(reasoning, item)
		}
	}
	return strings.Join(text, ""), tools, reasoning
}

func normalizeStream(body string, normalized map[string]any) {
	var text strings.Builder
	var reasoning []any
	chatTools := make(map[int]map[string]any)
	responseTools := make(map[string]map[string]any)
	anthropicTools := make(map[int]map[string]any)
	anthropicArguments := make(map[int]string)
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var event map[string]any
		if json.Unmarshal([]byte(payload), &event) != nil {
			continue
		}
		if choices, ok := event["choices"].([]any); ok && len(choices) > 0 {
			normalized["endpoint_type"] = "chat_completions"
			choice, _ := choices[0].(map[string]any)
			delta, _ := choice["delta"].(map[string]any)
			text.WriteString(extractText(delta["content"]))
			if calls, ok := delta["tool_calls"].([]any); ok {
				for _, rawCall := range calls {
					call, _ := rawCall.(map[string]any)
					indexValue, _ := numberValue(call["index"])
					index := int(indexValue)
					target := chatTools[index]
					if target == nil {
						target = map[string]any{"type": "function", "function": map[string]any{"name": "", "arguments": ""}}
						chatTools[index] = target
					}
					if call["id"] != nil {
						target["id"] = call["id"]
					}
					function, _ := call["function"].(map[string]any)
					targetFunction := target["function"].(map[string]any)
					if name := stringValue(function["name"]); name != "" {
						targetFunction["name"] = name
					}
					targetFunction["arguments"] = stringValue(targetFunction["arguments"]) + stringValue(function["arguments"])
				}
			}
			if reason := delta["reasoning_content"]; reason != nil {
				reasoning = append(reasoning, reason)
			}
			if choice["finish_reason"] != nil {
				normalized["response_finish_reason"] = choice["finish_reason"]
			}
			continue
		}
		eventType := stringValue(event["type"])
		switch eventType {
		case "response.output_text.delta":
			normalized["endpoint_type"] = "responses"
			text.WriteString(stringValue(event["delta"]))
		case "response.reasoning_summary_text.delta":
			normalized["endpoint_type"] = "responses"
			reasoning = append(reasoning, map[string]any{"type": "reasoning_text_delta", "text": stringValue(event["delta"])})
		case "response.output_item.added", "response.output_item.done":
			if item, ok := event["item"].(map[string]any); ok {
				typeName := stringValue(item["type"])
				if typeName == "function_call" || typeName == "computer_call" || typeName == "tool_call" {
					key := stringValue(firstNonNil(item["call_id"], item["id"]))
					if key == "" {
						key = stringValue(item["name"])
					}
					responseTools[key] = cloneMap(item)
				}
			}
		case "response.function_call_arguments.delta":
			key := stringValue(firstNonNil(event["call_id"], event["item_id"]))
			target := responseTools[key]
			if target == nil {
				target = map[string]any{"type": "function_call", "call_id": key, "arguments": ""}
				responseTools[key] = target
			}
			target["arguments"] = stringValue(target["arguments"]) + stringValue(event["delta"])
		case "response.completed":
			if response, ok := event["response"].(map[string]any); ok {
				normalized["response_finish_reason"] = response["status"]
				if output, ok := response["output"].([]any); ok {
					finalText, finalTools, finalReasoning := normalizeOutput(output)
					if finalText != "" {
						text.Reset()
						text.WriteString(finalText)
					}
					for _, rawTool := range finalTools {
						if tool, ok := rawTool.(map[string]any); ok {
							responseTools[stringValue(firstNonNil(tool["call_id"], tool["id"]))] = cloneMap(tool)
						}
					}
					if len(finalReasoning) > 0 {
						reasoning = finalReasoning
					}
				}
			}
		case "content_block_start":
			if block, ok := event["content_block"].(map[string]any); ok {
				indexValue, _ := numberValue(event["index"])
				index := int(indexValue)
				switch stringValue(block["type"]) {
				case "tool_use":
					anthropicTools[index] = cloneMap(block)
				case "text":
					text.WriteString(stringValue(block["text"]))
				case "thinking", "redacted_thinking":
					reasoning = append(reasoning, block)
				}
			}
		case "content_block_delta":
			normalized["endpoint_type"] = "messages"
			if delta, ok := event["delta"].(map[string]any); ok {
				indexValue, _ := numberValue(event["index"])
				index := int(indexValue)
				switch stringValue(delta["type"]) {
				case "text_delta":
					text.WriteString(stringValue(delta["text"]))
				case "thinking_delta", "signature_delta":
					reasoning = append(reasoning, delta)
				case "input_json_delta":
					anthropicArguments[index] += stringValue(delta["partial_json"])
				}
			}
		case "message_delta":
			if delta, ok := event["delta"].(map[string]any); ok {
				normalized["response_finish_reason"] = delta["stop_reason"]
			}
		}
	}
	if text.Len() > 0 {
		normalized["response_text"] = text.String()
	}
	tools := make([]any, 0, len(chatTools)+len(responseTools)+len(anthropicTools))
	if len(chatTools) > 0 {
		indices := sortedIntKeys(chatTools)
		for _, index := range indices {
			tools = append(tools, chatTools[index])
		}
	} else if len(responseTools) > 0 {
		keys := sortedStringKeys(responseTools)
		for _, key := range keys {
			tools = append(tools, responseTools[key])
		}
	} else if len(anthropicTools) > 0 {
		indices := sortedIntKeys(anthropicTools)
		for _, index := range indices {
			tool := anthropicTools[index]
			if arguments := anthropicArguments[index]; arguments != "" {
				var parsed any
				if json.Unmarshal([]byte(arguments), &parsed) == nil {
					tool["input"] = parsed
				} else {
					tool["input"] = arguments
				}
			}
			tools = append(tools, tool)
		}
	}
	if len(tools) > 0 {
		normalized["response_tool_calls"] = tools
	}
	if len(reasoning) > 0 {
		normalized["response_reasoning"] = reasoning
	}
	if text.Len() == 0 && len(tools) == 0 && len(reasoning) == 0 {
		var response map[string]any
		if json.Unmarshal([]byte(body), &response) == nil {
			normalizeResponse(response, normalized)
		}
	}
}

func cloneMap(input map[string]any) map[string]any {
	output := make(map[string]any, len(input))
	for key, value := range input {
		output[key] = value
	}
	return output
}

func sortedIntKeys[T any](values map[int]T) []int {
	keys := make([]int, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Ints(keys)
	return keys
}

func sortedStringKeys[T any](values map[string]T) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func extractUsage(response map[string]any, body string, stream bool) map[string]int64 {
	usage := map[string]int64{}
	mergeUsage(usage, response["usage"])
	if stream {
		for _, line := range strings.Split(body, "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			var event map[string]any
			if json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &event) != nil {
				continue
			}
			mergeUsage(usage, event["usage"])
			if message, ok := event["message"].(map[string]any); ok {
				mergeUsage(usage, message["usage"])
			}
			if nested, ok := event["response"].(map[string]any); ok {
				mergeUsage(usage, nested["usage"])
			}
		}
		if len(usage) == 0 {
			var payload map[string]any
			if json.Unmarshal([]byte(body), &payload) == nil {
				mergeUsage(usage, payload["usage"])
			}
		}
	}
	if _, ok := usage["total_tokens"]; !ok && (usage["input_tokens"] > 0 || usage["output_tokens"] > 0) {
		usage["total_tokens"] = usage["input_tokens"] + usage["output_tokens"]
	}
	return usage
}

func mergeUsage(target map[string]int64, raw any) {
	usage, ok := raw.(map[string]any)
	if !ok {
		return
	}
	copyNumber := func(targetKey string, sourceKeys ...string) {
		for _, key := range sourceKeys {
			if value, ok := numberValue(usage[key]); ok {
				target[targetKey] = value
				return
			}
		}
	}
	copyNumber("input_tokens", "input_tokens", "prompt_tokens")
	copyNumber("output_tokens", "output_tokens", "completion_tokens")
	copyNumber("total_tokens", "total_tokens")
	copyNumber("cache_read_input_tokens", "cache_read_input_tokens", "cache_read_tokens")
	copyNumber("cache_creation_input_tokens", "cache_creation_input_tokens", "cache_creation_tokens")
	if details, ok := usage["input_tokens_details"].(map[string]any); ok {
		if value, ok := numberValue(details["cached_tokens"]); ok {
			target["cache_read_input_tokens"] = value
		}
	}
}

func computeCost(model string, usage map[string]int64, pricing map[string]float64) map[string]any {
	if len(pricing) == 0 {
		return nil
	}
	input := usage["input_tokens"]
	output := usage["output_tokens"]
	cacheRead := usage["cache_read_input_tokens"]
	cacheCreate := usage["cache_creation_input_tokens"]
	baseInput := max(int64(0), input-cacheRead-cacheCreate)
	inputCost := float64(baseInput) * pricing["prompt_token_price"] / 1_000_000
	outputCost := float64(output) * pricing["completion_token_price"] / 1_000_000
	cacheReadCost := float64(cacheRead) * pricing["cache_read_token_price"] / 1_000_000
	cacheCreatePrice, ok := pricing["cache_creation_token_price"]
	if !ok {
		cacheCreatePrice = pricing["prompt_token_price"]
	}
	cacheCreateCost := float64(cacheCreate) * cacheCreatePrice / 1_000_000
	return map[string]any{
		"model":                   model,
		"currency":                "USD",
		"input_cost_usd":          round12(inputCost),
		"output_cost_usd":         round12(outputCost),
		"cache_read_cost_usd":     round12(cacheReadCost),
		"cache_creation_cost_usd": round12(cacheCreateCost),
		"total_cost_usd":          round12(inputCost + outputCost + cacheReadCost + cacheCreateCost),
	}
}

func Summarize(items []Event) map[string]any {
	integerKeys := []string{"input_tokens", "output_tokens", "total_tokens", "cache_read_input_tokens", "cache_creation_input_tokens"}
	floatKeys := []string{"input_cost_usd", "output_cost_usd", "cache_read_cost_usd", "cache_creation_cost_usd", "total_cost_usd"}
	totals := map[string]any{"usage_record_count": 0, "priced_record_count": 0}
	for _, key := range integerKeys {
		totals[key] = int64(0)
	}
	for _, key := range floatKeys {
		totals[key] = float64(0)
	}
	for _, item := range items {
		if usage, ok := item.Normalized["usage"].(map[string]any); ok {
			totals["usage_record_count"] = totals["usage_record_count"].(int) + 1
			for _, key := range integerKeys {
				value, _ := numberValue(usage[key])
				totals[key] = totals[key].(int64) + value
			}
		} else if usage, ok := item.Normalized["usage"].(map[string]int64); ok {
			totals["usage_record_count"] = totals["usage_record_count"].(int) + 1
			for _, key := range integerKeys {
				totals[key] = totals[key].(int64) + usage[key]
			}
		}
		if cost, ok := item.Normalized["cost"].(map[string]any); ok {
			totals["priced_record_count"] = totals["priced_record_count"].(int) + 1
			for _, key := range floatKeys {
				totals[key] = round12(totals[key].(float64) + floatValue(cost[key]))
			}
		}
	}
	return totals
}

func extractText(content any) string {
	if text, ok := content.(string); ok {
		return text
	}
	items, ok := content.([]any)
	if !ok {
		return ""
	}
	var parts []string
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if text := stringValue(item["text"]); text != "" {
			parts = append(parts, text)
		} else if nested := extractText(item["content"]); nested != "" {
			parts = append(parts, nested)
		}
	}
	return strings.Join(parts, "\n")
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}

func boolValue(value any) bool {
	result, _ := value.(bool)
	return result
}

func asList(value any) []any {
	if items, ok := value.([]any); ok {
		return items
	}
	return []any{value}
}

func numberValue(value any) (int64, bool) {
	switch number := value.(type) {
	case int:
		return int64(number), true
	case int64:
		return number, true
	case float64:
		return int64(number), true
	case json.Number:
		parsed, err := number.Int64()
		return parsed, err == nil
	default:
		return 0, false
	}
}

func floatValue(value any) float64 {
	switch number := value.(type) {
	case float64:
		return number
	case int64:
		return float64(number)
	case int:
		return float64(number)
	default:
		return 0
	}
}

func round12(value float64) float64 {
	return math.Round(value*1e12) / 1e12
}
