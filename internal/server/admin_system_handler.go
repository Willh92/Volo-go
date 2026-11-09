package server

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Willh92/Volo-go/internal/auth"
	"github.com/Willh92/Volo-go/internal/backup"
	"github.com/Willh92/Volo-go/internal/config"
	"github.com/Willh92/Volo-go/internal/content"
	"github.com/Willh92/Volo-go/internal/db/sqlx"
	"github.com/Willh92/Volo-go/internal/doctor"
	"github.com/Willh92/Volo-go/internal/domain"
	"github.com/Willh92/Volo-go/internal/etl"
	"github.com/Willh92/Volo-go/internal/logging"
	"github.com/Willh92/Volo-go/internal/repo"
	"github.com/Willh92/Volo-go/internal/security"
	"github.com/Willh92/Volo-go/internal/theme"
	"github.com/Willh92/Volo-go/internal/tinyxml"
	"github.com/Willh92/Volo-go/pkg/id"
)

func (s *AppServer) commentInputFromStaff(actor *domain.User, parent domain.Comment, md string, clientIP string) content.CreateCommentInput {
	return content.CreateCommentInput{
		TargetType: parent.TargetType,
		TargetID:   parent.TargetID,
		ParentID:   parent.ID,
		ContentMD:  md,
		ClientIP:   clientIP,
		Locale:     actor.PreferredLocale,
	}
}

func (s *AppServer) recordAuditLog(r *http.Request, actor *domain.User, action string, resourceType string, resourceID string, detail map[string]any) {
	if s.deps.Store == nil || actor == nil {
		return
	}
	clientIP := s.clientIP(r)
	detJSON := "{}"
	if detail != nil {
		if b, err := json.Marshal(detail); err == nil {
			detJSON = string(b)
		}
	}
	_, _ = s.deps.Store.ExecBgContext(r.Context(),
		`INSERT INTO audit_logs (id, actor_id, actor_name, action, resource_type, resource_id, client_ip, detail_json, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id.Next(), actor.ID, actor.Username, action, resourceType, resourceID, security.MaskIP(clientIP), detJSON, time.Now().UnixMilli())
	s.auditActionsMu.Lock()
	s.auditActionsExp = time.Time{}
	s.auditActionsMu.Unlock()
}

func (s *AppServer) listDistinctAuditActions(ctx context.Context) []string {
	now := time.Now()
	s.auditActionsMu.Lock()
	if s.auditActions != nil && now.Before(s.auditActionsExp) {
		out := make([]string, len(s.auditActions))
		copy(out, s.auditActions)
		s.auditActionsMu.Unlock()
		return out
	}
	s.auditActionsMu.Unlock()

	var actionRows []struct {
		Action string `db:"action"`
	}
	err := sqlx.SelectInto(ctx, s.deps.Store, &actionRows, `SELECT DISTINCT action FROM audit_logs WHERE action <> '' ORDER BY action ASC`)
	actions := make([]string, 0, len(actionRows))
	for _, ar := range actionRows {
		actions = append(actions, ar.Action)
	}
	if err == nil {
		s.auditActionsMu.Lock()
		s.auditActions = actions
		s.auditActionsExp = now.Add(60 * time.Second)
		s.auditActionsMu.Unlock()
	}
	return actions
}

func parseAdminPageParams(r *http.Request, defaultPerPage, maxPerPage int) (int, int) {
	page := 1
	if p, err := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("page"))); err == nil && p > 0 {
		page = p
	}
	perPage := defaultPerPage
	rawPP := strings.TrimSpace(r.URL.Query().Get("per_page"))
	if rawPP == "" {
		rawPP = strings.TrimSpace(r.URL.Query().Get("page_size"))
	}
	if pp, err := strconv.Atoi(rawPP); err == nil && pp > 0 {
		perPage = pp
	}
	if maxPerPage > 0 && perPage > maxPerPage {
		perPage = maxPerPage
	}
	return page, perPage
}

func (s *AppServer) resolveBackupDir() string {
	if s.deps.Config != nil && strings.TrimSpace(s.deps.Config.BackupDir) != "" {
		return strings.TrimSpace(s.deps.Config.BackupDir)
	}
	if s.deps.Config != nil && strings.TrimSpace(s.deps.Config.DataDir) != "" {
		return filepath.Join(s.deps.Config.DataDir, "backups")
	}
	return "backups"
}

func (s *AppServer) mintBackupExportTicket(userID string, sessionID string, ipBucket string, backupName string) (string, error) {
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(sessionID) == "" || strings.TrimSpace(ipBucket) == "" {
		return "", errors.New("interactive admin session required for backup ticket")
	}
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(raw[:])
	now := time.Now()
	s.backupTicketMu.Lock()
	defer s.backupTicketMu.Unlock()
	if s.backupTickets == nil {
		s.backupTickets = make(map[string]backupExportTicket)
	}
	for k, v := range s.backupTickets {
		if now.After(v.ExpiresAt) {
			delete(s.backupTickets, k)
		}
	}
	s.backupTickets[tok] = backupExportTicket{
		UserID:     userID,
		SessionID:  sessionID,
		IPBucket:   ipBucket,
		BackupName: strings.TrimSpace(backupName),
		ExpiresAt:  now.Add(60 * time.Second),
	}
	return tok, nil
}

func (s *AppServer) consumeBackupExportTicket(r *http.Request, rawTicket string, expectedBackupName string) (*domain.User, bool) {
	tok := strings.TrimSpace(rawTicket)
	if len(tok) != 64 || s.deps.Store == nil {
		return nil, false
	}
	viewer, sess := s.resolveViewer(r)
	if viewer == nil || sess == nil || sess.ID == "" || viewer.Status != domain.UserStatusActive || viewer.Role != domain.RoleAdmin {
		return nil, false
	}
	ipBucket := security.RateLimitBucketKey(s.clientIP(r))
	if ipBucket == "" {
		return nil, false
	}
	now := time.Now()
	s.backupTicketMu.Lock()
	for k, v := range s.backupTickets {
		if now.After(v.ExpiresAt) {
			delete(s.backupTickets, k)
		}
	}
	entry, exists := s.backupTickets[tok]
	if exists {
		delete(s.backupTickets, tok)
	}
	s.backupTicketMu.Unlock()
	if !exists || now.After(entry.ExpiresAt) {
		return nil, false
	}
	if entry.UserID != viewer.ID || entry.SessionID != sess.ID || entry.IPBucket != ipBucket {
		return nil, false
	}
	if entry.BackupName != strings.TrimSpace(expectedBackupName) {
		return nil, false
	}
	var u domain.User
	err := sqlx.GetInto(r.Context(), s.deps.Store, &u,
		`SELECT * FROM users WHERE id = ? AND status = ? AND role = ?`,
		entry.UserID, domain.UserStatusActive, domain.RoleAdmin)
	if err != nil {
		return nil, false
	}
	return &u, true
}

func sanitizeAdminIconField(raw string) (string, bool) {
	clean := strings.TrimSpace(repo.StripASCIIControls(raw))
	if clean == "" {
		return "", true
	}
	if strings.Contains(clean, "/") || strings.Contains(clean, ":") {
		u, ok := repo.SanitizeAllowedURL(clean, false)
		if !ok || u == "" {
			return "", false
		}
		lower := strings.ToLower(u)
		if strings.HasPrefix(u, "/") || strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") {
			return u, true
		}
		return "", false
	}
	return clean, true
}

func sanitizeRedirectTargetURL(raw string) (string, bool) {
	u, ok := repo.SanitizeAllowedURL(raw, false)
	if !ok || u == "" {
		return "", false
	}
	lower := strings.ToLower(u)
	if strings.HasPrefix(u, "/") || strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") {
		return u, true
	}
	return "", false
}

// handleAdminSystemRoutes serves Workspaces 3, 4, and 5.
// SECURITY RED-LINE: All System, Settings, Appearance/Plugins, Users, WAF/GeoIP, and Backup routes
// require `RoleAdmin` (`Editor` physically receives `403 Forbidden`)!
func (s *AppServer) handleAdminSystemRoutes(w http.ResponseWriter, r *http.Request, sub string) {
	if sub == "backup/export" || sub == "backups/export" || sub == "backup/download" || sub == "backups/download" {
		if ticket := strings.TrimSpace(r.URL.Query().Get("ticket")); ticket != "" {
			if r.Method != http.MethodGet && r.Method != http.MethodPost {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			actor, ok := s.consumeBackupExportTicket(r, ticket, "")
			if !ok {
				writeErrJSON(w, http.StatusUnauthorized, "invalid or expired backup export ticket")
				return
			}
			s.handleAdminBackupExport(w, r, actor)
			return
		}
		actor, sess, ok := s.authenticateAdminRequest(w, r, auth.ScopeBackupRead, true)
		if !ok {
			return
		}
		if sess != nil {
			writeErrJSON(w, http.StatusForbidden, "step-up password ticket required for interactive backup export")
			return
		}
		s.handleAdminBackupExport(w, r, actor)
		return
	}

	if (strings.HasPrefix(sub, "backup/local/") || strings.HasPrefix(sub, "backups/local/")) && r.Method == http.MethodGet {
		localRest := strings.TrimPrefix(sub, "backups/local/")
		if localRest == sub {
			localRest = strings.TrimPrefix(sub, "backup/local/")
		}
		if localRest != "" && !strings.Contains(localRest, "/") {
			base := filepath.Base(localRest)
			if base != localRest || strings.Contains(base, "..") || !strings.HasSuffix(strings.ToLower(base), ".zip") {
				writeErrJSON(w, http.StatusBadRequest, "invalid backup filename")
				return
			}
			if ticket := strings.TrimSpace(r.URL.Query().Get("ticket")); ticket != "" {
				actor, ok := s.consumeBackupExportTicket(r, ticket, base)
				if !ok {
					writeErrJSON(w, http.StatusUnauthorized, "invalid or expired backup download ticket")
					return
				}
				s.serveLocalBackupArchive(w, r, actor, base)
				return
			}
			actor, sess, ok := s.authenticateAdminRequest(w, r, auth.ScopeBackupRead, true)
			if !ok {
				return
			}
			if sess != nil {
				writeErrJSON(w, http.StatusForbidden, "step-up password ticket required for interactive local backup download")
				return
			}
			s.serveLocalBackupArchive(w, r, actor, base)
			return
		}
	}

	actor, sess, ok := s.authenticateAdminRequest(w, r, "", true)
	if !ok {
		return
	}

	switch {
	case sub == "nav" || strings.HasPrefix(sub, "nav/"):
		s.handleAdminNav(w, r, actor, strings.TrimPrefix(sub, "nav"))

	case sub == "links" || strings.HasPrefix(sub, "links/"):
		s.handleAdminLinks(w, r, actor, strings.TrimPrefix(sub, "links"))

	case sub == "follows" || strings.HasPrefix(sub, "follows/"):
		s.handleAdminFollows(w, r, actor, strings.TrimPrefix(sub, "follows"))

	case sub == "redirects" || strings.HasPrefix(sub, "redirects/"):
		s.handleAdminRedirects(w, r, actor, strings.TrimPrefix(sub, "redirects"))

	case sub == "webhooks" || strings.HasPrefix(sub, "webhooks/"):
		s.handleAdminWebhooks(w, r, actor, strings.TrimPrefix(sub, "webhooks"))

	case sub == "seo/github-sync" && r.Method == http.MethodPost:
		var req struct {
			Username       string `json:"username"`
			IncludeForks   *bool  `json:"include_forks"`
			PublishArticle bool   `json:"publish_article"`
		}
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req)
		if req.IncludeForks != nil {
			patchForks, _ := json.Marshal(map[string]any{"github_include_forks": *req.IncludeForks})
			_, _, _ = s.deps.Settings.PatchDomain(r.Context(), domain.SettingsDomainSEO, patchForks, 0)
		}
		repos, err := s.refreshGitHubRepositories(r.Context(), req.Username)
		if writeBadErr(w, err) {
			return
		}
		resp := map[string]any{
			"repos":            repos,
			"github_synced_at": s.deps.Settings.Snapshot().SEO.GitHubSyncedAt,
		}
		if req.PublishArticle {
			if art, pErr := s.publishGitHubShowcaseArticle(r.Context(), actor); pErr == nil && art != nil {
				resp["article_slug"] = art.Slug
				resp["article_id"] = art.ID
			}
		}
		writeJSON(w, http.StatusOK, resp)

	case sub == "seo/github-publish" && r.Method == http.MethodPost:
		art, err := s.publishGitHubShowcaseArticle(r.Context(), actor)
		if writeBadErr(w, err) {
			return
		}
		s.recordAuditLog(r, actor, "github.publish_showcase", "article", art.ID, map[string]any{"slug": art.Slug})
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":           true,
			"article_id":   art.ID,
			"article_slug": art.Slug,
			"article_url":  s.deps.Content.Guard().ArticleURL(*art),
		})

	case sub == "seo/indexnow-push" && r.Method == http.MethodPost:
		n, err := s.pushIndexNowURLs(r.Context(), s.publicBaseURL(r))
		if writeBadErr(w, err) {
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"pushed_urls": n})

	case sub == "themes" || strings.HasPrefix(sub, "themes/"):
		s.handleAdminThemes(w, r, actor, strings.TrimPrefix(sub, "themes"))

	case sub == "locales" || strings.HasPrefix(sub, "locales/"):
		s.handleAdminLocales(w, r, actor, strings.TrimPrefix(sub, "locales"))

	case sub == "plugins" || strings.HasPrefix(sub, "plugins/"):
		s.handleAdminPlugins(w, r, actor, strings.TrimPrefix(sub, "plugins"))

	case sub == "settings" || strings.HasPrefix(sub, "settings/"):
		s.handleAdminSettings(w, r, actor, strings.TrimPrefix(sub, "settings"))

	case sub == "users" || strings.HasPrefix(sub, "users/"):
		s.handleAdminUsers(w, r, actor, strings.TrimPrefix(sub, "users"))

	case sub == "security/rotate-share-secret" && r.Method == http.MethodPost:
		if _, err := s.deps.Settings.RotateShareTokenSecret(r.Context()); err != nil {
			writeErrJSON(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.recordAuditLog(r, actor, "security.rotate_share_secret", "settings", "security", nil)
		writeOKJSON(w)

	case sub == "security/ip-bans" || strings.HasPrefix(sub, "security/ip-bans/"):
		s.handleAdminIPBans(w, r, actor, strings.TrimPrefix(sub, "security/ip-bans"))

	case sub == "security/geo-rules" || strings.HasPrefix(sub, "security/geo-rules/"):
		s.handleAdminGeoRules(w, r, actor, strings.TrimPrefix(sub, "security/geo-rules"))

	case sub == "runtime-logs" && r.Method == http.MethodGet:
		lvl := strings.TrimSpace(r.URL.Query().Get("level"))
		q := strings.TrimSpace(r.URL.Query().Get("q"))
		limit, _ := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("limit")))
		if limit <= 0 || limit > logging.DefaultMaxEntries {
			limit = logging.DefaultMaxEntries
		}
		items := logging.Default().List(lvl, q, limit)
		if strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("format")), "text") {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Header().Set("Content-Disposition", `attachment; filename="volo-runtime.log"`)
			_, _ = w.Write([]byte(logging.FormatText(items)))
			return
		}
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		writeJSON(w, http.StatusOK, map[string]any{
			"items":            items,
			"total":            logging.Default().Count(),
			"capacity":         logging.DefaultMaxEntries,
			"goroutines":       runtime.NumGoroutine(),
			"heap_alloc_bytes": ms.HeapAlloc,
			"sys_bytes":        ms.Sys,
			"uptime_sec":       int64(time.Since(s.deps.StartedAt).Seconds()),
			"go_version":       runtime.Version(),
			"version":          s.deps.Version,
		})

	case sub == "runtime-logs" && r.Method == http.MethodDelete:
		logging.Default().Clear()
		s.recordAuditLog(r, actor, "system.clear_runtime_logs", "system", "runtime_logs", nil)
		writeOKJSON(w)

	case sub == "audit-logs" && r.Method == http.MethodGet:
		page, perPage := parseAdminPageParams(r, 50, 200)
		act := strings.TrimSpace(r.URL.Query().Get("action"))
		q := strings.TrimSpace(r.URL.Query().Get("q"))
		where := []string{"1=1"}
		var args []any
		if act != "" && act != "all" {
			where = append(where, "action = ?")
			args = append(args, act)
		}
		if q != "" {
			pat := "%" + escapeLikePattern(q) + "%"
			where = append(where, "(LOWER(actor_name) LIKE LOWER(?) ESCAPE '!' OR LOWER(action) LIKE LOWER(?) ESCAPE '!' OR LOWER(resource_type) LIKE LOWER(?) ESCAPE '!' OR LOWER(resource_id) LIKE LOWER(?) ESCAPE '!' OR LOWER(detail_json) LIKE LOWER(?) ESCAPE '!' OR LOWER(client_ip) LIKE LOWER(?) ESCAPE '!')")
			args = append(args, pat, pat, pat, pat, pat, pat)
		}
		whereSQL := strings.Join(where, " AND ")
		var total int
		_ = s.deps.Store.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM audit_logs WHERE `+whereSQL, args...).Scan(&total)
		baseSQL := `SELECT * FROM audit_logs WHERE ` + whereSQL + ` ORDER BY created_at DESC`
		qSQL, fullArgs := s.paginateQuery(baseSQL, args, page, perPage)
		var rows []domain.AuditLog
		_ = sqlx.SelectInto(r.Context(), s.deps.Store, &rows, qSQL, fullArgs...)
		if rows == nil {
			rows = []domain.AuditLog{}
		}
		actions := s.listDistinctAuditActions(r.Context())
		writeJSON(w, http.StatusOK, map[string]any{
			"items":     rows,
			"logs":      rows,
			"actions":   actions,
			"total":     total,
			"page":      page,
			"per_page":  perPage,
			"page_size": perPage,
		})

	case (sub == "backup/ticket" || sub == "backups/ticket") && r.Method == http.MethodPost:
		if sess == nil || sess.ID == "" {
			writeErrJSON(w, http.StatusForbidden, s.deps.Catalog.T(s.resolveLocale(r), "error.forbidden"))
			return
		}
		var req struct {
			CurrentPassword string `json:"current_password"`
			IncludeMedia    bool   `json:"include_media"`
		}
		if !decodeOptJSONBody(w, r, 4096, &req) {
			return
		}
		if !s.verifyStepUpPassword(w, r, actor.ID, req.CurrentPassword) {
			return
		}
		ipBucket := security.RateLimitBucketKey(s.clientIP(r))
		tok, err := s.mintBackupExportTicket(actor.ID, sess.ID, ipBucket, "")
		if writeServerErr(w, err) {
			return
		}
		mediaQuery := ""
		if req.IncludeMedia {
			mediaQuery = "&media=1"
		}
		s.recordAuditLog(r, actor, "backup.export_ticket", "system", "ticket", map[string]any{"include_media": req.IncludeMedia})
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":           true,
			"ticket":       tok,
			"expires_in":   60,
			"export_url":   "/api/admin/backups/export?ticket=" + tok + mediaQuery,
			"download_url": "/api/admin/backups/download?ticket=" + tok + mediaQuery,
		})

	case sub == "backup/local" || strings.HasPrefix(sub, "backup/local/"):
		s.handleAdminLocalBackups(w, r, actor, sess, strings.TrimPrefix(sub, "backup/local"))

	case sub == "backups/local" || strings.HasPrefix(sub, "backups/local/"):
		s.handleAdminLocalBackups(w, r, actor, sess, strings.TrimPrefix(sub, "backups/local"))

	case (sub == "backup/restore" || sub == "backups/restore") && r.Method == http.MethodPost:
		s.handleAdminBackupRestore(w, r, actor)

	case (sub == "backup/import-legacy" || sub == "backups/import-legacy") && r.Method == http.MethodPost:
		s.handleAdminImportLegacy(w, r, actor)

	case sub == "doctor" && r.Method == http.MethodGet:
		rep := doctor.Run(r.Context(), s.deps.Config, s.deps.Store)
		writeJSON(w, http.StatusOK, rep)

	default:
		http.NotFound(w, r)
	}
}

func (s *AppServer) handleAdminImportLegacy(w http.ResponseWriter, r *http.Request, actor *domain.User) {
	if !s.requireSameOrigin(w, r) {
		return
	}
	prep, err := s.spoolLegacyImportUpload(w, r)
	if writeBadErr(w, err) {
		return
	}
	defer os.Remove(prep.tmpPath)

	// Production import-legacy on an already-initialized site NEVER permits DB swaps,
	// admin credential overrides, or canonical PublicURL mutation.
	prep.rawDriver = ""
	prep.rawDSN = ""
	prep.adminUsername = ""
	prep.adminEmail = ""
	prep.adminDisplayName = ""
	prep.adminPasswordHash = ""
	prep.newSiteURL = ""

	if !prep.dryRun {
		if !s.verifyStepUpPassword(w, r, actor.ID, prep.currentPassword) {
			return
		}
	}

	rep, err := s.executeLegacyImport(r.Context(), prep, false)
	if writeBadErr(w, err) {
		return
	}
	if !prep.dryRun {
		s.reloadRuntimeAfterDBRestore(r.Context())
		s.recordAuditLog(r, actor, "backup.import_legacy", "system", "etl", nil)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "dry_run": prep.dryRun, "report": rep})
}

