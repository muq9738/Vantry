// Package remoteaccess connects Vantry workspaces through a relay gateway.
// The relay carries opaque TLS streams; Core remains the API authority.
package remoteaccess

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const certRenewLeadWindow = 30 * 24 * time.Hour

type Pair struct {
	Version         int       `json:"v"`
	T3URL           string    `json:"t3Url,omitempty"`
	T3Token         string    `json:"t3Token,omitempty"`
	Name            string    `json:"name"`
	Relay           string    `json:"relay"`
	Relays          []string  `json:"relays,omitempty"`
	Route           string    `json:"route"`
	Secret          string    `json:"secret"`
	Certificate     string    `json:"certificate"`
	NextCertificate string    `json:"nextCertificate,omitempty"`
	DeviceID        string    `json:"deviceId,omitempty"`
	Direct          []string  `json:"direct,omitempty"`
	Code            string    `json:"code,omitempty"`
	ExpiresAt       time.Time `json:"expiresAt,omitempty"`
}

func (p Pair) relayList() []string {
	var out []string
	add := func(raw string) {
		raw = strings.TrimSpace(raw)
		if raw != "" && !slices.Contains(out, raw) {
			out = append(out, raw)
		}
	}
	add(p.Relay)
	for _, extra := range p.Relays {
		add(extra)
	}
	return out
}

type RelayEndpoint struct {
	URL             string `json:"url"`
	RegistrationKey string `json:"registrationKey,omitempty"`
}

type Peer struct {
	ID   string `json:"id"`
	Pair Pair   `json:"pair"`
}

type PeerInfo struct {
	ID          string   `json:"id"`
	DeviceID    string   `json:"deviceId,omitempty"`
	Name        string   `json:"name"`
	Relay       string   `json:"relay"`
	Relays      []string `json:"relays,omitempty"`
	Direct      []string `json:"direct,omitempty"`
	Nearby      bool     `json:"nearby"`
	Reachable   *bool    `json:"reachable,omitempty"`
	Mode        string   `json:"mode,omitempty"`
	LatencyMs   int      `json:"latencyMs,omitempty"`
	HandshakeMs int      `json:"handshakeMs,omitempty"`
	RTTMs       int      `json:"rttMs,omitempty"`
	ErrorCode   string   `json:"errorCode,omitempty"`
	LastError   string   `json:"lastError,omitempty"`
}

type NearbyDevice struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Addresses []string `json:"addresses"`
	Port      int      `json:"port"`
	Removed   bool     `json:"removed,omitempty"`
}

type configuration struct {
	Enabled           bool            `json:"enabled"`
	Pair              Pair            `json:"host"`
	PrivateKey        string          `json:"privateKey,omitempty"`
	MobileCertificate string          `json:"mobileCertificate,omitempty"`
	MobilePrivateKey  string          `json:"mobilePrivateKey,omitempty"`
	NextCertificate   string          `json:"nextCertificate,omitempty"`
	NextPrivateKey    string          `json:"nextPrivateKey,omitempty"`
	RegistrationKey   string          `json:"registrationKey,omitempty"`
	Relays            []RelayEndpoint `json:"relays,omitempty"`
	Peers             []Peer          `json:"peers,omitempty"`
	DirectPort        int             `json:"directPort,omitempty"`
	PublicURL         string          `json:"publicUrl,omitempty"`
	T3Listen          string          `json:"t3Listen,omitempty"`
	Clients           []AccessClient  `json:"clients,omitempty"`
}

