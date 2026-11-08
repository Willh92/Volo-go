package notify

import (
	"bufio"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Willh92/Volo-go/internal/config"
	"github.com/Willh92/Volo-go/internal/db/migrate"
	"github.com/Willh92/Volo-go/internal/db/sqlx"
	"github.com/Willh92/Volo-go/internal/domain"
	"github.com/Willh92/Volo-go/internal/i18n"
	"github.com/Willh92/Volo-go/internal/repo"
	"github.com/Willh92/Volo-go/pkg/plugin"
)

type stubNotifierProvider struct {
	mu     sync.Mutex
	events []plugin.NotificationEvent
}

func (s *stubNotifierProvider) Name() string { return "test-np" }

func (s *stubNotifierProvider) Send(_ context.Context, ev plugin.NotificationEvent) error {
	s.mu.Lock()
	s.events = append(s.events, ev)
	s.mu.Unlock()
	return nil
}

func TestDispatcher_WebhookDeliveriesAuditNoDataRevBumpMetaAndBoundedQueue(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	store, err := sqlx.Open(ctx, sqlx.Options{
		Driver: "sqlite",
		DSN:    filepath.Join(tmp, "notify-r2.db"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := migrate.Up(ctx, store); err != nil {
		t.Fatal(err)
	}

	kr, _, err := config.LoadOrInitKeyRing(strings.Repeat("n", 32), tmp)
	if err != nil {
		t.Fatal(err)
	}
	settings := repo.NewSettingsManager(store, kr, "en-US")
	_, _ = settings.Reload(ctx)
	cat, _ := i18n.NewCatalog("en-US", "")

	plugMgr := plugin.NewManager(plugin.ManagerOptions{
		Store:      store,
		Keys:       kr,
		PluginsDir: filepath.Join(tmp, "plugins"),
		DataDir:    tmp,
		Catalog:    cat,
	})
	np := &stubNotifierProvider{}
	plugMgr.Providers().RegisterNotifier("test-np", np)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	// Allow loopback test server via emptyWebhookSender override on Dispatcher
	disp := NewDispatcher(DispatcherOptions{
		Store:     store,
		Keys:      kr,
		Settings:  settings,
		Catalog:   cat,
		Plugins:   plugMgr,
		PublicURL: "https://blog.example.com",
	})
	disp.webhooks.httpClient = srv.Client()
	defer disp.Close(ctx)

	now := time.Now().UnixMilli()
	secCipher, err := kr.SealString("feishu_secret_xyz", config.WebhookSecretAAD("wh_1"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.ExecContext(ctx,
		`INSERT INTO webhooks (id, name, provider, url, secret_ciphertext, chat_id, events_json, is_enabled, last_status_code, last_error, last_triggered_at, created_at, updated_at)
		 VALUES ('wh_1', 'Feishu Hook', 'feishu', ?, ?, '', '["*"]', ?, 0, '', 0, ?, ?)`,
		srv.URL, secCipher, true, now, now)
	if err != nil {
		t.Fatal(err)
	}

	revBefore := store.DataRevision()

	// 1. TriggerTestWebhook records row in webhook_deliveries with redacted Feishu sign and does NOT bump dataRev!
	wh, err := sqlx.Get[domain.Webhook](ctx, store, `SELECT * FROM webhooks WHERE id = 'wh_1'`)
	if err != nil {
		t.Fatal(err)
	}
	code, err := disp.TriggerTestWebhook(ctx, wh, "en-US")
	if err != nil || code != http.StatusOK {
		t.Fatalf("TriggerTestWebhook failed: code=%d err=%v", code, err)
	}
	if gotRev := store.DataRevision(); gotRev != revBefore {
		t.Fatalf("webhook delivery telemetry bumped global dataRev (%d -> %d)!", revBefore, gotRev)
	}

	deliveries, err := sqlx.Select[domain.WebhookDelivery](ctx, store, `SELECT * FROM webhook_deliveries WHERE webhook_id = 'wh_1'`)
	if err != nil || len(deliveries) != 1 {
		t.Fatalf("expected 1 webhook_deliveries row, got len=%d err=%v", len(deliveries), err)
	}
	if !deliveries[0].Success || deliveries[0].StatusCode != 200 || !strings.Contains(deliveries[0].PayloadJSON, "[REDACTED]") {
		t.Fatalf("expected successful delivery with redacted Feishu sign, got %+v", deliveries[0])
	}

	// 2. BroadcastWebhookEvent populates NotificationEvent.Meta for plugin NotifierProviders!
	disp.BroadcastWebhookEvent(ctx, WebhookEvent{
		Event:     "article.published",
		SiteTitle: "Volo Blog",
		Title:     "Hello",
		Body:      "Summary",
		TargetURL: "https://blog.example.com/posts/hello",
		Extra: map[string]any{
			"article_id": "a_99",
			"slug":       "hello",
		},
	})
	if len(np.events) != 1 || np.events[0].Meta["slug"] != "hello" || np.events[0].Meta["site_title"] != "Volo Blog" {
		t.Fatalf("expected NotificationEvent.Meta populated, got %+v", np.events)
	}

	// 3. TriggerTestNotifiers dispatches test notification to registered providers.
	sent, err := disp.TriggerTestNotifiers(ctx, "en-US")
	if err != nil || sent != 1 {
		t.Fatalf("expected TriggerTestNotifiers sent=1 err=nil, got sent=%d err=%v", sent, err)
	}
}

func TestMailerRound3_MultiIPFallback_MixedIPFailClosed_IPv4First_And_TLSCtxCancel(t *testing.T) {
	// 1. Direct unit check on orderAddrsIPv4First: unmaps IPv4-in-IPv6, deduplicates, and orders IPv4 ahead of IPv6.
	ordered := orderAddrsIPv4First([]netip.Addr{
		netip.MustParseAddr("2001:db8::1"),
		netip.MustParseAddr("203.0.113.10"),
		netip.MustParseAddr("::ffff:203.0.113.10"),
		netip.MustParseAddr("203.0.113.11"),
	})
	if len(ordered) != 3 ||
		ordered[0].String() != "203.0.113.10" ||
		ordered[1].String() != "203.0.113.11" ||
		ordered[2].String() != "2001:db8::1" {
		t.Fatalf("unexpected orderAddrsIPv4First result: %v", ordered)
	}

	// 2. Mixed public + private DNS answer MUST fail closed before dialing in both tls and starttls modes!
	// Put globally routable 8.8.8.8 FIRST and RFC1918 192.168.1.5 SECOND so index-0-only checks would have missed it.
	for _, secMode := range []string{"tls", "starttls"} {
		m := NewMailer()
		m.resolver = func(_ context.Context, _ string) ([]netip.Addr, error) {
			return []netip.Addr{
				netip.MustParseAddr("8.8.8.8"),
				netip.MustParseAddr("192.168.1.5"),
			}, nil
		}
		dialed := false
		m.dialFn = func(_ context.Context, _, _ string) (net.Conn, error) {
			dialed = true
			return nil, errors.New("unexpected dial")
		}
		err := m.Send(context.Background(), domain.NotificationsSettings{
			SMTPEnabled:   true,
			SMTPHost:      "mixed.smtp.example",
			SMTPPort:      465,
			SMTPSecurity:  secMode,
			SMTPFromEmail: "noreply@example.com",
		}, "", MailMessage{
			To:       "reader@example.com",
			Subject:  "Mixed DNS test",
			TextBody: "Hello",
		})
		if err == nil || !strings.Contains(err.Error(), "192.168.1.5") || !strings.Contains(err.Error(), "refuses non-public IP") {
			t.Fatalf("mode %s: expected fail-closed non-public IP rejection for 192.168.1.5, got %v", secMode, err)
		}
		if dialed {
			t.Fatalf("mode %s: dialFn must not be invoked when DNS contains mixed non-public IP", secMode)
		}
	}

	// 3. Mixed loopback + public DNS answer MUST fail closed before dialing in plain_internal mode!
	{
		m := NewMailer()
		m.resolver = func(_ context.Context, _ string) ([]netip.Addr, error) {
			return []netip.Addr{
				netip.MustParseAddr("127.0.0.1"),
				netip.MustParseAddr("1.1.1.1"),
			}, nil
		}
		dialed := false
		m.dialFn = func(_ context.Context, _, _ string) (net.Conn, error) {
			dialed = true
			return nil, errors.New("unexpected dial")
		}
		err := m.Send(context.Background(), domain.NotificationsSettings{
			SMTPEnabled:   true,
			SMTPHost:      "internal-mixed.local",
			SMTPPort:      25,
			SMTPSecurity:  "plain_internal",
			SMTPFromEmail: "noreply@example.com",
		}, "", MailMessage{
			To:       "reader@example.com",
			Subject:  "Mixed internal DNS test",
			TextBody: "Hello",
		})
		if err == nil || !strings.Contains(err.Error(), "1.1.1.1") || !strings.Contains(err.Error(), "refuses public IP") {
			t.Fatalf("plain_internal: expected fail-closed public IP rejection for 1.1.1.1, got %v", err)
		}
		if dialed {
			t.Fatalf("plain_internal: dialFn must not be invoked when DNS contains mixed public IP")
		}
	}

	// 4. Multi-IP fallback + IPv4-first preference:
	// DNS returns [::1, 127.0.0.2, 127.0.0.1]; 127.0.0.2 is tried ahead of ::1, fails, and falls back to 127.0.0.1!
	{
		m := NewMailer()
		m.resolver = func(_ context.Context, _ string) ([]netip.Addr, error) {
			return []netip.Addr{
				netip.MustParseAddr("::1"),
				netip.MustParseAddr("127.0.0.2"),
				netip.MustParseAddr("127.0.0.1"),
			}, nil
		}
		var (
			mu       sync.Mutex
			attempts []string
			gotMIME  strings.Builder
		)
		m.dialFn = func(_ context.Context, _, addr string) (net.Conn, error) {
			mu.Lock()
			attempts = append(attempts, addr)
			mu.Unlock()
			if addr == "127.0.0.2:2525" {
				return nil, errors.New("connection refused on primary IPv4")
			}
			if addr != "127.0.0.1:2525" {
				return nil, errors.New("unexpected address " + addr)
			}
			clientConn, serverConn := net.Pipe()
			go func() {
				defer serverConn.Close()
				_, _ = serverConn.Write([]byte("220 relay.internal ESMTP ready\r\n"))
				br := bufio.NewReader(serverConn)
				inData := false
				for {
					line, rErr := br.ReadString('\n')
					if rErr != nil {
						return
					}
					if inData {
						if strings.TrimRight(line, "\r\n") == "." {
							inData = false
							_, _ = serverConn.Write([]byte("250 2.0.0 Queued\r\n"))
							continue
						}
						mu.Lock()
						gotMIME.WriteString(line)
						mu.Unlock()
						continue
					}
					upper := strings.ToUpper(strings.TrimSpace(line))
					switch {
					case strings.HasPrefix(upper, "EHLO"), strings.HasPrefix(upper, "HELO"):
						_, _ = serverConn.Write([]byte("250-relay.internal\r\n250 8BITMIME\r\n"))
					case strings.HasPrefix(upper, "MAIL FROM:"):
						_, _ = serverConn.Write([]byte("250 2.1.0 OK\r\n"))
					case strings.HasPrefix(upper, "RCPT TO:"):
						_, _ = serverConn.Write([]byte("250 2.1.5 OK\r\n"))
					case upper == "DATA":
						inData = true
						_, _ = serverConn.Write([]byte("354 End data with <CR><LF>.<CR><LF>\r\n"))
					case upper == "QUIT":
						_, _ = serverConn.Write([]byte("221 2.0.0 Bye\r\n"))
						return
					default:
						_, _ = serverConn.Write([]byte("250 OK\r\n"))
					}
				}
			}()
			return clientConn, nil
		}

		err := m.Send(context.Background(), domain.NotificationsSettings{
			SMTPEnabled:   true,
			SMTPHost:      "relay.internal",
			SMTPPort:      2525,
			SMTPSecurity:  "plain_internal",
			SMTPFromEmail: "noreply@example.com",
			SMTPFromName:  "Volo Notify",
		}, "", MailMessage{
			To:       "admin@example.com",
			Subject:  "Fallback Delivery",
			TextBody: "Delivered on secondary IPv4!",
		})
		if err != nil {
			t.Fatalf("expected multi-IP fallback SMTP send to succeed, got %v", err)
		}
		mu.Lock()
		gotAttempts := append([]string(nil), attempts...)
		mimeStr := gotMIME.String()
		mu.Unlock()
		if len(gotAttempts) != 2 || gotAttempts[0] != "127.0.0.2:2525" || gotAttempts[1] != "127.0.0.1:2525" {
			t.Fatalf("expected IPv4-first fallback attempts [127.0.0.2:2525 127.0.0.1:2525], got %v", gotAttempts)
		}
		if !strings.Contains(mimeStr, "Delivered on secondary IPv4!") {
			t.Fatalf("expected delivered MIME body on secondary IPv4, got %q", mimeStr)
		}
	}

	// 5. Context cancellation in implicit "tls" mode aborts immediately without hanging on dialTimeout!
	{
		m := NewMailer()
		m.dialTimeout = 10 * time.Second
		m.resolver = func(_ context.Context, _ string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
		}

		// 5a. Pre-canceled context before dial
		preCtx, preCancel := context.WithCancel(context.Background())
		preCancel()
		start := time.Now()
		err := m.Send(preCtx, domain.NotificationsSettings{
			SMTPEnabled:   true,
			SMTPHost:      "smtp.example.com",
			SMTPPort:      465,
			SMTPSecurity:  "tls",
			SMTPFromEmail: "noreply@example.com",
		}, "", MailMessage{
			To:       "reader@example.com",
			Subject:  "Pre-canceled",
			TextBody: "Should abort immediately",
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled on pre-canceled tls Send, got %v", err)
		}
		if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
			t.Fatalf("pre-canceled tls Send took too long: %v", elapsed)
		}

		// 5b. Context timeout during stalled TLS handshake
		var peerConn net.Conn
		defer func() {
			if peerConn != nil {
				_ = peerConn.Close()
			}
		}()
		m.dialFn = func(_ context.Context, _, _ string) (net.Conn, error) {
			cConn, sConn := net.Pipe()
			// Drain client hello bytes asynchronously while keeping server side silent so net.Pipe Write doesn't block without TLS reply.
			go func() {
				buf := make([]byte, 4096)
				for {
					if _, rErr := sConn.Read(buf); rErr != nil {
						return
					}
				}
			}()
			peerConn = sConn
			return cConn, nil
		}
		hsCtx, hsCancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
		defer hsCancel()
		start = time.Now()
		err = m.Send(hsCtx, domain.NotificationsSettings{
			SMTPEnabled:   true,
			SMTPHost:      "smtp.example.com",
			SMTPPort:      465,
			SMTPSecurity:  "tls",
			SMTPFromEmail: "noreply@example.com",
		}, "", MailMessage{
			To:       "reader@example.com",
			Subject:  "Stalled TLS Handshake",
			TextBody: "Should honor context deadline",
		})
		if !errors.Is(err, context.DeadlineExceeded) && (err == nil || !strings.Contains(err.Error(), "i/o timeout") && !strings.Contains(err.Error(), "context deadline exceeded")) {
			t.Fatalf("expected context deadline / timeout on stalled TLS handshake, got %v", err)
		}
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Fatalf("stalled TLS handshake ignored context deadline (%v)", elapsed)
		}
	}
}
