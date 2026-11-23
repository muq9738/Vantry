package remoteaccess

import (
	"crypto/tls"
	"crypto/x509"
	"net"
	"sort"
	"strings"
	"time"
)

// MobileRoutes contains public connection hints and the separate mobile TLS
// certificate. Native pairing retains its existing identity; no credential is included.
func (m *Manager) MobileRoutes() map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.server == nil {
		return nil
	}
	var direct []string
	// Prefer physical LAN interfaces over VPN/TUN and virtual bridge routes.
	physical := map[string]bool{}
	interfaces, _ := net.Interfaces()
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || !(strings.HasPrefix(iface.Name, "en") || strings.HasPrefix(iface.Name, "eth") || strings.HasPrefix(iface.Name, "wl")) {
			continue
		}
		addrs, _ := iface.Addrs()
		for _, address := range addrs {
			if network, ok := address.(*net.IPNet); ok {
				physical[network.IP.String()] = true
			}
		}
	}
	for _, address := range m.cfg.Pair.Direct {
		host, _, err := net.SplitHostPort(address)
		ip := net.ParseIP(host)
		if err != nil || ip == nil || ip.IsLoopback() || ip.IsUnspecified() || ip.IsMulticast() || ip.IsLinkLocalUnicast() {
			continue
		}
		if len(physical) > 0 && !physical[ip.String()] {
			continue
		}
		direct = append(direct, "https://"+address)
	}
	sort.SliceStable(direct, func(i, j int) bool { return strings.Contains(direct[j], "[") && !strings.Contains(direct[i], "[") })
	if len(direct) > 12 {
		direct = direct[:12]
	}
	return map[string]any{"direct": direct, "certificates": []string{m.cfg.MobileCertificate}, "serverName": "vantry.mobile", "relay": m.cfg.PublicURL}
}

func (m *Manager) ensureMobileCertificateLocked() (tls.Certificate, error) {
	pair, err := tls.X509KeyPair([]byte(m.cfg.MobileCertificate), []byte(m.cfg.MobilePrivateKey))
	if err == nil {
		certificate, parseErr := x509.ParseCertificate(pair.Certificate[0])
		if parseErr == nil && certificate.VerifyHostname("vantry.mobile") == nil && time.Now().Before(certificate.NotAfter.Add(-certRenewLeadWindow)) {
			return pair, nil
		}
	}
	certificate, key, err := generateMobileCertPair()
	if err != nil {
		return tls.Certificate{}, err
	}
	m.cfg.MobileCertificate, m.cfg.MobilePrivateKey = certificate, key
	return tls.X509KeyPair([]byte(certificate), []byte(key))
}
