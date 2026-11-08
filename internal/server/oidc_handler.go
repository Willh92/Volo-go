package server

import (
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/Willh92/Volo-go/internal/config"
	"github.com/Willh92/Volo-go/internal/db/sqlx"
	"github.com/Willh92/Volo-go/internal/domain"
	"github.com/Willh92/Volo-go/internal/security"
	"github.com/Willh92/Volo-go/internal/theme"
	"github.com/Willh92/Volo-go/pkg/id"
)

const (
	oidcStateCookieName = "__volo_oidc_state"
	oidcStateAAD        = "oidc.state.v1"
	oidcStateTTL        = 10 * time.Minute
)

var oidcUsernameSanitizeRe = regexp.MustCompile(`[^a-z0-9_-]+`)

type oidcSealedState struct {
	Nonce     string `json:"nonce"`
	Provider  string `json:"provider"`
	Target    string `json:"target"`
	Next      string `json:"next"`
	ExpiresAt int64  `json:"expires_at"`
}

func isSafeRelativeRedirect(cand string) bool {
	if cand == "" {
		return false
	}
	if !strings.HasPrefix(cand, "/") || strings.HasPrefix(cand, "//") || strings.HasPrefix(cand, `/\`) {
		return false
	}
	for i := 0; i < len(cand); i++ {
		if cand[i] <= 0x1F || cand[i] == 0x7F {
			return false
		}
	}
	return true
}

func (s *AppServer) adminMountPath() string {
	adminPrefix := config.DefaultAdminPath
	if s.deps.Config != nil && strings.TrimSpace(s.deps.Config.AdminPath) != "" {
		adminPrefix = "/" + strings.Trim(strings.TrimSpace(s.deps.Config.AdminPath), "/")
	}
	if adminPrefix == "/" {
		adminPrefix = config.DefaultAdminPath
	}
	return adminPrefix
}

func (s *AppServer) buildOIDCStartURL(providerName, target, next string) string {
	u := "/api/v1/auth/oidc/" + url.PathEscape(providerName) + "/start"
	q := url.Values{}
	if target == "admin" {
		q.Set("target", "admin")
	} else {
		q.Set("target", "reader")
	}
	if isSafeRelativeRedirect(next) {
		q.Set("next", next)
	}
	if enc := q.Encode(); enc != "" {
		u += "?" + enc
	}
	return u
}

func (s *AppServer) listOIDCProviderViews(locale, target, next string) []theme.OIDCProviderView {
	if s.deps.Plugins == nil {
		return []theme.OIDCProviderView{}
	}
	normTarget := strings.ToLower(strings.TrimSpace(target))
	if normTarget != "admin" {
		normTarget = "reader"
	}
	safeNext := ""
	if trimmedNext := strings.TrimSpace(next); isSafeRelativeRedirect(trimmedNext) {
		safeNext = trimmedNext
	}
	providers := s.deps.Plugins.Providers().OIDCProviders()
	out := make([]theme.OIDCProviderView, 0, len(providers))
	for _, p := range providers {
		if p == nil || strings.TrimSpace(p.Name()) == "" {
			continue
		}
		name := strings.TrimSpace(p.Name())
		disp := strings.TrimSpace(p.DisplayName(locale))
		if disp == "" {
			disp = name
		}
		out = append(out, theme.OIDCProviderView{
			Name:        name,
			DisplayName: disp,
			StartURL:    s.buildOIDCStartURL(name, normTarget, safeNext),
		})
	}
	return out
}

func (s *AppServer) handleOIDCAuthRoutes(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "private, no-store")
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	sub := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/auth/oidc"), "/")
	if sub == "providers" {
		rawTarget := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("target")))
		if rawTarget != "" && rawTarget != "admin" && rawTarget != "reader" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "target must be admin or reader"})
			return
		}
		if rawTarget == "" {
			rawTarget = "reader"
		}
		rawNext := strings.TrimSpace(r.URL.Query().Get("next"))
		if rawNext != "" && !isSafeRelativeRedirect(rawNext) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid next redirect path"})
			return
		}
		views := s.listOIDCProviderViews(s.resolveLocale(r), rawTarget, rawNext)
		writeJSON(w, http.StatusOK, map[string]any{"providers": views})
		return
	}

	parts := strings.Split(sub, "/")
	if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" {
		http.NotFound(w, r)
		return
	}
	providerName, err := url.PathUnescape(parts[0])
	if err != nil || strings.TrimSpace(providerName) == "" {
		http.NotFound(w, r)
		return
	}
	providerName = strings.TrimSpace(providerName)
	action := parts[1]

	switch action {
	case "start":
		s.handleOIDCStart(w, r, providerName)
	case "callback":
		s.handleOIDCCallback(w, r, providerName)
	default:
		http.NotFound(w, r)
	}
}

func (s *AppServer) handleOIDCStart(w http.ResponseWriter, r *http.Request, providerName string) {
	if s.deps.Plugins == nil || s.deps.Config == nil || s.deps.Config.KeyRing == nil {
		http.NotFound(w, r)
		return
	}
	clientIP := security.ResolveClientIP(r, s.deps.Config.TrustedProxies)
	if s.deps.WAF != nil && !s.deps.WAF.AllowRate("oidc_start", clientIP, 20, 5*time.Minute) {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": s.deps.Catalog.T(s.resolveLocale(r), "error.rate_limited")})
		return
	}

	p, ok := s.deps.Plugins.Providers().OIDC(providerName)
	if !ok || p == nil {
		http.NotFound(w, r)
		return
	}

	target := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("target")))
	if target == "" {
		target = "reader"
	}
	if target != "admin" && target != "reader" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "target must be admin or reader"})
		return
	}

	rawNext := strings.TrimSpace(r.URL.Query().Get("next"))
	if rawNext != "" && !isSafeRelativeRedirect(rawNext) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid next redirect path"})
		return
	}
	if rawNext == "" {
		if target == "admin" {
			rawNext = s.adminMountPath()
		} else {
			rawNext = "/"
		}
	}

	var nonceBytes [24]byte
	if _, err := rand.Read(nonceBytes[:]); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to generate state nonce"})
		return
	}
	nonce := hex.EncodeToString(nonceBytes[:])
	stateObj := oidcSealedState{
		Nonce:     nonce,
		Provider:  providerName,
		Target:    target,
		Next:      rawNext,
		ExpiresAt: time.Now().Add(oidcStateTTL).UnixMilli(),
	}
	rawStateJSON, err := json.Marshal(stateObj)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to encode state"})
		return
	}
	sealedState, err := s.deps.Config.KeyRing.SealString(string(rawStateJSON), oidcStateAAD)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to seal state"})
		return
	}

	callbackURL := s.publicBaseURL(r) + "/api/v1/auth/oidc/" + url.PathEscape(providerName) + "/callback"
	authURL, err := p.AuthCodeURL(nonce, callbackURL)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "oidc provider error"})
		return
	}
	parsedAuth, uErr := url.Parse(strings.TrimSpace(authURL))
	if uErr != nil || (parsedAuth.Scheme != "https" && parsedAuth.Scheme != "http") || strings.TrimSpace(parsedAuth.Host) == "" {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "invalid authorization URL returned by provider"})
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     oidcStateCookieName,
		Value:    sealedState,
		Path:     "/api/v1/auth/oidc",
		MaxAge:   int(oidcStateTTL / time.Second),
		HttpOnly: true,
		Secure:   s.effectiveRequestScheme(r) == "https",
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, parsedAuth.String(), http.StatusFound)
}

func (s *AppServer) handleOIDCCallback(w http.ResponseWriter, r *http.Request, providerName string) {
	// Always clear the single-use state cookie immediately.
	http.SetCookie(w, &http.Cookie{
		Name:     oidcStateCookieName,
		Value:    "",
		Path:     "/api/v1/auth/oidc",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   s.effectiveRequestScheme(r) == "https",
		SameSite: http.SameSiteLaxMode,
	})

	if s.deps.Plugins == nil || s.deps.Config == nil || s.deps.Config.KeyRing == nil {
		http.NotFound(w, r)
		return
	}
	p, ok := s.deps.Plugins.Providers().OIDC(providerName)
	if !ok || p == nil {
		http.NotFound(w, r)
		return
	}

	stateCookie, err := r.Cookie(oidcStateCookieName)
	if err != nil || strings.TrimSpace(stateCookie.Value) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing oidc state cookie"})
		return
	}
	plainState, err := s.deps.Config.KeyRing.OpenString(stateCookie.Value, oidcStateAAD)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid oidc state cookie"})
		return
	}
	var st oidcSealedState
	if err := json.Unmarshal([]byte(plainState), &st); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "malformed oidc state"})
		return
	}
	if time.Now().UnixMilli() > st.ExpiresAt || st.Provider != providerName || st.Nonce == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "oidc state expired or mismatched"})
		return
	}

	queryState := strings.TrimSpace(r.URL.Query().Get("state"))
	if queryState == "" || subtle.ConstantTimeCompare([]byte(queryState), []byte(st.Nonce)) != 1 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "oidc state verification failed"})
		return
	}
	code := strings.TrimSpace(r.URL.Query().Get("code"))
	if code == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing authorization code"})
		return
	}

	callbackURL := s.publicBaseURL(r) + "/api/v1/auth/oidc/" + url.PathEscape(providerName) + "/callback"
	ident, err := p.Exchange(r.Context(), code, callbackURL)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "oidc code exchange failed"})
		return
	}

	normEmail := strings.ToLower(strings.TrimSpace(ident.Email))
	if normEmail == "" || !strings.Contains(normEmail, "@") || len(normEmail) > 190 {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "verified email is required from oidc provider"})
		return
	}

	dest := st.Next
	if !isSafeRelativeRedirect(dest) {
		if st.Target == "admin" {
			dest = s.adminMountPath()
		} else {
			dest = "/"
		}
	}

	var matchedUser domain.User
	switch st.Target {
	case "admin":
		u, qErr := sqlx.Get[domain.User](
			r.Context(), s.deps.Store,
			`SELECT * FROM users WHERE LOWER(email) = ? AND status = ? AND role IN (?, ?)`,
			normEmail, domain.UserStatusActive, domain.RoleAdmin, domain.RoleEditor,
		)
		if qErr != nil {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "no active admin or editor account matches this identity"})
			return
		}
		matchedUser = u

	case "reader":
		u, qErr := sqlx.Get[domain.User](
			r.Context(), s.deps.Store,
			`SELECT * FROM users WHERE LOWER(email) = ?`,
			normEmail,
		)
		if qErr == nil {
			if u.Status != domain.UserStatusActive {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": "account is disabled"})
				return
			}
			matchedUser = u
		} else if errors.Is(qErr, sql.ErrNoRows) {
			created, cErr := s.createOIDCReaderUser(r, normEmail, ident.DisplayName, ident.AvatarURL)
			if cErr != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to provision reader account"})
				return
			}
			matchedUser = created
		} else {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "database lookup failed"})
			return
		}

	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid oidc target"})
		return
	}

	clientIP := security.ResolveClientIP(r, s.deps.Config.TrustedProxies)
	var geo string
	if s.deps.Geo != nil {
		if loc, gOk := s.deps.Geo.Lookup(clientIP); gOk {
			geo = loc.Region
		}
	}
	_, rawTok, err := s.deps.Auth.CreateSession(r.Context(), matchedUser.ID, clientIP, geo, r.UserAgent(), 0)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to create session"})
		return
	}
	s.setSessionCookieForRequest(w, r, rawTok)
	http.Redirect(w, r, dest, http.StatusFound)
}

func (s *AppServer) createOIDCReaderUser(r *http.Request, normEmail, displayName, avatarURL string) (domain.User, error) {
	now := time.Now().UnixMilli()
	localPart := normEmail
	if at := strings.IndexByte(normEmail, '@'); at > 0 {
		localPart = normEmail[:at]
	}
	baseUsername := strings.Trim(oidcUsernameSanitizeRe.ReplaceAllString(strings.ToLower(localPart), "_"), "_-")
	if len(baseUsername) < 3 {
		baseUsername = "reader"
	}
	if len(baseUsername) > 24 {
		baseUsername = baseUsername[:24]
	}
	var suffixBytes [4]byte
	_, _ = rand.Read(suffixBytes[:])
	username := baseUsername + "_" + hex.EncodeToString(suffixBytes[:])

	disp := strings.TrimSpace(displayName)
	if disp == "" {
		disp = localPart
	}
	if len([]rune(disp)) > 64 {
		disp = string([]rune(disp)[:64])
	}

	cleanAvatar := ""
	if trimmedAv := strings.TrimSpace(avatarURL); trimmedAv != "" {
		if u, err := url.Parse(trimmedAv); err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" {
			cleanAvatar = trimmedAv
		}
	}

	user := domain.User{
		ID:              id.Next(),
		Username:        username,
		Email:           normEmail,
		DisplayName:     disp,
		AvatarURL:       cleanAvatar,
		Bio:             "",
		WebsiteURL:      "",
		Role:            domain.RoleReader,
		PasswordHash:    "",
		LegacyHash:      "",
		Status:          domain.UserStatusActive,
		EmailVerifiedAt: now,
		PreferredLocale: s.resolveLocale(r),
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	_, err := s.deps.Store.ExecContext(
		r.Context(),
		`INSERT INTO users (
			id, username, email, display_name, avatar_url, bio, website_url,
			role, password_hash, legacy_hash, status, email_verified_at, preferred_locale, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		user.ID, user.Username, user.Email, user.DisplayName, user.AvatarURL, user.Bio, user.WebsiteURL,
		user.Role, user.PasswordHash, user.LegacyHash, user.Status, user.EmailVerifiedAt, user.PreferredLocale,
		user.CreatedAt, user.UpdatedAt,
	)
	if err != nil {
		return domain.User{}, err
	}
	return user, nil
}
