package server_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/HugoSmits86/nativewebp"
	"github.com/Willh92/Volo-go/internal/auth"
	"github.com/Willh92/Volo-go/internal/domain"
	"github.com/Willh92/Volo-go/pkg/id"
)

func solveGuestPoW(challenge string, difficulty int) string {
	for nonce := 0; nonce < 5_000_000; nonce++ {
		ns := strconv.Itoa(nonce)
		sum := sha256.Sum256([]byte(challenge + ":" + ns))
		if hasLeadingZeroBitsTest(sum[:], difficulty) {
			return ns
		}
	}
	return ""
}

func hasLeadingZeroBitsTest(hash []byte, bits int) bool {
	fullBytes := bits / 8
	remBits := bits % 8
	for i := 0; i < fullBytes; i++ {
		if hash[i] != 0 {
			return false
		}
	}
	if remBits > 0 {
		mask := byte(0xFF << (8 - remBits))
		if hash[fullBytes]&mask != 0 {
			return false
		}
	}
	return true
}

func fetchGuestPoW(t *testing.T, baseURL string) (string, string) {
	t.Helper()
	resp, raw := doJSONReq(t, http.MethodGet, baseURL+"/api/v1/pow/challenge", nil, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/v1/pow/challenge = %d %s", resp.StatusCode, string(raw))
	}
	var ch struct {
		Challenge  string `json:"challenge"`
		Difficulty int    `json:"difficulty"`
	}
	if err := json.Unmarshal(raw, &ch); err != nil {
		t.Fatal(err)
	}
	return ch.Challenge, solveGuestPoW(ch.Challenge, ch.Difficulty)
}

func createAndLoginReader(t *testing.T, env *m5Env, username, email, password string) (domain.User, []*http.Cookie, map[string]string) {
	t.Helper()
	code, err := env.authSvc.IssueReaderVerificationCode(email)
	if err != nil {
		t.Fatalf("IssueReaderVerificationCode: %v", err)
	}
	resp, raw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/v1/reader/register", map[string]any{
		"username": username,
		"email":    email,
		"password": password,
		"code":     code,
	}, nil, map[string]string{"Origin": env.ts.URL})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("register reader %s = %d %s", username, resp.StatusCode, string(raw))
	}
	var reg struct {
		User      domain.User `json:"user"`
		CSRFToken string      `json:"csrf_token"`
	}
	if err := json.Unmarshal(raw, &reg); err != nil {
		t.Fatal(err)
	}
	hdrs := map[string]string{
		"Origin":            env.ts.URL,
		auth.CSRFHeaderName: reg.CSRFToken,
	}
	return reg.User, resp.Cookies(), hdrs
}

