package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/agentic-lineage/lineage/internal/compile"
	"github.com/agentic-lineage/lineage/internal/inventory"
	"github.com/agentic-lineage/lineage/internal/model"
)

const compileUsage = "usage: lineage compile <model.json> <source-path> --out <dir> [--force] [--yaml]"

// CompileReport is `lineage compile`'s structured output.
type CompileReport struct {
	Package  string   `yaml:"package"`
	Out      string   `yaml:"out"`
	Steps    []string `yaml:"steps"`
	Files    []string `yaml:"files"`
	Requires []string `yaml:"requires_install_yourself,omitempty"`
	Notes    []string `yaml:"notes,omitempty"`
	Result   string   `yaml:"result"`
}

// runCompile is `lineage compile`: turn a saved, author-reviewed behavioral
// model into a provider-neutral package. It makes no LLM call.
func runCompile(_ context.Context, args []string, stdout, stderr io.Writer) error {
	if hasHelpFlag(args) {
		fmt.Fprintln(stdout, compileUsage+"\n\nCompile a behavioral model (from `lineage analyze --model-out`) into a Lineage package. <source-path> is the workspace the model was analyzed from; its files are re-verified against the model's evidence before anything is copied. Refuses while the model has unresolved decisions and prints how to resolve each. Makes no LLM call.")
		return nil
	}
	modelPath, source, out, force, yamlOutput, err := parseCompileArgs(args)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return err
	}

	data, err := os.ReadFile(modelPath)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return err
	}
	m, err := model.ParseModel(data)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return err
	}
	source = filepath.Clean(source)
	inv, err := inventory.Discover(source)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return err
	}

	res, err := compile.Compile(m, source, inv)
	if err != nil {
		fmt.Fprintln(stderr, err)
		fmt.Fprintf(stderr, "\nRe-run after editing: lineage compile %s %s --out %s\n", modelPath, source, out)
		return err
	}
	if _, err := compile.Write(res, out, force); err != nil {
		fmt.Fprintln(stderr, err)
		return err
	}

	report := CompileReport{Package: res.Manifest.Name, Out: out, Requires: res.Requires, Notes: res.Notes, Result: "pass", Steps: res.Workflow.Steps}
	for _, f := range res.Files {
		report.Files = append(report.Files, f.Path)
	}
	if yamlOutput {
		return writeYAML(stdout, report)
	}
	printCompileReport(stdout, report)
	return nil
}

func printCompileReport(w io.Writer, r CompileReport) {
	fmt.Fprintf(w, "compiled package %s to %s\n\nsteps (in order):\n", r.Package, r.Out)
	for _, s := range r.Steps {
		fmt.Fprintf(w, "  - %s\n", s)
	}
	fmt.Fprintf(w, "\nfiles: %d generated (plus lineage.yaml and the workflow)\n", len(r.Files))
	if len(r.Requires) > 0 {
		fmt.Fprintln(w, "\nrequires (install yourself; Lineage installs nothing):")
		for _, q := range r.Requires {
			fmt.Fprintf(w, "  - %s\n", q)
		}
	}
	if len(r.Notes) > 0 {
		fmt.Fprintln(w, "\nnotes to review before publishing:")
		for _, n := range r.Notes {
			fmt.Fprintf(w, "  - %s\n", n)
		}
	}
	fmt.Fprintln(w, "\nvalidation: pass")
}

func parseCompileArgs(args []string) (modelPath, source, out string, force, yamlOutput bool, err error) {
	var positional []string
	for i := 0; i < len(args); i++ {
		switch arg := args[i]; {
		case arg == "--force":
			force = true
		case arg == "--yaml":
			yamlOutput = true
		case arg == "--out":
			if i+1 >= len(args) {
				return "", "", "", false, false, fmt.Errorf("--out requires a value\n%s", compileUsage)
			}
			i++
			out = args[i]
		case strings.HasPrefix(arg, "-"):
			return "", "", "", false, false, fmt.Errorf("unknown option %q\n%s", arg, compileUsage)
		default:
			positional = append(positional, arg)
		}
	}
	if len(positional) != 2 || out == "" {
		return "", "", "", false, false, fmt.Errorf("%s", compileUsage)
	}
	return positional[0], positional[1], out, force, yamlOutput, nil
}
