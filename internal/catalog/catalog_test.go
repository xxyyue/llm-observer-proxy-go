// SPDX-License-Identifier: Apache-2.0

package catalog

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
)

func TestLoadCatalogAndBuildAccount(t *testing.T) {
	t.Setenv("TEST_PROXY_KEY", "secret-value")
	path := filepath.Join(t.TempDir(), "models.json")
	data := []byte(`{
  "llm_proxy": {
    "base_url": "http://localhost:8080/",
    "api_key": "os.environ/TEST_PROXY_KEY",
    "network_config": {"max_retries": 3, "retry_backoff_initial": "1s", "retry_backoff_max": "4s"}
  },
  "models": [
    {"id": "gpt-5.5", "upstream_api": "openai"},
    {"id": "claude-test", "upstream_api": "anthropic"}
  ]
}`)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Settings.BaseURL != "http://localhost:8080" {
		t.Fatalf("unexpected base URL: %s", loaded.Settings.BaseURL)
	}
	if loaded.Settings.APIKey != "secret-value" {
		t.Fatal("environment-backed API key was not resolved")
	}
	providers, err := NewAccount(loaded).GetConfiguredProviders()
	if err != nil {
		t.Fatal(err)
	}
	if len(providers) != 2 || providers[0] != schemas.OpenAI || providers[1] != schemas.Anthropic {
		t.Fatalf("unexpected providers: %#v", providers)
	}
	account := NewAccount(loaded)
	for _, provider := range providers {
		providerConfig, err := account.GetConfigForProvider(provider)
		if err != nil {
			t.Fatal(err)
		}
		if providerConfig.NetworkConfig.MaxRetries != 3 {
			t.Fatalf("provider %s retries = %d, want 3", provider, providerConfig.NetworkConfig.MaxRetries)
		}
		keys, err := account.GetKeysForProvider(t.Context(), provider)
		if err != nil {
			t.Fatal(err)
		}
		if !keys[0].Models.IsAllowed("gpt-5.5") || !keys[0].Models.IsAllowed("claude-test") {
			t.Fatalf("provider %s does not accept every catalog model: %#v", provider, keys[0].Models)
		}
	}
}

func TestAccountDefaultsToSixRetries(t *testing.T) {
	catalog := &Catalog{Settings: ProxySettings{BaseURL: "https://example.com", APIKey: "key"}}
	account := NewAccount(catalog)
	config, err := account.GetConfigForProvider(schemas.OpenAI)
	if err != nil {
		t.Fatal(err)
	}
	if config.NetworkConfig.MaxRetries != 6 {
		t.Fatalf("default retries = %d, want 6", config.NetworkConfig.MaxRetries)
	}
}

func TestLoadRejectsUnknownProvider(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models.json")
	if err := os.WriteFile(path, []byte(`{
  "llm_observer_proxy": {"base_url": "https://example.com", "api_key": "key"},
  "models": [{"id": "model", "upstream_api": "unknown"}]
}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected unsupported provider error")
	}
}
