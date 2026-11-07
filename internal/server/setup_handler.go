package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Willh92/Volo-go/internal/adminui"
	"github.com/Willh92/Volo-go/internal/auth"
	"github.com/Willh92/Volo-go/internal/db/sqlx"
	"github.com/Willh92/Volo-go/internal/domain"
	"github.com/Willh92/Volo-go/internal/etl"
	"github.com/Willh92/Volo-go/internal/security"
	"github.com/Willh92/Volo-go/pkg/id"
)

// isSetupUnlocked reports whether the first-boot Setup Wizard is still unlocked.
// Once `Site.Initialized == true` OR any `admin` user exists in `users`, `/setup` and `/api/setup/*`
// permanently return 404 Not Found so the wizard can NEVER be re-entered or probed!
func (s *AppServer) isSetupUnlocked(ctx context.Context) bool {
	if s.deps.Settings.Snapshot().Site.Initialized {
		return false
	}
	var adminCount int
	if err := s.deps.Store.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM users WHERE role = ?`, domain.RoleAdmin).Scan(&adminCount); err == nil && adminCount > 0 {
		return false
	}
	return true
}

func (s *AppServer) registerSetupRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/setup", s.handleSetupSPA)
	mux.HandleFunc("/setup/", s.handleSetupSPA)
	mux.HandleFunc("/api/setup/status", s.handleSetupStatus)
	mux.HandleFunc("/api/setup/test-db", s.handleSetupTestDB)
	mux.HandleFunc("/api/setup/import-legacy", s.handleSetupImportLegacy)
	mux.HandleFunc("/api/setup/complete", s.handleSetupComplete)
}

func (s *AppServer) handleSetupSPA(w http.ResponseWriter, r *http.Request) {
	if !s.isSetupUnlocked(r.Context()) {
		http.NotFound(w, r)
		return
	}
	snap := s.deps.Settings.Snapshot()
	adminui.Default().ServeMount(w, r, "/setup", adminui.BootConfig{
		Mode:          "setup",
		AdminPath:     s.deps.Config.AdminPath,
		PublicURL:     s.publicBaseURL(r),
		DefaultLocale: snap.Site.DefaultLocale,
		Accent:        snap.Appearance.Accent,
		DefaultTheme:  snap.Appearance.DefaultMode,
		SiteTitle:     snap.Site.Title,
		Locales:       s.deps.Catalog.Locales(),
	})
}

func (s *AppServer) handleSetupStatus(w http.ResponseWriter, r *http.Request) {
	if !s.isSetupUnlocked(r.Context()) {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	snap := s.deps.Settings.Snapshot()
	writeJSON(w, http.StatusOK, map[string]any{
		"initialized":       false,
		"current_driver":    s.deps.Store.Dialect().Name(),
		"default_locale":    snap.Site.DefaultLocale,
		"supported_locales": s.deps.Catalog.Locales(),
		"admin_path":        s.deps.Config.AdminPath,
		"timezone":          snap.Site.TimeZone,
		"accent":            snap.Appearance.Accent,
	})
}

func (s *AppServer) handleSetupTestDB(w http.ResponseWriter, r *http.Request) {
	if !s.isSetupUnlocked(r.Context()) {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	clientIP := security.ResolveClientIP(r, s.deps.Config.TrustedProxies)
	if !s.deps.WAF.AllowRate("setup:testdb", clientIP, 15, time.Minute) {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "rate limited"})
		return
	}

	var req struct {
		Driver         string `json:"driver"`
		DSN            string `json:"dsn"`
		SimpleProtocol bool   `json:"simple_protocol"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	driver := strings.ToLower(strings.TrimSpace(req.Driver))
	dsn := strings.TrimSpace(req.DSN)
	if driver == "" {
		driver = s.deps.Config.DBDriver
	}
	if dsn == "" && driver == s.deps.Config.DBDriver {
		dsn = s.deps.Config.DBDSN
	}

	testCtx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	start := time.Now()
	probeStore, err := sqlx.Open(testCtx, sqlx.Options{
		Driver:         driver,
		DSN:            dsn,
		SimpleProtocol: req.SimpleProtocol,
		MaxOpenConns:   2,
		MaxIdleConns:   1,
	})
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"ok":    false,
			"error": err.Error(),
		})
		return
	}
	defer probeStore.Close()

	var one int
	if err := probeStore.QueryRowContext(testCtx, "SELECT 1").Scan(&one); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"ok":    false,
			"error": err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"dialect":    probeStore.Dialect().Name(),
		"latency_ms": time.Since(start).Milliseconds(),
	})
}

