package compile

import (
	"fmt"
	"strings"

	"github.com/agentic-lineage/lineage/internal/model"
	"gopkg.in/yaml.v3"
)

// renderSkill builds a step's SKILL.md from the model's own summary of the
// step. Source text is never copied; provenance lives in the evidence file.
func renderSkill(s model.Step, bundledTools, bundledRefs, requireLines []string) string {
	name := oneLine(s.Name)
	if name == "" {
		name = s.ID
	}
	description := oneLine(s.Description)
	if description == "" {
		description = name
	}
	front, _ := yaml.Marshal(struct {
		Name        string `yaml:"name"`
		Description string `yaml:"description"`
	}{Name: s.ID, Description: description})

	var b strings.Builder
	b.WriteString("---\n" + string(front) + "---\n\n")
	fmt.Fprintf(&b, "# %s\n\n%s\n", name, description)

	section := func(title string, lines []string) {
		if len(lines) == 0 {
			return
		}
		fmt.Fprintf(&b, "\n## %s\n\n", oneLine(title))
		for _, l := range lines {
			b.WriteString("- " + l + "\n")
		}
	}
	section("Inputs", claimLines(s.Inputs))
	section("Outputs", claimLines(s.Outputs))
	section("Tools", quoteAll(bundledTools))
	section("Requires (install yourself)", requireLines)
	section("References", quoteAll(bundledRefs))
	var setup []string
	for _, n := range s.Setup {
		setup = append(setup, fmt.Sprintf("`%s` (%s): %s", oneLine(n.Path), oneLine(n.Kind), oneLine(n.Description)))
	}
	section("Setup", setup)
	var gates []string
	for _, g := range s.Gates {
		gates = append(gates, oneLine(g.Description))
	}
	section("Validation gates", gates)
	return b.String()
}

// renderEvidence lists, per assertion in the step, where the source supports
// it. It is review-only and is never staged into a provider workspace.
func renderEvidence(s model.Step) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Evidence: %s\n\nReview-only provenance for skill `%s`. Not read at runtime.\n", oneLine(s.Name), s.ID)
	write := func(title string, refs []model.EvidenceRef) {
		fmt.Fprintf(&b, "\n## %s\n\n", oneLine(title))
		if len(refs) == 0 {
			b.WriteString("- no evidence recorded\n")
		}
		for _, r := range refs {
			line := fmt.Sprintf("- %s (%s)", oneLine(evidenceLocation(r)), oneLine(r.Digest))
			if r.Note != "" {
				line += ": " + oneLine(r.Note)
			}
			b.WriteString(line + "\n")
		}
	}
	write("Step", s.Evidence)
	for _, f := range []struct {
		name   string
		claims []model.Claim
	}{{"inputs", s.Inputs}, {"outputs", s.Outputs}, {"skills", s.Skills}, {"tools", s.Tools}, {"references", s.References}} {
		for _, c := range f.claims {
			write(fmt.Sprintf("%s: %s", f.name, c.Value), c.Evidence)
		}
	}
	for _, n := range s.Setup {
		write("setup: "+n.Path, n.Evidence)
	}
	for _, g := range s.Gates {
		write("gate: "+g.ID, g.Evidence)
	}
	return b.String()
}

func renderWorkflowBody(m model.BehavioralModel) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", m.Name)
	if strings.TrimSpace(m.Intent) != "" {
		b.WriteString(oneLine(m.Intent) + "\n\n")
	}
	b.WriteString("## Steps\n\n")
	for i, s := range m.Steps {
		fmt.Fprintf(&b, "%d. `%s`: %s\n", i+1, s.ID, oneLine(s.Name))
	}
	return b.String()
}

func claimLines(claims []model.Claim) []string {
	lines := make([]string, 0, len(claims))
	for _, c := range claims {
		lines = append(lines, "`"+oneLine(c.Value)+"`")
	}
	return lines
}

// oneLine collapses s to a single line and neutralizes backticks. Every model
// string reaches a SKILL.md an agent will read as instructions, so a newline
// or a code fence in a hand-edited or LLM-produced value must not be able to
// open a new heading or list item.
func oneLine(s string) string {
	return strings.ReplaceAll(strings.Join(strings.Fields(s), " "), "`", "'")
}

func quoteAll(values []string) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		out = append(out, "`"+v+"`")
	}
	return out
}

// RefString renders a model.Ref in plain words.
func RefString(r model.Ref) string {
	if r.StepID == "" {
		return "model-level"
	}
	s := fmt.Sprintf("step %s", r.StepID)
	if r.Field != "" {
		s += fmt.Sprintf(" field %s", r.Field)
	}
	if r.Key != "" {
		s += fmt.Sprintf(" key %s", r.Key)
	}
	return s
}

// describeDecision renders one blocking Decision as instructions the author
// can act on: what it is about, where the evidence is (with digests to copy
// into new claims), which JSON to edit, and what to do afterwards.
func describeDecision(m model.BehavioralModel, d model.Decision) string {
	var b strings.Builder
	fmt.Fprintf(&b, "unresolved decision %s: %s\n", d.ID, d.Description)
	if len(d.Refs) == 0 {
		b.WriteString("  Applies to: the whole model\n")
	}
	for _, r := range d.Refs {
		fmt.Fprintf(&b, "  Applies to: %s\n", RefString(r))
		if r.StepID != "" {
			for i, s := range m.Steps {
				if s.ID == r.StepID {
					where := fmt.Sprintf("steps[%d]", i)
					if r.Field != "" {
						where += "." + r.Field
					}
					fmt.Fprintf(&b, "  Edit: %s in the model file\n", where)
				}
			}
		}
	}
	if len(d.Evidence) == 0 {
		b.WriteString("  Evidence: none recorded\n")
	}
	for _, e := range d.Evidence {
		fmt.Fprintf(&b, "  Evidence: %s  digest %s\n", evidenceLocation(e), e.Digest)
	}
	b.WriteString("  To resolve: open the cited location(s), then in the model file add or correct the claim this decision is about\n")
	b.WriteString("  (a claim is {\"value\": ..., \"evidence\": [{\"path\": ..., \"digest\": <digest above>, \"line\": ...}]}),\n")
	b.WriteString("  and delete this entry from \"decisions\". Then re-run the same `lineage compile` command.")
	return b.String()
}