func (s *AppServer) handleAdminOIDCLinks(w http.ResponseWriter, r *http.Request, actor *domain.User, sess *domain.UserSession, rest string) {
	rest = strings.TrimPrefix(rest, "/")
	if rest == "" {
		if !requireMethod(w, r, http.MethodGet) {
			return
		}
		type oidcLinkDTO struct {
			ID           string `db:"id" json:"id"`
			Name         string `db:"name" json:"name"`
			CredentialID string `db:"credential_id" json:"credential_id"`
			CreatedAt    int64  `db:"created_at" json:"created_at"`
			LastUsedAt   int64  `db:"last_used_at" json:"last_used_at"`
		}
		var rows []oidcLinkDTO
		_ = sqlx.SelectInto(r.Context(), s.deps.Store, &rows,
			`SELECT id, name, credential_id, created_at, last_used_at
			 FROM user_credentials WHERE user_id = ? AND kind = 'oidc' ORDER BY created_at DESC`,
			actor.ID)
		if rows == nil {
			rows = []oidcLinkDTO{}
		}
		providers := make([]map[string]string, 0)
		if s.deps.Plugins != nil {
			loc := s.resolveLocale(r)
			for _, p := range s.deps.Plugins.Providers().OIDCProviders() {
				disp := strings.TrimSpace(p.DisplayName(loc))
				if disp == "" {
					disp = p.Name()
				}
				providers = append(providers, map[string]string{
					"name":         p.Name(),
					"display_name": disp,
				})
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"items":     rows,
			"providers": providers,
		})
		return
	}

	if rest == "begin" {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		if !s.requireSameOrigin(w, r) {
			return
		}
		if sess == nil || sess.ID == "" {
			writeErrJSON(w, http.StatusForbidden, s.deps.Catalog.T(s.resolveLocale(r), "error.forbidden"))
			return
		}
		var req struct {
			Provider        string `json:"provider"`
			Next            string `json:"next"`
			CurrentPassword string `json:"current_password"`
		}
		if !decodeJSONBody(w, r, 4096, &req) {
			return
		}
		provName := strings.TrimSpace(req.Provider)
		if provName == "" || s.deps.Plugins == nil {
			writeErrJSON(w, http.StatusBadRequest, "oidc provider is required")
			return
		}
		prov, ok := s.deps.Plugins.Providers().OIDC(provName)
		if !ok || prov == nil {
			writeErrJSON(w, http.StatusNotFound, "unknown oidc provider")
			return
		}
		if !s.verifyStepUpPassword(w, r, actor.ID, req.CurrentPassword) {
			return
		}
		tok, err := s.mintOIDCBindTicket(actor.ID, sess.ID, prov.Name())
		if writeServerErr(w, err) {
			return
		}
		redirectURL := "/api/v1/auth/oidc/" + url.PathEscape(prov.Name()) + "/start?target=bind&bind_ticket=" + url.QueryEscape(tok)
		if rawNext := strings.TrimSpace(req.Next); rawNext != "" {
			if !isSafeRelativeRedirect(rawNext) {
				writeErrJSON(w, http.StatusBadRequest, "invalid next redirect path")
				return
			}
			redirectURL += "&next=" + url.QueryEscape(rawNext)
		}
		s.recordAuditLog(r, actor, "security.oidc_bind_begin", "user_credential", prov.Name(), nil)
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":           true,
			"bind_ticket":  tok,
			"redirect_url": redirectURL,
		})
		return
	}

	if r.Method == http.MethodDelete {
		if sess == nil || sess.ID == "" {
			writeErrJSON(w, http.StatusForbidden, s.deps.Catalog.T(s.resolveLocale(r), "error.forbidden"))
			return
		}
		var req struct {
			CurrentPassword string `json:"current_password"`
		}
		if !decodeOptJSONBody(w, r, 4096, &req) {
			return
		}
		if !s.verifyStepUpPassword(w, r, actor.ID, req.CurrentPassword) {
			return
		}
		res, err := s.deps.Store.ExecContext(r.Context(),
			`DELETE FROM user_credentials WHERE id = ? AND user_id = ? AND kind = 'oidc'`,
			rest, actor.ID)
		if writeServerErr(w, err) {
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			writeErrJSON(w, http.StatusNotFound, "oidc link not found")
			return
		}
		s.recordAuditLog(r, actor, "security.oidc_unlink", "user_credential", rest, nil)
		writeOKJSON(w)
		return
	}

	w.WriteHeader(http.StatusMethodNotAllowed)
}

