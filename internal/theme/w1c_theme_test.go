package theme

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Willh92/Volo-go/internal/content"
	"github.com/Willh92/Volo-go/internal/domain"
	"github.com/Willh92/Volo-go/internal/i18n"
	"github.com/Willh92/Volo-go/internal/visibility"
)

func TestW1C_StandaloneFramelessErrorPage(t *testing.T) {
	localesDir := filepath.Join("..", "..", "locales")
	themesDir := filepath.Join("..", "..", "themes")

	cat, err := i18n.NewCatalog("zh-CN", localesDir)
	if err != nil {
		t.Fatalf("NewCatalog: %v", err)
	}
	eng, err := NewEngine(cat, themesDir)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}

	localeCases := []struct {
		locale      string
		wantTitle   string
		wantHint1   string
		wantHint2   string
		wantPrimary string
		wantSearch  string
	}{
		{
			locale:      "zh-CN",
			wantTitle:   "这一页没有找到",
			wantHint1:   "链接可能已经变更，也可能还没有发布。",
			wantHint2:   "试试搜索关键词，找找你想读的那篇文章。",
			wantPrimary: "回到首页",
			wantSearch:  "搜索文章",
		},
		{
			locale:      "zh-TW",
			wantTitle:   "找不到這個頁面",
			wantHint1:   "連結可能已經變更，也可能還沒有發布。",
			wantHint2:   "試試搜尋關鍵詞，找找你想讀的那篇文章。",
			wantPrimary: "回到首頁",
			wantSearch:  "搜尋文章",
		},
		{
			locale:      "en-US",
			wantTitle:   "This page could not be found",
			wantHint1:   "The link may have changed, or the post has not been published yet.",
			wantHint2:   "Try searching by keyword to find the post you were looking for.",
			wantPrimary: "Back to Home",
			wantSearch:  "Search Posts",
		},
		{
			locale:      "ja-JP",
			wantTitle:   "ページが見つかりません",
			wantHint1:   "URL が変更されたか、まだ公開されていない記事の可能性があります。",
			wantHint2:   "キーワードで検索して、お探しの記事を見つけてみてください。",
			wantPrimary: "ホームへ戻る",
			wantSearch:  "記事を検索",
		},
	}

	for _, themeID := range []string{DefaultThemeID, "volo-journal"} {
		for _, tc := range localeCases {
			rec := httptest.NewRecorder()
			err := eng.RenderWithFallback(rec, http.StatusNotFound, themeID, []string{"error.html"}, PageContext{
				Locale:       tc.locale,
				StatusCode:   http.StatusNotFound,
				ErrorMessage: "404 Not Found",
				Site: domain.SiteSettings{
					Title: "Volo Craft",
				},
				Appearance: domain.AppearanceSettings{
					Accent:      "ocean",
					DefaultMode: "auto",
				},
				Reading: domain.ReadingSettings{
					EnableInstantSearch: true,
				},
			})
			if err != nil {
				t.Fatalf("RenderWithFallback(%s, %s): %v", themeID, tc.locale, err)
			}
			body := rec.Body.String()

			for _, forbidden := range []string{
				`<header class="site-header"`,
				`<header class="studio-header"`,
				`<footer class="site-footer"`,
				`<footer class="studio-footer"`,
				`id="studio-main"`,
			} {
				if strings.Contains(body, forbidden) {
					t.Fatalf("expected standalone error.html (%s/%s) to omit layout chrome %q", themeID, tc.locale, forbidden)
				}
			}

			for _, want := range []string{
				`<!doctype html>`,
				`data-accent="ocean"`,
				`volo_theme_mode`,
				`class="err-card"`,
				`HTTP 404`,
				tc.wantTitle,
				tc.wantHint1,
				tc.wantHint2,
				tc.wantPrimary,
				tc.wantSearch,
				`id="volo-err-search-modal"`,
				`<polygon points="64,16 78,4 114,4 128,16 114,28 78,28"`,
				`Powered by <strong>Volo</strong>`,
			} {
				if !strings.Contains(body, want) {
					t.Fatalf("expected standalone error.html (%s/%s) to contain %q, got:\n%s", themeID, tc.locale, want, body)
				}
			}
		}
	}

	// Verify EnableInstantSearch=false hides search button/modal, and custom ErrorMessage overrides headline.
	recCustom := httptest.NewRecorder()
	if err := eng.RenderWithFallback(recCustom, http.StatusForbidden, DefaultThemeID, []string{"error.html"}, PageContext{
		Locale:       "en-US",
		StatusCode:   http.StatusForbidden,
		ErrorMessage: "Members-only essay",
		Site:         domain.SiteSettings{Title: "Volo Craft"},
		Reading:      domain.ReadingSettings{EnableInstantSearch: false},
	}); err != nil {
		t.Fatalf("RenderWithFallback(403 custom): %v", err)
	}
	outCustom := recCustom.Body.String()
	if !strings.Contains(outCustom, "Members-only essay") || !strings.Contains(outCustom, "HTTP 403") {
		t.Fatalf("expected custom 403 ErrorMessage and HTTP 403 eyebrow, got:\n%s", outCustom)
	}
	if strings.Contains(outCustom, `id="volo-err-search-modal"`) || strings.Contains(outCustom, `id="volo-err-search-open"`) {
		t.Fatalf("expected search modal and trigger to be omitted when EnableInstantSearch is false")
	}

	// Verify an external theme with an empty templates/error.html falls back cleanly to built-in standalone error.html.
	engIsolated, err := NewEngine(cat, t.TempDir())
	if err != nil {
		t.Fatalf("NewEngine(temp): %v", err)
	}
	zb := buildTestThemeZip(t, map[string]string{
		"theme.json":           `{"id":"empty-err-skin","name":{"en":"Empty Err"},"version":"1.0.0","author":"Volo"}`,
		"templates/error.html": "   \n\t  ",
	})
	if _, err := engIsolated.InstallZip(bytes.NewReader(zb), int64(len(zb)), true); err != nil {
		t.Fatalf("InstallZip(empty-err-skin): %v", err)
	}
	recFallback := httptest.NewRecorder()
	if err := engIsolated.RenderWithFallback(recFallback, http.StatusTooManyRequests, "empty-err-skin", []string{"error.html"}, PageContext{
		Locale:     "zh-CN",
		StatusCode: http.StatusTooManyRequests,
		Site:       domain.SiteSettings{Title: "Volo"},
	}); err != nil {
		t.Fatalf("RenderWithFallback(empty-err-skin): %v", err)
	}
	if !strings.Contains(recFallback.Body.String(), "访问频率稍微太快了") || strings.Contains(recFallback.Body.String(), `<header class="studio-header"`) {
		t.Fatalf("expected empty error.html override to fall back to standalone built-in error.html")
	}
}

