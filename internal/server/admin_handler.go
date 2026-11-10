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
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Willh92/Volo-go/internal/adminui"
	"github.com/Willh92/Volo-go/internal/auth"
	"github.com/Willh92/Volo-go/internal/content"
	"github.com/Willh92/Volo-go/internal/db/sqlx"
	"github.com/Willh92/Volo-go/internal/domain"
	"github.com/Willh92/Volo-go/internal/media"
	"github.com/Willh92/Volo-go/internal/render"
	"github.com/Willh92/Volo-go/internal/repo"
	"github.com/Willh92/Volo-go/internal/security"
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

func (s *AppServer) staticAdminPrefix() string {
	adminPrefix := "/console"
	if s.deps.Config != nil {
		if p := strings.Trim(strings.TrimSpace(s.deps.Config.AdminPath), "/"); p != "" {
			adminPrefix = "/" + p
		}
	}
	return adminPrefix
}

func (s *AppServer) effectiveAdminPrefix() (prefix string, isCustom bool) {
	base := s.staticAdminPrefix()
	custom := s.deps.Config != nil && s.deps.Config.IsCustomAdminPath
	if s.deps.Settings != nil {
		if dyn := strings.Trim(strings.TrimSpace(s.deps.Settings.Snapshot().Security.DynamicLoginPath), "/"); dyn != "" {
			dynPrefix := "/" + dyn
			return dynPrefix, dynPrefix != "/console" || custom
		}
	}
	return base, custom
}

func (s *AppServer) registerAdminRoutes(mux *http.ServeMux) {
	adminPrefix := s.staticAdminPrefix()
	mux.HandleFunc(adminPrefix, s.handleAdminSPA)
	mux.HandleFunc(adminPrefix+"/", s.handleAdminSPA)
	mux.HandleFunc("/api/admin/", s.handleAdminAPI)
}

func (s *AppServer) handleAdminSPA(w http.ResponseWriter, r *http.Request) {
	activePrefix, _ := s.effectiveAdminPrefix()
	s.serveAdminSPAAt(w, r, activePrefix)
}

func (s *AppServer) serveAdminSPAAt(w http.ResponseWriter, r *http.Request, mountPrefix string) {
	snap := s.deps.Settings.Snapshot()
	customVendor := strings.TrimRight(strings.TrimSpace(snap.Reading.VendorCDNPrefix), "/")
	switch customVendor {
	case "https://cdn.jsdelivr.net/npm", "https://fastly.jsdelivr.net/npm", "https://unpkg.com":
		customVendor = ""
	}
	adminui.Default().ServeMount(w, r, mountPrefix, adminui.BootConfig{
		Mode:            "console",
		AdminPath:       mountPrefix,
		BaseAdminPath:   s.staticAdminPrefix(),
		PublicURL:       s.publicBaseURL(r),
		DefaultLocale:   snap.Site.DefaultLocale,
		Accent:          snap.Appearance.Accent,
		DefaultTheme:    snap.Appearance.DefaultMode,
		SiteTitle:       snap.Site.Title,
		VendorCDNPrefix: customVendor,
		Locales:         s.deps.Catalog.Locales(),
	})
}

