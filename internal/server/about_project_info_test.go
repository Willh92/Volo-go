package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Willh92/Volo-go/internal/auth"
	"github.com/Willh92/Volo-go/internal/config"
	"github.com/Willh92/Volo-go/internal/content"
	"github.com/Willh92/Volo-go/internal/db/migrate"
	"github.com/Willh92/Volo-go/internal/db/sqlx"
	"github.com/Willh92/Volo-go/internal/guest"
	"github.com/Willh92/Volo-go/internal/i18n"
	"github.com/Willh92/Volo-go/internal/render"
	"github.com/Willh92/Volo-go/internal/repo"
	"github.com/Willh92/Volo-go/internal/security"
	"github.com/Willh92/Volo-go/internal/server"
	"github.com/Willh92/Volo-go/internal/visibility"
	"github.com/Willh92/Volo-go/pkg/plugin"
)

func setupLoopbackAboutEnv(t *testing.T, version string) (*server.AppServer, *httptest.Server, []*http.Cookie) {
	t.Helper()
	ctx := context.Background()
	tmp := t.TempDir()
	dataDir := filepath.Join(tmp, "data")
	secret := "0123456789abcdef0123456789abcdef-about-test-secret"
	pubURL := "https://studio.example.com"
	cfg, err := config.Load(config.CLIOverrides{
		SecretKey: &secret,
		PublicURL: &pubURL,
		DataDir:   &dataDir,
	})
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	store, err := sqlx.Open(ctx, sqlx.Options{
		Driver: "sqlite",
		DSN:    filepath.Join(dataDir, "about-test.db"),
	})
	if err != nil {
		t.Fatalf("sqlx.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := migrate.Up(ctx, store); err != nil {
		t.Fatalf("migrate.Up: %v", err)
	}
	cat, err := i18n.NewCatalog("en-US", "")
	if err != nil {
		t.Fatalf("i18n.NewCatalog: %v", err)
	}
	settingsMgr := repo.NewSettingsManager(store, cfg.KeyRing, "en-US")
	if _, err := settingsMgr.Reload(ctx); err != nil {
		t.Fatalf("settings.Reload: %v", err)
	}
	dialer := &security.OutboundSafeDialer{
		Timeout:               5 * time.Second,
		AllowLoopbackForTests: true,
	}
	pluginMgr := plugin.NewManager(plugin.ManagerOptions{
		Store:          store,
		Keys:           cfg.KeyRing,
		PluginsDir:     filepath.Join(tmp, "plugins"),
		DataDir:        dataDir,
		Catalog:        cat,
		OutboundDialer: dialer,
	})
	if err := pluginMgr.DiscoverAndStart(ctx); err != nil {
		t.Fatalf("DiscoverAndStart: %v", err)
	}
	t.Cleanup(func() { _ = pluginMgr.Close(ctx) })

	passports := guest.NewPassportManager(cfg.KeyRing, cfg.PublicURL)
	pow := guest.NewPoWVerifier(cfg.KeyRing)
	rnd := render.Default()
	grd := visibility.NewGuard(cfg.KeyRing, settingsMgr, passports, cat, rnd)
	srch := visibility.NewSearchIndex(store, rnd, 2<<20)
	cnt := content.NewService(content.ServiceOptions{
		Store:          store,
		Keys:           cfg.KeyRing,
		Settings:       settingsMgr,
		Passports:      passports,
		PoW:            pow,
		Guard:          grd,
		Search:         srch,
		Renderer:       rnd,
		Plugins:        pluginMgr,
		Catalog:        cat,
		OutboundDialer: dialer,
	})
	authSvc := auth.NewService(store, cfg.KeyRing, settingsMgr, cfg.PublicURL)

	app, err := server.NewAppServer(server.AppDeps{
		Version:        version,
		Config:         cfg,
		Store:          store,
		Settings:       settingsMgr,
		Catalog:        cat,
		Auth:           authSvc,
		Content:        cnt,
		Plugins:        pluginMgr,
		Passports:      passports,
		PoW:            pow,
		OutboundDialer: dialer,
	})
	if err != nil {
		t.Fatalf("NewAppServer: %v", err)
	}
	t.Cleanup(func() { _ = app.Close(context.Background()) })
	ts := httptest.NewServer(app.Handler())
	t.Cleanup(ts.Close)

	stResp, _ := doJSONReq(t, http.MethodGet, ts.URL+"/api/setup/status", nil, nil, nil)
	var csrf string
	for _, c := range stResp.Cookies() {
		if strings.Contains(c.Name, "csrf") {
			csrf = c.Value
		}
	}
	okResp, okBody := doJSONReq(t, http.MethodPost, ts.URL+"/api/setup/complete", map[string]any{
		"admin_username":     "aboutadmin",
		"admin_email":        "about@example.com",
		"admin_password":     "pass1234",
		"admin_display_name": "About Admin",
		"site_title":         "About Test",
		"default_locale":     "en-US",
	}, stResp.Cookies(), map[string]string{"X-CSRF-Token": csrf})
	if okResp.StatusCode != http.StatusOK {
		t.Fatalf("setup complete failed: %d %s", okResp.StatusCode, string(okBody))
	}
	return app, ts, okResp.Cookies()
}

type aboutProjectResp struct {
	Version       string `json:"version"`
	LatestVersion string `json:"latest_version"`
	HasUpdate     bool   `json:"has_update"`
	Online        bool   `json:"online"`
	LiveFetched   bool   `json:"live_fetched"`
	Repo          struct {
		FullName   string `json:"full_name"`
		Stars      int    `json:"stars"`
		Forks      int    `json:"forks"`
		OpenIssues int    `json:"open_issues"`
	} `json:"repo"`
	LatestRelease struct {
		TagName string `json:"tag_name"`
		Name    string `json:"name"`
	} `json:"latest_release"`
}

func TestAboutProjectInfo_LivePartialCooldownSingleflightAndVersion(t *testing.T) {
	t.Run("LiveParallelFetchAndRefreshCooldown", func(t *testing.T) {
		app, ts, cookies := setupLoopbackAboutEnv(t, "")

		var repoHits atomic.Int32
		var relHits atomic.Int32
		var stars atomic.Int32
		stars.Store(123)
		var tag atomic.Value
		tag.Store("v9.9.9")

		gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case "/repo":
				repoHits.Add(1)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"full_name":         "Willh92/Volo-go",
					"description":       "Pure-Go blog",
					"language":          "Go",
					"stargazers_count":  stars.Load(),
					"forks_count":       15,
					"open_issues_count": 4,
					"html_url":          "https://github.com/Willh92/Volo-go",
				})
			case "/release":
				relHits.Add(1)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"tag_name":     tag.Load().(string),
					"name":         tag.Load().(string),
					"html_url":     "https://github.com/Willh92/Volo-go/releases/tag/v9.9.9",
					"published_at": "2026-11-19T00:00:00Z",
				})
			default:
				http.NotFound(w, r)
			}
		}))
		defer gh.Close()

		var clockMu sync.Mutex
		now := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
		app.SetAboutGitHubURLsForTest(gh.URL+"/repo", gh.URL+"/release")
		app.SetAboutClockForTest(func() time.Time {
			clockMu.Lock()
			defer clockMu.Unlock()
			return now
		})

		resp, body := doJSONReq(t, http.MethodGet, ts.URL+"/api/admin/about/project-info", nil, cookies, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status=%d body=%s", resp.StatusCode, string(body))
		}
		var got aboutProjectResp
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatal(err)
		}
		if !got.Online || !got.LiveFetched || !got.HasUpdate || got.LatestVersion != "v9.9.9" ||
			got.Repo.Stars != 123 || got.Repo.Forks != 15 || got.Repo.OpenIssues != 4 || got.Version != "v1.0.4" {
			t.Fatalf("unexpected live about payload: %+v", got)
		}
		if repoHits.Load() != 1 || relHits.Load() != 1 {
			t.Fatalf("expected 1 hit each, got repo=%d rel=%d", repoHits.Load(), relHits.Load())
		}

		// ?refresh=1 within 10s cooldown must NOT hit GitHub again.
		clockMu.Lock()
		now = now.Add(5 * time.Second)
		clockMu.Unlock()
		stars.Store(456)
		_, body2 := doJSONReq(t, http.MethodGet, ts.URL+"/api/admin/about/project-info?refresh=1", nil, cookies, nil)
		var got2 aboutProjectResp
		_ = json.Unmarshal(body2, &got2)
		if repoHits.Load() != 1 || relHits.Load() != 1 || got2.Repo.Stars != 123 {
			t.Fatalf("expected cooldown cache hit, got hits=(%d,%d) stars=%d", repoHits.Load(), relHits.Load(), got2.Repo.Stars)
		}

		// After > 10s, ?refresh=1 triggers fresh fetch.
		clockMu.Lock()
		now = now.Add(6 * time.Second) // +11s total
		clockMu.Unlock()
		tag.Store("v10.0.0")
		_, body3 := doJSONReq(t, http.MethodGet, ts.URL+"/api/admin/about/project-info?refresh=1", nil, cookies, nil)
		var got3 aboutProjectResp
		_ = json.Unmarshal(body3, &got3)
		if repoHits.Load() != 2 || relHits.Load() != 2 || got3.Repo.Stars != 456 || got3.LatestVersion != "v10.0.0" {
			t.Fatalf("expected refresh after cooldown, got hits=(%d,%d) payload=%+v", repoHits.Load(), relHits.Load(), got3)
		}
	})

	t.Run("PartialFailureRepoOKRelFail", func(t *testing.T) {
		app, ts, cookies := setupLoopbackAboutEnv(t, "v1.0.4")
		var repoHits, relHits atomic.Int32
		var relStatus atomic.Int32
		relStatus.Store(http.StatusInternalServerError)

		gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/repo":
				repoHits.Add(1)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"full_name":        "Willh92/Volo-go",
					"stargazers_count": 77,
				})
			case "/release":
				relHits.Add(1)
				if relStatus.Load() != http.StatusOK {
					w.WriteHeader(int(relStatus.Load()))
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"tag_name": "v1.9.0",
					"name":     "v1.9.0",
				})
			}
		}))
		defer gh.Close()

		var clockMu sync.Mutex
		now := time.Date(2026, 11, 19, 11, 0, 0, 0, time.UTC)
		app.SetAboutGitHubURLsForTest(gh.URL+"/repo", gh.URL+"/release")
		app.SetAboutClockForTest(func() time.Time {
			clockMu.Lock()
			defer clockMu.Unlock()
			return now
		})

		_, body := doJSONReq(t, http.MethodGet, ts.URL+"/api/admin/about/project-info", nil, cookies, nil)
		var got aboutProjectResp
		_ = json.Unmarshal(body, &got)
		if got.Online || got.LiveFetched || got.Repo.Stars != 77 || got.LatestRelease.TagName != "v1.0.4" || got.HasUpdate {
			t.Fatalf("expected partial failure online=false with repo stars=77, got %+v", got)
		}

		// Advance 3m (> 2m failure TTL, < 30m success TTL): only /release retries.
		relStatus.Store(http.StatusOK)
		clockMu.Lock()
		now = now.Add(3 * time.Minute)
		clockMu.Unlock()

		_, body2 := doJSONReq(t, http.MethodGet, ts.URL+"/api/admin/about/project-info", nil, cookies, nil)
		var got2 aboutProjectResp
		_ = json.Unmarshal(body2, &got2)
		if !got2.Online || !got2.LiveFetched || got2.LatestRelease.TagName != "v1.9.0" {
			t.Fatalf("expected recovery to online=true, got %+v", got2)
		}
		if repoHits.Load() != 1 || relHits.Load() != 2 {
			t.Fatalf("expected selective retry (repo=1, rel=2), got repo=%d rel=%d", repoHits.Load(), relHits.Load())
		}
	})

	t.Run("PartialFailureRepoFailRelOK", func(t *testing.T) {
		app, ts, cookies := setupLoopbackAboutEnv(t, "v1.0.4")
		var repoHits, relHits atomic.Int32
		var repoStatus atomic.Int32
		repoStatus.Store(http.StatusBadGateway)

		gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/repo":
				repoHits.Add(1)
				if repoStatus.Load() != http.StatusOK {
					w.WriteHeader(int(repoStatus.Load()))
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"full_name":        "Willh92/Volo-go",
					"stargazers_count": 88,
				})
			case "/release":
				relHits.Add(1)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"tag_name": "v2.0.0",
					"name":     "v2.0.0",
				})
			}
		}))
		defer gh.Close()

		var clockMu sync.Mutex
		now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
		app.SetAboutGitHubURLsForTest(gh.URL+"/repo", gh.URL+"/release")
		app.SetAboutClockForTest(func() time.Time {
			clockMu.Lock()
			defer clockMu.Unlock()
			return now
		})

		_, body := doJSONReq(t, http.MethodGet, ts.URL+"/api/admin/about/project-info", nil, cookies, nil)
		var got aboutProjectResp
		_ = json.Unmarshal(body, &got)
		if got.Online || got.LiveFetched || got.Repo.Stars != 0 || got.LatestRelease.TagName != "v2.0.0" {
			t.Fatalf("expected partial failure online=false with release v2.0.0, got %+v", got)
		}

		repoStatus.Store(http.StatusOK)
		clockMu.Lock()
		now = now.Add(3 * time.Minute)
		clockMu.Unlock()

		_, body2 := doJSONReq(t, http.MethodGet, ts.URL+"/api/admin/about/project-info", nil, cookies, nil)
		var got2 aboutProjectResp
		_ = json.Unmarshal(body2, &got2)
		if !got2.Online || !got2.LiveFetched || got2.Repo.Stars != 88 {
			t.Fatalf("expected repo recovery online=true stars=88, got %+v", got2)
		}
		if repoHits.Load() != 2 || relHits.Load() != 1 {
			t.Fatalf("expected selective retry (repo=2, rel=1), got repo=%d rel=%d", repoHits.Load(), relHits.Load())
		}
	})

	t.Run("SingleflightAndCallerCancellation", func(t *testing.T) {
		app, ts, cookies := setupLoopbackAboutEnv(t, "v1.0.4")
		var repoHits, relHits atomic.Int32

		gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(120 * time.Millisecond)
			switch r.URL.Path {
			case "/repo":
				repoHits.Add(1)
				_ = json.NewEncoder(w).Encode(map[string]any{"full_name": "Willh92/Volo-go", "stargazers_count": 321})
			case "/release":
				relHits.Add(1)
				_ = json.NewEncoder(w).Encode(map[string]any{"tag_name": "v1.5.0"})
			}
		}))
		defer gh.Close()
		app.SetAboutGitHubURLsForTest(gh.URL+"/repo", gh.URL+"/release")

		var wg sync.WaitGroup
		errs := make(chan error, 5)
		results := make(chan aboutProjectResp, 4)

		// 1st caller cancels quickly; background singleflight fetch must still finish for the other 4 callers.
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/admin/about/project-info", nil)
			for _, c := range cookies {
				req.AddCookie(c)
			}
			_, _ = http.DefaultClient.Do(req)
		}()

		for i := 0; i < 4; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/admin/about/project-info", nil)
				for _, c := range cookies {
					req.AddCookie(c)
				}
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					errs <- err
					return
				}
				defer resp.Body.Close()
				var payload aboutProjectResp
				if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
					errs <- err
					return
				}
				results <- payload
			}()
		}
		wg.Wait()
		close(errs)
		close(results)
		for err := range errs {
			t.Fatalf("concurrent caller error: %v", err)
		}
		for res := range results {
			if !res.Online || res.Repo.Stars != 321 || res.LatestRelease.TagName != "v1.5.0" {
				t.Fatalf("unexpected concurrent result: %+v", res)
			}
		}
		if repoHits.Load() != 1 || relHits.Load() != 1 {
			t.Fatalf("singleflight expected 1 hit each, got repo=%d rel=%d", repoHits.Load(), relHits.Load())
		}
	})

	t.Run("VersionConsistencyWhenDepsVersionEmpty", func(t *testing.T) {
		ctx := context.Background()
		tmp := t.TempDir()
		dataDir := filepath.Join(tmp, "data")
		secret := "0123456789abcdef0123456789abcdef-ver-test-secret"
		pubURL := "https://studio.example.com"
		cfg, _ := config.Load(config.CLIOverrides{SecretKey: &secret, PublicURL: &pubURL, DataDir: &dataDir})
		store, _ := sqlx.Open(ctx, sqlx.Options{Driver: "sqlite", DSN: filepath.Join(dataDir, "ver.db")})
		defer store.Close()
		_ = migrate.Up(ctx, store)
		cat, _ := i18n.NewCatalog("en-US", "")
		settingsMgr := repo.NewSettingsManager(store, cfg.KeyRing, "en-US")
		_, _ = settingsMgr.Reload(ctx)
		app, err := server.NewAppServer(server.AppDeps{
			Version:  "", // Intentionally empty -> normalized to v1.0.4
			Config:   cfg,
			Store:    store,
			Settings: settingsMgr,
			Catalog:  cat,
		})
		if err != nil {
			t.Fatal(err)
		}
		defer app.Close(context.Background())
		ts := httptest.NewServer(app.Handler())
		defer ts.Close()

		// 1. Setup status + /setup BootConfig before setup complete
		stResp, stBody := doJSONReq(t, http.MethodGet, ts.URL+"/api/setup/status", nil, nil, nil)
		if stResp.StatusCode != http.StatusOK || !bytes.Contains(stBody, []byte(`"version":"v1.0.4"`)) {
			t.Fatalf("GET /api/setup/status missing version v1.0.4: %s", string(stBody))
		}
		setupHTMLResp, setupHTML := doJSONReq(t, http.MethodGet, ts.URL+"/setup", nil, nil, nil)
		if setupHTMLResp.StatusCode != http.StatusOK || !bytes.Contains(setupHTML, []byte(`"version":"v1.0.4"`)) {
			t.Fatalf("GET /setup missing BootConfig version v1.0.4")
		}

		// Complete setup to test runtime-logs & plugins catalog
		var csrf string
		for _, c := range stResp.Cookies() {
			if strings.Contains(c.Name, "csrf") {
				csrf = c.Value
			}
		}
		okResp, _ := doJSONReq(t, http.MethodPost, ts.URL+"/api/setup/complete", map[string]any{
			"admin_username":     "veradmin",
			"admin_email":        "ver@example.com",
			"admin_password":     "pass1234",
			"admin_display_name": "Ver Admin",
			"site_title":         "Ver Test",
			"default_locale":     "en-US",
		}, stResp.Cookies(), map[string]string{"X-CSRF-Token": csrf})
		if okResp.StatusCode != http.StatusOK {
			t.Fatalf("setup complete failed: %d", okResp.StatusCode)
		}

		logsResp, logsBody := doJSONReq(t, http.MethodGet, ts.URL+"/api/admin/runtime-logs", nil, okResp.Cookies(), nil)
		if logsResp.StatusCode != http.StatusOK || !bytes.Contains(logsBody, []byte(`"version":"v1.0.4"`)) {
			t.Fatalf("GET /api/admin/runtime-logs missing version v1.0.4: %s", string(logsBody))
		}
		catResp, catBody := doJSONReq(t, http.MethodGet, ts.URL+"/api/admin/plugins/catalog", nil, okResp.Cookies(), nil)
		if catResp.StatusCode != http.StatusOK || !bytes.Contains(catBody, []byte(`"host_version":"v1.0.4"`)) {
			t.Fatalf("GET /api/admin/plugins/catalog missing host_version v1.0.4: %s", string(catBody))
		}
	})
}
