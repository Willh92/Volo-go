package theme

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Willh92/Volo-go/internal/content"
	"github.com/Willh92/Volo-go/internal/domain"
	"github.com/Willh92/Volo-go/internal/i18n"
)

func buildTestThemeZip(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("zip create %s: %v", name, err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatalf("zip write %s: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

func TestThemeSDK_TimeZone_OneArgTT_Schema_PageTemplates_And_ExportZip(t *testing.T) {
	cat, err := i18n.NewCatalog("zh-CN", "")
	if err != nil {
		t.Fatalf("NewCatalog: %v", err)
	}
	themesDir := t.TempDir()
	eng, err := NewEngine(cat, themesDir)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}

	// 1. LoadSiteLocation caching & fallback
	locTokyo := LoadSiteLocation("Asia/Tokyo")
	if locTokyo.String() != "Asia/Tokyo" {
		t.Fatalf("expected Asia/Tokyo, got %s", locTokyo.String())
	}
	locInvalid := LoadSiteLocation("Invalid/NoSuchZone")
	if locInvalid != time.UTC {
		t.Fatalf("expected invalid timezone fallback to UTC, got %v", locInvalid)
	}

	// 2. Install custom theme exercising:
	//    - 1-arg {{tt "greeting"}} bound to activeLocale
	//    - date formatting in Site.TimeZone
	//    - extended ConfigField (group, required, min, max, step, image)
	//    - custom PageTemplates + RenderWithFallback template selection
	manifestJSON := `{
  "id": "aurora-test",
  "name": {"en": "Aurora Test", "zh-CN": "极光测试"},
  "version": "1.2.0",
  "author": "Volo SDK",
  "description": {"en": "SDK regression theme", "zh-CN": "SDK 回归主题"},
  "accents": ["teal"],
  "supports_dark_mode": true,
  "supports_pjax": true,
  "page_templates": [
    {"value": "default", "label": {"en": "Default", "zh-CN": "默认"}},
    {"value": "landing", "label": {"en": "Landing", "zh-CN": "着陆页"}}
  ],
  "config_schema": [
    {
      "key": "hero_banner",
      "type": "image",
      "group": {"en": "Hero", "zh-CN": "首屏"},
      "label": {"en": "Hero Banner", "zh-CN": "首屏大图"},
      "required": true,
      "default": "/uploads/banner.webp"
    },
    {
      "key": "columns",
      "type": "number",
      "group": {"en": "Layout", "zh-CN": "布局"},
      "label": {"en": "Columns", "zh-CN": "列数"},
      "min": 1,
      "max": 4,
      "step": 1,
      "default": 2
    }
  ]
}`
	zipBytes := buildTestThemeZip(t, map[string]string{
		"theme.json":                  manifestJSON,
		"locales/en.json":             `{"greeting": "Hello Reader"}`,
		"locales/zh-CN.json":          `{"greeting": "你好读者"}`,
		"templates/page-landing.html": `{{define "content"}}<section class="tpl-landing" data-tt="{{tt "greeting"}}" data-dt="{{formatDateTime .StandalonePage.UpdatedAt}}">{{.StandalonePage.Title}}</section>{{end}}`,
		"assets/custom.css":           `body { --aurora: 1; }`,
	})

	installed, err := eng.InstallZip(bytes.NewReader(zipBytes), int64(len(zipBytes)), true)
	if err != nil {
		t.Fatalf("InstallZip: %v", err)
	}
	if installed.ID != "aurora-test" || len(installed.PageTemplates) != 2 || len(installed.ConfigSchema) != 2 {
		t.Fatalf("unexpected installed manifest: %+v", installed)
	}

	// Verify LocalizedTheme exposes Group, Required, Min/Max/Step, Image type, and LocalizedPageTemplates
	localized := eng.ListLocalized("zh-CN")
	var found *LocalizedTheme
	for i := range localized {
		if localized[i].ID == "aurora-test" {
			found = &localized[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("aurora-test missing from ListLocalized")
	}
	if len(found.LocalizedPageTemplates) != 2 || found.LocalizedPageTemplates[1].Value != "landing" || found.LocalizedPageTemplates[1].Label != "着陆页" {
		t.Fatalf("unexpected LocalizedPageTemplates: %+v", found.LocalizedPageTemplates)
	}
	if found.LocalizedSchema[0].Group != "首屏" || !found.LocalizedSchema[0].Required || found.LocalizedSchema[0].Type != "image" {
		t.Fatalf("unexpected schema[0]: %+v", found.LocalizedSchema[0])
	}
	if found.LocalizedSchema[1].Min == nil || *found.LocalizedSchema[1].Min != 1 || found.LocalizedSchema[1].Max == nil || *found.LocalizedSchema[1].Max != 4 {
		t.Fatalf("unexpected schema[1] min/max: %+v", found.LocalizedSchema[1])
	}

	// Also verify default builtin theme gets default page templates ("default", "wide", "minimal")
	for _, th := range localized {
		if th.ID == DefaultThemeID {
			if len(th.LocalizedPageTemplates) < 3 {
				t.Fatalf("expected builtin theme to expose default page_templates, got %+v", th.LocalizedPageTemplates)
			}
		}
	}

	// 3. Verify compiled template cache isolates (selected, activeLocale, siteTimeZone)
	// Fixed timestamp: 2026-06-01T00:00:00Z -> 2026-06-01 09:00 in Asia/Tokyo vs 2026-05-31 20:00 in America/New_York
	fixedUTC := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC).UnixMilli()

	// Note: home.html uses .Stats / standard PageContext fields, let's test dateTime via Owner or RecentArticles, or custom template!
	// Wait: ProbeTS isn't a field on PageContext; RecentComments[0].CreatedAt IS a field on PageContext! Let's update home.html to format index .RecentComments 0 .CreatedAt or .Year.
	zipBytes2 := buildTestThemeZip(t, map[string]string{
		"theme.json":                  manifestJSON,
		"locales/en.json":             `{"greeting": "Hello Reader"}`,
		"locales/zh-CN.json":          `{"greeting": "你好读者"}`,
		"templates/page-landing.html": `{{define "content"}}<section class="tpl-landing" data-tt="{{tt "greeting"}}" data-dt="{{formatDateTime .StandalonePage.UpdatedAt}}">{{.StandalonePage.Title}}</section>{{end}}`,
		"assets/custom.css":           `body { --aurora: 1; }`,
	})
	if _, err := eng.InstallZip(bytes.NewReader(zipBytes2), int64(len(zipBytes2)), true); err != nil {
		t.Fatalf("InstallZip update: %v", err)
	}

	// Render page-landing.html with Tokyo + zh-CN vs New_York + en-US
	for _, tc := range []struct {
		locale string
		tz     string
		wantTT string
		wantDT string
	}{
		{locale: "zh-CN", tz: "Asia/Tokyo", wantTT: `data-tt="你好读者"`, wantDT: `data-dt="2026-06-01 09:00"`},
		{locale: "en-US", tz: "America/New_York", wantTT: `data-tt="Hello Reader"`, wantDT: `data-dt="2026-05-31 20:00"`},
	} {
		rec := httptest.NewRecorder()
		err := eng.RenderWithFallback(rec, http.StatusOK, "aurora-test", []string{"page-missing-slug.html", "page-landing.html", "page.html"}, newLandingPageContext(eng, tc.locale, tc.tz, fixedUTC))
		if err != nil {
			t.Fatalf("RenderWithFallback(%s, %s): %v", tc.locale, tc.tz, err)
		}
		out := rec.Body.String()
		if !strings.Contains(out, tc.wantTT) {
			t.Fatalf("locale %s: expected %s in rendered output:\n%s", tc.locale, tc.wantTT, out)
		}
		if !strings.Contains(out, tc.wantDT) {
			t.Fatalf("tz %s: expected %s in rendered output:\n%s", tc.tz, tc.wantDT, out)
		}
	}

	// 4. ExportZip for builtin volo-studio (inflating embedded assets_gz/*.gz) AND external theme
	var studioZip bytes.Buffer
	if err := eng.ExportZip(&studioZip, DefaultThemeID); err != nil {
		t.Fatalf("ExportZip(%s): %v", DefaultThemeID, err)
	}
	zrStudio, err := zip.NewReader(bytes.NewReader(studioZip.Bytes()), int64(studioZip.Len()))
	if err != nil {
		t.Fatalf("zip.NewReader(studio): %v", err)
	}
	hasManifest := false
	hasInflatedCSS := false
	for _, f := range zrStudio.File {
		if f.Name == "theme.json" {
			hasManifest = true
		}
		if f.Name == "assets/volo-studio.css" {
			hasInflatedCSS = true
		}
		if strings.HasSuffix(f.Name, ".gz") {
			t.Fatalf("expected embedded .gz asset to be inflated on ExportZip, got %s", f.Name)
		}
	}
	if !hasManifest || !hasInflatedCSS {
		t.Fatalf("ExportZip(volo-studio) missing theme.json (%v) or assets/volo-studio.css (%v)", hasManifest, hasInflatedCSS)
	}

	var extZip bytes.Buffer
	if err := eng.ExportZip(&extZip, "aurora-test"); err != nil {
		t.Fatalf("ExportZip(aurora-test): %v", err)
	}
	zrExt, err := zip.NewReader(bytes.NewReader(extZip.Bytes()), int64(extZip.Len()))
	if err != nil {
		t.Fatalf("zip.NewReader(aurora-test): %v", err)
	}
	var exportedManifest Manifest
	for _, f := range zrExt.File {
		if f.Name == "theme.json" {
			rc, _ := f.Open()
			_ = json.NewDecoder(rc).Decode(&exportedManifest)
			_ = rc.Close()
		}
	}
	if exportedManifest.ID != "aurora-test" {
		t.Fatalf("expected exported manifest id aurora-test, got %q", exportedManifest.ID)
	}
}

func newLandingPageContext(eng *Engine, locale, tz string, updatedAt int64) PageContext {
	return PageContext{
		ThemeID:          "aurora-test",
		Locale:           locale,
		TextDir:          "ltr",
		CurrentPath:      "/hello",
		IsStandalonePage: true,
		Site: domain.SiteSettings{
			Title:    "Aurora Site",
			TimeZone: tz,
		},
		ThemeConfig: eng.ResolveThemeConfig("aurora-test", nil),
		StandalonePage: content.PublicPageView{
			ID:        "p_1",
			Title:     "Aurora Landing",
			Slug:      "hello",
			Template:  "landing",
			UpdatedAt: updatedAt,
		},
	}
}
