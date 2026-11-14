package backup_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Willh92/Volo-go/internal/backup"
	"github.com/Willh92/Volo-go/internal/config"
	"github.com/Willh92/Volo-go/internal/db/migrate"
	"github.com/Willh92/Volo-go/internal/db/sqlx"
	"github.com/Willh92/Volo-go/internal/domain"
	"github.com/Willh92/Volo-go/internal/repo"
	"github.com/Willh92/Volo-go/internal/security"
)

func TestScheduler_LocalRetentionPruningAndRemoteS3WebDAVSync(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	dataDir := filepath.Join(tmp, "data")
	uploadDir := filepath.Join(tmp, "uploads")
	backupDir := filepath.Join(tmp, "backups")
	for _, d := range []string{dataDir, uploadDir, backupDir} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}

	secret := "0123456789abcdef0123456789abcdef-backup-sched-key"
	cfg, err := config.Load(config.CLIOverrides{
		SecretKey: &secret,
		DataDir:   &dataDir,
		UploadDir: &uploadDir,
		BackupDir: &backupDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	store, err := sqlx.Open(ctx, sqlx.Options{
		Driver: "sqlite",
		DSN:    filepath.Join(dataDir, "sched.db"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := migrate.Up(ctx, store); err != nil {
		t.Fatal(err)
	}

	settings := repo.NewSettingsManager(store, cfg.KeyRing, "zh-CN")
	if _, err := settings.Reload(ctx); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var webdavPuts int
	var s3Puts int

	davSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != "dav-user" || pass != "dav-secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.Method {
		case "MKCOL":
			w.WriteHeader(http.StatusCreated)
		case http.MethodPut:
			b, _ := io.ReadAll(r.Body)
			if len(b) < 64 {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			mu.Lock()
			webdavPuts++
			mu.Unlock()
			w.WriteHeader(http.StatusCreated)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer davSrv.Close()

	s3Srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 ") {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		b, _ := io.ReadAll(r.Body)
		if len(b) < 64 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		s3Puts++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer s3Srv.Close()

	// 1. Configure WebDAV remote sync + LocalKeepCount = 2
	_, _, err = settings.PatchDomain(ctx, domain.SettingsDomainBackup, []byte(`{
		"local_auto_enabled": true,
		"local_keep_count": 2,
		"remote_enabled": true,
		"remote_protocol": "webdav",
		"webdav_url": "`+davSrv.URL+`",
		"webdav_user": "dav-user",
		"webdav_password": "dav-secret",
		"webdav_dir": "volo-nightly"
	}`), 0)
	if err != nil {
		t.Fatal(err)
	}

	sched := backup.NewScheduler(backup.SchedulerOptions{
		Store:          store,
		Settings:       settings,
		BackupDir:      backupDir,
		UploadDir:      uploadDir,
		OutboundDialer: &security.OutboundSafeDialer{AllowLoopbackForTests: true},
	})

	for i := 0; i < 3; i++ {
		res, rErr := sched.RunOnce(ctx)
		if rErr != nil {
			t.Fatalf("WebDAV RunOnce #%d: %v", i, rErr)
		}
		if !res.RemoteUploaded {
			t.Fatalf("expected RemoteUploaded=true on cycle #%d", i)
		}
		time.Sleep(5 * time.Millisecond)
	}

	entries, _ := os.ReadDir(backupDir)
	if len(entries) != 2 {
		t.Fatalf("expected LocalKeepCount=2 archives in %s, got %d", backupDir, len(entries))
	}
	mu.Lock()
	gotDAV := webdavPuts
	mu.Unlock()
	if gotDAV != 3 {
		t.Fatalf("expected 3 WebDAV streaming PUTs, got %d", gotDAV)
	}

	// 2. Switch to S3 SigV4 remote sync
	_, _, err = settings.PatchDomain(ctx, domain.SettingsDomainBackup, []byte(`{
		"remote_protocol": "s3",
		"s3_endpoint": "`+s3Srv.URL+`",
		"s3_region": "auto",
		"s3_bucket": "backups",
		"s3_access_key": "AKIA_BK",
		"s3_secret_key": "bk-secret-key",
		"s3_prefix": "daily",
		"s3_path_style": true
	}`), 0)
	if err != nil {
		t.Fatal(err)
	}
	resS3, err := sched.RunOnce(ctx)
	if err != nil || !resS3.RemoteUploaded {
		t.Fatalf("S3 RunOnce failed: res=%+v err=%v", resS3, err)
	}
	mu.Lock()
	gotS3 := s3Puts
	mu.Unlock()
	if gotS3 != 1 {
		t.Fatalf("expected 1 S3 streaming PUT, got %d", gotS3)
	}

	// 3. Storage plugin protocol when plugin is detached/unavailable handles gracefully without failing automatic local backup!
	_, _, err = settings.PatchDomain(ctx, domain.SettingsDomainBackup, []byte(`{
		"remote_protocol": "plugin:detached-storage"
	}`), 0)
	if err != nil {
		t.Fatal(err)
	}
	resPlug, err := sched.RunOnce(ctx)
	if err != nil || resPlug.LocalPath == "" {
		t.Fatalf("expected detached plugin remote protocol to preserve automatic local backup without error, got res=%+v err=%v", resPlug, err)
	}

	// 4. Container restart / cold-start seeding from BackupDir:
	//    When a recent volo-auto-*.zip already exists on disk, a freshly constructed scheduler MUST NOT
	//    trigger a redundant backup on its first tick; when the newest auto-backup is older than LocalIntervalHrs,
	//    it MUST run.
	_, _, err = settings.PatchDomain(ctx, domain.SettingsDomainBackup, []byte(`{
		"local_auto_enabled": true,
		"local_interval_hrs": 24,
		"remote_enabled": false
	}`), 0)
	if err != nil {
		t.Fatal(err)
	}

	rebootSchedRecent := backup.NewScheduler(backup.SchedulerOptions{
		Store:     store,
		Settings:  settings,
		BackupDir: backupDir,
		UploadDir: uploadDir,
	})
	if _, ran, err := rebootSchedRecent.RunIfDue(ctx); err != nil || ran {
		t.Fatalf("expected cold-start scheduler to skip tick when recent volo-auto-*.zip exists, got ran=%v err=%v", ran, err)
	}

	staleTime := time.Now().Add(-25 * time.Hour)
	dirEntries, _ := os.ReadDir(backupDir)
	for _, e := range dirEntries {
		if !e.IsDir() {
			p := filepath.Join(backupDir, e.Name())
			if err := os.Chtimes(p, staleTime, staleTime); err != nil {
				t.Fatal(err)
			}
		}
	}

	rebootSchedStale := backup.NewScheduler(backup.SchedulerOptions{
		Store:     store,
		Settings:  settings,
		BackupDir: backupDir,
		UploadDir: uploadDir,
	})
	if resDue, ran, err := rebootSchedStale.RunIfDue(ctx); err != nil || !ran || resDue == nil || resDue.LocalPath == "" {
		t.Fatalf("expected cold-start scheduler to run when newest volo-auto-*.zip is stale (>24h), got ran=%v res=%+v err=%v", ran, resDue, err)
	}
}
