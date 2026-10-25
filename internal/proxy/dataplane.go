// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/maximhq/bifrost/core/schemas"

	"github.com/xxyyue/llm-observer-proxy-go/internal/observer"
)

const maxRequestBody = 32 << 20

type DataPlane struct {
	runID       string
	upstreamURL string
	engine      Engine
	recorder    *observer.Recorder
}

func NewDataPlane(runID, upstreamURL string, engine Engine, recorder *observer.Recorder) *DataPlane {
	return &DataPlane{runID: runID, upstreamURL: strings.TrimRight(upstreamURL, "/"), engine: engine, recorder: recorder}
}

func (p *DataPlane) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.URL.Path == "/healthz" {
		writeJSON(writer, http.StatusOK, map[string]any{"ok": true})
		return
	}
	if request.URL.Path == "/" && (request.Method == http.MethodGet || request.Method == http.MethodHead) {
		writer.Header().Set("Content-Type", "application/json; charset=utf-8")
		writer.WriteHeader(http.StatusOK)
		if request.Method == http.MethodGet {
			_ = json.NewEncoder(writer).Encode(map[string]any{"ok": true, "service": "llm-observer-proxy"})
		}
		return
	}
	started := time.Now()
	body, err := io.ReadAll(http.MaxBytesReader(writer, request.Body, maxRequestBody))
	if err != nil {
		writeJSON(writer, http.StatusRequestEntityTooLarge, map[string]any{"error": "request body exceeds 32 MiB"})
		return
	}
	var requestJSON any
	if len(bytes.TrimSpace(body)) > 0 && decodeRecordedJSON(body, &requestJSON) != nil {
		requestJSON = nil
	}
	requestMap, _ := requestJSON.(map[string]any)
	stream, _ := requestMap["stream"].(bool)
	provider := schemas.OpenAI
	if request.URL.Path == "/v1/messages" || strings.HasPrefix(request.URL.Path, "/v1/messages/") ||
		(request.URL.Path == "/v1/models" && request.Header.Get("anthropic-version") != "") {
		provider = schemas.Anthropic
	}
	forward := ForwardRequest{
		Provider: provider,
		Model:    stringFromMap(requestMap, "model"),
		Method:   request.Method,
		Path:     request.URL.Path,
		Query:    request.URL.RawQuery,
		Body:     body,
		Headers:  safeHeaders(request.Header),
	}

	id := exchangeID()
	writer.Header().Set("X-LLM-Observer-Exchange-ID", id)
	contextMetadata, metadataError := captureContextMetadata(request.Header)
	if metadataError != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]any{"error": metadataError.Error()})
		return
	}
	status := 0
	responseHeaders := map[string]string{}
	var responseBody bytes.Buffer
	var forwardError error
	if stream {
		status, responseHeaders, forwardError = p.forwardStream(writer, request, forward, &responseBody)
	} else {
		status, responseHeaders, forwardError = p.forwardUnary(writer, request, forward, &responseBody)
	}
	completed := time.Now()
	if status == 0 {
		status = statusFromError(forwardError)
	}
	requestBody := nullableString(body)
	responseBodyText := nullableString(responseBody.Bytes())
	var responseJSON any
	if responseBody.Len() > 0 && decodeRecordedJSON(responseBody.Bytes(), &responseJSON) != nil {
		responseJSON = nil
	}
	event := &observer.Event{
		ExchangeID:          id,
		ContextMetadata:     contextMetadata,
		RunID:               p.runID,
		StartedAtMS:         started.UnixMilli(),
		CompletedAtMS:       completed.UnixMilli(),
		Method:              request.Method,
		Path:                request.URL.Path,
		QueryString:         request.URL.RawQuery,
		UpstreamURL:         p.upstreamURL + request.URL.RequestURI(),
		RequestHeaders:      redactedHeaders(request.Header),
		RequestBody:         requestBody,
		RequestBodyEncoding: "text",
		RequestJSON:         requestJSON,
		ResponseStatus:      &status,
		ResponseHeaders:     stringHeaders(responseHeaders),
		ResponseBody:        responseBodyText,
		ResponseEncoding:    "text",
		ResponseJSON:        responseJSON,
		Stream:              stream,
	}
	if forwardError != nil {
		message := forwardError.Error()
		event.Error = &message
	}
	if err := p.recorder.Record(event); err != nil {
		log.Printf("record LLM exchange for run %s: %v", p.runID, err)
	}
}

func (p *DataPlane) forwardUnary(writer http.ResponseWriter, request *http.Request, forward ForwardRequest, body *bytes.Buffer) (int, map[string]string, error) {
	response, err := p.engine.Forward(request.Context(), forward)
	if err != nil {
		status := statusFromError(err)
		payload, _ := json.Marshal(map[string]any{"error": map[string]any{"message": err.Error(), "type": "upstream_error"}})
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(status)
		_, _ = writer.Write(payload)
		_, _ = body.Write(payload)
		return status, map[string]string{"Content-Type": "application/json"}, err
	}
	copyResponseHeaders(writer.Header(), response.Headers)
	writer.WriteHeader(response.StatusCode)
	_, writeErr := writer.Write(response.Body)
	_, _ = body.Write(response.Body)
	return response.StatusCode, response.Headers, writeErr
}

