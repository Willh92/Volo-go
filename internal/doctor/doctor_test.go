package doctor

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Willh92/Volo-go/internal/config"
	"github.com/Willh92/Volo-go/internal/db/migrate"
	"github.com/Willh92/Volo-go/internal/db/sqlx"
	"github.com/Willh92/Volo-go/internal/repo"
)

func TestDoctorHealthyAndCrossMachineSecretMismatchWarning(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()

	dataDir := filepath.Join(tmp, "data")
	adminPath := "/secret-console"
	cfg, err := config.LoadWithEnviron(config.CLIOverrides{
		DataDir:   &dataDir,
		AdminPath: &adminPath,
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

	var out bytes.Buffer
	rep.FormatHuman(&out)
	if !strings.Contains(out.String(), "Overall Status: HEALTHY") || !strings.Contains(out.String(), "journal_mode=wal") {
		t.Fatalf("unexpected formatted report:\n%s", out.String())
	}

	// Simulate an encrypted S3 secret sealed with Key A, then inspected with Key B -> should emit WARN on secret_key
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
		if c.Name == "secret_key" && c.Status == StatusWarn {
			foundWarn = true
		}
	}
	if !foundWarn {
		t.Fatalf("expected secret_key warn when VOLO_SECRET_KEY cannot open existing DB secret_ciphertext: %+v", repRotated.Checks)
	}
}
