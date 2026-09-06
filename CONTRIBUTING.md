# Contributing to Topos

Topos welcomes focused bug fixes, documentation improvements, and proposals that
strengthen its Go package surface.

## Before you start

Search the existing issues before opening a new one. For a substantial API or
behavior change, open a feature request first so the design and compatibility
cost can be discussed before implementation. Report vulnerabilities through the
private process in [SECURITY.md](SECURITY.md), not a public issue.

## Local development

Topos requires Go 1.27 or later. Clone the repository, download the module
dependencies, then run the same quality gate used in CI:

```sh
go mod download
make all
```

`make all` runs formatting and lint checks, `go vet`, tests under the race
detector, and the 90% coverage gate. Run `make vuln` when a change affects
dependencies, parsing, command execution, credentials, or another security
boundary.

## Pull requests

- Keep each change focused and explain the user-visible result.
- Add a regression test for every bug fix. The test must fail without the fix.
- Add or update runnable examples for new public behavior.
- Update package comments and README guidance when the public API changes.
- Preserve backward compatibility when practical. Call out any breaking change.
- Use `gofmt` and keep commits small enough to review independently.

## Writing

Every sentence Topos emits or carries is written for one reader, and the
register follows the reader:

- User, a person or a coding harness: the documentation, the text an
  application shows from a typed error. Short and plain: what happened and
  what to do next, naming a command or a page, never a package, a function,
  a table, or a Kubernetes object.
- Contributor, someone changing Topos: specs, this file, package
  documentation, commit messages, source comments. Precise, in the project's
  own terms, with the reason a design is what it is.
- Developer, someone debugging a running system: the typed errors' `Error()`
  text, observer events, logs. Exact and complete: object, operation,
  observed value, expected value, and the underlying error.

An error has one code, one fixed user sentence in `message`, and one
developer detail in a separate field shown only on request. The canonical
statement, worked examples, and the review checklist are in the registers
document in pkg:
https://github.com/latere-ai/pkg/blob/main/docs/writing/registers.md
The rule applies to new text and to reviews; existing text is fixed as it is
touched.

By contributing, you agree that your contribution is licensed under the
[Apache License 2.0](LICENSE).