func (s *AppServer) handleAdminNav(w http.ResponseWriter, r *http.Request, _ *domain.User, rest string) {
	rest = strings.TrimPrefix(rest, "/")
	if rest == "" {
		switch r.Method {
		case http.MethodGet:
			var rows []domain.NavItem
			_ = sqlx.SelectInto(r.Context(), s.deps.Store, &rows, `SELECT * FROM nav_items ORDER BY sort_order ASC, created_at ASC`)
			if rows == nil {
				rows = []domain.NavItem{}
			}
			writeJSON(w, http.StatusOK, map[string]any{"items": rows, "nav_items": rows, "total": len(rows)})
		case http.MethodPost:
			var in domain.NavItem
			if !decodeJSONBody(w, r, 16384, &in) {
				return
			}
			in.ID = id.Next()
			in.ParentID = strings.TrimSpace(in.ParentID)
			in.Title = strings.TrimSpace(in.Title)
			cleanURL, urlOK := repo.SanitizeAllowedURL(in.URL, true)
			if in.Title == "" || !urlOK || cleanURL == "" {
				writeErrJSON(w, http.StatusBadRequest, "valid title and allowed url required")
				return
			}
			cleanIcon, iconOK := sanitizeAdminIconField(in.Icon)
			if !iconOK {
				writeErrJSON(w, http.StatusBadRequest, "invalid icon URL scheme")
				return
			}
			in.URL = cleanURL
			in.Icon = cleanIcon
			if strings.TrimSpace(in.I18nTitlesJSON) == "" {
				in.I18nTitlesJSON = "{}"
			}
			if in.SortOrder <= 0 {
				_ = s.deps.Store.QueryRowContext(r.Context(),
					`SELECT COALESCE(MAX(sort_order), 0) + 1 FROM nav_items WHERE parent_id = ?`,
					in.ParentID).Scan(&in.SortOrder)
				if in.SortOrder <= 0 {
					in.SortOrder = 1
				}
			}
			now := time.Now().UnixMilli()
			in.CreatedAt = now
			in.UpdatedAt = now
			_, err := s.deps.Store.ExecContext(r.Context(),
				`INSERT INTO nav_items (id, parent_id, title, i18n_titles_json, url, open_in_new_tab, icon, sort_order, is_enabled, created_at, updated_at)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				in.ID, in.ParentID, in.Title, in.I18nTitlesJSON, in.URL, in.OpenInNewTab, in.Icon, in.SortOrder, in.IsEnabled, now, now)
			if writeBadErr(w, err) {
				return
			}
			s.invalidateMediaRefCache()
			writeJSON(w, http.StatusCreated, map[string]any{"item": in})
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
		return
	}
	switch r.Method {
	case http.MethodPut:
		var in domain.NavItem
		if !decodeJSONBody(w, r, 16384, &in) {
			return
		}
		title := strings.TrimSpace(in.Title)
		cleanURL, urlOK := repo.SanitizeAllowedURL(in.URL, true)
		if title == "" || !urlOK || cleanURL == "" {
			writeErrJSON(w, http.StatusBadRequest, "valid title and allowed url required")
			return
		}
		cleanIcon, iconOK := sanitizeAdminIconField(in.Icon)
		if !iconOK {
			writeErrJSON(w, http.StatusBadRequest, "invalid icon URL scheme")
			return
		}
		if strings.TrimSpace(in.I18nTitlesJSON) == "" {
			in.I18nTitlesJSON = "{}"
		}
		_, err := s.deps.Store.ExecContext(r.Context(),
			`UPDATE nav_items SET parent_id = ?, title = ?, i18n_titles_json = ?, url = ?, open_in_new_tab = ?, icon = ?, sort_order = ?, is_enabled = ?, updated_at = ? WHERE id = ?`,
			in.ParentID, title, in.I18nTitlesJSON, cleanURL,
			in.OpenInNewTab, cleanIcon, in.SortOrder, in.IsEnabled, time.Now().UnixMilli(), rest)
		if writeBadErr(w, err) {
			return
		}
		writeOKJSON(w)
	case http.MethodDelete:
		var delRows []struct {
			ID string `db:"id"`
		}
		_ = sqlx.SelectInto(r.Context(), s.deps.Store, &delRows, `SELECT id FROM nav_items WHERE id = ? OR parent_id = ?`, rest, rest)
		_, _ = s.deps.Store.ExecContext(r.Context(), `DELETE FROM nav_items WHERE id = ? OR parent_id = ?`, rest, rest)
		if len(delRows) > 0 {
			delIDs := make([]string, 0, len(delRows))
			for _, row := range delRows {
				if id := strings.TrimSpace(row.ID); id != "" {
					delIDs = append(delIDs, id)
				}
			}
			s.pruneDeletedNavFromThemeConfigs(r.Context(), delIDs)
		}
		s.invalidateMediaRefCache()
		writeOKJSON(w)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *AppServer) handleAdminLinks(w http.ResponseWriter, r *http.Request, actor *domain.User, rest string) {
	rest = strings.TrimPrefix(rest, "/")
	if rest == "patrol" && r.Method == http.MethodPost {
		n, err := s.patrolAllFriendLinks(r.Context())
		if writeServerErr(w, err) {
			return
		}
		var rows []domain.Link
		_ = sqlx.SelectInto(r.Context(), s.deps.Store, &rows, `SELECT * FROM links ORDER BY sort_order ASC`)
		writeJSON(w, http.StatusOK, map[string]any{"checked": n, "items": rows, "links": rows})
		return
	}
	if rest == "batch" && r.Method == http.MethodPost {
		var req struct {
			Action       string   `json:"action"`
			IDs          []string `json:"ids"`
			CategoryName string   `json:"category_name"`
		}
		if !decodeJSONBody(w, r, 65536, &req) {
			return
		}
		action := strings.ToLower(strings.TrimSpace(req.Action))
		if action != "delete" && action != "category" {
			writeErrJSON(w, http.StatusBadRequest, "invalid batch action")
			return
		}
		ids := dedupCleanIDs(req.IDs, 200)
		if len(ids) == 0 {
			writeErrJSON(w, http.StatusBadRequest, "ids required")
			return
		}
		affected := 0
		catName := strings.TrimSpace(req.CategoryName)
		now := time.Now().UnixMilli()
		err := s.deps.Store.WithTx(r.Context(), func(tx *sqlx.Tx) error {
			for _, lid := range ids {
				var (
					res interface{ RowsAffected() (int64, error) }
					e   error
				)
				if action == "delete" {
					res, e = tx.ExecContext(r.Context(), `DELETE FROM links WHERE id = ?`, lid)
				} else {
					res, e = tx.ExecContext(r.Context(), `UPDATE links SET category_name = ?, updated_at = ? WHERE id = ?`, catName, now, lid)
				}
				if e != nil {
					return e
				}
				if n, _ := res.RowsAffected(); n > 0 {
					affected += int(n)
				}
			}
			return nil
		})
		if writeBadErr(w, err) {
			return
		}
		if action == "delete" {
			s.invalidateMediaRefCache()
		}
		s.recordAuditLog(r, actor, "link.batch", "link", action, map[string]any{"action": action, "affected": affected})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "affected": affected})
		return
	}
	if rest == "" {
		switch r.Method {
		case http.MethodGet:
			page, perPage := parseAdminPageParams(r, 50, 200)
			q := strings.TrimSpace(r.URL.Query().Get("q"))
			health := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("health")))
			cat := strings.TrimSpace(r.URL.Query().Get("category"))
			where := []string{"1=1"}
			var args []any
			if q != "" {
				pat := "%" + escapeLikePattern(q) + "%"
				where = append(where, "(LOWER(title) LIKE LOWER(?) ESCAPE '!' OR LOWER(url) LIKE LOWER(?) ESCAPE '!' OR LOWER(description) LIKE LOWER(?) ESCAPE '!' OR LOWER(category_name) LIKE LOWER(?) ESCAPE '!')")
				args = append(args, pat, pat, pat, pat)
			}
			switch health {
			case "ok":
				where = append(where, "health_status = 'ok'")
			case "redirect", "3xx":
				where = append(where, "(health_status = 'redirect' OR (http_code >= 300 AND http_code < 400))")
			case "broken", "error", "down", "blocked":
				where = append(where, "(health_status IN ('error', 'broken', 'down', 'blocked') OR http_code >= 400)")
			case "unchecked", "unknown":
				where = append(where, "(health_status = 'unknown' OR health_status = '')")
			}
			if cat != "" && cat != "all" {
				where = append(where, "category_name = ?")
				args = append(args, cat)
			}
			whereSQL := strings.Join(where, " AND ")
			var total int
			_ = s.deps.Store.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM links WHERE `+whereSQL, args...).Scan(&total)
			baseSQL := `SELECT * FROM links WHERE ` + whereSQL + ` ORDER BY sort_order ASC, created_at DESC`
			qSQL, fullArgs := s.paginateQuery(baseSQL, args, page, perPage)
			var rows []domain.Link
			_ = sqlx.SelectInto(r.Context(), s.deps.Store, &rows, qSQL, fullArgs...)
			if rows == nil {
				rows = []domain.Link{}
			}
			var catRows []struct {
				CategoryName string `db:"category_name"`
			}
			_ = sqlx.SelectInto(r.Context(), s.deps.Store, &catRows, `SELECT DISTINCT category_name FROM links WHERE TRIM(category_name) <> '' ORDER BY category_name ASC`)
			categories := make([]string, 0, len(catRows))
			for _, cr := range catRows {
				categories = append(categories, cr.CategoryName)
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"items":      rows,
				"links":      rows,
				"categories": categories,
				"total":      total,
				"page":       page,
				"per_page":   perPage,
				"page_size":  perPage,
			})
		case http.MethodPost:
			var in domain.Link
			if !decodeJSONBody(w, r, 16384, &in) {
				return
			}
			in.ID = id.Next()
			in.Title = strings.TrimSpace(in.Title)
			cleanURL, urlOK := sanitizeRedirectTargetURL(in.URL)
			if in.Title == "" || !urlOK {
				writeErrJSON(w, http.StatusBadRequest, "valid title and http(s) or relative url required")
				return
			}
			in.URL = cleanURL
			if strings.TrimSpace(in.IconURL) != "" {
				cleanIcon, iconOK := sanitizeRedirectTargetURL(in.IconURL)
				if !iconOK {
					writeErrJSON(w, http.StatusBadRequest, "invalid icon_url")
					return
				}
				in.IconURL = cleanIcon
			} else {
				in.IconURL = ""
			}
			in.HealthStatus = "unknown"
			now := time.Now().UnixMilli()
			in.CreatedAt = now
			in.UpdatedAt = now
			if _, err := s.deps.Store.ExecContext(r.Context(),
				`INSERT INTO links (id, title, url, description, icon_url, category_name, sort_order, health_status, http_code, latency_ms, health_error, last_checked_at, created_at, updated_at)
				 VALUES (?, ?, ?, ?, ?, ?, ?, 'unknown', 0, 0, '', 0, ?, ?)`,
				in.ID, in.Title, in.URL, in.Description, in.IconURL, in.CategoryName, in.SortOrder, now, now); err != nil {
				writeErrJSON(w, http.StatusBadRequest, err.Error())
				return
			}
			s.invalidateMediaRefCache()
			writeJSON(w, http.StatusCreated, map[string]any{"link": in})
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
		return
	}

	parts := strings.Split(rest, "/")
	if len(parts) == 2 && parts[1] == "check" && r.Method == http.MethodPost {
		var lk domain.Link
		err := sqlx.GetInto(r.Context(), s.deps.Store, &lk, `SELECT * FROM links WHERE id = ?`, strings.TrimSpace(parts[0]))
		if err != nil {
			writeErrJSON(w, http.StatusNotFound, "link not found")
			return
		}
		updated := s.checkSingleFriendLink(r.Context(), lk)
		s.recordAuditLog(r, actor, "link.check", "link", lk.ID, map[string]any{"health_status": updated.HealthStatus, "http_code": updated.HTTPCode})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "link": updated, "item": updated})
		return
	}

	switch r.Method {
	case http.MethodPut:
		var in domain.Link
		if !decodeJSONBody(w, r, 16384, &in) {
			return
		}
		title := strings.TrimSpace(in.Title)
		cleanURL, urlOK := sanitizeRedirectTargetURL(in.URL)
		if title == "" || !urlOK {
			writeErrJSON(w, http.StatusBadRequest, "valid title and http(s) or relative url required")
			return
		}
		cleanIcon := ""
		if strings.TrimSpace(in.IconURL) != "" {
			var iconOK bool
			cleanIcon, iconOK = sanitizeRedirectTargetURL(in.IconURL)
			if !iconOK {
				writeErrJSON(w, http.StatusBadRequest, "invalid icon_url")
				return
			}
		}
		_, err := s.deps.Store.ExecContext(r.Context(),
			`UPDATE links SET title = ?, url = ?, description = ?, icon_url = ?, category_name = ?, sort_order = ?, updated_at = ? WHERE id = ?`,
			title, cleanURL, in.Description, cleanIcon, in.CategoryName, in.SortOrder, time.Now().UnixMilli(), rest)
		if writeBadErr(w, err) {
			return
		}
		s.invalidateMediaRefCache()
		writeOKJSON(w)
	case http.MethodDelete:
		_, _ = s.deps.Store.ExecContext(r.Context(), `DELETE FROM links WHERE id = ?`, rest)
		s.invalidateMediaRefCache()
		writeOKJSON(w)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

type opmlOutline struct {
	Text        string
	Title       string
	Type        string
	XMLURL      string
	HTMLURL     string
	Description string
}

func collectOPMLOutlinesFromNode(n *tinyxml.Node, dst *[]opmlOutline) {
	if n == nil {
		return
	}
	for _, ch := range n.Children {
		if strings.EqualFold(ch.Local, "outline") {
			rawXML := strings.TrimSpace(ch.Attr("xmlUrl"))
			if rawXML != "" {
				*dst = append(*dst, opmlOutline{
					Text:        ch.Attr("text"),
					Title:       ch.Attr("title"),
					Type:        ch.Attr("type"),
					XMLURL:      rawXML,
					HTMLURL:     strings.TrimSpace(ch.Attr("htmlUrl")),
					Description: ch.Attr("description"),
				})
			}
		}
		if len(ch.Children) > 0 {
			collectOPMLOutlinesFromNode(ch, dst)
		}
	}
}

func renderOPMLDocument(siteTitle string, dateCreated string, outlines []opmlOutline) []byte {
	var b bytes.Buffer
	b.WriteString(tinyxml.Header)
	b.WriteString("<opml version=\"2.0\">\n  <head>\n")
	if siteTitle != "" {
		b.WriteString("    <title>" + tinyxml.EscapeText(siteTitle) + "</title>\n")
	}
	if dateCreated != "" {
		b.WriteString("    <dateCreated>" + tinyxml.EscapeText(dateCreated) + "</dateCreated>\n")
	}
	b.WriteString("  </head>\n  <body>\n")
	for _, o := range outlines {
		b.WriteString("    <outline")
		writeOPMLAttr(&b, "text", o.Text)
		writeOPMLAttr(&b, "title", o.Title)
		writeOPMLAttr(&b, "type", o.Type)
		writeOPMLAttr(&b, "xmlUrl", o.XMLURL)
		writeOPMLAttr(&b, "htmlUrl", o.HTMLURL)
		writeOPMLAttr(&b, "description", o.Description)
		b.WriteString("></outline>\n")
	}
	b.WriteString("  </body>\n</opml>")
	return b.Bytes()
}

func writeOPMLAttr(b *bytes.Buffer, key, val string) {
	if val == "" {
		return
	}
	b.WriteByte(' ')
	b.WriteString(key)
	b.WriteString(`="`)
	b.WriteString(tinyxml.EscapeText(val))
	b.WriteByte('"')
}

func (s *AppServer) handleAdminFollows(w http.ResponseWriter, r *http.Request, actor *domain.User, rest string) {
	rest = strings.TrimPrefix(rest, "/")
	if (rest == "export.opml" || rest == "export" || rest == "opml") && r.Method == http.MethodGet {
		var rows []domain.Follow
		_ = sqlx.SelectInto(r.Context(), s.deps.Store, &rows, `SELECT * FROM follows ORDER BY sort_order ASC, created_at ASC`)
		siteTitle := "Volo Subscriptions"
		if s.deps.Settings != nil && strings.TrimSpace(s.deps.Settings.Snapshot().Site.Title) != "" {
			siteTitle = strings.TrimSpace(s.deps.Settings.Snapshot().Site.Title) + " Subscriptions"
		}
		outlines := make([]opmlOutline, 0, len(rows))
		for _, fw := range rows {
			outlines = append(outlines, opmlOutline{
				Text:        fw.Title,
				Title:       fw.Title,
				Type:        "rss",
				XMLURL:      fw.FeedURL,
				HTMLURL:     fw.SiteURL,
				Description: fw.Description,
			})
		}
		xmlBytes := renderOPMLDocument(siteTitle, time.Now().UTC().Format(time.RFC1123Z), outlines)
		w.Header().Set("Content-Type", "application/xml; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="volo-follows.opml"`)
		_, _ = w.Write(xmlBytes)
		return
	}

	if (rest == "import-opml" || rest == "import") && r.Method == http.MethodPost {
		var rawOPML []byte
		ct := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type")))
		if strings.HasPrefix(ct, "multipart/form-data") {
			files, fields, mErr := readMultipartInMemoryFiles(w, r, 4<<20, 4<<20, 2)
			if mErr != nil {
				writeErrJSON(w, http.StatusBadRequest, mErr.Error())
				return
			}
			if len(files) > 0 {
				rawOPML = files[0].Data
			} else if strings.TrimSpace(fields["opml"]) != "" {
				rawOPML = []byte(fields["opml"])
			} else if strings.TrimSpace(fields["data"]) != "" {
				rawOPML = []byte(fields["data"])
			}
		} else {
			bodyBytes, rErr := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<20))
			if rErr != nil {
				writeErrJSON(w, http.StatusBadRequest, rErr.Error())
				return
			}
			trimmed := bytes.TrimSpace(bodyBytes)
			if len(trimmed) > 0 && trimmed[0] == '{' {
				var jReq struct {
					OPML string `json:"opml"`
					Data string `json:"data"`
				}
				if err := json.Unmarshal(trimmed, &jReq); err != nil {
					writeErrJSON(w, http.StatusBadRequest, "invalid json")
					return
				}
				if strings.TrimSpace(jReq.OPML) != "" {
					rawOPML = []byte(jReq.OPML)
				} else {
					rawOPML = []byte(jReq.Data)
				}
			} else {
				rawOPML = trimmed
			}
		}
		if len(bytes.TrimSpace(rawOPML)) == 0 {
			writeErrJSON(w, http.StatusBadRequest, "empty OPML payload")
			return
		}
		root, err := tinyxml.Parse(rawOPML)
		if err != nil || root == nil || !strings.EqualFold(root.Local, "opml") {
			if err != nil {
				writeErrJSON(w, http.StatusBadRequest, "invalid OPML XML: "+err.Error())
			} else {
				writeErrJSON(w, http.StatusBadRequest, "invalid OPML XML: missing <opml> root")
			}
			return
		}
		var flat []opmlOutline
		if bodyNode := root.Child("body"); bodyNode != nil {
			collectOPMLOutlinesFromNode(bodyNode, &flat)
		} else {
			collectOPMLOutlinesFromNode(root, &flat)
		}

		var existing []struct {
			FeedURL string `db:"feed_url"`
		}
		_ = sqlx.SelectInto(r.Context(), s.deps.Store, &existing, `SELECT feed_url FROM follows`)
		seen := make(map[string]struct{}, len(existing)+len(flat))
		for _, ex := range existing {
			seen[strings.ToLower(strings.TrimSpace(ex.FeedURL))] = struct{}{}
		}

		imported := 0
		skipped := 0
		now := time.Now().UnixMilli()
		_ = s.deps.Store.WithTx(r.Context(), func(tx *sqlx.Tx) error {
			for idx, item := range flat {
				if imported >= 500 {
					skipped++
					continue
				}
				cleanFeed, feedOK := repo.SanitizeAllowedURL(item.XMLURL, false)
				lowerFeed := strings.ToLower(cleanFeed)
				if !feedOK || (!strings.HasPrefix(lowerFeed, "http://") && !strings.HasPrefix(lowerFeed, "https://")) {
					skipped++
					continue
				}
				if _, dup := seen[lowerFeed]; dup {
					skipped++
					continue
				}
				title := strings.TrimSpace(item.Title)
				if title == "" {
					title = strings.TrimSpace(item.Text)
				}
				if title == "" {
					if u, uErr := url.Parse(cleanFeed); uErr == nil && u.Host != "" {
						title = u.Host
					} else {
						title = cleanFeed
					}
				}
				cleanSite := ""
				if strings.TrimSpace(item.HTMLURL) != "" {
					if cs, ok := sanitizeRedirectTargetURL(item.HTMLURL); ok {
						cleanSite = cs
					}
				}
				desc := truncateRunes(strings.TrimSpace(item.Description), 300)
				if _, iErr := tx.ExecContext(r.Context(),
					`INSERT INTO follows (id, title, site_url, feed_url, description, icon_url, sort_order, last_synced_at, sync_error, created_at, updated_at)
					 VALUES (?, ?, ?, ?, ?, '', ?, 0, '', ?, ?)`,
					id.Next(), truncateRunes(title, 120), cleanSite, cleanFeed, desc, idx, now, now); iErr != nil {
					skipped++
					continue
				}
				seen[lowerFeed] = struct{}{}
				imported++
			}
			return nil
		})
		s.invalidateMediaRefCache()
		s.recordAuditLog(r, actor, "follow.import_opml", "follow", "opml", map[string]any{"imported": imported, "skipped": skipped})
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":       true,
			"imported": imported,
			"count":    imported,
			"skipped":  skipped,
		})
		return
	}

	if rest == "sync" && r.Method == http.MethodPost {
		n, err := s.syncAllFollowFeeds(r.Context())
		if writeServerErr(w, err) {
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"synced_items": n})
		return
	}
	if rest == "items" && r.Method == http.MethodGet {
		groups, _ := s.deps.Content.ListFollowsWithItems(r.Context(), 50)
		if groups == nil {
			groups = []content.FollowGroup{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"groups": groups})
		return
	}
	if rest == "" {
		switch r.Method {
		case http.MethodGet:
			page, perPage := parseAdminPageParams(r, 50, 200)
			q := strings.TrimSpace(r.URL.Query().Get("q"))
			st := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("status")))
			where := []string{"1=1"}
			var args []any
			if q != "" {
				pat := "%" + escapeLikePattern(q) + "%"
				where = append(where, "(LOWER(title) LIKE LOWER(?) ESCAPE '!' OR LOWER(site_url) LIKE LOWER(?) ESCAPE '!' OR LOWER(feed_url) LIKE LOWER(?) ESCAPE '!' OR LOWER(description) LIKE LOWER(?) ESCAPE '!')")
				args = append(args, pat, pat, pat, pat)
			}
			switch st {
			case "ok":
				where = append(where, "(sync_error = '' AND last_synced_at > 0)")
			case "error", "broken":
				where = append(where, "sync_error <> ''")
			case "pending", "unsynced":
				where = append(where, "(sync_error = '' AND last_synced_at = 0)")
			}
			whereSQL := strings.Join(where, " AND ")
			var total int
			_ = s.deps.Store.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM follows WHERE `+whereSQL, args...).Scan(&total)
			baseSQL := `SELECT * FROM follows WHERE ` + whereSQL + ` ORDER BY sort_order ASC, created_at DESC`
			qSQL, fullArgs := s.paginateQuery(baseSQL, args, page, perPage)
			var rows []domain.Follow
			_ = sqlx.SelectInto(r.Context(), s.deps.Store, &rows, qSQL, fullArgs...)
			if rows == nil {
				rows = []domain.Follow{}
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"items":     rows,
				"follows":   rows,
				"total":     total,
				"page":      page,
				"per_page":  perPage,
				"page_size": perPage,
			})
		case http.MethodPost:
			var in domain.Follow
			if !decodeJSONBody(w, r, 16384, &in) {
				return
			}
			in.ID = id.Next()
			in.Title = strings.TrimSpace(in.Title)
			cleanFeed, feedOK := repo.SanitizeAllowedURL(in.FeedURL, false)
			lowerFeed := strings.ToLower(cleanFeed)
			if in.Title == "" || !feedOK || (!strings.HasPrefix(lowerFeed, "http://") && !strings.HasPrefix(lowerFeed, "https://")) {
				writeErrJSON(w, http.StatusBadRequest, "title and valid http(s) feed_url required")
				return
			}
			in.FeedURL = cleanFeed
			if strings.TrimSpace(in.SiteURL) != "" {
				cs, ok := sanitizeRedirectTargetURL(in.SiteURL)
				if !ok {
					writeErrJSON(w, http.StatusBadRequest, "invalid site_url")
					return
				}
				in.SiteURL = cs
			} else {
				in.SiteURL = ""
			}
			if strings.TrimSpace(in.IconURL) != "" {
				ci, ok := sanitizeRedirectTargetURL(in.IconURL)
				if !ok {
					writeErrJSON(w, http.StatusBadRequest, "invalid icon_url")
					return
				}
				in.IconURL = ci
			} else {
				in.IconURL = ""
			}
			now := time.Now().UnixMilli()
			in.CreatedAt = now
			in.UpdatedAt = now
			if _, err := s.deps.Store.ExecContext(r.Context(),
				`INSERT INTO follows (id, title, site_url, feed_url, description, icon_url, sort_order, last_synced_at, sync_error, created_at, updated_at)
				 VALUES (?, ?, ?, ?, ?, ?, ?, 0, '', ?, ?)`,
				in.ID, in.Title, in.SiteURL, in.FeedURL, in.Description, in.IconURL, in.SortOrder, now, now); err != nil {
				writeErrJSON(w, http.StatusBadRequest, err.Error())
				return
			}
			s.invalidateMediaRefCache()
			writeJSON(w, http.StatusCreated, map[string]any{"follow": in})
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
		return
	}

	parts := strings.Split(rest, "/")
	if len(parts) == 2 && parts[1] == "sync" && r.Method == http.MethodPost {
		var fw domain.Follow
		err := sqlx.GetInto(r.Context(), s.deps.Store, &fw, `SELECT * FROM follows WHERE id = ?`, strings.TrimSpace(parts[0]))
		if err != nil {
			writeErrJSON(w, http.StatusNotFound, "follow subscription not found")
			return
		}
		cnt, updated, syncErr := s.syncSingleFollowFeed(r.Context(), fw)
		if cnt > 0 && s.deps.Content != nil {
			s.deps.Content.WarmCaches(r.Context())
		}
		s.recordAuditLog(r, actor, "follow.sync_single", "follow", fw.ID, map[string]any{"synced_items": cnt})
		if syncErr != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"ok":           false,
				"synced_items": cnt,
				"follow":       updated,
				"error":        syncErr.Error(),
			})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":           true,
			"synced_items": cnt,
			"follow":       updated,
		})
		return
	}

	switch r.Method {
	case http.MethodPut:
		var in domain.Follow
		if !decodeJSONBody(w, r, 16384, &in) {
			return
		}
		title := strings.TrimSpace(in.Title)
		cleanFeed, feedOK := repo.SanitizeAllowedURL(in.FeedURL, false)
		lowerFeed := strings.ToLower(cleanFeed)
		if title == "" || !feedOK || (!strings.HasPrefix(lowerFeed, "http://") && !strings.HasPrefix(lowerFeed, "https://")) {
			writeErrJSON(w, http.StatusBadRequest, "title and valid http(s) feed_url required")
			return
		}
		cleanSite := ""
		if strings.TrimSpace(in.SiteURL) != "" {
			var ok bool
			cleanSite, ok = sanitizeRedirectTargetURL(in.SiteURL)
			if !ok {
				writeErrJSON(w, http.StatusBadRequest, "invalid site_url")
				return
			}
		}
		cleanIcon := ""
		if strings.TrimSpace(in.IconURL) != "" {
			var ok bool
			cleanIcon, ok = sanitizeRedirectTargetURL(in.IconURL)
			if !ok {
				writeErrJSON(w, http.StatusBadRequest, "invalid icon_url")
				return
			}
		}
		_, err := s.deps.Store.ExecContext(r.Context(),
			`UPDATE follows SET title = ?, site_url = ?, feed_url = ?, description = ?, icon_url = ?, sort_order = ?, updated_at = ? WHERE id = ?`,
			title, cleanSite, cleanFeed,
			in.Description, cleanIcon, in.SortOrder, time.Now().UnixMilli(), rest)
		if writeBadErr(w, err) {
			return
		}
		s.invalidateMediaRefCache()
		writeOKJSON(w)
	case http.MethodDelete:
		_ = s.deps.Store.WithTx(r.Context(), func(tx *sqlx.Tx) error {
			_, _ = tx.ExecContext(r.Context(), `DELETE FROM follow_items WHERE follow_id = ?`, rest)
			_, err := tx.ExecContext(r.Context(), `DELETE FROM follows WHERE id = ?`, rest)
			return err
		})
		s.invalidateMediaRefCache()
		writeOKJSON(w)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func normalizeRedirectSourcePath(raw string) (string, bool) {
	clean := strings.TrimSpace(repo.StripASCIIControls(raw))
	if clean == "" || strings.HasPrefix(clean, "//") || strings.ContainsAny(clean, "?# \t\\") {
		return "", false
	}
	if !strings.HasPrefix(clean, "/") {
		clean = "/" + clean
	}
	if clean == "/" || strings.HasPrefix(clean, "//") || strings.HasPrefix(clean, "/api/admin") || strings.HasPrefix(clean, "/setup") {
		return "", false
	}
	return clean, true
}

func normalizeRedirectComparePath(p string) string {
	trimmed := strings.TrimRight(strings.TrimSpace(p), "/")
	if trimmed == "" {
		return "/"
	}
	return trimmed
}

func normalizeRedirectStatusCode(code int) int {
	switch code {
	case 301, 302, 307, 308:
		return code
	default:
		return 301
	}
}

func (s *AppServer) isRedirectSelfLoop(r *http.Request, srcPath, dstTarget string) bool {
	srcNorm := normalizeRedirectComparePath(srcPath)
	dstTrim := strings.TrimSpace(dstTarget)
	if dstTrim == "" || dstTrim == srcPath {
		return true
	}
	u, err := url.Parse(dstTrim)
	if err != nil {
		return true
	}
	dstNorm := normalizeRedirectComparePath(u.Path)
	if !u.IsAbs() && u.Host == "" {
		return dstNorm == srcNorm
	}
	dstHost := strings.ToLower(strings.TrimSpace(u.Host))
	dstHostname := strings.ToLower(strings.TrimSpace(u.Hostname()))
	matchHost := func(candidateHost string) bool {
		ch := strings.ToLower(strings.TrimSpace(candidateHost))
		if ch == "" {
			return false
		}
		if ch == dstHost || ch == dstHostname {
			return true
		}
		if cu, pErr := url.Parse("http://" + ch); pErr == nil && cu.Hostname() != "" {
			return strings.ToLower(cu.Hostname()) == dstHostname
		}
		return false
	}
	sameHost := false
	if r != nil && matchHost(r.Host) {
		sameHost = true
	}
	if !sameHost {
		if pubU, pErr := url.Parse(s.publicBaseURL(r)); pErr == nil && matchHost(pubU.Host) {
			sameHost = true
		}
	}
	return sameHost && dstNorm == srcNorm
}

type redirectImportCandidate struct {
	SourcePath string `json:"source_path"`
	TargetPath string `json:"target_path"`
	StatusCode int    `json:"status_code"`
}

func parseRedirectImportCandidates(formatHint string, rawData []byte) ([]redirectImportCandidate, error) {
	trimmed := bytes.TrimSpace(rawData)
	if len(trimmed) == 0 {
		return nil, errors.New("empty redirect import payload")
	}
	hint := strings.ToLower(strings.TrimSpace(formatHint))
	if hint == "json" || (hint == "" && (trimmed[0] == '[' || trimmed[0] == '{')) {
		if trimmed[0] == '[' {
			var arr []redirectImportCandidate
			if err := json.Unmarshal(trimmed, &arr); err != nil {
				return nil, fmt.Errorf("invalid redirect JSON array: %w", err)
			}
			return arr, nil
		}
		var env struct {
			Format    string                    `json:"format"`
			Data      string                    `json:"data"`
			Items     []redirectImportCandidate `json:"items"`
			Redirects []redirectImportCandidate `json:"redirects"`
		}
		if err := json.Unmarshal(trimmed, &env); err != nil {
			return nil, fmt.Errorf("invalid redirect JSON object: %w", err)
		}
		if strings.TrimSpace(env.Data) != "" {
			innerFmt := env.Format
			if innerFmt == "" {
				innerFmt = "csv"
				dt := strings.TrimSpace(env.Data)
				if strings.HasPrefix(dt, "[") || strings.HasPrefix(dt, "{") {
					innerFmt = "json"
				}
			}
			return parseRedirectImportCandidates(innerFmt, []byte(env.Data))
		}
		if len(env.Items) > 0 {
			return env.Items, nil
		}
		return env.Redirects, nil
	}

	records, err := parseSimpleCSV(trimmed)
	if err != nil {
		return nil, fmt.Errorf("invalid redirect CSV: %w", err)
	}
	out := make([]redirectImportCandidate, 0, len(records))
	for i, rec := range records {
		if len(rec) < 2 {
			continue
		}
		col0 := strings.TrimSpace(rec[0])
		col1 := strings.TrimSpace(rec[1])
		if col0 == "" || strings.HasPrefix(col0, "#") {
			continue
		}
		if i == 0 && (strings.EqualFold(col0, "source_path") || strings.EqualFold(col0, "source") || strings.EqualFold(col0, "from")) {
			continue
		}
		code := 301
		if len(rec) >= 3 && strings.TrimSpace(rec[2]) != "" {
			if c, cErr := strconv.Atoi(strings.TrimSpace(rec[2])); cErr == nil {
				code = c
			}
		}
		out = append(out, redirectImportCandidate{
			SourcePath: col0,
			TargetPath: col1,
			StatusCode: code,
		})
	}
	return out, nil
}

func parseSimpleCSV(data []byte) ([][]string, error) {
	var records [][]string
	var rec []string
	var field strings.Builder
	i, n := 0, len(data)
	fieldStart := true
	inQuotes := false

	for i < n {
		b := data[i]
		if inQuotes {
			if b == '"' {
				if i+1 < n && data[i+1] == '"' {
					field.WriteByte('"')
					i += 2
					continue
				}
				inQuotes = false
				i++
				for i < n && (data[i] == ' ' || data[i] == '\t') {
					i++
				}
				if i < n && data[i] != ',' && data[i] != '\r' && data[i] != '\n' {
					return nil, errors.New("extraneous character after closing quote")
				}
				continue
			}
			if b == '\r' && i+1 < n && data[i+1] == '\n' {
				field.WriteByte('\n')
				i += 2
				continue
			}
			field.WriteByte(b)
			i++
			continue
		}

		if fieldStart && (b == ' ' || b == '\t') {
			i++
			continue
		}
		if fieldStart && b == '"' {
			inQuotes = true
			fieldStart = false
			i++
			continue
		}
		if b == ',' {
			rec = append(rec, field.String())
			field.Reset()
			fieldStart = true
			i++
			continue
		}
		if b == '\r' || b == '\n' {
			if b == '\r' && i+1 < n && data[i+1] == '\n' {
				i += 2
			} else {
				i++
			}
			if len(rec) > 0 || field.Len() > 0 || !fieldStart {
				rec = append(rec, field.String())
				records = append(records, rec)
				rec = nil
				field.Reset()
			}
			fieldStart = true
			continue
		}
		if b == '"' {
			return nil, errors.New("bare quote in non-quoted CSV field")
		}
		fieldStart = false
		field.WriteByte(b)
		i++
	}
	if inQuotes {
		return nil, errors.New("unterminated quoted CSV field")
	}
	if len(rec) > 0 || field.Len() > 0 || !fieldStart {
		rec = append(rec, field.String())
		records = append(records, rec)
	}
	return records, nil
}

func appendCSVRecord(buf *bytes.Buffer, fields ...string) {
	for i, f := range fields {
		if i > 0 {
			buf.WriteByte(',')
		}
		if strings.ContainsAny(f, ",\"\r\n") {
			buf.WriteByte('"')
			for j := 0; j < len(f); j++ {
				if f[j] == '"' {
					buf.WriteString(`""`)
				} else {
					buf.WriteByte(f[j])
				}
			}
			buf.WriteByte('"')
		} else {
			buf.WriteString(f)
		}
	}
	buf.WriteByte('\n')
}

func (s *AppServer) handleAdminRedirects(w http.ResponseWriter, r *http.Request, actor *domain.User, rest string) {
	rest = strings.TrimPrefix(rest, "/")
	if rest == "export" && r.Method == http.MethodGet {
		var rows []domain.RedirectRule
		_ = sqlx.SelectInto(r.Context(), s.deps.Store, &rows, `SELECT * FROM redirects ORDER BY created_at DESC`)
		if rows == nil {
			rows = []domain.RedirectRule{}
		}
		format := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("format")))
		if format == "csv" {
			var buf bytes.Buffer
			appendCSVRecord(&buf, "source_path", "target_path", "status_code")
			for _, row := range rows {
				appendCSVRecord(&buf, row.SourcePath, row.TargetPath, strconv.Itoa(row.StatusCode))
			}
			w.Header().Set("Content-Type", "text/csv; charset=utf-8")
			w.Header().Set("Content-Disposition", `attachment; filename="volo-redirects.csv"`)
			_, _ = w.Write(buf.Bytes())
			return
		}
		w.Header().Set("Content-Disposition", `attachment; filename="volo-redirects.json"`)
		writeJSON(w, http.StatusOK, map[string]any{
			"items":     rows,
			"redirects": rows,
			"total":     len(rows),
		})
		return
	}

	if rest == "import" && r.Method == http.MethodPost {
		var (
			rawPayload []byte
			fmtHint    = strings.TrimSpace(r.URL.Query().Get("format"))
		)
		ct := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type")))
		if strings.HasPrefix(ct, "multipart/form-data") {
			files, fields, mErr := readMultipartInMemoryFiles(w, r, 8<<20, 8<<20, 2)
			if mErr != nil {
				writeErrJSON(w, http.StatusBadRequest, mErr.Error())
				return
			}
			if fields["format"] != "" {
				fmtHint = fields["format"]
			}
			if len(files) > 0 {
				rawPayload = files[0].Data
				if fmtHint == "" && strings.HasSuffix(strings.ToLower(files[0].Filename), ".csv") {
					fmtHint = "csv"
				} else if fmtHint == "" && strings.HasSuffix(strings.ToLower(files[0].Filename), ".json") {
					fmtHint = "json"
				}
			} else if fields["data"] != "" {
				rawPayload = []byte(fields["data"])
			}
		} else {
			if strings.Contains(ct, "text/csv") && fmtHint == "" {
				fmtHint = "csv"
			}
			var rErr error
			rawPayload, rErr = io.ReadAll(http.MaxBytesReader(w, r.Body, 8<<20))
			if rErr != nil {
				writeErrJSON(w, http.StatusBadRequest, rErr.Error())
				return
			}
		}

		candidates, pErr := parseRedirectImportCandidates(fmtHint, rawPayload)
		if pErr != nil {
			writeErrJSON(w, http.StatusBadRequest, pErr.Error())
			return
		}
		imported := 0
		skipped := 0
		selfLoops := 0
		now := time.Now().UnixMilli()
		err := s.deps.Store.WithTx(r.Context(), func(tx *sqlx.Tx) error {
			for _, cand := range candidates {
				if imported >= 2000 {
					skipped++
					continue
				}
				src, srcOK := normalizeRedirectSourcePath(cand.SourcePath)
				dst, dstOK := sanitizeRedirectTargetURL(cand.TargetPath)
				if !srcOK || !dstOK {
					skipped++
					continue
				}
				if s.isRedirectSelfLoop(r, src, dst) {
					selfLoops++
					skipped++
					continue
				}
				code := normalizeRedirectStatusCode(cand.StatusCode)
				var existingID string
				_ = tx.QueryRowContext(r.Context(), `SELECT id FROM redirects WHERE source_path = ?`, src).Scan(&existingID)
				if existingID != "" {
					if _, uErr := tx.ExecContext(r.Context(),
						`UPDATE redirects SET target_path = ?, status_code = ? WHERE id = ?`,
						dst, code, existingID); uErr != nil {
						return uErr
					}
				} else {
					if _, iErr := tx.ExecContext(r.Context(),
						`INSERT INTO redirects (id, source_path, target_path, status_code, is_system_legacy, hit_count, created_at)
						 VALUES (?, ?, ?, ?, ?, 0, ?)`,
						id.Next(), src, dst, code, false, now); iErr != nil {
						return iErr
					}
				}
				imported++
			}
			return nil
		})
		if writeBadErr(w, err) {
			return
		}
		if imported == 0 && len(candidates) > 0 {
			errMsg := "no valid redirect rules found in payload"
			if selfLoops > 0 {
				errMsg = "redirect target cannot point to source_path (self-loop)"
			}
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error":    errMsg,
				"imported": 0,
				"skipped":  skipped,
			})
			return
		}
		if s.deps.Content != nil {
			s.deps.Content.WarmCaches(r.Context())
		}
		s.recordAuditLog(r, actor, "redirect.import", "redirect", "import", map[string]any{"imported": imported, "skipped": skipped})
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":       true,
			"imported": imported,
			"count":    imported,
			"skipped":  skipped,
		})
		return
	}

	if rest == "batch" && r.Method == http.MethodPost {
		var req struct {
			Action string   `json:"action"`
			IDs    []string `json:"ids"`
		}
		if !decodeJSONBody(w, r, 65536, &req) {
			return
		}
		if strings.ToLower(strings.TrimSpace(req.Action)) != "delete" {
			writeErrJSON(w, http.StatusBadRequest, "invalid batch action")
			return
		}
		ids := dedupCleanIDs(req.IDs, 200)
		if len(ids) == 0 {
			writeErrJSON(w, http.StatusBadRequest, "ids required")
			return
		}
		affected := 0
		err := s.deps.Store.WithTx(r.Context(), func(tx *sqlx.Tx) error {
			for _, rid := range ids {
				res, dErr := tx.ExecContext(r.Context(), `DELETE FROM redirects WHERE id = ?`, rid)
				if dErr != nil {
					return dErr
				}
				if n, _ := res.RowsAffected(); n > 0 {
					affected += int(n)
				}
			}
			return nil
		})
		if writeBadErr(w, err) {
			return
		}
		if s.deps.Content != nil {
			s.deps.Content.WarmCaches(r.Context())
		}
		s.recordAuditLog(r, actor, "redirect.batch_delete", "redirect", "batch", map[string]any{"affected": affected})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "affected": affected})
		return
	}
	if rest == "" {
		switch r.Method {
		case http.MethodGet:
			page, perPage := parseAdminPageParams(r, 50, 300)
			q := strings.TrimSpace(r.URL.Query().Get("q"))
			codeStr := strings.TrimSpace(r.URL.Query().Get("status_code"))
			srcKind := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("source")))
			sortBy := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("sort")))

			where := []string{"1=1"}
			var args []any
			if q != "" {
				pat := "%" + escapeLikePattern(q) + "%"
				where = append(where, "(LOWER(source_path) LIKE LOWER(?) ESCAPE '!' OR LOWER(target_path) LIKE LOWER(?) ESCAPE '!')")
				args = append(args, pat, pat)
			}
			if codeStr == "301" || codeStr == "302" || codeStr == "307" || codeStr == "308" {
				codeVal, _ := strconv.Atoi(codeStr)
				where = append(where, "status_code = ?")
				args = append(args, codeVal)
			}
			switch srcKind {
			case "legacy", "system":
				where = append(where, "is_system_legacy = ?")
				args = append(args, true)
			case "manual", "custom":
				where = append(where, "is_system_legacy = ?")
				args = append(args, false)
			}
			orderClause := "created_at DESC"
			if sortBy == "hits" {
				orderClause = "hit_count DESC, created_at DESC"
			}
			whereSQL := strings.Join(where, " AND ")
			var total int
			_ = s.deps.Store.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM redirects WHERE `+whereSQL, args...).Scan(&total)
			baseSQL := `SELECT * FROM redirects WHERE ` + whereSQL + ` ORDER BY ` + orderClause
			qSQL, fullArgs := s.paginateQuery(baseSQL, args, page, perPage)
			var rows []domain.RedirectRule
			_ = sqlx.SelectInto(r.Context(), s.deps.Store, &rows, qSQL, fullArgs...)
			if rows == nil {
				rows = []domain.RedirectRule{}
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"items":     rows,
				"redirects": rows,
				"total":     total,
				"page":      page,
				"per_page":  perPage,
				"page_size": perPage,
			})
		case http.MethodPost:
			var in domain.RedirectRule
			if !decodeJSONBody(w, r, 8192, &in) {
				return
			}
			src, srcOK := normalizeRedirectSourcePath(in.SourcePath)
			dst, dstOK := sanitizeRedirectTargetURL(in.TargetPath)
			if !srcOK || !dstOK {
				writeErrJSON(w, http.StatusBadRequest, "valid distinct source_path and allowed target_path required")
				return
			}
			if s.isRedirectSelfLoop(r, src, dst) {
				writeErrJSON(w, http.StatusBadRequest, "redirect target cannot point to source_path (self-loop)")
				return
			}
			in.StatusCode = normalizeRedirectStatusCode(in.StatusCode)
			in.ID = id.Next()
			in.SourcePath = src
			in.TargetPath = dst
			in.CreatedAt = time.Now().UnixMilli()
			if _, err := s.deps.Store.ExecContext(r.Context(),
				`INSERT INTO redirects (id, source_path, target_path, status_code, is_system_legacy, hit_count, created_at)
				 VALUES (?, ?, ?, ?, ?, 0, ?)`,
				in.ID, in.SourcePath, in.TargetPath, in.StatusCode, false, in.CreatedAt); err != nil {
				writeErrJSON(w, http.StatusBadRequest, err.Error())
				return
			}
			if s.deps.Content != nil {
				s.deps.Content.WarmCaches(r.Context())
			}
			writeJSON(w, http.StatusCreated, map[string]any{"redirect": in})
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
		return
	}
	switch r.Method {
	case http.MethodPut:
		var in domain.RedirectRule
		if !decodeJSONBody(w, r, 8192, &in) {
			return
		}
		src, srcOK := normalizeRedirectSourcePath(in.SourcePath)
		dst, dstOK := sanitizeRedirectTargetURL(in.TargetPath)
		if !srcOK || !dstOK {
			writeErrJSON(w, http.StatusBadRequest, "valid distinct source_path and allowed target_path required")
			return
		}
		if s.isRedirectSelfLoop(r, src, dst) {
			writeErrJSON(w, http.StatusBadRequest, "redirect target cannot point to source_path (self-loop)")
			return
		}
		in.StatusCode = normalizeRedirectStatusCode(in.StatusCode)
		if _, err := s.deps.Store.ExecContext(r.Context(),
			`UPDATE redirects SET source_path = ?, target_path = ?, status_code = ? WHERE id = ?`,
			src, dst, in.StatusCode, rest); err != nil {
			writeErrJSON(w, http.StatusBadRequest, err.Error())
			return
		}
		if s.deps.Content != nil {
			s.deps.Content.WarmCaches(r.Context())
		}
		writeOKJSON(w)
	case http.MethodDelete:
		_, _ = s.deps.Store.ExecContext(r.Context(), `DELETE FROM redirects WHERE id = ?`, rest)
		if s.deps.Content != nil {
			s.deps.Content.WarmCaches(r.Context())
		}
		writeOKJSON(w)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func truncateWebhookUTF8Bytes(s string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(s) <= maxBytes {
		return s
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

func (s *AppServer) replayWebhookDelivery(ctx context.Context, wh domain.Webhook, orig domain.WebhookDelivery) (domain.WebhookDelivery, error) {
	now := time.Now().UnixMilli()
	evName := strings.TrimSpace(orig.Event)
	if evName == "" {
		evName = "webhook.replay"
	}
	evName = truncateWebhookUTF8Bytes(evName, 64)
	storedPayload := truncateWebhookUTF8Bytes(orig.PayloadJSON, 32768)

	persist := func(statusCode, durMS int, respBody string, dErr error) (domain.WebhookDelivery, error) {
		errMsg := ""
		if dErr != nil {
			errMsg = truncateWebhookUTF8Bytes(dErr.Error(), 240)
		}
		success := dErr == nil && statusCode >= 200 && statusCode < 300
		rec := domain.WebhookDelivery{
			ID:           id.Next(),
			WebhookID:    wh.ID,
			Event:        evName,
			StatusCode:   statusCode,
			DurationMS:   durMS,
			Success:      success,
			PayloadJSON:  storedPayload,
			ResponseBody: truncateWebhookUTF8Bytes(respBody, 4096),
			ErrorText:    errMsg,
			CreatedAt:    now,
		}
		_, _ = s.deps.Store.ExecBgContext(ctx,
			`UPDATE webhooks SET last_status_code = ?, last_error = ?, last_triggered_at = ?, updated_at = ? WHERE id = ?`,
			rec.StatusCode, rec.ErrorText, now, now, wh.ID)
		_, _ = s.deps.Store.ExecBgContext(ctx,
			`INSERT INTO webhook_deliveries (id, webhook_id, event, status_code, duration_ms, success, payload_json, response_body, error_text, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			rec.ID, rec.WebhookID, rec.Event, rec.StatusCode, rec.DurationMS, rec.Success, rec.PayloadJSON, rec.ResponseBody, rec.ErrorText, rec.CreatedAt)
		_, _ = s.deps.Store.ExecBgContext(ctx,
			`DELETE FROM webhook_deliveries WHERE webhook_id = ? AND id NOT IN (SELECT id FROM (SELECT id FROM webhook_deliveries WHERE webhook_id = ? ORDER BY created_at DESC, id DESC LIMIT 100) AS keep_rows)`,
			wh.ID, wh.ID)
		return rec, dErr
	}

	rawURL := strings.TrimSpace(wh.URL)
	parsedURL, uErr := url.Parse(rawURL)
	if rawURL == "" || uErr != nil || (parsedURL.Scheme != "https" && parsedURL.Scheme != "http") {
		return persist(0, 0, "", errors.New("webhook: url must use https or http"))
	}

	plainSecret := ""
	if strings.TrimSpace(wh.SecretCiphertext) != "" {
		if s.deps.Config == nil || s.deps.Config.KeyRing == nil {
			return persist(0, 0, "", errors.New("webhook: keyring unavailable to decrypt secret"))
		}
		var decErr error
		plainSecret, decErr = s.deps.Config.KeyRing.OpenStringFallback(
			wh.SecretCiphertext,
			config.WebhookSecretAAD(wh.ID),
			"webhooks:secret:"+wh.ID,
		)
		if decErr != nil {
			return persist(0, 0, "", fmt.Errorf("webhook secret decryption failed: %w", decErr))
		}
	}

	targetURL := rawURL
	payloadBytes := []byte(orig.PayloadJSON)
	headers := map[string]string{
		"Content-Type":     "application/json; charset=utf-8",
		"User-Agent":       "Volo-go-Webhook/1.0",
		"X-Volo-Replay-Of": orig.ID,
	}
	tsSec := time.Now().Unix()
	tsStr := strconv.FormatInt(tsSec, 10)
	provider := strings.ToLower(strings.TrimSpace(wh.Provider))
	if provider == "" {
		provider = "custom"
	}

	switch provider {
	case "feishu":
		if plainSecret != "" {
			var m map[string]any
			if json.Unmarshal(payloadBytes, &m) == nil {
				mac := hmac.New(sha256.New, []byte(tsStr+"\n"+plainSecret))
				m["timestamp"] = tsStr
				m["sign"] = base64.StdEncoding.EncodeToString(mac.Sum(nil))
				if signedBytes, mErr := json.Marshal(m); mErr == nil {
					payloadBytes = signedBytes
				}
				m["sign"] = "[REDACTED]"
				if redactedBytes, rErr := json.Marshal(m); rErr == nil {
					storedPayload = truncateWebhookUTF8Bytes(string(redactedBytes), 32768)
				}
			}
		}
	case "dingtalk":
		if plainSecret != "" {
			tsMS := strconv.FormatInt(tsSec*1000, 10)
			mac := hmac.New(sha256.New, []byte(plainSecret))
			mac.Write([]byte(tsMS + "\n" + plainSecret))
			q := parsedURL.Query()
			q.Set("timestamp", tsMS)
			q.Set("sign", base64.StdEncoding.EncodeToString(mac.Sum(nil)))
			parsedURL.RawQuery = q.Encode()
			targetURL = parsedURL.String()
		}
	default:
		headers["X-Volo-Event"] = evName
		headers["X-Volo-Timestamp"] = tsStr
		if plainSecret != "" {
			mac := hmac.New(sha256.New, []byte(plainSecret))
			mac.Write([]byte(tsStr + "."))
			mac.Write(payloadBytes)
			headers["X-Volo-Signature"] = "sha256=" + hex.EncodeToString(mac.Sum(nil))
		}
	}

	dialer := s.deps.OutboundDialer
	if dialer == nil {
		dialer = &security.OutboundSafeDialer{Timeout: 8 * time.Second}
	}
	client := dialer.NewHTTPClient(8 * time.Second)
	reqCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	req, rErr := http.NewRequestWithContext(reqCtx, http.MethodPost, targetURL, bytes.NewReader(payloadBytes))
	if rErr != nil {
		return persist(0, 0, "", rErr)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	started := time.Now()
	resp, doErr := client.Do(req)
	durMS := int(time.Since(started).Milliseconds())
	if durMS < 0 {
		durMS = 0
	}
	if doErr != nil {
		return persist(0, durMS, "", doErr)
	}
	respSnippet, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	_ = resp.Body.Close()
	respStr := strings.TrimSpace(string(respSnippet))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return persist(resp.StatusCode, durMS, respStr, fmt.Errorf("webhook HTTP %d: %s", resp.StatusCode, respStr))
	}
	return persist(resp.StatusCode, durMS, respStr, nil)
}

func (s *AppServer) validateWebhookOutboundURL(rawURL string) (string, error) {
	trimmed := strings.TrimSpace(rawURL)
	if trimmed == "" {
		return "", errors.New("webhook url is required")
	}
	dialer := s.deps.OutboundDialer
	if dialer == nil {
		dialer = &security.OutboundSafeDialer{Timeout: 8 * time.Second}
	}
	u, err := dialer.ValidateOutboundURL(trimmed)
	if err != nil {
		return "", err
	}
	return u.String(), nil
}

func (s *AppServer) handleAdminWebhooks(w http.ResponseWriter, r *http.Request, actor *domain.User, rest string) {
	rest = strings.TrimPrefix(rest, "/")
	if rest == "" {
		switch r.Method {
		case http.MethodGet:
			page, perPage := parseAdminPageParams(r, 20, 100)
			var total int
			_ = s.deps.Store.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM webhooks`).Scan(&total)
			baseSQL := `SELECT * FROM webhooks ORDER BY created_at DESC`
			qSQL, fullArgs := s.paginateQuery(baseSQL, nil, page, perPage)
			var rows []domain.Webhook
			_ = sqlx.SelectInto(r.Context(), s.deps.Store, &rows, qSQL, fullArgs...)
			out := make([]map[string]any, 0, len(rows))
			for _, wh := range rows {
				hasSec := wh.SecretCiphertext != ""
				wh.SecretCiphertext = ""
				out = append(out, map[string]any{"webhook": wh, "secret_set": hasSec})
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"items":     out,
				"webhooks":  out,
				"total":     total,
				"page":      page,
				"per_page":  perPage,
				"page_size": perPage,
			})

		case http.MethodPost:
			var in struct {
				domain.Webhook
				Secret string   `json:"secret"`
				Events []string `json:"events"`
			}
			if !decodeJSONBody(w, r, 16384, &in) {
				return
			}
			in.ID = id.Next()
			in.Name = strings.TrimSpace(in.Name)
			in.URL = strings.TrimSpace(in.URL)
			if in.Name == "" || in.URL == "" {
				writeErrJSON(w, http.StatusBadRequest, "name and url required")
				return
			}
			cleanURL, uErr := s.validateWebhookOutboundURL(in.URL)
			if uErr != nil {
				writeErrJSON(w, http.StatusBadRequest, uErr.Error())
				return
			}
			in.URL = cleanURL
			if in.Provider == "" {
				in.Provider = "custom"
			}
			if len(in.Events) > 0 {
				b, _ := json.Marshal(in.Events)
				in.EventsJSON = string(b)
			} else if in.EventsJSON == "" {
				in.EventsJSON = `["comment.created","article.published"]`
			}
			secCipher := ""
			if strings.TrimSpace(in.Secret) != "" {
				var sErr error
				secCipher, sErr = s.deps.Config.KeyRing.SealString(strings.TrimSpace(in.Secret), config.WebhookSecretAAD(in.ID))
				if sErr != nil {
					writeErrJSON(w, http.StatusInternalServerError, sErr.Error())
					return
				}
			}
			now := time.Now().UnixMilli()
			in.CreatedAt = now
			in.UpdatedAt = now
			if _, err := s.deps.Store.ExecContext(r.Context(),
				`INSERT INTO webhooks (id, name, provider, url, secret_ciphertext, chat_id, events_json, is_enabled, last_status_code, last_error, last_triggered_at, created_at, updated_at)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0, '', 0, ?, ?)`,
				in.ID, in.Name, in.Provider, in.URL, secCipher, in.ChatID, in.EventsJSON, in.IsEnabled, now, now); err != nil {
				writeErrJSON(w, http.StatusBadRequest, err.Error())
				return
			}
			writeJSON(w, http.StatusCreated, map[string]any{"webhook": in.Webhook, "secret_set": secCipher != ""})

		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
		return
	}

	parts := strings.Split(rest, "/")
	whID := parts[0]
	var wh domain.Webhook
	err := sqlx.GetInto(r.Context(), s.deps.Store, &wh, `SELECT * FROM webhooks WHERE id = ?`, whID)
	if err != nil {
		writeErrJSON(w, http.StatusNotFound, "webhook not found")
		return
	}
	if len(parts) == 2 && parts[1] == "deliveries" && r.Method == http.MethodGet {
		page, perPage := parseAdminPageParams(r, 20, 100)
		var total int
		_ = s.deps.Store.QueryRowContext(r.Context(),
			`SELECT COUNT(*) FROM webhook_deliveries WHERE webhook_id = ?`, wh.ID).Scan(&total)
		baseSQL := `SELECT * FROM webhook_deliveries WHERE webhook_id = ? ORDER BY created_at DESC, id DESC`
		qSQL, fullArgs := s.paginateQuery(baseSQL, []any{wh.ID}, page, perPage)
		var rows []domain.WebhookDelivery
		_ = sqlx.SelectInto(r.Context(), s.deps.Store, &rows, qSQL, fullArgs...)
		if rows == nil {
			rows = []domain.WebhookDelivery{}
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"items":      rows,
			"deliveries": rows,
			"total":      total,
			"page":       page,
			"per_page":   perPage,
			"page_size":  perPage,
		})
		return
	}
	if len(parts) == 4 && parts[1] == "deliveries" && parts[3] == "replay" && r.Method == http.MethodPost {
		var orig domain.WebhookDelivery
		dErr := sqlx.GetInto(r.Context(), s.deps.Store, &orig,
			`SELECT * FROM webhook_deliveries WHERE id = ? AND webhook_id = ?`, strings.TrimSpace(parts[2]), wh.ID)
		if dErr != nil {
			writeErrJSON(w, http.StatusNotFound, "webhook delivery not found")
			return
		}
		newDeliv, rErr := s.replayWebhookDelivery(r.Context(), wh, orig)
		s.recordAuditLog(r, actor, "webhook.replay", "webhook", wh.ID, map[string]any{"delivery_id": orig.ID, "new_delivery_id": newDeliv.ID, "status_code": newDeliv.StatusCode})
		if rErr != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"ok":          false,
				"status_code": newDeliv.StatusCode,
				"delivery":    newDeliv,
				"error":       rErr.Error(),
			})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":          true,
			"status_code": newDeliv.StatusCode,
			"delivery":    newDeliv,
		})
		return
	}
	if len(parts) == 2 && parts[1] == "test" && r.Method == http.MethodPost {
		if s.deps.TestWebhook == nil {
			writeErrJSON(w, http.StatusServiceUnavailable, "webhook dispatcher not wired")
			return
		}
		code, tErr := s.deps.TestWebhook(r.Context(), wh, actor.PreferredLocale)
		if tErr != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "status_code": code, "error": tErr.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status_code": code})
		return
	}

	switch r.Method {
	case http.MethodPut:
		var in struct {
			domain.Webhook
			Secret      string   `json:"secret"`
			ClearSecret bool     `json:"clear_secret"`
			Events      []string `json:"events"`
		}
		if !decodeJSONBody(w, r, 16384, &in) {
			return
		}
		nextName := strings.TrimSpace(in.Name)
		nextURL := strings.TrimSpace(in.URL)
		if nextName == "" || nextURL == "" {
			writeErrJSON(w, http.StatusBadRequest, "name and url required")
			return
		}
		cleanURL, uErr := s.validateWebhookOutboundURL(nextURL)
		if uErr != nil {
			writeErrJSON(w, http.StatusBadRequest, uErr.Error())
			return
		}
		secCipher := wh.SecretCiphertext
		if in.ClearSecret {
			secCipher = ""
		} else if strings.TrimSpace(in.Secret) != "" {
			secCipher, _ = s.deps.Config.KeyRing.SealString(strings.TrimSpace(in.Secret), config.WebhookSecretAAD(whID))
		}
		evJSON := wh.EventsJSON
		if len(in.Events) > 0 {
			b, _ := json.Marshal(in.Events)
			evJSON = string(b)
		} else if in.EventsJSON != "" {
			evJSON = in.EventsJSON
		}
		_, err := s.deps.Store.ExecContext(r.Context(),
			`UPDATE webhooks SET name = ?, provider = ?, url = ?, secret_ciphertext = ?, chat_id = ?, events_json = ?, is_enabled = ?, updated_at = ? WHERE id = ?`,
			nextName, in.Provider, cleanURL, secCipher, in.ChatID, evJSON, in.IsEnabled, time.Now().UnixMilli(), whID)
		if writeBadErr(w, err) {
			return
		}
		writeOKJSON(w)

	case http.MethodDelete:
		_, _ = s.deps.Store.ExecContext(r.Context(), `DELETE FROM webhook_deliveries WHERE webhook_id = ?`, whID)
		_, _ = s.deps.Store.ExecContext(r.Context(), `DELETE FROM webhooks WHERE id = ?`, whID)
		writeOKJSON(w)

	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *AppServer) listRenderableCustomNav(ctx context.Context, locale string) []content.PublicNavNode {
	if s.deps.Content == nil {
		return nil
	}
	raw, err := s.deps.Content.ListPublicNavItems(ctx, locale)
	if err != nil || len(raw) == 0 {
		return nil
	}
	adminPrefix, _ := s.effectiveAdminPrefix()
	staticAdmin := s.staticAdminPrefix()
	out := make([]content.PublicNavNode, 0, len(raw))
	seen := make(map[string]bool, len(raw))
	for _, it := range raw {
		id := strings.TrimSpace(it.ID)
		u := strings.TrimSpace(it.URL)
		if id == "" || seen[id] || u == "" || u == adminPrefix || u == staticAdmin || u == "/logout" {
			continue
		}
		seen[id] = true
		out = append(out, it)
	}
	return out
}

