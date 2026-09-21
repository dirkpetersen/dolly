# Contributing to Dolly

This guide covers contributing to Dolly using [Claude Code](https://docs.anthropic.com/en/docs/claude-code/overview), which is how this project is developed day to day. You can of course also work without it, using the plain `git`/`go`/`gh` commands shown below.

## Prerequisites

- **Claude Code.** See the [overview](https://docs.anthropic.com/en/docs/claude-code/overview). Install with `npm install -g @anthropic-ai/claude-code` (see the docs for other installers), then run `claude` from the repo root to start it.
- **GitHub CLI.** See [cli.github.com](https://cli.github.com/), then run `gh auth login`.
- **Go 1.22 or later.** See [go.dev/dl](https://go.dev/dl/).
- **Optional, for docs changes.** Python and [Zensical](https://zensical.org), as described in [docs/development.md](docs/development.md).

## Fork and clone

```bash
gh repo fork dirkpetersen/dolly --clone
cd dolly
```

`gh repo fork --clone` creates your fork, clones it, and sets up both remotes for you: `origin` points at your fork, `upstream` at `dirkpetersen/dolly`. Push your work to `origin`; open pull requests against `upstream`.

Keep your fork current before starting new work:

```bash
gh repo sync
# or: git fetch upstream && git merge upstream/main
```

## How this project is developed with Claude Code

- **Spec first.** `README.md` is the spec: CLI surface, config schema, and sync algorithm. `docs/` is the user-facing version of the same material. `CLAUDE.md` holds the binding rules Claude Code loads automatically at the start of every session. A behavior change updates `README.md`, `docs/`, `dolly.yaml.template`, and `CLAUDE.md` (where relevant) in the same commit.
- **Model routing** (from `CLAUDE.md`): coding is delegated to a background agent on Opus, documentation changes (`docs/`, README prose) to an agent on Sonnet, and every commit and push is preceded by a review from an agent on Fable. Fable's findings are fixed before committing, not noted for later.
- **Safety habits:**
  - Test changes against real AD/LDAP servers with `--dry-run` first.
  - Never commit `dolly.yaml`, `*.secret` files, or editor swap files (`*.swp`, `*~`) — they're git-ignored because they can hold passwords.
  - Tests must never touch the real `$HOME`, `systemctl`, or a live LDAP/SMTP server; use the fakes (`internal/ldapfake`, `internal/smtpfake`) or a build-tagged integration test against the CI/local OpenLDAP container.
  - The planner (AD snapshot, target snapshot, ownership records, config → plan) stays a pure function with no I/O. Every ownership rule in README's "Ownership records" gets its own table-driven test.
  - Integration tests for the target side run in CI against a real OpenLDAP container behind the `integration` build tag.
- **Checks before committing:**

  ```bash
  gofmt -l .
  go vet ./...
  go test -race -count=1 ./...
  ```

  For documentation changes: `zensical build --clean --strict` (see [docs/development.md](docs/development.md)).

## Making a change with Claude Code, step by step

1. Create a branch:

   ```bash
   git switch -c fix/short-name
   ```

2. Start Claude Code and describe the change or bug in plain language:

   ```bash
   claude
   ```

   Claude Code reads `CLAUDE.md` automatically, delegates the coding and documentation work to the right models, and runs the checks above.

3. Ask it to review before committing, for example: *"have Fable review the diff before we commit."*

4. Ask it to commit and open a pull request, for example: *"commit this and open a pull request against dirkpetersen/dolly."* Under the hood, Claude Code runs the same commands you'd run by hand:

   ```bash
   git push -u origin HEAD
   gh pr create --repo dirkpetersen/dolly --fill
   # or: gh pr create --repo dirkpetersen/dolly --title "..." --body "..."
   ```

5. Optionally, run `/code-review` in Claude Code for an additional review pass beyond the pre-commit Fable review.

Example prompt sequence for a typical bug fix:

```
> dolly check reports the SMTP port wrong when start_tls is false and the port is 465;
  it should treat 465 as implicit TLS. Fix it and update the docs if the behavior
  described there is wrong.

> have Fable review the diff before we commit

> commit this and open a pull request against dirkpetersen/dolly
```

## Pull request expectations

CI (`.github/workflows/ci.yml`) runs on every pull request that touches Go code, `go.mod`/`go.sum`, or `.goreleaser.yaml`:

- `gofmt -l` (fails if anything is unformatted)
- `go vet ./...` and `go vet -tags integration ./...`
- `go test -race ./...`
- `go build ./cmd/dolly`
- a GoReleaser config check (`goreleaser check`)
- an `integration` job that runs the target-writer tests against a real OpenLDAP container

Beyond CI passing:

- Keep the spec (`README.md`), `docs/`, and `dolly.yaml.template` in sync with any behavior change, in the same commit.
- Describe dry-run evidence in the PR description when the change affects sync behavior (`dolly sync --dry-run` or `dolly check` output), with hostnames and credentials redacted.
- Keep pull requests small and focused on one change.

## Reporting bugs

Open an issue. For mapping or sync bugs, include `dolly --dry-run` output or a minimal LDIF example showing the problem, with anything sensitive (hostnames, DNs, credentials) redacted.

## License

Contributions are accepted under the MIT license. See [LICENSE](LICENSE).
