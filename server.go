package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

const (
	// availableBudget bounds the read-only endpoints. The SPA uses
	// `GET /available` as its reachability probe under a 1500 ms abort, so the
	// discovery + health pass must finish well inside that or the station reads
	// as "no agent" instead of "no printer".
	availableBudget = 1200 * time.Millisecond

	// writeBudget bounds a print job end to end, from the request arriving to
	// the printer taking the job. A 4x6 label is ~540 KB of uncompressed ^GFA
	// hex, so spooling gets real headroom that a health probe does not; a job
	// the printer has not taken by then is cancelled and reported as failed.
	writeBudget = 20 * time.Second

	// maxWriteBody caps a /write body. ZPL never approaches this; a larger body
	// is a mistake or abuse, not a label.
	maxWriteBody = 8 << 20

	// maxPDFBody caps a /print-pdf body. A rendered label sheet is orders of
	// magnitude smaller; a PDF this big is a mistake, not a document. The limit
	// is deliberately far above maxWriteBody because base64 inflates the payload
	// by a third and a multi-cell sheet carries real page content.
	maxPDFBody = 50 << 20

	// pdfMagic is the signature every PDF opens with. Anything else decoded out
	// of 'data' is a bad payload, and printing it would put garbage on media
	// rather than fail.
	pdfMagic = "%PDF"
)

// printRequest is the body both print routes take: the Device the caller echoes
// back plus a payload. `/write` reads Data as raw ZPL and `/print-pdf` reads it
// as a base64-encoded PDF, but the envelope — and therefore printer selection —
// is identical, which is why they share one type.
type printRequest struct {
	Device *struct {
		UID  string `json:"uid"`
		Name string `json:"name"`
	} `json:"device"`
	Data *string `json:"data"`
}

// requestedPrinter returns the device the body names, or "" when unspecified.
// uid is the pinned identity; name is accepted as a fallback for hand-rolled
// requests.
func (r printRequest) requestedPrinter() string {
	if r.Device == nil {
		return ""
	}
	if r.Device.UID != "" {
		return r.Device.UID
	}
	return r.Device.Name
}

// agent serves the Browser Print wire contract on top of CUPS.
type agent struct {
	cups    *cupsClient
	health  *healthChecker
	drivers *driverChecker
	logger  *agentLogger

	// origins decides which pages may print. With nothing configured it denies
	// every print request; `*` restores upstream's log-and-allow on purpose.
	origins *originPolicy

	// printers decides which CUPS queues are label printers at all. A queue
	// that fails the match is invisible to every route except /health.
	printers *printerMatcher

	// writeBudget is the print routes' deadline — the writeBudget constant in
	// production, shortened by tests that exercise an undelivered job.
	writeBudget time.Duration
}

// agentOptions is everything a station configures on the agent beyond its
// listeners. Zero values are the defaults: deny every origin, and offer only
// queues that look like Zebra label printers.
type agentOptions struct {
	OriginAllow  []string
	OriginsFile  string
	PrinterMatch *regexp.Regexp
}

// newAgent wires an agent over an exec runner (the real one in main, a stub in
// tests) and the station's options.
func newAgent(runner execRunner, logger *agentLogger, options agentOptions) *agent {
	cups := newCUPSClient(runner)
	drivers := newDriverChecker(cups)
	pattern := options.PrinterMatch
	if pattern == nil {
		pattern = regexp.MustCompile(defaultPrinterMatch)
	}
	return &agent{
		cups:     cups,
		health:   newHealthChecker(cups),
		drivers:  drivers,
		logger:   logger,
		origins:  newOriginPolicy(options.OriginAllow, options.OriginsFile),
		printers: &printerMatcher{pattern: pattern, drivers: drivers},

		writeBudget: writeBudget,
	}
}

// printRoute reports whether a path spools to a printer. The origin posture
// gates exactly these; read routes stay open so a page can still tell the
// operator that the agent is there but not yet allowed to print.
func printRoute(path string) bool {
	return path == "/write" || path == "/print-pdf"
}

