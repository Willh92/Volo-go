package theme

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Willh92/Volo-go/internal/content"
	"github.com/Willh92/Volo-go/internal/domain"
	"github.com/Willh92/Volo-go/internal/i18n"
	"github.com/Willh92/Volo-go/internal/render"
	"github.com/Willh92/Volo-go/internal/visibility"
	"github.com/Willh92/Volo-go/pkg/plugin"
)

func TestRound3_IsMobileRequest_And_StaffDeviceOverride(t *testing.T) {
	// 1. IsMobileRequest must NEVER honor ?device= query overrides.
	reqDesktopSpoof := httptest.NewRequest(http.MethodGet, "https://example.com/?device=mobile", nil)
	reqDesktopSpoof.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 Chrome/130.0.0.0 Safari/537.36")
	if IsMobileRequest(reqDesktopSpoof) {
		t.Fatalf("expected desktop UA with ?device=mobile to remain false in IsMobileRequest")
	}

	reqMobileSpoof := httptest.NewRequest(http.MethodGet, "https://example.com/?device=desktop", nil)
	reqMobileSpoof.Header.Set("User-Agent", "Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 Mobile/15E148")
	if !IsMobileRequest(reqMobileSpoof) {
		t.Fatalf("expected iPhone UA with ?device=desktop to remain true in IsMobileRequest")
	}

	reqClientHint := httptest.NewRequest(http.MethodGet, "https://example.com/?device=pc", nil)
	reqClientHint.Header.Set("Sec-CH-UA-Mobile", "?1")
	if !IsMobileRequest(reqClientHint) {
		t.Fatalf("expected Sec-CH-UA-Mobile=?1 to remain true in IsMobileRequest")
	}

	// 2. ResolveThemeForRequest must restrict ?device= override strictly to active Admin/Editor.
	cat, err := i18n.NewCatalog("en-US", "")
	if err != nil {
		t.Fatalf("NewCatalog: %v", err)
	}
	eng, err := NewEngine(cat, t.TempDir())
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	for _, id := range []string{"desk-skin", "mob-skin"} {
		zb := buildTestThemeZip(t, map[string]string{
			"theme.json": `{"id":"` + id + `","name":{"en":"` + id + `"},"version":"1.0.0","author":"test"}`,
			"templates/index.html": `{{define "content"}}<div>` + id + `</div>{{end}}`,
		})
		if _, err := eng.InstallZip(bytes.NewReader(zb), int64(len(zb)), true); err != nil {
			t.Fatalf("InstallZip(%s): %v", id, err)
		}
	}

	app := domain.AppearanceSettings{
		ActiveTheme: "desk-skin",
		MobileTheme: "mob-skin",
	}

	readerViewer := &domain.User{ID: "u_reader", Role: domain.RoleReader, Status: domain.UserStatusActive}
	disabledAdmin := &domain.User{ID: "u_disabled", Role: domain.RoleAdmin, Status: domain.UserStatusDisabled}
	activeAdmin := &domain.User{ID: "u_admin", Role: domain.RoleAdmin, Status: domain.UserStatusActive}
	activeEditor := &domain.User{ID: "u_editor", Role: domain.RoleEditor, Status: domain.UserStatusActive}

	if got := eng.ResolveThemeForRequest(reqDesktopSpoof, nil, app); got != "desk-skin" {
		t.Fatalf("unauthenticated ?device=mobile must not switch theme, got %q", got)
	}
	if got := eng.ResolveThemeForRequest(reqDesktopSpoof, readerViewer, app); got != "desk-skin" {
		t.Fatalf("reader ?device=mobile must not switch theme, got %q", got)
	}
	if got := eng.ResolveThemeForRequest(reqDesktopSpoof, disabledAdmin, app); got != "desk-skin" {
		t.Fatalf("disabled admin ?device=mobile must not switch theme, got %q", got)
	}
	if got := eng.ResolveThemeForRequest(reqDesktopSpoof, activeAdmin, app); got != "mob-skin" {
		t.Fatalf("active admin ?device=mobile expected mob-skin, got %q", got)
	}
	if got := eng.ResolveThemeForRequest(reqMobileSpoof, activeEditor, app); got != "desk-skin" {
		t.Fatalf("active editor ?device=desktop expected desk-skin, got %q", got)
	}
}

