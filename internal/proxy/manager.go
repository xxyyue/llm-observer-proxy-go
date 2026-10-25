// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/xxyyue/llm-observer-proxy-go/internal/catalog"
	"github.com/xxyyue/llm-observer-proxy-go/internal/observer"
	"github.com/xxyyue/llm-observer-proxy-go/internal/trajectory"
)

var runIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

type Instance struct {
	RunID            string
	Port             int
	BindHost         string
	BaseURL          string
	ConfigPath       string
	LogPath          string
	HistoryPath      string
	ConversationPath string
	State            string
	server           *http.Server
	listener         net.Listener
}

type Manager struct {
	mu           sync.RWMutex
	artifactRoot string
	catalog      *catalog.Catalog
	engine       Engine
	instances    map[string]*Instance
	archives     map[string]*Instance
}

func NewManager(artifactRoot string, modelCatalog *catalog.Catalog, engine Engine) (*Manager, error) {
	absolute, err := filepath.Abs(artifactRoot)
	if err != nil {
		return nil, err
	}
	return &Manager{
		artifactRoot: absolute,
		catalog:      modelCatalog,
		engine:       engine,
		instances:    make(map[string]*Instance),
		archives:     make(map[string]*Instance),
	}, nil
}

func ValidateRunID(runID string) error {
	if !runIDPattern.MatchString(runID) {
		return errors.New("run_id contains unsupported characters")
	}
	return nil
}

func (m *Manager) Create(runID, bindHost string, port int) (*Instance, error) {
	return m.CreateWithMetadata(runID, bindHost, port, trajectory.Metadata{})
}

func (m *Manager) CreateWithMetadata(runID, bindHost string, port int, metadata trajectory.Metadata) (*Instance, error) {
	if err := ValidateRunID(runID); err != nil {
		return nil, err
	}
	if bindHost == "" {
		bindHost = "127.0.0.1"
	}
	if port < 0 || port > 65535 {
		return nil, errors.New("port must be between 1 and 65535")
	}
	m.mu.Lock()
	if _, exists := m.instances[runID]; exists {
		m.mu.Unlock()
		return nil, fmt.Errorf("proxy already exists for run_id: %s", runID)
	}
	// Reserve the run ID while the listener and artifact paths are created.
	m.instances[runID] = nil
	m.mu.Unlock()
	cleanupReservation := true
	defer func() {
		if cleanupReservation {
			m.mu.Lock()
			delete(m.instances, runID)
			m.mu.Unlock()
		}
	}()

	root := m.runRoot(runID)
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	historyPath := filepath.Join(root, "events.jsonl")
	conversationPath := filepath.Join(root, "conversation_snapshot.json")
	configPath := filepath.Join(root, "config.json")
	logPath := filepath.Join(root, "bifrost.log")
	if err := os.WriteFile(historyPath, nil, 0o644); err != nil {
		return nil, err
	}
	_ = os.Remove(conversationPath)
	if err := os.WriteFile(logPath, nil, 0o644); err != nil {
		return nil, err
	}
	configData, _ := json.MarshalIndent(map[string]any{
		"backend":             "bifrost",
		"embedded":            true,
		"model_catalog_path":  m.catalog.Path,
		"trajectory_metadata": metadata,
	}, "", "  ")
	if err := os.WriteFile(configPath, append(configData, '\n'), 0o644); err != nil {
		return nil, err
	}

	listener, err := net.Listen("tcp", net.JoinHostPort(bindHost, fmt.Sprint(port)))
	if err != nil {
		return nil, err
	}
	actualPort := listener.Addr().(*net.TCPAddr).Port
	baseURL := fmt.Sprintf("http://%s:%d", bindHost, actualPort)
	recorder := observer.NewRecorder(runID, actualPort, historyPath, conversationPath, m.catalog.Pricing)
	dataPlane := NewDataPlane(runID, m.catalog.Settings.BaseURL, m.engine, recorder)
	server := &http.Server{Handler: dataPlane, ReadHeaderTimeout: 10 * time.Second}
	instance := &Instance{
		RunID:            runID,
		Port:             actualPort,
		BindHost:         bindHost,
		BaseURL:          baseURL,
		ConfigPath:       configPath,
		LogPath:          logPath,
		HistoryPath:      historyPath,
		ConversationPath: conversationPath,
		State:            "running",
		server:           server,
		listener:         listener,
	}
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(os.Stderr, "data plane %s failed: %v\n", runID, err)
		}
	}()
	m.mu.Lock()
	m.instances[runID] = instance
	delete(m.archives, runID)
	m.mu.Unlock()
	cleanupReservation = false
	return instance, nil
}

func (m *Manager) Stop(runID string) bool {
	m.mu.Lock()
	instance, exists := m.instances[runID]
	if !exists || instance == nil {
		m.mu.Unlock()
		return false
	}
	delete(m.instances, runID)
	instance.State = "stopped"
	m.archives[runID] = instance
	m.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = instance.server.Shutdown(ctx)
	return true
}

func (m *Manager) StopAll() {
	m.mu.RLock()
	runIDs := make([]string, 0, len(m.instances))
	for runID, instance := range m.instances {
		if instance != nil {
			runIDs = append(runIDs, runID)
		}
	}
	m.mu.RUnlock()
	for _, runID := range runIDs {
		m.Stop(runID)
	}
}

