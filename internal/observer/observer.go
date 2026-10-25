// SPDX-License-Identifier: Apache-2.0

package observer

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// ContextMetadata is optional producer evidence, separate from provider payloads.
type ContextMetadata struct {
	ConversationID   string `json:"conversation_id,omitempty"`
	AgentID          string `json:"agent_id,omitempty"`
	ParentExchangeID string `json:"parent_exchange_id,omitempty"`
	Operation        string `json:"operation,omitempty"`
}

type Event struct {
	ContextMetadata     *ContextMetadata `json:"context_metadata,omitempty"`
	ExchangeID          string           `json:"exchange_id"`
	RunID               string           `json:"run_id,omitempty"`
	StartedAtMS         int64            `json:"started_at_ms"`
	CompletedAtMS       int64            `json:"completed_at_ms"`
	Method              string           `json:"method"`
	Path                string           `json:"path"`
	QueryString         string           `json:"query_string"`
	UpstreamURL         string           `json:"upstream_url"`
	RequestHeaders      map[string]any   `json:"request_headers"`
	RequestBody         *string          `json:"request_body"`
	RequestBodyEncoding string           `json:"request_body_encoding"`
	RequestJSON         any              `json:"request_json"`
	ResponseStatus      *int             `json:"response_status"`
	ResponseHeaders     map[string]any   `json:"response_headers"`
	ResponseBody        *string          `json:"response_body"`
	ResponseEncoding    string           `json:"response_body_encoding"`
	ResponseJSON        any              `json:"response_json"`
	Stream              bool             `json:"stream"`
	Error               *string          `json:"error"`
	Normalized          map[string]any   `json:"normalized"`
}

type Recorder struct {
	mu           sync.Mutex
	runID        string
	port         int
	eventsPath   string
	snapshotPath string
	pricing      func(string) map[string]float64
}

func NewRecorder(runID string, port int, eventsPath, snapshotPath string, pricing func(string) map[string]float64) *Recorder {
	return &Recorder{runID: runID, port: port, eventsPath: eventsPath, snapshotPath: snapshotPath, pricing: pricing}
}

func (r *Recorder) Record(event *Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	event.Normalized = Normalize(event.RequestJSON, event.ResponseJSON, valueOrEmpty(event.ResponseBody), event.Path, event.Stream, r.pricing)
	encoded, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("encode event: %w", err)
	}
	file, err := os.OpenFile(r.eventsPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open events file: %w", err)
	}
	if _, err = file.Write(append(encoded, '\n')); err != nil {
		_ = file.Close()
		return fmt.Errorf("append event: %w", err)
	}
	if err = file.Close(); err != nil {
		return fmt.Errorf("close events file: %w", err)
	}
	items, err := readEvents(r.eventsPath)
	if err != nil {
		return err
	}
	snapshot := BuildConversationSnapshot(r.runID, r.port, "running", items)
	return writeJSONAtomic(r.snapshotPath, snapshot)
}

func ReadHistory(runID string, port *int, state, path string, pricing func(string) map[string]float64) (map[string]any, error) {
	items, err := readEvents(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			items = []Event{}
		} else {
			return nil, err
		}
	}
	for index := range items {
		items[index].Normalized = Normalize(items[index].RequestJSON, items[index].ResponseJSON, valueOrEmpty(items[index].ResponseBody), items[index].Path, items[index].Stream, pricing)
	}
	return map[string]any{
		"run_id": runID,
		"port":   port,
		"state":  state,
		"count":  len(items),
		"items":  items,
		"totals": Summarize(items),
	}, nil
}

func ReadConversation(runID string, port *int, state, path string, pricing func(string) map[string]float64) (map[string]any, error) {
	items, err := readEvents(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			items = []Event{}
		} else {
			return nil, err
		}
	}
	resolvedPort := 0
	if port != nil {
		resolvedPort = *port
	}
	snapshot := BuildConversationSnapshot(runID, resolvedPort, state, items)
	if port == nil {
		snapshot["port"] = nil
	}
	return snapshot, nil
}

func readEvents(path string) ([]Event, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	items := make([]Event, 0)
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 64*1024*1024)
	for scanner.Scan() {
		var event Event
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			continue
		}
		items = append(items, event)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read events: %w", err)
	}
	return items, nil
}

func writeJSONAtomic(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".conversation-*.json")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if _, err = temporary.Write(append(data, '\n')); err != nil {
		_ = temporary.Close()
		return err
	}
	if err = temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

func valueOrEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
