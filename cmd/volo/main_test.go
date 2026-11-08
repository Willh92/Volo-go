package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Willh92/Volo-go/internal/auth"
	"github.com/Willh92/Volo-go/internal/config"
	"github.com/Willh92/Volo-go/internal/db/sqlx"
	"github.com/Willh92/Volo-go/internal/doctor"
	"github.com/Willh92/Volo-go/internal/domain"
)

func TestCLISwissArmyKnifeEndToEnd(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	dataDir := filepath.Join(tmp, "data")

	// 1. volo doctor --json (auto-initializes SQLite DB + .secret.key on zero config)
	var out, errBuf bytes.Buffer
	code := runCLI(ctx, []string{"doctor", "--data-dir", dataDir, "--json"}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("volo doctor failed (code=%d): stderr=%s stdout=%s", code, errBuf.String(), out.String())
	}
	var docRep doctor.Report
	if err := json.Unmarshal(out.Bytes(), &docRep); err != nil {
		t.Fatalf("parse doctor json: %v", err)
	}
	if !docRep.Healthy {
		t.Fatalf("expected healthy doctor report: %+v", docRep)
	}

	// 2. volo import-legacy <legacy.zip>
	legacyZip := filepath.Join(tmp, "legacy.zip")
	writeMinimalLegacyZip(t, legacyZip)
	out.Reset()
	errBuf.Reset()
	code = runCLI(ctx, []string{"import-legacy", legacyZip, "--data-dir", dataDir}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("volo import-legacy failed (code=%d): %s", code, errBuf.String())
	}

	// Seed a session + TOTP credential for imported admin ("admin") so we can test reset-password & disable-2fa
	cfg, err := config.LoadWithEnviron(config.CLIOverrides{DataDir: &dataDir}, nil)
	if err != nil {
		t.Fatal(err)
	}
	store, err := openMigratedStore(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	adminUser, err := sqlx.Get[domain.User](ctx, store, `SELECT * FROM users WHERE username = ?`, "admin")
	if err != nil {
		t.Fatalf("query imported user: %v", err)
	}
	_, err = store.ExecContext(ctx,
		`INSERT INTO user_sessions (id, user_id, token_hash, ip_masked, geo_region, user_agent, totp_verified_until, expires_at, last_seen_at, created_at)
		 VALUES ('s1', ?, 'th_1', '127.0.*.*', '', 'test', 0, ?, ?, ?)`,
		adminUser.ID, now+3600000, now, now)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.ExecContext(ctx,
		`INSERT INTO user_credentials (id, user_id, kind, name, credential_id, public_key_spki, cose_alg, sign_count, secret_ciphertext, meta_json, last_used_at, created_at)
		 VALUES ('cred_totp', ?, 'totp', 'Authenticator', 'totp_1', '', 0, 0, 'enc', '{}', 0, ?)`,
		adminUser.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	_ = store.Close()

	// 3. volo admin reset-password --user admin --password NewStrongPass99!
	out.Reset()
	errBuf.Reset()
	code = runCLI(ctx, []string{
		"admin", "reset-password",
		"--data-dir", dataDir,
		"--user", "admin",
		"--password", "NewStrongPass99!",
	}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("admin reset-password failed (code=%d): %s", code, errBuf.String())
	}

	// 4. volo admin disable-2fa --user admin@example.com
	out.Reset()
	errBuf.Reset()
	code = runCLI(ctx, []string{
		"admin", "disable-2fa",
		"--data-dir", dataDir,
		"--user", "admin@example.com",
	}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("admin disable-2fa failed (code=%d): %s", code, errBuf.String())
	}
	if !strings.Contains(out.String(), "removed_credentials=1") {
		t.Fatalf("expected removed_credentials=1, got: %s", out.String())
	}

	// Verify Argon2id password works, sessions were purged, and TOTP credential removed
	store2, err := openMigratedStore(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	updatedAdmin, err := sqlx.Get[domain.User](ctx, store2, `SELECT * FROM users WHERE id = ?`, adminUser.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ok, up := auth.VerifyPassword("NewStrongPass99!", updatedAdmin.PasswordHash, updatedAdmin.LegacyHash); !ok || up || updatedAdmin.LegacyHash != "" {
		t.Fatalf("expected clean Argon2id verification after CLI reset, got ok=%v up=%v legacy=%q", ok, up, updatedAdmin.LegacyHash)
	}
	remSessions, _ := sqlx.Select[domain.UserSession](ctx, store2, `SELECT * FROM user_sessions WHERE user_id = ?`, adminUser.ID)
	if len(remSessions) != 0 {
		t.Fatalf("expected 0 sessions after reset-password, got %d", len(remSessions))
	}
	_ = store2.Close()

	// 5. volo backup create & volo backup restore into a fresh second dataDir!
	backupZip := filepath.Join(tmp, "cli-backup.zip")
	out.Reset()
	errBuf.Reset()
	code = runCLI(ctx, []string{"backup", "create", "--data-dir", dataDir, "--output", backupZip}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("backup create failed: %s", errBuf.String())
	}

	dataDir2 := filepath.Join(tmp, "data-restored")
	out.Reset()
	errBuf.Reset()
	code = runCLI(ctx, []string{"backup", "restore", backupZip, "--data-dir", dataDir2}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("backup restore failed: %s", errBuf.String())
	}
	if !strings.Contains(out.String(), `"articles": 1`) {
		t.Fatalf("expected restored articles count in output: %s", out.String())
	}

	// 6. volo serve over Unix Domain Socket + /healthz check + graceful cancel!
	sockPath := filepath.Join(tmp, "cli.sock")
	serveCtx, cancelServe := context.WithCancel(ctx)
	doneCh := make(chan int, 1)
	go func() {
		var sOut, sErr bytes.Buffer
		doneCh <- runCLI(serveCtx, []string{
			"serve",
			"--data-dir", dataDir2,
			"--listen-socket", sockPath + ",0660",
		}, &sOut, &sErr)
	}()

	// Wait briefly for socket to appear, then gracefully stop
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, stErr := os.Lstat(sockPath); stErr == nil {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if _, stErr := os.Lstat(sockPath); stErr != nil {
		cancelServe()
		t.Fatalf("volo serve never created unix socket %s: %v", sockPath, stErr)
	}
	cancelServe()
	if exitCode := <-doneCh; exitCode != 0 {
		t.Fatalf("expected clean exit code 0 on graceful shutdown, got %d", exitCode)
	}
}

func writeMinimalLegacyZip(t *testing.T, path string) {
	t.Helper()
	payload := `{
		"options": [{"oId":"blogTitle","optionCategory":"preference","optionValue":"CLI Test Blog"}],
		"users": [{"oId":"default","userName":"admin","userEmail":"admin@example.com","userRole":"adminRole"}],
		"categories": [{"oId":"c1","categoryTitle":"Tech","categoryURI":"tech","categoryOrder":1}],
		"tags": [{"oId":"t1","tagTitle":"Go"}],
		"category_tag": [{"oId":"ct1","category_oId":"c1","tag_oId":"t1"}],
		"tag_articles": [{"oId":"ta1","article_oId":"a1","tag_oId":"t1"}],
		"articles": [{
			"oId":"a1","articleTitle":"Hello CLI","articleAbstract":"Sub","articleContent":"Body",
			"articleAuthorId":"default","articlePermalink":"/articles/2024/01/hello.html","articleStatus":0,"articleCreated":1704067200000
		}]
	}`
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("data.json")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write([]byte(payload))
	_ = zw.Close()
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}