func TestW1C_ReadingListStyle_And_NoHardcodedPersonalSlug(t *testing.T) {
	cat, err := i18n.NewCatalog("en-US", "")
	if err != nil {
		t.Fatalf("NewCatalog: %v", err)
	}
	eng, err := NewEngine(cat, filepath.Join("..", "..", "themes"))
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}

	cards := []visibility.PublicArticleCard{
		{
			ID:       "a_repo",
			Title:    "Open Source Notes",
			Slug:     "my-github-repos",
			URL:      "/posts/my-github-repos",
			Summary:  "Short summary excerpt",
			FullHTML: `<p>Full rendered <strong>article body</strong></p>`,
		},
	}

	for _, themeID := range []string{DefaultThemeID, "volo-journal"} {
		// 1. summary
		recSum := httptest.NewRecorder()
		if err := eng.Render(recSum, http.StatusOK, themeID, "index.html", PageContext{
			Locale:       "en-US",
			CurrentPath:  "/",
			IsHome:       true,
			Reading:      domain.ReadingSettings{ListStyle: "summary"},
			ArticleCards: cards,
		}); err != nil {
			t.Fatalf("Render(%s, summary): %v", themeID, err)
		}
		outSum := recSum.Body.String()
		if !strings.Contains(outSum, "Short summary excerpt") || strings.Contains(outSum, "studio-card-full") {
			t.Fatalf("expected %s summary list_style to render Summary and omit studio-card-full", themeID)
		}
		if strings.Contains(outSum, "/images/github_repo.jpg") {
			t.Fatalf("expected %s not to inject hardcoded /images/github_repo.jpg for my-github-repos slug", themeID)
		}

		// 2. title_only
		recTitle := httptest.NewRecorder()
		if err := eng.Render(recTitle, http.StatusOK, themeID, "index.html", PageContext{
			Locale:       "en-US",
			CurrentPath:  "/",
			IsHome:       true,
			Reading:      domain.ReadingSettings{ListStyle: "title_only"},
			ArticleCards: cards,
		}); err != nil {
			t.Fatalf("Render(%s, title_only): %v", themeID, err)
		}
		outTitle := recTitle.Body.String()
		if strings.Contains(outTitle, "Short summary excerpt") || strings.Contains(outTitle, "studio-card-full") {
			t.Fatalf("expected %s title_only list_style to omit both abstract and full HTML", themeID)
		}

		// 3. full
		recFull := httptest.NewRecorder()
		if err := eng.Render(recFull, http.StatusOK, themeID, "index.html", PageContext{
			Locale:       "en-US",
			CurrentPath:  "/",
			IsHome:       true,
			Reading:      domain.ReadingSettings{ListStyle: "full"},
			ArticleCards: cards,
		}); err != nil {
			t.Fatalf("Render(%s, full): %v", themeID, err)
		}
		outFull := recFull.Body.String()
		if !strings.Contains(outFull, `<div class="volo-prose studio-card-full"><p>Full rendered <strong>article body</strong></p></div>`) {
			t.Fatalf("expected %s full list_style to render unescaped FullHTML inside .studio-card-full, got:\n%s", themeID, outFull)
		}
	}
}