// ServeHTTP routes the four wire-contract endpoints, the two additive endpoints
// (diagnostics and document printing), and the CORS preflight.
//
// Every request is logged with its Origin (Q14): the loopback surface has no
// token, so any page the operator visits can talk to it, and v1 accepts that
// risk only on the condition that it is auditable rather than silent.
func (a *agent) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	a.logger.request(r.Method, r.URL.Path, origin)

	// Set before routing so EVERY response carries it — a 404 and a preflight
	// included. A station whose agent is answering the wrong thing is diagnosed
	// from the response it actually produced, which may well be the 404.
	w.Header().Set(versionHeader, version)

	// The status page is for the person at the Mac, not for web apps: it gets
	// no CORS headers at all, so no other origin can read or drive it.
	if uiRoute(r.URL.Path) {
		a.serveUI(w, r)
		return
	}
	setCORSHeaders(w, origin)

	if r.Method == http.MethodOptions {
		// Private Network Access. Chromium treats a public https page reaching
		// loopback as a private network request and preflights EVERY one of
		// them — including a simple GET that ordinary CORS would never
		// preflight — carrying Access-Control-Request-Private-Network. It then
		// drops the real request unless the response grants it back. Without
		// this header the SPA's GET /available never leaves the browser, the
		// station reads as "agent unreachable", and the macOS install gate
		// hard-blocks printing on a station whose agent is running fine. The
		// log is the tell: paired OPTIONS with no GET behind them.
		//
		// The grant is gated on the origin posture only for the print routes: a
		// page the station will not let print still gets to read /available,
		// which is how it learns the agent is present and what to tell the
		// operator to configure.
		if r.Header.Get("Access-Control-Request-Private-Network") == "true" &&
			(!printRoute(r.URL.Path) || a.origins.allowed(origin)) {
			w.Header().Set("Access-Control-Allow-Private-Network", "true")
		}
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusNoContent)
		return
	}

	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/available":
		a.handleAvailable(w, r)
	case r.Method == http.MethodGet && r.URL.Path == "/default":
		a.handleDefault(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/write":
		a.handleWrite(w, r, origin)
	case r.Method == http.MethodPost && r.URL.Path == "/print-pdf":
		a.handlePrintPDF(w, r, origin)
	case r.Method == http.MethodPost && r.URL.Path == "/read":
		a.handleRead(w, r)
	case r.Method == http.MethodGet && r.URL.Path == "/health":
		a.handleHealth(w, r)
	default:
		sendText(w, http.StatusNotFound, "not found\n")
	}
}

// setCORSHeaders echoes the request Origin (falling back to "*") so the browser
// lets the SPA read the response. Without this /write is browser-blocked.
func setCORSHeaders(w http.ResponseWriter, origin string) {
	allow := origin
	if allow == "" {
		allow = "*"
	}
	header := w.Header()
	header.Set("Access-Control-Allow-Origin", allow)
	header.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	header.Set("Access-Control-Allow-Headers", "Content-Type")
}

// handleAvailable lists ONLY printers that can print, USB first. An unhealthy
// printer is simply absent, so the SPA can never pin one that cannot print, and
// a station with nothing usable sees the real agent's empty list.
func (a *agent) handleAvailable(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), availableBudget)
	defer cancel()

	usable, err := a.usablePrinters(ctx)
	if err != nil {
		sendText(w, http.StatusInternalServerError, err.Error()+"\n")
		return
	}
	devices := make([]wireDevice, 0, len(usable))
	for _, candidate := range usable {
		devices = append(devices, candidate.device())
	}
	sendJSON(w, http.StatusOK, map[string]any{"printer": devices})
}

