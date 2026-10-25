// SPDX-License-Identifier: Apache-2.0
package trajectory

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/xxyyue/llm-observer-proxy-go/internal/observer"
)

type message struct {
	identity     string
	role         string
	content      any
	originalRole string
	text         string
	reasoning    string
	tools        []ToolCall
	resultID     string
	raw          any
}

func object(v any) map[string]any { m, _ := v.(map[string]any); return m }
func array(v any) []any           { a, _ := v.([]any); return a }
func str(v any) string            { s, _ := v.(string); return s }
func first(values ...any) any {
	for _, v := range values {
		if v != nil {
			return v
		}
	}
	return nil
}
func fingerprint(m message) string {
	if m.identity != "" {
		return m.identity
	}
	b, _ := json.Marshal([]any{m.role, m.value(), m.tools, m.resultID})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
func decodeBody(value any, body *string) any {
	if value != nil {
		return value
	}
	if body != nil {
		var result any
		if decodeJSON([]byte(*body), &result) == nil {
			return result
		}
	}
	return nil
}

func decodeJSON(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return fmt.Errorf("expected one JSON value")
	}
	return nil
}
func (b *builder) content(value any) string { return textOf(b.standardContent(value)) }
func (m message) value() any {
	if m.content != nil {
		return m.content
	}
	return m.text
}

func (b *builder) tool(raw any) (ToolCall, bool) {
	item := object(raw)
	function := object(item["function"])
	if function == nil {
		function = item
	}
	name := str(function["name"])
	id := str(first(item["call_id"], item["id"]))
	arguments := first(function["arguments"], function["input"])
	if text, ok := arguments.(string); ok {
		var parsed any
		if decodeJSON([]byte(text), &parsed) == nil {
			arguments = parsed
		}
	}
	args, ok := arguments.(map[string]any)
	if id == "" || name == "" || !ok || args == nil {
		b.unmapped("invalid_tool_call", "Tool call has no ID/name or its arguments are not a JSON object.", raw)
		return ToolCall{}, false
	}
	return ToolCall{ID: id, Name: name, Arguments: args}, true
}

func (b *builder) parseMessage(raw any) []message {
	item := object(raw)
	role := str(item["role"])
	if role == "" {
		b.unmapped("unsupported_message", "Message has no supported role.", raw)
		return nil
	}
	originalRole := role
	if role == "assistant" {
		role = "agent"
	}
	if role == "developer" {
		role = "system"
	}
	if role != "system" && role != "user" && role != "agent" && role != "tool" {
		b.unmapped("unsupported_role", "Unknown message role.", raw)
		return nil
	}
	content := b.standardContent(item["content"])
	m := message{role: role, text: textOf(content), content: content, originalRole: originalRole, raw: raw}
	if originalRole == "tool" {
		m.resultID = str(item["tool_call_id"])
		return []message{m}
	}
	m.reasoning = str(first(item["reasoning_content"], item["reasoning"]))
	for _, call := range array(item["tool_calls"]) {
		if tool, ok := b.tool(call); ok {
			m.tools = append(m.tools, tool)
		}
	}
	var results []message
	for _, rawPart := range array(item["content"]) {
		part := object(rawPart)
		switch str(part["type"]) {
		case "tool_use":
			if tool, ok := b.tool(part); ok {
				m.tools = append(m.tools, tool)
			}
		case "tool_result":
			results = append(results, message{role: "tool", content: b.standardContent(part["content"]), originalRole: "tool", resultID: str(part["tool_use_id"]), raw: rawPart})
		case "thinking":
			m.reasoning += str(part["thinking"])
		case "redacted_thinking":
			b.unmapped("opaque_reasoning", "Opaque reasoning was retained, not presented as readable reasoning.", rawPart)
		}
	}
	// Anthropic tool-result-only user envelopes are not extra empty user turns.
	if len(results) > 0 && !hasContent(m.value()) && len(m.tools) == 0 && m.reasoning == "" {
		return results
	}
	return append([]message{m}, results...)
}

func (b *builder) request(value any) []message {
	request := object(value)
	if request == nil {
		b.unmapped("invalid_request", "Request is not a JSON object.", value)
		return nil
	}
	var result []message
	if system := request["system"]; system != nil {
		result = append(result, message{role: "system", content: b.standardContent(system), originalRole: "system", raw: system})
	}
	if instructions := request["instructions"]; instructions != nil {
		result = append(result, message{role: "system", content: b.standardContent(instructions), originalRole: "system", raw: instructions})
	}
	if messages, ok := request["messages"].([]any); ok {
		for _, raw := range messages {
			result = append(result, b.parseMessage(raw)...)
		}
		return result
	}
	if input, ok := request["input"].(string); ok {
		return append(result, message{role: "user", text: input, originalRole: "user", raw: input})
	}
	for _, raw := range array(request["input"]) {
		item := object(raw)
		if item["role"] != nil {
			result = append(result, b.parseMessage(raw)...)
			continue
		}
		switch str(item["type"]) {
		case "function_call":
			if tool, ok := b.tool(item); ok {
				result = append(result, message{role: "agent", tools: []ToolCall{tool}, raw: item})
			}
		case "function_call_output":
			result = append(result, message{role: "tool", content: b.standardContent(item["output"]), originalRole: "tool", resultID: str(item["call_id"]), raw: item})
		case "reasoning": // preserve without inventing a second agent invocation
			b.fragments = append(b.fragments, map[string]any{"exchange_id": b.exchange, "source_record_index": b.recordIndex, "kind": "request_reasoning", "value": item})
		default:
			b.unmapped("unsupported_input", "Responses input item could not be mapped.", raw)
		}
	}
	return combineAgents(result)
}

