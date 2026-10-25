// SPDX-License-Identifier: Apache-2.0
package trajectory

import (
	"archive/zip"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var assetPattern = regexp.MustCompile(`^images/[a-f0-9]{64}\.(png|jpg|gif|webp)$`)

func validateAsset(key string, asset Asset) error {
	if !assetPattern.MatchString(key) {
		return fmt.Errorf("invalid image resource key")
	}
	if _, err := validateImage(asset.Data, asset.MediaType); err != nil {
		return err
	}
	if assetKey(asset.Data, asset.MediaType) != key {
		return fmt.Errorf("image resource hash mismatch")
	}
	return nil
}
func checkedAssetKeys(assets map[string]Asset) ([]string, error) {
	keys := make([]string, 0, len(assets))
	total := 0
	for key, asset := range assets {
		total += len(asset.Data)
		if total > MaxAssetBytes {
			return nil, fmt.Errorf("total image resources exceed limit")
		}
		if err := validateAsset(key, asset); err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys, nil
}

// WithAssetPaths copies only the standard content locations. Raw input_context
// manifests and all other extension metadata are deliberately left unchanged.
func (d *Document) WithAssetPaths(prefix string) (*Document, error) {
	if d == nil {
		return nil, fmt.Errorf("missing trajectory document")
	}
	if _, err := checkedAssetKeys(d.Assets); err != nil {
		return nil, err
	}
	clone := *d
	clone.Steps = append([]Step(nil), d.Steps...)
	rewrite := func(value any) any {
		change := func(source ImageSource) ImageSource {
			if _, ok := d.Assets[source.Path]; ok {
				if prefix != "" {
					source.Path = strings.TrimRight(prefix, "/") + "/" + source.Path
				}
			}
			return source
		}
		switch parts := value.(type) {
		case []ContentPart:
			result := append([]ContentPart(nil), parts...)
			for i, p := range result {
				if p.Type == "image" && p.Source != nil {
					source := change(*p.Source)
					result[i].Source = &source
				}
			}
			return result
		case []any:
			result := append([]any(nil), parts...)
			for i, p := range result {
				m := object(p)
				if str(m["type"]) != "image" {
					continue
				}
				source := object(m["source"])
				if source == nil {
					continue
				}
				if _, ok := d.Assets[str(source["path"])]; !ok {
					continue
				}
				next := map[string]any{}
				for k, v := range m {
					next[k] = v
				}
				newSource := map[string]any{}
				for k, v := range source {
					newSource[k] = v
				}
				newSource["path"] = change(ImageSource{Path: str(source["path"])}).Path
				next["source"] = newSource
				result[i] = next
			}
			return result
		}
		return value
	}
	for i := range clone.Steps {
		step := &clone.Steps[i]
		step.Message = rewrite(step.Message)
		if step.Observation != nil {
			observation := *step.Observation
			observation.Results = append([]Result(nil), observation.Results...)
			for j := range observation.Results {
				observation.Results[j].Content = rewrite(observation.Results[j].Content)
			}
			step.Observation = &observation
		}
	}
	return &clone, nil
}

// resourceRoot canonicalizes the caller-selected trusted location (including
// ordinary OS aliases such as macOS /var). The resource root itself must not be
// a symlink; generated descendants are checked separately and confined by Root.
func resourceRoot(path string, create bool) (*os.Root, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if create {
		if err := os.MkdirAll(absolute, 0700); err != nil {
			return nil, err
		}
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("image resource root must be a real directory")
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return nil, err
	}
	return os.OpenRoot(canonical)
}

func imageDirectory(root *os.Root, create bool) (*os.Root, error) {
	if create {
		if err := root.Mkdir("images", 0700); err != nil && !os.IsExist(err) {
			return nil, err
		}
	}
	info, err := root.Lstat("images")
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("image directory must be a real directory")
	}
	return root.OpenRoot("images")
}
func readResource(images *os.Root, name string) ([]byte, error) {
	info, err := images.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > MaxImageBytes {
		return nil, fmt.Errorf("image resource is not a bounded regular file")
	}
	file, err := images.Open(name)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(info, opened) {
		return nil, fmt.Errorf("image resource changed during open")
	}
	data, err := io.ReadAll(io.LimitReader(file, MaxImageBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxImageBytes {
		return nil, fmt.Errorf("image resource exceeds size limit")
	}
	return data, nil
}

// WriteAssets publishes validated immutable resources using atomic no-replace
// links. Existing resources are reusable only when their bytes match exactly.
func WriteAssets(path string, assets map[string]Asset) error {
	keys, err := checkedAssetKeys(assets)
	if err != nil {
		return err
	}
	if len(keys) == 0 {
		return nil
	}
	root, err := resourceRoot(path, true)
	if err != nil {
		return err
	}
	defer root.Close()
	images, err := imageDirectory(root, true)
	if err != nil {
		return err
	}
	defer images.Close()
	for _, key := range keys {
		name := strings.TrimPrefix(key, "images/")
		asset := assets[key]
		if data, err := readResource(images, name); err == nil {
			if !bytes.Equal(data, asset.Data) {
				return fmt.Errorf("existing image resource differs")
			}
			continue
		} else if !os.IsNotExist(err) {
			return err
		}
		random := make([]byte, 16)
		if _, err := rand.Read(random); err != nil {
			return err
		}
		temp := ".asset-" + hex.EncodeToString(random)
		file, err := images.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		_, writeErr := file.Write(asset.Data)
		if writeErr == nil {
			writeErr = file.Sync()
		}
		closeErr := file.Close()
		if writeErr == nil {
			writeErr = closeErr
		}
		if writeErr == nil {
			writeErr = images.Link(temp, name)
			if os.IsExist(writeErr) {
				data, readErr := readResource(images, name)
				if readErr == nil && bytes.Equal(data, asset.Data) {
					writeErr = nil
				}
			}
		}
		removeErr := images.Remove(temp)
		if writeErr != nil {
			return writeErr
		}
		if removeErr != nil {
			return removeErr
		}
	}
	return nil
}
func ReadAsset(path, key string) (Asset, error) {
	if !assetPattern.MatchString(key) {
		return Asset{}, fmt.Errorf("invalid image resource key")
	}
	root, err := resourceRoot(path, false)
	if err != nil {
		return Asset{}, err
	}
	defer root.Close()
	images, err := imageDirectory(root, false)
	if err != nil {
		return Asset{}, err
	}
	defer images.Close()
	data, err := readResource(images, strings.TrimPrefix(key, "images/"))
	if err != nil {
		return Asset{}, err
	}
	mime := "image/" + strings.TrimPrefix(filepath.Ext(key), ".")
	if mime == "image/jpg" {
		mime = "image/jpeg"
	}
	asset := Asset{Data: data, MediaType: mime}
	if err := validateAsset(key, asset); err != nil {
		return Asset{}, err
	}
	return asset, nil
}

func checkedOutput(path, source string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	input, err := filepath.Abs(source)
	if err != nil {
		return "", err
	}
	if absolute == input {
		return "", fmt.Errorf("output must not overwrite the source event log")
	}
	inputInfo, err := os.Stat(input)
	if err != nil {
		return "", err
	}
	outputInfo, err := os.Stat(absolute)
	if err == nil && os.SameFile(inputInfo, outputInfo) {
		return "", fmt.Errorf("output must not overwrite the source event log")
	}
	if err != nil && !os.IsNotExist(err) {
		return "", err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(absolute))
	if err != nil {
		return "", err
	}
	resolvedInput, err := filepath.EvalSymlinks(input)
	if err != nil {
		return "", err
	}
	resolved := filepath.Join(parent, filepath.Base(absolute))
	if resolved == resolvedInput {
		return "", fmt.Errorf("output must not overwrite the source event log")
	}
	if info, err := os.Lstat(resolved); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("output must not be a symlink")
	}
	return resolved, nil
}
func atomicOutput(path string, write func(io.Writer) error) error {
	temp, err := os.CreateTemp(filepath.Dir(path), ".trajectory-*")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	if err = write(temp); err != nil {
		temp.Close()
		return err
	}
	if err = temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err = temp.Close(); err != nil {
		return err
	}
	return os.Rename(temp.Name(), path)
}
func WriteFile(path, source string, document *Document) error {
	output, err := checkedOutput(path, source)
	if err != nil {
		return err
	}
	copy, err := document.WithAssetPaths("")
	if err != nil {
		return err
	}
	// Verify serializability before publishing any resources.
	if _, err = json.Marshal(copy); err != nil {
		return err
	}
	assetDirectory := ""
	published := false
	defer func() {
		if assetDirectory != "" && !published {
			os.RemoveAll(assetDirectory)
		}
	}()
	if len(document.Assets) > 0 {
		assetDirectory, err = os.MkdirTemp(filepath.Dir(output), filepath.Base(output)+".assets-")
		if err != nil {
			return err
		}
		if err = WriteAssets(assetDirectory, document.Assets); err != nil {
			return err
		}
		copy, err = document.WithAssetPaths(filepath.Base(assetDirectory))
		if err != nil {
			return err
		}
	}
	err = atomicOutput(output, func(writer io.Writer) error {
		encoder := json.NewEncoder(writer)
		encoder.SetIndent("", "  ")
		return encoder.Encode(copy)
	})
	if err == nil {
		published = true
	}
	return err
}
func WriteZIP(writer io.Writer, document *Document) error {
	copy, err := document.WithAssetPaths("")
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(copy, "", "  ")
	if err != nil {
		return err
	}
	keys, err := checkedAssetKeys(document.Assets)
	if err != nil {
		return err
	}
	archive := zip.NewWriter(writer)
	add := func(name string, data []byte) error {
		header := &zip.FileHeader{Name: name, Method: zip.Deflate}
		header.SetMode(0600)
		file, err := archive.CreateHeader(header)
		if err != nil {
			return err
		}
		_, err = file.Write(data)
		return err
	}
	if err = add("trajectory.json", append(data, '\n')); err != nil {
		archive.Close()
		return err
	}
	for _, key := range keys {
		if err = add(key, document.Assets[key].Data); err != nil {
			archive.Close()
			return err
		}
	}
	return archive.Close()
}
func WriteZIPFile(path, source string, document *Document) error {
	output, err := checkedOutput(path, source)
	if err != nil {
		return err
	}
	return atomicOutput(output, func(writer io.Writer) error { return WriteZIP(writer, document) })
}
