package remoteaccess

import (
	"errors"
	"net"
	"net/url"
	"strings"
)

func mobilePublicURL(raw string) (string, error) {
	value, err := ValidateRelayWithOptions(raw, false)
	if err != nil {
		return "", err
	}
	u, _ := url.Parse(value)
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	ip := net.ParseIP(host)
	if u.Scheme != "https" || host == "localhost" || strings.HasSuffix(host, ".localhost") || (ip != nil && (ip.IsLoopback() || ip.IsUnspecified())) {
		return "", errors.New("Mobile pairing needs a reachable HTTPS address, not this computer's localhost address")
	}
	if u.Path != "" && u.Path != "/" {
		return "", errors.New("Mobile pairing needs a root HTTPS address without a path")
	}
	return value, nil
}
