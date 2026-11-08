package visibility

import (
	"context"
	"crypto/pbkdf2"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Willh92/Volo-go/internal/config"
	"github.com/Willh92/Volo-go/internal/domain"
	"github.com/Willh92/Volo-go/internal/guest"
	"github.com/Willh92/Volo-go/internal/i18n"
	"github.com/Willh92/Volo-go/internal/render"
	"github.com/Willh92/Volo-go/internal/repo"
)

// ErrProtectedForbidden is returned when an un-unlocked request attempts to access comments or raw .md of a protected post.
var ErrProtectedForbidden = errors.New("visibility: article is password-protected")

// PublicArticleCard is the strictly redacted view model for home, category, tag, archive, and series lists.
type PublicArticleCard struct {
	ID                  string       `json:"id"`
	AuthorID            string       `json:"author_id,omitempty"`
	AuthorName          string       `json:"author_name,omitempty"`
	AuthorAvatarURL     string       `json:"author_avatar_url,omitempty"`
	Title               string       `json:"title"`
	Slug                string       `json:"slug"`
	URL                 string       `json:"url"`
	CategoryID          string       `json:"category_id,omitempty"`
	CategoryName        string       `json:"category_name,omitempty"`
	CategorySlug        string       `json:"category_slug,omitempty"`
	Tags                []domain.Tag `json:"tags,omitempty"`
	Summary             string       `json:"summary"`
	CoverURL            string       `json:"cover_url,omitempty"` // Empty on locked posts -> triggers offline SVG cover-art
	Lang                string       `json:"lang"`
	PublishedAt         int64        `json:"published_at"`
	UpdatedAt           int64        `json:"updated_at"`
	HasUpdated          bool         `json:"has_updated"`
	IsPinned            bool         `json:"is_pinned"`
	IsPasswordProtected bool         `json:"is_password_protected"`
	HasContentLock      bool         `json:"has_content_lock"`
	HasAudio            bool         `json:"has_audio"`
	WordCount           int          `json:"word_count"`
	ReadingMinutes      int          `json:"reading_minutes"`
	ViewCount           int64        `json:"view_count"`
	CommentCount        int          `json:"comment_count"`
}

// PublicNeighbor is the strictly minimal metadata for adjacent (Prev/Next) and sidebar related links.
type PublicNeighbor struct {
	ID                  string `json:"id"`
	Title               string `json:"title"`
	Slug                string `json:"slug"`
	URL                 string `json:"url"`
	PublishedAt         int64  `json:"published_at"`
	IsPasswordProtected bool   `json:"is_password_protected"`
}

// TranslationAlternate links another language version in the same translation_group.
type TranslationAlternate struct {
	Lang  string `json:"lang"`
	Title string `json:"title"`
	Slug  string `json:"slug"`
	URL   string `json:"url"`
}

// PublicArticleView is the sanitized full-page article view model passed to volo-studio templates.
type PublicArticleView struct {
	ID                  string                 `json:"id"`
	AuthorID            string                 `json:"author_id"`
	AuthorName          string                 `json:"author_name"`
	AuthorAvatarURL     string                 `json:"author_avatar_url,omitempty"`
	AuthorBio           string                 `json:"author_bio,omitempty"`
	AuthorWebsiteURL    string                 `json:"author_website_url,omitempty"`
	CategoryID          string                 `json:"category_id,omitempty"`
	CategoryName        string                 `json:"category_name,omitempty"`
	CategorySlug        string                 `json:"category_slug,omitempty"`
	Tags                []domain.Tag           `json:"tags,omitempty"`
	Title               string                 `json:"title"`
	Slug                string                 `json:"slug"`
	URL                 string                 `json:"url"`
	Summary             string                 `json:"summary"`
	HTML                string                 `json:"html"`
	CoverURL            string                 `json:"cover_url,omitempty"`
	OGImageURL          string                 `json:"og_image_url,omitempty"`
	IsPinned            bool                   `json:"is_pinned"`
	HasUpdated          bool                   `json:"has_updated"`
	AllowComment        bool                   `json:"allow_comment"`
	IsPasswordProtected bool                   `json:"is_password_protected"`
	RequiresPassword    bool                   `json:"requires_password"`
	UnlockedViaShare    bool                   `json:"unlocked_via_share"`
	HasContentLock      bool                   `json:"has_content_lock"`
	HasLockedSections   bool                   `json:"has_locked_sections"`
	AudioURL            string                 `json:"audio_url,omitempty"`
	AudioDurationSec    int                    `json:"audio_duration_sec,omitempty"`
	AudioSizeBytes      int64                  `json:"audio_size_bytes,omitempty"`
	AudioMIME           string                 `json:"audio_mime,omitempty"`
	Lang                string                 `json:"lang"`
	TranslationGroup    string                 `json:"translation_group,omitempty"`
	Translations        []TranslationAlternate `json:"translations,omitempty"`
	LicenseText         string                 `json:"license_text,omitempty"`
	TOC                 []render.TOCItem       `json:"toc,omitempty"`
	WordCount           int                    `json:"word_count"`
	ReadingMinutes      int                    `json:"reading_minutes"`
	ViewCount           int64                  `json:"view_count"`
	CommentCount        int                    `json:"comment_count"`
	PublishedAt         int64                  `json:"published_at"`
	UpdatedAt           int64                  `json:"updated_at"`
	NeedsMath           bool                   `json:"needs_math"`
	NeedsMermaid        bool                   `json:"needs_mermaid"`
}

