package content

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Willh92/Volo-go/internal/config"
	"github.com/Willh92/Volo-go/internal/db/migrate"
	"github.com/Willh92/Volo-go/internal/db/sqlx"
	"github.com/Willh92/Volo-go/internal/domain"
	"github.com/Willh92/Volo-go/internal/guest"
	"github.com/Willh92/Volo-go/internal/i18n"
	"github.com/Willh92/Volo-go/internal/render"
	"github.com/Willh92/Volo-go/internal/repo"
	"github.com/Willh92/Volo-go/internal/visibility"
)

type trashFixture struct {
	ctx      context.Context
	store    *sqlx.Store
	svc      *Service
	settings *repo.SettingsManager
	now      int64
}

func newTrashFixture(t *testing.T) *trashFixture {
	t.Helper()
	ctx := context.Background()
	tmp := t.TempDir()
	store, err := sqlx.Open(ctx, sqlx.Options{Driver: "sqlite", DSN: filepath.Join(tmp, "trash.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := migrate.Up(ctx, store); err != nil {
		t.Fatal(err)
	}
	kr, _, err := config.LoadOrInitKeyRing("unit-test-comment-trash-secret-2026", tmp)
	if err != nil {
		t.Fatal(err)
	}
	cat, err := i18n.NewCatalog("en-US", "")
	if err != nil {
		t.Fatal(err)
	}
	settingsMgr := repo.NewSettingsManager(store, kr, "en-US")
	if _, err := settingsMgr.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	passports := guest.NewPassportManager(kr, "https://example.com")
	rnd := render.Default()
	svc := NewService(ServiceOptions{
		Store:     store,
		Keys:      kr,
		Settings:  settingsMgr,
		Passports: passports,
		Guard:     visibility.NewGuard(kr, settingsMgr, passports, cat, rnd),
		Search:    visibility.NewSearchIndex(store, rnd, 1<<20),
		Renderer:  rnd,
		Catalog:   cat,
	})
	now := time.Now().UnixMilli()
	if _, err := store.ExecContext(ctx, `INSERT INTO users (
		id, username, email, display_name, avatar_url, bio, website_url,
		role, password_hash, legacy_hash, status, preferred_locale, created_at, updated_at
	) VALUES ('u1','admin1','a1@example.com','Admin One','','','','admin','x','','active','en',?,?)`, now, now); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if _, err := store.ExecContext(ctx, `INSERT INTO categories (id, name, slug, description, sort_order, created_at, updated_at) VALUES ('c1','Sys','sys','',1,?,?)`, now, now); err != nil {
		t.Fatalf("seed category: %v", err)
	}
	if _, err := store.ExecContext(ctx, `INSERT INTO articles (
		id, author_id, category_id, title, slug, legacy_permalink, summary_md, content_md,
		cover_url, status, published_at, allow_comment, password_ciphertext,
		audio_url, audio_mime, lang, translation_group, license_override, word_count, reading_minutes, created_at, updated_at
	) VALUES ('a1','u1','c1','Post','post','','S','B','','published',?,1,'','','','en','','',1,1,?,?)`, now, now, now); err != nil {
		t.Fatalf("seed article: %v", err)
	}
	return &trashFixture{ctx: ctx, store: store, svc: svc, settings: settingsMgr, now: now}
}

func (f *trashFixture) insertComment(t *testing.T, id, parentID, replyTo, author, body string, createdAt int64) {
	t.Helper()
	if _, err := f.store.ExecContext(f.ctx, `INSERT INTO comments (
		id, target_type, target_id, parent_id, reply_to_name, user_id, guest_id_hash, env_fp_hash,
		author_name, author_url, avatar_url, notify_email_ciphertext, notify_locale,
		client_ip, ip_masked, geo_region, content_md, status, is_deleted, created_at, updated_at
	) VALUES (?, 'article', 'a1', ?, ?, '', 'ghash-'||?, 'envfp-'||?, ?, 'https://example.org/'||?, '', 'cipher-'||?, 'en',
		'203.0.113.7', '203.0.113.*', 'Earth', ?, 'approved', ?, ?, ?)`,
		id, parentID, replyTo, id, id, author, id, id, body, false, createdAt, createdAt); err != nil {
		t.Fatalf("insert comment %s: %v", id, err)
	}
}

func (f *trashFixture) get(t *testing.T, id string) (domain.Comment, bool) {
	t.Helper()
	var c domain.Comment
	if err := sqlx.GetInto(f.ctx, f.store, &c, `SELECT * FROM comments WHERE id = ?`, id); err != nil {
		if isNoRows(err) {
			return c, false
		}
		t.Fatalf("get %s: %v", id, err)
	}
	return c, true
}

func (f *trashFixture) softDelete(t *testing.T, id string, at int64) {
	t.Helper()
	if err := f.store.WithTx(f.ctx, func(tx *sqlx.Tx) error {
		if err := SoftDeleteCommentTx(f.ctx, tx, id, at); err != nil {
			return err
		}
		return syncApprovedCommentCountTx(f.ctx, tx, "article", "a1")
	}); err != nil {
		t.Fatalf("soft delete %s: %v", id, err)
	}
}

func (f *trashFixture) articleCommentCount(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.store.QueryRowContext(f.ctx, `SELECT comment_count FROM articles WHERE id = 'a1'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestCommentTrashSoftDeleteKeepsFullRowUntilPurge(t *testing.T) {
	f := newTrashFixture(t)
	f.insertComment(t, "c1", "", "", "Alice", "hello world", f.now-1000)
	before, _ := f.get(t, "c1")

	admin := &domain.User{ID: "u1", Role: domain.RoleAdmin, Status: domain.UserStatusActive}
	req := httptest.NewRequest("DELETE", "/api/v1/comments/c1", nil)
	if err := f.svc.DeleteOwnComment(f.ctx, req, admin, "c1"); err != nil {
		t.Fatalf("DeleteOwnComment: %v", err)
	}
	c, ok := f.get(t, "c1")
	if !ok {
		t.Fatal("soft-deleted comment must stay in the table (trash)")
	}
	if !c.IsDeleted {
		t.Fatal("expected is_deleted = true")
	}
	if c.ContentMD != "hello world" || c.ClientIP != before.ClientIP || c.IPMasked != before.IPMasked ||
		c.GeoRegion != before.GeoRegion || c.EnvFPHash != before.EnvFPHash || c.NotifyEmailCiphertext != before.NotifyEmailCiphertext {
		t.Fatalf("trash must keep the full row for lossless restore, got %+v", c)
	}

	// Restore is lossless.
	restored, err := f.svc.RestoreComment(f.ctx, "c1")
	if err != nil {
		t.Fatalf("RestoreComment: %v", err)
	}
	if restored.ClientIP != before.ClientIP || restored.NotifyEmailCiphertext != before.NotifyEmailCiphertext {
		t.Fatalf("restore must bring back IP/email, got %+v", restored)
	}

	// Permanent deletion without live replies removes the row entirely.
	f.softDelete(t, "c1", f.now)
	if _, err := f.svc.PurgeComment(f.ctx, "c1"); err != nil {
		t.Fatalf("PurgeComment: %v", err)
	}
	if _, ok := f.get(t, "c1"); ok {
		t.Fatal("purged comment without replies must be physically deleted")
	}
}

func TestCommentTrashPlaceholderRestoreAndPurge(t *testing.T) {
	f := newTrashFixture(t)
	f.insertComment(t, "root", "", "", "Alice", "root body", f.now-3000)
	f.insertComment(t, "reply", "root", "Alice", "Bob", "reply body", f.now-2000)
	f.insertComment(t, "lonely", "", "", "Carol", "lonely body", f.now-1000)

	f.softDelete(t, "root", f.now)
	f.softDelete(t, "lonely", f.now)
	if n := f.articleCommentCount(t); n != 1 {
		t.Fatalf("expected comment_count=1 after deletes, got %d", n)
	}

	req := httptest.NewRequest("GET", "/api/v1/comments?target_type=article&target_id=a1", nil)
	views, err := f.svc.ListTargetComments(f.ctx, req, nil, "article", "a1")
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 {
		t.Fatalf("expected only the placeholder root (lonely deleted comment hidden), got %d views: %+v", len(views), views)
	}
	ph := views[0]
	if ph.ID != "root" || !ph.IsDeleted || ph.AuthorName != "" || ph.CanEdit {
		t.Fatalf("expected zero-PII placeholder for root, got %+v", ph)
	}
	if !strings.Contains(ph.ContentHTML, "vc-deleted-placeholder") || strings.Contains(ph.ContentHTML, "root body") {
		t.Fatalf("placeholder must render tombstone markup without content, got %q", ph.ContentHTML)
	}
	if len(ph.Replies) != 1 || ph.Replies[0].ID != "reply" {
		t.Fatalf("expected surviving reply under placeholder, got %+v", ph.Replies)
	}
	if ph.Replies[0].ReplyToName != "" {
		t.Fatalf("surviving reply must not echo deleted author's nickname, got %q", ph.Replies[0].ReplyToName)
	}

	// Restore brings the comment back with its original content and recounts.
	restored, err := f.svc.RestoreComment(f.ctx, "lonely")
	if err != nil {
		t.Fatalf("RestoreComment: %v", err)
	}
	if restored.IsDeleted || restored.ContentMD != "lonely body" {
		t.Fatalf("unexpected restored comment %+v", restored)
	}
	if n := f.articleCommentCount(t); n != 2 {
		t.Fatalf("expected comment_count=2 after restore, got %d", n)
	}
	if _, err := f.svc.RestoreComment(f.ctx, "lonely"); err != ErrCommentNotInTrash {
		t.Fatalf("restoring a live comment must fail with ErrCommentNotInTrash, got %v", err)
	}
	if _, err := f.svc.PurgeComment(f.ctx, "lonely"); err != ErrCommentNotInTrash {
		t.Fatalf("purging a live comment must fail with ErrCommentNotInTrash, got %v", err)
	}

	// Purging a trashed comment that still has a live reply keeps a minimal placeholder row.
	if _, err := f.store.ExecContext(f.ctx, `INSERT INTO reactions (target_type, target_id, actor_hash, kind, created_at) VALUES ('comment','root','user:x','like',?)`, f.now); err != nil {
		t.Fatalf("seed reaction: %v", err)
	}
	kept, err := f.svc.PurgeComment(f.ctx, "root")
	if err != nil {
		t.Fatalf("PurgeComment(root): %v", err)
	}
	if !kept {
		t.Fatal("expected placeholder to be kept while a live reply exists")
	}
	rootRow, ok := f.get(t, "root")
	if !ok || !rootRow.IsDeleted || rootRow.ContentMD != "" || rootRow.AuthorName != "" || rootRow.AuthorURL != "" || rootRow.GuestIDHash != "" {
		t.Fatalf("expected minimal placeholder row, got ok=%v %+v", ok, rootRow)
	}
	if rootRow.ClientIP != "" || rootRow.IPMasked != "" || rootRow.GeoRegion != "" || rootRow.EnvFPHash != "" || rootRow.NotifyEmailCiphertext != "" {
		t.Fatalf("purge placeholder must be scrubbed of IP/region/fingerprint/email, got %+v", rootRow)
	}
	var reactions int
	_ = f.store.QueryRowContext(f.ctx, `SELECT COUNT(*) FROM reactions WHERE target_type = 'comment' AND target_id = 'root'`).Scan(&reactions)
	if reactions != 0 {
		t.Fatalf("expected reactions of purged comment removed, got %d", reactions)
	}
	if _, err := f.svc.RestoreComment(f.ctx, "root"); err != ErrCommentNotInTrash {
		t.Fatalf("content-free purge placeholder must not be restorable, got %v", err)
	}
	views, err = f.svc.ListTargetComments(f.ctx, req, nil, "article", "a1")
	if err != nil {
		t.Fatal(err)
	}
	foundPlaceholder := false
	for _, v := range views {
		if v.ID == "root" {
			foundPlaceholder = v.IsDeleted && len(v.Replies) == 1
		}
	}
	if !foundPlaceholder {
		t.Fatalf("expected purged placeholder to keep the thread alive, got %+v", views)
	}

	// Once the reply is gone too, emptying the trash removes both rows physically.
	f.softDelete(t, "reply", f.now)
	purged, keptN, err := f.svc.EmptyCommentTrash(f.ctx)
	if err != nil {
		t.Fatalf("EmptyCommentTrash: %v", err)
	}
	if purged != 2 || keptN != 0 {
		t.Fatalf("expected purged=2 kept=0, got purged=%d kept=%d", purged, keptN)
	}
	if _, ok := f.get(t, "root"); ok {
		t.Fatal("root placeholder should be gone after its last reply was purged")
	}
	if _, ok := f.get(t, "reply"); ok {
		t.Fatal("reply should be purged")
	}
}

func TestCommentTrashRestorePromotesOrphanToRoot(t *testing.T) {
	f := newTrashFixture(t)
	f.insertComment(t, "p", "", "", "Alice", "parent", f.now-2000)
	f.insertComment(t, "ch", "p", "Alice", "Bob", "child", f.now-1000)
	f.softDelete(t, "ch", f.now)
	f.softDelete(t, "p", f.now)
	if _, err := f.svc.PurgeComment(f.ctx, "p"); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.get(t, "p"); ok {
		t.Fatal("parent without live replies should be physically purged")
	}
	restored, err := f.svc.RestoreComment(f.ctx, "ch")
	if err != nil {
		t.Fatal(err)
	}
	if restored.ParentID != "" || restored.ReplyToName != "" {
		t.Fatalf("orphaned restore must be promoted to root, got parent=%q reply_to=%q", restored.ParentID, restored.ReplyToName)
	}
}

func TestCommentTrashRetentionJanitor(t *testing.T) {
	f := newTrashFixture(t)
	day := int64(24 * time.Hour / time.Millisecond)
	f.insertComment(t, "old", "", "", "Old", "old body", f.now-90*day)
	f.insertComment(t, "fresh", "", "", "Fresh", "fresh body", f.now-90*day)
	f.softDelete(t, "old", f.now-40*day)
	f.softDelete(t, "fresh", f.now-5*day)

	if got := f.settings.Snapshot().Reading.CommentTrashRetentionDays; got != domain.DefaultCommentTrashRetentionDays {
		t.Fatalf("expected default retention %d, got %d", domain.DefaultCommentTrashRetentionDays, got)
	}

	// 0 = manual only: nothing is purged.
	if _, _, err := f.settings.PatchDomain(f.ctx, "reading", []byte(`{"comment_trash_retention_days":0}`), 0); err != nil {
		t.Fatal(err)
	}
	if p, _, err := f.svc.PurgeExpiredCommentTrash(f.ctx, time.UnixMilli(f.now)); err != nil || p != 0 {
		t.Fatalf("retention=0 must not purge, got purged=%d err=%v", p, err)
	}

	// Out-of-range values are clamped.
	if _, _, err := f.settings.PatchDomain(f.ctx, "reading", []byte(`{"comment_trash_retention_days":999999}`), 0); err != nil {
		t.Fatal(err)
	}
	if got := f.settings.Snapshot().Reading.CommentTrashRetentionDays; got != domain.MaxCommentTrashRetentionDays {
		t.Fatalf("expected clamp to %d, got %d", domain.MaxCommentTrashRetentionDays, got)
	}

	if _, _, err := f.settings.PatchDomain(f.ctx, "reading", []byte(`{"comment_trash_retention_days":30}`), 0); err != nil {
		t.Fatal(err)
	}
	purged, kept, err := f.svc.PurgeExpiredCommentTrash(f.ctx, time.UnixMilli(f.now))
	if err != nil {
		t.Fatal(err)
	}
	if purged != 1 || kept != 0 {
		t.Fatalf("expected exactly the 40-day-old trash purged, got purged=%d kept=%d", purged, kept)
	}
	if _, ok := f.get(t, "old"); ok {
		t.Fatal("expired trash should be purged")
	}
	if c, ok := f.get(t, "fresh"); !ok || !c.IsDeleted {
		t.Fatal("trash younger than retention must survive")
	}
}

func TestCommentIdentityPatchNormalizesAndSyncsReplies(t *testing.T) {
	if _, err := NormalizeCommentIdentityPatch(CommentIdentityPatch{AuthorName: ptrStr("   ")}); err == nil {
		t.Fatal("blank nickname must be rejected")
	}
	if _, err := NormalizeCommentIdentityPatch(CommentIdentityPatch{AuthorURL: ptrStr("javascript:alert(1)")}); err == nil {
		t.Fatal("javascript: website must be rejected")
	}
	if _, err := NormalizeCommentIdentityPatch(CommentIdentityPatch{AvatarURL: ptrStr("data:image/png;base64,AAAA")}); err == nil {
		t.Fatal("data: avatar must be rejected")
	}
	norm, err := NormalizeCommentIdentityPatch(CommentIdentityPatch{
		AuthorName: ptrStr("  New\u202eName  "),
		AvatarURL:  ptrStr(""),
		AuthorURL:  ptrStr("https://new.example.com/"),
	})
	if err != nil {
		t.Fatalf("valid identity rejected: %v", err)
	}
	if *norm.AuthorName != "NewName" {
		t.Fatalf("expected bidi override stripped and trimmed, got %q", *norm.AuthorName)
	}

	f := newTrashFixture(t)
	f.insertComment(t, "r", "", "", "Alice", "root", f.now-3000)
	f.insertComment(t, "r1", "r", "Alice", "Bob", "to alice", f.now-2000)
	f.insertComment(t, "r2", "r", "Someone", "Dan", "to someone", f.now-1000)
	orig, _ := f.get(t, "r")
	var changed bool
	if err := f.store.WithTx(f.ctx, func(tx *sqlx.Tx) error {
		var aErr error
		changed, aErr = ApplyCommentIdentityTx(f.ctx, tx, orig, norm, f.now)
		return aErr
	}); err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("expected identity change to be applied")
	}
	updated, _ := f.get(t, "r")
	if updated.AuthorName != "NewName" || updated.AuthorURL != "https://new.example.com/" || updated.AvatarURL != "" {
		t.Fatalf("identity not persisted: %+v", updated)
	}
	r1, _ := f.get(t, "r1")
	if r1.ReplyToName != "NewName" {
		t.Fatalf("direct reply reply_to_name must follow the rename, got %q", r1.ReplyToName)
	}
	r2, _ := f.get(t, "r2")
	if r2.ReplyToName != "Someone" {
		t.Fatalf("unrelated reply_to_name must be untouched, got %q", r2.ReplyToName)
	}
}

func ptrStr(s string) *string { return &s }
