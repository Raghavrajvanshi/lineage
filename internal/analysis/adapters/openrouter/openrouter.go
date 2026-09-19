// Package openrouter is the analysis adapter for OpenRouter, a gateway that
// routes to many upstream vendors. Its key works only at openrouter.ai, and
// content passes through OpenRouter before reaching the upstream vendor.
package openrouter

import (
	"context"

	"github.com/agentic-lineage/lineage/internal/analysis"
	"github.com/agentic-lineage/lineage/internal/analysis/adapters/chatcompletions"
)

type adapter struct{}

func init() { analysis.Register(adapter{}) }

func (adapter) Name() string            { return "openrouter" }
func (adapter) CredentialEnv() string   { return "OPENROUTER_API_KEY" }
func (adapter) DefaultEndpoint() string { return "https://openrouter.ai/api/v1/chat/completions" }

func (adapter) Send(ctx context.Context, req analysis.Request) (string, error) {
	return chatcompletions.Send(ctx, req)
}
