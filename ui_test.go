package main

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The status page is the one surface a person reads. It must answer "can I
// print" from the same probes /health runs, and its form must be usable only
// from the page itself.
func TestStatusPageShowsVerdictPrintersAndOrigins(t *testing.T) {
	originsFile := filepath.Join(t.TempDir(), originsFileName)
	fake := twoPrinterCUPS(stateEnabled, stateDisabled)
	fake.devices = append(fake.devices, queueDevice{
		Queue: "Brother_DCP_7070DW",
		URI:   "dnssd://Brother%20DCP-7070DW._pdl-datastream._tcp.local./?bidi",
	})
	fake.states["Brother_DCP_7070DW"] = stateEnabled
	base, _, _ := startAgentWith(t, fake, agentOptions{OriginsFile: originsFile})

	status, body := getPath(t, base, "/")
	if status != http.StatusOK {
		t.Fatalf("GET / = %d, want 200", status)
	}
	for _, want := range []string{
		"no site is allowed yet", usbQueue, netQueue, "Brother_DCP_7070DW",
		"not a label printer", "paused, rejecting jobs, or offline", "Allow this site",
		"Nothing can print until a site is allowed",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("GET / body lacks %q:\n%s", want, body)
		}
	}

	// No CORS on the page, ever: another origin must not be able to read it.
	request, _ := http.NewRequest(http.MethodGet, base+"/", nil)
	request.Header.Set("Origin", "https://evil.example")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("GET / with Origin: %v", err)
	}
	response.Body.Close()
	if got := response.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("status page sent Access-Control-Allow-Origin %q, want none", got)
	}
	if got := response.Header.Get("Content-Security-Policy"); !strings.Contains(got, "default-src 'none'") {
		t.Fatalf("status page CSP = %q", got)
	}
}

func TestStatusPageFormAllowsAndRemovesSites(t *testing.T) {
	originsFile := filepath.Join(t.TempDir(), originsFileName)
	fake := twoPrinterCUPS(stateEnabled, stateEnabled)
	base, logs, handler := startAgentWith(t, fake, agentOptions{
		OriginAllow: []string{"https://static.example"},
		OriginsFile: originsFile,
	})
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	post := func(form url.Values, headers map[string]string) *http.Response {
		t.Helper()
		request, _ := http.NewRequest(http.MethodPost, base+"/ui/origins",
			strings.NewReader(form.Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		for name, value := range headers {
			request.Header.Set(name, value)
		}
		response, err := client.Do(request)
		if err != nil {
			t.Fatalf("POST /ui/origins: %v", err)
		}
		response.Body.Close()
		return response
	}

	// A cross-site post — what a hostile page would send — is refused before
	// anything is written, on either signal.
	for _, headers := range []map[string]string{
		{"Origin": "https://evil.example"},
		{"Sec-Fetch-Site": "cross-site"},
	} {
		response := post(url.Values{"action": {"allow"}, "origin": {"https://evil.example"}}, headers)
		if response.StatusCode != http.StatusForbidden {
			t.Fatalf("cross-site form with %v = %d, want 403", headers, response.StatusCode)
		}
	}
	if _, err := os.Stat(originsFile); !os.IsNotExist(err) {
		t.Fatalf("origins file was written by a refused form")
	}

	// The page's own post — same-origin on both signals — allows the site,
	// normalising what a person typed into what a browser will send.
	self := map[string]string{"Origin": "http://" + strings.TrimPrefix(base, "http://"), "Sec-Fetch-Site": "same-origin"}
	response := post(url.Values{"action": {"allow"}, "origin": {" Labels.Example.COM:443/ "}}, self)
	if response.StatusCode != http.StatusSeeOther || !strings.Contains(response.Header.Get("Location"), "ok=") {
		t.Fatalf("allow = %d %q, want a redirect with ok=", response.StatusCode, response.Header.Get("Location"))
	}
	data, _ := os.ReadFile(originsFile)
	if !strings.Contains(string(data), "\nhttps://labels.example.com\n") {
		t.Fatalf("origins file = %q, want the normalised origin", data)
	}
	handler.origins.mu.Lock()
	handler.origins.checked = handler.origins.checked.Add(-originsFileTTL)
	handler.origins.mu.Unlock()
	if status, _ := postWrite(t, base, "https://labels.example.com", map[string]any{"data": "^XA^XZ"}); status != http.StatusOK {
		t.Fatalf("/write from the newly allowed site = %d, want 200", status)
	}
	if !strings.Contains(logs.String(), "ui allowed origin=https://labels.example.com") {
		t.Fatalf("logs = %q, want the allow recorded", logs.String())
	}

	// Bad input is bounced back to the page with a reason, and nothing changes.
	response = post(url.Values{"action": {"allow"}, "origin": {"ftp://x"}}, self)
	if !strings.Contains(response.Header.Get("Location"), "error=") {
		t.Fatalf("bad origin redirect = %q, want error=", response.Header.Get("Location"))
	}

	// The page lists the file's site as removable and the static one as not,
	// and removing works.
	_, body := getPath(t, base, "/")
	if !strings.Contains(body, "https://static.example") || !strings.Contains(body, "set by --origin-allow") {
		t.Fatalf("page does not mark the static origin:\n%s", body)
	}
	response = post(url.Values{"action": {"remove"}, "origin": {"https://labels.example.com"}}, self)
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("remove = %d, want 303", response.StatusCode)
	}
	data, _ = os.ReadFile(originsFile)
	if strings.Contains(string(data), "labels.example.com") || !strings.HasPrefix(string(data), "#") {
		t.Fatalf("origins file after remove = %q, want comments kept and the site gone", data)
	}
}

func TestNormalizeOrigin(t *testing.T) {
	good := map[string]string{
		"https://labels.example.com":  "https://labels.example.com",
		"HTTPS://Labels.Example.com/": "https://labels.example.com",
		"labels.example.com":          "https://labels.example.com",
		"http://localhost:3000":       "http://localhost:3000",
		"https://x.example:443":       "https://x.example",
		"http://x.example:80":         "http://x.example",
		" https://x.example:8443 ":    "https://x.example:8443",
		"*":                           "*",
	}
	for in, want := range good {
		got, err := normalizeOrigin(in)
		if err != nil || got != want {
			t.Fatalf("normalizeOrigin(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "ftp://x.example", "https://x.example/path", "https://u:p@x.example", "https://x.example?q=1", "://"} {
		if got, err := normalizeOrigin(bad); err == nil {
			t.Fatalf("normalizeOrigin(%q) = %q, want an error", bad, got)
		}
	}
}
