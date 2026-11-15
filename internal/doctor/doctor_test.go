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

func buildMiniDoctorMMDB(dbType string, isGeo bool) []byte {
	encStr := func(s string) []byte {
		b := []byte(s)
		if len(b) < 29 {
			return append([]byte{byte((2 << 5) | len(b))}, b...)
		}
		return append([]byte{byte((2 << 5) | 29), byte(len(b) - 29)}, b...)
	}
	var data bytes.Buffer
	if isGeo {
		data.WriteByte(byte((7 << 5) | 1))
		data.Write(encStr("country"))
		data.WriteByte(byte((7 << 5) | 1))
		data.Write(encStr("iso_code"))
		data.Write(encStr("US"))
	} else {
		data.WriteByte(byte((7 << 5) | 1))
		data.Write(encStr("autonomous_system_organization"))
		data.Write(encStr("GOOGLE"))
	}

	var out bytes.Buffer
	// 1-node 24-bit IPv4 tree: left -> 17 (offset 0 in data), right -> 1 (empty)
	out.Write([]byte{0, 0, 17, 0, 0, 1})
	out.Write(make([]byte, 16))
	out.Write(data.Bytes())
	out.WriteString("\xab\xcd\xefMaxMind.com")
	out.WriteByte(byte((7 << 5) | 5))
	out.Write(encStr("binary_format_major_version"))
	out.Write([]byte{byte((5 << 5) | 1), 2})
	out.Write(encStr("node_count"))
	out.Write([]byte{byte((6 << 5) | 1), 1})
	out.Write(encStr("record_size"))
	out.Write([]byte{byte((5 << 5) | 1), 24})
	out.Write(encStr("ip_version"))
	out.Write([]byte{byte((5 << 5) | 1), 4})
	out.Write(encStr("database_type"))
	out.Write(encStr(dbType))
	return out.Bytes()
}

