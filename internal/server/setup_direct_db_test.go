package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Willh92/Volo-go/internal/auth"
	"github.com/Willh92/Volo-go/internal/config"
	"github.com/Willh92/Volo-go/internal/content"
	"github.com/Willh92/Volo-go/internal/db/migrate"
	"github.com/Willh92/Volo-go/internal/db/sqlx"
	"github.com/Willh92/Volo-go/internal/guest"
	"github.com/Willh92/Volo-go/internal/i18n"
	"github.com/Willh92/Volo-go/internal/notify"
	"github.com/Willh92/Volo-go/internal/render"
	"github.com/Willh92/Volo-go/internal/repo"
	"github.com/Willh92/Volo-go/internal/security"
	"github.com/Willh92/Volo-go/internal/server"
	"github.com/Willh92/Volo-go/internal/visibility"
	"github.com/Willh92/Volo-go/pkg/plugin"
)

func newUninitializedWizardEnv(t *testing.T) (*round2Env, string) {
	t.Helper()
	ctx := context.Background()
	tmp := t.TempDir()

	dataDir := filepath.Join(tmp, "data")
	uploadDir := filepath.Join(tmp, "uploads")
	pluginsDir := filepath.Join(tmp, "plugins")
	themesDir := filepath.Join(tmp, "themes")
	backupDir := filepath.Join(tmp, "backups")
	for _, d := range []string{dataDir, uploadDir, pluginsDir, themesDir, backupDir} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}

	secret := "0123456789abcdef0123456789abcdef-direct-db-setup-secret"
	pubURL := "https://studio.example.com"
	adminPath := "/console"
	bootDB := filepath.Join(dataDir, "volo-boot.db")
	sqliteDriver := "sqlite"
	cfg, err := config.Load(config.CLIOverrides{
		SecretKey:  &secret,
		PublicURL:  &pubURL,
		AdminPath:  &adminPath,
		DBDriver:   &sqliteDriver,
		DBDSN:      &bootDB,
		DataDir:    &dataDir,
		UploadDir:  &uploadDir,
		PluginsDir: &pluginsDir,
		ThemesDir:  &themesDir,
		BackupDir:  &backupDir,
	})
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	store, err := sqlx.Open(ctx, sqlx.Options{
		Driver: "sqlite",
		DSN:    bootDB,
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
		t.Fatal(err)
	}
	settingsMgr := repo.NewSettingsManager(store, cfg.KeyRing, "en")
	if _, err := settingsMgr.Reload(ctx); err != nil {
		t.Fatal(err)
	}

	dialer := &security.OutboundSafeDialer{
		Timeout:               5 * time.Second,
		AllowLoopbackForTests: true,
	}

	pluginMgr := plugin.NewManager(plugin.ManagerOptions{
		Store:      store,
		Keys:       cfg.KeyRing,
		PluginsDir: pluginsDir,
		DataDir:    dataDir,
		Catalog:    cat,
	})
	if err := pluginMgr.DiscoverAndStart(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pluginMgr.Close(ctx) })

	passports := guest.NewPassportManager(cfg.KeyRing, cfg.PublicURL)
	pow := guest.NewPoWVerifier(cfg.KeyRing)
	rnd := render.Default()
	grd := visibility.NewGuard(cfg.KeyRing, settingsMgr, passports, cat, rnd)
	srch := visibility.NewSearchIndex(store, rnd, 2<<20)
	cnt := content.NewService(content.ServiceOptions{
		Store:     store,
		Keys:      cfg.KeyRing,
		Settings:  settingsMgr,
		Passports: passports,
		PoW:       pow,
		Guard:     grd,
		Search:    srch,
		Renderer:  rnd,
		Plugins:   pluginMgr,
		Catalog:   cat,
	})
	authSvc := auth.NewService(store, cfg.KeyRing, settingsMgr, cfg.PublicURL)
	disp := notify.NewDispatcher(notify.DispatcherOptions{
		Store:     store,
		Keys:      cfg.KeyRing,
		Settings:  settingsMgr,
		Catalog:   cat,
		Plugins:   pluginMgr,
		PublicURL: cfg.PublicURL,
	})

	appSrv, err := server.NewAppServer(server.AppDeps{
		Config:               cfg,
		Store:                store,
		Settings:             settingsMgr,
		Catalog:              cat,
		Auth:                 authSvc,
		Passports:            passports,
		PoW:                  pow,
		OutboundDialer:       dialer,
		Plugins:              pluginMgr,
		Content:              cnt,
		SendVerificationMail: disp.SendVerificationCode,
		NotifyCommentCreated: disp.NotifyCommentCreated,
		TestSMTP:             disp.SendTestSMTP,
		TestWebhook:          disp.TriggerTestWebhook,
	})
	if err != nil {
		t.Fatalf("NewAppServer: %v", err)
	}
	ts := httptest.NewServer(appSrv.Handler())
	t.Cleanup(ts.Close)

	return &round2Env{
		ctx:       ctx,
		cfg:       cfg,
		store:     store,
		settings:  settingsMgr,
		pluginMgr: pluginMgr,
		authSvc:   authSvc,
		content:   cnt,
		app:       appSrv,
		ts:        ts,
		uploadDir: uploadDir,
	}, dataDir
}

