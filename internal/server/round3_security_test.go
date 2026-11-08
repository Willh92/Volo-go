package server_test

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
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
	"github.com/Willh92/Volo-go/internal/domain"
	"github.com/Willh92/Volo-go/internal/guest"
	"github.com/Willh92/Volo-go/internal/i18n"
	"github.com/Willh92/Volo-go/internal/notify"
	"github.com/Willh92/Volo-go/internal/render"
	"github.com/Willh92/Volo-go/internal/repo"
	"github.com/Willh92/Volo-go/internal/security"
	"github.com/Willh92/Volo-go/internal/server"
	"github.com/Willh92/Volo-go/internal/visibility"
	"github.com/Willh92/Volo-go/pkg/id"
	"github.com/Willh92/Volo-go/pkg/plugin"
)

type round3AdvancedOIDCProvider struct {
	name string

	mu             sync.Mutex
	lastAuthParams plugin.OIDCAuthParams
	lastExchParams plugin.OIDCExchangeParams
}

func (m *round3AdvancedOIDCProvider) Name() string               { return m.name }
func (m *round3AdvancedOIDCProvider) DisplayName(_ string) string { return "Enterprise OIDC" }

func (m *round3AdvancedOIDCProvider) AuthCodeURL(state string, redirectURI string) (string, error) {
	return "https://idp.example.com/authorize?fallback=1&state=" + url.QueryEscape(state) + "&redirect_uri=" + url.QueryEscape(redirectURI), nil
}

func (m *round3AdvancedOIDCProvider) Exchange(_ context.Context, code string, redirectURI string) (plugin.OIDCIdentity, error) {
	return m.ExchangeWithParams(context.Background(), plugin.OIDCExchangeParams{Code: code, RedirectURI: redirectURI})
}

func (m *round3AdvancedOIDCProvider) AuthCodeURLWithParams(params plugin.OIDCAuthParams) (string, error) {
	m.mu.Lock()
	m.lastAuthParams = params
	m.mu.Unlock()
	return "https://idp.example.com/authorize?state=" + url.QueryEscape(params.State) +
		"&redirect_uri=" + url.QueryEscape(params.RedirectURI) +
		"&nonce=" + url.QueryEscape(params.Nonce) +
		"&code_challenge=" + url.QueryEscape(params.CodeChallenge) +
		"&code_challenge_method=" + url.QueryEscape(params.CodeChallengeMethod), nil
}

func (m *round3AdvancedOIDCProvider) ExchangeWithParams(_ context.Context, params plugin.OIDCExchangeParams) (plugin.OIDCIdentity, error) {
	m.mu.Lock()
	m.lastExchParams = params
	m.mu.Unlock()

	switch params.Code {
	case "nonce-mismatch":
		return plugin.OIDCIdentity{
			Provider:      m.name,
			Subject:       "sub_captain_1",
			Email:         "captain@example.com",
			EmailVerified: true,
			Nonce:         "forged_wrong_nonce",
			DisplayName:   "Captain",
		}, nil
	case "admin-sub":
		return plugin.OIDCIdentity{
			Provider:      m.name,
			Subject:       "sub_captain_1",
			Email:         "captain@example.com",
			EmailVerified: true,
			Nonce:         params.Nonce,
			DisplayName:   "Captain SSO",
		}, nil
	case "reader-unverified":
		return plugin.OIDCIdentity{
			Provider:      m.name,
			Subject:       "sub_reader_unv",
			Email:         "unverified@example.com",
			EmailVerified: false,
			Nonce:         params.Nonce,
			DisplayName:   "Unverified Reader",
		}, nil
	case "reader-staff-email":
		return plugin.OIDCIdentity{
			Provider:      m.name,
			Subject:       "sub_staff_impersonator",
			Email:         "captain@example.com",
			EmailVerified: true,
			Nonce:         params.Nonce,
			DisplayName:   "Staff Email via Reader Target",
		}, nil
	default:
		return plugin.OIDCIdentity{
			Provider:      m.name,
			Subject:       "sub_reader_claim",
			Email:         "claim-reader@example.com",
			EmailVerified: true,
			Nonce:         params.Nonce,
			DisplayName:   "Claim Reader",
		}, nil
	}
}

func (m *round3AdvancedOIDCProvider) snapshot() (plugin.OIDCAuthParams, plugin.OIDCExchangeParams) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastAuthParams, m.lastExchParams
}

