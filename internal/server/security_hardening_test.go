package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Willh92/Volo-go/internal/domain"
	"github.com/Willh92/Volo-go/internal/server"
	"github.com/Willh92/Volo-go/pkg/id"
)

func TestSecurityHardening_StrictJSONDecodeAndLegacyCategoryRedirect(t *testing.T) {
	env := setupM5TestEnv(t, "/console")

	// Complete setup
	stResp, _ := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/setup/status", nil, nil, nil)
	var setupCSRF string
	for _, c := range stResp.Cookies() {
		if strings.Contains(c.Name, "csrf") {
			setupCSRF = c.Value
		}
	}
	okSetup, okSetupBody := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/setup/complete", map[string]any{
		"admin_username":     "secadmin",
		"admin_email":        "sec@example.com",
		"admin_password":     "pass1234",
		"admin_display_name": "Sec Admin",
		"site_title":         "Sec Test",
		"default_locale":     "en-US",
	}, stResp.Cookies(), map[string]string{"X-CSRF-Token": setupCSRF})
	if okSetup.StatusCode != http.StatusOK {
		t.Fatalf("setup complete failed: %d", okSetup.StatusCode)
	}
	adminCookies := okSetup.Cookies()
	var setupDone struct {
		CSRFToken string `json:"csrf_token"`
	}
	_ = json.Unmarshal(okSetupBody, &setupDone)
	adminCSRF := map[string]string{"X-CSRF-Token": setupDone.CSRFToken}

	// Create password-protected article and comment with recorded IP so share-token, reply, ban-ip are reachable.
	artResp, artBody := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/articles", map[string]any{
		"title":      "Sec Test Post",
		"slug":       "sec-test-post",
		"content_md": "hello",
		"status":     "published",
		"password":   "secret123",
	}, adminCookies, adminCSRF)
	if artResp.StatusCode != http.StatusCreated {
		t.Fatalf("create article failed: %d %s", artResp.StatusCode, string(artBody))
	}
	var artCreated struct {
		Article struct {
			ID string `json:"id"`
		} `json:"article"`
	}
	if err := json.Unmarshal(artBody, &artCreated); err != nil {
		t.Fatal(err)
	}
	artID := artCreated.Article.ID

	cmtID := id.Next()
	_, err := env.store.ExecContext(env.ctx,
		`INSERT INTO comments (id, target_type, target_id, parent_id, reply_to_name, user_id, guest_id_hash, env_fp_hash, author_name, author_url, avatar_url, notify_email_ciphertext, notify_locale, client_ip, ip_masked, geo_region, content_md, status, is_deleted, created_at, updated_at)
		 VALUES (?, 'article', ?, '', '', '', '', '', 'Bob', '', '', '', 'en', '203.0.113.10', '203.0.113.x', '', 'hi', 'approved', 0, 1000, 1000)`,
		cmtID, artID)
	if err != nil {
		t.Fatalf("insert comment: %v", err)
	}

	// 1. Malformed JSON must return 400 invalid json on all hardened endpoints.
	badEndpoints := []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodPost, "/api/admin/seo/github-sync", `{bad`},
		{http.MethodPost, "/api/admin/plugins/nonexistent/cron", `{bad`},
		{http.MethodPost, "/api/admin/settings/storage/test", `{bad`},
		{http.MethodPost, "/api/admin/articles/" + artID + "/share-token", `{bad`},
		{http.MethodPost, "/api/admin/comments/" + cmtID + "/ban-ip", `{bad`},
		{http.MethodPost, "/api/admin/comments/" + cmtID + "/reply", `{bad`},
		{http.MethodPatch, "/api/admin/settings/" + domain.SettingsDomainSite, `{"expected_version":"not-int"}`},
	}
	for _, ep := range badEndpoints {
		req, _ := http.NewRequest(ep.method, env.ts.URL+ep.path, strings.NewReader(ep.body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", env.ts.URL)
		for _, c := range adminCookies {
			req.AddCookie(c)
		}
		for k, v := range adminCSRF {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", ep.method, ep.path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s %s malformed JSON expected 400, got %d", ep.method, ep.path, resp.StatusCode)
		}
	}

	// 2. Empty body must still be accepted by optional-JSON endpoints.
	for _, path := range []string{
		"/api/admin/settings/storage/test",
		"/api/admin/articles/" + artID + "/share-token",
		"/api/admin/plugins/nonexistent/cron",
		"/api/admin/comments/" + cmtID + "/ban-ip",
	} {
		req, _ := http.NewRequest(http.MethodPost, env.ts.URL+path, nil)
		req.Header.Set("Origin", env.ts.URL)
		for _, c := range adminCookies {
			req.AddCookie(c)
		}
		for k, v := range adminCSRF {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("empty body POST %s: %v", path, err)
		}
		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusBadRequest {
			t.Fatalf("empty body POST %s unexpectedly returned 400: %s", path, string(respBody))
		}
	}

	// 3. Trailing JSON garbage must be rejected with HTTP 400 on decodeJSONBody, decodeOptJSONBody, and decodePublicJSONBody endpoints.
	trailingGarbageEndpoints := []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodPost, "/api/admin/seo/github-sync", `{"username":"octocat"}{"b":2}`},
		{http.MethodPost, "/api/admin/plugins/nonexistent/cron", `{"name":"x"}{"b":2}`},
		{http.MethodPost, "/api/admin/settings/storage/test", `{"a":1}{"b":2}`},
		{http.MethodPost, "/api/admin/articles/" + artID + "/share-token", `{"ttl_hours":10}{"b":2}`},
		{http.MethodPost, "/api/admin/comments/" + cmtID + "/ban-ip", `{"note":"ok"}{"b":2}`},
		{http.MethodPost, "/api/admin/comments/" + cmtID + "/reply", `{"content_md":"reply"}{"b":2}`},
		{http.MethodPatch, "/api/admin/settings/" + domain.SettingsDomainSite, `{"expected_version":1}{"b":2}`},
		{http.MethodPost, "/api/v1/articles/sec-test-post/unlock", `{"password":"wrong"}{"b":2}`},
	}
	for _, ep := range trailingGarbageEndpoints {
		req, _ := http.NewRequest(ep.method, env.ts.URL+ep.path, strings.NewReader(ep.body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", env.ts.URL)
		for _, c := range adminCookies {
			req.AddCookie(c)
		}
		for k, v := range adminCSRF {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s trailing garbage: %v", ep.method, ep.path, err)
		}
		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s %s trailing JSON garbage expected 400, got %d (%s)", ep.method, ep.path, resp.StatusCode, string(respBody))
		}
	}

	// 4. Legacy /category/ and /articles/ redirect sanitization: rejects traversal/backslash, allows normal slugs.
	noFollow := &http.Client{
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	badRedirResp, _ := noFollow.Get(env.ts.URL + "/category/..%2Fevil")
	if badRedirResp.Body != nil {
		badRedirResp.Body.Close()
	}
	if badRedirResp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for traversal /category/..%%2Fevil, got %d", badRedirResp.StatusCode)
	}

	goodRedirResp, _ := noFollow.Get(env.ts.URL + "/category/golang.html?page=2")
	if goodRedirResp.Body != nil {
		goodRedirResp.Body.Close()
	}
	if goodRedirResp.StatusCode != http.StatusMovedPermanently || !bytes.Equal([]byte(goodRedirResp.Header.Get("Location")), []byte("/categories/golang?page=2")) {
		t.Fatalf("expected 301 -> /categories/golang?page=2, got %d %q", goodRedirResp.StatusCode, goodRedirResp.Header.Get("Location"))
	}

	badArtRedir, _ := noFollow.Get(env.ts.URL + "/articles/..%2Fevil")
	if badArtRedir.Body != nil {
		badArtRedir.Body.Close()
	}
	if badArtRedir.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for traversal /articles/..%%2Fevil, got %d", badArtRedir.StatusCode)
	}

	badArtSlashRedir, _ := noFollow.Get(env.ts.URL + "/articles/%5C%5Cevil.com")
	if badArtSlashRedir.Body != nil {
		badArtSlashRedir.Body.Close()
	}
	if badArtSlashRedir.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for backslash /articles/%%5C%%5Cevil.com, got %d", badArtSlashRedir.StatusCode)
	}

	goodArtRedir, _ := noFollow.Get(env.ts.URL + "/articles/2026/05/sec-test-post.html?page=2")
	if goodArtRedir.Body != nil {
		goodArtRedir.Body.Close()
	}
	if goodArtRedir.StatusCode != http.StatusMovedPermanently || goodArtRedir.Header.Get("Location") != "/posts/sec-test-post?page=2" {
		t.Fatalf("expected 301 -> /posts/sec-test-post?page=2, got %d %q", goodArtRedir.StatusCode, goodArtRedir.Header.Get("Location"))
	}
}

