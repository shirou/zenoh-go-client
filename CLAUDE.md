# Repo-local guidance for Claude Code

## Do not reference "Phase N" in committed artifacts

Do not use phrases like `Phase 1`, `P1-4`, `Phase 6 follow-up`, or similar
milestone markers in code comments, file names, identifiers, test
fixtures, `Makefile`, compose files, or commit messages.

Planning documents that define these phases (e.g. `docs/implementation-plan.md`,
`docs/testing-plan.md`) may or may not be committed, and the numbering is
internal and liable to change. A reader opening the code should not need
to cross-reference a plan to understand a comment.

When you are tempted to write a phase marker, instead:

- For "not yet implemented" notes: describe the missing behaviour
  directly ("a trie can replace this linear walk once subscriber count
  justifies it") — omit the milestone number.
- For "covered elsewhere" pointers: point to the test file or issue, not
  the phase ("covered by `fault_test.go`", "tracked in #123").
- For commit messages: describe what changed, not where it sits in the
  plan.