func TestDoctorGeoIPSmartInspectionAndCustomSecurityDir(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	dataDir := filepath.Join(tmp, "data")

	cfg, err := config.LoadWithEnviron(config.CLIOverrides{
		EnvFile: filepath.Join(tmp, "missing.env"),
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

	// 1. Dropping only GeoLite2-ASN.mmdb into default data/geoip must emit geoip_unusable_warn (not false OK!)
	defGeoDir := cfg.GetDefaultGeoIPDir()
	if err := os.MkdirAll(defGeoDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(defGeoDir, "GeoLite2-ASN.mmdb"), buildMiniDoctorMMDB("GeoLite2-ASN", false), 0o600); err != nil {
		t.Fatal(err)
	}

	repASN := Run(ctx, cfg, store)
	var geoASNCheck CheckResult
	for _, c := range repASN.Checks {
		if c.Name == "geoip" {
			geoASNCheck = c
		}
	}
	if geoASNCheck.Status != StatusWarn || geoASNCheck.Code != "geoip_unusable_warn" {
		t.Fatalf("expected geoip_unusable_warn when only ASN MMDB exists, got %+v", geoASNCheck)
	}

	// 2. Persisting custom security.geoip_dir in settings pointing to a folder with City + ASN DBs
	// must resolve the custom directory, mount City, and report geoip_ready_ok with ignored ASN note.
	customGeoDir := filepath.Join(tmp, "custom-geoip")
	if err := os.MkdirAll(customGeoDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(customGeoDir, "GeoLite2-ASN.mmdb"), buildMiniDoctorMMDB("GeoLite2-ASN", false), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(customGeoDir, "GeoLite2-City.mmdb"), buildMiniDoctorMMDB("GeoLite2-City", true), 0o600); err != nil {
		t.Fatal(err)
	}

	sm := repo.NewSettingsManager(store, cfg.KeyRing, "zh-CN")
	patchJSON := `{"geoip_dir":` + `"` + strings.ReplaceAll(customGeoDir, `\`, `\\`) + `"}`
	if _, _, err := sm.PatchDomain(ctx, "security", []byte(patchJSON), 0); err != nil {
		t.Fatalf("patch security.geoip_dir: %v", err)
	}

	repCustom := Run(ctx, cfg, store)
	var geoReadyCheck CheckResult
	for _, c := range repCustom.Checks {
		if c.Name == "geoip" {
			geoReadyCheck = c
		}
	}
	if geoReadyCheck.Status != StatusOK || geoReadyCheck.Code != "geoip_ready_ok" {
		t.Fatalf("expected geoip_ready_ok from persisted security.geoip_dir, got %+v", geoReadyCheck)
	}
	if !strings.Contains(geoReadyCheck.Params["files"], "GeoLite2-City.mmdb") || !strings.Contains(geoReadyCheck.Params["files"], "GeoLite2-ASN.mmdb") {
		t.Fatalf("expected files summary to report active City and ignored ASN DB, got %q", geoReadyCheck.Params["files"])
	}
}

func TestDoctorReadOnlyZeroSideEffectsAndAll8SecretStores(t *testing.T) {
	ctx := context.Background()
	emptyRoot := t.TempDir()
	uninitDataDir := filepath.Join(emptyRoot, "data")
	uninitUploadDir := filepath.Join(emptyRoot, "uploads")
	uninitBackupDir := filepath.Join(emptyRoot, "backups")

	cfgRO, err := config.LoadForDoctorWithEnviron(config.CLIOverrides{
		EnvFile:   filepath.Join(emptyRoot, "missing.env"),
		DataDir:   &uninitDataDir,
		UploadDir: &uninitUploadDir,
		BackupDir: &uninitBackupDir,
	}, nil)
	if err != nil {
		t.Fatalf("LoadForDoctorWithEnviron: %v", err)
	}
	repRO := Run(ctx, cfgRO, nil)
	if !repRO.Healthy {
		t.Fatalf("expected uninitialized read-only doctor report to remain non-fatal (warn only): %+v", repRO.Checks)
	}
	for _, p := range []string{uninitDataDir, uninitUploadDir, uninitBackupDir, filepath.Join(uninitDataDir, ".secret.key"), filepath.Join(uninitDataDir, "volo.db")} {
		if _, statErr := os.Stat(p); !os.IsNotExist(statErr) {
			t.Fatalf("doctor must be 100%% read-only; unexpected path created: %s (err=%v)", p, statErr)
		}
	}

	// Verify SQLite non-WAL (:memory: -> journal_mode=memory) emits db_sqlite_not_wal
	memStore, err := sqlx.Open(ctx, sqlx.Options{Driver: "sqlite", DSN: ":memory:"})
	if err != nil {
		t.Fatalf("open memory sqlite: %v", err)
	}
	defer memStore.Close()
	if err := migrate.Up(ctx, memStore); err != nil {
		t.Fatalf("migrate memory sqlite: %v", err)
	}
	keyA := strings.Repeat("a", 32)
	workRoot := t.TempDir()
	workData := filepath.Join(workRoot, "data")
	cfgA, err := config.LoadWithEnviron(config.CLIOverrides{
		EnvFile:   filepath.Join(workRoot, "missing.env"),
		DataDir:   &workData,
		SecretKey: &keyA,
	}, nil)
	if err != nil {
		t.Fatalf("load cfgA: %v", err)
	}
	repMem := Run(ctx, cfgA, memStore)
	foundNotWAL := false
	for _, c := range repMem.Checks {
		if c.Name == "database" && c.Status == StatusWarn && c.Code == "db_sqlite_not_wal" {
			foundNotWAL = true
		}
	}
	if !foundNotWAL {
		t.Fatalf("expected db_sqlite_not_wal warning on :memory: sqlite, got %+v", repMem.Checks)
	}

	// Populate ALL 8 encrypted stores under KeyA, verify StatusOK under KeyA, and verify each triggers secret_mismatch_warn under KeyB
	keyB := strings.Repeat("b", 32)
	cfgB, err := config.LoadForDoctorWithEnviron(config.CLIOverrides{
		EnvFile:   filepath.Join(workRoot, "missing.env"),
		DataDir:   &workData,
		SecretKey: &keyB,
	}, nil)
	if err != nil {
		t.Fatalf("load cfgB: %v", err)
	}

	now := time.Now().UnixMilli()
	checkMismatch := func(label string, setup func(), cleanup func()) {
		t.Helper()
		setup()
		defer cleanup()

		repOK := Run(ctx, cfgA, memStore)
		for _, c := range repOK.Checks {
			if c.Name == "secret_key" && c.Status != StatusOK {
				t.Fatalf("[%s] expected secret_key OK under KeyA, got %+v", label, c)
			}
		}
		repMismatch := Run(ctx, cfgB, memStore)
		matched := false
		for _, c := range repMismatch.Checks {
			if c.Name == "secret_key" && c.Status == StatusWarn && c.Code == "secret_mismatch_warn" {
				matched = true
			}
		}
		if !matched {
			t.Fatalf("[%s] expected secret_mismatch_warn under KeyB, got %+v", label, repMismatch.Checks)
		}
	}

	// 1. data/db.json
	checkMismatch("db.json", func() {
		if err := config.SaveWizardDBConfig(workData, cfgA.KeyRing, config.WizardDBSelection{Driver: "sqlite", DSN: filepath.Join(workData, "volo.db")}); err != nil {
			t.Fatal(err)
		}
	}, func() {
		_ = os.Remove(filepath.Join(workData, "db.json"))
	})

	// 2. settings.secret_ciphertext
	checkMismatch("settings.secret", func() {
		ct, _ := cfgA.KeyRing.SealString("smtp-pass", "settings:notifications")
		_, err := memStore.ExecContext(ctx, `INSERT INTO settings (domain, value_json, secret_ciphertext, version, updated_at) VALUES ('notifications', '{}', ?, 1, ?)`, ct, now)
		if err != nil {
			t.Fatal(err)
		}
	}, func() {
		_, _ = memStore.ExecContext(ctx, `DELETE FROM settings WHERE domain = 'notifications'`)
	})

	// 3. user_credentials (kind='totp')
	_, err = memStore.ExecContext(ctx, `INSERT INTO users (id, username, email, display_name, avatar_url, bio, website_url, role, password_hash, legacy_hash, status, email_verified_at, preferred_locale, created_at, updated_at) VALUES ('u-doc', 'docadmin', 'doc@example.com', 'Doc', '', '', '', 'admin', 'h', '', 'active', ?, 'zh-CN', ?, ?)`, now, now, now)
	if err != nil {
		t.Fatal(err)
	}
	checkMismatch("user_credentials.totp", func() {
		ct, _ := cfgA.KeyRing.SealString("JBSWY3DPEHPK3PXP", "cred:totp:u-doc")
		_, err := memStore.ExecContext(ctx, `INSERT INTO user_credentials (id, user_id, kind, name, credential_id, secret_ciphertext, public_key_spki, cose_alg, sign_count, meta_json, created_at, last_used_at) VALUES ('c1', 'u-doc', 'totp', 'TOTP', 'totp:u-doc', ?, '', 0, 0, '{}', ?, 0)`, ct, now)
		if err != nil {
			t.Fatal(err)
		}
	}, func() {
		_, _ = memStore.ExecContext(ctx, `DELETE FROM user_credentials WHERE id = 'c1'`)
	})

	// 4. plugins.secret_ciphertext
	checkMismatch("plugins.secret", func() {
		ct, _ := cfgA.KeyRing.SecretBox().Seal(`{"api_key":"secret"}`)
		_, err := memStore.ExecContext(ctx, `INSERT INTO plugins (id, kind, version, is_enabled, config_json, secret_ciphertext, circuit_state, failure_count, last_error, installed_at, updated_at) VALUES ('p1', 'external', '1.0.0', 1, '{}', ?, 'closed', 0, '', ?, ?)`, ct, now, now)
		if err != nil {
			t.Fatal(err)
		}
	}, func() {
		_, _ = memStore.ExecContext(ctx, `DELETE FROM plugins WHERE id = 'p1'`)
	})

	// 5. webhooks.secret_ciphertext
	checkMismatch("webhooks.secret", func() {
		ct, _ := cfgA.KeyRing.SealString("wh-sec", config.WebhookSecretAAD("w1"))
		_, err := memStore.ExecContext(ctx, `INSERT INTO webhooks (id, name, provider, url, chat_id, events_json, secret_ciphertext, is_enabled, last_status_code, last_error, last_triggered_at, created_at, updated_at) VALUES ('w1', 'WH', 'custom', 'https://example.com/hook', '', '["*"]', ?, 1, 0, '', 0, ?, ?)`, ct, now, now)
		if err != nil {
			t.Fatal(err)
		}
	}, func() {
		_, _ = memStore.ExecContext(ctx, `DELETE FROM webhooks WHERE id = 'w1'`)
	})

	// 6. articles.password_ciphertext
	checkMismatch("articles.password", func() {
		ct, _ := cfgA.KeyRing.SealString("art-pass", config.ArticlePasswordAAD("a1"))
		_, err := memStore.ExecContext(ctx, `INSERT INTO articles (id, author_id, category_id, title, slug, legacy_permalink, summary_md, content_md, cover_url, status, published_at, scheduled_at, is_pinned, allow_comment, password_ciphertext, has_content_lock, audio_url, audio_duration_sec, audio_size_bytes, audio_mime, lang, translation_group, license_override, word_count, reading_minutes, view_count, comment_count, created_at, updated_at) VALUES ('a1', 'u-doc', '', 'Protected', 'protected', '', '', 'body', '', 'published', ?, 0, 0, 1, ?, 0, '', 0, 0, '', 'zh-CN', '', '', 1, 1, 0, 0, ?, ?)`, now, ct, now, now)
		if err != nil {
			t.Fatal(err)
		}
	}, func() {
		_, _ = memStore.ExecContext(ctx, `DELETE FROM articles WHERE id = 'a1'`)
	})

	// 7. comments.notify_email_ciphertext
	checkMismatch("comments.notify_email", func() {
		ct, _ := cfgA.KeyRing.SealString("reader@example.com", config.CommentNotifyEmailAAD("cm1"))
		_, err := memStore.ExecContext(ctx, `INSERT INTO comments (id, target_type, target_id, parent_id, reply_to_name, user_id, guest_id_hash, env_fp_hash, author_name, author_url, avatar_url, notify_email_ciphertext, notify_locale, client_ip, ip_masked, geo_region, content_md, status, is_deleted, created_at, updated_at) VALUES ('cm1', 'page', 'about', '', '', '', '', '', 'Reader', '', '', ?, 'zh-CN', '', '', '', 'hi', 'approved', 0, ?, ?)`, ct, now, now)
		if err != nil {
			t.Fatal(err)
		}
	}, func() {
		_, _ = memStore.ExecContext(ctx, `DELETE FROM comments WHERE id = 'cm1'`)
	})

	// 8. settings(appearance).value_json -> theme_configs[themeID][key] ("enc:v1:...")
	checkMismatch("appearance.theme_configs", func() {
		ct, _ := cfgA.KeyRing.SealString("theme-token", "theme.config:volo-studio:analytics_token")
		valJSON := `{"active_theme":"volo-studio","theme_configs":{"volo-studio":{"analytics_token":"` + ct + `"}}}`
		_, err := memStore.ExecContext(ctx, `INSERT INTO settings (domain, value_json, secret_ciphertext, version, updated_at) VALUES ('appearance', ?, '', 1, ?)`, valJSON, now)
		if err != nil {
			t.Fatal(err)
		}
	}, func() {
		_, _ = memStore.ExecContext(ctx, `DELETE FROM settings WHERE domain = 'appearance'`)
	})
}
