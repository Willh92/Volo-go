package server_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/Willh92/Volo-go/internal/domain"
	"github.com/Willh92/Volo-go/internal/guest"
)

func TestAdminIPBanLifecycleCommentBanDurationsAndLogClear(t *testing.T) {
	env, adminCookies, adminHdrs, _ := setupRound2Env(t)

	// 1. Create a 15-minute manual IP ban via expires_minutes
	createResp, createRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/security/ip-bans", map[string]any{
		"cidr":            "203.0.113.55",
		"note":            "suspicious burst",
		"expires_minutes": 15,
	}, adminCookies, adminHdrs)
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("POST ip-bans = %d %s", createResp.StatusCode, string(createRaw))
	}
	var created struct {
		Ban domain.SecurityIPBan `json:"ban"`
	}
	_ = json.Unmarshal(createRaw, &created)
	if created.Ban.ID == "" || created.Ban.ExpiresAt <= 0 {
		t.Fatalf("expected finite 15m ban, got %+v", created.Ban)
	}

	// 2. Unban (early expire) preserves historical record and updates counts (active=0, expired=1, all=1)
	unbanResp, unbanRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/security/ip-bans/"+created.Ban.ID+"/unban", map[string]any{}, adminCookies, adminHdrs)
	if unbanResp.StatusCode != http.StatusOK {
		t.Fatalf("POST unban = %d %s", unbanResp.StatusCode, string(unbanRaw))
	}
	listResp, listRaw := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/admin/security/ip-bans?state=expired", nil, adminCookies, nil)
	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("GET expired bans = %d %s", listResp.StatusCode, string(listRaw))
	}
	var listBody struct {
		Items  []domain.SecurityIPBan `json:"items"`
		Counts struct {
			Active  int `json:"active"`
			Expired int `json:"expired"`
			All     int `json:"all"`
		} `json:"counts"`
	}
	_ = json.Unmarshal(listRaw, &listBody)
	if listBody.Counts.Active != 0 || listBody.Counts.Expired != 1 || listBody.Counts.All != 1 || len(listBody.Items) != 1 {
		t.Fatalf("expected unban to preserve expired record (active=0, expired=1, all=1), got counts=%+v items=%d", listBody.Counts, len(listBody.Items))
	}

	// 3. Re-ban via PATCH increments occurrences and updates note + permanent duration (expires_minutes=0)
	patchResp, patchRaw := doJSONReq(t, http.MethodPatch, env.ts.URL+"/api/admin/security/ip-bans/"+created.Ban.ID, map[string]any{
		"note":            "escalated to permanent",
		"expires_minutes": 0,
		"reban":           true,
	}, adminCookies, adminHdrs)
	if patchResp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH ip-ban = %d %s", patchResp.StatusCode, string(patchRaw))
	}
	var patched struct {
		Ban domain.SecurityIPBan `json:"ban"`
	}
	_ = json.Unmarshal(patchRaw, &patched)
	if patched.Ban.ExpiresAt != 0 || patched.Ban.Occurrences != 2 || patched.Ban.Note != "escalated to permanent" {
		t.Fatalf("unexpected patched ban: %+v", patched.Ban)
	}

	// 3b. Explicit occurrences reset (0) and manual adjustment (7) via PATCH without forcing reban
	resetResp, resetRaw := doJSONReq(t, http.MethodPatch, env.ts.URL+"/api/admin/security/ip-bans/"+created.Ban.ID, map[string]any{
		"note":        "reset risk count",
		"occurrences": 0,
	}, adminCookies, adminHdrs)
	if resetResp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH ip-ban occurrences=0: %d %s", resetResp.StatusCode, string(resetRaw))
	}
	var resetBan struct {
		Ban domain.SecurityIPBan `json:"ban"`
	}
	_ = json.Unmarshal(resetRaw, &resetBan)
	if resetBan.Ban.Occurrences != 0 || resetBan.Ban.Note != "reset risk count" {
		t.Fatalf("expected occurrences=0 after reset, got %+v", resetBan.Ban)
	}

	adjResp, adjRaw := doJSONReq(t, http.MethodPatch, env.ts.URL+"/api/admin/security/ip-bans/"+created.Ban.ID, map[string]any{
		"occurrences": 7,
	}, adminCookies, adminHdrs)
	if adjResp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH ip-ban occurrences=7: %d %s", adjResp.StatusCode, string(adjRaw))
	}
	_ = json.Unmarshal(adjRaw, &resetBan)
	if resetBan.Ban.Occurrences != 7 {
		t.Fatalf("expected occurrences=7 after manual adjustment, got %+v", resetBan.Ban)
	}

	// 4. Batch set_duration -> batch unban -> purge-expired
	bDurResp, bDurRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/security/ip-bans/batch", map[string]any{
		"action":          "set_duration",
		"ids":             []string{created.Ban.ID},
		"expires_minutes": 60,
	}, adminCookies, adminHdrs)
	if bDurResp.StatusCode != http.StatusOK {
		t.Fatalf("batch set_duration = %d %s", bDurResp.StatusCode, string(bDurRaw))
	}
	bUnbanResp, bUnbanRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/security/ip-bans/batch", map[string]any{
		"action": "unban",
		"ids":    []string{created.Ban.ID},
	}, adminCookies, adminHdrs)
	if bUnbanResp.StatusCode != http.StatusOK {
		t.Fatalf("batch unban = %d %s", bUnbanResp.StatusCode, string(bUnbanRaw))
	}
	purgeResp, purgeRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/security/ip-bans/purge-expired", map[string]any{}, adminCookies, adminHdrs)
	if purgeResp.StatusCode != http.StatusOK {
		t.Fatalf("purge-expired = %d %s", purgeResp.StatusCode, string(purgeRaw))
	}
	afterPurgeResp, afterPurgeRaw := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/admin/security/ip-bans?state=all", nil, adminCookies, nil)
	if afterPurgeResp.StatusCode != http.StatusOK {
		t.Fatal(string(afterPurgeRaw))
	}
	_ = json.Unmarshal(afterPurgeRaw, &listBody)
	if listBody.Counts.All != 0 {
		t.Fatalf("expected 0 bans after purge-expired, got %+v", listBody.Counts)
	}

	// 5. Comment single ban-ip (15m) & batch ban_ip (0 = permanent!)
	artResp, artRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/articles", map[string]any{
		"title":         "Ban Duration Post",
		"slug":          "ban-duration-post",
		"summary_md":    "s",
		"content_md":    "body",
		"status":        "published",
		"allow_comment": true,
	}, adminCookies, adminHdrs)
	if artResp.StatusCode != http.StatusCreated {
		t.Fatalf("create article = %d %s", artResp.StatusCode, string(artRaw))
	}
	var artCreated struct {
		Article domain.RawArticle `json:"article"`
	}
	_ = json.Unmarshal(artRaw, &artCreated)
	if _, err := env.store.ExecContext(env.ctx, `INSERT INTO comments (
		id, target_type, target_id, parent_id, reply_to_name, user_id, guest_id_hash, env_fp_hash,
		author_name, author_url, avatar_url, notify_email_ciphertext, notify_locale,
		client_ip, ip_masked, geo_region, content_md, status, is_deleted, created_at, updated_at
	) VALUES
		('cb_15m','article',?,'','','','g1','f1','Spammer1','','','','en','198.51.100.88','198.51.100.*','','spam1','pending',0,1700000000000,1700000000000),
		('cb_perm','article',?,'','','','g2','f2','Spammer2','','','','en','198.51.100.89','198.51.100.*','','spam2','pending',0,1700000001000,1700000001000)`,
		artCreated.Article.ID, artCreated.Article.ID); err != nil {
		t.Fatal(err)
	}

	cBan1, cBan1Raw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/comments/cb_15m/ban-ip", map[string]any{
		"expires_minutes": 15,
		"note":            "15m spam",
	}, adminCookies, adminHdrs)
	if cBan1.StatusCode != http.StatusCreated && cBan1.StatusCode != http.StatusOK {
		t.Fatalf("comment ban-ip 15m = %d %s", cBan1.StatusCode, string(cBan1Raw))
	}
	cBan2, cBan2Raw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/comments/batch", map[string]any{
		"ids":             []string{"cb_perm"},
		"action":          "ban_ip",
		"expires_minutes": 0,
		"note":            "permanent batch spam",
	}, adminCookies, adminHdrs)
	if cBan2.StatusCode != http.StatusOK {
		t.Fatalf("comment batch ban_ip perm = %d %s", cBan2.StatusCode, string(cBan2Raw))
	}
	var permExpires int64 = -1
	if err := env.store.QueryRowContext(env.ctx, `SELECT expires_at FROM security_ip_bans WHERE cidr = '198.51.100.89/32'`).Scan(&permExpires); err != nil || permExpires != 0 {
		t.Fatalf("expected batch ban_ip expires_minutes=0 to store permanent expires_at=0, got expires=%d err=%v", permExpires, err)
	}

	// 6. DELETE /api/admin/audit-logs clears audit logs cleanly
	clearAudResp, clearAudRaw := doJSONReq(t, http.MethodDelete, env.ts.URL+"/api/admin/audit-logs", nil, adminCookies, adminHdrs)
	if clearAudResp.StatusCode != http.StatusOK {
		t.Fatalf("DELETE audit-logs = %d %s", clearAudResp.StatusCode, string(clearAudRaw))
	}
	audListResp, audListRaw := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/admin/audit-logs", nil, adminCookies, nil)
	if audListResp.StatusCode != http.StatusOK {
		t.Fatal(string(audListRaw))
	}
	var audBody struct {
		Total int `json:"total"`
	}
	_ = json.Unmarshal(audListRaw, &audBody)
	if audBody.Total != 0 {
		t.Fatalf("expected 0 audit logs after DELETE /api/admin/audit-logs, got %d", audBody.Total)
	}
}

