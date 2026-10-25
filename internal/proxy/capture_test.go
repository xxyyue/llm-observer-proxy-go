// SPDX-License-Identifier: Apache-2.0
package proxy

import (
	"context"
	"encoding/json"
	"github.com/xxyyue/llm-observer-proxy-go/internal/observer"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type captureEngine struct{ t *testing.T }

func (e captureEngine) Forward(_ context.Context, r ForwardRequest) (*ForwardResponse, error) {
	for k := range r.Headers {
		if strings.HasPrefix(strings.ToLower(k), "x-llm-observer-") {
			e.t.Fatal("observer metadata leaked upstream")
		}
	}
	return &ForwardResponse{StatusCode: 200, Headers: map[string]string{"X-LLM-Observer-Exchange-ID": "upstream-forged"}, Body: []byte(`{"choices":[{"message":{"content":"ok"}}]}`)}, nil
}
func (e captureEngine) Stream(ctx context.Context, r ForwardRequest) (<-chan StreamChunk, error) {
	response, _ := e.Forward(ctx, r)
	ch := make(chan StreamChunk, 1)
	ch <- StreamChunk{StatusCode: 200, Headers: response.Headers, Body: []byte("data: [DONE]\n\n")}
	close(ch)
	return ch, nil
}
func (captureEngine) Close() {}
func TestCaptureMetadataAndExchangeIdentity(t *testing.T) {
	for _, stream := range []bool{false, true} {
		root := t.TempDir()
		path := filepath.Join(root, "events.jsonl")
		rec := observer.NewRecorder("run", 1, path, filepath.Join(root, "conversation.json"), nil)
		body := `{"model":"m","seed":9007199254740993,"messages":[{"role":"user","content":"hello"}],"stream":` + map[bool]string{false: "false", true: "true"}[stream] + `}`
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("X-LLM-Observer-Conversation-ID", "conversation")
		req.Header.Set("X-LLM-Observer-Agent-ID", "agent")
		req.Header.Set("X-LLM-Observer-Parent-Exchange-ID", "parent")
		req.Header.Set("X-LLM-Observer-Context-Operation", "compaction")
		writer := httptest.NewRecorder()
		NewDataPlane("run", "http://unused", captureEngine{t}, rec).ServeHTTP(writer, req)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var event observer.Event
		decoder := json.NewDecoder(strings.NewReader(string(data)))
		decoder.UseNumber()
		if err = decoder.Decode(&event); err != nil {
			t.Fatal(err)
		}
		id := writer.Header().Get("X-LLM-Observer-Exchange-ID")
		if id == "" || id == "upstream-forged" || id != event.ExchangeID {
			t.Fatal("exchange ID not stable or overridden")
		}
		if event.ContextMetadata == nil || event.ContextMetadata.ParentExchangeID != "parent" || event.ContextMetadata.Operation != "compaction" {
			t.Fatal("context metadata not captured")
		}
		if event.RequestJSON.(map[string]any)["seed"].(json.Number).String() != "9007199254740993" {
			t.Fatal("captured parameter rounded")
		}
	}
}

func TestMetadataValidationAndMalformedJSON(t *testing.T) {
	for _, operation := range []string{"made-up", strings.Repeat("x", 257)} {
		r := httptest.NewRequest("POST", "/", nil)
		r.Header.Set("X-LLM-Observer-Context-Operation", operation)
		if _, err := captureContextMetadata(r.Header); err == nil {
			t.Fatal("invalid metadata accepted")
		}
	}
	var value any
	for _, raw := range []string{`{} {}`, `{} trailing`} {
		if decodeRecordedJSON([]byte(raw), &value) == nil {
			t.Fatal("trailing JSON accepted")
		}
	}
}
