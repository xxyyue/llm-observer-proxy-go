// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/maximhq/bifrost/core/providers/anthropic"
	"github.com/maximhq/bifrost/core/providers/openai"
	"github.com/maximhq/bifrost/core/schemas"

	"github.com/xxyyue/llm-observer-proxy-go/internal/catalog"
)

type passthroughProvider interface {
	Passthrough(*schemas.BifrostContext, schemas.Key, *schemas.BifrostPassthroughRequest) (*schemas.BifrostPassthroughResponse, *schemas.BifrostError)
	PassthroughStream(*schemas.BifrostContext, schemas.PostHookRunner, func(context.Context), schemas.Key, *schemas.BifrostPassthroughRequest) (chan *schemas.BifrostStreamChunk, *schemas.BifrostError)
}

type NativeEngine struct {
	providers map[schemas.ModelProvider]passthroughProvider
	keys      map[schemas.ModelProvider][]schemas.Key
}

func NewNativeEngine(_ context.Context, modelCatalog *catalog.Catalog, logLevel string) (*NativeEngine, error) {
	logger := newProviderLogger(logLevel)
	account := catalog.NewAccount(modelCatalog)
	configured, err := account.GetConfiguredProviders()
	if err != nil {
		return nil, err
	}
	engine := &NativeEngine{
		providers: make(map[schemas.ModelProvider]passthroughProvider),
		keys:      make(map[schemas.ModelProvider][]schemas.Key),
	}
	for _, provider := range configured {
		config, err := account.GetConfigForProvider(provider)
		if err != nil {
			return nil, err
		}
		keys, err := account.GetKeysForProvider(context.Background(), provider)
		if err != nil {
			return nil, err
		}
		engine.keys[provider] = keys
		switch provider {
		case schemas.OpenAI:
			engine.providers[provider] = openai.NewOpenAIProvider(config, logger)
		case schemas.Anthropic:
			engine.providers[provider] = anthropic.NewAnthropicProvider(config, logger)
		default:
			return nil, fmt.Errorf("unsupported native provider %s", provider)
		}
	}
	return engine, nil
}

func (e *NativeEngine) Forward(ctx context.Context, request ForwardRequest) (*ForwardResponse, error) {
	provider, key, err := e.resolve(request.Provider, request.Model)
	if err != nil {
		return nil, err
	}
	bfContext := schemas.NewBifrostContext(ctx, schemas.NoDeadline)
	response, bfError := provider.Passthrough(bfContext, key, toBifrostRequest(request))
	if bfError != nil {
		return nil, fromBifrostError(bfError)
	}
	return &ForwardResponse{StatusCode: response.StatusCode, Headers: response.Headers, Body: response.Body}, nil
}

func (e *NativeEngine) Stream(ctx context.Context, request ForwardRequest) (<-chan StreamChunk, error) {
	provider, key, err := e.resolve(request.Provider, request.Model)
	if err != nil {
		return nil, err
	}
	bfContext := schemas.NewBifrostContext(ctx, schemas.NoDeadline)
	identityPostHook := func(_ *schemas.BifrostContext, response *schemas.BifrostResponse, bfError *schemas.BifrostError) (*schemas.BifrostResponse, *schemas.BifrostError) {
		return response, bfError
	}
	input, bfError := provider.PassthroughStream(bfContext, identityPostHook, nil, key, toBifrostRequest(request))
	if bfError != nil {
		return nil, fromBifrostError(bfError)
	}
	output := make(chan StreamChunk, 32)
	go func() {
		defer close(output)
		for chunk := range input {
			if chunk == nil {
				continue
			}
			if chunk.BifrostError != nil {
				output <- StreamChunk{Err: fromBifrostError(chunk.BifrostError)}
				continue
			}
			if response := chunk.BifrostPassthroughResponse; response != nil {
				output <- StreamChunk{StatusCode: response.StatusCode, Headers: response.Headers, Body: append([]byte(nil), response.Body...)}
			}
		}
	}()
	return output, nil
}

func (e *NativeEngine) resolve(providerKey schemas.ModelProvider, model string) (passthroughProvider, schemas.Key, error) {
	provider := e.providers[providerKey]
	if provider == nil {
		return nil, schemas.Key{}, &UpstreamError{StatusCode: 400, Message: fmt.Sprintf("provider %s is not configured", providerKey)}
	}
	for _, key := range e.keys[providerKey] {
		if model == "" || key.Models.IsAllowed(model) {
			return provider, key, nil
		}
	}
	return nil, schemas.Key{}, &UpstreamError{StatusCode: 400, Message: fmt.Sprintf("model %q is not configured for provider %s", model, providerKey)}
}

func (e *NativeEngine) Close() {}

func toBifrostRequest(request ForwardRequest) *schemas.BifrostPassthroughRequest {
	return &schemas.BifrostPassthroughRequest{
		Provider:    request.Provider,
		Model:       request.Model,
		Method:      request.Method,
		Path:        request.Path,
		RawQuery:    request.Query,
		Body:        request.Body,
		SafeHeaders: request.Headers,
	}
}

func fromBifrostError(err *schemas.BifrostError) error {
	status := 502
	if err.StatusCode != nil && *err.StatusCode >= 100 && *err.StatusCode <= 599 {
		status = *err.StatusCode
	}
	message := err.GetErrorString()
	if strings.TrimSpace(message) == "" {
		message = "Bifrost request failed"
	}
	return &UpstreamError{StatusCode: status, Message: message}
}

type providerLogger struct {
	level schemas.LogLevel
}

func newProviderLogger(level string) *providerLogger {
	parsed := schemas.LogLevel(strings.ToLower(strings.TrimSpace(level)))
	switch parsed {
	case schemas.LogLevelDebug, schemas.LogLevelInfo, schemas.LogLevelWarn, schemas.LogLevelError:
	default:
		parsed = schemas.LogLevelInfo
	}
	return &providerLogger{level: parsed}
}

func (l *providerLogger) Debug(message string, arguments ...any) {
	if l.enabled(schemas.LogLevelDebug) {
		log.Printf("DEBUG "+message, arguments...)
	}
}

func (l *providerLogger) Info(message string, arguments ...any) {
	if l.enabled(schemas.LogLevelInfo) {
		log.Printf("INFO "+message, arguments...)
	}
}

func (l *providerLogger) Warn(message string, arguments ...any) {
	if l.enabled(schemas.LogLevelWarn) {
		log.Printf("WARN "+message, arguments...)
	}
}

func (l *providerLogger) Error(message string, arguments ...any) {
	if l.enabled(schemas.LogLevelError) {
		log.Printf("ERROR "+message, arguments...)
	}
}

func (l *providerLogger) Fatal(message string, arguments ...any) {
	log.Printf("FATAL "+message, arguments...)
}

func (l *providerLogger) SetLevel(level schemas.LogLevel) { l.level = level }

func (l *providerLogger) SetOutputType(schemas.LoggerOutputType) {}

func (l *providerLogger) LogHTTPRequest(schemas.LogLevel, string) schemas.LogEventBuilder {
	return schemas.NoopLogEvent
}

func (l *providerLogger) enabled(level schemas.LogLevel) bool {
	weights := map[schemas.LogLevel]int{
		schemas.LogLevelDebug: 0,
		schemas.LogLevelInfo:  1,
		schemas.LogLevelWarn:  2,
		schemas.LogLevelError: 3,
	}
	return weights[level] >= weights[l.level]
}
