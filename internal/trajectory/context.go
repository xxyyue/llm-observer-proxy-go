// SPDX-License-Identifier: Apache-2.0
package trajectory

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/xxyyue/llm-observer-proxy-go/internal/observer"
)

type contextCandidate struct {
	record       int
	exchangeID   string
	responseID   string
	branch       string
	conversation string
	agent        string
	completed    int64
	messages     []message
	contextID    string
	responseText string
}

type contextMatch struct {
	kind     string
	evidence string
	branch   string
	parents  []int
	offset   int
	append   bool
}

func digestJSON(value any) string {
	data, _ := json.Marshal(value)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
func joinContent(left, right any) any {
	if !hasContent(left) {
		return right
	}
	if !hasContent(right) {
		return left
	}
	if a, ok := left.(string); ok {
		if c, ok := right.(string); ok {
			return a + c
		}
	}
	parts := []ContentPart{}
	appendParts := func(v any) {
		switch x := v.(type) {
		case string:
			t := x
			parts = append(parts, ContentPart{Type: "text", Text: &t})
		case []ContentPart:
			parts = append(parts, x...)
		}
	}
	appendParts(left)
	appendParts(right)
	return parts
}

func roleOf(m message) string {
	if m.originalRole != "" {
		return m.originalRole
	}
	if m.role == "agent" {
		return "assistant"
	}
	return m.role
}
func projection(m message) map[string]any {
	result := map[string]any{"role": roleOf(m), "content": m.value()}
	if m.role != roleOf(m) {
		result["atif_source"] = m.role
	}
	if len(m.tools) > 0 {
		result["tool_calls"] = m.tools
	}
	if m.resultID != "" {
		result["tool_call_id"] = m.resultID
	}
	if m.reasoning != "" {
		result["reasoning_content"] = m.reasoning
	}
	return result
}
func projected(messages []message) []map[string]any {
	result := make([]map[string]any, 0, len(messages))
	for _, m := range messages {
		result = append(result, projection(m))
	}
	return result
}
func contextConfiguration(request map[string]any) map[string]any {
	result := map[string]any{}
	for key, value := range request {
		switch key {
		case "messages", "input", "system", "instructions":
		default:
			result[key] = value
		}
	}
	return result
}

func (b *builder) captureContext(event observer.Event, messages []message) string {
	id := digestJSON(map[string]any{"path": event.Path, "request": event.RequestJSON, "request_body": event.RequestBody})
	if _, exists := b.inputContexts[id]; exists {
		return id
	}
	value := map[string]any{"path": event.Path, "request": event.RequestJSON, "projection": projected(messages), "configuration": contextConfiguration(object(event.RequestJSON)), "fidelity": "captured_typed_request", "provider_state": "not_observed"}
	if event.RequestBody != nil {
		body := *event.RequestBody
		hash := sha256.Sum256([]byte(body))
		value["request_body_sha256"] = hex.EncodeToString(hash[:])
		var decoded any
		equal := decodeJSON([]byte(body), &decoded) == nil && digestJSON(decoded) == digestJSON(event.RequestJSON)
		value["typed_matches_request_body"] = equal
		if !equal {
			value["raw_request_body"] = body
			b.notice("captured_body_differs", "Typed request and captured body differ; both are preserved, wire equivalence is not asserted.")
		}
	}
	b.inputContexts[id] = value
	return id
}

func temporalCandidate(candidate contextCandidate, event observer.Event) bool {
	return event.StartedAtMS <= 0 || candidate.completed <= 0 || candidate.completed <= event.StartedAtMS
}
func commonPrefix(a, b []message) int {
	i := 0
	for i < len(a) && i < len(b) && fingerprint(a[i]) == fingerprint(b[i]) {
		i++
	}
	return i
}
func suffixMatch(previous, current []message) int {
	ps, cs := 0, 0
	for ps < len(previous) && previous[ps].role == "system" {
		ps++
	}
	for cs < len(current) && current[cs].role == "system" {
		cs++
	}
	if ps != cs {
		return 0
	}
	for i := 0; i < ps; i++ {
		if fingerprint(previous[i]) != fingerprint(current[i]) {
			return 0
		}
	}
	for length := min(len(previous)-ps, len(current)-cs); length > 0; length-- {
		matched := true
		for i := 0; i < length; i++ {
			if fingerprint(previous[len(previous)-length+i]) != fingerprint(current[cs+i]) {
				matched = false
				break
			}
		}
		if matched {
			return cs + length
		}
	}
	return 0
}

// matchContext consults all prior contexts. Parent relationships are evidence,
// never a prerequisite for preserving the exact submitted input checkpoint.
func (b *builder) matchContext(event observer.Event, messages []message, contextID string) contextMatch {
	hints := event.ContextMetadata
	match := contextMatch{kind: "replacement", evidence: "observed_request", branch: fmt.Sprintf("context-%d", b.recordIndex)}
	if len(b.candidates) == 0 {
		if str(object(event.RequestJSON)["previous_response_id"]) != "" {
			b.notice("unknown_previous_response", "Initial request references unobserved provider state; captured request is exact, full effective context is unknown.")
		}
		match.kind = "snapshot"
		match.evidence = "initial_capture"
		match.append = true
		return b.applyOperation(match, hints)
	}
	previousID := str(object(event.RequestJSON)["previous_response_id"])
	explicitParent := ""
	if hints != nil {
		explicitParent = hints.ParentExchangeID
		if hints.ConversationID != "" {
			match.branch = "conversation:" + hints.ConversationID
		}
	}
	bestScore := -1
	kind := ""
	offset := 0
	var candidates []contextCandidate
	for _, candidate := range b.candidates {
		explicit := explicitParent != "" && candidate.exchangeID == explicitParent || previousID != "" && candidate.responseID == previousID
		if !explicit && !temporalCandidate(candidate, event) {
			continue
		}
		if explicitParent != "" && candidate.exchangeID != explicitParent {
			continue
		}
		if previousID != "" && candidate.responseID != previousID {
			continue
		}
		if hints != nil && hints.ConversationID != "" && candidate.conversation != "" && candidate.conversation != hints.ConversationID {
			continue
		}
		if hints != nil && hints.AgentID != "" && candidate.agent != "" && candidate.agent != hints.AgentID {
			continue
		}
		score := 0
		candidateKind := "replacement"
		candidateOffset := 0
		if explicit {
			score = 1_000_000
			candidateKind = "restore"
		}
		prefix := commonPrefix(candidate.messages, messages)
		if prefix == len(candidate.messages) && prefix > 0 {
			score += 10000 + prefix
			candidateOffset = prefix
			candidateKind = "snapshot"
		} else if suffix := suffixMatch(candidate.messages, messages); suffix > 0 {
			score += 100 + suffix
			candidateOffset = suffix
			candidateKind = "pruning"
		}
		// Matching tool IDs provide explicit action/result evidence even if the
		// producer replaced all surrounding natural-language context.
		if score == 0 {
			for _, current := range messages {
				if current.resultID != "" {
					for _, prior := range candidate.messages {
						for _, call := range prior.tools {
							if call.ID == current.resultID {
								score++
								candidateKind = "replacement"
							}
						}
					}
				}
			}
		}
		if score == 0 {
			continue
		}
		if score > bestScore {
			bestScore = score
			candidates = []contextCandidate{candidate}
			kind = candidateKind
			offset = candidateOffset
		} else if score == bestScore {
			candidates = append(candidates, candidate)
		}
	}
	if len(candidates) > 0 {
		branches := map[string]bool{}
		for _, candidate := range candidates {
			branches[candidate.branch] = true
			match.parents = append(match.parents, candidate.record)
		}
		match.kind = kind
		match.offset = offset
		match.evidence = "structural_context_match"
		if explicitParent != "" || previousID != "" {
			match.evidence = "explicit_parent_reference"
		}
		if len(branches) == 1 {
			match.branch = candidates[len(candidates)-1].branch
			latest := b.candidates[len(b.candidates)-1]
			if match.branch != latest.branch && match.kind == "snapshot" {
				match.kind = "restore"
			}
			// Only the current timeline's exact append becomes additional interaction
			// steps. Earlier retained context is never repeated as new interactions.
			chosen := candidates[len(candidates)-1]
			match.append = chosen.record == latest.record && kind == "snapshot" && bestScore >= 10000
		} else {
			match.kind = "replacement"
			match.evidence = "multiple_parent_candidates"
			b.notice("ambiguous_parent", "Several causal parents remain possible; checkpoint input is exact and no parent is chosen.")
		}
	} else if explicitParent != "" || previousID != "" {
		b.notice("unknown_previous_response", "Referenced predecessor is absent; this captured request remains an exact standalone checkpoint.")
	}
	// An adopted summary is positive evidence, not a keyword-only guess. The
	// complete output must occur verbatim in a known continuation wrapper, and a
	// temporally prior call must have produced it. Short quoted answers do not count.
	for _, candidate := range b.candidates {
		if !temporalCandidate(candidate, event) || len(candidate.responseText) < 128 {
			continue
		}
		for _, m := range messages {
			text := textOf(m.value())
			lower := strings.ToLower(text)
			continued := strings.HasPrefix(lower, "this session is being continued from a previous conversation") || strings.HasPrefix(lower, "this conversation is being continued from a previous conversation")
			if continued && strings.Contains(text, candidate.responseText) {
				match.kind = "compaction"
				match.evidence = "continuation_wrapper_and_exact_summary_output"
				match.parents = appendUnique(match.parents, candidate.record)
				match.append = false
			}
		}
	}
	if match.kind == "snapshot" {
		// Even a structural append may prune thinking blocks or change tool schemas
		// and parameters. Every call still receives its exact checkpoint.
		last := b.candidates[len(b.candidates)-1]
		if match.append && len(messages) >= len(last.messages) && digestJSON(projected(last.messages)) != digestJSON(projected(messages[:len(last.messages)])) {
			match.kind = "pruning"
			match.evidence = "observed_context_fields_changed"
		}
		if last.contextID != contextID && digestJSON(b.inputContexts[last.contextID].(map[string]any)["configuration"]) != digestJSON(contextConfiguration(object(event.RequestJSON))) {
			match.kind = "configuration_change"
		}
	}
	return b.applyOperation(match, hints)
}
func appendUnique(values []int, value int) []int {
	for _, v := range values {
		if v == value {
			return values
		}
	}
	return append(values, value)
}
func (b *builder) applyOperation(match contextMatch, hints *observer.ContextMetadata) contextMatch {
	if hints != nil && hints.Operation != "" {
		match.kind = hints.Operation
		match.evidence = "producer_context_metadata"
		match.append = false
	}
	return match
}

func (b *builder) boundary(event observer.Event, messages []message, contextID string, match contextMatch) int {
	results := make([]Result, 0, len(messages))
	for _, m := range messages {
		description := projection(m)
		delete(description, "content")
		results = append(results, Result{Content: m.value(), Extra: map[string]any{"context_message": description}})
	}
	// An empty captured message list is still a legitimate checkpoint (e.g. an
	// opaque server-side continuation). The raw request keeps its reference.
	meta := b.provenance()
	meta["input_context_id"] = contextID
	meta["source_started_at_ms"] = event.StartedAtMS
	meta["source_completed_at_ms"] = event.CompletedAtMS
	if event.ContextMetadata != nil {
		meta["producer_context_metadata"] = event.ContextMetadata
	}
	meta["branch_id"] = match.branch
	meta["parent_source_record_candidates"] = match.parents
	meta["relationship_evidence"] = match.evidence
	meta["context_configuration"] = contextConfiguration(object(event.RequestJSON))
	meta["projection_contract"] = "ordered_context_messages_v1"
	meta["origin"] = "captured_request_checkpoint"
	step := Step{ID: len(b.document.Steps) + 1, Source: "system", Message: "Captured model input context", Observation: &Observation{Results: results}, Extra: map[string]any{"context_management": map[string]any{"type": match.kind, "boundary": "replace"}, Namespace: meta}}
	b.document.Steps = append(b.document.Steps, step)
	return step.ID
}
