package backup

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Willh92/Volo-go/internal/config"
	"github.com/Willh92/Volo-go/internal/db/sqlx"
	"github.com/Willh92/Volo-go/internal/domain"
	"github.com/Willh92/Volo-go/internal/i18n"
	"github.com/Willh92/Volo-go/internal/repo"
	"github.com/Willh92/Volo-go/pkg/plugin"
)

func TestCoerceJSONValNullFidelityAndHeterogeneousNDJSONColumns(t *testing.T) {
	if got := coerceJSONValForDB("users", "email_verified_at", nil, "sqlite"); got != nil {
		t.Fatalf("expected nil JSON value to remain SQL NULL (nil), got %#v", got)
	}

	ctx := context.Background()
	store := openTestSQLiteStore(t, filepath.Join(t.TempDir(), "ndjson-null.db"))
	if _, err := store.ExecContext(ctx, `CREATE TABLE nullable_notes (id TEXT PRIMARY KEY, opt_note TEXT NULL, extra_tag TEXT NOT NULL DEFAULT '')`); err != nil {
		t.Fatal(err)
	}

	// Row 1 sets `opt_note: null` and OMITS `extra_tag`.
	// Row 2 provides BOTH `opt_note: "present"` and `extra_tag: "row2-tag"`.
	// Previously, coerceJSONValForDB(nil) coerced NULL -> "", AND streamNDJSONIntoTable permanently dropped `extra_tag` on Row 2!
	ndjson := strings.Join([]string{
		`{"id":"n_1","opt_note":null}`,
		`{"id":"n_2","opt_note":"present","extra_tag":"row2-tag"}`,
	}, "\n") + "\n"

	var zbuf bytes.Buffer
	zw := zip.NewWriter(&zbuf)
	zfWriter, err := zw.Create("tables/nullable_notes.ndjson")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := zfWriter.Write([]byte(ndjson)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(zbuf.Bytes()), int64(zbuf.Len()))
	if err != nil {
		t.Fatal(err)
	}

	err = store.WithTx(ctx, func(tx *sqlx.Tx) error {
		_, sErr := streamNDJSONIntoTable(ctx, tx, "nullable_notes", zr.File[0], nil)
		return sErr
	})
	if err != nil {
		t.Fatalf("streamNDJSONIntoTable failed: %v", err)
	}

	var optNote1 sql.NullString
	if err := store.QueryRowContext(ctx, `SELECT opt_note FROM nullable_notes WHERE id = 'n_1'`).Scan(&optNote1); err != nil {
		t.Fatal(err)
	}
	if optNote1.Valid {
		t.Fatalf("expected n_1.opt_note to be SQL NULL, got %q", optNote1.String)
	}

	var optNote2, extraTag2 sql.NullString
	if err := store.QueryRowContext(ctx, `SELECT opt_note, extra_tag FROM nullable_notes WHERE id = 'n_2'`).Scan(&optNote2, &extraTag2); err != nil {
		t.Fatal(err)
	}
	if !optNote2.Valid || optNote2.String != "present" || !extraTag2.Valid || extraTag2.String != "row2-tag" {
		t.Fatalf("expected Row 2 opt_note='present' and extra_tag='row2-tag' to survive despite Row 1 omitting extra_tag, got note=%+v tag=%+v", optNote2, extraTag2)
	}
}

type stubBackupStoragePlugin struct {
	mu      sync.Mutex
	putKeys []string
}

func (s *stubBackupStoragePlugin) Name() string { return "r2-plugin" }
func (s *stubBackupStoragePlugin) Put(_ context.Context, objectKey string, r io.Reader, _ int64, _ string) (string, error) {
	_, _ = io.Copy(io.Discard, r)
	s.mu.Lock()
	s.putKeys = append(s.putKeys, objectKey)
	s.mu.Unlock()
	return "https://plugin-cdn.example.com/" + objectKey, nil
}
func (s *stubBackupStoragePlugin) Delete(context.Context, string) error      { return nil }
func (s *stubBackupStoragePlugin) HealthCheck(context.Context) error         { return nil }

func TestWebDAVBlocksHTTPSDowngradeRedirectAndPluginBackupTarget(t *testing.T) {
	ctx := context.Background()

	// 1. HTTPS WebDAV server redirecting to plain HTTP MUST NOT leak Basic Auth over HTTP!
	plainTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("SECURITY VIOLATION: followed HTTPS -> HTTP downgrade redirect with Authorization=%q", r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusCreated)
	}))
	defer plainTarget.Close()

	tlsOrigin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plainTarget.URL+"/volo.zip", http.StatusMovedPermanently)
	}))
	defer tlsOrigin.Close()

	_, err := uploadWebDAVStream(ctx, domain.BackupSettings{
		WebDAVURL:  tlsOrigin.URL,
		WebDAVUser: "admin",
	}, "secret", bytes.NewReader([]byte("zip")), 3, "test.zip", nil)
	if err == nil {
		t.Fatal("expected TLS / downgrade redirect failure")
	}

	// 2. Scheduler RemoteProtocol = "plugin:r2-plugin" pushes backup ZIP through active plugin StorageProvider!
	tmp := t.TempDir()
	backupDir := filepath.Join(tmp, "backups")
	_ = os.MkdirAll(backupDir, 0o750)
	store := openTestSQLiteStore(t, filepath.Join(tmp, "sched-plugin.db"))
	kr, _, err := config.LoadOrInitKeyRing(strings.Repeat("b", 32), tmp)
	if err != nil {
		t.Fatal(err)
	}
	settings := repo.NewSettingsManager(store, kr, "en-US")
	_, _ = settings.Reload(ctx)
	cat, _ := i18n.NewCatalog("en-US", "")

	plugMgr := plugin.NewManager(plugin.ManagerOptions{
		Store:      store,
		Keys:       kr,
		PluginsDir: filepath.Join(tmp, "plugins"),
		DataDir:    tmp,
		Catalog:    cat,
	})
	sp := &stubBackupStoragePlugin{}
	plugMgr.Providers().RegisterStorage("r2-plugin", sp)

	_, _, err = settings.PatchDomain(ctx, domain.SettingsDomainBackup, []byte(`{
		"local_auto_enabled": true,
		"local_keep_count": 3,
		"remote_enabled": true,
		"remote_protocol": "plugin:r2-plugin"
	}`), 0)
	if err != nil {
		t.Fatal(err)
	}

	sched := NewScheduler(SchedulerOptions{
		Store:     store,
		Settings:  settings,
		Plugins:   plugMgr,
		BackupDir: backupDir,
	})
	res, err := sched.RunOnce(ctx)
	if err != nil {
		t.Fatalf("Scheduler.RunOnce with plugin storage failed: %v", err)
	}
	if !res.RemoteUploaded || !strings.HasPrefix(res.RemoteTarget, "https://plugin-cdn.example.com/backups/") || len(sp.putKeys) != 1 {
		t.Fatalf("expected remote backup uploaded to plugin StorageProvider, got res=%+v keys=%v", res, sp.putKeys)
	}
}