func toStringSliceAny(raw any) ([]string, bool) {
	switch v := raw.(type) {
	case []string:
		out := make([]string, 0, len(v))
		for _, s := range v {
			if t := strings.TrimSpace(s); t != "" {
				out = append(out, t)
			}
		}
		return out, true
	case []any:
		out := make([]string, 0, len(v))
		for _, elem := range v {
			if elem == nil {
				continue
			}
			if t := strings.TrimSpace(fmt.Sprint(elem)); t != "" {
				out = append(out, t)
			}
		}
		return out, true
	default:
		return nil, false
	}
}

func expandThemeNavKeyList(rawKeys []string, customNav []content.PublicNavNode, seenIDs []string, hasSeen bool) []string {
	validCustomSet := make(map[string]bool, len(customNav))
	for _, cn := range customNav {
		validCustomSet[cn.ID] = true
	}
	seenCustomSet := make(map[string]bool, len(seenIDs))
	for _, sid := range seenIDs {
		seenCustomSet[sid] = true
	}

	out := make([]string, 0, len(rawKeys)+len(customNav))
	emitted := make(map[string]bool, len(rawKeys)+len(customNav))
	hadWildcard := false

	for _, rawKey := range rawKeys {
		k := strings.TrimSpace(rawKey)
		if k == "" {
			continue
		}
		if strings.EqualFold(k, "custom") {
			hadWildcard = true
			for _, cn := range customNav {
				ck := "custom:" + cn.ID
				if !emitted[ck] {
					emitted[ck] = true
					out = append(out, ck)
				}
			}
			continue
		}
		if len(k) > 7 && strings.EqualFold(k[:7], "custom:") {
			cid := strings.TrimSpace(k[7:])
			if !validCustomSet[cid] {
				continue
			}
			ck := "custom:" + cid
			if !emitted[ck] {
				emitted[ck] = true
				out = append(out, ck)
			}
			continue
		}
		low := strings.ToLower(k)
		if !emitted[low] {
			emitted[low] = true
			out = append(out, low)
		}
	}

	if !hadWildcard && hasSeen {
		for _, cn := range customNav {
			ck := "custom:" + cn.ID
			if !emitted[ck] && !seenCustomSet[cn.ID] {
				emitted[ck] = true
				out = append(out, ck)
			}
		}
	}
	return out
}

