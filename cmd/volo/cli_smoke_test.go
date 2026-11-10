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

	"github.com/Willh92/Volo-go/internal/db/sqlx"
)

func buildMinimalLegacyZip(t *testing.T, zipPath string) {
	t.Helper()
	payload := map[string]any{
		"options": []map[string]any{
			{"oId": "blogTitle", "optionCategory": "preference", "optionValue": "CLI Imported Blog"},
			{"oId": "blogSubtitle", "optionCategory": "preference", "optionValue": "Clean-slate ETL"},
		},
		"users": []map[string]any{
			{
				"oId":              "u_cli_1",
				"userName":         "cliadmin",
				"userEmail":        "cliadmin@example.com",
				"userPasswordHash": "pbkdf2$sha256$120000$c2FsdHNhbHRzYWx0$aGFzaGhhc2hoYXNoaGFzaGhhc2hoYXNo",
				"userRole":         "adminRole",
			},
		},
		"articles": []map[string]any{
			{
				"oId":                "art_cli_1",
				"articleTitle":       "Hello From Legacy ETL",
				"articleContent":     "Streaming legacy import via CLI.",
				"articleAuthorId":    "u_cli_1",
				"articlePermalink":   "/articles/2024/01/10/1704844800000.html",
				"articleStatus":      0,
				"articleCommentable": true,
				"articleCreated":     1704844800000,
				"articleUpdated":     1704844800000,
			},
		},
	}
	rawJSON, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	w, err := zw.Create("data.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(rawJSON); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestM6_CLIUnixSocketHealthcheckAndAdminOps(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()

	dataDir := filepath.Join(tmp, "data1")
	uploadDir := filepath.Join(tmp, "uploads1")
	backupDir := filepath.Join(tmp, "backups")
	commonArgs := []string{
		"--data-dir", dataDir,
		"--upload-dir", uploadDir,
		"--backup-dir", backupDir,
		"--secret-key", "0123456789abcdef0123456789abcdef-cli-secret",
	}

	execCLI := func(args ...string) (int, string, string) {
		var outBuf, errBuf bytes.Buffer
		code := runCLI(ctx, args, &outBuf, &errBuf)
		return code, outBuf.String(), errBuf.String()
	}

	// 1. volo version & help
	code, out, errStr := execCLI("version")
	if code != 0 || !strings.Contains(out, "pure-go static") {
		t.Fatalf("volo version failed: code=%d out=%q err=%q", code, out, errStr)
	}

	// 2. volo doctor --json
	code, out, errStr = execCLI(append([]string{"doctor", "--json"}, commonArgs...)...)
	if code != 0 {
		t.Fatalf("volo doctor --json failed: code=%d out=%q err=%q", code, out, errStr)
	}
	var docRep struct {
		Healthy bool `json:"healthy"`
	}
	if err := json.Unmarshal([]byte(out), &docRep); err != nil || !docRep.Healthy {
		t.Fatalf("expected healthy doctor JSON report, got err=%v out=%s", err, out)
	}

	// 3. volo import-legacy (--dry-run then live import)
	legacyZip := filepath.Join(tmp, "legacy-cli.zip")
	buildMinimalLegacyZip(t, legacyZip)

	code, out, errStr = execCLI(append([]string{"import-legacy", legacyZip, "--dry-run"}, commonArgs...)...)
	if code != 0 || !strings.Contains(out, `"dry_run": true`) {
		t.Fatalf("volo import-legacy --dry-run failed: code=%d out=%q err=%q", code, out, errStr)
	}
	code, out, errStr = execCLI(append([]string{"import-legacy", legacyZip}, commonArgs...)...)
	if code != 0 || !strings.Contains(out, `"articles": 1`) {
		t.Fatalf("volo import-legacy failed: code=%d out=%q err=%q", code, out, errStr)
	}

	// 4. volo admin reset-password & disable-2fa
	code, out, errStr = execCLI(append([]string{"admin", "reset-password", "--user", "cliadmin", "--password", "NewStrong!Pass#99"}, commonArgs...)...)
	if code != 0 || !strings.Contains(out, "Password reset complete") {
		t.Fatalf("volo admin reset-password failed: code=%d out=%q err=%q", code, out, errStr)
	}
	code, out, errStr = execCLI(append([]string{"admin", "disable-2fa", "--user", "cliadmin@example.com"}, commonArgs...)...)
	if code != 0 || !strings.Contains(out, "2FA emergency reset complete") {
		t.Fatalf("volo admin disable-2fa failed: code=%d out=%q err=%q", code, out, errStr)
	}

	// 5. volo backup create + restore (--dry-run then restore into clean data2 dir)
	if err := os.MkdirAll(uploadDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(uploadDir, "note.txt"), []byte("media-payload"), 0o640); err != nil {
		t.Fatal(err)
	}
	nativeZip := filepath.Join(backupDir, "cli-native.zip")
	code, out, errStr = execCLI(append([]string{"backup", "create", "--output", nativeZip}, commonArgs...)...)
	if code != 0 || !strings.Contains(out, "Backup created:") {
		t.Fatalf("volo backup create failed: code=%d out=%q err=%q", code, out, errStr)
	}

	dataDir2 := filepath.Join(tmp, "data2")
	uploadDir2 := filepath.Join(tmp, "uploads2")
	commonArgs2 := []string{
		"--data-dir", dataDir2,
		"--upload-dir", uploadDir2,
		"--backup-dir", backupDir,
		"--secret-key", "0123456789abcdef0123456789abcdef-cli-secret",
	}
	code, out, errStr = execCLI(append([]string{"backup", "restore", nativeZip, "--dry-run"}, commonArgs2...)...)
	if code != 0 || !strings.Contains(out, `"dry_run": true`) {
		t.Fatalf("volo backup restore --dry-run failed: code=%d out=%q err=%q", code, out, errStr)
	}
	code, out, errStr = execCLI(append([]string{"backup", "restore", nativeZip}, commonArgs2...)...)
	if code != 0 || !strings.Contains(out, `"media_files": 1`) {
		t.Fatalf("volo backup restore failed: code=%d out=%q err=%q", code, out, errStr)
	}
	restoredMedia, err := os.ReadFile(filepath.Join(uploadDir2, "note.txt"))
	if err != nil || string(restoredMedia) != "media-payload" {
		t.Fatalf("expected restored media file, got err=%v content=%q", err, string(restoredMedia))
	}

	// 6. volo serve over Unix Domain Socket + volo healthcheck + graceful context shutdown
	sockPath := filepath.Join(tmp, "volo.sock")
	serveCtx, cancelServe := context.WithCancel(context.Background())
	serveDone := make(chan int, 1)
	var serveOut, serveErr bytes.Buffer
	go func() {
		serveArgs := append([]string{
			"serve",
			"--listen-addr", "",
			"--listen-socket", sockPath + ",0660",
			"--trusted-proxies", "unix",
		}, commonArgs2...)
		serveDone <- runCLI(serveCtx, serveArgs, &serveOut, &serveErr)
	}()

	healthy := false
	for i := 0; i < 40; i++ {
		var hcOut, hcErr bytes.Buffer
		hcCode := runCLI(ctx, append([]string{
			"healthcheck",
			"--listen-addr", "",
			"--listen-socket", sockPath + ",0660",
		}, commonArgs2...), &hcOut, &hcErr)
		if hcCode == 0 && strings.Contains(hcOut.String(), `"status":"ok"`) {
			healthy = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancelServe()
	exitCode := <-serveDone
	if !healthy {
		t.Fatalf("volo healthcheck over unix socket never succeeded; serve stderr=%s", serveErr.String())
	}
	if exitCode != 0 {
		t.Fatalf("volo serve graceful shutdown exited %d: %s", exitCode, serveErr.String())
	}
}

func TestRound3_CLIResetPasswordAndPasswdRevokeAPITokensAndSessions(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	dataDir := filepath.Join(tmp, "data")
	uploadDir := filepath.Join(tmp, "uploads")
	backupDir := filepath.Join(tmp, "backups")
	commonArgs := []string{
		"--data-dir", dataDir,
		"--upload-dir", uploadDir,
		"--backup-dir", backupDir,
		"--secret-key", "0123456789abcdef0123456789abcdef-cli-r3-secret",
	}

	execCLI := func(args ...string) (int, string, string) {
		var outBuf, errBuf bytes.Buffer
		code := runCLI(ctx, args, &outBuf, &errBuf)
		return code, outBuf.String(), errBuf.String()
	}

	legacyZip := filepath.Join(tmp, "legacy.zip")
	buildMinimalLegacyZip(t, legacyZip)
	if code, _, errStr := execCLI(append([]string{"import-legacy", legacyZip}, commonArgs...)...); code != 0 {
		t.Fatalf("import-legacy failed: %d %s", code, errStr)
	}

	dbPath := filepath.Join(dataDir, "volo.db")
	seedCreds := func(tag string) string {
		store, err := sqlx.Open(ctx, sqlx.Options{Driver: "sqlite", DSN: dbPath})
		if err != nil {
			t.Fatalf("sqlx.Open: %v", err)
		}
		defer store.Close()

		var uid string
		if err := store.QueryRowContext(ctx, `SELECT id FROM users WHERE username = 'cliadmin'`).Scan(&uid); err != nil {
			t.Fatalf("lookup cliadmin: %v", err)
		}
		now := time.Now().UnixMilli()
		if _, err := store.ExecContext(ctx, `INSERT INTO user_sessions (
			id, user_id, token_hash, ip_masked, geo_region, user_agent, totp_verified_until, expires_at, last_seen_at, created_at
		) VALUES (?, ?, ?, '127.0.0.0/24', '', 'cli-test', 0, ?, ?, ?)`,
			"sess_"+tag, uid, "hash_sess_"+tag, now+86400000, now, now); err != nil {
			t.Fatalf("insert user_session: %v", err)
		}
		if _, err := store.ExecContext(ctx, `INSERT INTO api_tokens (
			id, user_id, name, token_prefix, token_hash, scopes_json, last_used_at, expires_at, created_at
		) VALUES (?, ?, 'CLI Token', 'volo_pat_', ?, '["articles:write"]', 0, 0, ?)`,
			"pat_"+tag, uid, "hash_pat_"+tag, now); err != nil {
			t.Fatalf("insert api_token: %v", err)
		}
		// Seed a bystander token for another user to verify scoping
		_, _ = store.ExecContext(ctx, `INSERT INTO api_tokens (
			id, user_id, name, token_prefix, token_hash, scopes_json, last_used_at, expires_at, created_at
		) VALUES ('pat_other', 'u_other_99', 'Other Token', 'volo_pat_', 'hash_pat_other', '["articles:read"]', 0, 0, ?)`, now)
		return uid
	}

	countCreds := func(uid string) (int, int, int) {
		store, err := sqlx.Open(ctx, sqlx.Options{Driver: "sqlite", DSN: dbPath})
		if err != nil {
			t.Fatalf("sqlx.Open: %v", err)
		}
		defer store.Close()
		var sessCnt, patCnt, otherPatCnt int
		_ = store.QueryRowContext(ctx, `SELECT COUNT(*) FROM user_sessions WHERE user_id = ?`, uid).Scan(&sessCnt)
		_ = store.QueryRowContext(ctx, `SELECT COUNT(*) FROM api_tokens WHERE user_id = ?`, uid).Scan(&patCnt)
		_ = store.QueryRowContext(ctx, `SELECT COUNT(*) FROM api_tokens WHERE user_id = 'u_other_99'`).Scan(&otherPatCnt)
		return sessCnt, patCnt, otherPatCnt
	}

	uid := seedCreds("r1")
	code, out, errStr := execCLI(append([]string{"admin", "reset-password", "--user", "cliadmin", "--password", "R3!StrongPass#2026"}, commonArgs...)...)
	if code != 0 || !strings.Contains(out, "active sessions and API tokens revoked") {
		t.Fatalf("admin reset-password failed: code=%d out=%q err=%q", code, out, errStr)
	}
	sessCnt, patCnt, otherPatCnt := countCreds(uid)
	if sessCnt != 0 || patCnt != 0 || otherPatCnt != 1 {
		t.Fatalf("expected sess=0 pat=0 otherPat=1 after admin reset-password, got sess=%d pat=%d otherPat=%d", sessCnt, patCnt, otherPatCnt)
	}

	// Verify top-level `volo passwd` alias also revokes sessions + api_tokens and generates a strong password when --password is omitted
	_ = seedCreds("r2")
	code, out, errStr = execCLI(append([]string{"passwd", "--user", "cliadmin@example.com"}, commonArgs...)...)
	if code != 0 || !strings.Contains(out, "Generated temporary password:") {
		t.Fatalf("volo passwd alias failed: code=%d out=%q err=%q", code, out, errStr)
	}
	sessCnt, patCnt, otherPatCnt = countCreds(uid)
	if sessCnt != 0 || patCnt != 0 || otherPatCnt != 1 {
		t.Fatalf("expected sess=0 pat=0 otherPat=1 after volo passwd, got sess=%d pat=%d otherPat=%d", sessCnt, patCnt, otherPatCnt)
	}
}