func TestW1C_SecretConfigField_RedactionAndMerge(t *testing.T) {
	cat, err := i18n.NewCatalog("en-US", "")
	if err != nil {
		t.Fatalf("NewCatalog: %v", err)
	}
	eng, err := NewEngine(cat, t.TempDir())
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}

	manifest := `{
		"id": "secret-cfg-theme",
		"name": {"en": "Secret Config Theme"},
		"version": "1.0.0",
		"author": "Volo",
		"config_schema": [
			{"key": "headline", "type": "text", "label": {"en": "Headline"}, "default": "Hello"},
			{"key": "webhook_token", "type": "secret", "label": {"en": "Webhook Token"}, "default": "default-secret-999"}
		]
	}`
	zb := buildTestThemeZip(t, map[string]string{
		"theme.json":           manifest,
		"templates/index.html": `{{define "content"}}<div>{{index .ThemeConfig "headline"}}</div>{{end}}`,
	})
	if _, err := eng.InstallZip(bytes.NewReader(zb), int64(len(zb)), true); err != nil {
		t.Fatalf("InstallZip(secret-cfg-theme): %v", err)
	}

	// List() and ListLocalized() must redact secret defaults.
	for _, m := range eng.List() {
		if m.ID != "secret-cfg-theme" {
			continue
		}
		for _, f := range m.ConfigSchema {
			if f.Key == "webhook_token" && f.Default != SecretMask {
				t.Fatalf("expected List() to mask secret default with %q, got %v", SecretMask, f.Default)
			}
		}
	}
	for _, lm := range eng.ListLocalized("en-US") {
		if lm.ID != "secret-cfg-theme" {
			continue
		}
		for _, lf := range lm.LocalizedSchema {
			if lf.Key == "webhook_token" && lf.Default != SecretMask {
				t.Fatalf("expected ListLocalized() to mask secret default with %q, got %v", SecretMask, lf.Default)
			}
		}
	}

	// ResolveThemeConfig must still provide the real unredacted secret at runtime.
	rtDefault := eng.ResolveThemeConfig("secret-cfg-theme", nil)
	if rtDefault["webhook_token"] != "default-secret-999" {
		t.Fatalf("expected ResolveThemeConfig default secret 'default-secret-999', got %v", rtDefault["webhook_token"])
	}

	saved := map[string]map[string]any{
		"secret-cfg-theme": {
			"headline":      "Custom Title",
			"webhook_token": "prod-token-xyz",
		},
	}
	redactedAll := eng.RedactAllThemeConfigs(saved)
	if redactedAll["secret-cfg-theme"]["webhook_token"] != SecretMask {
		t.Fatalf("expected RedactAllThemeConfigs to mask secret, got %v", redactedAll["secret-cfg-theme"]["webhook_token"])
	}
	if saved["secret-cfg-theme"]["webhook_token"] != "prod-token-xyz" {
		t.Fatalf("expected RedactAllThemeConfigs not to mutate caller map")
	}

	// MergeThemeConfigSecrets preserves existing secret on SecretMask, updates on new string, clears on empty string.
	mergedKeep := eng.MergeThemeConfigSecrets("secret-cfg-theme", map[string]any{
		"headline":      "Updated Title",
		"webhook_token": SecretMask,
	}, saved["secret-cfg-theme"])
	if mergedKeep["webhook_token"] != "prod-token-xyz" || mergedKeep["headline"] != "Updated Title" {
		t.Fatalf("expected MergeThemeConfigSecrets to preserve secret on mask, got %+v", mergedKeep)
	}
	mergedRotate := eng.MergeThemeConfigSecrets("secret-cfg-theme", map[string]any{
		"webhook_token": "rotated-token-777",
	}, saved["secret-cfg-theme"])
	if mergedRotate["webhook_token"] != "rotated-token-777" {
		t.Fatalf("expected MergeThemeConfigSecrets to update secret, got %+v", mergedRotate)
	}
	mergedClear := eng.MergeThemeConfigSecrets("secret-cfg-theme", map[string]any{
		"webhook_token": "",
	}, saved["secret-cfg-theme"])
	if mergedClear["webhook_token"] != "" {
		t.Fatalf("expected MergeThemeConfigSecrets to allow explicit empty clear, got %+v", mergedClear)
	}
}