// handleDefault returns the highest-priority healthy printer as a single Device
// object, or an EMPTY body when none is healthy — the transport reads empty
// text as "no default", so an empty JSON object here would be a contract break.
func (a *agent) handleDefault(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), availableBudget)
	defer cancel()

	usable, err := a.usablePrinters(ctx)
	if err != nil {
		sendText(w, http.StatusInternalServerError, err.Error()+"\n")
		return
	}
	if len(usable) == 0 {
		sendText(w, http.StatusOK, "")
		return
	}
	sendJSON(w, http.StatusOK, usable[0].device())
}

// handleRead answers the real agent's status endpoint with an empty body. Dead
// surface for the SPA, kept so the agent stays a drop-in for any other caller.
func (a *agent) handleRead(w http.ResponseWriter, r *http.Request) {
	io.Copy(io.Discard, io.LimitReader(r.Body, maxWriteBody))
	sendText(w, http.StatusOK, "")
}

// confirmDelivery holds a print route's answer until the printer has taken the
// spooled job, inside the same deadline as the rest of the request. A job still
// queued when it runs out is cancelled, so a caller told "failed" can never get
// a late surprise print, and no failover follows: the deadline is spent.
func (a *agent) confirmDelivery(
	ctx context.Context, action string, target printer, requestID string,
) error {
	err := a.cups.awaitDelivery(ctx, target.Queue, requestID)
	if err == nil {
		return nil
	}
	if requestID != "" {
		if cancelErr := a.cups.cancelJob(ctx, requestID); cancelErr != nil {
			a.logger.undelivered(action, target, requestID, "cancel failed: "+cancelErr.Error())
			return fmt.Errorf("printer %s did not take job %s within %s, and cancelling it failed: %v",
				target.Queue, requestID, a.writeBudget, cancelErr)
		}
	}
	if errors.Is(err, errNotDelivered) {
		a.logger.undelivered(action, target, requestID, "cancelled after "+a.writeBudget.String())
		return fmt.Errorf("printer %s did not take job %s within %s; the job was cancelled",
			target.Queue, requestID, a.writeBudget)
	}
	a.logger.undelivered(action, target, requestID, err.Error())
	return err
}

// handleWrite spools raw ZPL, failing over rather than failing when the pinned
// printer died between listing and writing.
func (a *agent) handleWrite(w http.ResponseWriter, r *http.Request, origin string) {
	// Origin enforcement runs FIRST: when an allowlist is configured a
	// disallowed origin must be rejected before any CUPS work, so a hostile page
	// cannot spool a label or even enumerate health through timing.
	if !a.enforceOrigin(w, "write", origin) {
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxWriteBody+1))
	if err != nil {
		sendText(w, http.StatusBadRequest, "could not read request body\n")
		return
	}
	if len(body) > maxWriteBody {
		sendText(w, http.StatusRequestEntityTooLarge, fmt.Sprintf(
			"request body too large; the /write limit is %d bytes\n", maxWriteBody))
		return
	}

	var payload printRequest
	if err := json.Unmarshal(body, &payload); err != nil {
		sendText(w, http.StatusBadRequest, fmt.Sprintf("invalid JSON body: %v\n", err))
		return
	}
	if payload.Data == nil {
		sendText(w, http.StatusBadRequest,
			"missing 'data' (raw ZPL string) in request body\n")
		return
	}
	data := []byte(*payload.Data)

	ctx, cancel := context.WithTimeout(r.Context(), a.writeBudget)
	defer cancel()

	target, err := a.resolveTarget(ctx, "write", payload.requestedPrinter())
	if err != nil {
		sendText(w, http.StatusInternalServerError, err.Error()+"\n")
		return
	}

	requestID, err := a.cups.printRaw(ctx, target.Queue, data)
	if err != nil {
		a.logger.job("write", false, len(data), target, "", origin)
		sendText(w, http.StatusInternalServerError, err.Error()+"\n")
		return
	}
	if err := a.confirmDelivery(ctx, "write", target, requestID); err != nil {
		a.logger.job("write", false, len(data), target, requestID, origin)
		sendText(w, http.StatusInternalServerError, err.Error()+"\n")
		return
	}
	a.logger.job("write", true, len(data), target, requestID, origin)
	sendText(w, http.StatusOK, "")
}

