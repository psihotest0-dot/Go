package api

import (
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strings"
)

// The API has no token: it listens on loopback and identifies the user by a bare
// X-User-ID header. A web page can still reach it by DNS rebinding: the page's
// own domain starts to answer with 127.0.0.1, the browser treats the requests as
// same-origin, CORS does not apply, and X-User-ID is an ordinary header. What the
// page cannot fake is the Host header, which still holds its own domain name.
// HostCheck refuses every request whose Host is not one of the names this server
// is meant to be reached by.

// loopbackHosts are the names of the machine itself. host.docker.internal is how
// Prometheus and the trading bot, which run in containers, reach the backend.
var loopbackHosts = []string{"localhost", "127.0.0.1", "::1", "host.docker.internal"}

// maxLoggedHostLen bounds the Host value written to the log.
const maxLoggedHostLen = 100

// AllowedHosts returns the Host names the server accepts: the loopback names and,
// if listenAddr binds one specific address (LISTEN_ADDR), that address. An address
// that binds every interface (":8080", "0.0.0.0:8080") adds nothing: the names
// other machines would use are not known.
func AllowedHosts(listenAddr string) []string {
	hosts := append([]string(nil), loopbackHosts...)
	host, _, err := net.SplitHostPort(listenAddr)
	if err != nil {
		return hosts
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		return hosts
	}
	if name := hostName(host); name != "" && !slices.Contains(hosts, name) {
		hosts = append(hosts, name)
	}
	return hosts
}

// BeyondLoopbackWarning is the message logged at start when the server listens
// on more than this machine (ListensBeyondLoopback). The console shows it too.
const BeyondLoopbackWarning = "listening beyond loopback: the API identifies users by a bare X-User-ID header and /metrics has no authentication"

// ListensBeyondLoopback reports whether addr, the address the server listens on,
// can be reached from other machines: every interface ("", 0.0.0.0, ::), an
// address of a network card, or anything that cannot be read as a loopback
// address. It is meant for the bound address (net.Listener.Addr), where a name
// such as "localhost" is already an IP; "localhost" itself still counts as
// loopback. Listening there may be deliberate, so it is a warning, not an error.
func ListensBeyondLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return true
	}
	if strings.EqualFold(host, "localhost") {
		return false
	}
	ip := net.ParseIP(host)
	return ip == nil || !ip.IsLoopback()
}

// hostName reduces a Host header value to a comparable name: no port, no
// brackets around an IPv6 address, lower case, no trailing dot.
func hostName(hostport string) string {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	return strings.TrimSuffix(strings.ToLower(host), ".")
}

// HostCheck answers 421 (Misdirected Request) to a request whose Host is not in
// allowed, and logs it as a warning. Place it before CORS and the routes.
func HostCheck(allowed []string) func(http.Handler) http.Handler {
	set := make(map[string]struct{}, len(allowed))
	for _, h := range allowed {
		set[hostName(h)] = struct{}{}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if _, ok := set[hostName(r.Host)]; !ok {
				logged := r.Host
				if len(logged) > maxLoggedHostLen {
					logged = logged[:maxLoggedHostLen]
				}
				slog.WarnContext(r.Context(), "request refused: unexpected Host header",
					slog.String("host", logged), slog.String("method", r.Method), slog.String("path", r.URL.Path))
				http.Error(w, "Misdirected request", http.StatusMisdirectedRequest)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
