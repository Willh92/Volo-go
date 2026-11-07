package server

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Willh92/Volo-go/internal/auth"
	"github.com/Willh92/Volo-go/internal/backup"
	"github.com/Willh92/Volo-go/internal/content"
	"github.com/Willh92/Volo-go/internal/db/sqlx"
	"github.com/Willh92/Volo-go/internal/doctor"
	"github.com/Willh92/Volo-go/internal/domain"
	"github.com/Willh92/Volo-go/internal/etl"
	"github.com/Willh92/Volo-go/internal/logging"
	"github.com/Willh92/Volo-go/internal/repo"
	"github.com/Willh92/Volo-go/internal/security"
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
	clientIP := security.ResolveClientIP(r, s.deps.Config.TrustedProxies)
	detJSON := "{}"
	if detail != nil {
		if b, err := json.Marshal(detail); err == nil {
			detJSON = string(b)
		}
	}
	_, _ = s.deps.Store.ExecContext(r.Context(),
		`INSERT INTO audit_logs (id, actor_id, actor_name, action, resource_type, resource_id, client_ip, detail_json, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id.Next(), actor.ID, actor.Username, action, resourceType, resourceID, security.MaskIP(clientIP), detJSON, time.Now().UnixMilli())
}

// handleAdminSystemRoutes serves Workspaces 3, 4, and 5.
// SECURITY RED-LINE: All System, Settings, Appearance/Plugins, Users, WAF/GeoIP, and Backup routes
// require `RoleAdmin` (`Editor` physically receives `403 Forbidden`)!
func (s *AppServer) handleAdminSystemRoutes(w http.ResponseWriter, r *http.Request, sub string) {
	if sub == "backup/export" {
		actor, _, ok := s.authenticateAdminRequest(w, r, auth.ScopeBackupRead, true)
		if !ok {
			return
		}
		s.handleAdminBackupExport(w, r, actor)
		return
	}

	actor, _, ok := s.authenticateAdminRequest(w, r, "", true)
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
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
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
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		s.recordAuditLog(r, actor, "github.publish_showcase", "article", art.ID, map[string]any{"slug": art.Slug})
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":           true,
			"article_id":   art.ID,
			"article_slug": art.Slug,
			"article_url":  "/posts/" + art.Slug,
		})

	case sub == "seo/indexnow-push" && r.Method == http.MethodPost:
		n, err := s.pushIndexNowURLs(r.Context(), s.publicBaseURL(r))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
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
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		s.recordAuditLog(r, actor, "security.rotate_share_secret", "settings", "security", nil)
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})

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
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})

	case sub == "audit-logs" && r.Method == http.MethodGet:
		act := strings.TrimSpace(r.URL.Query().Get("action"))
		q := strings.TrimSpace(r.URL.Query().Get("q"))
		where := []string{"1=1"}
		var args []any
		if act != "" && act != "all" {
			where = append(where, "action = ?")
			args = append(args, act)
		}
		if q != "" {
			pat := "%" + q + "%"
			where = append(where, "(LOWER(actor_name) LIKE LOWER(?) OR LOWER(action) LIKE LOWER(?) OR LOWER(resource_type) LIKE LOWER(?) OR LOWER(resource_id) LIKE LOWER(?) OR LOWER(detail_json) LIKE LOWER(?) OR LOWER(client_ip) LIKE LOWER(?))")
			args = append(args, pat, pat, pat, pat, pat, pat)
		}
		rows, _ := sqlx.Select[domain.AuditLog](r.Context(), s.deps.Store,
			`SELECT * FROM audit_logs WHERE `+strings.Join(where, " AND ")+` ORDER BY created_at DESC LIMIT 200`, args...)
		if rows == nil {
			rows = []domain.AuditLog{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": rows})

	case sub == "backup/local" || strings.HasPrefix(sub, "backup/local/"):
		s.handleAdminLocalBackups(w, r, actor, strings.TrimPrefix(sub, "backup/local"))

	case sub == "backup/restore" && r.Method == http.MethodPost:
		s.handleAdminBackupRestore(w, r, actor)

	case sub == "backup/import-legacy" && r.Method == http.MethodPost:
		rep, dryRun, newSiteURL, err := s.streamLegacyImportUpload(w, r, false)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if !dryRun {
			if newSiteURL != "" {
				s.deps.Config.PublicURL = strings.TrimRight(newSiteURL, "/")
				if s.deps.WAF != nil {
					s.deps.WAF.SetPublicURL(s.deps.Config.PublicURL)
				}
			}
			_, _ = s.deps.Settings.Reload(r.Context())
			_ = s.deps.Content.WarmCaches(r.Context())
			if s.deps.WAF != nil {
				_ = s.deps.WAF.ReloadBans(r.Context())
			}
			if s.deps.Geo != nil {
				_ = s.deps.Geo.ReloadRules(r.Context(), s.deps.Store)
			}
			s.recordAuditLog(r, actor, "backup.import_legacy", "system", "etl", nil)
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "dry_run": dryRun, "report": rep})

	case sub == "doctor" && r.Method == http.MethodGet:
		rep := doctor.Run(r.Context(), s.deps.Config, s.deps.Store)
		writeJSON(w, http.StatusOK, rep)

	default:
		http.NotFound(w, r)
	}
}

func (s *AppServer) handleAdminNav(w http.ResponseWriter, r *http.Request, _ *domain.User, rest string) {
	rest = strings.TrimPrefix(rest, "/")
	if rest == "" {
		switch r.Method {
		case http.MethodGet:
			rows, _ := sqlx.Select[domain.NavItem](r.Context(), s.deps.Store, `SELECT * FROM nav_items ORDER BY sort_order ASC, created_at ASC`)
			if rows == nil {
				rows = []domain.NavItem{}
			}
			writeJSON(w, http.StatusOK, map[string]any{"items": rows})
		case http.MethodPost:
			var in domain.NavItem
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384)).Decode(&in); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
				return
			}
			in.ID = id.Next()
			in.Title = strings.TrimSpace(in.Title)
			in.URL = strings.TrimSpace(in.URL)
			if in.Title == "" || in.URL == "" {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "title and url required"})
				return
			}
			if strings.TrimSpace(in.I18nTitlesJSON) == "" {
				in.I18nTitlesJSON = "{}"
			}
			now := time.Now().UnixMilli()
			in.CreatedAt = now
			in.UpdatedAt = now
			_, err := s.deps.Store.ExecContext(r.Context(),
				`INSERT INTO nav_items (id, parent_id, title, i18n_titles_json, url, open_in_new_tab, icon, sort_order, is_enabled, created_at, updated_at)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				in.ID, in.ParentID, in.Title, in.I18nTitlesJSON, in.URL, in.OpenInNewTab, in.Icon, in.SortOrder, in.IsEnabled, now, now)
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
			writeJSON(w, http.StatusCreated, map[string]any{"item": in})
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
		return
	}
	switch r.Method {
	case http.MethodPut:
		var in domain.NavItem
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384)).Decode(&in); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
			return
		}
		if strings.TrimSpace(in.I18nTitlesJSON) == "" {
			in.I18nTitlesJSON = "{}"
		}
		_, err := s.deps.Store.ExecContext(r.Context(),
			`UPDATE nav_items SET parent_id = ?, title = ?, i18n_titles_json = ?, url = ?, open_in_new_tab = ?, icon = ?, sort_order = ?, is_enabled = ?, updated_at = ? WHERE id = ?`,
			in.ParentID, strings.TrimSpace(in.Title), in.I18nTitlesJSON, strings.TrimSpace(in.URL),
			in.OpenInNewTab, in.Icon, in.SortOrder, in.IsEnabled, time.Now().UnixMilli(), rest)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	case http.MethodDelete:
		_, _ = s.deps.Store.ExecContext(r.Context(), `DELETE FROM nav_items WHERE id = ? OR parent_id = ?`, rest, rest)
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *AppServer) handleAdminLinks(w http.ResponseWriter, r *http.Request, actor *domain.User, rest string) {
	rest = strings.TrimPrefix(rest, "/")
	if rest == "patrol" && r.Method == http.MethodPost {
		n, err := s.patrolAllFriendLinks(r.Context())
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		rows, _ := sqlx.Select[domain.Link](r.Context(), s.deps.Store, `SELECT * FROM links ORDER BY sort_order ASC`)
		writeJSON(w, http.StatusOK, map[string]any{"checked": n, "items": rows})
		return
	}
	if rest == "batch" && r.Method == http.MethodPost {
		var req struct {
			Action       string   `json:"action"`
			IDs          []string `json:"ids"`
			CategoryName string   `json:"category_name"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 65536)).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
			return
		}
		action := strings.ToLower(strings.TrimSpace(req.Action))
		if action != "delete" && action != "category" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid batch action"})
			return
		}
		ids := dedupCleanIDs(req.IDs, 200)
		if len(ids) == 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "ids required"})
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
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		s.recordAuditLog(r, actor, "link.batch", "link", action, map[string]any{"action": action, "affected": affected})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "affected": affected})
		return
	}
	if rest == "" {
		switch r.Method {
		case http.MethodGet:
			rows, _ := sqlx.Select[domain.Link](r.Context(), s.deps.Store, `SELECT * FROM links ORDER BY sort_order ASC, created_at DESC`)
			if rows == nil {
				rows = []domain.Link{}
			}
			writeJSON(w, http.StatusOK, map[string]any{"items": rows})
		case http.MethodPost:
			var in domain.Link
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384)).Decode(&in); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
				return
			}
			in.ID = id.Next()
			in.Title = strings.TrimSpace(in.Title)
			in.URL = strings.TrimSpace(in.URL)
			if in.Title == "" || in.URL == "" {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "title and url required"})
				return
			}
			in.HealthStatus = "unknown"
			now := time.Now().UnixMilli()
			in.CreatedAt = now
			in.UpdatedAt = now
			if _, err := s.deps.Store.ExecContext(r.Context(),
				`INSERT INTO links (id, title, url, description, icon_url, category_name, sort_order, health_status, http_code, latency_ms, health_error, last_checked_at, created_at, updated_at)
				 VALUES (?, ?, ?, ?, ?, ?, ?, 'unknown', 0, 0, '', 0, ?, ?)`,
				in.ID, in.Title, in.URL, in.Description, in.IconURL, in.CategoryName, in.SortOrder, now, now); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
			writeJSON(w, http.StatusCreated, map[string]any{"link": in})
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
		return
	}
	switch r.Method {
	case http.MethodPut:
		var in domain.Link
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384)).Decode(&in); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
			return
		}
		_, err := s.deps.Store.ExecContext(r.Context(),
			`UPDATE links SET title = ?, url = ?, description = ?, icon_url = ?, category_name = ?, sort_order = ?, updated_at = ? WHERE id = ?`,
			strings.TrimSpace(in.Title), strings.TrimSpace(in.URL), in.Description, in.IconURL, in.CategoryName, in.SortOrder, time.Now().UnixMilli(), rest)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	case http.MethodDelete:
		_, _ = s.deps.Store.ExecContext(r.Context(), `DELETE FROM links WHERE id = ?`, rest)
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *AppServer) handleAdminFollows(w http.ResponseWriter, r *http.Request, _ *domain.User, rest string) {
	rest = strings.TrimPrefix(rest, "/")
	if rest == "sync" && r.Method == http.MethodPost {
		n, err := s.syncAllFollowFeeds(r.Context())
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
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
			rows, _ := sqlx.Select[domain.Follow](r.Context(), s.deps.Store, `SELECT * FROM follows ORDER BY sort_order ASC, created_at DESC`)
			if rows == nil {
				rows = []domain.Follow{}
			}
			writeJSON(w, http.StatusOK, map[string]any{"items": rows})
		case http.MethodPost:
			var in domain.Follow
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384)).Decode(&in); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
				return
			}
			in.ID = id.Next()
			in.Title = strings.TrimSpace(in.Title)
			in.FeedURL = strings.TrimSpace(in.FeedURL)
			if in.Title == "" || in.FeedURL == "" {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "title and feed_url required"})
				return
			}
			now := time.Now().UnixMilli()
			in.CreatedAt = now
			in.UpdatedAt = now
			if _, err := s.deps.Store.ExecContext(r.Context(),
				`INSERT INTO follows (id, title, site_url, feed_url, description, icon_url, sort_order, last_synced_at, sync_error, created_at, updated_at)
				 VALUES (?, ?, ?, ?, ?, ?, ?, 0, '', ?, ?)`,
				in.ID, in.Title, in.SiteURL, in.FeedURL, in.Description, in.IconURL, in.SortOrder, now, now); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
			writeJSON(w, http.StatusCreated, map[string]any{"follow": in})
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
		return
	}
	switch r.Method {
	case http.MethodPut:
		var in domain.Follow
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384)).Decode(&in); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
			return
		}
		_, err := s.deps.Store.ExecContext(r.Context(),
			`UPDATE follows SET title = ?, site_url = ?, feed_url = ?, description = ?, icon_url = ?, sort_order = ?, updated_at = ? WHERE id = ?`,
			strings.TrimSpace(in.Title), strings.TrimSpace(in.SiteURL), strings.TrimSpace(in.FeedURL),
			in.Description, in.IconURL, in.SortOrder, time.Now().UnixMilli(), rest)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	case http.MethodDelete:
		_ = s.deps.Store.WithTx(r.Context(), func(tx *sqlx.Tx) error {
			_, _ = tx.ExecContext(r.Context(), `DELETE FROM follow_items WHERE follow_id = ?`, rest)
			_, err := tx.ExecContext(r.Context(), `DELETE FROM follows WHERE id = ?`, rest)
			return err
		})
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *AppServer) handleAdminRedirects(w http.ResponseWriter, r *http.Request, actor *domain.User, rest string) {
	rest = strings.TrimPrefix(rest, "/")
	if rest == "batch" && r.Method == http.MethodPost {
		var req struct {
			Action string   `json:"action"`
			IDs    []string `json:"ids"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 65536)).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
			return
		}
		if strings.ToLower(strings.TrimSpace(req.Action)) != "delete" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid batch action"})
			return
		}
		ids := dedupCleanIDs(req.IDs, 200)
		if len(ids) == 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "ids required"})
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
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		s.recordAuditLog(r, actor, "redirect.batch_delete", "redirect", "batch", map[string]any{"affected": affected})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "affected": affected})
		return
	}
	if rest == "" {
		switch r.Method {
		case http.MethodGet:
			rows, _ := sqlx.Select[domain.RedirectRule](r.Context(), s.deps.Store, `SELECT * FROM redirects ORDER BY created_at DESC LIMIT 300`)
			if rows == nil {
				rows = []domain.RedirectRule{}
			}
			writeJSON(w, http.StatusOK, map[string]any{"items": rows})
		case http.MethodPost:
			var in domain.RedirectRule
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&in); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
				return
			}
			src := "/" + strings.TrimLeft(strings.TrimSpace(in.SourcePath), "/")
			dst := strings.TrimSpace(in.TargetPath)
			if src == "/" || dst == "" || src == dst {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "valid distinct source_path and target_path required"})
				return
			}
			if in.StatusCode != 301 && in.StatusCode != 302 {
				in.StatusCode = 301
			}
			in.ID = id.Next()
			in.SourcePath = src
			in.TargetPath = dst
			in.CreatedAt = time.Now().UnixMilli()
			if _, err := s.deps.Store.ExecContext(r.Context(),
				`INSERT INTO redirects (id, source_path, target_path, status_code, is_system_legacy, hit_count, created_at)
				 VALUES (?, ?, ?, ?, ?, 0, ?)`,
				in.ID, in.SourcePath, in.TargetPath, in.StatusCode, false, in.CreatedAt); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
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
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&in); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
			return
		}
		src := "/" + strings.TrimLeft(strings.TrimSpace(in.SourcePath), "/")
		dst := strings.TrimSpace(in.TargetPath)
		if src == "/" || dst == "" || src == dst {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "valid distinct source_path and target_path required"})
			return
		}
		if in.StatusCode != 301 && in.StatusCode != 302 {
			in.StatusCode = 301
		}
		if _, err := s.deps.Store.ExecContext(r.Context(),
			`UPDATE redirects SET source_path = ?, target_path = ?, status_code = ? WHERE id = ?`,
			src, dst, in.StatusCode, rest); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	case http.MethodDelete:
		_, _ = s.deps.Store.ExecContext(r.Context(), `DELETE FROM redirects WHERE id = ?`, rest)
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *AppServer) handleAdminWebhooks(w http.ResponseWriter, r *http.Request, actor *domain.User, rest string) {
	rest = strings.TrimPrefix(rest, "/")
	if rest == "" {
		switch r.Method {
		case http.MethodGet:
			rows, _ := sqlx.Select[domain.Webhook](r.Context(), s.deps.Store, `SELECT * FROM webhooks ORDER BY created_at DESC`)
			out := make([]map[string]any, 0, len(rows))
			for _, wh := range rows {
				hasSec := wh.SecretCiphertext != ""
				wh.SecretCiphertext = ""
				out = append(out, map[string]any{"webhook": wh, "secret_set": hasSec})
			}
			writeJSON(w, http.StatusOK, map[string]any{"items": out})

		case http.MethodPost:
			var in struct {
				domain.Webhook
				Secret string   `json:"secret"`
				Events []string `json:"events"`
			}
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384)).Decode(&in); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
				return
			}
			in.ID = id.Next()
			in.Name = strings.TrimSpace(in.Name)
			in.URL = strings.TrimSpace(in.URL)
			if in.Name == "" || in.URL == "" {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name and url required"})
				return
			}
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
				secCipher, sErr = s.deps.Config.KeyRing.SealString(strings.TrimSpace(in.Secret), "webhook:"+in.ID)
				if sErr != nil {
					writeJSON(w, http.StatusInternalServerError, map[string]string{"error": sErr.Error()})
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
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
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
	wh, err := sqlx.Get[domain.Webhook](r.Context(), s.deps.Store, `SELECT * FROM webhooks WHERE id = ?`, whID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "webhook not found"})
		return
	}
	if len(parts) == 2 && parts[1] == "test" && r.Method == http.MethodPost {
		if s.deps.TestWebhook == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "webhook dispatcher not wired"})
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
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384)).Decode(&in); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
			return
		}
		secCipher := wh.SecretCiphertext
		if in.ClearSecret {
			secCipher = ""
		} else if strings.TrimSpace(in.Secret) != "" {
			secCipher, _ = s.deps.Config.KeyRing.SealString(strings.TrimSpace(in.Secret), "webhook:"+whID)
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
			strings.TrimSpace(in.Name), in.Provider, strings.TrimSpace(in.URL), secCipher, in.ChatID, evJSON, in.IsEnabled, time.Now().UnixMilli(), whID)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})

	case http.MethodDelete:
		_, _ = s.deps.Store.ExecContext(r.Context(), `DELETE FROM webhooks WHERE id = ?`, whID)
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})

	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *AppServer) handleAdminThemes(w http.ResponseWriter, r *http.Request, actor *domain.User, rest string) {
	rest = strings.TrimPrefix(rest, "/")
	locale := actor.PreferredLocale
	if locale == "" {
		locale = s.resolveLocale(r)
	}

	if rest == "" && r.Method == http.MethodGet {
		snap := s.deps.Settings.Snapshot()
		writeJSON(w, http.StatusOK, map[string]any{
			"active_theme": snap.Appearance.ActiveTheme,
			"mobile_theme": snap.Appearance.MobileTheme,
			"appearance":   snap.Appearance,
			"themes":       s.deps.Themes.ListLocalized(locale),
		})
		return
	}

	if rest == "reload" && r.Method == http.MethodPost {
		if err := s.deps.Themes.Reload(); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		snap := s.deps.Settings.Snapshot()
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":           true,
			"active_theme": snap.Appearance.ActiveTheme,
			"mobile_theme": snap.Appearance.MobileTheme,
			"appearance":   snap.Appearance,
			"themes":       s.deps.Themes.ListLocalized(locale),
		})
		return
	}

	if rest == "activate" && r.Method == http.MethodPost {
		var req struct {
			ThemeID string `json:"theme_id"`
			Target  string `json:"target"` // desktop (default) | mobile | both
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
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
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "theme not found: " + tid})
				return
			}
			patchMap["mobile_theme"] = tid
		case "both":
			if !s.deps.Themes.Has(tid) {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "theme not found: " + tid})
				return
			}
			patchMap["active_theme"] = tid
			patchMap["mobile_theme"] = tid
		default:
			if !s.deps.Themes.Has(tid) {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "theme not found: " + tid})
				return
			}
			patchMap["active_theme"] = tid
		}
		patch, _ := json.Marshal(patchMap)
		val, ver, err := s.deps.Settings.PatchDomain(r.Context(), domain.SettingsDomainAppearance, patch, 0)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		s.recordAuditLog(r, actor, "theme.activate", "theme", tid, map[string]any{"target": target})
		writeJSON(w, http.StatusOK, map[string]any{"appearance": val, "version": ver})
		return
	}

	if rest == "install" && r.Method == http.MethodPost {
		r.Body = http.MaxBytesReader(w, r.Body, 33<<20)
		if err := r.ParseMultipartForm(8 << 20); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "theme .zip upload exceeds 32MB limit"})
			return
		}
		f, _, err := r.FormFile("file")
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing 'file' field"})
			return
		}
		defer f.Close()
		tmp, err := os.CreateTemp(s.deps.Config.DataDir, "volo-theme-*.zip")
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		tmpName := tmp.Name()
		defer os.Remove(tmpName)
		sz, err := io.Copy(tmp, io.LimitReader(f, 32<<20+1))
		if err != nil {
			_ = tmp.Close()
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		_, _ = tmp.Seek(0, io.SeekStart)
		overwrite := r.FormValue("overwrite") == "1" || strings.EqualFold(r.FormValue("overwrite"), "true")
		mf, iErr := s.deps.Themes.InstallZip(tmp, sz, overwrite)
		_ = tmp.Close()
		if iErr != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": iErr.Error()})
			return
		}
		s.recordAuditLog(r, actor, "theme.install", "theme", mf.ID, map[string]any{"version": mf.Version})
		writeJSON(w, http.StatusCreated, map[string]any{
			"theme":  mf,
			"themes": s.deps.Themes.ListLocalized(locale),
		})
		return
	}

	parts := strings.Split(rest, "/")
	themeID := strings.TrimSpace(parts[0])
	if len(parts) == 2 && parts[1] == "config" && (r.Method == http.MethodPut || r.Method == http.MethodPatch) {
		if !s.deps.Themes.Has(themeID) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "theme not found: " + themeID})
			return
		}
		var cfgPatch map[string]any
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 65536)).Decode(&cfgPatch); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
			return
		}
		cur := s.deps.Settings.Snapshot().Appearance
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
		val, ver, err := s.deps.Settings.PatchDomain(r.Context(), domain.SettingsDomainAppearance, patchBytes, 0)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		s.recordAuditLog(r, actor, "theme.config", "theme", themeID, nil)
		writeJSON(w, http.StatusOK, map[string]any{"appearance": val, "version": ver})
		return
	}

	if len(parts) == 1 && themeID != "" && r.Method == http.MethodDelete {
		if err := s.deps.Themes.Uninstall(themeID); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
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
		s.recordAuditLog(r, actor, "theme.uninstall", "theme", themeID, nil)
		nextApp := s.deps.Settings.Snapshot().Appearance
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":           true,
			"active_theme": nextApp.ActiveTheme,
			"mobile_theme": nextApp.MobileTheme,
			"themes":       s.deps.Themes.ListLocalized(locale),
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
		if err := s.deps.Catalog.ReloadDir(); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
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
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&req); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
				return
			}
			raw, _ := json.Marshal(req.Messages)
			ow := req.Overwrite == nil || *req.Overwrite
			sum, err := s.deps.Catalog.InstallPackJSON(req.Code, raw, ow)
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
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

		r.Body = http.MaxBytesReader(w, r.Body, 8<<20)
		if err := r.ParseMultipartForm(8 << 20); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "language pack upload exceeds 8MB limit"})
			return
		}
		f, hdr, err := r.FormFile("file")
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing 'file' field"})
			return
		}
		defer f.Close()
		raw, err := io.ReadAll(io.LimitReader(f, 8<<20+1))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		overwrite := r.FormValue("overwrite") == "" || r.FormValue("overwrite") == "1" || strings.EqualFold(r.FormValue("overwrite"), "true")
		fname := strings.TrimSpace(hdr.Filename)
		if strings.HasSuffix(strings.ToLower(fname), ".zip") {
			installed, zErr := s.deps.Catalog.InstallPackZip(bytes.NewReader(raw), int64(len(raw)), overwrite)
			if zErr != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": zErr.Error()})
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

		codeHint := strings.TrimSpace(r.FormValue("code"))
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
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": jErr.Error()})
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
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		resolved := s.deps.Catalog.Match(code, "")
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.json"`, resolved))
		_, _ = w.Write(data)
		return
	}

	if len(parts) == 1 && code != "" && r.Method == http.MethodDelete {
		if err := s.deps.Catalog.DeleteExternalPack(code); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
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
	locale := actor.PreferredLocale
	if locale == "" {
		locale = s.deps.Settings.Snapshot().Site.DefaultLocale
	}
	rest = strings.TrimPrefix(rest, "/")

	if rest == "" && r.Method == http.MethodGet {
		writeJSON(w, http.StatusOK, map[string]any{"items": s.deps.Plugins.List(locale)})
		return
	}

	if rest == "reload" && r.Method == http.MethodPost {
		_ = s.deps.Plugins.DiscoverAndStart(r.Context())
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "items": s.deps.Plugins.List(locale)})
		return
	}

	if rest == "install" && r.Method == http.MethodPost {
		r.Body = http.MaxBytesReader(w, r.Body, 33<<20)
		if err := r.ParseMultipartForm(8 << 20); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "plugin .zip upload exceeds 32MB limit"})
			return
		}
		f, _, err := r.FormFile("file")
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing 'file' field"})
			return
		}
		defer f.Close()
		tmp, err := os.CreateTemp(s.deps.Config.DataDir, "volo-plugin-*.zip")
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		tmpName := tmp.Name()
		defer os.Remove(tmpName)
		sz, err := io.Copy(tmp, io.LimitReader(f, 32<<20+1))
		if err != nil {
			_ = tmp.Close()
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		_, _ = tmp.Seek(0, io.SeekStart)
		overwrite := r.FormValue("overwrite") == "1" || strings.EqualFold(r.FormValue("overwrite"), "true")
		sum, iErr := s.deps.Plugins.InstallFromZip(r.Context(), tmp, sz, overwrite, locale)
		_ = tmp.Close()
		if iErr != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": iErr.Error()})
			return
		}
		s.recordAuditLog(r, actor, "plugin.install", "plugin", sum.ID, map[string]any{"version": sum.Version})
		writeJSON(w, http.StatusCreated, map[string]any{"plugin": sum})
		return
	}

	parts := strings.Split(rest, "/")
	pid := parts[0]
	if len(parts) == 2 {
		switch {
		case parts[1] == "enable" && r.Method == http.MethodPost:
			if err := s.deps.Plugins.Enable(r.Context(), pid); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
			s.recordAuditLog(r, actor, "plugin.enable", "plugin", pid, nil)
			sum, _ := s.deps.Plugins.Get(pid, locale)
			writeJSON(w, http.StatusOK, map[string]any{"plugin": sum})
		case parts[1] == "disable" && r.Method == http.MethodPost:
			if err := s.deps.Plugins.Disable(r.Context(), pid); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
			s.recordAuditLog(r, actor, "plugin.disable", "plugin", pid, nil)
			sum, _ := s.deps.Plugins.Get(pid, locale)
			writeJSON(w, http.StatusOK, map[string]any{"plugin": sum})
		case parts[1] == "reload" && r.Method == http.MethodPost:
			sum, err := s.deps.Plugins.ReloadPlugin(r.Context(), pid, locale)
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
			s.recordAuditLog(r, actor, "plugin.reload", "plugin", pid, nil)
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "plugin": sum})
		case parts[1] == "reset-circuit" && r.Method == http.MethodPost:
			if err := s.deps.Plugins.ResetCircuit(r.Context(), pid); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
			sum, _ := s.deps.Plugins.Get(pid, locale)
			writeJSON(w, http.StatusOK, map[string]any{"plugin": sum})
		case parts[1] == "config" && r.Method == http.MethodPatch:
			var patch map[string]any
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 65536)).Decode(&patch); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
				return
			}
			sum, err := s.deps.Plugins.UpdateConfig(r.Context(), pid, patch, locale)
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
			s.recordAuditLog(r, actor, "plugin.config_patch", "plugin", pid, nil)
			writeJSON(w, http.StatusOK, map[string]any{"plugin": sum})
		case parts[1] == "logs" && r.Method == http.MethodGet:
			entries, _ := s.deps.Plugins.Logger().Tail(pid, 100)
			writeJSON(w, http.StatusOK, map[string]any{"items": entries})
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
		return
	}

	if len(parts) == 1 && r.Method == http.MethodDelete {
		if err := s.deps.Plugins.Uninstall(r.Context(), pid); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		s.recordAuditLog(r, actor, "plugin.uninstall", "plugin", pid, nil)
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}
	w.WriteHeader(http.StatusMethodNotAllowed)
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
				result[d] = val
				versions[d] = ver
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"domains": result, "versions": versions})
		return
	}

	if rest == "notifications/test-smtp" && r.Method == http.MethodPost {
		var req struct {
			ToEmail  string                        `json:"to_email"`
			Settings *domain.NotificationsSettings `json:"settings"`
			Password string                        `json:"smtp_password"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384)).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
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
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "SMTP mailer not wired"})
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
		if s.deps.Media != nil {
			if err := s.deps.Media.TestStorage(r.Context(), req.Settings, req.SecretKey); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
				return
			}
		} else {
			if err := os.MkdirAll(s.deps.Config.UploadDir, 0o750); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
				return
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}

	switch r.Method {
	case http.MethodGet:
		val, ver, err := s.deps.Settings.GetDomain(r.Context(), rest)
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		w.Header().Set("ETag", fmt.Sprintf(`"%d"`, ver))
		writeJSON(w, http.StatusOK, map[string]any{"domain": rest, "value": val, "version": ver})

	case http.MethodPatch:
		rawBytes, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 256*1024))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		var meta struct {
			ExpectedVersion int64 `json:"expected_version"`
		}
		_ = json.Unmarshal(rawBytes, &meta)
		var patchMap map[string]any
		if err := json.Unmarshal(rawBytes, &patchMap); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
			return
		}
		delete(patchMap, "expected_version")
		cleanPatch, _ := json.Marshal(patchMap)
		expectedVer := parseVersionHeader(r, meta.ExpectedVersion)

		val, ver, pErr := s.deps.Settings.PatchDomain(r.Context(), rest, cleanPatch, expectedVer)
		if errors.Is(pErr, repo.ErrSettingsConflict) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": pErr.Error()})
			return
		}
		if pErr != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": pErr.Error()})
			return
		}
		s.recordAuditLog(r, actor, "settings.patch", "settings", rest, map[string]any{"version": ver})
		w.Header().Set("ETag", fmt.Sprintf(`"%d"`, ver))
		writeJSON(w, http.StatusOK, map[string]any{"domain": rest, "value": val, "version": ver})

	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *AppServer) handleAdminUsers(w http.ResponseWriter, r *http.Request, actor *domain.User, rest string) {
	rest = strings.TrimPrefix(rest, "/")
	if rest == "" {
		switch r.Method {
		case http.MethodGet:
			rows, _ := sqlx.Select[domain.User](r.Context(), s.deps.Store,
				`SELECT id, username, email, display_name, avatar_url, bio, website_url, role, status, email_verified_at, preferred_locale, created_at, updated_at
				 FROM users ORDER BY created_at ASC LIMIT 300`)
			if rows == nil {
				rows = []domain.User{}
			}
			writeJSON(w, http.StatusOK, map[string]any{"items": rows})

		case http.MethodPost:
			var req struct {
				Username        string `json:"username"`
				Email           string `json:"email"`
				DisplayName     string `json:"display_name"`
				Role            string `json:"role"`
				Password        string `json:"password"`
				PreferredLocale string `json:"preferred_locale"`
			}
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384)).Decode(&req); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
				return
			}
			if err := auth.ValidatePasswordPolicy(req.Password); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
			role := strings.ToLower(strings.TrimSpace(req.Role))
			if role != domain.RoleEditor && role != domain.RoleAdmin && role != domain.RoleReader {
				role = domain.RoleEditor
			}
			pwHash, err := auth.HashPassword(req.Password)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
				return
			}
			now := time.Now().UnixMilli()
			uname := strings.TrimSpace(req.Username)
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
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
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
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "cannot delete your own active admin account"})
		return
	}
	switch r.Method {
	case http.MethodPatch:
		var req struct {
			DisplayName     *string `json:"display_name"`
			Email           *string `json:"email"`
			PreferredLocale *string `json:"preferred_locale"`
			Role            *string `json:"role"`
			Status          *string `json:"status"`
			Password        string  `json:"password"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
			return
		}
		u, err := sqlx.Get[domain.User](r.Context(), s.deps.Store, `SELECT * FROM users WHERE id = ?`, rest)
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "user not found"})
			return
		}
		if rest == actor.ID && ((req.Status != nil && *req.Status != domain.UserStatusActive) || (req.Role != nil && *req.Role != domain.RoleAdmin)) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "cannot demote or disable your own admin account"})
			return
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
		if req.Status != nil && (*req.Status == domain.UserStatusActive || *req.Status == domain.UserStatusDisabled) {
			u.Status = *req.Status
		}
		if req.Password != "" {
			if err := auth.ValidatePasswordPolicy(req.Password); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
			u.PasswordHash, _ = auth.HashPassword(req.Password)
			u.LegacyHash = ""
		}
		_, err = s.deps.Store.ExecContext(r.Context(),
			`UPDATE users SET display_name = ?, email = ?, preferred_locale = ?, role = ?, status = ?, password_hash = ?, legacy_hash = ?, updated_at = ? WHERE id = ?`,
			u.DisplayName, u.Email, u.PreferredLocale, u.Role, u.Status, u.PasswordHash, u.LegacyHash, time.Now().UnixMilli(), rest)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if u.Status == domain.UserStatusDisabled || req.Password != "" {
			_, _ = s.deps.Store.ExecContext(r.Context(), `DELETE FROM user_sessions WHERE user_id = ?`, rest)
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})

	case http.MethodDelete:
		_ = s.deps.Store.WithTx(r.Context(), func(tx *sqlx.Tx) error {
			_, _ = tx.ExecContext(r.Context(), `DELETE FROM user_sessions WHERE user_id = ?`, rest)
			_, _ = tx.ExecContext(r.Context(), `DELETE FROM user_credentials WHERE user_id = ?`, rest)
			_, _ = tx.ExecContext(r.Context(), `DELETE FROM api_tokens WHERE user_id = ?`, rest)
			_, err := tx.ExecContext(r.Context(), `DELETE FROM users WHERE id = ?`, rest)
			return err
		})
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})

	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *AppServer) handleAdminIPBans(w http.ResponseWriter, r *http.Request, actor *domain.User, rest string) {
	rest = strings.TrimPrefix(rest, "/")
	if rest == "" {
		switch r.Method {
		case http.MethodGet:
			rows, _ := sqlx.Select[domain.SecurityIPBan](r.Context(), s.deps.Store,
				`SELECT * FROM security_ip_bans ORDER BY created_at DESC LIMIT 300`)
			if rows == nil {
				rows = []domain.SecurityIPBan{}
			}
			writeJSON(w, http.StatusOK, map[string]any{"items": rows})

		case http.MethodPost:
			var req struct {
				CIDR         string `json:"cidr"`
				Note         string `json:"note"`
				ExpiresHours int    `json:"expires_hours"`
			}
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&req); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
				return
			}
			var expMS int64
			if req.ExpiresHours > 0 {
				expMS = time.Now().Add(time.Duration(req.ExpiresHours) * time.Hour).UnixMilli()
			}
			clientIP := security.ResolveClientIP(r, s.deps.Config.TrustedProxies)
			rec, err := s.deps.WAF.AddManualBan(r.Context(), req.CIDR, req.Note, expMS, clientIP)
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
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
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}
	w.WriteHeader(http.StatusMethodNotAllowed)
}

