// SPDX-License-Identifier: Apache-2.0
package trajectory

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"net/url"
	"path"
	"strings"

	_ "golang.org/x/image/webp"
)

const MaxImageBytes = 16 << 20
const MaxAssetBytes = 256 << 20
const MaxImagePixels = 16_000_000
const MaxImageDimension = 16384

var mediaExtensions = map[string]string{"image/png": "png", "image/jpeg": "jpg", "image/gif": "gif", "image/webp": "webp"}

// validateImage bounds allocation before fully decoding to reject truncated or
// corrupt images. Original encoded bytes, including animation, remain untouched.
func validateImage(data []byte, claimed string) (string, error) {
	if len(data) == 0 || len(data) > MaxImageBytes {
		return "", fmt.Errorf("image exceeds size limit or is empty")
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("invalid image header")
	}
	mime := "image/" + format
	if mediaExtensions[mime] == "" || claimed != mime {
		return "", fmt.Errorf("unsupported or mismatched image media type")
	}
	if config.Width <= 0 || config.Height <= 0 || config.Width > MaxImageDimension || config.Height > MaxImageDimension || int64(config.Width)*int64(config.Height) > MaxImagePixels {
		return "", fmt.Errorf("image dimensions exceed limit")
	}
	if _, _, err := image.Decode(bytes.NewReader(data)); err != nil {
		return "", fmt.Errorf("invalid image data")
	}
	return mime, nil
}

func assetKey(data []byte, mime string) string {
	sum := sha256.Sum256(data)
	return "images/" + hex.EncodeToString(sum[:]) + "." + mediaExtensions[mime]
}

func (b *builder) standardContent(v any) any {
	if text, ok := v.(string); ok {
		return text
	}
	if v == nil {
		return ""
	}
	items, ok := v.([]any)
	if !ok {
		b.unmapped("unsupported_content", "Unexpected content retained in export metadata.", v)
		return ""
	}
	parts := []ContentPart{}
	hasImage := false
	for _, raw := range items {
		part, ok := raw.(map[string]any)
		if !ok {
			b.unmapped("unsupported_content", "Unsupported content retained in export metadata.", raw)
			continue
		}
		switch str(part["type"]) {
		case "text", "input_text", "output_text", "summary_text":
			text, ok := part["text"].(string)
			if !ok {
				b.unmapped("unsupported_content", "Text content is not a string.", raw)
				continue
			}
			parts = append(parts, ContentPart{Type: "text", Text: &text})
		case "image", "image_url", "input_image":
			source, err := b.standardImage(part)
			if err != nil {
				b.unmapped("invalid_image", err.Error(), raw)
				continue
			}
			parts = append(parts, ContentPart{Type: "image", Source: source})
			hasImage = true
		case "tool_use", "tool_result", "thinking", "redacted_thinking":
		default:
			b.unmapped("unsupported_content", "Unsupported content retained in export metadata.", raw)
		}
	}
	if hasImage || len(parts) > 1 {
		return parts
	}
	var result strings.Builder
	for _, part := range parts {
		if part.Text != nil {
			result.WriteString(*part.Text)
		}
	}
	return result.String()
}

func (b *builder) standardImage(part map[string]any) (*ImageSource, error) {
	source := object(part["source"])
	imageURL := object(part["image_url"])
	mime := str(first(source["media_type"], part["media_type"], imageURL["media_type"]))
	location := str(first(source["url"], imageURL["url"], part["image_url"], part["url"]))
	var encoded string
	inline := str(source["type"]) == "base64"
	if inline {
		encoded = str(source["data"])
	}
	if strings.HasPrefix(location, "data:") {
		header, body, ok := strings.Cut(strings.TrimPrefix(location, "data:"), ",")
		if !ok || !strings.HasSuffix(header, ";base64") {
			return nil, fmt.Errorf("image data URL must use base64")
		}
		declared := strings.TrimSuffix(header, ";base64")
		if mime != "" && mime != declared {
			return nil, fmt.Errorf("conflicting image media types")
		}
		mime = declared
		encoded = body
		inline = true
	}
	if inline {
		if len(encoded) > base64.StdEncoding.EncodedLen(MaxImageBytes) {
			return nil, fmt.Errorf("image exceeds encoded size limit")
		}
		if mediaExtensions[mime] == "" {
			return nil, fmt.Errorf("inline image requires supported media type")
		}
		data, err := base64.StdEncoding.Strict().DecodeString(encoded)
		if err != nil {
			return nil, fmt.Errorf("invalid image base64")
		}
		if _, err := validateImage(data, mime); err != nil {
			return nil, err
		}
		key := assetKey(data, mime)
		if b.assets == nil {
			b.assets = map[string]Asset{}
		}
		if _, ok := b.assets[key]; !ok {
			total := len(data)
			for _, asset := range b.assets {
				total += len(asset.Data)
			}
			if total > MaxAssetBytes {
				return nil, fmt.Errorf("total image resources exceed limit")
			}
			b.assets[key] = Asset{Data: data, MediaType: mime}
		}
		return &ImageSource{MediaType: mime, Path: key}, nil
	}
	parsed, err := url.Parse(location)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" || parsed.User != nil {
		return nil, fmt.Errorf("image URL must be an absolute HTTP(S) URL without user information")
	}
	if mime == "" {
		mime = b.imageMediaType
	}
	if mime == "" {
		switch strings.ToLower(path.Ext(parsed.Path)) {
		case ".png":
			mime = "image/png"
		case ".jpg", ".jpeg":
			mime = "image/jpeg"
		case ".gif":
			mime = "image/gif"
		case ".webp":
			mime = "image/webp"
		}
		if mime != "" {
			b.unmapped("inferred_image_media_type", "Image URL media type inferred from filename; remote bytes were not fetched or verified.", part)
		}
	}
	if mediaExtensions[mime] == "" {
		return nil, fmt.Errorf("image URL requires a supported explicit media type or image media type option")
	}
	return &ImageSource{MediaType: mime, Path: location}, nil
}

func textOf(v any) string {
	if text, ok := v.(string); ok {
		return text
	}
	var out strings.Builder
	switch parts := v.(type) {
	case []ContentPart:
		for _, p := range parts {
			if p.Type == "text" && p.Text != nil {
				out.WriteString(*p.Text)
			}
		}
	case []any:
		for _, p := range parts {
			m := object(p)
			if str(m["type"]) == "text" {
				out.WriteString(str(m["text"]))
			}
		}
	}
	return out.String()
}
func hasContent(v any) bool {
	switch parts := v.(type) {
	case string:
		return parts != ""
	case []ContentPart:
		for _, p := range parts {
			if p.Type == "image" && p.Source != nil || p.Type == "text" && p.Text != nil && *p.Text != "" {
				return true
			}
		}
	case []any:
		for _, p := range parts {
			m := object(p)
			if str(m["type"]) == "image" && object(m["source"]) != nil || str(m["type"]) == "text" && str(m["text"]) != "" {
				return true
			}
		}
	}
	return false
}