// FeedItem represents a sanitized RSS / Atom / Podcast feed entry.
type FeedItem struct {
	ID               string
	Title            string
	URL              string
	Summary          string
	HTML             string
	AuthorName       string
	CategoryName     string
	PublishedAt      int64
	UpdatedAt        int64
	AudioURL         string
	AudioDurationSec int
	AudioSizeBytes   int64
	AudioMIME        string
}

// Guard enforces the 10-surface content visibility perimeter and stateless ShareToken verification.
type Guard struct {
	keys      *config.KeyRing
	settings  *repo.SettingsManager
	passports *guest.PassportManager
	catalog   *i18n.Catalog
	renderer  *render.Engine
	cache     *boundedRenderLRU
	nowFn     func() time.Time
}

// NewGuard constructs a ContentVisibilityGuard.
func NewGuard(
	keys *config.KeyRing,
	settings *repo.SettingsManager,
	passports *guest.PassportManager,
	catalog *i18n.Catalog,
	renderer *render.Engine,
) *Guard {
	if renderer == nil {
		renderer = render.Default()
	}
	return &Guard{
		keys:      keys,
		settings:  settings,
		passports: passports,
		catalog:   catalog,
		renderer:  renderer,
		cache:     newBoundedRenderLRU(DefaultRenderCacheMaxBytes),
		nowFn:     time.Now,
	}
}

// PurgeCache clears cached Markdown render artifacts (e.g. when media asset dimensions are refreshed).
func (g *Guard) PurgeCache() {
	if g == nil || g.cache == nil {
		return
	}
	g.cache.Purge()
}

// ArticlePath returns the canonical path `/posts/{slug}` for an article.
func ArticlePath(slug string) string {
	return "/posts/" + url.PathEscape(strings.TrimSpace(slug))
}

func openKeyRingWithFallback(kr *config.KeyRing, ciphertext string, primaryAAD string, fallbackAADs ...string) (string, error) {
	opened, err := kr.OpenString(ciphertext, primaryAAD)
	if err == nil {
		return opened, nil
	}
	for _, fb := range fallbackAADs {
		if alt, fbErr := kr.OpenString(ciphertext, fb); fbErr == nil {
			return alt, nil
		}
	}
	return "", err
}

const lockKDFRounds = 4096

// EqualLockPassword compares two lock passcodes in constant time using salted PBKDF2-HMAC-SHA256 stretching.
func EqualLockPassword(a string, b string, domainSalt string) bool {
	salt := []byte("volo-lock-cmp:v2:" + domainSalt)
	ka, errA := pbkdf2.Key(sha256.New, a, salt, lockKDFRounds, 32)
	kb, errB := pbkdf2.Key(sha256.New, b, salt, lockKDFRounds, 32)
	if errA != nil || errB != nil || len(ka) != 32 || len(kb) != 32 {
		return false
	}
	return subtle.ConstantTimeCompare(ka, kb) == 1
}

// InlineUnlockKey returns a lock-scoped passport entry key so multiple inline `:::lock pwd="..."` blocks
// on the same article or page can be unlocked independently without overwriting each other in GuestPassport.
func InlineUnlockKey(targetID string, pwd string) string {
	dk, _ := pbkdf2.Key(sha256.New, pwd, []byte("volo-inline-lock:v2:"+targetID), lockKDFRounds, 6)
	return targetID + "#" + hex.EncodeToString(dk)
}

// IsAuthorOrAdmin reports whether viewer is an active Admin or the active Editor who authored raw.
func (g *Guard) IsAuthorOrAdmin(raw domain.RawArticle, viewer *domain.User) bool {
	if viewer == nil || viewer.Status != domain.UserStatusActive {
		return false
	}
	return viewer.Role == domain.RoleAdmin || (viewer.Role == domain.RoleEditor && viewer.ID == raw.AuthorID)
}

// DecryptArticlePassword opens raw.PasswordCiphertext with AES-256-GCM bound to "articles.password:"+raw.ID
// (with legacy fallback AAD "articles:password:"+raw.ID).
// If decryption fails (e.g. tampered DB or changed master key), it FAILS CLOSED by returning a non-matchable sentinel!
func (g *Guard) DecryptArticlePassword(raw domain.RawArticle) (plain string, protected bool) {
	if strings.TrimSpace(raw.PasswordCiphertext) == "" {
		return "", false
	}
	if g.keys == nil {
		return "\x00fail-closed-locked", true
	}
	opened, err := openKeyRingWithFallback(g.keys, raw.PasswordCiphertext, "articles.password:"+raw.ID, "articles:password:"+raw.ID)
	if err != nil || opened == "" {
		return "\x00fail-closed-locked", true
	}
	return opened, true
}

