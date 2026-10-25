// SPDX-License-Identifier: Apache-2.0

package control

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/xxyyue/llm-observer-proxy-go/internal/proxy"
	"github.com/xxyyue/llm-observer-proxy-go/internal/trajectory"
)

type Server struct {
	manager *proxy.Manager
}

func NewServer(manager *proxy.Manager) *Server {
	return &Server{manager: manager}
}

func (s *Server) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.Method == http.MethodGet && request.URL.Path == "/healthz" {
		writeJSON(writer, http.StatusOK, map[string]any{"ok": true})
		return
	}
	if request.Method == http.MethodGet && request.URL.Path == "/api/proxies" {
		writeJSON(writer, http.StatusOK, map[string]any{"items": s.manager.List()})
		return
	}
	parts := strings.Split(strings.Trim(request.URL.Path, "/"), "/")
	if request.Method == http.MethodGet && len(parts) == 7 && parts[0] == "api" && parts[1] == "runs" && parts[3] == "trajectory" && parts[4] == "assets" {
		asset, err := s.manager.TrajectoryAsset(parts[2], strings.Join(parts[5:], "/"))
		if err != nil {
			http.NotFound(writer, request)
			return
		}
		writer.Header().Set("Content-Type", asset.MediaType)
		writer.Header().Set("Cache-Control", "no-store")
		writer.Header().Set("X-Content-Type-Options", "nosniff")
		writer.Write(asset.Data)
		return
	}
	runID, action, ok := parseRunPath(request.URL.Path)
	if !ok {
		writeJSON(writer, http.StatusNotFound, map[string]any{"error": "not found"})
		return
	}
	if err := proxy.ValidateRunID(runID); err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	switch {
	case action == "proxy" && request.Method == http.MethodPost:
		s.create(writer, request, runID)
	case action == "proxy" && request.Method == http.MethodGet:
		if record, found := s.manager.Get(runID); found {
			writeJSON(writer, http.StatusOK, record)
		} else {
			writeJSON(writer, http.StatusNotFound, map[string]any{"error": "Proxy not found for run_id " + runID})
		}
	case action == "proxy" && request.Method == http.MethodDelete:
		if s.manager.Stop(runID) {
			writeJSON(writer, http.StatusOK, map[string]any{"ok": true, "run_id": runID})
		} else {
			writeJSON(writer, http.StatusNotFound, map[string]any{"error": "Proxy not found for run_id " + runID})
		}
	case action == "history" && request.Method == http.MethodGet:
		s.history(writer, request, runID)
	case action == "trajectory" && request.Method == http.MethodGet:
		s.trajectory(writer, request, runID)
	case action == "conversation" && request.Method == http.MethodGet:
		s.conversation(writer, runID)
	default:
		writeJSON(writer, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
	}
}

func (s *Server) create(writer http.ResponseWriter, request *http.Request, runID string) {
	var payload map[string]any
	decoder := json.NewDecoder(io.LimitReader(request.Body, 1<<20))
	if err := decoder.Decode(&payload); err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]any{"error": "request body must contain valid JSON"})
		return
	}
	if payload == nil {
		writeJSON(writer, http.StatusBadRequest, map[string]any{"error": "request body must be a JSON object"})
		return
	}
	port, err := optionalPort(payload["port"])
	if err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	bindHost := "127.0.0.1"
	if value := payload["bind_host"]; value != nil {
		text, ok := value.(string)
		if !ok {
			writeJSON(writer, http.StatusBadRequest, map[string]any{"error": "bind_host must be a string"})
			return
		}
		if strings.TrimSpace(text) != "" {
			bindHost = strings.TrimSpace(text)
		}
	}
	var metadata trajectory.Metadata
	if value, exists := payload["trajectory_metadata"]; exists {
		if _, ok := value.(map[string]any); !ok {
			writeJSON(writer, http.StatusBadRequest, map[string]any{"error": "trajectory_metadata must be an object"})
			return
		}
		data, _ := json.Marshal(value)
		if err := json.Unmarshal(data, &metadata); err != nil {
			writeJSON(writer, http.StatusBadRequest, map[string]any{"error": "agent_name and agent_version must be strings"})
			return
		}
	}
	instance, err := s.manager.CreateWithMetadata(runID, bindHost, port, metadata)
	if err != nil {
		status := http.StatusBadRequest
		if strings.Contains(strings.ToLower(err.Error()), "already exists") {
			status = http.StatusConflict
		}
		writeJSON(writer, status, map[string]any{"error": err.Error()})
		return
	}
	record, _ := s.manager.Get(instance.RunID)
	writeJSON(writer, http.StatusCreated, record)
}