func combineAgents(input []message) []message {
	var result []message
	for _, m := range input {
		if m.role == "agent" && len(result) > 0 && result[len(result)-1].role == "agent" {
			previous := &result[len(result)-1]
			previous.content = joinContent(previous.value(), m.value())
			previous.text = textOf(previous.content)
			previous.reasoning += m.reasoning
			previous.tools = append(previous.tools, m.tools...)
		} else {
			result = append(result, m)
		}
	}
	return result
}

func (b *builder) response(event observer.Event) (message, map[string]any, string, bool) {
	response := object(decodeBody(event.ResponseJSON, event.ResponseBody))
	body := ""
	if event.ResponseBody != nil {
		body = *event.ResponseBody
	}
	normalized := observer.Normalize(event.RequestJSON, response, body, event.Path, event.Stream, nil)
	usage := object(response["usage"])
	responseID := str(response["id"])
	// Legacy capture layers may buffer a requested stream into a complete JSON
	// response. The request stream flag does not describe the captured transport.
	complete := !event.Stream || response != nil
	if event.Stream && response == nil {
		usage = map[string]any{}
		var terminal map[string]any
		var alternateReasoning strings.Builder
		for _, line := range strings.Split(body, "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data == "[DONE]" {
				complete = true
				continue
			}
			if data == "" {
				continue
			}
			var packet map[string]any
			if decodeJSON([]byte(data), &packet) != nil {
				b.unmapped("invalid_stream_event", "Malformed SSE payload retained.", data)
				continue
			}
			b.inspectPacket(packet)
			if choices := array(packet["choices"]); len(choices) > 0 {
				delta := object(object(choices[0])["delta"])
				if delta["reasoning_content"] == nil {
					if reason, ok := delta["reasoning"].(string); ok {
						alternateReasoning.WriteString(reason)
					}
				}
			}
			for k, v := range object(packet["usage"]) {
				usage[k] = v
			}
			for k, v := range object(object(packet["message"])["usage"]) {
				usage[k] = v
			}
			for k, v := range object(object(packet["response"])["usage"]) {
				usage[k] = v
			}
			if id := str(object(packet["message"])["id"]); id != "" {
				responseID = id
			}
			if id := str(packet["id"]); id != "" {
				responseID = id
			}
			if str(packet["type"]) == "message_stop" || object(packet["delta"])["stop_reason"] != nil {
				complete = true
			}
			for _, choice := range array(packet["choices"]) {
				if object(choice)["finish_reason"] != nil {
					complete = true
				}
			}
			switch str(packet["type"]) {
			case "response.completed":
				terminal = object(packet["response"])
				complete = true
			case "response.incomplete", "response.failed", "error":
				b.notice("stream_error", "Stream reported failure or incomplete output; operational evidence is retained.")
				b.fragments = append(b.fragments, map[string]any{"kind": "stream_error", "value": packet, "source_record_index": b.recordIndex})
				terminal = object(packet["response"])
			}
		}
		if alternateReasoning.Len() > 0 {
			normalized["response_reasoning"] = append(array(normalized["response_reasoning"]), alternateReasoning.String())
		}
		if terminal != nil {
			// The terminal Responses object is authoritative: do not combine it with
			// deltas keyed by item_id (which differs from call_id).
			response = terminal
			responseID = str(response["id"])
			normalized = observer.Normalize(event.RequestJSON, response, "", event.Path, false, nil)
		}
	}
	if status := str(response["status"]); status == "incomplete" || status == "failed" {
		complete = false
		b.notice("incomplete_response", "Response reports incomplete or failed generation; partial output is explicitly retained.")
		b.fragments = append(b.fragments, map[string]any{"kind": "incomplete_response", "value": response, "source_record_index": b.recordIndex})
	}
	if event.Stream && !complete {
		b.notice("incomplete_stream", "No terminal stream marker was observed.")
	}
	m := message{role: "agent", text: str(normalized["response_text"]), originalRole: "assistant", raw: response}
	for _, call := range array(normalized["response_tool_calls"]) {
		if tool, ok := b.tool(call); ok {
			m.tools = append(m.tools, tool)
		}
	}
	for _, raw := range array(normalized["response_reasoning"]) {
		if text, ok := raw.(string); ok {
			m.reasoning += text
			continue
		}
		part := object(raw)
		switch str(part["type"]) {
		case "thinking":
			m.reasoning += str(part["thinking"])
		case "thinking_delta":
			m.reasoning += str(part["thinking"])
		case "reasoning_text_delta":
			m.reasoning += str(part["text"])
		case "reasoning":
			for _, summary := range array(part["summary"]) {
				m.reasoning += str(object(summary)["text"])
			}
			for _, content := range array(part["content"]) {
				m.reasoning += str(object(content)["text"])
			}
			if part["encrypted_content"] != nil {
				b.fragments = append(b.fragments, map[string]any{"exchange_id": b.exchange, "source_record_index": b.recordIndex, "kind": "opaque_reasoning", "value": part})
			}
		case "signature_delta": // signature is metadata, not reasoning
		default:
			b.unmapped("opaque_reasoning", "Reasoning block is not readable text.", raw)
		}
	}
	// Detect unsupported media/content which the existing normalizer intentionally
	// reduces to text. Preserve the actual response whenever conversion is lossy.
	if response != nil {
		if choices := array(response["choices"]); len(choices) > 0 {
			if len(choices) > 1 {
				b.unmapped("multiple_choices", "Only the first completion choice is mapped into steps.", response)
			}
			msg := object(object(choices[0])["message"])
			if msg["audio"] != nil || msg["refusal"] != nil || msg["function_call"] != nil {
				b.unmapped("unsupported_response", "Unmapped response fields retained.", msg)
			}
			if content := msg["content"]; content != nil {
				m.content = b.standardContent(content)
				m.text = textOf(m.content)
			}
		}
		if content := response["content"]; content != nil {
			m.content = b.standardContent(content)
			m.text = textOf(m.content)
		}
		if response["output"] != nil {
			m.content = nil
		}
		for _, raw := range array(response["output"]) {
			item := object(raw)
			switch str(item["type"]) {
			case "message":
				content := b.standardContent(item["content"])
				if hasContent(content) {
					if m.content == nil {
						m.content = content
					} else {
						m.content = joinContent(m.content, content)
					}
				}
				m.text = textOf(m.value())
			case "function_call", "reasoning":
			default:
				b.unmapped("unsupported_output", "Responses output item could not be mapped.", raw)
			}
		}
	}
	if !hasContent(m.value()) && len(m.tools) == 0 && m.reasoning == "" && event.Error == nil && (event.ResponseStatus == nil || *event.ResponseStatus < 400) {
		b.notice("empty_response", "No assistant content was returned; an operational record is retained.")
	}
	return m, usage, responseID, complete
}

