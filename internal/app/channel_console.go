package app

import (
	"errors"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// The destination is derived from this daemon, never supplied as a URL by the
// caller. The console resolves the exact channel and credential before launch.
func channelConsoleURL(origin string, channelID int64, credentialID string) (string, error) {
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" && u.Path != "/" {
		return "", errors.New("invalid local console origin")
	}
	host := u.Hostname()
	if host != "localhost" && (net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback()) {
		return "", errors.New("console destination must be loopback")
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 || channelID <= 0 || strings.TrimSpace(credentialID) == "" {
		return "", errors.New("invalid channel console target")
	}
	query := url.Values{"channel_id": {strconv.FormatInt(channelID, 10)}, "credential_id": {credentialID}}
	return strings.TrimSuffix(origin, "/") + "/#/channels?" + query.Encode(), nil
}

func openChannelConsole(origin string, channelID int64, credentialID string) error {
	target, err := channelConsoleURL(origin, channelID, credentialID)
	if err != nil {
		return err
	}
	return openWithShell(target)
}