// SealArticlePassword encrypts a plaintext article/lock password with AES-256-GCM bound to articleID.
func (g *Guard) SealArticlePassword(articleID string, plainPassword string) (string, error) {
	trimmed := strings.TrimSpace(plainPassword)
	if trimmed == "" {
		return "", nil
	}
	return g.keys.SealString(trimmed, "articles.password:"+articleID)
}

// EffectiveUnlockPassword resolves the primary password required to unlock either a whole-article protected post
// or the first inline `:::lock pwd="..."` block on a public post.
func (g *Guard) EffectiveUnlockPassword(raw domain.RawArticle) (plain string, isWholeProtected bool, hasAnyPwd bool) {
	if p, whole := g.DecryptArticlePassword(raw); whole {
		return p, true, true
	}
	if inlinePwd := render.ExtractFirstLockPassword(raw.ContentMD); inlinePwd != "" {
		return inlinePwd, false, true
	}
	return "", false, false
}

// UnlockEntryKey returns the GuestPassport entry key to store for matchedPwd on raw:
// `raw.ID` when unlocking the whole-article password, or `InlineUnlockKey(raw.ID, matchedPwd)` for an inline lock block.
func (g *Guard) UnlockEntryKey(raw domain.RawArticle, matchedPwd string) string {
	if wholePwd, whole := g.DecryptArticlePassword(raw); whole && !strings.HasPrefix(wholePwd, "\x00") {
		if EqualLockPassword(wholePwd, matchedPwd, "article:"+raw.ID) {
			return raw.ID
		}
	}
	return InlineUnlockKey(raw.ID, matchedPwd)
}

// VerifyArticlePassword checks user-supplied password in constant time against the article's decrypted whole-article
// password AND any inline `:::lock pwd="..."` block passwords.
func (g *Guard) VerifyArticlePassword(raw domain.RawArticle, candidate string) (string, bool) {
	c := strings.TrimSpace(candidate)
	if c == "" {
		return "", false
	}
	salt := []byte("volo-lock-cmp:v2:article:" + raw.ID)
	candKey, err := pbkdf2.Key(sha256.New, c, salt, lockKDFRounds, 32)
	if err != nil || len(candKey) != 32 {
		return "", false
	}

	var matched string
	if wholePwd, whole := g.DecryptArticlePassword(raw); whole && !strings.HasPrefix(wholePwd, "\x00") {
		if wKey, wErr := pbkdf2.Key(sha256.New, wholePwd, salt, lockKDFRounds, 32); wErr == nil && subtle.ConstantTimeCompare(wKey, candKey) == 1 {
			matched = wholePwd
		}
	}
	for _, inlinePwd := range render.ExtractAllLockPasswords(raw.ContentMD) {
		if iKey, iErr := pbkdf2.Key(sha256.New, inlinePwd, salt, lockKDFRounds, 32); iErr == nil && subtle.ConstantTimeCompare(iKey, candKey) == 1 && matched == "" {
			matched = inlinePwd
		}
	}
	if matched == "" {
		return "", false
	}
	return matched, true
}

// ResolveInlineUnlockedPasswords returns the map of inline `:::lock pwd="..."` passwords unlocked by r's GuestPassport.
func (g *Guard) ResolveInlineUnlockedPasswords(r *http.Request, targetID string, contentMD string) map[string]bool {
	unlocked := make(map[string]bool)
	if r == nil || g.passports == nil || targetID == "" {
		return unlocked
	}
	pwds := render.ExtractAllLockPasswords(contentMD)
	if len(pwds) == 0 {
		return unlocked
	}
	gp, ok := g.passports.ResolveOrHeal(nil, r, false)
	if !ok || gp == nil {
		return unlocked
	}
	for _, p := range pwds {
		if g.passports.HasArticleUnlock(r, gp.GuestID, InlineUnlockKey(targetID, p), p) ||
			g.passports.HasArticleUnlock(r, gp.GuestID, targetID, p) {
			unlocked[p] = true
		}
	}
	return unlocked
}

// --- Timed Passwordless ShareToken (`?preview=<exp>.<sig>`) ---

// IssueShareToken generates a stateless, time-limited preview token bound to BOTH articleID
// and PBKDF2-HMAC-SHA256(current_article_password) plus the rotatable ShareTokenSecret.
func (g *Guard) IssueShareToken(ctx context.Context, raw domain.RawArticle, ttl time.Duration) (string, int64, error) {
	plainPwd, isWhole, _ := g.EffectiveUnlockPassword(raw)
	if !isWhole && !raw.HasContentLock && !render.ContainsLockDirective(raw.ContentMD) {
		return "", 0, errors.New("visibility: article has no password or content lock")
	}
	if strings.HasPrefix(plainPwd, "\x00") {
		return "", 0, errors.New("visibility: article password cannot be decrypted")
	}
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	if ttl > 90*24*time.Hour {
		ttl = 90 * 24 * time.Hour
	}
	var rotSecret string
	if g.settings != nil {
		var err error
		rotSecret, err = g.settings.GetOrInitShareTokenSecret(ctx)
		if err != nil {
			return "", 0, err
		}
	}
	exp := g.nowFn().Add(ttl).Unix()
	sig := g.computeShareMAC(rotSecret, raw.ID, exp, plainPwd)
	return fmt.Sprintf("%d.%s", exp, sig), exp, nil
}

