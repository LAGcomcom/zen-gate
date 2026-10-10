package gateway

import (
	"net/http"
	"strings"
)

// CORS for the /v1 API surface (issue #34). Browser pages on other origins —
// the model-provider formatters, web SDKs, any fetch() from a site — cannot
// read the gateway's replies without these headers; the request fails as
// "Failed to fetch" before the response is ever visible. This never widens
// what a page may do: the API key is still required on every call (a page
// without it now gets a readable 401 instead of an opaque CORS block), and
// /admin/* is deliberately excluded — the dashboard is same-origin, and the
// management surface must stay unreadable from any web page.
//
// The origin is echoed, not *: with ACAO * a page cannot send credentials
// mode include, and echoing the concrete Origin keeps that door open for
// clients that need it. No Vary is needed on a loopback service whose callers
// are browsers on this machine, but it is set anyway — the reply depends on
// the request header, and lying about that to a caching proxy is how CORS
// bugs are born.

// corsPreflightMaxAge caches an OPTIONS answer for ten minutes, so a chatty
// page pays one preflight per session, not one per request.
const corsPreflightMaxAge = "600"

// corsAPI stamps CORS headers on a /v1/* reply and answers preflight. It
// returns true when the request was an OPTIONS preflight fully handled here.
func corsAPI(w http.ResponseWriter, r *http.Request) bool {
	if origin := r.Header.Get("Origin"); origin != "" {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Add("Vary", "Origin")
	}
	// The gateway's own markers must survive the cross-origin wall: the
	// served-by header is how a client learns which model actually answered
	// after a throttle switch, and the request id ties a reply to the access
	// log. Without this list a browser reads both as absent.
	w.Header().Set("Access-Control-Expose-Headers", "x-zen-gate-served-by, x-zen-gate-request-id, content-type")
	if r.Method != http.MethodOptions {
		return false
	}
	w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, POST, OPTIONS")
	// Echo whatever the browser asked to send — SDKs bring a moving set
	// (authorization, x-api-key, anthropic-version, session_id, …) and a
	// fixed allow-list turns every new header into a fresh bug report.
	if req := r.Header.Get("Access-Control-Request-Headers"); req != "" {
		w.Header().Set("Access-Control-Allow-Headers", req)
	} else {
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, x-api-key, anthropic-version")
	}
	w.Header().Set("Access-Control-Max-Age", corsPreflightMaxAge)
	w.WriteHeader(http.StatusNoContent)
	return true
}

// isV1APIPath reports whether a route path belongs to the CORS-covered API
// surface. Split out so the route dispatch and the tests agree on the scope.
func isV1APIPath(path string) bool { return strings.HasPrefix(path, "/v1/") }