func TestW1C_ExportZip_DecompressesBuiltinAndAssetMemoryDiet(t *testing.T) {
	cat, err := i18n.NewCatalog("en-US", "")
	if err != nil {
		t.Fatalf("NewCatalog: %v", err)
	}
	eng, err := NewEngine(cat, t.TempDir())
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}

	// 1. ExportZip(DefaultThemeID) must contain clean human-editable files and zero .gz entries.
	var zipBuf bytes.Buffer
	if err := eng.ExportZip(&zipBuf, DefaultThemeID); err != nil {
		t.Fatalf("ExportZip(%s): %v", DefaultThemeID, err)
	}
	zipBytes := zipBuf.Bytes()
	zr, err := zip.NewReader(bytes.NewReader(zipBytes), int64(len(zipBytes)))
	if err != nil {
		t.Fatalf("zip.NewReader: %v", err)
	}
	entries := make(map[string][]byte, len(zr.File))
	for _, f := range zr.File {
		if strings.HasSuffix(f.Name, ".gz") || strings.Contains(f.Name, "_gz/") {
			t.Fatalf("ExportZip(%s) leaked compressed internal path %q", DefaultThemeID, f.Name)
		}
		rc, oErr := f.Open()
		if oErr != nil {
			t.Fatalf("open zip entry %s: %v", f.Name, oErr)
		}
		data, rErr := io.ReadAll(rc)
		_ = rc.Close()
		if rErr != nil {
			t.Fatalf("read zip entry %s: %v", f.Name, rErr)
		}
		entries[f.Name] = data
	}
	for _, mustExist := range []string{
		"theme.json",
		"preview.svg",
		"templates/error.html",
		"templates/layout.html",
		"assets/volo-studio.css",
		"assets/volo-studio.js",
		"assets/comment-widget.js",
	} {
		raw, ok := entries[mustExist]
		if !ok || len(raw) == 0 {
			t.Fatalf("expected ExportZip(%s) to include non-empty %q", DefaultThemeID, mustExist)
		}
	}
	var parsedManifest Manifest
	if err := json.Unmarshal(entries["theme.json"], &parsedManifest); err != nil || parsedManifest.ID != DefaultThemeID {
		t.Fatalf("expected exported theme.json to be valid decompressed JSON for %s, err=%v", DefaultThemeID, err)
	}

	// 2. Memory diet: ServeAsset keeps raw == nil for precompressed assets while serving both gzip and plain clients.
	reqGz := httptest.NewRequest(http.MethodGet, "/themes/volo-studio/assets/volo-studio.css", nil)
	reqGz.Header.Set("Accept-Encoding", "gzip")
	recGz := httptest.NewRecorder()
	eng.ServeAsset(recGz, reqGz, DefaultThemeID, "volo-studio.css")
	if recGz.Code != http.StatusOK || recGz.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("expected gzip asset response, code=%d enc=%q", recGz.Code, recGz.Header().Get("Content-Encoding"))
	}
	gr, err := gzip.NewReader(bytes.NewReader(recGz.Body.Bytes()))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	unzippedFromGz, err := io.ReadAll(gr)
	_ = gr.Close()
	if err != nil {
		t.Fatalf("read gzipped asset: %v", err)
	}

	reqPlain := httptest.NewRequest(http.MethodGet, "/themes/volo-studio/assets/volo-studio.css", nil)
	recPlain := httptest.NewRecorder()
	eng.ServeAsset(recPlain, reqPlain, DefaultThemeID, "volo-studio.css")
	if recPlain.Code != http.StatusOK || recPlain.Header().Get("Content-Encoding") != "" {
		t.Fatalf("expected plain asset response without Content-Encoding, code=%d", recPlain.Code)
	}
	if !bytes.Equal(recPlain.Body.Bytes(), unzippedFromGz) {
		t.Fatalf("expected on-demand decompressed asset to match gzipped payload byte-for-byte")
	}

	eng.mu.RLock()
	lt := eng.themes[DefaultThemeID]
	eng.mu.RUnlock()
	for _, assetKey := range []string{"volo-studio.css", "volo-studio.js", "comment-widget.js"} {
		ca, ok := lt.assets[assetKey]
		if !ok {
			t.Fatalf("missing cachedAsset %q", assetKey)
		}
		if ca.raw != nil {
			t.Fatalf("expected precompressed cachedAsset %q to keep raw == nil in memory", assetKey)
		}
		if len(ca.gzipped) == 0 {
			t.Fatalf("expected precompressed cachedAsset %q to store non-empty gzipped slice", assetKey)
		}
	}

	// ServePreview also works cleanly from preview.svg.gz without retaining raw bytes.
	reqPrev := httptest.NewRequest(http.MethodGet, "/themes/volo-studio/preview", nil)
	recPrev := httptest.NewRecorder()
	eng.ServePreview(recPrev, reqPrev, DefaultThemeID)
	if recPrev.Code != http.StatusOK || !strings.Contains(recPrev.Body.String(), "<svg") {
		t.Fatalf("expected ServePreview to serve decompressed SVG on demand, code=%d", recPrev.Code)
	}
	if !lt.hasPreview || lt.preview.raw != nil {
		t.Fatalf("expected preview to keep raw == nil when loaded from preview.svg.gz")
	}
}

