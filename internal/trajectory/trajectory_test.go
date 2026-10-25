// SPDX-License-Identifier: Apache-2.0
package trajectory

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xxyyue/llm-observer-proxy-go/internal/observer"
)

func event(id, request, response string) observer.Event {
	var req, res any
	if err := decodeJSON([]byte(request), &req); err != nil {
		panic(err)
	}
	if err := decodeJSON([]byte(response), &res); err != nil {
		panic(err)
	}
	status := 200
	return observer.Event{ExchangeID: id, RunID: "run-1", Path: "/v1/chat/completions", RequestJSON: req, ResponseJSON: res, ResponseStatus: &status}
}
func saveFixture(t *testing.T, name string, document *Document) {
	t.Helper()
	if root := os.Getenv("ATIF_FIXTURE_DIR"); root != "" {
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := WriteFile(filepath.Join(root, name+".json"), "", document); err != nil {
			t.Fatal(err)
		}
	}
}
func build(t *testing.T, events []observer.Event, strict bool) *Document {
	t.Helper()
	document, err := Build(events, Options{Strict: strict}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return document
}
func hasIssue(document *Document, code string) bool {
	for _, d := range document.Extra[Namespace].(map[string]any)["diagnostics"].([]Diagnostic) {
		if d.Code == code {
			return true
		}
	}
	return false
}

func TestToolResultsAndRepeatedContext(t *testing.T) {
	first := event("one", `{"model":"m","messages":[{"role":"system","content":"s"},{"role":"user","content":"run"}]}`, `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"call-1","type":"function","function":{"name":"shell","arguments":"{\"command\":\"ls\"}"}}]}}],"usage":{"prompt_tokens":5,"completion_tokens":2}}`)
	first.Normalized = map[string]any{"cost": map[string]any{"total_cost_usd": 0.12}}
	second := event("two", `{"model":"m","messages":[{"role":"system","content":"s"},{"role":"user","content":"run"},{"role":"assistant","content":"","tool_calls":[{"id":"call-1","type":"function","function":{"name":"shell","arguments":"{\"command\":\"ls\"}"}}]},{"role":"tool","tool_call_id":"call-1","content":"file.txt"}]}`, `{"choices":[{"message":{"role":"assistant","content":"done"}}],"usage":{"prompt_tokens":9,"completion_tokens":1}}`)
	doc := build(t, []observer.Event{first, second}, true)
	if len(observedSteps(doc)) != 2 {
		t.Fatalf("duplicated context: %#v", doc.Steps)
	}
	step := observedSteps(doc)[0]
	if step.Observation == nil || step.Observation.Results[0].CallID != "call-1" || step.Observation.Results[0].Content != "file.txt" {
		t.Fatal("tool result not on originating step")
	}
	if doc.FinalMetrics["total_prompt_tokens"] != int64(14) || doc.FinalMetrics["total_completion_tokens"] != int64(3) {
		t.Fatal(doc.FinalMetrics)
	}
	if doc.FinalMetrics["total_cost_usd"] != 0.12 {
		t.Fatal("stored cost changed")
	}
	if doc.Steps[1].Metrics != nil || doc.Steps[1].Timestamp != "" {
		t.Fatal("invented metrics/time for supplied context")
	}
	saveFixture(t, "tool-results", doc)
}

func TestRepeatedRealMessagesAndTruncation(t *testing.T) {
	first := event("one", `{"messages":[{"role":"user","content":"again"}]}`, `{"choices":[{"message":{"content":"ok"}}]}`)
	second := event("two", `{"messages":[{"role":"user","content":"again"},{"role":"assistant","content":"ok"},{"role":"user","content":"again"}]}`, `{"choices":[{"message":{"content":"ok"}}]}`)
	third := event("three", `{"messages":[{"role":"assistant","content":"ok"},{"role":"user","content":"continue"}]}`, `{"choices":[{"message":{"content":"finished"}}]}`)
	doc := build(t, []observer.Event{first, second, third}, true)
	if len(observedSteps(doc)) != 3 || observedSteps(doc)[2].Message != "finished" || boundaryFor(doc, 3).Observation.Results[1].Content != "continue" {
		t.Fatalf("real repeated messages dropped: %#v", doc.Steps)
	}
	saveFixture(t, "truncated-context", doc)
}

func TestResponsesStreamUsesTerminalToolCall(t *testing.T) {
	first := event("one", `{"model":"m","instructions":"work","input":"do it"}`, `{}`)
	first.Path = "/v1/responses"
	first.Stream = true
	first.ResponseJSON = nil
	body := `data: {"type":"response.output_item.added","item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"shell","arguments":""}}

data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"{}"}

data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[{"type":"function_call","id":"fc_1","call_id":"call_1","name":"shell","arguments":"{}"}],"usage":{"input_tokens":2,"output_tokens":3}}}

`
	first.ResponseBody = &body
	second := event("two", `{"model":"m","instructions":"work","previous_response_id":"resp_1","input":[{"type":"function_call_output","call_id":"call_1","output":"success"}]}`, `{"id":"resp_2","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}],"usage":{"input_tokens":4,"output_tokens":1}}`)
	second.Path = "/v1/responses"
	doc := build(t, []observer.Event{first, second}, true)
	if len(observedSteps(doc)) != 2 || len(observedSteps(doc)[0].Tools) != 1 || observedSteps(doc)[0].Tools[0].ID != "call_1" {
		t.Fatalf("duplicate stream call: %#v", doc.Steps)
	}
	if observedSteps(doc)[0].Observation == nil {
		t.Fatal("incremental result not linked")
	}
	saveFixture(t, "responses-stream", doc)
}

func TestAnthropicStreamUsageAndThinking(t *testing.T) {
	e := event("one", `{"model":"m","system":[{"type":"text","text":"system","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":"hello"}]}`, `{}`)
	e.Path = "/v1/messages"
	e.Stream = true
	e.ResponseJSON = nil
	body := `data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":2,"cache_read_input_tokens":10,"cache_creation_input_tokens":3}}}

data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"consider"}}

data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"opaque-signature"}}

data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"hello"}}

data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":4}}

data: {"type":"message_stop"}

`
	e.ResponseBody = &body
	doc := build(t, []observer.Event{e}, true)
	step := observedSteps(doc)[0]
	if step.Reasoning != "consider" || step.Metrics["prompt_tokens"] != int64(15) || step.Metrics["cached_tokens"] != int64(10) {
		t.Fatalf("incorrect reasoning/cache accounting: %#v", step)
	}
	saveFixture(t, "anthropic-stream", doc)
}

func TestLossAndAmbiguityStrictMode(t *testing.T) {
	base := event("one", `{"messages":[{"role":"user","content":"hello"}]}`, `{"choices":[{"message":{"content":"hi"}}]}`)
	tests := []struct {
		name   string
		events []observer.Event
		code   string
	}{
		{"unlinked", []observer.Event{base, event("two", `{"messages":[{"role":"user","content":"summary replaces context"}]}`, `{"choices":[{"message":{"content":"next"}}]}`)}, ""},
		{"invalid-arguments", []observer.Event{event("bad", `{"messages":[{"role":"user","content":"hello"}]}`, `{"choices":[{"message":{"tool_calls":[{"id":"call","function":{"name":"shell","arguments":"{broken"}}]}}]}`)}, "invalid_tool_call"},
		{"image", []observer.Event{event("image", `{"messages":[{"role":"user","content":[{"type":"text","text":"see"},{"type":"image_url","image_url":{"url":"https://example.com/image"}}]}]}`, `{"choices":[{"message":{"content":"seen"}}]}`)}, "invalid_image"},
		{"orphan", []observer.Event{event("orphan", `{"messages":[{"role":"user","content":"hello"},{"role":"tool","tool_call_id":"absent","content":"output"}]}`, `{"choices":[{"message":{"content":"hi"}}]}`)}, "unresolved_tool_origin"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc := build(t, tt.events, false)
			if tt.code != "" && !hasIssue(doc, tt.code) {
				t.Fatal(doc.Extra)
			}
			_, err := Build(tt.events, Options{Strict: true}, nil)
			var strict *SemanticError
			if tt.name == "unlinked" || tt.name == "orphan" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if !errors.As(err, &strict) {
				t.Fatalf("strict accepted loss: %v", err)
			}
			saveFixture(t, tt.name, doc)
		})
	}
}

