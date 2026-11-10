package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Willh92/Volo-go/internal/config"
)

// writeSelfSignedPair writes a fresh ECDSA P-256 self-signed cert/key for 127.0.0.1 and returns its serial.
func writeSelfSignedPair(t *testing.T, certPath, keyPath string, serial int64, notAfter time.Time) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject:      pkix.Name{CommonName: "volo-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
}

func bumpMTime(t *testing.T, paths ...string) {
	t.Helper()
	future := time.Now().Add(time.Duration(len(paths)+1) * time.Minute)
	for _, p := range paths {
		if err := os.Chtimes(p, future, future); err != nil {
			t.Fatal(err)
		}
	}
}

func newH2TestClient() *http.Client {
	return &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			ForceAttemptHTTP2: true,
			DisableKeepAlives: true,                                  // force a fresh handshake per request so reloads are observable
			TLSClientConfig:   &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // self-signed test cert
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func TestNativeTLSServesHTTP2AndHotReloadsCertificate(t *testing.T) {
	tmp := t.TempDir()
	certPath := filepath.Join(tmp, "cert.pem")
	keyPath := filepath.Join(tmp, "key.pem")
	writeSelfSignedPair(t, certPath, keyPath, 1001, time.Now().Add(90*24*time.Hour))

	cfg := &config.Config{
		ListenAddr:          "127.0.0.1:0",
		PublicURL:           "https://blog.example.test",
		TLSCertFile:         certPath,
		TLSKeyFile:          keyPath,
		TLSMinVersion:       "1.2",
		TLSHTTPRedirectAddr: "127.0.0.1:0",
	}
	handle, err := OpenListener(cfg)
	if err != nil {
		t.Fatalf("OpenListener: %v", err)
	}
	if handle.TLSConfig == nil || handle.RedirectListener == nil {
		t.Fatal("expected TLS config and redirect listener")
	}
	handle.tlsReloader.interval = 0 // check freshness on every handshake for the test

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- ServeWithGracefulShutdown(ctx, handle, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.TLS == nil {
				t.Errorf("expected r.TLS to be set")
			}
			_, _ = io.WriteString(w, r.Proto)
		}), 3*time.Second)
	}()

	client := newH2TestClient()
	baseURL := "https://" + handle.Address
	getSerial := func() int64 {
		t.Helper()
		resp, err := client.Get(baseURL + "/healthz")
		if err != nil {
			t.Fatalf("GET over TLS: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.ProtoMajor != 2 || string(body) != "HTTP/2.0" {
			t.Fatalf("expected HTTP/2, got proto=%s body=%q", resp.Proto, body)
		}
		if resp.TLS == nil || len(resp.TLS.PeerCertificates) == 0 {
			t.Fatal("missing peer certificate")
		}
		return resp.TLS.PeerCertificates[0].SerialNumber.Int64()
	}

	if got := getSerial(); got != 1001 {
		t.Fatalf("expected initial serial 1001, got %d", got)
	}

	// Renew in place (as acme.sh/certbot would) -> new handshakes must see the new certificate.
	writeSelfSignedPair(t, certPath, keyPath, 2002, time.Now().Add(90*24*time.Hour))
	bumpMTime(t, certPath, keyPath)
	if got := getSerial(); got != 2002 {
		t.Fatalf("expected reloaded serial 2002, got %d", got)
	}

	// A broken renewal must keep serving the last good certificate.
	if err := os.WriteFile(certPath, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	bumpMTime(t, certPath)
	if got := getSerial(); got != 2002 {
		t.Fatalf("expected last good serial 2002 after failed reload, got %d", got)
	}

	// Plain HTTP redirect listener: 308 to the PUBLIC_URL host (not the attacker-controlled Host header).
	_, tlsPort, _ := net.SplitHostPort(handle.Address)
	req, _ := http.NewRequest(http.MethodGet, "http://"+handle.RedirectListener.Addr().String()+"/posts/1?x=y", nil)
	req.Host = "evil.example"
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("redirect GET: %v", err)
	}
	_ = resp.Body.Close()
	wantLoc := "https://blog.example.test:" + tlsPort + "/posts/1?x=y"
	if resp.StatusCode != http.StatusPermanentRedirect || resp.Header.Get("Location") != wantLoc {
		t.Fatalf("expected 308 -> %s, got %d %q", wantLoc, resp.StatusCode, resp.Header.Get("Location"))
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("graceful shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not shut down")
	}
	if _, err := net.DialTimeout("tcp", handle.RedirectListener.Addr().String(), 300*time.Millisecond); err == nil {
		t.Fatal("redirect listener still accepting after shutdown")
	}
}