func (s *AppServer) enrichThemesWithCustomNav(ctx context.Context, locale string, themes []theme.LocalizedTheme) []theme.LocalizedTheme {
	if len(themes) == 0 {
		return themes
	}
	customNav := s.listRenderableCustomNav(ctx, locale)
	out := make([]theme.LocalizedTheme, len(themes))
	copy(out, themes)

	for ti := range out {
		if len(out[ti].LocalizedSchema) == 0 {
			continue
		}
		schemaCopy := make([]theme.LocalizedConfigField, len(out[ti].LocalizedSchema))
		copy(schemaCopy, out[ti].LocalizedSchema)
		modified := false
		for fi := range schemaCopy {
			f := &schemaCopy[fi]
			if f.Key != "header_nav_items" {
				continue
			}
			modified = true
			nextOpts := make([]theme.LocalizedConfigOption, 0, len(f.Options)+len(customNav))
			expandedOpt := false
			for _, opt := range f.Options {
				if opt.Value == "custom" {
					expandedOpt = true
					for _, cn := range customNav {
						lbl := strings.TrimSpace(cn.Title)
						if lbl == "" {
							lbl = cn.URL
						}
						nextOpts = append(nextOpts, theme.LocalizedConfigOption{
							Value: "custom:" + cn.ID,
							Label: fmt.Sprintf("%s (%s)", lbl, cn.URL),
						})
					}
					continue
				}
				nextOpts = append(nextOpts, opt)
			}
			if !expandedOpt {
				for _, cn := range customNav {
					lbl := strings.TrimSpace(cn.Title)
					if lbl == "" {
						lbl = cn.URL
					}
					nextOpts = append(nextOpts, theme.LocalizedConfigOption{
						Value: "custom:" + cn.ID,
						Label: fmt.Sprintf("%s (%s)", lbl, cn.URL),
					})
				}
			}
			f.Options = nextOpts
			if defSlice, ok := toStringSliceAny(f.Default); ok {
				f.Default = expandThemeNavKeyList(defSlice, customNav, nil, false)
			}
		}
		if modified {
			out[ti].LocalizedSchema = schemaCopy
		}
	}
	return out
}

func (s *AppServer) normalizeAppearanceNavConfigs(ctx context.Context, locale string, app domain.AppearanceSettings) domain.AppearanceSettings {
	if len(app.ThemeConfigs) == 0 {
		return app
	}
	customNav := s.listRenderableCustomNav(ctx, locale)
	activeIDs := make([]string, 0, len(customNav))
	for _, cn := range customNav {
		activeIDs = append(activeIDs, cn.ID)
	}
	nextConfigs := make(map[string]map[string]any, len(app.ThemeConfigs))
	for tid, cfg := range app.ThemeConfigs {
		copied := make(map[string]any, len(cfg)+1)
		for k, v := range cfg {
			copied[k] = v
		}
		if rawItems, exists := copied["header_nav_items"]; exists {
			if itemsSlice, ok := toStringSliceAny(rawItems); ok {
				seenSlice, hasSeen := toStringSliceAny(copied["header_nav_seen_custom"])
				copied["header_nav_items"] = expandThemeNavKeyList(itemsSlice, customNav, seenSlice, hasSeen)
				copied["header_nav_seen_custom"] = activeIDs
			}
		}
		nextConfigs[tid] = copied
	}
	app.ThemeConfigs = nextConfigs
	return app
}

func (s *AppServer) redactAppearanceThemeSecrets(app domain.AppearanceSettings) domain.AppearanceSettings {
	if s != nil && s.deps.Themes != nil && len(app.ThemeConfigs) > 0 {
		app.ThemeConfigs = s.deps.Themes.RedactAllThemeConfigs(app.ThemeConfigs)
	}
	return app
}

func (s *AppServer) pruneDeletedNavFromThemeConfigs(ctx context.Context, deletedIDs []string) {
	if len(deletedIDs) == 0 || s.deps.Settings == nil {
		return
	}
	delSet := make(map[string]bool, len(deletedIDs))
	for _, id := range deletedIDs {
		if t := strings.TrimSpace(id); t != "" {
			delSet[t] = true
		}
	}
	if len(delSet) == 0 {
		return
	}
	cur := s.deps.Settings.Snapshot().Appearance
	if len(cur.ThemeConfigs) == 0 {
		return
	}
	changed := false
	nextConfigs := make(map[string]map[string]any, len(cur.ThemeConfigs))
	for tid, cfg := range cur.ThemeConfigs {
		copied := make(map[string]any, len(cfg))
		for k, v := range cfg {
			copied[k] = v
		}
		if rawItems, exists := copied["header_nav_items"]; exists {
			if itemsSlice, ok := toStringSliceAny(rawItems); ok {
				filtered := make([]string, 0, len(itemsSlice))
				for _, item := range itemsSlice {
					if len(item) > 7 && strings.EqualFold(item[:7], "custom:") && delSet[strings.TrimSpace(item[7:])] {
						changed = true
						continue
					}
					filtered = append(filtered, item)
				}
				copied["header_nav_items"] = filtered
			}
		}
		if rawSeen, exists := copied["header_nav_seen_custom"]; exists {
			if seenSlice, ok := toStringSliceAny(rawSeen); ok {
				filteredSeen := make([]string, 0, len(seenSlice))
				for _, sid := range seenSlice {
					if delSet[sid] {
						changed = true
						continue
					}
					filteredSeen = append(filteredSeen, sid)
				}
				copied["header_nav_seen_custom"] = filteredSeen
			}
		}
		nextConfigs[tid] = copied
	}
	if !changed {
		return
	}
	patchBytes, err := json.Marshal(map[string]any{"theme_configs": nextConfigs})
	if err == nil {
		_, _, _ = s.deps.Settings.PatchDomain(ctx, domain.SettingsDomainAppearance, patchBytes, 0)
	}
}

func isAlreadyExistsInstallErr(err error) bool {
	if err == nil {
		return false
	}
	low := strings.ToLower(err.Error())
	return strings.Contains(low, "already exists") || strings.Contains(low, "already installed")
}

func (s *AppServer) handleAdminThemes(w http.ResponseWriter, r *http.Request, actor *domain.User, rest string) {
	rest = strings.TrimPrefix(rest, "/")
	locale := actor.PreferredLocale
	if locale == "" {
		locale = s.resolveLocale(r)
	}
	if qLoc := strings.TrimSpace(r.URL.Query().Get("locale")); qLoc != "" {
		locale = qLoc
	}

	if rest == "" && r.Method == http.MethodGet {
		snap := s.deps.Settings.Snapshot()
		normApp := s.redactAppearanceThemeSecrets(s.normalizeAppearanceNavConfigs(r.Context(), locale, snap.Appearance))
		writeJSON(w, http.StatusOK, map[string]any{
			"active_theme": normApp.ActiveTheme,
			"mobile_theme": normApp.MobileTheme,
			"appearance":   normApp,
			"themes":       s.enrichThemesWithCustomNav(r.Context(), locale, s.deps.Themes.ListLocalized(locale)),
		})
		return
	}

	if rest == "reload" && r.Method == http.MethodPost {
		if writeBadErr(w, s.deps.Themes.Reload()) {
			return
		}
		snap := s.deps.Settings.Snapshot()
		normApp := s.redactAppearanceThemeSecrets(s.normalizeAppearanceNavConfigs(r.Context(), locale, snap.Appearance))
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":           true,
			"active_theme": normApp.ActiveTheme,
			"mobile_theme": normApp.MobileTheme,
			"appearance":   normApp,
			"themes":       s.enrichThemesWithCustomNav(r.Context(), locale, s.deps.Themes.ListLocalized(locale)),
		})
		return
	}

	if rest == "activate" && r.Method == http.MethodPost {
		var req struct {
			ThemeID string `json:"theme_id"`
			Target  string `json:"target"` // desktop (default) | mobile | both
		}
		if !decodeJSONBody(w, r, 4096, &req) {
			return
		}
		target := strings.ToLower(strings.TrimSpace(req.Target))
		if target == "" {
			target = "desktop"
		}
		tid := strings.TrimSpace(req.ThemeID)
		_ = s.deps.Themes.Reload()
		patchMap := map[string]any{}
		switch target {
		case "mobile":
			if tid == "same" {
				tid = ""
			}
			if tid != "" && !s.deps.Themes.Has(tid) {
				writeErrJSON(w, http.StatusBadRequest, "theme not found: "+tid)
				return
			}
			patchMap["mobile_theme"] = tid
		case "both":
			if !s.deps.Themes.Has(tid) {
				writeErrJSON(w, http.StatusBadRequest, "theme not found: "+tid)
				return
			}
			patchMap["active_theme"] = tid
			patchMap["mobile_theme"] = tid
		default:
			if !s.deps.Themes.Has(tid) {
				writeErrJSON(w, http.StatusBadRequest, "theme not found: "+tid)
				return
			}
			patchMap["active_theme"] = tid
		}
		patch, _ := json.Marshal(patchMap)
		_, ver, err := s.deps.Settings.PatchDomain(r.Context(), domain.SettingsDomainAppearance, patch, 0)
		if writeBadErr(w, err) {
			return
		}
		s.recordAuditLog(r, actor, "theme.activate", "theme", tid, map[string]any{"target": target})
		normApp := s.redactAppearanceThemeSecrets(s.normalizeAppearanceNavConfigs(r.Context(), locale, s.deps.Settings.Snapshot().Appearance))
		writeJSON(w, http.StatusOK, map[string]any{"appearance": normApp, "version": ver})
		return
	}

	if rest == "install" && r.Method == http.MethodPost {
		files, fields, mErr := readMultipartInMemoryFiles(w, r, 32<<20, 32<<20, 2)
		if mErr != nil {
			writeErrJSON(w, http.StatusBadRequest, mErr.Error())
			return
		}
		var uf *inMemoryMultipartFile
		for i := range files {
			if files[i].FieldName == "file" || uf == nil {
				uf = &files[i]
				if files[i].FieldName == "file" {
					break
				}
			}
		}
		if uf == nil {
			writeErrJSON(w, http.StatusBadRequest, "missing 'file' field")
			return
		}
		owRaw := strings.TrimSpace(fields["overwrite"])
		if owRaw == "" {
			owRaw = strings.TrimSpace(r.URL.Query().Get("overwrite"))
		}
		overwrite := owRaw == "1" || strings.EqualFold(owRaw, "true")
		mf, iErr := s.deps.Themes.InstallZip(bytes.NewReader(uf.Data), int64(len(uf.Data)), overwrite)
		if iErr != nil {
			if !overwrite && isAlreadyExistsInstallErr(iErr) {
				writeErrJSON(w, http.StatusConflict, iErr.Error())
				return
			}
			writeErrJSON(w, http.StatusBadRequest, iErr.Error())
			return
		}
		s.recordAuditLog(r, actor, "theme.install", "theme", mf.ID, map[string]any{"version": mf.Version})
		writeJSON(w, http.StatusCreated, map[string]any{
			"theme":  mf,
			"themes": s.enrichThemesWithCustomNav(r.Context(), locale, s.deps.Themes.ListLocalized(locale)),
		})
		return
	}

	parts := strings.Split(rest, "/")
	themeID := strings.TrimSpace(parts[0])
	if len(parts) == 2 && (parts[1] == "export" || parts[1] == "export.zip") && r.Method == http.MethodGet {
		if s.deps.Themes == nil || !s.deps.Themes.Has(themeID) {
			writeErrJSON(w, http.StatusNotFound, "theme not found: "+themeID)
			return
		}
		var buf bytes.Buffer
		if writeBadErr(w, s.deps.Themes.ExportZip(&buf, themeID)) {
			return
		}
		s.recordAuditLog(r, actor, "theme.export", "theme", themeID, nil)
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.zip"`, themeID))
		_, _ = w.Write(buf.Bytes())
		return
	}

	if len(parts) == 2 && parts[1] == "config" && (r.Method == http.MethodPut || r.Method == http.MethodPatch) {
		if !s.deps.Themes.Has(themeID) {
			writeErrJSON(w, http.StatusNotFound, "theme not found: "+themeID)
			return
		}
		var cfgPatch map[string]any
		if !decodeJSONBody(w, r, 65536, &cfgPatch) {
			return
		}
		if nested, ok := cfgPatch["values"].(map[string]any); ok && len(cfgPatch) == 1 {
			cfgPatch = nested
		}
		if rawItems, exists := cfgPatch["header_nav_items"]; exists {
			customNav := s.listRenderableCustomNav(r.Context(), locale)
			seenIDs := make([]string, 0, len(customNav))
			for _, cn := range customNav {
				seenIDs = append(seenIDs, cn.ID)
			}
			if itemsSlice, ok := toStringSliceAny(rawItems); ok {
				cfgPatch["header_nav_items"] = expandThemeNavKeyList(itemsSlice, customNav, nil, false)
			}
			cfgPatch["header_nav_seen_custom"] = seenIDs
		}
		cur := s.deps.Settings.Snapshot().Appearance
		if s.deps.Themes != nil {
			cfgPatch = s.deps.Themes.MergeThemeConfigSecrets(themeID, cfgPatch, cur.ThemeConfigs[themeID])
		}
		nextConfigs := make(map[string]map[string]any, len(cur.ThemeConfigs)+1)
		for k, v := range cur.ThemeConfigs {
			copied := make(map[string]any, len(v))
			for ck, cv := range v {
				copied[ck] = cv
			}
			nextConfigs[k] = copied
		}
		nextConfigs[themeID] = cfgPatch
		patchBytes, _ := json.Marshal(map[string]any{"theme_configs": nextConfigs})
		_, ver, err := s.deps.Settings.PatchDomain(r.Context(), domain.SettingsDomainAppearance, patchBytes, 0)
		if writeBadErr(w, err) {
			return
		}
		s.invalidateMediaRefCache()
		s.recordAuditLog(r, actor, "theme.config", "theme", themeID, nil)
		outApp := s.deps.Settings.Snapshot().Appearance
		normApp := s.redactAppearanceThemeSecrets(s.normalizeAppearanceNavConfigs(r.Context(), locale, outApp))
		writeJSON(w, http.StatusOK, map[string]any{"appearance": normApp, "version": ver})
		return
	}

	if len(parts) == 1 && themeID != "" && r.Method == http.MethodDelete {
		if writeBadErr(w, s.deps.Themes.Uninstall(themeID)) {
			return
		}
		curApp := s.deps.Settings.Snapshot().Appearance
		patchReset := map[string]any{}
		if curApp.ActiveTheme == themeID {
			patchReset["active_theme"] = "volo-studio"
		}
		if curApp.MobileTheme == themeID {
			patchReset["mobile_theme"] = ""
		}
		if len(patchReset) > 0 {
			patchBytes, _ := json.Marshal(patchReset)
			_, _, _ = s.deps.Settings.PatchDomain(r.Context(), domain.SettingsDomainAppearance, patchBytes, 0)
		}
		s.invalidateMediaRefCache()
		s.recordAuditLog(r, actor, "theme.uninstall", "theme", themeID, nil)
		nextApp := s.deps.Settings.Snapshot().Appearance
		normApp := s.redactAppearanceThemeSecrets(s.normalizeAppearanceNavConfigs(r.Context(), locale, nextApp))
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":           true,
			"active_theme": normApp.ActiveTheme,
			"mobile_theme": normApp.MobileTheme,
			"appearance":   normApp,
			"themes":       s.enrichThemesWithCustomNav(r.Context(), locale, s.deps.Themes.ListLocalized(locale)),
		})
		return
	}

	w.WriteHeader(http.StatusMethodNotAllowed)
}

func (s *AppServer) ensureLocalesDirReady() string {
	if s.deps.Catalog == nil {
		return ""
	}
	dir := s.deps.Catalog.ExternalDir()
	if dir == "" && s.deps.Config != nil {
		if strings.TrimSpace(s.deps.Config.LocalesDir) != "" {
			dir = s.deps.Config.LocalesDir
		} else if strings.TrimSpace(s.deps.Config.DataDir) != "" {
			dir = filepath.Join(s.deps.Config.DataDir, "locales")
		}
		if dir != "" {
			_ = s.deps.Catalog.Reload(dir)
		}
	}
	return dir
}

func (s *AppServer) handleAdminLocales(w http.ResponseWriter, r *http.Request, actor *domain.User, rest string) {
	rest = strings.TrimPrefix(rest, "/")
	extDir := s.ensureLocalesDirReady()

	if rest == "" && r.Method == http.MethodGet {
		snap := s.deps.Settings.Snapshot()
		writeJSON(w, http.StatusOK, map[string]any{
			"locales_dir":         extDir,
			"default_locale":      snap.Site.DefaultLocale,
			"allow_locale_switch": snap.Site.AllowLocaleSwitch,
			"items":               s.deps.Catalog.Packs(),
			"locales":             s.deps.Catalog.Locales(),
		})
		return
	}

	if rest == "reload" && r.Method == http.MethodPost {
		if writeBadErr(w, s.deps.Catalog.ReloadDir()) {
			return
		}
		if s.deps.Themes != nil {
			_ = s.deps.Themes.Reload()
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":      true,
			"items":   s.deps.Catalog.Packs(),
			"locales": s.deps.Catalog.Locales(),
		})
		return
	}

	if rest == "install" && r.Method == http.MethodPost {
		ct := strings.ToLower(r.Header.Get("Content-Type"))
		if strings.HasPrefix(ct, "application/json") {
			var req struct {
				Code      string            `json:"code"`
				Messages  map[string]string `json:"messages"`
				Overwrite *bool             `json:"overwrite"`
			}
			if !decodeJSONBody(w, r, 4<<20, &req) {
				return
			}
			raw, _ := json.Marshal(req.Messages)
			ow := req.Overwrite == nil || *req.Overwrite
			sum, err := s.deps.Catalog.InstallPackJSON(req.Code, raw, ow)
			if writeBadErr(w, err) {
				return
			}
			if s.deps.Themes != nil {
				_ = s.deps.Themes.Reload()
			}
			s.recordAuditLog(r, actor, "locale.install", "locale", sum.Code, nil)
			writeJSON(w, http.StatusCreated, map[string]any{
				"pack":    sum,
				"items":   s.deps.Catalog.Packs(),
				"locales": s.deps.Catalog.Locales(),
			})
			return
		}

		files, fields, mErr := readMultipartInMemoryFiles(w, r, 8<<20, 8<<20, 2)
		if mErr != nil {
			writeErrJSON(w, http.StatusBadRequest, mErr.Error())
			return
		}
		var uf *inMemoryMultipartFile
		for i := range files {
			if files[i].FieldName == "file" || uf == nil {
				uf = &files[i]
				if files[i].FieldName == "file" {
					break
				}
			}
		}
		if uf == nil {
			writeErrJSON(w, http.StatusBadRequest, "missing 'file' field")
			return
		}
		raw := uf.Data
		owRaw := strings.TrimSpace(fields["overwrite"])
		overwrite := owRaw == "" || owRaw == "1" || strings.EqualFold(owRaw, "true")
		fname := strings.TrimSpace(uf.Filename)
		if strings.HasSuffix(strings.ToLower(fname), ".zip") {
			installed, zErr := s.deps.Catalog.InstallPackZip(bytes.NewReader(raw), int64(len(raw)), overwrite)
			if zErr != nil {
				writeErrJSON(w, http.StatusBadRequest, zErr.Error())
				return
			}
			if s.deps.Themes != nil {
				_ = s.deps.Themes.Reload()
			}
			s.recordAuditLog(r, actor, "locale.install_zip", "locale", installed[0].Code, map[string]any{"count": len(installed)})
			writeJSON(w, http.StatusCreated, map[string]any{
				"installed": installed,
				"items":     s.deps.Catalog.Packs(),
				"locales":   s.deps.Catalog.Locales(),
			})
			return
		}

		codeHint := strings.TrimSpace(fields["code"])
		if codeHint == "" {
			var peek map[string]string
			if json.Unmarshal(raw, &peek) == nil && strings.TrimSpace(peek["meta.code"]) != "" {
				codeHint = strings.TrimSpace(peek["meta.code"])
			} else {
				codeHint = strings.TrimSuffix(filepath.Base(fname), filepath.Ext(fname))
			}
		}
		sum, jErr := s.deps.Catalog.InstallPackJSON(codeHint, raw, overwrite)
		if jErr != nil {
			writeErrJSON(w, http.StatusBadRequest, jErr.Error())
			return
		}
		if s.deps.Themes != nil {
			_ = s.deps.Themes.Reload()
		}
		s.recordAuditLog(r, actor, "locale.install", "locale", sum.Code, nil)
		writeJSON(w, http.StatusCreated, map[string]any{
			"pack":    sum,
			"items":   s.deps.Catalog.Packs(),
			"locales": s.deps.Catalog.Locales(),
		})
		return
	}

	parts := strings.Split(rest, "/")
	code := strings.TrimSpace(parts[0])
	if len(parts) == 2 && parts[1] == "export" && r.Method == http.MethodGet {
		data, err := s.deps.Catalog.ExportPackJSON(code)
		if writeBadErr(w, err) {
			return
		}
		resolved := s.deps.Catalog.Match(code, "")
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.json"`, resolved))
		_, _ = w.Write(data)
		return
	}

	if len(parts) == 1 && code != "" && r.Method == http.MethodDelete {
		if writeBadErr(w, s.deps.Catalog.DeleteExternalPack(code)) {
			return
		}
		if s.deps.Themes != nil {
			_ = s.deps.Themes.Reload()
		}
		snap := s.deps.Settings.Snapshot()
		if !s.deps.Catalog.Has(snap.Site.DefaultLocale) {
			patch, _ := json.Marshal(map[string]any{"default_locale": "zh-CN"})
			_, _, _ = s.deps.Settings.PatchDomain(r.Context(), domain.SettingsDomainSite, patch, 0)
		}
		s.recordAuditLog(r, actor, "locale.delete", "locale", code, nil)
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":      true,
			"items":   s.deps.Catalog.Packs(),
			"locales": s.deps.Catalog.Locales(),
		})
		return
	}

	w.WriteHeader(http.StatusMethodNotAllowed)
}

