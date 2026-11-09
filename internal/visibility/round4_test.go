package visibility

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Willh92/Volo-go/internal/config"
	"github.com/Willh92/Volo-go/internal/db/migrate"
	"github.com/Willh92/Volo-go/internal/db/sqlx"
	"github.com/Willh92/Volo-go/internal/domain"
	"github.com/Willh92/Volo-go/internal/guest"
	"github.com/Willh92/Volo-go/internal/i18n"
	"github.com/Willh92/Volo-go/internal/render"
	"github.com/Willh92/Volo-go/internal/repo"
)

func TestToFeedItemSummaryFastPathPreservesInlineCodeAndLocalizedNotices(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	store, err := sqlx.Open(ctx, sqlx.Options{
		Driver: "sqlite",
		DSN:    filepath.Join(tmp, "vis-r4.db"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := migrate.Up(ctx, store); err != nil {
		t.Fatal(err)
	}
	kr, _, err := config.LoadOrInitKeyRing("unit-test-visibility-r4-secret-2026", tmp)
	if err != nil {
		t.Fatal(err)
	}
	cat, err := i18n.NewCatalog("en-US", "")
	if err != nil {
		t.Fatal(err)
	}
	sm := repo.NewSettingsManager(store, kr, "en-US")
	passports := guest.NewPassportManager(kr, "https://example.com")
	g := NewGuard(kr, sm, passports, cat, render.Default())

	// Public article with SummaryMD and !fullOutput -> fast path uses SummaryMD, preserves `<script>` inside backticks!
	raw := domain.RawArticle{
		ID:        "art_fast_feed",
		Title:     "Safe Escaping Guide",
		Slug:      "safe-escaping",
		SummaryMD: "Always escape `<script>` and `<div>` in **untrusted** templates.",
		ContentMD: strings.Repeat("Heavy body paragraph that need not be rendered in summary-only RSS. ", 200),
		Status:    domain.ArticleStatusPublished,
	}
	item, ok := g.ToFeedItem(raw, false, "en-US", "https://example.com", "Captain", "Security")
	if !ok {
		t.Fatal("expected public article to be included in feed")
	}
	if !strings.Contains(item.Summary, "<script>") || !strings.Contains(item.Summary, "<div>") {
		t.Fatalf("expected inline code span literals preserved in plain-text excerpt, got %q", item.Summary)
	}
	if strings.Contains(item.Summary, "**") || strings.Contains(item.HTML, "Heavy body paragraph") {
		t.Fatalf("expected SummaryMD fast-path without rendering ContentMD or leaking **, summary=%q html=%q", item.Summary, item.HTML)
	}

	// Localized protected notices for ja-JP and zh-TW
	jaNotice := g.ProtectedNotice("ja-JP")
	if !strings.Contains(jaNotice, "保護") {
		t.Fatalf("expected Japanese protected notice, got %q", jaNotice)
	}
	twNotice := g.ProtectedNotice("zh-TW")
	if !strings.Contains(twNotice, "保護") {
		t.Fatalf("expected Traditional Chinese protected notice, got %q", twNotice)
	}
}