// handlePrintPDF spools a base64-encoded PDF — the multi-cell label SHEET a
// caller renders when one ZPL label per print will not do.
//
// It is additive: it sits outside the frozen Zebra-compatible contract and no
// caller of the frozen four is affected by its existence. Everything downstream
// of payload validation is deliberately the SAME code /write runs — the origin
// gate, printer resolution with USB-to-network failover, the job log, and the
// empty-200/plain-text-error convention — so a sheet and a label can never
// disagree about which printer is usable or whether a station is dead.
//
// Submission differs only after the resolved driver is known: ordinary queues
// receive the PDF as a document, while the stock inverting ZPL driver is
// filtered offline and receives only validated generated ZPL. See
// printDocument for both branches.
func (a *agent) handlePrintPDF(w http.ResponseWriter, r *http.Request, origin string) {
	// Identical gate to /write, and load-bearing for the same reason. This route
	// spools to a physical printer, so leaving it off the allowlist would make
	// the allowlist trivially bypassable by posting a PDF instead of ZPL.
	if !a.enforceOrigin(w, "print-pdf", origin) {
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxPDFBody+1))
	if err != nil {
		sendText(w, http.StatusBadRequest, "could not read request body\n")
		return
	}
	if len(body) > maxPDFBody {
		sendText(w, http.StatusRequestEntityTooLarge, fmt.Sprintf(
			"request body too large; the /print-pdf limit is %d bytes\n", maxPDFBody))
		return
	}

	var payload printRequest
	if err := json.Unmarshal(body, &payload); err != nil {
		sendText(w, http.StatusBadRequest, fmt.Sprintf("invalid JSON body: %v\n", err))
		return
	}
	if payload.Data == nil {
		sendText(w, http.StatusBadRequest,
			"missing 'data' (base64-encoded PDF string) in request body\n")
		return
	}

	document, err := base64.StdEncoding.DecodeString(*payload.Data)
	if err != nil {
		sendText(w, http.StatusBadRequest, fmt.Sprintf("invalid base64 in 'data': %v\n", err))
		return
	}
	if !bytes.HasPrefix(document, []byte(pdfMagic)) {
		// Reject rather than spool: CUPS would accept the bytes and the operator
		// would get a page of garbage, which is the silent-wrong-output failure
		// mode this agent exists to prevent.
		sendText(w, http.StatusBadRequest,
			"decoded 'data' is not a PDF (missing "+pdfMagic+" header)\n")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), a.writeBudget)
	defer cancel()

	target, err := a.resolveTarget(ctx, "print-pdf", payload.requestedPrinter())
	if err != nil {
		sendText(w, http.StatusInternalServerError, err.Error()+"\n")
		return
	}

	// Asked of the RESOLVED queue, not the requested one: failover may have moved
	// the job to a different printer with a different driver, and compensating
	// for the driver of a printer the job did not go to is how a fix like this
	// turns into the bug it was meant to remove.
	inverting := a.drivers.inverting(ctx, target.Queue)

	requestID, err := a.cups.printDocument(ctx, target.Queue, document, inverting)
	if err != nil {
		a.logger.job("print-pdf", false, len(document), target, "", origin)
		sendText(w, http.StatusInternalServerError, err.Error()+"\n")
		return
	}
	if err := a.confirmDelivery(ctx, "print-pdf", target, requestID); err != nil {
		a.logger.job("print-pdf", false, len(document), target, requestID, origin)
		sendText(w, http.StatusInternalServerError, err.Error()+"\n")
		return
	}
	a.logger.job("print-pdf", true, len(document), target, requestID, origin)
	sendText(w, http.StatusOK, "")
}

