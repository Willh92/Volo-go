package visibility_test

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Willh92/Volo-go/internal/domain"
	"github.com/Willh92/Volo-go/internal/render"
	"github.com/Willh92/Volo-go/internal/visibility"
)

func TestSearchPaged_DeepBody_Pages_And_Stage2OverflowZeroLeak(t *testing.T) {
	ctx := context.Background()
	store, grd, srch := newVisibilityBenchEnv(t)
	now := time.Now().UnixMilli()

	if _, err := store.ExecContext(ctx, `INSERT INTO users (
		id, username, email, display_name, avatar_url, bio, website_url,
		role, password_hash, legacy_hash, status, preferred_locale, created_at, updated_at
	) VALUES ('u1', 'author', 'a@example.com', 'Author', '', '', '', 'admin', 'x', '', 'active', 'en-US', ?, ?)`, now, now); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	// 1. Article with keyword at ~1500 runes (past old 600-rune cap, within new 2400-rune cap)
	openingFiller := strings.Repeat("introductory prose words ", 50) // ~1250 runes
	deepBodyMD := openingFiller + "DEEP_BODY_KEYWORD_1500 " + strings.Repeat("closing words ", 20)
	if _, err := store.ExecContext(ctx, `INSERT INTO articles (
		id, author_id, category_id, title, slug, legacy_permalink, summary_md, content_md,
		cover_url, status, is_pinned, allow_comment, password_ciphertext, has_content_lock,
		audio_url, audio_duration_sec, audio_size_bytes, audio_mime,
		lang, translation_group, license_override, word_count, reading_minutes, view_count, comment_count,
		published_at, created_at, updated_at
	) VALUES ('art-deep', 'u1', '', 'Architecture Overview', 'arch-overview', '', 'Short summary', ?, '', 'published', 0, 1, '', ?, '', 0, 0, '', 'en-US', '', '', 200, 1, 0, 0, ?, ?, ?)`,
		deepBodyMD, false, now+1000, now+1000, now+1000); err != nil {
		t.Fatalf("seed deep article: %v", err)
	}

	// 2. Article with HasContentLock = true containing a secret body token (MUST NEVER match via Stage 2 SQL content_md!)
	lockedBodyMD := "Public lead paragraph.\n\n:::lock pwd=\"sec\"\nLOCKED_SECRET_TOKEN_7741\n:::\n"
	if _, err := store.ExecContext(ctx, `INSERT INTO articles (
		id, author_id, category_id, title, slug, legacy_permalink, summary_md, content_md,
		cover_url, status, is_pinned, allow_comment, password_ciphertext, has_content_lock,
		audio_url, audio_duration_sec, audio_size_bytes, audio_mime,
		lang, translation_group, license_override, word_count, reading_minutes, view_count, comment_count,
		published_at, created_at, updated_at
	) VALUES ('art-inline-lock', 'u1', '', 'Partially Locked Post', 'partially-locked', '', 'Safe summary', ?, '', 'published', 0, 1, '', ?, '', 0, 0, '', 'en-US', '', '', 50, 1, 0, 0, ?, ?, ?)`,
		lockedBodyMD, true, now+900, now+900, now+900); err != nil {
		t.Fatalf("seed inline-locked article: %v", err)
	}

	// 3. Password-protected article (MUST NEVER appear in search)
	cipherPwd, err := grd.SealArticlePassword("art-pwd", "topsecret")
	if err != nil {
		t.Fatalf("seal password: %v", err)
	}
	if _, err := store.ExecContext(ctx, `INSERT INTO articles (
		id, author_id, category_id, title, slug, legacy_permalink, summary_md, content_md,
		cover_url, status, is_pinned, allow_comment, password_ciphertext, has_content_lock,
		audio_url, audio_duration_sec, audio_size_bytes, audio_mime,
		lang, translation_group, license_override, word_count, reading_minutes, view_count, comment_count,
		published_at, created_at, updated_at
	) VALUES ('art-pwd', 'u1', '', 'Encrypted Post', 'encrypted-post', '', 'WHOLE_PWD_SECRET_8822', 'WHOLE_PWD_SECRET_8822', '', 'published', 0, 1, ?, ?, '', 0, 0, '', 'en-US', '', '', 50, 1, 0, 0, ?, ?, ?)`,
		cipherPwd, false, now+800, now+800, now+800); err != nil {
		t.Fatalf("seed pwd article: %v", err)
	}

	// 4. Published standalone page (with an inline :::lock block that MUST be stripped) + unpublished page
	pageBody := "Welcome to our standalone page with STANDALONE_PAGE_TOKEN_42.\n\n:::lock pwd=\"p1\"\nPAGE_LOCK_SECRET_999\n:::\n"
	if _, err := store.ExecContext(ctx, `INSERT INTO pages (
		id, author_id, title, slug, template, content_md, is_published, allow_comment, show_toc, lang, translation_group, created_at, updated_at
	) VALUES
		('pg-about', 'u1', 'About Studio', 'about', 'default', ?, ?, ?, ?, 'en-US', '', ?, ?),
		('pg-draft', 'u1', 'Draft Page STANDALONE_PAGE_TOKEN_42', 'draft-about', 'default', 'STANDALONE_PAGE_TOKEN_42', ?, ?, ?, 'en-US', '', ?, ?)`,
		pageBody, true, true, false, now+700, now+700,
		false, true, false, now+600, now+600); err != nil {
		t.Fatalf("seed pages: %v", err)
	}

	// 5. Seed 6 articles sharing "pagination_common_term" to verify SearchPaged pagination math.
	for i := 0; i < 6; i++ {
		ts := now + int64(100+i)
		if _, err := store.ExecContext(ctx, `INSERT INTO articles (
			id, author_id, category_id, title, slug, legacy_permalink, summary_md, content_md,
			cover_url, status, is_pinned, allow_comment, password_ciphertext, has_content_lock,
			audio_url, audio_duration_sec, audio_size_bytes, audio_mime,
			lang, translation_group, license_override, word_count, reading_minutes, view_count, comment_count,
			published_at, created_at, updated_at
		) VALUES (?, 'u1', '', ?, ?, '', '', 'pagination_common_term content', '', 'published', 0, 1, '', ?, '', 0, 0, '', 'en-US', '', '', 20, 1, 0, 0, ?, ?, ?)`,
			fmt.Sprintf("art-pg-%d", i),
			fmt.Sprintf("Paginated Note %d", i),
			fmt.Sprintf("paginated-note-%d", i),
			false, ts, ts, ts,
		); err != nil {
			t.Fatalf("seed paginated article %d: %v", i, err)
		}
	}

	if err := srch.Rebuild(ctx); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}

	// Verify deep body keyword past 600 runes matches in Stage 1
	deepHits, total, hasMore, err := srch.SearchPaged(ctx, "DEEP_BODY_KEYWORD_1500", "en-US", nil, 1, 10)
	if err != nil || total != 1 || hasMore || len(deepHits) != 1 || deepHits[0].Slug != "arch-overview" || deepHits[0].Kind != "article" {
		t.Fatalf("expected deep body keyword match on arch-overview, got total=%d hasMore=%v hits=%+v err=%v", total, hasMore, deepHits, err)
	}

	// Verify standalone page matches with URL="/about" and Kind="page", excluding draft page and stripped :::lock
	pageHits, total, _, err := srch.SearchPaged(ctx, "STANDALONE_PAGE_TOKEN_42", "en-US", nil, 1, 10)
	if err != nil || total != 1 || len(pageHits) != 1 || pageHits[0].URL != "/about" || pageHits[0].Kind != "page" {
		t.Fatalf("expected published standalone page hit (/about), got total=%d hits=%+v err=%v", total, pageHits, err)
	}
	for _, secret := range []string{"LOCKED_SECRET_TOKEN_7741", "WHOLE_PWD_SECRET_8822", "PAGE_LOCK_SECRET_999"} {
		leaked, lTotal, _, _ := srch.SearchPaged(ctx, secret, "en-US", nil, 1, 10)
		if lTotal != 0 || len(leaked) != 0 {
			t.Fatalf("CRITICAL: search leaked locked token %q: %+v", secret, leaked)
		}
	}

	// Verify SearchPaged pagination across 6 items with perPage=2
	p1, total, hasMore1, err := srch.SearchPaged(ctx, "pagination_common_term", "en-US", nil, 1, 2)
	if err != nil || total != 6 || !hasMore1 || len(p1) != 2 {
		t.Fatalf("unexpected page 1: total=%d hasMore=%v len=%d err=%v", total, hasMore1, len(p1), err)
	}
	p3, total3, hasMore3, err := srch.SearchPaged(ctx, "pagination_common_term", "en-US", nil, 3, 2)
	if err != nil || total3 != 6 || hasMore3 || len(p3) != 2 {
		t.Fatalf("unexpected page 3: total=%d hasMore=%v len=%d err=%v", total3, hasMore3, len(p3), err)
	}
	if p1[0].ID == p3[0].ID {
		t.Fatalf("expected distinct pages, got p1[0]=%s p3[0]=%s", p1[0].ID, p3[0].ID)
	}

	// Verify Stage 2 SQL overflow searches unlocked content_md AND standalone pages while never leaking locked content!
	tinyIndex := visibility.NewSearchIndex(store, render.Default(), 1) // 1-byte ceiling forces overflow=true
	if err := tinyIndex.Rebuild(ctx); err != nil {
		t.Fatalf("tinyIndex Rebuild: %v", err)
	}
	s2Art, s2ArtTotal, _, err := tinyIndex.SearchPaged(ctx, "DEEP_BODY_KEYWORD_1500", "en-US", nil, 1, 10)
	if err != nil || s2ArtTotal != 1 || len(s2Art) != 1 || s2Art[0].Slug != "arch-overview" {
		t.Fatalf("Stage 2 SQL fallback missed unlocked content_md: total=%d hits=%+v err=%v", s2ArtTotal, s2Art, err)
	}
	s2Page, s2PageTotal, _, err := tinyIndex.SearchPaged(ctx, "STANDALONE_PAGE_TOKEN_42", "en-US", nil, 1, 10)
	if err != nil || s2PageTotal != 1 || len(s2Page) != 1 || s2Page[0].URL != "/about" {
		t.Fatalf("Stage 2 SQL fallback missed published standalone page: total=%d hits=%+v err=%v", s2PageTotal, s2Page, err)
	}
	for _, secret := range []string{"LOCKED_SECRET_TOKEN_7741", "WHOLE_PWD_SECRET_8822", "PAGE_LOCK_SECRET_999"} {
		leaked, lTotal, _, _ := tinyIndex.SearchPaged(ctx, secret, "en-US", nil, 1, 10)
		if lTotal != 0 || len(leaked) != 0 {
			t.Fatalf("CRITICAL: Stage 2 SQL fallback leaked locked token %q: %+v", secret, leaked)
		}
	}
}

