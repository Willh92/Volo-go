package server

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/Willh92/Volo-go/internal/adminui"
	"github.com/Willh92/Volo-go/internal/auth"
	"github.com/Willh92/Volo-go/internal/db/sqlx"
	"github.com/Willh92/Volo-go/internal/domain"
	"github.com/Willh92/Volo-go/internal/media"
	"github.com/Willh92/Volo-go/internal/render"
	"github.com/Willh92/Volo-go/internal/repo"
	"github.com/Willh92/Volo-go/internal/security"
	"github.com/Willh92/Volo-go/internal/visibility"
	"github.com/Willh92/Volo-go/pkg/id"
	"github.com/Willh92/Volo-go/pkg/plugin"
)

const trusted2FACookieName = "__volo_2fa_trust"

// MediaProcessor allows M6's full EXIF-stripping + adaptive WebP + S3 + 9-scope orphan pipeline
// to plug seamlessly into AppServer while keeping M5 local uploads functional out-of-the-box.
type MediaProcessor interface {
	Upload(ctx context.Context, uploaderID string, origFilename string, contentType string, r io.Reader) (*domain.MediaAsset, error)
	Delete(ctx context.Context, assetID string) error
	ScanOrphans(ctx context.Context, deleteOrphans bool) ([]domain.MediaAsset, error)
	TestStorage(ctx context.Context, override *domain.StorageSettings, overrideSecret string) error
}

func (s *AppServer) registerAdminRoutes(mux *http.ServeMux) {
	adminPrefix := "/" + strings.Trim(strings.TrimSpace(s.deps.Config.AdminPath), "/")
	if adminPrefix == "/" {
		adminPrefix = "/console"
	}
	mux.HandleFunc(adminPrefix, s.handleAdminSPA)
	mux.HandleFunc(adminPrefix+"/", s.handleAdminSPA)
	mux.HandleFunc("/api/admin/", s.handleAdminAPI)
}

func (s *AppServer) handleAdminSPA(w http.ResponseWriter, r *http.Request) {
	adminPrefix := "/" + strings.Trim(strings.TrimSpace(s.deps.Config.AdminPath), "/")
	snap := s.deps.Settings.Snapshot()
	adminui.Default().ServeMount(w, r, adminPrefix, adminui.BootConfig{
		Mode:          "console",
		AdminPath:     adminPrefix,
		PublicURL:     s.publicBaseURL(r),
		DefaultLocale: snap.Site.DefaultLocale,
		Accent:        snap.Appearance.Accent,
		DefaultTheme:  snap.Appearance.DefaultMode,
		SiteTitle:     snap.Site.Title,
		Locales:       s.deps.Catalog.Locales(),
	})
}

// authenticateAdminRequest resolves either:
// 1. A scoped `Authorization: Bearer volo_pat_...` Personal Access Token (stateless, no CSRF needed), OR
// 2. An active `__volo_sid` Staff Session Cookie (Admin or Editor; Reader physically rejected with 403!),
//    enforcing `X-CSRF-Token` on all mutating HTTP verbs (`POST`, `PUT`, `PATCH`, `DELETE`).
func (s *AppServer) authenticateAdminRequest(w http.ResponseWriter, r *http.Request, requiredPATScope string, requireAdminOnly bool) (*domain.User, *domain.UserSession, bool) {
	authHdr := strings.TrimSpace(r.Header.Get("Authorization"))
	if strings.HasPrefix(authHdr, "Bearer ") {
		if requiredPATScope == "" {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "this endpoint requires an interactive admin session"})
			return nil, nil, false
		}
		actor, _, err := s.deps.Auth.AuthenticateAPIToken(r.Context(), authHdr, requiredPATScope)
		if errors.Is(err, auth.ErrForbidden) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
			return nil, nil, false
		}
		if err != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid or expired API token"})
			return nil, nil, false
		}
		if requireAdminOnly {
			if aErr := auth.RequireAdmin(actor); aErr != nil {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": aErr.Error()})
				return nil, nil, false
			}
		}
		totpScope := s.deps.Settings.Snapshot().Security.TOTPRequiredScope
		if totpScope == "staff" || (totpScope == "admin" && actor.Role == domain.RoleAdmin) {
			var cnt int
			_ = s.deps.Store.QueryRowContext(r.Context(),
				`SELECT COUNT(*) FROM user_credentials WHERE user_id = ? AND kind = 'totp'`, actor.ID).Scan(&cnt)
			if cnt == 0 {
				writeJSON(w, http.StatusForbidden, map[string]any{
					"error":              "TOTP two-factor enrollment is required by security policy before using API tokens",
					"requires_totp_init": true,
				})
				return nil, nil, false
			}
		}
		return actor, nil, true
	}

	c, err := r.Cookie(auth.SessionCookieName)
	if err != nil || strings.TrimSpace(c.Value) == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": s.deps.Catalog.T(s.resolveLocale(r), "error.unauthorized")})
		return nil, nil, false
	}
	actor, sess, err := s.deps.Auth.AuthenticateSession(r.Context(), c.Value)
	if err != nil || actor == nil || sess == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": s.deps.Catalog.T(s.resolveLocale(r), "error.unauthorized")})
		return nil, nil, false
	}

	// Physical RBAC boundary: `reader` accounts NEVER pass RequireStaff!
	if err := auth.RequireStaff(actor); err != nil {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": s.deps.Catalog.T(s.resolveLocale(r), "error.forbidden")})
		return nil, nil, false
	}
	if requireAdminOnly {
		if err := auth.RequireAdmin(actor); err != nil {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": s.deps.Catalog.T(s.resolveLocale(r), "error.forbidden")})
			return nil, nil, false
		}
	}

	// Enforce TOTP enrollment policy if configured (`totp_required_scope`: admin | staff)
	totpScope := s.deps.Settings.Snapshot().Security.TOTPRequiredScope
	if (totpScope == "staff" || (totpScope == "admin" && actor.Role == domain.RoleAdmin)) &&
		!strings.HasPrefix(r.URL.Path, "/api/admin/security/totp") &&
		r.URL.Path != "/api/admin/me" &&
		r.URL.Path != "/api/admin/auth/logout" {
		var cnt int
		_ = s.deps.Store.QueryRowContext(r.Context(),
			`SELECT COUNT(*) FROM user_credentials WHERE user_id = ? AND kind = 'totp'`, actor.ID).Scan(&cnt)
		if cnt == 0 {
			writeJSON(w, http.StatusForbidden, map[string]any{
				"error":              "TOTP two-factor enrollment is required by security policy before accessing console actions",
				"requires_totp_init": true,
			})
			return nil, nil, false
		}
	}

	// Enforce CSRF token on all state-mutating cookie requests
	if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions {
		csrf := r.Header.Get(auth.CSRFHeaderName)
		if !s.deps.Auth.VerifyCSRFToken(sess.ID, csrf) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "invalid or missing X-CSRF-Token"})
			return nil, nil, false
		}
	}
	return actor, sess, true
}

func (s *AppServer) handleAdminAPI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	sub := strings.TrimPrefix(r.URL.Path, "/api/admin/")

	// Pre-session Authentication Endpoints
	switch sub {
	case "auth/login":
		s.handleAdminLogin(w, r)
		return
	case "auth/2fa":
		s.handleAdminLogin2FA(w, r)
		return
	case "auth/passkey/login/begin", "auth/passkey/begin":
		s.handleAdminPasskeyLoginBegin(w, r)
		return
	case "auth/passkey/login/finish", "auth/passkey/finish":
		s.handleAdminPasskeyLoginFinish(w, r)
		return
	}

	switch {
	case sub == "auth/logout":
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		actor, sess, ok := s.authenticateAdminRequest(w, r, "", false)
		if !ok {
			return
		}
		if sess != nil {
			_ = s.deps.Auth.RevokeSession(r.Context(), actor, sess.ID)
		}
		sec := s.isCookieSecureForRequest(r)
		http.SetCookie(w, &http.Cookie{Name: auth.SessionCookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: sec, SameSite: http.SameSiteLaxMode})
		http.SetCookie(w, &http.Cookie{Name: trusted2FACookieName, Value: "", Path: "/api", MaxAge: -1, HttpOnly: true, Secure: sec, SameSite: http.SameSiteLaxMode})
		http.SetCookie(w, &http.Cookie{Name: trusted2FACookieName, Value: "", Path: "/api/admin/auth", MaxAge: -1, HttpOnly: true, Secure: sec, SameSite: http.SameSiteLaxMode})
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})

	case sub == "me":
		actor, sess, ok := s.authenticateAdminRequest(w, r, auth.ScopeArticlesRead, false)
		if !ok {
			return
		}
		s.handleAdminMe(w, r, actor, sess)

	case sub == "overview":
		actor, _, ok := s.authenticateAdminRequest(w, r, auth.ScopeArticlesRead, false)
		if !ok {
			return
		}
		s.handleAdminOverview(w, r, actor)

	case sub == "about/project-info":
		_, _, ok := s.authenticateAdminRequest(w, r, auth.ScopeArticlesRead, false)
		if !ok {
			return
		}
		s.handleAdminAboutProjectInfo(w, r)

	case sub == "plugins/quick-inserts" && r.Method == http.MethodGet:
		actor, _, ok := s.authenticateAdminRequest(w, r, auth.ScopeArticlesRead, false)
		if !ok {
			return
		}
		locale := actor.PreferredLocale
		if locale == "" {
			locale = s.deps.Settings.Snapshot().Site.DefaultLocale
		}
		var items []plugin.EditorQuickInsertView
		if s.deps.Plugins != nil {
			items = s.deps.Plugins.EditorQuickInserts(locale)
		}
		if items == nil {
			items = []plugin.EditorQuickInsertView{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": items, "quick_inserts": items})

	case sub == "articles" || strings.HasPrefix(sub, "articles/"):
		patScope := auth.ScopeArticlesWrite
		if r.Method == http.MethodGet {
			patScope = auth.ScopeArticlesRead
		}
		actor, _, ok := s.authenticateAdminRequest(w, r, patScope, false)
		if !ok {
			return
		}
		s.handleAdminArticles(w, r, actor, strings.TrimPrefix(sub, "articles"))

	case sub == "pages" || strings.HasPrefix(sub, "pages/"):
		patScope := auth.ScopePagesWrite
		if r.Method == http.MethodGet {
			patScope = auth.ScopeArticlesRead
		}
		actor, _, ok := s.authenticateAdminRequest(w, r, patScope, false)
		if !ok {
			return
		}
		s.handleAdminPages(w, r, actor, strings.TrimPrefix(sub, "pages"))

	case sub == "categories" || strings.HasPrefix(sub, "categories/"):
		actor, _, ok := s.authenticateAdminRequest(w, r, auth.ScopeArticlesWrite, false)
		if !ok {
			return
		}
		s.handleAdminCategories(w, r, actor, strings.TrimPrefix(sub, "categories"))

	case sub == "tags" || strings.HasPrefix(sub, "tags/"):
		actor, _, ok := s.authenticateAdminRequest(w, r, auth.ScopeArticlesWrite, false)
		if !ok {
			return
		}
		s.handleAdminTags(w, r, actor, strings.TrimPrefix(sub, "tags"))

	case sub == "series" || strings.HasPrefix(sub, "series/"):
		actor, _, ok := s.authenticateAdminRequest(w, r, auth.ScopeArticlesWrite, false)
		if !ok {
			return
		}
		s.handleAdminSeries(w, r, actor, strings.TrimPrefix(sub, "series"))

	case sub == "books" || strings.HasPrefix(sub, "books/"):
		actor, _, ok := s.authenticateAdminRequest(w, r, auth.ScopeArticlesWrite, false)
		if !ok {
			return
		}
		s.handleAdminBooks(w, r, actor, strings.TrimPrefix(sub, "books"))

	case sub == "comments" || strings.HasPrefix(sub, "comments/"):
		actor, _, ok := s.authenticateAdminRequest(w, r, "", false)
		if !ok {
			return
		}
		s.handleAdminComments(w, r, actor, strings.TrimPrefix(sub, "comments"))

	case sub == "media" || strings.HasPrefix(sub, "media/"):
		patScope := auth.ScopeMediaWrite
		if r.Method == http.MethodGet {
			patScope = auth.ScopeArticlesRead
		}
		actor, _, ok := s.authenticateAdminRequest(w, r, patScope, false)
		if !ok {
			return
		}
		s.handleAdminMedia(w, r, actor, strings.TrimPrefix(sub, "media"))

	case strings.HasPrefix(sub, "security/totp") ||
		strings.HasPrefix(sub, "security/passkeys") ||
		strings.HasPrefix(sub, "security/oidc-links") ||
		strings.HasPrefix(sub, "security/sessions") ||
		strings.HasPrefix(sub, "security/tokens"):
		actor, sess, ok := s.authenticateAdminRequest(w, r, "", false)
		if !ok {
			return
		}
		s.handleAdminSelfCredentials(w, r, actor, sess, strings.TrimPrefix(sub, "security/"))

	default:
		s.handleAdminSystemRoutes(w, r, sub)
	}
}

// --- Staff Login, TOTP 2FA, Passkey Ceremonies, Sessions & API Tokens ---

func (s *AppServer) handleAdminLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !s.requireSameOrigin(w, r) {
		return
	}
	clientIP := security.ResolveClientIP(r, s.deps.Config.TrustedProxies)
	if !s.deps.WAF.AllowRate("admin:login", clientIP, 12, 5*time.Minute) {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": s.deps.Catalog.T(s.resolveLocale(r), "error.rate_limited")})
		return
	}

	var req struct {
		Ident    string `json:"ident"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	geo := ""
	if s.deps.Geo != nil {
		geo = s.deps.Geo.LookupDisplay(clientIP)
	}
	trusted2FA := ""
	if c, err := r.Cookie(trusted2FACookieName); err == nil {
		trusted2FA = c.Value
	}

	res, err := s.deps.Auth.LoginStaff(r.Context(), req.Ident, req.Password, clientIP, geo, r.UserAgent(), trusted2FA)
	if errors.Is(err, auth.ErrForbidden) {
		s.deps.WAF.RecordStrike(r.Context(), clientIP, security.ReasonLoginFail, 2)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": auth.ErrInvalidCredentials.Error()})
		return
	}
	if err != nil {
		s.deps.WAF.RecordStrike(r.Context(), clientIP, security.ReasonLoginFail, 1)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": err.Error()})
		return
	}
	if res.Requires2FA {
		writeJSON(w, http.StatusOK, map[string]any{
			"requires_2fa":    true,
			"challenge_token": res.ChallengeToken,
		})
		return
	}
	s.setSessionCookieForRequest(w, r, res.RawToken)
	s.recordAuditLog(r, res.User, "auth.login", "user", res.User.ID, nil)
	writeJSON(w, http.StatusOK, map[string]any{
		"requires_2fa": false,
		"user":         res.User,
		"csrf_token":   res.CSRFToken,
	})
}

func (s *AppServer) handleAdminLogin2FA(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !s.requireSameOrigin(w, r) {
		return
	}
	clientIP := security.ResolveClientIP(r, s.deps.Config.TrustedProxies)
	if !s.deps.WAF.AllowRate("admin:2fa", clientIP, 10, 5*time.Minute) {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "rate limited"})
		return
	}
	var req struct {
		ChallengeToken string `json:"challenge_token"`
		Code           string `json:"code"`
		RememberDevice bool   `json:"remember_device"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	geo := ""
	if s.deps.Geo != nil {
		geo = s.deps.Geo.LookupDisplay(clientIP)
	}
	res, trustCookie, err := s.deps.Auth.CompleteStaff2FA(r.Context(), req.ChallengeToken, req.Code, req.RememberDevice, clientIP, geo, r.UserAgent())
	if err != nil {
		s.deps.WAF.RecordStrike(r.Context(), clientIP, security.Reason2FAFail, 1)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": err.Error()})
		return
	}
	s.setSessionCookieForRequest(w, r, res.RawToken)
	if trustCookie != "" {
		http.SetCookie(w, &http.Cookie{
			Name:     trusted2FACookieName,
			Value:    trustCookie,
			Path:     "/api",
			MaxAge:   s.deps.Settings.Snapshot().Security.TOTPRememberDays * 86400,
			HttpOnly: true,
			Secure:   s.isCookieSecureForRequest(r),
			SameSite: http.SameSiteLaxMode,
		})
	}
	s.recordAuditLog(r, res.User, "auth.login_2fa", "user", res.User.ID, nil)
	writeJSON(w, http.StatusOK, map[string]any{
		"user":       res.User,
		"csrf_token": res.CSRFToken,
	})
}