func (s *AppServer) handleSetupImportLegacy(w http.ResponseWriter, r *http.Request) {
	if !s.isSetupUnlocked(r.Context()) {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	rep, dryRun, err := s.streamLegacyImportUpload(w, r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if dryRun {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":         true,
			"dry_run":    true,
			"report":     rep,
			"admin_path": s.deps.Config.AdminPath,
		})
		return
	}

	_, _ = s.deps.Settings.Reload(r.Context())
	_, _, _ = s.deps.Settings.PatchDomain(r.Context(), domain.SettingsDomainSite, []byte(`{"initialized":true}`), 0)
	_ = s.deps.Content.WarmCaches(r.Context())
	if s.deps.WAF != nil {
		_ = s.deps.WAF.ReloadBans(r.Context())
	}
	if s.deps.Geo != nil {
		_ = s.deps.Geo.ReloadRules(r.Context(), s.deps.Store)
	}

	resp := map[string]any{
		"ok":         true,
		"report":     rep,
		"admin_path": s.deps.Config.AdminPath,
	}
	// Automatically sign in the primary imported administrator so entering /console works seamlessly!
	if adminUser, qErr := sqlx.Get[domain.User](r.Context(), s.deps.Store,
		`SELECT * FROM users WHERE role = ? AND status = ? ORDER BY created_at ASC LIMIT 1`,
		domain.RoleAdmin, domain.UserStatusActive); qErr == nil && adminUser.ID != "" {
		clientIP := security.ResolveClientIP(r, s.deps.Config.TrustedProxies)
		geo := ""
		if s.deps.Geo != nil {
			geo = s.deps.Geo.LookupDisplay(clientIP)
		}
		if sess, rawTok, sErr := s.deps.Auth.CreateSession(r.Context(), adminUser.ID, clientIP, geo, r.UserAgent(), 0); sErr == nil {
			s.setSessionCookie(w, rawTok)
			adminUser.PasswordHash = ""
			adminUser.LegacyHash = ""
			resp["user"] = adminUser
			resp["csrf_token"] = s.deps.Auth.ComputeCSRFToken(sess.ID)
		}
	}

	writeJSON(w, http.StatusOK, resp)
}

