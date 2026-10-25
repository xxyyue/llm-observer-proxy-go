// SPDX-License-Identifier: Apache-2.0
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestOfflineExportAndAtomicFailure(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "events.jsonl")
	output := filepath.Join(root, "trajectory.json")
	input := `{"exchange_id":"one","run_id":"run-1","path":"/v1/chat/completions","request_json":{"messages":[{"role":"user","content":"hello"}]},"response_json":{"choices":[{"message":{"content":"hi"}}]}}` + "\n"
	if err := os.WriteFile(source, []byte(input), 0o600); err != nil {
		t.Fatal(err)
	}
	// No model catalog or provider credential is needed.
	if err := runExport([]string{"--events", source, "--output", output, "--agent-name", "agent", "--agent-version", "1", "--strict"}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err = json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["schema_version"] != "ATIF-v1.7" || doc["agent"].(map[string]any)["name"] != "agent" {
		t.Fatal(string(data))
	}
	if err = runExport([]string{"--events", source, "--output", output, "--format-version", "1.8"}); err == nil {
		t.Fatal("unsupported version accepted")
	}
	after, _ := os.ReadFile(output)
	if string(after) != string(data) {
		t.Fatal("failed export replaced prior artifact")
	}
	if err = runExport([]string{"--events", source, "--output", source}); err == nil {
		t.Fatal("source overwrite allowed")
	}
	if err = runExport([]string{"--events", source}); err == nil {
		t.Fatal("missing output accepted")
	}
}