func seedLegacySoloSQLite(t *testing.T, dbPath string) {
	t.Helper()
	ctx := context.Background()
	st, err := sqlx.Open(ctx, sqlx.Options{
		Driver: "sqlite",
		DSN:    dbPath,
	})
	if err != nil {
		t.Fatalf("open legacy solo sqlite: %v", err)
	}
	defer st.Close()

	stmts := []string{
		`CREATE TABLE b3_solo_option (
			oId TEXT PRIMARY KEY,
			optionCategory TEXT NOT NULL,
			optionValue TEXT NOT NULL
		)`,
		`INSERT INTO b3_solo_option (oId, optionCategory, optionValue) VALUES
			('blogTitle', 'preference', 'Legacy Solo Studio'),
			('blogSubtitle', 'preference', 'From Solo DB'),
			('timeZoneId', 'preference', 'Asia/Shanghai')`,
		`CREATE TABLE b3_solo_user (
			oId TEXT PRIMARY KEY,
			userName TEXT NOT NULL,
			userEmail TEXT NOT NULL,
			userRole TEXT NOT NULL,
			userAvatar TEXT NOT NULL,
			userURL TEXT NOT NULL,
			userPasswordHash TEXT NOT NULL,
			userStatus INTEGER NOT NULL,
			userCreated INTEGER NOT NULL
		)`,
		`INSERT INTO b3_solo_user (oId, userName, userEmail, userRole, userAvatar, userURL, userPasswordHash, userStatus, userCreated) VALUES
			('u_1001', 'legacy_admin', 'legacy@example.com', 'adminRole', 'https://old.example.com/avatar.png', 'https://old.example.com', '', 0, 1715000000000)`,
		`CREATE TABLE b3_solo_category (
			oId TEXT PRIMARY KEY,
			categoryTitle TEXT NOT NULL,
			categoryURI TEXT NOT NULL,
			categoryDescription TEXT NOT NULL,
			categoryOrder INTEGER NOT NULL
		)`,
		`INSERT INTO b3_solo_category (oId, categoryTitle, categoryURI, categoryDescription, categoryOrder) VALUES
			('c_1', 'Architecture', 'arch', 'Architecture notes', 1)`,
		`CREATE TABLE b3_solo_tag (
			oId TEXT PRIMARY KEY,
			tagTitle TEXT NOT NULL
		)`,
		`INSERT INTO b3_solo_tag (oId, tagTitle) VALUES ('t_1', 'golang')`,
		`CREATE TABLE b3_solo_category_tag (
			oId TEXT PRIMARY KEY,
			category_oId TEXT NOT NULL,
			tag_oId TEXT NOT NULL
		)`,
		`INSERT INTO b3_solo_category_tag (oId, category_oId, tag_oId) VALUES ('ct_1', 'c_1', 't_1')`,
		`CREATE TABLE b3_solo_article (
			oId TEXT PRIMARY KEY,
			articleTitle TEXT NOT NULL,
			articleAbstract TEXT NOT NULL,
			articleContent TEXT NOT NULL,
			articleAuthorId TEXT NOT NULL,
			articleCommentCount INTEGER NOT NULL,
			articleViewCount INTEGER NOT NULL,
			articlePermalink TEXT NOT NULL,
			articlePutTop INTEGER NOT NULL,
			articleCreated TEXT NOT NULL,
			articleUpdated TEXT NOT NULL,
			articleStatus INTEGER NOT NULL,
			articleCommentable INTEGER NOT NULL,
			articleViewPwd TEXT NOT NULL,
			articleImg1URL TEXT NOT NULL,
			articleTags TEXT NOT NULL
		)`,
		`INSERT INTO b3_solo_article (
			oId, articleTitle, articleAbstract, articleContent, articleAuthorId,
			articleCommentCount, articleViewCount, articlePermalink, articlePutTop,
			articleCreated, articleUpdated, articleStatus, articleCommentable,
			articleViewPwd, articleImg1URL, articleTags
		) VALUES (
			'a_101', 'Direct DB Migration Post', 'Summary from https://old.example.com/cover.png',
			'Hello from Solo DB! ![Hero](https://old.example.com/images/hero.png)',
			'u_1001', 1, 42, '/articles/2024/05/direct-db-post.html', 0,
			'2024-05-12 10:20:30', '2024-05-12 11:20:30', 0, 1, '',
			'https://old.example.com/cover.png', 'golang'
		)`,
		`CREATE TABLE b3_solo_tag_article (
			oId TEXT PRIMARY KEY,
			article_oId TEXT NOT NULL,
			tag_oId TEXT NOT NULL
		)`,
		`INSERT INTO b3_solo_tag_article (oId, article_oId, tag_oId) VALUES ('ta_1', 'a_101', 't_1')`,
		`CREATE TABLE b3_solo_comment (
			oId TEXT PRIMARY KEY,
			commentContent TEXT NOT NULL,
			commentCreated TEXT NOT NULL,
			commentName TEXT NOT NULL,
			commentOnId TEXT NOT NULL,
			commentOriginalCommentId TEXT NOT NULL,
			commentOriginalCommentName TEXT NOT NULL,
			commentThumbnailURL TEXT NOT NULL,
			commentURL TEXT NOT NULL,
			commentStatus INTEGER NOT NULL
		)`,
		`INSERT INTO b3_solo_comment (
			oId, commentContent, commentCreated, commentName, commentOnId,
			commentOriginalCommentId, commentOriginalCommentName, commentThumbnailURL, commentURL, commentStatus
		) VALUES (
			'cm_1', 'Seamless direct DB migration!', '2024-05-13 09:00:00', 'Alice', 'a_101',
			'', '', '', 'https://alice.example.org', 0
		)`,
	}
	for _, q := range stmts {
		if _, err := st.ExecContext(ctx, q); err != nil {
			t.Fatalf("seed legacy stmt failed: %v\nSQL: %s", err, q)
		}
	}
}

