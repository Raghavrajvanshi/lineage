// Package chatcompletions is the low-level request/response translation for
// the Chat Completions wire shape. It is a helper, not an adapter: openai and
// openrouter each wrap it in their own adapter (own credentials, endpoint,
// errors) because sharing a wire shape does not make two services equivalent.
package chatcompletions

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/agentic-lineage/lineage/internal/analysis"
)

// Send posts a system+user prompt with Bearer auth and returns the first
// choice's message text.
func Send(ctx context.Context, req analysis.Request) (string, error) {
	body, err := analysis.PostJSON(ctx, req, map[string]string{
		"authorization": "Bearer " + req.APIKey,
	}, map[string]any{
		"model": req.Model,
		"messages": []map[string]string{
			{"role": "system", "content": req.System},
			{"role": "user", "content": req.User},
		},
	})
	if err != nil {
		return "", err
	}
	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", fmt.Errorf("parse response: %w", err)
	}
	if len(parsed.Choices) == 0 {
		return "", nil
	}
	return parsed.Choices[0].Message.Content, nil
}
