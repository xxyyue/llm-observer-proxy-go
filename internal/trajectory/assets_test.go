// SPDX-License-Identifier: Apache-2.0
package trajectory

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func assetDocument(t *testing.T) (*Document, string, []byte) {
	t.Helper()
	data := syntheticImage(t, "png")
	key := assetKey(data, "image/png")
	content := []ContentPart{{Type: "image", Source: &ImageSource{MediaType: "image/png", Path: key}}}
	return &Document{SchemaVersion: "ATIF-v1.7", Assets: map[string]Asset{key: {Data: data, MediaType: "image/png"}}, Steps: []Step{{ID: 1, Source: "user", Message: content, Observation: &Observation{Results: []Result{{CallID: "1", Content: content}}}}}, Extra: map[string]any{"input_context": content, "exact_number": json.Number("9007199254740993")}}, key, data
}
func realTempDir(t *testing.T) string {
	t.Helper()
	path, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return path
}
func TestAssetPersistenceAndSafeRead(t *testing.T) {
	document, key, data := assetDocument(t)
	root := t.TempDir()
	if err := WriteAssets(root, document.Assets); err != nil {
		t.Fatal(err)
	}
	if err := WriteAssets(root, document.Assets); err != nil {
		t.Fatal("cannot reuse exact resource", err)
	}
	asset, err := ReadAsset(root, key)
	if err != nil || !bytes.Equal(data, asset.Data) {
		t.Fatalf("read: %v", err)
	}
	info, _ := os.Stat(filepath.Join(root, key))
	if info.Mode().Perm() != 0600 {
		t.Fatal("unsafe image permissions")
	}
	for _, bad := range []string{"../secret", "images/../../secret", strings.ReplaceAll(key, "/", "\\"), key + "/extra", "images/secret.png"} {
		if _, err := ReadAsset(root, bad); err == nil {
			t.Fatal("unsafe key accepted", bad)
		}
	}
	os.WriteFile(filepath.Join(root, key), []byte("unrelated data"), 0600)
	if _, err := ReadAsset(root, key); err == nil {
		t.Fatal("hash mismatch accepted")
	}
	if err := WriteAssets(root, document.Assets); err == nil {
		t.Fatal("different existing data overwritten")
	}
}
func TestAssetSymlinksRejected(t *testing.T) {
	document, key, _ := assetDocument(t)
	root := realTempDir(t)
	outside := realTempDir(t)
	if err := os.Symlink(outside, filepath.Join(root, "images")); err != nil {
		t.Fatal(err)
	}
	if err := WriteAssets(root, document.Assets); err == nil {
		t.Fatal("image directory symlink accepted")
	}
	os.Remove(filepath.Join(root, "images"))
	os.Mkdir(filepath.Join(root, "images"), 0700)
	target := filepath.Join(outside, "secret")
	os.WriteFile(target, document.Assets[key].Data, 0600)
	os.Symlink(target, filepath.Join(root, key))
	if _, err := ReadAsset(root, key); err == nil {
		t.Fatal("file symlink read")
	}
	if err := WriteAssets(root, document.Assets); err == nil {
		t.Fatal("file symlink reused")
	}
	data, _ := os.ReadFile(target)
	if !bytes.Equal(data, document.Assets[key].Data) {
		t.Fatal("symlink target modified")
	}
	linkRoot := filepath.Join(root, "alias")
	os.Symlink(outside, linkRoot)
	if err := WriteAssets(linkRoot, document.Assets); err == nil {
		t.Fatal("root symlink accepted")
	}
}
func TestJSONPublishesPortableResourcesWithoutChangingRawContext(t *testing.T) {
	document, key, data := assetDocument(t)
	root := realTempDir(t)
	source := filepath.Join(root, "events.jsonl")
	os.WriteFile(source, []byte("original"), 0600)
	output := filepath.Join(root, "export.json")
	if err := WriteFile(output, source, document); err != nil {
		t.Fatal(err)
	}
	encoded, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	var loaded Document
	if err := decodeJSON(encoded, &loaded); err != nil {
		t.Fatal(err)
	}
	if loaded.Extra["exact_number"] != json.Number("9007199254740993") {
		t.Fatal("raw context number changed")
	}
	parts := loaded.Steps[0].Message.([]any)
	location := object(object(parts[0])["source"])["path"].(string)
	stored, err := os.ReadFile(filepath.Join(root, location))
	if err != nil || !bytes.Equal(stored, data) {
		t.Fatalf("portable resource: %v", err)
	}
	if filepath.IsAbs(location) || !strings.HasSuffix(location, key) {
		t.Fatal("non-portable resource")
	}
	if document.Steps[0].Message.([]ContentPart)[0].Source.Path != key {
		t.Fatal("input mutated")
	}
	original := loaded.Extra["input_context"].([]any)
	if object(object(original[0])["source"])["path"] != key {
		t.Fatal("raw context rewritten")
	}
	result := loaded.Steps[0].Observation.Results[0].Content.([]any)
	if object(object(result[0])["source"])["path"] != location {
		t.Fatal("observation image missing rewrite")
	}
	prefix := "https://example.invalid/trajectory/assets"
	absolute, err := document.WithAssetPaths(prefix)
	if err != nil || absolute.Steps[0].Message.([]ContentPart)[0].Source.Path != prefix+"/"+key {
		t.Fatalf("HTTP rewrite: %v", err)
	}
	// Also support a JSON round trip followed by restoring the out-of-band assets.
	loaded.Assets = document.Assets
	loaded.Steps[0].Message = []any{map[string]any{"type": "image", "source": map[string]any{"media_type": "image/png", "path": key}}}
	rewritten, err := loaded.WithAssetPaths(prefix)
	if err != nil || object(object(rewritten.Steps[0].Message.([]any)[0])["source"])["path"] != prefix+"/"+key {
		t.Fatal("round trip rewrite failed")
	}
}

