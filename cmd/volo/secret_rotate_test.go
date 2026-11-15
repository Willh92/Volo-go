package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Willh92/Volo-go/internal/config"
	"github.com/Willh92/Volo-go/internal/db/migrate"
	"github.com/Willh92/Volo-go/internal/db/sqlx"
	"github.com/Willh92/Volo-go/internal/doctor"
)

func TestSecretRotate_All8EncryptedStores_DryRun_AbortOnCorrupt_AndDoctorIntegration(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	uploadDir := filepath.Join(root, "uploads")
	backupDir := filepath.Join(root, "backups")
	wizardDBDir := filepath.Join(root, "custom-db")
	for _, d := range []string{dataDir, uploadDir, backupDir, wizardDBDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	noEnv := filepath.Join(root, "nonexistent.env")
	wizardDBPath := filepath.Join(wizardDBDir, "wizard.db")

	keyA := "old-master-secret-key-alpha-1234567890"
	keyB := "new-master-secret-key-bravo-0987654321"
	krA, _, err := config.LoadOrInitKeyRing(keyA, "")
	if err != nil {
		t.Fatalf("krA: %v", err)
	}
	krB, _, err := config.LoadOrInitKeyRing(keyB, "")
	if err != nil {
		t.Fatalf("krB: %v", err)
	}

	// 1/8: Save encrypted Setup Wizard db.json pointing to non-default wizardDBPath.
	if err := config.SaveWizardDBConfig(dataDir, krA, config.WizardDBSelection{
		Driver: "sqlite",
		DSN:    wizardDBPath,
	}); err != nil {
		t.Fatalf("SaveWizardDBConfig: %v", err)
	}

	store, err := sqlx.Open(ctx, sqlx.Options{
		Driver:       "sqlite",
		DSN:          wizardDBPath,
		MaxOpenConns: 2,
		MaxIdleConns: 1,
	})
	if err != nil {
		t.Fatalf("sqlx.Open: %v", err)
	}
	if err := migrate.Up(ctx, store); err != nil {
		_ = store.Close()
		t.Fatalf("migrate.Up: %v", err)
	}

	now := time.Now().UnixMilli()

	// 2/8: settings.secret_ciphertext
	settingsPlain := `{"api_key":"sk-live-ai-secret-value"}`
	settingsCT, err := krA.SealString(settingsPlain, "settings:ai")
	if err != nil {
		t.Fatalf("seal settings: %v", err)
	}
	if _, err := store.ExecContext(ctx,
		`INSERT INTO settings (domain, value_json, secret_ciphertext, version, updated_at) VALUES (?, ?, ?, 1, ?)`,
		"ai", `{"provider":"openai"}`, settingsCT, now,
	); err != nil {
		t.Fatalf("insert settings(ai): %v", err)
	}

	// 3/8: user_credentials.secret_ciphertext (kind='totp')
	if _, err := store.ExecContext(ctx,
		`INSERT INTO users (id, username, email, display_name, avatar_url, bio, website_url, role, password_hash, legacy_hash, status, email_verified_at, preferred_locale, created_at, updated_at)
		 VALUES ('u-rot-1', 'rotadmin', 'rot@example.com', 'Rot Admin', '', '', '', 'admin', 'argon2id$dummy', '', 'active', ?, 'en', ?, ?)`,
		now, now, now,
	); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	totpPlain := "JBSWY3DPEHPK3PXP"
	totpCT, err := krA.SealString(totpPlain, "cred:totp:u-rot-1")
	if err != nil {
		t.Fatalf("seal totp: %v", err)
	}
	if _, err := store.ExecContext(ctx,
		`INSERT INTO user_credentials (id, user_id, kind, name, credential_id, secret_ciphertext, public_key_spki, cose_alg, sign_count, meta_json, created_at, last_used_at)
		 VALUES ('cred-rot-1', 'u-rot-1', 'totp', 'Authenticator', 'totp-u-rot-1', ?, X'', 0, 0, '{}', ?, ?)`,
		totpCT, now, now,
	); err != nil {
		t.Fatalf("insert user_credentials: %v", err)
	}

	// 4/8: plugins.secret_ciphertext
	pluginPlain := `{"s3_secret":"plug-secret-99"}`
	pluginCT, err := krA.SecretBox().Seal(pluginPlain)
	if err != nil {
		t.Fatalf("seal plugin: %v", err)
	}
	if _, err := store.ExecContext(ctx,
		`INSERT INTO plugins (id, kind, version, is_enabled, config_json, secret_ciphertext, circuit_state, failure_count, last_error, installed_at, updated_at)
		 VALUES ('ext-rot-1', 'wasm', '1.0.0', 1, '{}', ?, 'closed', 0, '', ?, ?)`,
		pluginCT, now, now,
	); err != nil {
		t.Fatalf("insert plugins: %v", err)
	}

	// 5/8: webhooks.secret_ciphertext
	webhookPlain := "whsec_hmac_signing_secret_42"
	webhookCT, err := krA.SealString(webhookPlain, config.WebhookSecretAAD("wh-rot-1"))
	if err != nil {
		t.Fatalf("seal webhook: %v", err)
	}
	if _, err := store.ExecContext(ctx,
		`INSERT INTO webhooks (id, name, provider, url, chat_id, events_json, secret_ciphertext, is_enabled, last_status_code, last_error, last_triggered_at, created_at, updated_at)
		 VALUES ('wh-rot-1', 'Ops Hook', 'generic', 'https://example.com/hook', '', '["comment.created"]', ?, 1, 200, '', 0, ?, ?)`,
		webhookCT, now, now,
	); err != nil {
		t.Fatalf("insert webhooks: %v", err)
	}

	// 6/8: articles.password_ciphertext
	articlePlain := "article-reader-passcode-777"
	articleCT, err := krA.SealString(articlePlain, config.ArticlePasswordAAD("art-rot-1"))
	if err != nil {
		t.Fatalf("seal article: %v", err)
	}
	if _, err := store.ExecContext(ctx,
		`INSERT INTO articles (id, author_id, category_id, title, slug, legacy_permalink, summary_md, content_md, cover_url, status, published_at, scheduled_at, is_pinned, allow_comment, password_ciphertext, has_content_lock, audio_url, audio_duration_sec, audio_size_bytes, audio_mime, lang, translation_group, license_override, word_count, reading_minutes, view_count, comment_count, created_at, updated_at)
		 VALUES ('art-rot-1', 'u-rot-1', '', 'Locked Post', 'locked-post', '', '', 'Secret body', '', 'published', ?, 0, 0, 1, ?, 0, '', 0, 0, '', 'en', '', '', 2, 1, 0, 0, ?, ?)`,
		now, articleCT, now, now,
	); err != nil {
		t.Fatalf("insert articles: %v", err)
	}

	// 7/8: comments.notify_email_ciphertext
	commentEmailPlain := "subscriber@example.com"
	commentCT, err := krA.SealString(commentEmailPlain, config.CommentNotifyEmailAAD("cmt-rot-1"))
	if err != nil {
		t.Fatalf("seal comment email: %v", err)
	}
	if _, err := store.ExecContext(ctx,
		`INSERT INTO comments (id, target_type, target_id, parent_id, reply_to_name, user_id, guest_id_hash, env_fp_hash, author_name, author_url, avatar_url, notify_email_ciphertext, notify_locale, client_ip, ip_masked, geo_region, content_md, status, is_deleted, created_at, updated_at)
		 VALUES ('cmt-rot-1', 'article', 'art-rot-1', '', '', '', 'guest1', 'fp1', 'Reader', '', '', ?, 'en', '127.0.0.1', '127.0.0.*', '', 'Nice!', 'approved', 0, ?, ?)`,
		commentCT, now, now,
	); err != nil {
		t.Fatalf("insert comments: %v", err)
	}

	// 8/8: settings(appearance).value_json -> theme_configs[themeID][key] ("enc:v1:...")
	themeSecretPlain := "umami-token-secret-xyz"
	themeSecretCT, err := krA.SealString(themeSecretPlain, "theme.config:aurora:analytics_token")
	if err != nil {
		t.Fatalf("seal theme secret: %v", err)
	}
	appearanceMap := map[string]any{
		"active_theme": "aurora",
		"theme_configs": map[string]any{
			"aurora": map[string]any{
				"accent_color":    "#0ea5e9",
				"analytics_token": themeSecretCT,
			},
		},
	}
	appBytes, _ := json.Marshal(appearanceMap)
	if _, err := store.ExecContext(ctx,
		`INSERT INTO settings (domain, value_json, secret_ciphertext, version, updated_at) VALUES ('appearance', ?, '', 1, ?)`,
		string(appBytes), now,
	); err != nil {
		t.Fatalf("insert settings(appearance): %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close setup store: %v", err)
	}

	// Step A: --dry-run must verify all 8 stores and mutate NOTHING.
	var stdout, stderr bytes.Buffer
	code := runCLI(ctx, []string{
		"secret", "rotate",
		"--data-dir", dataDir,
		"--env-file", noEnv,
		"--secret-key", keyA,
		"--new-key", keyB,
		"--dry-run",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("dry-run expected exit 0, got %d (stderr=%s)", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Dry-run OK: verified 8 encrypted secret(s) across 8 stores") {
		t.Fatalf("unexpected dry-run stdout: %s", stdout.String())
	}
	if _, err := config.LoadWizardDBConfig(dataDir, krA); err != nil {
		t.Fatalf("dry-run mutated db.json (krA failed): %v", err)
	}
	if _, err := config.LoadWizardDBConfig(dataDir, krB); err == nil {
		t.Fatalf("dry-run should NOT have re-encrypted db.json to krB")
	}

	// Step B: Corrupt 1 row in webhooks -> rotation MUST abort pre-flight with zero mutations!
	storeCheck, err := sqlx.Open(ctx, sqlx.Options{Driver: "sqlite", DSN: wizardDBPath, MaxOpenConns: 2, MaxIdleConns: 1})
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	if _, err := storeCheck.ExecContext(ctx,
		`INSERT INTO webhooks (id, name, provider, url, chat_id, events_json, secret_ciphertext, is_enabled, last_status_code, last_error, last_triggered_at, created_at, updated_at)
		 VALUES ('wh-bad', 'Corrupt', 'generic', 'https://example.com/bad', '', '[]', 'enc:v1:corrupted-payload', 0, 0, '', 0, ?, ?)`,
		now, now,
	); err != nil {
		t.Fatalf("insert corrupt webhook: %v", err)
	}
	_ = storeCheck.Close()

	stdout.Reset()
	stderr.Reset()
	code = runCLI(ctx, []string{
		"secret", "rotate",
		"--data-dir", dataDir,
		"--env-file", noEnv,
		"--secret-key", keyA,
		"--new-key", keyB,
	}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("corrupt ciphertext expected exit 1, got %d (stdout=%s stderr=%s)", code, stdout.String(), stderr.String())
	}
	if _, err := config.LoadWizardDBConfig(dataDir, krA); err != nil {
		t.Fatalf("aborted rotation must leave db.json decryptable by krA: %v", err)
	}

	// Remove the corrupt row and run the real rotation from keyA -> keyB.
	storeClean, err := sqlx.Open(ctx, sqlx.Options{Driver: "sqlite", DSN: wizardDBPath, MaxOpenConns: 2, MaxIdleConns: 1})
	if err != nil {
		t.Fatalf("reopen storeClean: %v", err)
	}
	if _, err := storeClean.ExecContext(ctx, `DELETE FROM webhooks WHERE id = 'wh-bad'`); err != nil {
		t.Fatalf("delete corrupt webhook: %v", err)
	}
	_ = storeClean.Close()

	stdout.Reset()
	stderr.Reset()
	code = runCLI(ctx, []string{
		"secret", "rotate",
		"--data-dir", dataDir,
		"--env-file", noEnv,
		"--secret-key", keyA,
		"--new-key", keyB,
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("secret rotate expected exit 0, got %d (stderr=%s)", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Rotated 8 encrypted secret(s) across 8 stores") {
		t.Fatalf("unexpected rotate stdout: %s", stdout.String())
	}

	// Verify 1/8: db.json now opens ONLY with krB and fails with krA.
	if _, err := config.LoadWizardDBConfig(dataDir, krA); err == nil {
		t.Fatalf("db.json should no longer open with old keyA")
	}
	rotSel, err := config.LoadWizardDBConfig(dataDir, krB)
	if err != nil || rotSel == nil || rotSel.DSN != wizardDBPath {
		t.Fatalf("db.json failed to open with new keyB: sel=%+v err=%v", rotSel, err)
	}

	// Verify 2..8 in SQLite DB: every ciphertext opens with krB (preserving exact AAD & plaintext) and fails with krA.
	verifyStore, err := sqlx.Open(ctx, sqlx.Options{Driver: "sqlite", DSN: wizardDBPath, MaxOpenConns: 2, MaxIdleConns: 1})
	if err != nil {
		t.Fatalf("open verifyStore: %v", err)
	}
	defer verifyStore.Close()

	var gotSettingsCT string
	if err := verifyStore.QueryRowContext(ctx, `SELECT secret_ciphertext FROM settings WHERE domain = 'ai'`).Scan(&gotSettingsCT); err != nil {
		t.Fatalf("scan settings(ai): %v", err)
	}
	if _, err := krA.OpenString(gotSettingsCT, "settings:ai"); err == nil {
		t.Fatalf("settings(ai) still opened with old keyA")
	}
	if pt, err := krB.OpenString(gotSettingsCT, "settings:ai"); err != nil || pt != settingsPlain {
		t.Fatalf("settings(ai) krB mismatch: pt=%q err=%v", pt, err)
	}

	var gotTOTPCT string
	if err := verifyStore.QueryRowContext(ctx, `SELECT secret_ciphertext FROM user_credentials WHERE id = 'cred-rot-1'`).Scan(&gotTOTPCT); err != nil {
		t.Fatalf("scan totp: %v", err)
	}
	if _, err := krA.OpenString(gotTOTPCT, "cred:totp:u-rot-1"); err == nil {
		t.Fatalf("totp still opened with old keyA")
	}
	if pt, err := krB.OpenString(gotTOTPCT, "cred:totp:u-rot-1"); err != nil || pt != totpPlain {
		t.Fatalf("totp krB mismatch: pt=%q err=%v", pt, err)
	}

	var gotPluginCT string
	if err := verifyStore.QueryRowContext(ctx, `SELECT secret_ciphertext FROM plugins WHERE id = 'ext-rot-1'`).Scan(&gotPluginCT); err != nil {
		t.Fatalf("scan plugin: %v", err)
	}
	if _, err := krA.SecretBox().Open(gotPluginCT); err == nil {
		t.Fatalf("plugin secret still opened with old keyA")
	}
	if pt, err := krB.SecretBox().Open(gotPluginCT); err != nil || pt != pluginPlain {
		t.Fatalf("plugin krB mismatch: pt=%q err=%v", pt, err)
	}

	var gotWebhookCT string
	if err := verifyStore.QueryRowContext(ctx, `SELECT secret_ciphertext FROM webhooks WHERE id = 'wh-rot-1'`).Scan(&gotWebhookCT); err != nil {
		t.Fatalf("scan webhook: %v", err)
	}
	if pt, err := krB.OpenString(gotWebhookCT, config.WebhookSecretAAD("wh-rot-1")); err != nil || pt != webhookPlain {
		t.Fatalf("webhook krB mismatch: pt=%q err=%v", pt, err)
	}

	var gotArticleCT string
	if err := verifyStore.QueryRowContext(ctx, `SELECT password_ciphertext FROM articles WHERE id = 'art-rot-1'`).Scan(&gotArticleCT); err != nil {
		t.Fatalf("scan article: %v", err)
	}
	if pt, err := krB.OpenString(gotArticleCT, config.ArticlePasswordAAD("art-rot-1")); err != nil || pt != articlePlain {
		t.Fatalf("article krB mismatch: pt=%q err=%v", pt, err)
	}

	var gotCommentCT string
	if err := verifyStore.QueryRowContext(ctx, `SELECT notify_email_ciphertext FROM comments WHERE id = 'cmt-rot-1'`).Scan(&gotCommentCT); err != nil {
		t.Fatalf("scan comment: %v", err)
	}
	if pt, err := krB.OpenString(gotCommentCT, config.CommentNotifyEmailAAD("cmt-rot-1")); err != nil || pt != commentEmailPlain {
		t.Fatalf("comment krB mismatch: pt=%q err=%v", pt, err)
	}

	var gotAppJSON string
	if err := verifyStore.QueryRowContext(ctx, `SELECT value_json FROM settings WHERE domain = 'appearance'`).Scan(&gotAppJSON); err != nil {
		t.Fatalf("scan appearance: %v", err)
	}
	var parsedApp map[string]any
	if err := json.Unmarshal([]byte(gotAppJSON), &parsedApp); err != nil {
		t.Fatalf("unmarshal appearance: %v", err)
	}
	if parsedApp["active_theme"] != "aurora" {
		t.Fatalf("expected active_theme=aurora preserved, got %v", parsedApp["active_theme"])
	}
	auroraCfg := parsedApp["theme_configs"].(map[string]any)["aurora"].(map[string]any)
	if auroraCfg["accent_color"] != "#0ea5e9" {
		t.Fatalf("expected non-secret accent_color preserved, got %v", auroraCfg["accent_color"])
	}
	rotThemeCT, _ := auroraCfg["analytics_token"].(string)
	if _, err := krA.OpenString(rotThemeCT, "theme.config:aurora:analytics_token"); err == nil {
		t.Fatalf("theme_config secret still opened with old keyA")
	}
	if pt, err := krB.OpenString(rotThemeCT, "theme.config:aurora:analytics_token"); err != nil || pt != themeSecretPlain {
		t.Fatalf("theme_config krB mismatch: pt=%q err=%v", pt, err)
	}
	_ = verifyStore.Close()

	// Step C: `volo doctor --json` with new keyB reports healthy=true, and with old keyA reports failure (exit 1).
	stdout.Reset()
	stderr.Reset()
	code = runCLI(ctx, []string{
		"doctor",
		"--data-dir", dataDir,
		"--upload-dir", uploadDir,
		"--backup-dir", backupDir,
		"--env-file", noEnv,
		"--secret-key", keyB,
		"--json",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("volo doctor with new keyB expected exit 0, got %d (stdout=%s stderr=%s)", code, stdout.String(), stderr.String())
	}
	var rep doctor.Report
	if err := json.Unmarshal(stdout.Bytes(), &rep); err != nil {
		t.Fatalf("unmarshal doctor report: %v", err)
	}
	if !rep.Healthy {
		t.Fatalf("expected doctor Healthy=true after rotation, got %+v", rep)
	}

	for _, c := range rep.Checks {
		if c.Name == "secret_key" && c.Status != doctor.StatusOK {
			t.Fatalf("expected secret_key check StatusOK after rotation, got %+v", c)
		}
	}

	stdout.Reset()
	stderr.Reset()
	code = runCLI(ctx, []string{
		"doctor",
		"--data-dir", dataDir,
		"--upload-dir", uploadDir,
		"--backup-dir", backupDir,
		"--env-file", noEnv,
		"--secret-key", keyA,
		"--json",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("volo doctor with stale keyA expected exit 0 (warn), got %d", code)
	}
	if !strings.Contains(stdout.String(), "secret_mismatch_warn") {
		t.Fatalf("expected secret_mismatch_warn with stale keyA, got %s", stdout.String())
	}
}

func TestSecretRotate_EnvOldKeys_And_GenerateWriteKeyfile(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatalf("mkdir data: %v", err)
	}
	noEnv := filepath.Join(root, "nonexistent.env")
	dbPath := filepath.Join(dataDir, "volo.db")

	keyOld := "legacy-master-secret-key-charlie-111111"
	keyEnvNew := "target-master-secret-key-delta-22222222"
	krOld, _, err := config.LoadOrInitKeyRing(keyOld, "")
	if err != nil {
		t.Fatalf("krOld: %v", err)
	}
	if err := config.SaveWizardDBConfig(dataDir, krOld, config.WizardDBSelection{
		Driver: "sqlite",
		DSN:    dbPath,
	}); err != nil {
		t.Fatalf("SaveWizardDBConfig: %v", err)
	}

	store, err := sqlx.Open(ctx, sqlx.Options{Driver: "sqlite", DSN: dbPath, MaxOpenConns: 2, MaxIdleConns: 1})
	if err != nil {
		t.Fatalf("sqlx.Open: %v", err)
	}
	if err := migrate.Up(ctx, store); err != nil {
		_ = store.Close()
		t.Fatalf("migrate.Up: %v", err)
	}
	ct, err := krOld.SealString("smtp-pass-123", "settings:mail")
	if err != nil {
		t.Fatalf("seal mail: %v", err)
	}
	if _, err := store.ExecContext(ctx,
		`INSERT INTO settings (domain, value_json, secret_ciphertext, version, updated_at) VALUES ('mail', '{}', ?, 1, ?)`,
		ct, time.Now().UnixMilli(),
	); err != nil {
		t.Fatalf("insert mail setting: %v", err)
	}
	_ = store.Close()

	// 1. Rotate using VOLO_SECRET_OLD_KEYS + VOLO_NEW_SECRET_KEY (no CLI secret flags exposed in ps).
	t.Setenv("VOLO_SECRET_KEY", keyEnvNew)
	t.Setenv("VOLO_SECRET_OLD_KEYS", keyOld)
	t.Setenv("VOLO_NEW_SECRET_KEY", keyEnvNew)

	var stdout, stderr bytes.Buffer
	code := runCLI(ctx, []string{
		"secret", "rotate",
		"--data-dir", dataDir,
		"--env-file", noEnv,
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("env-driven secret rotate expected exit 0, got %d (stderr=%s)", code, stderr.String())
	}

	krEnvNew, _, err := config.LoadOrInitKeyRing(keyEnvNew, "")
	if err != nil {
		t.Fatalf("krEnvNew: %v", err)
	}
	if _, err := config.LoadWizardDBConfig(dataDir, krEnvNew); err != nil {
		t.Fatalf("db.json not decryptable by VOLO_NEW_SECRET_KEY: %v", err)
	}

	// 2. Rotate using --generate --write-keyfile and verify standard config.Load boots seamlessly from .secret.key alone!
	t.Setenv("VOLO_SECRET_KEY", "")
	t.Setenv("VOLO_SECRET_OLD_KEYS", "")
	t.Setenv("VOLO_NEW_SECRET_KEY", "")

	stdout.Reset()
	stderr.Reset()
	code = runCLI(ctx, []string{
		"secret", "rotate",
		"--data-dir", dataDir,
		"--env-file", noEnv,
		"--secret-key", keyEnvNew,
		"--generate",
		"--write-keyfile",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("--generate --write-keyfile expected exit 0, got %d (stderr=%s)", code, stderr.String())
	}

	keyFilePath := filepath.Join(dataDir, ".secret.key")
	st, err := os.Stat(keyFilePath)
	if err != nil || st.Size() < 32 {
		t.Fatalf("expected .secret.key >= 32 bytes at %s, stat=%v err=%v", keyFilePath, st, err)
	}

	loadedCfg, err := config.Load(config.CLIOverrides{
		DataDir: &dataDir,
		EnvFile: noEnv,
	})
	if err != nil {
		t.Fatalf("config.Load from rotated .secret.key + db.json failed: %v", err)
	}
	verifyStore, err := sqlx.Open(ctx, sqlx.Options{Driver: "sqlite", DSN: dbPath, MaxOpenConns: 2, MaxIdleConns: 1})
	if err != nil {
		t.Fatalf("reopen db: %v", err)
	}
	defer verifyStore.Close()
	var gotMailCT string
	if err := verifyStore.QueryRowContext(ctx, `SELECT secret_ciphertext FROM settings WHERE domain = 'mail'`).Scan(&gotMailCT); err != nil {
		t.Fatalf("scan mail setting: %v", err)
	}
	pt, err := loadedCfg.KeyRing.OpenString(gotMailCT, "settings:mail")
	if err != nil || pt != "smtp-pass-123" {
		t.Fatalf("loadedCfg.KeyRing failed to decrypt rotated mail secret: pt=%q err=%v", pt, err)
	}
}

func TestDoctorCLI_ZeroSideEffectsOnUninitializedDir_And_DetectsSchemaBehind(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	uninitData := filepath.Join(root, "fresh-data")
	uninitUp := filepath.Join(root, "fresh-uploads")
	uninitBk := filepath.Join(root, "fresh-backups")
	noEnv := filepath.Join(root, "nonexistent.env")

	// 1. Run `volo doctor` on completely non-existent directories: zero dirs or files may be created!
	var stdout, stderr bytes.Buffer
	code := runCLI(ctx, []string{
		"doctor",
		"--data-dir", uninitData,
		"--upload-dir", uninitUp,
		"--backup-dir", uninitBk,
		"--env-file", noEnv,
		"--json",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doctor on uninitialized writable parent expected exit 0 (warnings only), got %d (stdout=%s stderr=%s)", code, stdout.String(), stderr.String())
	}
	for _, p := range []string{
		uninitData,
		uninitUp,
		uninitBk,
		filepath.Join(uninitData, ".secret.key"),
		filepath.Join(uninitData, "volo.db"),
	} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("volo doctor must be 100%% read-only, but created %s (err=%v)", p, err)
		}
	}

	// 2. Create a SQLite DB pinned at schema version 1 (< CurrentSchemaVersion) and prove `volo doctor`
	// reports `db_schema_behind` WITHOUT auto-running migrate.Up!
	lagData := filepath.Join(root, "lag-data")
	if err := os.MkdirAll(lagData, 0o755); err != nil {
		t.Fatalf("mkdir lagData: %v", err)
	}
	lagDBPath := filepath.Join(lagData, "volo.db")
	store, err := sqlx.Open(ctx, sqlx.Options{Driver: "sqlite", DSN: lagDBPath, MaxOpenConns: 2, MaxIdleConns: 1})
	if err != nil {
		t.Fatalf("open lagDB: %v", err)
	}
	if _, err := store.ExecContext(ctx, `CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)`); err != nil {
		t.Fatalf("create schema_migrations: %v", err)
	}
	if _, err := store.ExecContext(ctx, `INSERT INTO schema_migrations (version, applied_at) VALUES (1, 1700000000000)`); err != nil {
		t.Fatalf("seed schema_migrations v1: %v", err)
	}
	_ = store.Close()

	stdout.Reset()
	stderr.Reset()
	code = runCLI(ctx, []string{
		"doctor",
		"--data-dir", lagData,
		"--upload-dir", uninitUp,
		"--backup-dir", uninitBk,
		"--env-file", noEnv,
		"--secret-key", "explicit-doctor-test-secret-key-12345",
		"--json",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doctor with lagging schema expected exit 0 (warn), got %d (stdout=%s stderr=%s)", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "db_schema_behind") {
		t.Fatalf("expected db_schema_behind in doctor report, got: %s", stdout.String())
	}

	recheck, err := sqlx.Open(ctx, sqlx.Options{Driver: "sqlite", DSN: lagDBPath, MaxOpenConns: 2, MaxIdleConns: 1})
	if err != nil {
		t.Fatalf("reopen lagDB: %v", err)
	}
	defer recheck.Close()
	var maxVer int
	if err := recheck.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&maxVer); err != nil {
		t.Fatalf("query max version: %v", err)
	}
	if maxVer != 1 {
		t.Fatalf("volo doctor mutated database schema! expected version=1, got %d (CurrentSchemaVersion=%d)", maxVer, migrate.CurrentSchemaVersion)
	}
}