func postMultipartRestore(
	t *testing.T,
	urlStr string,
	zipBytes []byte,
	fields map[string]string,
	cookies []*http.Cookie,
	headers map[string]string,
) (*http.Response, []byte) {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("file", "backup.zip")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(fw, bytes.NewReader(zipBytes)); err != nil {
		t.Fatal(err)
	}
	for k, v := range fields {
		if err := mw.WriteField(k, v); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, urlStr, &body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	for _, c := range cookies {
		req.AddCookie(c)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	cli := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := cli.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp, raw
}

func TestRound3_OIDC_PKCE_Nonce_Bind_2FA_And_RoleIsolation(t *testing.T) {
	env, adminCookies, adminHdrs, adminID := setupRound2Env(t)

	probe := &round2TestPlugin{id: "r3-oidc-plugin"}
	if err := env.pluginMgr.RegisterBuiltin(env.ctx, probe); err != nil {
		t.Fatalf("RegisterBuiltin: %v", err)
	}
	if err := env.pluginMgr.Enable(env.ctx, probe.id); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	advOIDC := &round3AdvancedOIDCProvider{name: "okta"}
	env.pluginMgr.Providers().RegisterOIDC(probe.id, advOIDC)

	// 1. Canonical PublicURL redirect_uri ignores Host header poisoning + PKCE S256 & Nonce generation
	startReq, _ := http.NewRequest(http.MethodGet, env.ts.URL+"/api/v1/auth/oidc/okta/start?target=admin&next=/console", nil)
	startReq.Host = "evil-poison.example.org"
	noRedirCli := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	startResp, err := noRedirCli.Do(startReq)
	if err != nil {
		t.Fatalf("OIDC start: %v", err)
	}
	_ = startResp.Body.Close()
	if startResp.StatusCode != http.StatusFound {
		t.Fatalf("expected 302 from OIDC start, got %d", startResp.StatusCode)
	}
	authParams, _ := advOIDC.snapshot()
	if authParams.RedirectURI != "https://studio.example.com/api/v1/auth/oidc/okta/callback" {
		t.Fatalf("expected canonical PublicURL callback URI, got %q", authParams.RedirectURI)
	}
	if len(authParams.State) != 48 || len(authParams.Nonce) != 32 || authParams.CodeChallengeMethod != "S256" || authParams.CodeChallenge == "" {
		t.Fatalf("unexpected OIDC PKCE/nonce params: %+v", authParams)
	}

	// 2. Nonce mismatch fails validation with friendly 302 ?oidc_error=state
	badNonceCB, _ := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/v1/auth/oidc/okta/callback?code=nonce-mismatch&state="+url.QueryEscape(authParams.State), nil, startResp.Cookies(), nil)
	if badNonceCB.StatusCode != http.StatusFound || badNonceCB.Header.Get("Location") != "/console?oidc_error=state" {
		t.Fatalf("expected 302 /console?oidc_error=state on OIDC nonce mismatch, got %d loc=%q", badNonceCB.StatusCode, badNonceCB.Header.Get("Location"))
	}
	_, exchParams := advOIDC.snapshot()
	sum := sha256.Sum256([]byte(exchParams.CodeVerifier))
	if base64.RawURLEncoding.EncodeToString(sum[:]) != authParams.CodeChallenge {
		t.Fatalf("PKCE S256 verifier mismatch: challenge=%q verifier=%q", authParams.CodeChallenge, exchParams.CodeVerifier)
	}

	// 3. target=bind without active staff session redirects 302 ?oidc_error=forbidden
	noAuthBind, _ := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/v1/auth/oidc/okta/start?target=bind", nil, nil, nil)
	if noAuthBind.StatusCode != http.StatusFound || noAuthBind.Header.Get("Location") != "/console?oidc_error=forbidden" {
		t.Fatalf("expected 302 /console?oidc_error=forbidden for unauthenticated target=bind, got %d loc=%q", noAuthBind.StatusCode, noAuthBind.Header.Get("Location"))
	}

	// 4. Bind OIDC credential to captain, verify GET /api/admin/security/oidc-links, and reject duplicate bind from another staff user
	bindStart, _ := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/v1/auth/oidc/okta/start?target=bind", nil, adminCookies, nil)
	bindAuthURL, _ := url.Parse(bindStart.Header.Get("Location"))
	bindCookies := append(append([]*http.Cookie{}, adminCookies...), bindStart.Cookies()...)
	bindCB, _ := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/v1/auth/oidc/okta/callback?code=admin-sub&state="+url.QueryEscape(bindAuthURL.Query().Get("state")), nil, bindCookies, nil)
	if bindCB.StatusCode != http.StatusFound || bindCB.Header.Get("Location") != "/console" {
		t.Fatalf("expected 302 /console after binding OIDC credential, got %d loc=%q", bindCB.StatusCode, bindCB.Header.Get("Location"))
	}

	linksResp, linksRaw := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/admin/security/oidc-links", nil, adminCookies, nil)
	if linksResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/admin/security/oidc-links = %d %s", linksResp.StatusCode, string(linksRaw))
	}
	var linksPayload struct {
		Items []struct {
			ID           string `json:"id"`
			CredentialID string `json:"credential_id"`
		} `json:"items"`
		Providers []struct {
			Name        string `json:"name"`
			DisplayName string `json:"display_name"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(linksRaw, &linksPayload); err != nil {
		t.Fatal(err)
	}
	if len(linksPayload.Items) != 1 || linksPayload.Items[0].CredentialID != "okta:sub_captain_1" || len(linksPayload.Providers) == 0 {
		t.Fatalf("unexpected oidc-links payload: %s", string(linksRaw))
	}
	linkID := linksPayload.Items[0].ID

	// Create editor1 and verify attempting to bind the same okta:sub_captain_1 returns already_linked
	_, _ = doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/users", map[string]any{
		"username":     "editor_oidc",
		"email":        "editor_oidc@example.com",
		"display_name": "Editor OIDC",
		"role":         "editor",
		"password":     "Ed1t0r!Pass#2026",
	}, adminCookies, adminHdrs)
	edLogin, _ := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/auth/login", map[string]any{
		"ident":    "editor_oidc",
		"password": "Ed1t0r!Pass#2026",
	}, nil, map[string]string{"Origin": env.ts.URL})
	edCookies := edLogin.Cookies()

	dupBindStart, _ := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/v1/auth/oidc/okta/start?target=bind", nil, edCookies, nil)
	dupBindURL, _ := url.Parse(dupBindStart.Header.Get("Location"))
	dupCookies := append(append([]*http.Cookie{}, edCookies...), dupBindStart.Cookies()...)
	dupBindCB, _ := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/v1/auth/oidc/okta/callback?code=admin-sub&state="+url.QueryEscape(dupBindURL.Query().Get("state")), nil, dupCookies, nil)
	if dupBindCB.StatusCode != http.StatusFound || dupBindCB.Header.Get("Location") != "/console?oidc_error=already_linked" {
		t.Fatalf("expected 302 /console?oidc_error=already_linked on cross-user duplicate bind, got %d loc=%q", dupBindCB.StatusCode, dupBindCB.Header.Get("Location"))
	}

	// 5. Enroll TOTP 2FA on captain -> OIDC staff login MUST enforce Step-2 TOTP (?challenge_token=...) without minting session cookie!
	totpSecret, err := auth.GenerateTOTPSecret()
	if err != nil {
		t.Fatal(err)
	}
	enrollCode, err := auth.ComputeTOTPCode(totpSecret, time.Now().Add(-30*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.authSvc.EnrollTOTP(env.ctx, adminID, totpSecret, enrollCode); err != nil {
		t.Fatalf("EnrollTOTP: %v", err)
	}

	adm2FAStart, _ := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/v1/auth/oidc/okta/start?target=admin&next=/console", nil, nil, nil)
	adm2FAURL, _ := url.Parse(adm2FAStart.Header.Get("Location"))
	adm2FACB, _ := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/v1/auth/oidc/okta/callback?code=admin-sub&state="+url.QueryEscape(adm2FAURL.Query().Get("state")), nil, adm2FAStart.Cookies(), nil)
	if adm2FACB.StatusCode != http.StatusFound || !strings.HasPrefix(adm2FACB.Header.Get("Location"), "/console?challenge_token=") {
		t.Fatalf("expected OIDC login with TOTP enabled to redirect to /console?challenge_token=..., got %d loc=%q", adm2FACB.StatusCode, adm2FACB.Header.Get("Location"))
	}
	for _, c := range adm2FACB.Cookies() {
		if c.Name == auth.SessionCookieName && c.Value != "" {
			t.Fatalf("OIDC callback must NOT set session cookie before Step-2 TOTP verification!")
		}
	}
	chalLoc, _ := url.Parse(adm2FACB.Header.Get("Location"))
	challengeToken := chalLoc.Query().Get("challenge_token")
	liveCode, err := auth.ComputeTOTPCode(totpSecret, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	step2Resp, step2Raw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/auth/2fa", map[string]any{
		"challenge_token": challengeToken,
		"code":            liveCode,
		"remember_device": true,
	}, nil, map[string]string{"Origin": env.ts.URL})
	if step2Resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/admin/auth/2fa = %d %s", step2Resp.StatusCode, string(step2Raw))
	}
	var trustCookie *http.Cookie
	for _, c := range step2Resp.Cookies() {
		if c.Name == "__volo_2fa_trust" && c.Value != "" {
			trustCookie = c
			if c.Path != "/api" {
				t.Fatalf("expected __volo_2fa_trust cookie Path=/api so OIDC /api/v1/auth/oidc receives it, got %q", c.Path)
			}
		}
	}
	if trustCookie == nil {
		t.Fatalf("expected __volo_2fa_trust cookie from remember_device=true")
	}

	// Subsequent OIDC login WITH __volo_2fa_trust skips 2FA challenge and issues session cookie
	admTrustStart, _ := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/v1/auth/oidc/okta/start?target=admin&next=/console", nil, nil, nil)
	admTrustURL, _ := url.Parse(admTrustStart.Header.Get("Location"))
	trustCBCookies := append([]*http.Cookie{trustCookie}, admTrustStart.Cookies()...)
	admTrustCB, _ := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/v1/auth/oidc/okta/callback?code=admin-sub&state="+url.QueryEscape(admTrustURL.Query().Get("state")), nil, trustCBCookies, nil)
	if admTrustCB.StatusCode != http.StatusFound || admTrustCB.Header.Get("Location") != "/console" {
		t.Fatalf("expected trusted-2FA OIDC login to redirect straight to /console, got %d loc=%q", admTrustCB.StatusCode, admTrustCB.Header.Get("Location"))
	}

	// 6. DELETE /api/admin/security/oidc-links/{id} requires step-up current_password
	delNoPwd, _ := doJSONReq(t, http.MethodDelete, env.ts.URL+"/api/admin/security/oidc-links/"+linkID, map[string]any{}, adminCookies, adminHdrs)
	if delNoPwd.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 when unlinking OIDC without current_password, got %d", delNoPwd.StatusCode)
	}
	delBadPwd, _ := doJSONReq(t, http.MethodDelete, env.ts.URL+"/api/admin/security/oidc-links/"+linkID, map[string]any{
		"current_password": "WrongPassword!",
	}, adminCookies, adminHdrs)
	if delBadPwd.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 when unlinking OIDC with wrong current_password, got %d", delBadPwd.StatusCode)
	}
	delOK, _ := doJSONReq(t, http.MethodDelete, env.ts.URL+"/api/admin/security/oidc-links/"+linkID, map[string]any{
		"current_password": "Str0ng!Pass#2026",
	}, adminCookies, adminHdrs)
	if delOK.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 when unlinking OIDC with valid current_password, got %d", delOK.StatusCode)
	}

	// 7. Reader target role isolation: unverified email -> error=unverified_email; staff email -> error=forbidden; verified reader claims guest comments!
	rUnvStart, _ := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/v1/auth/oidc/okta/start?target=reader&next=/archives", nil, nil, nil)
	rUnvURL, _ := url.Parse(rUnvStart.Header.Get("Location"))
	rUnvCB, _ := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/v1/auth/oidc/okta/callback?code=reader-unverified&state="+url.QueryEscape(rUnvURL.Query().Get("state")), nil, rUnvStart.Cookies(), nil)
	if rUnvCB.StatusCode != http.StatusFound || rUnvCB.Header.Get("Location") != "/reader/login?error=unverified_email&next=%2Farchives" {
		t.Fatalf("expected 302 /reader/login?error=unverified_email&next=%%2Farchives, got %d loc=%q", rUnvCB.StatusCode, rUnvCB.Header.Get("Location"))
	}

	rStaffStart, _ := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/v1/auth/oidc/okta/start?target=reader&next=/archives", nil, nil, nil)
	rStaffURL, _ := url.Parse(rStaffStart.Header.Get("Location"))
	rStaffCB, _ := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/v1/auth/oidc/okta/callback?code=reader-staff-email&state="+url.QueryEscape(rStaffURL.Query().Get("state")), nil, rStaffStart.Cookies(), nil)
	if rStaffCB.StatusCode != http.StatusFound || rStaffCB.Header.Get("Location") != "/reader/login?error=forbidden&next=%2Farchives" {
		t.Fatalf("expected target=reader with staff email to reject with error=forbidden, got %d loc=%q", rStaffCB.StatusCode, rStaffCB.Header.Get("Location"))
	}

	// Create a published article + guest comment with passport cookie, then sign in via OIDC reader and verify guest comment claim!
	artResp, artRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/articles", map[string]any{
		"title": "OIDC Claim Post", "slug": "oidc-claim-post", "content_md": "hello", "status": "published", "allow_comment": true,
	}, adminCookies, adminHdrs)
	if artResp.StatusCode != http.StatusCreated {
		t.Fatalf("create article = %d %s", artResp.StatusCode, string(artRaw))
	}
	var createdArt struct {
		Article domain.RawArticle `json:"article"`
	}
	_ = json.Unmarshal(artRaw, &createdArt)

	gpRec := httptest.NewRecorder()
	gpReq := httptest.NewRequest(http.MethodGet, "/", nil)
	gpMgr := guest.NewPassportManager(env.cfg.KeyRing, env.cfg.PublicURL)
	gp, ok := gpMgr.ResolveOrHeal(gpRec, gpReq, true)
	if !ok || gp == nil {
		t.Fatalf("ResolveOrHeal mint guest passport failed")
	}
	gpCookies := gpRec.Result().Cookies()
	now := time.Now().UnixMilli()
	cmtID := id.Next()
	_, err = env.store.ExecContext(env.ctx, `INSERT INTO comments (
		id, target_type, target_id, parent_id, reply_to_name, user_id,
		guest_id_hash, env_fp_hash, author_name, author_url, avatar_url,
		notify_email_ciphertext, notify_locale, client_ip, ip_masked, geo_region,
		content_md, status, is_deleted, created_at, updated_at
	) VALUES (?, 'article', ?, '', '', '', ?, '', 'Guest Before OIDC', '', '', '', 'en', '127.0.0.1', '127.0.0.0/24', '', 'Guest hello', 'approved', ?, ?, ?)`,
		cmtID, createdArt.Article.ID, gp.GuestIDHash, false, now, now)
	if err != nil {
		t.Fatalf("insert guest comment: %v", err)
	}

	rClaimStart, _ := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/v1/auth/oidc/okta/start?target=reader&next=/archives", nil, gpCookies, nil)
	rClaimURL, _ := url.Parse(rClaimStart.Header.Get("Location"))
	claimCBCookies := append(append([]*http.Cookie{}, gpCookies...), rClaimStart.Cookies()...)
	rClaimCB, _ := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/v1/auth/oidc/okta/callback?code=reader-claim&state="+url.QueryEscape(rClaimURL.Query().Get("state")), nil, claimCBCookies, nil)
	if rClaimCB.StatusCode != http.StatusFound || rClaimCB.Header.Get("Location") != "/archives" {
		t.Fatalf("expected 302 /archives on verified OIDC reader registration, got %d loc=%q", rClaimCB.StatusCode, rClaimCB.Header.Get("Location"))
	}
	claimedUser, err := sqlx.Get[domain.User](env.ctx, env.store, `SELECT * FROM users WHERE email = 'claim-reader@example.com'`)
	if err != nil {
		t.Fatalf("lookup OIDC reader: %v", err)
	}
	var claimedUID string
	_ = env.store.QueryRowContext(env.ctx, `SELECT user_id FROM comments WHERE id = ?`, cmtID).Scan(&claimedUID)
	if claimedUID != claimedUser.ID {
		t.Fatalf("expected OIDC reader registration to claim guest comment user_id=%q, got %q", claimedUser.ID, claimedUID)
	}
	var oidcCredCnt int
	_ = env.store.QueryRowContext(env.ctx, `SELECT COUNT(*) FROM user_credentials WHERE user_id = ? AND kind = 'oidc' AND credential_id = 'okta:sub_reader_claim'`, claimedUser.ID).Scan(&oidcCredCnt)
	if oidcCredCnt != 1 {
		t.Fatalf("expected OIDC reader user_credentials binding count=1, got %d", oidcCredCnt)
	}
}

func TestRound3_StepUpAuth_BackupTicketBinding_And_TokenRevocation(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	dataDir := filepath.Join(tmp, "data")
	uploadDir := filepath.Join(tmp, "uploads")
	pluginsDir := filepath.Join(tmp, "plugins")
	themesDir := filepath.Join(tmp, "themes")
	backupDir := filepath.Join(tmp, "backups")
	for _, d := range []string{dataDir, uploadDir, pluginsDir, themesDir, backupDir} {
		_ = os.MkdirAll(d, 0o750)
	}

	secret := "0123456789abcdef0123456789abcdef-r3-stepup-secret"
	trustedProxies := "127.0.0.1/32,::1/128"
	cfg, err := config.Load(config.CLIOverrides{
		SecretKey:      &secret,
		TrustedProxies: &trustedProxies,
		DataDir:        &dataDir,
		UploadDir:      &uploadDir,
		PluginsDir:     &pluginsDir,
		ThemesDir:      &themesDir,
		BackupDir:      &backupDir,
	})
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	store, err := sqlx.Open(ctx, sqlx.Options{Driver: "sqlite", DSN: filepath.Join(dataDir, "r3-stepup.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := migrate.Up(ctx, store); err != nil {
		t.Fatal(err)
	}

	cat, _ := i18n.NewCatalog("en", "")
	settingsMgr := repo.NewSettingsManager(store, cfg.KeyRing, "en")
	if _, err := settingsMgr.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	dialer := &security.OutboundSafeDialer{Timeout: 5 * time.Second, AllowLoopbackForTests: true}
	pluginMgr := plugin.NewManager(plugin.ManagerOptions{
		Store: store, Keys: cfg.KeyRing, PluginsDir: pluginsDir, DataDir: dataDir, Catalog: cat,
	})
	if err := pluginMgr.DiscoverAndStart(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pluginMgr.Close(ctx) })

	passports := guest.NewPassportManager(cfg.KeyRing, cfg.PublicURL)
	pow := guest.NewPoWVerifier(cfg.KeyRing)
	rnd := render.Default()
	grd := visibility.NewGuard(cfg.KeyRing, settingsMgr, passports, cat, rnd)
	srch := visibility.NewSearchIndex(store, rnd, 2<<20)
	cnt := content.NewService(content.ServiceOptions{
		Store: store, Keys: cfg.KeyRing, Settings: settingsMgr,
		Passports: passports, PoW: pow, Guard: grd, Search: srch,
		Renderer: rnd, Plugins: pluginMgr, Catalog: cat,
	})
	authSvc := auth.NewService(store, cfg.KeyRing, settingsMgr, cfg.PublicURL)
	disp := notify.NewDispatcher(notify.DispatcherOptions{
		Store: store, Keys: cfg.KeyRing, Settings: settingsMgr,
		Catalog: cat, Plugins: pluginMgr, PublicURL: cfg.PublicURL,
	})
	appSrv, err := server.NewAppServer(server.AppDeps{
		Config: cfg, Store: store, Settings: settingsMgr, Catalog: cat,
		Auth: authSvc, Passports: passports, PoW: pow,
		OutboundDialer: dialer, Plugins: pluginMgr, Content: cnt,
		SendVerificationMail: disp.SendVerificationCode,
		NotifyCommentCreated: disp.NotifyCommentCreated,
		TestSMTP:             disp.SendTestSMTP,
		TestWebhook:          disp.TriggerTestWebhook,
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(appSrv.Handler())
	t.Cleanup(ts.Close)

	compResp, compRaw := doJSONReq(t, http.MethodPost, ts.URL+"/api/setup/complete", map[string]any{
		"site_title": "StepUp Lab", "default_locale": "en", "timezone": "UTC",
		"admin_username": "boss", "admin_email": "boss@example.com",
		"admin_display_name": "Boss", "admin_password": "Init!Pass#2026",
	}, nil, map[string]string{"Origin": ts.URL, "X-Forwarded-For": "198.51.100.10"})
	if compResp.StatusCode != http.StatusOK {
		t.Fatalf("setup/complete = %d %s", compResp.StatusCode, string(compRaw))
	}
	var comp struct {
		CSRFToken string      `json:"csrf_token"`
		User      domain.User `json:"user"`
	}
	_ = json.Unmarshal(compRaw, &comp)
	sessACookies := compResp.Cookies()
	hdrsA := map[string]string{
		auth.CSRFHeaderName: comp.CSRFToken,
		"Origin":            ts.URL,
		"X-Forwarded-For":   "198.51.100.10",
	}

	// 1. PATCH /api/admin/users/{self}: display_name-only succeeds WITHOUT current_password AND writes audit log;
	// email/password change requires current_password (400 missing, 401 wrong, 200 valid)!
	dispResp, _ := doJSONReq(t, http.MethodPatch, ts.URL+"/api/admin/users/"+comp.User.ID, map[string]any{
		"display_name": "Boss Updated",
	}, sessACookies, hdrsA)
	if dispResp.StatusCode != http.StatusOK {
		t.Fatalf("expected display_name-only self-PATCH to succeed without current_password, got %d", dispResp.StatusCode)
	}
	var selfAuditCnt int
	for i := 0; i < 20; i++ {
		_ = store.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_logs WHERE action = 'user.update' AND resource_id = ?`, comp.User.ID).Scan(&selfAuditCnt)
		if selfAuditCnt >= 1 {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if selfAuditCnt < 1 {
		t.Fatalf("expected audit_logs to record self user.update, got count=%d", selfAuditCnt)
	}

	emMissing, _ := doJSONReq(t, http.MethodPatch, ts.URL+"/api/admin/users/"+comp.User.ID, map[string]any{
		"email": "boss-new@example.com",
	}, sessACookies, hdrsA)
	if emMissing.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 on self email change without current_password, got %d", emMissing.StatusCode)
	}
	emWrong, _ := doJSONReq(t, http.MethodPatch, ts.URL+"/api/admin/users/"+comp.User.ID, map[string]any{
		"email":            "boss-new@example.com",
		"current_password": "BadPassword!",
	}, sessACookies, hdrsA)
	if emWrong.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 on self email change with wrong current_password, got %d", emWrong.StatusCode)
	}
	emOK, _ := doJSONReq(t, http.MethodPatch, ts.URL+"/api/admin/users/"+comp.User.ID, map[string]any{
		"email":            "boss-new@example.com",
		"current_password": "Init!Pass#2026",
	}, sessACookies, hdrsA)
	if emOK.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on self email change with valid current_password, got %d", emOK.StatusCode)
	}

	// 2. POST /api/admin/security/tokens: 400 missing, 401 wrong, 201 valid; PAT cannot mint another PAT (403)
	patNoPwd, _ := doJSONReq(t, http.MethodPost, ts.URL+"/api/admin/security/tokens", map[string]any{
		"name": "NoPwd", "scopes": []string{"articles:read"},
	}, sessACookies, hdrsA)
	if patNoPwd.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 when creating PAT without current_password, got %d", patNoPwd.StatusCode)
	}
	patBadPwd, _ := doJSONReq(t, http.MethodPost, ts.URL+"/api/admin/security/tokens", map[string]any{
		"name": "BadPwd", "scopes": []string{"articles:read"}, "current_password": "Wrong!",
	}, sessACookies, hdrsA)
	if patBadPwd.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 when creating PAT with wrong current_password, got %d", patBadPwd.StatusCode)
	}
	patOK, patRaw := doJSONReq(t, http.MethodPost, ts.URL+"/api/admin/security/tokens", map[string]any{
		"name": "ValidPAT", "scopes": []string{"articles:read"}, "current_password": "Init!Pass#2026",
	}, sessACookies, hdrsA)
	if patOK.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 when creating PAT with valid current_password, got %d %s", patOK.StatusCode, string(patRaw))
	}
	var patResp struct {
		RawToken string `json:"plaintext_token"`
	}
	_ = json.Unmarshal(patRaw, &patResp)
	patMintResp, _ := doJSONReq(t, http.MethodPost, ts.URL+"/api/admin/security/tokens", map[string]any{
		"name": "EscalatedPAT", "scopes": []string{"articles:write"}, "current_password": "Init!Pass#2026",
	}, nil, map[string]string{"Authorization": "Bearer " + patResp.RawToken, "Origin": ts.URL})
	if patMintResp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected Bearer PAT to be forbidden (403) from minting new PATs, got %d", patMintResp.StatusCode)
	}

	// 3. DELETE /api/admin/security/passkeys/{id}: 400 missing, 401 wrong header, 200 valid X-Volo-Current-Password header
	now := time.Now().UnixMilli()
	_, err = store.ExecContext(ctx, `INSERT INTO user_credentials (
		id, user_id, kind, name, credential_id, public_key_spki, cose_alg, sign_count, secret_ciphertext, meta_json, last_used_at, created_at
	) VALUES ('pk_r3_1', ?, 'passkey', 'YubiKey', 'cred_pk_1', 'spki', -7, 0, '', '{}', ?, ?)`, comp.User.ID, now, now)
	if err != nil {
		t.Fatal(err)
	}
	pkNoPwd, _ := doJSONReq(t, http.MethodDelete, ts.URL+"/api/admin/security/passkeys/pk_r3_1", nil, sessACookies, hdrsA)
	if pkNoPwd.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 when deleting passkey without current_password, got %d", pkNoPwd.StatusCode)
	}
	hdrsWrongPwd := map[string]string{
		auth.CSRFHeaderName:       comp.CSRFToken,
		"Origin":                  ts.URL,
		"X-Volo-Current-Password": "WrongPassword!",
	}
	pkWrongPwd, _ := doJSONReq(t, http.MethodDelete, ts.URL+"/api/admin/security/passkeys/pk_r3_1", nil, sessACookies, hdrsWrongPwd)
	if pkWrongPwd.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 when deleting passkey with wrong X-Volo-Current-Password, got %d", pkWrongPwd.StatusCode)
	}
	hdrsValidPwd := map[string]string{
		auth.CSRFHeaderName:       comp.CSRFToken,
		"Origin":                  ts.URL,
		"X-Volo-Current-Password": "Init!Pass#2026",
	}
	pkOK, _ := doJSONReq(t, http.MethodDelete, ts.URL+"/api/admin/security/passkeys/pk_r3_1", nil, sessACookies, hdrsValidPwd)
	if pkOK.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 when deleting passkey with valid X-Volo-Current-Password, got %d", pkOK.StatusCode)
	}

	// 4. Backup ticket step-up + (UserID, SessionID, IPBucket) binding + Backup Restore step-up
	tkNoPwd, _ := doJSONReq(t, http.MethodPost, ts.URL+"/api/admin/backups/ticket", map[string]any{}, sessACookies, hdrsA)
	if tkNoPwd.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 on backups/ticket without current_password, got %d", tkNoPwd.StatusCode)
	}
	tkBadPwd, _ := doJSONReq(t, http.MethodPost, ts.URL+"/api/admin/backups/ticket", map[string]any{
		"current_password": "Wrong!",
	}, sessACookies, hdrsA)
	if tkBadPwd.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 on backups/ticket with wrong current_password, got %d", tkBadPwd.StatusCode)
	}

	// Mint ticket #1 in Session A from 198.51.100.10 -> attempt download from different IP 203.0.113.99 MUST fail (401)!
	tk1Resp, tk1Raw := doJSONReq(t, http.MethodPost, ts.URL+"/api/admin/backups/ticket", map[string]any{
		"current_password": "Init!Pass#2026",
	}, sessACookies, hdrsA)
	if tk1Resp.StatusCode != http.StatusOK {
		t.Fatalf("mint ticket #1 = %d %s", tk1Resp.StatusCode, string(tk1Raw))
	}
	var tk1 struct {
		Ticket string `json:"ticket"`
	}
	_ = json.Unmarshal(tk1Raw, &tk1)
	diffIPResp, _ := doJSONReq(t, http.MethodGet, ts.URL+"/api/admin/backups/download?ticket="+url.QueryEscape(tk1.Ticket), nil, sessACookies, map[string]string{
		"X-Forwarded-For": "203.0.113.99",
	})
	if diffIPResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected backup ticket download from different client IP bucket to return 401, got %d", diffIPResp.StatusCode)
	}

	// Mint ticket #2 in Session A -> attempt download from Session B (same user, same IP!) MUST fail (401)!
	loginB, _ := doJSONReq(t, http.MethodPost, ts.URL+"/api/admin/auth/login", map[string]any{
		"ident": "boss", "password": "Init!Pass#2026",
	}, nil, map[string]string{"Origin": ts.URL, "X-Forwarded-For": "198.51.100.10"})
	sessBCookies := loginB.Cookies()

	tk2Resp, tk2Raw := doJSONReq(t, http.MethodPost, ts.URL+"/api/admin/backups/ticket", map[string]any{
		"current_password": "Init!Pass#2026",
	}, sessACookies, hdrsA)
	var tk2 struct {
		Ticket string `json:"ticket"`
	}
	_ = json.Unmarshal(tk2Raw, &tk2)
	diffSessResp, _ := doJSONReq(t, http.MethodGet, ts.URL+"/api/admin/backups/download?ticket="+url.QueryEscape(tk2.Ticket), nil, sessBCookies, map[string]string{
		"X-Forwarded-For": "198.51.100.10",
	})
	if diffSessResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected backup ticket download from different session ID to return 401, got %d", diffSessResp.StatusCode)
	}
	_ = tk2Resp

	// Mint ticket #3 in Session A -> download from matching (UserID, SessionA, 198.51.100.10) succeeds (200)!
	_, tk3Raw := doJSONReq(t, http.MethodPost, ts.URL+"/api/admin/backups/ticket", map[string]any{
		"current_password": "Init!Pass#2026",
	}, sessACookies, hdrsA)
	var tk3 struct {
		Ticket string `json:"ticket"`
	}
	_ = json.Unmarshal(tk3Raw, &tk3)
	validDLResp, validZipBytes := doJSONReq(t, http.MethodGet, ts.URL+"/api/admin/backups/download?ticket="+url.QueryEscape(tk3.Ticket), nil, sessACookies, map[string]string{
		"X-Forwarded-For": "198.51.100.10",
	})
	if validDLResp.StatusCode != http.StatusOK {
		t.Fatalf("expected matching session+IP GET /api/admin/backups/download to return 200, got %d", validDLResp.StatusCode)
	}
	if _, zErr := zip.NewReader(bytes.NewReader(validZipBytes), int64(len(validZipBytes))); zErr != nil {
		t.Fatalf("downloaded backup zip invalid: %v", zErr)
	}

	// Backup restore: dry_run=true allowed without current_password; live restore requires valid current_password!
	dryRestResp, dryRestRaw := postMultipartRestore(t, ts.URL+"/api/admin/backups/restore", validZipBytes, map[string]string{
		"dry_run": "true",
	}, sessACookies, hdrsA)
	if dryRestResp.StatusCode != http.StatusOK {
		t.Fatalf("expected dry_run=true restore without current_password to succeed, got %d %s", dryRestResp.StatusCode, string(dryRestRaw))
	}
	liveNoPwdResp, _ := postMultipartRestore(t, ts.URL+"/api/admin/backups/restore", validZipBytes, map[string]string{
		"dry_run": "false",
	}, sessACookies, hdrsA)
	if liveNoPwdResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected live restore without current_password to return 400, got %d", liveNoPwdResp.StatusCode)
	}
	liveBadPwdResp, _ := postMultipartRestore(t, ts.URL+"/api/admin/backups/restore", validZipBytes, map[string]string{
		"dry_run":          "false",
		"current_password": "WrongPassword!",
	}, sessACookies, hdrsA)
	if liveBadPwdResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected live restore with wrong current_password to return 401, got %d", liveBadPwdResp.StatusCode)
	}
	liveOKResp, liveOKRaw := postMultipartRestore(t, ts.URL+"/api/admin/backups/restore", validZipBytes, map[string]string{
		"dry_run":          "false",
		"current_password": "Init!Pass#2026",
	}, sessACookies, hdrsA)
	if liveOKResp.StatusCode != http.StatusOK {
		t.Fatalf("expected live restore with valid current_password to return 200, got %d %s", liveOKResp.StatusCode, string(liveOKRaw))
	}

	// 5. Password change (`PATCH /api/admin/me`) and admin password reset (`PATCH /api/admin/users/{id}`) MUST revoke `api_tokens`!
	patBeforeResp, _ := doJSONReq(t, http.MethodGet, ts.URL+"/api/admin/articles", nil, nil, map[string]string{
		"Authorization": "Bearer " + patResp.RawToken,
	})
	if patBeforeResp.StatusCode != http.StatusOK {
		t.Fatalf("expected PAT to work before password change, got %d", patBeforeResp.StatusCode)
	}

	pwChangeResp, pwChangeRaw := doJSONReq(t, http.MethodPatch, ts.URL+"/api/admin/me", map[string]any{
		"current_password": "Init!Pass#2026",
		"new_password":     "Rotated!Pass#2027",
	}, sessACookies, hdrsA)
	if pwChangeResp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH /api/admin/me password change = %d %s", pwChangeResp.StatusCode, string(pwChangeRaw))
	}
	patAfterMeResp, _ := doJSONReq(t, http.MethodGet, ts.URL+"/api/admin/articles", nil, nil, map[string]string{
		"Authorization": "Bearer " + patResp.RawToken,
	})
	if patAfterMeResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected PAT to be revoked (401) after PATCH /api/admin/me password change, got %d", patAfterMeResp.StatusCode)
	}

	// Mint a fresh PAT, then reset password via PATCH /api/admin/users/{self} and verify PAT revocation too!
	_, pat2Raw := doJSONReq(t, http.MethodPost, ts.URL+"/api/admin/security/tokens", map[string]any{
		"name": "SecondPAT", "scopes": []string{"articles:read"}, "current_password": "Rotated!Pass#2027",
	}, sessACookies, hdrsA)
	var pat2 struct {
		RawToken string `json:"plaintext_token"`
	}
	_ = json.Unmarshal(pat2Raw, &pat2)

	usrResetResp, usrResetRaw := doJSONReq(t, http.MethodPatch, ts.URL+"/api/admin/users/"+comp.User.ID, map[string]any{
		"password":         "ResetByUsers!Pass#2028",
		"current_password": "Rotated!Pass#2027",
	}, sessACookies, hdrsA)
	if usrResetResp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH /api/admin/users/{id} password reset = %d %s", usrResetResp.StatusCode, string(usrResetRaw))
	}
	// Note: PATCH /api/admin/users/{id} revokes all sessions & api_tokens for target user!
	patAfterUsersResp, _ := doJSONReq(t, http.MethodGet, ts.URL+"/api/admin/articles", nil, nil, map[string]string{
		"Authorization": "Bearer " + pat2.RawToken,
	})
	if patAfterUsersResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected PAT to be revoked (401) after PATCH /api/admin/users/{id} password reset, got %d", patAfterUsersResp.StatusCode)
	}
}

