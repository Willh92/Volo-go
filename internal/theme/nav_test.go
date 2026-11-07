package theme

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Willh92/Volo-go/internal/content"
	"github.com/Willh92/Volo-go/internal/domain"
	"github.com/Willh92/Volo-go/internal/visibility"
)

func TestResolveNavMenuDefaultPlacesCustomLinksLast(t *testing.T) {
	eng, err := NewEngine(nil, "../../themes")
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}

	pctx := PageContext{
		ThemeID:     DefaultThemeID,
		Locale:      "en-US",
		CurrentPath: "/books",
		AdminPath:   "/admin",
		ThemeConfig: eng.ResolveThemeConfig(DefaultThemeID, nil),
		NavItems: []content.PublicNavNode{
			{ID: "n1", Title: "About", URL: "/posts/about"},
			{ID: "n2", Title: "AdminSkip", URL: "/admin"},
			{ID: "n3", Title: "GitHub", URL: "https://github.com/Willh92/Volo-go", OpenInNewTab: true, Icon: "github"},
		},
	}

	menu := eng.ResolveNavMenu(pctx)
	if len(menu.Items) != 11 {
		t.Fatalf("expected 11 resolved items (9 builtin + 2 custom), got %d", len(menu.Items))
	}
	if menu.Items[len(menu.Items)-2].URL != "/posts/about" || menu.Items[len(menu.Items)-1].URL != "https://github.com/Willh92/Volo-go" {
		t.Fatalf("expected custom links after last builtin item, got %+v and %+v", menu.Items[len(menu.Items)-2], menu.Items[len(menu.Items)-1])
	}
	if len(menu.Primary) != DefaultNavPrimaryCount {
		t.Fatalf("expected %d primary items, got %d", DefaultNavPrimaryCount, len(menu.Primary))
	}
	if !menu.OverflowActive {
		t.Fatalf("expected OverflowActive=true when /books is in overflow")
	}
}

func TestResolveNavMenuCustomOrderAndVisibility(t *testing.T) {
	eng, err := NewEngine(nil, "../../themes")
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}

	pctx := PageContext{
		ThemeID:     DefaultThemeID,
		Locale:      "en-US",
		CurrentPath: "/",
		ThemeConfig: map[string]any{
			"header_nav_items":         []any{"home", "books", "archives", "custom"},
			"header_nav_primary_count": float64(10),
		},
		NavItems: []content.PublicNavNode{
			{ID: "c1", Title: "Resume", URL: "/resume"},
		},
	}

	menu := eng.ResolveNavMenu(pctx)
	wantURLs := []string{"/", "/books", "/archives", "/resume"}
	if len(menu.Primary) != len(wantURLs) || len(menu.Overflow) != 0 {
		t.Fatalf("expected %d primary and 0 overflow, got primary=%d overflow=%d", len(wantURLs), len(menu.Primary), len(menu.Overflow))
	}
	for i, u := range wantURLs {
		if menu.Primary[i].URL != u {
			t.Fatalf("item[%d] URL = %q, want %q", i, menu.Primary[i].URL, u)
		}
	}
}

func TestResolveThemeForRequestDesktopVsMobile(t *testing.T) {
	eng, err := NewEngine(nil, "../../themes")
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	app := domain.AppearanceSettings{
		ActiveTheme: "volo-studio",
		MobileTheme: "volo-journal",
	}

	reqDesktop := httptest.NewRequest("GET", "http://example.com/", nil)
	reqDesktop.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 14_0) AppleWebKit/537.36")
	if got := eng.ResolveThemeForRequest(reqDesktop, nil, app); got != "volo-studio" {
		t.Fatalf("desktop theme = %q, want volo-studio", got)
	}

	reqMobile := httptest.NewRequest("GET", "http://example.com/", nil)
	reqMobile.Header.Set("User-Agent", "Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) Mobile/15E148")
	if got := eng.ResolveThemeForRequest(reqMobile, nil, app); got != "volo-journal" {
		t.Fatalf("mobile theme = %q, want volo-journal", got)
	}
}

func TestNavPrefixBoundaryAndConcurrentLocaleSafety(t *testing.T) {
	eng, err := NewEngine(nil, "../../themes")
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}

	// /books-notes MUST NOT activate /books or custom /notes-extra prefix mismatches
	pctx := PageContext{
		ThemeID:     DefaultThemeID,
		Locale:      "en-US",
		CurrentPath: "/books-notes",
		ThemeConfig: map[string]any{
			"header_nav_items":         []any{"books", "custom"},
			"header_nav_primary_count": float64(10),
		},
		NavItems: []content.PublicNavNode{
			{ID: "c1", Title: "Book", URL: "/book"},
			{ID: "c2", Title: "BooksSub", URL: "/books-notes"},
		},
	}
	menu := eng.ResolveNavMenu(pctx)
	for _, item := range menu.Items {
		if item.URL == "/books" && item.IsActive {
			t.Fatalf("/books must NOT be active on /books-notes")
		}
		if item.URL == "/book" && item.IsActive {
			t.Fatalf("custom /book must NOT be active on /books-notes")
		}
		if item.URL == "/books-notes" && !item.IsActive {
			t.Fatalf("custom /books-notes SHOULD be active on /books-notes")
		}
	}

	// /books/clean-code MUST activate /books
	pctx.CurrentPath = "/books/clean-code"
	menuSub := eng.ResolveNavMenu(pctx)
	if !menuSub.Items[0].IsActive {
		t.Fatalf("/books SHOULD be active on /books/clean-code")
	}

	// Concurrent ResolveNavMenu + RenderPage must be race-free on th.locales
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 20; i++ {
			rec := httptest.NewRecorder()
			rd := domain.DefaultReadingSettings()
			rd.EnablePJAX = false
			_ = eng.Render(rec, 200, DefaultThemeID, "links.html", PageContext{
				Locale:      "zh-CN",
				CurrentPath: "/links",
				Reading:     rd,
			})
		}
	}()
	for i := 0; i < 40; i++ {
		_ = eng.ResolveNavMenu(pctx)
	}
	<-done
}

