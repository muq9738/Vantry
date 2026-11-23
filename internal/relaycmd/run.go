// Package relaycmd runs the headless relay without importing desktop UI or Pi.
package relaycmd

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/muq9738/Vantry/internal/connectivity"
	"github.com/muq9738/Vantry/internal/desktoprelay"
	"golang.org/x/crypto/acme/autocert"
)

type Config struct {
	Listen                   string                    `json:"listen"`
	AllowInsecurePrivateHTTP bool                      `json:"allowInsecurePrivateHTTP,omitempty"`
	Certificate              string                    `json:"certificate,omitempty"`
	PrivateKey           string                    `json:"privateKey,omitempty"`
	ACMEHosts            []string                  `json:"acmeHosts,omitempty"`
	ACMEEmail            string                    `json:"acmeEmail,omitempty"`
	CertificateDirectory string                    `json:"certificateDirectory,omitempty"`
	Sessions             *connectivity.RelayConfig `json:"sessions,omitempty"`
	Desktop              *struct {
		RegistrationKey string `json:"registrationKey"`
	} `json:"desktop,omitempty"`
}

func Run(ctx context.Context, args []string, output io.Writer) error {
	if len(args) > 0 && args[0] == "init" {
		return initConfig(args[1:], output)
	}
	if len(args) > 0 && args[0] == "serve" {
		args = args[1:]
	}
	flags := flag.NewFlagSet("vantry-relay", flag.ContinueOnError)
	flags.SetOutput(output)
	configFile := flags.String("config", "", "私有中继 JSON 配置文件")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if *configFile == "" || flags.NArg() != 0 {
		return errors.New("用法：vantry-relay --config /path/to/relay.json")
	}
	file, err := os.Open(*configFile)
	if err != nil {
		return err
	}
	defer file.Close()
	var cfg Config
	decoder := json.NewDecoder(io.LimitReader(file, 64<<10))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&cfg); err != nil {
		return errors.New("中继配置格式无效，请检查字段和 JSON 结构")
	}
	return Serve(ctx, cfg, output)
}
func Serve(ctx context.Context, cfg Config, output io.Writer) error {
	if cfg.Listen == "" {
		cfg.Listen = "127.0.0.1:8787"
	}
	if cfg.Sessions == nil && cfg.Desktop == nil {
		return errors.New("至少启用 sessions 或 desktop 其中一项")
	}
	mux := http.NewServeMux()
	if cfg.Sessions != nil {
		relay, err := connectivity.NewRelay(*cfg.Sessions)
		if err != nil {
			return err
		}
		defer relay.Close()
		mux.Handle("/", relay)
	} else {
		mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
			connectivity.JSON(w, 200, map[string]string{"service": "vantry-relay"})
		})
	}
	if cfg.Desktop != nil {
		relay, err := desktoprelay.New(cfg.Desktop.RegistrationKey)
		if err != nil {
			return err
		}
		defer relay.Close()
		mux.Handle("/desktop/", relay)
		pairings, err := connectivity.NewRelay(connectivity.RelayConfig{RegistrationKey: cfg.Desktop.RegistrationKey})
		if err != nil {
			return err
		}
		defer pairings.Close()
		mux.Handle("/desktop/pairings", http.StripPrefix("/desktop", pairings))
		mux.Handle("/desktop/pairings/", http.StripPrefix("/desktop", pairings))
	}
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 90 * time.Second, MaxHeaderBytes: 32 << 10}
	if cfg.Certificate != "" || cfg.PrivateKey != "" {
		if cfg.Certificate == "" || cfg.PrivateKey == "" {
			return errors.New("certificate 与 privateKey 需要同时配置")
		}
		if _, err := tls.LoadX509KeyPair(cfg.Certificate, cfg.PrivateKey); err != nil {
			return errors.New("无法读取中继 TLS 证书和私钥")
		}
		server.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			cert, err := tls.LoadX509KeyPair(cfg.Certificate, cfg.PrivateKey)
			return &cert, err
		}}
	} else if len(cfg.ACMEHosts) > 0 {
		directory := cfg.CertificateDirectory
		if directory == "" {
			return errors.New("自动证书需要 certificateDirectory")
		}
		if !filepath.IsAbs(directory) {
			return errors.New("certificateDirectory 需要绝对路径")
		}
		manager := &autocert.Manager{Prompt: autocert.AcceptTOS, Cache: autocert.DirCache(directory), HostPolicy: autocert.HostWhitelist(cfg.ACMEHosts...), Email: cfg.ACMEEmail}
		server.TLSConfig = manager.TLSConfig()
		server.TLSConfig.MinVersion = tls.VersionTLS12
	} else {
		host, _, err := net.SplitHostPort(cfg.Listen)
		ip := net.ParseIP(host)
		if err != nil || (host != "localhost" && (ip == nil || (!ip.IsLoopback() && (!ip.IsPrivate() || !cfg.AllowInsecurePrivateHTTP)))) {
			return errors.New("公网及非回环监听需要直接配置 TLS 证书或 ACME；本机回环可使用 HTTP")
		}
	}
	listener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return err
	}
	defer listener.Close()
	fmt.Fprintf(output, "Vantry relay listening on %s (sessions=%t desktop=%t TLS=%t)\n", listener.Addr(), cfg.Sessions != nil, cfg.Desktop != nil, server.TLSConfig != nil)
	if server.TLSConfig != nil {
		listener = tls.NewListener(listener, server.TLSConfig)
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		// Close all accepted transports too, including upgraded WebSockets.
		_ = server.Close()
		err := <-done
		if err != nil && !errors.Is(err, http.ErrServerClosed) && !strings.Contains(err.Error(), "closed network connection") {
			return err
		}
		return nil
	}
}
