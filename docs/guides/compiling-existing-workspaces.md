# Compiling An Existing Agent Workspace

Lineage's package compiler is being built for workspaces that already contain
agent behavior but are not yet a clean Lineage package: `CLAUDE.md`,
`AGENTS.md`, `SKILL.md`, scripts, references, setup notes, and informal
workflow instructions.

Two commands implement the flow: `lineage analyze <path> --model-out
model.json` (agent-assisted analysis) and `lineage compile model.json <path>
--out <dir>` (package generation, no LLM call).

## Why Copying Files Is Not Enough

An existing workspace can be structurally complete and still be behaviorally
unclear. A script may be mentioned only from a skill, a setup file may be
implicit, and workflow order may be spread across several instructions. A
portable package must preserve the important behavior, not merely reproduce the
author's folders.

## Current Pipeline

1. **Evidence inventory (#203):** `internal/inventory` walks the source tree
   read-only, classifies files, hashes them, and records literal Markdown
   references between files. It rejects symlinks and skips common dependency,
   build, and cache directories.
2. **Behavioral model (#103):** `internal/model` converts inventory evidence
   into a versioned, provider-neutral `BehavioralModel` — ordered steps, each
   with typed `Claim`s for inputs, outputs, required skills, tools, and
   references, plus setup needs and validation gates. Every claim carries its
   own evidence back into the inventory; unresolved or ambiguous behavior
   becomes an explicit `Decision` rather than a guess. See
   [ADR 0018](../decisions/0018-behavioral-model-is-a-versioned-evidence-linked-schema.md)
   and the mapping to package concepts below.
3. **Agent-assisted analysis (#104):** `lineage analyze` asks a provider to
   resolve what literal matching cannot answer, such as an instruction that
   says “run the deploy script” without naming a path. The provider reports
   ambiguity as a `Decision` rather than guessing. `--model-out` saves the
   full model as indented JSON for the author to review and edit.
4. **Artifact compilation (#106):** `lineage compile` generates a
   provider-neutral Lineage package from the saved model (`internal/compile`).
   Provider-specific projection (`CLAUDE.md`, `AGENTS.md`) is #107/#108.
5. **Portability and behavior validation (#109, #113):** redact or
   parameterize machine-local assumptions and prove that generated artifacts
   still represent modeled workflow steps.

## Behavioral Model To Package Concepts

`internal/model` defines this correspondence; `internal/compile` (#106)
executes it. A **step** is one ordered stage of the workflow (for example
`lint`, then `deploy`). Each step becomes one generated skill directory named
by `Step.ID` and one entry in `WORKFLOW.md`. A step is not the same as a
`SKILL.md` in the source workspace, and `Step.Skills` are dependencies on other
skills, not the step's own skill.

| Model field | Manifest concept | Notes |
|---|---|---|
| `Step.Skills[].Value` (deduped across steps) | `Requires.Skills` | Steps are source of truth; #106 flattens/unions the claim values |
| `Step.Tools[].Value` | `skills/<step-id>/scripts/<basename>` when the value is an `executable_helper` file in the workspace; otherwise a "Requires (install yourself)" section in that step's `SKILL.md` and the compile report | Files are re-verified against their inventory digest before copying. Lineage never installs or runs anything; a preflight check is #109 |
| `Step.References[].Value` | `skills/<step-id>/references/<basename>` | Copied content; per-step because the materializer only stages whole `skills/<name>/` directories |
| `Step.Setup` | `Setup.Files` / `Setup.Directories` | Field shapes already match (`Path`, `Description`) |
| Whole `Steps` sequence | `WORKFLOW.md` frontmatter `steps:` (`packages.Workflow.Steps`) | `Step.ID`/`Name` becomes the ordered skill-name list `packages.Workflow` expects today |
| `BehavioralModel.Name`/`Intent` | `Manifest.Name`/`Description`, an `Exports.Workflows` entry | |
| `Step.Inputs[]`, `Step.Outputs[]` | sections in the step's `SKILL.md` | No manifest field |
| `Decision` (unresolved) | none | `lineage compile` refuses while any remain and prints how to resolve each (see below) |
| `Gate` | a "Validation gates" section in the step's `SKILL.md` | Described, not enforced; a runner is #109/#113 |
| Evidence on every claim | `references/evidence/<step-id>.md` | Review-only provenance (`path:line` plus digest); digested and exported with the package, never staged into a provider workspace |

## Author Workflow

1. `lineage analyze ./workspace --model-out model.json` (needs a provider key,
   see below). Read the report: inventory, inferred steps, decisions needing
   review.
2. Close each decision by editing `model.json`. `lineage compile` prints, per
   blocking decision, what it applies to, the `path:line` evidence with
   digests to copy into a new claim, and the JSON path to edit. Delete the
   decision once its claim is fixed.
3. `lineage compile model.json ./workspace --out ./pkg`. The package is built
   in a staging directory, checked with `lineage package validate` (including
   the secret scan), then moved into place. `--force` replaces only an
   existing Lineage package.
4. Read the notes before publishing.

`lineage compile` needs no key. `lineage analyze` currently calls the
Anthropic Messages API with `ANTHROPIC_API_KEY` and sends workspace content to
Anthropic; `--fixture` runs without credentials.

## Errors And Notes

Compile refuses (errors) on: unresolved decisions; a model that no longer
validates against the workspace; a cited file whose digest changed; zero
steps; invalid step ids or package name; unsafe paths, symlinks, or two files
copied to the same destination; a generated package failing validation.

Notes never block: a tool or reference whose inventory kind disagrees with how
the model used it; workspace files no claim or evidence mentions ("not
represented in package"); required skills that shadow a step or exist in the
workspace but are not bundled; one file cited under several fields; large,
binary, or machine-path-bearing copied files; gates that are described but not
enforced; and the Cursor adapter's SKILL.md-only limit for skills with
`scripts/` or `references/`.

## Known Model Gaps

The model has no fields for agents, policies, MCP dependencies, or
capabilities, and entrypoints are provider-specific (#107/#108). Source files
for these appear under "not represented in package" rather than vanishing
silently.

## Boundaries

- The inventory is evidence, not a complete call graph or an LLM judgment.
- Analysis must never execute source scripts.
- Unknown or ambiguous files are intentional outputs for later review; they
  must not be silently classified as safe or irrelevant.
- Generated packages still pass the normal validation, digest, import, setup,
  and materialization controls.

See [ADR 0016](../decisions/0016-prioritize-package-distribution-and-behavioral-compilation.md) and [the architecture](../architecture.md) for the current product boundary.