func (s *AppServer) handleAdminPasskeyLoginBegin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !s.requireSameOrigin(w, r) {
		return
	}
	clientIP := security.ResolveClientIP(r, s.deps.Config.TrustedProxies)
	if !s.deps.WAF.AllowRate("admin:passkey:begin", clientIP, 15, 5*time.Minute) {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": s.deps.Catalog.T(s.resolveLocale(r), "error.rate_limited")})
		return
	}
	res, err := auth.BeginPasskeyCeremony(s.deps.Config.KeyRing, "", "login", s.publicBaseURL(r))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *AppServer) handleAdminPasskeyLoginFinish(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !s.requireSameOrigin(w, r) {
		return
	}
	clientIP := security.ResolveClientIP(r, s.deps.Config.TrustedProxies)
	if !s.deps.WAF.AllowRate("admin:passkey:finish", clientIP, 12, 5*time.Minute) {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": s.deps.Catalog.T(s.resolveLocale(r), "error.rate_limited")})
		return
	}
	var req struct {
		StateToken        string `json:"state_token"`
		CredentialID      string `json:"credential_id"`
		ClientDataJSON    string `json:"client_data_json"`
		AuthenticatorData string `json:"authenticator_data"`
		Signature         string `json:"signature"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32768)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	cdBytes, err1 := base64.RawURLEncoding.DecodeString(req.ClientDataJSON)
	adBytes, err2 := base64.RawURLEncoding.DecodeString(req.AuthenticatorData)
	sigBytes, err3 := base64.RawURLEncoding.DecodeString(req.Signature)
	if err1 != nil || err2 != nil || err3 != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid base64url passkey fields"})
		return
	}
	geo := ""
	if s.deps.Geo != nil {
		geo = s.deps.Geo.LookupDisplay(clientIP)
	}
	res, err := s.deps.Auth.LoginWithPasskey(r.Context(), req.StateToken, req.CredentialID, cdBytes, adBytes, sigBytes, clientIP, geo, r.UserAgent())
	if err != nil {
		s.deps.WAF.RecordStrike(r.Context(), clientIP, security.ReasonLoginFail, 1)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": err.Error()})
		return
	}
	s.setSessionCookieForRequest(w, r, res.RawToken)
	s.recordAuditLog(r, res.User, "auth.login_passkey", "user", res.User.ID, nil)
	writeJSON(w, http.StatusOK, map[string]any{
		"user":       res.User,
		"csrf_token": res.CSRFToken,
	})
}

func (s *AppServer) handleAdminMe(w http.ResponseWriter, r *http.Request, actor *domain.User, sess *domain.UserSession) {
	if r.Method == http.MethodGet {
		csrf := ""
		sessID := ""
		if sess != nil {
			csrf = s.deps.Auth.ComputeCSRFToken(sess.ID)
			sessID = sess.ID
		}
		var totpCnt, pkCnt int
		_ = s.deps.Store.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM user_credentials WHERE user_id = ? AND kind = 'totp'`, actor.ID).Scan(&totpCnt)
		_ = s.deps.Store.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM user_credentials WHERE user_id = ? AND kind = 'passkey'`, actor.ID).Scan(&pkCnt)
		snap := s.deps.Settings.Snapshot()
		writeJSON(w, http.StatusOK, map[string]any{
			"user":           actor,
			"session_id":     sessID,
			"csrf_token":     csrf,
			"totp_enabled":   totpCnt > 0,
			"passkeys":       pkCnt,
			"site":           snap.Site,
			"appearance":     snap.Appearance,
			"storage_driver": snap.Storage.ActiveDriver,
			"locales":        s.deps.Catalog.Locales(),
		})
		return
	}

	if r.Method == http.MethodPatch {
		if sess == nil {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "profile mutation requires an interactive admin session"})
			return
		}
		var req struct {
			DisplayName     *string `json:"display_name"`
			Email           *string `json:"email"`
			Bio             *string `json:"bio"`
			WebsiteURL      *string `json:"website_url"`
			AvatarURL       *string `json:"avatar_url"`
			PreferredLocale *string `json:"preferred_locale"`
			CurrentPassword string  `json:"current_password"`
			NewPassword     string  `json:"new_password"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384)).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
			return
		}
		fullUser, err := sqlx.Get[domain.User](r.Context(), s.deps.Store, `SELECT * FROM users WHERE id = ?`, actor.ID)
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "user not found"})
			return
		}
		if req.DisplayName != nil && strings.TrimSpace(*req.DisplayName) != "" {
			fullUser.DisplayName = strings.TrimSpace(*req.DisplayName)
		}
		if req.Email != nil {
			nextEmail := strings.ToLower(strings.TrimSpace(*req.Email))
			if nextEmail != "" && nextEmail != strings.ToLower(fullUser.Email) {
				if !strings.Contains(nextEmail, "@") || len(nextEmail) > 190 {
					writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid email address"})
					return
				}
				ok, _ := auth.VerifyPassword(req.CurrentPassword, fullUser.PasswordHash, fullUser.LegacyHash)
				if !ok {
					writeJSON(w, http.StatusForbidden, map[string]string{"error": "current_password is incorrect"})
					return
				}
				fullUser.Email = nextEmail
			}
		}
		if req.Bio != nil {
			fullUser.Bio = strings.TrimSpace(*req.Bio)
		}
		if req.WebsiteURL != nil {
			rawWeb := strings.TrimSpace(*req.WebsiteURL)
			if rawWeb == "" {
				fullUser.WebsiteURL = ""
			} else {
				cleanWeb, ok := sanitizeRedirectTargetURL(rawWeb)
				if !ok {
					writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid website_url"})
					return
				}
				fullUser.WebsiteURL = cleanWeb
			}
		}
		if req.AvatarURL != nil {
			rawAv := strings.TrimSpace(*req.AvatarURL)
			if rawAv == "" {
				fullUser.AvatarURL = ""
			} else {
				cleanAv, ok := sanitizeRedirectTargetURL(rawAv)
				if !ok {
					writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid avatar_url"})
					return
				}
				fullUser.AvatarURL = cleanAv
			}
		}
		if req.PreferredLocale != nil {
			fullUser.PreferredLocale = s.deps.Catalog.Match(*req.PreferredLocale, "")
		}
		if req.NewPassword != "" {
			ok, _ := auth.VerifyPassword(req.CurrentPassword, fullUser.PasswordHash, fullUser.LegacyHash)
			if !ok {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": "current_password is incorrect"})
				return
			}
			if err := auth.ValidatePasswordPolicy(req.NewPassword); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
			newHash, err := auth.HashPassword(req.NewPassword)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
				return
			}
			fullUser.PasswordHash = newHash
			fullUser.LegacyHash = ""
		}
		fullUser.UpdatedAt = time.Now().UnixMilli()
		_, err = s.deps.Store.ExecContext(r.Context(),
			`UPDATE users SET display_name = ?, email = ?, bio = ?, website_url = ?, avatar_url = ?, preferred_locale = ?, password_hash = ?, legacy_hash = ?, updated_at = ? WHERE id = ?`,
			fullUser.DisplayName, fullUser.Email, fullUser.Bio, fullUser.WebsiteURL, fullUser.AvatarURL, fullUser.PreferredLocale,
			fullUser.PasswordHash, fullUser.LegacyHash, fullUser.UpdatedAt, fullUser.ID)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if req.NewPassword != "" {
			curSessID := ""
			if sess != nil {
				curSessID = sess.ID
			}
			_ = s.deps.Auth.RevokeOtherSessions(r.Context(), fullUser.ID, curSessID)
			_ = s.deps.Auth.RevokeUserAPITokens(r.Context(), fullUser.ID)
		}
		fullUser.PasswordHash = ""
		fullUser.LegacyHash = ""
		writeJSON(w, http.StatusOK, map[string]any{"user": fullUser})
		return
	}
	w.WriteHeader(http.StatusMethodNotAllowed)
}

func (s *AppServer) stepUpErrorMessage(r *http.Request, missing bool) string {
	loc := strings.ToLower(s.resolveLocale(r))
	if missing {
		if s.deps.Catalog != nil {
			if msg := s.deps.Catalog.T(s.resolveLocale(r), "sys.stepup_pwd_required"); msg != "" && msg != "sys.stepup_pwd_required" {
				return msg
			}
		}
		if strings.HasPrefix(loc, "zh-tw") || strings.HasPrefix(loc, "zh-hk") {
			return "請輸入目前登入密碼以完成安全確認 (current_password is required)"
		}
		if strings.HasPrefix(loc, "zh") {
			return "请输入当前登录密码以完成安全确认 (current_password is required)"
		}
		return "current_password is required"
	}
	if strings.HasPrefix(loc, "zh-tw") || strings.HasPrefix(loc, "zh-hk") {
		return "目前登入密碼不正確 (current_password is incorrect)"
	}
	if strings.HasPrefix(loc, "zh") {
		return "当前登录密码不正确 (current_password is incorrect)"
	}
	return "current_password is incorrect"
}

func (s *AppServer) verifyStepUpPassword(w http.ResponseWriter, r *http.Request, actorID string, candidate string) bool {
	pwd := candidate
	if strings.TrimSpace(pwd) == "" {
		pwd = r.Header.Get("X-Volo-Current-Password")
	}
	if strings.TrimSpace(pwd) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": s.stepUpErrorMessage(r, true)})
		return false
	}
	fullUser, err := sqlx.Get[domain.User](r.Context(), s.deps.Store, `SELECT * FROM users WHERE id = ?`, actorID)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": s.stepUpErrorMessage(r, false)})
		return false
	}
	ok, _ := auth.VerifyPassword(pwd, fullUser.PasswordHash, fullUser.LegacyHash)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": s.stepUpErrorMessage(r, false)})
		return false
	}
	return true
}

func (s *AppServer) handleAdminSelfCredentials(w http.ResponseWriter, r *http.Request, actor *domain.User, sess *domain.UserSession, sub string) {
	switch {
	case sub == "oidc-links" || strings.HasPrefix(sub, "oidc-links/"):
		s.handleAdminOIDCLinks(w, r, actor, sess, strings.TrimPrefix(sub, "oidc-links"))

	case sub == "totp/setup" && r.Method == http.MethodPost:
		secret, err := auth.GenerateTOTPSecret()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		uri := auth.BuildOTPAuthURI(s.deps.Settings.Snapshot().Site.Title, actor.Username, secret)
		qrSVG := auth.RenderQRCodeSVG(uri, 240)
		writeJSON(w, http.StatusOK, map[string]string{"secret": secret, "otpauth_uri": uri, "qr_svg": qrSVG})

	case sub == "totp/enroll" && r.Method == http.MethodPost:
		var req struct {
			Secret          string `json:"secret"`
			Code            string `json:"code"`
			CurrentPassword string `json:"current_password"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
			return
		}
		fullUser, err := sqlx.Get[domain.User](r.Context(), s.deps.Store, `SELECT * FROM users WHERE id = ?`, actor.ID)
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "user not found"})
			return
		}
		if req.CurrentPassword == "" {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "current_password is incorrect"})
			return
		}
		if ok, _ := auth.VerifyPassword(req.CurrentPassword, fullUser.PasswordHash, fullUser.LegacyHash); !ok {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "current_password is incorrect"})
			return
		}
		codes, err := s.deps.Auth.EnrollTOTP(r.Context(), actor.ID, req.Secret, req.Code)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		s.recordAuditLog(r, actor, "security.totp_enroll", "user", actor.ID, nil)
		writeJSON(w, http.StatusOK, map[string]any{"recovery_codes": codes})

	case sub == "totp" && r.Method == http.MethodDelete:
		var req struct {
			CurrentPassword string `json:"current_password"`
		}
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req)
		fullUser, err := sqlx.Get[domain.User](r.Context(), s.deps.Store, `SELECT * FROM users WHERE id = ?`, actor.ID)
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "user not found"})
			return
		}
		ok, _ := auth.VerifyPassword(req.CurrentPassword, fullUser.PasswordHash, fullUser.LegacyHash)
		if !ok {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "current_password is incorrect"})
			return
		}
		_, _ = s.deps.Store.ExecContext(r.Context(),
			`DELETE FROM user_credentials WHERE user_id = ? AND kind IN ('totp', 'recovery_codes')`, actor.ID)
		s.recordAuditLog(r, actor, "security.totp_disable", "user", actor.ID, nil)
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})

	case sub == "passkeys" && r.Method == http.MethodGet:
		rows, _ := sqlx.Select[domain.UserCredential](r.Context(), s.deps.Store,
			`SELECT id, user_id, kind, name, credential_id, cose_alg, sign_count, last_used_at, created_at
			 FROM user_credentials WHERE user_id = ? AND kind = 'passkey' ORDER BY created_at DESC`, actor.ID)
		if rows == nil {
			rows = []domain.UserCredential{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": rows})

	case sub == "passkeys/begin" && r.Method == http.MethodPost:
		res, err := auth.BeginPasskeyCeremony(s.deps.Config.KeyRing, actor.ID, "register", s.publicBaseURL(r))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, res)

	case sub == "passkeys/finish" && r.Method == http.MethodPost:
		var req struct {
			Name              string `json:"name"`
			StateToken        string `json:"state_token"`
			ClientDataJSON    string `json:"client_data_json"`
			AttestationObject string `json:"attestation_object"`
			CurrentPassword   string `json:"current_password"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 65536)).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
			return
		}
		fullUser, err := sqlx.Get[domain.User](r.Context(), s.deps.Store, `SELECT * FROM users WHERE id = ?`, actor.ID)
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "user not found"})
			return
		}
		if req.CurrentPassword == "" {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "current_password is incorrect"})
			return
		}
		if ok, _ := auth.VerifyPassword(req.CurrentPassword, fullUser.PasswordHash, fullUser.LegacyHash); !ok {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "current_password is incorrect"})
			return
		}
		cdBytes, err1 := base64.RawURLEncoding.DecodeString(req.ClientDataJSON)
		attBytes, err2 := base64.RawURLEncoding.DecodeString(req.AttestationObject)
		if err1 != nil || err2 != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid base64url passkey registration payload"})
			return
		}
		cred, err := s.deps.Auth.RegisterPasskey(r.Context(), actor.ID, req.Name, req.StateToken, cdBytes, attBytes)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		cred.PublicKeySPKI = ""
		s.recordAuditLog(r, actor, "security.passkey_register", "user_credential", cred.ID, nil)
		writeJSON(w, http.StatusCreated, map[string]any{"passkey": cred})

	case strings.HasPrefix(sub, "passkeys/") && r.Method == http.MethodDelete:
		if sess == nil || sess.ID == "" {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": s.deps.Catalog.T(s.resolveLocale(r), "error.forbidden")})
			return
		}
		var req struct {
			CurrentPassword string `json:"current_password"`
		}
		if r.Body != nil {
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
				return
			}
		}
		if !s.verifyStepUpPassword(w, r, actor.ID, req.CurrentPassword) {
			return
		}
		pkID := strings.TrimPrefix(sub, "passkeys/")
		_, _ = s.deps.Store.ExecContext(r.Context(),
			`DELETE FROM user_credentials WHERE id = ? AND user_id = ? AND kind = 'passkey'`, pkID, actor.ID)
		s.recordAuditLog(r, actor, "security.passkey_delete", "user_credential", pkID, nil)
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})

	case sub == "sessions" && r.Method == http.MethodGet:
		rows, _ := s.deps.Auth.ListUserSessions(r.Context(), actor.ID)
		if rows == nil {
			rows = []domain.UserSession{}
		}
		curID := ""
		if sess != nil {
			curID = sess.ID
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": rows, "current_session_id": curID})

	case sub == "sessions" && r.Method == http.MethodDelete:
		var (
			res sql.Result
			err error
		)
		if sess != nil && sess.ID != "" {
			res, err = s.deps.Store.ExecContext(r.Context(),
				`DELETE FROM user_sessions WHERE user_id = ? AND id <> ?`, actor.ID, sess.ID)
		} else {
			res, err = s.deps.Store.ExecContext(r.Context(),
				`DELETE FROM user_sessions WHERE user_id = ?`, actor.ID)
		}
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		revoked, _ := res.RowsAffected()
		s.recordAuditLog(r, actor, "security.sessions_revoke_others", "user_session", actor.ID, map[string]any{"revoked": revoked})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "revoked": revoked})

	case strings.HasPrefix(sub, "sessions/") && r.Method == http.MethodDelete:
		sid := strings.TrimPrefix(sub, "sessions/")
		if err := s.deps.Auth.RevokeSession(r.Context(), actor, sid); err != nil {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
			return
		}
		s.recordAuditLog(r, actor, "security.session_revoke", "user_session", sid, nil)
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})

	case sub == "tokens" && r.Method == http.MethodGet:
		rows, _ := sqlx.Select[domain.APIToken](r.Context(), s.deps.Store,
			`SELECT id, user_id, name, token_prefix, scopes_json, last_used_at, expires_at, created_at
			 FROM api_tokens WHERE user_id = ? ORDER BY created_at DESC`, actor.ID)
		if rows == nil {
			rows = []domain.APIToken{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": rows})

	case sub == "tokens" && r.Method == http.MethodPost:
		if sess == nil || sess.ID == "" {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": s.deps.Catalog.T(s.resolveLocale(r), "error.forbidden")})
			return
		}
		var req struct {
			Name            string   `json:"name"`
			Scopes          []string `json:"scopes"`
			ExpiresIn       int      `json:"expires_in_days"`
			CurrentPassword string   `json:"current_password"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
			return
		}
		if !s.verifyStepUpPassword(w, r, actor.ID, req.CurrentPassword) {
			return
		}
		var expMS int64
		if req.ExpiresIn > 0 {
			expMS = time.Now().Add(time.Duration(req.ExpiresIn) * 24 * time.Hour).UnixMilli()
		}
		rec, plainTok, err := s.deps.Auth.CreateAPIToken(r.Context(), actor, req.Name, req.Scopes, expMS)
		if errors.Is(err, auth.ErrForbidden) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
			return
		}
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		s.recordAuditLog(r, actor, "security.api_token_create", "api_token", rec.ID, map[string]any{"scopes": req.Scopes})
		writeJSON(w, http.StatusCreated, map[string]any{"token": rec, "plaintext_token": plainTok})

	case strings.HasPrefix(sub, "tokens/") && r.Method == http.MethodDelete:
		tid := strings.TrimPrefix(sub, "tokens/")
		_, _ = s.deps.Store.ExecContext(r.Context(), `DELETE FROM api_tokens WHERE id = ? AND user_id = ?`, tid, actor.ID)
		s.recordAuditLog(r, actor, "security.api_token_revoke", "api_token", tid, nil)
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})

	default:
		http.NotFound(w, r)
	}
}

// --- Workspace 1: Overview Dashboard & Unified Articles + Revisions + Live Preview ---

func (s *AppServer) handleAdminOverview(w http.ResponseWriter, r *http.Request, actor *domain.User) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if s.deps.Content != nil {
		_ = s.deps.Content.FlushAnalytics(r.Context())
	}
	isAdmin := actor.Role == domain.RoleAdmin
	var pubCnt, draftCnt, schedCnt, protCnt, pageCnt, cmtPending, mediaCnt int
	var totalViews int64
	if isAdmin {
		_ = s.deps.Store.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM articles WHERE status = 'published'`).Scan(&pubCnt)
		_ = s.deps.Store.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM articles WHERE status = 'draft'`).Scan(&draftCnt)
		_ = s.deps.Store.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM articles WHERE status = 'scheduled'`).Scan(&schedCnt)
		_ = s.deps.Store.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM articles WHERE password_ciphertext <> '' OR has_content_lock = ?`, true).Scan(&protCnt)
		_ = s.deps.Store.QueryRowContext(r.Context(), `SELECT COALESCE(SUM(view_count), 0) FROM articles`).Scan(&totalViews)
		_ = s.deps.Store.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM pages`).Scan(&pageCnt)
		_ = s.deps.Store.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM comments WHERE status = 'pending' AND is_deleted = ?`, false).Scan(&cmtPending)
		_ = s.deps.Store.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM media_assets`).Scan(&mediaCnt)
	} else {
		_ = s.deps.Store.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM articles WHERE author_id = ? AND status = 'published'`, actor.ID).Scan(&pubCnt)
		_ = s.deps.Store.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM articles WHERE author_id = ? AND status = 'draft'`, actor.ID).Scan(&draftCnt)
		_ = s.deps.Store.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM articles WHERE author_id = ? AND status = 'scheduled'`, actor.ID).Scan(&schedCnt)
		_ = s.deps.Store.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM articles WHERE author_id = ? AND (password_ciphertext <> '' OR has_content_lock = ?)`, actor.ID, true).Scan(&protCnt)
		_ = s.deps.Store.QueryRowContext(r.Context(), `SELECT COALESCE(SUM(view_count), 0) FROM articles WHERE author_id = ?`, actor.ID).Scan(&totalViews)
		_ = s.deps.Store.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM pages WHERE author_id = ?`, actor.ID).Scan(&pageCnt)
		_ = s.deps.Store.QueryRowContext(r.Context(),
			`SELECT COUNT(*) FROM comments c
			 LEFT JOIN articles a ON c.target_type = 'article' AND c.target_id = a.id
			 LEFT JOIN pages p ON c.target_type = 'page' AND c.target_id = p.id
			 WHERE c.is_deleted = ? AND c.status = 'pending' AND (a.author_id = ? OR p.author_id = ?)`,
			false, actor.ID, actor.ID).Scan(&cmtPending)
		_ = s.deps.Store.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM media_assets WHERE uploader_id = ?`, actor.ID).Scan(&mediaCnt)
	}

	windowDays := 30
	if rawDays, err := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("days"))); err == nil && rawDays == 7 {
		windowDays = 7
	}
	now := time.Now()
	cutoffDate := now.AddDate(0, 0, -(windowDays - 1)).Format("2006-01-02")
	var trend []domain.AnalyticsDaily
	if isAdmin {
		trend, _ = sqlx.Select[domain.AnalyticsDaily](r.Context(), s.deps.Store,
			`SELECT * FROM analytics_daily WHERE stat_date >= ? ORDER BY stat_date ASC`, cutoffDate)
	}
	if trend == nil {
		trend = []domain.AnalyticsDaily{}
	}
	byDate := make(map[string]domain.AnalyticsDaily, len(trend))
	for _, pt := range trend {
		byDate[pt.StatDate] = pt
	}
	series := make([]domain.AnalyticsDaily, 0, windowDays)
	for i := windowDays - 1; i >= 0; i-- {
		d := now.AddDate(0, 0, -i).Format("2006-01-02")
		if pt, ok := byDate[d]; ok {
			series = append(series, pt)
		} else {
			series = append(series, domain.AnalyticsDaily{StatDate: d, PV: 0, UV: 0})
		}
	}

	type topArticleView struct {
		ItemKey string `json:"item_key"`
		ID      string `json:"id"`
		Title   string `json:"title"`
		Slug    string `json:"slug"`
		Hits    int64  `json:"hits"`
	}
	var topPosts []topArticleView
	if isAdmin {
		rawTopPosts, _ := sqlx.Select[struct {
			ItemKey string `db:"item_key" json:"item_key"`
			Hits    int64  `db:"hits" json:"hits"`
		}](r.Context(), s.deps.Store,
			`SELECT item_key, SUM(hits) AS hits FROM analytics_top_items
			 WHERE stat_date >= ? AND kind = 'article' GROUP BY item_key ORDER BY hits DESC LIMIT 10`, cutoffDate)
		topPosts = make([]topArticleView, 0, len(rawTopPosts))
		for _, tp := range rawTopPosts {
			var artID, title, slug string
			_ = s.deps.Store.QueryRowContext(r.Context(),
				`SELECT id, title, slug FROM articles WHERE id = ? OR slug = ? LIMIT 1`, tp.ItemKey, tp.ItemKey).Scan(&artID, &title, &slug)
			if artID == "" {
				artID = tp.ItemKey
			}
			if title == "" {
				title = tp.ItemKey
			}
			topPosts = append(topPosts, topArticleView{
				ItemKey: tp.ItemKey,
				ID:      artID,
				Title:   title,
				Slug:    slug,
				Hits:    tp.Hits,
			})
		}
	} else {
		rawTopPosts, _ := sqlx.Select[struct {
			ID    string `db:"id"`
			Title string `db:"title"`
			Slug  string `db:"slug"`
			Hits  int64  `db:"hits"`
		}](r.Context(), s.deps.Store,
			`SELECT a.id, a.title, a.slug, SUM(t.hits) AS hits
			 FROM analytics_top_items t
			 INNER JOIN articles a ON (a.id = t.item_key OR a.slug = t.item_key)
			 WHERE t.stat_date >= ? AND t.kind = 'article' AND a.author_id = ?
			 GROUP BY a.id, a.title, a.slug
			 ORDER BY hits DESC LIMIT 10`, cutoffDate, actor.ID)
		topPosts = make([]topArticleView, 0, len(rawTopPosts))
		for _, tp := range rawTopPosts {
			topPosts = append(topPosts, topArticleView{
				ItemKey: tp.ID,
				ID:      tp.ID,
				Title:   tp.Title,
				Slug:    tp.Slug,
				Hits:    tp.Hits,
			})
		}
		if len(topPosts) == 0 {
			fallbackTop, _ := sqlx.Select[struct {
				ID        string `db:"id"`
				Title     string `db:"title"`
				Slug      string `db:"slug"`
				ViewCount int64  `db:"view_count"`
			}](r.Context(), s.deps.Store,
				`SELECT id, title, slug, view_count FROM articles WHERE author_id = ? AND view_count > 0 ORDER BY view_count DESC LIMIT 10`, actor.ID)
			for _, tp := range fallbackTop {
				topPosts = append(topPosts, topArticleView{
					ItemKey: tp.ID,
					ID:      tp.ID,
					Title:   tp.Title,
					Slug:    tp.Slug,
					Hits:    tp.ViewCount,
				})
			}
		}
	}
	if topPosts == nil {
		topPosts = []topArticleView{}
	}

	type topRefererView struct {
		ItemKey string `db:"item_key" json:"item_key"`
		Hits    int64  `db:"hits" json:"hits"`
	}
	var topReferers []topRefererView
	if isAdmin {
		topReferers, _ = sqlx.Select[topRefererView](r.Context(), s.deps.Store,
			`SELECT item_key, SUM(hits) AS hits FROM analytics_top_items
			 WHERE stat_date >= ? AND kind = 'referer' GROUP BY item_key ORDER BY hits DESC LIMIT 10`, cutoffDate)
	}
	if topReferers == nil {
		topReferers = []topRefererView{}
	}

	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)

	snap := s.deps.Settings.Snapshot()
	locale := actor.PreferredLocale
	if locale == "" {
		locale = snap.Site.DefaultLocale
	}
	var quickInserts []plugin.EditorQuickInsertView
	var dashCards []plugin.RenderedSlot
	if s.deps.Plugins != nil {
		quickInserts = s.deps.Plugins.EditorQuickInserts(locale)
		dashCards = s.deps.Plugins.RenderSlots(r.Context(), plugin.SlotAdminDashboardCard, plugin.SlotRenderContext{
			Locale: locale,
			Path:   r.URL.Path,
		})
	}
	if quickInserts == nil {
		quickInserts = []plugin.EditorQuickInsertView{}
	}
	if dashCards == nil {
		dashCards = []plugin.RenderedSlot{}
	}

	// 12-month published article trend (dialect-agnostic Go bucketing)
	type monthBucket struct {
		Month string `json:"month"`
		Count int    `json:"count"`
	}
	monthKeys := make([]string, 0, 12)
	monthCounts := make(map[string]int, 12)
	curMonthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
	for i := 11; i >= 0; i-- {
		m := curMonthStart.AddDate(0, -i, 0).Format("2006-01")
		monthKeys = append(monthKeys, m)
		monthCounts[m] = 0
	}
	earliestMS := curMonthStart.AddDate(0, -11, 0).UnixMilli()
	type artDateRow struct {
		PublishedAt int64 `db:"published_at"`
		CreatedAt   int64 `db:"created_at"`
	}
	var artDates []artDateRow
	if isAdmin {
		artDates, _ = sqlx.Select[artDateRow](r.Context(), s.deps.Store,
			`SELECT published_at, created_at FROM articles WHERE status = 'published' AND (published_at >= ? OR created_at >= ?)`,
			earliestMS, earliestMS)
	} else {
		artDates, _ = sqlx.Select[artDateRow](r.Context(), s.deps.Store,
			`SELECT published_at, created_at FROM articles WHERE author_id = ? AND status = 'published' AND (published_at >= ? OR created_at >= ?)`,
			actor.ID, earliestMS, earliestMS)
	}
	for _, ad := range artDates {
		ts := ad.PublishedAt
		if ts <= 0 {
			ts = ad.CreatedAt
		}
		if ts > 0 {
			ym := time.UnixMilli(ts).In(now.Location()).Format("2006-01")
			if _, ok := monthCounts[ym]; ok {
				monthCounts[ym]++
			}
		}
	}
	monthlyArticles := make([]monthBucket, 0, 12)
	for _, m := range monthKeys {
		monthlyArticles = append(monthlyArticles, monthBucket{Month: m, Count: monthCounts[m]})
	}

	catUsage, _ := s.deps.Content.ListCategoriesWithCounts(r.Context())
	topTags, _ := s.deps.Content.ListTagsWithCounts(r.Context(), 5)

	type recentCommentView struct {
		domain.Comment
		TargetTitle string `json:"target_title"`
		TargetSlug  string `json:"target_slug"`
	}
	var rawRecentComments []domain.Comment
	if isAdmin {
		rawRecentComments, _ = sqlx.Select[domain.Comment](r.Context(), s.deps.Store,
			`SELECT * FROM comments WHERE is_deleted = ? ORDER BY created_at DESC LIMIT 8`, false)
	} else {
		rawRecentComments, _ = sqlx.Select[domain.Comment](r.Context(), s.deps.Store,
			`SELECT c.* FROM comments c
			 LEFT JOIN articles a ON c.target_type = 'article' AND c.target_id = a.id
			 LEFT JOIN pages p ON c.target_type = 'page' AND c.target_id = p.id
			 WHERE c.is_deleted = ? AND (a.author_id = ? OR p.author_id = ?)
			 ORDER BY c.created_at DESC LIMIT 8`,
			false, actor.ID, actor.ID)
	}
	recentComments := make([]recentCommentView, 0, len(rawRecentComments))
	for _, rc := range rawRecentComments {
		ownerID := actor.ID
		if isAdmin {
			ownerID = ""
		}
		scrubbed, sErr := authorizeAndScrubAdminComment(actor, rc, ownerID)
		if sErr != nil {
			continue
		}
		var tTitle, tSlug string
		if rc.TargetType == domain.TargetTypePage {
			_ = s.deps.Store.QueryRowContext(r.Context(), `SELECT title, slug FROM pages WHERE id = ?`, rc.TargetID).Scan(&tTitle, &tSlug)
		} else {
			_ = s.deps.Store.QueryRowContext(r.Context(), `SELECT title, slug FROM articles WHERE id = ?`, rc.TargetID).Scan(&tTitle, &tSlug)
		}
		recentComments = append(recentComments, recentCommentView{
			Comment:     scrubbed,
			TargetTitle: tTitle,
			TargetSlug:  tSlug,
		})
	}

	var recentLogs []domain.AuditLog
	if isAdmin {
		recentLogs, _ = sqlx.Select[domain.AuditLog](r.Context(), s.deps.Store,
			`SELECT * FROM audit_logs ORDER BY created_at DESC LIMIT 8`)
	} else {
		recentLogs, _ = sqlx.Select[domain.AuditLog](r.Context(), s.deps.Store,
			`SELECT * FROM audit_logs WHERE actor_id = ? ORDER BY created_at DESC LIMIT 8`, actor.ID)
		for i := range recentLogs {
			recentLogs[i].ClientIP = ""
		}
	}
	if recentLogs == nil {
		recentLogs = []domain.AuditLog{}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"counts": map[string]int{
			"published":        pubCnt,
			"drafts":           draftCnt,
			"scheduled":        schedCnt,
			"protected":        protCnt,
			"pages":            pageCnt,
			"pending_comments": cmtPending,
			"media":            mediaCnt,
			"views":            int(totalViews),
		},
		"total_views":      totalViews,
		"window_days":      windowDays,
		"analytics_daily":  trend,
		"analytics_series": series,
		"top_articles":     topPosts,
		"top_referers":     topReferers,
		"monthly_articles": monthlyArticles,
		"category_usage":   catUsage,
		"top_tags":         topTags,
		"notice_board":     snap.Site.NoticeBoard,
		"recent_comments":  recentComments,
		"recent_logs":      recentLogs,
		"quick_inserts":    quickInserts,
		"plugin_cards":     dashCards,
		"runtime_dialect":  s.deps.Store.Dialect().Name(),
		"uptime_seconds":   int64(time.Since(s.deps.StartedAt).Seconds()),
		"heap_alloc_mb":    float64(mem.Alloc) / (1024 * 1024),
		"goroutines":       runtime.NumGoroutine(),
		"admin_bundle_kb":  float64(adminui.Default().TotalGzipBytes()) / 1024.0,
	})
}

