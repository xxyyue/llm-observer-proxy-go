// SPDX-License-Identifier: Apache-2.0
package trajectory

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func syntheticImage(t *testing.T, format string) []byte {
	t.Helper()
	picture := image.NewRGBA(image.Rect(0, 0, 2, 3))
	picture.Set(1, 1, color.RGBA{R: 255, A: 255})
	var out bytes.Buffer
	var err error
	switch format {
	case "png":
		err = png.Encode(&out, picture)
	case "jpeg":
		err = jpeg.Encode(&out, picture, nil)
	case "gif":
		err = gif.Encode(&out, picture, nil)
	case "webp":
		// Synthetic 2x3 solid red image encoded with cwebp -lossless.
		data, decodeErr := base64.StdEncoding.DecodeString("UklGRhwAAABXRUJQVlA4TA8AAAAvAYAAAAcQ/Y/+ByKi/wEA")
		if decodeErr != nil {
			t.Fatal(decodeErr)
		}
		return data
	default:
		t.Fatal("unknown synthetic format")
	}
	if err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}
func imageBlock(data []byte, mime string) map[string]any {
	return map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": mime, "data": base64.StdEncoding.EncodeToString(data)}}
}
func TestStandardMixedImages(t *testing.T) {
	data := syntheticImage(t, "png")
	b := builder{}
	input := []any{map[string]any{"type": "text", "text": " before\n"}, imageBlock(data, "image/png"), map[string]any{"type": "input_text", "text": "after "}, map[string]any{"type": "input_image", "image_url": "data:image/png;base64," + base64.StdEncoding.EncodeToString(data)}}
	content := b.standardContent(input)
	parts, ok := content.([]ContentPart)
	if !ok || len(parts) != 4 {
		t.Fatalf("unexpected parts: %#v", content)
	}
	if len(b.assets) != 1 || len(b.diagnostics) != 0 {
		t.Fatalf("assets/diagnostics: %d/%#v", len(b.assets), b.diagnostics)
	}
	if textOf(content) != " before\nafter " || !hasContent(content) {
		t.Fatal("text or content lost")
	}
	if !bytes.Equal(b.assets[parts[1].Source.Path].Data, data) || parts[1].Source.Path != parts[3].Source.Path {
		t.Fatal("bytes changed or dedup failed")
	}
	serialized, _ := json.Marshal(content)
	var roundTrip any
	json.Unmarshal(serialized, &roundTrip)
	if textOf(roundTrip) != textOf(content) || !hasContent(roundTrip) {
		t.Fatal("round-trip content helpers differ")
	}
	result := Result{CallID: "tool-1", Content: b.standardContent(input)}
	if textOf(result.Content) != textOf(content) {
		t.Fatal("tool image/text changed")
	}
}
func TestStandardTextIsExactAndUnsupportedDiagnosed(t *testing.T) {
	b := builder{}
	got := b.standardContent([]any{map[string]any{"type": "text", "text": "a"}, map[string]any{"type": "text", "text": " b"}})
	if textOf(got) != "a b" {
		t.Fatalf("invented separator: %#v", got)
	}
	if parts, ok := got.([]ContentPart); !ok || len(parts) != 2 {
		t.Fatal("text block boundaries lost")
	}
	for _, value := range []any{42, true, map[string]any{"bad": "object"}, []any{23, map[string]any{"type": "audio"}}} {
		b.standardContent(value)
	}
	if len(b.diagnostics) != 5 {
		t.Fatalf("unsupported values missing: %#v", b.diagnostics)
	}
}
func TestStandardImageFormatsAndFailures(t *testing.T) {
	for _, format := range []string{"png", "jpeg", "gif", "webp"} {
		t.Run(format, func(t *testing.T) {
			b := builder{}
			if !hasContent(b.standardContent([]any{imageBlock(syntheticImage(t, format), "image/"+format)})) || len(b.assets) != 1 {
				t.Fatalf("image lost: %#v", b.diagnostics)
			}
		})
	}
	tests := []map[string]any{imageBlock(syntheticImage(t, "png"), "image/jpeg"), imageBlock([]byte("broken"), "image/png"), imageBlock(syntheticImage(t, "png"), "image/svg+xml"), {"type": "input_image", "image_url": "file:///etc/passwd"}, {"type": "input_image", "image_url": "https://example.invalid/opaque"}, imageBlock(make([]byte, MaxImageBytes+1), "image/png"), {"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": "!!!"}}}
	for i, value := range tests {
		b := builder{}
		if hasContent(b.standardContent([]any{value})) || len(b.assets) != 0 || len(b.diagnostics) == 0 {
			t.Fatalf("invalid image %d not diagnosed: %#v", i, b)
		}
	}
	large := image.NewGray(image.Rect(0, 0, MaxImageDimension+1, 1))
	var out bytes.Buffer
	png.Encode(&out, large)
	b := builder{}
	b.standardContent([]any{imageBlock(out.Bytes(), "image/png")})
	if len(b.diagnostics) != 1 {
		t.Fatal("oversized dimension accepted")
	}
}
func TestRemoteImagesNeverFetchedAndMIMEIsExplicit(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer server.Close()
	tests := []struct {
		location, mime, override string
		valid, diagnostic        bool
	}{{server.URL + "/opaque", "image/png", "", true, false}, {server.URL + "/opaque", "", "image/webp", true, false}, {server.URL + "/photo.jpg", "", "", true, true}, {server.URL + "/opaque", "", "", false, true}, {server.URL + "/x.png", "image/svg+xml", "", false, true}}
	for _, test := range tests {
		b := builder{imageMediaType: test.override}
		got := b.standardContent([]any{map[string]any{"type": "image_url", "image_url": map[string]any{"url": test.location, "media_type": test.mime}}})
		if hasContent(got) != test.valid || (len(b.diagnostics) > 0) != test.diagnostic {
			t.Fatalf("bad URL handling: %#v %#v", test, b.diagnostics)
		}
	}
	if requests.Load() != 0 {
		t.Fatal("hidden image network access")
	}
	b := builder{}
	b.standardContent([]any{map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": server.URL + "/a.png"}}})
	if !strings.Contains(b.diagnostics[0].Code, "inferred") {
		t.Fatal("inference uncertainty missing")
	}
}