func TestNativeTLSFailFastAndMinVersion(t *testing.T) {
	tmp := t.TempDir()
	certPath := filepath.Join(tmp, "cert.pem")
	keyPath := filepath.Join(tmp, "key.pem")
	otherCert := filepath.Join(tmp, "other.pem")
	otherKey := filepath.Join(tmp, "other.key")
	writeSelfSignedPair(t, certPath, keyPath, 1, time.Now().Add(time.Hour))
	writeSelfSignedPair(t, otherCert, otherKey, 2, time.Now().Add(time.Hour))

	cases := map[string]*config.Config{
		"missing cert":   {ListenAddr: "127.0.0.1:0", TLSCertFile: filepath.Join(tmp, "nope.pem"), TLSKeyFile: keyPath},
		"mismatched key": {ListenAddr: "127.0.0.1:0", TLSCertFile: certPath, TLSKeyFile: otherKey},
		"bad version":    {ListenAddr: "127.0.0.1:0", TLSCertFile: certPath, TLSKeyFile: keyPath, TLSMinVersion: "1.0"},
	}
	for name, cfg := range cases {
		if h, err := OpenListener(cfg); err == nil {
			_ = h.Close()
			t.Fatalf("%s: expected OpenListener to fail fast", name)
		}
	}

	cfg := &config.Config{ListenAddr: "127.0.0.1:0", TLSCertFile: certPath, TLSKeyFile: keyPath, TLSMinVersion: "1.3"}
	handle, err := OpenListener(cfg)
	if err != nil {
		t.Fatalf("OpenListener: %v", err)
	}
	if handle.TLSConfig.MinVersion != tls.VersionTLS13 || handle.RedirectListener != nil {
		t.Fatalf("unexpected tls config: min=%x redirect=%v", handle.TLSConfig.MinVersion, handle.RedirectListener)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- ServeWithGracefulShutdown(ctx, handle, http.NotFoundHandler(), time.Second)
	}()
	// A TLS 1.2-only client must be rejected.
	conn, err := tls.Dial("tcp", handle.Address, &tls.Config{InsecureSkipVerify: true, MaxVersion: tls.VersionTLS12}) //nolint:gosec // test
	if err == nil {
		_ = conn.Close()
		t.Fatal("expected TLS 1.2 handshake to be rejected with min version 1.3")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

func TestHTTPSRedirectHandlerHostAndPort(t *testing.T) {
	cases := []struct {
		pub, port, uri, want string
	}{
		{"https://blog.example.com/", "443", "/a?b=c", "https://blog.example.com/a?b=c"},
		{"http://localhost:8080", "8443", "/", "https://localhost:8443/"},
		{"https://[::1]/", "443", "/x", "https://[::1]/x"},
		// Explicit https:// public URL port is the externally reachable address (e.g. Docker -p 9443:8443).
		{"https://[::1]:9000/", "443", "/x", "https://[::1]:9000/x"},
		{"https://blog.example.com:9443", "8443", "/p", "https://blog.example.com:9443/p"},
		{"https://blog.example.com:443", "8443", "/p", "https://blog.example.com/p"},
		{"https://blog.example.com", "443", "//evil.example/x", "https://blog.example.com/"},
	}
	for _, tc := range cases {
		cfg := &config.Config{PublicURL: tc.pub}
		rec := &recordingWriter{header: http.Header{}}
		req, _ := http.NewRequest(http.MethodGet, "http://attacker.example"+tc.uri, nil)
		req.Host = "attacker.example"
		newHTTPSRedirectHandler(cfg, tc.port).ServeHTTP(rec, req)
		if rec.status != http.StatusPermanentRedirect || rec.header.Get("Location") != tc.want {
			t.Fatalf("pub=%s port=%s uri=%s: got %d %q want %q", tc.pub, tc.port, tc.uri, rec.status, rec.header.Get("Location"), tc.want)
		}
		if strings.Contains(rec.header.Get("Location"), "attacker") {
			t.Fatal("redirect leaked client Host header")
		}
	}
}

type recordingWriter struct {
	header http.Header
	status int
}

func (w *recordingWriter) Header() http.Header         { return w.header }
func (w *recordingWriter) Write(b []byte) (int, error) { return len(b), nil }
func (w *recordingWriter) WriteHeader(code int)        { w.status = code }