// VerifyShareToken validates `?preview=<exp>.<sig>` against the article's current password and rotatable secret
// with zero database write side-effects.
func (g *Guard) VerifyShareToken(ctx context.Context, raw domain.RawArticle, token string) bool {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 2 {
		return false
	}
	exp, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || g.nowFn().Unix() >= exp {
		return false
	}
	plainPwd, _, _ := g.EffectiveUnlockPassword(raw)
	if strings.HasPrefix(plainPwd, "\x00") {
		return false
	}
	var rotSecret string
	if g.settings != nil {
		rotSecret = g.settings.PeekShareTokenSecret(ctx)
	}
	expected := g.computeShareMAC(rotSecret, raw.ID, exp, plainPwd)
	return subtle.ConstantTimeCompare([]byte(parts[1]), []byte(expected)) == 1
}

func (g *Guard) computeShareMAC(rotSecret string, articleID string, exp int64, plainPwd string) string {
	seed := g.keys.ShareTokenKey()
	salt := make([]byte, 0, len(seed)+len(rotSecret)+len(articleID)+48)
	salt = append(salt, seed[:]...)
	salt = fmt.Appendf(salt, ":volo-share:v2:%d:%s:%d:%s:%d", len(rotSecret), rotSecret, len(articleID), articleID, exp)
	dk, _ := pbkdf2.Key(sha256.New, plainPwd, salt, lockKDFRounds, 24)
	return base64.RawURLEncoding.EncodeToString(dk)
}

// UnlockStatus describes how/whether the current HTTP request has unlocked a protected article.
type UnlockStatus struct {
	Unlocked            bool
	WholeUnlocked       bool
	AuthorOrShareBypass bool
	UnlockedPasswords   map[string]bool
	ViaShareToken       bool
	IsStaffOwner        bool
}

// CheckRequestUnlock evaluates whether r is authorized to view protected content of raw.
// Note: unsigned `env_fp` is NEVER accepted for unlocking!
func (g *Guard) CheckRequestUnlock(r *http.Request, raw domain.RawArticle, viewer *domain.User) UnlockStatus {
	wholePwd, isWholeProtected := g.DecryptArticlePassword(raw)
	inlinePwds := render.ExtractAllLockPasswords(raw.ContentMD)
	hasAnyDirective := raw.HasContentLock || len(inlinePwds) > 0 || render.ContainsLockDirective(raw.ContentMD)

	if !isWholeProtected && !hasAnyDirective {
		return UnlockStatus{Unlocked: true, WholeUnlocked: true}
	}

	// 1. Admin or the Editor who authored the post always has access.
	if g.IsAuthorOrAdmin(raw, viewer) {
		return UnlockStatus{
			Unlocked:            true,
			WholeUnlocked:       true,
			AuthorOrShareBypass: true,
			IsStaffOwner:        true,
		}
	}

	// 2. Timed ShareToken (`?preview=<exp>.<sig>` or `X-Volo-Share-Token`).
	if r != nil {
		shareTok := strings.TrimSpace(r.URL.Query().Get("preview"))
		if shareTok == "" {
			shareTok = strings.TrimSpace(r.Header.Get("X-Volo-Share-Token"))
		}
		if shareTok != "" && g.VerifyShareToken(r.Context(), raw, shareTok) {
			return UnlockStatus{
				Unlocked:            true,
				WholeUnlocked:       true,
				AuthorOrShareBypass: true,
				ViaShareToken:       true,
			}
		}
	}

	// 3. Cryptographically signed GuestPassport + password-hash-bound unlock cookie.
	unlockedPwds := make(map[string]bool)
	wholeUnlocked := !isWholeProtected
	if r != nil && g.passports != nil {
		if gp, ok := g.passports.ResolveOrHeal(nil, r, false); ok && gp != nil {
			if isWholeProtected && wholePwd != "" && !strings.HasPrefix(wholePwd, "\x00") {
				if g.passports.HasArticleUnlock(r, gp.GuestID, raw.ID, wholePwd) {
					wholeUnlocked = true
					unlockedPwds[wholePwd] = true
				}
			}
			for _, p := range inlinePwds {
				if g.passports.HasArticleUnlock(r, gp.GuestID, InlineUnlockKey(raw.ID, p), p) ||
					g.passports.HasArticleUnlock(r, gp.GuestID, raw.ID, p) {
					unlockedPwds[p] = true
				}
			}
		}
	}

	allUnlocked := wholeUnlocked
	if allUnlocked && len(inlinePwds) > 0 {
		for _, p := range inlinePwds {
			if !unlockedPwds[p] {
				allUnlocked = false
				break
			}
		}
	}

	return UnlockStatus{
		Unlocked:          allUnlocked && (!hasAnyDirective || len(inlinePwds) > 0 || isWholeProtected),
		WholeUnlocked:     wholeUnlocked,
		UnlockedPasswords: unlockedPwds,
	}
}

