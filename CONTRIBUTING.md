# Contributing to Cadre

Thank you for taking the time to contribute. Cadre is at the design stage, so the most valuable contributions right now are design review and discussion.

## Ways to contribute

- **Design review.** Read the documents under [docs/](docs/) and open an issue for anything unclear, inconsistent or wrong. Point at the file and the paragraph.
- **Bug reports and feature requests.** Use the issue templates.
- **Pull requests.** For anything larger than a typo, open an issue first so the approach is agreed before the work is done.

## Pull requests

- Keep each pull request to one logical change.
- Write the description for someone who has not read the issue: what changes, why, and how it was verified.
- Design documents are written in plain English, with one idea per sentence. Prefer a table to a paragraph when items are parallel.
- Code must pass `make check` (format, vet, lint, tests) before review.

## Development

Requirements: Go (see `go.mod`), `make`, a local Kubernetes (Docker Desktop, OrbStack or kind) and MinIO for object storage once M0 lands.

```sh
make build      # build all binaries into bin/
make test       # run unit tests
make check      # gofmt, go vet, golangci-lint and tests
```

## Commit messages

Use the conventional prefix that matches the change: `feat`, `fix`, `docs`, `refactor`, `test`, `build`, `ci`, `chore`. The subject line is imperative and under 72 characters. The body explains why, not what.

## Design principles

Changes must respect the principles in the [README](README.md#design-principles). In particular:

- The core depends on three storage operations only. A change that needs a multi-object transaction or a query only SQL can answer belongs in a driver, not in the core.
- Every state transition is a conditional write that carries a run id.
- Anything outside task orchestration and the agent runner is a plugin: an MCP tool server or a separate controller.

## Code of Conduct

This project follows the [Contributor Covenant](CODE_OF_CONDUCT.md). Report unacceptable behaviour through the contact given there.

## License

By contributing you agree that your contributions are licensed under the [Apache License 2.0](LICENSE).