func TestRound3_SetupConcurrency_MediaOrphans_EditorRBAC_ScaleDTOs_And_CacheVary(t *testing.T) {
	// 1. Setup Wizard concurrency lock (`setupMu`): 10 concurrent POST /api/setup/complete -> exactly ONE 200 OK, nine 404s!
	setupEnv := setupM5TestEnv(t, "/secret-console")
	const workers = 10
	var okCount, notFoundCount atomic.Int32
	var wg sync.WaitGroup
	startGate := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-startGate
			resp, _ := doJSONReq(t, http.MethodPost, setupEnv.ts.URL+"/api/setup/complete", map[string]any{
				"site_title":         "Concurrent Setup",
				"default_locale":     "en",
				"timezone":           "UTC",
				"admin_username":     "founder",
				"admin_email":        "founder@example.com",
				"admin_display_name": "Founder",
				"admin_password":     "Founder!Pass#2026",
			}, nil, map[string]string{"Origin": setupEnv.ts.URL})
			switch resp.StatusCode {
			case http.StatusOK:
				okCount.Add(1)
			case http.StatusNotFound:
				notFoundCount.Add(1)
			default:
				t.Errorf("worker %d unexpected status %d", idx, resp.StatusCode)
			}
		}(i)
	}
	close(startGate)
	wg.Wait()
	if okCount.Load() != 1 || notFoundCount.Load() != workers-1 {
		t.Fatalf("expected exactly 1 setup/complete 200 OK and %d 404s, got ok=%d 404=%d", workers-1, okCount.Load(), notFoundCount.Load())
	}

	// Custom AdminPath (`/secret-console`) stealth check: `/admin-index.do` and `/admin/index.do` MUST return 404!
	for _, legacyAdminURL := range []string{"/admin-index.do", "/admin/index.do"} {
		legResp, _ := doJSONReq(t, http.MethodGet, setupEnv.ts.URL+legacyAdminURL, nil, nil, nil)
		if legResp.StatusCode != http.StatusNotFound {
			t.Fatalf("expected 404 on %s when custom AdminPath is set, got %d", legacyAdminURL, legResp.StatusCode)
		}
	}

	// 2. Unified Media Orphans (`nav_items.url`, `book_chapters.url`, `redirects.target_path`), `.mp4`/`.webm` reconcile, and Editor Media/Import RBAC
	env, adminCookies, adminHdrs, _ := setupRound2Env(t)
	csrf := adminHdrs[auth.CSRFHeaderName]

	uploadPNG := func(name string) domain.MediaAsset {
		resp, raw := uploadMultipartMedia(t, env.ts.URL+"/api/admin/media/upload", name, "image/png", makeSmallPNG(t), adminCookies, csrf)
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("upload %s = %d %s", name, resp.StatusCode, string(raw))
		}
		var out struct {
			Asset domain.MediaAsset `json:"asset"`
		}
		_ = json.Unmarshal(raw, &out)
		return out.Asset
	}

	navAsset := uploadPNG("nav-icon.png")
	chapAsset := uploadPNG("chapter-handout.png")
	redirAsset := uploadPNG("legacy-brochure.png")
	orphanAsset := uploadPNG("truly-unused.png")

	// Reference navAsset ONLY in nav_items.url (and verify auto-assigned sort_order on POST /api/admin/nav!)
	nav1Resp, nav1Raw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/nav", map[string]any{
		"title": "Media Brochure", "url": navAsset.PublicURL, "is_enabled": true,
	}, adminCookies, adminHdrs)
	if nav1Resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /api/admin/nav #1 = %d %s", nav1Resp.StatusCode, string(nav1Raw))
	}
	nav2Resp, nav2Raw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/nav", map[string]any{
		"title": "Archives", "url": "/archives", "is_enabled": true,
	}, adminCookies, adminHdrs)
	if nav2Resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /api/admin/nav #2 = %d %s", nav2Resp.StatusCode, string(nav2Raw))
	}
	var nav1DTO, nav2DTO struct {
		Item domain.NavItem `json:"item"`
	}
	_ = json.Unmarshal(nav1Raw, &nav1DTO)
	_ = json.Unmarshal(nav2Raw, &nav2DTO)
	if nav2DTO.Item.SortOrder <= nav1DTO.Item.SortOrder {
		t.Fatalf("expected auto-incremented nav sort_order (%d > %d)", nav2DTO.Item.SortOrder, nav1DTO.Item.SortOrder)
	}

	// Reference chapAsset ONLY in book_chapters.url (and verify auto-assigned sort_order on POST /api/admin/books!)
	bk1Resp, bk1Raw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/books", map[string]any{
		"title": "Go Handbook", "slug": "go-handbook", "status": "published",
	}, adminCookies, adminHdrs)
	bk2Resp, bk2Raw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/books", map[string]any{
		"title": "Security Guide", "slug": "security-guide", "status": "published",
	}, adminCookies, adminHdrs)
	if bk1Resp.StatusCode != http.StatusCreated || bk2Resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /api/admin/books failed: %d / %d", bk1Resp.StatusCode, bk2Resp.StatusCode)
	}
	var bk1DTO, bk2DTO struct {
		Book domain.Book `json:"book"`
	}
	_ = json.Unmarshal(bk1Raw, &bk1DTO)
	_ = json.Unmarshal(bk2Raw, &bk2DTO)
	if bk2DTO.Book.SortOrder <= bk1DTO.Book.SortOrder {
		t.Fatalf("expected auto-incremented book sort_order (%d > %d)", bk2DTO.Book.SortOrder, bk1DTO.Book.SortOrder)
	}
	_, err := env.store.ExecContext(env.ctx, `INSERT INTO book_chapters (
		id, book_id, parent_id, title, url, depth, sort_order
	) VALUES (?, ?, '', 'Handout PDF', ?, 0, 1)`, id.Next(), bk1DTO.Book.ID, chapAsset.PublicURL)
	if err != nil {
		t.Fatalf("insert book_chapter: %v", err)
	}

	// Reference redirAsset ONLY in redirects.target_path
	redirResp, redirRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/redirects", map[string]any{
		"source_path": "/old-brochure.png",
		"target_path": redirAsset.PublicURL,
		"status_code": 301,
	}, adminCookies, adminHdrs)
	if redirResp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /api/admin/redirects = %d %s", redirResp.StatusCode, string(redirRaw))
	}

	// Verify GET /api/admin/media sees ref_count >= 1 on navAsset, chapAsset, redirAsset and orphan_count == 1 (orphanAsset)
	medListResp, medListRaw := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/admin/media?page_size=20", nil, adminCookies, nil)
	if medListResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/admin/media = %d %s", medListResp.StatusCode, string(medListRaw))
	}
	var medPage struct {
		OrphanCount int `json:"orphan_count"`
		AllTotal    int `json:"all_total"`
		Items       []struct {
			ID       string `json:"id"`
			RefCount int    `json:"ref_count"`
			IsOrphan bool   `json:"is_orphan"`
		} `json:"items"`
	}
	if err := json.Unmarshal(medListRaw, &medPage); err != nil {
		t.Fatal(err)
	}
	if medPage.OrphanCount != 1 {
		t.Fatalf("expected orphan_count=1 (only truly-unused.png), got %d (%s)", medPage.OrphanCount, string(medListRaw))
	}
	for _, it := range medPage.Items {
		switch it.ID {
		case navAsset.ID, chapAsset.ID, redirAsset.ID:
			if it.RefCount < 1 || it.IsOrphan {
				t.Fatalf("expected referenced asset %s to have ref_count>=1 and is_orphan=false, got %+v", it.ID, it)
			}
		case orphanAsset.ID:
			if it.RefCount != 0 || !it.IsOrphan {
				t.Fatalf("expected orphanAsset %s to have ref_count=0 and is_orphan=true, got %+v", it.ID, it)
			}
		}
	}

	// Clean orphans (`POST /api/admin/media/orphans?delete=1`): deletes ONLY orphanAsset while preserving nav/chapter/redirect assets!
	cleanResp, cleanRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/media/orphans?delete=1", map[string]any{}, adminCookies, adminHdrs)
	if cleanResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/admin/media/orphans?delete=1 = %d %s", cleanResp.StatusCode, string(cleanRaw))
	}
	var remCnt int
	_ = env.store.QueryRowContext(env.ctx, `SELECT COUNT(*) FROM media_assets WHERE id IN (?, ?, ?)`, navAsset.ID, chapAsset.ID, redirAsset.ID).Scan(&remCnt)
	if remCnt != 3 {
		t.Fatalf("expected all 3 nav/chapter/redirect referenced assets to survive orphan cleanup, got %d", remCnt)
	}

	// Reconcile `.mp4` and `.webm` video files on disk
	if err := os.WriteFile(filepath.Join(env.uploadDir, "clip.mp4"), []byte{0x00, 0x00, 0x00, 0x18, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm'}, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(env.uploadDir, "clip.webm"), []byte{0x1A, 0x45, 0xDF, 0xA3, 0x01, 0x00, 0x00, 0x00}, 0o640); err != nil {
		t.Fatal(err)
	}
	recResp, recRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/media/reconcile", map[string]any{}, adminCookies, adminHdrs)
	if recResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/admin/media/reconcile = %d %s", recResp.StatusCode, string(recRaw))
	}
	var videoCnt int
	_ = env.store.QueryRowContext(env.ctx, `SELECT COUNT(*) FROM media_assets WHERE object_key IN ('clip.mp4', 'clip.webm')`).Scan(&videoCnt)
	if videoCnt != 2 {
		t.Fatalf("expected .mp4 and .webm to be imported by reconcile, got %d", videoCnt)
	}

	// 3. Editor Media Scoping & Batch Import Category/Cover RBAC
	catResp, catRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/categories", map[string]any{
		"name": "Engineering", "slug": "engineering",
	}, adminCookies, adminHdrs)
	if catResp.StatusCode != http.StatusCreated {
		t.Fatalf("create category = %d %s", catResp.StatusCode, string(catRaw))
	}

	_, _ = doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/users", map[string]any{
		"username": "ed_r3", "email": "ed_r3@example.com", "display_name": "Editor R3",
		"role": "editor", "password": "Ed1t0r!Pass#2026",
	}, adminCookies, adminHdrs)
	edLogin, edLoginRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/auth/login", map[string]any{
		"ident": "ed_r3", "password": "Ed1t0r!Pass#2026",
	}, nil, map[string]string{"Origin": env.ts.URL})
	var edAuth struct {
		CSRFToken string `json:"csrf_token"`
	}
	_ = json.Unmarshal(edLoginRaw, &edAuth)
	edCookies := edLogin.Cookies()

	// Editor uploads 1 image -> GET /api/admin/media scopes both items and all_total to editor (1), while filtered query preserves all_total=1 and admin sees global all_total >= 6!
	edUpResp, _ := uploadMultipartMedia(t, env.ts.URL+"/api/admin/media/upload", "editor-own.png", "image/png", makeSmallPNG(t), edCookies, edAuth.CSRFToken)
	if edUpResp.StatusCode != http.StatusCreated {
		t.Fatalf("editor media upload failed: %d", edUpResp.StatusCode)
	}
	edMedResp, edMedRaw := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/admin/media?mime=video", nil, edCookies, nil)
	var edMedPage struct {
		Total    int `json:"total"`
		AllTotal int `json:"all_total"`
	}
	_ = json.Unmarshal(edMedRaw, &edMedPage)
	if edMedResp.StatusCode != http.StatusOK || edMedPage.Total != 0 || edMedPage.AllTotal != 1 {
		t.Fatalf("expected editor video-filtered media list (total=0, all_total=1), got status=%d %+v", edMedResp.StatusCode, edMedPage)
	}
	admMedResp, admMedRaw := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/admin/media?mime=video", nil, adminCookies, nil)
	var admMedPage struct {
		Total    int `json:"total"`
		AllTotal int `json:"all_total"`
	}
	_ = json.Unmarshal(admMedRaw, &admMedPage)
	if admMedResp.StatusCode != http.StatusOK || admMedPage.Total != 2 || admMedPage.AllTotal < 6 {
		t.Fatalf("expected admin video-filtered media list (total=2, all_total>=6), got status=%d %+v", admMedResp.StatusCode, admMedPage)
	}

	// Editor batch import with non-existent category MUST NOT auto-create global categories!
	badCatZip := buildMemoryZip(t, map[string]string{
		"post.md": "---\ntitle: Unauthorized Cat\nslug: unauth-cat-post\ncategory: BrandNewCategory\n---\nHello world\n",
	})
	badImpCode, _ := uploadBatchZip(t, env.ts.URL+"/api/admin/articles/import", badCatZip, edCookies, edAuth.CSRFToken)
	if badImpCode != http.StatusOK {
		t.Fatalf("editor batch import status = %d", badImpCode)
	}
	var rogueCatCnt int
	_ = env.store.QueryRowContext(env.ctx, `SELECT COUNT(*) FROM categories WHERE LOWER(name) = 'brandnewcategory'`).Scan(&rogueCatCnt)
	if rogueCatCnt != 0 {
		t.Fatalf("editor batch import must NOT auto-create categories, got count=%d", rogueCatCnt)
	}
	var unauthArtCatID string
	_ = env.store.QueryRowContext(env.ctx, `SELECT category_id FROM articles WHERE slug = 'unauth-cat-post'`).Scan(&unauthArtCatID)
	if unauthArtCatID != "" {
		t.Fatalf("expected unauth-cat-post category_id='', got %q", unauthArtCatID)
	}

	// Editor batch import with existing category + javascript: cover URL -> 200 OK with cover sanitized to empty!
	goodCatZip := buildMemoryZip(t, map[string]string{
		"safe.md": "---\ntitle: Editor Imported Post\nslug: editor-imported-post\ncategory: Engineering\ncover: javascript:alert(1)\ntags: [go, legacy-go]\n---\nSafe body\n",
	})
	goodImpCode, goodImpRaw := uploadBatchZip(t, env.ts.URL+"/api/admin/articles/import", goodCatZip, edCookies, edAuth.CSRFToken)
	if goodImpCode != http.StatusOK {
		t.Fatalf("expected 200 when editor batch-imports existing category, got %d %s", goodImpCode, string(goodImpRaw))
	}

	// 4. Admin Articles `category_name` DTO + Set-based Multi-Tag Merge
	artListResp, artListRaw := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/admin/articles?q=Editor", nil, adminCookies, nil)
	if artListResp.StatusCode != http.StatusOK || !strings.Contains(string(artListRaw), `"category_name":"Engineering"`) {
		t.Fatalf("expected category_name in GET /api/admin/articles, got %d %s", artListResp.StatusCode, string(artListRaw))
	}
	var impArt domain.RawArticle
	impArt, err = sqlx.Get[domain.RawArticle](env.ctx, env.store, `SELECT * FROM articles WHERE slug = 'editor-imported-post'`)
	if err != nil {
		t.Fatal(err)
	}
	if impArt.CoverURL != "" {
		t.Fatalf("expected javascript: frontmatter cover URL to be stripped, got %q", impArt.CoverURL)
	}
	artDetResp, artDetRaw := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/admin/articles/"+impArt.ID, nil, adminCookies, nil)
	if artDetResp.StatusCode != http.StatusOK || !strings.Contains(string(artDetRaw), `"category_name":"Engineering"`) {
		t.Fatalf("expected category_name in GET /api/admin/articles/{id}, got %d %s", artDetResp.StatusCode, string(artDetRaw))
	}

	var goTagID, legTagID string
	_ = env.store.QueryRowContext(env.ctx, `SELECT id FROM tags WHERE slug = 'go'`).Scan(&goTagID)
	_ = env.store.QueryRowContext(env.ctx, `SELECT id FROM tags WHERE slug = 'legacy-go'`).Scan(&legTagID)
	mergeResp, mergeRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/tags/merge", map[string]any{
		"source_ids": []string{legTagID},
		"target_id":  goTagID,
	}, adminCookies, adminHdrs)
	if mergeResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/admin/tags/merge = %d %s", mergeResp.StatusCode, string(mergeRaw))
	}
	var remLegTag int
	_ = env.store.QueryRowContext(env.ctx, `SELECT COUNT(*) FROM tags WHERE id = ?`, legTagID).Scan(&remLegTag)
	if remLegTag != 0 {
		t.Fatalf("expected merged source tag to be deleted, got count=%d", remLegTag)
	}

	// 5. Feeds & /api/v1/i18n Vary headers + GET /api/v1/comments credentialed Cache-Control: private, no-store
	for _, p := range []string{"/rss.xml", "/atom.xml", "/feed.json", "/podcast.xml", "/api/v1/i18n"} {
		rResp, _ := doJSONReq(t, http.MethodGet, env.ts.URL+p, nil, nil, nil)
		vary := rResp.Header.Get("Vary")
		if !strings.Contains(vary, "Accept-Language") || !strings.Contains(vary, "Cookie") {
			t.Fatalf("expected Vary on %s to contain Accept-Language and Cookie, got %q", p, vary)
		}
	}

	pubArtResp, pubArtRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/articles", map[string]any{
		"title": "Comments Cache Post", "slug": "comments-cache-post", "content_md": "hi", "status": "published", "allow_comment": true,
	}, adminCookies, adminHdrs)
	if pubArtResp.StatusCode != http.StatusCreated {
		t.Fatalf("create published article = %d %s", pubArtResp.StatusCode, string(pubArtRaw))
	}
	var pubArtDTO struct {
		Article domain.RawArticle `json:"article"`
	}
	_ = json.Unmarshal(pubArtRaw, &pubArtDTO)

	anonCmtResp, _ := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/v1/comments?target_type=article&target_id="+pubArtDTO.Article.ID, nil, nil, nil)
	if anonCmtResp.StatusCode != http.StatusOK {
		t.Fatalf("anonymous GET /api/v1/comments = %d", anonCmtResp.StatusCode)
	}
	if vary := anonCmtResp.Header.Get("Vary"); !strings.Contains(vary, "Cookie") || !strings.Contains(vary, "X-Volo-Unlock-State") {
		t.Fatalf("expected GET /api/v1/comments Vary to include Cookie and X-Volo-Unlock-State, got %q", vary)
	}
	authCmtResp, _ := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/v1/comments?target_type=article&target_id="+pubArtDTO.Article.ID, nil, adminCookies, nil)
	if cc := authCmtResp.Header.Get("Cache-Control"); !strings.Contains(cc, "private") || !strings.Contains(cc, "no-store") {
		t.Fatalf("expected credentialed GET /api/v1/comments Cache-Control: private, no-store, got %q", cc)
	}
	unlockCmtResp, _ := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/v1/comments?target_type=article&target_id="+pubArtDTO.Article.ID, nil, nil, map[string]string{
		"X-Volo-Unlock-State": "tok_example",
	})
	if cc := unlockCmtResp.Header.Get("Cache-Control"); !strings.Contains(cc, "private") || !strings.Contains(cc, "no-store") {
		t.Fatalf("expected X-Volo-Unlock-State GET /api/v1/comments Cache-Control: private, no-store, got %q", cc)
	}
}
