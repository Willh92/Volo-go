package repo

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/Willh92/Volo-go/internal/config"
	"github.com/Willh92/Volo-go/internal/db/migrate"
	"github.com/Willh92/Volo-go/internal/db/sqlx"
)

func TestSiteCustomVarsKeyDeletionPersistsCleanly(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()
	kr, _, err := config.LoadOrInitKeyRing("unit-test-master-secret-key-2026-r4", tmpDir)
	if err != nil {
		t.Fatal(err)
	}
	store, err := sqlx.Open(ctx, sqlx.Options{
		Driver: "sqlite",
		DSN:    filepath.Join(tmpDir, "settings-r4.db"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := migrate.Up(ctx, store); err != nil {
		t.Fatal(err)
	}

	mgr := NewSettingsManager(store, kr, "en-US")

	if _, _, err := mgr.PatchDomain(ctx, "site", []byte(`{"custom_vars":{"public_url":"https://studio.example.com","legacy_banner":"old","temp_key":"drop_me"}}`), 0); err != nil {
		t.Fatalf("initial PatchDomain(site) failed: %v", err)
	}
	snap := mgr.Snapshot()
	if len(snap.Site.CustomVars) != 3 || snap.Site.CustomVars["temp_key"] != "drop_me" {
		t.Fatalf("expected 3 custom_vars initially, got %+v", snap.Site.CustomVars)
	}

	// Patch custom_vars omitting temp_key and passing legacy_banner: null -> deleted keys must NOT be resurrected by deepMergeMaps!
	if _, _, err := mgr.PatchDomain(ctx, "site", []byte(`{"custom_vars":{"public_url":"https://studio.example.com","legacy_banner":null}}`), 0); err != nil {
		t.Fatalf("delete PatchDomain(site) failed: %v", err)
	}
	snap = mgr.Snapshot()
	if len(snap.Site.CustomVars) != 1 || snap.Site.CustomVars["public_url"] != "https://studio.example.com" {
		t.Fatalf("expected deleted custom_vars keys to stay removed, got %+v", snap.Site.CustomVars)
	}

	// Patching unrelated site field without custom_vars key in payload must preserve remaining custom_vars!
	if _, _, err := mgr.PatchDomain(ctx, "site", []byte(`{"title":"Updated Title"}`), 0); err != nil {
		t.Fatalf("unrelated PatchDomain(site) failed: %v", err)
	}
	snap = mgr.Snapshot()
	if snap.Site.Title != "Updated Title" || snap.Site.CustomVars["public_url"] != "https://studio.example.com" {
		t.Fatalf("expected unrelated site patch to preserve custom_vars, got %+v", snap.Site)
	}
}
