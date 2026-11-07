package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/Willh92/Volo-go/internal/config"
	"github.com/Willh92/Volo-go/internal/db/sqlx"
	"github.com/Willh92/Volo-go/internal/domain"
	"github.com/Willh92/Volo-go/internal/guest"
	"github.com/Willh92/Volo-go/internal/repo"
	"github.com/Willh92/Volo-go/internal/security"
	"github.com/Willh92/Volo-go/pkg/id"
)

const (
	SessionCookieName = "__volo_sid"
	CSRFHeaderName    = "X-CSRF-Token"
	APITokenPrefix    = "volo_pat_"
	SessionTTL        = 14 * 24 * time.Hour

	ScopeArticlesRead  = "articles:read"
	ScopeArticlesWrite = "articles:write"
	ScopePagesWrite    = "pages:write"
	ScopeMediaWrite    = "media:write"
	ScopeBackupRead    = "backup:read"
)

var (
	ErrInvalidCredentials = errors.New("auth: invalid username/email or password")
	ErrUnauthorized       = errors.New("auth: authentication required")
	ErrForbidden          = errors.New("auth: insufficient permissions")
	ErrVerificationCode   = errors.New("auth: invalid or expired email verification code")
)

var validAPITokenScopes = map[string]bool{
	ScopeArticlesRead:  true,
	ScopeArticlesWrite: true,
	ScopePagesWrite:    true,
	ScopeMediaWrite:    true,
	ScopeBackupRead:    true,
}

type pendingEmailCode struct {
	emailHash string
	codeHash  string
	attempts  int
	expiresAt time.Time
}

// LoginResult describes the outcome of staff login (either immediate session or 2FA challenge).
type LoginResult struct {
	Requires2FA    bool                `json:"requires_2fa"`
	ChallengeToken string              `json:"challenge_token,omitempty"`
	User           *domain.User        `json:"user,omitempty"`
	Session        *domain.UserSession `json:"session,omitempty"`
	RawToken       string              `json:"-"`
	CSRFToken      string              `json:"csrf_token,omitempty"`
}

// Service manages Sovereign Accounts (admin / editor / reader), RBAC isolation, silent Argon2id
// password upgrades, TOTP + Recovery Codes, WebAuthn Passkeys, kickable device sessions,
// fine-grained API tokens, and one-click guest comment claiming.
type Service struct {
	store       *sqlx.Store
	keyRing     *config.KeyRing
	settingsMgr *repo.SettingsManager
	publicURL   string
	replayGuard *TOTPReplayGuard

	codeMu       sync.Mutex
	pendingCodes map[string]*pendingEmailCode
}

var (
	dummyHashOnce     sync.Once
	dummyPasswordHash string
)

func verifyDummyPassword(rawPassword string) {
	dummyHashOnce.Do(func() {
		dummyPasswordHash, _ = HashPassword("volo-timing-equalizer-dummy-secret")
	})
	if dummyPasswordHash != "" {
		VerifyPassword(rawPassword, dummyPasswordHash, "")
	}
}

func NewService(store *sqlx.Store, keyRing *config.KeyRing, settingsMgr *repo.SettingsManager, publicURL string) *Service {
	return &Service{
		store:        store,
		keyRing:      keyRing,
		settingsMgr:  settingsMgr,
		publicURL:    publicURL,
		replayGuard:  NewTOTPReplayGuard(),
		pendingCodes: make(map[string]*pendingEmailCode),
	}
}

// --- Sovereign RBAC & Collaborator Isolation Guards ---

// RequireAdmin ensures actor is an active sovereign administrator.
// Editors and Readers are strictly rejected with ErrForbidden.
func RequireAdmin(actor *domain.User) error {
	if actor == nil {
		return ErrUnauthorized
	}
	if actor.Status != domain.UserStatusActive || actor.Role != domain.RoleAdmin {
		return ErrForbidden
	}
	return nil
}

// RequireStaff ensures actor is an active Admin or Editor.
// Readers are physically blocked from ALL admin console & /api/admin/* endpoints with ErrForbidden!
func RequireStaff(actor *domain.User) error {
	if actor == nil {
		return ErrUnauthorized
	}
	if actor.Status != domain.UserStatusActive {
		return ErrForbidden
	}
	if actor.Role != domain.RoleAdmin && actor.Role != domain.RoleEditor {
		return ErrForbidden
	}
	return nil
}

// CanManageArticle enforces author isolation for Editors:
// - Admin can manage any article.
// - Editor can ONLY manage articles where articleAuthorID == actor.ID.
// - Reader can NEVER manage articles.
func CanManageArticle(actor *domain.User, articleAuthorID string) bool {
	if actor == nil || actor.Status != domain.UserStatusActive {
		return false
	}
	if actor.Role == domain.RoleAdmin {
		return true
	}
	if actor.Role == domain.RoleEditor {
		return actor.ID != "" && actor.ID == articleAuthorID
	}
	return false
}