func (s *AppServer) handleAdminPlugins(w http.ResponseWriter, r *http.Request, actor *domain.User, rest string) {
	if s.deps.Plugins == nil {
		writeJSON(w, http.StatusOK, map[string]any{"items": []any{}})
		return
	}
	locale := strings.TrimSpace(r.URL.Query().Get("locale"))
	if locale == "" {
		locale = actor.PreferredLocale
	}
	if locale == "" {
		locale = s.deps.Settings.Snapshot().Site.DefaultLocale
	}
	rest = strings.TrimPrefix(rest, "/")

	if rest == "" && r.Method == http.MethodGet {
		list := s.deps.Plugins.List(locale)
		writeJSON(w, http.StatusOK, map[string]any{
			"items":     list,
			"plugins":   list,
			"cron_jobs": s.deps.Plugins.CronJobs(),
		})
		return
	}

	if rest == "cron" && r.Method == http.MethodGet {
		jobs := s.deps.Plugins.CronJobs()
		writeJSON(w, http.StatusOK, map[string]any{"items": jobs, "cron_jobs": jobs})
		return
	}

	if rest == "reorder" && r.Method == http.MethodPost {
		var req struct {
			OrderedIDs []string `json:"ordered_ids"`
			IDs        []string `json:"ids"`
		}
		if !decodeJSONBody(w, r, 65536, &req) {
			return
		}
		ids := req.OrderedIDs
		if len(ids) == 0 {
			ids = req.IDs
		}
		list, err := s.deps.Plugins.Reorder(r.Context(), ids, locale)
		if writeBadErr(w, err) {
			return
		}
		s.invalidateMediaRefCache()
		s.recordAuditLog(r, actor, "plugin.reorder", "plugin", "chain", map[string]any{"count": len(ids)})
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":        true,
			"items":     list,
			"plugins":   list,
			"cron_jobs": s.deps.Plugins.CronJobs(),
		})
		return
	}

	if (rest == "reload" || rest == "discover") && r.Method == http.MethodPost {
		if writeBadErr(w, s.deps.Plugins.ReloadAll(r.Context())) {
			return
		}
		list := s.deps.Plugins.List(locale)
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":        true,
			"items":     list,
			"plugins":   list,
			"cron_jobs": s.deps.Plugins.CronJobs(),
		})
		return
	}

	if rest == "install" && r.Method == http.MethodPost {
		files, fields, mErr := readMultipartInMemoryFiles(w, r, 32<<20, 32<<20, 2)
		if mErr != nil {
			writeErrJSON(w, http.StatusBadRequest, mErr.Error())
			return
		}
		var uf *inMemoryMultipartFile
		for i := range files {
			if files[i].FieldName == "file" || uf == nil {
				uf = &files[i]
				if files[i].FieldName == "file" {
					break
				}
			}
		}
		if uf == nil {
			writeErrJSON(w, http.StatusBadRequest, "missing 'file' field")
			return
		}
		owRaw := strings.TrimSpace(fields["overwrite"])
		if owRaw == "" {
			owRaw = strings.TrimSpace(r.URL.Query().Get("overwrite"))
		}
		overwrite := owRaw == "1" || strings.EqualFold(owRaw, "true")
		sum, iErr := s.deps.Plugins.InstallFromZip(r.Context(), bytes.NewReader(uf.Data), int64(len(uf.Data)), overwrite, locale)
		if iErr != nil {
			if !overwrite && isAlreadyExistsInstallErr(iErr) {
				writeErrJSON(w, http.StatusConflict, iErr.Error())
				return
			}
			writeErrJSON(w, http.StatusBadRequest, iErr.Error())
			return
		}
		s.recordAuditLog(r, actor, "plugin.install", "plugin", sum.ID, map[string]any{"version": sum.Version})
		writeJSON(w, http.StatusCreated, map[string]any{"plugin": sum})
		return
	}

	parts := strings.Split(rest, "/")
	pid := strings.TrimSpace(parts[0])
	if len(parts) == 2 {
		switch {
		case (parts[1] == "export" || parts[1] == "export.zip") && r.Method == http.MethodGet:
			var buf bytes.Buffer
			if err := s.deps.Plugins.ExportZip(pid, &buf); err != nil {
				if strings.Contains(strings.ToLower(err.Error()), "not found") {
					writeErrJSON(w, http.StatusNotFound, err.Error())
				} else {
					writeErrJSON(w, http.StatusBadRequest, err.Error())
				}
				return
			}
			s.recordAuditLog(r, actor, "plugin.export", "plugin", pid, nil)
			w.Header().Set("Content-Type", "application/zip")
			w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.zip"`, pid))
			_, _ = w.Write(buf.Bytes())
		case parts[1] == "enable" && r.Method == http.MethodPost:
			if writeBadErr(w, s.deps.Plugins.Enable(r.Context(), pid)) {
				return
			}
			s.recordAuditLog(r, actor, "plugin.enable", "plugin", pid, nil)
			sum, _ := s.deps.Plugins.Get(pid, locale)
			writeJSON(w, http.StatusOK, map[string]any{"plugin": sum})
		case parts[1] == "disable" && r.Method == http.MethodPost:
			if writeBadErr(w, s.deps.Plugins.Disable(r.Context(), pid)) {
				return
			}
			s.recordAuditLog(r, actor, "plugin.disable", "plugin", pid, nil)
			sum, _ := s.deps.Plugins.Get(pid, locale)
			writeJSON(w, http.StatusOK, map[string]any{"plugin": sum})
		case parts[1] == "reload" && r.Method == http.MethodPost:
			sum, err := s.deps.Plugins.ReloadPlugin(r.Context(), pid, locale)
			if writeBadErr(w, err) {
				return
			}
			s.recordAuditLog(r, actor, "plugin.reload", "plugin", pid, nil)
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "plugin": sum})
		case parts[1] == "reset-circuit" && r.Method == http.MethodPost:
			if writeBadErr(w, s.deps.Plugins.ResetCircuit(r.Context(), pid)) {
				return
			}
			sum, _ := s.deps.Plugins.Get(pid, locale)
			writeJSON(w, http.StatusOK, map[string]any{"plugin": sum})
		case parts[1] == "kv" && r.Method == http.MethodDelete:
			cleared, err := s.deps.Plugins.ClearPluginKV(r.Context(), pid)
			if writeBadErr(w, err) {
				return
			}
			if s.deps.Content != nil {
				s.deps.Content.PurgeRuntimeCaches()
			}
			s.recordAuditLog(r, actor, "plugin.clear_kv", "plugin", pid, map[string]any{"cleared": cleared})
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "cleared": cleared, "deleted": cleared})
		case parts[1] == "config" && r.Method == http.MethodPatch:
			var patch map[string]any
			if !decodeJSONBody(w, r, 65536, &patch) {
				return
			}
			sum, err := s.deps.Plugins.UpdateConfig(r.Context(), pid, patch, locale)
			if writeBadErr(w, err) {
				return
			}
			s.invalidateMediaRefCache()
			s.recordAuditLog(r, actor, "plugin.config_patch", "plugin", pid, nil)
			writeJSON(w, http.StatusOK, map[string]any{"plugin": sum})
		case parts[1] == "priority" && (r.Method == http.MethodPost || r.Method == http.MethodPatch || r.Method == http.MethodPut):
			var req struct {
				Priority int `json:"priority"`
			}
			if !decodeJSONBody(w, r, 4096, &req) {
				return
			}
			sum, err := s.deps.Plugins.SetPriority(r.Context(), pid, req.Priority, locale)
			if writeBadErr(w, err) {
				return
			}
			s.invalidateMediaRefCache()
			s.recordAuditLog(r, actor, "plugin.priority", "plugin", pid, map[string]any{"priority": sum.Priority})
			list := s.deps.Plugins.List(locale)
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "plugin": sum, "items": list, "plugins": list})
		case parts[1] == "logs" && r.Method == http.MethodGet:
			entries, _ := s.deps.Plugins.Logger().Tail(pid, 100)
			writeJSON(w, http.StatusOK, map[string]any{"items": entries})
		case parts[1] == "cron" && r.Method == http.MethodGet:
			jobs := s.deps.Plugins.PluginCronJobs(pid)
			writeJSON(w, http.StatusOK, map[string]any{"items": jobs, "cron_jobs": jobs})
		case (parts[1] == "cron" || parts[1] == "trigger-cron") && r.Method == http.MethodPost:
			jobName := strings.TrimSpace(r.URL.Query().Get("name"))
			if jobName == "" && r.ContentLength != 0 {
				var body struct {
					Name string `json:"name"`
				}
				_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body)
				jobName = strings.TrimSpace(body.Name)
			}
			triggered := s.deps.Plugins.TriggerCronJob(pid, jobName)
			s.recordAuditLog(r, actor, "plugin.cron_trigger", "plugin", pid, map[string]any{"name": jobName, "triggered": triggered})
			writeJSON(w, http.StatusOK, map[string]any{
				"ok":        true,
				"triggered": triggered,
				"cron_jobs": s.deps.Plugins.PluginCronJobs(pid),
			})
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
		return
	}

	if len(parts) == 1 && r.Method == http.MethodDelete {
		if writeBadErr(w, s.deps.Plugins.Uninstall(r.Context(), pid)) {
			return
		}
		s.invalidateMediaRefCache()
		s.recordAuditLog(r, actor, "plugin.uninstall", "plugin", pid, nil)
		writeOKJSON(w)
		return
	}
	w.WriteHeader(http.StatusMethodNotAllowed)
}

func (s *AppServer) pluginsDir() string {
	if s != nil && s.deps.Config != nil {
		return strings.TrimSpace(s.deps.Config.PluginsDir)
	}
	return ""
}

func (s *AppServer) themesDir() string {
	if s != nil && s.deps.Config != nil {
		return strings.TrimSpace(s.deps.Config.ThemesDir)
	}
	return ""
}

func (s *AppServer) handleAdminSettings(w http.ResponseWriter, r *http.Request, actor *domain.User, rest string) {
	rest = strings.TrimPrefix(rest, "/")
	if rest == "" && r.Method == http.MethodGet {
		domains := []string{
			domain.SettingsDomainSite,
			domain.SettingsDomainReading,
			domain.SettingsDomainFeed,
			domain.SettingsDomainSEO,
			domain.SettingsDomainStorage,
			domain.SettingsDomainNotifications,
			domain.SettingsDomainSecurity,
			domain.SettingsDomainAppearance,
			domain.SettingsDomainBackup,
		}
		result := make(map[string]any, len(domains))
		versions := make(map[string]int64, len(domains))
		for _, d := range domains {
			val, ver, err := s.deps.Settings.GetDomain(r.Context(), d)
			if err == nil {
				if d == domain.SettingsDomainAppearance {
					if appVal, ok := val.(domain.AppearanceSettings); ok {
						val = s.redactAppearanceThemeSecrets(appVal)
					}
				}
				result[d] = val
				versions[d] = ver
			}
		}
		storageDrivers := []string{"local", "s3"}
		if s.deps.Plugins != nil && s.deps.Plugins.Providers() != nil {
			seenDrivers := map[string]bool{"local": true, "s3": true}
			for _, drv := range s.deps.Plugins.Providers().StorageProviderIDs() {
				cleanDrv := strings.TrimSpace(drv)
				if cleanDrv != "" && !seenDrivers[cleanDrv] {
					seenDrivers[cleanDrv] = true
					storageDrivers = append(storageDrivers, cleanDrv)
				}
			}
		}
		defaultUploadDir, effectiveUploadDir := "", ""
		uploadDirPinned, geoIPDirPinned := false, false
		if s.deps.Config != nil {
			defaultUploadDir = s.deps.Config.GetDefaultUploadDir()
			effectiveUploadDir = s.deps.Config.GetUploadDir()
			uploadDirPinned = s.deps.Config.IsUploadDirPinned()
			geoIPDirPinned = s.deps.Config.IsGeoIPDirPinned()
		}
		_, defaultGeoIPDir, effectiveGeoIPDir, geoStatus := s.geoIPMeta()
		writeJSON(w, http.StatusOK, map[string]any{
			"domains":              result,
			"versions":             versions,
			"storage_drivers":      storageDrivers,
			"default_upload_dir":   defaultUploadDir,
			"effective_upload_dir": effectiveUploadDir,
			"upload_dir_pinned":    uploadDirPinned,
			"default_geoip_dir":    defaultGeoIPDir,
			"effective_geoip_dir":  effectiveGeoIPDir,
			"geoip_dir_pinned":     geoIPDirPinned,
			"geoip_status":         geoStatus,
		})
		return
	}

	if rest == "notifications/test-smtp" && r.Method == http.MethodPost {
		var req struct {
			ToEmail  string                        `json:"to_email"`
			Settings *domain.NotificationsSettings `json:"settings"`
			Password string                        `json:"smtp_password"`
		}
		if !decodeJSONBody(w, r, 16384, &req) {
			return
		}
		to := strings.TrimSpace(req.ToEmail)
		if to == "" {
			to = actor.Email
		}
		cfg := s.deps.Settings.Snapshot().Notifications
		if req.Settings != nil {
			cfg = *req.Settings
		}
		if s.deps.TestSMTP == nil {
			writeErrJSON(w, http.StatusServiceUnavailable, "SMTP mailer not wired")
			return
		}
		if err := s.deps.TestSMTP(r.Context(), cfg, req.Password, to, actor.PreferredLocale); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "to": to})
		return
	}

	if rest == "storage/test" && r.Method == http.MethodPost {
		var req struct {
			Settings  *domain.StorageSettings `json:"settings"`
			SecretKey string                  `json:"s3_secret_key"`
		}
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384)).Decode(&req)
		if req.Settings != nil && s.deps.Config != nil {
			drv := strings.ToLower(strings.TrimSpace(req.Settings.ActiveDriver))
			if drv == "" || drv == "local" {
				if _, vErr := s.deps.Config.ValidateCustomUploadDir(req.Settings.LocalDir); vErr != nil {
					writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": vErr.Error()})
					return
				}
			}
		}
		if s.deps.Media != nil {
			if err := s.deps.Media.TestStorage(r.Context(), req.Settings, req.SecretKey); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
				return
			}
		} else if s.deps.Config != nil {
			if err := os.MkdirAll(s.deps.Config.GetUploadDir(), 0o750); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
				return
			}
		}
		writeOKJSON(w)
		return
	}

	switch r.Method {
	case http.MethodGet:
		val, ver, err := s.deps.Settings.GetDomain(r.Context(), rest)
		if err != nil {
			writeErrJSON(w, http.StatusNotFound, err.Error())
			return
		}
		if rest == domain.SettingsDomainAppearance {
			if appVal, ok := val.(domain.AppearanceSettings); ok {
				val = s.redactAppearanceThemeSecrets(appVal)
			}
		}
		w.Header().Set("ETag", fmt.Sprintf(`"%d"`, ver))
		resp := map[string]any{"domain": rest, "value": val, "version": ver}
		if rest == domain.SettingsDomainStorage && s.deps.Config != nil {
			resp["effective_upload_dir"] = s.deps.Config.GetUploadDir()
			resp["upload_dir_pinned"] = s.deps.Config.IsUploadDirPinned()
		}
		if rest == domain.SettingsDomainSecurity && s.deps.Config != nil {
			_, _, effectiveGeoIPDir, geoStatus := s.geoIPMeta()
			resp["effective_geoip_dir"] = effectiveGeoIPDir
			resp["geoip_dir_pinned"] = s.deps.Config.IsGeoIPDirPinned()
			resp["geoip_status"] = geoStatus
		}
		writeJSON(w, http.StatusOK, resp)

	case http.MethodPatch:
		rawBytes, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 256*1024))
		if writeBadErr(w, err) {
			return
		}
		var meta struct {
			ExpectedVersion int64 `json:"expected_version"`
		}
		_ = json.Unmarshal(rawBytes, &meta)
		var patchMap map[string]any
		if err := json.Unmarshal(rawBytes, &patchMap); err != nil {
			writeErrJSON(w, http.StatusBadRequest, "invalid json")
			return
		}
		delete(patchMap, "expected_version")
		if rest == domain.SettingsDomainAppearance && s.deps.Themes != nil && s.deps.Settings != nil {
			if rawTC, hasTC := patchMap["theme_configs"]; hasTC && rawTC != nil {
				if tcMap, ok := rawTC.(map[string]any); ok {
					curConfigs := s.deps.Settings.Snapshot().Appearance.ThemeConfigs
					mergedTC := make(map[string]map[string]any, len(curConfigs)+len(tcMap))
					for tid, existing := range curConfigs {
						copied := make(map[string]any, len(existing))
						for k, v := range existing {
							copied[k] = v
						}
						mergedTC[tid] = copied
					}
					for tid, incomingAny := range tcMap {
						if incomingCfg, ok := incomingAny.(map[string]any); ok {
							mergedTC[tid] = s.deps.Themes.MergeThemeConfigSecrets(tid, incomingCfg, curConfigs[tid])
						}
					}
					patchMap["theme_configs"] = mergedTC
				}
			}
		}
		if rest == domain.SettingsDomainStorage && s.deps.Config != nil {
			if rawLocalDir, hasLocalDir := patchMap["local_dir"]; hasLocalDir {
				dirStr, ok := rawLocalDir.(string)
				if !ok {
					writeErrJSON(w, http.StatusBadRequest, "local_dir must be a string")
					return
				}
				normDir, vErr := s.deps.Config.ValidateCustomUploadDir(dirStr)
				if vErr != nil {
					writeErrJSON(w, http.StatusBadRequest, vErr.Error())
					return
				}
				patchMap["local_dir"] = normDir
			}
		}
		var prevSec domain.SecuritySettings
		hasGeoIPDirPatch := false
		if rest == domain.SettingsDomainSecurity {
			if s.deps.Settings != nil {
				prevSec = s.deps.Settings.Snapshot().Security
			}
			if rawGeoDir, hasGeoDir := patchMap["geoip_dir"]; hasGeoDir {
				hasGeoIPDirPatch = true
				dirStr, ok := rawGeoDir.(string)
				if !ok {
					writeErrJSON(w, http.StatusBadRequest, "geoip_dir must be a string")
					return
				}
				if s.deps.Config != nil {
					normDir, vErr := s.deps.Config.ValidateCustomGeoIPPath(dirStr)
					if vErr != nil {
						writeErrJSON(w, http.StatusBadRequest, vErr.Error())
						return
					}
					patchMap["geoip_dir"] = normDir
				}
			}
		}
		cleanPatch, _ := json.Marshal(patchMap)
		expectedVer := parseVersionHeader(r, meta.ExpectedVersion)

		val, ver, pErr := s.deps.Settings.PatchDomain(r.Context(), rest, cleanPatch, expectedVer)
		if errors.Is(pErr, repo.ErrSettingsConflict) {
			writeErrJSON(w, http.StatusConflict, pErr.Error())
			return
		}
		if pErr != nil {
			writeErrJSON(w, http.StatusBadRequest, pErr.Error())
			return
		}
		if rest == domain.SettingsDomainStorage {
			s.syncConfiguredUploadDir()
		}
		if rest == domain.SettingsDomainSecurity {
			if hasGeoIPDirPatch || s.deps.Settings.Snapshot().Security.GeoIPDir != prevSec.GeoIPDir {
				if lErr := s.syncConfiguredGeoIPDir(); lErr != nil {
					s.rollbackSecuritySettings(r, prevSec, true)
					writeErrJSON(w, http.StatusBadRequest, lErr.Error())
					return
				}
			}
			if s.deps.Config != nil && s.deps.Geo != nil && s.deps.Config.GeoAccessEnabled {
				clientIP := s.clientIP(r)
				if !s.deps.Geo.Allows(clientIP) {
					s.rollbackSecuritySettings(r, prevSec, hasGeoIPDirPatch || prevSec.GeoIPDir != "")
					writeErrJSON(w, http.StatusBadRequest, "refusing security policy or GeoIP configuration that would immediately lock out your current IP")
					return
				}
			}
			if s.deps.WAF != nil {
				s.deps.WAF.UpdateConfig(s.deps.Settings.Snapshot().Security.WAFWindowSeconds)
			}
		}
		if (rest == domain.SettingsDomainSite || rest == domain.SettingsDomainSEO) && s.deps.Content != nil {
			s.deps.Content.PurgeRuntimeCaches()
			if srch := s.deps.Content.SearchIndex(); srch != nil {
				_ = srch.Rebuild(r.Context())
			}
		}
		if rest == domain.SettingsDomainReading && s.deps.Content != nil {
			s.deps.Content.PurgeRuntimeCaches()
		}
		if rest == domain.SettingsDomainAppearance {
			if appVal, ok := val.(domain.AppearanceSettings); ok {
				val = s.redactAppearanceThemeSecrets(appVal)
			}
		}
		s.invalidateMediaRefCache()
		s.recordAuditLog(r, actor, "settings.patch", "settings", rest, map[string]any{"version": ver})
		w.Header().Set("ETag", fmt.Sprintf(`"%d"`, ver))
		resp := map[string]any{"domain": rest, "value": val, "version": ver}
		if rest == domain.SettingsDomainStorage && s.deps.Config != nil {
			resp["effective_upload_dir"] = s.deps.Config.GetUploadDir()
			resp["upload_dir_pinned"] = s.deps.Config.IsUploadDirPinned()
		}
		if rest == domain.SettingsDomainSecurity {
			_, _, effectiveGeoIPDir, geoStatus := s.geoIPMeta()
			resp["effective_geoip_dir"] = effectiveGeoIPDir
			resp["geoip_dir_pinned"] = s.deps.Config != nil && s.deps.Config.IsGeoIPDirPinned()
			resp["geoip_status"] = geoStatus
		}
		writeJSON(w, http.StatusOK, resp)

	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *AppServer) geoIPMeta() (configuredDir, defaultDir, effectiveDir string, status security.GeoDBStatus) {
	if s.deps.Config != nil {
		defaultDir = s.deps.Config.GetDefaultGeoIPDir()
		effectiveDir = s.deps.Config.GetGeoIPDir()
	}
	if s.deps.Settings != nil {
		configuredDir = s.deps.Settings.Snapshot().Security.GeoIPDir
	}
	if s.deps.Geo != nil {
		status = s.deps.Geo.Status()
	}
	return
}