// ApplyNoIndexPrivateHeaders enforces anti-indexing & anti-caching headers on ShareToken or unlocked views.
func ApplyNoIndexPrivateHeaders(w http.ResponseWriter) {
	if w == nil {
		return
	}
	w.Header().Set("X-Robots-Tag", "noindex, nofollow")
	w.Header().Set("Cache-Control", "private, no-store")
}

// --- Surface 1 & 2: List Cards, Summaries & Cover / OG Image Redaction ---

// ProtectedNotice returns the localized redaction placeholder for password-protected article summaries.
func (g *Guard) ProtectedNotice(locale string) string {
	return g.protectedNotice(locale)
}

// ToCard converts RawArticle into a zero-leak PublicArticleCard.
func (g *Guard) ToCard(raw domain.RawArticle, locale string, cat *domain.Category, tags []domain.Tag, lookupDims func(string) (int, int, bool)) PublicArticleCard {
	card := PublicArticleCard{
		ID:             raw.ID,
		AuthorID:       raw.AuthorID,
		Title:          raw.Title,
		Slug:           raw.Slug,
		URL:            ArticlePath(raw.Slug),
		CategoryID:     raw.CategoryID,
		Tags:           tags,
		Lang:           raw.Lang,
		PublishedAt:    raw.PublishedAt,
		UpdatedAt:      raw.UpdatedAt,
		HasUpdated:     raw.PublishedAt > 0 && raw.UpdatedAt > raw.PublishedAt+60_000,
		IsPinned:       raw.IsPinned,
		HasContentLock: raw.HasContentLock,
		WordCount:      raw.WordCount,
		ReadingMinutes: raw.ReadingMinutes,
		ViewCount:      raw.ViewCount,
		CommentCount:   raw.CommentCount,
	}
	if cat != nil {
		card.CategoryName = cat.Name
		card.CategorySlug = cat.Slug
	}

	if _, isProtected := g.DecryptArticlePassword(raw); isProtected {
		card.IsPasswordProtected = true
		card.Summary = g.protectedNotice(locale)
		// Surface 2: NEVER expose custom cover_url, body first-image, word count, or audio presence on locked posts!
		card.CoverURL = ""
		card.HasAudio = false
		card.WordCount = 0
		card.ReadingMinutes = 1
		return card
	}

	ck := cardCacheKey(raw, locale, lookupDims != nil)
	if cached, ok := g.cache.getCard(ck); ok {
		card.Summary = cached.summary
		card.CoverURL = cached.coverURL
		card.HasAudio = strings.TrimSpace(raw.AudioURL) != ""
		if card.WordCount <= 0 {
			card.WordCount = cached.wordCount
		}
		if card.ReadingMinutes <= 0 {
			card.ReadingMinutes = cached.readingMinutes
		}
		return card
	}

	// Surface 1 & 2 for `:::lock` posts: physically strip ALL `:::lock ... :::` blocks BEFORE summary & image extraction!
	safeSummaryMD := render.StripAllLockBlocks(raw.SummaryMD)
	safeContentMD := render.StripAllLockBlocks(raw.ContentMD)

	renderedContent := g.renderer.Render(safeContentMD, render.Options{
		Mode:            render.RenderModeWeb,
		Locale:          locale,
		ArticleID:       raw.ID,
		Unlocked:        false,
		LookupMediaDims: lookupDims,
	})

	summaryText := ""
	if strings.TrimSpace(safeSummaryMD) != "" {
		sumOut := g.renderer.Render(safeSummaryMD, render.Options{
			Mode:     render.RenderModeWeb,
			Locale:   locale,
			Unlocked: false,
		})
		summaryText = sumOut.PlainText
	} else {
		summaryText = truncateRunes(renderedContent.PlainText, 180)
	}

	cover := strings.TrimSpace(raw.CoverURL)
	if cover == "" {
		cover = renderedContent.FirstImageURL
	}

	g.cache.putCard(ck, cachedCardMeta{
		summary:        summaryText,
		coverURL:       cover,
		wordCount:      renderedContent.WordCount,
		readingMinutes: renderedContent.ReadingMinutes,
	})

	card.Summary = summaryText
	card.CoverURL = cover
	card.HasAudio = strings.TrimSpace(raw.AudioURL) != ""
	if card.WordCount <= 0 {
		card.WordCount = renderedContent.WordCount
	}
	if card.ReadingMinutes <= 0 {
		card.ReadingMinutes = renderedContent.ReadingMinutes
	}
	return card
}

// --- Surface 3: Adjacent Prev/Next & Related Articles ---

// ToNeighbor strips all summary & cover fields for Prev/Next & Related links.
func (g *Guard) ToNeighbor(raw domain.RawArticle) PublicNeighbor {
	_, isProtected := g.DecryptArticlePassword(raw)
	return PublicNeighbor{
		ID:                  raw.ID,
		Title:               raw.Title,
		Slug:                raw.Slug,
		URL:                 ArticlePath(raw.Slug),
		PublishedAt:         raw.PublishedAt,
		IsPasswordProtected: isProtected,
	}
}