func (m *Manager) Close() {
	m.StopAll()
	m.engine.Close()
}

func (m *Manager) Get(runID string) (map[string]any, bool) {
	m.mu.RLock()
	instance := m.instances[runID]
	if instance == nil {
		instance = m.archives[runID]
	}
	m.mu.RUnlock()
	if instance == nil {
		return nil, false
	}
	return m.record(instance), true
}

func (m *Manager) List() []map[string]any {
	m.mu.RLock()
	instances := make([]*Instance, 0, len(m.instances)+len(m.archives))
	for _, instance := range m.instances {
		if instance != nil {
			instances = append(instances, instance)
		}
	}
	for _, instance := range m.archives {
		instances = append(instances, instance)
	}
	m.mu.RUnlock()
	sort.Slice(instances, func(i, j int) bool { return instances[i].Port < instances[j].Port })
	result := make([]map[string]any, 0, len(instances))
	for _, instance := range instances {
		result = append(result, m.record(instance))
	}
	return result
}

func (m *Manager) History(runID string) (map[string]any, bool, error) {
	instance, state, port, path, ok := m.resolveView(runID)
	_ = instance
	if !ok {
		return nil, false, nil
	}
	snapshot, err := observer.ReadHistory(runID, port, state, path, m.catalog.Pricing)
	return snapshot, true, err
}

func (m *Manager) Conversation(runID string) (map[string]any, bool, error) {
	_, state, port, path, ok := m.resolveView(runID)
	if !ok {
		return nil, false, nil
	}
	snapshot, err := observer.ReadConversation(runID, port, state, path, m.catalog.Pricing)
	return snapshot, true, err
}

func (m *Manager) resolveView(runID string) (*Instance, string, *int, string, bool) {
	if ValidateRunID(runID) != nil {
		return nil, "", nil, "", false
	}
	m.mu.RLock()
	instance := m.instances[runID]
	if instance == nil {
		instance = m.archives[runID]
	}
	m.mu.RUnlock()
	if instance != nil {
		port := instance.Port
		return instance, instance.State, &port, instance.HistoryPath, true
	}
	path := filepath.Join(m.runRoot(runID), "events.jsonl")
	if _, err := os.Stat(path); err == nil {
		return nil, "stopped", nil, path, true
	}
	return nil, "", nil, "", false
}

func (m *Manager) record(instance *Instance) map[string]any {
	historyCount := 0
	port := instance.Port
	if snapshot, err := observer.ReadHistory(instance.RunID, &port, instance.State, instance.HistoryPath, m.catalog.Pricing); err == nil {
		historyCount, _ = snapshot["count"].(int)
	}
	return map[string]any{
		"backend":                    "bifrost",
		"run_id":                     instance.RunID,
		"port":                       instance.Port,
		"bind_host":                  instance.BindHost,
		"base_url":                   instance.BaseURL,
		"openai_base_url":            instance.BaseURL + "/v1",
		"anthropic_base_url":         instance.BaseURL,
		"config_path":                instance.ConfigPath,
		"log_path":                   instance.LogPath,
		"history_path":               instance.HistoryPath,
		"conversation_snapshot_path": instance.ConversationPath,
		"history_count":              historyCount,
		"state":                      instance.State,
		"paths": map[string]any{
			"events":       instance.HistoryPath,
			"conversation": instance.ConversationPath,
			"config":       instance.ConfigPath,
			"log":          instance.LogPath,
		},
	}
}

func (m *Manager) runRoot(runID string) string {
	return filepath.Join(m.artifactRoot, "runs", runID, "platform", "llm")
}

// Trajectory exports the complete event log, including archived runs after restart.
func (m *Manager) Trajectory(runID string, options trajectory.Options) (*trajectory.Document, bool, error) {
	_, state, _, path, ok := m.resolveView(runID)
	if !ok {
		return nil, false, nil
	}
	options.RunID = runID
	options.State = state
	data, err := os.ReadFile(filepath.Join(m.runRoot(runID), "config.json"))
	if err != nil && !os.IsNotExist(err) {
		return nil, true, err
	}
	if err == nil {
		var config struct {
			Metadata trajectory.Metadata `json:"trajectory_metadata"`
		}
		if err := json.Unmarshal(data, &config); err != nil {
			return nil, true, fmt.Errorf("invalid run metadata")
		}
		options.Metadata = config.Metadata
	}
	document, err := trajectory.ExportFile(path, options)
	return document, true, err
}

// PublishTrajectoryAssets keeps generated resources scoped to the owning run.
func (m *Manager) PublishTrajectoryAssets(runID string, document *trajectory.Document) error {
	if err := ValidateRunID(runID); err != nil {
		return err
	}
	return trajectory.WriteAssets(filepath.Join(m.runRoot(runID), ".atif-assets"), document.Assets)
}
func (m *Manager) TrajectoryAsset(runID, key string) (trajectory.Asset, error) {
	if err := ValidateRunID(runID); err != nil {
		return trajectory.Asset{}, err
	}
	return trajectory.ReadAsset(filepath.Join(m.runRoot(runID), ".atif-assets"), key)
}
