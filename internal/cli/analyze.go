package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/agentic-lineage/lineage/internal/analysis"
	_ "github.com/agentic-lineage/lineage/internal/analysis/adapters/all"
	"github.com/agentic-lineage/lineage/internal/config"
	"github.com/agentic-lineage/lineage/internal/inventory"
	"github.com/agentic-lineage/lineage/internal/model"
)

const analyzeUsage = "usage: lineage analyze <path> [--provider name --model id] [--endpoint url] [--yes] [--fixture file] [--yaml] [--model-out file]"

// runAnalyze is `lineage analyze <path>`: discover the source workspace's
// inventory, hand it to the explicitly chosen analysis provider, and validate what
// comes back - the CLI entry point for #104's agent-assisted analysis
// stage. It never writes package artifacts: --model-out saves the full
// BehavioralModel for `lineage compile` to consume, and compile is what
// generates the package.
func runAnalyze(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if hasHelpFlag(args) {
		fmt.Fprintln(stdout, analyzeUsage+"\n\nRun agent-assisted analysis over a source workspace: discover its inventory, ask a provider to infer a BehavioralModel grounded in that evidence, and validate the result. Never writes package artifacts. --model-out saves the full behavioral model as indented JSON so an author can review or edit it and then run `lineage compile`. --fixture reads a canned raw provider response from a file instead of calling a live provider, for use without credentials.\n\nA live provider is never inferred or defaulted. Choose one with --provider (or LINEAGE_ANALYSIS_PROVIDER, or analysis.provider in .lineage/config.yaml) and a model with --model (LINEAGE_ANALYSIS_MODEL). The API key is read from the provider's environment variable and is never a flag or saved. --endpoint (LINEAGE_ANALYSIS_ENDPOINT) overrides the provider's default endpoint; it must be https unless it is localhost. Before anything is sent, the notice names the provider and host; confirm at the prompt or pass --yes (required when not run from a terminal). Registered providers: "+strings.Join(analysis.AdapterNames(), ", ")+".")
		return nil
	}

	opts, err := parseAnalyzeArgs(args)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return err
	}
	if opts.path == "" {
		err := fmt.Errorf(analyzeUsage)
		fmt.Fprintln(stderr, err)
		return err
	}

	inv, err := inventory.Discover(filepath.Clean(opts.path))
	if err != nil {
		fmt.Fprintln(stderr, err)
		return err
	}

	p, providerLabel, err := resolveAnalysisProvider(opts, analysisEnv{getenv: os.Getenv, stdin: os.Stdin, stderr: stderr, interactive: stdinIsTerminal()})
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

	if opts.yamlOutput {
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
	if opts.modelOut != "" {
		if err := writeModelFile(opts.modelOut, result.Model); err != nil {
			fmt.Fprintln(stderr, err)
			return err
		}
	}
	return nil
}

// writeModelFile saves m in the canonical form model.MarshalModel defines, the
// format model.ParseModel reads.
// Only a model that passed validation is ever written, so a saved file never
// holds a model that compile would have to reject for schema or evidence
// reasons it could have been told about here.
func writeModelFile(path string, m model.BehavioralModel) error {
	data, err := model.MarshalModel(m)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write behavioral model: %w", err)
	}
	return nil
}

type analyzeOptions struct {
	path, fixturePath, provider, model, endpoint, modelOut string
	yamlOutput, yes                                        bool
}

// parseAnalyzeArgs parses `lineage analyze`'s arguments. Every recognized
// option that takes a value is checked for a missing value explicitly and
// rejected before it ever falls through to being treated as the positional
// <path> - a bare "lineage analyze --fixture" must not silently run against
// a workspace literally named "--fixture". Any other argument starting with
// "-" is rejected outright rather than being treated as a path, for the same
// reason.
func parseAnalyzeArgs(args []string) (analyzeOptions, error) {
	var o analyzeOptions
	strFlags := map[string]*string{
		"--fixture":   &o.fixturePath,
		"--model-out": &o.modelOut,
		"--provider":  &o.provider,
		"--model":     &o.model,
		"--endpoint":  &o.endpoint,
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch arg {
		case "--yaml":
			o.yamlOutput = true
			continue
		case "--yes":
			o.yes = true
			continue
		}
		if dst, ok := strFlags[arg]; ok {
			if i+1 >= len(args) {
				return analyzeOptions{}, fmt.Errorf("%s requires a value\n%s", arg, analyzeUsage)
			}
			i++
			*dst = args[i]
			continue
		}
		if strings.HasPrefix(arg, "-") {
			return analyzeOptions{}, fmt.Errorf("unknown option %q\n%s", arg, analyzeUsage)
		}
		if o.path != "" {
			return analyzeOptions{}, fmt.Errorf(analyzeUsage)
		}
		o.path = arg
	}
	return o, nil
}