func TestPublicCommentConfigurableRateLimit(t *testing.T) {
	env, adminCookies, adminHdrs, _ := setupRound2Env(t)
	hdrs := map[string]string{"Origin": env.ts.URL}
	for k, v := range adminHdrs {
		hdrs[k] = v
	}

	artResp, artRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/articles", map[string]any{
		"title":         "Rate Limit Test Post",
		"slug":          "rate-limit-test-post",
		"summary_md":    "summary",
		"content_md":    "Testing dynamic per-IP comment rate limit.",
		"status":        "published",
		"allow_comment": true,
	}, adminCookies, hdrs)
	if artResp.StatusCode != http.StatusCreated {
		t.Fatalf("create article = %d %s", artResp.StatusCode, string(artRaw))
	}
	var artBody struct {
		Article domain.RawArticle `json:"article"`
	}
	_ = json.Unmarshal(artRaw, &artBody)
	artID := artBody.Article.ID

	// Configure per-IP rate limit to max 1 comment per 60s window
	patchResp, patchRaw := doJSONReq(t, http.MethodPatch, env.ts.URL+"/api/admin/settings/reading", map[string]any{
		"comment_access_mode":         "all",
		"comment_moderation_mode":     "off",
		"pow_difficulty_bits":         8,
		"comment_rate_window_seconds": 60,
		"comment_rate_max_count":      1,
	}, adminCookies, hdrs)
	if patchResp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH settings/reading = %d %s", patchResp.StatusCode, string(patchRaw))
	}

	guestHdrs := map[string]string{"Origin": env.ts.URL}
	postGuestComment := func(author, body string) (*http.Response, []byte) {
		t.Helper()
		chalResp, chalRaw := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/v1/pow/challenge", nil, nil, nil)
		if chalResp.StatusCode != http.StatusOK {
			t.Fatalf("GET pow/challenge = %d %s", chalResp.StatusCode, string(chalRaw))
		}
		var chal guest.PoWChallenge
		if err := json.Unmarshal(chalRaw, &chal); err != nil {
			t.Fatal(err)
		}
		nonce := guest.SolveForTest(chal.Challenge, chal.DifficultyBits)
		return doJSONReq(t, http.MethodPost, env.ts.URL+"/api/v1/comments", map[string]any{
			"target_type":   "article",
			"target_id":     artID,
			"author_name":   author,
			"content_md":    body,
			"pow_challenge": chal.Challenge,
			"pow_solution":  nonce,
			"env_fp":        "1920x1080|2|Asia/Shanghai|zh-CN|8",
		}, nil, guestHdrs)
	}

	c1Resp, c1Raw := postGuestComment("GuestOne", "First comment allowed within limit.")
	if c1Resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 1st comment 201 Created, got %d %s", c1Resp.StatusCode, string(c1Raw))
	}

	c2Resp, c2Raw := postGuestComment("GuestTwo", "Second immediate comment should be rate-limited.")
	if c2Resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected 2nd comment 429 TooManyRequests, got %d %s", c2Resp.StatusCode, string(c2Raw))
	}

	// Switching max_count to 0 removes per-IP rate limiting immediately
	patchUnlimResp, patchUnlimRaw := doJSONReq(t, http.MethodPatch, env.ts.URL+"/api/admin/settings/reading", map[string]any{
		"comment_rate_max_count": 0,
	}, adminCookies, hdrs)
	if patchUnlimResp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH settings/reading unlimited = %d %s", patchUnlimResp.StatusCode, string(patchUnlimRaw))
	}

	c3Resp, c3Raw := postGuestComment("GuestThree", "Third comment succeeds once unlimited (max=0).")
	if c3Resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 3rd comment 201 Created after max_count=0, got %d %s", c3Resp.StatusCode, string(c3Raw))
	}

	c4Resp, c4Raw := postGuestComment("GuestFour", "Fourth comment also succeeds when unlimited.")
	if c4Resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 4th comment 201 Created after max_count=0, got %d %s", c4Resp.StatusCode, string(c4Raw))
	}
}
