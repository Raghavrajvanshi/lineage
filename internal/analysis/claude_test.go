package analysis

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentic-lineage/lineage/internal/inventory"
	"github.com/agentic-lineage/lineage/internal/model"
)

// captureTransport is a fake http.RoundTripper that records the outgoing
// request body (if any request is made at all) and returns a canned
// response, so tests can inspect exactly what ClaudeProvider sends without
// a live network call or a real API key.
type captureTransport struct {
	called       bool
	capturedBody []byte
	resp         *http.Response
	err          error
}

func (c *captureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	c.called = true
	if req.Body != nil {
		c.capturedBody, _ = io.ReadAll(req.Body)
	}
	if c.err != nil {
		return nil, c.err
	}
	return c.resp, nil
}

func textResponse(text string) *http.Response {
	body := `{"content":[{"type":"text","text":` + jsonQuote(text) + `}]}`
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
	}
}

func jsonQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// TestClaudeProviderRefusesSecretBearingWorkspace covers the review
// finding that buildSourceExcerpts sent credential file content (e.g.
// .env) to the API verbatim. Analyze must refuse before any request is
// made, not filter the file out silently - the capture transport's
// `called` field proves no network call happened at all.
func TestClaudeProviderRefusesSecretBearingWorkspace(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "CLAUDE.md"), "# Instructions\n")
	mustWrite(t, filepath.Join(root, ".env"), "API_KEY=super-secret-value\n")
	inv, err := inventory.Discover(root)
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}

	capture := &captureTransport{resp: textResponse("{}")}
	p := ClaudeProvider{APIKey: "test-key", Client: &http.Client{Transport: capture}}

	_, err = p.Analyze(context.Background(), inv)
	if err == nil {
		t.Fatal("Analyze() error = nil, want refusal for a workspace containing a credential file")
	}
	if !strings.Contains(err.Error(), ".env") {
		t.Fatalf("error %q does not mention the flagged file", err.Error())
	}
	if capture.called {
		t.Fatal("Analyze() made an HTTP request despite the secret finding - must refuse before sending anything")
	}
}

// TestClaudeProviderRequestIncludesSourceInventoryDigest covers the review
// finding that the system prompt required a source_inventory_digest value
// the payload never actually supplied. Intercepts the outgoing request via
// a fake RoundTripper (no real network call, no URL override needed) and
// asserts the nested evidencePayload carries the same digest
// model.Validate will later compare against.
func TestClaudeProviderRequestIncludesSourceInventoryDigest(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "CLAUDE.md"), "# Instructions\n")
	inv, err := inventory.Discover(root)
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}
	wantDigest := model.ComputeInventoryDigest(inv)

	capture := &captureTransport{resp: textResponse("{}")}
	p := ClaudeProvider{APIKey: "test-key", Client: &http.Client{Transport: capture}}

	if _, err := p.Analyze(context.Background(), inv); err != nil {
		t.Fatalf("Analyze() error = %v", err)
	}

	var outer struct {
		Messages []struct {
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(capture.capturedBody, &outer); err != nil {
		t.Fatalf("unmarshal request body: %v (body=%s)", err, capture.capturedBody)
	}
	if len(outer.Messages) != 1 {
		t.Fatalf("len(Messages) = %d, want 1", len(outer.Messages))
	}

	var payload evidencePayload
	if err := json.Unmarshal([]byte(outer.Messages[0].Content), &payload); err != nil {
		t.Fatalf("unmarshal evidence payload: %v", err)
	}
	if payload.SourceInventoryDigest == "" {
		t.Fatal("payload SourceInventoryDigest is empty")
	}
	if payload.SourceInventoryDigest != wantDigest {
		t.Fatalf("SourceInventoryDigest = %q, want %q", payload.SourceInventoryDigest, wantDigest)
	}
}

// TestDefaultClaudeClientHasBoundedTimeout covers the review finding that
// ClaudeProvider fell back to http.DefaultClient (Timeout: 0, unbounded)
// whenever no Client was injected, so a hung or slow-drip response could
// block Analyze forever - runAnalyze passes its command context through
// without adding a deadline of its own. Mirrors registryRequestTimeout in
// internal/packages/registry.go, the same fix for the same failure mode.
func TestDefaultClaudeClientHasBoundedTimeout(t *testing.T) {
	client := defaultClaudeClient()
	if client.Timeout != claudeRequestTimeout {
		t.Fatalf("defaultClaudeClient().Timeout = %v, want %v", client.Timeout, claudeRequestTimeout)
	}
	if client.Timeout <= 0 {
		t.Fatal("defaultClaudeClient().Timeout is unbounded, want a positive bound")
	}
}

// TestBuildSourceExcerptsOmitsDriftedFile covers the review finding that
// buildSourceExcerpts never re-hashed content against the digest
// inventory.Discover recorded, so a file edited after discovery could
// still have its (now-stale) content sent to the provider under the old
// digest.
func TestBuildSourceExcerptsOmitsDriftedFile(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "CLAUDE.md")
	mustWrite(t, path, "# Instructions\n\nRun scripts/deploy.sh to deploy.\n")
	inv, err := inventory.Discover(root)
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}

	// Mutate on disk after discovery: the inventory's recorded digest for
	// CLAUDE.md is now stale.
	mustWrite(t, path, "# Instructions\n\nRun scripts/deploy.sh to deploy.\n\nAlso run scripts/release.sh.\n")

	excerpts := buildSourceExcerpts(inv)
	for _, ex := range excerpts {
		if ex.Path == "CLAUDE.md" {
			t.Fatalf("buildSourceExcerpts included drifted file %q, want it omitted", ex.Path)
		}
	}
}