// AuthorizeAndScrubCommentForStaff enforces strict collaborator comment isolation & PII minimization:
// - Admin can access and moderate all comments.
// - Editor can ONLY access/moderate comments on articles authored by themselves (articleAuthorID == actor.ID),
//   and sensitive PII (NotifyEmailCiphertext, ClientIP) is stripped so Editors only see IPMasked & GeoRegion!
// - Reader is strictly forbidden.
func AuthorizeAndScrubCommentForStaff(actor *domain.User, c domain.Comment, articleAuthorID string) (domain.Comment, error) {
	if err := RequireStaff(actor); err != nil {
		return domain.Comment{}, err
	}
	if actor.Role == domain.RoleAdmin {
		return c, nil
	}
	// Editor: forbidden on standalone pages or other authors' articles!
	if c.TargetType != domain.TargetTypeArticle || articleAuthorID == "" || articleAuthorID != actor.ID {
		return domain.Comment{}, ErrForbidden
	}
	c.NotifyEmailCiphertext = ""
	c.ClientIP = ""
	return c, nil
}

// --- Staff Login (Admin & Editor), Silent Argon2id Upgrade & TOTP 2FA ---

// LoginStaff authenticates an Admin or Editor account, physically rejecting Reader accounts!
func (s *Service) LoginStaff(
	ctx context.Context,
	ident string,
	rawPassword string,
	clientIP netip.Addr,
	geoRegion string,
	userAgent string,
	trusted2FACookie string,
) (*LoginResult, error) {
	u, err := s.findUserByIdent(ctx, ident)
	if err != nil {
		verifyDummyPassword(rawPassword)
		return nil, ErrInvalidCredentials
	}

	matched, needsUpgrade := VerifyPassword(rawPassword, u.PasswordHash, u.LegacyHash)
	if !matched {
		return nil, ErrInvalidCredentials
	}
	// Physical boundary: Reader accounts CANNOT log in to the staff console!
	if u.Role != domain.RoleAdmin && u.Role != domain.RoleEditor {
		return nil, ErrForbidden
	}
	if u.Status != domain.UserStatusActive {
		return nil, ErrForbidden
	}

	// Silent Legacy Password Upgrade (PBKDF2 / bolomd5x2 -> Argon2id) on first login!
	if needsUpgrade {
		if newHash, hErr := HashPassword(rawPassword); hErr == nil {
			_, _ = s.store.ExecContext(ctx,
				`UPDATE users SET password_hash = ?, legacy_hash = '', updated_at = ? WHERE id = ?`,
				newHash, time.Now().UnixMilli(), u.ID)
			u.PasswordHash = newHash
			u.LegacyHash = ""
		}
	}

	hasTOTP, err := s.userHasTOTPEnabled(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	if hasTOTP && !s.verifyTrusted2FATicket(trusted2FACookie, u.ID, u.PasswordHash, userAgent) {
		ipBucket := security.RateLimitBucketKey(clientIP)
		chal, cErr := SealTwoFactorChallenge(s.keyRing, u.ID, ipBucket, userAgent)
		if cErr != nil {
			return nil, cErr
		}
		return &LoginResult{
			Requires2FA:    true,
			ChallengeToken: chal,
		}, nil
	}

	sess, rawTok, err := s.CreateSession(ctx, u.ID, clientIP, geoRegion, userAgent, 0)
	if err != nil {
		return nil, err
	}
	uSanitized := sanitizeUser(u)
	return &LoginResult{
		Requires2FA: false,
		User:        &uSanitized,
		Session:     sess,
		RawToken:    rawTok,
		CSRFToken:   s.ComputeCSRFToken(sess.ID),
	}, nil
}

// CompleteStaff2FA verifies Step-2 TOTP (or single-use recovery code) against an AES-256-GCM
// sealed challenge token and only THEN mints an authenticated staff session!
func (s *Service) CompleteStaff2FA(
	ctx context.Context,
	challengeToken string,
	codeOrRecovery string,
	rememberDevice bool,
	clientIP netip.Addr,
	geoRegion string,
	userAgent string,
) (res *LoginResult, trustedDeviceCookie string, err error) {
	ipBucket := security.RateLimitBucketKey(clientIP)
	claims, err := OpenTwoFactorChallenge(s.keyRing, challengeToken, ipBucket, userAgent)
	if err != nil {
		return nil, "", err
	}

	u, err := sqlx.Get[domain.User](ctx, s.store, `SELECT * FROM users WHERE id = ?`, claims.UserID)
	if err != nil || u.Status != domain.UserStatusActive {
		return nil, "", ErrUnauthorized
	}

	totpCred, err := sqlx.Get[domain.UserCredential](ctx, s.store,
		`SELECT * FROM user_credentials WHERE user_id = ? AND kind = 'totp'`, u.ID)
	if err != nil {
		return nil, "", ErrTOTPInvalid
	}
	totpSecret, err := s.keyRing.OpenString(totpCred.SecretCiphertext, "cred:totp:"+u.ID)
	if err != nil || totpSecret == "" {
		return nil, "", ErrTOTPInvalid
	}

	winSteps := 1
	rememberDays := 7
	if s.settingsMgr != nil {
		sec := s.settingsMgr.Snapshot().Security
		if sec.TOTPWindowSteps >= 0 && sec.TOTPWindowSteps <= 3 {
			winSteps = sec.TOTPWindowSteps
		}
		if sec.TOTPRememberDays > 0 {
			rememberDays = sec.TOTPRememberDays
		}
	}

	now := time.Now()
	verified := false
	if vErr := VerifyTOTPCode(u.ID, totpSecret, codeOrRecovery, now, winSteps, s.replayGuard); vErr == nil {
		verified = true
		_, _ = s.store.ExecContext(ctx, `UPDATE user_credentials SET last_used_at = ? WHERE id = ?`, now.UnixMilli(), totpCred.ID)
	} else if errors.Is(vErr, ErrTOTPReplay) {
		return nil, "", vErr
	} else {
		// Attempt single-use Recovery Code consumption inside a transaction
		consumed := false
		_ = s.store.WithTx(ctx, func(tx *sqlx.Tx) error {
			rcRow, qErr := sqlx.Get[domain.UserCredential](ctx, tx,
				`SELECT * FROM user_credentials WHERE user_id = ? AND kind = 'recovery_codes'`, u.ID)
			if qErr != nil {
				return qErr
			}
			var hashes []string
			if json.Unmarshal([]byte(rcRow.MetaJSON), &hashes) != nil {
				return ErrTOTPInvalid
			}
			rem, ok := ConsumeRecoveryCodeHash(codeOrRecovery, hashes)
			if !ok {
				return ErrTOTPInvalid
			}
			remJSON, _ := json.Marshal(rem)
			_, uErr := tx.ExecContext(ctx,
				`UPDATE user_credentials SET meta_json = ?, last_used_at = ? WHERE id = ?`,
				string(remJSON), now.UnixMilli(), rcRow.ID)
			if uErr == nil {
				consumed = true
			}
			return uErr
		})
		if consumed {
			verified = true
		}
	}
	if !verified {
		return nil, "", ErrTOTPInvalid
	}

	var totpUntil int64
	if rememberDevice {
		totpUntil = now.Add(time.Duration(rememberDays) * 24 * time.Hour).UnixMilli()
		trustedDeviceCookie, _ = s.mintTrusted2FATicket(u.ID, u.PasswordHash, userAgent, totpUntil)
	}

	sess, rawTok, err := s.CreateSession(ctx, u.ID, clientIP, geoRegion, userAgent, totpUntil)
	if err != nil {
		return nil, "", err
	}
	uSanitized := sanitizeUser(u)
	return &LoginResult{
		Requires2FA: false,
		User:        &uSanitized,
		Session:     sess,
		RawToken:    rawTok,
		CSRFToken:   s.ComputeCSRFToken(sess.ID),
	}, trustedDeviceCookie, nil
}

// EnrollTOTP persists an encrypted TOTP secret and 8 hashed recovery codes for userID after verifying code.
func (s *Service) EnrollTOTP(ctx context.Context, userID string, secretBase32 string, verificationCode string) (recoveryPlain []string, err error) {
	if err := VerifyTOTPCode(userID, secretBase32, verificationCode, time.Now(), 1, s.replayGuard); err != nil {
		return nil, err
	}
	sealedSecret, err := s.keyRing.SealString(secretBase32, "cred:totp:"+userID)
	if err != nil {
		return nil, err
	}
	plainCodes, codeHashes, err := GenerateRecoveryCodes()
	if err != nil {
		return nil, err
	}
	hashesJSON, _ := json.Marshal(codeHashes)
	now := time.Now().UnixMilli()

	err = s.store.WithTx(ctx, func(tx *sqlx.Tx) error {
		if _, dErr := tx.ExecContext(ctx,
			`DELETE FROM user_credentials WHERE user_id = ? AND kind IN ('totp', 'recovery_codes')`, userID); dErr != nil {
			return dErr
		}
		if _, iErr := tx.ExecContext(ctx,
			`INSERT INTO user_credentials (id, user_id, kind, name, credential_id, public_key_spki, cose_alg, sign_count, secret_ciphertext, meta_json, last_used_at, created_at)
			 VALUES (?, ?, 'totp', 'Authenticator App', ?, '', 0, 0, ?, '{}', ?, ?)`,
			id.Next(), userID, "totp:"+userID, sealedSecret, now, now); iErr != nil {
			return iErr
		}
		_, rErr := tx.ExecContext(ctx,
			`INSERT INTO user_credentials (id, user_id, kind, name, credential_id, public_key_spki, cose_alg, sign_count, secret_ciphertext, meta_json, last_used_at, created_at)
			 VALUES (?, ?, 'recovery_codes', 'Recovery Codes', ?, '', 0, 0, '', ?, 0, ?)`,
			id.Next(), userID, "recovery:"+userID, string(hashesJSON), now)
		return rErr
	})
	if err != nil {
		return nil, err
	}
	return plainCodes, nil
}

// --- WebAuthn Multi-Device Named Passkeys ---

func (s *Service) RegisterPasskey(
	ctx context.Context,
	userID string,
	deviceName string,
	stateToken string,
	clientDataJSON []byte,
	attestationObjectCBOR []byte,
) (*domain.UserCredential, error) {
	verified, err := FinishPasskeyRegistration(s.keyRing, stateToken, userID, clientDataJSON, attestationObjectCBOR)
	if err != nil {
		return nil, err
	}
	name := strings.TrimSpace(deviceName)
	if name == "" {
		name = "Passkey"
	}
	now := time.Now().UnixMilli()
	rec := domain.UserCredential{
		ID:            id.Next(),
		UserID:        userID,
		Kind:          "passkey",
		Name:          name,
		CredentialID:  verified.CredentialIDB64URL,
		PublicKeySPKI: verified.PublicKeySPKIB64,
		COSEAlg:       verified.COSEAlg,
		SignCount:     verified.SignCount,
		MetaJSON:      "{}",
		LastUsedAt:    now,
		CreatedAt:     now,
	}
	if _, err := s.store.ExecContext(ctx,
		`INSERT INTO user_credentials (id, user_id, kind, name, credential_id, public_key_spki, cose_alg, sign_count, secret_ciphertext, meta_json, last_used_at, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, '', '{}', ?, ?)`,
		rec.ID, rec.UserID, rec.Kind, rec.Name, rec.CredentialID, rec.PublicKeySPKI, rec.COSEAlg, rec.SignCount, rec.LastUsedAt, rec.CreatedAt,
	); err != nil {
		return nil, err
	}
	return &rec, nil
}

func (s *Service) LoginWithPasskey(
	ctx context.Context,
	stateToken string,
	credentialIDB64URL string,
	clientDataJSON []byte,
	authenticatorData []byte,
	signature []byte,
	clientIP netip.Addr,
	geoRegion string,
	userAgent string,
) (*LoginResult, error) {
	cred, err := sqlx.Get[domain.UserCredential](ctx, s.store,
		`SELECT * FROM user_credentials WHERE kind = 'passkey' AND credential_id = ?`,
		strings.TrimSpace(credentialIDB64URL))
	if err != nil {
		return nil, ErrPasskeyVerifyFailed
	}

	newSignCount, err := FinishPasskeyAssertion(
		s.keyRing,
		stateToken,
		cred.PublicKeySPKI,
		cred.COSEAlg,
		cred.SignCount,
		clientDataJSON,
		authenticatorData,
		signature,
	)
	if err != nil {
		return nil, err
	}

	u, err := sqlx.Get[domain.User](ctx, s.store, `SELECT * FROM users WHERE id = ?`, cred.UserID)
	if err != nil || u.Status != domain.UserStatusActive || (u.Role != domain.RoleAdmin && u.Role != domain.RoleEditor) {
		return nil, ErrForbidden
	}

	now := time.Now().UnixMilli()
	_, _ = s.store.ExecContext(ctx,
		`UPDATE user_credentials SET sign_count = ?, last_used_at = ? WHERE id = ?`,
		newSignCount, now, cred.ID)

	sess, rawTok, err := s.CreateSession(ctx, u.ID, clientIP, geoRegion, userAgent, now+int64(SessionTTL/time.Millisecond))
	if err != nil {
		return nil, err
	}
	uSanitized := sanitizeUser(u)
	return &LoginResult{
		Requires2FA: false,
		User:        &uSanitized,
		Session:     sess,
		RawToken:    rawTok,
		CSRFToken:   s.ComputeCSRFToken(sess.ID),
	}, nil
}

// --- Reader Email-Verified Registration, Reader Login Isolation & Guest Comment Claim ---

// IssueReaderVerificationCode generates a 6-digit email verification code (10m TTL, max 5 attempts).
func (s *Service) IssueReaderVerificationCode(email string) (string, error) {
	normEmail := strings.ToLower(strings.TrimSpace(email))
	if !strings.Contains(normEmail, "@") || len(normEmail) > 190 {
		return "", errors.New("auth: invalid email address")
	}
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	num := (uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])) % 1_000_000
	code := fmt.Sprintf("%06d", num)

	emailSum := sha256.Sum256([]byte(normEmail))
	codeSum := sha256.Sum256([]byte(normEmail + ":" + code))

	const maxPendingEmailCodes = 2048
	key := hex.EncodeToString(emailSum[:])
	s.codeMu.Lock()
	if _, exists := s.pendingCodes[key]; !exists && len(s.pendingCodes) >= maxPendingEmailCodes {
		now := time.Now()
		for k, v := range s.pendingCodes {
			if now.After(v.expiresAt) {
				delete(s.pendingCodes, k)
			}
		}
		for k := range s.pendingCodes {
			if len(s.pendingCodes) < maxPendingEmailCodes {
				break
			}
			delete(s.pendingCodes, k)
		}
	}
	s.pendingCodes[key] = &pendingEmailCode{
		emailHash: key,
		codeHash:  hex.EncodeToString(codeSum[:]),
		attempts:  0,
		expiresAt: time.Now().Add(10 * time.Minute),
	}
	s.codeMu.Unlock()
	return code, nil
}

