package content

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Willh92/Volo-go/internal/config"
	"github.com/Willh92/Volo-go/internal/db/migrate"
	"github.com/Willh92/Volo-go/internal/db/sqlx"
	"github.com/Willh92/Volo-go/internal/domain"
	"github.com/Willh92/Volo-go/internal/guest"
	"github.com/Willh92/Volo-go/internal/i18n"
	"github.com/Willh92/Volo-go/internal/render"
	"github.com/Willh92/Volo-go/internal/repo"
	"github.com/Willh92/Volo-go/internal/visibility"
	"github.com/Willh92/Volo-go/pkg/plugin"
)

type testSpamProvider struct {
	verdict plugin.SpamVerdict
}

func (p *testSpamProvider) Name() string { return "test-antispam" }
func (p *testSpamProvider) CheckComment(_ context.Context, _ plugin.CommentCheckInput) (plugin.SpamVerdict, error) {
	return p.verdict, nil
}

type testHookPlugin struct {
	plugin.BaseBuiltinPlugin
	id     string
	onHook func(hookName string, payload any) (any, error)
}

func (p *testHookPlugin) Manifest() plugin.Manifest {
	return plugin.Manifest{
		ID:          p.id,
		Version:     "1.0.0",
		Runtime:     plugin.RuntimeBuiltin,
		Name:        map[string]string{"en": "Test Hook Plugin"},
		Permissions: []string{"hooks:*", "provider:antispam"},
		Hooks: []string{
			plugin.HookCommentBeforeCreate,
			plugin.HookRenderPreMarkdown,
			plugin.HookRenderPostHTML,
		},
	}
}

func (p *testHookPlugin) OnHook(_ context.Context, hookName string, payload any) (any, error) {
	if p.onHook != nil {
		return p.onHook(hookName, payload)
	}
	return payload, nil
}

