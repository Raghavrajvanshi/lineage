package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentic-lineage/lineage/internal/config"
	"github.com/agentic-lineage/lineage/internal/inventory"
	"github.com/agentic-lineage/lineage/internal/model"
)

func TestParseCompileArgs(t *testing.T) {
	m, s, out, force, yamlOut, err := parseCompileArgs([]string{"m.json", "ws", "--out", "pkg", "--force", "--yaml"})
	if err != nil || m != "m.json" || s != "ws" || out != "pkg" || !force || !yamlOut {
		t.Fatalf("got %q %q %q %v %v %v", m, s, out, force, yamlOut, err)
	}
	for _, args := range [][]string{
		{"m.json", "ws"},
		{"m.json", "--out", "pkg"},
		{"m.json", "ws", "--out"},
		{"m.json", "ws", "--out", "pkg", "--bogus"},
		{"m.json", "ws", "extra", "--out", "pkg"},
	} {
		if _, _, _, _, _, err := parseCompileArgs(args); err == nil {
			t.Errorf("parseCompileArgs(%v) error = nil, want error", args)
		}
	}
}

func write(t *testing.T, root, rel, content string, mode os.FileMode) {
	t.Helper()
	full := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

// TestAnalyzeCompileValidateEndToEnd runs the whole author flow with a
// fixture provider: analyze --model-out, compile, then package validate.
func TestAnalyzeCompileValidateEndToEnd(t *testing.T) {
	t.Setenv(config.HomeEnv, t.TempDir())
	ws := t.TempDir()
	write(t, ws, "CLAUDE.md", "Run scripts/deploy.sh after tests. Use gh to open a PR.\n", 0o644)
	write(t, ws, "scripts/deploy.sh", "#!/bin/sh\necho deploy\n", 0o755)
	inv, err := inventory.Discover(ws)
	if err != nil {
		t.Fatal(err)
	}
	digest := func(p string) string {
		for _, e := range inv.Entries {
			if e.Path == p {
				return e.Digest
			}
		}
		t.Fatalf("no entry %s", p)
		return ""
	}
	cite := func(p string) []model.EvidenceRef {
		src, err := os.ReadFile(filepath.Join(ws, p))
		if err != nil {
			t.Fatal(err)
		}
		first := strings.SplitN(string(src), "\n", 2)[0]
		return []model.EvidenceRef{{Path: p, Digest: digest(p), Line: 1, Note: first}}
	}
	fixture := model.BehavioralModel{
		Schema: model.CurrentSchema, Name: "ship-it", Intent: "Deploy after tests.",
		SourceInventoryDigest: model.ComputeInventoryDigest(inv),
		Steps: []model.Step{{
			ID: "deploy", Name: "Deploy", Description: "Deploy the app.", Evidence: cite("CLAUDE.md"),
			Tools: []model.Claim{
				{Value: "scripts/deploy.sh", Evidence: cite("scripts/deploy.sh")},
				{Value: "gh", Evidence: cite("CLAUDE.md")},
			},
		}},
	}
	data, err := json.Marshal(fixture)
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	fixturePath := filepath.Join(tmp, "response.json")
	modelPath := filepath.Join(tmp, "model.json")
	out := filepath.Join(tmp, "pkg")
	write(t, tmp, "response.json", string(data), 0o644)

	run := func(args ...string) (string, string, error) {
		var stdout, stderr bytes.Buffer
		err := Execute(context.Background(), args, nil, &stdout, &stderr)
		return stdout.String(), stderr.String(), err
	}

	if astdout, stderr, err := run("analyze", ws, "--fixture", fixturePath, "--model-out", modelPath); err != nil {
		t.Fatalf("analyze: %v\n%s\n%s", err, astdout, stderr)
	}
	saved, err := os.ReadFile(modelPath)
	if err != nil || !bytes.Contains(saved, []byte("\n  \"steps\"")) {
		t.Fatalf("model file not indented JSON: %v\n%s", err, saved)
	}

	stdout, stderr, err := run("compile", modelPath, ws, "--out", out)
	if err != nil {
		t.Fatalf("compile: %v\n%s", err, stderr)
	}
	for _, want := range []string{"compiled package ship-it", "- deploy", "requires (install yourself", "deploy: gh"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("compile output missing %q:\n%s", want, stdout)
		}
	}
	if _, err := os.Stat(filepath.Join(out, "skills", "deploy", "scripts", "deploy.sh")); err != nil {
		t.Errorf("script not bundled: %v", err)
	}
	if _, stderr, err := run("package", "validate", out); err != nil {
		t.Fatalf("package validate: %v\n%s", err, stderr)
	}

	blocked := fixture
	blocked.Decisions = []model.Decision{{ID: "d1", Description: "Which deploy?", Evidence: cite("CLAUDE.md")}}
	bdata, _ := json.Marshal(blocked)
	write(t, tmp, "blocked.json", string(bdata), 0o644)
	_, stderr, err = run("compile", filepath.Join(tmp, "blocked.json"), ws, "--out", filepath.Join(tmp, "pkg2"))
	if err == nil {
		t.Fatal("compile with unresolved decision must fail")
	}
	for _, want := range []string{"unresolved decision d1", "CLAUDE.md:1", "Re-run after editing: lineage compile"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("blocked output missing %q:\n%s", want, stderr)
		}
	}
	if _, statErr := os.Stat(filepath.Join(tmp, "pkg2")); statErr == nil {
		t.Error("blocked compile created the output directory")
	}
}
