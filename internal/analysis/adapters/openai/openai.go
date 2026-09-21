// Package openai is the analysis adapter for the OpenAI Chat Completions API.
package openai

import (
	"context"

	"github.com/agentic-lineage/lineage/internal/analysis"
	"github.com/agentic-lineage/lineage/internal/analysis/adapters/chatcompletions"
)

type adapter struct{}

func init() { analysis.Register(adapter{}) }

func (adapter) Name() string            { return "openai" }
func (adapter) CredentialEnv() string   { return "OPENAI_API_KEY" }
func (adapter) DefaultEndpoint() string { return "https://api.openai.com/v1/chat/completions" }

func (adapter) Send(ctx context.Context, req analysis.Request) (string, error) {
	return chatcompletions.Send(ctx, req)
}
