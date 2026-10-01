# `.githooks/`

Hooks here run on a contributor's or maintainer's machine during an ordinary git
operation. A diff under this directory is a code-execution surface: **review it
like a workflow change.**

The directory currently holds one hook, `prepare-commit-msg` (which adds the DCO
`Signed-off-by` trailer). The default answer to a new hook is no. A hook may be
added only if it meets every criterion below.

1. **It serves a control CI already enforces.** A hook is convenience for an
   existing gate, never a gate of its own: a fresh clone does not have it, and a
   hook missing its exec bit is skipped without warning. Today that control is the
   required `DCO` check, plus `Commit identity routable`.
2. **It cannot block or fail a commit, and is idempotent.**
3. **It does not read or execute the working tree's code.** No linters,
   formatters, tests, or codegen. Those belong in CI, where the code is not
   running on a reviewer's machine.
4. **It ships with a self-test**, following `prepare-commit-msg.test.sh`.
5. **It is installed by copy from a trusted ref** (`make hooks`, default
   `origin/main`). Never `core.hooksPath` into the tracked directory, never a
   symlink, never read from the working tree. The reasoning is in the header of
   `prepare-commit-msg`.

See [Sign Off Automatically](../.github/CONTRIBUTING.md#sign-off-automatically)
for installing the hook.
