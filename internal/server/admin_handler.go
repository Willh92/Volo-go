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
	"github.com/Willh92/Volo-go/internal/render"
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
	case "auth/passkey/login/begin":
		s.handleAdminPasskeyLoginBegin(w, r)
		return
	case "auth/passkey/login/finish":
		s.handleAdminPasskeyLoginFinish(w, r)
		return
	}

	switch {
	case sub == "auth/logout":
		actor, sess, ok := s.authenticateAdminRequest(w, r, "", false)
		if !ok {
			return
		}
		if sess != nil {
			_ = s.deps.Auth.RevokeSession(r.Context(), actor, sess.ID)
		}
		http.SetCookie(w, &http.Cookie{Name: auth.SessionCookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode})
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
		writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
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
	s.setSessionCookie(w, res.RawToken)
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
	s.setSessionCookie(w, res.RawToken)
	if trustCookie != "" {
		secure := strings.HasPrefix(strings.ToLower(s.deps.Config.PublicURL), "https://")
		http.SetCookie(w, &http.Cookie{
			Name:     trusted2FACookieName,
			Value:    trustCookie,
			Path:     "/api/admin/auth",
			MaxAge:   s.deps.Settings.Snapshot().Security.TOTPRememberDays * 86400,
			HttpOnly: true,
			Secure:   secure,
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
	clientIP := security.ResolveClientIP(r, s.deps.Config.TrustedProxies)
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
	s.setSessionCookie(w, res.RawToken)
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
			"user":         actor,
			"session_id":   sessID,
			"csrf_token":   csrf,
			"totp_enabled": totpCnt > 0,
			"passkeys":     pkCnt,
			"site":         snap.Site,
			"appearance":   snap.Appearance,
			"locales":      s.deps.Catalog.Locales(),
		})
		return
	}

	if r.Method == http.MethodPatch {
		var req struct {
			DisplayName     *string `json:"display_name"`
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
		if req.Bio != nil {
			fullUser.Bio = strings.TrimSpace(*req.Bio)
		}
		if req.WebsiteURL != nil {
			fullUser.WebsiteURL = strings.TrimSpace(*req.WebsiteURL)
		}
		if req.AvatarURL != nil {
			fullUser.AvatarURL = strings.TrimSpace(*req.AvatarURL)
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
			`UPDATE users SET display_name = ?, bio = ?, website_url = ?, avatar_url = ?, preferred_locale = ?, password_hash = ?, legacy_hash = ?, updated_at = ? WHERE id = ?`,
			fullUser.DisplayName, fullUser.Bio, fullUser.WebsiteURL, fullUser.AvatarURL, fullUser.PreferredLocale,
			fullUser.PasswordHash, fullUser.LegacyHash, fullUser.UpdatedAt, fullUser.ID)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		fullUser.PasswordHash = ""
		fullUser.LegacyHash = ""
		writeJSON(w, http.StatusOK, map[string]any{"user": fullUser})
		return
	}
	w.WriteHeader(http.StatusMethodNotAllowed)
}

func (s *AppServer) handleAdminSelfCredentials(w http.ResponseWriter, r *http.Request, actor *domain.User, sess *domain.UserSession, sub string) {
	switch {
	case sub == "totp/setup" && r.Method == http.MethodPost:
		secret, err := auth.GenerateTOTPSecret()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		uri := auth.BuildOTPAuthURI(s.deps.Settings.Snapshot().Site.Title, actor.Username, secret)
		writeJSON(w, http.StatusOK, map[string]string{"secret": secret, "otpauth_uri": uri})

	case sub == "totp/enroll" && r.Method == http.MethodPost:
		var req struct {
			Secret string `json:"secret"`
			Code   string `json:"code"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
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
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 65536)).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
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
		pkID := strings.TrimPrefix(sub, "passkeys/")
		_, _ = s.deps.Store.ExecContext(r.Context(),
			`DELETE FROM user_credentials WHERE id = ? AND user_id = ? AND kind = 'passkey'`, pkID, actor.ID)
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
		var req struct {
			Name      string   `json:"name"`
			Scopes    []string `json:"scopes"`
			ExpiresIn int      `json:"expires_in_days"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
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
	var pubCnt, draftCnt, schedCnt, protCnt, pageCnt, cmtPending, mediaCnt int
	_ = s.deps.Store.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM articles WHERE status = 'published'`).Scan(&pubCnt)
	_ = s.deps.Store.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM articles WHERE status = 'draft'`).Scan(&draftCnt)
	_ = s.deps.Store.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM articles WHERE status = 'scheduled'`).Scan(&schedCnt)
	_ = s.deps.Store.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM articles WHERE password_ciphertext <> '' OR has_content_lock = ?`, true).Scan(&protCnt)
	_ = s.deps.Store.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM pages`).Scan(&pageCnt)
	if actor.Role == domain.RoleAdmin {
		_ = s.deps.Store.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM comments WHERE status = 'pending' AND is_deleted = ?`, false).Scan(&cmtPending)
	} else {
		_ = s.deps.Store.QueryRowContext(r.Context(),
			`SELECT COUNT(*) FROM comments c INNER JOIN articles a ON c.target_type = 'article' AND c.target_id = a.id
			 WHERE a.author_id = ? AND c.status = 'pending' AND c.is_deleted = ?`, actor.ID, false).Scan(&cmtPending)
	}
	_ = s.deps.Store.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM media_assets`).Scan(&mediaCnt)

	windowDays := 30
	if rawDays, err := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("days"))); err == nil && rawDays == 7 {
		windowDays = 7
	}
	now := time.Now()
	cutoffDate := now.AddDate(0, 0, -(windowDays - 1)).Format("2006-01-02")
	trend, _ := sqlx.Select[domain.AnalyticsDaily](r.Context(), s.deps.Store,
		`SELECT * FROM analytics_daily WHERE stat_date >= ? ORDER BY stat_date ASC`, cutoffDate)
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

	rawTopPosts, _ := sqlx.Select[struct {
		ItemKey string `db:"item_key" json:"item_key"`
		Hits    int64  `db:"hits" json:"hits"`
	}](r.Context(), s.deps.Store,
		`SELECT item_key, SUM(hits) AS hits FROM analytics_top_items
		 WHERE stat_date >= ? AND kind = 'article' GROUP BY item_key ORDER BY hits DESC LIMIT 10`, cutoffDate)

	type topArticleView struct {
		ItemKey string `json:"item_key"`
		ID      string `json:"id"`
		Title   string `json:"title"`
		Slug    string `json:"slug"`
		Hits    int64  `json:"hits"`
	}
	topPosts := make([]topArticleView, 0, len(rawTopPosts))
	for _, tp := range rawTopPosts {
		var title, slug string
		_ = s.deps.Store.QueryRowContext(r.Context(),
			`SELECT title, slug FROM articles WHERE id = ? OR slug = ? LIMIT 1`, tp.ItemKey, tp.ItemKey).Scan(&title, &slug)
		if title == "" {
			title = tp.ItemKey
		}
		topPosts = append(topPosts, topArticleView{
			ItemKey: tp.ItemKey,
			ID:      tp.ItemKey,
			Title:   title,
			Slug:    slug,
			Hits:    tp.Hits,
		})
	}

	type topRefererView struct {
		ItemKey string `db:"item_key" json:"item_key"`
		Hits    int64  `db:"hits" json:"hits"`
	}
	topReferers, _ := sqlx.Select[topRefererView](r.Context(), s.deps.Store,
		`SELECT item_key, SUM(hits) AS hits FROM analytics_top_items
		 WHERE stat_date >= ? AND kind = 'referer' GROUP BY item_key ORDER BY hits DESC LIMIT 10`, cutoffDate)
	if topReferers == nil {
		topReferers = []topRefererView{}
	}

	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)

	locale := actor.PreferredLocale
	if locale == "" {
		locale = s.deps.Settings.Snapshot().Site.DefaultLocale
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

	writeJSON(w, http.StatusOK, map[string]any{
		"counts": map[string]int{
			"published":        pubCnt,
			"drafts":           draftCnt,
			"scheduled":        schedCnt,
			"protected":        protCnt,
			"pages":            pageCnt,
			"pending_comments": cmtPending,
			"media":            mediaCnt,
		},
		"window_days":      windowDays,
		"analytics_daily":  trend,
		"analytics_series": series,
		"top_articles":     topPosts,
		"top_referers":     topReferers,
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
	CategoryID       string   `json:"category_id"`
	Title            string   `json:"title"`
	Slug             string   `json:"slug"`
	LegacyPermalink  string   `json:"legacy_permalink"`
	SummaryMD        string   `json:"summary_md"`
	ContentMD        string   `json:"content_md"`
	CoverURL         string   `json:"cover_url"`
	Status           string   `json:"status"`
	PublishedAt      int64    `json:"published_at"`
	ScheduledAt      int64    `json:"scheduled_at"`
	IsPinned         bool     `json:"is_pinned"`
	AllowComment     bool     `json:"allow_comment"`
	Password         *string  `json:"password"`
	ClearPassword    bool     `json:"clear_password"`
	AudioURL         string   `json:"audio_url"`
	AudioDurationSec int      `json:"audio_duration_sec"`
	AudioSizeBytes   int64    `json:"audio_size_bytes"`
	AudioMIME        string   `json:"audio_mime"`
	Lang             string   `json:"lang"`
	TranslationGroup string   `json:"translation_group"`
	LicenseOverride  string   `json:"license_override"`
	Tags             []string `json:"tags"`
	SeriesID         string   `json:"series_id"`
	RevisionNote     string   `json:"revision_note"`
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
			Mode:             render.RenderModeWeb,
			Locale:           s.deps.Catalog.Match(req.Locale, actor.PreferredLocale),
			Unlocked:         true,
			IsReaderLoggedIn: true,
			ShowLineNumbers:  s.deps.Settings.Snapshot().Reading.ShowCodeLineNumbers,
			LookupMediaDims:  s.deps.Content.LookupMediaDims,
		})
		writeJSON(w, http.StatusOK, map[string]any{"html": out.HTML, "toc": out.TOC})
		return
	}

	if rest == "import" && r.Method == http.MethodPost {
		s.handleAdminBatchImportArticles(w, r, actor)
		return
	}

	if rest == "" {
		switch r.Method {
		case http.MethodGet:
			status := strings.TrimSpace(r.URL.Query().Get("status"))
			q := strings.TrimSpace(r.URL.Query().Get("q"))
			catID := strings.TrimSpace(r.URL.Query().Get("category_id"))

			where := []string{"1=1"}
			var args []any
			if actor.Role == domain.RoleEditor {
				where = append(where, "author_id = ?")
				args = append(args, actor.ID)
			}
			if status == "protected" {
				where = append(where, "(password_ciphertext <> '' OR has_content_lock = ?)")
				args = append(args, true)
			} else if status != "" && status != "all" {
				where = append(where, "status = ?")
				args = append(args, status)
			}
			if catID != "" {
				where = append(where, "category_id = ?")
				args = append(args, catID)
			}
			if q != "" {
				where = append(where, "(LOWER(title) LIKE LOWER(?) OR LOWER(slug) LIKE LOWER(?))")
				pat := "%" + q + "%"
				args = append(args, pat, pat)
			}
			rows, err := sqlx.Select[domain.RawArticle](r.Context(), s.deps.Store,
				`SELECT id, author_id, category_id, title, slug, legacy_permalink, summary_md, '' AS content_md,
				        cover_url, status, published_at, scheduled_at, is_pinned, allow_comment,
				        password_ciphertext, has_content_lock, audio_url, audio_duration_sec, audio_size_bytes, audio_mime,
				        lang, translation_group, license_override, word_count, reading_minutes, view_count, comment_count,
				        created_at, updated_at
				 FROM articles WHERE `+strings.Join(where, " AND ")+` ORDER BY is_pinned DESC, updated_at DESC LIMIT 300`,
				args...)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
				return
			}
			items := make([]map[string]any, 0, len(rows))
			for _, row := range rows {
				hasPwd := strings.TrimSpace(row.PasswordCiphertext) != "" || row.HasContentLock
				row.PasswordCiphertext = ""
				items = append(items, map[string]any{
					"article":               row,
					"is_password_protected": hasPwd,
				})
			}
			writeJSON(w, http.StatusOK, map[string]any{"items": items})

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
		if raw.Status == domain.ArticleStatusPublished {
			s.deps.Content.SearchIndex().Upsert(raw)
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
			plainPwd, hasPwd := s.deps.Content.Guard().DecryptArticlePassword(raw)
			if strings.HasPrefix(plainPwd, "\x00") {
				plainPwd = ""
			}
			raw.PasswordCiphertext = ""
			writeJSON(w, http.StatusOK, map[string]any{
				"article":               raw,
				"tags":                  tags,
				"series_id":             seriesID,
				"is_password_protected": hasPwd,
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
				_, dErr := tx.ExecContext(r.Context(), `DELETE FROM articles WHERE id = ?`, artID)
				return dErr
			})
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
				return
			}
			s.deps.Content.SearchIndex().Remove(artID)
			s.recordAuditLog(r, actor, "article.delete", "article", artID, map[string]any{"title": raw.Title})
			writeJSON(w, http.StatusOK, map[string]bool{"ok": true})

		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
		return
	}
	http.NotFound(w, r)
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
		if p == "" {
			pwdCipher = ""
		} else {
			sealed, sErr := s.deps.Content.Guard().SealArticlePassword(cur.ID, p)
			if sErr != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": sErr.Error()})
				return
			}
			pwdCipher = sealed
		}
	}

	oldContentChanged := !isCreate && (cur.Title != title || cur.ContentMD != in.ContentMD || cur.SummaryMD != in.SummaryMD)
	prevSnapshot := cur

	cur.CategoryID = strings.TrimSpace(in.CategoryID)
	cur.Title = title
	cur.Slug = slug
	if in.LegacyPermalink != "" || isCreate {
		cur.LegacyPermalink = strings.TrimSpace(in.LegacyPermalink)
	}
	cur.SummaryMD = in.SummaryMD
	cur.ContentMD = in.ContentMD
	cur.CoverURL = strings.TrimSpace(in.CoverURL)
	cur.Status = status
	cur.PublishedAt = pubAt
	cur.ScheduledAt = in.ScheduledAt
	cur.IsPinned = in.IsPinned
	cur.AllowComment = in.AllowComment
	cur.PasswordCiphertext = pwdCipher
	cur.HasContentLock = render.ContainsLockDirective(in.ContentMD)
	cur.AudioURL = strings.TrimSpace(in.AudioURL)
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

	if cur.Status == domain.ArticleStatusPublished {
		s.deps.Content.SearchIndex().Upsert(cur)
	} else {
		s.deps.Content.SearchIndex().Remove(cur.ID)
	}

	if s.deps.Plugins != nil {
		hookName := plugin.HookArticleUpdated
		if cur.Status == domain.ArticleStatusPublished && !wasPublished {
			hookName = plugin.HookArticlePublished
		}
		s.deps.Plugins.DispatchArticleHook(r.Context(), hookName, plugin.ArticleHookPayload{
			ID:                  cur.ID,
			Title:               cur.Title,
			Slug:                cur.Slug,
			Lang:                cur.Lang,
			SummaryMD:           cur.SummaryMD,
			ContentMD:           cur.ContentMD,
			CoverURL:            cur.CoverURL,
			AudioURL:            cur.AudioURL,
			IsPasswordProtected: cur.PasswordCiphertext != "",
			HasContentLock:      cur.HasContentLock,
		})
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
	writeJSON(w, code, map[string]any{"article": respArt, "is_password_protected": hasPwd})
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

func (s *AppServer) handleAdminPages(w http.ResponseWriter, r *http.Request, actor *domain.User, rest string) {
	rest = strings.TrimPrefix(rest, "/")
	if rest == "" {
		switch r.Method {
		case http.MethodGet:
			rows, _ := sqlx.Select[domain.Page](r.Context(), s.deps.Store, `SELECT * FROM pages ORDER BY updated_at DESC`)
			if rows == nil {
				rows = []domain.Page{}
			}
			writeJSON(w, http.StatusOK, map[string]any{"items": rows})

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
			if in.Lang == "" {
				in.Lang = s.deps.Settings.Snapshot().Site.DefaultLocale
			}
			now := time.Now().UnixMilli()
			in.CreatedAt = now
			in.UpdatedAt = now
			if _, err := s.deps.Store.ExecContext(r.Context(),
				`INSERT INTO pages (id, author_id, title, slug, content_md, is_published, allow_comment, show_toc, lang, translation_group, view_count, comment_count, created_at, updated_at)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, 0, ?, ?)`,
				in.ID, in.AuthorID, in.Title, in.Slug, in.ContentMD, in.IsPublished, in.AllowComment, in.ShowTOC,
				in.Lang, strings.TrimSpace(in.TranslationGroup), in.CreatedAt, in.UpdatedAt); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
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
		slug := sanitizeSlug(in.Slug)
		if slug == "" {
			slug = pg.Slug
		}
		now := time.Now().UnixMilli()
		_, err := s.deps.Store.ExecContext(r.Context(),
			`UPDATE pages SET title = ?, slug = ?, content_md = ?, is_published = ?, allow_comment = ?, show_toc = ?, lang = ?, translation_group = ?, updated_at = ? WHERE id = ?`,
			strings.TrimSpace(in.Title), slug, in.ContentMD, in.IsPublished, in.AllowComment, in.ShowTOC,
			strings.TrimSpace(in.Lang), strings.TrimSpace(in.TranslationGroup), now, pg.ID)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		pg, _ = sqlx.Get[domain.Page](r.Context(), s.deps.Store, `SELECT * FROM pages WHERE id = ?`, pg.ID)
		writeJSON(w, http.StatusOK, map[string]any{"page": pg})
	case http.MethodDelete:
		_, _ = s.deps.Store.ExecContext(r.Context(), `DELETE FROM pages WHERE id = ?`, pg.ID)
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
			rows, _ := s.deps.Content.ListCategoriesWithCounts(r.Context())
			writeJSON(w, http.StatusOK, map[string]any{"items": rows})
		case http.MethodPost:
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
				in.ID, in.Name, in.Slug, strings.TrimSpace(in.Description), strings.TrimSpace(in.Icon), in.SortOrder, now, now); err != nil {
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
		var in domain.Category
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384)).Decode(&in); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
			return
		}
		_, err := s.deps.Store.ExecContext(r.Context(),
			`UPDATE categories SET name = ?, slug = ?, description = ?, icon = ?, sort_order = ?, updated_at = ? WHERE id = ?`,
			strings.TrimSpace(in.Name), sanitizeSlug(in.Slug), strings.TrimSpace(in.Description), strings.TrimSpace(in.Icon),
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
	if rest == "" {
		switch r.Method {
		case http.MethodGet:
			rows, _ := s.deps.Content.ListTagsWithCounts(r.Context(), 500)
			writeJSON(w, http.StatusOK, map[string]any{"items": rows})
		case http.MethodPost:
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
	if r.Method == http.MethodDelete {
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
		return
	}
	w.WriteHeader(http.StatusMethodNotAllowed)
}

func (s *AppServer) handleAdminSeries(w http.ResponseWriter, r *http.Request, _ *domain.User, rest string) {
	rest = strings.TrimPrefix(rest, "/")
	if rest == "" {
		switch r.Method {
		case http.MethodGet:
			rows, _ := sqlx.Select[domain.Series](r.Context(), s.deps.Store, `SELECT * FROM series ORDER BY sort_order ASC, created_at DESC`)
			if rows == nil {
				rows = []domain.Series{}
			}
			writeJSON(w, http.StatusOK, map[string]any{"items": rows})
		case http.MethodPost:
			var in struct {
				domain.Series
				ArticleIDs []string `json:"article_ids"`
			}
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32768)).Decode(&in); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
				return
			}
			in.ID = id.Next()
			in.Title = strings.TrimSpace(in.Title)
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
			writeJSON(w, http.StatusCreated, map[string]any{"series": in.Series})
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
		return
	}

	switch r.Method {
	case http.MethodPut:
		var in struct {
			domain.Series
			ArticleIDs []string `json:"article_ids"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32768)).Decode(&in); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
			return
		}
		err := s.deps.Store.WithTx(r.Context(), func(tx *sqlx.Tx) error {
			if _, uErr := tx.ExecContext(r.Context(),
				`UPDATE series SET title = ?, slug = ?, description = ?, cover_url = ?, sort_order = ?, updated_at = ? WHERE id = ?`,
				strings.TrimSpace(in.Title), sanitizeSlug(in.Slug), in.Description, in.CoverURL, in.SortOrder, time.Now().UnixMilli(), rest); uErr != nil {
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
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	case http.MethodDelete:
		_ = s.deps.Store.WithTx(r.Context(), func(tx *sqlx.Tx) error {
			_, _ = tx.ExecContext(r.Context(), `DELETE FROM series_articles WHERE series_id = ?`, rest)
			_, err := tx.ExecContext(r.Context(), `DELETE FROM series WHERE id = ?`, rest)
			return err
		})
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *AppServer) handleAdminBooks(w http.ResponseWriter, r *http.Request, _ *domain.User, rest string) {
	rest = strings.TrimPrefix(rest, "/")
	if rest == "" {
		switch r.Method {
		case http.MethodGet:
			rows, _ := s.deps.Content.ListBooks(r.Context())
			if rows == nil {
				rows = []domain.Book{}
			}
			writeJSON(w, http.StatusOK, map[string]any{"items": rows})
		case http.MethodPost:
			var in struct {
				domain.Book
				Chapters []domain.BookChapter `json:"chapters"`
			}
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256*1024)).Decode(&in); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
				return
			}
			in.ID = id.Next()
			in.Title = strings.TrimSpace(in.Title)
			in.Slug = sanitizeSlug(in.Slug)
			if in.Slug == "" {
				in.Slug = sanitizeSlug(in.Title)
			}
			if in.SourceType == "" {
				in.SourceType = "auto"
			}
			if in.EmbedMode == "" {
				in.EmbedMode = "link"
			}
			now := time.Now().UnixMilli()
			in.CreatedAt = now
			in.UpdatedAt = now
			in.ChapterCount = len(in.Chapters)
			err := s.deps.Store.WithTx(r.Context(), func(tx *sqlx.Tx) error {
				if _, iErr := tx.ExecContext(r.Context(),
					`INSERT INTO books (id, title, slug, description, cover_url, source_type, source_url, series_id, embed_mode, chapter_count, synced_at, sort_order, created_at, updated_at)
					 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?, ?)`,
					in.ID, in.Title, in.Slug, in.Description, in.CoverURL, in.SourceType, in.SourceURL, in.SeriesID,
					in.EmbedMode, in.ChapterCount, in.SortOrder, now, now); iErr != nil {
					return iErr
				}
				for idx, ch := range in.Chapters {
					_, _ = tx.ExecContext(r.Context(),
						`INSERT INTO book_chapters (id, book_id, parent_id, title, url, depth, sort_order) VALUES (?, ?, '', ?, ?, ?, ?)`,
						id.Next(), in.ID, ch.Title, ch.URL, ch.Depth, idx+1)
				}
				return nil
			})
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
			writeJSON(w, http.StatusCreated, map[string]any{"book": in.Book})
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
		return
	}

	parts := strings.Split(rest, "/")
	bookID := parts[0]
	if len(parts) == 2 && parts[1] == "sync" && r.Method == http.MethodPost {
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
		var in struct {
			domain.Book
			Chapters []domain.BookChapter `json:"chapters"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256*1024)).Decode(&in); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
			return
		}
		err := s.deps.Store.WithTx(r.Context(), func(tx *sqlx.Tx) error {
			if _, uErr := tx.ExecContext(r.Context(),
				`UPDATE books SET title = ?, slug = ?, description = ?, cover_url = ?, source_type = ?, source_url = ?, series_id = ?, embed_mode = ?, sort_order = ?, updated_at = ? WHERE id = ?`,
				strings.TrimSpace(in.Title), sanitizeSlug(in.Slug), in.Description, in.CoverURL, in.SourceType, in.SourceURL,
				in.SeriesID, in.EmbedMode, in.SortOrder, time.Now().UnixMilli(), bookID); uErr != nil {
				return uErr
			}
			if in.Chapters != nil {
				_, _ = tx.ExecContext(r.Context(), `DELETE FROM book_chapters WHERE book_id = ?`, bookID)
				for idx, ch := range in.Chapters {
					_, _ = tx.ExecContext(r.Context(),
						`INSERT INTO book_chapters (id, book_id, parent_id, title, url, depth, sort_order) VALUES (?, ?, '', ?, ?, ?, ?)`,
						id.Next(), bookID, ch.Title, ch.URL, ch.Depth, idx+1)
				}
				_, _ = tx.ExecContext(r.Context(), `UPDATE books SET chapter_count = ? WHERE id = ?`, len(in.Chapters), bookID)
			}
			return nil
		})
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})

	case http.MethodDelete:
		_ = s.deps.Store.WithTx(r.Context(), func(tx *sqlx.Tx) error {
			_, _ = tx.ExecContext(r.Context(), `DELETE FROM book_chapters WHERE book_id = ?`, bookID)
			_, err := tx.ExecContext(r.Context(), `DELETE FROM books WHERE id = ?`, bookID)
			return err
		})
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})

	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *AppServer) handleAdminComments(w http.ResponseWriter, r *http.Request, actor *domain.User, rest string) {
	rest = strings.TrimPrefix(rest, "/")
	if rest == "" {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		status := strings.TrimSpace(r.URL.Query().Get("status"))
		var rows []domain.Comment
		var err error
		if actor.Role == domain.RoleAdmin {
			if status != "" && status != "all" {
				rows, err = sqlx.Select[domain.Comment](r.Context(), s.deps.Store,
					`SELECT * FROM comments WHERE status = ? AND is_deleted = 0 ORDER BY created_at DESC LIMIT 250`, status)
			} else {
				rows, err = sqlx.Select[domain.Comment](r.Context(), s.deps.Store,
					`SELECT * FROM comments WHERE is_deleted = 0 ORDER BY created_at DESC LIMIT 250`)
			}
		} else {
			if status != "" && status != "all" {
				rows, err = sqlx.Select[domain.Comment](r.Context(), s.deps.Store,
					`SELECT c.* FROM comments c
					 INNER JOIN articles a ON c.target_type = 'article' AND c.target_id = a.id
					 WHERE a.author_id = ? AND c.status = ? AND c.is_deleted = 0
					 ORDER BY c.created_at DESC LIMIT 250`, actor.ID, status)
			} else {
				rows, err = sqlx.Select[domain.Comment](r.Context(), s.deps.Store,
					`SELECT c.* FROM comments c
					 INNER JOIN articles a ON c.target_type = 'article' AND c.target_id = a.id
					 WHERE a.author_id = ? AND c.is_deleted = 0
					 ORDER BY c.created_at DESC LIMIT 250`, actor.ID)
			}
		}
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}

		items := make([]domain.Comment, 0, len(rows))
		for _, c := range rows {
			authorID := actor.ID
			if actor.Role == domain.RoleAdmin {
				authorID = ""
			}
			scrubbed, sErr := auth.AuthorizeAndScrubCommentForStaff(actor, c, authorID)
			if sErr == nil {
				scrubbed.NotifyEmailCiphertext = ""
				items = append(items, scrubbed)
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": items})
		return
	}

	parts := strings.Split(rest, "/")
	cmtID := parts[0]
	cmt, err := sqlx.Get[domain.Comment](r.Context(), s.deps.Store, `SELECT * FROM comments WHERE id = ?`, cmtID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "comment not found"})
		return
	}
	artAuthorID := ""
	if cmt.TargetType == domain.TargetTypeArticle {
		_ = s.deps.Store.QueryRowContext(r.Context(), `SELECT author_id FROM articles WHERE id = ?`, cmt.TargetID).Scan(&artAuthorID)
	}
	if _, authErr := auth.AuthorizeAndScrubCommentForStaff(actor, cmt, artAuthorID); authErr != nil {
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
		if s.deps.NotifyCommentCreated != nil {
			s.deps.NotifyCommentCreated(r.Context(), view, in)
		}
		writeJSON(w, http.StatusCreated, map[string]any{"comment": view})
		return
	}

	switch r.Method {
	case http.MethodPatch:
		var req struct {
			Status string `json:"status"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
			return
		}
		switch req.Status {
		case domain.CommentStatusApproved, domain.CommentStatusPending, domain.CommentStatusRejected:
		default:
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid comment status"})
			return
		}
		_, err := s.deps.Store.ExecContext(r.Context(),
			`UPDATE comments SET status = ?, updated_at = ? WHERE id = ?`, req.Status, time.Now().UnixMilli(), cmtID)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		s.recountTargetComments(r.Context(), cmt.TargetType, cmt.TargetID)
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})

	case http.MethodDelete:
		_, _ = s.deps.Store.ExecContext(r.Context(),
			`UPDATE comments SET is_deleted = 1, content_md = '', notify_email_ciphertext = '', updated_at = ? WHERE id = ?`,
			time.Now().UnixMilli(), cmtID)
		s.recountTargetComments(r.Context(), cmt.TargetType, cmt.TargetID)
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})

	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *AppServer) recountTargetComments(ctx context.Context, targetType, targetID string) {
	var cnt int
	_ = s.deps.Store.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM comments WHERE target_type = ? AND target_id = ? AND status = 'approved' AND is_deleted = 0`,
		targetType, targetID).Scan(&cnt)
	if targetType == domain.TargetTypePage {
		_, _ = s.deps.Store.ExecContext(ctx, `UPDATE pages SET comment_count = ? WHERE id = ?`, cnt, targetID)
	} else {
		_, _ = s.deps.Store.ExecContext(ctx, `UPDATE articles SET comment_count = ? WHERE id = ?`, cnt, targetID)
	}
}

func (s *AppServer) handleAdminMedia(w http.ResponseWriter, r *http.Request, actor *domain.User, rest string) {
	rest = strings.TrimPrefix(rest, "/")
	if rest == "" && r.Method == http.MethodGet {
		rows, _ := sqlx.Select[domain.MediaAsset](r.Context(), s.deps.Store,
			`SELECT * FROM media_assets ORDER BY created_at DESC LIMIT 300`)
		if rows == nil {
			rows = []domain.MediaAsset{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": rows})
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
		_ = s.deps.Content.RefreshMediaDimsCache(r.Context())
		writeJSON(w, http.StatusCreated, map[string]any{"asset": asset, "url": asset.PublicURL})
		return
	}

	if rest == "orphans" && r.Method == http.MethodPost {
		if err := auth.RequireAdmin(actor); err != nil {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
			return
		}
		clean := r.URL.Query().Get("delete") == "1" || r.URL.Query().Get("delete") == "true"
		if s.deps.Media != nil {
			orphans, err := s.deps.Media.ScanOrphans(r.Context(), clean)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"orphans": orphans, "deleted": clean})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"orphans": []domain.MediaAsset{}, "deleted": false})
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
				_ = os.Remove(filepath.Join(s.deps.Config.UploadDir, filepath.FromSlash(asset.ObjectKey)))
			}
			_, _ = s.deps.Store.ExecContext(r.Context(), `DELETE FROM media_assets WHERE id = ?`, rest)
		}
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
	sniff := http.DetectContentType(raw[:minInt(512, len(raw))])
	ext := strings.ToLower(filepath.Ext(origName))
	switch ext {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".svg", ".mp3", ".m4a", ".ogg", ".wav", ".pdf", ".zip":
	default:
		return nil, fmt.Errorf("unsupported file extension %q", ext)
	}
	if ext == ".svg" {
		return nil, errors.New("raw SVG upload disallowed for XSS safety; use PNG/WebP/JPEG")
	}

	var width, height int
	if strings.HasPrefix(sniff, "image/") {
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
		MIMEType:      sniff,
		SizeBytes:     int64(len(raw)),
		Width:         width,
		Height:        height,
		SHA256:        hex.EncodeToString(sum[:]),
		EXIFStripped:  false,
		UploaderID:    uploaderID,
		CreatedAt:     now.UnixMilli(),
	}
	if _, err := s.deps.Store.ExecContext(ctx,
		`INSERT INTO media_assets (id, storage_driver, object_key, public_url, original_name, mime_type, size_bytes, width, height, sha256, exif_stripped, uploader_id, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		asset.ID, asset.StorageDriver, asset.ObjectKey, asset.PublicURL, asset.OriginalName, asset.MIMEType,
		asset.SizeBytes, asset.Width, asset.Height, asset.SHA256, asset.EXIFStripped, asset.UploaderID, asset.CreatedAt); err != nil {
		return nil, err
	}
	return &asset, nil
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
