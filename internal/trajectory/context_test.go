// SPDX-License-Identifier: Apache-2.0
package trajectory

import (
	"encoding/json"
	"github.com/xxyyue/llm-observer-proxy-go/internal/observer"
	"strings"
	"testing"
)

// Check the final standard boundary after all tool-result backfills. Expected
// values below are derived independently from the submitted synthetic requests.
func assertProjection(t *testing.T, doc *Document, record int, expected string) {
	t.Helper()
	step := boundaryFor(doc, record)
	got := []map[string]any{}
	for _, result := range step.Observation.Results {
		item := map[string]any{}
		for k, v := range result.Extra["context_message"].(map[string]any) {
			if k != "atif_source" {
				item[k] = v
			}
		}
		item["content"] = result.Content
		got = append(got, item)
	}
	var want any
	if err := decodeJSON([]byte(expected), &want); err != nil {
		t.Fatal(err)
	}
	var normalized any
	_ = decodeJSON([]byte(mustJSON(got)), &normalized)
	if digestJSON(normalized) != digestJSON(want) {
		t.Fatalf("record %d standard projection mismatch got=%s want=%s", record, mustJSON(got), mustJSON(want))
	}
	if step.Extra["context_management"].(map[string]any)["boundary"] != "replace" {
		t.Fatal("not a context reset")
	}
}
func TestCheckpointsAfterFutureToolBackfill(t *testing.T) {
	a := event("a", `{"messages":[{"role":"user","content":"work"}]}`, `{"choices":[{"message":{"tool_calls":[{"id":"call","function":{"name":"read","arguments":"{}"}}]}}]}`)
	b := event("b", `{"messages":[{"role":"user","content":"independent auxiliary"}]}`, `{"choices":[{"message":{"content":"auxiliary answer"}}]}`)
	c := event("c", `{"messages":[{"role":"user","content":"work"},{"role":"assistant","content":"","tool_calls":[{"id":"call","function":{"name":"read","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call","content":"later result"}]}`, `{"choices":[{"message":{"content":"done"}}]}`)
	doc := build(t, []observer.Event{a, b, c}, true)
	if observedSteps(doc)[0].Observation.Results[0].Content != "later result" {
		t.Fatal("result not backfilled")
	}
	assertProjection(t, doc, 2, `[{"role":"user","content":"independent auxiliary"}]`)
	assertProjection(t, doc, 3, `[{"role":"user","content":"work"},{"role":"assistant","content":"","tool_calls":[{"tool_call_id":"call","function_name":"read","arguments":{}}]},{"role":"tool","tool_call_id":"call","content":"later result"}]`)
	if boundaryFor(doc, 3).Extra[Namespace].(map[string]any)["relationship_evidence"] != "structural_context_match" {
		t.Fatal("earlier main context not matched")
	}
	saveFixture(t, "future-tool-backfill", doc)
}
func TestContextOperationsAndFalseCompaction(t *testing.T) {
	summary := strings.Repeat("captured summary detail. ", 9)
	a := event("a", `{"messages":[{"role":"developer","content":"policy"},{"role":"user","content":"long task"}],"tools":[{"type":"function","function":{"name":"a"}}]}`, `{"choices":[{"message":{"content":"answer"}}]}`)
	b := event("b", `{"messages":[{"role":"user","content":"summarize for continuation"}]}`, `{"choices":[{"message":{"content":`+mustJSON(summary)+`}}]}`)
	c := event("c", `{"messages":[{"role":"developer","content":"new policy"},{"role":"user","content":`+mustJSON("This session is being continued from a previous conversation. "+summary)+`}],"tools":[{"type":"function","function":{"name":"b"}}]}`, `{"choices":[{"message":{"content":"continued"}}]}`)
	doc := build(t, []observer.Event{a, b, c}, true)
	boundary := boundaryFor(doc, 3)
	if boundary.Extra["context_management"].(map[string]any)["type"] != "compaction" {
		t.Fatal("adopted summary not represented")
	}
	assertProjection(t, doc, 3, `[{"role":"developer","content":"new policy"},{"role":"user","content":`+mustJSON("This session is being continued from a previous conversation. "+summary)+`}]`)
	if len(observedSteps(doc)) != 3 || observedSteps(doc)[0].Message != "answer" {
		t.Fatal("old execution lost")
	}
	for _, text := range []string{"please explain compaction and summary", "quoted answer", "This session is being continued from a previous conversation. answer"} {
		c.RequestJSON = map[string]any{"messages": []any{map[string]any{"role": "user", "content": text}}, "context_management": map[string]any{"edits": []any{map[string]any{"type": "clear_thinking_20251015"}}}}
		check := build(t, []observer.Event{a, c}, true)
		if boundaryFor(check, 2).Extra["context_management"].(map[string]any)["type"] == "compaction" {
			t.Fatal("keyword or short answer falsely inferred compaction")
		}
	}
	saveFixture(t, "compaction-checkpoint", doc)
}
func mustJSON(v any) string { data, _ := json.Marshal(v); return string(data) }
func TestPruningParametersAndTemporalCandidates(t *testing.T) {
	a := event("a", `{"messages":[{"role":"system","content":"s"},{"role":"user","content":"one"}],"seed":9007199254740993}`, `{"choices":[{"message":{"content":"answer"}}]}`)
	b := event("b", `{"messages":[{"role":"system","content":"s"},{"role":"assistant","content":"answer"},{"role":"user","content":"two"}],"seed":9007199254740995}`, `{"choices":[{"message":{"content":"done"}}]}`)
	doc := build(t, []observer.Event{a, b}, true)
	assertProjection(t, doc, 2, `[{"role":"system","content":"s"},{"role":"assistant","content":"answer"},{"role":"user","content":"two"}]`)
	if boundaryFor(doc, 2).Extra["context_management"].(map[string]any)["type"] != "pruning" {
		t.Fatal("pruned context not detected")
	}
	if boundaryFor(doc, 2).Extra[Namespace].(map[string]any)["configuration_id"] != digestJSON(contextConfiguration(object(b.RequestJSON))) {
		t.Fatal("configuration identity changed")
	}

	a.StartedAtMS = 1
	a.CompletedAtMS = 10
	b.StartedAtMS = 2
	b.CompletedAtMS = 3
	doc = build(t, []observer.Event{a, b}, true)
	if len(boundaryFor(doc, 2).Extra[Namespace].(map[string]any)["parent_source_record_candidates"].([]int)) != 0 {
		t.Fatal("incomplete response used as temporal parent")
	}
	saveFixture(t, "pruning-checkpoint", doc)
}
func TestRawBodyDifferencesAndStreamLargeInteger(t *testing.T) {
	a := event("a", `{"messages":[{"role":"user","content":"one"}]}`, `{"choices":[{"message":{"content":"ok"}}]}`)
	b := a
	b.ExchangeID = "b"
	rawA := `{"wire":1}`
	rawB := `{"wire":2}`
	a.RequestBody = &rawA
	b.RequestBody = &rawB
	doc := build(t, []observer.Event{a, b}, true)
	if boundaryFor(doc, 1).Extra[Namespace].(map[string]any)["input_context_id"] == boundaryFor(doc, 2).Extra[Namespace].(map[string]any)["input_context_id"] {
		t.Fatal("different raw bodies collapsed")
	}
	a.ResponseJSON = nil
	a.Stream = true
	a.Path = "/v1/messages"
	stream := "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"call\",\"name\":\"calc\",\"input\":{}}}\n\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"n\\\":9007199254740993}\"}}\n\ndata: {\"type\":\"message_stop\"}\n\n"
	a.ResponseBody = &stream
	doc = build(t, []observer.Event{a}, true)
	if observedSteps(doc)[0].Tools[0].Arguments["n"].(json.Number).String() != "9007199254740993" {
		t.Fatal("stream integer rounded")
	}
	saveFixture(t, "stream-large-integer", doc)
}
func TestCapturedProviderStateContractAndExplicitHints(t *testing.T) {
	a := event("same", `{"input":"one"}`, `{"id":"r1","output":[{"type":"message","content":[{"type":"output_text","text":"answer"}]}]}`)
	b := a
	b.ContextMetadata = &observer.ContextMetadata{ConversationID: "main", AgentID: "agent", ParentExchangeID: "same", Operation: "compaction"}
	doc := build(t, []observer.Event{a, a, b}, true)
	boundary := boundaryFor(doc, 3)
	if boundary.Extra["context_management"].(map[string]any)["type"] != "compaction" || boundary.Extra[Namespace].(map[string]any)["relationship_evidence"] != "producer_context_metadata" {
		t.Fatal("producer evidence ignored")
	}
	if len(boundary.Extra[Namespace].(map[string]any)["parent_source_record_candidates"].([]int)) != 2 {
		t.Fatal("reused parent IDs collapsed")
	}
	saveFixture(t, "producer-context-hints", doc)
}
