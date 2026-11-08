package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
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
	"github.com/Willh92/Volo-go/pkg/plugin"
)

const (
	oidcStateCookieName = "__volo_oidc_state"
	oidcStateAAD        = "oidc.state.v2"
	oidcStateTTL        = 10 * time.Minute
)

var (
	oidcUsernameSanitizeRe = regexp.MustCompile(`[^a-z0-9_-]+`)
	safeRelativeRedirectRe = regexp.MustCompile(`^/[A-Za-z0-9/_~.%?=&+#:-]*$`)
)

type oidcSealedState struct {
	State        string `json:"state"`
	OIDCNonce    string `json:"oidc_nonce"`
	CodeVerifier string `json:"code_verifier"`
	Provider     string `json:"provider"`
	Target       string `json:"target"`
	Next         string `json:"next"`
	BindUserID   string `json:"bind_user_id,omitempty"`
	ExpiresAt    int64  `json:"expires_at"`
}

func isLocalUrl(cand string) bool {
	if cand == "" || len(cand) > 2048 {
		return false
	}
	if !strings.HasPrefix(cand, "/") || strings.HasPrefix(cand, "//") || strings.HasPrefix(cand, `/\`) {
		return false
	}
	return safeRelativeRedirectRe.MatchString(cand)
}

func isSafeRelativeRedirect(cand string) bool {
	return isLocalUrl(cand)
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

func (s *AppServer) oidcCallbackURL(r *http.Request, providerName string) string {
	base := ""
	if s.deps.Config != nil {
		base = normalizeTrustedBaseURL(s.deps.Config.GetPublicURL())
	}
	if base == "" {
		base = s.publicBaseURL(r)
	}
	return base + "/api/v1/auth/oidc/" + url.PathEscape(providerName) + "/callback"
}

func (s *AppServer) redirectOIDCError(w http.ResponseWriter, r *http.Request, target, next, code string) {
	if code == "" {
		code = "state"
	}
	if target == "admin" || target == "bind" {
		http.Redirect(w, r, s.adminMountPath()+"?oidc_error="+url.QueryEscape(code), http.StatusFound)
		return
	}
	q := url.Values{}
	q.Set("error", code)
	if trimmed := strings.TrimSpace(next); isSafeRelativeRedirect(trimmed) && trimmed != "/" && trimmed != "/reader/login" && trimmed != "/reader/register" {
		q.Set("next", trimmed)
	}
	http.Redirect(w, r, "/reader/login?"+q.Encode(), http.StatusFound)
}

func (s *AppServer) inferOIDCFallbackTarget(r *http.Request) string {
	if r != nil {
		switch strings.ToLower(strings.TrimSpace(r.URL.Query().Get("target"))) {
		case "admin", "bind":
			return "admin"
		}
		if viewer, _ := s.resolveViewer(r); viewer != nil && (viewer.Role == domain.RoleAdmin || viewer.Role == domain.RoleEditor) {
			return "admin"
		}
	}
	return "reader"
}

func formatOIDCCredentialName(providerName, normEmail, subject string) string {
	label := strings.TrimSpace(normEmail)
	if label == "" {
		label = strings.TrimSpace(subject)
	}
	name := strings.TrimSpace(providerName) + " (" + label + ")"
	if runes := []rune(name); len(runes) > 120 {
		name = string(runes[:120])
	}
	return name
}

func (s *AppServer) insertOIDCCredential(ctx context.Context, userID, credKey, credName string, now int64) (string, error) {
	credID := id.Next()
	_, err := s.deps.Store.ExecContext(ctx,
		`INSERT INTO user_credentials (
			id, user_id, kind, name, credential_id, public_key_spki,
			cose_alg, sign_count, secret_ciphertext, meta_json, last_used_at, created_at
		) VALUES (?, ?, 'oidc', ?, ?, '', 0, 0, '', '{}', ?, ?)`,
		credID, userID, credName, credKey, now, now,
	)
	if err != nil {
		return "", err
	}
	return credID, nil
}

func (s *AppServer) buildOIDCStartURL(providerName, target, next string) string {
	u := "/api/v1/auth/oidc/" + url.PathEscape(providerName) + "/start"
	q := url.Values{}
	switch target {
	case "admin":
		q.Set("target", "admin")
	case "bind":
		q.Set("target", "bind")
	default:
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
	if normTarget != "admin" && normTarget != "bind" {
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
		if rawTarget != "" && rawTarget != "admin" && rawTarget != "reader" && rawTarget != "bind" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "target must be admin, reader, or bind"})
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
	p, ok := s.deps.Plugins.Providers().OIDC(providerName)
	if !ok || p == nil {
		http.NotFound(w, r)
		return
	}

	target := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("target")))
	if target == "" {
		target = "reader"
	}
	if target != "admin" && target != "reader" && target != "bind" {
		s.redirectOIDCError(w, r, "reader", "", "state")
		return
	}

	rawNext := strings.TrimSpace(r.URL.Query().Get("next"))
	if rawNext != "" && !isSafeRelativeRedirect(rawNext) {
		s.redirectOIDCError(w, r, target, "", "invalid_redirect")
		return
	}

	clientIP := security.ResolveClientIP(r, s.deps.Config.TrustedProxies)
	if s.deps.WAF != nil && !s.deps.WAF.AllowRate("oidc_start", clientIP, 20, 5*time.Minute) {
		s.redirectOIDCError(w, r, target, rawNext, "rate_limited")
		return
	}

	bindUserID := ""
	if target == "bind" {
		actor, sess := s.resolveViewer(r)
		if actor == nil || sess == nil || actor.Status != domain.UserStatusActive || (actor.Role != domain.RoleAdmin && actor.Role != domain.RoleEditor) {
			s.redirectOIDCError(w, r, "bind", "", "forbidden")
			return
		}
		bindUserID = actor.ID
	}

	if rawNext == "" {
		if target == "admin" || target == "bind" {
			rawNext = s.adminMountPath()
		} else {
			rawNext = "/"
		}
	}

	var (
		stateBytes    [24]byte
		nonceBytes    [16]byte
		verifierBytes [32]byte
	)
	if _, err := rand.Read(stateBytes[:]); err != nil {
		s.redirectOIDCError(w, r, target, rawNext, "state")
		return
	}
	if _, err := rand.Read(nonceBytes[:]); err != nil {
		s.redirectOIDCError(w, r, target, rawNext, "state")
		return
	}
	if _, err := rand.Read(verifierBytes[:]); err != nil {
		s.redirectOIDCError(w, r, target, rawNext, "state")
		return
	}

	stateHex := hex.EncodeToString(stateBytes[:])
	oidcNonce := hex.EncodeToString(nonceBytes[:])
	codeVerifier := base64.RawURLEncoding.EncodeToString(verifierBytes[:])
	challengeSum := sha256.Sum256([]byte(codeVerifier))
	codeChallenge := base64.RawURLEncoding.EncodeToString(challengeSum[:])

	stateObj := oidcSealedState{
		State:        stateHex,
		OIDCNonce:    oidcNonce,
		CodeVerifier: codeVerifier,
		Provider:     providerName,
		Target:       target,
		Next:         rawNext,
		BindUserID:   bindUserID,
		ExpiresAt:    time.Now().Add(oidcStateTTL).UnixMilli(),
	}
	rawStateJSON, err := json.Marshal(stateObj)
	if err != nil {
		s.redirectOIDCError(w, r, target, rawNext, "state")
		return
	}
	sealedState, err := s.deps.Config.KeyRing.SealString(string(rawStateJSON), oidcStateAAD)
	if err != nil {
		s.redirectOIDCError(w, r, target, rawNext, "state")
		return
	}

	callbackURL := s.oidcCallbackURL(r, providerName)
	var authURL string
	if adv, ok := p.(plugin.OIDCAdvancedProvider); ok {
		authURL, err = adv.AuthCodeURLWithParams(plugin.OIDCAuthParams{
			State:               stateHex,
			RedirectURI:         callbackURL,
			Nonce:               oidcNonce,
			CodeChallenge:       codeChallenge,
			CodeChallengeMethod: "S256",
		})
	} else {
		authURL, err = p.AuthCodeURL(stateHex, callbackURL)
	}
	if err != nil {
		s.redirectOIDCError(w, r, target, rawNext, "provider_error")
		return
	}
	parsedAuth, uErr := url.Parse(strings.TrimSpace(authURL))
	if uErr != nil || (parsedAuth.Scheme != "https" && parsedAuth.Scheme != "http") || strings.TrimSpace(parsedAuth.Host) == "" {
		s.redirectOIDCError(w, r, target, rawNext, "provider_error")
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

	fallbackTarget := s.inferOIDCFallbackTarget(r)
	stateCookie, err := r.Cookie(oidcStateCookieName)
	if err != nil || strings.TrimSpace(stateCookie.Value) == "" {
		s.redirectOIDCError(w, r, fallbackTarget, "", "state")
		return
	}
	plainState, err := s.deps.Config.KeyRing.OpenString(stateCookie.Value, oidcStateAAD)
	if err != nil {
		s.redirectOIDCError(w, r, fallbackTarget, "", "state")
		return
	}
	var st oidcSealedState
	if err := json.Unmarshal([]byte(plainState), &st); err != nil {
		s.redirectOIDCError(w, r, fallbackTarget, "", "state")
		return
	}
	if st.Target != "admin" && st.Target != "reader" && st.Target != "bind" {
		s.redirectOIDCError(w, r, fallbackTarget, "", "state")
		return
	}
	if time.Now().UnixMilli() > st.ExpiresAt || st.Provider != providerName || st.State == "" || st.OIDCNonce == "" || st.CodeVerifier == "" {
		s.redirectOIDCError(w, r, st.Target, st.Next, "state")
		return
	}

	queryState := strings.TrimSpace(r.URL.Query().Get("state"))
	if queryState == "" || subtle.ConstantTimeCompare([]byte(queryState), []byte(st.State)) != 1 {
		s.redirectOIDCError(w, r, st.Target, st.Next, "state")
		return
	}
	if idpErr := strings.TrimSpace(r.URL.Query().Get("error")); idpErr != "" {
		s.redirectOIDCError(w, r, st.Target, st.Next, "provider_error")
		return
	}
	code := strings.TrimSpace(r.URL.Query().Get("code"))
	if code == "" {
		s.redirectOIDCError(w, r, st.Target, st.Next, "state")
		return
	}

	callbackURL := s.oidcCallbackURL(r, providerName)
	var ident plugin.OIDCIdentity
	if adv, ok := p.(plugin.OIDCAdvancedProvider); ok {
		ident, err = adv.ExchangeWithParams(r.Context(), plugin.OIDCExchangeParams{
			Code:         code,
			RedirectURI:  callbackURL,
			Nonce:        st.OIDCNonce,
			CodeVerifier: st.CodeVerifier,
		})
	} else {
		ident, err = p.Exchange(r.Context(), code, callbackURL)
	}
	if err != nil {
		s.redirectOIDCError(w, r, st.Target, st.Next, "exchange_failed")
		return
	}
	if ident.Nonce != "" && subtle.ConstantTimeCompare([]byte(ident.Nonce), []byte(st.OIDCNonce)) != 1 {
		s.redirectOIDCError(w, r, st.Target, st.Next, "state")
		return
	}

	sub := strings.TrimSpace(ident.Subject)
	if sub == "" {
		s.redirectOIDCError(w, r, st.Target, st.Next, "invalid_identity")
		return
	}
	credKey := providerName + ":" + sub
	normEmail := strings.ToLower(strings.TrimSpace(ident.Email))
	if normEmail != "" && (!strings.Contains(normEmail, "@") || len(normEmail) > 190) {
		normEmail = ""
	}

	dest := st.Next
	if !isSafeRelativeRedirect(dest) {
		if st.Target == "admin" || st.Target == "bind" {
			dest = s.adminMountPath()
		} else {
			dest = "/"
		}
	}

	clientIP := security.ResolveClientIP(r, s.deps.Config.TrustedProxies)
	var geo string
	if s.deps.Geo != nil {
		if st.Target == "admin" || st.Target == "bind" {
			geo = s.deps.Geo.LookupDisplay(clientIP)
		} else if loc, gOk := s.deps.Geo.Lookup(clientIP); gOk {
			geo = loc.Region
		}
	}
	now := time.Now().UnixMilli()

	switch st.Target {
	case "bind":
		actor, sess := s.resolveViewer(r)
		if actor == nil || sess == nil || st.BindUserID == "" || actor.ID != st.BindUserID {
			s.redirectOIDCError(w, r, "bind", st.Next, "state")
			return
		}
		if actor.Status != domain.UserStatusActive {
			s.redirectOIDCError(w, r, "bind", st.Next, "disabled")
			return
		}
		if actor.Role != domain.RoleAdmin && actor.Role != domain.RoleEditor {
			s.redirectOIDCError(w, r, "bind", st.Next, "forbidden")
			return
		}
		existing, qErr := sqlx.Get[domain.UserCredential](
			r.Context(), s.deps.Store,
			`SELECT * FROM user_credentials WHERE kind = 'oidc' AND credential_id = ?`,
			credKey,
		)
		if qErr == nil {
			if existing.UserID != actor.ID {
				s.redirectOIDCError(w, r, "bind", st.Next, "already_linked")
				return
			}
			_, _ = s.deps.Store.ExecContext(r.Context(),
				`UPDATE user_credentials SET last_used_at = ? WHERE id = ?`, now, existing.ID)
			http.Redirect(w, r, s.adminMountPath(), http.StatusFound)
			return
		}
		if !errors.Is(qErr, sql.ErrNoRows) {
			s.redirectOIDCError(w, r, "bind", st.Next, "bind_failed")
			return
		}

		credName := formatOIDCCredentialName(providerName, normEmail, sub)
		credID, insErr := s.insertOIDCCredential(r.Context(), actor.ID, credKey, credName, now)
		if insErr != nil {
			s.redirectOIDCError(w, r, "bind", st.Next, "already_linked")
			return
		}
		s.recordAuditLog(r, actor, "security.oidc_link", "user_credential", credID, map[string]any{
			"provider":      providerName,
			"credential_id": credKey,
		})
		http.Redirect(w, r, s.adminMountPath(), http.StatusFound)
		return

	case "admin":
		cred, qErr := sqlx.Get[domain.UserCredential](
			r.Context(), s.deps.Store,
			`SELECT * FROM user_credentials WHERE kind = 'oidc' AND credential_id = ?`,
			credKey,
		)
		if qErr != nil {
			s.redirectOIDCError(w, r, "admin", st.Next, "unlinked")
			return
		}
		u, uErr := sqlx.Get[domain.User](
			r.Context(), s.deps.Store,
			`SELECT * FROM users WHERE id = ?`,
			cred.UserID,
		)
		if uErr != nil {
			s.redirectOIDCError(w, r, "admin", st.Next, "unlinked")
			return
		}
		if u.Status != domain.UserStatusActive {
			s.redirectOIDCError(w, r, "admin", st.Next, "disabled")
			return
		}
		if u.Role != domain.RoleAdmin && u.Role != domain.RoleEditor {
			s.redirectOIDCError(w, r, "admin", st.Next, "forbidden")
			return
		}
		if strings.TrimSpace(ident.Email) != "" && !ident.EmailVerified {
			s.redirectOIDCError(w, r, "admin", st.Next, "forbidden")
			return
		}

		_, _ = s.deps.Store.ExecContext(r.Context(),
			`UPDATE user_credentials SET last_used_at = ? WHERE id = ?`, now, cred.ID)

		trusted2FA := ""
		if c, cErr := r.Cookie(trusted2FACookieName); cErr == nil {
			trusted2FA = c.Value
		}
		res, lErr := s.deps.Auth.CompleteStaffPrimaryLogin(r.Context(), u, clientIP, geo, r.UserAgent(), trusted2FA)
		if lErr != nil {
			s.redirectOIDCError(w, r, "admin", st.Next, "forbidden")
			return
		}
		if res.Requires2FA {
			http.Redirect(w, r, s.adminMountPath()+"?challenge_token="+url.QueryEscape(res.ChallengeToken), http.StatusFound)
			return
		}
		s.setSessionCookieForRequest(w, r, res.RawToken)
		s.recordAuditLog(r, &u, "auth.login_oidc", "user", u.ID, map[string]any{"provider": providerName})
		http.Redirect(w, r, dest, http.StatusFound)
		return

	case "reader":
		if !ident.EmailVerified || normEmail == "" {
			s.redirectOIDCError(w, r, "reader", st.Next, "unverified_email")
			return
		}

		var matchedUser domain.User
		if cred, qErr := sqlx.Get[domain.UserCredential](
			r.Context(), s.deps.Store,
			`SELECT * FROM user_credentials WHERE kind = 'oidc' AND credential_id = ?`,
			credKey,
		); qErr == nil {
			u, uErr := sqlx.Get[domain.User](r.Context(), s.deps.Store, `SELECT * FROM users WHERE id = ?`, cred.UserID)
			if uErr != nil {
				s.redirectOIDCError(w, r, "reader", st.Next, "state")
				return
			}
			if u.Role != domain.RoleReader {
				s.redirectOIDCError(w, r, "reader", st.Next, "forbidden")
				return
			}
			if u.Status != domain.UserStatusActive {
				s.redirectOIDCError(w, r, "reader", st.Next, "disabled")
				return
			}
			_, _ = s.deps.Store.ExecContext(r.Context(),
				`UPDATE user_credentials SET last_used_at = ? WHERE id = ?`, now, cred.ID)
			matchedUser = u
		} else if errors.Is(qErr, sql.ErrNoRows) {
			u, uErr := sqlx.Get[domain.User](
				r.Context(), s.deps.Store,
				`SELECT * FROM users WHERE LOWER(email) = ?`,
				normEmail,
			)
			if uErr == nil {
				if u.Role != domain.RoleReader {
					s.redirectOIDCError(w, r, "reader", st.Next, "forbidden")
					return
				}
				if u.Status != domain.UserStatusActive {
					s.redirectOIDCError(w, r, "reader", st.Next, "disabled")
					return
				}
				credName := formatOIDCCredentialName(providerName, normEmail, sub)
				_, _ = s.insertOIDCCredential(r.Context(), u.ID, credKey, credName, now)
				matchedUser = u
			} else if errors.Is(uErr, sql.ErrNoRows) {
				created, cErr := s.createOIDCReaderUser(r, normEmail, ident.DisplayName, ident.AvatarURL, providerName, credKey)
				if cErr != nil {
					s.redirectOIDCError(w, r, "reader", st.Next, "register_failed")
					return
				}
				matchedUser = created
			} else {
				s.redirectOIDCError(w, r, "reader", st.Next, "server_error")
				return
			}
		} else {
			s.redirectOIDCError(w, r, "reader", st.Next, "server_error")
			return
		}

		_, rawTok, sErr := s.deps.Auth.CreateSession(r.Context(), matchedUser.ID, clientIP, geo, r.UserAgent(), 0)
		if sErr != nil {
			s.redirectOIDCError(w, r, "reader", st.Next, "server_error")
			return
		}
		s.setSessionCookieForRequest(w, r, rawTok)
		http.Redirect(w, r, dest, http.StatusFound)
		return

	default:
		s.redirectOIDCError(w, r, "reader", "", "state")
		return
	}
}

func (s *AppServer) createOIDCReaderUser(r *http.Request, normEmail, displayName, avatarURL, providerName, credKey string) (domain.User, error) {
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
	credID := id.Next()
	credName := formatOIDCCredentialName(providerName, normEmail, normEmail)
	err := s.deps.Store.WithTx(r.Context(), func(tx *sqlx.Tx) error {
		if _, iErr := tx.ExecContext(
			r.Context(),
			`INSERT INTO users (
				id, username, email, display_name, avatar_url, bio, website_url,
				role, password_hash, legacy_hash, status, email_verified_at, preferred_locale, created_at, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			user.ID, user.Username, user.Email, user.DisplayName, user.AvatarURL, user.Bio, user.WebsiteURL,
			user.Role, user.PasswordHash, user.LegacyHash, user.Status, user.EmailVerifiedAt, user.PreferredLocale,
			user.CreatedAt, user.UpdatedAt,
		); iErr != nil {
			return iErr
		}
		if credKey != "" {
			if _, cErr := tx.ExecContext(
				r.Context(),
				`INSERT INTO user_credentials (
					id, user_id, kind, name, credential_id, public_key_spki,
					cose_alg, sign_count, secret_ciphertext, meta_json, last_used_at, created_at
				) VALUES (?, ?, 'oidc', ?, ?, '', 0, 0, '', '{}', ?, ?)`,
				credID, user.ID, credName, credKey, now, now,
			); cErr != nil {
				return cErr
			}
		}
		return nil
	})
	if err != nil {
		return domain.User{}, err
	}

	if user.Role == domain.RoleReader && s.deps.Passports != nil && s.deps.Auth != nil {
		if gp, ok := s.deps.Passports.ResolveOrHeal(nil, r, false); ok && gp != nil {
			_, _ = s.deps.Auth.ClaimGuestComments(r.Context(), &user, gp)
		}
	}
	s.recordAuditLog(r, &user, "reader.register_oidc", "user", user.ID, map[string]any{"provider": providerName})
	return user, nil
}
