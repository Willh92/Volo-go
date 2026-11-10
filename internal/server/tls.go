package server

import (
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Willh92/Volo-go/internal/config"
)

// defaultCertCheckInterval throttles the cheap os.Stat freshness probe performed during TLS
// handshakes, so certificates renewed in place (acme.sh / certbot) are picked up without restart.
const defaultCertCheckInterval = 30 * time.Second

type fileStamp struct {
	mod  time.Time
	size int64
}

func statStamp(path string) (fileStamp, error) {
	st, err := os.Stat(path)
	if err != nil {
		return fileStamp{}, err
	}
	return fileStamp{mod: st.ModTime(), size: st.Size()}, nil
}

// certReloader serves a cached tls.Certificate via tls.Config.GetCertificate and transparently
// reloads it when the cert or key file's ModTime/size changes. On reload failure the last good
// certificate keeps being served (with a warning) so a half-written renewal never takes the site down.
type certReloader struct {
	certFile string
	keyFile  string
	interval time.Duration

	mu        sync.Mutex
	cert      *tls.Certificate
	certStamp fileStamp
	keyStamp  fileStamp
	lastCheck time.Time
}

// newCertReloader loads and validates the key pair immediately (fail-fast at startup).
func newCertReloader(certFile, keyFile string) (*certReloader, error) {
	r := &certReloader{certFile: certFile, keyFile: keyFile, interval: defaultCertCheckInterval}
	cs, ks, err := r.stamps()
	if err != nil {
		return nil, err
	}
	if err := r.loadLocked(cs, ks); err != nil {
		return nil, err
	}
	r.lastCheck = time.Now()
	if leaf := r.cert.Leaf; leaf != nil && time.Now().After(leaf.NotAfter) {
		slog.Warn("tls certificate is expired", "component", "tls", "cert", certFile, "not_after", leaf.NotAfter.UTC().Format(time.RFC3339))
	}
	return r, nil
}

func (r *certReloader) stamps() (fileStamp, fileStamp, error) {
	cs, err := statStamp(r.certFile)
	if err != nil {
		return fileStamp{}, fileStamp{}, fmt.Errorf("server: tls certificate %q: %w", r.certFile, err)
	}
	ks, err := statStamp(r.keyFile)
	if err != nil {
		return fileStamp{}, fileStamp{}, fmt.Errorf("server: tls private key %q: %w", r.keyFile, err)
	}
	return cs, ks, nil
}

func (r *certReloader) loadLocked(cs, ks fileStamp) error {
	pair, err := tls.LoadX509KeyPair(r.certFile, r.keyFile)
	if err != nil {
		return fmt.Errorf("server: load tls key pair (cert=%q key=%q): %w", r.certFile, r.keyFile, err)
	}
	r.cert = &pair
	r.certStamp = cs
	r.keyStamp = ks
	return nil
}

// GetCertificate implements tls.Config.GetCertificate with throttled hot reload.
func (r *certReloader) GetCertificate(_ *tls.ClientHelloInfo) (*tls.Certificate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	if r.cert != nil && now.Sub(r.lastCheck) < r.interval {
		return r.cert, nil
	}
	r.lastCheck = now
	cs, ks, err := r.stamps()
	if err != nil {
		if r.cert == nil {
			return nil, err
		}
		slog.Warn("tls certificate reload check failed; keeping last good certificate", "component", "tls", "err", err)
		return r.cert, nil
	}
	if r.cert != nil && cs == r.certStamp && ks == r.keyStamp {
		return r.cert, nil
	}
	if err := r.loadLocked(cs, ks); err != nil {
		if r.cert == nil {
			return nil, err
		}
		// Stamps are intentionally not advanced so the next check retries (e.g. key not yet rotated).
		slog.Warn("tls certificate reload failed; keeping last good certificate", "component", "tls", "err", err)
		return r.cert, nil
	}
	slog.Info("tls certificate reloaded", "component", "tls", "cert", r.certFile)
	return r.cert, nil
}

func parseTLSMinVersion(v string) (uint16, error) {
	switch strings.TrimSpace(v) {
	case "", "1.2":
		return tls.VersionTLS12, nil
	case "1.3":
		return tls.VersionTLS13, nil
	}
	return 0, fmt.Errorf("server: unsupported TLS minimum version %q (must be 1.2 or 1.3)", v)
}

// buildTLSConfig validates the configured key pair and returns a server tls.Config that
// negotiates HTTP/2 ("h2") with HTTP/1.1 fallback and hot-reloads renewed certificates.
func buildTLSConfig(cfg *config.Config) (*tls.Config, *certReloader, error) {
	minVer, err := parseTLSMinVersion(cfg.TLSMinVersion)
	if err != nil {
		return nil, nil, err
	}
	reloader, err := newCertReloader(cfg.TLSCertFile, cfg.TLSKeyFile)
	if err != nil {
		return nil, nil, err
	}
	return &tls.Config{
		MinVersion:     minVer,
		NextProtos:     []string{"h2", "http/1.1"},
		GetCertificate: reloader.GetCertificate,
	}, reloader, nil
}

// newHTTPSRedirectHandler 308-redirects plain HTTP requests to HTTPS. The target host is derived
// from the configured VOLO_PUBLIC_URL hostname (never from the client's Host header, preventing
// Host-header injection / open redirects). The port is the explicit port of an https:// public URL
// when present (it is the externally reachable address, e.g. behind Docker port publishing),
// otherwise the TLS listen port when it is not 443.
func newHTTPSRedirectHandler(cfg *config.Config, tlsPort string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := "localhost"
		port := tlsPort
		if u, err := url.Parse(cfg.GetPublicURL()); err == nil && u.Hostname() != "" {
			host = u.Hostname()
			if strings.EqualFold(u.Scheme, "https") && u.Port() != "" {
				port = u.Port()
			}
		}
		authority := host
		if port != "" && port != "443" {
			authority = net.JoinHostPort(host, port)
		} else if strings.Contains(host, ":") {
			authority = "[" + host + "]"
		}
		target := r.URL.RequestURI()
		if !strings.HasPrefix(target, "/") || strings.HasPrefix(target, "//") {
			target = "/"
		}
		w.Header().Set("Connection", "close")
		http.Redirect(w, r, "https://"+authority+target, http.StatusPermanentRedirect)
	})
}

// openTLSRedirectListener binds the optional plain-HTTP redirect listener (fail fast at startup).
func openTLSRedirectListener(cfg *config.Config, tlsAddr net.Addr) (net.Listener, http.Handler, error) {
	addr := strings.TrimSpace(cfg.TLSHTTPRedirectAddr)
	if addr == "" {
		return nil, nil, nil
	}
	tlsPort := ""
	if tcp, ok := tlsAddr.(*net.TCPAddr); ok {
		tlsPort = strconv.Itoa(tcp.Port)
	} else if _, p, err := net.SplitHostPort(tlsAddr.String()); err == nil {
		tlsPort = p
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, nil, fmt.Errorf("server: listen tcp %s (https redirect): %w", addr, err)
	}
	return ln, newHTTPSRedirectHandler(cfg, tlsPort), nil
}

func newRedirectServer(handler http.Handler) *http.Server {
	return &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    16 * 1024,
	}
}
