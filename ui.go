package main

import (
	"context"
	"html/template"
	"net/http"
	"strings"
	"time"
)

// The status page. It is the one human-facing surface of an otherwise headless
// daemon, served on loopback at `/`, and it exists to answer the question every
// station call opens with — "can this Mac print, and if not, what do I do?" —
// without curl, the log file, or the runbook.
//
// It is deliberately outside the frozen wire contract and outside CORS: no
// Access-Control header is ever sent for it, the page's own form posts are
// accepted only from the page itself (same-origin, checked two ways), and a
// strict Content-Security-Policy keeps it inert. A web page on any other origin
// can neither read it nor drive it, which matters because it can edit the
// allowlist that decides who may print.

// uiPathPrefix is the form endpoint's namespace; `/` itself is the page.
const uiPathPrefix = "/ui/"

// uiRoute reports whether a path belongs to the status page rather than the
// print API.
func uiRoute(path string) bool {
	return path == "/" || strings.HasPrefix(path, uiPathPrefix)
}

// pageModel is everything the template renders, computed fresh per request.
type pageModel struct {
	Version      string
	Verdict      string
	Detail       string
	Ready        bool
	Printers     []pagePrinter
	PrinterMatch string
	Origins      []pageOrigin
	AllowAll     bool
	OriginsFile  string
	CanEdit      bool
	Notice       string
	Error        string
	Log          []string
	CUPSError    string
	Host         string
}

// pageOrigin is one allowed site and whether the page may take it away: a
// site from --origin-allow lives in the LaunchAgent plist, not the file.
type pageOrigin struct {
	Value     string
	Removable bool
}

// pagePrinter is one discovered queue with the reason it will or will not be
// used, in the operator's words.
type pagePrinter struct {
	Name       string
	Connection string
	Ready      bool
	Reason     string
}

// serveUI routes the page and its form.
func (a *agent) serveUI(w http.ResponseWriter, r *http.Request) {
	header := w.Header()
	header.Set("Content-Security-Policy",
		"default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
	header.Set("X-Frame-Options", "DENY")
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("Referrer-Policy", "no-referrer")
	header.Set("Cache-Control", "no-store")

	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/":
		a.renderPage(w, r, r.URL.Query().Get("ok"), r.URL.Query().Get("error"))
	case r.Method == http.MethodPost && r.URL.Path == uiPathPrefix+"origins":
		a.handleOriginsForm(w, r)
	default:
		sendText(w, http.StatusNotFound, "not found\n")
	}
}

// sameOriginForm rejects a form post that did not come from this page. Both
// signals a browser offers are checked: the Sec-Fetch-Site metadata every
// modern browser attaches, and the Origin header, which must name this very
// listener. curl from the same Mac sends neither and is let through — it is
// already on the loopback side of the trust boundary.
func sameOriginForm(r *http.Request) bool {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
		return false
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		if !strings.EqualFold(origin, scheme+"://"+r.Host) {
			return false
		}
	}
	return true
}