// inspectPacket catches semantic content which the text normalizer cannot map.
func (b *builder) inspectPacket(packet map[string]any) {
	if choices := array(packet["choices"]); len(choices) > 0 {
		if len(choices) > 1 {
			b.unmapped("multiple_choices", "Only first stream choice is mapped.", packet)
		}
		delta := object(object(choices[0])["delta"])
		if delta["refusal"] != nil || delta["audio"] != nil || delta["function_call"] != nil {
			b.unmapped("unsupported_stream_content", "Unmapped completion delta retained.", delta)
		}
		if content := delta["content"]; content != nil {
			if parts, ok := b.standardContent(content).([]ContentPart); ok && len(parts) > 0 {
				b.unmapped("unsupported_stream_media", "Structured stream content is not assembled; raw packet retained.", packet)
			}
		}
	}
	if str(packet["type"]) == "content_block_start" {
		block := object(packet["content_block"])
		switch str(block["type"]) {
		case "text", "tool_use", "thinking", "redacted_thinking":
		default:
			b.unmapped("unsupported_stream_content", "Unmapped Anthropic content block retained.", packet)
		}
	}
	if str(packet["type"]) == "content_block_delta" {
		delta := object(packet["delta"])
		switch str(delta["type"]) {
		case "text_delta", "input_json_delta", "thinking_delta", "signature_delta":
		default:
			b.unmapped("unsupported_stream_content", "Unmapped Anthropic delta retained.", packet)
		}
	}
	if str(packet["type"]) == "response.output_item.added" || str(packet["type"]) == "response.output_item.done" {
		item := object(packet["item"])
		switch str(item["type"]) {
		case "message":
			if parts, ok := b.standardContent(item["content"]).([]ContentPart); ok && len(parts) > 0 {
				b.unmapped("unsupported_stream_media", "Structured stream content requires terminal response; raw packet retained.", packet)
			}
		case "function_call", "reasoning":
		default:
			b.unmapped("unsupported_stream_content", "Unmapped Responses output retained.", packet)
		}
	}
}
