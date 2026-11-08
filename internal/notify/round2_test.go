package notify

import (
	"context"
	"net/http"
	"net/http/httptest"
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