// --- Full Detail View ---

// DetailRenderDeps supplies optional per-page enrichments (bookshelf cards, media dimensions, translations).
type DetailRenderDeps struct {
	Locale               string
	AuthorName           string
	AuthorAvatarURL      string
	AuthorBio            string
	AuthorWebsiteURL     string
	Category             *domain.Category
	Tags                 []domain.Tag
	Translations         []TranslationAlternate
	DefaultOGImageURL    string
	SiteTitle            string
	PublicBaseURL        string
	LicenseTemplate      string
	DisableLicenseNotice bool
	ShowLineNumbers      bool
	LookupBook           func(slug string) (render.BookCardInfo, bool)
	ListBooks            func() []render.BookCardInfo
	LookupMediaDims      func(src string) (int, int, bool)
}

// ToDetail produces a sanitized PublicArticleView for article detail pages.
func (g *Guard) ToDetail(
	w http.ResponseWriter,
	r *http.Request,
	raw domain.RawArticle,
	viewer *domain.User,
	deps DetailRenderDeps,
) PublicArticleView {
	_, isWholeProtected := g.DecryptArticlePassword(raw)
	status := g.CheckRequestUnlock(r, raw, viewer)
	if status.ViaShareToken || (isWholeProtected && status.WholeUnlocked) || len(status.UnlockedPasswords) > 0 {
		ApplyNoIndexPrivateHeaders(w)
	}

	artURL := ArticlePath(raw.Slug)
	view := PublicArticleView{
		ID:                  raw.ID,
		AuthorID:            raw.AuthorID,
		AuthorName:          deps.AuthorName,
		AuthorAvatarURL:     deps.AuthorAvatarURL,
		AuthorBio:           deps.AuthorBio,
		AuthorWebsiteURL:    deps.AuthorWebsiteURL,
		CategoryID:          raw.CategoryID,
		Tags:                deps.Tags,
		Title:               raw.Title,
		Slug:                raw.Slug,
		URL:                 artURL,
		IsPinned:            raw.IsPinned,
		HasUpdated:          raw.PublishedAt > 0 && raw.UpdatedAt > raw.PublishedAt+60_000,
		AllowComment:        raw.AllowComment,
		IsPasswordProtected: isWholeProtected,
		UnlockedViaShare:    status.ViaShareToken,
		HasContentLock:      raw.HasContentLock,
		Lang:                raw.Lang,
		TranslationGroup:    raw.TranslationGroup,
		Translations:        deps.Translations,
		ViewCount:           raw.ViewCount,
		CommentCount:        raw.CommentCount,
		PublishedAt:         raw.PublishedAt,
		UpdatedAt:           raw.UpdatedAt,
	}
	if deps.Category != nil {
		view.CategoryName = deps.Category.Name
		view.CategorySlug = deps.Category.Slug
	}

	if isWholeProtected && !status.WholeUnlocked {
		view.RequiresPassword = true
		view.Summary = g.protectedNotice(deps.Locale)
		view.OGImageURL = deps.DefaultOGImageURL
		return view
	}

	isReaderLoggedIn := viewer != nil && viewer.Status == domain.UserStatusActive
	hasBookEmbed := strings.Contains(raw.ContentMD, ":::book")
	unattributedUnlocked := isWholeProtected && status.WholeUnlocked
	cacheableDetail := !isWholeProtected && !status.ViaShareToken && !status.AuthorOrShareBypass && !hasBookEmbed

	var out render.Output
	var gotCached bool
	var outKey string
	if cacheableDetail {
		outKey = outputCacheKey(raw, render.RenderModeWeb, deps.Locale, unattributedUnlocked, status.AuthorOrShareBypass, status.UnlockedPasswords, isReaderLoggedIn, deps.ShowLineNumbers, deps.LookupMediaDims != nil)
		out, gotCached = g.cache.getOutput(outKey)
	}
	unlockedMap := status.UnlockedPasswords
	if unlockedMap == nil {
		unlockedMap = map[string]bool{}
	}
	if !gotCached {
		out = g.renderer.Render(raw.ContentMD, render.Options{
			Mode:                render.RenderModeWeb,
			Locale:              deps.Locale,
			ArticleID:           raw.ID,
			Unlocked:            unattributedUnlocked,
			AuthorOrShareBypass: status.AuthorOrShareBypass,
			UnlockedPasswords:   unlockedMap,
			IsReaderLoggedIn:    isReaderLoggedIn,
			HasAudio:            strings.TrimSpace(raw.AudioURL) != "",
			ShowLineNumbers:     deps.ShowLineNumbers,
			LookupBook:          deps.LookupBook,
			ListBooks:           deps.ListBooks,
			LookupMediaDims:     deps.LookupMediaDims,
		})
		if cacheableDetail {
			g.cache.putOutput(outKey, out)
		}
	}

	view.HTML = out.HTML
	view.TOC = out.TOC
	view.NeedsMath = out.NeedsMath
	view.NeedsMermaid = out.NeedsMermaid
	view.HasLockedSections = out.HasUnresolvedLock
	if !view.HasLockedSections {
		view.WordCount = out.WordCount
		view.ReadingMinutes = out.ReadingMinutes
	} else {
		view.WordCount = raw.WordCount
		if view.WordCount <= 0 {
			view.WordCount = out.WordCount
		}
		view.ReadingMinutes = raw.ReadingMinutes
		if view.ReadingMinutes <= 0 {
			view.ReadingMinutes = out.ReadingMinutes
		}
	}

	// Compute public-safe summary & OG image (never leaking locked blocks into meta tags!).
	// Fast-path: when the article contains NO lock blocks at all, `out` already represents 100% public content,
	// avoiding a duplicate second full goldmark + bluemonday render pass!
	hasAnyLock := isWholeProtected || raw.HasContentLock || out.HasLockBlock || render.ContainsLockDirective(raw.ContentMD) || render.ContainsLockDirective(raw.SummaryMD)
	if !hasAnyLock {
		if strings.TrimSpace(raw.SummaryMD) != "" {
			sOut := g.renderer.Render(raw.SummaryMD, render.Options{Locale: deps.Locale})
			view.Summary = sOut.PlainText
		} else {
			view.Summary = truncateRunes(out.PlainText, 160)
		}
		view.CoverURL = raw.CoverURL
		og := strings.TrimSpace(raw.CoverURL)
		if og == "" {
			og = out.FirstImageURL
		}
		if og == "" {
			og = deps.DefaultOGImageURL
		}
		view.OGImageURL = og
	} else {
		publicSummarySrc := render.StripAllLockBlocks(raw.SummaryMD)
		publicContentSrc := render.StripAllLockBlocks(raw.ContentMD)
		pubOut := g.renderer.Render(publicContentSrc, render.Options{
			Mode:            render.RenderModeWeb,
			Locale:          deps.Locale,
			Unlocked:        false,
			LookupMediaDims: deps.LookupMediaDims,
		})
		if strings.TrimSpace(publicSummarySrc) != "" {
			sOut := g.renderer.Render(publicSummarySrc, render.Options{Locale: deps.Locale})
			view.Summary = sOut.PlainText
		} else {
			view.Summary = truncateRunes(pubOut.PlainText, 160)
		}

		if isWholeProtected {
			// Even when unlocked by the current visitor, OG meta tags remain generic!
			view.OGImageURL = deps.DefaultOGImageURL
		} else {
			view.CoverURL = raw.CoverURL
			og := strings.TrimSpace(raw.CoverURL)
			if og == "" {
				og = pubOut.FirstImageURL
			}
			if og == "" {
				og = deps.DefaultOGImageURL
			}
			view.OGImageURL = og
		}
	}

	view.AudioURL = raw.AudioURL
	view.AudioDurationSec = raw.AudioDurationSec
	view.AudioSizeBytes = raw.AudioSizeBytes
	view.AudioMIME = raw.AudioMIME
	if !deps.DisableLicenseNotice {
		view.LicenseText = formatLicense(raw.LicenseOverride, deps.LicenseTemplate, deps.SiteTitle, raw.Title, deps.AuthorName, strings.TrimRight(deps.PublicBaseURL, "/")+artURL, raw.PublishedAt)
	}

	return view
}

