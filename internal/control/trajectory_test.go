// SPDX-License-Identifier: Apache-2.0
package control_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/xxyyue/llm-observer-proxy-go/internal/catalog"
	"github.com/xxyyue/llm-observer-proxy-go/internal/control"
	"github.com/xxyyue/llm-observer-proxy-go/internal/proxy"
	"github.com/xxyyue/llm-observer-proxy-go/internal/trajectory"
)

type unusedEngine struct{}

func (unusedEngine) Forward(context.Context, proxy.ForwardRequest) (*proxy.ForwardResponse, error) {
	panic("export must not call upstream")
}
func (unusedEngine) Stream(context.Context, proxy.ForwardRequest) (<-chan proxy.StreamChunk, error) {
	panic("export must not call upstream")
}
func (unusedEngine) Close() {}

func TestTrajectoryHTTPAndRestart(t *testing.T) {
	root := t.TempDir()
	modelCatalog := &catalog.Catalog{Path: "test", Settings: catalog.ProxySettings{BaseURL: "http://unused.invalid"}}
	manager, err := proxy.NewManager(root, modelCatalog, unusedEngine{})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	server := httptest.NewServer(control.NewServer(manager))
	defer server.Close()
	created := requestJSON(t, http.MethodPost, server.URL+"/api/runs/run-1/proxy", map[string]any{"trajectory_metadata": map[string]any{"agent_name": "test-agent", "agent_version": "2.0"}})
	if created.status != 201 {
		t.Fatal(created.raw)
	}
	path := created.body["history_path"].(string)
	source := `{"exchange_id":"one","run_id":"run-1","path":"/v1/chat/completions","request_json":{"messages":[{"role":"user","content":"hello"}]},"response_json":{"choices":[{"message":{"content":"hi"}}]},"response_status":200}` + "\n"
	if err = os.WriteFile(path, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	response, err := http.Get(server.URL + "/api/runs/run-1/trajectory?format=atif&version=1.7&strict=true")
	if err != nil {
		t.Fatal(err)
	}
	var document trajectory.Document
	if err = json.NewDecoder(response.Body).Decode(&document); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 200 || response.Header.Get("Content-Disposition") == "" || document.SchemaVersion != "ATIF-v1.7" || document.Agent.Name != "test-agent" || len(document.Steps) != 2 {
		t.Fatalf("unexpected export: %#v", document)
	}
	if !document.Extra[trajectory.Namespace].(map[string]any)["snapshot"].(bool) {
		t.Fatal("running export not a snapshot")
	}
	if doc := requestJSON(t, http.MethodGet, server.URL+"/api/runs/run-1/history", nil); doc.status != 200 || doc.body["count"] != float64(1) {
		t.Fatal("history API regressed", doc.raw)
	}
	for _, query := range []string{"format=other", "version=1.8", "strict=yes"} {
		if result := requestJSON(t, http.MethodGet, server.URL+"/api/runs/run-1/trajectory?"+query, nil); result.status != 400 {
			t.Fatal("bad option accepted", query, result.raw)
		}
	}
	if result := requestJSON(t, http.MethodGet, server.URL+"/api/runs/missing/trajectory", nil); result.status != 404 {
		t.Fatal(result.raw)
	}
	if !manager.Stop("run-1") {
		t.Fatal("stop failed")
	}
	restarted, err := proxy.NewManager(root, modelCatalog, unusedEngine{})
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	second := httptest.NewServer(control.NewServer(restarted))
	defer second.Close()
	result := requestJSON(t, http.MethodGet, second.URL+"/api/runs/run-1/trajectory", nil)
	if result.status != 200 || result.body["agent"].(map[string]any)["version"] != "2.0" {
		t.Fatal("metadata lost after restart", result.raw)
	}
	// Legacy run metadata is allowed, and unknown identity is explicit.
	if err = os.WriteFile(filepath.Join(filepath.Dir(path), "config.json"), []byte(`{"backend":"bifrost"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	result = requestJSON(t, http.MethodGet, second.URL+"/api/runs/run-1/trajectory", nil)
	if result.status != 200 || result.body["agent"].(map[string]any)["name"] != "unknown" {
		t.Fatal("legacy run incompatible", result.raw)
	}
	unchanged, _ := os.ReadFile(path)
	if string(unchanged) != source {
		t.Fatal("export altered internal event log")
	}
	if err = os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if result = requestJSON(t, http.MethodGet, second.URL+"/api/runs/run-1/trajectory", nil); result.status != 422 {
		t.Fatal("empty export accepted", result.raw)
	}
	if err = os.WriteFile(path, []byte(source+"unfinished"), 0o600); err != nil {
		t.Fatal(err)
	}
	if result = requestJSON(t, http.MethodGet, second.URL+"/api/runs/run-1/trajectory?strict=true", nil); result.status != 422 {
		t.Fatal("strict incomplete export accepted", result.raw)
	}
}