func (s *AppServer) handleSetupComplete(w http.ResponseWriter, r *http.Request) {
	if !s.isSetupUnlocked(r.Context()) {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		SiteTitle        string `json:"site_title"`
		SiteSubtitle     string `json:"site_subtitle"`
		SiteDescription  string `json:"site_description"`
		DefaultLocale    string `json:"default_locale"`
		TimeZone         string `json:"timezone"`
		Accent           string `json:"accent"`
		AdminUsername    string `json:"admin_username"`
		AdminEmail       string `json:"admin_email"`
		AdminDisplayName string `json:"admin_display_name"`
		AdminPassword    string `json:"admin_password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32768)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}

	uname := strings.TrimSpace(req.AdminUsername)
	email := strings.ToLower(strings.TrimSpace(req.AdminEmail))
	if len(uname) < 2 || len(uname) > 48 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "admin_username must be 2-48 characters"})
		return
	}
	if !strings.Contains(email, "@") || len(email) > 190 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "valid admin_email is required"})
		return
	}
	if err := auth.ValidatePasswordPolicy(req.AdminPassword); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	pwHash, err := auth.HashPassword(req.AdminPassword)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "password hashing failed"})
		return
	}

	locale := s.deps.Catalog.Match(req.DefaultLocale, "")
	isZH := strings.HasPrefix(strings.ToLower(locale), "zh")
	displayName := strings.TrimSpace(req.AdminDisplayName)
	if displayName == "" {
		displayName = uname
	}
	siteTitle := strings.TrimSpace(req.SiteTitle)
	if siteTitle == "" {
		if isZH {
			siteTitle = "Volo 随笔"
		} else {
			siteTitle = "Volo Blog"
		}
	}
	siteSubtitle := strings.TrimSpace(req.SiteSubtitle)
	if siteSubtitle == "" {
		if isZH {
			siteSubtitle = "独立思考与工程手艺"
		} else {
			siteSubtitle = "Independent thoughts & craft"
		}
	}
	tz := strings.TrimSpace(req.TimeZone)
	if tz == "" {
		tz = "Asia/Shanghai"
	}
	accent := strings.ToLower(strings.TrimSpace(req.Accent))
	switch accent {
	case "forest", "ocean", "teal", "amber", "plum", "rose":
	default:
		accent = "forest"
	}

	now := time.Now().UnixMilli()
	adminUser := domain.User{
		ID:              id.Next(),
		Username:        uname,
		Email:           email,
		DisplayName:     displayName,
		AvatarURL:       "/avatars/" + uname + ".svg",
		Role:            domain.RoleAdmin,
		PasswordHash:    pwHash,
		Status:          domain.UserStatusActive,
		EmailVerifiedAt: now,
		PreferredLocale: locale,
		CreatedAt:       now,
		UpdatedAt:       now,
	}

	// Atomically ensure zero race condition on initial admin creation + starter About page!
	err = s.deps.Store.WithTx(r.Context(), func(tx *sqlx.Tx) error {
		var cnt int
		if qErr := tx.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM users WHERE role = ?`, domain.RoleAdmin).Scan(&cnt); qErr != nil {
			return qErr
		}
		if cnt > 0 {
			return fmt.Errorf("setup already completed")
		}
		if _, iErr := tx.ExecContext(r.Context(),
			`INSERT INTO users (id, username, email, display_name, avatar_url, bio, website_url, role, password_hash, legacy_hash, status, email_verified_at, preferred_locale, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, '', '', ?, ?, '', ?, ?, ?, ?, ?)`,
			adminUser.ID, adminUser.Username, adminUser.Email, adminUser.DisplayName, adminUser.AvatarURL,
			adminUser.Role, adminUser.PasswordHash, adminUser.Status, adminUser.EmailVerifiedAt, adminUser.PreferredLocale,
			adminUser.CreatedAt, adminUser.UpdatedAt,
		); iErr != nil {
			return iErr
		}

		// Seed default starter category & "/about" standalone page if empty
		var catCnt int
		_ = tx.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM categories`).Scan(&catCnt)
		if catCnt == 0 {
			catID := id.Next()
			catName := "Notes"
			catDesc := "Essays & engineering notes"
			if isZH {
				catName = "随笔"
				catDesc = "独立思考与工程手记"
			}
			_, _ = tx.ExecContext(r.Context(),
				`INSERT INTO categories (id, name, slug, description, icon, sort_order, created_at, updated_at)
				 VALUES (?, ?, 'notes', ?, '', 1, ?, ?)`,
				catID, catName, catDesc, now, now)
		}
		var pageCnt int
		_ = tx.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM pages`).Scan(&pageCnt)
		if pageCnt == 0 {
			pageID := id.Next()
			aboutTitle := "About"
			aboutBody := "# About\n\nWelcome to **" + siteTitle + "**, powered by Volo-go."
			if isZH {
				aboutTitle = "关于"
				aboutBody = "# 关于\n\n欢迎来到 **" + siteTitle + "**，本站由纯 Go 单文件独立博客引擎 Volo-go 驱动。"
			}
			_, _ = tx.ExecContext(r.Context(),
				`INSERT INTO pages (id, author_id, title, slug, content_md, is_published, allow_comment, show_toc, lang, translation_group, view_count, comment_count, created_at, updated_at)
				 VALUES (?, ?, ?, 'about', ?, 1, 1, 0, ?, '', 0, 0, ?, ?)`,
				pageID, adminUser.ID, aboutTitle, aboutBody, locale, now, now)
			_, _ = tx.ExecContext(r.Context(),
				`INSERT INTO nav_items (id, parent_id, title, i18n_titles_json, url, open_in_new_tab, icon, sort_order, is_enabled, created_at, updated_at)
				 VALUES (?, '', ?, '{"zh-CN":"关于","en-US":"About"}', '/about', 0, '', 10, 1, ?, ?)`,
				id.Next(), aboutTitle, now, now)
		}
		return nil
	})
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}

	sitePatch, _ := json.Marshal(map[string]any{
		"initialized":    true,
		"title":          siteTitle,
		"subtitle":       siteSubtitle,
		"description":    strings.TrimSpace(req.SiteDescription),
		"default_locale": locale,
		"timezone":       tz,
	})
	_, _, _ = s.deps.Settings.PatchDomain(r.Context(), domain.SettingsDomainSite, sitePatch, 0)

	appPatch, _ := json.Marshal(map[string]any{"accent": accent})
	_, _, _ = s.deps.Settings.PatchDomain(r.Context(), domain.SettingsDomainAppearance, appPatch, 0)
	_ = s.deps.Content.WarmCaches(r.Context())

	clientIP := security.ResolveClientIP(r, s.deps.Config.TrustedProxies)
	geo := ""
	if s.deps.Geo != nil {
		geo = s.deps.Geo.LookupDisplay(clientIP)
	}
	sess, rawTok, err := s.deps.Auth.CreateSession(r.Context(), adminUser.ID, clientIP, geo, r.UserAgent(), 0)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.setSessionCookie(w, rawTok)
	adminUser.PasswordHash = ""

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"admin_path": s.deps.Config.AdminPath,
		"user":       adminUser,
		"csrf_token": s.deps.Auth.ComputeCSRFToken(sess.ID),
	})
}

