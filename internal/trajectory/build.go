// SPDX-License-Identifier: Apache-2.0
package trajectory

import (
	"fmt"
	"sort"
	"time"

	"github.com/xxyyue/llm-observer-proxy-go/internal/observer"
)

type builder struct {
	document        Document
	exchange        string
	diagnostics     []Diagnostic
	fragments       []any
	calls           map[string][]int
	contexts        map[string][]message
	previous        []message
	totals          map[string]any
	metricsCount    int
	metricsComplete int
	costCount       int
}

func (b *builder) issue(code, text string) {
	b.diagnostics = append(b.diagnostics, Diagnostic{Code: code, Message: text, ExchangeID: b.exchange})
}
func (b *builder) unmapped(code, text string, value any) {
	b.issue(code, text)
	b.fragments = append(b.fragments, map[string]any{"exchange_id": b.exchange, "kind": code, "value": value})
}

// Build reconstructs only observable interactions. When context relationships are
// ambiguous, the request context is retained in extra instead of guessing a link.
func Build(events []observer.Event, options Options, initial []Diagnostic) (*Document, error) {
	if err := options.Validate(); err != nil {
		return nil, err
	}
	b := builder{calls: map[string][]int{}, contexts: map[string][]message{}, totals: map[string]any{}, diagnostics: initial}
	b.document = Document{SchemaVersion: "ATIF-v" + Version, SessionID: options.RunID, Agent: Agent{Name: options.Metadata.Name, Version: options.Metadata.Version}, Steps: []Step{}}
	if b.document.Agent.Name == "" {
		b.document.Agent.Name = "unknown"
	}
	if b.document.Agent.Version == "" {
		b.document.Agent.Version = "unknown"
	}
	b.document.Agent.Extra = map[string]any{Namespace: map[string]any{"metadata_source": "caller_or_unknown"}}
	events = append([]observer.Event(nil), events...)
	allTimed := len(events) > 0
	for _, event := range events {
		if event.StartedAtMS <= 0 {
			allTimed = false
		}
	}
	if allTimed {
		sort.SliceStable(events, func(i, j int) bool { return events[i].StartedAtMS < events[j].StartedAtMS })
	}
	seenExchanges := map[string]bool{}
	var maxCompleted int64
	latestResponseID := ""
	for index, event := range events {
		b.exchange = event.ExchangeID
		if event.RunID != "" {
			if b.document.SessionID == "" {
				b.document.SessionID = event.RunID
			} else if event.RunID != b.document.SessionID {
				return nil, fmt.Errorf("event %s belongs to a different run", event.ExchangeID)
			}
		}
		if event.ExchangeID != "" && seenExchanges[event.ExchangeID] {
			return nil, fmt.Errorf("duplicate exchange_id %q in source log", event.ExchangeID)
		}
		seenExchanges[event.ExchangeID] = true
		concurrent := event.StartedAtMS > 0 && event.StartedAtMS < maxCompleted
		if concurrent {
			b.issue("overlapping_exchanges", "Requests overlap; steps are ordered by observed start time, not asserted causal order.")
		}
		if event.CompletedAtMS > maxCompleted {
			maxCompleted = event.CompletedAtMS
		}
		requestJSON := decodeBody(event.RequestJSON, event.RequestBody)
		event.RequestJSON = requestJSON
		request := object(requestJSON)
		messages := b.request(requestJSON)
		linked := false
		offset := 0
		if index == 0 {
			linked = true
			if str(request["previous_response_id"]) != "" {
				b.issue("unknown_previous_response", "Initial request references prior context absent from this event log.")
			}
		} else {
			previousID := str(request["previous_response_id"])
			if previousID != "" {
				if prior, ok := b.contexts[previousID]; ok {
					linked = true
					if previousID != latestResponseID {
						b.issue("branched_response_context", "Continuation references an earlier branch; no complete linear trajectory is asserted.")
					}
					// Responses supplies only incremental input; repeated instructions are
					// context, not another system event when they are unchanged.
					for offset < len(messages) && offset < len(prior) && messages[offset].role == "system" && fingerprint(messages[offset]) == fingerprint(prior[offset]) {
						offset++
					}
				} else {
					b.issue("unknown_previous_response", "previous_response_id is not present in the observed history.")
				}
			} else if !concurrent {
				offset, linked = overlap(b.previous, messages)
			}
		}
		if linked {
			for _, m := range messages[offset:] {
				b.add(m, false, event)
			}
		} else {
			b.unmapped("unlinked_context", "Request context could not be uniquely linked; retained separately instead of duplicating or guessing its steps.", requestJSON)
			// Tool results carry their own explicit causal link, even when their
			// surrounding conversation was truncated or concurrently modified.
			for _, m := range messages {
				if m.role == "tool" {
					b.add(m, false, event)
				}
			}
		}
		response, usage, responseID, complete := b.response(event)
		metrics := b.metrics(event, usage)
		failed := event.Error != nil || (event.ResponseStatus != nil && *event.ResponseStatus >= 400)
		if failed {
			b.issue("failed_exchange", "An observed request failed; it is retained as an error, not an assistant answer.")
			b.fragments = append(b.fragments, map[string]any{"kind": "failed_exchange", "exchange_id": event.ExchangeID, "status": event.ResponseStatus, "error": event.Error, "response": decodeBody(event.ResponseJSON, event.ResponseBody), "body": event.ResponseBody, "metrics": metrics})
		}
		if response.text != "" || len(response.tools) > 0 || response.reasoning != "" {
			b.add(response, true, event)
			step := &b.document.Steps[len(b.document.Steps)-1]
			step.Model = str(request["model"])
			step.Metrics = metrics
			meta := step.Extra[Namespace].(map[string]any)
			meta["context_linked"] = linked
			if failed || !complete {
				meta["partial_response"] = true
			}
		}
		context := append([]message(nil), messages...)
		if response.text != "" || len(response.tools) > 0 || response.reasoning != "" {
			context = append(context, response)
		}
		if previousID := str(request["previous_response_id"]); previousID != "" {
			if prior, ok := b.contexts[previousID]; ok {
				context = append(append([]message(nil), prior...), context...)
			}
		}
		b.previous = context
		if responseID != "" {
			b.contexts[responseID] = context
			latestResponseID = responseID
		}
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
				b.exchange = ""
				b.issue("missing_tool_result", "No observed result for tool call "+call.ID+"; execution outcome is unknown.")
			}
		}
	}
	if len(b.document.Steps) == 0 {
		return nil, &EmptyError{}
	}
	if options.Strict && len(b.diagnostics) > 0 {
		return nil, &SemanticError{Diagnostics: b.diagnostics}
	}
	b.totals["total_steps"] = len(b.document.Steps)
	b.totals["extra"] = map[string]any{Namespace: map[string]any{"observed_exchanges": len(events), "exchanges_with_usage": b.metricsCount, "exchanges_with_recorded_cost": b.costCount, "usage_coverage_complete": b.metricsComplete == len(events), "cost_coverage_complete": b.costCount == len(events), "includes_known_failed_exchange_metrics": true}}
	b.document.FinalMetrics = b.totals
	state := options.State
	if state == "" {
		state = "unknown"
	}
	meta := map[string]any{"exporter_version": 1, "source": "events.jsonl", "state": state, "scope": "observed_llm_traffic", "snapshot": state != "stopped", "reconstruction_complete": len(b.diagnostics) == 0, "event_count": len(events), "diagnostics": b.diagnostics, "unmapped": b.fragments}
	if state == "stopped" {
		meta["completion_note"] = "Proxy stopped; this does not prove the agent task or unobserved tools completed."
	}
	b.document.Extra = map[string]any{Namespace: meta}
	return &b.document, nil
}

