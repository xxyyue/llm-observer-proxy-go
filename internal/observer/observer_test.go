// SPDX-License-Identifier: Apache-2.0

package observer

import "testing"

func TestConversationPreservesAnthropicSystemArray(t *testing.T) {
	event := Event{
		ExchangeID:    "exchange-1",
		RunID:         "run-1",
		StartedAtMS:   1,
		CompletedAtMS: 2,
		Path:          "/v1/messages",
		RequestJSON: map[string]any{
			"model": "claude-test",
			"system": []any{
				map[string]any{"type": "text", "text": "first", "cache_control": map[string]any{"type": "ephemeral"}},
				map[string]any{"type": "text", "text": "second"},
			},
			"messages": []any{map[string]any{"role": "user", "content": "hello"}},
		},
		ResponseJSON: map[string]any{
			"content":     []any{map[string]any{"type": "text", "text": "world"}},
			"stop_reason": "end_turn",
			"usage":       map[string]any{"input_tokens": float64(3), "output_tokens": float64(2)},
		},
	}
	event.Normalized = Normalize(event.RequestJSON, event.ResponseJSON, "", event.Path, false, nil)
	snapshot := BuildConversationSnapshot("run-1", 1234, "running", []Event{event})
	messages := snapshot["messages"].([]any)
	system := messages[0].(map[string]any)
	if system["role"] != "system" || system["content"] != "first\nsecond" {
		t.Fatalf("unexpected system message: %#v", system)
	}
	assistant := messages[2].(map[string]any)
	if assistant["content"] != "world" {
		t.Fatalf("unexpected assistant message: %#v", assistant)
	}
}

func TestNormalizeStreamingUsage(t *testing.T) {
	body := "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":7}}}\n\n" +
		"data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n" +
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":4}}\n\n"
	normalized := Normalize(
		map[string]any{"model": "claude-test", "messages": []any{}, "stream": true},
		nil,
		body,
		"/v1/messages",
		true,
		nil,
	)
	if normalized["response_text"] != "hello" {
		t.Fatalf("unexpected stream text: %#v", normalized)
	}
	usage := normalized["usage"].(map[string]int64)
	if usage["input_tokens"] != 7 || usage["output_tokens"] != 4 || usage["total_tokens"] != 11 {
		t.Fatalf("unexpected usage: %#v", usage)
	}
}

func TestNormalizeStreamingToolCalls(t *testing.T) {
	openAIBody := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call-1\",\"type\":\"function\",\"function\":{\"name\":\"lookup\",\"arguments\":\"{\\\"q\\\":\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"\\\"value\\\"}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n"
	openAI := Normalize(map[string]any{"model": "gpt-5.5", "stream": true}, nil, openAIBody, "/v1/chat/completions", true, nil)
	openAITools := openAI["response_tool_calls"].([]any)
	openAIFunction := openAITools[0].(map[string]any)["function"].(map[string]any)
	if openAIFunction["name"] != "lookup" || openAIFunction["arguments"] != `{"q":"value"}` {
		t.Fatalf("OpenAI tool call was not assembled: %#v", openAITools)
	}

	anthropicBody := "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"tool-1\",\"name\":\"lookup\",\"input\":{}}}\n\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"q\\\":\"}}\n\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"\\\"value\\\"}\"}}\n\n"
	anthropic := Normalize(map[string]any{"model": "claude-test", "stream": true}, nil, anthropicBody, "/v1/messages", true, nil)
	anthropicTools := anthropic["response_tool_calls"].([]any)
	input := anthropicTools[0].(map[string]any)["input"].(map[string]any)
	if input["q"] != "value" {
		t.Fatalf("Anthropic tool call was not assembled: %#v", anthropicTools)
	}
}
