// SPDX-License-Identifier: Apache-2.0

package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/maximhq/bifrost/core/schemas"
)

const defaultCatalog = `{
  "llm_observer_proxy": {
    "base_url": "https://api.openai.com",
    "api_key": "os.environ/OPENAI_API_KEY"
  },
  "models": [
    {"id": "gpt-5.5", "upstream_api": "openai"}
  ]
}`

type ProxySettings struct {
	BaseURL string `json:"base_url"`
	APIKey  string `json:"api_key"`
}

type Model struct {
	ID          string             `json:"id"`
	UpstreamAPI string             `json:"upstream_api"`
	Pricing     map[string]float64 `json:"pricing,omitempty"`
}

type rawCatalog struct {
	Observer ProxySettings `json:"llm_observer_proxy"`
	Legacy   ProxySettings `json:"llm_proxy"`
	Models   []Model       `json:"models"`
}

type Catalog struct {
	Path     string
	Settings ProxySettings
	Models   []Model
}

func Load(path string) (*Catalog, error) {
	var data []byte
	var resolvedPath string
	if strings.TrimSpace(path) == "" {
		candidate := filepath.Join("config", "models.json")
		if _, err := os.Stat(candidate); err == nil {
			path = candidate
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	if strings.TrimSpace(path) == "" {
		data = []byte(defaultCatalog)
		resolvedPath = "embedded:config/models.json"
	} else {
		absolute, err := filepath.Abs(path)
		if err != nil {
			return nil, fmt.Errorf("resolve model catalog: %w", err)
		}
		data, err = os.ReadFile(absolute)
		if err != nil {
			return nil, fmt.Errorf("read model catalog: %w", err)
		}
		resolvedPath = absolute
	}

	var raw rawCatalog
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse model catalog: %w", err)
	}
	settings := raw.Observer
	if strings.TrimSpace(settings.BaseURL) == "" {
		settings = raw.Legacy
	}
	settings.BaseURL = strings.TrimRight(strings.TrimSpace(settings.BaseURL), "/")
	settings.BaseURL = strings.TrimSuffix(settings.BaseURL, "/v1")
	if settings.BaseURL == "" {
		return nil, errors.New("model catalog requires llm_observer_proxy.base_url")
	}
	settings.APIKey = resolveSecret(settings.APIKey)
	if len(raw.Models) == 0 {
		return nil, errors.New("model catalog requires at least one model")
	}

	seen := make(map[string]struct{}, len(raw.Models))
	models := make([]Model, 0, len(raw.Models))
	for _, model := range raw.Models {
		model.ID = strings.TrimSpace(model.ID)
		model.UpstreamAPI = strings.ToLower(strings.TrimSpace(model.UpstreamAPI))
		if model.ID == "" {
			return nil, errors.New("model id must not be empty")
		}
		if model.UpstreamAPI == "" {
			model.UpstreamAPI = "openai"
		}
		if model.UpstreamAPI != "openai" && model.UpstreamAPI != "anthropic" {
			return nil, fmt.Errorf("model %q has unsupported upstream_api %q", model.ID, model.UpstreamAPI)
		}
		key := strings.ToLower(model.ID)
		if _, ok := seen[key]; ok {
			return nil, fmt.Errorf("duplicate model id %q", model.ID)
		}
		seen[key] = struct{}{}
		models = append(models, model)
	}
	return &Catalog{Path: resolvedPath, Settings: settings, Models: models}, nil
}

func resolveSecret(value string) string {
	value = strings.TrimSpace(value)
	for _, prefix := range []string{"os.environ/", "env."} {
		if strings.HasPrefix(value, prefix) {
			return os.Getenv(strings.TrimPrefix(value, prefix))
		}
	}
	if strings.HasPrefix(value, "${") && strings.HasSuffix(value, "}") {
		return os.Getenv(strings.TrimSuffix(strings.TrimPrefix(value, "${"), "}"))
	}
	return value
}

func (c *Catalog) Pricing(model string) map[string]float64 {
	for _, item := range c.Models {
		if strings.EqualFold(item.ID, model) {
			return item.Pricing
		}
	}
	return nil
}

type Account struct {
	providers []schemas.ModelProvider
	keys      map[schemas.ModelProvider][]schemas.Key
	configs   map[schemas.ModelProvider]*schemas.ProviderConfig
}

func NewAccount(c *Catalog) *Account {
	models := make([]string, 0, len(c.Models))
	for _, model := range c.Models {
		models = append(models, model.ID)
	}
	sort.Strings(models)

	account := &Account{
		keys:    make(map[schemas.ModelProvider][]schemas.Key),
		configs: make(map[schemas.ModelProvider]*schemas.ProviderConfig),
	}
	for _, provider := range []schemas.ModelProvider{schemas.OpenAI, schemas.Anthropic} {
		account.providers = append(account.providers, provider)
		account.keys[provider] = []schemas.Key{{
			ID:     "catalog-" + string(provider),
			Name:   "model-catalog",
			Value:  *schemas.NewSecretVar(c.Settings.APIKey),
			Models: schemas.WhiteList(append([]string(nil), models...)),
			Weight: 1,
		}}
		account.configs[provider] = &schemas.ProviderConfig{
			NetworkConfig: schemas.NetworkConfig{
				BaseURL:                    c.Settings.BaseURL,
				AllowPrivateNetwork:        true,
				InsecureSkipVerify:         false,
				StreamIdleTimeoutInSeconds: 120,
			},
			ConcurrencyAndBufferSize: schemas.ConcurrencyAndBufferSize{
				Concurrency: 256,
				BufferSize:  1024,
			},
		}
	}
	return account
}

func (a *Account) GetConfiguredProviders() ([]schemas.ModelProvider, error) {
	return append([]schemas.ModelProvider(nil), a.providers...), nil
}

func (a *Account) GetKeysForProvider(_ context.Context, provider schemas.ModelProvider) ([]schemas.Key, error) {
	keys, ok := a.keys[provider]
	if !ok {
		return nil, fmt.Errorf("provider %s is not configured", provider)
	}
	return append([]schemas.Key(nil), keys...), nil
}

func (a *Account) GetConfigForProvider(provider schemas.ModelProvider) (*schemas.ProviderConfig, error) {
	config, ok := a.configs[provider]
	if !ok {
		return nil, fmt.Errorf("provider %s is not configured", provider)
	}
	copy := *config
	return &copy, nil
}