type adminArticlePayload struct {
	CategoryID           string   `json:"category_id"`
	Title                string   `json:"title"`
	Slug                 string   `json:"slug"`
	LegacyPermalink      string   `json:"legacy_permalink"`
	ClearLegacyPermalink bool     `json:"clear_legacy_permalink"`
	SummaryMD            string   `json:"summary_md"`
	ContentMD            string   `json:"content_md"`
	CoverURL             string   `json:"cover_url"`
	Status               string   `json:"status"`
	PublishedAt          int64    `json:"published_at"`
	ScheduledAt          int64    `json:"scheduled_at"`
	IsPinned             bool     `json:"is_pinned"`
	AllowComment         bool     `json:"allow_comment"`
	Password             *string  `json:"password"`
	ClearPassword        bool     `json:"clear_password"`
	AudioURL             string   `json:"audio_url"`
	AudioDurationSec     int      `json:"audio_duration_sec"`
	AudioSizeBytes       int64    `json:"audio_size_bytes"`
	AudioMIME            string   `json:"audio_mime"`
	Lang                 string   `json:"lang"`
	TranslationGroup     string   `json:"translation_group"`
	LicenseOverride      string   `json:"license_override"`
	Tags                 []string `json:"tags"`
	SeriesID             string   `json:"series_id"`
	RevisionNote         string   `json:"revision_note"`
}

func (s *AppServer) deleteTargetCommentsAndReactionsTx(ctx context.Context, tx *sqlx.Tx, targetType string, targetID string) {
	_, _ = tx.ExecContext(ctx,
		`DELETE FROM reactions WHERE target_type = ? AND target_id IN (SELECT id FROM comments WHERE target_type = ? AND target_id = ?)`,
		"comment", targetType, targetID)
	_, _ = tx.ExecContext(ctx,
		`DELETE FROM comments WHERE target_type = ? AND target_id = ?`,
		targetType, targetID)
	_, _ = tx.ExecContext(ctx,
		`DELETE FROM reactions WHERE target_type = ? AND target_id = ?`,
		targetType, targetID)
}

func (s *AppServer) validateLegacyPermalink(ctx context.Context, rawLegacy string, selfArticleID string) (string, error) {
	clean := strings.TrimSpace(repo.StripASCIIControls(rawLegacy))
	if clean == "" {
		return "", nil
	}
	if len(clean) > 255 || !strings.HasPrefix(clean, "/") || strings.HasPrefix(clean, "//") || clean == "/" {
		return "", errors.New("legacy_permalink must be a site-relative path starting with '/'")
	}
	if strings.ContainsAny(clean, " \t\r\n?#\\") || strings.Contains(clean, "..") {
		return "", errors.New("legacy_permalink must not contain whitespace, query strings, fragments, backslashes, or '..'")
	}
	low := strings.ToLower(clean)
	adminPref := "/" + strings.ToLower(strings.Trim(strings.TrimSpace(s.deps.Config.AdminPath), "/"))
	if adminPref == "/" {
		adminPref = "/console"
	}
	reservedPrefixes := []string{
		"/api/", "/console/", "/setup/", "/posts/", "/uploads/", "/themes/", "/plugins/", "/static/", "/assets/",
		adminPref + "/",
	}
	reservedExact := []string{
		"/api", "/console", "/setup", "/feed", "/feed.xml", "/rss", "/rss.xml", "/atom.xml", "/feed.json",
		"/sitemap.xml", "/robots.txt", "/llms.txt", "/llms-full.txt", adminPref,
	}
	for _, ex := range reservedExact {
		if low == ex {
			return "", fmt.Errorf("legacy_permalink %q collides with reserved system route", clean)
		}
	}
	for _, pref := range reservedPrefixes {
		if strings.HasPrefix(low, pref) {
			return "", fmt.Errorf("legacy_permalink %q collides with reserved prefix %q", clean, pref)
		}
	}
	var existingID string
	err := s.deps.Store.QueryRowContext(ctx,
		`SELECT id FROM articles WHERE legacy_permalink = ? AND id <> ?`, clean, selfArticleID).Scan(&existingID)
	if err == nil && existingID != "" {
		return "", errors.New("legacy_permalink is already assigned to another article")
	}
	return clean, nil
}