func TestPopulateCardFullHTML_ZeroLeakAndSanitization(t *testing.T) {
	ctx := context.Background()
	_, grd, _ := newVisibilityBenchEnv(t)
	req := httptest.NewRequest("GET", "/", nil)

	// 1. Public unlocked article renders sanitized FullHTML
	pubRaw := domain.RawArticle{
		ID:          "full-pub",
		Title:       "Public Full Post",
		Slug:        "public-full",
		ContentMD:   "Hello **full mode**!<script>alert(1)</script>",
		Status:      domain.ArticleStatusPublished,
		PublishedAt: 1700000000000,
		UpdatedAt:   1700000000000,
	}
	pubCard := grd.ToCard(pubRaw, "en-US", nil, nil, nil)
	grd.PopulateCardFullHTML(ctx, req, nil, pubRaw, &pubCard)
	if !strings.Contains(pubCard.FullHTML, "<strong>full mode</strong>") || strings.Contains(pubCard.FullHTML, "<script") {
		t.Fatalf("expected sanitized FullHTML for public post, got %q", pubCard.FullHTML)
	}

	// 2. Password-protected article NEVER populates FullHTML (even if admin viewer is passed!)
	cipherPwd, err := grd.SealArticlePassword("full-pwd", "secret")
	if err != nil {
		t.Fatal(err)
	}
	pwdRaw := pubRaw
	pwdRaw.ID = "full-pwd"
	pwdRaw.ContentMD = "TOP_SECRET_FULL_FEED"
	pwdRaw.PasswordCiphertext = cipherPwd
	pwdCard := grd.ToCard(pwdRaw, "en-US", nil, nil, nil)
	admin := &domain.User{ID: "u1", Role: domain.RoleAdmin, Status: domain.UserStatusActive}
	grd.PopulateCardFullHTML(ctx, req, admin, pwdRaw, &pwdCard)
	if pwdCard.FullHTML != "" {
		t.Fatalf("CRITICAL: password-protected post leaked FullHTML: %q", pwdCard.FullHTML)
	}

	// 3. HasContentLock / inline :::lock article NEVER populates FullHTML
	lockRaw := pubRaw
	lockRaw.ID = "full-lock"
	lockRaw.HasContentLock = true
	lockRaw.ContentMD = "Lead\n\n:::lock pwd=\"x\"\nhidden\n:::\n"
	lockCard := grd.ToCard(lockRaw, "en-US", nil, nil, nil)
	grd.PopulateCardFullHTML(ctx, req, admin, lockRaw, &lockCard)
	if lockCard.FullHTML != "" {
		t.Fatalf("CRITICAL: content-locked post leaked FullHTML: %q", lockCard.FullHTML)
	}
}
