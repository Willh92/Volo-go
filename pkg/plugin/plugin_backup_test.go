package plugin_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/Willh92/Volo-go/internal/backup"
	"github.com/Willh92/Volo-go/internal/db/migrate"
	"github.com/Willh92/Volo-go/internal/db/sqlx"
	"github.com/Willh92/Volo-go/pkg/plugin"
)

func TestPluginKVBackupRestoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()

	store, err := sqlx.Open(ctx, sqlx.Options{
		Driver: "sqlite",
		DSN:    filepath.Join(tmpDir, "src.db"),
	})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer store.Close()
	if err := migrate.Up(ctx, store); err != nil {
		t.Fatalf("migrate up: %v", err)
	}

	kv := plugin.NewKVStore(store, "counter-plugin")
	for i := 0; i < 5; i++ {
		if _, err := kv.IncrBy(ctx, "hits:total", 20, 0); err != nil {
			t.Fatalf("IncrBy: %v", err)
		}
	}

	zipPath := filepath.Join(tmpDir, "plugin-kv-backup.zip")
	if _, err := backup.CreateToFile(ctx, store, zipPath, backup.CreateOptions{}); err != nil {
		t.Fatalf("backup.CreateToFile: %v", err)
	}

	store2, err := sqlx.Open(ctx, sqlx.Options{
		Driver: "sqlite",
		DSN:    filepath.Join(tmpDir, "restored.db"),
	})
	if err != nil {
		t.Fatalf("open restored sqlite: %v", err)
	}
	defer store2.Close()
	if err := migrate.Up(ctx, store2); err != nil {
		t.Fatalf("migrate restored sqlite: %v", err)
	}
	if _, err := backup.RestoreFromFile(ctx, store2, zipPath, backup.RestoreOptions{}); err != nil {
		t.Fatalf("backup.RestoreFromFile: %v", err)
	}

	kv2 := plugin.NewKVStore(store2, "counter-plugin")
	restoredHits, ok, err := kv2.Get(ctx, "hits:total")
	if err != nil || !ok || restoredHits != "100" {
		t.Fatalf("expected hits:total=100 after backup restore, got %q ok=%v err=%v", restoredHits, ok, err)
	}
}
