package theme

import (
	"net/http/httptest"
	"testing"

	"github.com/Willh92/Volo-go/internal/content"
	"github.com/Willh92/Volo-go/internal/domain"
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
