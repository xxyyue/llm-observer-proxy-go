// SPDX-License-Identifier: Apache-2.0

package control_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/xxyyue/llm-observer-proxy-go/internal/catalog"
	"github.com/xxyyue/llm-observer-proxy-go/internal/control"
	"github.com/xxyyue/llm-observer-proxy-go/internal/proxy"
)

func TestNativeBifrostControlAndDataPlaneCompatibility(t *testing.T) {
	var mu sync.Mutex
	var anthropicRequest map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		var payload map[string]any
		_ = json.Unmarshal(body, &payload)
		if request.Header.Get("Authorization") != "Bearer test-key" && request.Header.Get("x-api-key") != "test-key" {
			t.Errorf("upstream authorization was not set: %#v", request.Header)
		}
		switch request.URL.Path {
		case "/v1/chat/completions":
			writeUpstreamJSON(writer, map[string]any{
				"id": "chat-1", "choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "openai-ok"}, "finish_reason": "stop"}},
				"usage": map[string]any{"prompt_tokens": 2, "completion_tokens": 1, "total_tokens": 3},
			})
		case "/v1/messages":
			mu.Lock()
			anthropicRequest = payload
			mu.Unlock()
			writer.Header().Set("Content-Type", "text/event-stream")
			writer.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(writer, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":7}}}\n\n")
			_, _ = io.WriteString(writer, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"anthropic-ok\"}}\n\n")
			_, _ = io.WriteString(writer, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":4}}\n\n")
		default:
			http.NotFound(writer, request)
		}
	}))
	defer upstream.Close()

	temporary := t.TempDir()
	catalogPath := filepath.Join(temporary, "models.json")
	catalogJSON := map[string]any{
		"llm_observer_proxy": map[string]any{"base_url": upstream.URL, "api_key": "test-key"},
		"models": []any{
			map[string]any{"id": "gpt-5.5", "upstream_api": "openai"},
			map[string]any{"id": "claude-test", "upstream_api": "anthropic"},
		},
	}
	encodedCatalog, _ := json.Marshal(catalogJSON)
	if err := os.WriteFile(catalogPath, encodedCatalog, 0o644); err != nil {
		t.Fatal(err)
	}
	modelCatalog, err := catalog.Load(catalogPath)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := proxy.NewNativeEngine(context.Background(), modelCatalog, "error")
	if err != nil {
		t.Fatal(err)
	}
	manager, err := proxy.NewManager(filepath.Join(temporary, "artifacts"), modelCatalog, engine)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	controlPlane := httptest.NewServer(control.NewServer(manager))
	defer controlPlane.Close()

	create := requestJSON(t, http.MethodPost, controlPlane.URL+"/api/runs/run-001/proxy", map[string]any{
		"model": "ignored-model", "mode": "openai", "backend": "litellm",
	})
	if create.status != http.StatusCreated {
		t.Fatalf("create failed: status=%d body=%s", create.status, create.raw)
	}
	baseURL := create.body["base_url"].(string)
	if create.body["openai_base_url"] != baseURL+"/v1" || create.body["anthropic_base_url"] != baseURL {
		t.Fatalf("unexpected data plane URLs: %#v", create.body)
	}
	root := requestJSON(t, http.MethodGet, baseURL+"/", nil)
	if root.status != http.StatusOK || root.body["ok"] != true {
		t.Fatalf("data-plane root health check failed: status=%d body=%s", root.status, root.raw)
	}

	openAI := requestJSON(t, http.MethodPost, baseURL+"/v1/chat/completions", map[string]any{
		"model": "gpt-5.5", "messages": []any{map[string]any{"role": "user", "content": "hello"}},
	})
	if openAI.status != http.StatusOK || openAI.body["id"] != "chat-1" {
		t.Fatalf("OpenAI request failed: status=%d body=%s", openAI.status, openAI.raw)
	}

	system := []any{map[string]any{"type": "text", "text": "keep this system prompt", "cache_control": map[string]any{"type": "ephemeral"}}}
	anthropic := requestJSON(t, http.MethodPost, baseURL+"/v1/messages", map[string]any{
		"model": "claude-test", "max_tokens": 32, "stream": true, "system": system,
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	})
	if anthropic.status != http.StatusOK || !bytes.Contains([]byte(anthropic.raw), []byte("anthropic-ok")) {
		t.Fatalf("Anthropic stream failed: status=%d body=%s", anthropic.status, anthropic.raw)
	}
	mu.Lock()
	seenSystem := anthropicRequest["system"]
	mu.Unlock()
	if !jsonEqual(seenSystem, system) {
		t.Fatalf("Anthropic system array changed upstream: got=%#v want=%#v", seenSystem, system)
	}

	history := requestJSON(t, http.MethodGet, controlPlane.URL+"/api/runs/run-001/history", nil)
	if history.status != http.StatusOK || int(history.body["count"].(float64)) != 2 {
		t.Fatalf("unexpected history: status=%d body=%s", history.status, history.raw)
	}
	items := history.body["items"].([]any)
	recordedRequest := items[1].(map[string]any)["request_json"].(map[string]any)
	if !jsonEqual(recordedRequest["system"], system) {
		t.Fatalf("system array missing from trace: %#v", recordedRequest)
	}

	conversation := requestJSON(t, http.MethodGet, controlPlane.URL+"/api/runs/run-001/conversation", nil)
	messages := conversation.body["messages"].([]any)
	if messages[0].(map[string]any)["content"] != "keep this system prompt" {
		t.Fatalf("system prompt missing from conversation: %s", conversation.raw)
	}
	if messages[len(messages)-1].(map[string]any)["content"] != "anthropic-ok" {
		t.Fatalf("stream response missing from conversation: %s", conversation.raw)
	}

	beforeExport, err := os.ReadFile(create.body["history_path"].(string))
	if err != nil {
		t.Fatal(err)
	}
	exported := requestJSON(t, http.MethodGet, controlPlane.URL+"/api/runs/run-001/trajectory?format=atif", nil)
	if exported.status != http.StatusOK || exported.body["schema_version"] != "ATIF-v1.7" {
		t.Fatal("recorded export failed", exported.raw)
	}
	var observed []map[string]any
	for _, raw := range exported.body["steps"].([]any) {
		step := raw.(map[string]any)
		if step["source"] == "agent" {
			observed = append(observed, step)
		}
	}
	if len(observed) != 2 || observed[0]["message"] != "openai-ok" || observed[1]["message"] != "anthropic-ok" || observed[1]["model_name"] != "claude-test" {
		t.Fatal("recorded responses changed", exported.raw)
	}
	totals := exported.body["final_metrics"].(map[string]any)
	if totals["total_prompt_tokens"] != float64(9) || totals["total_completion_tokens"] != float64(5) {
		t.Fatal("recorded usage changed", exported.raw)
	}
	afterExport, err := os.ReadFile(create.body["history_path"].(string))
	if err != nil || !bytes.Equal(beforeExport, afterExport) {
		t.Fatal("export changed recorder event log")
	}

	deleted := requestJSON(t, http.MethodDelete, controlPlane.URL+"/api/runs/run-001/proxy", nil)
	if deleted.status != http.StatusOK {
		t.Fatalf("delete failed: %s", deleted.raw)
	}
	archived := requestJSON(t, http.MethodGet, controlPlane.URL+"/api/runs/run-001/history?limit=1", nil)
	if archived.status != http.StatusOK || int(archived.body["count"].(float64)) != 1 {
		t.Fatalf("archived history unavailable: %s", archived.raw)
	}
}

type response struct {
	status int
	body   map[string]any
	raw    string
}

func requestJSON(t *testing.T, method, url string, payload any) response {
	t.Helper()
	var body io.Reader
	if payload != nil {
		encoded, _ := json.Marshal(payload)
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequest(method, url, body)
	if err != nil {
		t.Fatal(err)
	}
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	result, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer result.Body.Close()
	raw, _ := io.ReadAll(result.Body)
	decoded := map[string]any{}
	_ = json.Unmarshal(raw, &decoded)
	return response{status: result.StatusCode, body: decoded, raw: string(raw)}
}

func writeUpstreamJSON(writer http.ResponseWriter, payload any) {
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(payload)
}

func jsonEqual(left, right any) bool {
	leftJSON, _ := json.Marshal(left)
	rightJSON, _ := json.Marshal(right)
	return bytes.Equal(leftJSON, rightJSON)
}
