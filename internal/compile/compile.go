// Package compile turns a validated model.BehavioralModel into a
// provider-neutral Lineage package: lineage.yaml, one skill per workflow
// step, a WORKFLOW.md ordering those steps, per-step scripts and references,
// and review-only evidence files.
//
// Compile never calls an LLM and never executes source content. It refuses
// (BlockedError) rather than guessing whenever the model is unresolved or the
// source workspace no longer matches the evidence the model cites. Anything
// it can proceed past but an author should double-check becomes a Note.
package compile

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/agentic-lineage/lineage/internal/inventory"
	"github.com/agentic-lineage/lineage/internal/model"
	"github.com/agentic-lineage/lineage/internal/packages"
)

const (
	largeFileBytes = 256 << 10
	maxListedPaths = 25
)

// absPathPattern flags machine-local absolute paths in copied text; making a
// package free of them is #109's job, this only warns.
var absPathPattern = regexp.MustCompile(`(?:/Users/|/home/|[A-Za-z]:\\Users\\)[^\s"')]+`)

// File is one generated file, relative to the package root.
type File struct {
	Path string
	Data []byte
	Mode fs.FileMode
}

// Result is a compiled package held in memory. Write puts it on disk.
type Result struct {
	Manifest     packages.Manifest
	Workflow     packages.Workflow
	WorkflowBody string
	Files        []File   // skills/ and references/ content, sorted by Path
	Requires     []string // prose-only tools declared for the author to install, "step: tool (evidence)"
	Notes        []string // non-blocking cross-check findings, sorted
}

// BlockedError is returned when the model cannot be compiled. Each problem is
// a self-contained, actionable block of text.
type BlockedError struct {
	Problems []string
}

func (e *BlockedError) Error() string {
	return fmt.Sprintf("compile blocked by %d problem(s):\n\n%s", len(e.Problems), strings.Join(e.Problems, "\n\n"))
}

type compiler struct {
	sourceRoot string
	byPath     map[string]inventory.Entry
	files      map[string]File
	fileSource map[string]string // lowercased generated path -> source path, to detect collisions (also case-only ones)
	fieldUse   map[string]map[string]bool
	cited      map[string]bool
	problems   []string
	notes      []string
	requires   []string
}

