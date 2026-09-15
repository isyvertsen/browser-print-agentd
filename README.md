# browser-print-agentd

**Print labels from a web page to a label printer on your Mac.**

Some web apps print labels by talking to a small helper program running on your own computer.
Zebra ships one for that job; this is a drop-in replacement for Macs that do not have it. You
install it once and label printing starts working in the browser — there is no account to create,
no window to keep open, and nothing to set up afterwards. It runs quietly in the background.

It only ever talks to your own Mac and to the printers already configured on it. It is not a
network service, nothing on the internet can reach it, and your labels are never sent anywhere.

**Not affiliated with Zebra Technologies.** `browser-print-agentd` is an independent
reimplementation of a publicly observable localhost HTTP interface. It is not produced,
endorsed, sponsored, certified, or supported by Zebra Technologies Corporation. "Zebra",
"Browser Print", and "ZPL" are trademarks of their respective owners and appear here only to
name the wire contract this agent emulates.

**This is a fork.** The original is
[sharaf-nassar/browser-print-agentd](https://github.com/sharaf-nassar/browser-print-agentd) by
Sharaf Nassar, MIT licensed, and the wire contract, CUPS spooling, health-gated failover and
origin posture are all upstream's work. This fork, maintained by Ivar Syvertsen, differs in four
deliberate ways:

- **Updates are opt-in and signed by you.** Upstream's package always shipped a root
  LaunchDaemon that installed whatever GitHub's latest release said, trusting Apple's notarization
  of the maintainer's build. Here the updater ships only when the package is built with a signing
  key you own, polls a feed URL you choose, and installs only what a manifest signed with that key
  names. A checkout with no key builds a package that never phones home.
- **The installer never removes other software.** Upstream's `preinstall` deleted Zebra Browser
  Print by path glob, as root. Here it refuses to install while another agent holds ports
  9100/9101 and tells you to quit or uninstall that program yourself.
- **No page may print until you allow it.** Upstream allowed every origin by default and only
  logged. Here the default is deny, and the allowlist is a plain file in your own Application
  Support directory that the agent re-reads when it changes.
- **Only label printers are offered.** Upstream listed every CUPS queue as a Zebra printer, office
  laser included, and would spool raw ZPL to it. Here a queue has to match `--printer-match`
  (Zebra by default) on its name, device URI or driver.

The bundle id, launchd label and package identifier are `io.github.isyvertsen.…`, so this fork
and upstream never overwrite each other's install receipts.

**Running this on a station?** [`RUNBOOK.md`](./RUNBOOK.md) is the admin-facing guide: install,
migrate from another localhost print agent, roll back, uninstall, diagnose a station that will not
print, and validate one on real hardware.

## Install

You need a Mac with Apple Silicon and your Mac's administrator password.

**[Download the installer](https://github.com/isyvertsen/browser-print-agentd/releases/latest/download/browser-print-agentd.pkg)**

Open the downloaded file, follow the prompts, and enter your Mac password when it asks. When it
finishes, label printing works. There is no application to launch and no next step.

A few things worth knowing:

- **Stay logged in while it installs.** It sets itself up under your own account, and stops with
  an error rather than half-finishing if you are not there.
- **The label printer must already be set up on this Mac.** This agent prints to the printers your
  Mac already has; it does not add them for you.
- **If the installer stops and mentions port 9100 or 9101**, another label-printing program is
  already running and has to be quit or uninstalled first — the installer never removes other
  software for you. Zebra Browser Print is the usual one; see
  [the runbook](./RUNBOOK.md#migrating-from-another-localhost-print-agent).

That link always points at the newest release. Every installer is signed and notarized by Apple,
so macOS will open it without warnings.

## Updates

The agent itself never makes a network request. Whether the *package* keeps itself current is a
build-time choice:

- **Built without a signing key** (the default for a checkout), the package carries no updater.
  To upgrade, run a newer installer over the existing one; to downgrade, run an older one. Either
  way it is a normal install and nothing has to be removed first.
- **Built with a key in `packaging/allowed_signers`**, the package also installs a short-lived root
  LaunchDaemon that checks a release feed hourly and installs whatever a **signed** manifest names.
  The feed URL is baked in at build time (`UPDATE_BASE_URL`; default: this repository's GitHub
  Releases) and can be any HTTPS file server you control — behind Cloudflare Tunnel, for example.
  Trust never comes from the server: the manifest is verified with `ssh-keygen -Y verify` against
  the public key shipped in the package, and the package's SHA-256 is read from that signed
  manifest. If the installed binary is Apple-signed, the downloaded package must also be signed by
  the same Team ID and notarized. Pin a station with
  `sudo launchctl disable system/io.github.isyvertsen.browser-print-agentd.updater`.

To set the feed up: generate a key with
`ssh-keygen -t ed25519 -N '' -C browser-print-agentd-release -f release-signing-key`, paste the
public key into `packaging/allowed_signers` as `release ssh-ed25519 AAAA…`, store the private key
as the `RELEASE_SIGNING_KEY` Actions secret, and tag a release. The workflow signs
`update-manifest.txt`, verifies it against `allowed_signers` before publishing, and attaches
both. See `RUNBOOK.md` for the feed layout and how to point stations at your own server.

## Uninstall

The installer puts an uninstaller on the Mac. Open **Uninstall Browser Print Agent** from
Applications — Spotlight finds it too — confirm, and enter your Mac password when asked. There is
nothing to download.

It removes all of it — the launchd job and its plist, the binary, the launcher,
keychain trust (matched by SHA-1 fingerprint, never by name), the certificate and log
directories, the installer receipt, and itself. It deletes the log ring too, so copy that directory
first if you are uninstalling because something was wrong.

The same thing from Terminal, if you prefer:

```bash
sudo /usr/local/bin/browser-print-agentd-uninstall
```

Both run exactly the same code — the app is a confirm dialog and one administrator prompt in front
of the command above.

## What the installer does

Everything below is root work the package scripts do for you:

- **`preinstall`** stops any prior install of **this** agent and then proves ports 9100 and 9101
  are actually free. It never removes software it did not install. Anything holding those ports —
  Zebra Browser Print, or a differently-named localhost print agent — would make a `KeepAlive`
  agent crash-loop, so the install stops loudly and tells you to quit or uninstall it yourself.
- **`postinstall`** generates or reuses a per-station self-signed cert pair (CN/SAN `localhost`,
  EKU `serverAuth`) under `~/Library/Application Support/browser-print-agentd/`, bootstraps the
  LaunchAgent into `gui/<uid>`, and proves both listeners are ready. It then uses a normal
  `https://localhost:9101/available` request as the trust check: working **System** keychain trust
  is left untouched, while a first install adds SSL-only trust and requires that same request to
  succeed. A failed HTTP, HTTPS, or trust probe **fails the install**.

Installed layout:

| Path                                                                       | What                               |
| -------------------------------------------------------------------------- | ---------------------------------- |
| `/usr/local/bin/browser-print-agentd`                                      | the agent binary                   |
| `/usr/local/bin/browser-print-agentd-uninstall`                            | the uninstaller                    |
| `/Applications/Uninstall Browser Print Agent.app`                          | GUI front end for the uninstaller  |
| `/usr/local/libexec/browser-print-agentd/launcher`                         | agent launchd entry point          |
| `/Library/LaunchAgents/io.github.isyvertsen.browser-print-agentd.plist` | per-user LaunchAgent               |
| `~/Library/Application Support/browser-print-agentd/`                      | `cert.pem` and `key.pem`           |
| `~/Library/Logs/browser-print-agentd/`                                     | private, bounded per-user log ring |

Nothing runs as root after the installer exits, and nothing on the machine has network egress.

## Configuration

**Which pages may print** is the one thing you have to configure. Nothing may print until an
origin is allowed. Add your web app's origin — scheme and host, no path — to
`~/Library/Application Support/browser-print-agentd/allowed-origins.txt`, one per line:

```text
https://labels.example.com
```

The running agent picks the change up within a couple of seconds; no restart, no admin
password. A lone `*` allows every origin, which is what upstream did by default and is not
recommended. The same list can be passed as `--origin-allow https://a,https://b` in the
LaunchAgent plist, or seeded at install time by setting `BROWSER_PRINT_AGENTD_ORIGIN_ALLOW` in
the installer's environment. Read routes (`/available`, `/default`, `/health`) always answer, so a
page can tell you the agent is present but not yet allowed.

**Which queues are label printers** is decided by `--printer-match`, a regular expression tested
against each CUPS queue's name, device URI and driver identity. The default matches Zebra by
brand, language (`ZPL`), driver family (`ZDesigner`) and model prefix (`ZD621`, `ZT411`, …), so
the office laser never appears on `/available` and never receives raw ZPL. Pass `--printer-match .`
to offer every queue; `GET /health` shows each queue's `eligible` verdict.

Everything else: `--bind`, `--port`, `--https-port`, `--cert-dir`, `--origins-file`, each with an
environment mirror (`BROWSER_PRINT_AGENTD_BIND` and so on). A flag always wins over its
environment mirror, which always wins over the built-in default.

The agent does not create CUPS queues for you. Add the printer once with `lpadmin`
(`-m drv:///sample.drv/zebra.ppd`; `lpadmin -m raw` no longer exists on macOS).

## Build

Go 1.24, stdlib only — no third-party modules and no `go.sum`.

```bash
go build ./...          # build; the binary lands next to the sources
go vet ./...            # vet
go test -race ./...     # unit tests, race detector on
scripts/check-naming.sh # repository hygiene gate (see below)
```

The installer is built by `packaging/build-pkg.sh`, which cross-compiles the `darwin/<arch>`
binary, stages the payload and scripts, then runs `pkgbuild` → `productbuild` → optional
`productsign`. Stages 1 and 2 run anywhere Go runs; `--stage-only` stops before `pkgbuild`,
which is what makes the payload layout, file modes, and script set verifiable on Linux CI with
no Apple hardware.

```bash
packaging/build-pkg.sh --stage-only          # layout check, no macOS needed
packaging/build-pkg.sh --version 0.1.0       # full build (macOS)
packaging/dev-run.sh                         # run it on this Mac, as you, no installer
```

`dev-run.sh` builds the binary and registers it as a per-user LaunchAgent under a `.dev` label
with no administrator password: no certificate, no `:9101`, no updater, but the same ports and
the same allowlist file, so a web app in Chrome can print through it right away. `--status` and
`--remove` do what they say.

Its environment interface, equivalent to the flags:

| Variable                     | Meaning                                                            |
| ---------------------------- | ------------------------------------------------------------------ |
| `VERSION`                    | package version (default: derived from the newest `vX.Y.Z` tag)     |
| `ARCH`                       | `arm64` (default) or `amd64`                                        |
| `OUTPUT_DIR`                 | where the `.pkg` lands (default `packaging/dist`)                   |
| `APP_SIGNING_IDENTITY`       | "Developer ID Application" identity; codesigns the binary           |
| `INSTALLER_SIGNING_IDENTITY` | "Developer ID Installer" identity; `productsign`s the package       |
| `STAGE_ONLY`                 | `1` to stop after staging                                           |
| `GO_LDFLAGS`                 | extra link flags; the release workflow injects `-X main.version=…`  |

Signing identities are opt-in rather than assumed, so a local build works unsigned — and an
unsigned build is named `-unsigned.pkg` so it cannot be mistaken for something shippable.

Product identity (binary name, bundle id, install directories) lives in exactly one place,
`packaging/identity.sh`, and every shipped packaging artifact is a `.in` template rendered from
it. `scripts/check-naming.sh` is the repository hygiene gate: it bans the originating org and
project names and every hardware or lab artifact outright, asserts `identity.sh` and
`identity.go` agree on the binary name,
asserts `X-Print-Agent-Version` is still present and unchanged, and asserts the release
workflow's trigger surface stays tag-only. It runs as a required CI job on every push and pull
request, and is callable locally with no arguments.

Releases are cut by tagging `v*.*.*`; `RUNBOOK.md` and `lat.md/infrastructure.md` own the signing,
notarization, and asset-retention details.

## Wire contract

This agent is not a byte-for-byte clone of a vendor daemon. Three things are deliberately
different: printers are **discovered** from CUPS (`lpstat -v`) instead of hand-listed; every queue
is **health-checked at job initiation** with USB-to-network failover, so a job never reports "Sent"
into a dead printer; and every request's `Origin` is logged, with an optional allowlist enforced
on `/write`.

The first four rows are the **frozen** Zebra-compatible surface. Their paths, request and response
shapes, status codes, plain-text error bodies, and the CORS origin echo are compatibility surface
and do not change. The last two rows are **additive extensions** — they are not part of the frozen
contract, no caller of the frozen four is affected by their existence, and an agent that predates
them answers those paths with the plain-text `404` its default arm has always produced.

| Method | Path          | Response                                                                                                                 |
| ------ | ------------- | ------------------------------------------------------------------------------------------------------------------------ |
| `GET`  | `/available`  | `{"printer": [Device, …]}` — only queues that can actually print, USB before network, inside a 1500 ms probe budget       |
| `GET`  | `/default`    | one `Device` object, or an **empty body** when nothing is healthy (an empty JSON object here would break callers)         |
| `POST` | `/write`      | spools `{"data": "<raw ZPL>"}` to the requested (or resolved) printer; empty `200` on success, plain-text body on failure |
| `POST` | `/read`       | empty `200` — dead surface for most callers, kept so the agent stays a drop-in                                            |
| `GET`  | `/health`     | **additive** diagnostics: running version, origin posture, and every queue's health                                      |
| `POST` | `/print-pdf`  | **additive**: spools `{"data": "<base64 PDF>"}` as a rendered document; same `200`/plain-text convention as `/write`      |

`OPTIONS` on any path answers the CORS preflight with `204`.

**`/print-pdf`.** For callers that render a multi-cell label *sheet* rather than one ZPL label.
It takes the same `{device, data}` envelope as `/write` and reuses the same printer resolution,
USB-to-network failover, and origin gating, so a sheet and a label can never disagree about which
printer is usable. `data` that will not base64-decode, or that decodes to bytes not beginning with
`%PDF`, is a `400` before any CUPS call; a body over 50 MB is a `413`. A PDF always runs through a
CUPS rendering chain — raw PDF bytes would reach the device unrendered and print as garbage. Most
queues receive the PDF as an ordinary document. For the one stock Zebra ZPL driver that emits an
inverted, device-stored graphic, the agent runs that queue's validated PPD offline, converts the
bounded bitmap to upright inline `^GFA`, and raw-spools only the generated printer-native ZPL. The
caller still sends the PDF the way up it wants it printed, and all other drivers remain untouched.

**Ports.** `9100` is plain HTTP; loopback is exempt from mixed-content blocking, so
Chromium-family browsers reach it directly from an HTTPS page. `9101` is TLS and exists only
when the station cert pair is present — Safari needs it. Both bind loopback only; this is a
bridge between the local browser and local CUPS, never a network service.

**Versioning.** Every response — 404s and preflights included — carries
`X-Print-Agent-Version`. That header and `GET /health` are the *only* places the running version
is reported: the `Device` shape must never grow a version field, because callers parse and pin
it. A binary built any way other than a tagged release reports `dev`.

**Origin posture.** Deny until configured. Both print routes — `/write` and `/print-pdf` —
reject any origin not on the allowlist with a `403` that names the file to edit, *before* any
CUPS work happens. The allowlist is the union of `--origin-allow` and the per-user
`allowed-origins.txt`; `*` allows all. The Chromium private-network preflight grant follows the
same split: always granted for reads, granted for print routes only to an allowed origin.

**Printer eligibility.** Only queues matching `--printer-match` are discovered, health-checked,
offered, or used as a failover target. An ineligible queue appears on `/health` with
`"eligible": false` and nowhere else.

## License

[MIT](./LICENSE).
