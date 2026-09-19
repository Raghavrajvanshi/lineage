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
	"sort"
	"strings"
	"time"

	"github.com/agentic-lineage/lineage/internal/inventory"
	"github.com/agentic-lineage/lineage/internal/model"
	"github.com/agentic-lineage/lineage/internal/packages"
)

const (
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
	// MaxEvidenceBytes bounds the whole outbound evidence payload (inventory
	// plus every excerpt, as JSON). Per-file caps alone let a workspace of
	// many small files produce an unbounded request; over this budget the run
	// is refused, never silently trimmed, so what is sent is always what the
	// author was told about.
	MaxEvidenceBytes = 512 << 10 // 512KB

	// requestTimeout bounds a single Analyze call end to end,
	// mirroring registryRequestTimeout in internal/packages/registry.go —
	// the same fix for the same failure mode: http.DefaultClient's
	// Timeout is zero (unbounded), and runAnalyze passes its command
	// context straight through without adding a deadline of its own, so a
	// hung or slow-drip response would otherwise block `lineage analyze`
	// indefinitely.
	requestTimeout = 60 * time.Second
)

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
- Copy source_inventory_digest verbatim from the top-level "source_inventory_digest" field of this payload into your response's source_inventory_digest field - do not compute or alter it.
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
// message: the inventory (structure, digests, citation edges), real file
// content for it to read and quote from, and the source_inventory_digest
// it's required to echo back verbatim (see model.ComputeInventoryDigest -
// inventory.Inventory itself carries no such field, so the payload has to
// supply it explicitly rather than the system prompt pointing at a value
// that was never actually there). Sending the inventory alone gives a
// provider metadata about the workspace but never the prose or code it
// would need to infer workflow steps or produce a supporting quote - see
// buildSourceExcerpts.
type evidencePayload struct {
	Inventory             inventory.Inventory `json:"inventory"`
	Sources               []sourceExcerpt     `json:"sources"`
	SourceInventoryDigest string              `json:"source_inventory_digest"`
}

// buildSourceExcerpts reads inv's files from inv.Root (the same workspace
// inventory.Discover walked) and returns their content, capped per-file so
// the prompt stays bounded regardless of workspace size. A file it can't
// read, that's too large to bother with, or whose live content no longer
// matches the digest inv.Discover recorded for it (the workspace changed
// on disk after discovery ran) is simply omitted rather than failing the
// whole call - Analyze's evidence checks (path/digest resolution, quote
// verification) are the real gate on whether the provider's eventual
// output is trustworthy, not this best-effort read. Omitting a
// digest-mismatched file specifically avoids handing the provider content
// that no longer matches the digest the rest of the payload still claims
// for that path - it would otherwise cite a citation with a digest that
// looks valid (it's the one still in "inventory") but describes content
// that isn't what's actually there anymore.
func buildSourceExcerpts(inv inventory.Inventory) ([]sourceExcerpt, error) {
	// The inventory is sent whole, so its metadata spends the same budget as
	// the excerpts; checking it here means a workspace of very many tiny or
	// empty files is refused before anything is read or serialized.
	total := inventoryBytes(inv)
	if total > MaxEvidenceBytes {
		return nil, errEvidenceTooLarge(inv.Root, total)
	}
	excerpts := make([]sourceExcerpt, 0, len(inv.Entries))
	for _, e := range inv.Entries {
		if e.Size > maxSourceFileBytes {
			continue
		}
		data, err := os.ReadFile(filepath.Join(inv.Root, filepath.FromSlash(e.Path)))
		if err != nil {
			continue
		}
		if digestOf(data) != e.Digest {
			continue
		}
		truncated := false
		if len(data) > maxSourceExcerptBytes {
			data = data[:maxSourceExcerptBytes]
			truncated = true
		}
		total += len(data)
		if total > MaxEvidenceBytes {
			return nil, errEvidenceTooLarge(inv.Root, total)
		}
		excerpts = append(excerpts, sourceExcerpt{Path: e.Path, Digest: e.Digest, Content: string(data), Truncated: truncated})
	}
	return excerpts, nil
}

// inventoryBytes estimates inv's serialized size without marshaling it: the
// length of every string field plus a fixed per-object overhead covering JSON
// keys and punctuation. It is a deterministic budget input, not an exact
// size; the final len(evidence) check after marshaling remains as defense in
// depth for escaping overhead.
func inventoryBytes(inv inventory.Inventory) int {
	const entryOverhead, citationOverhead = 160, 120
	n := len(inv.Root)
	cites := func(cs []inventory.Citation) {
		for _, c := range cs {
			n += citationOverhead + len(c.FromPath) + len(c.ToPath) + len(c.MatchKind) + len(c.AsWritten) + len(c.Snippet)
		}
	}
	for _, e := range inv.Entries {
		n += entryOverhead + len(e.Path) + len(e.Kind) + len(e.Reason) + len(e.Digest) + len(e.Language)
		cites(e.Mentions)
		cites(e.ReferencedBy)
	}
	return n
}