// Compile builds the package for m. sourceRoot is the workspace inv was
// discovered from; file content is read from it and re-verified against
// inventory digests before use.
func Compile(m model.BehavioralModel, sourceRoot string, inv inventory.Inventory) (Result, error) {
	report, err := model.Validate(m, inv)
	if err != nil {
		return Result{}, err
	}
	if !report.Passed() {
		var b strings.Builder
		b.WriteString("the model does not validate against the source workspace:\n")
		for _, e := range report.Errors {
			b.WriteString("  - " + e + "\n")
		}
		b.WriteString("If the workspace changed since analysis, re-run `lineage analyze <path> --model-out <file>`; otherwise fix the entries above in the model file.")
		return Result{}, &BlockedError{Problems: []string{b.String()}}
	}

	c := &compiler{
		sourceRoot: sourceRoot,
		byPath:     make(map[string]inventory.Entry, len(inv.Entries)),
		files:      map[string]File{},
		fileSource: map[string]string{},
		fieldUse:   map[string]map[string]bool{},
		cited:      map[string]bool{},
	}
	for _, e := range inv.Entries {
		c.byPath[e.Path] = e
	}

	for _, d := range m.Decisions {
		c.problems = append(c.problems, describeDecision(m, d))
	}
	if len(m.Steps) == 0 {
		c.problems = append(c.problems, "the model has no steps, so there is nothing to compile.")
	}
	if !packages.IsValidIdentifier(m.Name) {
		c.problems = append(c.problems, fmt.Sprintf("model name %q cannot be a package name: it must start with a letter or digit and contain only letters, digits, '.', '_', '-', or '+'. Edit \"name\" in the model file.", m.Name))
	}

	manifest := packages.DefaultManifest(m.Name)
	if strings.TrimSpace(m.Intent) != "" {
		manifest.Description = strings.TrimSpace(m.Intent)
	}
	manifest.Exports.Workflows = []string{m.Name}

	stepIDs := make([]string, 0, len(m.Steps))
	stepSet := map[string]bool{}
	lowerIDs := map[string]string{}
	for i, s := range m.Steps {
		if !packages.IsValidIdentifier(s.ID) {
			c.problems = append(c.problems, fmt.Sprintf("steps[%d] has id %q, which cannot be a skill directory name: use letters, digits, '.', '_', '-', or '+', starting with a letter or digit. Edit \"id\" in the model file.", i, s.ID))
			continue
		}
		if prev, dup := lowerIDs[strings.ToLower(s.ID)]; dup && prev != s.ID {
			c.problems = append(c.problems, fmt.Sprintf("step ids %q and %q differ only by case and would share one directory on a case-insensitive filesystem. Rename one in the model file.", prev, s.ID))
			continue
		}
		lowerIDs[strings.ToLower(s.ID)] = s.ID
		stepIDs = append(stepIDs, s.ID)
		stepSet[s.ID] = true
	}

	requiredSkills := map[string]bool{}
	setupFiles := map[string]packages.SetupFile{}
	setupDirs := map[string]packages.SetupDirectory{}
	gateCount := 0

	for _, s := range m.Steps {
		if !stepSet[s.ID] {
			continue
		}
		c.compileStep(s, stepSet, requiredSkills, setupFiles, setupDirs, &gateCount)
	}

	for name := range requiredSkills {
		manifest.Requires.Skills = append(manifest.Requires.Skills, name)
	}
	sort.Strings(manifest.Requires.Skills)
	for _, p := range sortedKeys(setupFiles) {
		manifest.Setup.Files = append(manifest.Setup.Files, setupFiles[p])
	}
	for _, p := range sortedKeys(setupDirs) {
		manifest.Setup.Directories = append(manifest.Setup.Directories, setupDirs[p])
	}

	c.crossChecks(m, inv, requiredSkills, stepSet, gateCount)

	if len(c.problems) > 0 {
		return Result{}, &BlockedError{Problems: c.problems}
	}

	files := make([]File, 0, len(c.files))
	for _, f := range c.files {
		files = append(files, f)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	sort.Strings(c.notes)
	sort.Strings(c.requires)

	return Result{
		Manifest:     manifest,
		Workflow:     packages.Workflow{Name: m.Name, Steps: stepIDs},
		WorkflowBody: renderWorkflowBody(m),
		Files:        files,
		Requires:     c.requires,
		Notes:        c.notes,
	}, nil
}

func (c *compiler) compileStep(s model.Step, stepSet map[string]bool, requiredSkills map[string]bool, setupFiles map[string]packages.SetupFile, setupDirs map[string]packages.SetupDirectory, gateCount *int) {
	c.citeAll(s)

	var bundledTools, bundledRefs, requireLines []string

	for _, t := range s.Tools {
		e, ok := c.byPath[t.Value]
		if ok && e.Kind == inventory.KindExecutableHelper {
			c.markUse(e.Path, "tools")
			dest := path.Join("skills", s.ID, "scripts", path.Base(e.Path))
			if c.copySource(e, dest, s.ID, "tools", t.Value) {
				bundledTools = append(bundledTools, "scripts/"+path.Base(e.Path))
			}
			continue
		}
		if ok {
			c.markUse(e.Path, "tools")
			c.notes = append(c.notes, fmt.Sprintf("step %s: tool %q is in the source workspace but classified %s, not executable_helper, so it was not bundled; it is listed as a requirement instead.", s.ID, t.Value, e.Kind))
		}
		requireLines = append(requireLines, fmt.Sprintf("`%s` (%s)", oneLine(t.Value), oneLine(evidenceLocations(t.Evidence))))
		c.requires = append(c.requires, fmt.Sprintf("step %s: %s (%s)", s.ID, t.Value, evidenceLocations(t.Evidence)))
	}

	for _, r := range s.References {
		e, ok := c.byPath[r.Value]
		if !ok {
			c.problems = append(c.problems, fmt.Sprintf("step %s: reference %q is not a file in the source workspace, so it cannot be copied into the package.\n  Cited at: %s\n  Fix: in the model file, correct steps[].references \"value\" to a workspace-relative file path, or remove the claim.", s.ID, r.Value, evidenceLocations(r.Evidence)))
			continue
		}
		c.markUse(e.Path, "references")
		if e.Kind == inventory.KindExecutableHelper {
			c.notes = append(c.notes, fmt.Sprintf("step %s: reference %q is classified executable_helper; it was copied as a reference, not as a tool.", s.ID, r.Value))
		}
		dest := path.Join("skills", s.ID, "references", path.Base(e.Path))
		if c.copySource(e, dest, s.ID, "references", r.Value) {
			bundledRefs = append(bundledRefs, "references/"+path.Base(e.Path))
		}
	}

	for _, sk := range s.Skills {
		requiredSkills[sk.Value] = true
	}

	for _, need := range s.Setup {
		if _, err := packages.SafeJoin(c.sourceRoot, need.Path); err != nil {
			c.problems = append(c.problems, fmt.Sprintf("step %s: setup path %q is unsafe (%v). Edit steps[].setup \"path\" in the model file.", s.ID, need.Path, err))
			continue
		}
		if e, ok := c.byPath[need.Path]; ok {
			c.markUse(e.Path, "setup")
			if e.Kind != inventory.KindSetupMaterial {
				c.notes = append(c.notes, fmt.Sprintf("step %s: setup path %q is classified %s, not setup_material.", s.ID, need.Path, e.Kind))
			}
		}
		switch need.Kind {
		case "file":
			if _, dup := setupDirs[need.Path]; dup {
				c.problems = append(c.problems, fmt.Sprintf("setup path %q is declared as both a file and a directory. Fix \"kind\" in the model file.", need.Path))
				continue
			}
			if _, dup := setupFiles[need.Path]; !dup {
				setupFiles[need.Path] = packages.SetupFile{Path: need.Path, Description: need.Description}
			}
		case "directory":
			if _, dup := setupFiles[need.Path]; dup {
				c.problems = append(c.problems, fmt.Sprintf("setup path %q is declared as both a file and a directory. Fix \"kind\" in the model file.", need.Path))
				continue
			}
			if _, dup := setupDirs[need.Path]; !dup {
				setupDirs[need.Path] = packages.SetupDirectory{Path: need.Path, Description: need.Description}
			}
		default:
			c.problems = append(c.problems, fmt.Sprintf("step %s: setup path %q has kind %q; it must be \"file\" or \"directory\". Fix \"kind\" in the model file.", s.ID, need.Path, need.Kind))
		}
	}
	*gateCount += len(s.Gates)

	c.addFile(File{Path: path.Join("skills", s.ID, "SKILL.md"), Data: []byte(renderSkill(s, bundledTools, bundledRefs, requireLines)), Mode: 0o644}, "")
	c.addFile(File{Path: path.Join("references", "evidence", s.ID+".md"), Data: []byte(renderEvidence(s)), Mode: 0o644}, "")
}

// copySource reads e from the workspace, re-verifies its digest, and stages
// it at dest. It reports whether the file was staged.
func (c *compiler) copySource(e inventory.Entry, dest, stepID, field, claim string) bool {
	full, err := packages.SafeJoin(c.sourceRoot, e.Path)
	if err != nil {
		c.problems = append(c.problems, fmt.Sprintf("step %s: %s %q has an unsafe path (%v).", stepID, field, claim, err))
		return false
	}
	info, err := os.Lstat(full)
	if err != nil {
		c.problems = append(c.problems, fmt.Sprintf("step %s: cannot read %s %q: %v. Re-run `lineage analyze` if the workspace changed.", stepID, field, claim, err))
		return false
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		c.problems = append(c.problems, fmt.Sprintf("step %s: %s %q is not a regular file (symlinks are never followed).", stepID, field, claim))
		return false
	}
	data, err := os.ReadFile(full)
	if err != nil {
		c.problems = append(c.problems, fmt.Sprintf("step %s: cannot read %s %q: %v", stepID, field, claim, err))
		return false
	}
	sum := sha256.Sum256(data)
	if got := "sha256:" + hex.EncodeToString(sum[:]); got != e.Digest {
		c.problems = append(c.problems, fmt.Sprintf("step %s: %s %q changed since the workspace was inventoried (digest %s, expected %s). Re-run `lineage analyze <path> --model-out <file>`.", stepID, field, claim, got, e.Digest))
		return false
	}
	if prev, ok := c.fileSource[strings.ToLower(dest)]; ok && prev != e.Path {
		c.problems = append(c.problems, fmt.Sprintf("step %s: %q and %q would both be copied to %s. Rename one in the source workspace or drop one claim.", stepID, prev, e.Path, dest))
		return false
	}
	mode := fs.FileMode(0o644)
	if info.Mode()&0o111 != 0 {
		mode = 0o755
	}
	c.addFile(File{Path: dest, Data: data, Mode: mode}, e.Path)
	if len(data) > largeFileBytes {
		c.notes = append(c.notes, fmt.Sprintf("%s is %d bytes; large files add package weight.", dest, len(data)))
	}
	if bytes.IndexByte(data, 0) >= 0 {
		c.notes = append(c.notes, fmt.Sprintf("%s looks binary; it was copied unchanged.", dest))
	} else if loc := absPathPattern.Find(data); loc != nil {
		c.notes = append(c.notes, fmt.Sprintf("%s contains a machine-local path (%s); it will not be portable until sanitized.", dest, string(loc)))
	}
	return true
}

func (c *compiler) addFile(f File, source string) {
	c.files[f.Path] = f
	if source != "" {
		c.fileSource[strings.ToLower(f.Path)] = source
	}
}

func (c *compiler) markUse(p, field string) {
	if c.fieldUse[p] == nil {
		c.fieldUse[p] = map[string]bool{}
	}
	c.fieldUse[p][field] = true
	c.cited[p] = true
}

func (c *compiler) citeAll(s model.Step) {
	for _, e := range s.Evidence {
		c.cited[e.Path] = true
	}
	for _, claims := range [][]model.Claim{s.Inputs, s.Outputs, s.Skills, s.Tools, s.References} {
		for _, cl := range claims {
			c.cited[cl.Value] = true
			for _, e := range cl.Evidence {
				c.cited[e.Path] = true
			}
		}
	}
	for _, n := range s.Setup {
		for _, e := range n.Evidence {
			c.cited[e.Path] = true
		}
	}
	for _, g := range s.Gates {
		for _, e := range g.Evidence {
			c.cited[e.Path] = true
		}
	}
}

func (c *compiler) crossChecks(m model.BehavioralModel, inv inventory.Inventory, requiredSkills, stepSet map[string]bool, gateCount int) {
	for p, fields := range c.fieldUse {
		if len(fields) > 1 {
			c.notes = append(c.notes, fmt.Sprintf("%s is cited under more than one field (%s).", p, strings.Join(sortedKeys(fields), ", ")))
		}
	}

	skillDirs := map[string]string{}
	for _, e := range inv.Entries {
		if path.Base(e.Path) == "SKILL.md" {
			skillDirs[path.Base(path.Dir(e.Path))] = e.Path
		}
	}
	for _, name := range sortedKeys(requiredSkills) {
		if stepSet[name] {
			c.notes = append(c.notes, fmt.Sprintf("required skill %q has the same name as a generated step skill; the generated one is used.", name))
		} else if src, ok := skillDirs[name]; ok {
			c.notes = append(c.notes, fmt.Sprintf("required skill %q exists in the source workspace (%s) but is not bundled; receivers must have it installed.", name, src))
		}
	}

	var missing []string
	for _, e := range inv.Entries {
		if !c.cited[e.Path] {
			missing = append(missing, fmt.Sprintf("%s (%s)", e.Path, e.Kind))
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		shown := missing
		extra := ""
		if len(shown) > maxListedPaths {
			extra = fmt.Sprintf(" and %d more", len(shown)-maxListedPaths)
			shown = shown[:maxListedPaths]
		}
		c.notes = append(c.notes, fmt.Sprintf("not represented in package (no claim or evidence names them; the model cannot yet express agents, MCP dependencies, or capabilities): %s%s.", strings.Join(shown, ", "), extra))
	}

	if gateCount > 0 {
		c.notes = append(c.notes, fmt.Sprintf("%d validation gate(s) are described in SKILL.md files but not enforced (#109/#113).", gateCount))
	}
	for _, f := range c.files {
		if strings.Contains(f.Path, "/scripts/") || (strings.Contains(f.Path, "/references/") && !strings.HasPrefix(f.Path, "references/")) {
			c.notes = append(c.notes, "skills with scripts/ or references/ files cannot be materialized by the Cursor adapter, which supports only SKILL.md.")
			break
		}
	}
}

// evidenceLocations renders refs as "path:line, path" for human-readable
// report and SKILL.md text.
func evidenceLocations(refs []model.EvidenceRef) string {
	if len(refs) == 0 {
		return "no evidence"
	}
	parts := make([]string, 0, len(refs))
	for _, r := range refs {
		parts = append(parts, evidenceLocation(r))
	}
	return strings.Join(parts, ", ")
}

func evidenceLocation(r model.EvidenceRef) string {
	if r.Line > 0 {
		return fmt.Sprintf("%s:%d", r.Path, r.Line)
	}
	return r.Path
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
