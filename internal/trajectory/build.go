// SPDX-License-Identifier: Apache-2.0
package trajectory

import (
	"fmt"
	"sort"
	"time"

	"github.com/xxyyue/llm-observer-proxy-go/internal/observer"
)

type builder struct {
	assets          map[string]Asset
	imageMediaType  string
	document        Document
	exchange        string
	recordIndex     int
	diagnostics     []Diagnostic
	calls           map[string][]int
	candidates      []contextCandidate
	totals          map[string]any
	metricsCount    int
	metricsComplete int
	costCount       int
}

func (b *builder) issue(code, text string) {
	b.diagnostics = append(b.diagnostics, Diagnostic{Severity: "error", Code: code, Message: text, ExchangeID: b.exchange, SourceRecordIndex: b.recordIndex})
}
func (b *builder) notice(code, text string) {
	b.diagnostics = append(b.diagnostics, Diagnostic{Severity: "info", Code: code, Message: text, ExchangeID: b.exchange, SourceRecordIndex: b.recordIndex})
}
func (b *builder) provenance() map[string]any {
	return map[string]any{"exchange_id": b.exchange, "source_record_index": b.recordIndex}
}
func semanticErrors(diagnostics []Diagnostic) []Diagnostic {
	var result []Diagnostic
	for _, d := range diagnostics {
		if d.Severity == "error" || d.Severity == "" {
			result = append(result, d)
		}
	}
	return result
}

// Build retains an execution timeline and an semantic input checkpoint for
// every source record. Causal ancestry and representation fidelity are separate.
func Build(events []observer.Event, options Options, initial []Diagnostic) (*Document, error) {
	if err := options.Validate(); err != nil {
		return nil, err
	}
	b := builder{assets: map[string]Asset{}, imageMediaType: options.ImageMediaType, calls: map[string][]int{}, totals: map[string]any{}, diagnostics: initial}
	b.document = Document{SchemaVersion: "ATIF-v" + Version, SessionID: options.RunID, Agent: Agent{Name: options.Metadata.Name, Version: options.Metadata.Version}, Steps: []Step{}}
	if b.document.Agent.Name == "" {
		b.document.Agent.Name = "unknown"
	}
	if b.document.Agent.Version == "" {
		b.document.Agent.Version = "unknown"
	}
	b.document.Agent.Extra = map[string]any{Namespace: map[string]any{"metadata_source": "caller_or_unknown"}}
	type indexedEvent struct {
		event  observer.Event
		record int
	}
	ordered := make([]indexedEvent, len(events))
	allTimed := len(events) > 0
	for i, event := range events {
		ordered[i] = indexedEvent{event, i + 1}
		if event.StartedAtMS <= 0 {
			allTimed = false
		}
	}
	if allTimed {
		sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].event.StartedAtMS < ordered[j].event.StartedAtMS })
	}
	seen := map[string]bool{}
	var maxCompleted int64
	for index, entry := range ordered {
		event := entry.event
		b.exchange = event.ExchangeID
		b.recordIndex = entry.record
		if event.RunID != "" {
			if b.document.SessionID == "" {
				b.document.SessionID = event.RunID
			} else if event.RunID != b.document.SessionID {
				return nil, fmt.Errorf("event %s belongs to a different run", event.ExchangeID)
			}
		}
		reused := event.ExchangeID != "" && seen[event.ExchangeID]
		seen[event.ExchangeID] = true
		if reused {
			b.notice("reused_exchange_id", "Source identifier reused; records stay distinct by source position and totals may include duplicate logging.")
		}
		overlapping := event.StartedAtMS > 0 && event.StartedAtMS < maxCompleted
		if overlapping {
			b.notice("overlapping_exchanges", "Calls overlap; each has its own captured input checkpoint, and observation order does not assert causal order.")
		}
		if event.CompletedAtMS > maxCompleted {
			maxCompleted = event.CompletedAtMS
		}
		event.RequestJSON = decodeBody(event.RequestJSON, event.RequestBody)
		messages := b.request(event.RequestJSON)
		for i := range messages {
			messages[i].identity = fingerprint(messages[i])
		}
		contextID := b.captureContext(event)
		match := b.matchContext(event, messages, contextID)
		if index == 0 {
			for _, m := range messages {
				if m.role != "tool" {
					b.add(m, false, event)
				}
			}
		} else if match.append {
			for _, m := range messages[match.offset:] {
				if m.role != "tool" {
					b.add(m, false, event)
				}
			}
		}
		// Results can be observed after intervening calls. Backfill for display, but
		// every subsequent call's replacement checkpoint prevents future leakage.
		for _, m := range messages {
			if m.role == "tool" {
				b.add(m, false, event)
			}
		}
		boundaryID := b.boundary(event, messages, contextID, match)
		response, usage, responseID, complete := b.response(event)
		response.identity = fingerprint(response)
		metrics := b.metrics(event, usage)
		failed := event.Error != nil || (event.ResponseStatus != nil && *event.ResponseStatus >= 400)
		meaningful := hasContent(response.value()) || len(response.tools) > 0 || response.reasoning != ""
		if meaningful {
			b.add(response, true, event)
			step := &b.document.Steps[len(b.document.Steps)-1]
			step.Model = str(object(event.RequestJSON)["model"])
			step.Metrics = metrics
			meta := step.Extra[Namespace].(map[string]any)
			meta["input_context_id"] = contextID
			meta["input_boundary_step_id"] = boundaryID
			meta["branch_id"] = match.branch
			if failed || !complete {
				meta["response_state"] = "partial"
			} else {
				meta["response_state"] = "captured"
			}
		}
		if failed || !meaningful || reused {
			kind := "request_completed_without_content"
			if failed {
				kind = "request_failed"
			} else if reused {
				kind = "source_identifier_reused"
			}
			meta := b.provenance()
			meta["operational_event"] = kind
			meta["input_context_id"] = contextID
			meta["input_boundary_step_id"] = boundaryID
			meta["response_status"] = event.ResponseStatus
			meta["transport_error"] = event.Error != nil
			meta["metrics"] = metrics
			b.document.Steps = append(b.document.Steps, Step{ID: len(b.document.Steps) + 1, Source: "system", Message: "Observed LLM operation: " + kind, Extra: map[string]any{Namespace: meta}})
			if failed {
				b.notice("failed_exchange", "Failure is represented as an operational event, not an invented assistant answer.")
			}
		}
		context := append([]message(nil), messages...)
		if meaningful {
			context = append(context, response)
		}
		candidate := contextCandidate{record: entry.record, exchangeID: event.ExchangeID, responseID: responseID, branch: match.branch, completed: event.CompletedAtMS, messages: context, contextID: contextID, configurationID: digestJSON(contextConfiguration(object(event.RequestJSON))), responseText: textOf(response.value())}
		if event.ContextMetadata != nil {
			candidate.conversation = event.ContextMetadata.ConversationID
			candidate.agent = event.ContextMetadata.AgentID
		}
		b.candidates = append(b.candidates, candidate)
	}
	for _, step := range b.document.Steps {
		observed := map[string]bool{}
		if step.Observation != nil {
			for _, result := range step.Observation.Results {
				observed[result.CallID] = true
			}
		}
		for _, call := range step.Tools {
			if !observed[call.ID] {
				meta := step.Extra[Namespace].(map[string]any)
				b.exchange = str(meta["exchange_id"])
				b.recordIndex, _ = meta["source_record_index"].(int)
				b.notice("missing_tool_result", "Tool result was not observed; execution outcome remains unknown.")
				meta["unobserved_tool_result"] = true
			}
		}
	}
	if len(events) == 0 || len(b.document.Steps) == 0 {
		return nil, &EmptyError{}
	}
	problems := semanticErrors(b.diagnostics)
	if options.Strict && len(problems) > 0 {
		return nil, &SemanticError{Diagnostics: problems}
	}
	b.totals["total_steps"] = len(b.document.Steps)
	b.totals["extra"] = map[string]any{Namespace: map[string]any{"observed_exchanges": len(events), "exchanges_with_usage": b.metricsCount, "exchanges_with_recorded_cost": b.costCount, "usage_coverage_complete": b.metricsComplete == len(events), "cost_coverage_complete": b.costCount == len(events), "includes_known_failed_exchange_metrics": true, "aggregation_basis": "source_records", "distinct_provider_inferences_verified": false}}
	b.document.FinalMetrics = b.totals
	b.document.Assets = b.assets
	state := options.State
	if state == "" {
		state = "unknown"
	}
	b.document.Extra = map[string]any{Namespace: map[string]any{"exporter_version": 3, "source": "events.jsonl", "state": state, "scope": "captured_llm_requests_and_responses", "snapshot": state != "stopped", "reconstruction_complete": len(problems) == 0, "semantic_projection_complete": len(problems) == 0, "causal_relationships": "evidence_only", "event_count": len(events), "diagnostics": b.diagnostics, "context_contract": "ordered_context_messages_v1; semantic ATIF projection only; original request parameters and provider envelopes remain in events.jsonl"}}
	return &b.document, nil
}

