package content

import (
	"context"
	"errors"
	"fmt"
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

func TestContentRound3_SQLCommentPagination_TwoGenCache_PendingViews_And_BoundedTags(t *testing.T) {
	ctx, store, _, _, svc := setupContentTestEnv(t)
	now := time.Now().UnixMilli()

	// Seed admin, reader, and 25 published articles (oldest art_old_0 is outside top-20 recentCards!).
	if _, err := store.ExecContext(ctx, `INSERT INTO users (
		id, username, email, display_name, avatar_url, bio, website_url,
		role, password_hash, legacy_hash, status, preferred_locale, created_at, updated_at
	) VALUES
		('u_adm', 'adm', 'adm@example.com', 'Admin', '', '', '', 'admin', 'x', '', 'active', 'en', ?, ?),
		('u_rdr', 'rdr', 'rdr@example.com', 'Reader', '', '', '', 'reader', 'x', '', 'active', 'en', ?, ?)`,
		now, now, now, now); err != nil {
		t.Fatalf("seed users: %v", err)
	}

	for i := 0; i < 25; i++ {
		aid := fmt.Sprintf("art_%02d", i)
		slug := fmt.Sprintf("post-%02d", i)
		pubAt := now + int64(i)*1000
		if _, err := store.ExecContext(ctx, `INSERT INTO articles (
			id, author_id, category_id, title, slug, legacy_permalink, summary_md, content_md,
			cover_url, status, published_at, allow_comment, password_ciphertext,
			audio_url, audio_mime, lang, translation_group, license_override, created_at, updated_at
		) VALUES (?, 'u_adm', '', ?, ?, '', 'sum', 'body', '', 'published', ?, ?, '', '', '', 'en', '', '', ?, ?)`,
			aid, slug, slug, pubAt, true, pubAt, pubAt); err != nil {
			t.Fatalf("seed article %s: %v", aid, err)
		}
	}

	// Seed 210 tags (> maxCachedChromeTags=200), attaching tag_hot to 3 articles.
	for i := 0; i < 210; i++ {
		tid := fmt.Sprintf("tag_%03d", i)
		name := fmt.Sprintf("Tag-%03d", i)
		slug := fmt.Sprintf("tag-%03d", i)
		if _, err := store.ExecContext(ctx, `INSERT INTO tags (id, name, slug, created_at) VALUES (?, ?, ?, ?)`, tid, name, slug, now); err != nil {
			t.Fatalf("seed tag: %v", err)
		}
	}
	for i := 0; i < 3; i++ {
		if _, err := store.ExecContext(ctx, `INSERT INTO article_tags (article_id, tag_id) VALUES (?, 'tag_205')`, fmt.Sprintf("art_%02d", i)); err != nil {
			t.Fatalf("seed article_tags: %v", err)
		}
	}

	// Verify >200 tags: Stats.TagCount is exact (210), popular tags hit bounded cache, and full ListTagsWithCounts(0) returns all 210.
	extras := svc.GetPublicChromeExtras(ctx, "en", 6)
	if extras.Stats.TagCount != 210 {
		t.Fatalf("expected exact Stats.TagCount=210, got %d", extras.Stats.TagCount)
	}
	top5, err := svc.ListTagsWithCounts(ctx, 5)
	if err != nil || len(top5) != 5 || top5[0].ID != "tag_205" || top5[0].ArticleCount != 3 {
		t.Fatalf("unexpected top5 tags: %+v (err=%v)", top5, err)
	}
	allTags, err := svc.ListTagsWithCounts(ctx, 0)
	if err != nil || len(allTags) != 210 {
		t.Fatalf("expected all 210 tags when limit=0, got %d (err=%v)", len(allTags), err)
	}

	// Record views on oldest article art_00 (NOT in recentCards top-20!) and verify TotalPendingArticleViews() updates Stats.ViewCount!
	svc.RecordView(ctx, domain.TargetTypeArticle, "art_00", "visitor-1", "news.ycombinator.com")
	svc.RecordView(ctx, domain.TargetTypeArticle, "art_00", "visitor-1", "news.ycombinator.com")
	extrasAfter := svc.GetPublicChromeExtras(ctx, "en", 6)
	if extrasAfter.Stats.ViewCount != extras.Stats.ViewCount+2 {
		t.Fatalf("expected Stats.ViewCount to include pending views on older article (+2), before=%d after=%d", extras.Stats.ViewCount, extrasAfter.Stats.ViewCount)
	}
	if err := svc.FlushAnalytics(ctx); err != nil {
		t.Fatalf("FlushAnalytics: %v", err)
	}

	// Seed 5 approved root comments + 2 replies on oldest root_0 + 1 pending comment owned by u_rdr on art_00.
	for i := 0; i < 5; i++ {
		cid := fmt.Sprintf("root_%d", i)
		ts := now + int64(i)*100
		md := fmt.Sprintf("Root %d <div id=\"volo-lightbox\">spoof</div> <input type=\"password\"> text[^1]\n\n[^1]: fn", i)
		if _, err := store.ExecContext(ctx, `INSERT INTO comments (
			id, target_type, target_id, parent_id, reply_to_name, user_id, guest_id_hash, env_fp_hash,
			author_name, author_url, avatar_url, notify_email_ciphertext, notify_locale,
			client_ip, ip_masked, geo_region, content_md, status, is_deleted, created_at, updated_at
		) VALUES (?, 'article', 'art_00', '', '', '', '', '', 'Guest', '', '', 'enc_secret', 'en', '203.0.113.9', '203.0.113.0/24', '', ?, 'approved', ?, ?, ?)`,
			cid, md, false, ts, ts); err != nil {
			t.Fatalf("seed root comment: %v", err)
		}
	}
	for rIdx, rid := range []string{"rep_early", "rep_late"} {
		ts := now + 10 + int64(rIdx)*20
		if _, err := store.ExecContext(ctx, `INSERT INTO comments (
			id, target_type, target_id, parent_id, reply_to_name, user_id, guest_id_hash, env_fp_hash,
			author_name, author_url, avatar_url, notify_email_ciphertext, notify_locale,
			client_ip, ip_masked, geo_region, content_md, status, is_deleted, created_at, updated_at
		) VALUES (?, 'article', 'art_00', 'root_0', 'Guest', '', '', '', 'Replier', '', '', '', 'en', '', '', '', ?, 'approved', ?, ?, ?)`,
			rid, "Reply "+rid, false, ts, ts); err != nil {
			t.Fatalf("seed reply: %v", err)
		}
	}
	if _, err := store.ExecContext(ctx, `INSERT INTO comments (
		id, target_type, target_id, parent_id, reply_to_name, user_id, guest_id_hash, env_fp_hash,
		author_name, author_url, avatar_url, notify_email_ciphertext, notify_locale,
		client_ip, ip_masked, geo_region, content_md, status, is_deleted, created_at, updated_at
	) VALUES ('root_pending_own', 'article', 'art_00', '', '', 'u_rdr', '', '', 'Reader', '', '', '', 'en', '', '', '', 'Pending own comment', 'pending', ?, ?, ?)`,
		false, now+900, now+900); err != nil {
		t.Fatalf("seed pending own comment: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/comments", nil)
	// Anonymous viewer sees 5 roots (7 total comments), page 1 perPage=2 has root_4, root_3.
	anonPage1, err := svc.ListTargetCommentsPage(ctx, req, nil, domain.TargetTypeArticle, "art_00", 1, 2, "")
	if err != nil {
		t.Fatalf("ListTargetCommentsPage (anon p1): %v", err)
	}
	if anonPage1.TotalRoots != 5 || anonPage1.TotalComments != 7 || anonPage1.TotalPages != 3 {
		t.Fatalf("unexpected anon counts: %+v", anonPage1)
	}
	if len(anonPage1.Comments) != 2 || anonPage1.Comments[0].ID != "root_4" || anonPage1.Comments[1].ID != "root_3" {
		t.Fatalf("expected newest-first roots [root_4, root_3], got %+v", anonPage1.Comments)
	}
	// Verify RenderComment stripped <div id="volo-lightbox">, <input>, and footnote IDs!
	cHTML := anonPage1.Comments[0].ContentHTML
	if strings.Contains(cHTML, "volo-lightbox") || strings.Contains(cHTML, "<input") || strings.Contains(cHTML, "fn:1") {
		t.Fatalf("expected comment HTML sanitizer to neutralize raw HTML & footnote IDs, got: %s", cHTML)
	}

	// focusCommentID="rep_late" (attached to oldest root_0) jumps automatically to Page 3 and preserves chronological replies!
	focused, err := svc.ListTargetCommentsPage(ctx, req, nil, domain.TargetTypeArticle, "art_00", 1, 2, "rep_late")
	if err != nil {
		t.Fatalf("ListTargetCommentsPage (focus): %v", err)
	}
	if focused.Page != 3 || len(focused.Comments) != 1 || focused.Comments[0].ID != "root_0" {
		t.Fatalf("expected focusCommentID=rep_late to jump to page 3 [root_0], got page=%d comments=%+v", focused.Page, focused.Comments)
	}
	if len(focused.Comments[0].Replies) != 2 || focused.Comments[0].Replies[0].ID != "rep_early" || focused.Comments[0].Replies[1].ID != "rep_late" {
		t.Fatalf("expected chronological replies [rep_early, rep_late], got %+v", focused.Comments[0].Replies)
	}

	// Authenticated owner u_rdr sees their own pending comment at top (TotalRoots=6, TotalComments=8).
	rdr := &domain.User{ID: "u_rdr", Role: domain.RoleReader, Status: domain.UserStatusActive}
	rdrPage1, err := svc.ListTargetCommentsPage(ctx, req, rdr, domain.TargetTypeArticle, "art_00", 1, 2, "")
	if err != nil {
		t.Fatalf("ListTargetCommentsPage (owner): %v", err)
	}
	if rdrPage1.TotalRoots != 6 || rdrPage1.TotalComments != 8 || rdrPage1.Comments[0].ID != "root_pending_own" || !rdrPage1.Comments[0].CanEdit {
		t.Fatalf("expected owner to see pending own comment with CanEdit=true, got %+v", rdrPage1)
	}

	// Active admin sees CanBanIP=true (derived via SQL has_client_ip without selecting client_ip!).
	adm := &domain.User{ID: "u_adm", Role: domain.RoleAdmin, Status: domain.UserStatusActive}
	admPage1, err := svc.ListTargetCommentsPage(ctx, req, adm, domain.TargetTypeArticle, "art_00", 1, 2, "")
	if err != nil || !admPage1.Comments[1].CanBanIP || !admPage1.Comments[1].CanModerate {
		t.Fatalf("expected admin CanBanIP & CanModerate on root_4, got %+v (err=%v)", admPage1, err)
	}

	// Verify two-generation commentHTML cache rotation (> maxCommentHTMLGenEntries=2048).
	_ = svc.renderCommentMarkdownHTML(ctx, domain.SettingsSnapshot{}, domain.Comment{ID: "hot_cmt", UpdatedAt: 1, ContentMD: "**hot**"})
	for i := 0; i < maxCommentHTMLGenEntries+50; i++ {
		_ = svc.renderCommentMarkdownHTML(ctx, domain.SettingsSnapshot{}, domain.Comment{
			ID:        fmt.Sprintf("gen_%d", i),
			UpdatedAt: 1,
			ContentMD: "item",
		})
	}
	// Promote hot_cmt from Prev -> Curr, then trigger one more generation rotation.
	if got := svc.renderCommentMarkdownHTML(ctx, domain.SettingsSnapshot{}, domain.Comment{ID: "hot_cmt", UpdatedAt: 1, ContentMD: "**hot**"}); !strings.Contains(got, "<strong>hot</strong>") {
		t.Fatalf("unexpected hot_cmt HTML: %s", got)
	}
	svc.commentHTMLMu.RLock()
	totalCached := len(svc.commentHTMLCurr) + len(svc.commentHTMLPrev)
	_, hotInCurr := svc.commentHTMLCurr["hot_cmt:1"]
	svc.commentHTMLMu.RUnlock()
	if !hotInCurr || totalCached > 2*maxCommentHTMLGenEntries {
		t.Fatalf("expected hot_cmt promoted to Curr and totalCached <= %d, got hotInCurr=%v total=%d", 2*maxCommentHTMLGenEntries, hotInCurr, totalCached)
	}
}