// analysisEnv is the ambient state resolveAnalysisProvider reads, injectable
// so tests need no real terminal or process environment.
type analysisEnv struct {
	getenv      func(string) string
	stdin       io.Reader
	stderr      io.Writer
	interactive bool
}

func stdinIsTerminal() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// firstNonEmpty implements the flag > env > saved-config precedence.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// resolveAnalysisProvider picks the analysis.Provider for this run, with no
// default and no inference: a live provider must be named by flag, env or
// saved config. label is recorded in the report so a fixture-driven test run
// and a real run are never visually indistinguishable in output - see
// AnalysisReport.Provider's doc comment. For a live provider it prints the
// egress notice and requires consent before returning, so nothing is sent
// until the caller has a provider back.
func resolveAnalysisProvider(o analyzeOptions, env analysisEnv) (analysis.Provider, string, error) {
	if o.fixturePath != "" {
		data, err := os.ReadFile(o.fixturePath)
		if err != nil {
			return nil, "", err
		}
		return analysis.FixtureProvider{Response: data}, "fixture:" + o.fixturePath, nil
	}

	// Saved config comes from the analysis target's workspace, not the
	// process working directory: `lineage analyze ~/customer-app` run from
	// another repo must not apply that repo's provider settings.
	var saved config.AnalysisConfig
	found, err := config.FindProjectConfig(filepath.Clean(o.path))
	switch {
	case err == nil:
		saved = found.Config.Analysis
	case !errors.Is(err, config.ErrProjectConfigNotFound):
		return nil, "", err
	}

	// Provider is chosen first; every other setting is then scoped to it. A
	// layer's model/endpoint/key_env only applies when that layer's provider
	// is the one selected (saved values need an exact match, env values may
	// omit the provider), so an --provider override never inherits another
	// provider's saved endpoint or credential variable.
	envProvider := env.getenv("LINEAGE_ANALYSIS_PROVIDER")
	name := firstNonEmpty(o.provider, envProvider, saved.Provider)
	if name == "" {
		return nil, "", fmt.Errorf("no analysis provider selected: pass --provider (one of: %s), set LINEAGE_ANALYSIS_PROVIDER, or use --fixture", strings.Join(analysis.AdapterNames(), ", "))
	}
	adapter, ok := analysis.LookupAdapter(name)
	if !ok {
		return nil, "", fmt.Errorf("unknown analysis provider %q (one of: %s)", name, strings.Join(analysis.AdapterNames(), ", "))
	}
	if saved.Provider != name {
		saved = config.AnalysisConfig{}
	}
	var envModel, envEndpoint string
	if envProvider == "" || envProvider == name {
		envModel, envEndpoint = env.getenv("LINEAGE_ANALYSIS_MODEL"), env.getenv("LINEAGE_ANALYSIS_ENDPOINT")
	}
	model := firstNonEmpty(o.model, envModel, saved.Model)
	if model == "" {
		return nil, "", fmt.Errorf("no model selected for provider %q: pass --model or set LINEAGE_ANALYSIS_MODEL", name)
	}
	endpoint := firstNonEmpty(o.endpoint, envEndpoint, saved.Endpoint, adapter.DefaultEndpoint())
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return nil, "", fmt.Errorf("invalid analysis endpoint %q", endpoint)
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && isLoopback(u.Hostname())) {
		return nil, "", fmt.Errorf("analysis endpoint %q must be https (http is allowed only for localhost)", endpoint)
	}
	keyEnv := firstNonEmpty(saved.KeyEnv, adapter.CredentialEnv())
	key := env.getenv(keyEnv)
	if key == "" {
		return nil, "", fmt.Errorf("no API key for provider %q: set %s", name, keyEnv)
	}

	fmt.Fprintf(env.stderr, "lineage will send this workspace's file contents to provider %q at %s (model %s).\n", name, u.Host, model)
	if !o.yes {
		if !env.interactive {
			return nil, "", fmt.Errorf("not running in a terminal: pass --yes to confirm sending workspace contents to %s", u.Host)
		}
		fmt.Fprint(env.stderr, "Continue? [y/N] ")
		line, _ := bufio.NewReader(env.stdin).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
			return nil, "", fmt.Errorf("analysis cancelled; nothing was sent")
		}
	}
	return analysis.RemoteProvider{Adapter: adapter, Model: model, Endpoint: endpoint, APIKey: key}, name, nil
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