func TestOverlappingAndBranchedRequests(t *testing.T) {
	a := event("a", `{"input":"one"}`, `{"id":"r1","output":[{"type":"message","content":[{"type":"output_text","text":"a"}]}]}`)
	b := event("b", `{"previous_response_id":"r1","input":"two"}`, `{"id":"r2","output":[{"type":"message","content":[{"type":"output_text","text":"b"}]}]}`)
	c := event("c", `{"previous_response_id":"r1","input":"branch"}`, `{"id":"r3","output":[{"type":"message","content":[{"type":"output_text","text":"c"}]}]}`)
	doc := build(t, []observer.Event{a, b, c}, false)
	if boundaryFor(doc, 3).Extra[Namespace].(map[string]any)["relationship_evidence"] != "explicit_parent_reference" {
		t.Fatal("branch claimed linear")
	}
	a.StartedAtMS = 1
	a.CompletedAtMS = 10
	b.StartedAtMS = 2
	b.CompletedAtMS = 3
	doc = build(t, []observer.Event{b, a}, false)
	if !hasIssue(doc, "overlapping_exchanges") || doc.Steps[0].Message != "one" {
		t.Fatal("completion order treated as chronology")
	}
}

func TestUnsupportedStreamAndFailure(t *testing.T) {
	e := event("one", `{"messages":[{"role":"user","content":"hello"}]}`, `{}`)
	e.Stream = true
	e.ResponseJSON = nil
	body := "data: {\"type\":\"content_block_start\",\"content_block\":{\"type\":\"server_tool_use\",\"name\":\"search\"}}\n\ndata: {\"type\":\"message_stop\"}\n\n"
	e.ResponseBody = &body
	doc := build(t, []observer.Event{e}, false)
	if !hasIssue(doc, "unsupported_stream_content") {
		t.Fatal("unsupported SSE content dropped")
	}
	e.Stream = false
	e.ResponseJSON = map[string]any{"error": "unavailable"}
	status := 503
	e.ResponseStatus = &status
	doc = build(t, []observer.Event{e}, false)
	if len(observedSteps(doc)) != 0 || !hasIssue(doc, "failed_exchange") {
		t.Fatal("failure became an assistant answer")
	}
	saveFixture(t, "failed-exchange", doc)
}