type failingAssetWriter struct{}

func (failingAssetWriter) Write([]byte) (int, error) { return 0, errors.New("writer failure") }
func TestZIPContainsExactBytesAndSafePaths(t *testing.T) {
	document, key, data := assetDocument(t)
	var out bytes.Buffer
	if err := WriteZIP(&out, document); err != nil {
		t.Fatal(err)
	}
	archive, err := zip.NewReader(bytes.NewReader(out.Bytes()), int64(out.Len()))
	if err != nil {
		t.Fatal(err)
	}
	if len(archive.File) != 2 {
		t.Fatal("unexpected ZIP entries")
	}
	root := realTempDir(t)
	for _, file := range archive.File {
		if file.Name != "trajectory.json" && file.Name != key {
			t.Fatal("unsafe ZIP path", file.Name)
		}
		reader, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		contents, err := io.ReadAll(reader)
		reader.Close()
		if err != nil {
			t.Fatal(err)
		}
		destination := filepath.Join(root, filepath.FromSlash(file.Name))
		os.MkdirAll(filepath.Dir(destination), 0700)
		if err := os.WriteFile(destination, contents, 0600); err != nil {
			t.Fatal(err)
		}
		if file.Name == key && !bytes.Equal(contents, data) {
			t.Fatal("ZIP image bytes changed")
		}
	}
	var loaded Document
	contents, _ := os.ReadFile(filepath.Join(root, "trajectory.json"))
	json.Unmarshal(contents, &loaded)
	parts := loaded.Steps[0].Message.([]any)
	if object(object(parts[0])["source"])["path"] != key {
		t.Fatal("ZIP reference not canonical")
	}
	if err := WriteZIP(failingAssetWriter{}, document); err == nil {
		t.Fatal("writer error swallowed")
	}
}
func TestPublicationFailurePreservesExistingAndSource(t *testing.T) {
	document, _, _ := assetDocument(t)
	root := realTempDir(t)
	source := filepath.Join(root, "events.jsonl")
	os.WriteFile(source, []byte("original"), 0600)
	for _, write := range []func(string, string, *Document) error{WriteFile, WriteZIPFile} {
		for _, target := range []string{source, filepath.Join(root, "hardlink"), filepath.Join(root, "symlink")} {
			if strings.HasSuffix(target, "hardlink") {
				os.Link(source, target)
			}
			if strings.HasSuffix(target, "symlink") {
				os.Symlink(source, target)
			}
			if err := write(target, source, document); err == nil {
				t.Fatal("source alias overwritten")
			}
			os.Remove(filepath.Join(root, "hardlink"))
			os.Remove(filepath.Join(root, "symlink"))
		}
		output := filepath.Join(root, "export")
		os.WriteFile(output, []byte("previous export"), 0600)
		bad := *document
		bad.Extra = map[string]any{"unserializable": make(chan int)}
		if err := write(output, source, &bad); err == nil {
			t.Fatal("serialization failure ignored")
		}
		existing, _ := os.ReadFile(output)
		if string(existing) != "previous export" {
			t.Fatal("previous export changed")
		}
		// Rename over a directory must fail, cleaning only the new resource directory.
		blocked := filepath.Join(root, "directory")
		os.Mkdir(blocked, 0700)
		if err := write(blocked, source, document); err == nil {
			t.Fatal("directory target accepted")
		}
		leftovers, _ := filepath.Glob(blocked + ".assets-*")
		if len(leftovers) != 0 {
			t.Fatal("resource rollback leaked", leftovers)
		}
	}
	original, _ := os.ReadFile(source)
	if string(original) != "original" {
		t.Fatal("source changed")
	}
}
