// SPDX-License-Identifier: Apache-2.0
package trajectory

import (
	"encoding/json"
	"github.com/xxyyue/llm-observer-proxy-go/internal/observer"
	"strings"
	"testing"
)

func TestExportExtraDoesNotCopySourcePayloads(t *testing.T) {
	const sentinel = "RAW_ONLY_DISTINCTIVE_PAYLOAD_729438"
	first := event("one", `{"model":"m","metadata":{"private":"`+sentinel+`"},"tools":[{"name":"f","description":"`+sentinel+`","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"task"}]}`, `{"choices":[{"message":{"tool_calls":[{"id":"call","function":{"name":"f","arguments":"{}"}}]}}],"usage":{"prompt_tokens":5,"completion_tokens":2,"cache_creation_input_tokens":1,"provider_extension":{"raw":"`+sentinel+`"}},"provider_private":"`+sentinel+`"}`)
	body := `{"different_raw_body":"` + sentinel + `"}`
	first.RequestBody = &body
	second := event("two", `{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"call","content":"actual result","private":"`+sentinel+`"}]}]}`, `{"error":{"message":"`+sentinel+`"}}`)
	status := 503
	second.ResponseStatus = &status
	failure := sentinel
	second.Error = &failure
	doc := build(t, []observer.Event{first, second}, true)
	encoded, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), sentinel) {
		t.Fatal("raw-only sentinel escaped source log")
	}
	for _, key := range []string{`"input_contexts"`, `"raw_request_body"`, `"provider_result"`, `"provider_usage"`, `"context_configuration"`, `"unmapped"`} {
		if strings.Contains(string(encoded), key) {
			t.Fatal("raw payload container retained", key)
		}
	}
	if observedSteps(doc)[0].Observation.Results[0].Content != "actual result" {
		t.Fatal("standard tool result lost")
	}
	if doc.FinalMetrics["total_prompt_tokens"] != int64(5) || doc.FinalMetrics["total_completion_tokens"] != int64(2) {
		t.Fatal("normalized usage changed")
	}
	if !hasIssue(doc, "captured_body_differs") || !hasIssue(doc, "failed_exchange") {
		t.Fatal("raw removal suppressed evidence")
	}
	if _, ok := doc.Extra[Namespace].(map[string]any)["captured_input_fidelity"]; ok {
		t.Fatal("obsolete exact-input claim retained")
	}
	saveFixture(t, "compact-extra", doc)
}

func TestUnsupportedPayloadsStayDiagnosedWithoutRawFallback(t *testing.T) {
	const sentinel = "UNMAPPED_RAW_SENTINEL_89234"
	tests := []observer.Event{
		event("content", `{"messages":[{"role":"user","content":[{"type":"audio","data":"`+sentinel+`"}]}]}`, `{"choices":[{"message":{"content":"answer"}}]}`),
		event("argument", `{"messages":[{"role":"user","content":"task"}]}`, `{"choices":[{"message":{"tool_calls":[{"id":"bad","function":{"name":"f","arguments":"`+sentinel+`"}}]}}]}`),
		event("opaque", `{"input":"task"}`, `{"output":[{"type":"reasoning","encrypted_content":"`+sentinel+`","summary":[]},{"type":"message","content":[{"type":"output_text","text":"answer"}]}]}`),
		event("input-reasoning", `{"input":[{"type":"reasoning","encrypted_content":"`+sentinel+`"},{"role":"user","content":"task"}]}`, `{"choices":[{"message":{"content":"answer"}}]}`),
	}
	stream := event("sse", `{"messages":[{"role":"user","content":"task"}]}`, `{}`)
	stream.Stream = true
	stream.ResponseJSON = nil
	raw := "data: " + sentinel + "\n\ndata: [DONE]\n\n"
	stream.ResponseBody = &raw
	tests = append(tests, stream)
	for _, e := range tests {
		t.Run(e.ExchangeID, func(t *testing.T) {
			doc := build(t, []observer.Event{e}, false)
			data, _ := json.Marshal(doc)
			if strings.Contains(string(data), sentinel) {
				t.Fatal("unmapped payload embedded in export")
			}
			if len(semanticErrors(doc.Extra[Namespace].(map[string]any)["diagnostics"].([]Diagnostic))) == 0 {
				t.Fatal("unsupported mapping silently accepted")
			}
			if _, err := Build([]observer.Event{e}, Options{Strict: true}, nil); err == nil {
				t.Fatal("strict accepted unsupported mapping")
			}
		})
	}
}