func (s *AppServer) handleAdminArticles(w http.ResponseWriter, r *http.Request, actor *domain.User, rest string) {
	rest = strings.TrimPrefix(rest, "/")

	if rest == "preview" && r.Method == http.MethodPost {
		var req struct {
			ContentMD string `json:"content_md"`
			Locale    string `json:"locale"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
			return
		}
		out := render.Default().Render(req.ContentMD, render.Options{
			Mode:                render.RenderModeWeb,
			Locale:              s.deps.Catalog.Match(req.Locale, actor.PreferredLocale),
			Unlocked:            true,
			AuthorOrShareBypass: true,
			IsReaderLoggedIn:    true,
			ShowLineNumbers:     s.deps.Settings.Snapshot().Reading.ShowCodeLineNumbers,
			LookupMediaDims:     s.deps.Content.LookupMediaDims,
		})
		writeJSON(w, http.StatusOK, map[string]any{"html": out.HTML, "toc": out.TOC})
		return
	}

	if rest == "import" && r.Method == http.MethodPost {
		s.handleAdminBatchImportArticles(w, r, actor)
		return
	}

	if rest == "batch" && r.Method == http.MethodPost {
		var req struct {
			Action     string   `json:"action"`
			IDs        []string `json:"ids"`
			CategoryID string   `json:"category_id"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 65536)).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
			return
		}
		action := strings.ToLower(strings.TrimSpace(req.Action))
		switch action {
		case "publish", "draft", "delete", "category", "pin", "unpin":
		default:
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid batch action"})
			return
		}
		ids := dedupCleanIDs(req.IDs, 200)
		if len(ids) == 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "ids required"})
			return
		}
		articles := make([]domain.RawArticle, 0, len(ids))
		for _, aid := range ids {
			raw, gErr := s.deps.Content.GetRawArticleByID(r.Context(), aid)
			if gErr != nil {
				continue
			}
			if !auth.CanManageArticle(actor, raw.AuthorID) {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": s.deps.Catalog.T(s.resolveLocale(r), "error.forbidden")})
				return
			}
			articles = append(articles, raw)
		}
		now := time.Now().UnixMilli()
		affected := 0
		catID := strings.TrimSpace(req.CategoryID)
		for _, raw := range articles {
			switch action {
			case "publish":
				wasPublished := raw.Status == domain.ArticleStatusPublished
				pubAt := raw.PublishedAt
				if pubAt <= 0 {
					pubAt = now
				}
				if _, uErr := s.deps.Store.ExecContext(r.Context(),
					`UPDATE articles SET status = ?, published_at = ?, updated_at = ? WHERE id = ?`,
					domain.ArticleStatusPublished, pubAt, now, raw.ID); uErr == nil {
					raw.Status = domain.ArticleStatusPublished
					raw.PublishedAt = pubAt
					raw.UpdatedAt = now
					s.deps.Content.SearchIndex().Upsert(raw)
					if !wasPublished {
						if s.deps.Plugins != nil {
							s.deps.Plugins.DispatchArticleHook(r.Context(), plugin.HookArticlePublished, articleHookPayloadFromRaw(raw))
						}
						if s.deps.NotifyArticlePublished != nil {
							s.deps.NotifyArticlePublished(r.Context(), raw)
						}
					} else if s.deps.Plugins != nil {
						s.deps.Plugins.DispatchArticleHook(r.Context(), plugin.HookArticleUpdated, articleHookPayloadFromRaw(raw))
					}
					affected++
				}
			case "draft":
				wasPublished := raw.Status == domain.ArticleStatusPublished
				if _, uErr := s.deps.Store.ExecContext(r.Context(),
					`UPDATE articles SET status = ?, updated_at = ? WHERE id = ?`,
					domain.ArticleStatusDraft, now, raw.ID); uErr == nil {
					raw.Status = domain.ArticleStatusDraft
					raw.UpdatedAt = now
					s.deps.Content.SearchIndex().Remove(raw.ID)
					if wasPublished && s.deps.Plugins != nil {
						s.deps.Plugins.DispatchArticleHook(r.Context(), plugin.HookArticleDeleted, articleHookPayloadFromRaw(raw))
					}
					affected++
				}
			case "category":
				if _, uErr := s.deps.Store.ExecContext(r.Context(),
					`UPDATE articles SET category_id = ?, updated_at = ? WHERE id = ?`,
					catID, now, raw.ID); uErr == nil {
					raw.CategoryID = catID
					raw.UpdatedAt = now
					if raw.Status == domain.ArticleStatusPublished {
						s.deps.Content.SearchIndex().Upsert(raw)
						if s.deps.Plugins != nil {
							s.deps.Plugins.DispatchArticleHook(r.Context(), plugin.HookArticleUpdated, articleHookPayloadFromRaw(raw))
						}
					}
					affected++
				}
			case "pin", "unpin":
				pinned := action == "pin"
				if _, uErr := s.deps.Store.ExecContext(r.Context(),
					`UPDATE articles SET is_pinned = ?, updated_at = ? WHERE id = ?`,
					pinned, now, raw.ID); uErr == nil {
					raw.IsPinned = pinned
					raw.UpdatedAt = now
					if raw.Status == domain.ArticleStatusPublished {
						s.deps.Content.SearchIndex().Upsert(raw)
						if s.deps.Plugins != nil {
							s.deps.Plugins.DispatchArticleHook(r.Context(), plugin.HookArticleUpdated, articleHookPayloadFromRaw(raw))
						}
					}
					affected++
				}
			case "delete":
				dErr := s.deps.Store.WithTx(r.Context(), func(tx *sqlx.Tx) error {
					_, _ = tx.ExecContext(r.Context(), `DELETE FROM article_tags WHERE article_id = ?`, raw.ID)
					_, _ = tx.ExecContext(r.Context(), `DELETE FROM series_articles WHERE article_id = ?`, raw.ID)
					_, _ = tx.ExecContext(r.Context(), `DELETE FROM article_revisions WHERE article_id = ?`, raw.ID)
					_, _ = tx.ExecContext(r.Context(), `DELETE FROM article_backlinks WHERE source_article_id = ? OR target_article_id = ?`, raw.ID, raw.ID)
					s.deleteTargetCommentsAndReactionsTx(r.Context(), tx, domain.TargetTypeArticle, raw.ID)
					_, err := tx.ExecContext(r.Context(), `DELETE FROM articles WHERE id = ?`, raw.ID)
					return err
				})
				if dErr == nil {
					s.deps.Content.SearchIndex().Remove(raw.ID)
					if s.deps.Plugins != nil {
						deletedCopy := raw
						deletedCopy.Status = "deleted"
						s.deps.Plugins.DispatchArticleHook(r.Context(), plugin.HookArticleDeleted, articleHookPayloadFromRaw(deletedCopy))
					}
					affected++
				}
			}
		}
		if affected > 0 {
			s.invalidateMediaRefCache()
		}
		s.recordAuditLog(r, actor, "article.batch", "article", action, map[string]any{"action": action, "affected": affected})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "affected": affected})
		return
	}

	if rest == "" {
		switch r.Method {
		case http.MethodGet:
			page, perPage := parseAdminPageParams(r, 50, 300)
			status := strings.TrimSpace(r.URL.Query().Get("status"))
			visFilter := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("visibility")))
			q := strings.TrimSpace(r.URL.Query().Get("q"))
			catID := strings.TrimSpace(r.URL.Query().Get("category_id"))
			tagID := strings.TrimSpace(r.URL.Query().Get("tag_id"))
			sortBy := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("sort")))

			scopeWhere := "1=1"
			var scopeArgs []any
			if actor.Role == domain.RoleEditor {
				scopeWhere = "author_id = ?"
				scopeArgs = append(scopeArgs, actor.ID)
			}

			var cntAll, cntPub, cntSched, cntDraft, cntProt int
			cntArgs := append([]any{true}, scopeArgs...)
			_ = s.deps.Store.QueryRowContext(r.Context(),
				`SELECT COUNT(*),
				        COALESCE(SUM(CASE WHEN status = 'published' THEN 1 ELSE 0 END), 0),
				        COALESCE(SUM(CASE WHEN status = 'scheduled' THEN 1 ELSE 0 END), 0),
				        COALESCE(SUM(CASE WHEN status = 'draft' THEN 1 ELSE 0 END), 0),
				        COALESCE(SUM(CASE WHEN password_ciphertext <> '' OR has_content_lock = ? THEN 1 ELSE 0 END), 0)
				 FROM articles WHERE `+scopeWhere, cntArgs...).Scan(&cntAll, &cntPub, &cntSched, &cntDraft, &cntProt)
			statusCounts := map[string]int{
				"all":       cntAll,
				"published": cntPub,
				"scheduled": cntSched,
				"draft":     cntDraft,
				"protected": cntProt,
			}

			where := []string{scopeWhere}
			args := append([]any{}, scopeArgs...)
			if status == "protected" {
				where = append(where, "(password_ciphertext <> '' OR has_content_lock = ?)")
				args = append(args, true)
			} else if status != "" && status != "all" {
				where = append(where, "status = ?")
				args = append(args, status)
			}
			switch visFilter {
			case "password":
				where = append(where, "password_ciphertext <> ''")
			case "locked", "protected":
				where = append(where, "(password_ciphertext <> '' OR has_content_lock = ?)")
				args = append(args, true)
			case "pinned":
				where = append(where, "is_pinned = ?")
				args = append(args, true)
			case "public":
				where = append(where, "(password_ciphertext = '' AND has_content_lock = ?)")
				args = append(args, false)
			}
			if catID != "" {
				where = append(where, "category_id = ?")
				args = append(args, catID)
			}
			if tagID != "" {
				where = append(where, "EXISTS (SELECT 1 FROM article_tags at WHERE at.article_id = articles.id AND at.tag_id = ?)")
				args = append(args, tagID)
			}
			if q != "" {
				where = append(where, "(LOWER(title) LIKE LOWER(?) ESCAPE '!' OR LOWER(slug) LIKE LOWER(?) ESCAPE '!' OR LOWER(summary_md) LIKE LOWER(?) ESCAPE '!')")
				pat := "%" + escapeLikePattern(q) + "%"
				args = append(args, pat, pat, pat)
			}

			orderClause := "is_pinned DESC, updated_at DESC"
			switch sortBy {
			case "published":
				orderClause = "is_pinned DESC, published_at DESC, updated_at DESC"
			case "views":
				orderClause = "view_count DESC, updated_at DESC"
			case "comments":
				orderClause = "comment_count DESC, updated_at DESC"
			case "title":
				orderClause = "title ASC, updated_at DESC"
			}

			whereSQL := strings.Join(where, " AND ")
			var total int
			_ = s.deps.Store.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM articles WHERE `+whereSQL, args...).Scan(&total)

			baseSQL := `SELECT id, author_id, category_id, title, slug, legacy_permalink, summary_md, '' AS content_md,
				        cover_url, status, published_at, scheduled_at, is_pinned, allow_comment,
				        password_ciphertext, has_content_lock, audio_url, audio_duration_sec, audio_size_bytes, audio_mime,
				        lang, translation_group, license_override, word_count, reading_minutes, view_count, comment_count,
				        created_at, updated_at
				 FROM articles WHERE ` + whereSQL + ` ORDER BY ` + orderClause
			qSQL, pageArgs := s.deps.Store.Dialect().PaginateSQL(baseSQL, perPage, (page-1)*perPage)
			fullArgs := append(append([]any{}, args...), pageArgs...)
			rows, err := sqlx.Select[domain.RawArticle](r.Context(), s.deps.Store, qSQL, fullArgs...)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
				return
			}
			catRows, _ := sqlx.Select[domain.Category](r.Context(), s.deps.Store, `SELECT id, name FROM categories`)
			catNames := make(map[string]string, len(catRows))
			for _, c := range catRows {
				catNames[c.ID] = c.Name
			}
			items := make([]map[string]any, 0, len(rows))
			for _, row := range rows {
				hasArticlePwd := strings.TrimSpace(row.PasswordCiphertext) != ""
				hasPwd := hasArticlePwd || row.HasContentLock
				row.PasswordCiphertext = ""
				items = append(items, map[string]any{
					"article":               row,
					"category_name":         catNames[row.CategoryID],
					"is_password_protected": hasPwd,
					"has_password":          hasArticlePwd,
				})
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"items":         items,
				"articles":      items,
				"total":         total,
				"page":          page,
				"per_page":      perPage,
				"page_size":     perPage,
				"status_counts": statusCounts,
			})

		case http.MethodPost:
			s.saveAdminArticle(w, r, actor, "")

		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
		return
	}

	parts := strings.Split(rest, "/")
	artID := parts[0]
	raw, err := s.deps.Content.GetRawArticleByID(r.Context(), artID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "article not found"})
		return
	}
	if !auth.CanManageArticle(actor, raw.AuthorID) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": s.deps.Catalog.T(s.resolveLocale(r), "error.forbidden")})
		return
	}

	if len(parts) == 2 && parts[1] == "revisions" && r.Method == http.MethodGet {
		revs, _ := sqlx.Select[domain.ArticleRevision](r.Context(), s.deps.Store,
			`SELECT * FROM article_revisions WHERE article_id = ? ORDER BY created_at DESC LIMIT 20`, artID)
		if revs == nil {
			revs = []domain.ArticleRevision{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": revs})
		return
	}

	if len(parts) == 4 && parts[1] == "revisions" && parts[3] == "restore" && r.Method == http.MethodPost {
		revID := parts[2]
		rev, rErr := sqlx.Get[domain.ArticleRevision](r.Context(), s.deps.Store,
			`SELECT * FROM article_revisions WHERE id = ? AND article_id = ?`, revID, artID)
		if rErr != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "revision not found"})
			return
		}
		now := time.Now().UnixMilli()
		words, mins := computeReadingMetrics(rev.ContentMD)
		hasLock := render.ContainsLockDirective(rev.ContentMD)
		err := s.deps.Store.WithTx(r.Context(), func(tx *sqlx.Tx) error {
			_ = s.appendArticleRevisionTx(r.Context(), tx, raw, actor.DisplayName, "Before rollback to "+revID)
			raw.Title = rev.Title
			raw.SummaryMD = rev.SummaryMD
			raw.ContentMD = rev.ContentMD
			raw.WordCount = words
			raw.ReadingMinutes = mins
			raw.HasContentLock = hasLock
			raw.UpdatedAt = now
			if _, uErr := tx.ExecContext(r.Context(),
				`UPDATE articles SET title = ?, summary_md = ?, content_md = ?, has_content_lock = ?, word_count = ?, reading_minutes = ?, updated_at = ? WHERE id = ?`,
				raw.Title, raw.SummaryMD, raw.ContentMD, raw.HasContentLock, raw.WordCount, raw.ReadingMinutes, raw.UpdatedAt, raw.ID); uErr != nil {
				return uErr
			}
			return s.deps.Content.ReindexArticleBacklinksTx(r.Context(), tx, raw)
		})
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		s.invalidateMediaRefCache()
		if raw.Status == domain.ArticleStatusPublished {
			s.deps.Content.SearchIndex().Upsert(raw)
			if s.deps.Plugins != nil {
				s.deps.Plugins.DispatchArticleHook(r.Context(), plugin.HookArticleUpdated, articleHookPayloadFromRaw(raw))
			}
		}
		raw.PasswordCiphertext = ""
		writeJSON(w, http.StatusOK, map[string]any{"article": raw})
		return
	}

	if len(parts) == 2 && parts[1] == "share-token" && r.Method == http.MethodPost {
		var req struct {
			TTLHours int `json:"ttl_hours"`
		}
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req)
		if req.TTLHours <= 0 {
			req.TTLHours = 72
		}
		if req.TTLHours > 24*90 {
			req.TTLHours = 24 * 90
		}
		tok, expUnix, tErr := s.deps.Content.Guard().IssueShareToken(r.Context(), raw, time.Duration(req.TTLHours)*time.Hour)
		if tErr != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": tErr.Error()})
			return
		}
		shareURL := strings.TrimRight(s.publicBaseURL(r), "/") + visibility.ArticlePath(raw.Slug) + "?preview=" + tok
		writeJSON(w, http.StatusOK, map[string]any{
			"token":      tok,
			"expires_at": expUnix,
			"share_url":  shareURL,
			"ttl_hours":  req.TTLHours,
		})
		return
	}

	if len(parts) == 2 && parts[1] == "pin" && r.Method == http.MethodPost {
		nextPinned := !raw.IsPinned
		now := time.Now().UnixMilli()
		if _, err := s.deps.Store.ExecContext(r.Context(),
			`UPDATE articles SET is_pinned = ?, updated_at = ? WHERE id = ?`,
			nextPinned, now, raw.ID); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		raw.IsPinned = nextPinned
		if raw.Status == domain.ArticleStatusPublished {
			s.deps.Content.SearchIndex().Upsert(raw)
			if s.deps.Plugins != nil {
				s.deps.Plugins.DispatchArticleHook(r.Context(), plugin.HookArticleUpdated, articleHookPayloadFromRaw(raw))
			}
		}
		s.recordAuditLog(r, actor, "article.pin", "article", raw.ID, map[string]any{"is_pinned": nextPinned})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "is_pinned": nextPinned})
		return
	}

	if len(parts) == 1 {
		switch r.Method {
		case http.MethodGet:
			tagRows, _ := sqlx.Select[domain.Tag](r.Context(), s.deps.Store,
				`SELECT t.* FROM tags t INNER JOIN article_tags at ON at.tag_id = t.id WHERE at.article_id = ? ORDER BY t.name ASC`, artID)
			tags := make([]string, 0, len(tagRows))
			for _, tg := range tagRows {
				tags = append(tags, tg.Name)
			}
			var seriesID string
			_ = s.deps.Store.QueryRowContext(r.Context(), `SELECT series_id FROM series_articles WHERE article_id = ? LIMIT 1`, artID).Scan(&seriesID)
			var catName string
			if raw.CategoryID != "" {
				_ = s.deps.Store.QueryRowContext(r.Context(), `SELECT name FROM categories WHERE id = ?`, raw.CategoryID).Scan(&catName)
			}
			plainPwd, hasPwd := s.deps.Content.Guard().DecryptArticlePassword(raw)
			if strings.HasPrefix(plainPwd, "\x00") {
				plainPwd = ""
			}
			authHdr := strings.TrimSpace(r.Header.Get("Authorization"))
			if strings.HasPrefix(authHdr, "Bearer ") {
				if _, _, wErr := s.deps.Auth.AuthenticateAPIToken(r.Context(), authHdr, auth.ScopeArticlesWrite); wErr != nil {
					plainPwd = ""
				}
			}
			raw.PasswordCiphertext = ""
			writeJSON(w, http.StatusOK, map[string]any{
				"article":               raw,
				"category_name":         catName,
				"tags":                  tags,
				"series_id":             seriesID,
				"is_password_protected": hasPwd,
				"has_password":          hasPwd,
				"password":              plainPwd,
			})

		case http.MethodPut:
			s.saveAdminArticle(w, r, actor, artID)

		case http.MethodDelete:
			err := s.deps.Store.WithTx(r.Context(), func(tx *sqlx.Tx) error {
				_, _ = tx.ExecContext(r.Context(), `DELETE FROM article_tags WHERE article_id = ?`, artID)
				_, _ = tx.ExecContext(r.Context(), `DELETE FROM series_articles WHERE article_id = ?`, artID)
				_, _ = tx.ExecContext(r.Context(), `DELETE FROM article_revisions WHERE article_id = ?`, artID)
				_, _ = tx.ExecContext(r.Context(), `DELETE FROM article_backlinks WHERE source_article_id = ? OR target_article_id = ?`, artID, artID)
				s.deleteTargetCommentsAndReactionsTx(r.Context(), tx, domain.TargetTypeArticle, artID)
				_, dErr := tx.ExecContext(r.Context(), `DELETE FROM articles WHERE id = ?`, artID)
				return dErr
			})
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
				return
			}
			s.invalidateMediaRefCache()
			s.deps.Content.SearchIndex().Remove(artID)
			if s.deps.Plugins != nil {
				deletedCopy := raw
				deletedCopy.Status = "deleted"
				s.deps.Plugins.DispatchArticleHook(r.Context(), plugin.HookArticleDeleted, articleHookPayloadFromRaw(deletedCopy))
			}
			s.recordAuditLog(r, actor, "article.delete", "article", artID, map[string]any{"title": raw.Title})
			writeJSON(w, http.StatusOK, map[string]bool{"ok": true})

		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
		return
	}
	http.NotFound(w, r)
}

func articleHookPayloadFromRaw(raw domain.RawArticle) plugin.ArticleHookPayload {
	return plugin.ArticleHookPayload{
		ID:                  raw.ID,
		Title:               raw.Title,
		Slug:                raw.Slug,
		Lang:                raw.Lang,
		Status:              raw.Status,
		SummaryMD:           raw.SummaryMD,
		ContentMD:           raw.ContentMD,
		CoverURL:            raw.CoverURL,
		AudioURL:            raw.AudioURL,
		IsPasswordProtected: strings.TrimSpace(raw.PasswordCiphertext) != "",
		HasContentLock:      raw.HasContentLock,
	}
}

func (s *AppServer) saveAdminArticle(w http.ResponseWriter, r *http.Request, actor *domain.User, existingID string) {
	var in adminArticlePayload
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20)).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	title := strings.TrimSpace(in.Title)
	if title == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "title is required"})
		return
	}

	cleanCover := ""
	if rawCover := strings.TrimSpace(in.CoverURL); rawCover != "" {
		u, ok := sanitizeRedirectTargetURL(rawCover)
		if !ok {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid cover_url"})
			return
		}
		cleanCover = u
	}
	cleanAudio := ""
	if rawAudio := strings.TrimSpace(in.AudioURL); rawAudio != "" {
		u, ok := sanitizeRedirectTargetURL(rawAudio)
		if !ok {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid audio_url"})
			return
		}
		cleanAudio = u
	}

	now := time.Now().UnixMilli()
	var cur domain.RawArticle
	isCreate := existingID == ""
	wasPublished := false

	if isCreate {
		cur.ID = id.Next()
		cur.AuthorID = actor.ID
		cur.CreatedAt = now
	} else {
		existing, err := s.deps.Content.GetRawArticleByID(r.Context(), existingID)
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "article not found"})
			return
		}
		if !auth.CanManageArticle(actor, existing.AuthorID) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
			return
		}
		cur = existing
		wasPublished = existing.Status == domain.ArticleStatusPublished
	}

	if in.ClearLegacyPermalink {
		if cur.LegacyPermalink != "" {
			if err := auth.RequireAdmin(actor); err != nil {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": "only administrators can modify legacy_permalink"})
				return
			}
		}
		cur.LegacyPermalink = ""
	} else if rawLeg := strings.TrimSpace(repo.StripASCIIControls(in.LegacyPermalink)); rawLeg != "" {
		if rawLeg != cur.LegacyPermalink {
			if err := auth.RequireAdmin(actor); err != nil {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": "only administrators can set or modify legacy_permalink"})
				return
			}
			cleanLeg, vErr := s.validateLegacyPermalink(r.Context(), rawLeg, cur.ID)
			if vErr != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": vErr.Error()})
				return
			}
			cur.LegacyPermalink = cleanLeg
		}
	}

	slug := sanitizeSlug(in.Slug)
	if in.Slug == "" {
		slug = sanitizeSlug(title)
	}
	slug = s.ensureUniqueArticleSlug(r.Context(), slug, cur.ID)

	status := strings.ToLower(strings.TrimSpace(in.Status))
	switch status {
	case domain.ArticleStatusPublished, domain.ArticleStatusDraft, domain.ArticleStatusScheduled:
	default:
		status = domain.ArticleStatusPublished
	}
	pubAt := in.PublishedAt
	if status == domain.ArticleStatusPublished && pubAt <= 0 {
		if cur.PublishedAt > 0 {
			pubAt = cur.PublishedAt
		} else {
			pubAt = now
		}
	}
	lang := strings.TrimSpace(in.Lang)
	if lang == "" {
		lang = s.deps.Settings.Snapshot().Site.DefaultLocale
	}
	words, mins := computeReadingMetrics(in.ContentMD)

	pwdCipher := cur.PasswordCiphertext
	if in.ClearPassword {
		pwdCipher = ""
	} else if in.Password != nil {
		p := strings.TrimSpace(*in.Password)
		if p != "" {
			sealed, sErr := s.deps.Content.Guard().SealArticlePassword(cur.ID, p)
			if sErr != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": sErr.Error()})
				return
			}
			pwdCipher = sealed
		} else if isCreate {
			pwdCipher = ""
		}
	}

	oldContentChanged := !isCreate && (cur.Title != title || cur.ContentMD != in.ContentMD || cur.SummaryMD != in.SummaryMD)
	prevSnapshot := cur

	cur.CategoryID = strings.TrimSpace(in.CategoryID)
	cur.Title = title
	cur.Slug = slug
	cur.SummaryMD = in.SummaryMD
	cur.ContentMD = in.ContentMD
	cur.CoverURL = cleanCover
	cur.Status = status
	cur.PublishedAt = pubAt
	cur.ScheduledAt = in.ScheduledAt
	cur.IsPinned = in.IsPinned
	cur.AllowComment = in.AllowComment
	cur.PasswordCiphertext = pwdCipher
	cur.HasContentLock = render.ContainsLockDirective(in.ContentMD)
	cur.AudioURL = cleanAudio
	cur.AudioDurationSec = in.AudioDurationSec
	cur.AudioSizeBytes = in.AudioSizeBytes
	cur.AudioMIME = strings.TrimSpace(in.AudioMIME)
	cur.Lang = lang
	cur.TranslationGroup = strings.TrimSpace(in.TranslationGroup)
	cur.LicenseOverride = strings.TrimSpace(in.LicenseOverride)
	cur.WordCount = words
	cur.ReadingMinutes = mins
	cur.UpdatedAt = now

	err := s.deps.Store.WithTx(r.Context(), func(tx *sqlx.Tx) error {
		if oldContentChanged {
			_ = s.appendArticleRevisionTx(r.Context(), tx, prevSnapshot, actor.DisplayName, in.RevisionNote)
		}
		if isCreate {
			_, iErr := tx.ExecContext(r.Context(),
				`INSERT INTO articles (
					id, author_id, category_id, title, slug, legacy_permalink, summary_md, content_md,
					cover_url, status, published_at, scheduled_at, is_pinned, allow_comment,
					password_ciphertext, has_content_lock, audio_url, audio_duration_sec, audio_size_bytes, audio_mime,
					lang, translation_group, license_override, word_count, reading_minutes, view_count, comment_count,
					created_at, updated_at
				) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, 0, ?, ?)`,
				cur.ID, cur.AuthorID, cur.CategoryID, cur.Title, cur.Slug, cur.LegacyPermalink, cur.SummaryMD, cur.ContentMD,
				cur.CoverURL, cur.Status, cur.PublishedAt, cur.ScheduledAt, cur.IsPinned, cur.AllowComment,
				cur.PasswordCiphertext, cur.HasContentLock, cur.AudioURL, cur.AudioDurationSec, cur.AudioSizeBytes, cur.AudioMIME,
				cur.Lang, cur.TranslationGroup, cur.LicenseOverride, cur.WordCount, cur.ReadingMinutes,
				cur.CreatedAt, cur.UpdatedAt)
			if iErr != nil {
				return iErr
			}
			_ = s.appendArticleRevisionTx(r.Context(), tx, cur, actor.DisplayName, "Initial version")
		} else {
			_, uErr := tx.ExecContext(r.Context(),
				`UPDATE articles SET
					category_id = ?, title = ?, slug = ?, legacy_permalink = ?, summary_md = ?, content_md = ?,
					cover_url = ?, status = ?, published_at = ?, scheduled_at = ?, is_pinned = ?, allow_comment = ?,
					password_ciphertext = ?, has_content_lock = ?, audio_url = ?, audio_duration_sec = ?, audio_size_bytes = ?, audio_mime = ?,
					lang = ?, translation_group = ?, license_override = ?, word_count = ?, reading_minutes = ?, updated_at = ?
				 WHERE id = ?`,
				cur.CategoryID, cur.Title, cur.Slug, cur.LegacyPermalink, cur.SummaryMD, cur.ContentMD,
				cur.CoverURL, cur.Status, cur.PublishedAt, cur.ScheduledAt, cur.IsPinned, cur.AllowComment,
				cur.PasswordCiphertext, cur.HasContentLock, cur.AudioURL, cur.AudioDurationSec, cur.AudioSizeBytes, cur.AudioMIME,
				cur.Lang, cur.TranslationGroup, cur.LicenseOverride, cur.WordCount, cur.ReadingMinutes, cur.UpdatedAt, cur.ID)
			if uErr != nil {
				return uErr
			}
		}

		if err := s.bindArticleTagsByNamesTx(r.Context(), tx, cur.ID, in.Tags, now); err != nil {
			return err
		}
		_, _ = tx.ExecContext(r.Context(), `DELETE FROM series_articles WHERE article_id = ?`, cur.ID)
		if sid := strings.TrimSpace(in.SeriesID); sid != "" {
			var maxOrd int
			_ = tx.QueryRowContext(r.Context(), `SELECT COALESCE(MAX(sort_order), 0) FROM series_articles WHERE series_id = ?`, sid).Scan(&maxOrd)
			_, _ = tx.ExecContext(r.Context(),
				`INSERT INTO series_articles (series_id, article_id, sort_order) VALUES (?, ?, ?)`,
				sid, cur.ID, maxOrd+1)
		}
		return s.deps.Content.ReindexArticleBacklinksTx(r.Context(), tx, cur)
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	s.invalidateMediaRefCache()
	if cur.Status == domain.ArticleStatusPublished {
		s.deps.Content.SearchIndex().Upsert(cur)
	} else {
		s.deps.Content.SearchIndex().Remove(cur.ID)
	}

	if s.deps.Plugins != nil {
		switch {
		case cur.Status == domain.ArticleStatusPublished && !wasPublished:
			s.deps.Plugins.DispatchArticleHook(r.Context(), plugin.HookArticlePublished, articleHookPayloadFromRaw(cur))
		case cur.Status == domain.ArticleStatusPublished && wasPublished:
			s.deps.Plugins.DispatchArticleHook(r.Context(), plugin.HookArticleUpdated, articleHookPayloadFromRaw(cur))
		case cur.Status != domain.ArticleStatusPublished && wasPublished:
			s.deps.Plugins.DispatchArticleHook(r.Context(), plugin.HookArticleDeleted, articleHookPayloadFromRaw(cur))
		}
	}
	if cur.Status == domain.ArticleStatusPublished && !wasPublished && s.deps.NotifyArticlePublished != nil {
		s.deps.NotifyArticlePublished(r.Context(), cur)
	}

	respArt := cur
	hasPwd := respArt.PasswordCiphertext != ""
	respArt.PasswordCiphertext = ""
	code := http.StatusOK
	if isCreate {
		code = http.StatusCreated
	}
	s.recordAuditLog(r, actor, "article.save", "article", cur.ID, map[string]any{"slug": cur.Slug, "status": cur.Status})
	writeJSON(w, code, map[string]any{"article": respArt, "is_password_protected": hasPwd, "has_password": hasPwd})
}

func (s *AppServer) appendArticleRevisionTx(ctx context.Context, tx *sqlx.Tx, art domain.RawArticle, authorName string, note string) error {
	now := time.Now().UnixMilli()
	if strings.TrimSpace(note) == "" {
		note = "Auto snapshot"
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO article_revisions (id, article_id, title, summary_md, content_md, author_name, note, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		id.Next(), art.ID, art.Title, art.SummaryMD, art.ContentMD, authorName, strings.TrimSpace(note), now,
	); err != nil {
		return err
	}
	oldRows, err := sqlx.Select[struct {
		ID string `db:"id"`
	}](ctx, tx, `SELECT id FROM article_revisions WHERE article_id = ? ORDER BY created_at DESC`, art.ID)
	if err == nil && len(oldRows) > 20 {
		for _, r := range oldRows[20:] {
			_, _ = tx.ExecContext(ctx, `DELETE FROM article_revisions WHERE id = ?`, r.ID)
		}
	}
	return nil
}

func (s *AppServer) ensureUniqueArticleSlug(ctx context.Context, base string, selfID string) string {
	cand := base
	for i := 2; i < 200; i++ {
		var existingID string
		err := s.deps.Store.QueryRowContext(ctx, `SELECT id FROM articles WHERE slug = ?`, cand).Scan(&existingID)
		if errors.Is(err, sql.ErrNoRows) || existingID == selfID {
			return cand
		}
		cand = fmt.Sprintf("%s-%d", base, i)
	}
	return base + "-" + strings.ToLower(id.Next())[:6]
}

func (s *AppServer) resolveOrCreateCategoryTx(ctx context.Context, tx *sqlx.Tx, name string, now int64) (string, error) {
	clean := strings.TrimSpace(name)
	if clean == "" {
		return "", nil
	}
	slug := sanitizeSlug(clean)
	cat, err := sqlx.Get[domain.Category](ctx, tx, `SELECT id FROM categories WHERE LOWER(name) = LOWER(?) OR slug = ?`, clean, slug)
	if err == nil {
		return cat.ID, nil
	}
	newID := id.Next()
	_, iErr := tx.ExecContext(ctx,
		`INSERT INTO categories (id, name, slug, description, icon, sort_order, created_at, updated_at)
		 VALUES (?, ?, ?, '', '', 10, ?, ?)`, newID, clean, slug, now, now)
	return newID, iErr
}

func (s *AppServer) bindArticleTagsByNamesTx(ctx context.Context, tx *sqlx.Tx, articleID string, names []string, now int64) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM article_tags WHERE article_id = ?`, articleID); err != nil {
		return err
	}
	seen := make(map[string]bool)
	for _, raw := range names {
		tName := strings.TrimSpace(raw)
		if tName == "" {
			continue
		}
		low := strings.ToLower(tName)
		if seen[low] {
			continue
		}
		seen[low] = true
		tSlug := sanitizeSlug(tName)
		tg, err := sqlx.Get[domain.Tag](ctx, tx, `SELECT id FROM tags WHERE LOWER(name) = LOWER(?) OR slug = ?`, tName, tSlug)
		tagID := tg.ID
		if err != nil {
			tagID = id.Next()
			if _, iErr := tx.ExecContext(ctx,
				`INSERT INTO tags (id, name, slug, created_at) VALUES (?, ?, ?, ?)`,
				tagID, tName, tSlug, now); iErr != nil {
				return iErr
			}
		}
		if _, bErr := tx.ExecContext(ctx,
			`INSERT INTO article_tags (article_id, tag_id) VALUES (?, ?)`, articleID, tagID); bErr != nil {
			return bErr
		}
	}
	return nil
}