// handleOriginsForm adds or removes one allowed site, then sends the browser
// back to the page with the outcome in the query string so a refresh cannot
// repeat the change.
func (a *agent) handleOriginsForm(w http.ResponseWriter, r *http.Request) {
	if !sameOriginForm(r) {
		a.logger.write("ui REJECTED reason=cross-site-form origin=" +
			originField(r.Header.Get("Origin")))
		sendText(w, http.StatusForbidden,
			"this form can only be submitted from the status page itself\n")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if err := r.ParseForm(); err != nil {
		redirectWithError(w, r, "the form could not be read")
		return
	}
	switch r.PostForm.Get("action") {
	case "allow":
		origin, err := a.origins.add(r.PostForm.Get("origin"))
		if err != nil {
			redirectWithError(w, r, err.Error())
			return
		}
		a.logger.write("ui allowed origin=" + origin)
		redirectWithNotice(w, r, "Allowed "+origin)
	case "remove":
		origin := strings.TrimSpace(r.PostForm.Get("origin"))
		if err := a.origins.remove(origin); err != nil {
			redirectWithError(w, r, err.Error())
			return
		}
		a.logger.write("ui removed origin=" + origin)
		redirectWithNotice(w, r, "Removed "+origin)
	default:
		redirectWithError(w, r, "unknown action")
	}
}

func redirectWithNotice(w http.ResponseWriter, r *http.Request, notice string) {
	http.Redirect(w, r, "/?ok="+template.URLQueryEscaper(notice), http.StatusSeeOther)
}

func redirectWithError(w http.ResponseWriter, r *http.Request, message string) {
	http.Redirect(w, r, "/?error="+template.URLQueryEscaper(message), http.StatusSeeOther)
}

// interestingLog drops the lines a person never needs on the page: plain
// read requests (the page's own reloads, the web app's polling) and the
// page's own form traffic. Jobs, refusals, fallbacks, allowlist changes and
// print requests stay.
func interestingLog(lines []string) []string {
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		if strings.Contains(line, " request method=GET ") ||
			strings.Contains(line, " request method=OPTIONS ") ||
			strings.Contains(line, " path=/ui/") {
			continue
		}
		kept = append(kept, line)
	}
	return kept
}