func (s *AppServer) streamLegacyImportUpload(w http.ResponseWriter, r *http.Request) (*etl.ImportReport, bool, error) {
	const maxLegacyBytes = 256 << 20 // 256 MiB streaming cap
	r.Body = http.MaxBytesReader(w, r.Body, maxLegacyBytes)
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		return nil, false, fmt.Errorf("upload parse error: %w", err)
	}
	file, hdr, err := r.FormFile("file")
	if err != nil {
		return nil, false, fmt.Errorf("missing multipart 'file' field")
	}
	defer file.Close()

	ext := strings.ToLower(filepath.Ext(hdr.Filename))
	if ext != ".zip" && ext != ".json" {
		ext = ".zip"
	}
	tmpFile, err := os.CreateTemp(s.deps.Config.DataDir, "volo-legacy-*"+ext)
	if err != nil {
		return nil, false, err
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	if _, err := io.Copy(tmpFile, io.LimitReader(file, maxLegacyBytes)); err != nil {
		_ = tmpFile.Close()
		return nil, false, err
	}
	_ = tmpFile.Close()

	dryRun := strings.EqualFold(r.FormValue("dry_run"), "true") || r.FormValue("dry_run") == "1"
	rep, err := etl.ImportLegacyArchive(r.Context(), s.deps.Store, s.deps.Settings, s.deps.Config.KeyRing, tmpPath, etl.ImportOptions{
		DryRun:          dryRun,
		TargetUploadDir: s.deps.Config.UploadDir,
	})
	if err != nil {
		return nil, dryRun, err
	}
	return rep, dryRun, nil
}
