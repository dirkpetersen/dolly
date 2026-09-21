# Development

## Building from source

```bash
git clone https://github.com/dirkpetersen/dolly.git
cd dolly
go build -o dolly ./cmd/dolly
go test ./...
```

Requires Go 1.22 or later. Built on [go-ldap/ldap](https://github.com/go-ldap/ldap).

To run a single test:

```bash
go test ./path/to/pkg -run TestName
```

## CI

`.github/workflows/ci.yml` runs on every push and pull request that touches Go code, `go.mod`/`go.sum`, or `.goreleaser.yaml`:

- `gofmt -l` (fails if any file isn't formatted)
- `go vet ./...`
- `go test -race ./...`
- `go build ./cmd/dolly`
- a GoReleaser config check (`goreleaser check`)

## Releasing

To cut a release, push a semver tag:

```bash
git tag -a v0.1.0 -m "v0.1.0"
git push origin v0.1.0
```

`.github/workflows/release.yml` runs the tests and then [GoReleaser](https://goreleaser.com) (configured in `.goreleaser.yaml`), which publishes static, `CGO_ENABLED=0` binaries for Linux and macOS (amd64 and arm64), `checksums.txt`, and a changelog to the GitHub release. Each archive also contains `LICENSE`, `README.md`, and `dolly.yaml.template`. `dolly version` prints the tag, commit, and build date, set at build time via `-ldflags -X main.…`.

## Building the docs locally

This site is built with [Zensical](https://zensical.org), a Material-for-MkDocs-compatible static site generator, from the Markdown in `docs/`.

```bash
python3 -m venv .venv
.venv/bin/pip install zensical
.venv/bin/zensical serve   # live preview at http://localhost:8000
```

To produce the static output the same way CI does:

```bash
.venv/bin/zensical build --clean --strict
```

Docs publish automatically to [https://dirkpetersen.github.io/dolly/](https://dirkpetersen.github.io/dolly/) via `.github/workflows/docs.yml` whenever `docs/**` or `zensical.toml` changes on `main`.

## Contributing

Issues and pull requests are welcome. Please include a dry-run output or a minimal LDIF example when reporting mapping bugs, with anything sensitive redacted.

## License

MIT. See [LICENSE](https://github.com/dirkpetersen/dolly/blob/main/LICENSE).