func TestW1C_TaxonomiesLinksPagination_JumpPillsCap_And_LayoutCSRF(t *testing.T) {
	cat, err := i18n.NewCatalog("en-US", "")
	if err != nil {
		t.Fatalf("NewCatalog: %v", err)
	}
	eng, err := NewEngine(cat, t.TempDir())
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}

	// 1. /categories and /series render pagination when TotalPages > 1.
	for _, kind := range []string{"categories", "series"} {
		rec := httptest.NewRecorder()
		err := eng.Render(rec, http.StatusOK, DefaultThemeID, "taxonomies.html", PageContext{
			Locale:       "en-US",
			CurrentPath:  "/" + kind,
			TaxonomyKind: kind,
			Page:         1,
			TotalPages:   3,
			Categories: []content.CategoryWithCount{
				{Category: domain.Category{ID: "c1", Name: "Systems", Slug: "systems"}, ArticleCount: 4},
			},
			SeriesList: []domain.Series{
				{ID: "s1", Title: "Go Runtime", Slug: "go-runtime"},
			},
		})
		if err != nil {
			t.Fatalf("Render taxonomies.html (%s): %v", kind, err)
		}
		if !strings.Contains(rec.Body.String(), `class="pagination"`) {
			t.Fatalf("expected /%s with TotalPages=3 to render .pagination", kind)
		}
	}

	// 2. links.html caps .studio-jump-pills at 18 with .js-jump-pills-toggle (+4) and renders .pagination.
	groups := make([]LinkCategoryGroup, 22)
	for i := 0; i < 22; i++ {
		idxStr := strconv.Itoa(i + 1)
		groups[i] = LinkCategoryGroup{
			Category: "Group " + idxStr,
			Items:    []domain.Link{{ID: "l_" + idxStr, Title: "Site " + idxStr, URL: "https://s" + idxStr + ".example"}},
		}
	}
	recLinks := httptest.NewRecorder()
	if err := eng.Render(recLinks, http.StatusOK, DefaultThemeID, "links.html", PageContext{
		Locale:          "en-US",
		CurrentPath:     "/links",
		Page:            1,
		TotalPages:      2,
		LinkGroups:      groups,
		Viewer:          &domain.User{ID: "u_1", Username: "alice", DisplayName: "Alice", Role: domain.RoleReader, Status: domain.UserStatusActive},
		ViewerCSRFToken: "csrf_tok_98765",
	}); err != nil {
		t.Fatalf("Render links.html: %v", err)
	}
	outLinks := recLinks.Body.String()
	for _, needle := range []string{
		`class="pagination"`,
		`js-jump-pills-toggle`,
		`data-more-label="+4"`,
		`data-extra-pill hidden`,
		`<meta name="csrf-token" content="csrf_tok_98765">`,
		`js-reader-logout-form`,
		`name="csrf_token" value="csrf_tok_98765"`,
	} {
		if !strings.Contains(outLinks, needle) {
			t.Fatalf("expected links.html output to contain %q", needle)
		}
	}
}
