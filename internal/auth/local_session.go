package auth

import (
	"net"
	"net/http"
	"net/url"
	"time"
)

func loopbackHost(authority string) bool {
	host := authority
	if name, _, err := net.SplitHostPort(authority); err == nil {
		host = name
	}
	return host == "localhost" || (net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback())
}

// Local consoles establish a normal management session without asking their
// owner to copy a key. This does not grant model API access or remote login.
func (a *Auth) localSession(w http.ResponseWriter, r *http.Request) {
	peer, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil || net.ParseIP(peer) == nil || !net.ParseIP(peer).IsLoopback() || !loopbackHost(r.Host) || r.Header.Get("X-To-Router") != "1" {
		denied(w, http.StatusForbidden, "local management request required")
		return
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		if err != nil || u.Scheme != "http" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || (u.Host != r.Host && u.Host != "127.0.0.1:5173" && u.Host != "localhost:5173") {
			denied(w, http.StatusForbidden, "local console origin required")
			return
		}
	}
	if c, err := r.Cookie(cookieName); err == nil && a.Store.ValidAdminSession(c.Value) {
		reply(w, http.StatusOK, map[string]bool{"authenticated": true})
		return
	}
	settings, err := a.Store.GetSettings()
	if err != nil {
		denied(w, 500, "settings unavailable")
		return
	}
	token, err := a.Store.CreateAdminSession(settings.GatewayKey, 12*time.Hour)
	if err != nil {
		denied(w, 500, "cannot create session")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: token, Path: "/api", HttpOnly: true, SameSite: http.SameSiteStrictMode})
	reply(w, http.StatusOK, map[string]bool{"authenticated": true})
}