func (p *DataPlane) forwardStream(writer http.ResponseWriter, request *http.Request, forward ForwardRequest, body *bytes.Buffer) (int, map[string]string, error) {
	chunks, err := p.engine.Stream(request.Context(), forward)
	if err != nil {
		status := statusFromError(err)
		payload, _ := json.Marshal(map[string]any{"error": map[string]any{"message": err.Error(), "type": "upstream_error"}})
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(status)
		_, _ = writer.Write(payload)
		_, _ = body.Write(payload)
		return status, map[string]string{"Content-Type": "application/json"}, err
	}
	status := 0
	headers := map[string]string{}
	flusher, _ := writer.(http.Flusher)
	var streamError error
	for chunk := range chunks {
		if chunk.Err != nil {
			streamError = chunk.Err
			if status == 0 {
				status = statusFromError(chunk.Err)
				payload, _ := json.Marshal(map[string]any{"error": map[string]any{"message": chunk.Err.Error(), "type": "upstream_error"}})
				writer.Header().Set("Content-Type", "application/json")
				writer.WriteHeader(status)
				_, _ = writer.Write(payload)
				_, _ = body.Write(payload)
			}
			continue
		}
		if status == 0 {
			status = chunk.StatusCode
			if status == 0 {
				status = http.StatusOK
			}
			headers = chunk.Headers
			copyResponseHeaders(writer.Header(), headers)
			writer.WriteHeader(status)
		}
		if len(chunk.Body) > 0 {
			if _, err := writer.Write(chunk.Body); err != nil {
				streamError = err
				break
			}
			_, _ = body.Write(chunk.Body)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}
	if status == 0 {
		status = statusFromError(streamError)
		writer.WriteHeader(status)
	}
	return status, headers, streamError
}

func safeHeaders(headers http.Header) map[string]string {
	result := make(map[string]string)
	for key, values := range headers {
		if strings.HasPrefix(strings.ToLower(key), "x-llm-observer-") || isHopByHop(key) || strings.EqualFold(key, "Authorization") || strings.EqualFold(key, "x-api-key") || strings.EqualFold(key, "Content-Length") {
			continue
		}
		if len(values) > 0 {
			result[key] = values[0]
		}
	}
	return result
}

func redactedHeaders(headers http.Header) map[string]any {
	result := make(map[string]any, len(headers))
	for key, values := range headers {
		if strings.EqualFold(key, "Authorization") || strings.EqualFold(key, "x-api-key") {
			result[key] = "[REDACTED]"
		} else {
			result[key] = append([]string(nil), values...)
		}
	}
	return result
}

func copyResponseHeaders(destination http.Header, source map[string]string) {
	for key, value := range source {
		if !strings.HasPrefix(strings.ToLower(key), "x-llm-observer-") && !isHopByHop(key) && !strings.EqualFold(key, "Content-Length") {
			destination.Set(key, value)
		}
	}
}

func isHopByHop(key string) bool {
	switch strings.ToLower(key) {
	case "connection", "keep-alive", "proxy-authenticate", "proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade":
		return true
	default:
		return false
	}
}

func statusFromError(err error) int {
	if err == nil {
		return http.StatusOK
	}
	var upstream *UpstreamError
	if errors.As(err, &upstream) && upstream.StatusCode >= 100 {
		return upstream.StatusCode
	}
	return http.StatusBadGateway
}

func nullableString(data []byte) *string {
	if len(data) == 0 {
		return nil
	}
	value := string(data)
	return &value
}

func stringFromMap(values map[string]any, key string) string {
	if values == nil {
		return ""
	}
	value, _ := values[key].(string)
	return value
}

func stringHeaders(values map[string]string) map[string]any {
	result := make(map[string]any, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}

func exchangeID() string {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return fmt.Sprintf("exchange-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buffer)
}

func writeJSON(writer http.ResponseWriter, status int, payload any) {
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(payload)
}

func decodeRecordedJSON(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return fmt.Errorf("expected one JSON value")
	}
	return nil
}

func captureContextMetadata(headers http.Header) (*observer.ContextMetadata, error) {
	keys := []string{"Conversation-ID", "Agent-ID", "Parent-Exchange-ID", "Context-Operation"}
	values := make([]string, len(keys))
	present := false
	for i, key := range keys {
		raw := headers.Values("X-LLM-Observer-" + key)
		if len(raw) > 1 {
			return nil, fmt.Errorf("observer metadata header must occur once")
		}
		value := headers.Get("X-LLM-Observer-" + key)
		if len(value) > 256 || strings.ContainsAny(value, "\r\n\x00") {
			return nil, fmt.Errorf("observer metadata header is invalid or exceeds 256 bytes")
		}
		values[i] = value
		present = present || value != ""
	}
	if !present {
		return nil, nil
	}
	switch values[3] {
	case "", "snapshot", "compaction", "pruning", "replacement", "restore", "injection":
	default:
		return nil, fmt.Errorf("unsupported observer context operation")
	}
	return &observer.ContextMetadata{ConversationID: values[0], AgentID: values[1], ParentExchangeID: values[2], Operation: values[3]}, nil
}