func (s *Server) history(writer http.ResponseWriter, request *http.Request, runID string) {
	snapshot, found, err := s.manager.History(runID)
	if err != nil {
		writeJSON(writer, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if !found {
		writeJSON(writer, http.StatusNotFound, map[string]any{"error": "Proxy not found for run_id " + runID})
		return
	}
	if raw := strings.TrimSpace(request.URL.Query().Get("limit")); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil {
			writeJSON(writer, http.StatusBadRequest, map[string]any{"error": "invalid limit"})
			return
		}
		if limit < 1 {
			limit = 1
		}
		if limit > 200 {
			limit = 200
		}
		// Re-marshal through []any so the concrete observer.Event slice can be limited without coupling packages.
		encoded, _ := json.Marshal(snapshot["items"])
		var generic []any
		_ = json.Unmarshal(encoded, &generic)
		if len(generic) > limit {
			generic = generic[len(generic)-limit:]
		}
		snapshot["items"] = generic
		snapshot["count"] = len(generic)
	}
	writeJSON(writer, http.StatusOK, snapshot)
}

func (s *Server) conversation(writer http.ResponseWriter, runID string) {
	snapshot, found, err := s.manager.Conversation(runID)
	if err != nil {
		writeJSON(writer, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if !found {
		writeJSON(writer, http.StatusNotFound, map[string]any{"error": "Proxy not found for run_id " + runID})
		return
	}
	writeJSON(writer, http.StatusOK, snapshot)
}

func parseRunPath(path string) (string, string, bool) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) != 4 || parts[0] != "api" || parts[1] != "runs" {
		return "", "", false
	}
	if parts[3] != "proxy" && parts[3] != "history" && parts[3] != "conversation" && parts[3] != "trajectory" {
		return "", "", false
	}
	return parts[2], parts[3], true
}

func optionalPort(value any) (int, error) {
	if value == nil {
		return 0, nil
	}
	var number float64
	switch typed := value.(type) {
	case float64:
		number = typed
	case string:
		parsed, err := strconv.Atoi(strings.TrimSpace(typed))
		if err != nil {
			return 0, errors.New("port must be an integer")
		}
		number = float64(parsed)
	default:
		return 0, errors.New("port must be an integer")
	}
	port := int(number)
	if port < 1 || port > 65535 {
		return 0, errors.New("port must be between 1 and 65535")
	}
	return port, nil
}

func writeJSON(writer http.ResponseWriter, status int, payload any) {
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(payload)
}

func (s *Server) trajectory(writer http.ResponseWriter, request *http.Request, runID string) {
	packaging := request.URL.Query().Get("package")
	if packaging != "" && packaging != "json" && packaging != "zip" {
		writeJSON(writer, 400, map[string]any{"error": "package must be json or zip"})
		return
	}
	options := trajectory.Options{ImageMediaType: request.URL.Query().Get("image_media_type"), Format: request.URL.Query().Get("format"), Version: request.URL.Query().Get("version")}
	if err := options.Validate(); err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if raw, present := request.URL.Query()["strict"]; present {
		if len(raw) != 1 || (raw[0] != "true" && raw[0] != "false") {
			writeJSON(writer, http.StatusBadRequest, map[string]any{"error": "strict must be true or false"})
			return
		}
		options.Strict = raw[0] == "true"
	}
	document, found, err := s.manager.Trajectory(runID, options)
	if err != nil {
		status := http.StatusInternalServerError
		var semantic *trajectory.SemanticError
		var empty *trajectory.EmptyError
		if errors.As(err, &semantic) || errors.As(err, &empty) {
			status = http.StatusUnprocessableEntity
		}
		payload := map[string]any{"error": err.Error()}
		if semantic != nil {
			payload["diagnostics"] = semantic.Diagnostics
		}
		writeJSON(writer, status, payload)
		return
	}
	if !found {
		writeJSON(writer, http.StatusNotFound, map[string]any{"error": "Run history not found"})
		return
	}
	if packaging == "zip" {
		var data bytes.Buffer
		if err := trajectory.WriteZIP(&data, document); err != nil {
			writeJSON(writer, 500, map[string]any{"error": "cannot package trajectory"})
			return
		}
		writer.Header().Set("Content-Type", "application/zip")
		writer.Header().Set("Content-Disposition", `attachment; filename="trajectory.atif.zip"`)
		writer.Header().Set("Cache-Control", "no-store")
		writer.Write(data.Bytes())
		return
	}
	if len(document.Assets) > 0 {
		if err := s.manager.PublishTrajectoryAssets(runID, document); err != nil {
			writeJSON(writer, 500, map[string]any{"error": "cannot publish trajectory resources"})
			return
		}
		document, err = document.WithAssetPaths(servingOrigin(request) + "/api/runs/" + runID + "/trajectory/assets")
		if err != nil {
			writeJSON(writer, 500, map[string]any{"error": "cannot map trajectory resources"})
			return
		}
	}
	writer.Header().Set("Content-Disposition", `attachment; filename="trajectory.atif.json"`)
	writer.Header().Set("Cache-Control", "no-store")
	writeJSON(writer, http.StatusOK, document)
}

func servingOrigin(request *http.Request) string {
	scheme := "http"
	if request.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + request.Host
}