// --- Workspace 2: Standalone Pages, Categories, Tags, Series, Bookshelf, Comments & Media ---

func sanitizePageTemplateName(raw string) string {
	clean := strings.ToLower(strings.TrimSpace(raw))
	if clean == "" || len(clean) > 32 {
		return "default"
	}
	for i := 0; i < len(clean); i++ {
		c := clean[i]
		isAlnum := (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
		if i == 0 && !isAlnum {
			return "default"
		}
		if !isAlnum && c != '-' && c != '_' {
			return "default"
		}
	}
	return clean
}

func (s *AppServer) dispatchPageLifecycleHook(ctx context.Context, hookName string, pg domain.Page, status string) {
	if s.deps.Plugins == nil {
		return
	}
	_, _ = s.deps.Plugins.DispatchHook(ctx, hookName, plugin.PageHookPayload{
		ID:       pg.ID,
		Title:    pg.Title,
		Slug:     pg.Slug,
		Template: sanitizePageTemplateName(pg.Template),
		Status:   status,
	})
}

func (s *AppServer) handleAdminPages(w http.ResponseWriter, r *http.Request, actor *domain.User, rest string) {
	rest = strings.TrimPrefix(rest, "/")
	if rest == "" {
		switch r.Method {
		case http.MethodGet:
			page, perPage := parseAdminPageParams(r, 50, 200)
			status := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("status")))
			q := strings.TrimSpace(r.URL.Query().Get("q"))

			where := []string{"1=1"}
			var args []any
			if actor.Role == domain.RoleEditor {
				where = append(where, "author_id = ?")
				args = append(args, actor.ID)
			}
			switch status {
			case "published":
				where = append(where, "is_published = ?")
				args = append(args, true)
			case "draft":
				where = append(where, "is_published = ?")
				args = append(args, false)
			}
			if q != "" {
				pat := "%" + escapeLikePattern(q) + "%"
				where = append(where, "(LOWER(title) LIKE LOWER(?) ESCAPE '!' OR LOWER(slug) LIKE LOWER(?) ESCAPE '!')")
				args = append(args, pat, pat)
			}
			whereSQL := strings.Join(where, " AND ")
			var total int
			_ = s.deps.Store.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM pages WHERE `+whereSQL, args...).Scan(&total)

			baseSQL := `SELECT * FROM pages WHERE ` + whereSQL + ` ORDER BY updated_at DESC`
			qSQL, pageArgs := s.deps.Store.Dialect().PaginateSQL(baseSQL, perPage, (page-1)*perPage)
			fullArgs := append(append([]any{}, args...), pageArgs...)
			rows, _ := sqlx.Select[domain.Page](r.Context(), s.deps.Store, qSQL, fullArgs...)
			if rows == nil {
				rows = []domain.Page{}
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"items":     rows,
				"pages":     rows,
				"total":     total,
				"page":      page,
				"per_page":  perPage,
				"page_size": perPage,
			})

		case http.MethodPost:
			var in domain.Page
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&in); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
				return
			}
			in.Title = strings.TrimSpace(in.Title)
			if in.Title == "" {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "title is required"})
				return
			}
			in.ID = id.Next()
			in.AuthorID = actor.ID
			in.Slug = sanitizeSlug(in.Slug)
			if in.Slug == "" {
				in.Slug = sanitizeSlug(in.Title)
			}
			in.Template = sanitizePageTemplateName(in.Template)
			if in.Lang == "" {
				in.Lang = s.deps.Settings.Snapshot().Site.DefaultLocale
			}
			now := time.Now().UnixMilli()
			in.CreatedAt = now
			in.UpdatedAt = now
			if _, err := s.deps.Store.ExecContext(r.Context(),
				`INSERT INTO pages (id, author_id, title, slug, template, content_md, is_published, allow_comment, show_toc, lang, translation_group, view_count, comment_count, created_at, updated_at)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, 0, ?, ?)`,
				in.ID, in.AuthorID, in.Title, in.Slug, in.Template, in.ContentMD, in.IsPublished, in.AllowComment, in.ShowTOC,
				in.Lang, strings.TrimSpace(in.TranslationGroup), in.CreatedAt, in.UpdatedAt); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
			s.invalidateMediaRefCache()
			if in.IsPublished {
				s.dispatchPageLifecycleHook(r.Context(), plugin.HookPagePublished, in, "published")
			}
			writeJSON(w, http.StatusCreated, map[string]any{"page": in})

		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
		return
	}

	pg, err := sqlx.Get[domain.Page](r.Context(), s.deps.Store, `SELECT * FROM pages WHERE id = ?`, rest)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "page not found"})
		return
	}
	if !auth.CanManageArticle(actor, pg.AuthorID) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"page": pg})
	case http.MethodPut:
		var in domain.Page
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&in); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
			return
		}
		wasPublished := pg.IsPublished
		slug := sanitizeSlug(in.Slug)
		if slug == "" {
			slug = pg.Slug
		}
		tmpl := pg.Template
		if strings.TrimSpace(in.Template) != "" {
			tmpl = sanitizePageTemplateName(in.Template)
		} else {
			tmpl = sanitizePageTemplateName(tmpl)
		}
		now := time.Now().UnixMilli()
		_, err := s.deps.Store.ExecContext(r.Context(),
			`UPDATE pages SET title = ?, slug = ?, template = ?, content_md = ?, is_published = ?, allow_comment = ?, show_toc = ?, lang = ?, translation_group = ?, updated_at = ? WHERE id = ?`,
			strings.TrimSpace(in.Title), slug, tmpl, in.ContentMD, in.IsPublished, in.AllowComment, in.ShowTOC,
			strings.TrimSpace(in.Lang), strings.TrimSpace(in.TranslationGroup), now, pg.ID)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		s.invalidateMediaRefCache()
		pg, _ = sqlx.Get[domain.Page](r.Context(), s.deps.Store, `SELECT * FROM pages WHERE id = ?`, pg.ID)
		if !wasPublished && pg.IsPublished {
			s.dispatchPageLifecycleHook(r.Context(), plugin.HookPagePublished, pg, "published")
		} else if wasPublished && pg.IsPublished {
			s.dispatchPageLifecycleHook(r.Context(), plugin.HookPageUpdated, pg, "published")
		} else if wasPublished && !pg.IsPublished {
			s.dispatchPageLifecycleHook(r.Context(), plugin.HookPageDeleted, pg, "draft")
		}
		writeJSON(w, http.StatusOK, map[string]any{"page": pg})
	case http.MethodDelete:
		_ = s.deps.Store.WithTx(r.Context(), func(tx *sqlx.Tx) error {
			s.deleteTargetCommentsAndReactionsTx(r.Context(), tx, domain.TargetTypePage, pg.ID)
			_, dErr := tx.ExecContext(r.Context(), `DELETE FROM pages WHERE id = ?`, pg.ID)
			return dErr
		})
		s.invalidateMediaRefCache()
		s.dispatchPageLifecycleHook(r.Context(), plugin.HookPageDeleted, pg, "deleted")
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *AppServer) handleAdminCategories(w http.ResponseWriter, r *http.Request, actor *domain.User, rest string) {
	rest = strings.TrimPrefix(rest, "/")
	if rest == "" {
		switch r.Method {
		case http.MethodGet:
			page, perPage := parseAdminPageParams(r, 100, 500)
			q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
			rows, _ := s.deps.Content.ListCategoriesWithCounts(r.Context())
			filtered := rows[:0:0]
			for _, cat := range rows {
				if q != "" &&
					!strings.Contains(strings.ToLower(cat.Name), q) &&
					!strings.Contains(strings.ToLower(cat.Slug), q) &&
					!strings.Contains(strings.ToLower(cat.Description), q) {
					continue
				}
				filtered = append(filtered, cat)
			}
			total := len(filtered)
			start := (page - 1) * perPage
			if start > total {
				start = total
			}
			end := start + perPage
			if end > total {
				end = total
			}
			paged := filtered[start:end]
			writeJSON(w, http.StatusOK, map[string]any{
				"items":      paged,
				"categories": paged,
				"total":      total,
				"page":       page,
				"per_page":   perPage,
				"page_size":  perPage,
			})
		case http.MethodPost:
			if err := auth.RequireAdmin(actor); err != nil {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
				return
			}
			var in domain.Category
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384)).Decode(&in); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
				return
			}
			in.Name = strings.TrimSpace(in.Name)
			if in.Name == "" {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name is required"})
				return
			}
			cleanIcon, ok := sanitizeAdminIconField(in.Icon)
			if !ok {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid category icon"})
				return
			}
			in.Icon = cleanIcon
			in.ID = id.Next()
			if strings.TrimSpace(in.Slug) == "" {
				in.Slug = sanitizeSlug(in.Name)
			} else {
				in.Slug = sanitizeSlug(in.Slug)
			}
			now := time.Now().UnixMilli()
			in.CreatedAt = now
			in.UpdatedAt = now
			if _, err := s.deps.Store.ExecContext(r.Context(),
				`INSERT INTO categories (id, name, slug, description, icon, sort_order, created_at, updated_at)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
				in.ID, in.Name, in.Slug, strings.TrimSpace(in.Description), in.Icon, in.SortOrder, now, now); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
			writeJSON(w, http.StatusCreated, map[string]any{"category": in})
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
		return
	}

	switch r.Method {
	case http.MethodPut:
		if err := auth.RequireAdmin(actor); err != nil {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
			return
		}
		var in domain.Category
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384)).Decode(&in); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
			return
		}
		cleanIcon, ok := sanitizeAdminIconField(in.Icon)
		if !ok {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid category icon"})
			return
		}
		name := strings.TrimSpace(in.Name)
		slug := sanitizeSlug(in.Slug)
		if slug == "" && name != "" {
			slug = sanitizeSlug(name)
		}
		_, err := s.deps.Store.ExecContext(r.Context(),
			`UPDATE categories SET name = ?, slug = ?, description = ?, icon = ?, sort_order = ?, updated_at = ? WHERE id = ?`,
			name, slug, strings.TrimSpace(in.Description), cleanIcon,
			in.SortOrder, time.Now().UnixMilli(), rest)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	case http.MethodDelete:
		if err := auth.RequireAdmin(actor); err != nil {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
			return
		}
		_ = s.deps.Store.WithTx(r.Context(), func(tx *sqlx.Tx) error {
			_, _ = tx.ExecContext(r.Context(), `UPDATE articles SET category_id = '' WHERE category_id = ?`, rest)
			_, err := tx.ExecContext(r.Context(), `DELETE FROM categories WHERE id = ?`, rest)
			return err
		})
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *AppServer) handleAdminTags(w http.ResponseWriter, r *http.Request, actor *domain.User, rest string) {
	rest = strings.TrimPrefix(rest, "/")
	if rest == "cleanup" && r.Method == http.MethodPost {
		if err := auth.RequireAdmin(actor); err != nil {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
			return
		}
		res, err := s.deps.Store.ExecContext(r.Context(),
			`DELETE FROM tags WHERE id NOT IN (SELECT DISTINCT tag_id FROM article_tags)`)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		deleted, _ := res.RowsAffected()
		s.recordAuditLog(r, actor, "tags.cleanup", "tag", "unused", map[string]any{"deleted": deleted})
		rows, _ := s.deps.Content.ListTagsWithCounts(r.Context(), 500)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "deleted": deleted, "items": rows, "tags": rows})
		return
	}
	if rest == "merge" && r.Method == http.MethodPost {
		if err := auth.RequireAdmin(actor); err != nil {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
			return
		}
		var req struct {
			SourceID  string   `json:"source_id"`
			SourceIDs []string `json:"source_ids"`
			TargetID  string   `json:"target_id"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 65536)).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
			return
		}
		targetID := strings.TrimSpace(req.TargetID)
		if targetID == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "target_id is required"})
			return
		}
		rawSources := append([]string{}, req.SourceIDs...)
		if strings.TrimSpace(req.SourceID) != "" {
			rawSources = append(rawSources, req.SourceID)
		}
		allCandidates := dedupCleanIDs(rawSources, 200)
		sourceIDs := make([]string, 0, len(allCandidates))
		for _, sid := range allCandidates {
			if sid != targetID {
				sourceIDs = append(sourceIDs, sid)
			}
		}
		if len(sourceIDs) == 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "source_ids or source_id is required and must differ from target_id"})
			return
		}
		var targetExists int
		_ = s.deps.Store.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM tags WHERE id = ?`, targetID).Scan(&targetExists)
		if targetExists == 0 {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "target tag not found"})
			return
		}
		merged := 0
		reboundArticles := 0
		ph := strings.TrimSuffix(strings.Repeat("?,", len(sourceIDs)), ",")
		srcArgs := make([]any, len(sourceIDs))
		for i, v := range sourceIDs {
			srcArgs[i] = v
		}
		err := s.deps.Store.WithTx(r.Context(), func(tx *sqlx.Tx) error {
			insSQL := `INSERT INTO article_tags (article_id, tag_id)
				SELECT DISTINCT at.article_id, t.id FROM article_tags at
				INNER JOIN tags t ON t.id = ?
				WHERE at.tag_id IN (` + ph + `)
				ON CONFLICT (article_id, tag_id) DO NOTHING`
			if s.deps.Store.Dialect().Name() == "mysql" {
				insSQL = `INSERT IGNORE INTO article_tags (article_id, tag_id)
					SELECT DISTINCT at.article_id, t.id FROM article_tags at
					INNER JOIN tags t ON t.id = ?
					WHERE at.tag_id IN (` + ph + `)`
			}
			insArgs := append([]any{targetID}, srcArgs...)
			insRes, iErr := tx.ExecContext(r.Context(), insSQL, insArgs...)
			if iErr != nil {
				return iErr
			}
			if n, _ := insRes.RowsAffected(); n > 0 {
				reboundArticles = int(n)
			}
			if _, dErr := tx.ExecContext(r.Context(), `DELETE FROM article_tags WHERE tag_id IN (`+ph+`)`, srcArgs...); dErr != nil {
				return dErr
			}
			delRes, dErr := tx.ExecContext(r.Context(), `DELETE FROM tags WHERE id IN (`+ph+`)`, srcArgs...)
			if dErr != nil {
				return dErr
			}
			if n, _ := delRes.RowsAffected(); n > 0 {
				merged = int(n)
			}
			return nil
		})
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		s.recordAuditLog(r, actor, "tags.merge", "tag", targetID, map[string]any{
			"source_ids":       sourceIDs,
			"merged":           merged,
			"rebound_articles": reboundArticles,
		})
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":               true,
			"merged":           merged,
			"rebound_articles": reboundArticles,
			"target_id":        targetID,
		})
		return
	}
	if rest == "" {
		switch r.Method {
		case http.MethodGet:
			page, perPage := parseAdminPageParams(r, 100, 500)
			q := strings.TrimSpace(r.URL.Query().Get("q"))
			sortBy := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("sort")))
			unusedOnly := r.URL.Query().Get("unused") == "1" || strings.EqualFold(r.URL.Query().Get("unused"), "true")

			where := []string{"1=1"}
			var args []any
			if q != "" {
				pat := "%" + escapeLikePattern(q) + "%"
				where = append(where, "(LOWER(t.name) LIKE LOWER(?) ESCAPE '!' OR LOWER(t.slug) LIKE LOWER(?) ESCAPE '!')")
				args = append(args, pat, pat)
			}
			if unusedOnly {
				where = append(where, "NOT EXISTS (SELECT 1 FROM article_tags at2 WHERE at2.tag_id = t.id)")
			}
			whereSQL := strings.Join(where, " AND ")

			var total int
			_ = s.deps.Store.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM tags t WHERE `+whereSQL, args...).Scan(&total)

			orderSQL := "COUNT(at.article_id) DESC, LOWER(t.name) ASC"
			if sortBy == "name" {
				orderSQL = "LOWER(t.name) ASC"
			}
			baseSQL := `SELECT t.id, t.name, t.slug, t.created_at, COUNT(at.article_id) AS article_count
				FROM tags t
				LEFT JOIN article_tags at ON at.tag_id = t.id
				WHERE ` + whereSQL + `
				GROUP BY t.id, t.name, t.slug, t.created_at
				ORDER BY ` + orderSQL
			qSQL, pageArgs := s.deps.Store.Dialect().PaginateSQL(baseSQL, perPage, (page-1)*perPage)
			fullArgs := append(append([]any{}, args...), pageArgs...)

			type adminTagRow struct {
				ID           string `db:"id" json:"id"`
				Name         string `db:"name" json:"name"`
				Slug         string `db:"slug" json:"slug"`
				CreatedAt    int64  `db:"created_at" json:"created_at"`
				ArticleCount int    `db:"article_count" json:"article_count"`
			}
			paged, _ := sqlx.Select[adminTagRow](r.Context(), s.deps.Store, qSQL, fullArgs...)
			if paged == nil {
				paged = []adminTagRow{}
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"items":     paged,
				"tags":      paged,
				"total":     total,
				"page":      page,
				"per_page":  perPage,
				"page_size": perPage,
			})
		case http.MethodPost:
			if err := auth.RequireAdmin(actor); err != nil {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
				return
			}
			var in domain.Tag
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&in); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
				return
			}
			in.Name = strings.TrimSpace(in.Name)
			if in.Name == "" {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name required"})
				return
			}
			in.ID = id.Next()
			in.Slug = sanitizeSlug(in.Slug)
			if in.Slug == "" {
				in.Slug = sanitizeSlug(in.Name)
			}
			in.CreatedAt = time.Now().UnixMilli()
			if _, err := s.deps.Store.ExecContext(r.Context(),
				`INSERT INTO tags (id, name, slug, created_at) VALUES (?, ?, ?, ?)`,
				in.ID, in.Name, in.Slug, in.CreatedAt); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
			writeJSON(w, http.StatusCreated, map[string]any{"tag": in})
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
		return
	}
	if rest == "batch" && r.Method == http.MethodPost {
		if err := auth.RequireAdmin(actor); err != nil {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
			return
		}
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
			for _, tid := range ids {
				_, _ = tx.ExecContext(r.Context(), `DELETE FROM article_tags WHERE tag_id = ?`, tid)
				if res, dErr := tx.ExecContext(r.Context(), `DELETE FROM tags WHERE id = ?`, tid); dErr == nil {
					if n, _ := res.RowsAffected(); n > 0 {
						affected += int(n)
					}
				} else {
					return dErr
				}
			}
			return nil
		})
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		s.recordAuditLog(r, actor, "tag.batch_delete", "tag", "batch", map[string]any{"affected": affected})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "affected": affected})
		return
	}
	switch r.Method {
	case http.MethodPut:
		if err := auth.RequireAdmin(actor); err != nil {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
			return
		}
		var in domain.Tag
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&in); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
			return
		}
		name := strings.TrimSpace(in.Name)
		if name == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name required"})
			return
		}
		slug := sanitizeSlug(in.Slug)
		if slug == "" {
			slug = sanitizeSlug(name)
		}
		if _, err := s.deps.Store.ExecContext(r.Context(),
			`UPDATE tags SET name = ?, slug = ? WHERE id = ?`, name, slug, rest); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	case http.MethodDelete:
		if err := auth.RequireAdmin(actor); err != nil {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
			return
		}
		_ = s.deps.Store.WithTx(r.Context(), func(tx *sqlx.Tx) error {
			_, _ = tx.ExecContext(r.Context(), `DELETE FROM article_tags WHERE tag_id = ?`, rest)
			_, err := tx.ExecContext(r.Context(), `DELETE FROM tags WHERE id = ?`, rest)
			return err
		})
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *AppServer) handleAdminSeries(w http.ResponseWriter, r *http.Request, actor *domain.User, rest string) {
	rest = strings.TrimPrefix(rest, "/")
	if rest == "" {
		switch r.Method {
		case http.MethodGet:
			page, perPage := parseAdminPageParams(r, 50, 200)
			q := strings.TrimSpace(r.URL.Query().Get("q"))
			where := "1=1"
			var args []any
			if q != "" {
				pat := "%" + escapeLikePattern(q) + "%"
				where = "(LOWER(title) LIKE LOWER(?) ESCAPE '!' OR LOWER(slug) LIKE LOWER(?) ESCAPE '!' OR LOWER(description) LIKE LOWER(?) ESCAPE '!')"
				args = append(args, pat, pat, pat)
			}
			var total int
			_ = s.deps.Store.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM series WHERE `+where, args...).Scan(&total)
			baseSQL := `SELECT * FROM series WHERE ` + where + ` ORDER BY sort_order ASC, created_at DESC`
			qSQL, pageArgs := s.deps.Store.Dialect().PaginateSQL(baseSQL, perPage, (page-1)*perPage)
			fullArgs := append(append([]any{}, args...), pageArgs...)
			rows, _ := sqlx.Select[domain.Series](r.Context(), s.deps.Store, qSQL, fullArgs...)

			type seriesArticleSummary struct {
				ID        string `db:"id" json:"id"`
				Title     string `db:"title" json:"title"`
				Slug      string `db:"slug" json:"slug"`
				Status    string `db:"status" json:"status"`
				SortOrder int    `db:"sort_order" json:"sort_order"`
			}
			bySeriesIDs := make(map[string][]string)
			bySeriesArticles := make(map[string][]seriesArticleSummary)
			if len(rows) > 0 {
				placeholders := make([]string, len(rows))
				relArgs := make([]any, len(rows))
				for i, sr := range rows {
					placeholders[i] = "?"
					relArgs[i] = sr.ID
				}
				rels, _ := sqlx.Select[struct {
					SeriesID  string `db:"series_id"`
					ArticleID string `db:"article_id"`
					SortOrder int    `db:"sort_order"`
					Title     string `db:"title"`
					Slug      string `db:"slug"`
					Status    string `db:"status"`
				}](
					r.Context(), s.deps.Store,
					`SELECT sa.series_id, sa.article_id, sa.sort_order,
					        COALESCE(a.title, '') AS title, COALESCE(a.slug, '') AS slug, COALESCE(a.status, '') AS status
					 FROM series_articles sa
					 LEFT JOIN articles a ON a.id = sa.article_id
					 WHERE sa.series_id IN (`+strings.Join(placeholders, ",")+`)
					 ORDER BY sa.sort_order ASC`,
					relArgs...,
				)
				for _, rel := range rels {
					bySeriesIDs[rel.SeriesID] = append(bySeriesIDs[rel.SeriesID], rel.ArticleID)
					if rel.Title != "" || rel.Slug != "" {
						bySeriesArticles[rel.SeriesID] = append(bySeriesArticles[rel.SeriesID], seriesArticleSummary{
							ID:        rel.ArticleID,
							Title:     rel.Title,
							Slug:      rel.Slug,
							Status:    rel.Status,
							SortOrder: rel.SortOrder,
						})
					}
				}
			}
			type seriesAdminView struct {
				domain.Series
				ArticleIDs []string               `json:"article_ids"`
				Articles   []seriesArticleSummary `json:"articles"`
			}
			items := make([]seriesAdminView, 0, len(rows))
			for _, sr := range rows {
				aids := bySeriesIDs[sr.ID]
				if aids == nil {
					aids = []string{}
				}
				arts := bySeriesArticles[sr.ID]
				if arts == nil {
					arts = []seriesArticleSummary{}
				}
				items = append(items, seriesAdminView{
					Series:     sr,
					ArticleIDs: aids,
					Articles:   arts,
				})
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"items":     items,
				"series":    items,
				"total":     total,
				"page":      page,
				"per_page":  perPage,
				"page_size": perPage,
			})
		case http.MethodPost:
			if err := auth.RequireAdmin(actor); err != nil {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
				return
			}
			var in struct {
				domain.Series
				ArticleIDs []string `json:"article_ids"`
			}
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32768)).Decode(&in); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
				return
			}
			in.Title = strings.TrimSpace(in.Title)
			if in.Title == "" {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "title is required"})
				return
			}
			if rawCov := strings.TrimSpace(in.CoverURL); rawCov != "" {
				u, ok := sanitizeRedirectTargetURL(rawCov)
				if !ok {
					writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid cover_url"})
					return
				}
				in.CoverURL = u
			} else {
				in.CoverURL = ""
			}
			in.ID = id.Next()
			in.Slug = sanitizeSlug(in.Slug)
			if in.Slug == "" {
				in.Slug = sanitizeSlug(in.Title)
			}
			now := time.Now().UnixMilli()
			in.CreatedAt = now
			in.UpdatedAt = now
			err := s.deps.Store.WithTx(r.Context(), func(tx *sqlx.Tx) error {
				if _, iErr := tx.ExecContext(r.Context(),
					`INSERT INTO series (id, title, slug, description, cover_url, sort_order, created_at, updated_at)
					 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
					in.ID, in.Title, in.Slug, in.Description, in.CoverURL, in.SortOrder, now, now); iErr != nil {
					return iErr
				}
				for idx, aid := range in.ArticleIDs {
					_, _ = tx.ExecContext(r.Context(),
						`INSERT INTO series_articles (series_id, article_id, sort_order) VALUES (?, ?, ?)`,
						in.ID, aid, idx+1)
				}
				return nil
			})
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
			s.invalidateMediaRefCache()
			writeJSON(w, http.StatusCreated, map[string]any{"series": in.Series})
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
		return
	}

	switch r.Method {
	case http.MethodPut:
		if err := auth.RequireAdmin(actor); err != nil {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
			return
		}
		var in struct {
			domain.Series
			ArticleIDs []string `json:"article_ids"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32768)).Decode(&in); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
			return
		}
		cleanCov := ""
		if rawCov := strings.TrimSpace(in.CoverURL); rawCov != "" {
			u, ok := sanitizeRedirectTargetURL(rawCov)
			if !ok {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid cover_url"})
				return
			}
			cleanCov = u
		}
		title := strings.TrimSpace(in.Title)
		slug := sanitizeSlug(in.Slug)
		if slug == "" && title != "" {
			slug = sanitizeSlug(title)
		}
		err := s.deps.Store.WithTx(r.Context(), func(tx *sqlx.Tx) error {
			if _, uErr := tx.ExecContext(r.Context(),
				`UPDATE series SET title = ?, slug = ?, description = ?, cover_url = ?, sort_order = ?, updated_at = ? WHERE id = ?`,
				title, slug, in.Description, cleanCov, in.SortOrder, time.Now().UnixMilli(), rest); uErr != nil {
				return uErr
			}
			if in.ArticleIDs != nil {
				_, _ = tx.ExecContext(r.Context(), `DELETE FROM series_articles WHERE series_id = ?`, rest)
				for idx, aid := range in.ArticleIDs {
					_, _ = tx.ExecContext(r.Context(),
						`INSERT INTO series_articles (series_id, article_id, sort_order) VALUES (?, ?, ?)`,
						rest, aid, idx+1)
				}
			}
			return nil
		})
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		s.invalidateMediaRefCache()
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	case http.MethodDelete:
		if err := auth.RequireAdmin(actor); err != nil {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
			return
		}
		_ = s.deps.Store.WithTx(r.Context(), func(tx *sqlx.Tx) error {
			_, _ = tx.ExecContext(r.Context(), `DELETE FROM series_articles WHERE series_id = ?`, rest)
			_, err := tx.ExecContext(r.Context(), `DELETE FROM series WHERE id = ?`, rest)
			return err
		})
		s.invalidateMediaRefCache()
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func sanitizeBookURLs(book *domain.Book, chapters []domain.BookChapter) error {
	if rawCov := strings.TrimSpace(book.CoverURL); rawCov != "" {
		u, ok := sanitizeRedirectTargetURL(rawCov)
		if !ok {
			return errors.New("invalid cover_url")
		}
		book.CoverURL = u
	} else {
		book.CoverURL = ""
	}
	if rawSrc := strings.TrimSpace(book.SourceURL); rawSrc != "" {
		u, ok := sanitizeRedirectTargetURL(rawSrc)
		if !ok {
			return errors.New("invalid source_url")
		}
		book.SourceURL = u
	} else {
		book.SourceURL = ""
	}
	for i := range chapters {
		if rawURL := strings.TrimSpace(chapters[i].URL); rawURL != "" {
			u, ok := sanitizeRedirectTargetURL(rawURL)
			if !ok {
				return fmt.Errorf("invalid chapter url at index %d", i)
			}
			chapters[i].URL = u
		} else {
			chapters[i].URL = ""
		}
	}
	return nil
}

