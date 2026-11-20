package backup

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/Willh92/Volo-go/internal/security"
)

func TestNewWebDAVClient_NilDialerBlocksSSRF(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	_, _, err := newWebDAVClient(srv.URL, nil, 5*time.Second)
	if err == nil || !errors.Is(err, security.ErrSSRFBlocked) {
		t.Fatalf("expected ErrSSRFBlocked when safeDialer is nil, got: %v", err)
	}
}

func TestNewWebDAVClient_RedirectStripsHeadersAndBlocksDowngrade(t *testing.T) {
	var targetAuth, targetCookie string
	targetSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetAuth = r.Header.Get("Authorization")
		targetCookie = r.Header.Get("Cookie")
		w.WriteHeader(http.StatusOK)
	}))
	defer targetSrv.Close()

	redirectSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Redirect to a different host (same loopback IP, different hostname) to trigger cross-host header stripping.
		targetHost := strings.TrimPrefix(targetSrv.URL, "http://")
		_, port, _ := strings.Cut(targetHost, ":")
		http.Redirect(w, r, "http://other.example.test:"+port+"/dav", http.StatusFound)
	}))
	defer redirectSrv.Close()

	dialer := &security.OutboundSafeDialer{
		Timeout:               5 * time.Second,
		AllowLoopbackForTests: true,
		Resolver: func(_ context.Context, host string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
		},
	}

	_, redirectPort, _ := strings.Cut(strings.TrimPrefix(redirectSrv.URL, "http://"), ":")
	u, client, err := newWebDAVClient("http://origin.example.test:"+redirectPort+"/dav", dialer, 5*time.Second)
	if err != nil {
		t.Fatalf("newWebDAVClient: %v", err)
	}

	req, err := http.NewRequest(http.MethodGet, u.String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.SetBasicAuth("user", "pass")
	req.Header.Set("Cookie", "sid=secret")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("client.Do: %v", err)
	}
	resp.Body.Close()

	if targetAuth != "" || targetCookie != "" {
		t.Fatalf("expected Authorization and Cookie stripped on cross-host redirect, got auth=%q cookie=%q", targetAuth, targetCookie)
	}

	// Verify HTTPS -> HTTP downgrade rejection in CheckRedirect
	prevReq, _ := http.NewRequest(http.MethodGet, "https://origin.example.test/dav", nil)
	nextReq, _ := http.NewRequest(http.MethodGet, "http://origin.example.test/dav", nil)
	if err := client.CheckRedirect(nextReq, []*http.Request{prevReq}); err == nil || !strings.Contains(err.Error(), "downgrade") {
		t.Fatalf("expected https->http downgrade error, got: %v", err)
	}
}