func doSetupWizardJSON(t *testing.T, tsURL, path string, payload any) (*http.Response, map[string]any) {
	t.Helper()
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, tsURL+path, bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", tsURL)
	cli := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := cli.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp, out
}

func TestSetupDirectDBMigrationEndToEnd(t *testing.T) {
	env, dataDir := newUninitializedWizardEnv(t)
	legacyPath := filepath.Join(dataDir, "legacy-solo.db")
	seedLegacySoloSQLite(t, legacyPath)

	// 1. Path traversal & invalid table prefix must be rejected.
	resp, _ := doSetupWizardJSON(t, env.ts.URL, "/api/setup/test-legacy-db", map[string]any{
		"source_driver": "sqlite",
		"source_dsn":    "../outside.db",
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for ../outside.db path traversal, got %d", resp.StatusCode)
	}

	resp, _ = doSetupWizardJSON(t, env.ts.URL, "/api/setup/test-legacy-db", map[string]any{
		"source_driver":       "sqlite",
		"source_dsn":          "legacy-solo.db",
		"source_table_prefix": "b3_solo_; DROP TABLE users;--",
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid source_table_prefix, got %d", resp.StatusCode)
	}

	// 2. Test Legacy DB inspection succeeds read-only and detects Solo schema + counts.
	resp, body := doSetupWizardJSON(t, env.ts.URL, "/api/setup/test-legacy-db", map[string]any{
		"source_driver": "sqlite",
		"source_dsn":    "./data/legacy-solo.db",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("test-legacy-db failed (%d): %#v", resp.StatusCode, body)
	}
	if body["schema_kind"] != "solo" || body["table_prefix"] != "b3_solo_" {
		t.Fatalf("unexpected inspection result: %#v", body)
	}
	if int(body["articles"].(float64)) != 1 || int(body["comments"].(float64)) != 1 || int(body["users"].(float64)) != 1 {
		t.Fatalf("unexpected entity counts: %#v", body)
	}

	// 3. Reject migrating a database onto itself (source == target).
	resp, _ = doSetupWizardJSON(t, env.ts.URL, "/api/setup/migrate-db", map[string]any{
		"driver":        "sqlite",
		"dsn":           "legacy-solo.db",
		"source_driver": "sqlite",
		"source_dsn":    "./data/legacy-solo.db",
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 when source == target DB, got %d", resp.StatusCode)
	}

	// 4. Dry-Run preview streams and reports counts & URL rewrites without locking setup.
	resp, dryBody := doSetupWizardJSON(t, env.ts.URL, "/api/setup/migrate-db?dry_run=1", map[string]any{
		"driver":        "sqlite",
		"dsn":           "volo-migrated.db",
		"source_driver": "sqlite",
		"source_dsn":    "legacy-solo.db",
		"old_site_url":  "https://old.example.com",
		"new_site_url":  "https://new.example.com",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("dry-run migrate-db failed (%d): %#v", resp.StatusCode, dryBody)
	}
	if dryBody["dry_run"] != true {
		t.Fatalf("expected dry_run=true, got %#v", dryBody)
	}
	dryRep, _ := dryBody["report"].(map[string]any)
	if int(dryRep["articles"].(float64)) != 1 || int(dryRep["urls_rewritten"].(float64)) < 1 {
		t.Fatalf("unexpected dry-run report: %#v", dryRep)
	}

	// Setup must still be unlocked after Dry-Run!
	stResp, err := http.Get(env.ts.URL + "/api/setup/status")
	if err != nil {
		t.Fatal(err)
	}
	_ = stResp.Body.Close()
	if stResp.StatusCode != http.StatusOK {
		t.Fatalf("expected setup to remain unlocked after dry-run, got %d", stResp.StatusCode)
	}

	// 5. Execute live direct DB migration into new target SQLite DB with admin override & URL rewrite!
	resp, liveBody := doSetupWizardJSON(t, env.ts.URL, "/api/setup/migrate-db", map[string]any{
		"driver":         "sqlite",
		"dsn":            "volo-migrated.db",
		"source_driver":  "sqlite",
		"source_dsn":     "legacy-solo.db",
		"old_site_url":   "https://old.example.com",
		"new_site_url":   "https://new.example.com",
		"site_title":     "Migrated Volo Studio",
		"admin_username": "captain_migrated",
		"admin_email":    "captain@new.example.com",
		"admin_password": "StrongMigratedPassword!2026",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("live migrate-db failed (%d): %#v", resp.StatusCode, liveBody)
	}
	if strings.TrimSpace(liveBody["csrf_token"].(string)) == "" {
		t.Fatalf("expected csrf_token in live migration response, got %#v", liveBody)
	}
	if len(resp.Cookies()) == 0 {
		t.Fatalf("expected admin session cookie after live DB migration")
	}

	// Verify 301 redirect from legacy .html permalink works on public server!
	cliNoRedir := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	redirResp, err := cliNoRedir.Get(env.ts.URL + "/articles/2024/05/direct-db-post.html")
	if err != nil {
		t.Fatal(err)
	}
	_ = redirResp.Body.Close()
	if redirResp.StatusCode != http.StatusMovedPermanently {
		t.Fatalf("expected 301 redirect for legacy .html permalink, got %d", redirResp.StatusCode)
	}
	loc := redirResp.Header.Get("Location")
	if !strings.HasPrefix(loc, "/posts/") {
		t.Fatalf("expected redirect Location under /posts/, got %q", loc)
	}

	// Verify rewritten image URL in rendered public post!
	postResp, err := http.Get(env.ts.URL + loc)
	if err != nil {
		t.Fatal(err)
	}
	postHTMLBytes, _ := io.ReadAll(postResp.Body)
	_ = postResp.Body.Close()
	postHTML := string(postHTMLBytes)
	if !strings.Contains(postHTML, "https://new.example.com/images/hero.png") {
		t.Fatalf("expected rewritten URL in rendered post HTML, got snippet: %.400s", postHTML)
	}

	// 6. Verify /setup, /api/setup/test-legacy-db, and /api/setup/migrate-db permanently return 404!
	for _, ep := range []string{"/api/setup/status", "/api/setup/test-legacy-db", "/api/setup/migrate-db"} {
		chk, _ := doSetupWizardJSON(t, env.ts.URL, ep, map[string]any{})
		if chk.StatusCode != http.StatusNotFound {
			t.Fatalf("expected 404 for %s after setup completion, got %d", ep, chk.StatusCode)
		}
	}
}