func (s *AppServer) handleAdminBooks(w http.ResponseWriter, r *http.Request, actor *domain.User, rest string) {
	rest = strings.TrimPrefix(rest, "/")
	if rest == "" {
		switch r.Method {
		case http.MethodGet:
			page, perPage := parseAdminPageParams(r, 50, 200)
			q := strings.TrimSpace(r.URL.Query().Get("q"))
			sourceType := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("source_type")))
			embedMode := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("embed_mode")))
			statusFilter := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("status")))
			readingStatus := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("reading_status")))

			where := []string{"1=1"}
			var args []any
			if q != "" {
				pat := "%" + escapeLikePattern(q) + "%"
				where = append(where, "(LOWER(title) LIKE LOWER(?) ESCAPE '!' OR LOWER(slug) LIKE LOWER(?) ESCAPE '!' OR LOWER(description) LIKE LOWER(?) ESCAPE '!')")
				args = append(args, pat, pat, pat)
			}
			if sourceType != "" && sourceType != "all" {
				where = append(where, "source_type = ?")
				args = append(args, sourceType)
			}
			if embedMode != "" && embedMode != "all" {
				where = append(where, "embed_mode = ?")
				args = append(args, embedMode)
			}
			if readingStatus != "" && readingStatus != "all" {
				where = append(where, "reading_status = ?")
				args = append(args, readingStatus)
			}
			switch statusFilter {
			case "synced":
				where = append(where, "synced_at > 0")
			case "unsynced":
				where = append(where, "synced_at = 0")
			}
			whereSQL := strings.Join(where, " AND ")

			var total int
			_ = s.deps.Store.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM books WHERE `+whereSQL, args...).Scan(&total)

			baseSQL := `SELECT * FROM books WHERE ` + whereSQL + ` ORDER BY sort_order ASC, created_at DESC`
			qSQL, pageArgs := s.deps.Store.Dialect().PaginateSQL(baseSQL, perPage, (page-1)*perPage)
			fullArgs := append(append([]any{}, args...), pageArgs...)
			paged, _ := sqlx.Select[domain.Book](r.Context(), s.deps.Store, qSQL, fullArgs...)
			if paged == nil {
				paged = []domain.Book{}
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"items":     paged,
				"books":     paged,
				"total":     total,
				"page":      page,
				"per_page":  perPage,
				"page_size": perPage,
			})
		case http.MethodPost:
			if err := auth.RequireAdmin(actor); err != nil {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
				return
			}
			var in struct {
				domain.Book
				Chapters []domain.BookChapter `json:"chapters"`
			}
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256*1024)).Decode(&in); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
				return
			}
			in.Title = strings.TrimSpace(in.Title)
			if in.Title == "" {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "title is required"})
				return
			}
			if err := sanitizeBookURLs(&in.Book, in.Chapters); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
			in.ID = id.Next()
			in.Slug = sanitizeSlug(in.Slug)
			if in.Slug == "" {
				in.Slug = sanitizeSlug(in.Title)
			}
			in.SourceType = strings.ToLower(strings.TrimSpace(in.SourceType))
			if in.SourceType == "" {
				in.SourceType = "auto"
			}
			in.EmbedMode = strings.ToLower(strings.TrimSpace(in.EmbedMode))
			if in.EmbedMode == "" {
				in.EmbedMode = "link"
			}
			in.ReadingStatus = strings.ToLower(strings.TrimSpace(in.ReadingStatus))
			now := time.Now().UnixMilli()
			in.CreatedAt = now
			in.UpdatedAt = now
			in.ChapterCount = len(in.Chapters)
			err := s.deps.Store.WithTx(r.Context(), func(tx *sqlx.Tx) error {
				if in.SortOrder <= 0 {
					_ = tx.QueryRowContext(r.Context(), `SELECT COALESCE(MAX(sort_order), 0) + 1 FROM books`).Scan(&in.SortOrder)
					if in.SortOrder <= 0 {
						in.SortOrder = 1
					}
				}
				if _, iErr := tx.ExecContext(r.Context(),
					`INSERT INTO books (id, title, slug, description, cover_url, source_type, source_url, series_id, embed_mode, reading_status, chapter_count, synced_at, sort_order, created_at, updated_at)
					 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?, ?)`,
					in.ID, in.Title, in.Slug, in.Description, in.CoverURL, in.SourceType, in.SourceURL, in.SeriesID,
					in.EmbedMode, in.ReadingStatus, in.ChapterCount, in.SortOrder, now, now); iErr != nil {
					return iErr
				}
				for idx, ch := range in.Chapters {
					_, _ = tx.ExecContext(r.Context(),
						`INSERT INTO book_chapters (id, book_id, parent_id, title, url, depth, sort_order) VALUES (?, ?, '', ?, ?, ?, ?)`,
						id.Next(), in.ID, strings.TrimSpace(ch.Title), ch.URL, ch.Depth, idx+1)
				}
				return nil
			})
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
			s.invalidateMediaRefCache()
			writeJSON(w, http.StatusCreated, map[string]any{"book": in.Book})
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
		return
	}

	parts := strings.Split(rest, "/")
	bookID := parts[0]
	if len(parts) == 2 && parts[1] == "sync" && r.Method == http.MethodPost {
		if err := auth.RequireAdmin(actor); err != nil {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
			return
		}
		cnt, err := s.deps.Content.SyncBookTOC(r.Context(), bookID)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"chapter_count": cnt})
		return
	}

	switch r.Method {
	case http.MethodGet:
		bk, err := sqlx.Get[domain.Book](r.Context(), s.deps.Store, `SELECT * FROM books WHERE id = ?`, bookID)
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "book not found"})
			return
		}
		chs, _ := sqlx.Select[domain.BookChapter](r.Context(), s.deps.Store,
			`SELECT * FROM book_chapters WHERE book_id = ? ORDER BY sort_order ASC`, bookID)
		writeJSON(w, http.StatusOK, map[string]any{"book": bk, "chapters": chs})

	case http.MethodPut:
		if err := auth.RequireAdmin(actor); err != nil {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
			return
		}
		var in struct {
			domain.Book
			Chapters []domain.BookChapter `json:"chapters"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256*1024)).Decode(&in); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
			return
		}
		if err := sanitizeBookURLs(&in.Book, in.Chapters); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		title := strings.TrimSpace(in.Title)
		slug := sanitizeSlug(in.Slug)
		if slug == "" && title != "" {
			slug = sanitizeSlug(title)
		}
		sourceType := strings.ToLower(strings.TrimSpace(in.SourceType))
		if sourceType == "" {
			sourceType = "auto"
		}
		embedMode := strings.ToLower(strings.TrimSpace(in.EmbedMode))
		if embedMode == "" {
			embedMode = "link"
		}
		readingStatus := strings.ToLower(strings.TrimSpace(in.ReadingStatus))
		err := s.deps.Store.WithTx(r.Context(), func(tx *sqlx.Tx) error {
			if _, uErr := tx.ExecContext(r.Context(),
				`UPDATE books SET title = ?, slug = ?, description = ?, cover_url = ?, source_type = ?, source_url = ?, series_id = ?, embed_mode = ?, reading_status = ?, sort_order = ?, updated_at = ? WHERE id = ?`,
				title, slug, in.Description, in.CoverURL, sourceType, in.SourceURL,
				in.SeriesID, embedMode, readingStatus, in.SortOrder, time.Now().UnixMilli(), bookID); uErr != nil {
				return uErr
			}
			if in.Chapters != nil {
				_, _ = tx.ExecContext(r.Context(), `DELETE FROM book_chapters WHERE book_id = ?`, bookID)
				for idx, ch := range in.Chapters {
					_, _ = tx.ExecContext(r.Context(),
						`INSERT INTO book_chapters (id, book_id, parent_id, title, url, depth, sort_order) VALUES (?, ?, '', ?, ?, ?, ?)`,
						id.Next(), bookID, strings.TrimSpace(ch.Title), ch.URL, ch.Depth, idx+1)
				}
				_, _ = tx.ExecContext(r.Context(), `UPDATE books SET chapter_count = ? WHERE id = ?`, len(in.Chapters), bookID)
			}
			return nil
		})
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		s.invalidateMediaRefCache()
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})

	case http.MethodDelete:
		if err := auth.RequireAdmin(actor); err != nil {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
			return
		}
		_ = s.deps.Store.WithTx(r.Context(), func(tx *sqlx.Tx) error {
			_, _ = tx.ExecContext(r.Context(), `DELETE FROM book_chapters WHERE book_id = ?`, bookID)
			_, err := tx.ExecContext(r.Context(), `DELETE FROM books WHERE id = ?`, bookID)
			return err
		})
		s.invalidateMediaRefCache()
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})

	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *AppServer) lookupCommentTargetAuthorID(ctx context.Context, targetType, targetID string) string {
	var ownerID string
	switch targetType {
	case domain.TargetTypeArticle:
		_ = s.deps.Store.QueryRowContext(ctx, `SELECT author_id FROM articles WHERE id = ?`, targetID).Scan(&ownerID)
	case domain.TargetTypePage:
		_ = s.deps.Store.QueryRowContext(ctx, `SELECT author_id FROM pages WHERE id = ?`, targetID).Scan(&ownerID)
	}
	return ownerID
}