func TestRound3_HomeFallbackAlias_And_PageTemplatesFallback(t *testing.T) {
	cat, err := i18n.NewCatalog("en-US", "")
	if err != nil {
		t.Fatalf("NewCatalog: %v", err)
	}
	eng, err := NewEngine(cat, t.TempDir())
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}

	// External theme provides templates/home.html WITHOUT templates/index.html and omits page_templates.
	zb := buildTestThemeZip(t, map[string]string{
		"theme.json":          `{"id":"legacy-home","name":{"en":"Legacy Home"},"version":"1.0.0","author":"Volo"}`,
		"templates/home.html": `{{define "content"}}<section class="legacy-home-marker">{{.Site.Title}}</section>{{end}}`,
	})
	if _, err := eng.InstallZip(bytes.NewReader(zb), int64(len(zb)), true); err != nil {
		t.Fatalf("InstallZip(legacy-home): %v", err)
	}

	rec := httptest.NewRecorder()
	err = eng.Render(rec, http.StatusOK, "legacy-home", "index.html", PageContext{
		Locale:      "en-US",
		CurrentPath: "/",
		IsHome:      true,
		Site:        domain.SiteSettings{Title: "Fallback Home Works"},
	})
	if err != nil {
		t.Fatalf("Render(index.html -> home.html): %v", err)
	}
	if !strings.Contains(rec.Body.String(), `<section class="legacy-home-marker">Fallback Home Works</section>`) {
		t.Fatalf("expected home.html content to render for index.html, got:\n%s", rec.Body.String())
	}

	localized := eng.ListLocalized("en-US")
	var found *LocalizedTheme
	for i := range localized {
		if localized[i].ID == "legacy-home" {
			found = &localized[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("legacy-home missing from ListLocalized")
	}
	if len(found.LocalizedPageTemplates) < 3 {
		t.Fatalf("expected legacy-home to inherit builtin PageTemplates fallback, got %+v", found.LocalizedPageTemplates)
	}
}

func TestRound3_ActiveChapterNeedsMermaid_And_ProseHTMLLockLoginNext(t *testing.T) {
	cat, err := i18n.NewCatalog("en-US", "")
	if err != nil {
		t.Fatalf("NewCatalog: %v", err)
	}
	eng, err := NewEngine(cat, t.TempDir())
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	eng.SetVendorCDNPrefixFunc(func() string {
		return "https://vendor-cdn.example.com/assets"
	})

	zb := buildTestThemeZip(t, map[string]string{
		"theme.json": `{"id":"probe-mermaid","name":{"en":"Probe"},"version":"1.0.0","author":"Volo"}`,
		"templates/book_reader.html": `{{define "content"}}<div class="probe-prose">{{proseHTML .ActiveChapterHTML .CurrentPath .RawQuery}}</div>{{end}}`,
	})
	if _, err := eng.InstallZip(bytes.NewReader(zb), int64(len(zb)), true); err != nil {
		t.Fatalf("InstallZip: %v", err)
	}

	rec := httptest.NewRecorder()
	err = eng.Render(rec, http.StatusOK, "probe-mermaid", "book_reader.html", PageContext{
		Locale:                    "en-US",
		CurrentPath:               "/books/go-guide",
		RawQuery:                  "ch=ch_2",
		ActiveChapterNeedsMermaid: true,
		ActiveChapterHTML:         `<div class="volo-lock-card"><a class="volo-btn" href="/reader/login">Sign in</a></div>`,
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `<link rel="preconnect" href="https://vendor-cdn.example.com" crossorigin>`) {
		t.Fatalf("expected ActiveChapterNeedsMermaid to inject vendor CDN preconnect, got:\n%s", body)
	}
	if !strings.Contains(body, `href="/reader/login?next=%2Fbooks%2Fgo-guide%3Fch%3Dch_2"`) {
		t.Fatalf("expected proseHTML to append URL-encoded ?next= to /reader/login, got:\n%s", body)
	}

	// Self-referential /reader/login or external protocol paths must NOT rewrite ?next=
	for _, unsafePath := range []string{"/reader/login", "//evil.example/path"} {
		recUnsafe := httptest.NewRecorder()
		if err := eng.Render(recUnsafe, http.StatusOK, "probe-mermaid", "book_reader.html", PageContext{
			Locale:            "en-US",
			CurrentPath:       unsafePath,
			ActiveChapterHTML: `<a href="/reader/login">Login</a>`,
		}); err != nil {
			t.Fatalf("Render(%s): %v", unsafePath, err)
		}
		if strings.Contains(recUnsafe.Body.String(), "next=") {
			t.Fatalf("expected %q not to append next= inside proseHTML, got %s", unsafePath, recUnsafe.Body.String())
		}
	}
}

func TestRound3_VoloJournal_Parity(t *testing.T) {
	cat, err := i18n.NewCatalog("en-US", "")
	if err != nil {
		t.Fatalf("NewCatalog: %v", err)
	}
	themesPath := filepath.Join("..", "..", "themes")
	eng, err := NewEngine(cat, themesPath)
	if err != nil {
		t.Fatalf("NewEngine(%s): %v", themesPath, err)
	}
	if !eng.Has("volo-journal") {
		t.Fatalf("expected volo-journal to load from %s", themesPath)
	}
	if _, statErr := os.Stat(filepath.Join(themesPath, "volo-journal", "templates", "home.html")); !os.IsNotExist(statErr) {
		t.Fatalf("expected legacy themes/volo-journal/templates/home.html to be removed")
	}

	rec := httptest.NewRecorder()
	err = eng.Render(rec, http.StatusOK, "volo-journal", "index.html", PageContext{
		Locale:      "en-US",
		CurrentPath: "/",
		IsHome:      true,
		Site: domain.SiteSettings{
			Title:    "Journal Press",
			Subtitle: "Slow Craft Notes",
		},
		ThemeConfig: map[string]any{
			"show_recent_posts": false,
			"journal_motto":     "",
		},
		HeaderWidgets: []plugin.RenderedSlot{
			{PluginID: "hdr-test", HTML: `<span class="test-hdr-widget">HDR-SLOT</span>`},
		},
		ArticleCards: []visibility.PublicArticleCard{
			{
				ID:    "a_1",
				Title: "Disabling Sidebar Recent Must Keep Main Feed",
				Slug:  "keep-main-feed",
				URL:   "/posts/keep-main-feed",
			},
		},
	})
	if err != nil {
		t.Fatalf("Render(volo-journal index.html): %v", err)
	}
	out := rec.Body.String()
	for _, needle := range []string{
		"Disabling Sidebar Recent Must Keep Main Feed",
		"Slow Craft Notes",
		`<span class="test-hdr-widget">HDR-SLOT</span>`,
	} {
		if !strings.Contains(out, needle) {
			t.Fatalf("expected volo-journal index output to contain %q", needle)
		}
	}
}

func installLiveVoloStudioFromSource(t *testing.T, eng *Engine, aliasID string) {
	t.Helper()
	srcRoot := filepath.Join("builtin", "volo-studio")
	files := make(map[string]string)

	rawManifest, err := os.ReadFile(filepath.Join(srcRoot, "theme.json"))
	if err != nil {
		t.Fatalf("read volo-studio theme.json: %v", err)
	}
	files["theme.json"] = strings.Replace(string(rawManifest), `"id": "volo-studio"`, `"id": "`+aliasID+`"`, 1)

	tplEntries, err := os.ReadDir(filepath.Join(srcRoot, "templates"))
	if err != nil {
		t.Fatalf("readdir templates: %v", err)
	}
	for _, ent := range tplEntries {
		if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".html") {
			continue
		}
		b, rErr := os.ReadFile(filepath.Join(srcRoot, "templates", ent.Name()))
		if rErr != nil {
			t.Fatalf("read template %s: %v", ent.Name(), rErr)
		}
		files["templates/"+ent.Name()] = string(b)
	}

	assetEntries, err := os.ReadDir(filepath.Join(srcRoot, "assets"))
	if err != nil {
		t.Fatalf("readdir assets: %v", err)
	}
	for _, ent := range assetEntries {
		if ent.IsDir() {
			continue
		}
		b, rErr := os.ReadFile(filepath.Join(srcRoot, "assets", ent.Name()))
		if rErr != nil {
			t.Fatalf("read asset %s: %v", ent.Name(), rErr)
		}
		files["assets/"+ent.Name()] = string(b)
	}

	zb := buildTestThemeZip(t, files)
	if _, err := eng.InstallZip(bytes.NewReader(zb), int64(len(zb)), true); err != nil {
		t.Fatalf("InstallZip(%s): %v", aliasID, err)
	}
}

func TestRound3_VoloStudioSourceTemplates_RenderAllFeatures(t *testing.T) {
	cat, err := i18n.NewCatalog("en-US", "")
	if err != nil {
		t.Fatalf("NewCatalog: %v", err)
	}
	eng, err := NewEngine(cat, t.TempDir())
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	const studioSrcID = "volo-studio-src"
	installLiveVoloStudioFromSource(t, eng, studioSrcID)

	// Build 15 sidebar categories to verify 12-item cap + View all link.
	cats := make([]content.CategoryWithCount, 15)
	for i := 0; i < 15; i++ {
		cats[i] = content.CategoryWithCount{
			Category: domain.Category{
				ID:   "c_" + string(rune('a'+i)),
				Name: "Category " + string(rune('A'+i)),
				Slug: "cat-" + string(rune('a'+i)),
			},
			ArticleCount: i + 1,
		}
	}

	// 1. Article with CommentAccessMode="off" and CommentCount=0 -> omits #volo-comments;
	//    verifies header login ?next=, :::lock prose ?next=, Site.Subtitle, and sidebar category cap.
	recOffEmpty := httptest.NewRecorder()
	err = eng.Render(recOffEmpty, http.StatusOK, studioSrcID, "article.html", PageContext{
		Locale:      "en-US",
		CurrentPath: "/posts/deep-dive",
		IsArticle:   true,
		Site: domain.SiteSettings{
			Title:    "Volo Studio",
			Subtitle: "Crafted Publishing",
		},
		Reading: domain.ReadingSettings{
			CommentAccessMode: "off",
		},
		SidebarCategories: cats,
		Detail: visibility.PublicArticleView{
			ID:           "art_1",
			Title:        "Deep Dive",
			Slug:         "deep-dive",
			URL:          "/posts/deep-dive",
			AllowComment: true,
			CommentCount: 0,
			HTML:         `<p>Intro</p><div class="volo-lock-card"><a href="/reader/login" class="volo-btn">Login to Read</a></div>`,
		},
	})
	if err != nil {
		t.Fatalf("Render article (off, 0 comments): %v", err)
	}
	outOffEmpty := recOffEmpty.Body.String()
	if strings.Contains(outOffEmpty, `id="volo-comments"`) {
		t.Fatalf("expected #volo-comments to be omitted when CommentAccessMode==off and CommentCount==0")
	}
	if !strings.Contains(outOffEmpty, `/reader/login?next=%2Fposts%2Fdeep-dive`) {
		t.Fatalf("expected header & lock-card login links to preserve ?next=%%2Fposts%%2Fdeep-dive")
	}
	if !strings.Contains(outOffEmpty, `Crafted Publishing`) {
		t.Fatalf("expected brand subtitle to render in layout.html")
	}

	// 1b. Home (index.html) renders {{template "sidebar" .}}: verify SidebarCategories caps at 12 with View all -> /categories.
	recHome := httptest.NewRecorder()
	if err := eng.Render(recHome, http.StatusOK, studioSrcID, "index.html", PageContext{
		Locale:            "en-US",
		CurrentPath:       "/",
		IsHome:            true,
		SidebarCategories: cats,
	}); err != nil {
		t.Fatalf("Render index.html: %v", err)
	}
	outHome := recHome.Body.String()
	if strings.Contains(outHome, `/categories/cat-m`) || !strings.Contains(outHome, `studio-side-more__link`) {
		t.Fatalf("expected sidebar categories on index.html to cap at 12 items with View all link")
	}

	// 2. Article with CommentAccessMode="off" and CommentCount=4 -> renders #volo-comments read-only mount.
	recOffHistory := httptest.NewRecorder()
	err = eng.Render(recOffHistory, http.StatusOK, studioSrcID, "article.html", PageContext{
		Locale:      "en-US",
		CurrentPath: "/posts/archive-post",
		IsArticle:   true,
		Reading: domain.ReadingSettings{
			CommentAccessMode: "off",
		},
		Detail: visibility.PublicArticleView{
			ID:           "art_2",
			Title:        "Archive Post",
			Slug:         "archive-post",
			URL:          "/posts/archive-post",
			AllowComment: true,
			CommentCount: 4,
			HTML:         `<p>Historical discussion</p>`,
		},
	})
	if err != nil {
		t.Fatalf("Render article (off, 4 comments): %v", err)
	}
	outOffHistory := recOffHistory.Body.String()
	if !strings.Contains(outOffHistory, `id="volo-comments"`) || !strings.Contains(outOffHistory, `data-comment-mode="off"`) {
		t.Fatalf("expected #volo-comments with data-comment-mode=off when historical comments exist")
	}

	// 3. Book Reader: BookLocked inline password card, >8 chapter filter, string chapter IDs, and ActiveChapterNeedsMermaid in layout.html.
	flatChaps := make([]domain.BookChapter, 10)
	for i := 0; i < 10; i++ {
		flatChaps[i] = domain.BookChapter{
			ID:    "ch_" + string(rune('a'+i)),
			Title: "Chapter " + string(rune('A'+i)),
			URL:   "/posts/ch-" + string(rune('a'+i)),
		}
	}
	recBook := httptest.NewRecorder()
	err = eng.Render(recBook, http.StatusOK, studioSrcID, "book_reader.html", PageContext{
		Locale:                    "en-US",
		CurrentPath:               "/books/handbook",
		RawQuery:                  "ch=ch_b",
		ActiveChapterNeedsMermaid: true,
		BookLocked:                true,
		BookArticleID:             "art_locked_9",
		BookArticleSlug:           "ch-b",
		BookDetail: content.BookDetailView{
			Book:         domain.Book{ID: "b_1", Title: "Handbook", Slug: "handbook", EmbedMode: "inline"},
			FlatChapters: flatChaps,
		},
		ActiveChapter: &flatChaps[1],
		PrevChapter:   &domain.BookChapter{ID: "ch_prev", Title: "Prev Chap"},
		NextChapter:   &domain.BookChapter{ID: "ch_next", Title: "Next Chap"},
	})
	if err != nil {
		t.Fatalf("Render book_reader.html: %v", err)
	}
	outBook := recBook.Body.String()
	for _, needle := range []string{
		`book-unlock-card`,
		`data-article-id="art_locked_9"`,
		`js-book-chap-filter`,
		`/books/handbook?ch=ch_prev`,
		`/books/handbook?ch=ch_next`,
	} {
		if !strings.Contains(outBook, needle) {
			t.Fatalf("expected locked book_reader output to contain %q", needle)
		}
	}
	if strings.Contains(outBook, `data-needs-mermaid="1"`) {
		t.Fatalf("expected locked book chapter to suppress data-needs-mermaid until unlocked")
	}
	if strings.Contains(outBook, "%!d") {
		t.Fatalf("unexpected printf format error in book_reader output")
	}

	// 3b. Unlocked Book Chapter with ActiveChapterNeedsMermaid=true -> emits data-needs-mermaid="1".
	recBookUnlocked := httptest.NewRecorder()
	if err := eng.Render(recBookUnlocked, http.StatusOK, studioSrcID, "book_reader.html", PageContext{
		Locale:                    "en-US",
		CurrentPath:               "/books/handbook",
		RawQuery:                  "ch=ch_b",
		ActiveChapterNeedsMermaid: true,
		BookLocked:                false,
		BookDetail: content.BookDetailView{
			Book:         domain.Book{ID: "b_1", Title: "Handbook", Slug: "handbook", EmbedMode: "inline"},
			FlatChapters: flatChaps,
		},
		ActiveChapter:     &flatChaps[1],
		ActiveChapterHTML: `<pre class="mermaid">graph TD; A-->B;</pre>`,
	}); err != nil {
		t.Fatalf("Render unlocked book_reader.html: %v", err)
	}
	if !strings.Contains(recBookUnlocked.Body.String(), `data-needs-mermaid="1"`) {
		t.Fatalf("expected unlocked Mermaid chapter to emit data-needs-mermaid=\"1\"")
	}

	// 4. Reader Auth: ReaderAuthError banner, hidden next input, and ?next= cross-links.
	recAuth := httptest.NewRecorder()
	err = eng.Render(recAuth, http.StatusOK, studioSrcID, "reader_auth.html", PageContext{
		Locale:          "en-US",
		CurrentPath:     "/reader/login",
		ReaderNextURL:   "/posts/vip-note",
		ReaderAuthError: "Invalid credentials",
		Reading: domain.ReadingSettings{
			AllowReaderRegister: true,
		},
	})
	if err != nil {
		t.Fatalf("Render reader_auth.html: %v", err)
	}
	outAuth := recAuth.Body.String()
	for _, needle := range []string{
		`Invalid credentials`,
		`<input type="hidden" name="next" value="/posts/vip-note">`,
		`/reader/register?next=%2Fposts%2Fvip-note`,
		`/reader/login?next=%2Fposts%2Fvip-note`,
	} {
		if !strings.Contains(outAuth, needle) {
			t.Fatalf("expected reader_auth output to contain %q", needle)
		}
	}

	// 5. Standalone Page: mobile TOC bar on default template, suppressed on minimal template.
	sampleTOC := []render.TOCItem{
		{ID: "sec-1", Text: "Section One", Level: 2},
		{ID: "sec-2", Text: "Section Two", Level: 3},
	}
	recPageDefault := httptest.NewRecorder()
	if err := eng.Render(recPageDefault, http.StatusOK, studioSrcID, "page.html", PageContext{
		Locale:           "en-US",
		CurrentPath:      "/about",
		IsStandalonePage: true,
		StandalonePage: content.PublicPageView{
			ID:       "p_about",
			Title:    "About",
			Slug:     "about",
			Template: "default",
			ShowTOC:  true,
			TOC:      sampleTOC,
			HTML:     `<h2 id="sec-1">Section One</h2>`,
		},
	}); err != nil {
		t.Fatalf("Render page.html (default): %v", err)
	}
	if strings.Contains(recPageDefault.Body.String(), `studio-mobile-toc-bar`) || !strings.Contains(recPageDefault.Body.String(), `id="volo-mobile-toc-btn"`) || !strings.Contains(recPageDefault.Body.String(), `id="volo-toc-list"`) {
		t.Fatalf("expected default standalone page with ShowTOC to render unified #volo-mobile-toc-btn and #volo-toc-list without duplicate inline mobile TOC bar")
	}

	recPageMinimal := httptest.NewRecorder()
	if err := eng.Render(recPageMinimal, http.StatusOK, studioSrcID, "page.html", PageContext{
		Locale:           "en-US",
		CurrentPath:      "/now",
		IsStandalonePage: true,
		StandalonePage: content.PublicPageView{
			ID:       "p_now",
			Title:    "Now",
			Slug:     "now",
			Template: "minimal",
			ShowTOC:  true,
			TOC:      sampleTOC,
			HTML:     `<h2 id="sec-1">Section One</h2>`,
		},
	}); err != nil {
		t.Fatalf("Render page.html (minimal): %v", err)
	}
	if strings.Contains(recPageMinimal.Body.String(), `studio-mobile-toc-bar`) || strings.Contains(recPageMinimal.Body.String(), `id="volo-toc-list"`) {
		t.Fatalf("expected minimal standalone page template to suppress TOC bar and #volo-toc-list")
	}

	// 6. Links & Taxonomies discovery toolbars compile and render cleanly.
	recLinks := httptest.NewRecorder()
	if err := eng.Render(recLinks, http.StatusOK, studioSrcID, "links.html", PageContext{
		Locale:      "en-US",
		CurrentPath: "/links",
		LinkGroups: []LinkCategoryGroup{
			{Category: "Friends", Items: []domain.Link{{ID: "l1", Title: "Alice", URL: "https://alice.example"}}},
			{Category: "Tools", Items: []domain.Link{{ID: "l2", Title: "Bob", URL: "https://bob.example"}}},
		},
	}); err != nil {
		t.Fatalf("Render links.html: %v", err)
	}
	if !strings.Contains(recLinks.Body.String(), `js-collection-filter`) || !strings.Contains(recLinks.Body.String(), `studio-jump-pills`) {
		t.Fatalf("expected links.html to render filter input and category jump pills")
	}

	for _, kind := range []string{"categories", "series", "tags"} {
		recTax := httptest.NewRecorder()
		if err := eng.Render(recTax, http.StatusOK, studioSrcID, "taxonomies.html", PageContext{
			Locale:       "en-US",
			CurrentPath:  "/" + kind,
			TaxonomyKind: kind,
			Categories:   cats[:3],
			SeriesList: []domain.Series{
				{ID: "s1", Title: "Go Internals", Slug: "go-internals"},
				{ID: "s2", Title: "SQLite Craft", Slug: "sqlite-craft"},
			},
			Tags: []content.TagWithCount{
				{Tag: domain.Tag{ID: "t1", Name: "golang", Slug: "golang"}, ArticleCount: 5},
			},
		}); err != nil {
			t.Fatalf("Render taxonomies.html (%s): %v", kind, err)
		}
		if kind == "tags" && (!strings.Contains(recTax.Body.String(), `js-tag-filter-count`) || !strings.Contains(recTax.Body.String(), `js-tag-filter-clear`)) {
			t.Fatalf("expected /tags to render .js-tag-filter-count and .js-tag-filter-clear")
		}
		if kind != "tags" && !strings.Contains(recTax.Body.String(), `js-collection-filter`) {
			t.Fatalf("expected /%s to render .js-collection-filter", kind)
		}
	}
}