func setupContentTestEnv(t *testing.T) (context.Context, *sqlx.Store, *repo.SettingsManager, *plugin.Manager, *Service) {
	t.Helper()
	ctx := context.Background()
	tmp := t.TempDir()

	secret := "0123456789abcdef0123456789abcdef-content-test-key"
	pubURL := "https://blog.example.com"
	dataDir := filepath.Join(tmp, "data")
	pluginsDir := filepath.Join(tmp, "plugins")
	cfg, err := config.Load(config.CLIOverrides{
		SecretKey:  &secret,
		PublicURL:  &pubURL,
		DataDir:    &dataDir,
		PluginsDir: &pluginsDir,
	})
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	store, err := sqlx.Open(ctx, sqlx.Options{
		Driver: "sqlite",
		DSN:    filepath.Join(tmp, "content.db"),
	})
	if err != nil {
		t.Fatalf("sqlx.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := migrate.Up(ctx, store); err != nil {
		t.Fatalf("migrate.Up: %v", err)
	}

	cat, err := i18n.NewCatalog("en", "")
	if err != nil {
		t.Fatalf("i18n.NewCatalog: %v", err)
	}
	settings := repo.NewSettingsManager(store, cfg.KeyRing, "en")
	if _, err := settings.Reload(ctx); err != nil {
		t.Fatalf("settings.Reload: %v", err)
	}

	pluginMgr := plugin.NewManager(plugin.ManagerOptions{
		Store:      store,
		Keys:       cfg.KeyRing,
		PluginsDir: pluginsDir,
		DataDir:    dataDir,
		Catalog:    cat,
	})
	if err := pluginMgr.DiscoverAndStart(ctx); err != nil {
		t.Fatalf("DiscoverAndStart: %v", err)
	}
	t.Cleanup(func() { _ = pluginMgr.Close(ctx) })

	passports := guest.NewPassportManager(cfg.KeyRing, cfg.PublicURL)
	rnd := render.Default()
	grd := visibility.NewGuard(cfg.KeyRing, settings, passports, cat, rnd)
	srch := visibility.NewSearchIndex(store, rnd, 1<<20)

	svc := NewService(ServiceOptions{
		Store:     store,
		Keys:      cfg.KeyRing,
		Settings:  settings,
		Passports: passports,
		PoW:       nil, // nil PoW simplifies direct service-level comment tests
		Guard:     grd,
		Search:    srch,
		Renderer:  rnd,
		Plugins:   pluginMgr,
		Catalog:   cat,
	})
	return ctx, store, settings, pluginMgr, svc
}

func TestBookshelfSync_CrossOriginAndDangerousSchemeHardening(t *testing.T) {
	baseHTTPS, _ := url.Parse("https://docs.example.com/guide/SUMMARY.md")

	if !isSafeSameHostChapterURL("https://docs.example.com/guide/ch1.md", baseHTTPS) {
		t.Fatalf("expected same-host HTTPS chapter URL to be allowed")
	}
	for _, bad := range []string{
		"https://evil.example.org/ch1.md",
		"http://docs.example.com/guide/ch1.md", // HTTPS -> HTTP downgrade
		"http://169.254.169.254/latest/meta-data/",
		"javascript:alert(1)",
		"data:text/html,<script>alert(1)</script>",
		`/\evil.example.com/ch1.md`,
		"https://user:pass@docs.example.com/guide/ch1.md",
	} {
		if isSafeSameHostChapterURL(bad, baseHTTPS) {
			t.Fatalf("expected %q to be rejected by isSafeSameHostChapterURL", bad)
		}
	}

	// Verify parseSummaryMarkdownTOC strips cross-origin / javascript / downgrade entries while keeping valid chapters
	md := strings.Join([]string{
		"- [Intro](intro.md)",
		"- [Cross Origin](https://attacker.invalid/pwn.md)",
		"- [Downgrade](http://docs.example.com/guide/plain.md)",
		"- [XSS](javascript:alert(1))",
	}, "\n")
	chapters := parseSummaryMarkdownTOC("bk_1", md, baseHTTPS)
	if len(chapters) != 1 || chapters[0].URL != "https://docs.example.com/guide/intro.html" {
		t.Fatalf("expected only same-host HTTPS chapter to survive parseSummaryMarkdownTOC, got %+v", chapters)
	}
}

func TestBuildGitHubStatsSVGWithTheme_ModesAndJSONFeed(t *testing.T) {
	ctx, _, _, _, svc := setupContentTestEnv(t)

	autoSVG := string(svc.BuildGitHubStatsSVGWithTheme("octocat", "auto"))
	if !strings.Contains(autoSVG, "@media (prefers-color-scheme: dark)") {
		t.Fatalf("expected prefers-color-scheme media query in auto SVG:\n%s", autoSVG)
	}
	darkSVG := string(svc.BuildGitHubStatsSVGWithTheme("octocat", "dark"))
	if strings.Contains(darkSVG, "@media") || !strings.Contains(darkSVG, "#141b18") {
		t.Fatalf("expected dark-only palette in dark SVG:\n%s", darkSVG)
	}
	lightSVG := string(svc.BuildGitHubStatsSVGWithTheme("octocat", "light"))
	if strings.Contains(lightSVG, "#141b18") || !strings.Contains(lightSVG, "#f6f7f3") {
		t.Fatalf("expected light-only palette in light SVG:\n%s", lightSVG)
	}

	doc, err := svc.GetCachedJSONFeed(ctx, "https://blog.example.com", "en")
	if err != nil {
		t.Fatalf("GetCachedJSONFeed: %v", err)
	}
	if !strings.Contains(string(doc.Raw), "https://jsonfeed.org/version/1.1") {
		t.Fatalf("unexpected JSON feed body: %s", string(doc.Raw))
	}
}

func TestComments_CreateSpamAllow_HookRejectReason_And_UpdateOwnCommentAntiSpam(t *testing.T) {
	ctx, store, settings, pluginMgr, svc := setupContentTestEnv(t)

	now := time.Now().UnixMilli()
	// Seed author, reader, and published article
	_, err := store.ExecContext(ctx, `INSERT INTO users (
			id, username, email, display_name, avatar_url, bio, website_url,
			role, password_hash, legacy_hash, status, preferred_locale, created_at, updated_at
		) VALUES
			('u_admin', 'admin', 'admin@example.com', 'Admin', '', '', '', 'admin', 'x', '', 'active', 'en', ?, ?),
			('u_reader', 'reader', 'reader@example.com', 'Reader', '', '', '', 'reader', 'x', '', 'active', 'en', ?, ?)`,
		now, now, now, now)
	if err != nil {
		t.Fatalf("seed users: %v", err)
	}
	_, err = store.ExecContext(ctx, `INSERT INTO articles (
			id, author_id, category_id, title, slug, legacy_permalink, summary_md, content_md,
			cover_url, status, published_at, allow_comment, password_ciphertext,
			audio_url, audio_mime, lang, translation_group, license_override, created_at, updated_at
		) VALUES ('art_1', 'u_admin', '', 'Hello World', 'hello-world', '', 'sum', 'body', '', 'published', ?, ?, '', '', '', 'en', '', '', ?, ?)`,
		now, true, now, now)
	if err != nil {
		t.Fatalf("seed article: %v", err)
	}

	// Configure blocked keyword "casino" under smart moderation
	if _, _, err := settings.PatchDomain(ctx, "reading", []byte(`{"comment_moderation_mode":"smart","blocked_keywords":["casino"]}`), 0); err != nil {
		t.Fatalf("PatchDomain(reading): %v", err)
	}

	spamProv := &testSpamProvider{verdict: plugin.SpamVerdict{Action: plugin.SpamActionAllow}}
	var rejectReason string
	hp := &testHookPlugin{
		id: "comment-guard",
		onHook: func(hookName string, payload any) (any, error) {
			if hookName == plugin.HookCommentBeforeCreate && rejectReason != "" {
				return nil, &plugin.HookRejectedError{PluginID: "comment-guard", Reason: rejectReason}
			}
			if hookName == plugin.HookRenderPreMarkdown {
				if ap, ok := payload.(plugin.ArticleHookPayload); ok {
					ap.ContentMD = strings.ReplaceAll(ap.ContentMD, "{{BADGE}}", "**HOOKED**")
					return ap, nil
				}
			}
			return payload, nil
		},
	}
	if err := pluginMgr.RegisterBuiltin(ctx, hp); err != nil {
		t.Fatalf("RegisterBuiltin: %v", err)
	}
	if err := pluginMgr.Enable(ctx, hp.id); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	pluginMgr.Providers().RegisterAntiSpam(hp.id, spamProv)

	reader := &domain.User{
		ID:          "u_reader",
		Username:    "reader",
		DisplayName: "Reader",
		Role:        domain.RoleReader,
		Status:      domain.UserStatusActive,
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/comments", nil)
	rec := httptest.NewRecorder()

	// 1. SpamActionAllow overrides keyword-triggered pending -> approved when ModerationMode != "all"
	view, _, err := svc.CreateComment(ctx, rec, req, reader, CreateCommentInput{
		TargetType: domain.TargetTypeArticle,
		TargetID:   "art_1",
		ContentMD:  "Friendly note mentioning casino once",
		Locale:     "en",
	})
	if err != nil {
		t.Fatalf("CreateComment (SpamActionAllow): %v", err)
	}
	if view.Status != domain.CommentStatusApproved {
		t.Fatalf("expected SpamActionAllow to override keyword pending -> approved, got %q", view.Status)
	}

	// Verify article comment_count incremented to 1
	cntRow, _ := sqlx.Get[domain.RawArticle](ctx, store, `SELECT comment_count FROM articles WHERE id = 'art_1'`)
	if cntRow.CommentCount != 1 {
		t.Fatalf("expected comment_count = 1, got %d", cntRow.CommentCount)
	}

	// 2. HookRejectedError preserves custom Reason on CreateComment
	rejectReason = "link velocity exceeded"
	_, _, err = svc.CreateComment(ctx, rec, req, reader, CreateCommentInput{
		TargetType: domain.TargetTypeArticle,
		TargetID:   "art_1",
		ContentMD:  "Another comment",
		Locale:     "en",
	})
	if !errors.Is(err, ErrCommentRejected) || !strings.Contains(err.Error(), "link velocity exceeded") {
		t.Fatalf("expected ErrCommentRejected preserving reason, got %v", err)
	}
	rejectReason = ""

	// 3. UpdateOwnComment blocked when AntiSpamProvider returns SpamActionReject
	spamProv.verdict = plugin.SpamVerdict{Action: plugin.SpamActionReject, Reason: "phishing link in edit"}
	_, err = svc.UpdateOwnComment(ctx, req, reader, view.ID, "Edited into spam link")
	if !errors.Is(err, ErrCommentRejected) || !strings.Contains(err.Error(), "phishing link in edit") {
		t.Fatalf("expected UpdateOwnComment reject with reason, got %v", err)
	}

	// 4. UpdateOwnComment demotes approved -> pending on SpamActionModerate and syncs comment_count -> 0
	spamProv.verdict = plugin.SpamVerdict{Action: plugin.SpamActionModerate}
	edited, err := svc.UpdateOwnComment(ctx, req, reader, view.ID, "Edited suspicious text")
	if err != nil {
		t.Fatalf("UpdateOwnComment (moderate): %v", err)
	}
	if edited.Status != domain.CommentStatusPending {
		t.Fatalf("expected edited comment status demoted to pending, got %q", edited.Status)
	}
	cntRow, _ = sqlx.Get[domain.RawArticle](ctx, store, `SELECT comment_count FROM articles WHERE id = 'art_1'`)
	if cntRow.CommentCount != 0 {
		t.Fatalf("expected comment_count synced to 0 after demotion to pending, got %d", cntRow.CommentCount)
	}

	// 5. Standalone Page template + HookRenderPreMarkdown pipeline check
	_, err = store.ExecContext(ctx, `INSERT INTO pages (id, author_id, title, slug, content_md, template, is_published, allow_comment, lang, translation_group, created_at, updated_at)
		VALUES ('pg_1', 'u_admin', 'Landing', 'landing', 'Welcome {{BADGE}}', 'wide', ?, ?, 'en', '', ?, ?)`,
		true, true, now, now)
	if err != nil {
		t.Fatalf("seed page: %v", err)
	}
	pgView, err := svc.GetPublicPageBySlug(ctx, "landing", "en")
	if err != nil {
		t.Fatalf("GetPublicPageBySlug: %v", err)
	}
	if pgView.Template != "wide" || !strings.Contains(pgView.HTML, "<strong>HOOKED</strong>") {
		t.Fatalf("expected Template='wide' and HookRenderPreMarkdown transformation, got tpl=%q html=%s", pgView.Template, pgView.HTML)
	}
}