// --- Surface 4: Comments Access Guard ---

// CanAccessArticleComments enforces Surface 4: reading or posting comments on a whole-article
// password-protected post is strictly forbidden (403) unless the requester has unlocked the post!
func (g *Guard) CanAccessArticleComments(r *http.Request, raw domain.RawArticle, viewer *domain.User) bool {
	if _, isProtected := g.DecryptArticlePassword(raw); !isProtected {
		return true
	}
	st := g.CheckRequestUnlock(r, raw, viewer)
	return st.WholeUnlocked
}

// --- Surface 6: RSS / Atom / Podcast Feeds ---

// ToFeedItem enforces Surface 6:
// - Password-protected articles (and their audio enclosures) NEVER enter any public feed.
// - Partial `:::lock` blocks are replaced by a localized notice blockquote with zero inner text.
func (g *Guard) ToFeedItem(raw domain.RawArticle, fullOutput bool, locale string, publicBaseURL string, authorName string, categoryName string) (FeedItem, bool) {
	if _, isProtected := g.DecryptArticlePassword(raw); isProtected {
		return FeedItem{}, false
	}

	fKey := outputCacheKey(raw, render.RenderModeFeed, locale, false, false, nil, false, false, false)
	feedOut, ok := g.cache.getOutput(fKey)
	if !ok {
		feedOut = g.renderer.Render(raw.ContentMD, render.Options{
			Mode:      render.RenderModeFeed,
			Locale:    locale,
			ArticleID: raw.ID,
			Unlocked:  false,
		})
		if !strings.Contains(raw.ContentMD, ":::book") {
			g.cache.putOutput(fKey, feedOut)
		}
	}

	hasAnyLock := raw.HasContentLock || feedOut.HasLockBlock || render.ContainsLockDirective(raw.ContentMD) || render.ContainsLockDirective(raw.SummaryMD)
	summary := ""
	if !hasAnyLock {
		if strings.TrimSpace(raw.SummaryMD) != "" {
			sOut := g.renderer.Render(raw.SummaryMD, render.Options{Mode: render.RenderModeFeed, Locale: locale})
			summary = sOut.PlainText
		} else {
			summary = truncateRunes(feedOut.PlainText, 200)
		}
	} else {
		safeSummaryMD := render.StripAllLockBlocks(raw.SummaryMD)
		if strings.TrimSpace(safeSummaryMD) != "" {
			sOut := g.renderer.Render(safeSummaryMD, render.Options{Mode: render.RenderModeFeed, Locale: locale})
			summary = sOut.PlainText
		} else {
			safeContentMD := render.StripAllLockBlocks(raw.ContentMD)
			pubPlainOut := g.renderer.Render(safeContentMD, render.Options{
				Mode:     render.RenderModeFeed,
				Locale:   locale,
				Unlocked: false,
			})
			summary = truncateRunes(pubPlainOut.PlainText, 200)
		}
	}

	bodyHTML := feedOut.HTML
	if !fullOutput {
		bodyHTML = "<p>" + html.EscapeString(summary) + "</p>"
	}

	absURL := strings.TrimRight(publicBaseURL, "/") + ArticlePath(raw.Slug)
	audioURL := strings.TrimSpace(raw.AudioURL)
	if strings.HasPrefix(audioURL, "/") {
		audioURL = strings.TrimRight(publicBaseURL, "/") + audioURL
	}

	return FeedItem{
		ID:               raw.ID,
		Title:            raw.Title,
		URL:              absURL,
		Summary:          summary,
		HTML:             bodyHTML,
		AuthorName:       authorName,
		CategoryName:     categoryName,
		PublishedAt:      raw.PublishedAt,
		UpdatedAt:        raw.UpdatedAt,
		AudioURL:         audioURL,
		AudioDurationSec: raw.AudioDurationSec,
		AudioSizeBytes:   raw.AudioSizeBytes,
		AudioMIME:        raw.AudioMIME,
	}, true
}

