package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestParseAnalyzeArgs is table-driven per the PR review finding that
// missing option values and unknown flags were silently falling through to
// become the positional <path> - e.g. `lineage analyze --fixture` (no
// value) previously ran against a workspace literally named "--fixture"
// instead of failing with a clear error.
func TestParseAnalyzeArgs(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		want    analyzeOptions
		wantErr bool
	}{
		{name: "path only", args: []string{"workspace"}, want: analyzeOptions{path: "workspace"}},
		{name: "path with fixture", args: []string{"workspace", "--fixture", "f.json"}, want: analyzeOptions{path: "workspace", fixturePath: "f.json"}},
		{name: "provider model endpoint yes", args: []string{"workspace", "--provider", "p", "--model", "m", "--endpoint", "https://e", "--yes"}, want: analyzeOptions{path: "workspace", provider: "p", model: "m", endpoint: "https://e", yes: true}},
		{name: "path with yaml", args: []string{"workspace", "--yaml"}, want: analyzeOptions{path: "workspace", yamlOutput: true}},
		{name: "path with model-out", args: []string{"workspace", "--model-out", "m.json"}, want: analyzeOptions{path: "workspace", modelOut: "m.json"}},
		{name: "model-out missing value", args: []string{"workspace", "--model-out"}, wantErr: true},
		{name: "fixture missing value at end", args: []string{"workspace", "--fixture"}, wantErr: true},
		{name: "fixture missing value alone", args: []string{"--fixture"}, wantErr: true},
		{name: "provider missing value", args: []string{"workspace", "--provider"}, wantErr: true},
		{name: "model missing value", args: []string{"workspace", "--model"}, wantErr: true},
		{name: "unknown flag with empty path", args: []string{"--bogus"}, wantErr: true},
		{name: "unknown flag with path set", args: []string{"workspace", "--bogus"}, wantErr: true},
		{name: "two positional args", args: []string{"workspace1", "workspace2"}, wantErr: true},
		{name: "no args", args: []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseAnalyzeArgs(tt.args)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseAnalyzeArgs(%v) error = nil, want error", tt.args)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("parseAnalyzeArgs(%v) = %+v, %v; want %+v", tt.args, got, err, tt.want)
			}
		})
	}
}

func testEnv(vars map[string]string, interactive bool, stdin string) (analysisEnv, *bytes.Buffer) {
	var stderr bytes.Buffer
	return analysisEnv{
		getenv:      func(k string) string { return vars[k] },
		stdin:       strings.NewReader(stdin),
		stderr:      &stderr,
		interactive: interactive,
	}, &stderr
}

func TestResolveAnalysisProvider(t *testing.T) {
	wd, _ := os.Getwd()
	if err := os.Chdir(t.TempDir()); err != nil { // no project config above
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(wd) })
	key := map[string]string{"OPENAI_API_KEY": "k"}
	tests := []struct {
		name        string
		opts        analyzeOptions
		vars        map[string]string
		interactive bool
		stdin       string
		wantErr     string
		wantNotice  bool
	}{
		{name: "no provider", opts: analyzeOptions{}, wantErr: "no analysis provider selected"},
		{name: "unknown provider", opts: analyzeOptions{provider: "nope"}, wantErr: "unknown analysis provider"},
		{name: "no model", opts: analyzeOptions{provider: "openai"}, vars: key, wantErr: "no model selected"},
		{name: "no key", opts: analyzeOptions{provider: "openai", model: "m"}, wantErr: "OPENAI_API_KEY"},
		{name: "key never inferred to pick provider", opts: analyzeOptions{model: "m"}, vars: key, wantErr: "no analysis provider selected"},
		{name: "http endpoint rejected", opts: analyzeOptions{provider: "openai", model: "m", endpoint: "http://example.com/x"}, vars: key, wantErr: "must be https"},
		{name: "localhost http allowed, CI needs yes", opts: analyzeOptions{provider: "openai", model: "m", endpoint: "http://localhost:8080/v1"}, vars: key, wantErr: "pass --yes", wantNotice: true},
		{name: "CI with yes proceeds", opts: analyzeOptions{provider: "openai", model: "m", yes: true}, vars: key, wantNotice: true},
		{name: "tty decline", opts: analyzeOptions{provider: "openai", model: "m"}, vars: key, interactive: true, stdin: "n\n", wantErr: "nothing was sent", wantNotice: true},
		{name: "tty accept", opts: analyzeOptions{provider: "openai", model: "m"}, vars: key, interactive: true, stdin: "y\n", wantNotice: true},
		{name: "env selects provider and model", opts: analyzeOptions{yes: true}, vars: map[string]string{"LINEAGE_ANALYSIS_PROVIDER": "openai", "LINEAGE_ANALYSIS_MODEL": "m", "OPENAI_API_KEY": "k"}, wantNotice: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env, stderr := testEnv(tt.vars, tt.interactive, tt.stdin)
			_, _, err := resolveAnalysisProvider(tt.opts, env)
			if tt.wantErr == "" && err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
			}
			if got := strings.Contains(stderr.String(), "will send"); got != tt.wantNotice {
				t.Fatalf("egress notice printed = %v, want %v (stderr=%q)", got, tt.wantNotice, stderr.String())
			}
		})
	}
}

func TestResolveAnalysisProviderFixtureNeedsNoProvider(t *testing.T) {
	f := filepath.Join(t.TempDir(), "r.json")
	os.WriteFile(f, []byte("{}"), 0o644)
	env, _ := testEnv(nil, false, "")
	if _, label, err := resolveAnalysisProvider(analyzeOptions{fixturePath: f}, env); err != nil || !strings.HasPrefix(label, "fixture:") {
		t.Fatalf("label=%q err=%v", label, err)
	}
}

// TestCoreHasNoVendorCoupling guards the provider-blind boundary: vendor
// names, endpoints and key env vars live only in adapter packages.
func TestCoreHasNoVendorCoupling(t *testing.T) {
	for _, f := range []string{"analyze.go", "../analysis/provider.go", "../analysis/analysis.go"} {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, bad := range []string{"anthropic", "openai", "openrouter", "claude-", "_API_KEY"} {
			if strings.Contains(strings.ToLower(string(data)), strings.ToLower(bad)) {
				t.Errorf("%s mentions vendor string %q", f, bad)
			}
		}
	}
}
