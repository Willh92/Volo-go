package doctor

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Willh92/Volo-go/internal/config"
	"github.com/Willh92/Volo-go/internal/db/migrate"
	"github.com/Willh92/Volo-go/internal/db/sqlx"
	"github.com/Willh92/Volo-go/internal/repo"
)

func TestDoctorHealthyDefaultConfigAndWindowsPermLogic(t *testing.T) {
	// 1. Windows NTFS 0666 mode must pass regular-file check, while symlink fails and Unix 0666 fails.
	if !isSecretKeyModeValid("windows", 0o666) {
		t.Fatal("expected Windows regular file mode 0666 to be accepted")
	}
	if isSecretKeyModeValid("windows", os.ModeSymlink|0o666) {
		t.Fatal("expected Windows symlinked secret key to be rejected")
	}
	if isSecretKeyModeValid("linux", 0o666) {
		t.Fatal("expected Linux mode 0666 to be rejected")
	}
	if !isSecretKeyModeValid("linux", 0o600) {
		t.Fatal("expected Linux mode 0600 to be accepted")
	}

	ctx := context.Background()
	tmp := t.TempDir()

	dataDir := filepath.Join(tmp, "data")
	// Use default AdminPath (/console) and unpopulated data/geoip -> all checks must be StatusOK (zero false warnings!)
	cfg, err := config.LoadWithEnviron(config.CLIOverrides{
		DataDir: &dataDir,
	}, nil)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	store, err := sqlx.Open(ctx, sqlx.Options{
		Driver: cfg.DBDriver,
		DSN:    cfg.DBDSN,
	})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer store.Close()
	if err := migrate.Up(ctx, store); err != nil {
		t.Fatalf("migrate up: %v", err)
	}

	rep := Run(ctx, cfg, store)
	if !rep.Healthy {
		var buf bytes.Buffer
		rep.FormatHuman(&buf)
		t.Fatalf("expected healthy doctor report:\n%s", buf.String())
	}
	for _, c := range rep.Checks {
		if c.Status != StatusOK {
			t.Fatalf("expected zero warnings on fresh default setup, got %s=%s (%s)", c.Name, c.Status, c.Summary)
		}
		if c.Code == "" {
			t.Fatalf("expected structured i18n code on check %s", c.Name)
		}
	}

	var out bytes.Buffer
	rep.FormatHuman(&out)
	if !strings.Contains(out.String(), "Overall Status: HEALTHY") || !strings.Contains(out.String(), "journal_mode=wal") {
		t.Fatalf("unexpected formatted report:\n%s", out.String())
	}

	// 2. Adding an active GeoIP rule while data/geoip has no .mmdb/.xdb should emit StatusWarn on geoip.
	nowMs := time.Now().UnixMilli()
	if _, err := store.ExecContext(ctx,
		`INSERT INTO security_geo_rules (id, name, is_enabled, action, on_miss, countries, regions, sort_order, created_at, updated_at)
		 VALUES ('geo1', 'Allow CN', 1, 'allow', 'allow', '["CN"]', '[]', 10, ?, ?)`, nowMs, nowMs); err != nil {
		t.Fatalf("insert geo rule: %v", err)
	}
	repGeo := Run(ctx, cfg, store)
	foundGeoWarn := false
	for _, c := range repGeo.Checks {
		if c.Name == "geoip" && c.Status == StatusWarn && c.Code == "geoip_missing_warn" {
			foundGeoWarn = true
		}
	}
	if !foundGeoWarn {
		t.Fatalf("expected geoip warn when active geo rules exist without .mmdb/.xdb: %+v", repGeo.Checks)
	}

	// 3. Simulate an encrypted S3 secret sealed with Key A, then inspected with Key B -> should emit WARN on secret_key
	sm := repo.NewSettingsManager(store, cfg.KeyRing, "zh-CN")
	if _, _, err := sm.PatchDomain(ctx, "storage", []byte(`{"s3_secret_key":"top-secret-s3"}`), 0); err != nil {
		t.Fatal(err)
	}

	otherKey := strings.Repeat("z", 32)
	cfgRotated, err := config.LoadWithEnviron(config.CLIOverrides{
		DataDir:   &dataDir,
		SecretKey: &otherKey,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	repRotated := Run(ctx, cfgRotated, store)
	foundWarn := false
	for _, c := range repRotated.Checks {
		if c.Name == "secret_key" && c.Status == StatusWarn && c.Code == "secret_mismatch_warn" {
			foundWarn = true
		}
	}
	if !foundWarn {
		t.Fatalf("expected secret_key warn when VOLO_SECRET_KEY cannot open existing DB secret_ciphertext: %+v", repRotated.Checks)
	}
}
