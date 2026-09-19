package adapters_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/agentic-lineage/lineage/internal/analysis"
	_ "github.com/agentic-lineage/lineage/internal/analysis/adapters/all"
)

type rt func(*http.Request) (*http.Response, error)

func (f rt) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestAdaptersWireFormat(t *testing.T) {
	tests := []struct {
		name, credEnv, endpoint, authHeader, authValue, reply, wantText string
	}{
		{"anthropic", "ANTHROPIC_API_KEY", "https://api.anthropic.com/v1/messages", "x-api-key", "k",
			`{"content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]}`, "ab"},
		{"openai", "OPENAI_API_KEY", "https://api.openai.com/v1/chat/completions", "Authorization", "Bearer k",
			`{"choices":[{"message":{"content":"hi"}}]}`, "hi"},
		{"openrouter", "OPENROUTER_API_KEY", "https://openrouter.ai/api/v1/chat/completions", "Authorization", "Bearer k",
			`{"choices":[{"message":{"content":"hi"}}]}`, "hi"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, ok := analysis.LookupAdapter(tt.name)
			if !ok {
				t.Fatalf("adapter %q not registered", tt.name)
			}
			if a.CredentialEnv() != tt.credEnv || a.DefaultEndpoint() != tt.endpoint {
				t.Fatalf("env/endpoint = %s %s, want %s %s", a.CredentialEnv(), a.DefaultEndpoint(), tt.credEnv, tt.endpoint)
			}
			var got *http.Request
			client := &http.Client{Transport: rt(func(r *http.Request) (*http.Response, error) {
				got = r
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(tt.reply)), Header: http.Header{}}, nil
			})}
			text, err := a.Send(context.Background(), analysis.Request{Endpoint: a.DefaultEndpoint(), Model: "m", APIKey: "k", System: "s", User: "u", Client: client})
			if err != nil || text != tt.wantText {
				t.Fatalf("Send = %q, %v; want %q", text, err, tt.wantText)
			}
			if got.URL.String() != tt.endpoint || got.Header.Get(tt.authHeader) != tt.authValue {
				t.Fatalf("request = %s %s=%q", got.URL, tt.authHeader, got.Header.Get(tt.authHeader))
			}
		})
	}
}