func TestThemeSanitizersAndPageURL(t *testing.T) {
	// UTF-8 multi-byte case-folding characters + </style> escape
	css := "\u023A\u212A </StYlE><script>alert(1)</script> a { width: ExPrEsSiOn(alert(1)); background: url(JaVaScRiPt:evil()); }"
	gotCSS := sanitizeStyleContent(css)
	if gotCSS == "" ||
		indexFoldASCIITheme(gotCSS, "</style") >= 0 ||
		indexFoldASCIITheme(gotCSS, "expression(") >= 0 ||
		indexFoldASCIITheme(gotCSS, "javascript:") >= 0 {
		t.Fatalf("sanitizeStyleContent failed: %q", gotCSS)
	}

	// Control-char obfuscated scheme must be rejected
	if _, ok := sanitizeThemeURL("java\tscript:alert(1)", true); ok {
		t.Fatal("expected control-char javascript: URL to be rejected")
	}
	if _, ok := sanitizeThemeURL("//evil.example/x", true); ok {
		t.Fatal("expected protocol-relative URL to be rejected")
	}
	if u, ok := sanitizeThemeURL("/archives?year=2025", true); !ok || u != "/archives?year=2025" {
		t.Fatalf("expected valid site-relative URL, got %q ok=%v", u, ok)
	}

	// pageURL query preservation
	if got := buildThemePageURL("/archives?year=2025&page=3", 1); got != "/archives?year=2025" {
		t.Fatalf("pageURL(page=1) = %q, want /archives?year=2025", got)
	}
	if got := buildThemePageURL("/tags?q=golang&sort=name", 2); got != "/tags?page=2&q=golang&sort=name" {
		t.Fatalf("pageURL(page=2) = %q, want /tags?page=2&q=golang&sort=name", got)
	}
}

func TestDualCDNAssetAndVendorUnifiedRuntime(t *testing.T) {
	eng, err := NewEngine(nil, "../../themes")
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	eng.SetAssetCDNPrefixFunc(func() string { return "https://static.example.com/" })
	eng.SetVendorCDNPrefixFunc(func() string { return "https://s4.zstatic.net/npm/" })

	// 1. Ordinary page: rewrites theme assets to static.example.com, injects window.__VOLO_VENDOR__ with s4.zstatic.net/npm, NO preconnect
	rec := httptest.NewRecorder()
	rd := domain.DefaultReadingSettings()
	rd.AssetCDNPrefix = "https://static.example.com"
	rd.VendorCDNPrefix = "https://s4.zstatic.net/npm"
	if err := eng.Render(rec, 200, DefaultThemeID, "links.html", PageContext{
		Locale:      "zh-CN",
		CurrentPath: "/links",
		Reading:     rd,
	}); err != nil {
		t.Fatalf("Render links.html: %v", err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `https://static.example.com/themes/volo-studio/assets/volo-studio.css?v=`) {
		t.Fatalf("expected asset_cdn_prefix rewrite on CSS asset URL, got snippet: %s", body[:min(len(body), 600)])
	}
	if !strings.Contains(body, `window.__VOLO_VENDOR__`) || !strings.Contains(body, `"https://s4.zstatic.net/npm"`) {
		t.Fatalf("expected backend-unified window.__VOLO_VENDOR__ with s4.zstatic.net/npm in <head>")
	}
	if strings.Contains(body, `rel="preconnect"`) {
		t.Fatalf("did not expect preconnect on page without math or mermaid")
	}

	// 2. Math page: emits <link rel="preconnect" href="https://s4.zstatic.net" crossorigin>
	recMath := httptest.NewRecorder()
	if err := eng.Render(recMath, 200, "volo-journal", "article.html", PageContext{
		Locale:      "zh-CN",
		CurrentPath: "/posts/math",
		IsArticle:   true,
		Reading:     rd,
		Detail:      visibility.PublicArticleView{Title: "Math", NeedsMath: true},
	}); err != nil {
		t.Fatalf("Render math article: %v", err)
	}
	if !strings.Contains(recMath.Body.String(), `<link rel="preconnect" href="https://s4.zstatic.net" crossorigin>`) {
		t.Fatalf("expected conditional preconnect for math article")
	}

	// 3. ServeAsset emits Access-Control-Allow-Origin: * for external pull-CDNs
	reqAsset := httptest.NewRequest("GET", "/themes/volo-studio/assets/volo-studio.css?v=abc", nil)
	recAsset := httptest.NewRecorder()
	eng.ServeAsset(recAsset, reqAsset, DefaultThemeID, "volo-studio.css")
	if got := recAsset.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("ServeAsset Access-Control-Allow-Origin = %q, want *", got)
	}
}

