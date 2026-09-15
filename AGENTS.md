# Agent Instructions

`browser-print-agentd` is a **Go 1.24 + macOS packaging** project. It is a single flat
`package main` at the repository root plus a `packaging/` tree that builds a signed, notarized
`.pkg` installer. There is no frontend, no database, no service backend, and no design system —
if a task description implies one, the description is wrong.

Read `README.md` before touching anything: it states the frozen wire contract and the install
layout. The release chain lives in `RUNBOOK.md` and `lat.md/infrastructure.md`. Read `lat.md/` for
the design intent behind the code.

## Repository shape

| Path                      | What it is                                                                           |
| ------------------------- | ------------------------------------------------------------------------------------ |
| `*.go` (repo root)        | the whole agent — flat `package main`, stdlib only, no `go.sum`                      |
| `agent_test.go`           | the unit suite, including the CUPS-parsing fixtures captured from real hardware      |
| `packaging/identity.sh`   | **the single identity source**: product name, bundle id, binary name, dir names      |
| `packaging/*.in`          | templates rendered from `identity.sh` by `build-pkg.sh` — never edit the output      |
| `packaging/build-pkg.sh`  | cross-compile → stage → `pkgbuild` → `productbuild` → optional `productsign`         |
| `scripts/check-naming.sh` | the repository hygiene gate                                                          |
| `README.md`               | what the agent **is**: plain-language intro, install, the frozen wire contract        |
| `RUNBOOK.md`              | what an admin **does** to a station: install, migrate, roll back, diagnose, validate |
| `lat.md/`                 | **why** it behaves that way — the design/architecture knowledge graph                |

## Commands

```bash
go build ./...                    # build
go vet ./...                      # vet
go test -race ./...               # unit tests, race detector on
scripts/check-naming.sh           # repository hygiene gate — exit 0/1
packaging/build-pkg.sh --stage-only   # verify the installer layout, no macOS needed
lat check                         # validate lat.md wiki links and code refs
```

`agent_test.go` is the entire test surface. There is no Python suite, no pytest, and no vendored
harness in this repository.

Before handing work back, `go build ./...`, `go vet ./...`, `go test -race ./...`,
`scripts/check-naming.sh`, and `lat check` must all be green.

## Hard constraints

- **The wire contract is frozen.** `/available`, `/default`, `/write`, `/read`, `/health`, their
  request and response shapes, status codes, plain-text error bodies, the CORS origin echo, the
  1500 ms `/available` probe budget, and byte-exact `^GFA` passthrough do not change. `/default`
  returns an **empty body** — not `{}` — when nothing is healthy.
- **`X-Print-Agent-Version` is a fixed header name**, and the `Device` object must never grow a
  version field. `check-naming.sh` asserts both.
- **Identity lives in one place.** Change `packaging/identity.sh` (and `identity.go`'s
  `productName` in lockstep), never a literal in a plist, `distribution.xml`, an install script,
  or a workflow. Every packaging artifact is a `.in` template.
- **The release workflow stays tag-only.** No `push: branches:` trigger may reach `main`;
  `check-naming.sh` fails the build if one does.
- **No lab-specific strings.** No originating-monorepo names, no printer serials, no station IP
  addresses, no site-specific queue names, no Team ID. Documentation examples use
  `https://lab.example`. `check-naming.sh` enforces this with **no allowlist and no exception
  mechanism**; fix a hit by renaming the string, never by widening the gate.
- **New tests require an explicit request** and must be anchored to a section in
  `lat.md/tests.md` with a matching `// @lat:` code ref.
- **Signing and notarization credentials never appear in a file, a log, a command line, or a
  commit.** They are GitHub secrets, set interactively with `gh secret set`.

## Non-Interactive Shell Commands

**ALWAYS use non-interactive flags** with file operations to avoid hanging on confirmation
prompts. `cp`, `mv`, and `rm` may be aliased to `-i` on some systems, which makes an agent hang
forever waiting for y/n.

```bash
cp -f source dest           # NOT: cp source dest
mv -f source dest           # NOT: mv source dest
rm -f file                  # NOT: rm file
rm -rf directory            # NOT: rm -r directory
cp -rf source dest          # NOT: cp -r source dest
```

Other commands that may prompt: `scp` and `ssh` (`-o BatchMode=yes`), `apt-get` (`-y`), `brew`
(`HOMEBREW_NO_AUTO_UPDATE=1`).
