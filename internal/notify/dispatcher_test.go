package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Willh92/Volo-go/internal/config"
	"github.com/Willh92/Volo-go/internal/content"
	"github.com/Willh92/Volo-go/internal/db/migrate"
	"github.com/Willh92/Volo-go/internal/db/sqlx"
	"github.com/Willh92/Volo-go/internal/domain"
	"github.com/Willh92/Volo-go/internal/i18n"
	"github.com/Willh92/Volo-go/internal/repo"
)

func TestDispatcher_ProtectedTitleScrubbingAndPendingCommentWebhookGuard(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()

	store, err := sqlx.Open(ctx, sqlx.Options{
		Driver: "sqlite",
		DSN:    filepath.Join(tmp, "notify.db"),
	})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := migrate.Up(ctx, store); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	kr, _, err := config.LoadOrInitKeyRing(tmp, "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY")
	if err != nil {
		t.Fatalf("keyring: %v", err)
	}
	settingsMgr := repo.NewSettingsManager(store, kr, "en-US")
	_, _ = settingsMgr.Reload(ctx)
	catalog, _ := i18n.NewCatalog("", "en-US")

	var mu sync.Mutex
	var payloads []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		payloads = append(payloads, string(b))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	now := time.Now().UnixMilli()
	pwCipher, err := kr.SealString("topsecret", config.ArticlePasswordAAD("art_secret"))
	if err != nil {
		t.Fatalf("seal password: %v", err)
	}
	_, err = store.ExecContext(ctx, `INSERT INTO articles
		(id, author_id, category_id, title, slug, legacy_permalink, summary_md, content_md, cover_url, status, published_at, scheduled_at, is_pinned, allow_comment, password_ciphertext, has_content_lock, audio_url, audio_duration_sec, audio_size_bytes, audio_mime, lang, translation_group, license_override, word_count, reading_minutes, view_count, comment_count, created_at, updated_at)
		VALUES ('art_secret', 'u1', '', 'Ultra Confidential Merger Plan', 'secret-merger', '', '', 'body', '', 'published', ?, 0, 0, 1, ?, 0, '', 0, 0, '', 'en-US', '', '', 1, 1, 0, 0, ?, ?)`,
		now, pwCipher, now, now)
	if err != nil {
		t.Fatalf("insert protected article: %v", err)
	}

	eventsBytes, _ := json.Marshal([]string{"*"})
	_, err = store.ExecContext(ctx, `INSERT INTO webhooks
		(id, name, provider, url, secret_ciphertext, chat_id, events_json, is_enabled, last_status_code, last_error, last_triggered_at, created_at, updated_at)
		VALUES ('wh_1', 'Test Hook', 'custom', ?, '', '', ?, 1, 0, '', 0, ?, ?)`,
		srv.URL, string(eventsBytes), now, now)
	if err != nil {
		t.Fatalf("insert webhook: %v", err)
	}

	disp := NewDispatcher(DispatcherOptions{
		Store:     store,
		Keys:      kr,
		Settings:  settingsMgr,
		Catalog:   catalog,
		PublicURL: "https://blog.example.com",
	})
	disp.webhooks.httpClient = srv.Client()
	t.Cleanup(func() { _ = disp.Close(context.Background()) })

	// 1. Direct target title check for protected article must scrub real title!
	gotTitle, _ := disp.resolveTargetTitleAndPath(ctx, domain.TargetTypeArticle, "art_secret")
	if gotTitle != "[Protected Article]" {
		t.Fatalf("expected [Protected Article], got %q", gotTitle)
	}

	// 2. Pending comment MUST NOT broadcast external webhooks!
	disp.dispatchCommentNotifications(ctx, content.PublicCommentView{
		ID:         "cmt_pending",
		AuthorName: "SpamBot",
		Status:     domain.CommentStatusPending,
	}, content.CreateCommentInput{
		TargetType: domain.TargetTypeArticle,
		TargetID:   "art_secret",
		ContentMD:  "Buy cheap spam!",
	})
	mu.Lock()
	pendingDeliveries := len(payloads)
	mu.Unlock()
	if pendingDeliveries != 0 {
		t.Fatalf("expected 0 webhook deliveries for pending comment, got %d", pendingDeliveries)
	}

	// 3. Approved comment on protected article MUST broadcast webhook WITH scrubbed title!
	disp.dispatchCommentNotifications(ctx, content.PublicCommentView{
		ID:         "cmt_approved",
		AuthorName: "Reader",
		Status:     domain.CommentStatusApproved,
	}, content.CreateCommentInput{
		TargetType: domain.TargetTypeArticle,
		TargetID:   "art_secret",
		ContentMD:  "Great insight!",
	})
	mu.Lock()
	approvedDeliveries := len(payloads)
	var body string
	if approvedDeliveries > 0 {
		body = payloads[0]
	}
	mu.Unlock()
	if approvedDeliveries != 1 {
		t.Fatalf("expected 1 webhook delivery for approved comment, got %d", approvedDeliveries)
	}
	if strings.Contains(body, "Ultra Confidential Merger Plan") {
		t.Fatalf("protected article title leaked in webhook payload: %s", body)
	}
}
