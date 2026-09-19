// Package analysis is the agent-assisted analysis stage (#104) that sits
// between internal/inventory's evidence and internal/model's behavioral
// model: it hands a Provider the source inventory as evidence, parses and
// validates whatever BehavioralModel comes back, and fails closed if that
// output is malformed, invalid, or contradicts the evidence it was given.
//
// It never edits the source workspace, executes anything in it, or writes
// package artifacts — this stage only produces a validated (or rejected)
// BehavioralModel for a human, or `lineage compile`, to act
// on.
package analysis

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/agentic-lineage/lineage/internal/inventory"
	"github.com/agentic-lineage/lineage/internal/model"
)

// digestOf hashes data the same way inventory.Entry.Digest is computed
// ("sha256:<hex>"), so freshly re-read file content can be compared
// directly against the digest inventory.Discover recorded for it. Shared
// by claude.go's buildSourceExcerpts and this file's verifyQuotedEvidence
// - both need to detect a file that's changed on disk since discovery ran.
func digestOf(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Provider turns source evidence into a candidate behavioral model. It
// returns the provider's raw output, not a parsed model.BehavioralModel:
// parsing and validation happen exactly once, in Analyze, so a malformed
// response is always caught in the same place regardless of which Provider
// produced it.
type Provider interface {
	Analyze(ctx context.Context, inv inventory.Inventory) ([]byte, error)
}

// Result is one analysis pass's outcome. Model is populated whenever
// parsing succeeds, even if Report fails — a caller can see exactly what
// the provider produced and why it was rejected. Callers must check
// Report.Passed() before trusting Model; Model alone is never a signal
// that analysis succeeded.
type Result struct {
	Inventory inventory.Inventory
	Model     model.BehavioralModel
	Report    model.ValidateReport
}

// Analyze runs one analysis pass: it hands inv to p as evidence, parses and
// validates whatever model p returns via model.ParseModel and
// model.Validate, and layers a small set of provider-output-quality checks
// on top — see qualityNotes. Those checks judge how carefully the analysis
// was done, not model/inventory consistency (that's model.Validate's job),
// so they land in Report.Notes, never Report.Errors: a provider that did
// something worth double-checking is not the same as one whose model is
// actually invalid.
//
// inv is discovered once by the caller and threaded through unchanged to
// both the provider call and the final Validate — Analyze never
// re-discovers it, so every check in one pass agrees on exactly which
// source snapshot it's judging evidence against.
func Analyze(ctx context.Context, p Provider, inv inventory.Inventory) (Result, error) {
	raw, err := p.Analyze(ctx, inv)
	if err != nil {
		return Result{}, fmt.Errorf("provider analysis failed: %w", err)
	}

	m, err := model.ParseModel(raw)
	if err != nil {
		return Result{}, fmt.Errorf("parse provider output: %w", err)
	}

	report, err := model.Validate(m, inv)
	if err != nil {
		return Result{}, fmt.Errorf("validate provider output: %w", err)
	}

	refs := allEvidenceRefs(m)
	// A fabricated quote is evidence drift by another name — a claim
	// citing a real path+digest whose "supporting" text doesn't actually
	// appear in that file is not meaningfully different from citing a
	// stale digest, which model.Validate already treats as an Errors
	// entry, never a Note. Verification needs file content model.Validate
	// doesn't have (it only sees inventory metadata), so it lives here,
	// but the failure belongs in Errors for the same reason.
	report.Errors = append(report.Errors, verifyQuotedEvidence(refs, inv)...)
	report.Notes = append(report.Notes, qualityNotes(refs, m, inv)...)

	return Result{Inventory: inv, Model: m, Report: report}, nil
}

// evidenceRefContext is one EvidenceRef together with a human-readable
// description of where in the model it came from, shared by qualityNotes
// and verifyQuotedEvidence so both walk the model's Steps/Claims/Decisions
// exactly once between them.
type evidenceRefContext struct {
	Context string
	Ref     model.EvidenceRef
}

func allEvidenceRefs(m model.BehavioralModel) []evidenceRefContext {
	var refs []evidenceRefContext
	add := func(context string, evidence []model.EvidenceRef) {
		for _, ref := range evidence {
			refs = append(refs, evidenceRefContext{Context: context, Ref: ref})
		}
	}

	for _, step := range m.Steps {
		add(fmt.Sprintf("step %q", step.ID), step.Evidence)
		for _, claims := range [][]model.Claim{step.Inputs, step.Outputs, step.Skills, step.Tools, step.References} {
			for _, c := range claims {
				add(fmt.Sprintf("step %q claim %q", step.ID, c.Value), c.Evidence)
			}
		}
		for _, s := range step.Setup {
			add(fmt.Sprintf("step %q setup %q", step.ID, s.Path), s.Evidence)
		}
		for _, g := range step.Gates {
			add(fmt.Sprintf("step %q gate %q", step.ID, g.ID), g.Evidence)
		}
	}
	for _, d := range m.Decisions {
		add(fmt.Sprintf("decision %q", d.ID), d.Evidence)
	}
	return refs
}

// verifyQuotedEvidence re-reads each cited file from inv.Root — the same
// workspace inv was discovered from — and checks two things: that the
// content on disk still matches the digest inv.Discover recorded for it
// (the file hasn't changed since discovery ran), and that every non-empty
// EvidenceRef.Note actually appears in that content (at the cited Line, if
// set; anywhere in the file otherwise). model.Validate already proves the
// cited digest resolves against inv's metadata; this proves both that the
// quote attached to a citation is real rather than invented, and that the
// content being checked against is still the content the model was
// actually built from - a file edited after discovery (even one that
// still contains the original quoted line, just with more added) would
// otherwise pass a naive substring check while no longer being what the
// evidence claims to describe.
//
// A ref with no Note is skipped here — that's qualityNotes' concern
// (nothing to verify against), not a fabrication to catch.
func verifyQuotedEvidence(refs []evidenceRefContext, inv inventory.Inventory) []string {
	entries := make(map[string]inventory.Entry, len(inv.Entries))
	for _, e := range inv.Entries {
		entries[e.Path] = e
	}

	content := make(map[string]string)
	read := func(path string) (string, error) {
		if c, ok := content[path]; ok {
			return c, nil
		}
		data, err := os.ReadFile(filepath.Join(inv.Root, filepath.FromSlash(path)))
		if err != nil {
			return "", err
		}
		if entry, known := entries[path]; known {
			if got := digestOf(data); got != entry.Digest {
				return "", fmt.Errorf("file has changed since inventory discovery (expected digest %s, found %s)", entry.Digest, got)
			}
		}
		content[path] = string(data)
		return content[path], nil
	}

	var errs []string
	for _, rc := range refs {
		ref := rc.Ref
		if ref.Note == "" {
			continue
		}
		text, err := read(ref.Path)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: evidence for %q could not be verified: %v", rc.Context, ref.Path, err))
			continue
		}
		if ref.Line > 0 {
			lines := strings.Split(text, "\n")
			if ref.Line > len(lines) || !strings.Contains(lines[ref.Line-1], ref.Note) {
				errs = append(errs, fmt.Sprintf("%s: evidence note for %q does not match line %d of the source file - possible fabricated quote", rc.Context, ref.Path, ref.Line))
			}
			continue
		}
		if !strings.Contains(text, ref.Note) {
			errs = append(errs, fmt.Sprintf("%s: evidence note for %q does not appear anywhere in the source file - possible fabricated quote", rc.Context, ref.Path))
		}
	}
	return errs
}