func (s *AppServer) rollbackSecuritySettings(r *http.Request, prev domain.SecuritySettings, resyncGeo bool) {
	if s.deps.Settings != nil {
		rbBytes, _ := json.Marshal(prev)
		_, _, _ = s.deps.Settings.PatchDomain(r.Context(), domain.SettingsDomainSecurity, rbBytes, 0)
	}
	if s.deps.WAF != nil {
		s.deps.WAF.UpdateConfig(prev.WAFWindowSeconds)
	}
	if resyncGeo {
		_ = s.syncConfiguredGeoIPDir()
	}
}

// syncConfiguredUploadDir applies the persisted `storage.local_dir` setting (validated against
// workspace containment boundaries) to Config, Media pipeline, and BackupScheduler at runtime,
// unless UploadDir was explicitly pinned via CLI flag, OS ENV, or `.env`.
func (s *AppServer) syncConfiguredUploadDir() {
	if s == nil || s.deps.Config == nil {
		return
	}
	if !s.deps.Config.IsUploadDirPinned() {
		rawDir := ""
		if s.deps.Settings != nil {
			rawDir = strings.TrimSpace(s.deps.Settings.Snapshot().Storage.LocalDir)
		}
		if rawDir == "" {
			s.deps.Config.SetUploadDir("")
		} else if validDir, err := s.deps.Config.ValidateCustomUploadDir(rawDir); err == nil && validDir != "" {
			_ = os.MkdirAll(validDir, 0o750)
			s.deps.Config.SetUploadDir(validDir)
		} else {
			s.deps.Config.SetUploadDir("")
		}
	}
	effective := s.deps.Config.GetUploadDir()
	if setter, ok := s.deps.Media.(interface{ SetUploadDir(string) }); ok {
		setter.SetUploadDir(effective)
	}
	if s.deps.BackupScheduler != nil {
		s.deps.BackupScheduler.SetUploadDir(effective)
	}
}

// syncConfiguredGeoIPDir applies the persisted `security.geoip_dir` setting (validated against
// workspace containment boundaries) to Config and atomically hot-reloads GeoEngine offline databases,
// unless GeoIPDir was explicitly pinned via CLI flag, OS ENV, or `.env`.
func (s *AppServer) syncConfiguredGeoIPDir() error {
	if s == nil || s.deps.Config == nil {
		return nil
	}
	if !s.deps.Config.IsGeoIPDirPinned() {
		rawPath := ""
		if s.deps.Settings != nil {
			rawPath = strings.TrimSpace(s.deps.Settings.Snapshot().Security.GeoIPDir)
		}
		if rawPath == "" {
			s.deps.Config.SetGeoIPDir("")
		} else if validPath, err := s.deps.Config.ValidateCustomGeoIPPath(rawPath); err == nil && validPath != "" {
			s.deps.Config.SetGeoIPDir(validPath)
		} else {
			s.deps.Config.SetGeoIPDir("")
		}
	}
	if s.deps.Geo != nil {
		return s.deps.Geo.LoadDatabases(s.deps.Config.GetGeoIPDir())
	}
	return nil
}

func (s *AppServer) handleAdminUsers(w http.ResponseWriter, r *http.Request, actor *domain.User, rest string) {
	rest = strings.TrimPrefix(rest, "/")
	if rest == "" {
		switch r.Method {
		case http.MethodGet:
			page, perPage := parseAdminPageParams(r, 50, 300)
			roleFilter := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("role")))
			statusFilter := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("status")))
			q := strings.TrimSpace(r.URL.Query().Get("q"))
			nowMS := time.Now().UnixMilli()
			where := []string{"1=1"}
			var args []any
			switch roleFilter {
			case domain.RoleAdmin, domain.RoleEditor, domain.RoleReader:
				where = append(where, "role = ?")
				args = append(args, roleFilter)
			case "staff":
				where = append(where, "role IN ('admin', 'editor')")
			}
			switch statusFilter {
			case "active":
				where = append(where, "(status = 'active' OR (status = 'disabled' AND ban_until > 0 AND ban_until <= ?))")
				args = append(args, nowMS)
			case "disabled", "banned":
				where = append(where, "(status = 'disabled' AND (ban_until <= 0 OR ban_until > ?))")
				args = append(args, nowMS)
			case "muted":
				where = append(where, "(muted_until < 0 OR muted_until > ?)")
				args = append(args, nowMS)
			}
			if q != "" {
				pat := "%" + escapeLikePattern(q) + "%"
				where = append(where, "(LOWER(username) LIKE LOWER(?) ESCAPE '!' OR LOWER(email) LIKE LOWER(?) ESCAPE '!' OR LOWER(display_name) LIKE LOWER(?) ESCAPE '!')")
				args = append(args, pat, pat, pat)
			}
			whereSQL := strings.Join(where, " AND ")
			var total int
			_ = s.deps.Store.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM users WHERE `+whereSQL, args...).Scan(&total)
			baseSQL := `SELECT id, username, email, display_name, avatar_url, bio, website_url, role, status, ban_until, muted_until, ban_reason, shield_comments, shield_text, email_verified_at, preferred_locale, created_at, updated_at
				 FROM users WHERE ` + whereSQL + `
				 ORDER BY CASE WHEN role = 'admin' THEN 0 WHEN role = 'editor' THEN 1 ELSE 2 END, created_at DESC`
			qSQL, fullArgs := s.paginateQuery(baseSQL, args, page, perPage)
			var rows []domain.User
			_ = sqlx.SelectInto(r.Context(), s.deps.Store, &rows, qSQL, fullArgs...)
			if rows == nil {
				rows = []domain.User{}
			}
			if s.deps.Auth != nil {
				for i := range rows {
					s.deps.Auth.HealExpiredUser(r.Context(), &rows[i])
				}
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"items":     rows,
				"users":     rows,
				"total":     total,
				"page":      page,
				"per_page":  perPage,
				"page_size": perPage,
			})

		case http.MethodPost:
			var req struct {
				Username        string `json:"username"`
				Email           string `json:"email"`
				DisplayName     string `json:"display_name"`
				Role            string `json:"role"`
				Password        string `json:"password"`
				PreferredLocale string `json:"preferred_locale"`
				CurrentPassword string `json:"current_password"`
			}
			if !decodeJSONBody(w, r, 16384, &req) {
				return
			}
			uname := strings.TrimSpace(req.Username)
			if writeBadErr(w, auth.ValidateUsername(uname)) {
				return
			}
			if writeBadErr(w, s.deps.Auth.EnsureUsernameNotMatchingEmail(r.Context(), uname, "")) {
				return
			}
			if writeBadErr(w, auth.ValidatePasswordPolicy(req.Password)) {
				return
			}
			role := strings.ToLower(strings.TrimSpace(req.Role))
			if role != domain.RoleEditor && role != domain.RoleAdmin && role != domain.RoleReader {
				role = domain.RoleEditor
			}
			if role == domain.RoleAdmin || role == domain.RoleEditor {
				if !s.verifyStepUpPassword(w, r, actor.ID, req.CurrentPassword) {
					return
				}
			}
			pwHash, err := auth.HashPassword(req.Password)
			if writeServerErr(w, err) {
				return
			}
			now := time.Now().UnixMilli()
			disp := strings.TrimSpace(req.DisplayName)
			if disp == "" {
				disp = uname
			}
			loc := s.deps.Catalog.Match(req.PreferredLocale, "")
			u := domain.User{
				ID:              id.Next(),
				Username:        uname,
				Email:           strings.ToLower(strings.TrimSpace(req.Email)),
				DisplayName:     disp,
				AvatarURL:       "/avatars/" + uname + ".svg",
				Role:            role,
				Status:          domain.UserStatusActive,
				EmailVerifiedAt: now,
				PreferredLocale: loc,
				CreatedAt:       now,
				UpdatedAt:       now,
			}
			if _, err := s.deps.Store.ExecContext(r.Context(),
				`INSERT INTO users (id, username, email, display_name, avatar_url, bio, website_url, role, password_hash, legacy_hash, status, email_verified_at, preferred_locale, created_at, updated_at)
				 VALUES (?, ?, ?, ?, ?, '', '', ?, ?, '', ?, ?, ?, ?, ?)`,
				u.ID, u.Username, u.Email, u.DisplayName, u.AvatarURL, u.Role, pwHash, u.Status, u.EmailVerifiedAt, u.PreferredLocale, now, now); err != nil {
				writeErrJSON(w, http.StatusBadRequest, err.Error())
				return
			}
			s.recordAuditLog(r, actor, "user.create", "user", u.ID, map[string]any{"role": u.Role, "username": u.Username})
			writeJSON(w, http.StatusCreated, map[string]any{"user": u})

		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
		return
	}

	if rest == actor.ID && r.Method == http.MethodDelete {
		writeErrJSON(w, http.StatusBadRequest, "cannot delete your own active admin account")
		return
	}
	switch r.Method {
	case http.MethodPatch:
		var req struct {
			Action          string   `json:"action"`
			DurationHours   *float64 `json:"duration_hours"`
			DisplayName     *string  `json:"display_name"`
			Email           *string  `json:"email"`
			PreferredLocale *string  `json:"preferred_locale"`
			Role            *string  `json:"role"`
			Status          *string  `json:"status"`
			BanUntil        *int64   `json:"ban_until"`
			MutedUntil      *int64   `json:"muted_until"`
			BanReason       *string  `json:"ban_reason"`
			ShieldComments  *bool    `json:"shield_comments"`
			ShieldText      *string  `json:"shield_text"`
			Password        string   `json:"password"`
			CurrentPassword string   `json:"current_password"`
		}
		if !decodeJSONBody(w, r, 16384, &req) {
			return
		}
		var u domain.User
		err := sqlx.GetInto(r.Context(), s.deps.Store, &u, `SELECT * FROM users WHERE id = ?`, rest)
		if err != nil {
			writeErrJSON(w, http.StatusNotFound, "user not found")
			return
		}
		if s.deps.Auth != nil {
			s.deps.Auth.HealExpiredUser(r.Context(), &u)
		}

		action := strings.ToLower(strings.TrimSpace(req.Action))
		isSelfModeration := action == "ban" || action == "mute" ||
			(req.Status != nil && *req.Status != domain.UserStatusActive) ||
			(req.BanUntil != nil && *req.BanUntil > 0) ||
			(req.MutedUntil != nil && *req.MutedUntil != 0) ||
			(req.ShieldComments != nil && *req.ShieldComments) ||
			(req.Role != nil && *req.Role != domain.RoleAdmin)
		if rest == actor.ID && isSelfModeration {
			writeErrJSON(w, http.StatusBadRequest, "cannot demote, ban, or mute your own admin account")
			return
		}

		roleChanged := req.Role != nil && (*req.Role == domain.RoleAdmin || *req.Role == domain.RoleEditor || *req.Role == domain.RoleReader) && *req.Role != u.Role
		emailChanged := req.Email != nil && strings.TrimSpace(*req.Email) != "" && !strings.EqualFold(strings.TrimSpace(*req.Email), u.Email)
		if roleChanged || emailChanged || req.Password != "" {
			if !s.verifyStepUpPassword(w, r, actor.ID, req.CurrentPassword) {
				return
			}
		}
		if req.DisplayName != nil && strings.TrimSpace(*req.DisplayName) != "" {
			u.DisplayName = strings.TrimSpace(*req.DisplayName)
		}
		if req.Email != nil && strings.TrimSpace(*req.Email) != "" {
			u.Email = strings.ToLower(strings.TrimSpace(*req.Email))
		}
		if req.PreferredLocale != nil {
			u.PreferredLocale = s.deps.Catalog.Match(*req.PreferredLocale, "")
		}
		if req.Role != nil && (*req.Role == domain.RoleAdmin || *req.Role == domain.RoleEditor || *req.Role == domain.RoleReader) {
			u.Role = *req.Role
		}

		nowMS := time.Now().UnixMilli()
		auditAction := "user.update"

		if req.Status != nil && (*req.Status == domain.UserStatusActive || *req.Status == domain.UserStatusDisabled) {
			u.Status = *req.Status
			if u.Status == domain.UserStatusActive {
				u.BanUntil = 0
			} else if req.BanUntil == nil && req.DurationHours == nil && action == "" {
				u.BanUntil = 0 // permanent ban when status explicitly set to disabled without duration
			}
		}
		if req.BanUntil != nil {
			if *req.BanUntil <= 0 {
				u.BanUntil = 0
			} else {
				u.BanUntil = *req.BanUntil
			}
		}
		if req.MutedUntil != nil {
			if *req.MutedUntil < 0 {
				u.MutedUntil = -1
			} else if *req.MutedUntil == 0 {
				u.MutedUntil = 0
			} else {
				u.MutedUntil = *req.MutedUntil
			}
		}
		if req.BanReason != nil {
			u.BanReason = sanitizeModerationText(*req.BanReason, 300)
		}
		if req.ShieldText != nil {
			u.ShieldText = sanitizeModerationText(*req.ShieldText, 240)
		}
		if req.ShieldComments != nil {
			u.ShieldComments = *req.ShieldComments
		}

		switch action {
		case "ban":
			u.Status = domain.UserStatusDisabled
			if req.DurationHours != nil && *req.DurationHours > 0 {
				dur := time.Duration(*req.DurationHours * float64(time.Hour))
				if dur > 10*365*24*time.Hour {
					dur = 10 * 365 * 24 * time.Hour
				}
				u.BanUntil = nowMS + dur.Milliseconds()
			} else if req.BanUntil == nil {
				u.BanUntil = 0 // permanent ban
			}
			auditAction = "user.ban"
		case "unban":
			u.Status = domain.UserStatusActive
			u.BanUntil = 0
			if !u.IsMutedAt(nowMS) {
				u.BanReason = ""
				if req.ShieldComments == nil {
					u.ShieldComments = false
				}
			}
			auditAction = "user.unban"
		case "mute":
			if req.DurationHours != nil && *req.DurationHours > 0 {
				dur := time.Duration(*req.DurationHours * float64(time.Hour))
				if dur > 10*365*24*time.Hour {
					dur = 10 * 365 * 24 * time.Hour
				}
				u.MutedUntil = nowMS + dur.Milliseconds()
			} else if req.MutedUntil == nil {
				u.MutedUntil = -1 // indefinite mute
			}
			auditAction = "user.mute"
		case "unmute":
			u.MutedUntil = 0
			if !u.IsBannedAt(nowMS) {
				u.BanReason = ""
				if req.ShieldComments == nil {
					u.ShieldComments = false
				}
			}
			auditAction = "user.unmute"
		case "":
			// Direct field updates handled above
		default:
			writeErrJSON(w, http.StatusBadRequest, "invalid user moderation action")
			return
		}

		// Reversible shielding only applies while banned or muted; once neither applies, restore comments automatically.
		if !u.IsBannedAt(nowMS) && !u.IsMutedAt(nowMS) {
			u.ShieldComments = false
		}

		if req.Password != "" {
			if writeBadErr(w, auth.ValidatePasswordPolicy(req.Password)) {
				return
			}
			u.PasswordHash, _ = auth.HashPassword(req.Password)
			u.LegacyHash = ""
		}
		u.UpdatedAt = nowMS
		_, err = s.deps.Store.ExecContext(r.Context(),
			`UPDATE users SET display_name = ?, email = ?, preferred_locale = ?, role = ?, status = ?, ban_until = ?, muted_until = ?, ban_reason = ?, shield_comments = ?, shield_text = ?, password_hash = ?, legacy_hash = ?, updated_at = ? WHERE id = ?`,
			u.DisplayName, u.Email, u.PreferredLocale, u.Role, u.Status, u.BanUntil, u.MutedUntil, u.BanReason, u.ShieldComments, u.ShieldText, u.PasswordHash, u.LegacyHash, nowMS, rest)
		if writeBadErr(w, err) {
			return
		}
		if u.IsBannedAt(nowMS) || req.Password != "" || roleChanged {
			_, _ = s.deps.Store.ExecContext(r.Context(), `DELETE FROM user_sessions WHERE user_id = ?`, rest)
			_, _ = s.deps.Store.ExecContext(r.Context(), `DELETE FROM user_credentials WHERE user_id = ? AND kind = 'oidc'`, rest)
			_ = s.deps.Auth.RevokeUserAPITokens(r.Context(), rest)
		}
		if s.deps.Content != nil {
			s.deps.Content.PurgeRuntimeCaches()
		}
		s.recordAuditLog(r, actor, auditAction, "user", rest, map[string]any{
			"role":             u.Role,
			"status":           u.Status,
			"ban_until":        u.BanUntil,
			"muted_until":      u.MutedUntil,
			"shield_comments":  u.ShieldComments,
			"self":             rest == actor.ID,
			"role_changed":     roleChanged,
			"email_changed":    emailChanged,
			"password_changed": req.Password != "",
		})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "user": u})

	case http.MethodDelete:
		_ = s.deps.Store.WithTx(r.Context(), func(tx *sqlx.Tx) error {
			_, _ = tx.ExecContext(r.Context(), `DELETE FROM user_sessions WHERE user_id = ?`, rest)
			_, _ = tx.ExecContext(r.Context(), `DELETE FROM user_credentials WHERE user_id = ?`, rest)
			_, _ = tx.ExecContext(r.Context(), `DELETE FROM api_tokens WHERE user_id = ?`, rest)
			_, err := tx.ExecContext(r.Context(), `DELETE FROM users WHERE id = ?`, rest)
			return err
		})
		if s.deps.Content != nil {
			s.deps.Content.PurgeRuntimeCaches()
		}
		s.recordAuditLog(r, actor, "user.delete", "user", rest, nil)
		writeOKJSON(w)

	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func sanitizeModerationText(raw string, maxRunes int) string {
	clean := strings.TrimSpace(repo.StripASCIIControls(raw))
	if clean == "" {
		return ""
	}
	runes := []rune(clean)
	if len(runes) > maxRunes {
		clean = string(runes[:maxRunes])
	}
	return clean
}

func (s *AppServer) handleAdminIPBans(w http.ResponseWriter, r *http.Request, actor *domain.User, rest string) {
	rest = strings.TrimPrefix(rest, "/")
	if rest == "batch" && r.Method == http.MethodPost {
		var req struct {
			Action string   `json:"action"`
			IDs    []string `json:"ids"`
		}
		if !decodeJSONBody(w, r, 65536, &req) {
			return
		}
		if strings.ToLower(strings.TrimSpace(req.Action)) != "delete" {
			writeErrJSON(w, http.StatusBadRequest, "invalid batch action")
			return
		}
		ids := dedupCleanIDs(req.IDs, 200)
		if len(ids) == 0 {
			writeErrJSON(w, http.StatusBadRequest, "ids required")
			return
		}
		affected := 0
		err := s.deps.Store.WithTx(r.Context(), func(tx *sqlx.Tx) error {
			for _, bid := range ids {
				res, dErr := tx.ExecContext(r.Context(), `DELETE FROM security_ip_bans WHERE id = ?`, bid)
				if dErr != nil {
					return dErr
				}
				if n, _ := res.RowsAffected(); n > 0 {
					affected += int(n)
				}
			}
			return nil
		})
		if writeBadErr(w, err) {
			return
		}
		if s.deps.WAF != nil {
			_ = s.deps.WAF.ReloadBans(r.Context())
		}
		s.recordAuditLog(r, actor, "security.ip_ban_batch_delete", "ip_ban", "batch", map[string]any{"affected": affected})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "affected": affected})
		return
	}
	if rest == "" {
		switch r.Method {
		case http.MethodGet:
			page, perPage := parseAdminPageParams(r, 50, 300)
			q := strings.TrimSpace(r.URL.Query().Get("q"))
			state := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("state")))
			now := time.Now().UnixMilli()

			where := []string{"1=1"}
			var args []any
			if q != "" {
				pat := "%" + escapeLikePattern(q) + "%"
				where = append(where, "(LOWER(cidr) LIKE LOWER(?) ESCAPE '!' OR LOWER(reason_code) LIKE LOWER(?) ESCAPE '!' OR LOWER(reason) LIKE LOWER(?) ESCAPE '!' OR LOWER(note) LIKE LOWER(?) ESCAPE '!')")
				args = append(args, pat, pat, pat, pat)
			}
			switch state {
			case "active":
				where = append(where, "(expires_at = 0 OR expires_at > ?)")
				args = append(args, now)
			case "expired":
				where = append(where, "(expires_at > 0 AND expires_at <= ?)")
				args = append(args, now)
			}
			whereSQL := strings.Join(where, " AND ")
			var total int
			_ = s.deps.Store.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM security_ip_bans WHERE `+whereSQL, args...).Scan(&total)
			baseSQL := `SELECT * FROM security_ip_bans WHERE ` + whereSQL + ` ORDER BY created_at DESC`
			qSQL, fullArgs := s.paginateQuery(baseSQL, args, page, perPage)
			var rows []domain.SecurityIPBan
			_ = sqlx.SelectInto(r.Context(), s.deps.Store, &rows, qSQL, fullArgs...)
			if rows == nil {
				rows = []domain.SecurityIPBan{}
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"items":     rows,
				"bans":      rows,
				"total":     total,
				"page":      page,
				"per_page":  perPage,
				"page_size": perPage,
			})

		case http.MethodPost:
			var req struct {
				CIDR         string `json:"cidr"`
				Note         string `json:"note"`
				ExpiresHours int    `json:"expires_hours"`
			}
			if !decodeJSONBody(w, r, 8192, &req) {
				return
			}
			var expMS int64
			if req.ExpiresHours > 0 {
				expMS = time.Now().Add(time.Duration(req.ExpiresHours) * time.Hour).UnixMilli()
			}
			clientIP := s.clientIP(r)
			rec, err := s.deps.WAF.AddManualBan(r.Context(), req.CIDR, req.Note, expMS, clientIP)
			if writeBadErr(w, err) {
				return
			}
			s.recordAuditLog(r, actor, "security.ip_ban_add", "ip_ban", rec.ID, map[string]any{"cidr": rec.CIDR})
			writeJSON(w, http.StatusCreated, map[string]any{"ban": rec})

		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
		return
	}

	if r.Method == http.MethodDelete {
		_, _ = s.deps.Store.ExecContext(r.Context(), `DELETE FROM security_ip_bans WHERE id = ?`, rest)
		_ = s.deps.WAF.ReloadBans(r.Context())
		s.recordAuditLog(r, actor, "security.ip_ban_remove", "ip_ban", rest, nil)
		writeOKJSON(w)
		return
	}
	w.WriteHeader(http.StatusMethodNotAllowed)
}