type AccessClient struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"createdAt"`
	Secret    string    `json:"secret,omitempty"`
}

func ValidateRelay(raw string) (string, error) {
	return ValidateRelayWithOptions(raw, os.Getenv("VANTRY_ALLOW_INSECURE_LAN_RELAY") == "1")
}

func ValidateRelayWithOptions(raw string, allowInsecurePrivate bool) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("请输入中继的 HTTPS 地址")
	}
	if u.Scheme != "https" {
		ip := net.ParseIP(u.Hostname())
		if u.Scheme != "http" || (u.Hostname() != "localhost" && (ip == nil || (!ip.IsLoopback() && (!ip.IsPrivate() || !allowInsecurePrivate)))) {
			return "", errors.New("公网或局域网中继需要 HTTPS（私网明文 HTTP 仅限显式测试选项）")
		}
	}
	return strings.TrimRight(u.String(), "/"), nil
}

func randomID() string {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(value)
}

func generateCertPair() (string, string, error) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", "", err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		return "", "", err
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "Vantry remote access"},
		DNSNames:              []string{"vantry.remote"},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.AddDate(1, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, public, private)
	if err != nil {
		return "", "", err
	}
	key, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		return "", "", err
	}
	certPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	keyPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}))
	return certPEM, keyPEM, nil
}

func newIdentity(name, relay string) (Pair, string, error) {
	certPEM, keyPEM, err := generateCertPair()
	if err != nil {
		return Pair{}, "", err
	}
	pair := Pair{
		Version:     1,
		Name:        name,
		Relay:       relay,
		DeviceID:    randomID(),
		Route:       randomID(),
		Secret:      randomID(),
		Certificate: certPEM,
	}
	return pair, keyPEM, nil
}

func parseAndValidateCert(pemStr string, allowExpired bool) (*x509.Certificate, error) {
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil {
		return nil, errors.New("缺少主机身份证书")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil || cert.VerifyHostname("vantry.remote") != nil {
		return nil, errors.New("主机身份证书无效")
	}
	if !allowExpired && time.Now().After(cert.NotAfter) {
		return nil, errors.New("主机身份证书已过期，请重新配对")
	}
	return cert, nil
}

func certPoolFromPair(pair Pair) (*x509.CertPool, error) {
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM([]byte(pair.Certificate)) {
		return nil, errors.New("主机身份无效，请重新配对")
	}
	if pair.NextCertificate != "" {
		if _, err := parseAndValidateCert(pair.NextCertificate, true); err == nil {
			roots.AppendCertsFromPEM([]byte(pair.NextCertificate))
		}
	}
	return roots, nil
}

func (p Pair) Link() string {
	data, _ := json.Marshal(p)
	return "vantry://connect#" + base64.RawURLEncoding.EncodeToString(data)
}

func ParseLink(link string) (Pair, error) {
	return parseLinkWithOptions(link, os.Getenv("VANTRY_ALLOW_INSECURE_LAN_RELAY") == "1")
}

func parseLinkWithOptions(link string, allowInsecurePrivate bool) (Pair, error) {
	u, err := url.Parse(strings.TrimSpace(link))
	if err != nil || u.Scheme != "vantry" || u.Host != "connect" || len(u.Fragment) > 16<<10 {
		return Pair{}, errors.New("请粘贴 Vantry 主机提供的配对链接")
	}
	data, err := base64.RawURLEncoding.DecodeString(u.Fragment)
	if err != nil {
		return Pair{}, errors.New("配对链接不完整")
	}
	var pair Pair
	if json.Unmarshal(data, &pair) == nil && pair.Version == 3 {
		if len(pair.T3Token) < 16 || pair.DeviceID == "" {
			return Pair{}, errors.New("T3 设备授权无效")
		}
		base, err := ValidateRelayWithOptions(pair.T3URL, allowInsecurePrivate)
		if err != nil {
			return Pair{}, err
		}
		u, _ := url.Parse(base)
		if u.Path != "" && u.Path != "/" {
			return Pair{}, errors.New("T3 设备需要根路径地址")
		}
		pair.T3URL = base
		return pair, nil
	}
	if json.Unmarshal(data, &pair) != nil || (pair.Version != 1 && pair.Version != 2) || len(pair.Route) < 32 || (pair.Version == 1 && len(pair.Secret) < 32) || (pair.Version == 2 && (pair.Code == "" || !time.Now().Before(pair.ExpiresAt))) {
		return Pair{}, errors.New("配对链接无效或版本不支持")
	}
	if pair.Relay != "" {
		pair.Relay, err = ValidateRelayWithOptions(pair.Relay, allowInsecurePrivate)
		if err != nil {
			return Pair{}, err
		}
	}
	if len(pair.Relays) > 0 {
		validatedRelays := make([]string, 0, len(pair.Relays))
		for _, r := range pair.Relays {
			v, rErr := ValidateRelayWithOptions(r, allowInsecurePrivate)
			if rErr != nil {
				return Pair{}, rErr
			}
			if !slices.Contains(validatedRelays, v) {
				validatedRelays = append(validatedRelays, v)
			}
		}
		pair.Relays = validatedRelays
	}
	for _, address := range pair.Direct {
		if _, _, err := net.SplitHostPort(address); err != nil {
			return Pair{}, errors.New("配对链接包含无效的直连地址")
		}
	}
	if pair.Relay == "" && len(pair.Relays) == 0 && len(pair.Direct) == 0 {
		return Pair{}, errors.New("配对链接缺少连接地址")
	}
	if _, err := parseAndValidateCert(pair.Certificate, false); err != nil {
		return Pair{}, errors.New("主机身份无效或已过期，请重新配对")
	}
	if pair.NextCertificate != "" {
		if _, err := parseAndValidateCert(pair.NextCertificate, false); err != nil {
			pair.NextCertificate = ""
		}
	}
	return pair, nil
}

func loadConfig(dir string) (configuration, error) {
	data, err := os.ReadFile(filepath.Join(dir, "remote-access.json"))
	if errors.Is(err, os.ErrNotExist) {
		return configuration{}, nil
	}
	if err != nil {
		return configuration{}, err
	}
	var cfg configuration
	err = json.Unmarshal(data, &cfg)
	return cfg, err
}

func saveConfig(dir string, cfg configuration) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".remote-access-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), filepath.Join(dir, "remote-access.json"))
}

// URLSession does not negotiate the native Ed25519 TLS identity. Give mobile
// clients a standard P-256 identity without rotating existing native pairings.
func generateMobileCertPair() (string, string, error) {
	private, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", "", err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		return "", "", err
	}
	now := time.Now()
	template := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "Vantry mobile access"}, DNSNames: []string{"vantry.mobile"}, NotBefore: now.Add(-time.Minute), NotAfter: now.AddDate(1, 0, 0), KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &private.PublicKey, private)
	if err != nil {
		return "", "", err
	}
	key, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		return "", "", err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key})), nil
}
