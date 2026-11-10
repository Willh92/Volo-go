package server_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/Willh92/Volo-go/internal/domain"
)

func TestAdminCommentTrash_DeleteRestorePurgeBatchEmptyAndIdentity(t *testing.T) {
	env, adminCookies, adminHdrs, _ := setupRound2Env(t)

	artResp, artRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/articles", map[string]any{
		"title":         "Trash Test Post",
		"slug":          "trash-test-post",
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
	artID := artCreated.Article.ID

	if _, err := env.store.ExecContext(env.ctx, `INSERT INTO comments (
		id, target_type, target_id, parent_id, reply_to_name, user_id, guest_id_hash, env_fp_hash,
		author_name, author_url, avatar_url, notify_email_ciphertext, notify_locale,
		client_ip, ip_masked, geo_region, content_md, status, is_deleted, created_at, updated_at
	) VALUES
		('tc_root','article',?,'','','','gh1','fp1','Guest One','','','cipher1','en','198.51.100.1','198.51.100.*','Mars','Root text','approved',0,1700000000000,1700000000000),
		('tc_reply','article',?,'tc_root','Guest One','','gh2','fp2','Guest Two','','','','en','198.51.100.2','198.51.100.*','Mars','Reply text','approved',0,1700000001000,1700000001000),
		('tc_solo','article',?,'','','','gh3','fp3','Guest Three','','','cipher3','en','198.51.100.3','198.51.100.*','Mars','Solo text','approved',0,1700000002000,1700000002000)`,
		artID, artID, artID); err != nil {
		t.Fatalf("seed comments: %v", err)
	}

	commentRow := func(id string) (domain.Comment, bool) {
		var c domain.Comment
		err := env.store.QueryRowContext(env.ctx,
			`SELECT is_deleted, content_md, client_ip, ip_masked, geo_region, env_fp_hash, notify_email_ciphertext, author_name, author_url, avatar_url FROM comments WHERE id = ?`, id).
			Scan(&c.IsDeleted, &c.ContentMD, &c.ClientIP, &c.IPMasked, &c.GeoRegion, &c.EnvFPHash, &c.NotifyEmailCiphertext, &c.AuthorName, &c.AuthorURL, &c.AvatarURL)
		return c, err == nil
	}

	// 1. Admin DELETE soft-deletes into the trash and keeps the full row (scrub happens on purge).
	delResp, delRaw := doJSONReq(t, http.MethodDelete, env.ts.URL+"/api/admin/comments/tc_solo", nil, adminCookies, adminHdrs)
	if delResp.StatusCode != http.StatusOK {
		t.Fatalf("DELETE comment = %d %s", delResp.StatusCode, string(delRaw))
	}
	solo, ok := commentRow("tc_solo")
	if !ok || !solo.IsDeleted || solo.ContentMD != "Solo text" {
		t.Fatalf("expected soft-deleted row keeping content, got ok=%v %+v", ok, solo)
	}
	if solo.ClientIP == "" && solo.IPMasked == "" && solo.GeoRegion == "" && solo.EnvFPHash == "" && solo.NotifyEmailCiphertext == "" {
		t.Fatalf("trash must keep IP/email until permanent deletion, got %+v", solo)
	}

	// 2. Batch delete root (has a live reply) -> kept intact in the trash.
	bResp, bRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/comments/batch", map[string]any{
		"action": "delete", "ids": []string{"tc_root"},
	}, adminCookies, adminHdrs)
	if bResp.StatusCode != http.StatusOK || !strings.Contains(string(bRaw), `"affected":1`) {
		t.Fatalf("batch delete = %d %s", bResp.StatusCode, string(bRaw))
	}
	if root, _ := commentRow("tc_root"); !root.IsDeleted || root.ContentMD != "Root text" {
		t.Fatalf("batch delete must keep the row intact, got %+v", root)
	}

	// 3. Trash listing + counts.
	listResp, listRaw := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/admin/comments?status=trash", nil, adminCookies, nil)
	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("GET trash = %d %s", listResp.StatusCode, string(listRaw))
	}
	var list struct {
		Items        []domain.Comment `json:"items"`
		StatusCounts map[string]int   `json:"status_counts"`
		Retention    int              `json:"trash_retention_days"`
	}
	_ = json.Unmarshal(listRaw, &list)
	if len(list.Items) != 2 || list.StatusCounts["trash"] != 2 || list.StatusCounts["all"] != 1 {
		t.Fatalf("expected 2 trashed + 1 live, got items=%d counts=%v", len(list.Items), list.StatusCounts)
	}
	if list.Retention != domain.DefaultCommentTrashRetentionDays {
		t.Fatalf("expected trash_retention_days=%d, got %d", domain.DefaultCommentTrashRetentionDays, list.Retention)
	}

	// 4. Public thread renders a placeholder for the deleted root with the surviving reply.
	pubResp, pubRaw := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/v1/comments?target_type=article&target_id="+artID, nil, nil, nil)
	if pubResp.StatusCode != http.StatusOK {
		t.Fatalf("public comments = %d %s", pubResp.StatusCode, string(pubRaw))
	}
	{
		body := string(pubRaw)
		if strings.Contains(body, "Root text") || strings.Contains(body, "Solo text") || !strings.Contains(body, "vc-deleted-placeholder") || !strings.Contains(body, "Reply text") {
			t.Fatalf("unexpected public thread body: %s", body)
		}
	}

	// 5. Restore (single) brings content back; restoring again is a conflict.
	rResp, rRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/comments/tc_solo/restore", nil, adminCookies, adminHdrs)
	if rResp.StatusCode != http.StatusOK {
		t.Fatalf("restore = %d %s", rResp.StatusCode, string(rRaw))
	}
	if s, _ := commentRow("tc_solo"); s.IsDeleted || s.ContentMD != "Solo text" {
		t.Fatalf("restore failed: %+v", s)
	}
	if again, _ := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/comments/tc_solo/restore", nil, adminCookies, adminHdrs); again.StatusCode != http.StatusConflict {
		t.Fatalf("restoring a live comment should be 409, got %d", again.StatusCode)
	}
	if notTrash, _ := doJSONReq(t, http.MethodDelete, env.ts.URL+"/api/admin/comments/tc_solo?purge=1", nil, adminCookies, adminHdrs); notTrash.StatusCode != http.StatusConflict {
		t.Fatalf("purging a live comment should be 409, got %d", notTrash.StatusCode)
	}

	// 6. Permanent delete of root keeps a minimal placeholder because tc_reply is live.
	pResp, pRaw := doJSONReq(t, http.MethodDelete, env.ts.URL+"/api/admin/comments/tc_root?purge=1", nil, adminCookies, adminHdrs)
	if pResp.StatusCode != http.StatusOK || !strings.Contains(string(pRaw), `"kept_placeholder":true`) {
		t.Fatalf("purge root = %d %s", pResp.StatusCode, string(pRaw))
	}
	if root, ok := commentRow("tc_root"); !ok || root.ContentMD != "" || root.AuthorName != "" {
		t.Fatalf("expected minimal placeholder row, got ok=%v %+v", ok, root)
	}

	// 7. Identity edit: nickname / avatar / website validated and reply_to_name synced.
	badResp, _ := doJSONReq(t, http.MethodPatch, env.ts.URL+"/api/admin/comments/tc_solo", map[string]any{
		"author_url": "javascript:alert(1)",
	}, adminCookies, adminHdrs)
	if badResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid author_url should be 400, got %d", badResp.StatusCode)
	}
	if _, err := env.store.ExecContext(env.ctx, `UPDATE comments SET parent_id = 'tc_solo', reply_to_name = 'Guest Three' WHERE id = 'tc_reply'`); err != nil {
		t.Fatal(err)
	}
	idResp, idRaw := doJSONReq(t, http.MethodPatch, env.ts.URL+"/api/admin/comments/tc_solo", map[string]any{
		"author_name": "Renamed Guest",
		"avatar_url":  "https://img.example.com/a.png",
		"author_url":  "https://guest.example.com",
	}, adminCookies, adminHdrs)
	if idResp.StatusCode != http.StatusOK {
		t.Fatalf("identity PATCH = %d %s", idResp.StatusCode, string(idRaw))
	}
	if s, _ := commentRow("tc_solo"); s.AuthorName != "Renamed Guest" || s.AvatarURL != "https://img.example.com/a.png" || !strings.HasPrefix(s.AuthorURL, "https://guest.example.com") {
		t.Fatalf("identity not applied: %+v", s)
	}
	var replyTo string
	_ = env.store.QueryRowContext(env.ctx, `SELECT reply_to_name FROM comments WHERE id = 'tc_reply'`).Scan(&replyTo)
	if replyTo != "Renamed Guest" {
		t.Fatalf("expected child reply_to_name synced, got %q", replyTo)
	}

	// 8. Batch restore / purge.
	_, _ = doJSONReq(t, http.MethodDelete, env.ts.URL+"/api/admin/comments/tc_solo", nil, adminCookies, adminHdrs)
	brResp, brRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/comments/batch", map[string]any{
		"action": "restore", "ids": []string{"tc_solo"},
	}, adminCookies, adminHdrs)
	if brResp.StatusCode != http.StatusOK || !strings.Contains(string(brRaw), `"affected":1`) {
		t.Fatalf("batch restore = %d %s", brResp.StatusCode, string(brRaw))
	}
	_, _ = doJSONReq(t, http.MethodDelete, env.ts.URL+"/api/admin/comments/tc_solo", nil, adminCookies, adminHdrs)
	_, _ = doJSONReq(t, http.MethodDelete, env.ts.URL+"/api/admin/comments/tc_reply", nil, adminCookies, adminHdrs)
	bpResp, bpRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/comments/batch", map[string]any{
		"action": "purge", "ids": []string{"tc_reply"},
	}, adminCookies, adminHdrs)
	if bpResp.StatusCode != http.StatusOK || !strings.Contains(string(bpRaw), `"affected":1`) {
		t.Fatalf("batch purge = %d %s", bpResp.StatusCode, string(bpRaw))
	}
	if _, ok := commentRow("tc_reply"); ok {
		t.Fatal("batch purge should physically remove tc_reply")
	}

	// 9. Empty trash removes the rest (tc_solo + now-orphaned tc_root placeholder).
	eResp, eRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/comments/trash/empty", nil, adminCookies, adminHdrs)
	if eResp.StatusCode != http.StatusOK || !strings.Contains(string(eRaw), `"purged":2`) {
		t.Fatalf("empty trash = %d %s", eResp.StatusCode, string(eRaw))
	}
	var remaining int
	_ = env.store.QueryRowContext(env.ctx, `SELECT COUNT(*) FROM comments WHERE target_id = ?`, artID).Scan(&remaining)
	if remaining != 0 {
		t.Fatalf("expected all comments purged, %d remain", remaining)
	}

	// 10. Audit trail.
	for _, action := range []string{"comment.delete", "comment.restore", "comment.purge", "comment.identity_update", "comment.trash_empty", "comment.batch"} {
		var n int
		_ = env.store.QueryRowContext(env.ctx, `SELECT COUNT(*) FROM audit_logs WHERE action = ?`, action).Scan(&n)
		if n == 0 {
			t.Fatalf("expected audit log entry for %s", action)
		}
	}
}