func TestV100_PerIdentityCommentBounds_And_HeightBombDefense(t *testing.T) {
	env, adminCookies, adminHdrs, _ := bootstrapAuditEnv(t)
	adminHdrs["Origin"] = env.ts.URL

	// Disable guest PoW and auto-approve comments so we can exercise length & line limits rapidly without WAF rate noise.
	resp, raw := doJSONReq(t, http.MethodPatch, env.ts.URL+"/api/admin/settings/reading", map[string]any{
		"posts_per_page":        10,
		"comment_mode":          "guest_allowed",
		"comment_moderation":    "all_auto",
		"enable_pow":            false,
		"allow_reader_register": true,
	}, adminCookies, adminHdrs)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH settings/reading = %d %s", resp.StatusCode, string(raw))
	}

	resp, artRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/articles", map[string]any{
		"title":         "Comment Bounds & Height Guard",
		"slug":          "comment-bounds-height",
		"content_md":    "Testing per-identity limits and vertical height defense.",
		"status":        "published",
		"allow_comment": true,
	}, adminCookies, adminHdrs)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create article = %d %s", resp.StatusCode, string(artRaw))
	}
	var createdArt struct {
		Article domain.RawArticle `json:"article"`
	}
	_ = json.Unmarshal(artRaw, &createdArt)
	artID := createdArt.Article.ID

	_, readerCookies, readerHdrs := createAndLoginReader(t, env, "reader_one", "reader_one@example.com", "Str0ng!Reader#2026")

	// 1. Verify GET /api/v1/comments returns identity-aware bounds: Guest=512/16, Reader=1024/24, Admin=4096/64, Min=2
	checkBounds := func(cookies []*http.Cookie, wantMin, wantMax, wantLines int) {
		t.Helper()
		r, b := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/v1/comments?target_type=article&target_id="+artID, nil, cookies, nil)
		if r.StatusCode != http.StatusOK {
			t.Fatalf("GET comments = %d %s", r.StatusCode, string(b))
		}
		var meta struct {
			MinLen   int `json:"comment_min_length"`
			MaxLen   int `json:"comment_max_length"`
			MaxLines int `json:"comment_max_lines"`
		}
		_ = json.Unmarshal(b, &meta)
		if meta.MinLen != wantMin || meta.MaxLen != wantMax || meta.MaxLines != wantLines {
			t.Fatalf("bounds mismatch: got min=%d max=%d lines=%d, want min=%d max=%d lines=%d",
				meta.MinLen, meta.MaxLen, meta.MaxLines, wantMin, wantMax, wantLines)
		}
	}
	checkBounds(nil, 2, 512, 16)
	checkBounds(readerCookies, 2, 1024, 24)
	checkBounds(adminCookies, 2, 4096, 64)

	guestHdrs := map[string]string{"Origin": env.ts.URL, "Accept-Language": "zh-CN"}

	// 2. Min length = 2 runes: 1 rune rejected, 2 runes accepted
	resp, raw = doJSONReq(t, http.MethodPost, env.ts.URL+"/api/v1/comments", map[string]any{
		"target_type": "article",
		"target_id":   artID,
		"author_name": "GuestA",
		"content_md":  "好",
	}, nil, guestHdrs)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(raw), "2～512") {
		t.Fatalf("expected 1-rune guest comment to fail with 2~512 message, got %d %s", resp.StatusCode, string(raw))
	}

	// 3. Guest max = 512 runes: 513 runes rejected for guest, but accepted for logged-in reader (<= 1024)!
	text513 := strings.Repeat("测", 513)
	resp, raw = doJSONReq(t, http.MethodPost, env.ts.URL+"/api/v1/comments", map[string]any{
		"target_type": "article",
		"target_id":   artID,
		"author_name": "GuestA",
		"content_md":  text513,
	}, nil, guestHdrs)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 513-rune guest comment to fail, got %d %s", resp.StatusCode, string(raw))
	}

	resp, raw = doJSONReq(t, http.MethodPost, env.ts.URL+"/api/v1/comments", map[string]any{
		"target_type": "article",
		"target_id":   artID,
		"content_md":  text513,
	}, readerCookies, readerHdrs)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 513-rune reader comment to succeed (<=1024), got %d %s", resp.StatusCode, string(raw))
	}

	// Reader 1025 runes rejected, Admin 1500 runes accepted!
	text1025 := strings.Repeat("读", 1025)
	resp, raw = doJSONReq(t, http.MethodPost, env.ts.URL+"/api/v1/comments", map[string]any{
		"target_type": "article",
		"target_id":   artID,
		"content_md":  text1025,
	}, readerCookies, readerHdrs)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 1025-rune reader comment to fail, got %d %s", resp.StatusCode, string(raw))
	}

	resp, raw = doJSONReq(t, http.MethodPost, env.ts.URL+"/api/v1/comments", map[string]any{
		"target_type": "article",
		"target_id":   artID,
		"content_md":  strings.Repeat("管", 1500),
	}, adminCookies, adminHdrs)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 1500-rune admin comment to succeed (<=4096), got %d %s", resp.StatusCode, string(raw))
	}

	// 4. Vertical Height Bombing Defense:
	// (a) Invisible blank lines (Braille U+2800, Hangul U+3164, ZWSP), lone backslashes, consecutive <hr>, consecutive <br>, H1 headings, and Zalgo towers
	zalgoTower := "Z" + strings.Repeat("\u0300", 120)
	heightPayload := "# Giant Title\n\n\n\n\u2800\n\u3164\n\\\n\\\n---\n---\n" + zalgoTower + "<br><br><br><br><br>Tail"
	resp, prevRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/v1/comments/preview", map[string]any{
		"content_md": heightPayload,
	}, readerCookies, readerHdrs)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/v1/comments/preview = %d %s", resp.StatusCode, string(prevRaw))
	}
	var prevRes struct {
		HTML string `json:"html"`
	}
	_ = json.Unmarshal(prevRaw, &prevRes)
	if strings.Contains(prevRes.HTML, "<h1") || !strings.Contains(prevRes.HTML, "<h4>Giant Title</h4>") {
		t.Fatalf("expected H1 in comment to be demoted to <h4>, got: %s", prevRes.HTML)
	}
	if strings.Count(prevRes.HTML, "<hr") > 1 {
		t.Fatalf("expected consecutive <hr> rules to collapse to 1, got: %s", prevRes.HTML)
	}
	if strings.Count(prevRes.HTML, "<br") > 2 {
		t.Fatalf("expected consecutive <br> hard breaks to be capped at <= 2, got: %s", prevRes.HTML)
	}
	if strings.Count(prevRes.HTML, "\u0300") > 3 {
		t.Fatalf("expected Zalgo combining marks to be capped at <= 3, got %d", strings.Count(prevRes.HTML, "\u0300"))
	}

	// (b) Line-break flooding (25 distinct non-empty lines) rejected for reader (max 24 lines)!
	var spamLines []string
	for i := 0; i < 25; i++ {
		spamLines = append(spamLines, "row-"+strconv.Itoa(i))
	}
	resp, raw = doJSONReq(t, http.MethodPost, env.ts.URL+"/api/v1/comments", map[string]any{
		"target_type": "article",
		"target_id":   artID,
		"content_md":  strings.Join(spamLines, "\n"),
	}, readerCookies, map[string]string{
		"Origin":            env.ts.URL,
		"Accept-Language":   "zh-CN",
		auth.CSRFHeaderName: readerHdrs[auth.CSRFHeaderName],
	})
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(raw), "评论换行过多") {
		t.Fatalf("expected 25-line comment to be blocked by vertical line cap, got %d %s", resp.StatusCode, string(raw))
	}

	// 5. Dynamic Admin Console configuration override per identity
	resp, raw = doJSONReq(t, http.MethodPatch, env.ts.URL+"/api/admin/settings/reading", map[string]any{
		"posts_per_page":           10,
		"comment_access_mode":      "all",
		"comment_moderation_mode":  "off",
		"allow_reader_register":    true,
		"comment_min_length":       5,
		"comment_max_length_guest": 120,
		"comment_max_length_user":  300,
		"comment_max_lines_guest":  6,
		"comment_max_lines_user":   10,
	}, adminCookies, adminHdrs)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH custom comment bounds = %d %s", resp.StatusCode, string(raw))
	}
	checkBounds(nil, 5, 120, 6)
	checkBounds(readerCookies, 5, 300, 10)
}