func TestReadSnapshotAndProtectSource(t *testing.T) {
	e := event("one", `{"messages":[{"role":"user","content":"hello"}]}`, `{"choices":[{"message":{"content":"hi"}}]}`)
	data, _ := json.Marshal(e)
	root := t.TempDir()
	source := filepath.Join(root, "events.jsonl")
	original := append(append(data, '\n'), []byte(`{"exchange_id":"partial`)...)
	if err := os.WriteFile(source, original, 0o600); err != nil {
		t.Fatal(err)
	}
	doc, err := ExportFile(source, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !hasIssue(doc, "incomplete_tail") {
		t.Fatal("tail not diagnosed")
	}
	if _, err = ExportFile(source, Options{Strict: true}); err == nil {
		t.Fatal("strict accepted incomplete tail")
	}
	for _, alias := range []string{source, filepath.Join(root, "symbolic"), filepath.Join(root, "hard")} {
		if strings.HasSuffix(alias, "symbolic") {
			if err = os.Symlink(source, alias); err != nil {
				t.Fatal(err)
			}
		}
		if strings.HasSuffix(alias, "hard") {
			if err = os.Link(source, alias); err != nil {
				t.Fatal(err)
			}
		}
		if err = WriteFile(alias, source, doc); err == nil {
			t.Fatal("source overwrite accepted", alias)
		}
	}
	unchanged, _ := os.ReadFile(source)
	if string(unchanged) != string(original) {
		t.Fatal("source log modified")
	}
	output := filepath.Join(root, "export.json")
	if err = WriteFile(output, source, doc); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(source, append(append(data, '\n'), []byte("broken\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err = ExportFile(source, Options{}); err == nil {
		t.Fatal("corrupt complete line skipped")
	}
	if _, err = Build(nil, Options{}, nil); err == nil {
		t.Fatal("empty trajectory accepted")
	}
}

func TestInitialPreviousResponseMissing(t *testing.T) {
	e := event("one", `{"previous_response_id":"unobserved","input":"continue"}`, `{"id":"r2","output":[{"type":"message","content":[{"type":"output_text","text":"continued"}]}]}`)
	document := build(t, []observer.Event{e}, false)
	if !hasIssue(document, "unknown_previous_response") || len(observedSteps(document)) != 1 {
		t.Fatal("missing initial history not diagnosed")
	}
	if _, err := Build([]observer.Event{e}, Options{Strict: true}, nil); err != nil {
		t.Fatal("captured request fidelity rejected for unknown provider state")
	}
	saveFixture(t, "missing-prior-response", document)
}

func TestIncompleteUnaryResponsesAndAlternateStreamReasoning(t *testing.T) {
	for _, status := range []string{"incomplete", "failed"} {
		e := event("one", `{"input":"question"}`, `{"id":"r1","status":"`+status+`","incomplete_details":{"reason":"max_output_tokens"},"output":[{"type":"message","content":[{"type":"output_text","text":"partial"}]}]}`)
		e.Path = "/v1/responses"
		doc := build(t, []observer.Event{e}, false)
		if !hasIssue(doc, "incomplete_response") || observedSteps(doc)[0].Extra[Namespace].(map[string]any)["response_state"] != "partial" {
			t.Fatal("incomplete unary output accepted as complete")
		}
		if _, err := Build([]observer.Event{e}, Options{Strict: true}, nil); err != nil {
			t.Fatal("faithfully captured partial output rejected")
		}
		saveFixture(t, "unary-"+status, doc)
	}
	e := event("stream", `{"messages":[{"role":"user","content":"think"}]}`, `{}`)
	e.Stream = true
	e.ResponseJSON = nil
	body := `data: {"choices":[{"delta":{"reasoning":"visible thought"}}]}

data: {"choices":[{"delta":{"content":"answer"},"finish_reason":"stop"}]}

data: [DONE]

`
	e.ResponseBody = &body
	doc := build(t, []observer.Event{e}, true)
	if observedSteps(doc)[0].Reasoning != "visible thought" {
		t.Fatal("visible reasoning discarded")
	}
	saveFixture(t, "chat-reasoning-stream", doc)
}

func TestBufferedStreamWithOpenAIUsageOnAnthropicRoute(t *testing.T) {
	e := event("buffered", `{"stream":true,"messages":[{"role":"user","content":"hello"}]}`, `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"complete"}}],"usage":{"prompt_tokens":20,"completion_tokens":3,"cache_read_input_tokens":10,"cache_creation_input_tokens":2,"prompt_tokens_details":{"cached_tokens":10}}}`)
	e.Path = "/v1/messages"
	e.Stream = true
	body, _ := json.Marshal(e.ResponseJSON)
	bodyText := string(body)
	e.ResponseBody = &bodyText
	doc := build(t, []observer.Event{e}, true)
	if hasIssue(doc, "incomplete_stream") || observedSteps(doc)[0].Metrics["prompt_tokens"] != int64(20) || doc.FinalMetrics["total_prompt_tokens"] != int64(20) {
		t.Fatal("buffered stream or inclusive usage misclassified")
	}
	saveFixture(t, "buffered-cross-protocol", doc)
}

func TestLegacyReusedExchangeIDPreservesSourceRecords(t *testing.T) {
	failed := event("reused", `{"messages":[{"role":"user","content":"retry"}]}`, `{"error":{"message":"rate limited"}}`)
	failureStatus := 429
	failed.ResponseStatus = &failureStatus
	failed.StartedAtMS = 10
	failed.CompletedAtMS = 20
	success := event("reused", `{"messages":[{"role":"user","content":"retry"}]}`, `{"choices":[{"message":{"content":"done"},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":1}}`)
	success.StartedAtMS = 30
	success.CompletedAtMS = 40
	// File order intentionally differs from timestamp order. Provenance must refer
	// to original source record positions, not the export's sort order.
	doc := build(t, []observer.Event{success, failed, failed}, false)
	if !hasIssue(doc, "reused_exchange_id") || len(observedSteps(doc)) != 1 {
		t.Fatal("source records dropped or assistant response duplicated")
	}
	if observedSteps(doc)[0].Extra[Namespace].(map[string]any)["source_record_index"] != 1 {
		t.Fatal("source position changed during sorting")
	}
	if doc.FinalMetrics["total_prompt_tokens"] != int64(4) {
		t.Fatal("failed retries invented usage")
	}
	metadata := doc.FinalMetrics["extra"].(map[string]any)[Namespace].(map[string]any)
	if metadata["aggregation_basis"] != "source_records" || metadata["distinct_provider_inferences_verified"] != false {
		t.Fatal("ambiguous records claimed distinct inferences")
	}
	if _, err := Build([]observer.Event{success, failed, failed}, Options{Strict: true}, nil); err != nil {
		t.Fatal("faithful operational source records rejected")
	}
	saveFixture(t, "legacy-reused-ids", doc)
}

func observedSteps(doc *Document) []Step {
	var result []Step
	for _, s := range doc.Steps {
		if s.Extra[Namespace].(map[string]any)["observed_response"] == true {
			result = append(result, s)
		}
	}
	return result
}
func boundaryFor(doc *Document, record int) Step {
	for _, s := range doc.Steps {
		meta := s.Extra[Namespace].(map[string]any)
		if meta["origin"] == "captured_request_checkpoint" && meta["source_record_index"] == record {
			return s
		}
	}
	panic("missing checkpoint")
}
