# Development

## Building from source

```bash
git clone https://github.com/dirkpetersen/dolly.git
cd dolly
go build -o dolly ./cmd/dolly
go test ./...
```

Requires Go 1.22 or later to build: CI's `go-min` job builds and vets with the Go version from `go.mod` (1.22.x), while the tests, the integration tests, and release builds use the latest stable Go. Built on [go-ldap/ldap](https://github.com/go-ldap/ldap).

To run a single test:

```bash
go test ./path/to/pkg -run TestName
```

## CI

`.github/workflows/ci.yml` runs on every push and pull request that touches Go code, `go.mod`/`go.sum`, or `.goreleaser.yaml`:

- `gofmt -l` (fails if any file isn't formatted)
- `go vet ./...` and `go vet -tags integration ./...`
- `go test -race ./...`
- `go build ./cmd/dolly`
- a GoReleaser config check (`goreleaser check`)
- an `integration` job (see below)

## Integration tests

The target writer — apply, the run lock, and `cn=status` — is also tested end to end against a real OpenLDAP server, behind the `integration` build tag (`internal/target/integration_test.go`). These tests don't run with a plain `go test ./...`.

To run them locally, point a scratch OpenLDAP server's connection details at these environment variables and pass `-tags integration`:

```bash
export DOLLY_IT_URL=ldap://localhost:389
export DOLLY_IT_BIND_DN=cn=admin,dc=example,dc=org
export DOLLY_IT_PASSWORD=admin
export DOLLY_IT_BASE=dc=example,dc=org
go test -tags integration ./...
```

The server needs the core, cosine, and either `nis` (RFC 2307) or `rfc2307bis` schema. By default groups are tested as `groupOfNames` + `posixGroup`, which needs `rfc2307bis`; set `DOLLY_IT_SCHEMA=rfc2307` to test `memberUid`-only groups instead, against a server with `nis.schema`. Each test works in its own `ou=dolly-it-<n>` subtree under `DOLLY_IT_BASE` and deletes it afterwards, so a shared scratch server is safe to reuse. If the four required variables above aren't set, the tests skip themselves quietly; setting `DOLLY_IT_REQUIRED` (as CI does) turns that into a failure instead, so a misconfigured CI job can't pass by silently skipping.

In CI, the `integration` job in `ci.yml` starts an `osixia/openldap` service container configured with the `rfc2307bis` schema and runs `go test -race -count=1 -tags integration ./...` against it.

## Releasing

To cut a release, push a semver tag:

```bash
git tag -a v0.1.0 -m "v0.1.0"
git push origin v0.1.0
```

`.github/workflows/release.yml` runs the tests and then [GoReleaser](https://goreleaser.com) (configured in `.goreleaser.yaml`), which publishes static, `CGO_ENABLED=0` binaries for Linux and macOS (amd64 and arm64), `checksums.txt`, and a changelog to the GitHub release. Each archive also contains `LICENSE`, `README.md`, and `dolly.yaml.template`. Release binaries are built with the latest stable Go (`actions/setup-go` with `go-version: stable` and `check-latest: true`), so they carry the current standard library fixes (`crypto/tls`, `crypto/x509`, and so on); `go.mod` keeps `go 1.22` only as the minimum. `dolly version` prints the tag, commit, and build date, set at build time via `-ldflags -X main.…`, and the Go version the binary was built with.

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

Issues and pull requests are welcome. Please include a dry-run output or a minimal LDIF example when reporting mapping bugs, with anything sensitive redacted. See [CONTRIBUTING.md](https://github.com/dirkpetersen/dolly/blob/main/CONTRIBUTING.md) for how to set up a fork and how this project is developed with Claude Code.

## License

MIT. See [LICENSE](https://github.com/dirkpetersen/dolly/blob/main/LICENSE).