// RegisterReader verifies the 6-digit email code and creates an isolated role="reader" account.
func (s *Service) RegisterReader(
	ctx context.Context,
	username string,
	email string,
	rawPassword string,
	verificationCode string,
	preferredLocale string,
) (*domain.User, error) {
	normEmail := strings.ToLower(strings.TrimSpace(email))
	uname := strings.TrimSpace(username)
	if len(uname) < 2 || len(uname) > 48 {
		return nil, errors.New("auth: username must be 2-48 characters")
	}
	if err := ValidatePasswordPolicy(rawPassword); err != nil {
		return nil, err
	}

	emailSum := sha256.Sum256([]byte(normEmail))
	key := hex.EncodeToString(emailSum[:])
	candSum := sha256.Sum256([]byte(normEmail + ":" + strings.TrimSpace(verificationCode)))
	candHex := hex.EncodeToString(candSum[:])

	s.codeMu.Lock()
	entry, exists := s.pendingCodes[key]
	if !exists || time.Now().After(entry.expiresAt) || entry.attempts >= 5 {
		delete(s.pendingCodes, key)
		s.codeMu.Unlock()
		return nil, ErrVerificationCode
	}
	entry.attempts++
	if subtle.ConstantTimeCompare([]byte(entry.codeHash), []byte(candHex)) != 1 {
		s.codeMu.Unlock()
		return nil, ErrVerificationCode
	}
	delete(s.pendingCodes, key)
	s.codeMu.Unlock()

	pwHash, err := HashPassword(rawPassword)
	if err != nil {
		return nil, err
	}
	now := time.Now().UnixMilli()
	u := domain.User{
		ID:              id.Next(),
		Username:        uname,
		Email:           normEmail,
		DisplayName:     uname,
		Role:            domain.RoleReader,
		PasswordHash:    pwHash,
		Status:          domain.UserStatusActive,
		EmailVerifiedAt: now,
		PreferredLocale: preferredLocale,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if _, err := s.store.ExecContext(ctx,
		`INSERT INTO users (id, username, email, display_name, avatar_url, bio, website_url, role, password_hash, legacy_hash, status, email_verified_at, preferred_locale, created_at, updated_at)
		 VALUES (?, ?, ?, ?, '', '', '', ?, ?, '', ?, ?, ?, ?, ?)`,
		u.ID, u.Username, u.Email, u.DisplayName, u.Role, u.PasswordHash, u.Status, u.EmailVerifiedAt, u.PreferredLocale, u.CreatedAt, u.UpdatedAt,
	); err != nil {
		return nil, err
	}
	clean := sanitizeUser(u)
	return &clean, nil
}

// LoginReader authenticates a front-office Reader account ONLY.
// SECURITY RED-LINE: Admin and Editor accounts are strictly rejected on public /api/v1/reader/login!
func (s *Service) LoginReader(
	ctx context.Context,
	ident string,
	rawPassword string,
	clientIP netip.Addr,
	geoRegion string,
	userAgent string,
) (*LoginResult, error) {
	u, err := s.findUserByIdent(ctx, ident)
	if err != nil {
		verifyDummyPassword(rawPassword)
		return nil, ErrInvalidCredentials
	}
	matched, needsUpgrade := VerifyPassword(rawPassword, u.PasswordHash, u.LegacyHash)
	if !matched {
		return nil, ErrInvalidCredentials
	}
	// Block Admin and Editor accounts on public reader login endpoint!
	if u.Role != domain.RoleReader {
		return nil, ErrForbidden
	}
	if u.Status != domain.UserStatusActive || u.EmailVerifiedAt <= 0 {
		return nil, ErrForbidden
	}
	if needsUpgrade {
		if newHash, hErr := HashPassword(rawPassword); hErr == nil {
			_, _ = s.store.ExecContext(ctx,
				`UPDATE users SET password_hash = ?, legacy_hash = '', updated_at = ? WHERE id = ?`,
				newHash, time.Now().UnixMilli(), u.ID)
		}
	}
	sess, rawTok, err := s.CreateSession(ctx, u.ID, clientIP, geoRegion, userAgent, 0)
	if err != nil {
		return nil, err
	}
	uSanitized := sanitizeUser(u)
	return &LoginResult{
		User:      &uSanitized,
		Session:   sess,
		RawToken:  rawTok,
		CSRFToken: s.ComputeCSRFToken(sess.ID),
	}, nil
}

// ClaimGuestComments binds historical guest comments matching the visitor's verified HMAC
// GuestPassport (guest_id_hash) to their newly registered Reader account!
// SECURITY RED-LINE: Matches strictly guest_id_hash (NEVER env_fp_hash!).
func (s *Service) ClaimGuestComments(ctx context.Context, reader *domain.User, passport *guest.Identity) (int64, error) {
	if reader == nil || reader.Status != domain.UserStatusActive || reader.ID == "" {
		return 0, ErrUnauthorized
	}
	if passport == nil || strings.TrimSpace(passport.GuestIDHash) == "" {
		return 0, errors.New("auth: verified GuestPassport required to claim historical comments")
	}
	res, err := s.store.ExecContext(ctx,
		`UPDATE comments SET user_id = ?, updated_at = ? WHERE user_id = '' AND guest_id_hash = ?`,
		reader.ID, time.Now().UnixMilli(), passport.GuestIDHash)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// --- Active Device Sessions (user_sessions) & Kick-Offline ---

func (s *Service) CreateSession(
	ctx context.Context,
	userID string,
	clientIP netip.Addr,
	geoRegion string,
	userAgent string,
	totpVerifiedUntil int64,
) (*domain.UserSession, string, error) {
	rawTok, err := id.RandomHex(32)
	if err != nil {
		return nil, "", err
	}
	tokHash := hashOpaqueToken(rawTok)
	now := time.Now().UnixMilli()
	ua := strings.TrimSpace(userAgent)
	if len(ua) > 250 {
		ua = ua[:250]
	}
	sess := domain.UserSession{
		ID:                id.Next(),
		UserID:            userID,
		TokenHash:         tokHash,
		IPMasked:          security.MaskIP(clientIP),
		GeoRegion:         geoRegion,
		UserAgent:         ua,
		TOTPVerifiedUntil: totpVerifiedUntil,
		ExpiresAt:         now + int64(SessionTTL/time.Millisecond),
		LastSeenAt:        now,
		CreatedAt:         now,
	}
	if _, err := s.store.ExecContext(ctx,
		`INSERT INTO user_sessions (id, user_id, token_hash, ip_masked, geo_region, user_agent, totp_verified_until, expires_at, last_seen_at, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sess.ID, sess.UserID, sess.TokenHash, sess.IPMasked, sess.GeoRegion, sess.UserAgent,
		sess.TOTPVerifiedUntil, sess.ExpiresAt, sess.LastSeenAt, sess.CreatedAt,
	); err != nil {
		return nil, "", err
	}
	sess.TokenHash = ""
	return &sess, rawTok, nil
}

func (s *Service) AuthenticateSession(ctx context.Context, rawToken string) (*domain.User, *domain.UserSession, error) {
	trimmed := strings.TrimSpace(rawToken)
	if len(trimmed) < 32 {
		return nil, nil, ErrUnauthorized
	}
	tokHash := hashOpaqueToken(trimmed)
	now := time.Now().UnixMilli()

	sess, err := sqlx.Get[domain.UserSession](ctx, s.store,
		`SELECT * FROM user_sessions WHERE token_hash = ? AND expires_at > ?`, tokHash, now)
	if err != nil {
		return nil, nil, ErrUnauthorized
	}
	u, err := sqlx.Get[domain.User](ctx, s.store, `SELECT * FROM users WHERE id = ?`, sess.UserID)
	if err != nil || u.Status != domain.UserStatusActive {
		return nil, nil, ErrUnauthorized
	}
	if now-sess.LastSeenAt > 60_000 {
		_, _ = s.store.ExecBgContext(ctx, `UPDATE user_sessions SET last_seen_at = ? WHERE id = ?`, now, sess.ID)
	}
	sess.TokenHash = ""
	clean := sanitizeUser(u)
	return &clean, &sess, nil
}

func (s *Service) ListUserSessions(ctx context.Context, userID string) ([]domain.UserSession, error) {
	rows, err := sqlx.Select[domain.UserSession](ctx, s.store,
		`SELECT id, user_id, '' AS token_hash, ip_masked, geo_region, user_agent, totp_verified_until, expires_at, last_seen_at, created_at
		 FROM user_sessions WHERE user_id = ? AND expires_at > ? ORDER BY last_seen_at DESC`,
		userID, time.Now().UnixMilli())
	return rows, err
}

// RevokeSession immediately kicks a specific device session offline.
func (s *Service) RevokeSession(ctx context.Context, actor *domain.User, sessionID string) error {
	if actor == nil {
		return ErrUnauthorized
	}
	if actor.Role == domain.RoleAdmin {
		_, err := s.store.ExecContext(ctx, `DELETE FROM user_sessions WHERE id = ?`, sessionID)
		return err
	}
	_, err := s.store.ExecContext(ctx, `DELETE FROM user_sessions WHERE id = ? AND user_id = ?`, sessionID, actor.ID)
	return err
}

// RevokeOtherSessions kicks all sessions of userID except keepSessionID (e.g. after password rotation).
func (s *Service) RevokeOtherSessions(ctx context.Context, userID string, keepSessionID string) error {
	if strings.TrimSpace(userID) == "" {
		return nil
	}
	if strings.TrimSpace(keepSessionID) == "" {
		_, err := s.store.ExecContext(ctx, `DELETE FROM user_sessions WHERE user_id = ?`, userID)
		return err
	}
	_, err := s.store.ExecContext(ctx, `DELETE FROM user_sessions WHERE user_id = ? AND id <> ?`, userID, keepSessionID)
	return err
}

func (s *Service) ComputeCSRFToken(sessionID string) string {
	key := s.keyRing.SessionCSRFKey()
	mac := hmac.New(sha256.New, key[:])
	mac.Write([]byte("volo-csrf:v1:"))
	mac.Write([]byte(sessionID))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil)[:24])
}

func (s *Service) VerifyCSRFToken(sessionID string, candidate string) bool {
	if sessionID == "" || candidate == "" {
		return false
	}
	expected := s.ComputeCSRFToken(sessionID)
	return subtle.ConstantTimeCompare([]byte(strings.TrimSpace(candidate)), []byte(expected)) == 1
}

// --- Fine-Grained Scoped API Tokens (Obsidian / PicGo / CLI) ---

// CreateAPIToken mints a scoped Personal Access Token ("volo_pat_..."), storing only SHA-256 in DB.
// Editors are strictly barred from requesting "backup:read" scope!
func (s *Service) CreateAPIToken(
	ctx context.Context,
	actor *domain.User,
	name string,
	scopes []string,
	expiresAt int64,
) (tokenRecord *domain.APIToken, plaintextToken string, err error) {
	if err := RequireStaff(actor); err != nil {
		return nil, "", err
	}
	if len(scopes) == 0 {
		return nil, "", errors.New("auth: at least one API token scope is required")
	}
	seen := make(map[string]bool)
	cleanScopes := make([]string, 0, len(scopes))
	for _, sc := range scopes {
		sc = strings.TrimSpace(sc)
		if !validAPITokenScopes[sc] {
			return nil, "", fmt.Errorf("auth: unknown API token scope %q", sc)
		}
		if sc == ScopeBackupRead && actor.Role != domain.RoleAdmin {
			return nil, "", ErrForbidden
		}
		if !seen[sc] {
			seen[sc] = true
			cleanScopes = append(cleanScopes, sc)
		}
	}

	rnd, err := id.RandomHex(24)
	if err != nil {
		return nil, "", err
	}
	rawPAT := APITokenPrefix + rnd
	tokHash := hashOpaqueToken(rawPAT)
	scopesJSON, _ := json.Marshal(cleanScopes)
	now := time.Now().UnixMilli()

	rec := domain.APIToken{
		ID:          id.Next(),
		UserID:      actor.ID,
		Name:        strings.TrimSpace(name),
		TokenPrefix: rawPAT[:16],
		TokenHash:   tokHash,
		ScopesJSON:  string(scopesJSON),
		ExpiresAt:   expiresAt,
		CreatedAt:   now,
	}
	if rec.Name == "" {
		rec.Name = "API Token"
	}
	if _, err := s.store.ExecContext(ctx,
		`INSERT INTO api_tokens (id, user_id, name, token_prefix, token_hash, scopes_json, last_used_at, expires_at, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, 0, ?, ?)`,
		rec.ID, rec.UserID, rec.Name, rec.TokenPrefix, rec.TokenHash, rec.ScopesJSON, rec.ExpiresAt, rec.CreatedAt,
	); err != nil {
		return nil, "", err
	}
	rec.TokenHash = ""
	return &rec, rawPAT, nil
}

// AuthenticateAPIToken validates a Bearer volo_pat_... token and enforces requiredScope!
func (s *Service) AuthenticateAPIToken(ctx context.Context, rawToken string, requiredScope string) (*domain.User, *domain.APIToken, error) {
	trimmed := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(rawToken), "Bearer "))
	if !strings.HasPrefix(trimmed, APITokenPrefix) {
		return nil, nil, ErrUnauthorized
	}
	tokHash := hashOpaqueToken(trimmed)
	rec, err := sqlx.Get[domain.APIToken](ctx, s.store,
		`SELECT * FROM api_tokens WHERE token_hash = ?`, tokHash)
	if err != nil {
		return nil, nil, ErrUnauthorized
	}
	now := time.Now().UnixMilli()
	if rec.ExpiresAt > 0 && now >= rec.ExpiresAt {
		return nil, nil, ErrUnauthorized
	}

	var scopes []string
	if json.Unmarshal([]byte(rec.ScopesJSON), &scopes) != nil {
		return nil, nil, ErrForbidden
	}
	if requiredScope != "" {
		hasScope := false
		for _, sc := range scopes {
			if sc == requiredScope || (requiredScope == ScopeArticlesRead && sc == ScopeArticlesWrite) {
				hasScope = true
				break
			}
		}
		if !hasScope {
			return nil, nil, ErrForbidden
		}
	}

	u, err := sqlx.Get[domain.User](ctx, s.store, `SELECT * FROM users WHERE id = ?`, rec.UserID)
	if err != nil || u.Status != domain.UserStatusActive {
		return nil, nil, ErrUnauthorized
	}
	if err := RequireStaff(&u); err != nil {
		return nil, nil, err
	}
	if now-rec.LastUsedAt > 60_000 {
		_, _ = s.store.ExecBgContext(ctx, `UPDATE api_tokens SET last_used_at = ? WHERE id = ?`, now, rec.ID)
		rec.LastUsedAt = now
	}
	rec.TokenHash = ""
	clean := sanitizeUser(u)
	return &clean, &rec, nil
}

// --- Internal helpers ---

func (s *Service) findUserByIdent(ctx context.Context, ident string) (domain.User, error) {
	clean := strings.TrimSpace(ident)
	if clean == "" {
		return domain.User{}, sql.ErrNoRows
	}
	return sqlx.Get[domain.User](ctx, s.store,
		`SELECT * FROM users WHERE LOWER(username) = LOWER(?) OR LOWER(email) = LOWER(?)`,
		clean, clean)
}

func (s *Service) userHasTOTPEnabled(ctx context.Context, userID string) (bool, error) {
	rows, err := sqlx.Select[struct {
		ID string `db:"id"`
	}](ctx, s.store, `SELECT id FROM user_credentials WHERE user_id = ? AND kind = 'totp'`, userID)
	if err != nil {
		return false, err
	}
	return len(rows) > 0, nil
}

func passwordBindingDigest(pwHash string) string {
	n := len(pwHash)
	if n > 16 {
		n = 16
	}
	sum := sha256.Sum256([]byte("volo-pwbind:v1:" + pwHash[:n]))
	return hex.EncodeToString(sum[:8])
}

func (s *Service) mintTrusted2FATicket(userID string, passwordHash string, userAgent string, expiresAtMS int64) (string, error) {
	uaSum := sha256.Sum256([]byte(strings.TrimSpace(userAgent)))
	pwTag := passwordBindingDigest(passwordHash)
	payload := fmt.Sprintf("%s|%x|%s|%d", userID, uaSum[:8], pwTag, expiresAtMS)
	return s.keyRing.SealString(payload, "auth:trusted-2fa")
}

func (s *Service) verifyTrusted2FATicket(cookieVal string, expectedUserID string, passwordHash string, userAgent string) bool {
	if strings.TrimSpace(cookieVal) == "" {
		return false
	}
	opened, err := s.keyRing.OpenString(strings.TrimSpace(cookieVal), "auth:trusted-2fa")
	if err != nil || opened == "" {
		return false
	}
	parts := strings.Split(opened, "|")
	if len(parts) != 4 || parts[0] != expectedUserID {
		return false
	}
	uaSum := sha256.Sum256([]byte(strings.TrimSpace(userAgent)))
	if subtle.ConstantTimeCompare([]byte(parts[1]), []byte(hex.EncodeToString(uaSum[:8]))) != 1 {
		return false
	}
	expectedPWTag := passwordBindingDigest(passwordHash)
	if subtle.ConstantTimeCompare([]byte(parts[2]), []byte(expectedPWTag)) != 1 {
		return false
	}
	var exp int64
	_, _ = fmt.Sscanf(parts[3], "%d", &exp)
	return time.Now().UnixMilli() < exp
}

func hashOpaqueToken(raw string) string {
	sum := sha256.Sum256([]byte("volo-token:v1:" + raw))
	return hex.EncodeToString(sum[:])
}

func sanitizeUser(u domain.User) domain.User {
	u.PasswordHash = ""
	u.LegacyHash = ""
	return u
}
