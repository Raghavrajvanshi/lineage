package compile

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/agentic-lineage/lineage/internal/packages"
)

// Write puts res on disk at outDir. It builds the package in a staging
// directory next to outDir, runs packages.Validate (which includes the secret
// scan) on it, and only then moves it into place, so a failed or interrupted
// compile never leaves a broken or secret-bearing package at outDir.
//
// An existing non-empty outDir is refused unless force is set, and force only
// replaces a directory that already holds a lineage.yaml, never an arbitrary
// directory.
func Write(res Result, outDir string, force bool) (packages.ValidateReport, error) {
	outDir = filepath.Clean(outDir)
	existing, err := os.ReadDir(outDir)
	dirExists := err == nil
	switch {
	case err == nil && len(existing) > 0:
		if !force {
			return packages.ValidateReport{}, fmt.Errorf("%s already exists and is not empty; pass --force to replace an existing package", outDir)
		}
		if _, statErr := os.Stat(filepath.Join(outDir, packages.ManifestFileName)); statErr != nil {
			return packages.ValidateReport{}, fmt.Errorf("%s is not empty and holds no %s; refusing to replace a directory that is not a Lineage package", outDir, packages.ManifestFileName)
		}
	case err != nil && !os.IsNotExist(err):
		return packages.ValidateReport{}, fmt.Errorf("inspect %s: %w", outDir, err)
	}

	parent := filepath.Dir(outDir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return packages.ValidateReport{}, fmt.Errorf("create %s: %w", parent, err)
	}
	stage, err := os.MkdirTemp(parent, ".lineage-compile-*")
	if err != nil {
		return packages.ValidateReport{}, fmt.Errorf("create staging directory: %w", err)
	}
	defer os.RemoveAll(stage)

	if err := packages.SaveManifest(stage, res.Manifest); err != nil {
		return packages.ValidateReport{}, err
	}
	if err := packages.SaveWorkflow(stage, res.Workflow, res.WorkflowBody); err != nil {
		return packages.ValidateReport{}, err
	}
	for _, f := range res.Files {
		dest, err := packages.SafeJoin(stage, f.Path)
		if err != nil {
			return packages.ValidateReport{}, fmt.Errorf("generated path %q: %w", f.Path, err)
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return packages.ValidateReport{}, fmt.Errorf("create directory for %s: %w", f.Path, err)
		}
		if err := os.WriteFile(dest, f.Data, f.Mode); err != nil {
			return packages.ValidateReport{}, fmt.Errorf("write %s: %w", f.Path, err)
		}
		if err := os.Chmod(dest, f.Mode); err != nil {
			return packages.ValidateReport{}, fmt.Errorf("set mode on %s: %w", f.Path, err)
		}
	}

	report, err := packages.Validate(stage)
	if err != nil {
		return report, fmt.Errorf("validate generated package: %w", err)
	}
	if !report.Passed() {
		return report, fmt.Errorf("generated package failed validation with %d error(s); nothing was written to %s", len(report.Errors), outDir)
	}

	// The old package is moved aside, not deleted, until the new one is in
	// place, so a failed rename leaves the author's existing package intact.
	backup := stage + ".old"
	if len(existing) > 0 {
		if err := os.Rename(outDir, backup); err != nil {
			return report, fmt.Errorf("set aside existing package %s: %w", outDir, err)
		}
	} else if dirExists {
		_ = os.Remove(outDir)
	}
	if err := os.Rename(stage, outDir); err != nil {
		if len(existing) > 0 {
			_ = os.Rename(backup, outDir)
		}
		return report, fmt.Errorf("move package into place: %w", err)
	}
	if len(existing) > 0 {
		_ = os.RemoveAll(backup)
	}
	return report, nil
}

// CheckOutputPath refuses an output directory that overlaps the source
// workspace: the same directory, an ancestor of it, or a directory inside it.
// --force replaces an existing output package, so an overlapping target would
// let a compile delete the workspace it was analyzing, and a target inside the
// workspace would feed the generated package back into the next analysis.
// Paths are compared after resolving symlinks, and by file identity, so a
// symlink alias or a differently cased spelling of the same directory is caught
// too. An output that does not exist yet is resolved through its nearest
// existing ancestor.
func CheckOutputPath(source, out string) error {
	src, err := canonicalPath(source)
	if err != nil {
		return err
	}
	dst, err := canonicalPath(out)
	if err != nil {
		return err
	}
	srcInfo, err := os.Stat(src)
	if err != nil {
		return nil
	}
	dstInfo, dstErr := os.Stat(dst)

	if dstErr == nil {
		for p := src; ; p = filepath.Dir(p) {
			if info, err := os.Stat(p); err == nil && os.SameFile(info, dstInfo) {
				return fmt.Errorf("output %s contains or is the source workspace %s; choose an output directory outside the workspace", out, source)
			}
			if filepath.Dir(p) == p {
				break
			}
		}
	}
	for p := dst; ; p = filepath.Dir(p) {
		if info, err := os.Stat(p); err == nil && os.SameFile(info, srcInfo) {
			return fmt.Errorf("output %s is inside the source workspace %s; choose an output directory outside the workspace", out, source)
		}
		if filepath.Dir(p) == p {
			break
		}
	}
	return nil
}

// canonicalPath returns p as an absolute path with symlinks resolved. The part
// of p that does not exist yet is appended unresolved to its nearest existing
// ancestor.
func canonicalPath(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", p, err)
	}
	rest := ""
	for cur := abs; ; {
		resolved, err := filepath.EvalSymlinks(cur)
		if err == nil {
			return filepath.Join(resolved, rest), nil
		}
		if !os.IsNotExist(err) {
			return "", fmt.Errorf("resolve %s: %w", p, err)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return abs, nil
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}
