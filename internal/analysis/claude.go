package analysis

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/agentic-lineage/lineage/internal/inventory"
)

const (
	claudeAPIURL      = "https://api.anthropic.com/v1/messages"
	claudeModel       = "claude-opus-5"
	claudeAPIVersion  = "2023-06-01"
	claudeMaxTokens   = 8192
	claudeAPIKeyEnv   = "ANTHROPIC_API_KEY"
	claudeContentType = "application/json"

	// maxResponseBytes bounds how much of the HTTP response body is ever
	// read, success or error - a misbehaving endpoint or proxy otherwise
	// has no limit on how much memory a single Analyze call can force this
	// process to allocate.
	maxResponseBytes = 10 << 20 // 10MB
	// maxErrorBodyBytes further bounds how much of a non-2xx response body
	// is embedded in the returned error - maxResponseBytes alone still
	// permits echoing megabytes of attacker- or misconfiguration-controlled
	// text into a log or terminal.
	maxErrorBodyBytes = 2 << 10 // 2KB

	// maxSourceExcerptBytes caps how much of any single file's content is
	// included per source, so the prompt stays bounded regardless of one
	// unusually large file in the workspace.
	maxSourceExcerptBytes = 8 << 10 // 8KB
	// maxSourceFileBytes skips pulling in a file's content at all once it's
	// this large - such files are unlikely to be prose or scripts a
	// semantic pass needs verbatim, and reading every large binary in a
	// workspace into the prompt would be wasted cost for no signal.
	maxSourceFileBytes = 256 << 10 // 256KB
)

// ClaudeProvider calls the Anthropic Messages API directly over HTTP.
// Deliberately net/http + encoding/json rather than anthropic-sdk-go: this
// needs exactly one request/response shape — a single non-streaming call —
// which the standard library covers completely, without vendoring a large
// SDK and its transitive dependencies (this repo vendors its one existing
// dependency, gopkg.in/yaml.v3, explicitly) for one call site.
type ClaudeProvider struct {
	// APIKey overrides the ANTHROPIC_API_KEY environment variable when set.
	APIKey string
	// Client overrides http.DefaultClient when set, e.g. for a test double.
	Client *http.Client
}

// analysisSystemPrompt constrains the model to emit exactly one
// model.BehavioralModel as JSON, grounded strictly in the inventory and
// source excerpts given in the user message. No tool use, file access, or
// script execution is granted by this call — it can only read the evidence
// it's given and respond with text — matching #104's non-goals.
const analysisSystemPrompt = `You are analyzing a Lineage source workspace to produce a BehavioralModel: an ordered list of workflow Steps, each with typed Claims (inputs, outputs, skills, tools, references), setup needs, and gates.

The user message is a JSON object with two fields:
- "inventory": file paths, content digests, and literal citation edges between markdown files and the artifacts they mention.
- "sources": for each inventoried file (below a size cap), its path, digest, and actual text content (possibly truncated - see the "truncated" field). This is the only place you see real file content; the inventory alone has no prose or code in it.

Rules:
- Respond with exactly one JSON object matching the BehavioralModel schema (schema, name, intent, source_inventory_digest, steps, decisions) and nothing else - no prose, no markdown code fences.
- Every Claim, Step, and Decision must carry at least one evidence entry whose path and digest come from the supplied inventory, and whose note is an exact, verbatim quote taken from that file's content in "sources" - never a paraphrase or a claim about content you were not actually given. If a file's content is truncated and the fact you need lives past the cutoff, do not fabricate a quote for it - raise a Decision instead.
- source_inventory_digest must equal the source_inventory_digest already present in the supplied inventory payload.
- If something is ambiguous and the sources do not resolve it, emit a Decision naming exactly what's unresolved - never guess.
- Never cite evidence for a file that is not present in the supplied inventory.`

// sourceExcerpt is one file's content, bounded and path/digest-tagged so a
// provider's evidence citations can be checked against exactly the same
// snapshot the inventory itself was built from.
type sourceExcerpt struct {
	Path      string `json:"path"`
	Digest    string `json:"digest"`
	Content   string `json:"content"`
	Truncated bool   `json:"truncated,omitempty"`
}

