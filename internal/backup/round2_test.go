package backup

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Willh92/Volo-go/internal/config"
	"github.com/Willh92/Volo-go/internal/db/migrate"
	"github.com/Willh92/Volo-go/internal/db/sqlx"
	"github.com/Willh92/Volo-go/internal/domain"
	"github.com/Willh92/Volo-go/internal/i18n"
	"github.com/Willh92/Volo-go/internal/repo"
	"github.com/Willh92/Volo-go/internal/security"
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
func (s *stubBackupStoragePlugin) Delete(context.Context, string) error { return nil }
func (s *stubBackupStoragePlugin) HealthCheck(context.Context) error    { return nil }

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

func TestBackupRound3_ConcurrentCreate_UnsignedEntryRejection_MySQLUnicodeCI_And_RemotePrune(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	store := openTestSQLiteStore(t, filepath.Join(tmp, "round3-bk.db"))
	if err := migrate.Up(ctx, store); err != nil {
		t.Fatal(err)
	}

	// 1. Concurrent CreateToFile to the exact same destination path must never collide on `.tmp`!
	targetZip := filepath.Join(tmp, "concurrent.zip")
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := CreateToFile(ctx, store, targetZip, CreateOptions{}); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent CreateToFile failed: %v", err)
	}
	if _, err := RestoreFromFile(ctx, store, targetZip, RestoreOptions{DryRun: true}); err != nil {
		t.Fatalf("expected valid archive after concurrent CreateToFile, got: %v", err)
	}

	// 2. Reject unsigned extra ZIP entries under tables/, uploads/, or media/ missing from manifest.json.
	origBytes, err := os.ReadFile(targetZip)
	if err != nil {
		t.Fatal(err)
	}
	origZr, err := zip.NewReader(bytes.NewReader(origBytes), int64(len(origBytes)))
	if err != nil {
		t.Fatal(err)
	}
	for _, rogueEntry := range []string{"tables/users_rogue.ndjson", "uploads/2026/rogue.png", "media/rogue.webp"} {
		var tampered bytes.Buffer
		zw := zip.NewWriter(&tampered)
		for _, f := range origZr.File {
			rc, oErr := f.Open()
			if oErr != nil {
				t.Fatal(oErr)
			}
			w, cErr := zw.Create(f.Name)
			if cErr != nil {
				t.Fatal(cErr)
			}
			_, _ = io.Copy(w, rc)
			_ = rc.Close()
		}
		rw, _ := zw.Create(rogueEntry)
		_, _ = rw.Write([]byte(`{"rogue":true}`))
		_ = zw.Close()

		_, rErr := Restore(ctx, store, bytes.NewReader(tampered.Bytes()), int64(tampered.Len()), RestoreOptions{DryRun: true})
		if rErr == nil || !strings.Contains(rErr.Error(), "unsigned extra archive entry") {
			t.Fatalf("expected unsigned extra entry %q to fail closed, got err=%v", rogueEntry, rErr)
		}
	}

	// 3. MySQL utf8mb4_unicode_ci folding & disambiguation handles case, accents, ligatures, trailing spaces, and emails.
	seen := make(map[string]map[string]bool)
	rows := []map[string]any{
		{"name": "Café", "slug": "strasse"},
		{"name": "cafe ", "slug": "straße"},
		{"name": "CAFE", "slug": "STRASSE"},
	}
	for _, r := range rows {
		disambiguateMySQLUniqueRow("tags", r, seen)
	}
	if rows[0]["name"] != "Café" || rows[1]["name"] != "cafe (2)" || rows[2]["name"] != "CAFE (3)" {
		t.Fatalf("unexpected MySQL disambiguated tag names: %+v", rows)
	}
	if rows[0]["slug"] != "strasse" || rows[1]["slug"] != "straße-2" || rows[2]["slug"] != "STRASSE-3" {
		t.Fatalf("unexpected MySQL disambiguated tag slugs: %+v", rows)
	}
	userSeen := make(map[string]map[string]bool)
	u1 := map[string]any{"username": "Admin", "email": "Admin@Example.com"}
	u2 := map[string]any{"username": "admin", "email": "admin@example.com"}
	disambiguateMySQLUniqueRow("users", u1, userSeen)
	disambiguateMySQLUniqueRow("users", u2, userSeen)
	if u2["username"] != "admin-2" || u2["email"] != "admin+2@example.com" {
		t.Fatalf("unexpected MySQL disambiguated user row: %+v", u2)
	}

	// 4. Remote S3 & WebDAV retention pruning keeps `LocalKeepCount = 2` newest auto-backups!
	type remoteObj struct {
		modTime time.Time
		size    int
	}
	var s3Mu sync.Mutex
	s3Store := map[string]remoteObj{
		"nightly/manual-keep.zip": {modTime: time.Now().UTC().Add(-48 * time.Hour), size: 128},
	}
	s3Srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 ") {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		key := strings.TrimPrefix(r.URL.Path, "/bk-bucket/")
		s3Mu.Lock()
		defer s3Mu.Unlock()
		switch r.Method {
		case http.MethodPut:
			b, _ := io.ReadAll(r.Body)
			s3Store[key] = remoteObj{modTime: time.Now().UTC(), size: len(b)}
			w.WriteHeader(http.StatusOK)
		case http.MethodGet:
			if r.URL.Query().Get("list-type") != "2" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			pfx := r.URL.Query().Get("prefix")
			w.Header().Set("Content-Type", "application/xml")
			var xmlBuf strings.Builder
			xmlBuf.WriteString(`<?xml version="1.0" encoding="UTF-8"?><ListBucketResult><IsTruncated>false</IsTruncated>`)
			keys := make([]string, 0, len(s3Store))
			for k := range s3Store {
				if strings.HasPrefix(k, pfx) {
					keys = append(keys, k)
				}
			}
			sort.Strings(keys)
			for _, k := range keys {
				obj := s3Store[k]
				fmt.Fprintf(&xmlBuf, "<Contents><Key>%s</Key><LastModified>%s</LastModified><Size>%d</Size></Contents>",
					k, obj.modTime.UTC().Format(time.RFC3339Nano), obj.size)
			}
			xmlBuf.WriteString(`</ListBucketResult>`)
			_, _ = w.Write([]byte(xmlBuf.String()))
		case http.MethodDelete:
			delete(s3Store, key)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer s3Srv.Close()

	var davMu sync.Mutex
	davStore := map[string]time.Time{
		"manual-keep.zip": time.Now().UTC().Add(-48 * time.Hour),
	}
	davSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		davMu.Lock()
		defer davMu.Unlock()
		switch r.Method {
		case "MKCOL":
			w.WriteHeader(http.StatusCreated)
		case http.MethodPut:
			_, _ = io.Copy(io.Discard, r.Body)
			davStore[path.Base(r.URL.Path)] = time.Now().UTC()
			w.WriteHeader(http.StatusCreated)
		case "PROPFIND":
			w.Header().Set("Content-Type", "application/xml; charset=utf-8")
			w.WriteHeader(http.StatusMultiStatus)
			var xb strings.Builder
			xb.WriteString(`<?xml version="1.0" encoding="utf-8"?><D:multistatus xmlns:D="DAV:">`)
			xb.WriteString(`<D:response><D:href>/dav/volo-nightly/</D:href></D:response>`)
			names := make([]string, 0, len(davStore))
			for n := range davStore {
				names = append(names, n)
			}
			sort.Strings(names)
			for _, n := range names {
				fmt.Fprintf(&xb, `<D:response><D:href>/dav/volo-nightly/%s</D:href><D:propstat><D:prop><D:getlastmodified>%s</D:getlastmodified></D:prop></D:propstat></D:response>`,
					n, davStore[n].UTC().Format(http.TimeFormat))
			}
			xb.WriteString(`</D:multistatus>`)
			_, _ = w.Write([]byte(xb.String()))
		case http.MethodDelete:
			delete(davStore, path.Base(r.URL.Path))
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer davSrv.Close()

	kr, _, err := config.LoadOrInitKeyRing(strings.Repeat("k", 32), tmp)
	if err != nil {
		t.Fatal(err)
	}
	settings := repo.NewSettingsManager(store, kr, "en-US")
	_, _ = settings.Reload(ctx)
	backupDir := filepath.Join(tmp, "auto-backups")

	// Run 4 S3 cycles with LocalKeepCount = 2
	_, _, err = settings.PatchDomain(ctx, domain.SettingsDomainBackup, []byte(`{
		"local_auto_enabled": true,
		"local_keep_count": 2,
		"remote_enabled": true,
		"remote_protocol": "s3",
		"s3_endpoint": "`+s3Srv.URL+`",
		"s3_region": "us-east-1",
		"s3_bucket": "bk-bucket",
		"s3_access_key": "AKIA_PRUNE",
		"s3_secret_key": "prune-secret",
		"s3_prefix": "nightly",
		"s3_path_style": true
	}`), 0)
	if err != nil {
		t.Fatal(err)
	}
	sched := NewScheduler(SchedulerOptions{
		Store:          store,
		Settings:       settings,
		BackupDir:      backupDir,
		OutboundDialer: &security.OutboundSafeDialer{AllowLoopbackForTests: true},
	})
	for i := 0; i < 4; i++ {
		if _, err := sched.RunOnce(ctx); err != nil {
			t.Fatalf("S3 RunOnce #%d: %v", i, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	s3Mu.Lock()
	s3Count := len(s3Store)
	_, manualS3Kept := s3Store["nightly/manual-keep.zip"]
	s3Mu.Unlock()
	if s3Count != 3 || !manualS3Kept {
		t.Fatalf("expected 2 pruned S3 auto-backups + 1 manual-keep.zip = 3 keys, got count=%d store=%+v", s3Count, s3Store)
	}

	// Run 4 WebDAV cycles with LocalKeepCount = 2
	_, _, err = settings.PatchDomain(ctx, domain.SettingsDomainBackup, []byte(`{
		"remote_protocol": "webdav",
		"webdav_url": "`+davSrv.URL+`/dav",
		"webdav_user": "u",
		"webdav_password": "p",
		"webdav_dir": "volo-nightly"
	}`), 0)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		if _, err := sched.RunOnce(ctx); err != nil {
			t.Fatalf("WebDAV RunOnce #%d: %v", i, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	davMu.Lock()
	davCount := len(davStore)
	_, manualDAVKept := davStore["manual-keep.zip"]
	davMu.Unlock()
	if davCount != 3 || !manualDAVKept {
		t.Fatalf("expected 2 pruned WebDAV auto-backups + 1 manual-keep.zip = 3 entries, got count=%d store=%+v", davCount, davStore)
	}
}
