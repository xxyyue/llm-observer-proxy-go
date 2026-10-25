// SPDX-License-Identifier: Apache-2.0
package control_test

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/xxyyue/llm-observer-proxy-go/internal/catalog"
	"github.com/xxyyue/llm-observer-proxy-go/internal/control"
	"github.com/xxyyue/llm-observer-proxy-go/internal/proxy"
	"github.com/xxyyue/llm-observer-proxy-go/internal/trajectory"
)

func httpImageFixture(t *testing.T) ([]byte, map[string][]byte) {
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
func httpImageSources(content any) []string {
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
func httpRead(t *testing.T, url string) (int, http.Header, []byte) {
	t.Helper()
	response, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, response.Header, data
}
func saveHTTPImageFixture(t *testing.T, name string, data []byte) {
	t.Helper()
	root := os.Getenv("ATIF_FIXTURE_DIR")
	if root == "" {
		return
	}
	destination := filepath.Join(root, "http-images", filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destination, data, 0600); err != nil {
		t.Fatal(err)
	}
}
func TestTrajectoryHTTPImagesAndPortableZIP(t *testing.T) {
	root := t.TempDir()
	modelCatalog := &catalog.Catalog{Path: "test", Settings: catalog.ProxySettings{BaseURL: "http://unused.invalid"}}
	manager, err := proxy.NewManager(root, modelCatalog, unusedEngine{})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	server := httptest.NewServer(control.NewServer(manager))
	defer server.Close()
	created := requestJSON(t, http.MethodPost, server.URL+"/api/runs/run-images/proxy", map[string]any{})
	if created.status != 201 {
		t.Fatal(created.raw)
	}
	unrelated := requestJSON(t, http.MethodPost, server.URL+"/api/runs/unrelated/proxy", map[string]any{})
	if unrelated.status != 201 {
		t.Fatal(unrelated.raw)
	}
	source, images := httpImageFixture(t)
	history := created.body["history_path"].(string)
	if err := os.WriteFile(history, source, 0600); err != nil {
		t.Fatal(err)
	}
	status, headers, encoded := httpRead(t, server.URL+"/api/runs/run-images/trajectory?package=json&strict=true")
	if status != 200 || !strings.HasPrefix(headers.Get("Content-Type"), "application/json") {
		t.Fatalf("JSON export %d: %s", status, encoded)
	}
	var document trajectory.Document
	if err := json.Unmarshal(encoded, &document); err != nil {
		t.Fatal(err)
	}
	prefix := server.URL + "/api/runs/run-images/trajectory/assets/"
	references := map[string]bool{}
	userImage, toolImage := false, false
	for _, step := range document.Steps {
		for _, location := range httpImageSources(step.Message) {
			references[location] = true
			if step.Source == "user" {
				userImage = true
			}
		}
		if step.Observation != nil {
			for _, result := range step.Observation.Results {
				for _, location := range httpImageSources(result.Content) {
					references[location] = true
					if result.CallID == "inspect-1" {
						toolImage = true
					}
				}
			}
		}
	}
	if !userImage || !toolImage || len(references) != 2 {
		t.Fatalf("user/tool image missing: user=%t tool=%t refs=%v", userImage, toolImage, references)
	}
	for location := range references {
		if !strings.HasPrefix(location, prefix) {
			t.Fatal("non-run-scoped resource", location)
		}
		key := strings.TrimPrefix(location, prefix)
		expected, ok := images[key]
		if !ok {
			t.Fatal("unknown resource hash", key)
		}
		status, headers, actual := httpRead(t, location)
		if status != 200 || !bytes.Equal(actual, expected) || headers.Get("Content-Type") != "image/png" || headers.Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("resource GET failed: %d %v", status, headers)
		}
		saveHTTPImageFixture(t, key, actual)
		for _, bad := range []string{strings.Replace(location, "run-images", "unrelated", 1), strings.Replace(location, "run-images", "missing", 1), prefix + "images/../config.json", prefix + "images/%2e%2e%2fconfig.json", prefix + "../config.json", prefix + "images/unknown.png", prefix + "images/%2fetc%2fpasswd"} {
			status, _, _ := httpRead(t, bad)
			if status >= 200 && status < 300 {
				t.Fatal("unsafe or unrelated resource served", bad)
			}
		}
	}
	status, headers, bundle := httpRead(t, server.URL+"/api/runs/run-images/trajectory?package=zip&strict=true")
	if status != 200 || headers.Get("Content-Type") != "application/zip" {
		t.Fatalf("ZIP export %d: %s", status, bundle)
	}
	archive, err := zip.NewReader(bytes.NewReader(bundle), int64(len(bundle)))
	if err != nil {
		t.Fatal(err)
	}
	if len(archive.File) != len(images)+1 {
		t.Fatal("unexpected ZIP entries")
	}
	extracted := t.TempDir()
	var portable trajectory.Document
	for _, file := range archive.File {
		if !filepath.IsLocal(file.Name) || strings.Contains(file.Name, "\\") {
			t.Fatal("unsafe ZIP entry", file.Name)
		}
		if file.Name != "trajectory.json" {
			if _, ok := images[file.Name]; !ok {
				t.Fatal("unrelated ZIP entry", file.Name)
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
			t.Fatal("ZIP bytes changed")
		}
	}
	for _, step := range portable.Steps {
		contents := []any{step.Message}
		if step.Observation != nil {
			for _, result := range step.Observation.Results {
				contents = append(contents, result.Content)
			}
		}
		for _, content := range contents {
			for _, location := range httpImageSources(content) {
				if _, ok := images[location]; !ok {
					t.Fatal("ZIP reference is not canonical", location)
				}
				data, err := os.ReadFile(filepath.Join(extracted, location))
				if err != nil || !bytes.Equal(data, images[location]) {
					t.Fatal("extracted resource missing")
				}
			}
		}
	}
	for _, query := range []string{"package=tar", "image_media_type=text/plain"} {
		status, _, _ := httpRead(t, server.URL+"/api/runs/run-images/trajectory?"+query)
		if status != 400 {
			t.Fatalf("invalid option %s returned %d", query, status)
		}
	}
	after, err := os.ReadFile(history)
	if err != nil || !bytes.Equal(after, source) {
		t.Fatal("export mutated source history")
	}
	saveHTTPImageFixture(t, "events.jsonl", source)
	saveHTTPImageFixture(t, "trajectory-http.json", encoded)
	saveHTTPImageFixture(t, "trajectory.zip", bundle)
}
func TestTrajectoryHTTPRemoteImageMIMEAndNoFetch(t *testing.T) {
	var fetched atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		fetched.Add(1)
		http.Error(writer, "must not fetch", 500)
	}))
	defer remote.Close()
	manager, err := proxy.NewManager(t.TempDir(), &catalog.Catalog{Path: "test", Settings: catalog.ProxySettings{BaseURL: "http://unused.invalid"}}, unusedEngine{})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	server := httptest.NewServer(control.NewServer(manager))
	defer server.Close()
	created := requestJSON(t, http.MethodPost, server.URL+"/api/runs/remote-images/proxy", map[string]any{})
	if created.status != 201 {
		t.Fatal(created.raw)
	}
	location := remote.URL + "/opaque"
	event := map[string]any{"exchange_id": "opaque", "run_id": "remote-images", "path": "/v1/chat/completions", "request_json": map[string]any{"messages": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "image_url", "image_url": map[string]any{"url": location}}}}}}, "response_json": map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "done"}}}}, "response_status": 200}
	source, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	source = append(source, '\n')
	if err := os.WriteFile(created.body["history_path"].(string), source, 0600); err != nil {
		t.Fatal(err)
	}
	endpoint := server.URL + "/api/runs/remote-images/trajectory?strict=true"
	if status, _, _ := httpRead(t, endpoint); status != 422 {
		t.Fatal("unknown image MIME accepted in strict mode", status)
	}
	status, _, data := httpRead(t, endpoint+"&image_media_type=image%2Fpng")
	if status != 200 {
		t.Fatalf("explicit media type rejected: %d %s", status, data)
	}
	var document trajectory.Document
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, step := range document.Steps {
		for _, path := range httpImageSources(step.Message) {
			count++
			if path != location {
				t.Fatal("remote image URL changed")
			}
		}
	}
	if count == 0 {
		t.Fatal("remote image lost")
	}
	if fetched.Load() != 0 {
		t.Fatal("export fetched remote image")
	}
}
