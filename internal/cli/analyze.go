package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/agentic-lineage/lineage/internal/analysis"
	"github.com/agentic-lineage/lineage/internal/inventory"
	"github.com/agentic-lineage/lineage/internal/model"
)

const analyzeUsage = "usage: lineage analyze <path> [--provider claude] [--fixture file] [--yaml] [--model-out file]"

// runAnalyze is `lineage analyze <path>`: discover the source workspace's
// inventory, hand it to a Provider (Claude by default), and validate what
// comes back - the CLI entry point for #104's agent-assisted analysis
// stage. It never writes package artifacts: --model-out saves the full
// BehavioralModel for `lineage compile` to consume, and compile is what
// generates the package.
func runAnalyze(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if hasHelpFlag(args) {
		fmt.Fprintln(stdout, analyzeUsage+"\n\nRun agent-assisted analysis over a source workspace: discover its inventory, ask a provider to infer a BehavioralModel grounded in that evidence, and validate the result. Never writes package artifacts. --model-out saves the full behavioral model as indented JSON so an author can review or edit it and then run `lineage compile`. --fixture reads a canned raw provider response from a file instead of calling a live provider, for use without credentials.")
		return nil
	}

	path, fixturePath, providerName, modelOut, yamlOutput, err := parseAnalyzeArgs(args)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return err
	}
	if path == "" {
		err := fmt.Errorf(analyzeUsage)
		fmt.Fprintln(stderr, err)
		return err
	}

	inv, err := inventory.Discover(filepath.Clean(path))
	if err != nil {
		fmt.Fprintln(stderr, err)
		return err
	}

	p, providerLabel, err := resolveAnalysisProvider(providerName, fixturePath)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return err
	}

	result, err := analysis.Analyze(ctx, p, inv)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return err
	}
	report := buildAnalysisReport(providerLabel, result)

	if yamlOutput {
		if err := writeAnalysisYAML(stdout, report); err != nil {
			fmt.Fprintln(stderr, err)
			return err
		}
	} else {
		printAnalysisReport(stdout, report)
	}

	if !result.Report.Passed() {
		err := fmt.Errorf("analysis failed with %d error(s)", len(result.Report.Errors))
		fmt.Fprintln(stderr, err)
		return err
	}
	if modelOut != "" {
		if err := writeModelFile(modelOut, result.Model); err != nil {
			fmt.Fprintln(stderr, err)
			return err
		}
	}
	return nil
}

// writeModelFile saves m as indented JSON, the format model.ParseModel reads.
// Only a model that passed validation is ever written, so a saved file never
// holds a model that compile would have to reject for schema or evidence
// reasons it could have been told about here.
func writeModelFile(path string, m model.BehavioralModel) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("encode behavioral model: %w", err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("write behavioral model: %w", err)
	}
	return nil
}

// parseAnalyzeArgs parses `lineage analyze`'s arguments. Every recognized
// option that takes a value (--fixture, --provider) is checked for a
// missing value explicitly and rejected before it ever falls through to
// being treated as the positional <path> - a bare "lineage analyze
// --fixture" must not silently run against a workspace literally named
// "--fixture". Any other argument starting with "-" is rejected outright
// rather than being treated as a path, for the same reason.
func parseAnalyzeArgs(args []string) (path, fixturePath, providerName, modelOut string, yamlOutput bool, err error) {
	providerName = "claude"
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch arg {
		case "--yaml":
			yamlOutput = true
			continue
		case "--fixture":
			if i+1 >= len(args) {
				return "", "", "", "", false, fmt.Errorf("--fixture requires a value\n%s", analyzeUsage)
			}
			i++
			fixturePath = args[i]
			continue
		case "--model-out":
			if i+1 >= len(args) {
				return "", "", "", "", false, fmt.Errorf("--model-out requires a value\n%s", analyzeUsage)
			}
			i++
			modelOut = args[i]
			continue
		case "--provider":
			if i+1 >= len(args) {
				return "", "", "", "", false, fmt.Errorf("--provider requires a value\n%s", analyzeUsage)
			}
			i++
			providerName = args[i]
			continue
		}
		if strings.HasPrefix(arg, "-") {
			return "", "", "", "", false, fmt.Errorf("unknown option %q\n%s", arg, analyzeUsage)
		}
		if path != "" {
			return "", "", "", "", false, fmt.Errorf(analyzeUsage)
		}
		path = arg
	}
	return path, fixturePath, providerName, modelOut, yamlOutput, nil
}

// resolveAnalysisProvider picks the analysis.Provider for this run.
// providerLabel is recorded in the report so a fixture-driven test run and
// a real Claude run are never visually indistinguishable in output - see
// AnalysisReport.Provider's doc comment.
func resolveAnalysisProvider(providerName, fixturePath string) (analysis.Provider, string, error) {
	if fixturePath != "" {
		data, err := os.ReadFile(fixturePath)
		if err != nil {
			return nil, "", err
		}
		return analysis.FixtureProvider{Response: data}, "fixture:" + fixturePath, nil
	}
	if providerName == "claude" {
		return analysis.ClaudeProvider{}, "claude", nil
	}
	return nil, "", fmt.Errorf("unknown provider %q; use --fixture to run without a live provider", providerName)
}
