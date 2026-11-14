package esp

// Request guards shared by the admin panel (:26080) and the Galnet server (:80). hostGuard
// refuses a Host header that is not one of ours, which is what a DNS-rebinding page arrives
// with; csrfGuard refuses a state-changing request whose Origin or Referer names another
// site. Neither needs a token in the forms. See docs/security-audit-2026-11.md.

import (
	"net"
	"net/http"
	"net/url"
	"strings"
)

// AllowedHosts is the extra Host names the two HTTP servers answer to, beyond loopback, the
// Galnet names and each listener's own address (config.json esp.allowedHosts). Set before
// esp.New.
var AllowedHosts []string

// builtinHosts is what every install answers to: loopback under its usual names and the two
// CCP hostnames the client's hosts file points at this server.
var builtinHosts = []string{"localhost", "127.0.0.1", "::1", "myeve.eve-online.com", "img.eve.is"}

// hostAllowed reports whether a Host header value belongs to this server. The port is ignored;
// an empty Host (HTTP/1.0 client) passes, since a browser always sends one.
func hostAllowed(hostHeader string, listenAddr string) bool {
	host := strings.ToLower(strings.TrimSpace(hostHeader))
	if host == "" {
		return true
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return true
	}
	for _, h := range builtinHosts {
		if host == h {
			return true
		}
	}
	for _, h := range AllowedHosts {
		if host == strings.ToLower(strings.TrimSpace(h)) {
			return true
		}
	}
	if lh, _, err := net.SplitHostPort(listenAddr); err == nil {
		lh = strings.ToLower(strings.Trim(lh, "[]"))
		if lh != "" && lh != "0.0.0.0" && lh != "::" && host == lh {
			return true
		}
	}
	return false
}

// hostGuard answers 421 to a request for a host this server is not.
func hostGuard(listenAddr string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !hostAllowed(r.Host, listenAddr) {
			http.Error(w, "Misdirected Request", http.StatusMisdirectedRequest)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// crossSite reports whether a request's Origin (or, without one, Referer) names a host other
// than the one it was sent to. A request with neither header (curl, the in-game browser) is
// not cross-site: a browser form or fetch always carries at least one.
func crossSite(r *http.Request) bool {
	reqHost := strings.ToLower(r.Host)
	if o := r.Header.Get("Origin"); o != "" {
		if o == "null" {
			return true
		}
		u, err := url.Parse(o)
		if err != nil {
			return true
		}
		return !sameHost(u.Host, reqHost)
	}
	if ref := r.Header.Get("Referer"); ref != "" {
		u, err := url.Parse(ref)
		if err != nil {
			return true
		}
		return !sameHost(u.Host, reqHost)
	}
	return false
}

// sameHost compares two host[:port] strings, ignoring case and a default or missing port.
func sameHost(a, b string) bool {
	norm := func(h string) string {
		h = strings.ToLower(h)
		if host, port, err := net.SplitHostPort(h); err == nil {
			if port == "80" {
				return host
			}
			return host + ":" + port
		}
		return h
	}
	return norm(a) == norm(b)
}

// csrfGuard answers 403 to a form or API request that another site sent; reads pass untouched.
func csrfGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
		default:
			if crossSite(r) {
				http.Error(w, "Forbidden: cross-site request", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
