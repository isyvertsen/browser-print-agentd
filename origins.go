package main

import (
	"bufio"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	// originsFileName is the per-account allowlist the agent reads next to its
	// cert pair. It exists so a station can be locked to its web app without
	// editing the root-owned LaunchAgent plist: one origin per line, `#`
	// comments, and a lone `*` to allow every origin.
	originsFileName = "allowed-origins.txt"

	// allowAllOrigins is the explicit opt-in to upstream's original posture. It
	// has to be written down by someone; it is never the default.
	allowAllOrigins = "*"

	// originsFileTTL bounds how often the file is re-stat'ed. It is tiny and the
	// check is one stat, so this is about not hammering the disk under a burst
	// of preflights, not about staleness.
	originsFileTTL = 2 * time.Second

	// maxOriginsFileBytes caps what is read. An origin list is a handful of
	// lines; anything larger is a mistake and is ignored rather than parsed.
	maxOriginsFileBytes = 64 << 10
)

// Origin postures reported by the diagnostics endpoint.
const (
	// postureDeny is the default: nothing is configured, so no page may print.
	// Read routes still answer so the web app can tell the operator what to do.
	postureDeny = "deny-all (no origin configured)"

	// postureAllowAll is upstream's log-and-allow, reachable only by writing
	// `*` into the allowlist on purpose.
	postureAllowAll = "allow-all"

	// postureAllowlist is the configured state: only listed origins print.
	postureAllowlist = "allowlist"
)

// originPolicy merges the static allowlist (flag or environment) with the
// per-account allowlist file, re-reading the file when it changes so an
// operator can add their web app without restarting the agent.
type originPolicy struct {
	static []string
	file   string

	mu        sync.Mutex
	checked   time.Time
	modTime   time.Time
	size      int64
	fromFile  []string
	fileState string
	now       func() time.Time
}

// newOriginPolicy builds a policy over the static list and the file path. An
// empty path disables the file source.
func newOriginPolicy(static []string, file string) *originPolicy {
	return &originPolicy{
		static: append([]string{}, static...),
		file:   file,
		now:    time.Now,
	}
}

// effective returns the merged, de-duplicated allowlist as it stands now.
func (p *originPolicy) effective() []string {
	p.refresh()
	p.mu.Lock()
	defer p.mu.Unlock()
	merged := make([]string, 0, len(p.static)+len(p.fromFile))
	seen := map[string]bool{}
	for _, list := range [][]string{p.static, p.fromFile} {
		for _, origin := range list {
			if !seen[origin] {
				seen[origin] = true
				merged = append(merged, origin)
			}
		}
	}
	return merged
}

// posture names the current state for logs and /health.
func (p *originPolicy) posture() string {
	list := p.effective()
	if len(list) == 0 {
		return postureDeny
	}
	for _, origin := range list {
		if origin == allowAllOrigins {
			return postureAllowAll
		}
	}
	return postureAllowlist
}

// allowed reports whether origin may print. A request with no Origin header
// is never on an allowlist and is allowed only under `*`.
func (p *originPolicy) allowed(origin string) bool {
	for _, candidate := range p.effective() {
		if candidate == allowAllOrigins || candidate == origin {
			return true
		}
	}
	return false
}

// refresh re-reads the allowlist file when its mtime or size moved. Any error
// — missing file, unreadable, oversized — reads as an empty file contribution,
// never as allow-all.
func (p *originPolicy) refresh() {
	if p.file == "" {
		return
	}
	now := p.now()
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.checked.IsZero() && now.Sub(p.checked) < originsFileTTL {
		return
	}
	p.checked = now

	info, err := os.Stat(p.file)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxOriginsFileBytes {
		p.fromFile = nil
		p.modTime, p.size = time.Time{}, 0
		if err != nil {
			p.fileState = "absent"
		} else {
			p.fileState = "ignored"
		}
		return
	}
	if info.ModTime().Equal(p.modTime) && info.Size() == p.size {
		return
	}
	handle, err := os.Open(p.file)
	if err != nil {
		p.fromFile = nil
		p.fileState = "unreadable"
		return
	}
	defer handle.Close()
	p.fromFile = parseOriginsFile(handle)
	p.modTime, p.size = info.ModTime(), info.Size()
	p.fileState = "loaded"
}

// parseOriginsFile reads one origin per line, ignoring blanks and `#`
// comments, trimming whitespace, de-duplicating in order.
func parseOriginsFile(handle *os.File) []string {
	origins := make([]string, 0, 4)
	seen := map[string]bool{}
	scanner := bufio.NewScanner(handle)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if hash := strings.Index(line, " #"); hash >= 0 {
			line = strings.TrimSpace(line[:hash])
		}
		if !seen[line] {
			seen[line] = true
			origins = append(origins, line)
		}
	}
	return origins
}