func (b *builder) add(m message, observed bool, event observer.Event) {
	if m.role == "tool" {
		targets := b.calls[m.resultID]
		if m.resultID == "" || len(targets) != 1 {
			// The result still exists in its exact standard context checkpoint. Without a
			// unique originating call, do not fabricate or arbitrarily select a reference.
			b.notice("unresolved_tool_origin", "Tool result is preserved in its input checkpoint; no unique originating call is established.")
			return
		}
		step := &b.document.Steps[targets[0]]
		if step.Observation == nil {
			step.Observation = &Observation{Results: []Result{}}
		}
		for _, result := range step.Observation.Results {
			if result.CallID == m.resultID {
				if digestJSON(result.Content) != digestJSON(m.value()) {
					b.notice("tool_result_changed", "Different observations for the same tool ID are preserved in their per-call checkpoints.")
				}
				return
			}
		}
		step.Observation.Results = append(step.Observation.Results, Result{CallID: m.resultID, Content: m.value(), Extra: map[string]any{Namespace: map[string]any{"observed_in_exchange": b.exchange, "observed_in_record": b.recordIndex}}})
		return
	}
	step := Step{ID: len(b.document.Steps) + 1, Source: m.role, Message: m.value()}
	meta := b.provenance()
	meta["observed_response"] = observed
	if !observed {
		meta["origin"] = "request_context"
		meta["original_role"] = roleOf(m)
	}
	if observed && event.CompletedAtMS > 0 {
		step.Timestamp = time.UnixMilli(event.CompletedAtMS).UTC().Format(time.RFC3339Nano)
	}
	if m.role == "agent" {
		step.Reasoning = m.reasoning
		step.Tools = m.tools
	}
	step.Extra = map[string]any{Namespace: meta}
	for _, tool := range step.Tools {
		b.calls[tool.ID] = append(b.calls[tool.ID], len(b.document.Steps))
		if len(b.calls[tool.ID]) > 1 {
			b.notice("duplicate_tool_id", "Repeated tool ID retained; result attribution requires a unique origin.")
		}
	}
	b.document.Steps = append(b.document.Steps, step)
}
