package server

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

func TestOutboundClients_NilDialerBlocksSSRF(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// AppServer with OutboundDialer == nil must STILL block loopback/metadata SSRF
	s := &AppServer{}

	for _, tc := range []struct {
		name   string
		client *http.Client
		target string
	}{
		{"linkPatrol-loopback", s.newLinkPatrolHTTPClient(), srv.URL},
		{"linkPatrol-metadata", s.newLinkPatrolHTTPClient(), "http://169.254.169.254/latest/meta-data/"},
		{"followSync-loopback", s.newFollowSyncHTTPClient(), srv.URL},
		{"followSync-metadata", s.newFollowSyncHTTPClient(), "http://169.254.169.254/latest/meta-data/"},
	} {
		req, _ := http.NewRequest(http.MethodGet, tc.target, nil)
		resp, err := tc.client.Do(req)
		if resp != nil {
			resp.Body.Close()
		}
		if err == nil || !errors.Is(err, security.ErrSSRFBlocked) {
			t.Fatalf("%s: expected ErrSSRFBlocked with nil OutboundDialer, got: %v", tc.name, err)
		}
	}
}

func TestLinkPatrolHTTPClient_BlocksDowngradeAndStripsCrossHostHeaders(t *testing.T) {
	var seenAuth, seenCookie string
	dstSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		seenCookie = r.Header.Get("Cookie")
		w.WriteHeader(http.StatusOK)
	}))
	defer dstSrv.Close()

	_, dstPort, _ := strings.Cut(strings.TrimPrefix(dstSrv.URL, "http://"), ":")
	srcSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://target.example.test:"+dstPort+"/ok", http.StatusFound)
	}))
	defer srcSrv.Close()
	_, srcPort, _ := strings.Cut(strings.TrimPrefix(srcSrv.URL, "http://"), ":")

	s := &AppServer{
		deps: AppDeps{
			OutboundDialer: &security.OutboundSafeDialer{
				Timeout:               5 * time.Second,
				AllowLoopbackForTests: true,
				Resolver: func(_ context.Context, host string) ([]netip.Addr, error) {
					return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
				},
			},
		},
	}

	cli := s.newLinkPatrolHTTPClient()
	req, _ := http.NewRequest(http.MethodGet, "http://source.example.test:"+srcPort+"/start", nil)
	req.Header.Set("Authorization", "Bearer top-secret")
	req.Header.Set("Cookie", "session=xyz")
	resp, err := cli.Do(req)
	if err != nil {
		t.Fatalf("cli.Do: %v", err)
	}
	resp.Body.Close()
	if seenAuth != "" || seenCookie != "" {
		t.Fatalf("expected cross-host headers stripped, got auth=%q cookie=%q", seenAuth, seenCookie)
	}

	prevReq, _ := http.NewRequest(http.MethodGet, "https://source.example.test/a", nil)
	nextReq, _ := http.NewRequest(http.MethodGet, "http://source.example.test/b", nil)
	if err := cli.CheckRedirect(nextReq, []*http.Request{prevReq}); err == nil || !strings.Contains(err.Error(), "downgrade") {
		t.Fatalf("expected https->http downgrade rejection, got: %v", err)
	}
}