// --- Surface 7 & 8: Sitemap / IndexNow & Backlinks Eligibility ---

// IsPublicIndexable reports whether raw may appear in /sitemap.xml, IndexNow pushes, or public backlinks.
func (g *Guard) IsPublicIndexable(raw domain.RawArticle) bool {
	if raw.Status != domain.ArticleStatusPublished {
		return false
	}
	_, isProtected := g.DecryptArticlePassword(raw)
	return !isProtected
}

// --- Surface 9: /llms.txt & /posts/{slug}.md ---

// ToSafeRawMarkdown enforces Surface 9:
// - Password-protected articles return ("", false) -> excluded from /llms.txt and 403 on /posts/{slug}.md.
// - Partial `:::lock` articles physically strip all locked sections before returning Markdown.
func (g *Guard) ToSafeRawMarkdown(raw domain.RawArticle) (string, bool) {
	if _, isProtected := g.DecryptArticlePassword(raw); isProtected {
		return "", false
	}
	cleanBody := render.StripAllLockBlocks(raw.ContentMD)
	var b strings.Builder
	b.WriteString("# ")
	b.WriteString(raw.Title)
	b.WriteString("\n\n")
	b.WriteString(cleanBody)
	if !strings.HasSuffix(cleanBody, "\n") {
		b.WriteByte('\n')
	}
	return b.String(), true
}

func (g *Guard) protectedNotice(locale string) string {
	if g.catalog != nil {
		msg := g.catalog.T(locale, "article.protected_summary")
		if msg != "article.protected_summary" && msg != "" {
			return msg
		}
	}
	if strings.HasPrefix(strings.ToLower(locale), "en") {
		return "This post is password-protected."
	}
	return "受密码保护的文章"
}

func formatLicense(override string, tmpl string, siteTitle string, title string, author string, fullURL string, pubUnix int64) string {
	src := strings.TrimSpace(override)
	if src == "" {
		src = strings.TrimSpace(tmpl)
	}
	if src == "" {
		return ""
	}
	site := strings.TrimSpace(siteTitle)
	if site == "" {
		site = title
	}
	if src == "本文由 {author} 原创发布于 {title}，转载请注明原文链接：{url}" {
		src = "本文由 {author} 原创发布于 {site}，转载请注明原文链接：{url}"
	}
	dateStr := ""
	if pubUnix > 0 {
		dateStr = time.UnixMilli(pubUnix).UTC().Format("2006-01-02")
	}
	r := strings.NewReplacer(
		"{site}", site,
		"{title}", title,
		"{author}", author,
		"{url}", fullURL,
		"{date}", dateStr,
	)
	return r.Replace(src)
}

func truncateRunes(s string, maxRunes int) string {
	trimmed := strings.TrimSpace(s)
	if len(trimmed) <= maxRunes {
		return trimmed
	}
	pos := 0
	for n := 0; n < maxRunes && pos < len(trimmed); n++ {
		_, w := utf8.DecodeRuneInString(trimmed[pos:])
		pos += w
	}
	if pos >= len(trimmed) {
		return trimmed
	}
	return strings.TrimSpace(trimmed[:pos]) + "…"
}