func TestSecurityHardening_SetupDBPostgresHostaddrSSRFBlocked(t *testing.T) {
	env, _ := newUninitializedWizardEnv(t)

	// Start a local TCP listener to verify setup endpoints never dial hostaddr.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	var dialed atomic.Int32
	go func() {
		for {
			c, aErr := ln.Accept()
			if aErr != nil {
				return
			}
			dialed.Add(1)
			_ = c.Close()
		}
	}()
	tcpPort := ln.Addr().(*net.TCPAddr).Port

	evilDSNs := []string{
		fmt.Sprintf("host=8.8.8.8 hostaddr=127.0.0.1 port=%d dbname=volo sslmode=disable", tcpPort),
		"host=127.0.0.1 port=5432 hostaddr=169.254.169.254 dbname=volo",
		"postgres://127.0.0.1:5432/volo?hostaddr=169.254.169.254",
		"postgres://127.0.0.1:5432/volo?%68ostaddr=169.254.169.254",
		"host=127.0.0.1 port=5432 HOSTADDR = '169.254.169.254' dbname=volo",
	}

	for _, dsn := range evilDSNs {
		if _, _, _, vErr := env.app.ValidateNormalizeAndPinSetupDBForTest(context.Background(), "postgres", dsn); vErr == nil {
			t.Fatalf("ValidateNormalizeAndPinSetupDB unexpectedly accepted hostaddr DSN %q", dsn)
		}
	}

	// Verify pinSetupDSNToValidatedIP unconditionally strips/overwrites any hostaddr.
	pinnedKW := server.PinSetupDSNToValidatedIPForTest("postgres", "host=8.8.8.8 hostaddr=169.254.169.254 port=5432 dbname=volo", netip.MustParseAddr("8.8.8.8"), 5432)
	if strings.Contains(pinnedKW, "169.254.169.254") || !strings.Contains(pinnedKW, "hostaddr=8.8.8.8") {
		t.Fatalf("expected keyword DSN hostaddr stripped and pinned to 8.8.8.8, got %q", pinnedKW)
	}
	pinnedURL := server.PinSetupDSNToValidatedIPForTest("postgres", "postgres://8.8.8.8:5432/volo?HOSTADDR=169.254.169.254", netip.MustParseAddr("8.8.8.8"), 5432)
	if strings.Contains(pinnedURL, "169.254.169.254") || !strings.Contains(pinnedURL, "hostaddr=8.8.8.8") {
		t.Fatalf("expected URL DSN hostaddr overwritten and pinned to 8.8.8.8, got %q", pinnedURL)
	}

	// Verify /api/setup/test-db and /api/setup/complete reject all hostaddr payloads with HTTP 400 and never dial.
	stResp, _ := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/setup/status", nil, nil, nil)
	var setupCSRF string
	for _, c := range stResp.Cookies() {
		if strings.Contains(c.Name, "csrf") {
			setupCSRF = c.Value
		}
	}
	headers := map[string]string{"X-CSRF-Token": setupCSRF}

	for _, dsn := range evilDSNs {
		testResp, testBody := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/setup/test-db", map[string]any{
			"driver": "postgres",
			"dsn":    dsn,
		}, stResp.Cookies(), headers)
		if testResp.StatusCode != http.StatusBadRequest {
			t.Fatalf("POST /api/setup/test-db expected 400 for %q, got %d (%s)", dsn, testResp.StatusCode, string(testBody))
		}

		compResp, compBody := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/setup/complete", map[string]any{
			"admin_username":     "secadmin",
			"admin_email":        "sec@example.com",
			"admin_password":     "pass1234",
			"admin_display_name": "Sec Admin",
			"site_title":         "Sec Test",
			"default_locale":     "en-US",
			"db_driver":          "postgres",
			"db_dsn":             dsn,
		}, stResp.Cookies(), headers)
		if compResp.StatusCode != http.StatusBadRequest {
			t.Fatalf("POST /api/setup/complete expected 400 for %q, got %d (%s)", dsn, compResp.StatusCode, string(compBody))
		}
	}

	if dialed.Load() != 0 {
		t.Fatalf("SSRF dial occurred despite hostaddr rejection: count=%d", dialed.Load())
	}
}