func TestV100_AccountBanMute_TimedExpiry_And_ReversibleCommentShielding(t *testing.T) {
	env, adminCookies, adminHdrs, adminID := bootstrapAuditEnv(t)
	adminHdrs["Origin"] = env.ts.URL

	resp, raw := doJSONReq(t, http.MethodPatch, env.ts.URL+"/api/admin/settings/reading", map[string]any{
		"posts_per_page":        10,
		"comment_mode":          "guest_allowed",
		"comment_moderation":    "all_auto",
		"enable_pow":            false,
		"allow_reader_register": true,
	}, adminCookies, adminHdrs)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH settings/reading = %d %s", resp.StatusCode, string(raw))
	}

	resp, artRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/articles", map[string]any{
		"title":         "Discourse Moderation Post",
		"slug":          "discourse-moderation",
		"content_md":    "Testing ban, mute, and reversible historical comment shielding.",
		"status":        "published",
		"allow_comment": true,
	}, adminCookies, adminHdrs)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create article = %d %s", resp.StatusCode, string(artRaw))
	}
	var createdArt struct {
		Article domain.RawArticle `json:"article"`
	}
	_ = json.Unmarshal(artRaw, &createdArt)
	artID := createdArt.Article.ID

	// Self-ban / self-mute guard on admin
	resp, raw = doJSONReq(t, http.MethodPatch, env.ts.URL+"/api/admin/users/"+adminID, map[string]any{
		"action": "ban",
	}, adminCookies, adminHdrs)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected self-ban on admin to fail with 400, got %d %s", resp.StatusCode, string(raw))
	}

	// Register Alice and post a historical comment
	alice, aliceCookies, aliceHdrs := createAndLoginReader(t, env, "alice_mod", "alice_mod@example.com", "Str0ng!Alice#2026")
	origCommentText := "Alice original historical comment with **markdown**!"
	resp, cmtRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/v1/comments", map[string]any{
		"target_type": "article",
		"target_id":   artID,
		"author_url":  "https://alice.example.com",
		"content_md":  origCommentText,
	}, aliceCookies, aliceHdrs)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("Alice create comment = %d %s", resp.StatusCode, string(cmtRaw))
	}
	var cmtCreated struct {
		Comment struct {
			ID string `json:"id"`
		} `json:"comment"`
	}
	_ = json.Unmarshal(cmtRaw, &cmtCreated)
	aliceCommentID := cmtCreated.Comment.ID

	// 1. Mute Alice with historical comment shielding enabled + custom shield copy
	customShieldCopy := "该账号处于禁言观察期，历史评论暂予隐藏。"
	resp, raw = doJSONReq(t, http.MethodPatch, env.ts.URL+"/api/admin/users/"+alice.ID, map[string]any{
		"action":          "mute",
		"duration_hours":  24,
		"ban_reason":      "刷屏灌水",
		"shield_comments": true,
		"shield_text":     customShieldCopy,
	}, adminCookies, adminHdrs)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("mute Alice = %d %s", resp.StatusCode, string(raw))
	}

	// Muted Alice CAN still log in!
	resp, loginRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/v1/reader/login", map[string]any{
		"ident":    "alice_mod",
		"password": "Str0ng!Alice#2026",
	}, nil, map[string]string{"Origin": env.ts.URL})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected muted Alice to still be able to log in, got %d %s", resp.StatusCode, string(loginRaw))
	}

	// Muted Alice CANNOT post comments (403), edit comments (403), or react (403)!
	resp, raw = doJSONReq(t, http.MethodPost, env.ts.URL+"/api/v1/comments", map[string]any{
		"target_type": "article",
		"target_id":   artID,
		"content_md":  "Trying to comment while muted",
	}, aliceCookies, aliceHdrs)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected muted Alice comment POST to fail with 403, got %d %s", resp.StatusCode, string(raw))
	}

	resp, raw = doJSONReq(t, http.MethodPatch, env.ts.URL+"/api/v1/comments/"+aliceCommentID, map[string]any{
		"content_md": "Trying to edit while muted",
	}, aliceCookies, aliceHdrs)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected muted Alice comment PATCH to fail with 403, got %d %s", resp.StatusCode, string(raw))
	}

	resp, raw = doJSONReq(t, http.MethodPost, env.ts.URL+"/api/v1/reactions", map[string]any{
		"target_type": "article",
		"target_id":   artID,
		"kind":        "like",
	}, aliceCookies, aliceHdrs)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected muted Alice reaction POST to fail with 403, got %d %s", resp.StatusCode, string(raw))
	}

	// Public GET /api/v1/comments AND /dynamic must show replacement copy and NOT leak original text or author_url!
	resp, listRaw := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/v1/comments?target_type=article&target_id="+artID, nil, aliceCookies, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET comments = %d %s", resp.StatusCode, string(listRaw))
	}
	listStr := string(listRaw)
	if strings.Contains(listStr, "Alice original historical comment") || strings.Contains(listStr, "alice.example.com") {
		t.Fatalf("shielded comment leaked original content or author_url: %s", listStr)
	}
	if !strings.Contains(listStr, customShieldCopy) || !strings.Contains(listStr, `"is_shielded":true`) || !strings.Contains(listStr, `"viewer_muted":true`) {
		t.Fatalf("expected shielded placeholder + is_shielded + viewer_muted in response, got: %s", listStr)
	}

	resp, dynRaw := doJSONReq(t, http.MethodGet, env.ts.URL+"/dynamic", nil, nil, nil)
	if resp.StatusCode == http.StatusOK && strings.Contains(string(dynRaw), "Alice original historical comment") {
		t.Fatalf("/dynamic recent comments leaked shielded comment: %s", string(dynRaw))
	}

	// Verify DB row in `comments` still holds the untouched original Markdown!
	var dbContentMD string
	if err := env.store.QueryRowContext(env.ctx, `SELECT content_md FROM comments WHERE id = ?`, aliceCommentID).Scan(&dbContentMD); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(dbContentMD, "Alice original historical comment") {
		t.Fatalf("DB content_md was destructively overwritten: %q", dbContentMD)
	}

	// 2. Unmute Alice -> original comment immediately restored!
	resp, raw = doJSONReq(t, http.MethodPatch, env.ts.URL+"/api/admin/users/"+alice.ID, map[string]any{
		"action": "unmute",
	}, adminCookies, adminHdrs)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unmute Alice = %d %s", resp.StatusCode, string(raw))
	}

	resp, listRaw = doJSONReq(t, http.MethodGet, env.ts.URL+"/api/v1/comments?target_type=article&target_id="+artID, nil, nil, nil)
	if !strings.Contains(string(listRaw), "Alice original historical comment") || strings.Contains(string(listRaw), customShieldCopy) {
		t.Fatalf("expected unmuting Alice to restore original comment, got: %s", string(listRaw))
	}

	// 3. Timed Ban with Shielding -> kicks sessions, blocks login (403), shields comments; auto-restores upon expiry!
	resp, raw = doJSONReq(t, http.MethodPatch, env.ts.URL+"/api/admin/users/"+alice.ID, map[string]any{
		"action":          "ban",
		"duration_hours":  2,
		"ban_reason":      "恶意攻击",
		"shield_comments": true,
	}, adminCookies, adminHdrs)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("ban Alice = %d %s", resp.StatusCode, string(raw))
	}

	// Banned reader login with correct password returns 403 with localized ban reason
	resp, loginRaw = doJSONReq(t, http.MethodPost, env.ts.URL+"/api/v1/reader/login", map[string]any{
		"ident":    "alice_mod",
		"password": "Str0ng!Alice#2026",
	}, nil, map[string]string{"Origin": env.ts.URL, "Accept-Language": "zh-CN"})
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(loginRaw), "封禁") || !strings.Contains(string(loginRaw), "恶意攻击") {
		t.Fatalf("expected 403 localized ban notice on reader login, got %d %s", resp.StatusCode, string(loginRaw))
	}

	// Probing staff username on /api/v1/reader/login still returns 401 (anti-enumeration preserved!)
	resp, staffProbeRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/v1/reader/login", map[string]any{
		"ident":    "captain",
		"password": "Str0ng!Pass#2026",
	}, nil, map[string]string{"Origin": env.ts.URL})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected staff probe on /api/v1/reader/login to remain 401, got %d %s", resp.StatusCode, string(staffProbeRaw))
	}

	// Comments are shielded while banned
	resp, listRaw = doJSONReq(t, http.MethodGet, env.ts.URL+"/api/v1/comments?target_type=article&target_id="+artID, nil, nil, map[string]string{"Accept-Language": "zh-CN"})
	if strings.Contains(string(listRaw), "Alice original historical comment") || !strings.Contains(string(listRaw), "volo-comment-shielded") {
		t.Fatalf("expected banned Alice comment to be shielded, got: %s", string(listRaw))
	}

	// Fast-forward ban_until into the past to verify automatic expiry healing & comment restoration!
	pastMS := time.Now().Add(-5 * time.Minute).UnixMilli()
	if _, err := env.store.ExecContext(env.ctx, `UPDATE users SET ban_until = ? WHERE id = ?`, pastMS, alice.ID); err != nil {
		t.Fatal(err)
	}
	env.content.PurgeRuntimeCaches()

	resp, listRaw = doJSONReq(t, http.MethodGet, env.ts.URL+"/api/v1/comments?target_type=article&target_id="+artID, nil, nil, nil)
	if !strings.Contains(string(listRaw), "Alice original historical comment") {
		t.Fatalf("expected expired timed ban to auto-restore Alice's historical comment, got: %s", string(listRaw))
	}

	resp, loginRaw = doJSONReq(t, http.MethodPost, env.ts.URL+"/api/v1/reader/login", map[string]any{
		"ident":    "alice_mod",
		"password": "Str0ng!Alice#2026",
	}, nil, map[string]string{"Origin": env.ts.URL})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected Alice to log in cleanly after timed ban expired, got %d %s", resp.StatusCode, string(loginRaw))
	}
}