func normalizeGeoAction(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "allow":
		return "allow"
	case "block":
		return "block"
	case "challenge":
		return "challenge"
	default:
		return "deny"
	}
}

func normalizeGeoOnMiss(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "allow":
		return "allow"
	case "deny":
		return "deny"
	case "block":
		return "block"
	case "challenge":
		return "challenge"
	default:
		return "next"
	}
}

func (s *AppServer) handleAdminGeoRules(w http.ResponseWriter, r *http.Request, actor *domain.User, rest string) {
	rest = strings.TrimPrefix(rest, "/")
	if rest == "reload" && r.Method == http.MethodPost {
		var req struct {
			GeoIPDir *string `json:"geoip_dir"`
		}
		if !decodeOptJSONBody(w, r, 8192, &req) {
			return
		}
		var prevSec domain.SecuritySettings
		if s.deps.Settings != nil {
			prevSec = s.deps.Settings.Snapshot().Security
		}
		var secVer int64
		if req.GeoIPDir != nil && s.deps.Settings != nil {
			normDir := strings.TrimSpace(*req.GeoIPDir)
			if s.deps.Config != nil {
				validDir, vErr := s.deps.Config.ValidateCustomGeoIPPath(normDir)
				if vErr != nil {
					writeErrJSON(w, http.StatusBadRequest, vErr.Error())
					return
				}
				normDir = validDir
			}
			patchBytes, _ := json.Marshal(map[string]any{"geoip_dir": normDir})
			_, ver, pErr := s.deps.Settings.PatchDomain(r.Context(), domain.SettingsDomainSecurity, patchBytes, 0)
			if pErr != nil {
				writeErrJSON(w, http.StatusBadRequest, pErr.Error())
				return
			}
			secVer = ver
		} else if s.deps.Settings != nil {
			_, secVer, _ = s.deps.Settings.GetDomain(r.Context(), domain.SettingsDomainSecurity)
		}

		if lErr := s.syncConfiguredGeoIPDir(); lErr != nil {
			if req.GeoIPDir != nil {
				s.rollbackSecuritySettings(r, prevSec, true)
			}
			writeErrJSON(w, http.StatusBadRequest, lErr.Error())
			return
		}
		if s.deps.Geo != nil {
			_ = s.deps.Geo.ReloadRules(r.Context(), s.deps.Store)
		}
		if req.GeoIPDir != nil && s.deps.Config != nil && s.deps.Geo != nil && s.deps.Config.GeoAccessEnabled {
			clientIP := s.clientIP(r)
			if !s.deps.Geo.Allows(clientIP) {
				s.rollbackSecuritySettings(r, prevSec, true)
				writeErrJSON(w, http.StatusBadRequest, "refusing GeoIP database path that would immediately lock out your current IP")
				return
			}
		}

		configuredGeoIPDir, defaultGeoIPDir, effectiveGeoIPDir, geoStatus := s.geoIPMeta()
		s.recordAuditLog(r, actor, "security.geoip_reload", "settings", "security", map[string]any{
			"path":         effectiveGeoIPDir,
			"active_files": geoStatus.ActiveFiles,
		})
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":                  true,
			"geoip_dir":           configuredGeoIPDir,
			"default_geoip_dir":   defaultGeoIPDir,
			"effective_geoip_dir": effectiveGeoIPDir,
			"geoip_dir_pinned":    s.deps.Config != nil && s.deps.Config.IsGeoIPDirPinned(),
			"geoip_status":        geoStatus,
			"version":             secVer,
		})
		return
	}

	if rest == "" {
		switch r.Method {
		case http.MethodGet:
			page, perPage := parseAdminPageParams(r, 50, 200)
			var total int
			_ = s.deps.Store.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM security_geo_rules`).Scan(&total)
			baseSQL := `SELECT * FROM security_geo_rules ORDER BY sort_order ASC, created_at ASC`
			qSQL, fullArgs := s.paginateQuery(baseSQL, nil, page, perPage)
			var rows []domain.SecurityGeoRule
			_ = sqlx.SelectInto(r.Context(), s.deps.Store, &rows, qSQL, fullArgs...)
			if rows == nil {
				rows = []domain.SecurityGeoRule{}
			}
			configuredGeoIPDir, defaultGeoIPDir, effectiveGeoIPDir, geoStatus := s.geoIPMeta()
			writeJSON(w, http.StatusOK, map[string]any{
				"items":                      rows,
				"rules":                      rows,
				"total":                      total,
				"page":                       page,
				"per_page":                   perPage,
				"page_size":                  perPage,
				"geo_access_circuit_enabled": s.deps.Config != nil && s.deps.Config.GeoAccessEnabled,
				"geoip_dir":                  configuredGeoIPDir,
				"default_geoip_dir":          defaultGeoIPDir,
				"effective_geoip_dir":        effectiveGeoIPDir,
				"geoip_dir_pinned":           s.deps.Config != nil && s.deps.Config.IsGeoIPDirPinned(),
				"geoip_status":               geoStatus,
			})
		case http.MethodPost:
			var in domain.SecurityGeoRule
			if !decodeJSONBody(w, r, 16384, &in) {
				return
			}
			in.ID = id.Next()
			in.Name = strings.TrimSpace(in.Name)
			in.Action = normalizeGeoAction(in.Action)
			in.OnMiss = normalizeGeoOnMiss(in.OnMiss)
			now := time.Now().UnixMilli()
			in.CreatedAt = now
			in.UpdatedAt = now
			if _, err := s.deps.Store.ExecContext(r.Context(),
				`INSERT INTO security_geo_rules (id, name, is_enabled, action, on_miss, countries, regions, sort_order, created_at, updated_at)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				in.ID, in.Name, in.IsEnabled, in.Action, in.OnMiss, in.Countries, in.Regions, in.SortOrder, now, now); err != nil {
				writeErrJSON(w, http.StatusBadRequest, err.Error())
				return
			}
			_ = s.deps.Geo.ReloadRules(r.Context(), s.deps.Store)
			clientIP := s.clientIP(r)
			if s.deps.Config.GeoAccessEnabled && !s.deps.Geo.Allows(clientIP) {
				_, _ = s.deps.Store.ExecContext(r.Context(), `DELETE FROM security_geo_rules WHERE id = ?`, in.ID)
				_ = s.deps.Geo.ReloadRules(r.Context(), s.deps.Store)
				writeErrJSON(w, http.StatusBadRequest, "refusing GeoIP rule that would immediately lock out your current IP")
				return
			}
			s.recordAuditLog(r, actor, "security.geo_rule_add", "geo_rule", in.ID, nil)
			writeJSON(w, http.StatusCreated, map[string]any{"rule": in})
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
		return
	}

	switch r.Method {
	case http.MethodPut:
		var prev domain.SecurityGeoRule
		pErr := sqlx.GetInto(r.Context(), s.deps.Store, &prev, `SELECT * FROM security_geo_rules WHERE id = ?`, rest)
		if pErr != nil {
			writeErrJSON(w, http.StatusNotFound, "geo rule not found")
			return
		}
		var in domain.SecurityGeoRule
		if !decodeJSONBody(w, r, 16384, &in) {
			return
		}
		in.Name = strings.TrimSpace(in.Name)
		in.Action = normalizeGeoAction(in.Action)
		in.OnMiss = normalizeGeoOnMiss(in.OnMiss)
		now := time.Now().UnixMilli()
		if _, err := s.deps.Store.ExecContext(r.Context(),
			`UPDATE security_geo_rules SET name = ?, is_enabled = ?, action = ?, on_miss = ?, countries = ?, regions = ?, sort_order = ?, updated_at = ? WHERE id = ?`,
			in.Name, in.IsEnabled, in.Action, in.OnMiss, in.Countries, in.Regions, in.SortOrder, now, rest); err != nil {
			writeErrJSON(w, http.StatusBadRequest, err.Error())
			return
		}
		_ = s.deps.Geo.ReloadRules(r.Context(), s.deps.Store)
		clientIP := s.clientIP(r)
		if s.deps.Config.GeoAccessEnabled && !s.deps.Geo.Allows(clientIP) {
			_, _ = s.deps.Store.ExecContext(r.Context(),
				`UPDATE security_geo_rules SET name = ?, is_enabled = ?, action = ?, on_miss = ?, countries = ?, regions = ?, sort_order = ?, updated_at = ? WHERE id = ?`,
				prev.Name, prev.IsEnabled, prev.Action, prev.OnMiss, prev.Countries, prev.Regions, prev.SortOrder, prev.UpdatedAt, rest)
			_ = s.deps.Geo.ReloadRules(r.Context(), s.deps.Store)
			writeErrJSON(w, http.StatusBadRequest, "refusing GeoIP rule update that would immediately lock out your current IP")
			return
		}
		s.recordAuditLog(r, actor, "security.geo_rule_update", "geo_rule", rest, nil)
		writeOKJSON(w)

	case http.MethodDelete:
		_, _ = s.deps.Store.ExecContext(r.Context(), `DELETE FROM security_geo_rules WHERE id = ?`, rest)
		_ = s.deps.Geo.ReloadRules(r.Context(), s.deps.Store)
		writeOKJSON(w)

	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *AppServer) handleAdminBackupExport(w http.ResponseWriter, r *http.Request, actor *domain.User) {
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	withMedia := r.URL.Query().Get("media") == "1" || r.URL.Query().Get("media") == "true" ||
		r.URL.Query().Get("include_media") == "1" || r.URL.Query().Get("include_media") == "true"
	filename := fmt.Sprintf("volo-backup-%s.zip", time.Now().UTC().Format("20060102-150405"))
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	s.recordAuditLog(r, actor, "backup.export", "system", filename, map[string]any{"include_media": withMedia})
	_, _ = backup.Create(r.Context(), s.deps.Store, w, backup.CreateOptions{
		IncludeMedia: withMedia,
		UploadDir:    s.deps.Config.GetUploadDir(),
		PluginsDir:   s.pluginsDir(),
		ThemesDir:    s.themesDir(),
	})
}

type localBackupItem struct {
	Name      string `json:"name"`
	SizeBytes int64  `json:"size_bytes"`
	CreatedAt int64  `json:"created_at"`
}

func (s *AppServer) listLocalBackups(backupDir string) []localBackupItem {
	entries, err := os.ReadDir(backupDir)
	if err != nil {
		return []localBackupItem{}
	}
	items := make([]localBackupItem, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".zip") {
			continue
		}
		info, iErr := e.Info()
		if iErr != nil {
			continue
		}
		items = append(items, localBackupItem{
			Name:      e.Name(),
			SizeBytes: info.Size(),
			CreatedAt: info.ModTime().UnixMilli(),
		})
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i].CreatedAt > items[j].CreatedAt
	})
	return items
}

func (s *AppServer) serveLocalBackupArchive(w http.ResponseWriter, r *http.Request, actor *domain.User, base string) {
	clean := filepath.Base(base)
	if clean != base || strings.Contains(clean, "..") || !strings.HasSuffix(strings.ToLower(clean), ".zip") {
		writeErrJSON(w, http.StatusBadRequest, "invalid backup filename")
		return
	}
	fullPath := filepath.Join(s.resolveBackupDir(), clean)
	st, err := os.Lstat(fullPath)
	if err != nil || !st.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(fullPath)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.IsDir() {
		http.NotFound(w, r)
		return
	}
	s.recordAuditLog(r, actor, "backup.local_download", "system", clean, nil)
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, clean))
	http.ServeContent(w, r, clean, info.ModTime(), f)
}

func (s *AppServer) handleAdminLocalBackups(w http.ResponseWriter, r *http.Request, actor *domain.User, sess *domain.UserSession, rest string) {
	rest = strings.TrimPrefix(rest, "/")
	backupDir := s.resolveBackupDir()

	if rest == "" {
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, http.StatusOK, map[string]any{"items": s.listLocalBackups(backupDir)})
		case http.MethodPost:
			sched := s.deps.BackupScheduler
			if sched == nil {
				sched = backup.NewScheduler(backup.SchedulerOptions{
					Store:          s.deps.Store,
					Settings:       s.deps.Settings,
					Plugins:        s.deps.Plugins,
					BackupDir:      backupDir,
					UploadDir:      s.deps.Config.GetUploadDir(),
					PluginsDir:     s.pluginsDir(),
					ThemesDir:      s.themesDir(),
					OutboundDialer: s.deps.OutboundDialer,
				})
			}
			res, err := sched.RunOnce(r.Context())
			if err != nil && (res == nil || res.LocalPath == "") {
				writeErrJSON(w, http.StatusInternalServerError, err.Error())
				return
			}
			warn := ""
			if err != nil {
				warn = err.Error()
			}
			s.recordAuditLog(r, actor, "backup.local_snapshot", "system", filepath.Base(res.LocalPath), nil)
			writeJSON(w, http.StatusOK, map[string]any{
				"ok":              true,
				"file":            filepath.Base(res.LocalPath),
				"pruned_count":    res.PrunedCount,
				"remote_uploaded": res.RemoteUploaded,
				"remote_warning":  warn,
				"items":           s.listLocalBackups(backupDir),
			})
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
		return
	}

	parts := strings.Split(rest, "/")
	if len(parts) == 2 && parts[1] == "ticket" && r.Method == http.MethodPost {
		if sess == nil || sess.ID == "" {
			writeErrJSON(w, http.StatusForbidden, "interactive admin session required for backup download ticket")
			return
		}
		base := filepath.Base(parts[0])
		if base != parts[0] || strings.Contains(base, "..") || !strings.HasSuffix(strings.ToLower(base), ".zip") {
			writeErrJSON(w, http.StatusBadRequest, "invalid backup filename")
			return
		}
		fullPath := filepath.Join(backupDir, base)
		st, err := os.Lstat(fullPath)
		if err != nil || !st.Mode().IsRegular() {
			http.NotFound(w, r)
			return
		}
		var req struct {
			CurrentPassword string `json:"current_password"`
		}
		if !decodeOptJSONBody(w, r, 4096, &req) {
			return
		}
		if !s.verifyStepUpPassword(w, r, actor.ID, req.CurrentPassword) {
			return
		}
		ipBucket := security.RateLimitBucketKey(s.clientIP(r))
		tok, err := s.mintBackupExportTicket(actor.ID, sess.ID, ipBucket, base)
		if writeServerErr(w, err) {
			return
		}
		s.recordAuditLog(r, actor, "backup.local_download_ticket", "system", base, nil)
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":           true,
			"ticket":       tok,
			"expires_in":   60,
			"download_url": "/api/admin/backups/local/" + url.PathEscape(base) + "?ticket=" + tok,
		})
		return
	}

	if len(parts) == 2 && parts[1] == "restore" && r.Method == http.MethodPost {
		base := filepath.Base(parts[0])
		if base != parts[0] || strings.Contains(base, "..") || !strings.HasSuffix(strings.ToLower(base), ".zip") {
			writeErrJSON(w, http.StatusBadRequest, "invalid backup filename")
			return
		}
		fullPath := filepath.Join(backupDir, base)
		st, err := os.Lstat(fullPath)
		if err != nil || !st.Mode().IsRegular() {
			http.NotFound(w, r)
			return
		}
		dryRun, _ := strconv.ParseBool(r.URL.Query().Get("dry_run"))
		var bodyReq struct {
			DryRun          bool   `json:"dry_run"`
			CurrentPassword string `json:"current_password"`
		}
		if !decodeOptJSONBody(w, r, 4096, &bodyReq) {
			return
		}
		if bodyReq.DryRun {
			dryRun = true
		}
		if !dryRun {
			curPwd := bodyReq.CurrentPassword
			if strings.TrimSpace(curPwd) == "" {
				curPwd = r.URL.Query().Get("current_password")
			}
			if !s.verifyStepUpPassword(w, r, actor.ID, curPwd) {
				return
			}
		}
		if isLegacyArchiveFile(fullPath) {
			rep, lErr := etl.ImportLegacyArchive(r.Context(), s.deps.Store, s.deps.Settings, s.deps.Config.KeyRing, fullPath, etl.ImportOptions{
				DryRun:          dryRun,
				TargetUploadDir: s.deps.Config.GetUploadDir(),
				AdminPath:       s.deps.Config.AdminPath,
			})
			if lErr != nil {
				writeErrJSON(w, http.StatusBadRequest, lErr.Error())
				return
			}
			if !dryRun {
				_, _ = s.deps.Settings.Reload(r.Context())
				_, _, _ = s.deps.Settings.PatchDomain(r.Context(), domain.SettingsDomainSite, []byte(`{"initialized":true}`), 0)
				s.reloadRuntimeAfterDBRestore(r.Context())
				s.recordAuditLog(r, actor, "backup.local_restore", "system", base, map[string]any{"legacy": true})
			}
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "dry_run": dryRun, "legacy": true, "report": rep})
			return
		}

		sum, rErr := backup.RestoreFromFile(r.Context(), s.deps.Store, fullPath, backup.RestoreOptions{
			DryRun:     dryRun,
			UploadDir:  s.deps.Config.GetUploadDir(),
			PluginsDir: s.pluginsDir(),
			ThemesDir:  s.themesDir(),
		})
		if rErr != nil {
			writeErrJSON(w, http.StatusBadRequest, rErr.Error())
			return
		}
		if !dryRun {
			s.reloadRuntimeAfterDBRestore(r.Context())
			s.recordAuditLog(r, actor, "backup.local_restore", "system", base, nil)
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "dry_run": dryRun, "summary": sum})
		return
	}

	base := filepath.Base(rest)
	if base != rest || strings.Contains(base, "..") || !strings.HasSuffix(strings.ToLower(base), ".zip") {
		writeErrJSON(w, http.StatusBadRequest, "invalid backup filename")
		return
	}
	fullPath := filepath.Join(backupDir, base)

	switch r.Method {
	case http.MethodGet:
		if sess != nil {
			writeErrJSON(w, http.StatusForbidden, "step-up password ticket required for interactive local backup download")
			return
		}
		s.serveLocalBackupArchive(w, r, actor, base)
	case http.MethodDelete:
		if err := os.Remove(fullPath); err != nil && !os.IsNotExist(err) {
			writeErrJSON(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.recordAuditLog(r, actor, "backup.local_delete", "system", base, nil)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "items": s.listLocalBackups(backupDir)})
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func isLegacyArchiveFile(archivePath string) bool {
	if strings.HasSuffix(strings.ToLower(archivePath), ".json") {
		return true
	}
	zr, err := zip.OpenReader(archivePath)
	if err != nil {
		return false
	}
	defer zr.Close()
	hasManifest := false
	hasJSON := false
	for _, f := range zr.File {
		name := strings.ToLower(strings.TrimPrefix(filepath.ToSlash(f.Name), "./"))
		if name == "manifest.json" {
			hasManifest = true
		}
		if strings.HasSuffix(name, ".json") {
			hasJSON = true
		}
	}
	return !hasManifest && hasJSON
}

func (s *AppServer) handleAdminBackupRestore(w http.ResponseWriter, r *http.Request, actor *domain.User) {
	const maxRestoreUpload = 512 << 20
	uploaded, sErr := streamMultipartSingleFile(w, r, "file", maxRestoreUpload, s.ensureTempDir(), "volo-restore-", ".zip", ".zip", ".json")
	if sErr != nil {
		writeErrJSON(w, http.StatusBadRequest, sErr.Error())
		return
	}
	tmpPath := uploaded.TempPath
	defer os.Remove(tmpPath)

	dryRunRaw := strings.TrimSpace(uploaded.FormFields["dry_run"])
	if dryRunRaw == "" {
		dryRunRaw = strings.TrimSpace(r.URL.Query().Get("dry_run"))
	}
	dryRun, _ := strconv.ParseBool(dryRunRaw)
	if !dryRun {
		curPwd := uploaded.FormFields["current_password"]
		if strings.TrimSpace(curPwd) == "" {
			curPwd = r.URL.Query().Get("current_password")
		}
		if !s.verifyStepUpPassword(w, r, actor.ID, curPwd) {
			return
		}
	}
	if isLegacyArchiveFile(tmpPath) {
		rep, lErr := etl.ImportLegacyArchive(r.Context(), s.deps.Store, s.deps.Settings, s.deps.Config.KeyRing, tmpPath, etl.ImportOptions{
			DryRun:          dryRun,
			TargetUploadDir: s.deps.Config.GetUploadDir(),
			AdminPath:       s.deps.Config.AdminPath,
		})
		if lErr != nil {
			writeErrJSON(w, http.StatusBadRequest, lErr.Error())
			return
		}
		if !dryRun {
			_, _ = s.deps.Settings.Reload(r.Context())
			_, _, _ = s.deps.Settings.PatchDomain(r.Context(), domain.SettingsDomainSite, []byte(`{"initialized":true}`), 0)
			s.reloadRuntimeAfterDBRestore(r.Context())
			s.recordAuditLog(r, actor, "backup.restore_legacy", "system", "legacy", nil)
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "legacy": true, "report": rep})
		return
	}

	sum, err := backup.RestoreFromFile(r.Context(), s.deps.Store, tmpPath, backup.RestoreOptions{
		DryRun:     dryRun,
		UploadDir:  s.deps.Config.GetUploadDir(),
		PluginsDir: s.pluginsDir(),
		ThemesDir:  s.themesDir(),
	})
	if writeBadErr(w, err) {
		return
	}
	if !dryRun {
		s.reloadRuntimeAfterDBRestore(r.Context())
		s.recordAuditLog(r, actor, "backup.restore", "system", "native", nil)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "summary": sum})
}
