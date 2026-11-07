package i18n

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

func TestAdminSPALocaleSymmetryAndSourceKeyCoverage(t *testing.T) {
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}

	zhRaw, err := os.ReadFile(filepath.Join(repoRoot, "web", "admin", "src", "locales", "zh-CN.json"))
	if err != nil {
		t.Fatalf("read web/admin zh-CN.json: %v", err)
	}
	enRaw, err := os.ReadFile(filepath.Join(repoRoot, "web", "admin", "src", "locales", "en-US.json"))
	if err != nil {
		t.Fatalf("read web/admin en-US.json: %v", err)
	}

	var zhMap, enMap map[string]string
	if err := json.Unmarshal(zhRaw, &zhMap); err != nil {
		t.Fatalf("unmarshal admin zh-CN.json: %v", err)
	}
	if err := json.Unmarshal(enRaw, &enMap); err != nil {
		t.Fatalf("unmarshal admin en-US.json: %v", err)
	}

	if len(zhMap) != len(enMap) {
		t.Fatalf("admin SPA locale count mismatch: zh-CN=%d vs en-US=%d", len(zhMap), len(enMap))
	}

	for k, zv := range zhMap {
		ev, ok := enMap[k]
		if !ok {
			t.Fatalf("admin SPA key %q present in zh-CN.json but missing in en-US.json", k)
		}
		if strings.TrimSpace(zv) == "" || strings.TrimSpace(ev) == "" {
			t.Fatalf("admin SPA key %q has blank value (zh=%q, en=%q)", k, zv, ev)
		}
		zhVars := placeholderRe.FindAllString(zv, -1)
		enVars := placeholderRe.FindAllString(ev, -1)
		sort.Strings(zhVars)
		sort.Strings(enVars)
		if strings.Join(zhVars, ",") != strings.Join(enVars, ",") {
			t.Fatalf("admin SPA placeholder mismatch on key %q: zh=%v vs en=%v", k, zhVars, enVars)
		}
	}

	// Verify every literal t("...") / tFor(..., "...") / labelKey / titleKey in web/admin/src exists in both catalogs
	srcDir := filepath.Join(repoRoot, "web", "admin", "src")
	reT := regexp.MustCompile(`\bt\(\s*["']([a-zA-Z0-9_.-]+)["']`)
	reTFor := regexp.MustCompile(`\btFor\([^,]+,\s*["']([a-zA-Z0-9_.-]+)["']`)
	reProp := regexp.MustCompile(`\b(?:labelKey|titleKey|descKey|groupKey)\s*:\s*["']([a-zA-Z0-9_.-]+)["']`)

	err = filepath.WalkDir(srcDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() {
			return walkErr
		}
		ext := filepath.Ext(path)
		if ext != ".ts" && ext != ".tsx" {
			return nil
		}
		raw, rErr := os.ReadFile(path)
		if rErr != nil {
			return rErr
		}
		txt := string(raw)
		for _, re := range []*regexp.Regexp{reT, reTFor, reProp} {
			for _, m := range re.FindAllStringSubmatch(txt, -1) {
				key := m[1]
				if _, ok := zhMap[key]; !ok {
					t.Errorf("%s references missing i18n key %q in zh-CN.json", path, key)
				}
				if _, ok := enMap[key]; !ok {
					t.Errorf("%s references missing i18n key %q in en-US.json", path, key)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk web/admin/src: %v", err)
	}
}

func TestLanguagePackExtensionLifecycleAndZhTWBundle(t *testing.T) {
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}

	// 1. Out-of-the-box Traditional Chinese pack (`locales/zh-TW.json`) preserves regional tag `zh-TW`
	shippedCat, err := NewCatalog("zh-CN", filepath.Join(repoRoot, "locales"))
	if err != nil {
		t.Fatalf("NewCatalog with shipped ./locales: %v", err)
	}
	if !shippedCat.Has("zh-TW") {
		t.Fatalf("expected shipped locales/zh-TW.json to register zh-TW")
	}
	if got := shippedCat.Match("zh-TW", "zh-CN"); got != "zh-TW" {
		t.Fatalf("expected Match(zh-TW) -> zh-TW, got %q", got)
	}
	if got := shippedCat.T("zh-TW", "nav.home"); got != "首頁" {
		t.Fatalf("expected zh-TW nav.home = 首頁, got %q", got)
	}
	customTW := shippedCat.CustomMap("zh-TW")
	if customTW["admin.nav.locales"] != "語言包擴充" {
		t.Fatalf("expected zh-TW CustomMap admin.nav.locales = 語言包擴充, got %q", customTW["admin.nav.locales"])
	}

	// 2. Hot-install single JSON pack, ZIP pack, export JSON, and delete external pack
	tmpDir := t.TempDir()
	cat, err := NewCatalog("zh-CN", tmpDir)
	if err != nil {
		t.Fatal(err)
	}

	jaRaw := []byte(`{
		"meta.code": "ja-JP",
		"meta.name": "日本語",
		"meta.dir": "ltr",
		"meta.author": "Volo QA",
		"meta.version": "1.2.0",
		"nav.home": "ホーム",
		"admin.nav.locales": "言語パック拡張"
	}`)
	jaSum, err := cat.InstallPackJSON("ja-JP", jaRaw, true)
	if err != nil {
		t.Fatalf("InstallPackJSON(ja-JP): %v", err)
	}
	if jaSum.Code != "ja-JP" || jaSum.Builtin || jaSum.CustomKeyCount == 0 {
		t.Fatalf("unexpected ja-JP pack summary: %+v", jaSum)
	}
	if got := cat.T("ja-JP", "nav.home"); got != "ホーム" {
		t.Fatalf("expected ja-JP nav.home = ホーム, got %q", got)
	}

	var zipBuf bytes.Buffer
	zw := zip.NewWriter(&zipBuf)
	zf, err := zw.Create("locales/ko-KR.json")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = zf.Write([]byte(`{"meta.code":"ko-KR","meta.name":"한국어","nav.home":"홈"}`))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	installedZip, err := cat.InstallPackZip(bytes.NewReader(zipBuf.Bytes()), int64(zipBuf.Len()), true)
	if err != nil || len(installedZip) != 1 || installedZip[0].Code != "ko-KR" {
		t.Fatalf("InstallPackZip(ko-KR): installed=%+v err=%v", installedZip, err)
	}
	if got := cat.T("ko-KR", "nav.home"); got != "홈" {
		t.Fatalf("expected ko-KR nav.home = 홈, got %q", got)
	}

	exported, err := cat.ExportPackJSON("ja-JP")
	if err != nil || !bytes.Contains(exported, []byte(`"ホーム"`)) {
		t.Fatalf("ExportPackJSON(ja-JP) failed: err=%v body=%s", err, string(exported))
	}

	// Built-in without external override cannot be deleted
	if err := cat.DeleteExternalPack("zh-CN"); err == nil {
		t.Fatalf("expected DeleteExternalPack(zh-CN) without external override to fail")
	}

	// External pack deletes cleanly
	if err := cat.DeleteExternalPack("ja-JP"); err != nil {
		t.Fatalf("DeleteExternalPack(ja-JP): %v", err)
	}
	if cat.Has("ja-JP") {
		t.Fatalf("expected ja-JP to be removed after DeleteExternalPack")
	}
}
