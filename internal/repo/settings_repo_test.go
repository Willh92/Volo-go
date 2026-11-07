package repo

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Willh92/Volo-go/internal/config"
	"github.com/Willh92/Volo-go/internal/db/migrate"
	"github.com/Willh92/Volo-go/internal/db/sqlx"
	"github.com/Willh92/Volo-go/internal/domain"
)

func TestSettingsManagerAtomicPatchSecretMaskingAndHotReload(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()
	kr, _, err := config.LoadOrInitKeyRing("unit-test-master-secret-key-2026", tmpDir)
	if err != nil {
		t.Fatal(err)
	}
	store, err := sqlx.Open(ctx, sqlx.Options{
		Driver: "sqlite",
		DSN:    filepath.Join(tmpDir, "settings.db"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := migrate.Up(ctx, store); err != nil {
		t.Fatal(err)
	}

	mgr := NewSettingsManager(store, kr, "zh-CN")

	// 1. Partial PATCH on "site": updates title without overwriting subtitle or license_template!
	_, v1, err := mgr.PatchDomain(ctx, "site", []byte(`{"title":"Will's Notes","social_links":[{"id":"gh","label":"GitHub","icon":"github","url":"https://github.com/Willh92"}]}`), 0)
	if err != nil || v1 != 1 {
		t.Fatalf("PatchDomain(site) failed: v=%d err=%v", v1, err)
	}
	snap := mgr.Snapshot()
	if snap.Site.Title != "Will's Notes" || snap.Site.Subtitle != "记录 · 思考 · 分享" || len(snap.Site.SocialLinks) != 1 {
		t.Fatalf("unexpected site snapshot after partial PATCH: %+v", snap.Site)
	}

	// 2. Partial PATCH on nested "feed.podcast" without wiping feed.item_count
	_, _, err = mgr.PatchDomain(ctx, "feed", []byte(`{"podcast":{"title":"Volo Cast","author":"Will"}}`), 0)
	if err != nil {
		t.Fatalf("PatchDomain(feed) failed: %v", err)
	}
	snap = mgr.Snapshot()
	if snap.Feed.Podcast.Title != "Volo Cast" || snap.Feed.ItemCount != 20 || !snap.Feed.EnableLLMSTxt {
		t.Fatalf("nested feed PATCH clobbered sibling fields: %+v", snap.Feed)
	}

	// 3. Storage secret encryption at rest + *Set masking + preserve on blank update + explicit clear
	val, ver, err := mgr.PatchDomain(ctx, "storage", []byte(`{"active_driver":"s3","s3_bucket":"blog-assets","s3_secret_key":"MY_RAW_S3_SECRET"}`), 0)
	if err != nil || ver != 1 {
		t.Fatalf("PatchDomain(storage) failed: ver=%d err=%v", ver, err)
	}
	stView := val.(domain.StorageSettings)
	if !stView.S3SecretKeySet || stView.S3Bucket != "blog-assets" {
		t.Fatalf("expected S3SecretKeySet=true, got %+v", stView)
	}

	// Verify DB never stores plaintext MY_RAW_S3_SECRET!
	rawRec, err := sqlx.Get[domain.SettingRecord](ctx, store, `SELECT * FROM settings WHERE domain = 'storage'`)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(rawRec.ValueJSON, "MY_RAW_S3_SECRET") || strings.Contains(rawRec.SecretCiphertext, "MY_RAW_S3_SECRET") {
		t.Fatalf("CRITICAL: plaintext S3 secret leaked in DB record: %+v", rawRec)
	}
	if !strings.HasPrefix(rawRec.SecretCiphertext, "enc:v1:") {
		t.Fatalf("expected AES-GCM enc:v1: ciphertext, got %q", rawRec.SecretCiphertext)
	}

	// Blank s3_secret_key in subsequent PATCH must preserve stored secret
	_, ver2, err := mgr.PatchDomain(ctx, "storage", []byte(`{"max_upload_mb":80,"s3_secret_key":""}`), ver)
	if err != nil || ver2 != 2 {
		t.Fatalf("second storage PATCH failed: ver=%d err=%v", ver2, err)
	}
	sec, err := mgr.GetStorageSecrets(ctx)
	if err != nil || sec.S3SecretKey != "MY_RAW_S3_SECRET" || mgr.Snapshot().Storage.MaxUploadMB != 80 {
		t.Fatalf("secret was not preserved on blank PATCH: sec=%+v snap=%+v err=%v", sec, mgr.Snapshot().Storage, err)
	}

	// Optimistic concurrency check: stale version must return ErrSettingsConflict
	if _, _, err := mgr.PatchDomain(ctx, "storage", []byte(`{"max_upload_mb":90}`), 1); !errors.Is(err, ErrSettingsConflict) {
		t.Fatalf("expected ErrSettingsConflict on stale version, got %v", err)
	}

	// Explicit clear_secrets removes stored secret cleanly
	_, _, err = mgr.PatchDomain(ctx, "storage", []byte(`{"active_driver":"local","clear_secrets":["s3_secret_key"]}`), ver2)
	if err != nil {
		t.Fatal(err)
	}
	if mgr.Snapshot().Storage.S3SecretKeySet {
		t.Fatal("expected S3SecretKeySet=false after clear_secrets")
	}
}
