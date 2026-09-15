# Security

`browser-print-agentd` is a LaunchAgent that listens on loopback and spools raw printer
commands on behalf of web pages. That makes three things security-relevant: which pages may
talk to it, what it will send to which printer, and what runs as root on the machine.

## What the design promises

- **Loopback only.** Both listeners bind `127.0.0.1` by default. The agent is never a network
  service.
- **Nothing prints until you allow it.** Print routes refuse every origin until one is listed in
  `~/Library/Application Support/browser-print-agentd/allowed-origins.txt` or passed with
  `--origin-allow`. Read routes answer so a page can tell you the agent is present.
- **Only label printers.** A CUPS queue is offered only if it matches `--printer-match` (Zebra by
  default) on its name, device URI or driver. Raw ZPL never goes to the office printer.
- **The agent has no network egress.** It never fetches anything. A package built with a signing
  key also installs a root updater; that updater trusts only a manifest signed with the key
  whose public half is in `packaging/allowed_signers`, never the server it downloads from.
- **The installer never removes other software.** It refuses to install while another agent
  holds ports 9100/9101.
- **The status page cannot be driven by other sites.** `GET /` and its form endpoint send no
  CORS headers, carry a `default-src 'none'` Content-Security-Policy, and refuse a form post
  whose `Sec-Fetch-Site` is not same-origin or whose `Origin` is not this listener. A page on
  another origin cannot read the status page or add itself to the allowlist through it. A
  process on the same Mac can, via `curl`; that is the loopback trust boundary already assumed
  everywhere else.

## Known limitations

- The station certificate for `https://localhost:9101` is self-signed with `CA:TRUE` and trusted
  in the System keychain for SSL, and its private key is readable by the logged-in user. Anything
  running as that user can therefore mint certificates Safari trusts. The listener exists only
  for Safari; Chromium-family browsers use `http://localhost:9100` and need no certificate. If you
  do not use Safari, delete `cert.pem` and `key.pem` and the HTTPS listener will not start.
- The origin allowlist is exact-match on the `Origin` header. A compromised allowed origin can
  print; the agent cannot tell a page apart from a script on the same origin.
- `/health` lists every CUPS queue on the machine, including ineligible ones, to any origin.

## Reporting a vulnerability

Email the maintainer at the address on the GitHub profile of `isyvertsen`, or open a private
security advisory on this repository. Please do not file public issues for anything that would
let a web page print, read, or run something it should not. You will get an acknowledgement
within a week.
