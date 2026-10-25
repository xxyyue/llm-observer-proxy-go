// SPDX-License-Identifier: Apache-2.0

// Package trajectory reconstructs observed agent trajectories without changing the
// append-only source log. ATIF is an export representation, never the event store.
package trajectory

import "fmt"

const Version = "1.7"
const Namespace = "llm_observer_proxy"

type Metadata struct {
	Name    string `json:"agent_name,omitempty"`
	Version string `json:"agent_version,omitempty"`
}

type Options struct {
	ImageMediaType string
	Format         string
	Version        string
	RunID          string
	State          string
	Strict         bool
	Metadata       Metadata
}

func (o Options) Validate() error {
	if o.Format != "" && o.Format != "atif" {
		return fmt.Errorf("unsupported format %q; supported: atif", o.Format)
	}
	if o.Version != "" && o.Version != Version {
		return fmt.Errorf("unsupported ATIF version %q; supported: %s", o.Version, Version)
	}
	if o.ImageMediaType != "" && mediaExtensions[o.ImageMediaType] == "" {
		return fmt.Errorf("unsupported image media type")
	}
	return nil
}

type Diagnostic struct {
	Severity          string `json:"severity"`
	Code              string `json:"code"`
	Message           string `json:"message"`
	ExchangeID        string `json:"exchange_id,omitempty"`
	SourceRecordIndex int    `json:"source_record_index,omitempty"`
}

type SemanticError struct{ Diagnostics []Diagnostic }

func (e *SemanticError) Error() string {
	return fmt.Sprintf("strict export rejected %d reconstruction issue(s): %s", len(e.Diagnostics), e.Diagnostics[0].Code)
}

type EmptyError struct{}

func (*EmptyError) Error() string { return "no observable conversation steps to export" }

type Document struct {
	Assets        map[string]Asset `json:"-"`
	SchemaVersion string           `json:"schema_version"`
	SessionID     string           `json:"session_id,omitempty"`
	Agent         Agent            `json:"agent"`
	Steps         []Step           `json:"steps"`
	FinalMetrics  map[string]any   `json:"final_metrics,omitempty"`
	Extra         map[string]any   `json:"extra,omitempty"`
}

type Agent struct {
	Name    string         `json:"name"`
	Version string         `json:"version"`
	Extra   map[string]any `json:"extra,omitempty"`
}

type Step struct {
	ID          int            `json:"step_id"`
	Source      string         `json:"source"`
	Message     any            `json:"message"`
	Timestamp   string         `json:"timestamp,omitempty"`
	Model       string         `json:"model_name,omitempty"`
	Reasoning   string         `json:"reasoning_content,omitempty"`
	Tools       []ToolCall     `json:"tool_calls,omitempty"`
	Observation *Observation   `json:"observation,omitempty"`
	Metrics     map[string]any `json:"metrics,omitempty"`
	Extra       map[string]any `json:"extra,omitempty"`
}

type ToolCall struct {
	ID        string         `json:"tool_call_id"`
	Name      string         `json:"function_name"`
	Arguments map[string]any `json:"arguments"`
}

type Observation struct {
	Results []Result `json:"results"`
}
type Result struct {
	CallID  string         `json:"source_call_id,omitempty"`
	Content any            `json:"content"`
	Extra   map[string]any `json:"extra,omitempty"`
}

// Asset bytes are carried separately from ATIF JSON and published by the exporter.
type Asset struct {
	Data      []byte
	MediaType string
}
type ContentPart struct {
	Type   string       `json:"type"`
	Text   *string      `json:"text,omitempty"`
	Source *ImageSource `json:"source,omitempty"`
}
type ImageSource struct {
	MediaType string `json:"media_type"`
	Path      string `json:"path"`
}