func TestV100_ImageViewHTTP_NonDestructive_OrphanAndCLS_Permalinks_And_CSRFConsole(t *testing.T) {
	env, adminCookies, adminHdrs, adminID := bootstrapAuditEnv(t)
	adminHdrs["Origin"] = env.ts.URL

	// 1. CSRF Console Header Guard on GET /api/admin/me
	resp, meNoHdr := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/admin/me", nil, adminCookies, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/admin/me = %d", resp.StatusCode)
	}
	var me1 struct {
		CSRFToken string `json:"csrf_token"`
	}
	_ = json.Unmarshal(meNoHdr, &me1)
	if me1.CSRFToken != "" {
		t.Fatalf("expected empty csrf_token without X-Volo-Console: 1, got %q", me1.CSRFToken)
	}

	resp, meWithHdr := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/admin/me", nil, adminCookies, map[string]string{"X-Volo-Console": "1"})
	var me2 struct {
		CSRFToken string `json:"csrf_token"`
	}
	_ = json.Unmarshal(meWithHdr, &me2)
	if me2.CSRFToken == "" {
		t.Fatalf("expected non-empty csrf_token with X-Volo-Console: 1")
	}

	// 2. Seed a real 200x100 PNG in uploads/2026/11/sample.png and verify non-destructive real-time transforms
	relKey := "2026/11/sample.png"
	absPath := filepath.Join(env.cfg.GetUploadDir(), filepath.FromSlash(relKey))
	if err := os.MkdirAll(filepath.Dir(absPath), 0o750); err != nil {
		t.Fatal(err)
	}
	img := image.NewNRGBA(image.Rect(0, 0, 200, 100))
	for y := 0; y < 100; y++ {
		for x := 0; x < 200; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: uint8(x), G: uint8(y * 2), B: 140, A: 255})
		}
	}
	var pngBuf bytes.Buffer
	if err := png.Encode(&pngBuf, img); err != nil {
		t.Fatal(err)
	}
	origBytes := pngBuf.Bytes()
	origSum := sha256.Sum256(origBytes)
	origHex := hex.EncodeToString(origSum[:])
	if err := os.WriteFile(absPath, origBytes, 0o640); err != nil {
		t.Fatal(err)
	}

	assetID := id.Next()
	pubPath := "/uploads/" + relKey
	nowMS := time.Now().UnixMilli()
	if _, err := env.store.ExecContext(env.ctx,
		`INSERT INTO media_assets (id, storage_driver, object_key, public_url, original_name, alt_text, mime_type, size_bytes, width, height, sha256, exif_stripped, uploader_id, created_at)
		 VALUES (?, 'local', ?, ?, 'sample.png', '', 'image/png', ?, 200, 100, ?, 1, ?, ?)`,
		assetID, relKey, pubPath, len(origBytes), origHex, adminID, nowMS); err != nil {
		t.Fatal(err)
	}
	env.content.PutMediaDims(pubPath, 200, 100)

	// (a) Unparameterized passthrough returns exact original bytes
	resp, passthroughBytes := doJSONReq(t, http.MethodGet, env.ts.URL+pubPath, nil, nil, nil)
	if resp.StatusCode != http.StatusOK || !bytes.Equal(passthroughBytes, origBytes) {
		t.Fatalf("unparameterized passthrough mismatch: status=%d len=%d (want %d)", resp.StatusCode, len(passthroughBytes), len(origBytes))
	}

	// (b) Qiniu path style vs KV style produce identical ETag and valid 80x60 WebP
	respQ, webpQ := doJSONReq(t, http.MethodGet, env.ts.URL+pubPath+"?imageView2/1/w/80/h/60/format/webp/q/82", nil, nil, nil)
	if respQ.StatusCode != http.StatusOK || respQ.Header.Get("Content-Type") != "image/webp" {
		t.Fatalf("Qiniu imageView2 failed: %d ct=%s body=%s", respQ.StatusCode, respQ.Header.Get("Content-Type"), string(webpQ))
	}
	cfgW, err := nativewebp.DecodeConfig(bytes.NewReader(webpQ))
	if err != nil || cfgW.Width != 80 || cfgW.Height != 60 {
		t.Fatalf("expected 80x60 WebP, got %+v err=%v", cfgW, err)
	}
	etagQ := respQ.Header.Get("ETag")
	if etagQ == "" {
		t.Fatalf("expected strong ETag on transformed image")
	}

	respKV, _ := doJSONReq(t, http.MethodGet, env.ts.URL+pubPath+"?mode=1&w=80&h=60&format=webp&q=82", nil, nil, nil)
	if respKV.StatusCode != http.StatusOK || respKV.Header.Get("ETag") != etagQ {
		t.Fatalf("expected KV query to share identical ETag %s, got %d %s", etagQ, respKV.StatusCode, respKV.Header.Get("ETag"))
	}

	// (c) Conditional If-None-Match returns 304 Not Modified with zero body
	resp304, body304 := doJSONReq(t, http.MethodGet, env.ts.URL+pubPath+"?mode=1&w=80&h=60&format=webp&q=82", nil, nil, map[string]string{
		"If-None-Match": etagQ,
	})
	if resp304.StatusCode != http.StatusNotModified || len(body304) != 0 {
		t.Fatalf("expected 304 Not Modified with empty body, got %d (%d bytes)", resp304.StatusCode, len(body304))
	}

	// (d) format=avif transcode fallback emits X-Volo-Image-Format-Fallback: avif->webp
	respAVIF, _ := doJSONReq(t, http.MethodGet, env.ts.URL+pubPath+"?w=64&format=avif", nil, nil, nil)
	if respAVIF.StatusCode != http.StatusOK || respAVIF.Header.Get("X-Volo-Image-Format-Fallback") != "avif->webp" {
		t.Fatalf("expected avif->webp fallback header, got status=%d hdr=%q", respAVIF.StatusCode, respAVIF.Header.Get("X-Volo-Image-Format-Fallback"))
	}

	// (e) Malformed mode without ignore-error -> 400; with ignore-error=1 -> 200 original PNG!
	respBad, _ := doJSONReq(t, http.MethodGet, env.ts.URL+pubPath+"?imageView2/99/w/80", nil, nil, nil)
	if respBad.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 on invalid mode without ignore-error, got %d", respBad.StatusCode)
	}
	respIgn, ignBytes := doJSONReq(t, http.MethodGet, env.ts.URL+pubPath+"?imageView2/99/w/80/ignore-error/1", nil, nil, nil)
	if respIgn.StatusCode != http.StatusOK || !bytes.Equal(ignBytes, origBytes) {
		t.Fatalf("expected ignore-error/1 to fall back to untouched original PNG, got %d len=%d", respIgn.StatusCode, len(ignBytes))
	}

	// (f) Original file on disk MUST remain 100% byte-for-byte unmutated!
	afterDisk, err := os.ReadFile(absPath)
	if err != nil {
		t.Fatal(err)
	}
	afterSum := sha256.Sum256(afterDisk)
	if hex.EncodeToString(afterSum[:]) != origHex {
		t.Fatalf("CRITICAL: original upload file on disk was mutated!")
	}

	// 3. CLS Dimension Prediction + Orphan Scanner Protection for parameterized /uploads/...?imageView2/... URLs
	resp, artRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/articles", map[string]any{
		"title":      "Parameterized Image Post",
		"slug":       "param-image-post",
		"content_md": "![cropped](" + pubPath + "?imageView2/1/w/80/h/60/format/webp)",
		"status":     "published",
	}, adminCookies, adminHdrs)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create article = %d %s", resp.StatusCode, string(artRaw))
	}
	var artCreated struct {
		Article domain.RawArticle `json:"article"`
	}
	_ = json.Unmarshal(artRaw, &artCreated)

	resp, postHTMLBytes := doJSONReq(t, http.MethodGet, env.ts.URL+"/posts/param-image-post", nil, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /posts/param-image-post = %d", resp.StatusCode)
	}
	postHTML := string(postHTMLBytes)
	if !strings.Contains(postHTML, `width="80"`) || !strings.Contains(postHTML, `height="60"`) {
		t.Fatalf("expected predicted transformed CLS dimensions width=80 height=60 in HTML, got:\n%s", postHTML)
	}

	resp, orphRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/media/orphans?delete=0", nil, adminCookies, adminHdrs)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/admin/media/orphans = %d %s", resp.StatusCode, string(orphRaw))
	}
	if strings.Contains(string(orphRaw), assetID) || strings.Contains(string(orphRaw), relKey) {
		t.Fatalf("upload referenced with ?imageView2 query params was falsely flagged as orphan: %s", string(orphRaw))
	}

	// 4. PostPermalinkStyle Closure (`/p/{slug}`, `/archives/{slug}`, `/{year}/{month}/{slug}`, `/posts/{slug}` + `.md` raw markdown + search URL)
	_, _ = doJSONReq(t, http.MethodPatch, env.ts.URL+"/api/admin/settings/feed", map[string]any{
		"output_mode":         "full",
		"item_count":          20,
		"enable_llms_txt":     true,
		"enable_raw_markdown": true,
	}, adminCookies, adminHdrs)

	pubTime := time.UnixMilli(artCreated.Article.CreatedAt).UTC()
	if artCreated.Article.PublishedAt > 0 {
		pubTime = time.UnixMilli(artCreated.Article.PublishedAt).UTC()
	}
	ymPrefix := "/" + pubTime.Format("2006/01") + "/param-image-post"

	for _, tc := range []struct {
		style    string
		wantPath string
		wantMD   string
	}{
		{domain.PermalinkStyleShortP, "/p/param-image-post", "/p/param-image-post.md"},
		{domain.PermalinkStyleArchives, "/archives/param-image-post", "/archives/param-image-post.md"},
		{domain.PermalinkStyleYearMonth, ymPrefix, ymPrefix + ".md"},
		{domain.PermalinkStylePosts, "/posts/param-image-post", "/posts/param-image-post.md"},
	} {
		rSet, bSet := doJSONReq(t, http.MethodPatch, env.ts.URL+"/api/admin/settings/seo", map[string]any{
			"enable_sitemap":       true,
			"enable_robots_txt":    true,
			"post_permalink_style": tc.style,
		}, adminCookies, adminHdrs)
		if rSet.StatusCode != http.StatusOK {
			t.Fatalf("PATCH seo permalink style %s = %d %s", tc.style, rSet.StatusCode, string(bSet))
		}
		rPage, _ := doJSONReq(t, http.MethodGet, env.ts.URL+tc.wantPath, nil, nil, nil)
		if rPage.StatusCode != http.StatusOK {
			t.Fatalf("GET %s (style=%s) = %d", tc.wantPath, tc.style, rPage.StatusCode)
		}
		rMD, mdBytes := doJSONReq(t, http.MethodGet, env.ts.URL+tc.wantMD, nil, nil, nil)
		if rMD.StatusCode != http.StatusOK || !strings.Contains(string(mdBytes), "Parameterized Image Post") {
			t.Fatalf("GET %s (style=%s) = %d %s", tc.wantMD, tc.style, rMD.StatusCode, string(mdBytes))
		}
		rHome, homeB := doJSONReq(t, http.MethodGet, env.ts.URL+"/", nil, nil, nil)
		if rHome.StatusCode != http.StatusOK || !strings.Contains(string(homeB), tc.wantPath) {
			t.Fatalf("expected home HTML to link canonical permalink %s for style=%s", tc.wantPath, tc.style)
		}
		rSearch, searchB := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/v1/search?q=Parameterized", nil, nil, nil)
		if rSearch.StatusCode != http.StatusOK || !strings.Contains(string(searchB), tc.wantPath) {
			t.Fatalf("expected search index URL %s for style=%s, got: %s", tc.wantPath, tc.style, string(searchB))
		}
	}
}
