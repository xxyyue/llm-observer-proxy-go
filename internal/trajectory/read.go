// SPDX-License-Identifier: Apache-2.0
package trajectory

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"

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
		if err := decodeJSON(data, &event); err != nil {
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