func (s *AppServer) handleAdminGeoRules(w http.ResponseWriter, r *http.Request, actor *domain.User, rest string) {
	rest = strings.TrimPrefix(rest, "/")
	if rest == "" {
		switch r.Method {
		case http.MethodGet:
			rows, _ := sqlx.Select[domain.SecurityGeoRule](r.Context(), s.deps.Store,
				`SELECT * FROM security_geo_rules ORDER BY sort_order ASC, created_at ASC`)
			if rows == nil {
				rows = []domain.SecurityGeoRule{}
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"items":                      rows,
				"geo_access_circuit_enabled": s.deps.Config.GeoAccessEnabled,
			})
		case http.MethodPost:
			var in domain.SecurityGeoRule
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384)).Decode(&in); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
				return
			}
			in.ID = id.Next()
			in.Name = strings.TrimSpace(in.Name)
			if in.Action != "allow" && in.Action != "deny" {
				in.Action = "deny"
			}
			if in.OnMiss != "next" && in.OnMiss != "allow" && in.OnMiss != "deny" {
				in.OnMiss = "next"
			}
			now := time.Now().UnixMilli()
			in.CreatedAt = now
			in.UpdatedAt = now
			if _, err := s.deps.Store.ExecContext(r.Context(),
				`INSERT INTO security_geo_rules (id, name, is_enabled, action, on_miss, countries, regions, sort_order, created_at, updated_at)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				in.ID, in.Name, in.IsEnabled, in.Action, in.OnMiss, in.Countries, in.Regions, in.SortOrder, now, now); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
			_ = s.deps.Geo.ReloadRules(r.Context(), s.deps.Store)
			clientIP := security.ResolveClientIP(r, s.deps.Config.TrustedProxies)
			if s.deps.Config.GeoAccessEnabled && !s.deps.Geo.Allows(clientIP) {
				_, _ = s.deps.Store.ExecContext(r.Context(), `DELETE FROM security_geo_rules WHERE id = ?`, in.ID)
				_ = s.deps.Geo.ReloadRules(r.Context(), s.deps.Store)
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "refusing GeoIP rule that would immediately lock out your current IP"})
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
		prev, pErr := sqlx.Get[domain.SecurityGeoRule](r.Context(), s.deps.Store, `SELECT * FROM security_geo_rules WHERE id = ?`, rest)
		if pErr != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "geo rule not found"})
			return
		}
		var in domain.SecurityGeoRule
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384)).Decode(&in); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
			return
		}
		in.Name = strings.TrimSpace(in.Name)
		if in.Action != "allow" && in.Action != "deny" {
			in.Action = "deny"
		}
		if in.OnMiss != "next" && in.OnMiss != "allow" && in.OnMiss != "deny" {
			in.OnMiss = "next"
		}
		now := time.Now().UnixMilli()
		if _, err := s.deps.Store.ExecContext(r.Context(),
			`UPDATE security_geo_rules SET name = ?, is_enabled = ?, action = ?, on_miss = ?, countries = ?, regions = ?, sort_order = ?, updated_at = ? WHERE id = ?`,
			in.Name, in.IsEnabled, in.Action, in.OnMiss, in.Countries, in.Regions, in.SortOrder, now, rest); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		_ = s.deps.Geo.ReloadRules(r.Context(), s.deps.Store)
		clientIP := security.ResolveClientIP(r, s.deps.Config.TrustedProxies)
		if s.deps.Config.GeoAccessEnabled && !s.deps.Geo.Allows(clientIP) {
			_, _ = s.deps.Store.ExecContext(r.Context(),
				`UPDATE security_geo_rules SET name = ?, is_enabled = ?, action = ?, on_miss = ?, countries = ?, regions = ?, sort_order = ?, updated_at = ? WHERE id = ?`,
				prev.Name, prev.IsEnabled, prev.Action, prev.OnMiss, prev.Countries, prev.Regions, prev.SortOrder, prev.UpdatedAt, rest)
			_ = s.deps.Geo.ReloadRules(r.Context(), s.deps.Store)
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "refusing GeoIP rule update that would immediately lock out your current IP"})
			return
		}
		s.recordAuditLog(r, actor, "security.geo_rule_update", "geo_rule", rest, nil)
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})

	case http.MethodDelete:
		_, _ = s.deps.Store.ExecContext(r.Context(), `DELETE FROM security_geo_rules WHERE id = ?`, rest)
		_ = s.deps.Geo.ReloadRules(r.Context(), s.deps.Store)
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})

	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *AppServer) handleAdminBackupExport(w http.ResponseWriter, r *http.Request, actor *domain.User) {
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	withMedia := r.URL.Query().Get("media") == "1" || r.URL.Query().Get("media") == "true"
	filename := fmt.Sprintf("volo-backup-%s.zip", time.Now().UTC().Format("20060102-150405"))
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	s.recordAuditLog(r, actor, "backup.export", "system", filename, map[string]any{"include_media": withMedia})
	_, _ = backup.Create(r.Context(), s.deps.Store, w, backup.CreateOptions{
		IncludeMedia: withMedia,
		UploadDir:    s.deps.Config.UploadDir,
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

func (s *AppServer) handleAdminLocalBackups(w http.ResponseWriter, r *http.Request, actor *domain.User, rest string) {
	rest = strings.TrimPrefix(rest, "/")
	backupDir := filepath.Join(s.deps.Config.DataDir, "backups")

	if rest == "" {
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, http.StatusOK, map[string]any{"items": s.listLocalBackups(backupDir)})
		case http.MethodPost:
			sched := backup.NewScheduler(backup.SchedulerOptions{
				Store:          s.deps.Store,
				Settings:       s.deps.Settings,
				BackupDir:      backupDir,
				UploadDir:      s.deps.Config.UploadDir,
				OutboundDialer: s.deps.OutboundDialer,
			})
			res, err := sched.RunOnce(r.Context())
			if err != nil && (res == nil || res.LocalPath == "") {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
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
	if len(parts) == 2 && parts[1] == "restore" && r.Method == http.MethodPost {
		base := filepath.Base(parts[0])
		if base != parts[0] || strings.Contains(base, "..") || !strings.HasSuffix(strings.ToLower(base), ".zip") {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid backup filename"})
			return
		}
		fullPath := filepath.Join(backupDir, base)
		st, err := os.Lstat(fullPath)
		if err != nil || !st.Mode().IsRegular() {
			http.NotFound(w, r)
			return
		}
		dryRun, _ := strconv.ParseBool(r.URL.Query().Get("dry_run"))
		if !dryRun && r.ContentLength > 0 {
			var bodyReq struct {
				DryRun bool `json:"dry_run"`
			}
			if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&bodyReq) == nil && bodyReq.DryRun {
				dryRun = true
			}
		}
		if isLegacyArchiveFile(fullPath) {
			rep, lErr := etl.ImportLegacyArchive(r.Context(), s.deps.Store, s.deps.Settings, s.deps.Config.KeyRing, fullPath, etl.ImportOptions{
				DryRun:          dryRun,
				TargetUploadDir: s.deps.Config.UploadDir,
				AdminPath:       s.deps.Config.AdminPath,
			})
			if lErr != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": lErr.Error()})
				return
			}
			if !dryRun {
				_, _ = s.deps.Settings.Reload(r.Context())
				_, _, _ = s.deps.Settings.PatchDomain(r.Context(), domain.SettingsDomainSite, []byte(`{"initialized":true}`), 0)
				_ = s.deps.Content.WarmCaches(r.Context())
				if s.deps.WAF != nil {
					_ = s.deps.WAF.ReloadBans(r.Context())
				}
				if s.deps.Geo != nil {
					_ = s.deps.Geo.ReloadRules(r.Context(), s.deps.Store)
				}
				s.recordAuditLog(r, actor, "backup.local_restore", "system", base, map[string]any{"legacy": true})
			}
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "dry_run": dryRun, "legacy": true, "report": rep})
			return
		}

		sum, rErr := backup.RestoreFromFile(r.Context(), s.deps.Store, fullPath, backup.RestoreOptions{
			DryRun:    dryRun,
			UploadDir: s.deps.Config.UploadDir,
		})
		if rErr != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": rErr.Error()})
			return
		}
		if !dryRun {
			_, _ = s.deps.Settings.Reload(r.Context())
			_ = s.deps.Content.WarmCaches(r.Context())
			if s.deps.WAF != nil {
				_ = s.deps.WAF.ReloadBans(r.Context())
			}
			if s.deps.Geo != nil {
				_ = s.deps.Geo.ReloadRules(r.Context(), s.deps.Store)
			}
			s.recordAuditLog(r, actor, "backup.local_restore", "system", base, nil)
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "dry_run": dryRun, "summary": sum})
		return
	}

	base := filepath.Base(rest)
	if base != rest || strings.Contains(base, "..") || !strings.HasSuffix(strings.ToLower(base), ".zip") {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid backup filename"})
		return
	}
	fullPath := filepath.Join(backupDir, base)

	switch r.Method {
	case http.MethodGet:
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
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, base))
		http.ServeContent(w, r, base, info.ModTime(), f)
	case http.MethodDelete:
		if err := os.Remove(fullPath); err != nil && !os.IsNotExist(err) {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
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
	r.Body = http.MaxBytesReader(w, r.Body, maxRestoreUpload)
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid multipart backup archive"})
		return
	}
	f, hdr, err := r.FormFile("file")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing 'file' field"})
		return
	}
	defer f.Close()

	ext := strings.ToLower(filepath.Ext(hdr.Filename))
	if ext != ".zip" && ext != ".json" {
		ext = ".zip"
	}
	tmp, err := os.CreateTemp(s.deps.Config.DataDir, "volo-restore-*"+ext)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if _, err := io.Copy(tmp, io.LimitReader(f, maxRestoreUpload)); err != nil {
		_ = tmp.Close()
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	_ = tmp.Close()

	dryRun, _ := strconv.ParseBool(r.FormValue("dry_run"))
	if isLegacyArchiveFile(tmpPath) {
		rep, lErr := etl.ImportLegacyArchive(r.Context(), s.deps.Store, s.deps.Settings, s.deps.Config.KeyRing, tmpPath, etl.ImportOptions{
			DryRun:          dryRun,
			TargetUploadDir: s.deps.Config.UploadDir,
			AdminPath:       s.deps.Config.AdminPath,
		})
		if lErr != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": lErr.Error()})
			return
		}
		if !dryRun {
			_, _ = s.deps.Settings.Reload(r.Context())
			_, _, _ = s.deps.Settings.PatchDomain(r.Context(), domain.SettingsDomainSite, []byte(`{"initialized":true}`), 0)
			_ = s.deps.Content.WarmCaches(r.Context())
			if s.deps.WAF != nil {
				_ = s.deps.WAF.ReloadBans(r.Context())
			}
			if s.deps.Geo != nil {
				_ = s.deps.Geo.ReloadRules(r.Context(), s.deps.Store)
			}
			s.recordAuditLog(r, actor, "backup.restore_legacy", "system", "legacy", nil)
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "legacy": true, "report": rep})
		return
	}

	sum, err := backup.RestoreFromFile(r.Context(), s.deps.Store, tmpPath, backup.RestoreOptions{
		DryRun:    dryRun,
		UploadDir: s.deps.Config.UploadDir,
	})
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if !dryRun {
		_, _ = s.deps.Settings.Reload(r.Context())
		_ = s.deps.Content.WarmCaches(r.Context())
		if s.deps.WAF != nil {
			_ = s.deps.WAF.ReloadBans(r.Context())
		}
		if s.deps.Geo != nil {
			_ = s.deps.Geo.ReloadRules(r.Context(), s.deps.Store)
		}
		s.recordAuditLog(r, actor, "backup.restore", "system", "native", nil)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "summary": sum})
}
