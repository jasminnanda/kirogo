# Contributing to kirogo

Thanks for considering it. This document is short on purpose — the important parts
are the four rules below.

## The rules

**1. Zero dependencies.** `go.mod` has no `require` block and must keep it that
way. The standard library only. This is the constraint that makes kirogo a single
static binary with nothing to audit, and it is not negotiable. If something seems
to need a library, it probably needs less code instead — the read-only SQLite
reader in `internal/store` exists for exactly this reason.

**2. Both surfaces, both modes.** kirogo exposes OpenAI and Anthropic APIs, each
streaming and non-streaming. A change to one is a change to all four unless there
is a documented reason it only applies to some. A feature that works on
`/v1/messages` streaming and nowhere else will be asked for the other three.

**3. Tests are not optional.** Every change ships with tests, and tests exist to
break the code, not to confirm it works. Happy path alone is not enough: cover
the boundaries, the malformed input, the empty case, the multi-byte case, and the
failure mode. If you cannot think of a way to break your change, look harder.

Tests make no network calls. The upstream backend, credential store and clock are
all substituted — see `internal/api/openai_stream_test.go` for the harness. The
suite must pass offline.

**4. English identifiers, comments and commit messages.** Comments explain *why*,
not *what*. Every exported symbol gets a doc comment. Every function parameter and
return value is typed.

## Before you open a pull request

```sh
gofmt -l .          # must print nothing
go vet ./...        # must be clean
go build ./...
go test -race ./... # must pass
```

## Working on the protocol

Most of the difficulty in kirogo is that the Kiro API is undocumented and its
error messages are unhelpful. `Improperly formed request.` is returned for every
kind of schema violation with no further detail.

If you are changing anything that touches the wire format:

- Run with `LOG_LEVEL=DEBUG` to see the exact payload sent upstream
- Verify against the live service, not just the schema. Several fields are
  declared and either rejected or silently ignored in the deployment — see the
  "How it works" section of the README for the ones already found
- Say in your pull request what you verified and how. "The schema says so" is not
  verification; a request id and an observed response is

## Commit messages

```
<type>(<scope>): <summary>
```

Types: `feat`, `fix`, `docs`, `test`, `refactor`, `chore`. Scope is the package or
area, for example `auth`, `catalog`, `translate`, `api`.

```
feat(api): enforce max_tokens locally on both surfaces
fix(catalog): read effort schema per model instead of assuming
```

## Licensing

kirogo is MIT licensed. By contributing you agree your contribution is licensed
under the same terms.

New files carry the same two-line copyright notice as the rest of the codebase, at
the end of the file.

## Reporting bugs

Include your kirogo version (`./kirogo -version`), your client and its version,
what you expected, and what happened. `LOG_LEVEL=DEBUG` output is the single most
useful thing you can attach — it is designed to be safe to share, since tokens are
redacted at every level, but read it over first.

Never paste a real token or API key. For security problems, see
[SECURITY.md](SECURITY.md) rather than opening a public issue.