// evidencePayload is what's actually sent to the provider as the user
// message: the inventory (structure, digests, citation edges) plus real
// file content for it to read and quote from. Sending the inventory alone
// gives a provider metadata about the workspace but never the prose or
// code it would need to infer workflow steps or produce a supporting quote
// - see buildSourceExcerpts.
type evidencePayload struct {
	Inventory inventory.Inventory `json:"inventory"`
	Sources   []sourceExcerpt     `json:"sources"`
}

// buildSourceExcerpts reads inv's files from inv.Root (the same workspace
// inventory.Discover walked) and returns their content, capped per-file so
// the prompt stays bounded regardless of workspace size. A file it can't
// read, or that's too large to bother with, is simply omitted rather than
// failing the whole call - Analyze's evidence checks (path/digest
// resolution, quote verification) are the real gate on whether the
// provider's eventual output is trustworthy, not this best-effort read.
func buildSourceExcerpts(inv inventory.Inventory) []sourceExcerpt {
	excerpts := make([]sourceExcerpt, 0, len(inv.Entries))
	for _, e := range inv.Entries {
		if e.Size > maxSourceFileBytes {
			continue
		}
		data, err := os.ReadFile(filepath.Join(inv.Root, filepath.FromSlash(e.Path)))
		if err != nil {
			continue
		}
		truncated := false
		if len(data) > maxSourceExcerptBytes {
			data = data[:maxSourceExcerptBytes]
			truncated = true
		}
		excerpts = append(excerpts, sourceExcerpt{Path: e.Path, Digest: e.Digest, Content: string(data), Truncated: truncated})
	}
	return excerpts
}

func (c ClaudeProvider) Analyze(ctx context.Context, inv inventory.Inventory) ([]byte, error) {
	apiKey := c.APIKey
	if apiKey == "" {
		apiKey = os.Getenv(claudeAPIKeyEnv)
	}
	if apiKey == "" {
		return nil, fmt.Errorf("no Claude API key: set %s or ClaudeProvider.APIKey", claudeAPIKeyEnv)
	}

	evidence, err := json.Marshal(evidencePayload{Inventory: inv, Sources: buildSourceExcerpts(inv)})
	if err != nil {
		return nil, fmt.Errorf("marshal inventory evidence: %w", err)
	}

	reqBody, err := json.Marshal(map[string]any{
		"model":      claudeModel,
		"max_tokens": claudeMaxTokens,
		"system":     analysisSystemPrompt,
		"messages": []map[string]string{
			{"role": "user", "content": string(evidence)},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, claudeAPIURL, bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("content-type", claudeContentType)
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("anthropic-version", claudeAPIVersion)

	client := c.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call Claude: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("read Claude response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("claude API returned %d: %s", resp.StatusCode, truncateForError(body))
	}

	var parsed struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("parse Claude response: %w", err)
	}

	// The Messages API can split one response across multiple text content
	// blocks; returning only the first would silently truncate a JSON
	// document that happened to span more than one block. Concatenate
	// every text block instead, so a split response is reassembled before
	// model.ParseModel ever sees it.
	var text strings.Builder
	for _, block := range parsed.Content {
		if block.Type == "text" {
			text.WriteString(block.Text)
		}
	}
	if text.Len() == 0 {
		return nil, fmt.Errorf("claude response had no text content")
	}
	return []byte(stripJSONFence(text.String())), nil
}

// truncateForError bounds how much of a raw response body is embedded in
// an error message - maxResponseBytes already bounds what's read into
// memory, but that alone still permits echoing megabytes of
// attacker-or-misconfiguration-controlled text into a log or terminal.
func truncateForError(body []byte) string {
	if len(body) <= maxErrorBodyBytes {
		return string(body)
	}
	return string(body[:maxErrorBodyBytes]) + "...(truncated)"
}

// stripJSONFence removes a leading/trailing ```json or ``` fence if the
// model wrapped its response in one despite the system prompt asking for
// bare JSON - defensive, since model.ParseModel's json.Unmarshal would
// otherwise reject an otherwise-valid response over formatting alone.
func stripJSONFence(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	return strings.TrimSpace(s)
}
