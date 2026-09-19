package cli

import "testing"

// TestParseAnalyzeArgs is table-driven per the PR review finding that
// missing option values and unknown flags were silently falling through to
// become the positional <path> - e.g. `lineage analyze --fixture` (no
// value) previously ran against a workspace literally named "--fixture"
// instead of failing with a clear error.
func TestParseAnalyzeArgs(t *testing.T) {
	tests := []struct {
		name         string
		args         []string
		wantPath     string
		wantFixture  string
		wantProvider string
		wantModelOut string
		wantYAML     bool
		wantErr      bool
	}{
		{name: "path only", args: []string{"workspace"}, wantPath: "workspace", wantProvider: "claude"},
		{name: "path with fixture", args: []string{"workspace", "--fixture", "f.json"}, wantPath: "workspace", wantFixture: "f.json", wantProvider: "claude"},
		{name: "path with provider", args: []string{"workspace", "--provider", "claude"}, wantPath: "workspace", wantProvider: "claude"},
		{name: "path with yaml", args: []string{"workspace", "--yaml"}, wantPath: "workspace", wantProvider: "claude", wantYAML: true},
		{name: "path with model-out", args: []string{"workspace", "--model-out", "m.json"}, wantPath: "workspace", wantProvider: "claude", wantModelOut: "m.json"},
		{name: "model-out missing value", args: []string{"workspace", "--model-out"}, wantErr: true},
		{name: "fixture missing value at end", args: []string{"workspace", "--fixture"}, wantErr: true},
		{name: "fixture missing value alone", args: []string{"--fixture"}, wantErr: true},
		{name: "provider missing value", args: []string{"workspace", "--provider"}, wantErr: true},
		{name: "unknown flag with empty path", args: []string{"--bogus"}, wantErr: true},
		{name: "unknown flag with path set", args: []string{"workspace", "--bogus"}, wantErr: true},
		{name: "two positional args", args: []string{"workspace1", "workspace2"}, wantErr: true},
		{name: "no args", args: []string{}, wantPath: "", wantProvider: "claude"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path, fixture, provider, modelOut, yamlOut, err := parseAnalyzeArgs(tt.args)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseAnalyzeArgs(%v) error = nil, want error", tt.args)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseAnalyzeArgs(%v) error = %v, want nil", tt.args, err)
			}
			if path != tt.wantPath {
				t.Errorf("path = %q, want %q", path, tt.wantPath)
			}
			if fixture != tt.wantFixture {
				t.Errorf("fixture = %q, want %q", fixture, tt.wantFixture)
			}
			if provider != tt.wantProvider {
				t.Errorf("provider = %q, want %q", provider, tt.wantProvider)
			}
			if modelOut != tt.wantModelOut {
				t.Errorf("modelOut = %q, want %q", modelOut, tt.wantModelOut)
			}
			if yamlOut != tt.wantYAML {
				t.Errorf("yamlOutput = %v, want %v", yamlOut, tt.wantYAML)
			}
		})
	}
}
