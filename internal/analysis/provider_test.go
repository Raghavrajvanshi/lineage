package analysis

import (
	"context"
	"encoding/json"
	"fmt"
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
// response, so tests can inspect exactly what RemoteProvider sends without
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

// fakeAdapter is a vendor-free Adapter: it proves RemoteProvider needs
// nothing vendor-specific from core.
type fakeAdapter struct{}

func (fakeAdapter) Name() string            { return "fake" }
func (fakeAdapter) CredentialEnv() string   { return "FAKE_API_KEY" }
func (fakeAdapter) DefaultEndpoint() string { return "https://fake.invalid/v1" }
func (fakeAdapter) Send(ctx context.Context, req Request) (string, error) {
	body, err := PostJSON(ctx, req, nil, map[string]any{
		"model":    req.Model,
		"messages": []map[string]string{{"role": "user", "content": req.User}},
	})
	if err != nil {
		return "", err
	}
	var parsed struct {
		Text string `json:"text"`
	}
	err = json.Unmarshal(body, &parsed)
	return parsed.Text, err
}

func textResponse(text string) *http.Response {
	body := `{"text":` + jsonQuote(text) + `}`
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

// TestRemoteProviderRefusesSecretBearingWorkspace covers the review
// finding that buildSourceExcerpts sent credential file content (e.g.
// .env) to the API verbatim. Analyze must refuse before any request is
// made, not filter the file out silently - the capture transport's
// `called` field proves no network call happened at all.
func TestRemoteProviderRefusesSecretBearingWorkspace(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "CLAUDE.md"), "# Instructions\n")
	mustWrite(t, filepath.Join(root, ".env"), "API_KEY=super-secret-value\n")
	inv, err := inventory.Discover(root)
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}

	capture := &captureTransport{resp: textResponse("{}")}
	p := RemoteProvider{Adapter: fakeAdapter{}, Model: "m", APIKey: "test-key", Client: &http.Client{Transport: capture}}

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

// TestRemoteProviderRequestIncludesSourceInventoryDigest covers the review
// finding that the system prompt required a source_inventory_digest value
// the payload never actually supplied. Intercepts the outgoing request via
// a fake RoundTripper (no real network call, no URL override needed) and
// asserts the nested evidencePayload carries the same digest
// model.Validate will later compare against.
func TestRemoteProviderRequestIncludesSourceInventoryDigest(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "CLAUDE.md"), "# Instructions\n")
	inv, err := inventory.Discover(root)
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}
	wantDigest := model.ComputeInventoryDigest(inv)

	capture := &captureTransport{resp: textResponse("{}")}
	p := RemoteProvider{Adapter: fakeAdapter{}, Model: "m", APIKey: "test-key", Client: &http.Client{Transport: capture}}

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

func TestRegistryListsAndRejectsUnknown(t *testing.T) {
	Register(fakeAdapter{})
	if _, ok := LookupAdapter("fake"); !ok {
		t.Fatal("LookupAdapter(fake) not found after Register")
	}
	if _, ok := LookupAdapter("nope"); ok {
		t.Fatal("LookupAdapter(nope) found, want miss")
	}
}

func TestRemoteProviderMissingKey(t *testing.T) {
	t.Setenv("FAKE_API_KEY", "")
	_, err := RemoteProvider{Adapter: fakeAdapter{}, Model: "m"}.Analyze(context.Background(), inventory.Inventory{})
	if err == nil || !strings.Contains(err.Error(), "FAKE_API_KEY") {
		t.Fatalf("err = %v, want missing-key error naming FAKE_API_KEY", err)
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

	excerpts, err := buildSourceExcerpts(inv)
	if err != nil {
		t.Fatal(err)
	}
	for _, ex := range excerpts {
		if ex.Path == "CLAUDE.md" {
			t.Fatalf("buildSourceExcerpts included drifted file %q, want it omitted", ex.Path)
		}
	}
}

// TestRemoteProviderRefusesOversizedEvidence: many small files pass every
// per-file cap but must still trip the total budget before anything is sent.
func TestRemoteProviderRefusesOversizedEvidence(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "CLAUDE.md"), "# Instructions\n")
	chunk := strings.Repeat("x", 4<<10)
	for i := 0; i < MaxEvidenceBytes/(4<<10)+8; i++ {
		mustWrite(t, filepath.Join(root, "docs", fmt.Sprintf("f%04d.md", i)), chunk)
	}
	inv, err := inventory.Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	capture := &captureTransport{resp: textResponse("{}")}
	_, err = RemoteProvider{Adapter: fakeAdapter{}, Model: "m", APIKey: "k", Client: &http.Client{Transport: capture}}.Analyze(context.Background(), inv)
	if err == nil || !strings.Contains(err.Error(), "over the") {
		t.Fatalf("err = %v, want evidence-too-large refusal", err)
	}
	if capture.called {
		t.Fatal("request was sent despite oversized evidence")
	}
}

// TestRemoteProviderRefusesOversizedInventory: enough empty files that the
// inventory metadata alone exceeds the budget must be refused before any
// excerpt is built or request made - excerpts are zero bytes here, so only
// the inventory budget can trip.
func TestRemoteProviderRefusesOversizedInventory(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < MaxEvidenceBytes/100; i++ {
		mustWrite(t, filepath.Join(root, "empty", fmt.Sprintf("f%05d.txt", i)), "")
	}
	inv, err := inventory.Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	if got := inventoryBytes(inv); got <= MaxEvidenceBytes {
		t.Fatalf("inventoryBytes = %d, test setup must exceed %d", got, MaxEvidenceBytes)
	}
	capture := &captureTransport{resp: textResponse("{}")}
	_, err = RemoteProvider{Adapter: fakeAdapter{}, Model: "m", APIKey: "k", Client: &http.Client{Transport: capture}}.Analyze(context.Background(), inv)
	if err == nil || !strings.Contains(err.Error(), "over the") {
		t.Fatalf("err = %v, want evidence-too-large refusal", err)
	}
	if capture.called {
		t.Fatal("request was sent despite oversized inventory")
	}
}