// renderPage builds the model from the same probes /health runs and writes
// the HTML.
func (a *agent) renderPage(w http.ResponseWriter, r *http.Request, notice string, errText string) {
	ctx, cancel := context.WithTimeout(r.Context(), availableBudget)
	defer cancel()

	model := pageModel{
		Version:      version,
		PrinterMatch: a.printers.pattern.String(),
		OriginsFile:  a.origins.file,
		CanEdit:      a.origins.file != "",
		Notice:       notice,
		Error:        errText,
		Log:          interestingLog(a.logger.tail()),
		Host:         r.Host,
	}
	static := map[string]bool{}
	for _, origin := range a.origins.static {
		static[origin] = true
	}
	for _, origin := range a.origins.effective() {
		if origin == allowAllOrigins {
			model.AllowAll = true
		}
		model.Origins = append(model.Origins, pageOrigin{
			Value:     origin,
			Removable: model.CanEdit && !static[origin],
		})
	}
	// Newest first: the line that explains the last failure is the one wanted.
	for i, j := 0, len(model.Log)-1; i < j; i, j = i+1, j-1 {
		model.Log[i], model.Log[j] = model.Log[j], model.Log[i]
	}

	var target *pagePrinter
	discovered, err := discoverPrinters(ctx, a.cups)
	if err != nil {
		model.CUPSError = err.Error()
	} else {
		usable := a.health.healthyPrinters(ctx, a.printers.filter(ctx, discovered))
		ready := map[string]bool{}
		for _, candidate := range usable {
			ready[candidate.Queue] = true
		}
		for _, candidate := range discovered {
			entry := pagePrinter{Name: candidate.Name, Connection: candidate.Connection}
			switch {
			case ready[candidate.Queue]:
				entry.Ready = true
				entry.Reason = "ready"
			case !a.printers.eligible(ctx, candidate):
				entry.Reason = "not a label printer; skipped"
			default:
				entry.Reason = "paused, rejecting jobs, or offline in CUPS"
			}
			model.Printers = append(model.Printers, entry)
			if entry.Ready && target == nil {
				copied := entry
				target = &copied
			}
		}
	}

	switch {
	case model.CUPSError != "":
		model.Verdict = "Cannot see the printers."
		model.Detail = "CUPS did not answer: " + model.CUPSError
	case target == nil:
		model.Verdict = "No label printer is ready."
		if len(model.Printers) == 0 {
			model.Detail = "This Mac has no printer queues. Add the label printer in System Settings, then reload."
		} else {
			model.Detail = "Every queue on this Mac is either not a label printer or not able to print right now. The list below says which."
		}
	case len(model.Origins) == 0:
		model.Verdict = "Ready to print, but no site is allowed yet."
		model.Detail = "Labels will go to " + target.Name + ". Allow your web app below and it can start printing."
	default:
		model.Ready = true
		model.Verdict = "Ready. Labels go to " + target.Name + "."
		if model.AllowAll {
			model.Detail = "Any site may print. Remove the * entry below to lock this down."
		} else {
			model.Detail = "Allowed sites are listed below."
		}
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := pageTemplate.Execute(w, model); err != nil {
		a.logger.write("ui render failed: " + err.Error())
	}
}

var pageTemplate = template.Must(template.New("status").Funcs(template.FuncMap{
	"now": func() string { return time.Now().Format("15:04:05") },
}).Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Label printing on this Mac</title>
<style>
  :root {
    --paper: #f3f4f1;
    --ink: #141513;
    --print: #3a3c38;
    --liner: #c9cbc6;
    --ok: #1f6f3d;
    --stop: #a8281f;
    color-scheme: light;
  }
  * { box-sizing: border-box; }
  html { background: var(--paper); }
  body {
    margin: 0;
    padding: 40px 20px 64px;
    color: var(--ink);
    background: var(--paper);
    font: 15px/1.5 ui-monospace, "SF Mono", Menlo, Consolas, monospace;
  }
  main { max-width: 62ch; margin: 0 auto; }
  a { color: inherit; }

  /* The verdict is printed on a label: solid ink border, torn off the roll
     along a perforated bottom edge. It is the only decorated thing here. */
  .label {
    border: 2px solid var(--ink);
    border-bottom: 2px dashed var(--ink);
    padding: 22px 22px 26px;
    margin: 0 0 40px;
    background: #fff;
  }
  .label h1 {
    margin: 0 0 10px;
    font-size: 26px;
    line-height: 1.2;
    font-weight: 700;
    letter-spacing: -0.01em;
  }
  .label.ready h1 { color: var(--ok); }
  .label.stop h1 { color: var(--stop); }
  .label p { margin: 0; color: var(--print); }

  .notice, .error {
    margin: 0 0 24px;
    padding: 10px 14px;
    border-left: 4px solid var(--ok);
    background: #fff;
  }
  .error { border-left-color: var(--stop); }

  section { margin: 0 0 40px; }
  h2 {
    margin: 0 0 12px;
    font-size: 15px;
    font-weight: 700;
    padding-bottom: 6px;
    border-bottom: 1px solid var(--liner);
  }
  table { width: 100%; border-collapse: collapse; }
  td, th { text-align: left; vertical-align: top; padding: 6px 14px 6px 0; }
  td:last-child, th:last-child { padding-right: 0; }
  th { font-weight: 400; color: var(--print); border-bottom: 1px solid var(--liner); }
  td.status { width: 2.2ch; font-weight: 700; }
  td.status.ok { color: var(--ok); }
  td.status.no { color: var(--stop); }
  td.muted, .muted { color: var(--print); }
  .narrow-only { display: none; }
  @media (max-width: 560px) {
    /* Four columns do not fit a phone; the transport folds into the status. */
    .via { display: none; }
    .narrow-only { display: inline; }
  }

  ul { list-style: none; margin: 0; padding: 0; }
  li { display: flex; gap: 12px; align-items: baseline; padding: 6px 0; border-bottom: 1px solid var(--liner); }
  li span { flex: 1; overflow-wrap: anywhere; }
  form.inline { display: inline; }
  form.add { display: flex; gap: 8px; margin-top: 14px; flex-wrap: wrap; }
  input[type=text] {
    flex: 1;
    min-width: 16ch;
    font: inherit;
    padding: 8px 10px;
    border: 1px solid var(--ink);
    background: #fff;
    color: var(--ink);
  }
  button {
    font: inherit;
    padding: 8px 14px;
    border: 1px solid var(--ink);
    background: var(--ink);
    color: var(--paper);
    cursor: pointer;
  }
  button.quiet { background: transparent; color: var(--ink); padding: 2px 8px; }
  button:focus-visible, input:focus-visible { outline: 3px solid var(--ok); outline-offset: 2px; }

  pre {
    margin: 0;
    padding: 12px 14px;
    background: #fff;
    border: 1px solid var(--liner);
    font: 13px/1.5 inherit;
    white-space: pre-wrap;
    overflow-wrap: anywhere;
    max-height: 22em;
    overflow: auto;
  }
  footer { color: var(--print); font-size: 13px; }
  footer p { margin: 4px 0; overflow-wrap: anywhere; }
  @media (prefers-reduced-motion: no-preference) {
    .label { animation: peel 240ms ease-out; }
    @keyframes peel { from { transform: translateY(-4px); opacity: 0; } to { transform: none; opacity: 1; } }
  }
</style>
</head>
<body>
<main>
  <div class="label {{if .Ready}}ready{{else}}stop{{end}}">
    <h1>{{.Verdict}}</h1>
    <p>{{.Detail}}</p>
  </div>

  {{if .Notice}}<div class="notice">{{.Notice}}</div>{{end}}
  {{if .Error}}<div class="error">{{.Error}}</div>{{end}}

  <section>
    <h2>Printers</h2>
    {{if .Printers}}
    <table>
      <tr><th></th><th>Queue</th><th class="via">Via</th><th>Status</th></tr>
      {{range .Printers}}
      <tr>
        <td class="status {{if .Ready}}ok{{else}}no{{end}}">{{if .Ready}}✓{{else}}–{{end}}</td>
        <td>{{.Name}}</td>
        <td class="muted via">{{.Connection}}</td>
        <td class="muted">{{.Reason}}<span class="narrow-only"> ({{.Connection}})</span></td>
      </tr>
      {{end}}
    </table>
    {{else}}
    <p class="muted">No printer queues on this Mac.</p>
    {{end}}
    <p class="muted">Queues count as label printers when their name, address or driver matches <code>{{.PrinterMatch}}</code>.</p>
  </section>

  <section>
    <h2>Sites allowed to print</h2>
    {{if .Origins}}
    <ul>
      {{range .Origins}}
      <li>
        <span>{{if eq .Value "*"}}* (any site){{else}}{{.Value}}{{end}}</span>
        {{if .Removable}}
        <form class="inline" method="post" action="/ui/origins">
          <input type="hidden" name="action" value="remove">
          <input type="hidden" name="origin" value="{{.Value}}">
          <button class="quiet" type="submit">Remove</button>
        </form>
        {{else}}<span class="muted" style="flex:0">set by --origin-allow</span>{{end}}
      </li>
      {{end}}
    </ul>
    {{else}}
    <p class="muted">None yet. Nothing can print until a site is allowed.</p>
    {{end}}
    {{if .CanEdit}}
    <form class="add" method="post" action="/ui/origins">
      <input type="hidden" name="action" value="allow">
      <label for="origin" hidden>Site address</label>
      <input type="text" id="origin" name="origin" placeholder="https://labels.example.com" autocomplete="off" spellcheck="false">
      <button type="submit">Allow this site</button>
    </form>
    <p class="muted">Use the address as it appears in the browser's location bar, without a path. Changes take effect within seconds.</p>
    {{else}}
    <p class="muted">This agent was started without an allowlist file; sites are set with --origin-allow.</p>
    {{end}}
  </section>

  <section>
    <h2>Recent activity</h2>
    {{if .Log}}<pre>{{range .Log}}{{.}}
{{end}}</pre>{{else}}<p class="muted">Nothing yet.</p>{{end}}
  </section>

  <footer>
    <p>browser-print-agentd {{.Version}} on {{.Host}}, checked {{now}}.</p>
    {{if .OriginsFile}}<p>Allowlist file: {{.OriginsFile}}</p>{{end}}
  </footer>
</main>
</body>
</html>
`))
