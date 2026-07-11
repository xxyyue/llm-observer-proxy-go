// SPDX-License-Identifier: Apache-2.0

package observer

import "strings"

func BuildConversationSnapshot(runID string, port int, state string, items []Event) map[string]any {
	var latest *Event
	for index := len(items) - 1; index >= 0; index-- {
		if len(requestMessages(items[index].RequestJSON)) > 0 {
			latest = &items[index]
			break
		}
	}
	messages := []any{}
	var exchangeID any
	var updatedAt any
	if latest != nil {
		for _, message := range requestMessages(latest.RequestJSON) {
			messages = append(messages, message)
		}
		if assistant := assistantMessage(latest.Normalized); assistant != nil {
			messages = append(messages, assistant)
		}
		exchangeID = latest.ExchangeID
		updatedAt = latest.CompletedAtMS
	}
	turnCount := 0
	for _, raw := range messages {
		if message, ok := raw.(map[string]any); ok && message["role"] == "assistant" {
			turnCount++
		}
	}
	snapshot := map[string]any{
		"schemaVersion": 2,
		"run_id":        runID,
		"port":          port,
		"state":         state,
		"count":         len(items),
		"messageCount":  len(messages),
		"turnCount":     turnCount,
		"messages":      messages,
		"exchangeId":    exchangeID,
		"updatedAtMs":   updatedAt,
		"totals":        Summarize(items),
	}
	if latestError := latestError(items); latestError != nil {
		snapshot["latestError"] = latestError
	}
	return snapshot
}

func requestMessages(raw any) []map[string]any {
	request, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	messages := make([]map[string]any, 0)
	if system := extractText(request["system"]); strings.TrimSpace(system) != "" {
		messages = append(messages, map[string]any{"role": "system", "content": system})
	}
	if rawMessages, ok := request["messages"].([]any); ok {
		for _, rawMessage := range rawMessages {
			message, ok := rawMessage.(map[string]any)
			if !ok {
				continue
			}
			messages = append(messages, normalizeMessage(message)...)
		}
		return messages
	}
	if input, ok := request["input"].(string); ok {
		return append(messages, map[string]any{"role": "user", "content": input})
	}
	if inputs, ok := request["input"].([]any); ok {
		for _, rawInput := range inputs {
			input, ok := rawInput.(map[string]any)
			if !ok {
				continue
			}
			if role := stringValue(input["role"]); role != "" {
				messages = append(messages, normalizeMessage(input)...)
				continue
			}
			typeName := stringValue(input["type"])
			if strings.HasSuffix(typeName, "_call_output") {
				messages = append(messages, map[string]any{
					"role":         "tool",
					"tool_call_id": firstNonNil(input["call_id"], input["id"]),
					"content":      extractText(firstNonNil(input["output"], input["content"])),
				})
			} else if strings.Contains(typeName, "call") {
				messages = append(messages, map[string]any{"role": "assistant", "content": "", "tool_calls": []any{normalizeToolCall(input)}})
			}
		}
	}
	return messages
}

func normalizeMessage(message map[string]any) []map[string]any {
	role := stringValue(message["role"])
	if role == "" {
		return nil
	}
	content, isList := message["content"].([]any)
	if !isList {
		normalized := map[string]any{"role": role, "content": extractText(message["content"])}
		if calls, ok := message["tool_calls"].([]any); ok && len(calls) > 0 {
			normalized["tool_calls"] = calls
		}
		return []map[string]any{normalized}
	}
	var text []string
	var calls []any
	var results []map[string]any
	for _, raw := range content {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		switch stringValue(item["type"]) {
		case "text", "input_text", "output_text":
			if value := stringValue(item["text"]); value != "" {
				text = append(text, value)
			}
		case "tool_use":
			calls = append(calls, normalizeToolCall(item))
		case "tool_result":
			results = append(results, map[string]any{
				"role":         "tool",
				"tool_call_id": firstNonNil(item["tool_use_id"], item["id"]),
				"content":      extractText(item["content"]),
			})
		}
	}
	normalized := map[string]any{"role": role, "content": strings.Join(text, "\n\n")}
	if len(calls) > 0 {
		normalized["tool_calls"] = calls
	}
	output := []map[string]any{normalized}
	return append(output, results...)
}

func assistantMessage(normalized map[string]any) map[string]any {
	if normalized == nil {
		return nil
	}
	text := extractText(normalized["response_text"])
	tools := anySlice(normalized["response_tool_calls"])
	reasoning := anySlice(normalized["response_reasoning"])
	if strings.TrimSpace(text) == "" && len(tools) == 0 && len(reasoning) == 0 {
		return nil
	}
	message := map[string]any{"role": "assistant", "content": text}
	if len(tools) > 0 {
		calls := make([]any, 0, len(tools))
		for _, raw := range tools {
			if item, ok := raw.(map[string]any); ok {
				calls = append(calls, normalizeToolCall(item))
			}
		}
		message["tool_calls"] = calls
	}
	if len(reasoning) > 0 {
		message["reasoning"] = reasoning
	}
	if finish := normalized["response_finish_reason"]; finish != nil {
		message["finish_reason"] = finish
	}
	return message
}

func normalizeToolCall(item map[string]any) any {
	if _, ok := item["function"].(map[string]any); ok {
		return item
	}
	name := stringValue(item["name"])
	if name == "" {
		name = stringValue(item["type"])
	}
	return map[string]any{
		"id":   firstNonNil(item["call_id"], item["id"]),
		"type": "function",
		"function": map[string]any{
			"name":      name,
			"arguments": firstNonNil(item["arguments"], item["input"]),
		},
	}
}

func latestError(items []Event) map[string]any {
	for index := len(items) - 1; index >= 0; index-- {
		item := items[index]
		if item.Error == nil && (item.ResponseStatus == nil || *item.ResponseStatus < 400) {
			continue
		}
		message := "upstream request failed"
		if item.Error != nil && *item.Error != "" {
			message = *item.Error
		}
		result := map[string]any{
			"message":       message,
			"exchangeId":    item.ExchangeID,
			"path":          item.Path,
			"completedAtMs": item.CompletedAtMS,
		}
		if item.ResponseStatus != nil {
			result["status"] = *item.ResponseStatus
		}
		if request, ok := item.RequestJSON.(map[string]any); ok && request["model"] != nil {
			result["model"] = request["model"]
		}
		return result
	}
	return nil
}

func anySlice(value any) []any {
	if items, ok := value.([]any); ok {
		return items
	}
	return nil
}

func firstNonNil(values ...any) any {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}
