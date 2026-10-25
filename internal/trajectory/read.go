// SPDX-License-Identifier: Apache-2.0
package trajectory

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/xxyyue/llm-observer-proxy-go/internal/observer"
)

// ExportFile reads only complete newline-terminated records within the file size
// observed at open. An in-flight final append is never interpreted as an event.
func ExportFile(path string, options Options) (*Document, error) {
	if err := options.Validate(); err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	reader := bufio.NewReader(io.LimitReader(file, info.Size()))
	var events []observer.Event
	var diagnostics []Diagnostic
	for line := 1; ; line++ {
		data, readErr := reader.ReadBytes('\n')
		if readErr == io.EOF {
			if len(bytes.TrimSpace(data)) > 0 {
				diagnostics = append(diagnostics, Diagnostic{Code: "incomplete_tail", Message: "Ignored an unterminated final event; export is a partial snapshot."})
			}
			break
		}
		if readErr != nil {
			return nil, fmt.Errorf("read event line %d: %w", line, readErr)
		}
		if len(bytes.TrimSpace(data)) == 0 {
			continue
		}
		var event observer.Event
		if err := json.Unmarshal(data, &event); err != nil {
			return nil, fmt.Errorf("invalid event JSON at line %d", line)
		}
		if event.RequestJSON == nil && event.ResponseJSON == nil && event.RequestBody == nil && event.ResponseBody == nil {
			return nil, fmt.Errorf("invalid event object at line %d: no request or response", line)
		}
		if event.ExchangeID == "" {
			event.ExchangeID = fmt.Sprintf("line-%d", line)
		}
		events = append(events, event)
	}
	document, err := Build(events, options, diagnostics)
	if err != nil {
		return nil, err
	}
	document.Extra[Namespace].(map[string]any)["source_bytes"] = info.Size()
	return document, nil
}

// WriteFile atomically replaces an export, but never its source event log.
func WriteFile(path, source string, document *Document) error {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	input, err := filepath.Abs(source)
	if err != nil {
		return err
	}
	if absolute == input {
		return fmt.Errorf("output must not overwrite the source event log")
	}
	inputInfo, err := os.Stat(input)
	if err != nil {
		return err
	}
	outputInfo, err := os.Stat(absolute)
	if err == nil && os.SameFile(inputInfo, outputInfo) {
		return fmt.Errorf("output must not overwrite the source event log")
	}
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	// Resolve parent directory symlinks as well, including a not-yet-created target.
	parent, err := filepath.EvalSymlinks(filepath.Dir(absolute))
	if err != nil {
		return err
	}
	resolvedInput, err := filepath.EvalSymlinks(input)
	if err != nil {
		return err
	}
	if filepath.Join(parent, filepath.Base(absolute)) == resolvedInput {
		return fmt.Errorf("output must not overwrite the source event log")
	}
	data, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(absolute), ".trajectory-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	if _, err = temp.Write(append(data, '\n')); err != nil {
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
	return os.Rename(temp.Name(), absolute)
}