func errEvidenceTooLarge(root string, size int) error {
	return fmt.Errorf("workspace %s produces at least %d bytes of evidence, over the %d-byte limit; nothing was sent. Analyze a smaller workspace or remove files that are not part of the workflow", root, size, MaxEvidenceBytes)
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

// Request is everything an Adapter needs for one call. Core resolves every
// field; an adapter only translates it into its vendor's wire format.
type Request struct {
	Endpoint string
	Model    string
	APIKey   string
	System   string
	User     string
	Client   *http.Client
}

// Adapter is one analysis provider's wire protocol: request/response
// translation only. Core owns the prompt, evidence, caps, secret scan and
// output validation; an Adapter owns its credential env var, default
// endpoint and errors, and returns the model's raw text.
type Adapter interface {
	Name() string
	// CredentialEnv is the environment variable holding this provider's key.
	CredentialEnv() string
	DefaultEndpoint() string
	Send(ctx context.Context, req Request) (string, error)
}

var adapters = map[string]Adapter{}

// Register makes a available under a.Name(). Adapters call it from init().
func Register(a Adapter) { adapters[a.Name()] = a }

// LookupAdapter returns the adapter registered under name.
func LookupAdapter(name string) (Adapter, bool) {
	a, ok := adapters[name]
	return a, ok
}

// AdapterNames lists registered adapter names, sorted.
func AdapterNames() []string {
	names := make([]string, 0, len(adapters))
	for n := range adapters {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// RemoteProvider sends workspace evidence to a registered Adapter's API.
type RemoteProvider struct {
	Adapter  Adapter
	Model    string
	Endpoint string // empty: Adapter.DefaultEndpoint()
	// APIKey overrides the adapter's credential env var when set.
	APIKey string
	// Client overrides the bounded default client when set.
	Client *http.Client
}

func (p RemoteProvider) Analyze(ctx context.Context, inv inventory.Inventory) ([]byte, error) {
	name := p.Adapter.Name()
	apiKey := p.APIKey
	if apiKey == "" {
		apiKey = os.Getenv(p.Adapter.CredentialEnv())
	}
	if apiKey == "" {
		return nil, fmt.Errorf("no %s API key: set %s", name, p.Adapter.CredentialEnv())
	}

	// Refuse outright rather than silently filtering: packages.Validate/
	// export.go already treat any ScanForSecrets finding as a hard blocker
	// ("must not ship something with an unresolved secret finding"), and a
	// workspace containing a credential file is the same situation here -
	// this call is about to send file content to an external API, so any
	// finding must stop that before it happens, not just quietly work
	// around it. FixtureProvider never makes a network call, so it isn't
	// subject to this check - the risk is specific to sending data
	// externally.
	if findings, err := packages.ScanForSecrets(inv.Root); err != nil {
		return nil, fmt.Errorf("scan workspace for secrets before sending to %s: %w", name, err)
	} else if len(findings) > 0 {
		return nil, fmt.Errorf("refusing to send workspace %s to %s: %d file(s) look like credentials (e.g. %s: %s); remove or exclude them before analyzing", inv.Root, name, len(findings), findings[0].Path, findings[0].Reason)
	}

	sources, err := buildSourceExcerpts(inv)
	if err != nil {
		return nil, err
	}
	evidence, err := json.Marshal(evidencePayload{
		Inventory:             inv,
		Sources:               sources,
		SourceInventoryDigest: model.ComputeInventoryDigest(inv),
	})
	if err != nil {
		return nil, fmt.Errorf("marshal inventory evidence: %w", err)
	}
	if len(evidence) > MaxEvidenceBytes {
		return nil, errEvidenceTooLarge(inv.Root, len(evidence))
	}

	endpoint := p.Endpoint
	if endpoint == "" {
		endpoint = p.Adapter.DefaultEndpoint()
	}
	client := p.Client
	if client == nil {
		client = &http.Client{Timeout: requestTimeout}
	}
	text, err := p.Adapter.Send(ctx, Request{
		Endpoint: endpoint,
		Model:    p.Model,
		APIKey:   apiKey,
		System:   analysisSystemPrompt,
		User:     string(evidence),
		Client:   client,
	})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if text == "" {
		return nil, fmt.Errorf("%s response had no text content", name)
	}
	return []byte(stripJSONFence(text)), nil
}

// PostJSON is the shared low-level transport for adapters: POST body to
// req.Endpoint with headers, read at most maxResponseBytes, and turn a
// non-200 into an error with a bounded body excerpt. It knows nothing about
// any vendor's payload shape.
func PostJSON(ctx context.Context, req Request, headers map[string]string, body any) ([]byte, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, req.Endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	hr.Header.Set("content-type", "application/json")
	for k, v := range headers {
		hr.Header.Set(k, v)
	}
	resp, err := req.Client.Do(hr)
	if err != nil {
		return nil, fmt.Errorf("call API: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("API returned %d: %s", resp.StatusCode, truncateForError(data))
	}
	return data, nil
}
