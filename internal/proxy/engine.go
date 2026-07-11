// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"

	"github.com/maximhq/bifrost/core/schemas"
)

type ForwardRequest struct {
	Provider schemas.ModelProvider
	Model    string
	Method   string
	Path     string
	Query    string
	Body     []byte
	Headers  map[string]string
}

type ForwardResponse struct {
	StatusCode int
	Headers    map[string]string
	Body       []byte
}

type StreamChunk struct {
	StatusCode int
	Headers    map[string]string
	Body       []byte
	Err        error
}

type Engine interface {
	Forward(context.Context, ForwardRequest) (*ForwardResponse, error)
	Stream(context.Context, ForwardRequest) (<-chan StreamChunk, error)
	Close()
}

type UpstreamError struct {
	StatusCode int
	Message    string
}

func (e *UpstreamError) Error() string {
	return e.Message
}
