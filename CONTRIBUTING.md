# Contributing

Bug reports, fixes, and well-scoped features are welcome.

## Before you start

- For anything beyond a straightforward bug fix, open an issue first to discuss the idea. This avoids wasted effort if the direction doesn't fit the project.
- Check open issues and pull requests before starting work — someone may already be on it.

## Development setup

```bash
git clone https://github.com/thomasteoh/llmesh
cd llmesh
go build ./...
go test ./...
```

Requirements: Go 1.26+. Docker is only needed to build or run the published images.

Run the router locally:

```bash
go run ./router/cmd/router -config router/config.yaml.example -state /tmp/state.json
```

## Running tests

Unit tests (with race detection):

```bash
go test -v -race -count=1 ./...
```

End-to-end tests:

```bash
go test -v -timeout 120s ./router/e2e/...
```

Both suites must pass before a PR will be merged.

## Pull requests

- **Keep PRs focused.** One logical change per PR. If you are fixing a bug and spot an unrelated issue, open a separate PR.
- **Start from `master`.** Branch off the latest `master` and target it on submission.
- **Tests are required.** New behaviour needs tests; bug fixes should include a regression test where practical.
- **Document config changes.** If you add a config field, update `config.yaml.example` and the README table.
- **No generated files in the diff.** Do not commit `vendor/` or build artefacts.

### What gets accepted

- Bug fixes with a clear reproduction case
- Performance improvements with a benchmark
- Features that fit the project's scope: routing, scheduling, protocol translation, the admin UI

### What probably won't

- Additional inference backends (the project targets llama.cpp's HTTP API)
- Cloud or managed-service integrations
- Breaking changes to the WebSocket protocol without a migration path

## Commit messages

Follow [Conventional Commits](https://www.conventionalcommits.org/). Keep the subject line under 72 characters and in the imperative mood:

```
feat(scheduler): add weighted round-robin option
fix(hub): prevent race on client disconnect
docs: update client config reference
```

Types: `feat`, `fix`, `refactor`, `perf`, `docs`, `test`, `chore`, `build`, `ci`.

Include a body when the *why* is not obvious from the diff.

## Code style

- Standard `gofmt` formatting. Run `go vet ./...` before submitting.
- No external dependencies added without prior discussion.
- Error strings lowercase, no trailing punctuation (Go convention).

## License

By contributing you agree that your work will be released under the [MIT License](LICENSE).