// enforceOrigin applies the Q14 posture to a print route, writing the rejection
// itself and reporting whether the caller may proceed. Both /write and
// /print-pdf go through it so a route that spools to a printer cannot acquire a
// second, weaker copy of the allowlist check.
func (a *agent) enforceOrigin(w http.ResponseWriter, action string, origin string) bool {
	if a.origins.allowed(origin) {
		return true
	}
	a.logger.originRejected(action, origin)
	hint := "it is not on this station's allowlist"
	if a.origins.posture() == postureDeny {
		hint = "no origin is allowed to print on this station yet"
	}
	where := "pass --origin-allow"
	if a.origins.file != "" {
		where = fmt.Sprintf("add it to %s (one origin per line) or pass --origin-allow",
			a.origins.file)
	}
	sendText(w, http.StatusForbidden, fmt.Sprintf(
		"origin %q may not print: %s; %s\n", origin, hint, where))
	return false
}

// usablePrinters discovers the station's queues and filters them to the ones
// that are label printers and can print, keeping USB-first order.
func (a *agent) usablePrinters(ctx context.Context) ([]printer, error) {
	printers, err := discoverPrinters(ctx, a.cups)
	if err != nil {
		return nil, fmt.Errorf("could not enumerate CUPS printers: %w", err)
	}
	return a.health.healthyPrinters(ctx, a.printers.filter(ctx, printers)), nil
}

// resolveTarget picks the printer a job goes to, performing device-level
// failover at job initiation.
//
// This is the one deliberate behavioral difference from the dev shim, and it is
// invisible to the transport: the shim 500s a named-but-dead queue, while the
// agent falls over to the next healthy printer (USB before network, since
// discovery already orders them that way), prints, and logs an explicit
// fallback line naming the skipped device. Only when NO printer is healthy does
// the job fail — loudly, never as a phantom "Sent".
func (a *agent) resolveTarget(
	ctx context.Context, action string, requested string,
) (printer, error) {
	usable, err := a.usablePrinters(ctx)
	if err != nil {
		return printer{}, err
	}

	if requested != "" {
		if target, found := matchPrinter(usable, requested); found {
			return target, nil
		}
		if len(usable) == 0 {
			return printer{}, fmt.Errorf(
				"printer %q is not usable and no other printer on this station is "+
					"healthy", requested)
		}
		a.logger.fallback(action, requested, usable[0])
		return usable[0], nil
	}

	if len(usable) == 0 {
		return printer{}, fmt.Errorf(
			"no printer on this station is usable (none enabled and accepting in CUPS)")
	}
	return usable[0], nil
}

// sendJSON writes a JSON body with the CORS headers already set by ServeHTTP.
func sendJSON(w http.ResponseWriter, status int, payload any) {
	body, err := json.Marshal(payload)
	if err != nil {
		sendText(w, http.StatusInternalServerError, "could not encode response\n")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", fmt.Sprint(len(body)))
	w.WriteHeader(status)
	w.Write(body)
}

// sendText writes a plain-text body. Every non-2xx the transport sees is plain
// text: the SPA surfaces it verbatim through PrintSendError.
func sendText(w http.ResponseWriter, status int, text string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Length", fmt.Sprint(len(text)))
	w.WriteHeader(status)
	io.WriteString(w, text)
}

// parseOriginAllow splits a comma-separated --origin-allow value into an
// ordered, de-duplicated allowlist. An empty value keeps the default
// log-and-allow posture.
func parseOriginAllow(raw string) []string {
	allow := make([]string, 0, 2)
	for _, chunk := range strings.Split(raw, ",") {
		origin := strings.TrimSpace(chunk)
		if origin == "" {
			continue
		}
		duplicate := false
		for _, existing := range allow {
			if existing == origin {
				duplicate = true
				break
			}
		}
		if !duplicate {
			allow = append(allow, origin)
		}
	}
	return allow
}