// overlap only matches context positions, never globally deduplicates equal text.
func overlap(previous, current []message) (int, bool) {
	if len(previous) == 0 {
		return 0, true
	}
	if len(current) >= len(previous) {
		match := true
		for i := range previous {
			if fingerprint(previous[i]) != fingerprint(current[i]) {
				match = false
				break
			}
		}
		if match {
			return len(previous), true
		}
	}
	// A unique retained suffix supports context-window truncation. Keep identical
	// leading system instructions out of the suffix alignment.
	ps, cs := 0, 0
	for ps < len(previous) && previous[ps].role == "system" {
		ps++
	}
	for cs < len(current) && current[cs].role == "system" {
		cs++
	}
	if ps != cs {
		return 0, false
	}
	for i := 0; i < ps; i++ {
		if fingerprint(previous[i]) != fingerprint(current[i]) {
			return 0, false
		}
	}
	for length := min(len(previous)-ps, len(current)-cs); length > 0; length-- {
		match := true
		for i := 0; i < length; i++ {
			if fingerprint(previous[len(previous)-length+i]) != fingerprint(current[cs+i]) {
				match = false
				break
			}
		}
		if match {
			return cs + length, true
		}
	}
	return 0, false
}

func (b *builder) add(m message, observed bool, event observer.Event) {
	if m.role == "tool" {
		targets := b.calls[m.resultID]
		if m.resultID == "" || len(targets) != 1 {
			b.unmapped("orphan_tool_result", "Tool result has no unique observed or supplied call.", m.raw)
			return
		}
		step := &b.document.Steps[targets[0]]
		if step.Observation == nil {
			step.Observation = &Observation{Results: []Result{}}
		}
		for _, result := range step.Observation.Results {
			if result.CallID == m.resultID {
				if result.Content != m.text {
					b.unmapped("conflicting_tool_result", "Different results refer to the same tool call.", m.raw)
				}
				return
			}
		}
		step.Observation.Results = append(step.Observation.Results, Result{CallID: m.resultID, Content: m.text, Extra: map[string]any{Namespace: map[string]any{"observed_in_exchange": b.exchange, "provider_result": m.raw}}})
		return
	}
	step := Step{ID: len(b.document.Steps) + 1, Source: m.role, Message: m.text}
	meta := map[string]any{"exchange_id": b.exchange, "observed_response": observed}
	if !observed {
		meta["origin"] = "request_context"
		if role := str(object(m.raw)["role"]); role != "" {
			meta["original_role"] = role
		}
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
			b.issue("duplicate_tool_id", "Tool call ID is not unique: "+tool.ID)
		}
	}
	b.document.Steps = append(b.document.Steps, step)
}
