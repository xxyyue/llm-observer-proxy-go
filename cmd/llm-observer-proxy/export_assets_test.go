// SPDX-License-Identifier: Apache-2.0
package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"github.com/xxyyue/llm-observer-proxy-go/internal/trajectory"
	"image"
	"image/color"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func cliImageFixture(t *testing.T) ([]byte, map[string][]byte) {
	t.Helper()
	images := map[string][]byte{}
	blocks := []any{}
	for _, shade := range []color.RGBA{{R: 255, A: 255}, {B: 255, A: 255}} {
		picture := image.NewRGBA(image.Rect(0, 0, 3, 2))
		picture.Set(1, 1, shade)
		var out bytes.Buffer
		if err := png.Encode(&out, picture); err != nil {
			t.Fatal(err)
		}
		data := out.Bytes()
		sum := sha256.Sum256(data)
		key := "images/" + hex.EncodeToString(sum[:]) + ".png"
		images[key] = data
		blocks = append(blocks, map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": base64.StdEncoding.EncodeToString(data)}})
	}
	user := map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "inspect user image"}, blocks[0]}}
	call := map[string]any{"type": "tool_use", "id": "inspect-1", "name": "inspect", "input": map[string]any{"region": "all"}}
	assistant := map[string]any{"role": "assistant", "content": []any{call}}
	result := map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "inspect-1", "content": []any{map[string]any{"type": "text", "text": "tool screenshot"}, blocks[1]}}}}
	events := []map[string]any{
		{"exchange_id": "images-1", "run_id": "run-images", "path": "/v1/messages", "request_json": map[string]any{"model": "test", "messages": []any{user}}, "response_json": map[string]any{"role": "assistant", "content": []any{call}}, "response_status": 200},
		{"exchange_id": "images-2", "run_id": "run-images", "path": "/v1/messages", "request_json": map[string]any{"model": "test", "messages": []any{user, assistant, result}}, "response_json": map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": "done"}}}, "response_status": 200},
	}
	var log bytes.Buffer
	encoder := json.NewEncoder(&log)
	for _, event := range events {
		if err := encoder.Encode(event); err != nil {
			t.Fatal(err)
		}
	}
	return log.Bytes(), images
}
func cliImageSources(content any) []string {
	var result []string
	parts, _ := content.([]any)
	for _, raw := range parts {
		part, _ := raw.(map[string]any)
		if part["type"] != "image" {
			continue
		}
		source, _ := part["source"].(map[string]any)
		location, _ := source["path"].(string)
		result = append(result, location)
	}
	return result
}
func saveCLIImageFixture(t *testing.T, name string, data []byte) {
	t.Helper()
	root := os.Getenv("ATIF_FIXTURE_DIR")
	if root == "" {
		return
	}
	destination := filepath.Join(root, "cli-images", filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destination, data, 0600); err != nil {
		t.Fatal(err)
	}
}
func cliAllImageSources(document trajectory.Document) []string {
	var result []string
	for _, step := range document.Steps {
		result = append(result, cliImageSources(step.Message)...)
		if step.Observation != nil {
			for _, observation := range step.Observation.Results {
				result = append(result, cliImageSources(observation.Content)...)
			}
		}
	}
	return result
}
func TestCLIImagePublicationJSONZIPAndSourceProtection(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "events.jsonl")
	output := filepath.Join(root, "trajectory.json")
	input, images := cliImageFixture(t)
	if err := os.WriteFile(source, input, 0600); err != nil {
		t.Fatal(err)
	}
	if err := runExport([]string{"--events", source, "--output", output, "--package", "json", "--strict"}); err != nil {
		t.Fatal(err)
	}
	encoded, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	var document trajectory.Document
	if err := json.Unmarshal(encoded, &document); err != nil {
		t.Fatal(err)
	}
	references := cliAllImageSources(document)
	seen := map[string]bool{}
	for _, location := range references {
		if !filepath.IsLocal(location) || strings.Contains(location, "\\") {
			t.Fatal("unsafe sidecar reference", location)
		}
		key := "images/" + filepath.Base(location)
		expected, ok := images[key]
		if !ok {
			t.Fatal("unknown image in JSON", key)
		}
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(location)))
		if err != nil || !bytes.Equal(data, expected) {
			t.Fatal("JSON sidecar bytes changed", location, err)
		}
		seen[key] = true
		saveCLIImageFixture(t, location, data)
	}
	if len(seen) != 2 {
		t.Fatal("user or tool image absent", seen)
	}
	userImage, toolImage := false, false
	for _, step := range document.Steps {
		if step.Source == "user" && len(cliImageSources(step.Message)) > 0 {
			userImage = true
		}
		if step.Observation != nil {
			for _, result := range step.Observation.Results {
				if result.CallID == "inspect-1" && len(cliImageSources(result.Content)) > 0 {
					toolImage = true
				}
			}
		}
	}
	if !userImage || !toolImage {
		t.Fatal("user/tool image relationship missing")
	}
	zipPath := filepath.Join(root, "trajectory.zip")
	if err := runExport([]string{"--events", source, "--output", zipPath, "--package", "zip", "--strict"}); err != nil {
		t.Fatal(err)
	}
	zipData, err := os.ReadFile(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	archive, err := zip.NewReader(bytes.NewReader(zipData), int64(len(zipData)))
	if err != nil {
		t.Fatal(err)
	}
	if len(archive.File) != 3 {
		t.Fatal("ZIP resource deduplication failed")
	}
	portable := trajectory.Document{}
	extracted := t.TempDir()
	for _, file := range archive.File {
		if !filepath.IsLocal(file.Name) || strings.Contains(file.Name, "\\") {
			t.Fatal("unsafe ZIP member", file.Name)
		}
		if file.Name != "trajectory.json" {
			if _, ok := images[file.Name]; !ok {
				t.Fatal("unrelated ZIP member", file.Name)
			}
		}
		reader, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(reader)
		reader.Close()
		if err != nil {
			t.Fatal(err)
		}
		destination := filepath.Join(extracted, filepath.FromSlash(file.Name))
		if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(destination, data, 0600); err != nil {
			t.Fatal(err)
		}
		if file.Name == "trajectory.json" {
			if err := json.Unmarshal(data, &portable); err != nil {
				t.Fatal(err)
			}
		} else if !bytes.Equal(data, images[file.Name]) {
			t.Fatal("ZIP resource bytes changed")
		}
	}
	for _, location := range cliAllImageSources(portable) {
		expected, ok := images[location]
		if !ok {
			t.Fatal("ZIP reference noncanonical", location)
		}
		actual, err := os.ReadFile(filepath.Join(extracted, location))
		if err != nil || !bytes.Equal(actual, expected) {
			t.Fatal("extracted ZIP reference cannot resolve")
		}
	}
	for _, packaging := range []string{"json", "zip"} {
		for _, name := range []string{"events.jsonl", "source-hardlink", "source-symlink"} {
			target := filepath.Join(root, name)
			if name == "source-hardlink" {
				if err := os.Link(source, target); err != nil {
					t.Fatal(err)
				}
			}
			if name == "source-symlink" {
				if err := os.Symlink(source, target); err != nil {
					t.Fatal(err)
				}
			}
			if err := runExport([]string{"--events", source, "--output", target, "--package", packaging}); err == nil {
				t.Fatal("source alias overwrite accepted", packaging, name)
			}
			if name != "events.jsonl" {
				os.Remove(target)
			}
		}
	}
	for _, args := range [][]string{{"--package", "tar"}, {"--image-media-type", "image/svg+xml"}, {"--format-version", "1.8"}} {
		command := append([]string{"--events", source, "--output", output}, args...)
		if err := runExport(command); err == nil {
			t.Fatal("invalid export options accepted", args)
		}
	}
	after, err := os.ReadFile(output)
	if err != nil || !bytes.Equal(after, encoded) {
		t.Fatal("failed export replaced previous JSON")
	}
	after, err = os.ReadFile(source)
	if err != nil || !bytes.Equal(after, input) {
		t.Fatal("source log modified")
	}
	saveCLIImageFixture(t, "events.jsonl", input)
	saveCLIImageFixture(t, "trajectory.json", encoded)
	saveCLIImageFixture(t, "trajectory.zip", zipData)
}
func TestCLIRemoteImageExplicitMediaType(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "events.jsonl")
	output := filepath.Join(root, "trajectory.json")
	input := `{"exchange_id":"opaque-image","path":"/v1/responses","request_json":{"input":[{"role":"user","content":[{"type":"input_image","image_url":"https://example.invalid/opaque-resource"}]}]},"response_json":{"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}]}}` + "\n"
	if err := os.WriteFile(source, []byte(input), 0600); err != nil {
		t.Fatal(err)
	}
	base := []string{"--events", source, "--output", output, "--strict"}
	if err := runExport(base); err == nil {
		t.Fatal("opaque image URL accepted without MIME")
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatal("failed strict export left a JSON file")
	}
	if err := runExport(append(base, "--image-media-type", "image/png")); err != nil {
		t.Fatal(err)
	}
	encoded, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	var document trajectory.Document
	if err := json.Unmarshal(encoded, &document); err != nil {
		t.Fatal(err)
	}
	references := cliAllImageSources(document)
	if len(references) == 0 {
		t.Fatal("remote image lost")
	}
	for _, location := range references {
		if location != "https://example.invalid/opaque-resource" {
			t.Fatal("remote URL changed")
		}
	}
	sidecars, _ := filepath.Glob(output + ".assets-*")
	if len(sidecars) != 0 {
		t.Fatal("remote image fetched into local sidecar")
	}
}