func authorizeAndScrubAdminComment(actor *domain.User, c domain.Comment, targetAuthorID string) (domain.Comment, error) {
	if err := auth.RequireStaff(actor); err != nil {
		return domain.Comment{}, err
	}
	if actor.Role == domain.RoleEditor && c.TargetType == domain.TargetTypePage {
		if targetAuthorID == "" || targetAuthorID != actor.ID {
			return domain.Comment{}, auth.ErrForbidden
		}
		c.NotifyEmailCiphertext = ""
		c.ClientIP = ""
		return c, nil
	}
	scrubbed, err := auth.AuthorizeAndScrubCommentForStaff(actor, c, targetAuthorID)
	if err != nil {
		return domain.Comment{}, err
	}
	scrubbed.NotifyEmailCiphertext = ""
	return scrubbed, nil
}

func (s *AppServer) dispatchCommentModeratedHook(ctx context.Context, cmt domain.Comment, status string) {
	if s.deps.Plugins == nil {
		return
	}
	_, _ = s.deps.Plugins.DispatchHook(ctx, plugin.HookCommentModerated, plugin.CommentModeratedHookPayload{
		ID:         cmt.ID,
		TargetType: cmt.TargetType,
		TargetID:   cmt.TargetID,
		Status:     status,
	})
}

func (s *AppServer) handleAdminComments(w http.ResponseWriter, r *http.Request, actor *domain.User, rest string) {
	rest = strings.TrimPrefix(rest, "/")
	if rest == "" {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		page, perPage := parseAdminPageParams(r, 50, 250)
		status := strings.TrimSpace(r.URL.Query().Get("status"))
		q := strings.TrimSpace(r.URL.Query().Get("q"))

		joinSQL := `FROM comments c
			LEFT JOIN users u ON c.user_id = u.id
			LEFT JOIN articles a ON c.target_type = 'article' AND c.target_id = a.id
			LEFT JOIN pages p ON c.target_type = 'page' AND c.target_id = p.id`

		scopeWhere := "c.is_deleted = ?"
		scopeArgs := []any{false}
		if actor.Role != domain.RoleAdmin {
			scopeWhere = "c.is_deleted = ? AND (a.author_id = ? OR p.author_id = ?)"
			scopeArgs = append(scopeArgs, actor.ID, actor.ID)
		}

		var cntAll, cntPend, cntApp, cntRej int
		_ = s.deps.Store.QueryRowContext(r.Context(),
			`SELECT COUNT(*),
			        COALESCE(SUM(CASE WHEN c.status = 'pending' THEN 1 ELSE 0 END), 0),
			        COALESCE(SUM(CASE WHEN c.status = 'approved' THEN 1 ELSE 0 END), 0),
			        COALESCE(SUM(CASE WHEN c.status = 'rejected' THEN 1 ELSE 0 END), 0)
			 `+joinSQL+` WHERE `+scopeWhere, scopeArgs...).Scan(&cntAll, &cntPend, &cntApp, &cntRej)
		statusCounts := map[string]int{
			"all":      cntAll,
			"pending":  cntPend,
			"approved": cntApp,
			"rejected": cntRej,
		}

		where := []string{scopeWhere}
		args := append([]any{}, scopeArgs...)
		if status != "" && status != "all" {
			where = append(where, "c.status = ?")
			args = append(args, status)
		}
		if q != "" {
			pat := "%" + escapeLikePattern(q) + "%"
			if actor.Role == domain.RoleAdmin {
				where = append(where, `(
					LOWER(c.content_md) LIKE LOWER(?) ESCAPE '!' OR
					LOWER(c.author_name) LIKE LOWER(?) ESCAPE '!' OR
					LOWER(COALESCE(u.email, '')) LIKE LOWER(?) ESCAPE '!' OR
					LOWER(c.ip_masked) LIKE LOWER(?) ESCAPE '!' OR
					LOWER(c.client_ip) LIKE LOWER(?) ESCAPE '!' OR
					LOWER(COALESCE(a.title, '')) LIKE LOWER(?) ESCAPE '!' OR
					LOWER(COALESCE(a.slug, '')) LIKE LOWER(?) ESCAPE '!' OR
					LOWER(COALESCE(p.title, '')) LIKE LOWER(?) ESCAPE '!' OR
					LOWER(COALESCE(p.slug, '')) LIKE LOWER(?) ESCAPE '!'
				)`)
				args = append(args, pat, pat, pat, pat, pat, pat, pat, pat, pat)
			} else {
				where = append(where, `(
					LOWER(c.content_md) LIKE LOWER(?) ESCAPE '!' OR
					LOWER(c.author_name) LIKE LOWER(?) ESCAPE '!' OR
					LOWER(c.ip_masked) LIKE LOWER(?) ESCAPE '!' OR
					LOWER(COALESCE(a.title, '')) LIKE LOWER(?) ESCAPE '!' OR
					LOWER(COALESCE(a.slug, '')) LIKE LOWER(?) ESCAPE '!' OR
					LOWER(COALESCE(p.title, '')) LIKE LOWER(?) ESCAPE '!' OR
					LOWER(COALESCE(p.slug, '')) LIKE LOWER(?) ESCAPE '!'
				)`)
				args = append(args, pat, pat, pat, pat, pat, pat, pat)
			}
		}

		whereSQL := strings.Join(where, " AND ")
		var total int
		_ = s.deps.Store.QueryRowContext(r.Context(), `SELECT COUNT(*) `+joinSQL+` WHERE `+whereSQL, args...).Scan(&total)

		baseSQL := `SELECT c.* ` + joinSQL + ` WHERE ` + whereSQL + ` ORDER BY c.created_at DESC`
		qSQL, pageArgs := s.deps.Store.Dialect().PaginateSQL(baseSQL, perPage, (page-1)*perPage)
		fullArgs := append(append([]any{}, args...), pageArgs...)
		rows, err := sqlx.Select[domain.Comment](r.Context(), s.deps.Store, qSQL, fullArgs...)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}

		type targetMeta struct {
			AuthorID string
			Title    string
			Slug     string
		}
		artMeta := make(map[string]targetMeta)
		pageMeta := make(map[string]targetMeta)
		var artIDs, pageIDs []any
		var artPH, pagePH []string
		seenArt := make(map[string]bool)
		seenPg := make(map[string]bool)
		for _, c := range rows {
			if c.TargetType == domain.TargetTypeArticle && c.TargetID != "" && !seenArt[c.TargetID] {
				seenArt[c.TargetID] = true
				artIDs = append(artIDs, c.TargetID)
				artPH = append(artPH, "?")
			} else if c.TargetType == domain.TargetTypePage && c.TargetID != "" && !seenPg[c.TargetID] {
				seenPg[c.TargetID] = true
				pageIDs = append(pageIDs, c.TargetID)
				pagePH = append(pagePH, "?")
			}
		}
		if len(artIDs) > 0 {
			aRows, _ := sqlx.Select[struct {
				ID       string `db:"id"`
				AuthorID string `db:"author_id"`
				Title    string `db:"title"`
				Slug     string `db:"slug"`
			}](r.Context(), s.deps.Store, `SELECT id, author_id, title, slug FROM articles WHERE id IN (`+strings.Join(artPH, ",")+`)`, artIDs...)
			for _, ar := range aRows {
				artMeta[ar.ID] = targetMeta{AuthorID: ar.AuthorID, Title: ar.Title, Slug: ar.Slug}
			}
		}
		if len(pageIDs) > 0 {
			pRows, _ := sqlx.Select[struct {
				ID       string `db:"id"`
				AuthorID string `db:"author_id"`
				Title    string `db:"title"`
				Slug     string `db:"slug"`
			}](r.Context(), s.deps.Store, `SELECT id, author_id, title, slug FROM pages WHERE id IN (`+strings.Join(pagePH, ",")+`)`, pageIDs...)
			for _, pr := range pRows {
				pageMeta[pr.ID] = targetMeta{AuthorID: pr.AuthorID, Title: pr.Title, Slug: pr.Slug}
			}
		}

		type adminCommentView struct {
			domain.Comment
			TargetTitle string `json:"target_title"`
			TargetSlug  string `json:"target_slug"`
		}
		items := make([]adminCommentView, 0, len(rows))
		for _, c := range rows {
			var tm targetMeta
			if c.TargetType == domain.TargetTypePage {
				tm = pageMeta[c.TargetID]
			} else {
				tm = artMeta[c.TargetID]
			}
			ownerID := tm.AuthorID
			if actor.Role == domain.RoleAdmin {
				ownerID = ""
			}
			scrubbed, sErr := authorizeAndScrubAdminComment(actor, c, ownerID)
			if sErr == nil {
				items = append(items, adminCommentView{
					Comment:     scrubbed,
					TargetTitle: tm.Title,
					TargetSlug:  tm.Slug,
				})
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"items":         items,
			"comments":      items,
			"total":         total,
			"page":          page,
			"per_page":      perPage,
			"page_size":     perPage,
			"status_counts": statusCounts,
		})
		return
	}

	if rest == "batch" && r.Method == http.MethodPost {
		var req struct {
			Action       string   `json:"action"`
			IDs          []string `json:"ids"`
			Note         string   `json:"note"`
			ExpiresHours int      `json:"expires_hours"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 65536)).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
			return
		}
		action := strings.ToLower(strings.TrimSpace(req.Action))
		switch action {
		case "approve", "approved":
			action = "approve"
		case "reject", "rejected":
			action = "reject"
		case "pending":
			action = "pending"
		case "delete":
			action = "delete"
		case "ban_ip", "ban-ip":
			if err := auth.RequireAdmin(actor); err != nil {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
				return
			}
			action = "ban_ip"
		default:
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid batch action"})
			return
		}
		ids := dedupCleanIDs(req.IDs, 200)
		if len(ids) == 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "ids required"})
			return
		}
		comments := make([]domain.Comment, 0, len(ids))
		for _, cid := range ids {
			cmt, gErr := sqlx.Get[domain.Comment](r.Context(), s.deps.Store, `SELECT * FROM comments WHERE id = ?`, cid)
			if gErr != nil {
				continue
			}
			targetAuthorID := s.lookupCommentTargetAuthorID(r.Context(), cmt.TargetType, cmt.TargetID)
			if _, authErr := authorizeAndScrubAdminComment(actor, cmt, targetAuthorID); authErr != nil {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": authErr.Error()})
				return
			}
			comments = append(comments, cmt)
		}
		now := time.Now().UnixMilli()
		callerIP := security.ResolveClientIP(r, s.deps.Config.TrustedProxies)
		affected := 0
		type targetPair struct {
			tType string
			tID   string
		}
		recountSet := make(map[targetPair]struct{})
		for _, cmt := range comments {
			var uErr error
			nextModStatus := ""
			switch action {
			case "approve":
				nextModStatus = domain.CommentStatusApproved
				_, uErr = s.deps.Store.ExecContext(r.Context(),
					`UPDATE comments SET status = ?, updated_at = ? WHERE id = ?`,
					domain.CommentStatusApproved, now, cmt.ID)
			case "reject":
				nextModStatus = domain.CommentStatusRejected
				_, uErr = s.deps.Store.ExecContext(r.Context(),
					`UPDATE comments SET status = ?, updated_at = ? WHERE id = ?`,
					domain.CommentStatusRejected, now, cmt.ID)
			case "pending":
				nextModStatus = domain.CommentStatusPending
				_, uErr = s.deps.Store.ExecContext(r.Context(),
					`UPDATE comments SET status = ?, updated_at = ? WHERE id = ?`,
					domain.CommentStatusPending, now, cmt.ID)
			case "delete":
				_, uErr = s.deps.Store.ExecContext(r.Context(),
					`UPDATE comments SET is_deleted = ?, content_md = '', notify_email_ciphertext = '', updated_at = ? WHERE id = ?`,
					true, now, cmt.ID)
			case "ban_ip":
				targetIP := strings.TrimSpace(cmt.ClientIP)
				if targetIP != "" && s.deps.WAF != nil {
					note := strings.TrimSpace(req.Note)
					if note == "" {
						note = "comment batch moderation (" + cmt.AuthorName + ")"
					}
					hours := req.ExpiresHours
					if hours <= 0 {
						hours = 30 * 24
					}
					expMS := time.Now().Add(time.Duration(hours) * time.Hour).UnixMilli()
					_, _ = s.deps.WAF.AddManualBan(r.Context(), targetIP, note, expMS, callerIP)
				}
				nextModStatus = domain.CommentStatusRejected
				_, uErr = s.deps.Store.ExecContext(r.Context(),
					`UPDATE comments SET status = ?, updated_at = ? WHERE id = ?`,
					domain.CommentStatusRejected, now, cmt.ID)
			}
			if uErr == nil {
				affected++
				recountSet[targetPair{tType: cmt.TargetType, tID: cmt.TargetID}] = struct{}{}
				if nextModStatus != "" && nextModStatus != cmt.Status {
					s.dispatchCommentModeratedHook(r.Context(), cmt, nextModStatus)
				}
			}
		}
		for tp := range recountSet {
			s.recountTargetComments(r.Context(), tp.tType, tp.tID)
		}
		s.recordAuditLog(r, actor, "comment.batch", "comment", action, map[string]any{"action": action, "affected": affected})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "affected": affected})
		return
	}

	parts := strings.Split(rest, "/")
	cmtID := parts[0]
	cmt, err := sqlx.Get[domain.Comment](r.Context(), s.deps.Store, `SELECT * FROM comments WHERE id = ?`, cmtID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "comment not found"})
		return
	}
	targetAuthorID := s.lookupCommentTargetAuthorID(r.Context(), cmt.TargetType, cmt.TargetID)
	if _, authErr := authorizeAndScrubAdminComment(actor, cmt, targetAuthorID); authErr != nil {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": authErr.Error()})
		return
	}

	if len(parts) == 2 && parts[1] == "reply" && r.Method == http.MethodPost {
		var req struct {
			ContentMD string `json:"content_md"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32768)).Decode(&req); err != nil || strings.TrimSpace(req.ContentMD) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "content_md required"})
			return
		}
		clientIP := security.ResolveClientIP(r, s.deps.Config.TrustedProxies)
		in := s.commentInputFromStaff(actor, cmt, req.ContentMD, clientIP.String())
		view, _, cErr := s.deps.Content.CreateComment(r.Context(), w, r, actor, in)
		if cErr != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": cErr.Error()})
			return
		}
		parentAutoApproved := false
		if cmt.Status == domain.CommentStatusPending {
			if _, uErr := s.deps.Store.ExecContext(r.Context(),
				`UPDATE comments SET status = ?, updated_at = ? WHERE id = ?`,
				domain.CommentStatusApproved, time.Now().UnixMilli(), cmt.ID); uErr == nil {
				parentAutoApproved = true
				s.recountTargetComments(r.Context(), cmt.TargetType, cmt.TargetID)
				s.dispatchCommentModeratedHook(r.Context(), cmt, domain.CommentStatusApproved)
			}
		}
		if s.deps.NotifyCommentCreated != nil {
			s.deps.NotifyCommentCreated(r.Context(), view, in)
		}
		writeJSON(w, http.StatusCreated, map[string]any{
			"comment":              view,
			"parent_auto_approved": parentAutoApproved,
		})
		return
	}

	if len(parts) == 2 && parts[1] == "ban-ip" && r.Method == http.MethodPost {
		if err := auth.RequireAdmin(actor); err != nil {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
			return
		}
		targetIP := strings.TrimSpace(cmt.ClientIP)
		if targetIP == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "comment has no recorded client IP"})
			return
		}
		var req struct {
			Note          string `json:"note"`
			ExpiresHours  int    `json:"expires_hours"`
			RejectComment bool   `json:"reject_comment"`
			RejectPending bool   `json:"reject_pending"`
		}
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&req)
		note := strings.TrimSpace(req.Note)
		if note == "" {
			note = "comment moderation (" + cmt.AuthorName + ")"
		}
		var expMS int64
		if req.ExpiresHours > 0 {
			expMS = time.Now().Add(time.Duration(req.ExpiresHours) * time.Hour).UnixMilli()
		}
		callerIP := security.ResolveClientIP(r, s.deps.Config.TrustedProxies)
		rec, bErr := s.deps.WAF.AddManualBan(r.Context(), targetIP, note, expMS, callerIP)
		if bErr != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": bErr.Error()})
			return
		}
		if req.RejectComment || req.RejectPending {
			_, _ = s.deps.Store.ExecContext(r.Context(),
				`UPDATE comments SET status = ?, updated_at = ? WHERE id = ?`,
				domain.CommentStatusRejected, time.Now().UnixMilli(), cmtID)
			s.recountTargetComments(r.Context(), cmt.TargetType, cmt.TargetID)
			if cmt.Status != domain.CommentStatusRejected {
				s.dispatchCommentModeratedHook(r.Context(), cmt, domain.CommentStatusRejected)
			}
		}
		s.recordAuditLog(r, actor, "security.comment_ip_ban", "ip_ban", rec.ID, map[string]any{"comment_id": cmtID, "cidr": rec.CIDR})
		writeJSON(w, http.StatusCreated, map[string]any{"ok": true, "ban": rec})
		return
	}

	switch r.Method {
	case http.MethodPatch:
		var req struct {
			Status    *string `json:"status"`
			ContentMD *string `json:"content_md"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32768)).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
			return
		}
		if req.Status == nil && req.ContentMD == nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "status or content_md required"})
			return
		}
		nextStatus := cmt.Status
		if req.Status != nil && strings.TrimSpace(*req.Status) != "" {
			sVal := strings.TrimSpace(*req.Status)
			switch sVal {
			case domain.CommentStatusApproved, domain.CommentStatusPending, domain.CommentStatusRejected:
				nextStatus = sVal
			default:
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid comment status"})
				return
			}
		}
		nextContent := cmt.ContentMD
		if req.ContentMD != nil {
			trimmed := strings.TrimSpace(*req.ContentMD)
			if trimmed == "" {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "content_md required"})
				return
			}
			if len([]rune(trimmed)) > 8000 {
				trimmed = string([]rune(trimmed)[:8000])
			}
			nextContent = trimmed
		}
		_, err := s.deps.Store.ExecContext(r.Context(),
			`UPDATE comments SET status = ?, content_md = ?, updated_at = ? WHERE id = ?`,
			nextStatus, nextContent, time.Now().UnixMilli(), cmtID)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		s.recountTargetComments(r.Context(), cmt.TargetType, cmt.TargetID)
		if nextStatus != cmt.Status {
			s.dispatchCommentModeratedHook(r.Context(), cmt, nextStatus)
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})

	case http.MethodDelete:
		_, _ = s.deps.Store.ExecContext(r.Context(),
			`UPDATE comments SET is_deleted = ?, content_md = '', notify_email_ciphertext = '', updated_at = ? WHERE id = ?`,
			true, time.Now().UnixMilli(), cmtID)
		s.recountTargetComments(r.Context(), cmt.TargetType, cmt.TargetID)
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})

	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *AppServer) recountTargetComments(ctx context.Context, targetType, targetID string) {
	var cnt int
	_ = s.deps.Store.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM comments WHERE target_type = ? AND target_id = ? AND status = 'approved' AND is_deleted = ?`,
		targetType, targetID, false).Scan(&cnt)
	if targetType == domain.TargetTypePage {
		_, _ = s.deps.Store.ExecContext(ctx, `UPDATE pages SET comment_count = ? WHERE id = ?`, cnt, targetID)
	} else {
		_, _ = s.deps.Store.ExecContext(ctx, `UPDATE articles SET comment_count = ? WHERE id = ?`, cnt, targetID)
	}
}