func (s *AppServer) withDynamicAdminMount(next http.Handler) http.Handler {
	staticPrefix := s.staticAdminPrefix()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		activePrefix, _ := s.effectiveAdminPrefix()
		if p == activePrefix || strings.HasPrefix(p, activePrefix+"/") {
			s.serveAdminSPAAt(w, r, activePrefix)
			return
		}
		if activePrefix != staticPrefix && (p == staticPrefix || strings.HasPrefix(p, staticPrefix+"/")) {
			r, _ = withErrorProbeTracker(r)
			viewer, _ := s.resolveViewer(r)
			s.record404ProbeOnce(r, viewer, security.ReasonScannerTrap, 2)
			s.renderErrorPage(w, r, http.StatusNotFound, "error.not_found")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// authenticateAdminRequest resolves either:
//  1. A scoped `Authorization: Bearer volo_pat_...` Personal Access Token (stateless, no CSRF needed), OR
//  2. An active `__volo_sid` Staff Session Cookie (Admin or Editor; Reader physically rejected with 403!),
//     enforcing `X-CSRF-Token` on all mutating HTTP verbs (`POST`, `PUT`, `PATCH`, `DELETE`).
func (s *AppServer) authenticateAdminRequest(w http.ResponseWriter, r *http.Request, requiredPATScope string, requireAdminOnly bool) (*domain.User, *domain.UserSession, bool) {
	authHdr := strings.TrimSpace(r.Header.Get("Authorization"))
	if strings.HasPrefix(authHdr, "Bearer ") {
		if requiredPATScope == "" {
			writeErrJSON(w, http.StatusForbidden, "this endpoint requires an interactive admin session")
			return nil, nil, false
		}
		actor, _, err := s.deps.Auth.AuthenticateAPIToken(r.Context(), authHdr, requiredPATScope)
		if errors.Is(err, auth.ErrForbidden) {
			writeErrJSON(w, http.StatusForbidden, err.Error())
			return nil, nil, false
		}
		if err != nil {
			writeErrJSON(w, http.StatusUnauthorized, "invalid or expired API token")
			return nil, nil, false
		}
		if requireAdminOnly {
			if aErr := auth.RequireAdmin(actor); aErr != nil {
				writeErrJSON(w, http.StatusForbidden, aErr.Error())
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
		writeErrJSON(w, http.StatusUnauthorized, s.deps.Catalog.T(s.resolveLocale(r), "error.unauthorized"))
		return nil, nil, false
	}
	actor, sess, err := s.deps.Auth.AuthenticateSession(r.Context(), c.Value)
	if err != nil || actor == nil || sess == nil {
		writeErrJSON(w, http.StatusUnauthorized, s.deps.Catalog.T(s.resolveLocale(r), "error.unauthorized"))
		return nil, nil, false
	}

	// Physical RBAC boundary: `reader` accounts NEVER pass RequireStaff!
	if err := auth.RequireStaff(actor); err != nil {
		writeErrJSON(w, http.StatusForbidden, s.deps.Catalog.T(s.resolveLocale(r), "error.forbidden"))
		return nil, nil, false
	}
	if requireAdminOnly {
		if err := auth.RequireAdmin(actor); err != nil {
			writeErrJSON(w, http.StatusForbidden, s.deps.Catalog.T(s.resolveLocale(r), "error.forbidden"))
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
			writeErrJSON(w, http.StatusForbidden, "invalid or missing X-CSRF-Token")
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
		if !requireMethod(w, r, http.MethodPost) {
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
		writeOKJSON(w)

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

	case sub == "render/preview" && r.Method == http.MethodPost:
		actor, _, ok := s.authenticateAdminRequest(w, r, auth.ScopeArticlesRead, false)
		if !ok {
			return
		}
		s.handleAdminRenderPreview(w, r, actor)

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
	if !s.requireSameOriginPost(w, r) {
		return
	}
	clientIP := s.clientIP(r)
	if !s.deps.WAF.AllowRate("admin:login", clientIP, 12, 5*time.Minute) {
		writeErrJSON(w, http.StatusTooManyRequests, s.deps.Catalog.T(s.resolveLocale(r), "error.rate_limited"))
		return
	}

	var req struct {
		Ident    string `json:"ident"`
		Password string `json:"password"`
	}
	if !decodeJSONBody(w, r, 8192, &req) {
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
		writeErrJSON(w, http.StatusUnauthorized, s.localizeAdminAuthError(r, auth.ErrInvalidCredentials))
		return
	}
	if err != nil {
		s.deps.WAF.RecordStrike(r.Context(), clientIP, security.ReasonLoginFail, 1)
		writeErrJSON(w, http.StatusUnauthorized, s.localizeAdminAuthError(r, err))
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
	if !s.requireSameOriginPost(w, r) {
		return
	}
	clientIP := s.clientIP(r)
	if !s.deps.WAF.AllowRate("admin:2fa", clientIP, 10, 5*time.Minute) {
		writeErrJSON(w, http.StatusTooManyRequests, s.deps.Catalog.T(s.resolveLocale(r), "error.rate_limited"))
		return
	}
	var req struct {
		ChallengeToken string `json:"challenge_token"`
		Code           string `json:"code"`
		RememberDevice bool   `json:"remember_device"`
	}
	if !decodeJSONBody(w, r, 8192, &req) {
		return
	}
	geo := ""
	if s.deps.Geo != nil {
		geo = s.deps.Geo.LookupDisplay(clientIP)
	}
	res, trustCookie, err := s.deps.Auth.CompleteStaff2FA(r.Context(), req.ChallengeToken, req.Code, req.RememberDevice, clientIP, geo, r.UserAgent())
	if err != nil {
		s.deps.WAF.RecordStrike(r.Context(), clientIP, security.Reason2FAFail, 1)
		writeErrJSON(w, http.StatusUnauthorized, s.localizeAdminAuthError(r, err))
		return
	}
	s.setSessionCookieForRequest(w, r, res.RawToken)
	remDays := s.deps.Settings.Snapshot().Security.TOTPRememberDays
	if trustCookie != "" && remDays > 0 {
		http.SetCookie(w, &http.Cookie{
			Name:     trusted2FACookieName,
			Value:    trustCookie,
			Path:     "/api",
			MaxAge:   remDays * 86400,
			HttpOnly: true,
			Secure:   s.isCookieSecureForRequest(r),
			SameSite: http.SameSiteLaxMode,
		})
	} else {
		http.SetCookie(w, &http.Cookie{
			Name:     trusted2FACookieName,
			Value:    "",
			Path:     "/api",
			MaxAge:   -1,
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
	if !s.requireSameOriginPost(w, r) {
		return
	}
	clientIP := s.clientIP(r)
	if !s.deps.WAF.AllowRate("admin:passkey:begin", clientIP, 15, 5*time.Minute) {
		writeErrJSON(w, http.StatusTooManyRequests, s.deps.Catalog.T(s.resolveLocale(r), "error.rate_limited"))
		return
	}
	res, err := auth.BeginPasskeyCeremony(s.deps.Config.KeyRing, "", "login", s.publicBaseURL(r))
	if err != nil {
		writeErrJSON(w, http.StatusBadRequest, s.localizeAdminAuthError(r, err))
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *AppServer) handleAdminPasskeyLoginFinish(w http.ResponseWriter, r *http.Request) {
	if !s.requireSameOriginPost(w, r) {
		return
	}
	clientIP := s.clientIP(r)
	if !s.deps.WAF.AllowRate("admin:passkey:finish", clientIP, 12, 5*time.Minute) {
		writeErrJSON(w, http.StatusTooManyRequests, s.deps.Catalog.T(s.resolveLocale(r), "error.rate_limited"))
		return
	}
	var req struct {
		StateToken        string `json:"state_token"`
		CredentialID      string `json:"credential_id"`
		ClientDataJSON    string `json:"client_data_json"`
		AuthenticatorData string `json:"authenticator_data"`
		Signature         string `json:"signature"`
	}
	if !decodeJSONBody(w, r, 32768, &req) {
		return
	}
	cdBytes, err1 := base64.RawURLEncoding.DecodeString(req.ClientDataJSON)
	adBytes, err2 := base64.RawURLEncoding.DecodeString(req.AuthenticatorData)
	sigBytes, err3 := base64.RawURLEncoding.DecodeString(req.Signature)
	if err1 != nil || err2 != nil || err3 != nil {
		writeErrJSON(w, http.StatusBadRequest, s.deps.Catalog.T(s.resolveLocale(r), "error.bad_request"))
		return
	}
	geo := ""
	if s.deps.Geo != nil {
		geo = s.deps.Geo.LookupDisplay(clientIP)
	}
	res, err := s.deps.Auth.LoginWithPasskey(r.Context(), req.StateToken, req.CredentialID, cdBytes, adBytes, sigBytes, clientIP, geo, r.UserAgent())
	if err != nil {
		s.deps.WAF.RecordStrike(r.Context(), clientIP, security.ReasonLoginFail, 1)
		writeErrJSON(w, http.StatusUnauthorized, s.localizeAdminAuthError(r, err))
		return
	}
	s.setSessionCookieForRequest(w, r, res.RawToken)
	s.recordAuditLog(r, res.User, "auth.login_passkey", "user", res.User.ID, nil)
	writeJSON(w, http.StatusOK, map[string]any{
		"user":       res.User,
		"csrf_token": res.CSRFToken,
	})
}

func (s *AppServer) localizeAdminAuthError(r *http.Request, err error) string {
	if err == nil {
		return ""
	}
	locale := s.resolveLocale(r)
	cat := s.deps.Catalog
	var banErr *auth.UserBannedError
	if errors.As(err, &banErr) {
		return formatLocalizedBanNotice(locale, banErr)
	}
	switch {
	case errors.Is(err, auth.ErrInvalidCredentials):
		return cat.T(locale, "reader.err_invalid_credentials")
	case errors.Is(err, auth.ErrUnauthorized):
		return cat.T(locale, "error.unauthorized")
	case errors.Is(err, auth.ErrForbidden):
		return cat.T(locale, "error.forbidden")
	case errors.Is(err, auth.ErrTOTPInvalid):
		return cat.T(locale, "auth.err_totp_invalid")
	case errors.Is(err, auth.ErrTOTPReplay):
		return cat.T(locale, "auth.err_totp_replay")
	case errors.Is(err, auth.ErrTwoFactorExpired):
		return cat.T(locale, "auth.err_2fa_expired")
	case errors.Is(err, auth.ErrPasskeyVerifyFailed), errors.Is(err, auth.ErrPasskeyCloneDetected):
		return cat.T(locale, "auth.err_passkey_failed")
	case errors.Is(err, auth.ErrWeakPassword):
		return cat.T(locale, "reader.err_password_policy")
	case errors.Is(err, auth.ErrVerificationCode):
		return cat.T(locale, "reader.err_verify_code")
	case errors.Is(err, auth.ErrCodeCooldown):
		return cat.T(locale, "reader.err_code_cooldown")
	default:
		return err.Error()
	}
}

func (s *AppServer) handleAdminMe(w http.ResponseWriter, r *http.Request, actor *domain.User, sess *domain.UserSession) {
	if r.Method == http.MethodGet {
		csrf := ""
		sessID := ""
		if sess != nil {
			sessID = sess.ID
			if strings.TrimSpace(r.Header.Get("X-Volo-Console")) == "1" {
				csrf = s.deps.Auth.ComputeCSRFToken(sess.ID)
			}
		}
		var totpCnt, pkCnt int
		_ = s.deps.Store.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM user_credentials WHERE user_id = ? AND kind = 'totp'`, actor.ID).Scan(&totpCnt)
		_ = s.deps.Store.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM user_credentials WHERE user_id = ? AND kind = 'passkey'`, actor.ID).Scan(&pkCnt)
		snap := s.deps.Settings.Snapshot()
		appView := snap.Appearance
		if s.deps.Themes != nil && len(appView.ThemeConfigs) > 0 {
			appView.ThemeConfigs = s.deps.Themes.RedactAllThemeConfigs(appView.ThemeConfigs)
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"user":                     actor,
			"session_id":               sessID,
			"csrf_token":               csrf,
			"totp_enabled":             totpCnt > 0,
			"totp_enrollment_required": !s.isStaffTOTPCompliant(r.Context(), actor),
			"passkeys":                 pkCnt,
			"site":                     snap.Site,
			"appearance":               appView,
			"storage_driver":           snap.Storage.ActiveDriver,
			"locales":                  s.deps.Catalog.Locales(),
		})
		return
	}

	if r.Method == http.MethodPatch {
		if sess == nil {
			writeErrJSON(w, http.StatusForbidden, "profile mutation requires an interactive admin session")
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
		if !decodeJSONBody(w, r, 16384, &req) {
			return
		}
		var fullUser domain.User
		err := sqlx.GetInto(r.Context(), s.deps.Store, &fullUser, `SELECT * FROM users WHERE id = ?`, actor.ID)
		if err != nil {
			writeErrJSON(w, http.StatusNotFound, "user not found")
			return
		}
		locale := s.resolveLocale(r)
		maskedIP := security.MaskIP(s.clientIP(r))
		if req.DisplayName != nil && strings.TrimSpace(*req.DisplayName) != "" {
			nextDisp := strings.TrimSpace(repo.StripASCIIControls(*req.DisplayName))
			if runes := []rune(nextDisp); len(runes) < 2 || len(runes) > 64 {
				writeErrJSON(w, http.StatusBadRequest, s.deps.Catalog.T(locale, "reader.err_username_len"))
				return
			}
			if actor.Role == domain.RoleEditor && s.deps.Auth != nil {
				if nErr := s.deps.Auth.ValidateNickname(r.Context(), nextDisp, fullUser.Email, maskedIP, fullUser.ID, locale, "editor_profile"); nErr != nil {
					writeErrJSON(w, http.StatusBadRequest, s.localizePublicError(locale, nErr))
					return
				}
			}
			fullUser.DisplayName = nextDisp
		}
		if req.Email != nil {
			nextEmail := strings.ToLower(strings.TrimSpace(*req.Email))
			if nextEmail != "" && nextEmail != strings.ToLower(fullUser.Email) {
				if !strings.Contains(nextEmail, "@") || len(nextEmail) > 190 {
					writeErrJSON(w, http.StatusBadRequest, "invalid email address")
					return
				}
				ok, _ := auth.VerifyPassword(req.CurrentPassword, fullUser.PasswordHash, fullUser.LegacyHash)
				if !ok {
					writeErrJSON(w, http.StatusForbidden, "current_password is incorrect")
					return
				}
				fullUser.Email = nextEmail
			}
		}
		if req.Bio != nil {
			nextBio := strings.TrimSpace(repo.StripASCIIControls(*req.Bio))
			if len([]rune(nextBio)) > 500 {
				writeErrJSON(w, http.StatusBadRequest, "bio must not exceed 500 characters")
				return
			}
			if actor.Role == domain.RoleEditor && nextBio != "" && s.deps.Plugins != nil {
				if p := s.deps.Plugins.Providers(); p != nil {
					verdict := p.EvaluateComment(r.Context(), plugin.CommentCheckInput{
						TargetType: "profile_bio",
						TargetID:   fullUser.ID,
						AuthorName: fullUser.DisplayName,
						AuthorURL:  fullUser.WebsiteURL,
						ContentMD:  nextBio,
						IPMasked:   maskedIP,
						IsGuest:    false,
						Locale:     locale,
					})
					if verdict.Action == plugin.SpamActionReject || verdict.Action == plugin.SpamActionModerate {
						msg := strings.TrimSpace(repo.StripASCIIControls(verdict.Reason))
						if msg == "" {
							msg = s.deps.Catalog.T(locale, "comment.err_antispam")
						}
						writeErrJSON(w, http.StatusBadRequest, msg)
						return
					}
				}
				if _, hErr := s.deps.Plugins.DispatchHook(r.Context(), plugin.HookUserBeforeUpdateProfile, map[string]any{
					"user_id":          fullUser.ID,
					"username":         fullUser.Username,
					"display_name":     fullUser.DisplayName,
					"bio":              nextBio,
					"email":            fullUser.Email,
					"client_ip_masked": maskedIP,
					"locale":           locale,
					"source":           "editor_profile_bio",
				}); hErr != nil {
					var rej *plugin.HookRejectedError
					if errors.As(hErr, &rej) {
						writeHookRejectedJSON(w, rej)
						return
					}
				}
			}
			fullUser.Bio = nextBio
		}
		if req.WebsiteURL != nil {
			rawWeb := strings.TrimSpace(*req.WebsiteURL)
			if rawWeb == "" {
				fullUser.WebsiteURL = ""
			} else {
				cleanWeb, ok := sanitizeRedirectTargetURL(rawWeb)
				if !ok {
					writeErrJSON(w, http.StatusBadRequest, "invalid website_url")
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
					writeErrJSON(w, http.StatusBadRequest, "invalid avatar_url")
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
				writeErrJSON(w, http.StatusForbidden, "current_password is incorrect")
				return
			}
			if writeBadErr(w, auth.ValidatePasswordPolicy(req.NewPassword)) {
				return
			}
			newHash, err := auth.HashPassword(req.NewPassword)
			if writeServerErr(w, err) {
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
		if writeBadErr(w, err) {
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
		writeErrJSON(w, http.StatusBadRequest, s.stepUpErrorMessage(r, true))
		return false
	}
	var fullUser domain.User
	err := sqlx.GetInto(r.Context(), s.deps.Store, &fullUser, `SELECT * FROM users WHERE id = ?`, actorID)
	if err != nil {
		writeErrJSON(w, http.StatusUnauthorized, s.stepUpErrorMessage(r, false))
		return false
	}
	ok, _ := auth.VerifyPassword(pwd, fullUser.PasswordHash, fullUser.LegacyHash)
	if !ok {
		writeErrJSON(w, http.StatusUnauthorized, s.stepUpErrorMessage(r, false))
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
		if writeServerErr(w, err) {
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
		if !decodeJSONBody(w, r, 8192, &req) {
			return
		}
		var fullUser domain.User
		err := sqlx.GetInto(r.Context(), s.deps.Store, &fullUser, `SELECT * FROM users WHERE id = ?`, actor.ID)
		if err != nil {
			writeErrJSON(w, http.StatusNotFound, "user not found")
			return
		}
		if req.CurrentPassword == "" {
			writeErrJSON(w, http.StatusForbidden, "current_password is incorrect")
			return
		}
		if ok, _ := auth.VerifyPassword(req.CurrentPassword, fullUser.PasswordHash, fullUser.LegacyHash); !ok {
			writeErrJSON(w, http.StatusForbidden, "current_password is incorrect")
			return
		}
		codes, err := s.deps.Auth.EnrollTOTP(r.Context(), actor.ID, req.Secret, req.Code)
		if writeBadErr(w, err) {
			return
		}
		s.recordAuditLog(r, actor, "security.totp_enroll", "user", actor.ID, nil)
		writeJSON(w, http.StatusOK, map[string]any{"recovery_codes": codes})

	case sub == "totp" && r.Method == http.MethodDelete:
		var req struct {
			CurrentPassword string `json:"current_password"`
		}
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req)
		var fullUser domain.User
		err := sqlx.GetInto(r.Context(), s.deps.Store, &fullUser, `SELECT * FROM users WHERE id = ?`, actor.ID)
		if err != nil {
			writeErrJSON(w, http.StatusNotFound, "user not found")
			return
		}
		ok, _ := auth.VerifyPassword(req.CurrentPassword, fullUser.PasswordHash, fullUser.LegacyHash)
		if !ok {
			writeErrJSON(w, http.StatusForbidden, "current_password is incorrect")
			return
		}
		_, _ = s.deps.Store.ExecContext(r.Context(),
			`DELETE FROM user_credentials WHERE user_id = ? AND kind IN ('totp', 'recovery_codes')`, actor.ID)
		s.recordAuditLog(r, actor, "security.totp_disable", "user", actor.ID, nil)
		writeOKJSON(w)

	case sub == "passkeys" && r.Method == http.MethodGet:
		page, perPage := parseAdminPageParams(r, 20, 100)
		var total int
		_ = s.deps.Store.QueryRowContext(r.Context(),
			`SELECT COUNT(*) FROM user_credentials WHERE user_id = ? AND kind = 'passkey'`, actor.ID).Scan(&total)
		baseSQL := `SELECT id, user_id, kind, name, credential_id, cose_alg, sign_count, last_used_at, created_at
			 FROM user_credentials WHERE user_id = ? AND kind = 'passkey' ORDER BY created_at DESC`
		qSQL, fullArgs := s.paginateQuery(baseSQL, []any{actor.ID}, page, perPage)
		var rows []domain.UserCredential
		_ = sqlx.SelectInto(r.Context(), s.deps.Store, &rows, qSQL, fullArgs...)
		if rows == nil {
			rows = []domain.UserCredential{}
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"items":     rows,
			"total":     total,
			"page":      page,
			"per_page":  perPage,
			"page_size": perPage,
		})

	case sub == "passkeys/begin" && r.Method == http.MethodPost:
		res, err := auth.BeginPasskeyCeremony(s.deps.Config.KeyRing, actor.ID, "register", s.publicBaseURL(r))
		if writeBadErr(w, err) {
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
		if !decodeJSONBody(w, r, 65536, &req) {
			return
		}
		var fullUser domain.User
		err := sqlx.GetInto(r.Context(), s.deps.Store, &fullUser, `SELECT * FROM users WHERE id = ?`, actor.ID)
		if err != nil {
			writeErrJSON(w, http.StatusNotFound, "user not found")
			return
		}
		if req.CurrentPassword == "" {
			writeErrJSON(w, http.StatusForbidden, "current_password is incorrect")
			return
		}
		if ok, _ := auth.VerifyPassword(req.CurrentPassword, fullUser.PasswordHash, fullUser.LegacyHash); !ok {
			writeErrJSON(w, http.StatusForbidden, "current_password is incorrect")
			return
		}
		cdBytes, err1 := base64.RawURLEncoding.DecodeString(req.ClientDataJSON)
		attBytes, err2 := base64.RawURLEncoding.DecodeString(req.AttestationObject)
		if err1 != nil || err2 != nil {
			writeErrJSON(w, http.StatusBadRequest, "invalid base64url passkey registration payload")
			return
		}
		cred, err := s.deps.Auth.RegisterPasskey(r.Context(), actor.ID, req.Name, req.StateToken, cdBytes, attBytes)
		if writeBadErr(w, err) {
			return
		}
		cred.PublicKeySPKI = ""
		s.recordAuditLog(r, actor, "security.passkey_register", "user_credential", cred.ID, nil)
		writeJSON(w, http.StatusCreated, map[string]any{"passkey": cred})

	case strings.HasPrefix(sub, "passkeys/") && r.Method == http.MethodDelete:
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
		pkID := strings.TrimPrefix(sub, "passkeys/")
		_, _ = s.deps.Store.ExecContext(r.Context(),
			`DELETE FROM user_credentials WHERE id = ? AND user_id = ? AND kind = 'passkey'`, pkID, actor.ID)
		s.recordAuditLog(r, actor, "security.passkey_delete", "user_credential", pkID, nil)
		writeOKJSON(w)

	case sub == "sessions" && r.Method == http.MethodGet:
		page, perPage := parseAdminPageParams(r, 20, 100)
		rows, _ := s.deps.Auth.ListUserSessions(r.Context(), actor.ID)
		if rows == nil {
			rows = []domain.UserSession{}
		}
		total := len(rows)
		start := (page - 1) * perPage
		if start > total {
			start = total
		}
		end := start + perPage
		if end > total {
			end = total
		}
		paged := rows[start:end]
		curID := ""
		if sess != nil {
			curID = sess.ID
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"items":              paged,
			"current_session_id": curID,
			"total":              total,
			"page":               page,
			"per_page":           perPage,
			"page_size":          perPage,
		})

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
		if writeServerErr(w, err) {
			return
		}
		revoked, _ := res.RowsAffected()
		s.recordAuditLog(r, actor, "security.sessions_revoke_others", "user_session", actor.ID, map[string]any{"revoked": revoked})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "revoked": revoked})

	case strings.HasPrefix(sub, "sessions/") && r.Method == http.MethodDelete:
		sid := strings.TrimPrefix(sub, "sessions/")
		if err := s.deps.Auth.RevokeSession(r.Context(), actor, sid); err != nil {
			writeErrJSON(w, http.StatusForbidden, err.Error())
			return
		}
		s.recordAuditLog(r, actor, "security.session_revoke", "user_session", sid, nil)
		writeOKJSON(w)

	case sub == "tokens" && r.Method == http.MethodGet:
		page, perPage := parseAdminPageParams(r, 20, 100)
		var total int
		_ = s.deps.Store.QueryRowContext(r.Context(),
			`SELECT COUNT(*) FROM api_tokens WHERE user_id = ?`, actor.ID).Scan(&total)
		baseSQL := `SELECT id, user_id, name, token_prefix, scopes_json, last_used_at, expires_at, created_at
			 FROM api_tokens WHERE user_id = ? ORDER BY created_at DESC`
		qSQL, fullArgs := s.paginateQuery(baseSQL, []any{actor.ID}, page, perPage)
		var rows []domain.APIToken
		_ = sqlx.SelectInto(r.Context(), s.deps.Store, &rows, qSQL, fullArgs...)
		if rows == nil {
			rows = []domain.APIToken{}
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"items":     rows,
			"total":     total,
			"page":      page,
			"per_page":  perPage,
			"page_size": perPage,
		})

	case sub == "tokens" && r.Method == http.MethodPost:
		if sess == nil || sess.ID == "" {
			writeErrJSON(w, http.StatusForbidden, s.deps.Catalog.T(s.resolveLocale(r), "error.forbidden"))
			return
		}
		var req struct {
			Name            string   `json:"name"`
			Scopes          []string `json:"scopes"`
			ExpiresIn       int      `json:"expires_in_days"`
			CurrentPassword string   `json:"current_password"`
		}
		if !decodeJSONBody(w, r, 8192, &req) {
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
			writeErrJSON(w, http.StatusForbidden, err.Error())
			return
		}
		if writeBadErr(w, err) {
			return
		}
		s.recordAuditLog(r, actor, "security.api_token_create", "api_token", rec.ID, map[string]any{"scopes": req.Scopes})
		writeJSON(w, http.StatusCreated, map[string]any{"token": rec, "plaintext_token": plainTok})

	case strings.HasPrefix(sub, "tokens/") && r.Method == http.MethodDelete:
		tid := strings.TrimPrefix(sub, "tokens/")
		_, _ = s.deps.Store.ExecContext(r.Context(), `DELETE FROM api_tokens WHERE id = ? AND user_id = ?`, tid, actor.ID)
		s.recordAuditLog(r, actor, "security.api_token_revoke", "api_token", tid, nil)
		writeOKJSON(w)

	default:
		http.NotFound(w, r)
	}
}

// --- Workspace 1: Overview Dashboard & Unified Articles + Revisions + Live Preview ---

type overviewKPIs struct {
	pubCnt     int
	draftCnt   int
	schedCnt   int
	protCnt    int
	pageCnt    int
	cmtPending int
	mediaCnt   int
	totalViews int64
}

type overviewTopArticleView struct {
	ItemKey string `json:"item_key"`
	ID      string `json:"id"`
	Title   string `json:"title"`
	Slug    string `json:"slug"`
	Hits    int64  `json:"hits"`
}

type overviewRecentCommentView struct {
	domain.Comment
	TargetTitle string `json:"target_title"`
	TargetSlug  string `json:"target_slug"`
}

type overviewMonthBucket struct {
	Month string `json:"month"`
	Count int    `json:"count"`
}

func (s *AppServer) queryOverviewKPIs(ctx context.Context, actor *domain.User) overviewKPIs {
	var k overviewKPIs
	if actor.Role == domain.RoleAdmin {
		_ = s.deps.Store.QueryRowContext(ctx,
			`SELECT
				COALESCE(SUM(CASE WHEN status = 'published' THEN 1 ELSE 0 END), 0),
				COALESCE(SUM(CASE WHEN status = 'draft' THEN 1 ELSE 0 END), 0),
				COALESCE(SUM(CASE WHEN status = 'scheduled' THEN 1 ELSE 0 END), 0),
				COALESCE(SUM(CASE WHEN password_ciphertext <> '' OR has_content_lock = ? THEN 1 ELSE 0 END), 0),
				COALESCE(SUM(view_count), 0)
			 FROM articles`, true).Scan(&k.pubCnt, &k.draftCnt, &k.schedCnt, &k.protCnt, &k.totalViews)
		_ = s.deps.Store.QueryRowContext(ctx, `SELECT COUNT(*) FROM pages`).Scan(&k.pageCnt)
		_ = s.deps.Store.QueryRowContext(ctx, `SELECT COUNT(*) FROM comments WHERE status = 'pending' AND is_deleted = ?`, false).Scan(&k.cmtPending)
		_ = s.deps.Store.QueryRowContext(ctx, `SELECT COUNT(*) FROM media_assets`).Scan(&k.mediaCnt)
		return k
	}
	_ = s.deps.Store.QueryRowContext(ctx,
		`SELECT
			COALESCE(SUM(CASE WHEN status = 'published' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN status = 'draft' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN status = 'scheduled' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN password_ciphertext <> '' OR has_content_lock = ? THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(view_count), 0)
		 FROM articles WHERE author_id = ?`, true, actor.ID).Scan(&k.pubCnt, &k.draftCnt, &k.schedCnt, &k.protCnt, &k.totalViews)
	_ = s.deps.Store.QueryRowContext(ctx, `SELECT COUNT(*) FROM pages WHERE author_id = ?`, actor.ID).Scan(&k.pageCnt)
	_ = s.deps.Store.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM comments c
		 LEFT JOIN articles a ON c.target_type = 'article' AND c.target_id = a.id
		 LEFT JOIN pages p ON c.target_type = 'page' AND c.target_id = p.id
		 WHERE c.is_deleted = ? AND c.status = 'pending' AND (a.author_id = ? OR p.author_id = ?)`,
		false, actor.ID, actor.ID).Scan(&k.cmtPending)
	_ = s.deps.Store.QueryRowContext(ctx, `SELECT COUNT(*) FROM media_assets WHERE uploader_id = ?`, actor.ID).Scan(&k.mediaCnt)
	return k
}

func (s *AppServer) queryOverviewTopPosts(ctx context.Context, actor *domain.User, cutoffDate string) []overviewTopArticleView {
	if actor.Role == domain.RoleAdmin {
		var rawTopPosts []struct {
			ItemKey string `db:"item_key"`
			Hits    int64  `db:"hits"`
		}
		_ = sqlx.SelectInto(ctx, s.deps.Store, &rawTopPosts,
			`SELECT item_key, SUM(hits) AS hits FROM analytics_top_items
			 WHERE stat_date >= ? AND kind = 'article' GROUP BY item_key ORDER BY hits DESC LIMIT 10`, cutoffDate)
		if len(rawTopPosts) == 0 {
			return []overviewTopArticleView{}
		}
		ph := strings.Repeat("?,", len(rawTopPosts)-1) + "?"
		args := make([]any, 0, len(rawTopPosts)*2)
		for _, tp := range rawTopPosts {
			args = append(args, tp.ItemKey)
		}
		for _, tp := range rawTopPosts {
			args = append(args, tp.ItemKey)
		}
		type artMeta struct {
			ID    string `db:"id"`
			Title string `db:"title"`
			Slug  string `db:"slug"`
		}
		var metas []artMeta
		_ = sqlx.SelectInto(ctx, s.deps.Store, &metas,
			`SELECT id, title, slug FROM articles WHERE id IN (`+ph+`) OR slug IN (`+ph+`)`, args...)
		byID := make(map[string]artMeta, len(metas))
		bySlug := make(map[string]artMeta, len(metas))
		for _, m := range metas {
			byID[m.ID] = m
			if m.Slug != "" {
				bySlug[m.Slug] = m
			}
		}
		topPosts := make([]overviewTopArticleView, 0, len(rawTopPosts))
		for _, tp := range rawTopPosts {
			m, ok := byID[tp.ItemKey]
			if !ok {
				m = bySlug[tp.ItemKey]
			}
			artID := m.ID
			if artID == "" {
				artID = tp.ItemKey
			}
			title := m.Title
			if title == "" {
				title = tp.ItemKey
			}
			topPosts = append(topPosts, overviewTopArticleView{
				ItemKey: tp.ItemKey,
				ID:      artID,
				Title:   title,
				Slug:    m.Slug,
				Hits:    tp.Hits,
			})
		}
		return topPosts
	}

	var rawTopPosts []struct {
		ID    string `db:"id"`
		Title string `db:"title"`
		Slug  string `db:"slug"`
		Hits  int64  `db:"hits"`
	}
	_ = sqlx.SelectInto(ctx, s.deps.Store, &rawTopPosts,
		`SELECT a.id, a.title, a.slug, SUM(t.hits) AS hits
		 FROM analytics_top_items t
		 INNER JOIN articles a ON (a.id = t.item_key OR a.slug = t.item_key)
		 WHERE t.stat_date >= ? AND t.kind = 'article' AND a.author_id = ?
		 GROUP BY a.id, a.title, a.slug
		 ORDER BY hits DESC LIMIT 10`, cutoffDate, actor.ID)
	topPosts := make([]overviewTopArticleView, 0, len(rawTopPosts))
	for _, tp := range rawTopPosts {
		topPosts = append(topPosts, overviewTopArticleView{
			ItemKey: tp.ID,
			ID:      tp.ID,
			Title:   tp.Title,
			Slug:    tp.Slug,
			Hits:    tp.Hits,
		})
	}
	if len(topPosts) == 0 {
		var fallbackTop []struct {
			ID        string `db:"id"`
			Title     string `db:"title"`
			Slug      string `db:"slug"`
			ViewCount int64  `db:"view_count"`
		}
		_ = sqlx.SelectInto(ctx, s.deps.Store, &fallbackTop,
			`SELECT id, title, slug, view_count FROM articles WHERE author_id = ? AND view_count > 0 ORDER BY view_count DESC LIMIT 10`, actor.ID)
		for _, tp := range fallbackTop {
			topPosts = append(topPosts, overviewTopArticleView{
				ItemKey: tp.ID,
				ID:      tp.ID,
				Title:   tp.Title,
				Slug:    tp.Slug,
				Hits:    tp.ViewCount,
			})
		}
	}
	if topPosts == nil {
		topPosts = []overviewTopArticleView{}
	}
	return topPosts
}

func (s *AppServer) queryOverviewRecentComments(ctx context.Context, actor *domain.User) []overviewRecentCommentView {
	isAdmin := actor.Role == domain.RoleAdmin
	var rawRecentComments []domain.Comment
	if isAdmin {
		_ = sqlx.SelectInto(ctx, s.deps.Store, &rawRecentComments,
			`SELECT * FROM comments WHERE is_deleted = ? ORDER BY created_at DESC LIMIT 8`, false)
	} else {
		_ = sqlx.SelectInto(ctx, s.deps.Store, &rawRecentComments,
			`SELECT c.* FROM comments c
			 LEFT JOIN articles a ON c.target_type = 'article' AND c.target_id = a.id
			 LEFT JOIN pages p ON c.target_type = 'page' AND c.target_id = p.id
			 WHERE c.is_deleted = ? AND (a.author_id = ? OR p.author_id = ?)
			 ORDER BY c.created_at DESC LIMIT 8`,
			false, actor.ID, actor.ID)
	}
	if len(rawRecentComments) == 0 {
		return []overviewRecentCommentView{}
	}

	type targetMeta struct {
		ID    string `db:"id"`
		Title string `db:"title"`
		Slug  string `db:"slug"`
	}
	artIDSet := make(map[string]struct{})
	pageIDSet := make(map[string]struct{})
	var artArgs, pageArgs []any
	for _, rc := range rawRecentComments {
		if rc.TargetID == "" {
			continue
		}
		if rc.TargetType == domain.TargetTypePage {
			if _, ok := pageIDSet[rc.TargetID]; !ok {
				pageIDSet[rc.TargetID] = struct{}{}
				pageArgs = append(pageArgs, rc.TargetID)
			}
		} else {
			if _, ok := artIDSet[rc.TargetID]; !ok {
				artIDSet[rc.TargetID] = struct{}{}
				artArgs = append(artArgs, rc.TargetID)
			}
		}
	}
	artMetas := make(map[string]targetMeta, len(artArgs))
	if len(artArgs) > 0 {
		ph := strings.Repeat("?,", len(artArgs)-1) + "?"
		var rows []targetMeta
		_ = sqlx.SelectInto(ctx, s.deps.Store, &rows, `SELECT id, title, slug FROM articles WHERE id IN (`+ph+`)`, artArgs...)
		for _, m := range rows {
			artMetas[m.ID] = m
		}
	}
	pageMetas := make(map[string]targetMeta, len(pageArgs))
	if len(pageArgs) > 0 {
		ph := strings.Repeat("?,", len(pageArgs)-1) + "?"
		var rows []targetMeta
		_ = sqlx.SelectInto(ctx, s.deps.Store, &rows, `SELECT id, title, slug FROM pages WHERE id IN (`+ph+`)`, pageArgs...)
		for _, m := range rows {
			pageMetas[m.ID] = m
		}
	}

	recentComments := make([]overviewRecentCommentView, 0, len(rawRecentComments))
	for _, rc := range rawRecentComments {
		ownerID := actor.ID
		if isAdmin {
			ownerID = ""
		}
		scrubbed, sErr := authorizeAndScrubAdminComment(actor, rc, ownerID)
		if sErr != nil {
			continue
		}
		var m targetMeta
		if rc.TargetType == domain.TargetTypePage {
			m = pageMetas[rc.TargetID]
		} else {
			m = artMetas[rc.TargetID]
		}
		recentComments = append(recentComments, overviewRecentCommentView{
			Comment:     scrubbed,
			TargetTitle: m.Title,
			TargetSlug:  m.Slug,
		})
	}
	return recentComments
}

func (s *AppServer) queryOverviewMonthlyBuckets(ctx context.Context, actor *domain.User, tz string, siteLoc *time.Location, now time.Time) ([]overviewMonthBucket, []overviewMonthBucket) {
	isAdmin := actor.Role == domain.RoleAdmin
	allMonthCounts := make(map[string]int)
	if isAdmin && s.deps.Content != nil {
		if stats, err := s.deps.Content.ArchiveMonthStats(ctx, tz); err == nil && stats.MonthCounts != nil {
			allMonthCounts = stats.MonthCounts
		}
	} else {
		type artDateRow struct {
			PublishedAt int64 `db:"published_at"`
			CreatedAt   int64 `db:"created_at"`
		}
		var artDates []artDateRow
		if isAdmin {
			_ = sqlx.SelectInto(ctx, s.deps.Store, &artDates,
				`SELECT published_at, created_at FROM articles WHERE status = 'published'`)
		} else {
			_ = sqlx.SelectInto(ctx, s.deps.Store, &artDates,
				`SELECT published_at, created_at FROM articles WHERE author_id = ? AND status = 'published'`,
				actor.ID)
		}
		for _, ad := range artDates {
			ts := ad.PublishedAt
			if ts <= 0 {
				ts = ad.CreatedAt
			}
			if ts > 0 {
				ym := time.UnixMilli(ts).In(siteLoc).Format("2006-01")
				allMonthCounts[ym]++
			}
		}
	}
	histKeys := make([]string, 0, len(allMonthCounts))
	for ym, cnt := range allMonthCounts {
		if cnt > 0 {
			histKeys = append(histKeys, ym)
		}
	}
	sort.Strings(histKeys)
	monthlyHistory := make([]overviewMonthBucket, 0, len(histKeys))
	for _, ym := range histKeys {
		monthlyHistory = append(monthlyHistory, overviewMonthBucket{Month: ym, Count: allMonthCounts[ym]})
	}

	curMonthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, siteLoc)
	anchorMonthStart := curMonthStart
	trailingTotal := 0
	for i := 11; i >= 0; i-- {
		m := curMonthStart.AddDate(0, -i, 0).Format("2006-01")
		trailingTotal += allMonthCounts[m]
	}
	if trailingTotal == 0 && len(histKeys) > 0 {
		if lt, pErr := time.ParseInLocation("2006-01", histKeys[len(histKeys)-1], siteLoc); pErr == nil {
			anchorMonthStart = time.Date(lt.Year(), lt.Month(), 1, 0, 0, 0, 0, siteLoc)
		}
	}
	monthlyArticles := make([]overviewMonthBucket, 0, 12)
	for i := 11; i >= 0; i-- {
		m := anchorMonthStart.AddDate(0, -i, 0).Format("2006-01")
		monthlyArticles = append(monthlyArticles, overviewMonthBucket{Month: m, Count: allMonthCounts[m]})
	}
	return monthlyArticles, monthlyHistory
}

func (s *AppServer) handleAdminOverview(w http.ResponseWriter, r *http.Request, actor *domain.User) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	ctx := r.Context()
	if s.deps.Content != nil {
		_ = s.deps.Content.FlushAnalytics(ctx)
	}
	isAdmin := actor.Role == domain.RoleAdmin
	kpis := s.queryOverviewKPIs(ctx, actor)

	snap := s.deps.Settings.Snapshot()
	siteLoc := s.siteTimezoneLocation(snap.Site.TimeZone)

	windowDays := 30
	if rawDays, err := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("days"))); err == nil && rawDays == 7 {
		windowDays = 7
	}
	now := time.Now().In(siteLoc)
	cutoffDate := now.AddDate(0, 0, -(windowDays - 1)).Format("2006-01-02")
	var trend []domain.AnalyticsDaily
	if isAdmin {
		_ = sqlx.SelectInto(ctx, s.deps.Store, &trend,
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

	topPosts := s.queryOverviewTopPosts(ctx, actor, cutoffDate)

	type topRefererView struct {
		ItemKey string `db:"item_key" json:"item_key"`
		Hits    int64  `db:"hits" json:"hits"`
	}
	var topReferers []topRefererView
	var topCountries []topRefererView
	if isAdmin {
		_ = sqlx.SelectInto(ctx, s.deps.Store, &topReferers,
			`SELECT item_key, SUM(hits) AS hits FROM analytics_top_items
			 WHERE stat_date >= ? AND kind = 'referer' GROUP BY item_key ORDER BY hits DESC LIMIT 10`, cutoffDate)
		_ = sqlx.SelectInto(ctx, s.deps.Store, &topCountries,
			`SELECT item_key, SUM(hits) AS hits FROM analytics_top_items
			 WHERE stat_date >= ? AND kind = 'country' GROUP BY item_key ORDER BY hits DESC LIMIT 10`, cutoffDate)
	}
	if topReferers == nil {
		topReferers = []topRefererView{}
	}
	if topCountries == nil {
		topCountries = []topRefererView{}
	}

	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)

	locale := actor.PreferredLocale
	if locale == "" {
		locale = snap.Site.DefaultLocale
	}
	var quickInserts []plugin.EditorQuickInsertView
	var dashCards []plugin.RenderedSlot
	if s.deps.Plugins != nil {
		quickInserts = s.deps.Plugins.EditorQuickInserts(locale)
		dashCards = s.deps.Plugins.RenderSlots(ctx, plugin.SlotAdminDashboardCard, plugin.SlotRenderContext{
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

	monthlyArticles, monthlyHistory := s.queryOverviewMonthlyBuckets(ctx, actor, snap.Site.TimeZone, siteLoc, now)
	catUsage, _ := s.deps.Content.ListCategoriesWithCounts(ctx)
	topTags, _ := s.deps.Content.ListTagsWithCounts(ctx, 5)
	recentComments := s.queryOverviewRecentComments(ctx, actor)

	var recentLogs []domain.AuditLog
	if isAdmin {
		_ = sqlx.SelectInto(ctx, s.deps.Store, &recentLogs,
			`SELECT * FROM audit_logs ORDER BY created_at DESC LIMIT 8`)
	} else {
		_ = sqlx.SelectInto(ctx, s.deps.Store, &recentLogs,
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
			"published":        kpis.pubCnt,
			"drafts":           kpis.draftCnt,
			"scheduled":        kpis.schedCnt,
			"protected":        kpis.protCnt,
			"pages":            kpis.pageCnt,
			"pending_comments": kpis.cmtPending,
			"media":            kpis.mediaCnt,
			"views":            int(kpis.totalViews),
		},
		"total_views":      kpis.totalViews,
		"window_days":      windowDays,
		"analytics_daily":  trend,
		"analytics_series": series,
		"top_articles":     topPosts,
		"top_referers":     topReferers,
		"top_countries":    topCountries,
		"monthly_articles": monthlyArticles,
		"monthly_history":  monthlyHistory,
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
	adminPref := strings.ToLower(s.staticAdminPrefix())
	effAdmin, _ := s.effectiveAdminPrefix()
	effPref := strings.ToLower(effAdmin)
	reservedPrefixes := []string{
		"/api/", "/console/", "/setup/", "/posts/", "/uploads/", "/themes/", "/plugins/", "/static/", "/assets/",
		adminPref + "/", effPref + "/",
	}
	reservedExact := []string{
		"/api", "/console", "/setup", "/feed", "/feed.xml", "/rss", "/rss.xml", "/atom.xml", "/feed.json",
		"/sitemap.xml", "/robots.txt", "/llms.txt", "/llms-full.txt", adminPref, effPref,
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

func (s *AppServer) handleAdminRenderPreview(w http.ResponseWriter, r *http.Request, actor *domain.User) {
	var req struct {
		ContentMD          string `json:"content_md"`
		Markdown           string `json:"markdown"`
		Locale             string `json:"locale"`
		PreserveDirectives bool   `json:"preserve_directives"`
		HasAudio           bool   `json:"has_audio"`
	}
	if !decodeJSONBody(w, r, 4<<20, &req) {
		return
	}
	if req.ContentMD == "" && req.Markdown != "" {
		req.ContentMD = req.Markdown
	}
	ctx := r.Context()
	resolvedLoc := s.deps.Catalog.Match(req.Locale, actor.PreferredLocale)
	contentMD := req.ContentMD
	if !req.PreserveDirectives && s.deps.Plugins != nil {
		preMD := s.deps.Plugins.DispatchRenderPreMarkdown(ctx, plugin.ArticleHookPayload{
			ContentMD:           contentMD,
			Lang:                resolvedLoc,
			Status:              domain.ArticleStatusDraft,
			IsPasswordProtected: false,
			HasContentLock:      false,
		})
		if preMD != "" {
			contentMD = preMD
		}
	}
	var customDirective func(string, map[string]string, string) (string, bool)
	if !req.PreserveDirectives && s.deps.Plugins != nil {
		customDirective = func(name string, attrs map[string]string, body string) (string, bool) {
			return s.deps.Plugins.DispatchRenderDirective(ctx, name, attrs, body)
		}
	}
	out := render.Default().Render(contentMD, render.Options{
		Mode:                render.RenderModeWeb,
		Locale:              resolvedLoc,
		Unlocked:            true,
		AuthorOrShareBypass: true,
		IsReaderLoggedIn:    true,
		HasAudio:            req.HasAudio,
		ShowLineNumbers:     s.deps.Settings.Snapshot().Reading.ShowCodeLineNumbers,
		PreserveDirectives:  req.PreserveDirectives,
		LookupBook:          func(bSlug string) (render.BookCardInfo, bool) { return s.deps.Content.LookupBookCard(ctx, bSlug) },
		ListBooks:           func() []render.BookCardInfo { return s.deps.Content.ListBookCards(ctx) },
		LookupMediaDims:     s.deps.Content.LookupMediaDims,
		CustomDirective:     customDirective,
	})
	if !req.PreserveDirectives && s.deps.Plugins != nil && out.HTML != "" && !render.ContainsLockDirective(contentMD) {
		postHTML := s.deps.Plugins.DispatchRenderPostHTML(ctx, plugin.ArticleHookPayload{
			ContentMD:           contentMD,
			HTML:                out.HTML,
			Lang:                resolvedLoc,
			Status:              domain.ArticleStatusDraft,
			IsPasswordProtected: false,
			HasContentLock:      false,
		})
		if postHTML != "" && s.deps.Content != nil {
			out.HTML = s.deps.Content.SanitizeHTMLFragment(postHTML)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"html":          out.HTML,
		"toc":           out.TOC,
		"needs_math":    out.NeedsMath,
		"needs_mermaid": out.NeedsMermaid,
		"has_audio":     req.HasAudio || strings.Contains(out.HTML, "<audio"),
	})
}

func (s *AppServer) handleAdminArticlesBatch(w http.ResponseWriter, r *http.Request, actor *domain.User) {
	var req struct {
		Action     string   `json:"action"`
		IDs        []string `json:"ids"`
		CategoryID string   `json:"category_id"`
	}
	if !decodeJSONBody(w, r, 65536, &req) {
		return
	}
	action := strings.ToLower(strings.TrimSpace(req.Action))
	switch action {
	case "publish", "draft", "delete", "category", "pin", "unpin":
	default:
		writeErrJSON(w, http.StatusBadRequest, "invalid batch action")
		return
	}
	ids := dedupCleanIDs(req.IDs, 200)
	if len(ids) == 0 {
		writeErrJSON(w, http.StatusBadRequest, "ids required")
		return
	}

	ctx := r.Context()
	ph := strings.Repeat("?,", len(ids)-1) + "?"
	idArgs := make([]any, len(ids))
	for i, aid := range ids {
		idArgs[i] = aid
	}
	var fetched []domain.RawArticle
	if err := sqlx.SelectInto(ctx, s.deps.Store, &fetched, `SELECT * FROM articles WHERE id IN (`+ph+`)`, idArgs...); writeServerErr(w, err) {
		return
	}
	byID := make(map[string]domain.RawArticle, len(fetched))
	for _, raw := range fetched {
		byID[raw.ID] = raw
	}
	articles := make([]domain.RawArticle, 0, len(fetched))
	for _, aid := range ids {
		raw, ok := byID[aid]
		if !ok {
			continue
		}
		if !auth.CanManageArticle(actor, raw.AuthorID) {
			writeErrJSON(w, http.StatusForbidden, s.deps.Catalog.T(s.resolveLocale(r), "error.forbidden"))
			return
		}
		articles = append(articles, raw)
	}

	type batchEffect struct {
		raw          domain.RawArticle
		wasPublished bool
	}
	effects := make([]batchEffect, 0, len(articles))
	now := time.Now().UnixMilli()
	catID := strings.TrimSpace(req.CategoryID)

	if len(articles) > 0 {
		txErr := s.deps.Store.WithTx(ctx, func(tx *sqlx.Tx) error {
			for _, raw := range articles {
				wasPublished := raw.Status == domain.ArticleStatusPublished
				switch action {
				case "publish":
					pubAt := raw.PublishedAt
					if pubAt <= 0 {
						pubAt = now
					}
					if _, uErr := tx.ExecContext(ctx,
						`UPDATE articles SET status = ?, published_at = ?, updated_at = ? WHERE id = ?`,
						domain.ArticleStatusPublished, pubAt, now, raw.ID); uErr != nil {
						return uErr
					}
					raw.Status = domain.ArticleStatusPublished
					raw.PublishedAt = pubAt
					raw.UpdatedAt = now
					effects = append(effects, batchEffect{raw: raw, wasPublished: wasPublished})
				case "draft":
					if _, uErr := tx.ExecContext(ctx,
						`UPDATE articles SET status = ?, updated_at = ? WHERE id = ?`,
						domain.ArticleStatusDraft, now, raw.ID); uErr != nil {
						return uErr
					}
					raw.Status = domain.ArticleStatusDraft
					raw.UpdatedAt = now
					effects = append(effects, batchEffect{raw: raw, wasPublished: wasPublished})
				case "category":
					if _, uErr := tx.ExecContext(ctx,
						`UPDATE articles SET category_id = ?, updated_at = ? WHERE id = ?`,
						catID, now, raw.ID); uErr != nil {
						return uErr
					}
					raw.CategoryID = catID
					raw.UpdatedAt = now
					effects = append(effects, batchEffect{raw: raw, wasPublished: wasPublished})
				case "pin", "unpin":
					pinned := action == "pin"
					if _, uErr := tx.ExecContext(ctx,
						`UPDATE articles SET is_pinned = ?, updated_at = ? WHERE id = ?`,
						pinned, now, raw.ID); uErr != nil {
						return uErr
					}
					raw.IsPinned = pinned
					raw.UpdatedAt = now
					effects = append(effects, batchEffect{raw: raw, wasPublished: wasPublished})
				case "delete":
					_, _ = tx.ExecContext(ctx, `DELETE FROM article_tags WHERE article_id = ?`, raw.ID)
					_, _ = tx.ExecContext(ctx, `DELETE FROM series_articles WHERE article_id = ?`, raw.ID)
					_, _ = tx.ExecContext(ctx, `DELETE FROM article_revisions WHERE article_id = ?`, raw.ID)
					_, _ = tx.ExecContext(ctx, `DELETE FROM article_backlinks WHERE source_article_id = ? OR target_article_id = ?`, raw.ID, raw.ID)
					s.deleteTargetCommentsAndReactionsTx(ctx, tx, domain.TargetTypeArticle, raw.ID)
					if _, dErr := tx.ExecContext(ctx, `DELETE FROM articles WHERE id = ?`, raw.ID); dErr != nil {
						return dErr
					}
					effects = append(effects, batchEffect{raw: raw, wasPublished: wasPublished})
				}
			}
			return nil
		})
		if writeServerErr(w, txErr) {
			return
		}
	}

	affected := len(effects)
	for _, eff := range effects {
		raw := eff.raw
		switch action {
		case "publish":
			s.deps.Content.SearchIndex().Upsert(raw)
			if !eff.wasPublished {
				if s.deps.Plugins != nil {
					s.deps.Plugins.DispatchArticleHook(ctx, plugin.HookArticlePublished, articleHookPayloadFromRaw(raw))
				}
				s.dispatchFederationForArticle(ctx, "publish", raw)
				if s.deps.NotifyArticlePublished != nil {
					s.deps.NotifyArticlePublished(ctx, raw)
				}
			} else {
				if s.deps.Plugins != nil {
					s.deps.Plugins.DispatchArticleHook(ctx, plugin.HookArticleUpdated, articleHookPayloadFromRaw(raw))
				}
				s.dispatchFederationForArticle(ctx, "update", raw)
			}
		case "draft":
			s.deps.Content.SearchIndex().Remove(raw.ID)
			if eff.wasPublished && s.deps.Plugins != nil {
				s.deps.Plugins.DispatchArticleHook(ctx, plugin.HookArticleDeleted, articleHookPayloadFromRaw(raw))
			}
		case "category", "pin", "unpin":
			if raw.Status == domain.ArticleStatusPublished {
				s.deps.Content.SearchIndex().Upsert(raw)
				if s.deps.Plugins != nil {
					s.deps.Plugins.DispatchArticleHook(ctx, plugin.HookArticleUpdated, articleHookPayloadFromRaw(raw))
				}
				s.dispatchFederationForArticle(ctx, "update", raw)
			}
		case "delete":
			s.deps.Content.SearchIndex().Remove(raw.ID)
			if s.deps.Plugins != nil {
				deletedCopy := raw
				deletedCopy.Status = "deleted"
				s.deps.Plugins.DispatchArticleHook(ctx, plugin.HookArticleDeleted, articleHookPayloadFromRaw(deletedCopy))
			}
		}
	}
	if affected > 0 {
		s.invalidateMediaRefCache()
	}
	s.recordAuditLog(r, actor, "article.batch", "article", action, map[string]any{"action": action, "affected": affected})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "affected": affected})
}

func (s *AppServer) handleAdminArticles(w http.ResponseWriter, r *http.Request, actor *domain.User, rest string) {
	rest = strings.TrimPrefix(rest, "/")

	if rest == "preview" && r.Method == http.MethodPost {
		s.handleAdminRenderPreview(w, r, actor)
		return
	}

	if rest == "import" && r.Method == http.MethodPost {
		s.handleAdminBatchImportArticles(w, r, actor)
		return
	}

	if rest == "batch" && r.Method == http.MethodPost {
		s.handleAdminArticlesBatch(w, r, actor)
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
			qSQL, fullArgs := s.paginateQuery(baseSQL, args, page, perPage)
			var rows []domain.RawArticle
			err := sqlx.SelectInto(r.Context(), s.deps.Store, &rows, qSQL, fullArgs...)
			if writeServerErr(w, err) {
				return
			}
			var catRows []domain.Category
			_ = sqlx.SelectInto(r.Context(), s.deps.Store, &catRows, `SELECT id, name FROM categories`)
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
		writeErrJSON(w, http.StatusNotFound, "article not found")
		return
	}
	if !auth.CanManageArticle(actor, raw.AuthorID) {
		writeErrJSON(w, http.StatusForbidden, s.deps.Catalog.T(s.resolveLocale(r), "error.forbidden"))
		return
	}

	if len(parts) == 2 && parts[1] == "revisions" && r.Method == http.MethodGet {
		page, perPage := parseAdminPageParams(r, 20, 100)
		var total int
		_ = s.deps.Store.QueryRowContext(r.Context(),
			`SELECT COUNT(*) FROM article_revisions WHERE article_id = ?`, artID).Scan(&total)
		summaryOnly := r.URL.Query().Get("summary") == "1" || strings.EqualFold(r.URL.Query().Get("summary"), "true")
		baseSQL := `SELECT id, article_id, title, summary_md, content_md, author_name, note, created_at FROM article_revisions WHERE article_id = ? ORDER BY created_at DESC`
		if summaryOnly {
			baseSQL = `SELECT id, article_id, title, summary_md, '' AS content_md, author_name, note, created_at FROM article_revisions WHERE article_id = ? ORDER BY created_at DESC`
		}
		qSQL, fullArgs := s.paginateQuery(baseSQL, []any{artID}, page, perPage)
		var revs []domain.ArticleRevision
		_ = sqlx.SelectInto(r.Context(), s.deps.Store, &revs, qSQL, fullArgs...)
		if revs == nil {
			revs = []domain.ArticleRevision{}
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"items":     revs,
			"total":     total,
			"page":      page,
			"per_page":  perPage,
			"page_size": perPage,
		})
		return
	}

	if len(parts) == 3 && parts[1] == "revisions" && r.Method == http.MethodGet {
		revID := parts[2]
		var rev domain.ArticleRevision
		if rErr := sqlx.GetInto(r.Context(), s.deps.Store, &rev,
			`SELECT * FROM article_revisions WHERE id = ? AND article_id = ?`, revID, artID); rErr != nil {
			writeErrJSON(w, http.StatusNotFound, "revision not found")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"revision": rev})
		return
	}

	if len(parts) == 4 && parts[1] == "revisions" && parts[3] == "restore" && r.Method == http.MethodPost {
		revID := parts[2]
		var rev domain.ArticleRevision
		rErr := sqlx.GetInto(r.Context(), s.deps.Store, &rev,
			`SELECT * FROM article_revisions WHERE id = ? AND article_id = ?`, revID, artID)
		if rErr != nil {
			writeErrJSON(w, http.StatusNotFound, "revision not found")
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
		if writeServerErr(w, err) {
			return
		}
		s.invalidateMediaRefCache()
		if raw.Status == domain.ArticleStatusPublished {
			s.deps.Content.SearchIndex().Upsert(raw)
			if s.deps.Plugins != nil {
				s.deps.Plugins.DispatchArticleHook(r.Context(), plugin.HookArticleUpdated, articleHookPayloadFromRaw(raw))
			}
			s.dispatchFederationForArticle(r.Context(), "update", raw)
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
			writeErrJSON(w, http.StatusBadRequest, tErr.Error())
			return
		}
		shareURL := strings.TrimRight(s.publicBaseURL(r), "/") + s.deps.Content.Guard().ArticleURL(raw) + "?preview=" + tok
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
			writeErrJSON(w, http.StatusInternalServerError, err.Error())
			return
		}
		raw.IsPinned = nextPinned
		raw.UpdatedAt = now
		if raw.Status == domain.ArticleStatusPublished {
			s.deps.Content.SearchIndex().Upsert(raw)
			if s.deps.Plugins != nil {
				s.deps.Plugins.DispatchArticleHook(r.Context(), plugin.HookArticleUpdated, articleHookPayloadFromRaw(raw))
			}
			s.dispatchFederationForArticle(r.Context(), "update", raw)
		}
		s.recordAuditLog(r, actor, "article.pin", "article", raw.ID, map[string]any{"is_pinned": nextPinned})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "is_pinned": nextPinned})
		return
	}

	if len(parts) == 1 {
		switch r.Method {
		case http.MethodGet:
			var tagRows []domain.Tag
			_ = sqlx.SelectInto(r.Context(), s.deps.Store, &tagRows,
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
			if writeServerErr(w, err) {
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
			writeOKJSON(w)

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
	if !decodeJSONBody(w, r, 8<<20, &in) {
		return
	}
	title := strings.TrimSpace(in.Title)
	if title == "" {
		writeErrJSON(w, http.StatusBadRequest, "title is required")
		return
	}

	cleanCover := ""
	if rawCover := strings.TrimSpace(in.CoverURL); rawCover != "" {
		u, ok := sanitizeRedirectTargetURL(rawCover)
		if !ok {
			writeErrJSON(w, http.StatusBadRequest, "invalid cover_url")
			return
		}
		cleanCover = u
	}
	cleanAudio := ""
	if rawAudio := strings.TrimSpace(in.AudioURL); rawAudio != "" {
		u, ok := sanitizeRedirectTargetURL(rawAudio)
		if !ok {
			writeErrJSON(w, http.StatusBadRequest, "invalid audio_url")
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
			writeErrJSON(w, http.StatusNotFound, "article not found")
			return
		}
		if !auth.CanManageArticle(actor, existing.AuthorID) {
			writeErrJSON(w, http.StatusForbidden, "forbidden")
			return
		}
		cur = existing
		wasPublished = existing.Status == domain.ArticleStatusPublished
	}

	if in.ClearLegacyPermalink {
		if cur.LegacyPermalink != "" {
			if err := auth.RequireAdmin(actor); err != nil {
				writeErrJSON(w, http.StatusForbidden, "only administrators can modify legacy_permalink")
				return
			}
		}
		cur.LegacyPermalink = ""
	} else if rawLeg := strings.TrimSpace(repo.StripASCIIControls(in.LegacyPermalink)); rawLeg != "" {
		if rawLeg != cur.LegacyPermalink {
			if err := auth.RequireAdmin(actor); err != nil {
				writeErrJSON(w, http.StatusForbidden, "only administrators can set or modify legacy_permalink")
				return
			}
			cleanLeg, vErr := s.validateLegacyPermalink(r.Context(), rawLeg, cur.ID)
			if vErr != nil {
				writeErrJSON(w, http.StatusBadRequest, vErr.Error())
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
				writeErrJSON(w, http.StatusInternalServerError, sErr.Error())
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

	if s.deps.Plugins != nil {
		hookOut, hErr := s.deps.Plugins.DispatchArticleHookWithError(r.Context(), plugin.HookArticleBeforeSave, articleHookPayloadFromRaw(cur))
		if hErr != nil {
			var rej *plugin.HookRejectedError
			if errors.As(hErr, &rej) {
				writeHookRejectedJSON(w, rej)
				return
			}
		} else {
			if nextTitle := strings.TrimSpace(hookOut.Title); nextTitle != "" && nextTitle != cur.Title {
				cur.Title = nextTitle
				if in.Slug == "" && isCreate {
					cur.Slug = s.ensureUniqueArticleSlug(r.Context(), sanitizeSlug(cur.Title), cur.ID)
				}
			}
			if hookOut.SummaryMD != cur.SummaryMD {
				cur.SummaryMD = hookOut.SummaryMD
			}
			if hookOut.ContentMD != cur.ContentMD {
				cur.ContentMD = hookOut.ContentMD
				cur.HasContentLock = render.ContainsLockDirective(cur.ContentMD)
				cur.WordCount, cur.ReadingMinutes = computeReadingMetrics(cur.ContentMD)
			}
			oldContentChanged = !isCreate && (prevSnapshot.Title != cur.Title || prevSnapshot.ContentMD != cur.ContentMD || prevSnapshot.SummaryMD != cur.SummaryMD)
		}
	}

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
	if writeServerErr(w, err) {
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
	if cur.Status == domain.ArticleStatusPublished {
		if !wasPublished {
			s.dispatchFederationForArticle(r.Context(), "publish", cur)
		} else {
			s.dispatchFederationForArticle(r.Context(), "update", cur)
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
	var oldRows []struct {
		ID string `db:"id"`
	}
	err := sqlx.SelectInto(ctx, tx, &oldRows, `SELECT id FROM article_revisions WHERE article_id = ? ORDER BY created_at DESC`, art.ID)
	if err == nil && len(oldRows) > 50 {
		for _, r := range oldRows[50:] {
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
	var cat domain.Category
	err := sqlx.GetInto(ctx, tx, &cat, `SELECT id FROM categories WHERE LOWER(name) = LOWER(?) OR slug = ?`, clean, slug)
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
		var tg domain.Tag
		err := sqlx.GetInto(ctx, tx, &tg, `SELECT id FROM tags WHERE LOWER(name) = LOWER(?) OR slug = ?`, tName, tSlug)
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

func (s *AppServer) dispatchPageBeforeSaveHook(ctx context.Context, pg *domain.Page, isCreate bool) *plugin.HookRejectedError {
	if s.deps.Plugins == nil || pg == nil {
		return nil
	}
	res, err := s.deps.Plugins.DispatchHook(ctx, plugin.HookPageBeforeSave, map[string]any{
		"id":           pg.ID,
		"author_id":    pg.AuthorID,
		"title":        pg.Title,
		"slug":         pg.Slug,
		"template":     pg.Template,
		"content_md":   pg.ContentMD,
		"lang":         pg.Lang,
		"is_published": pg.IsPublished,
		"is_create":    isCreate,
	})
	if err != nil {
		var rej *plugin.HookRejectedError
		if errors.As(err, &rej) {
			return rej
		}
		return nil
	}
	if outMap, ok := res.(map[string]any); ok {
		if nextTitle, sOk := outMap["title"].(string); sOk && strings.TrimSpace(nextTitle) != "" {
			pg.Title = strings.TrimSpace(nextTitle)
		}
		if nextMD, sOk := outMap["content_md"].(string); sOk {
			pg.ContentMD = nextMD
		}
	}
	return nil
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

			baseSQL := `SELECT id, author_id, title, slug, template, '' AS content_md, is_published, allow_comment, show_toc, lang, translation_group, view_count, comment_count, created_at, updated_at FROM pages WHERE ` + whereSQL + ` ORDER BY updated_at DESC`
			qSQL, fullArgs := s.paginateQuery(baseSQL, args, page, perPage)
			var rows []domain.Page
			_ = sqlx.SelectInto(r.Context(), s.deps.Store, &rows, qSQL, fullArgs...)
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
			if !decodeJSONBody(w, r, 4<<20, &in) {
				return
			}
			in.Title = strings.TrimSpace(in.Title)
			if in.Title == "" {
				writeErrJSON(w, http.StatusBadRequest, "title is required")
				return
			}
			in.ID = id.Next()
			in.AuthorID = actor.ID
			rawSlug := in.Slug
			in.Slug = sanitizeSlug(in.Slug)
			if in.Slug == "" {
				in.Slug = sanitizeSlug(in.Title)
			}
			in.Template = sanitizePageTemplateName(in.Template)
			if in.Lang == "" {
				in.Lang = s.deps.Settings.Snapshot().Site.DefaultLocale
			}
			if rej := s.dispatchPageBeforeSaveHook(r.Context(), &in, true); rej != nil {
				writeHookRejectedJSON(w, rej)
				return
			}
			if rawSlug == "" {
				in.Slug = sanitizeSlug(in.Title)
			}
			now := time.Now().UnixMilli()
			in.CreatedAt = now
			in.UpdatedAt = now
			if _, err := s.deps.Store.ExecContext(r.Context(),
				`INSERT INTO pages (id, author_id, title, slug, template, content_md, is_published, allow_comment, show_toc, lang, translation_group, view_count, comment_count, created_at, updated_at)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, 0, ?, ?)`,
				in.ID, in.AuthorID, in.Title, in.Slug, in.Template, in.ContentMD, in.IsPublished, in.AllowComment, in.ShowTOC,
				in.Lang, strings.TrimSpace(in.TranslationGroup), in.CreatedAt, in.UpdatedAt); err != nil {
				writeErrJSON(w, http.StatusBadRequest, err.Error())
				return
			}
			s.invalidateMediaRefCache()
			if in.IsPublished {
				if s.deps.Content != nil && s.deps.Content.SearchIndex() != nil {
					s.deps.Content.SearchIndex().UpsertPage(in)
				}
				s.dispatchPageLifecycleHook(r.Context(), plugin.HookPagePublished, in, "published")
			}
			writeJSON(w, http.StatusCreated, map[string]any{"page": in})

		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
		return
	}

	var pg domain.Page
	err := sqlx.GetInto(r.Context(), s.deps.Store, &pg, `SELECT * FROM pages WHERE id = ?`, rest)
	if err != nil {
		writeErrJSON(w, http.StatusNotFound, "page not found")
		return
	}
	if !auth.CanManageArticle(actor, pg.AuthorID) {
		writeErrJSON(w, http.StatusForbidden, "forbidden")
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"page": pg})
	case http.MethodPut:
		var in domain.Page
		if !decodeJSONBody(w, r, 4<<20, &in) {
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
		lang := strings.TrimSpace(in.Lang)
		if lang == "" {
			lang = pg.Lang
		}
		candidate := pg
		candidate.Title = strings.TrimSpace(in.Title)
		candidate.Slug = slug
		candidate.Template = tmpl
		candidate.ContentMD = in.ContentMD
		candidate.IsPublished = in.IsPublished
		candidate.AllowComment = in.AllowComment
		candidate.ShowTOC = in.ShowTOC
		candidate.Lang = lang
		candidate.TranslationGroup = strings.TrimSpace(in.TranslationGroup)
		if rej := s.dispatchPageBeforeSaveHook(r.Context(), &candidate, false); rej != nil {
			writeHookRejectedJSON(w, rej)
			return
		}
		now := time.Now().UnixMilli()
		_, err := s.deps.Store.ExecContext(r.Context(),
			`UPDATE pages SET title = ?, slug = ?, template = ?, content_md = ?, is_published = ?, allow_comment = ?, show_toc = ?, lang = ?, translation_group = ?, updated_at = ? WHERE id = ?`,
			candidate.Title, candidate.Slug, candidate.Template, candidate.ContentMD, candidate.IsPublished, candidate.AllowComment, candidate.ShowTOC,
			candidate.Lang, candidate.TranslationGroup, now, pg.ID)
		if writeBadErr(w, err) {
			return
		}
		s.invalidateMediaRefCache()
		_ = sqlx.GetInto(r.Context(), s.deps.Store, &pg, `SELECT * FROM pages WHERE id = ?`, pg.ID)
		if s.deps.Content != nil && s.deps.Content.SearchIndex() != nil {
			if pg.IsPublished {
				s.deps.Content.SearchIndex().UpsertPage(pg)
			} else {
				s.deps.Content.SearchIndex().RemovePage(pg.ID)
			}
		}
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
		if s.deps.Content != nil && s.deps.Content.SearchIndex() != nil {
			s.deps.Content.SearchIndex().RemovePage(pg.ID)
		}
		s.dispatchPageLifecycleHook(r.Context(), plugin.HookPageDeleted, pg, "deleted")
		writeOKJSON(w)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *AppServer) handleAdminCategories(w http.ResponseWriter, r *http.Request, actor *domain.User, rest string) {
	rest = strings.TrimPrefix(rest, "/")
	if rest == "reorder" && r.Method == http.MethodPost {
		if !requireAdminJSON(w, actor) {
			return
		}
		var req struct {
			OrderedIDs []string `json:"ordered_ids"`
			IDs        []string `json:"ids"`
		}
		if !decodeJSONBody(w, r, 65536, &req) {
			return
		}
		rawIDs := req.OrderedIDs
		if len(rawIDs) == 0 {
			rawIDs = req.IDs
		}
		ids := dedupCleanIDs(rawIDs, 500)
		if len(ids) == 0 {
			writeErrJSON(w, http.StatusBadRequest, "ordered_ids required")
			return
		}
		now := time.Now().UnixMilli()
		if err := s.deps.Store.WithTx(r.Context(), func(tx *sqlx.Tx) error {
			for idx, cid := range ids {
				if _, uErr := tx.ExecContext(r.Context(),
					`UPDATE categories SET sort_order = ?, updated_at = ? WHERE id = ?`,
					idx+1, now, cid); uErr != nil {
					return uErr
				}
			}
			return nil
		}); err != nil {
			writeErrJSON(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.recordAuditLog(r, actor, "category.reorder", "category", "reorder", map[string]any{"count": len(ids)})
		writeOKJSON(w)
		return
	}
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
			if !requireAdminJSON(w, actor) {
				return
			}
			var in domain.Category
			if !decodeJSONBody(w, r, 16384, &in) {
				return
			}
			in.Name = strings.TrimSpace(in.Name)
			if in.Name == "" {
				writeErrJSON(w, http.StatusBadRequest, "name is required")
				return
			}
			cleanIcon, ok := sanitizeAdminIconField(in.Icon)
			if !ok {
				writeErrJSON(w, http.StatusBadRequest, "invalid category icon")
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
				writeErrJSON(w, http.StatusBadRequest, err.Error())
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
		if !requireAdminJSON(w, actor) {
			return
		}
		var in domain.Category
		if !decodeJSONBody(w, r, 16384, &in) {
			return
		}
		cleanIcon, ok := sanitizeAdminIconField(in.Icon)
		if !ok {
			writeErrJSON(w, http.StatusBadRequest, "invalid category icon")
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
		if writeBadErr(w, err) {
			return
		}
		writeOKJSON(w)
	case http.MethodDelete:
		if !requireAdminJSON(w, actor) {
			return
		}
		_ = s.deps.Store.WithTx(r.Context(), func(tx *sqlx.Tx) error {
			_, _ = tx.ExecContext(r.Context(), `UPDATE articles SET category_id = '' WHERE category_id = ?`, rest)
			_, err := tx.ExecContext(r.Context(), `DELETE FROM categories WHERE id = ?`, rest)
			return err
		})
		writeOKJSON(w)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *AppServer) handleAdminTags(w http.ResponseWriter, r *http.Request, actor *domain.User, rest string) {
	rest = strings.TrimPrefix(rest, "/")
	if rest == "cleanup" && r.Method == http.MethodPost {
		if !requireAdminJSON(w, actor) {
			return
		}
		res, err := s.deps.Store.ExecContext(r.Context(),
			`DELETE FROM tags WHERE id NOT IN (SELECT DISTINCT tag_id FROM article_tags)`)
		if writeServerErr(w, err) {
			return
		}
		deleted, _ := res.RowsAffected()
		s.recordAuditLog(r, actor, "tags.cleanup", "tag", "unused", map[string]any{"deleted": deleted})
		rows, _ := s.deps.Content.ListTagsWithCounts(r.Context(), 500)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "deleted": deleted, "items": rows, "tags": rows})
		return
	}
	if rest == "merge" && r.Method == http.MethodPost {
		if !requireAdminJSON(w, actor) {
			return
		}
		var req struct {
			SourceID  string   `json:"source_id"`
			SourceIDs []string `json:"source_ids"`
			TargetID  string   `json:"target_id"`
		}
		if !decodeJSONBody(w, r, 65536, &req) {
			return
		}
		targetID := strings.TrimSpace(req.TargetID)
		if targetID == "" {
			writeErrJSON(w, http.StatusBadRequest, "target_id is required")
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
			writeErrJSON(w, http.StatusBadRequest, "source_ids or source_id is required and must differ from target_id")
			return
		}
		var targetExists int
		_ = s.deps.Store.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM tags WHERE id = ?`, targetID).Scan(&targetExists)
		if targetExists == 0 {
			writeErrJSON(w, http.StatusNotFound, "target tag not found")
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
		if writeBadErr(w, err) {
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
			qSQL, fullArgs := s.paginateQuery(baseSQL, args, page, perPage)

			type adminTagRow struct {
				ID           string `db:"id" json:"id"`
				Name         string `db:"name" json:"name"`
				Slug         string `db:"slug" json:"slug"`
				CreatedAt    int64  `db:"created_at" json:"created_at"`
				ArticleCount int    `db:"article_count" json:"article_count"`
			}
			var paged []adminTagRow
			_ = sqlx.SelectInto(r.Context(), s.deps.Store, &paged, qSQL, fullArgs...)
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
			if !requireAdminJSON(w, actor) {
				return
			}
			var in domain.Tag
			if !decodeJSONBody(w, r, 8192, &in) {
				return
			}
			in.Name = strings.TrimSpace(in.Name)
			if in.Name == "" {
				writeErrJSON(w, http.StatusBadRequest, "name required")
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
				writeErrJSON(w, http.StatusBadRequest, err.Error())
				return
			}
			writeJSON(w, http.StatusCreated, map[string]any{"tag": in})
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
		return
	}
	if rest == "batch" && r.Method == http.MethodPost {
		if !requireAdminJSON(w, actor) {
			return
		}
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
		if writeBadErr(w, err) {
			return
		}
		s.recordAuditLog(r, actor, "tag.batch_delete", "tag", "batch", map[string]any{"affected": affected})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "affected": affected})
		return
	}
	switch r.Method {
	case http.MethodPut:
		if !requireAdminJSON(w, actor) {
			return
		}
		var in domain.Tag
		if !decodeJSONBody(w, r, 8192, &in) {
			return
		}
		name := strings.TrimSpace(in.Name)
		if name == "" {
			writeErrJSON(w, http.StatusBadRequest, "name required")
			return
		}
		slug := sanitizeSlug(in.Slug)
		if slug == "" {
			slug = sanitizeSlug(name)
		}
		if _, err := s.deps.Store.ExecContext(r.Context(),
			`UPDATE tags SET name = ?, slug = ? WHERE id = ?`, name, slug, rest); err != nil {
			writeErrJSON(w, http.StatusBadRequest, err.Error())
			return
		}
		writeOKJSON(w)
	case http.MethodDelete:
		if !requireAdminJSON(w, actor) {
			return
		}
		_ = s.deps.Store.WithTx(r.Context(), func(tx *sqlx.Tx) error {
			_, _ = tx.ExecContext(r.Context(), `DELETE FROM article_tags WHERE tag_id = ?`, rest)
			_, err := tx.ExecContext(r.Context(), `DELETE FROM tags WHERE id = ?`, rest)
			return err
		})
		writeOKJSON(w)
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
			qSQL, fullArgs := s.paginateQuery(baseSQL, args, page, perPage)
			var rows []domain.Series
			_ = sqlx.SelectInto(r.Context(), s.deps.Store, &rows, qSQL, fullArgs...)

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
				var rels []struct {
					SeriesID  string `db:"series_id"`
					ArticleID string `db:"article_id"`
					SortOrder int    `db:"sort_order"`
					Title     string `db:"title"`
					Slug      string `db:"slug"`
					Status    string `db:"status"`
				}
				_ = sqlx.SelectInto(r.Context(), s.deps.Store, &rels,
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
			if !requireAdminJSON(w, actor) {
				return
			}
			var in struct {
				domain.Series
				ArticleIDs []string `json:"article_ids"`
			}
			if !decodeJSONBody(w, r, 32768, &in) {
				return
			}
			in.Title = strings.TrimSpace(in.Title)
			if in.Title == "" {
				writeErrJSON(w, http.StatusBadRequest, "title is required")
				return
			}
			if rawCov := strings.TrimSpace(in.CoverURL); rawCov != "" {
				u, ok := sanitizeRedirectTargetURL(rawCov)
				if !ok {
					writeErrJSON(w, http.StatusBadRequest, "invalid cover_url")
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
			if writeBadErr(w, err) {
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
		if !requireAdminJSON(w, actor) {
			return
		}
		var in struct {
			domain.Series
			ArticleIDs []string `json:"article_ids"`
		}
		if !decodeJSONBody(w, r, 32768, &in) {
			return
		}
		cleanCov := ""
		if rawCov := strings.TrimSpace(in.CoverURL); rawCov != "" {
			u, ok := sanitizeRedirectTargetURL(rawCov)
			if !ok {
				writeErrJSON(w, http.StatusBadRequest, "invalid cover_url")
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
		if writeBadErr(w, err) {
			return
		}
		s.invalidateMediaRefCache()
		writeOKJSON(w)
	case http.MethodDelete:
		if !requireAdminJSON(w, actor) {
			return
		}
		_ = s.deps.Store.WithTx(r.Context(), func(tx *sqlx.Tx) error {
			_, _ = tx.ExecContext(r.Context(), `DELETE FROM series_articles WHERE series_id = ?`, rest)
			_, err := tx.ExecContext(r.Context(), `DELETE FROM series WHERE id = ?`, rest)
			return err
		})
		s.invalidateMediaRefCache()
		writeOKJSON(w)
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
	if rest == "reorder" && r.Method == http.MethodPost {
		if !requireAdminJSON(w, actor) {
			return
		}
		var req struct {
			OrderedIDs []string `json:"ordered_ids"`
			IDs        []string `json:"ids"`
		}
		if !decodeJSONBody(w, r, 65536, &req) {
			return
		}
		rawIDs := req.OrderedIDs
		if len(rawIDs) == 0 {
			rawIDs = req.IDs
		}
		ids := dedupCleanIDs(rawIDs, 500)
		if len(ids) == 0 {
			writeErrJSON(w, http.StatusBadRequest, "ordered_ids required")
			return
		}
		now := time.Now().UnixMilli()
		if err := s.deps.Store.WithTx(r.Context(), func(tx *sqlx.Tx) error {
			for idx, bid := range ids {
				if _, uErr := tx.ExecContext(r.Context(),
					`UPDATE books SET sort_order = ?, updated_at = ? WHERE id = ?`,
					idx+1, now, bid); uErr != nil {
					return uErr
				}
			}
			return nil
		}); err != nil {
			writeErrJSON(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.recordAuditLog(r, actor, "book.reorder", "book", "reorder", map[string]any{"count": len(ids)})
		writeOKJSON(w)
		return
	}
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
			qSQL, fullArgs := s.paginateQuery(baseSQL, args, page, perPage)
			var paged []domain.Book
			_ = sqlx.SelectInto(r.Context(), s.deps.Store, &paged, qSQL, fullArgs...)
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
			if !requireAdminJSON(w, actor) {
				return
			}
			var in struct {
				domain.Book
				Chapters []domain.BookChapter `json:"chapters"`
			}
			if !decodeJSONBody(w, r, 2<<20, &in) {
				return
			}
			in.Title = strings.TrimSpace(in.Title)
			if in.Title == "" {
				writeErrJSON(w, http.StatusBadRequest, "title is required")
				return
			}
			if writeBadErr(w, sanitizeBookURLs(&in.Book, in.Chapters)) {
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
						`INSERT INTO book_chapters (id, book_id, parent_id, title, url, depth, sort_order) VALUES (?, ?, ?, ?, ?, ?, ?)`,
						id.Next(), in.ID, strings.TrimSpace(ch.ParentID), strings.TrimSpace(ch.Title), ch.URL, ch.Depth, idx+1)
				}
				return nil
			})
			if writeBadErr(w, err) {
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
		if !requireAdminJSON(w, actor) {
			return
		}
		cnt, err := s.deps.Content.SyncBookTOC(r.Context(), bookID)
		if writeBadErr(w, err) {
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"chapter_count": cnt})
		return
	}

	switch r.Method {
	case http.MethodGet:
		var bk domain.Book
		err := sqlx.GetInto(r.Context(), s.deps.Store, &bk, `SELECT * FROM books WHERE id = ?`, bookID)
		if err != nil {
			writeErrJSON(w, http.StatusNotFound, "book not found")
			return
		}
		var chs []domain.BookChapter
		_ = sqlx.SelectInto(r.Context(), s.deps.Store, &chs,
			`SELECT * FROM book_chapters WHERE book_id = ? ORDER BY sort_order ASC`, bookID)
		writeJSON(w, http.StatusOK, map[string]any{"book": bk, "chapters": chs})

	case http.MethodPut:
		if !requireAdminJSON(w, actor) {
			return
		}
		var in struct {
			domain.Book
			Chapters []domain.BookChapter `json:"chapters"`
		}
		if !decodeJSONBody(w, r, 2<<20, &in) {
			return
		}
		if writeBadErr(w, sanitizeBookURLs(&in.Book, in.Chapters)) {
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
						`INSERT INTO book_chapters (id, book_id, parent_id, title, url, depth, sort_order) VALUES (?, ?, ?, ?, ?, ?, ?)`,
						id.Next(), bookID, strings.TrimSpace(ch.ParentID), strings.TrimSpace(ch.Title), ch.URL, ch.Depth, idx+1)
				}
				_, _ = tx.ExecContext(r.Context(), `UPDATE books SET chapter_count = ? WHERE id = ?`, len(in.Chapters), bookID)
			}
			return nil
		})
		if writeBadErr(w, err) {
			return
		}
		s.invalidateMediaRefCache()
		writeOKJSON(w)

	case http.MethodDelete:
		if !requireAdminJSON(w, actor) {
			return
		}
		_ = s.deps.Store.WithTx(r.Context(), func(tx *sqlx.Tx) error {
			_, _ = tx.ExecContext(r.Context(), `DELETE FROM book_chapters WHERE book_id = ?`, bookID)
			_, err := tx.ExecContext(r.Context(), `DELETE FROM books WHERE id = ?`, bookID)
			return err
		})
		s.invalidateMediaRefCache()
		writeOKJSON(w)

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
	if status == domain.CommentStatusApproved && cmt.Status != domain.CommentStatusApproved &&
		strings.TrimSpace(cmt.ParentID) != "" && s.deps.NotifyCommentApproved != nil {
		approvedCopy := cmt
		approvedCopy.Status = domain.CommentStatusApproved
		go s.deps.NotifyCommentApproved(context.Background(), approvedCopy)
	}
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
		if !requireMethod(w, r, http.MethodGet) {
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
		// Trash scope: soft-deleted rows that still carry restorable content (purge placeholders are hidden).
		trashWhere := "c.is_deleted = ? AND c.content_md <> ''"
		trashArgs := []any{true}
		if actor.Role != domain.RoleAdmin {
			scopeWhere = "c.is_deleted = ? AND (a.author_id = ? OR p.author_id = ?)"
			scopeArgs = append(scopeArgs, actor.ID, actor.ID)
			trashWhere += " AND (a.author_id = ? OR p.author_id = ?)"
			trashArgs = append(trashArgs, actor.ID, actor.ID)
		}

		var cntAll, cntPend, cntApp, cntRej, cntTrash int
		_ = s.deps.Store.QueryRowContext(r.Context(),
			`SELECT COUNT(*),
			        COALESCE(SUM(CASE WHEN c.status = 'pending' THEN 1 ELSE 0 END), 0),
			        COALESCE(SUM(CASE WHEN c.status = 'approved' THEN 1 ELSE 0 END), 0),
			        COALESCE(SUM(CASE WHEN c.status = 'rejected' THEN 1 ELSE 0 END), 0)
			 `+joinSQL+` WHERE `+scopeWhere, scopeArgs...).Scan(&cntAll, &cntPend, &cntApp, &cntRej)
		_ = s.deps.Store.QueryRowContext(r.Context(), `SELECT COUNT(*) `+joinSQL+` WHERE `+trashWhere, trashArgs...).Scan(&cntTrash)
		statusCounts := map[string]int{
			"all":      cntAll,
			"pending":  cntPend,
			"approved": cntApp,
			"rejected": cntRej,
			"trash":    cntTrash,
		}

		trashMode := status == "trash"
		where := []string{scopeWhere}
		args := append([]any{}, scopeArgs...)
		if trashMode {
			where = []string{trashWhere}
			args = append([]any{}, trashArgs...)
		} else if status != "" && status != "all" {
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

		orderSQL := ` ORDER BY c.created_at DESC`
		if trashMode {
			orderSQL = ` ORDER BY c.updated_at DESC, c.id DESC`
		}
		baseSQL := `SELECT c.* ` + joinSQL + ` WHERE ` + whereSQL + orderSQL
		qSQL, fullArgs := s.paginateQuery(baseSQL, args, page, perPage)
		var rows []domain.Comment
		err := sqlx.SelectInto(r.Context(), s.deps.Store, &rows, qSQL, fullArgs...)
		if writeServerErr(w, err) {
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
			var aRows []struct {
				ID       string `db:"id"`
				AuthorID string `db:"author_id"`
				Title    string `db:"title"`
				Slug     string `db:"slug"`
			}
			_ = sqlx.SelectInto(r.Context(), s.deps.Store, &aRows, `SELECT id, author_id, title, slug FROM articles WHERE id IN (`+strings.Join(artPH, ",")+`)`, artIDs...)
			for _, ar := range aRows {
				artMeta[ar.ID] = targetMeta{AuthorID: ar.AuthorID, Title: ar.Title, Slug: ar.Slug}
			}
		}
		if len(pageIDs) > 0 {
			var pRows []struct {
				ID       string `db:"id"`
				AuthorID string `db:"author_id"`
				Title    string `db:"title"`
				Slug     string `db:"slug"`
			}
			_ = sqlx.SelectInto(r.Context(), s.deps.Store, &pRows, `SELECT id, author_id, title, slug FROM pages WHERE id IN (`+strings.Join(pagePH, ",")+`)`, pageIDs...)
			for _, pr := range pRows {
				pageMeta[pr.ID] = targetMeta{AuthorID: pr.AuthorID, Title: pr.Title, Slug: pr.Slug}
			}
		}

		userModMap := make(map[string]domain.User)
		{
			userIDSet := make(map[string]struct{})
			var userArgs []any
			var userPH []string
			for _, c := range rows {
				uid := strings.TrimSpace(c.UserID)
				if uid == "" {
					continue
				}
				if _, exists := userIDSet[uid]; !exists {
					userIDSet[uid] = struct{}{}
					userArgs = append(userArgs, uid)
					userPH = append(userPH, "?")
				}
			}
			if len(userArgs) > 0 {
				var uRows []domain.User
				_ = sqlx.SelectInto(r.Context(), s.deps.Store, &uRows,
					`SELECT id, role, status, ban_until, muted_until, ban_reason, shield_comments, shield_text FROM users WHERE id IN (`+strings.Join(userPH, ",")+`)`,
					userArgs...)
				for i := range uRows {
					if s.deps.Auth != nil {
						s.deps.Auth.HealExpiredUser(r.Context(), &uRows[i])
					}
					userModMap[uRows[i].ID] = uRows[i]
				}
			}
		}

		type adminCommentView struct {
			domain.Comment
			TargetTitle        string `json:"target_title"`
			TargetSlug         string `json:"target_slug"`
			UserRole           string `json:"user_role,omitempty"`
			UserStatus         string `json:"user_status,omitempty"`
			UserBanUntil       int64  `json:"user_ban_until,omitempty"`
			UserMutedUntil     int64  `json:"user_muted_until,omitempty"`
			UserBanReason      string `json:"user_ban_reason,omitempty"`
			UserShieldComments bool   `json:"user_shield_comments,omitempty"`
			UserShieldText     string `json:"user_shield_text,omitempty"`
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
				view := adminCommentView{
					Comment:     scrubbed,
					TargetTitle: tm.Title,
					TargetSlug:  tm.Slug,
				}
				if uMod, ok := userModMap[strings.TrimSpace(c.UserID)]; ok {
					view.UserRole = uMod.Role
					view.UserStatus = uMod.Status
					view.UserBanUntil = uMod.BanUntil
					view.UserMutedUntil = uMod.MutedUntil
					view.UserBanReason = uMod.BanReason
					view.UserShieldComments = uMod.ShieldComments
					view.UserShieldText = uMod.ShieldText
				}
				items = append(items, view)
			}
		}
		trashRetentionDays := domain.DefaultCommentTrashRetentionDays
		if s.deps.Settings != nil {
			trashRetentionDays = s.deps.Settings.Snapshot().Reading.CommentTrashRetentionDays
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"items":                items,
			"comments":             items,
			"total":                total,
			"page":                 page,
			"per_page":             perPage,
			"page_size":            perPage,
			"status_counts":        statusCounts,
			"trash_retention_days": trashRetentionDays,
		})
		return
	}

	if rest == "trash/empty" {
		if !requireMethod(w, r, http.MethodPost) || !requireAdminJSON(w, actor) {
			return
		}
		purged, kept, err := s.deps.Content.EmptyCommentTrash(r.Context())
		if writeServerErr(w, err) {
			return
		}
		s.recordAuditLog(r, actor, "comment.trash_empty", "comment", "trash", map[string]any{"purged": purged, "kept_placeholders": kept})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "purged": purged, "kept_placeholders": kept})
		return
	}

	if rest == "batch" && r.Method == http.MethodPost {
		s.handleAdminCommentsBatch(w, r, actor)
		return
	}

	parts := strings.Split(rest, "/")
	cmtID := parts[0]
	var cmt domain.Comment
	err := sqlx.GetInto(r.Context(), s.deps.Store, &cmt, `SELECT * FROM comments WHERE id = ?`, cmtID)
	if err != nil {
		writeErrJSON(w, http.StatusNotFound, "comment not found")
		return
	}
	targetAuthorID := s.lookupCommentTargetAuthorID(r.Context(), cmt.TargetType, cmt.TargetID)
	if _, authErr := authorizeAndScrubAdminComment(actor, cmt, targetAuthorID); authErr != nil {
		writeErrJSON(w, http.StatusForbidden, authErr.Error())
		return
	}

	if len(parts) == 2 && parts[1] == "reply" && r.Method == http.MethodPost {
		var req struct {
			ContentMD string `json:"content_md"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32768)).Decode(&req); err != nil || strings.TrimSpace(req.ContentMD) == "" {
			writeErrJSON(w, http.StatusBadRequest, "content_md required")
			return
		}
		clientIP := s.clientIP(r)
		in := s.commentInputFromStaff(actor, cmt, req.ContentMD, clientIP.String())
		view, _, cErr := s.deps.Content.CreateComment(r.Context(), w, r, actor, in)
		if cErr != nil {
			writeErrJSON(w, http.StatusBadRequest, cErr.Error())
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
		if !requireAdminJSON(w, actor) {
			return
		}
		targetIP := strings.TrimSpace(cmt.ClientIP)
		if targetIP == "" {
			writeErrJSON(w, http.StatusBadRequest, "comment has no recorded client IP")
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
		callerIP := s.clientIP(r)
		rec, bErr := s.deps.WAF.AddManualBan(r.Context(), targetIP, note, expMS, callerIP)
		if bErr != nil {
			writeErrJSON(w, http.StatusBadRequest, bErr.Error())
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

	if len(parts) == 2 && parts[1] == "restore" {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		restored, rErr := s.deps.Content.RestoreComment(r.Context(), cmt.ID)
		if errors.Is(rErr, content.ErrCommentNotInTrash) {
			writeErrJSON(w, http.StatusConflict, "comment is not in trash")
			return
		}
		if writeServerErr(w, rErr) {
			return
		}
		s.recordAuditLog(r, actor, "comment.restore", "comment", cmt.ID, map[string]any{
			"target_type": cmt.TargetType, "target_id": cmt.TargetID, "promoted_to_root": restored.ParentID != cmt.ParentID,
		})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "parent_id": restored.ParentID})
		return
	}
	if len(parts) != 1 {
		writeErrJSON(w, http.StatusNotFound, "not found")
		return
	}

	switch r.Method {
	case http.MethodPatch:
		var req struct {
			Status     *string `json:"status"`
			ContentMD  *string `json:"content_md"`
			AuthorName *string `json:"author_name"`
			AvatarURL  *string `json:"avatar_url"`
			AuthorURL  *string `json:"author_url"`
		}
		if !decodeJSONBody(w, r, 32768, &req) {
			return
		}
		identityPatch := content.CommentIdentityPatch{AuthorName: req.AuthorName, AvatarURL: req.AvatarURL, AuthorURL: req.AuthorURL}
		if req.Status == nil && req.ContentMD == nil && identityPatch.Empty() {
			writeErrJSON(w, http.StatusBadRequest, "status, content_md or author fields required")
			return
		}
		identity, idErr := content.NormalizeCommentIdentityPatch(identityPatch)
		if idErr != nil {
			writeErrJSON(w, http.StatusBadRequest, idErr.Error())
			return
		}
		nextStatus := cmt.Status
		if req.Status != nil && strings.TrimSpace(*req.Status) != "" {
			sVal := strings.TrimSpace(*req.Status)
			switch sVal {
			case domain.CommentStatusApproved, domain.CommentStatusPending, domain.CommentStatusRejected:
				nextStatus = sVal
			default:
				writeErrJSON(w, http.StatusBadRequest, "invalid comment status")
				return
			}
		}
		nextContent := cmt.ContentMD
		if req.ContentMD != nil {
			trimmed := strings.TrimSpace(*req.ContentMD)
			if trimmed == "" {
				writeErrJSON(w, http.StatusBadRequest, "content_md required")
				return
			}
			if len([]rune(trimmed)) > 8000 {
				trimmed = string([]rune(trimmed)[:8000])
			}
			nextContent = trimmed
		}
		nowMS := time.Now().UnixMilli()
		identityChanged := false
		err := s.deps.Store.WithTx(r.Context(), func(tx *sqlx.Tx) error {
			if nextStatus != cmt.Status || nextContent != cmt.ContentMD {
				if _, uErr := tx.ExecContext(r.Context(),
					`UPDATE comments SET status = ?, content_md = ?, updated_at = ? WHERE id = ?`,
					nextStatus, nextContent, nowMS, cmtID); uErr != nil {
					return uErr
				}
			}
			changed, aErr := content.ApplyCommentIdentityTx(r.Context(), tx, cmt, identity, nowMS)
			identityChanged = changed
			if aErr != nil {
				return aErr
			}
			return s.recountTargetCommentsTx(r.Context(), tx, cmt.TargetType, cmt.TargetID)
		})
		if writeServerErr(w, err) {
			return
		}
		if nextStatus != cmt.Status {
			cmtMod := cmt
			cmtMod.ContentMD = nextContent
			s.dispatchCommentModeratedHook(r.Context(), cmtMod, nextStatus)
		}
		if identityChanged {
			detail := map[string]any{"target_type": cmt.TargetType, "target_id": cmt.TargetID}
			if identity.AuthorName != nil {
				detail["author_name"] = *identity.AuthorName
			}
			if identity.AvatarURL != nil {
				detail["avatar_url"] = *identity.AvatarURL
			}
			if identity.AuthorURL != nil {
				detail["author_url"] = *identity.AuthorURL
			}
			s.recordAuditLog(r, actor, "comment.identity_update", "comment", cmt.ID, detail)
		}
		writeOKJSON(w)

	case http.MethodDelete:
		purge := r.URL.Query().Get("purge")
		if purge == "1" || strings.EqualFold(purge, "true") {
			if !cmt.IsDeleted {
				writeErrJSON(w, http.StatusConflict, "comment is not in trash")
				return
			}
			kept, pErr := s.deps.Content.PurgeComment(r.Context(), cmt.ID)
			if writeServerErr(w, pErr) {
				return
			}
			s.recordAuditLog(r, actor, "comment.purge", "comment", cmt.ID, map[string]any{
				"target_type": cmt.TargetType, "target_id": cmt.TargetID, "kept_placeholder": kept,
			})
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "kept_placeholder": kept})
			return
		}
		if !cmt.IsDeleted {
			err := s.deps.Store.WithTx(r.Context(), func(tx *sqlx.Tx) error {
				if dErr := content.SoftDeleteCommentTx(r.Context(), tx, cmt.ID, time.Now().UnixMilli()); dErr != nil {
					return dErr
				}
				return s.recountTargetCommentsTx(r.Context(), tx, cmt.TargetType, cmt.TargetID)
			})
			if writeServerErr(w, err) {
				return
			}
			s.recordAuditLog(r, actor, "comment.delete", "comment", cmt.ID, map[string]any{
				"target_type": cmt.TargetType, "target_id": cmt.TargetID,
			})
		}
		writeOKJSON(w)

	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *AppServer) handleAdminCommentsBatch(w http.ResponseWriter, r *http.Request, actor *domain.User) {
	var req struct {
		Action       string   `json:"action"`
		IDs          []string `json:"ids"`
		Note         string   `json:"note"`
		ExpiresHours int      `json:"expires_hours"`
	}
	if !decodeJSONBody(w, r, 65536, &req) {
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
	case "restore":
		action = "restore"
	case "purge":
		action = "purge"
	case "ban_ip", "ban-ip":
		if !requireAdminJSON(w, actor) {
			return
		}
		action = "ban_ip"
	default:
		writeErrJSON(w, http.StatusBadRequest, "invalid batch action")
		return
	}
	ids := dedupCleanIDs(req.IDs, 200)
	if len(ids) == 0 {
		writeErrJSON(w, http.StatusBadRequest, "ids required")
		return
	}

	ctx := r.Context()
	ph := strings.Repeat("?,", len(ids)-1) + "?"
	idArgs := make([]any, len(ids))
	for i, cid := range ids {
		idArgs[i] = cid
	}
	var fetched []domain.Comment
	if err := sqlx.SelectInto(ctx, s.deps.Store, &fetched, `SELECT * FROM comments WHERE id IN (`+ph+`)`, idArgs...); writeServerErr(w, err) {
		return
	}
	byID := make(map[string]domain.Comment, len(fetched))
	artIDSet := make(map[string]struct{})
	pageIDSet := make(map[string]struct{})
	var artArgs, pageArgs []any
	for _, cmt := range fetched {
		byID[cmt.ID] = cmt
		if cmt.TargetID == "" {
			continue
		}
		if cmt.TargetType == domain.TargetTypePage {
			if _, ok := pageIDSet[cmt.TargetID]; !ok {
				pageIDSet[cmt.TargetID] = struct{}{}
				pageArgs = append(pageArgs, cmt.TargetID)
			}
		} else if cmt.TargetType == domain.TargetTypeArticle {
			if _, ok := artIDSet[cmt.TargetID]; !ok {
				artIDSet[cmt.TargetID] = struct{}{}
				artArgs = append(artArgs, cmt.TargetID)
			}
		}
	}

	type targetOwnerRow struct {
		ID       string `db:"id"`
		AuthorID string `db:"author_id"`
	}
	artOwners := make(map[string]string, len(artArgs))
	if len(artArgs) > 0 {
		tph := strings.Repeat("?,", len(artArgs)-1) + "?"
		var rows []targetOwnerRow
		_ = sqlx.SelectInto(ctx, s.deps.Store, &rows, `SELECT id, author_id FROM articles WHERE id IN (`+tph+`)`, artArgs...)
		for _, row := range rows {
			artOwners[row.ID] = row.AuthorID
		}
	}
	pageOwners := make(map[string]string, len(pageArgs))
	if len(pageArgs) > 0 {
		tph := strings.Repeat("?,", len(pageArgs)-1) + "?"
		var rows []targetOwnerRow
		_ = sqlx.SelectInto(ctx, s.deps.Store, &rows, `SELECT id, author_id FROM pages WHERE id IN (`+tph+`)`, pageArgs...)
		for _, row := range rows {
			pageOwners[row.ID] = row.AuthorID
		}
	}

	comments := make([]domain.Comment, 0, len(fetched))
	for _, cid := range ids {
		cmt, ok := byID[cid]
		if !ok {
			continue
		}
		var targetAuthorID string
		if cmt.TargetType == domain.TargetTypePage {
			targetAuthorID = pageOwners[cmt.TargetID]
		} else if cmt.TargetType == domain.TargetTypeArticle {
			targetAuthorID = artOwners[cmt.TargetID]
		}
		if _, authErr := authorizeAndScrubAdminComment(actor, cmt, targetAuthorID); authErr != nil {
			writeErrJSON(w, http.StatusForbidden, authErr.Error())
			return
		}
		comments = append(comments, cmt)
	}

	now := time.Now().UnixMilli()
	callerIP := s.clientIP(r)
	if action == "ban_ip" && s.deps.WAF != nil {
		hours := req.ExpiresHours
		if hours <= 0 {
			hours = 30 * 24
		}
		expMS := time.Now().Add(time.Duration(hours) * time.Hour).UnixMilli()
		for _, cmt := range comments {
			targetIP := strings.TrimSpace(cmt.ClientIP)
			if targetIP == "" {
				continue
			}
			note := strings.TrimSpace(req.Note)
			if note == "" {
				note = "comment batch moderation (" + cmt.AuthorName + ")"
			}
			_, _ = s.deps.WAF.AddManualBan(ctx, targetIP, note, expMS, callerIP)
		}
	}

	type modHookItem struct {
		cmt        domain.Comment
		nextStatus string
	}
	type targetPair struct {
		tType string
		tID   string
	}
	recountSet := make(map[targetPair]struct{})
	var hooks []modHookItem
	affected := 0
	keptPlaceholders := 0

	if len(comments) > 0 {
		txErr := s.deps.Store.WithTx(ctx, func(tx *sqlx.Tx) error {
			affected, keptPlaceholders = 0, 0
			hooks = hooks[:0]
			for _, cmt := range comments {
				var uErr error
				nextModStatus := ""
				switch action {
				case "approve":
					nextModStatus = domain.CommentStatusApproved
					_, uErr = tx.ExecContext(ctx,
						`UPDATE comments SET status = ?, updated_at = ? WHERE id = ?`,
						domain.CommentStatusApproved, now, cmt.ID)
				case "reject", "ban_ip":
					nextModStatus = domain.CommentStatusRejected
					_, uErr = tx.ExecContext(ctx,
						`UPDATE comments SET status = ?, updated_at = ? WHERE id = ?`,
						domain.CommentStatusRejected, now, cmt.ID)
				case "pending":
					nextModStatus = domain.CommentStatusPending
					_, uErr = tx.ExecContext(ctx,
						`UPDATE comments SET status = ?, updated_at = ? WHERE id = ?`,
						domain.CommentStatusPending, now, cmt.ID)
				case "delete":
					if cmt.IsDeleted {
						continue
					}
					uErr = content.SoftDeleteCommentTx(ctx, tx, cmt.ID, now)
				case "restore":
					if _, rErr := content.RestoreCommentTx(ctx, tx, cmt, now); rErr != nil {
						if errors.Is(rErr, content.ErrCommentNotInTrash) {
							continue
						}
						uErr = rErr
					}
				case "purge":
					if !cmt.IsDeleted {
						continue
					}
					_, k, pErr := content.PurgeTrashedCommentsTx(ctx, tx, []domain.Comment{cmt})
					keptPlaceholders += k
					uErr = pErr
				}
				if uErr != nil {
					return uErr
				}
				affected++
				recountSet[targetPair{tType: cmt.TargetType, tID: cmt.TargetID}] = struct{}{}
				if nextModStatus != "" && nextModStatus != cmt.Status {
					hooks = append(hooks, modHookItem{cmt: cmt, nextStatus: nextModStatus})
				}
			}
			for tp := range recountSet {
				if rErr := s.recountTargetCommentsTx(ctx, tx, tp.tType, tp.tID); rErr != nil {
					return rErr
				}
			}
			return nil
		})
		if writeServerErr(w, txErr) {
			return
		}
	}

	for _, h := range hooks {
		s.dispatchCommentModeratedHook(ctx, h.cmt, h.nextStatus)
	}
	detail := map[string]any{"action": action, "affected": affected}
	resp := map[string]any{"ok": true, "affected": affected}
	if action == "purge" {
		detail["kept_placeholders"] = keptPlaceholders
		resp["kept_placeholders"] = keptPlaceholders
	}
	s.recordAuditLog(r, actor, "comment.batch", "comment", action, detail)
	writeJSON(w, http.StatusOK, resp)
}

func (s *AppServer) recountTargetCommentsTx(ctx context.Context, tx *sqlx.Tx, targetType, targetID string) error {
	var cnt int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM comments WHERE target_type = ? AND target_id = ? AND status = 'approved' AND is_deleted = ?`,
		targetType, targetID, false).Scan(&cnt); err != nil {
		return err
	}
	if targetType == domain.TargetTypePage {
		_, err := tx.ExecContext(ctx, `UPDATE pages SET comment_count = ? WHERE id = ?`, cnt, targetID)
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE articles SET comment_count = ? WHERE id = ?`, cnt, targetID)
	return err
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
		if !requireAdminJSON(w, actor) {
			return
		}
		effectiveDir := ""
		if s.deps.Config != nil {
			effectiveDir = strings.TrimSpace(s.deps.Config.GetUploadDir())
		}
		if effectiveDir != "" {
			if absDir, _, rwErr := probeAndDescribeLocalUploadDir(effectiveDir); rwErr != nil {
				writeErrJSON(w, http.StatusBadRequest, formatUploadDirAccessError(absDir, rwErr))
				return
			} else {
				effectiveDir = absDir
			}
		}
		imported := s.reconcileLocalUploads(r.Context())
		if imported > 0 {
			_ = s.deps.Content.RefreshMediaDimsCache(r.Context())
			s.invalidateMediaRefCache()
		}
		s.recordAuditLog(r, actor, "media.reconcile", "media_asset", "local", map[string]any{"imported": imported, "effective_dir": effectiveDir})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "imported": imported, "effective_dir": effectiveDir})
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
		assets := make([]domain.MediaAsset, 0, len(ids))
		for _, mid := range ids {
			var asset domain.MediaAsset
			gErr := sqlx.GetInto(r.Context(), s.deps.Store, &asset, `SELECT * FROM media_assets WHERE id = ?`, mid)
			if gErr != nil {
				continue
			}
			if actor.Role != domain.RoleAdmin && asset.UploaderID != actor.ID {
				writeErrJSON(w, http.StatusForbidden, "forbidden")
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
				if asset.StorageDriver == "local" && asset.ObjectKey != "" && s.deps.Config != nil {
					curDir := s.deps.Config.GetUploadDir()
					_ = safeUnlinkLocalAsset(curDir, asset.ObjectKey)
					if defDir := s.deps.Config.GetDefaultUploadDir(); defDir != "" && defDir != curDir {
						_ = safeUnlinkLocalAsset(defDir, asset.ObjectKey)
					}
				}
				if _, dErr := s.deps.Store.ExecContext(r.Context(), `DELETE FROM media_assets WHERE id = ?`, asset.ID); dErr == nil {
					affected++
				}
			}
		}
		if affected > 0 {
			_ = s.deps.Content.RefreshMediaDimsCache(r.Context())
			s.invalidateMediaRefCache()
			if s.imageView != nil {
				s.imageView.Purge()
			}
		}
		s.recordAuditLog(r, actor, "media.batch_delete", "media_asset", "batch", map[string]any{"affected": affected})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "affected": affected})
		return
	}

	if (rest == "upload" || rest == "") && r.Method == http.MethodPost {
		maxMB := s.deps.Settings.Snapshot().Storage.MaxUploadMB
		if maxMB <= 0 {
			maxMB = 25
		}
		maxBytes := int64(maxMB) << 20
		r.Body = http.MaxBytesReader(w, r.Body, maxBytes+1024*1024)
		if err := r.ParseMultipartForm(8 << 20); err != nil {
			writeErrJSON(w, http.StatusBadRequest, "upload exceeds maximum size")
			return
		}
		file, hdr, err := r.FormFile("file")
		if err != nil {
			writeErrJSON(w, http.StatusBadRequest, "missing 'file' field")
			return
		}
		defer file.Close()

		uploadFilename := hdr.Filename
		uploadContentType := hdr.Header.Get("Content-Type")
		if s.deps.Plugins != nil {
			res, hErr := s.deps.Plugins.DispatchHook(r.Context(), plugin.HookMediaBeforeUpload, map[string]any{
				"uploader_id":   actor.ID,
				"uploader_role": actor.Role,
				"filename":      uploadFilename,
				"content_type":  uploadContentType,
				"size_bytes":    hdr.Size,
			})
			if hErr != nil {
				var rej *plugin.HookRejectedError
				if errors.As(hErr, &rej) {
					writeHookRejectedJSON(w, rej)
					return
				}
			} else if outMap, ok := res.(map[string]any); ok {
				if nextName, sOk := outMap["filename"].(string); sOk && strings.TrimSpace(nextName) != "" {
					uploadFilename = strings.TrimSpace(nextName)
				}
			}
		}

		var asset *domain.MediaAsset
		if s.deps.Media != nil {
			asset, err = s.deps.Media.Upload(r.Context(), actor.ID, uploadFilename, uploadContentType, file)
		} else {
			asset, err = s.saveFallbackLocalMedia(r.Context(), actor.ID, uploadFilename, file, maxBytes)
		}
		if writeBadErr(w, err) {
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
		if !requireAdminJSON(w, actor) {
			return
		}
		clean := r.URL.Query().Get("delete") == "1" || r.URL.Query().Get("delete") == "true"
		orphans, freed, err := s.scanOrCleanOrphans(r.Context(), clean)
		if writeServerErr(w, err) {
			return
		}
		deletedCnt := 0
		if clean {
			deletedCnt = len(orphans)
			s.invalidateMediaRefCache()
			if s.imageView != nil {
				s.imageView.Purge()
			}
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
		var asset domain.MediaAsset
		err := sqlx.GetInto(r.Context(), s.deps.Store, &asset, `SELECT * FROM media_assets WHERE id = ?`, rest)
		if err != nil {
			writeErrJSON(w, http.StatusNotFound, "asset not found")
			return
		}
		if actor.Role != domain.RoleAdmin && asset.UploaderID != actor.ID {
			writeErrJSON(w, http.StatusForbidden, "forbidden")
			return
		}
		var req struct {
			OriginalName *string `json:"original_name"`
			AltText      *string `json:"alt_text"`
		}
		if !decodeJSONBody(w, r, 16384, &req) {
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
			writeErrJSON(w, http.StatusInternalServerError, uErr.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "asset": asset})
		return
	}

	if rest != "" && r.Method == http.MethodDelete {
		var asset domain.MediaAsset
		err := sqlx.GetInto(r.Context(), s.deps.Store, &asset, `SELECT * FROM media_assets WHERE id = ?`, rest)
		if err != nil {
			writeErrJSON(w, http.StatusNotFound, "asset not found")
			return
		}
		if actor.Role != domain.RoleAdmin && asset.UploaderID != actor.ID {
			writeErrJSON(w, http.StatusForbidden, "forbidden")
			return
		}
		if s.deps.Media != nil {
			if dErr := s.deps.Media.Delete(r.Context(), rest); dErr != nil {
				writeErrJSON(w, http.StatusInternalServerError, dErr.Error())
				return
			}
		} else {
			if asset.StorageDriver == "local" && asset.ObjectKey != "" && s.deps.Config != nil {
				curDir := s.deps.Config.GetUploadDir()
				_ = safeUnlinkLocalAsset(curDir, asset.ObjectKey)
				if defDir := s.deps.Config.GetDefaultUploadDir(); defDir != "" && defDir != curDir {
					_ = safeUnlinkLocalAsset(defDir, asset.ObjectKey)
				}
			}
			_, _ = s.deps.Store.ExecContext(r.Context(), `DELETE FROM media_assets WHERE id = ?`, rest)
		}
		_ = s.deps.Content.RefreshMediaDimsCache(r.Context())
		s.invalidateMediaRefCache()
		if s.imageView != nil {
			s.imageView.Purge()
		}
		writeOKJSON(w)
		return
	}
	w.WriteHeader(http.StatusMethodNotAllowed)
}

func canonicalAllowedUploadExt(origName string) (string, error) {
	rawExt := strings.ToLower(filepath.Ext(filepath.Base(origName)))
	switch rawExt {
	case ".png":
		return ".png", nil
	case ".jpg":
		return ".jpg", nil
	case ".jpeg":
		return ".jpeg", nil
	case ".gif":
		return ".gif", nil
	case ".webp":
		return ".webp", nil
	case ".avif":
		return ".avif", nil
	case ".mp3":
		return ".mp3", nil
	case ".m4a":
		return ".m4a", nil
	case ".ogg":
		return ".ogg", nil
	case ".wav":
		return ".wav", nil
	case ".weba":
		return ".weba", nil
	case ".mp4":
		return ".mp4", nil
	case ".webm":
		return ".webm", nil
	case ".pdf":
		return ".pdf", nil
	case ".zip":
		return ".zip", nil
	case ".svg":
		return "", errors.New("raw SVG upload disallowed for XSS safety; use PNG/WebP/JPEG")
	default:
		return "", fmt.Errorf("unsupported file extension %q", rawExt)
	}
}

func sanitizeLocalUploadPrefix(raw string) string {
	trimmed := strings.Trim(strings.TrimSpace(raw), "/\\")
	if trimmed == "" {
		return ""
	}
	parts := strings.Split(strings.ReplaceAll(trimmed, "\\", "/"), "/")
	cleanParts := make([]string, 0, len(parts))
	for _, seg := range parts {
		seg = strings.TrimSpace(seg)
		if seg == "" || seg == "." || seg == ".." {
			continue
		}
		valid := true
		for i := 0; i < len(seg); i++ {
			ch := seg[i]
			if !((ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || ch == '-' || ch == '_' || ch == '.') {
				valid = false
				break
			}
		}
		if valid {
			cleanParts = append(cleanParts, seg)
		}
	}
	return strings.Join(cleanParts, "/")
}

func (s *AppServer) saveFallbackLocalMedia(ctx context.Context, uploaderID string, origName string, r io.Reader, maxBytes int64) (*domain.MediaAsset, error) {
	raw, err := readBoundedStream(r, maxBytes)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, errors.New("empty upload")
	}
	ext, err := canonicalAllowedUploadExt(origName)
	if err != nil {
		return nil, err
	}

	mimeType := defaultReconcileMIMEType(ext, raw[:minInt(512, len(raw))])
	exifStripped := false
	var width, height int

	if ext == ".jpg" || ext == ".jpeg" || ext == ".png" || ext == ".webp" || ext == ".gif" || ext == ".avif" {
		cleanBytes, normMIME, w, h, changed, nErr := media.NormalizeLocalImageBytes(raw, ext)
		if nErr != nil {
			return nil, nErr
		}
		if changed && len(cleanBytes) > 0 {
			raw = cleanBytes
		}
		mimeType = strings.ToLower(strings.TrimSpace(normMIME))
		width, height = w, h
		exifStripped = ext != ".gif" && ext != ".avif"
	} else if strings.HasPrefix(mimeType, "image/") {
		if cfg, _, iErr := image.DecodeConfig(strings.NewReader(string(raw))); iErr == nil {
			width, height = cfg.Width, cfg.Height
		}
	}

	now := time.Now()
	relKey := fmt.Sprintf("%s/%s%s", now.UTC().Format("2006/01"), strings.ToLower(id.Next()), ext)
	if s.deps.Settings != nil {
		if pfx := sanitizeLocalUploadPrefix(s.deps.Settings.Snapshot().Storage.LocalKeyPrefix); pfx != "" {
			relKey = pfx + "/" + relKey
		}
	}
	baseDir := filepath.Clean(s.deps.Config.GetUploadDir())
	dest := filepath.Clean(filepath.Join(baseDir, filepath.FromSlash(relKey)))
	if !strings.HasPrefix(dest, baseDir+string(filepath.Separator)) {
		return nil, errors.New("upload destination escapes configured upload directory")
	}
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
