package security

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"sync/atomic"
	"testing"
	"time"
)

func TestOutboundSafeDialer_ZoneIDAndLocalHostBlocking(t *testing.T) {
	d := &OutboundSafeDialer{}

	badURLs := []string{
		"http://[fe80::1%25eth0]/",
		"http://[64:ff9b::a9fe:a9fe%25eth0]/latest/meta-data/",
		"http://[2606:4700:4700::1111%25eth0]/",
		"http://printer.local/admin",
		"http://local/",
		"http://metadata.internal/",
		"http://internal/",
		"http://sub.localhost/",
		"http://example.com%25eth0/",
		"http://example.com/\x00bad",
	}
	for _, u := range badURLs {
		if _, err := d.ValidateOutboundURL(u); !errors.Is(err, ErrSSRFBlocked) {
			t.Fatalf("expected ValidateOutboundURL(%q) to fail with ErrSSRFBlocked, got %v", u, err)
		}
	}

	badHosts := []string{
		"fe80::1%eth0",
		"64:ff9b::a9fe:a9fe%eth0",
		"nas.local",
		"nas.local.",
		"local",
		"svc.internal",
		"localhost",
	}
	for _, h := range badHosts {
		if err := d.CheckHostAllowed(h); !errors.Is(err, ErrSSRFBlocked) {
			t.Fatalf("expected CheckHostAllowed(%q) to fail with ErrSSRFBlocked, got %v", h, err)
		}
	}
}

func TestOutboundSafeDialer_RedirectDowngradeHeaderStripAndSingleDNS(t *testing.T) {
	var gotAuth, gotProxyAuth, gotCookie atomic.Value
	targetSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth.Store(r.Header.Get("Authorization"))
		gotProxyAuth.Store(r.Header.Get("Proxy-Authorization"))
		gotCookie.Store(r.Header.Get("Cookie"))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer targetSrv.Close()

	parsedTarget, _ := url.Parse(targetSrv.URL)
	targetPort := parsedTarget.Port()

	redirectSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://cdn.example.test:"+targetPort+"/asset", http.StatusFound)
	}))
	defer redirectSrv.Close()
	parsedRedir, _ := url.Parse(redirectSrv.URL)
	redirPort := parsedRedir.Port()

	var dnsLookups atomic.Int32
	d := &OutboundSafeDialer{
		AllowLoopbackForTests: true,
		Resolver: func(_ context.Context, host string) ([]netip.Addr, error) {
			dnsLookups.Add(1)
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
		},
	}

	client := d.NewHTTPClient(5 * time.Second)

	// 1. Direct request without proxy must resolve DNS exactly ONCE per hostname (zero double-DNS).
	reqDirect, err := http.NewRequest(http.MethodGet, "http://cdn.example.test:"+targetPort+"/direct", nil)
	if err != nil {
		t.Fatal(err)
	}
	respDirect, err := client.Do(reqDirect)
	if err != nil {
		t.Fatalf("direct GET failed: %v", err)
	}
	_ = respDirect.Body.Close()
	if got := dnsLookups.Load(); got != 1 {
		t.Fatalf("expected exactly 1 DNS lookup without proxy, got %d", got)
	}

	// 2. Cross-origin redirect must strip Authorization, Proxy-Authorization, and Cookie headers.
	clientCross := d.NewHTTPClient(5 * time.Second)
	reqCross, err := http.NewRequest(http.MethodGet, "http://origin.example.test:"+redirPort+"/start", nil)
	if err != nil {
		t.Fatal(err)
	}
	reqCross.Header.Set("Authorization", "Bearer secret-token")
	reqCross.Header.Set("Proxy-Authorization", "Basic cHJveHk6cGFzcw==")
	reqCross.Header.Set("Cookie", "sid=super-secret")
	respCross, err := clientCross.Do(reqCross)
	if err != nil {
		t.Fatalf("cross-origin redirect GET failed: %v", err)
	}
	_ = respCross.Body.Close()
	if gotAuth.Load().(string) != "" || gotProxyAuth.Load().(string) != "" || gotCookie.Load().(string) != "" {
		t.Fatalf("expected auth/cookie headers stripped on cross-origin redirect, got auth=%q proxyAuth=%q cookie=%q",
			gotAuth.Load(), gotProxyAuth.Load(), gotCookie.Load())
	}

	// 3. HTTPS -> HTTP redirect downgrade must be blocked by CheckRedirect!
	reqDowngrade, _ := http.NewRequest(http.MethodGet, "http://cdn.example.test/downgrade", nil)
	prevHTTPS, _ := http.NewRequest(http.MethodGet, "https://origin.example.test/secure", nil)
	if err := client.CheckRedirect(reqDowngrade, []*http.Request{prevHTTPS}); !errors.Is(err, ErrSSRFBlocked) {
		t.Fatalf("expected https->http redirect downgrade to return ErrSSRFBlocked, got %v", err)
	}

	// 4. When a proxy IS active, ssrfValidatingRoundTripper must still pre-validate target DNS against SSRF!
	privateDialer := &OutboundSafeDialer{
		Resolver: func(_ context.Context, host string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("169.254.169.254")}, nil
		},
	}
	proxyURL, _ := url.Parse(targetSrv.URL)
	rt := &ssrfValidatingRoundTripper{
		dialer: privateDialer,
		next:   http.DefaultTransport,
		proxyFunc: func(*http.Request) (*url.URL, error) {
			return proxyURL, nil
		},
	}
	reqProxied, _ := http.NewRequest(http.MethodGet, "http://metadata-rebind.example.com/latest", nil)
	if _, err := rt.RoundTrip(reqProxied); !errors.Is(err, ErrSSRFBlocked) {
		t.Fatalf("expected proxied request to private-resolving hostname to fail with ErrSSRFBlocked, got %v", err)
	}
}