func (s *AppServer) handleAdminMedia(w http.ResponseWriter, r *http.Request, actor *domain.User, rest string) {
	rest = strings.TrimPrefix(rest, "/")
	if rest == "" && r.Method == http.MethodGet {
		page, perPage := parseAdminPageParams(r, 50, 200)
		q := r.URL.Query().Get("q")
		mimeFilter := r.URL.Query().Get("mime")
		monthFilter := r.URL.Query().Get("month")
		orphanOnly := r.URL.Query().Get("orphan_only") == "1" ||
			strings.EqualFold(r.URL.Query().Get("orphan_only"), "true") ||
			strings.EqualFold(r.URL.Query().Get("filter"), "orphan")
		usage := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("usage")))
		if usage == "" && orphanOnly {
			usage = "unused"
		}
		withRefs := r.URL.Query().Get("refs") != "0" && !strings.EqualFold(r.URL.Query().Get("refs"), "false")
		sortBy := r.URL.Query().Get("sort")

		items, total, allTotal, orphanCnt, totalBytes := s.queryAdminMediaPage(r.Context(), actor, q, mimeFilter, monthFilter, usage, orphanOnly, withRefs, sortBy, page, perPage)
		drv := strings.TrimSpace(s.deps.Settings.Snapshot().Storage.ActiveDriver)
		if drv == "" {
			drv = "local"
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"items":          items,
			"assets":         items,
			"orphan_count":   orphanCnt,
			"total_bytes":    totalBytes,
			"storage_driver": drv,
			"total":          total,
			"all_total":      allTotal,
			"page":           page,
			"per_page":       perPage,
			"page_size":      perPage,
		})
		return
	}

	if rest == "reconcile" && r.Method == http.MethodPost {
		if err := auth.RequireAdmin(actor); err != nil {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
			return
		}
		imported := s.reconcileLocalUploads(r.Context())
		if imported > 0 {
			_ = s.deps.Content.RefreshMediaDimsCache(r.Context())
			s.invalidateMediaRefCache()
		}
		s.recordAuditLog(r, actor, "media.reconcile", "media_asset", "local", map[string]any{"imported": imported})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "imported": imported})
		return
	}

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
		assets := make([]domain.MediaAsset, 0, len(ids))
		for _, mid := range ids {
			asset, gErr := sqlx.Get[domain.MediaAsset](r.Context(), s.deps.Store, `SELECT * FROM media_assets WHERE id = ?`, mid)
			if gErr != nil {
				continue
			}
			if actor.Role != domain.RoleAdmin && asset.UploaderID != actor.ID {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
				return
			}
			assets = append(assets, asset)
		}
		affected := 0
		for _, asset := range assets {
			if s.deps.Media != nil {
				if dErr := s.deps.Media.Delete(r.Context(), asset.ID); dErr == nil {
					affected++
				}
			} else {
				if asset.StorageDriver == "local" && asset.ObjectKey != "" {
					safeUnlinkLocalAsset(s.deps.Config.UploadDir, asset.ObjectKey)
				}
				if _, dErr := s.deps.Store.ExecContext(r.Context(), `DELETE FROM media_assets WHERE id = ?`, asset.ID); dErr == nil {
					affected++
				}
			}
		}
		if affected > 0 {
			_ = s.deps.Content.RefreshMediaDimsCache(r.Context())
			s.invalidateMediaRefCache()
		}
		s.recordAuditLog(r, actor, "media.batch_delete", "media_asset", "batch", map[string]any{"affected": affected})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "affected": affected})
		return
	}

	if rest == "upload" && r.Method == http.MethodPost {
		maxMB := s.deps.Settings.Snapshot().Storage.MaxUploadMB
		if maxMB <= 0 {
			maxMB = 25
		}
		maxBytes := int64(maxMB) << 20
		r.Body = http.MaxBytesReader(w, r.Body, maxBytes+1024*1024)
		if err := r.ParseMultipartForm(8 << 20); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "upload exceeds maximum size"})
			return
		}
		file, hdr, err := r.FormFile("file")
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing 'file' field"})
			return
		}
		defer file.Close()

		var asset *domain.MediaAsset
		if s.deps.Media != nil {
			asset, err = s.deps.Media.Upload(r.Context(), actor.ID, hdr.Filename, hdr.Header.Get("Content-Type"), file)
		} else {
			asset, err = s.saveFallbackLocalMedia(r.Context(), actor.ID, hdr.Filename, file, maxBytes)
		}
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if asset != nil && asset.Width > 0 && asset.Height > 0 {
			s.deps.Content.PutMediaDims(asset.PublicURL, asset.Width, asset.Height)
		}
		s.invalidateMediaRefCache()
		writeJSON(w, http.StatusCreated, map[string]any{"asset": asset, "url": asset.PublicURL})
		return
	}

	if rest == "orphans" && r.Method == http.MethodPost {
		if err := auth.RequireAdmin(actor); err != nil {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
			return
		}
		clean := r.URL.Query().Get("delete") == "1" || r.URL.Query().Get("delete") == "true"
		orphans, freed, err := s.scanOrCleanOrphans(r.Context(), clean)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		deletedCnt := 0
		if clean {
			deletedCnt = len(orphans)
			s.invalidateMediaRefCache()
			s.recordAuditLog(r, actor, "media.orphans_clean", "media_asset", "orphans", map[string]any{"deleted_count": deletedCnt, "freed_bytes": freed})
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"orphans":       orphans,
			"deleted":       clean,
			"deleted_count": deletedCnt,
			"freed_bytes":   freed,
		})
		return
	}

	if rest != "" && r.Method == http.MethodPatch {
		asset, err := sqlx.Get[domain.MediaAsset](r.Context(), s.deps.Store, `SELECT * FROM media_assets WHERE id = ?`, rest)
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "asset not found"})
			return
		}
		if actor.Role != domain.RoleAdmin && asset.UploaderID != actor.ID {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
			return
		}
		var req struct {
			OriginalName *string `json:"original_name"`
			AltText      *string `json:"alt_text"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384)).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
			return
		}
		if req.OriginalName != nil {
			cleanName := strings.TrimSpace(filepath.Base(strings.ReplaceAll(*req.OriginalName, "\\", "/")))
			if cleanName != "" && cleanName != "." && cleanName != ".." {
				if len([]rune(cleanName)) > 180 {
					cleanName = string([]rune(cleanName)[:180])
				}
				asset.OriginalName = cleanName
			}
		}
		if req.AltText != nil {
			alt := strings.TrimSpace(*req.AltText)
			if len([]rune(alt)) > 500 {
				alt = string([]rune(alt)[:500])
			}
			asset.AltText = alt
		}
		if _, uErr := s.deps.Store.ExecContext(r.Context(),
			`UPDATE media_assets SET original_name = ?, alt_text = ? WHERE id = ?`,
			asset.OriginalName, asset.AltText, asset.ID); uErr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": uErr.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "asset": asset})
		return
	}

	if rest != "" && r.Method == http.MethodDelete {
		asset, err := sqlx.Get[domain.MediaAsset](r.Context(), s.deps.Store, `SELECT * FROM media_assets WHERE id = ?`, rest)
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "asset not found"})
			return
		}
		if actor.Role != domain.RoleAdmin && asset.UploaderID != actor.ID {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
			return
		}
		if s.deps.Media != nil {
			if dErr := s.deps.Media.Delete(r.Context(), rest); dErr != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": dErr.Error()})
				return
			}
		} else {
			if asset.StorageDriver == "local" && asset.ObjectKey != "" {
				safeUnlinkLocalAsset(s.deps.Config.UploadDir, asset.ObjectKey)
			}
			_, _ = s.deps.Store.ExecContext(r.Context(), `DELETE FROM media_assets WHERE id = ?`, rest)
		}
		_ = s.deps.Content.RefreshMediaDimsCache(r.Context())
		s.invalidateMediaRefCache()
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}
	w.WriteHeader(http.StatusMethodNotAllowed)
}

func (s *AppServer) saveFallbackLocalMedia(ctx context.Context, uploaderID string, origName string, r io.Reader, maxBytes int64) (*domain.MediaAsset, error) {
	raw, err := readBoundedStream(r, maxBytes)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, errors.New("empty upload")
	}
	ext := strings.ToLower(filepath.Ext(origName))
	switch ext {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".avif", ".mp3", ".m4a", ".ogg", ".wav", ".mp4", ".webm", ".pdf", ".zip":
	case ".svg":
		return nil, errors.New("raw SVG upload disallowed for XSS safety; use PNG/WebP/JPEG")
	default:
		return nil, fmt.Errorf("unsupported file extension %q", ext)
	}

	mimeType := defaultReconcileMIMEType(ext, raw[:minInt(512, len(raw))])
	exifStripped := false
	var width, height int

	if ext == ".jpg" || ext == ".jpeg" || ext == ".png" || ext == ".webp" || ext == ".gif" {
		cleanBytes, normMIME, w, h, changed, nErr := media.NormalizeLocalImageBytes(raw, ext)
		if nErr != nil {
			return nil, nErr
		}
		if changed && len(cleanBytes) > 0 {
			raw = cleanBytes
		}
		mimeType = strings.ToLower(strings.TrimSpace(normMIME))
		width, height = w, h
		exifStripped = ext != ".gif"
	} else if strings.HasPrefix(mimeType, "image/") {
		if cfg, _, iErr := image.DecodeConfig(strings.NewReader(string(raw))); iErr == nil {
			width, height = cfg.Width, cfg.Height
		}
	}

	now := time.Now()
	relKey := fmt.Sprintf("%s/%s%s", now.UTC().Format("2006/01"), strings.ToLower(id.Next()), ext)
	dest := filepath.Join(s.deps.Config.UploadDir, filepath.FromSlash(relKey))
	if err := os.MkdirAll(filepath.Dir(dest), 0o750); err != nil {
		return nil, err
	}
	if err := os.WriteFile(dest, raw, 0o640); err != nil {
		return nil, err
	}
	sum := sha256.Sum256(raw)
	asset := domain.MediaAsset{
		ID:            id.Next(),
		StorageDriver: "local",
		ObjectKey:     relKey,
		PublicURL:     "/uploads/" + relKey,
		OriginalName:  filepath.Base(origName),
		AltText:       "",
		MIMEType:      mimeType,
		SizeBytes:     int64(len(raw)),
		Width:         width,
		Height:        height,
		SHA256:        hex.EncodeToString(sum[:]),
		EXIFStripped:  exifStripped,
		UploaderID:    uploaderID,
		CreatedAt:     now.UnixMilli(),
	}
	if _, err := s.deps.Store.ExecContext(ctx,
		`INSERT INTO media_assets (id, storage_driver, object_key, public_url, original_name, alt_text, mime_type, size_bytes, width, height, sha256, exif_stripped, uploader_id, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		asset.ID, asset.StorageDriver, asset.ObjectKey, asset.PublicURL, asset.OriginalName, asset.AltText, asset.MIMEType,
		asset.SizeBytes, asset.Width, asset.Height, asset.SHA256, asset.EXIFStripped, asset.UploaderID, asset.CreatedAt); err != nil {
		return nil, err
	}
	return &asset, nil
}

func dedupCleanIDs(raw []string, maxCount int) []string {
	if len(raw) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, minInt(len(raw), maxCount))
	out := make([]string, 0, minInt(len(raw), maxCount))
	for _, s := range raw {
		v := strings.TrimSpace(s)
		if v == "" {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
		if len(out) >= maxCount {
			break
		}
	}
	return out
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func parseVersionHeader(r *http.Request, bodyVer int64) int64 {
	if bodyVer > 0 {
		return bodyVer
	}
	if im := strings.Trim(strings.TrimSpace(r.Header.Get("If-Match")), `"W/`); im != "" {
		if v, err := strconv.ParseInt(im, 10, 64); err == nil && v > 0 {
			return v
		}
	}
	return 0
}