// qualityNotes flags provider output that is technically valid (it passes
// model.Validate and verifyQuotedEvidence) but not trustworthy enough to
// accept without a second look:
//
//   - evidence with no supporting quote (EvidenceRef.Note), so a reviewer
//     has nothing to spot-check the claim against without reopening the
//     source file;
//   - evidence citing a file whose basename is ambiguous
//     (inventory.Entry.AmbiguousBasename) with no Line or Note pinning
//     which occurrence is meant — path+digest still resolve, so
//     model.Validate accepts it, but nothing disambiguates which mention
//     the claim is actually about;
//   - a suspiciously high decision-to-step ratio, which can mean the
//     provider punted on ambiguity it could have resolved from the
//     evidence it was given rather than doing the analysis.
//
// These are judgment calls about analysis quality, not model/inventory
// consistency, so they don't belong in model.Validate itself — they live
// here, specific to this stage.
func qualityNotes(refs []evidenceRefContext, m model.BehavioralModel, inv inventory.Inventory) []string {
	entries := make(map[string]inventory.Entry, len(inv.Entries))
	for _, e := range inv.Entries {
		entries[e.Path] = e
	}

	var notes []string
	for _, rc := range refs {
		ref := rc.Ref
		entry, known := entries[ref.Path]
		switch {
		case known && entry.AmbiguousBasename && ref.Line == 0 && ref.Note == "":
			notes = append(notes, fmt.Sprintf("%s: evidence cites %q, whose basename is ambiguous, with no line or note to disambiguate which file is meant", rc.Context, ref.Path))
		case ref.Note == "":
			notes = append(notes, fmt.Sprintf("%s: evidence for %q has no supporting note/quote", rc.Context, ref.Path))
		}
	}

	if len(m.Steps) > 0 && len(m.Decisions) > len(m.Steps) {
		notes = append(notes, fmt.Sprintf("decision-to-step ratio is high (%d decisions for %d steps) — review whether resolvable ambiguity was deferred instead of analyzed", len(m.Decisions), len(m.Steps)))
	}

	sort.Strings(notes)
	return notes
}
