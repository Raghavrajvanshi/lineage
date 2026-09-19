// Package anthropic is the analysis adapter for the Anthropic Messages API.
package anthropic

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/agentic-lineage/lineage/internal/analysis"
)

const (
	endpoint   = "https://api.anthropic.com/v1/messages"
	apiVersion = "2023-06-01"
	maxTokens  = 8192
)

type adapter struct{}

func init() { analysis.Register(adapter{}) }

func (adapter) Name() string            { return "anthropic" }
func (adapter) CredentialEnv() string   { return "ANTHROPIC_API_KEY" }
func (adapter) DefaultEndpoint() string { return endpoint }

func (adapter) Send(ctx context.Context, req analysis.Request) (string, error) {
	body, err := analysis.PostJSON(ctx, req, map[string]string{
		"x-api-key":         req.APIKey,
		"anthropic-version": apiVersion,
	}, map[string]any{
		"model":      req.Model,
		"max_tokens": maxTokens,
		"system":     req.System,
		"messages":   []map[string]string{{"role": "user", "content": req.User}},
	})
	if err != nil {
		return "", err
	}
	var parsed struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", fmt.Errorf("parse response: %w", err)
	}
	// One response can be split across several text blocks; concatenate them
	// so a JSON document spanning blocks is reassembled before parsing.
	var text strings.Builder
	for _, b := range parsed.Content {
		if b.Type == "text" {
			text.WriteString(b.Text)
		}
	}
	return text.String(), nil
}
