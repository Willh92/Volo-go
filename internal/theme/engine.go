package theme

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"html/template"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/Willh92/Volo-go/internal/content"
	"github.com/Willh92/Volo-go/internal/domain"
	"github.com/Willh92/Volo-go/internal/i18n"
	"github.com/Willh92/Volo-go/internal/visibility"
	"github.com/Willh92/Volo-go/pkg/plugin"
)

//go:embed builtin/volo-studio/theme.json builtin/volo-studio/preview.svg builtin/volo-studio/templates/*.html builtin/volo-studio/assets/*
var builtinThemeFS embed.FS

const DefaultThemeID = "volo-studio"

// ConfigOption defines a selectable item for a `select` theme setting.
type ConfigOption struct {
	Value string            `json:"value"`
	Label map[string]string `json:"label"`
}

// ConfigField defines a declarative per-theme configuration item in `theme.json`.
type ConfigField struct {
	Key         string            `json:"key"`
	Type        string            `json:"type"` // "text" | "textarea" | "url" | "color" | "boolean" | "number" | "select"
	Label       map[string]string `json:"label"`
	Description map[string]string `json:"description,omitempty"`
	Placeholder map[string]string `json:"placeholder,omitempty"`
	Default     any               `json:"default,omitempty"`
	Options     []ConfigOption    `json:"options,omitempty"`
}

// Manifest describes a built-in or external theme (`theme.json`).
type Manifest struct {
	ID               string            `json:"id"`
	Name             map[string]string `json:"name"`
	Version          string            `json:"version"`
	Author           string            `json:"author"`
	Homepage         string            `json:"homepage,omitempty"`
	License          string            `json:"license,omitempty"`
	Preview          string            `json:"preview,omitempty"`
	Description      map[string]string `json:"description"`
	Accents          []string          `json:"accents"`
	SupportsDarkMode bool              `json:"supports_dark_mode"`
	SupportsPJAX     bool              `json:"supports_pjax"`
	ConfigSchema     []ConfigField     `json:"config_schema,omitempty"`
	IsBuiltin        bool              `json:"is_builtin"`
}

// LocalizedConfigOption holds a locale-resolved select option for Admin UI.
type LocalizedConfigOption struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// LocalizedConfigField holds a locale-resolved theme configuration field for Admin UI.
type LocalizedConfigField struct {
	Key         string                  `json:"key"`
	Type        string                  `json:"type"`
	Label       string                  `json:"label"`
	Description string                  `json:"description,omitempty"`
	Placeholder string                  `json:"placeholder,omitempty"`
	Default     any                     `json:"default,omitempty"`
	Options     []LocalizedConfigOption `json:"options,omitempty"`
}

// LocalizedTheme extends Manifest with locale-resolved labels and preview endpoint URL.
type LocalizedTheme struct {
	Manifest
	DisplayName        string                 `json:"display_name"`
	DisplayDescription string                 `json:"display_description"`
	PreviewURL         string                 `json:"preview_url"`
	LocalizedSchema    []LocalizedConfigField `json:"localized_schema,omitempty"`
}

// PaginationView provides windowed page numbers and navigation pointers for templates.
type PaginationView struct {
	CurrentPage int   `json:"current_page"`
	TotalPages  int   `json:"total_pages"`
	PageSize    int   `json:"page_size"`
	TotalItems  int   `json:"total_items"`
	PrevPage    int   `json:"prev_page"`
	NextPage    int   `json:"next_page"`
	PageNumbers []int `json:"page_numbers"`
	ShowFirst   bool  `json:"show_first"`
	ShowLast    bool  `json:"show_last"`
}

// BuildPagination computes a centered sliding window of page numbers.
func BuildPagination(currentPage, totalPages, pageSize, totalItems, windowSize int) PaginationView {
	if totalPages < 1 {
		totalPages = 1
	}
	if currentPage < 1 {
		currentPage = 1
	}
	if currentPage > totalPages {
		currentPage = totalPages
	}
	if windowSize <= 0 {
		windowSize = 5
	}
	half := windowSize / 2
	start := currentPage - half
	end := start + windowSize - 1
	if start < 1 {
		start = 1
		end = start + windowSize - 1
	}
	if end > totalPages {
		end = totalPages
		start = end - windowSize + 1
		if start < 1 {
			start = 1
		}
	}
	nums := make([]int, 0, end-start+1)
	for p := start; p <= end; p++ {
		nums = append(nums, p)
	}
	prev := 0
	if currentPage > 1 {
		prev = currentPage - 1
	}
	next := 0
	if currentPage < totalPages {
		next = currentPage + 1
	}
	return PaginationView{
		CurrentPage: currentPage,
		TotalPages:  totalPages,
		PageSize:    pageSize,
		TotalItems:  totalItems,
		PrevPage:    prev,
		NextPage:    next,
		PageNumbers: nums,
		ShowFirst:   start > 1,
		ShowLast:    end < totalPages,
	}
}

// ArticleSeriesContext describes the current article's position inside a Series.
type ArticleSeriesContext struct {
	Series domain.Series
	Index  int
	Total  int
}

type cachedAsset struct {
	raw         []byte
	gzipped     []byte
	contentType string
	etag        string
	shortHash   string
}

type loadedTheme struct {
	manifest   Manifest
	pages      map[string]*template.Template
	hasOwnPage map[string]bool
	assets     map[string]cachedAsset
	locales    map[string]map[string]string
	preview    cachedAsset
	hasPreview bool
}

// PageContext holds all data passed into theme templates.
type PageContext struct {
	ThemeID         string
	ThemeConfig     map[string]any
	EnablePJAX      bool
	Locale          string
	TextDir         string
	Locales         []i18n.LocaleInfo
	Year            int
	CurrentPath     string
	BasePath        string
	CanonicalURL    string
	PageTitle       string
	MetaDescription string
	OGImage         string
	JSONLD          string
	NoIndex         bool
	IsHome          bool
	IsArticle       bool
	AdminPath       string

	Site       domain.SiteSettings
	Reading    domain.ReadingSettings
	Feed       domain.FeedSettings
	SEO        domain.SEOSettings
	Appearance domain.AppearanceSettings

	Viewer          *domain.User
	ViewerCSRFToken string
	Owner           content.PublicAuthorProfile
	Authors  []content.PublicAuthorProfile
	Stats    content.PublicSiteStats
	NavItems []content.PublicNavNode

	HeroTitle    string
	HeroSubtitle string

	// List & Pagination
	ArticleCards []visibility.PublicArticleCard
	Page         int
	TotalPages   int
	Pagination   PaginationView

	// Article Detail
	Detail        visibility.PublicArticleView
	PrevPost      *visibility.PublicNeighbor
	NextPost      *visibility.PublicNeighbor
	Backlinks     []visibility.PublicNeighbor
	RelatedPosts  []visibility.PublicArticleCard
	RandomPosts   []visibility.PublicArticleCard
	Alternates    []visibility.TranslationAlternate
	SeriesContext *ArticleSeriesContext
	Reactions     map[string]int
	OutdatedDays  int

	// Standalone Page
	StandalonePage content.PublicPageView

	// Taxonomies, Books, Links, Follows
	TaxonomyKind      string
	Categories        []content.CategoryWithCount
	Tags              []content.TagWithCount
	ArchiveBuckets    []content.ArchiveMonth
	SeriesList        []domain.Series
	Books             []domain.Book
	BookDetail        content.BookDetailView
	ActiveChapter     *domain.BookChapter
	ActiveChapterHTML string
	LinksByCategory   map[string][]domain.Link
	FollowGroups      []content.FollowGroup

	// Sidebar & Global Lists
	SidebarCategories  []content.CategoryWithCount
	SidebarTags        []content.TagWithCount
	SidebarArchives    []content.ArchiveMonth
	RecentPosts        []visibility.PublicArticleCard
	MostViewedPosts    []visibility.PublicArticleCard
	MostCommentedPosts []visibility.PublicArticleCard
	RecentComments     []content.RecentCommentItem

	// Plugin Widget Slots
	HeadWidgets    []plugin.RenderedSlot
	SidebarWidgets []plugin.RenderedSlot
	FooterWidgets  []plugin.RenderedSlot

	// Error Page
	StatusCode   int
	ErrorMessage string
}

// Engine manages embedded `volo-studio` and hot-reloadable external `./themes/<id>` packages.
type Engine struct {
	catalog   *i18n.Catalog
	themesDir string

	mu     sync.RWMutex
	themes map[string]*loadedTheme
}

// NewEngine loads the embedded `volo-studio` theme and scans themesDir for external themes.
func NewEngine(catalog *i18n.Catalog, themesDir string) (*Engine, error) {
	e := &Engine{
		catalog:   catalog,
		themesDir: strings.TrimSpace(themesDir),
		themes:    make(map[string]*loadedTheme),
	}
	if err := e.Reload(); err != nil {
		return nil, err
	}
	return e, nil
}

// Reload reloads built-in `volo-studio` and discovers any valid external themes in themesDir.
func (e *Engine) Reload() error {
	next := make(map[string]*loadedTheme)

	subFS, err := fs.Sub(builtinThemeFS, "builtin/volo-studio")
	if err != nil {
		return fmt.Errorf("theme: open embedded volo-studio: %w", err)
	}
	builtinLoaded, err := e.compileThemeFS(subFS, nil, true)
	if err != nil {
		return fmt.Errorf("theme: compile embedded volo-studio: %w", err)
	}
	next[builtinLoaded.manifest.ID] = builtinLoaded

	if e.themesDir != "" {
		entries, err := os.ReadDir(e.themesDir)
		if err == nil {
			for _, ent := range entries {
				if !ent.IsDir() || strings.HasPrefix(ent.Name(), ".") {
					continue
				}
				dirPath := filepath.Join(e.themesDir, ent.Name())
				if st, lErr := os.Lstat(dirPath); lErr != nil || st.Mode()&os.ModeSymlink != 0 {
					continue
				}
				extLoaded, cErr := e.compileThemeFS(os.DirFS(dirPath), subFS, false)
				if cErr == nil && extLoaded.manifest.ID != "" && extLoaded.manifest.ID != DefaultThemeID {
					next[extLoaded.manifest.ID] = extLoaded
				}
			}
		}
	}

	e.mu.Lock()
	e.themes = next
	e.mu.Unlock()
	return nil
}

// List returns all loaded theme manifests sorted with `volo-studio` first.
func (e *Engine) List() []Manifest {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]Manifest, 0, len(e.themes))
	for _, th := range e.themes {
		out = append(out, th.manifest)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ID == DefaultThemeID {
			return true
		}
		if out[j].ID == DefaultThemeID {
			return false
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// ListLocalized returns all loaded themes with locale-resolved names, descriptions, preview URLs, and config schemas.
func (e *Engine) ListLocalized(locale string) []LocalizedTheme {
	manifests := e.List()
	out := make([]LocalizedTheme, 0, len(manifests))
	for _, mf := range manifests {
		dispName := pickLocalizedMapValue(mf.Name, locale, mf.ID)
		dispDesc := pickLocalizedMapValue(mf.Description, locale, "")
		schema := make([]LocalizedConfigField, 0, len(mf.ConfigSchema))
		for _, f := range mf.ConfigSchema {
			opts := make([]LocalizedConfigOption, 0, len(f.Options))
			for _, o := range f.Options {
				opts = append(opts, LocalizedConfigOption{
					Value: o.Value,
					Label: pickLocalizedMapValue(o.Label, locale, o.Value),
				})
			}
			schema = append(schema, LocalizedConfigField{
				Key:         f.Key,
				Type:        f.Type,
				Label:       pickLocalizedMapValue(f.Label, locale, f.Key),
				Description: pickLocalizedMapValue(f.Description, locale, ""),
				Placeholder: pickLocalizedMapValue(f.Placeholder, locale, ""),
				Default:     f.Default,
				Options:     opts,
			})
		}
		out = append(out, LocalizedTheme{
			Manifest:           mf,
			DisplayName:        dispName,
			DisplayDescription: dispDesc,
			PreviewURL:         "/themes/" + mf.ID + "/preview",
			LocalizedSchema:    schema,
		})
	}
	return out
}

// Has reports whether themeID is currently available.
func (e *Engine) Has(themeID string) bool {
	e.mu.RLock()
	_, ok := e.themes[strings.TrimSpace(themeID)]
	e.mu.RUnlock()
	return ok
}

// ResolveThemeConfig merges manifest `config_schema` defaults with persisted `ThemeConfigs[themeID]`.
func (e *Engine) ResolveThemeConfig(themeID string, saved map[string]map[string]any) map[string]any {
	e.mu.RLock()
	th, ok := e.themes[themeID]
	if !ok {
		th = e.themes[DefaultThemeID]
	}
	var schema []ConfigField
	if th != nil {
		schema = th.manifest.ConfigSchema
	}
	e.mu.RUnlock()

	out := make(map[string]any, len(schema)+4)
	for _, field := range schema {
		if field.Key != "" && field.Default != nil {
			out[field.Key] = field.Default
		}
	}
	if saved != nil {
		if perTheme, exists := saved[themeID]; exists {
			for k, v := range perTheme {
				out[k] = v
			}
		}
	}
	return out
}

// ResolveActiveTheme picks `?skin=<id>` or `?theme_preview=<id>` when viewer is Admin/Editor,
// otherwise Appearance.ActiveTheme, falling back cleanly to `volo-studio` if an external theme was removed.
func (e *Engine) ResolveActiveTheme(r *http.Request, viewer *domain.User, configuredID string) string {
	if r != nil && viewer != nil && viewer.Status == domain.UserStatusActive &&
		(viewer.Role == domain.RoleAdmin || viewer.Role == domain.RoleEditor) {
		q := r.URL.Query()
		for _, param := range []string{"skin", "theme_preview"} {
			if candidate := strings.TrimSpace(q.Get(param)); candidate != "" && e.Has(candidate) {
				return candidate
			}
		}
	}
	if cfg := strings.TrimSpace(configuredID); cfg != "" && e.Has(cfg) {
		return cfg
	}
	return DefaultThemeID
}

// Render executes pageName (`index.html`, `article.html`, etc.) and writes HTML to w.
func (e *Engine) Render(w http.ResponseWriter, status int, themeID string, pageName string, data PageContext) error {
	return e.RenderWithFallback(w, status, themeID, []string{pageName}, data)
}

// RenderWithFallback selects the first page template in `candidates` explicitly provided by the active theme,
// or falls back to the last candidate in the compiled template map. Supports both `layout` inheritance (`{{define "content"}}`)
// and standalone full-HTML skin templates (`{{template "header" .}} ... {{template "footer" .}}`).
func (e *Engine) RenderWithFallback(w http.ResponseWriter, status int, themeID string, candidates []string, data PageContext) error {
	if len(candidates) == 0 {
		return fmt.Errorf("theme %s: empty template candidate list", themeID)
	}
	e.mu.RLock()
	th, ok := e.themes[themeID]
	if !ok {
		th = e.themes[DefaultThemeID]
		themeID = DefaultThemeID
	}

	var selected string
	var tmpl *template.Template
	for _, cand := range candidates {
		if th.hasOwnPage[cand] {
			if t, exists := th.pages[cand]; exists {
				selected = cand
				tmpl = t
				break
			}
		}
	}
	if tmpl == nil {
		for _, cand := range candidates {
			if t, exists := th.pages[cand]; exists {
				selected = cand
				tmpl = t
				break
			}
		}
	}
	supportsPJAX := th.manifest.SupportsPJAX
	e.mu.RUnlock()

	if tmpl == nil {
		return fmt.Errorf("theme %s: missing page templates %v", themeID, candidates)
	}
	data.ThemeID = themeID
	data.EnablePJAX = supportsPJAX
	if data.ThemeConfig == nil {
		data.ThemeConfig = e.ResolveThemeConfig(themeID, data.Appearance.ThemeConfigs)
	}

	entryTemplate := selected
	if tmpl.Lookup("content") != nil && tmpl.Lookup("layout") != nil {
		entryTemplate = "layout"
	}

	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, entryTemplate, data); err != nil {
		return fmt.Errorf("theme %s render %s (via %s): %w", themeID, selected, entryTemplate, err)
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if data.NoIndex {
		w.Header().Set("X-Robots-Tag", "noindex, nofollow")
	}
	w.WriteHeader(status)
	_, err := w.Write(buf.Bytes())
	return err
}

// ServePreview serves `/themes/{theme}/preview` with either the theme's custom preview image or a generated SVG card.
func (e *Engine) ServePreview(w http.ResponseWriter, r *http.Request, themeID string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	e.mu.RLock()
	th, ok := e.themes[themeID]
	e.mu.RUnlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	if th.hasPreview {
		serveCachedAsset(w, r, th.preview, false)
		return
	}
	svg := renderThemePreviewSVG(th.manifest)
	w.Header().Set("Content-Type", "image/svg+xml; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write([]byte(svg))
}

// ServeAsset serves `/themes/{theme}/assets/{file}` with pre-compressed Gzip negotiation,
// `Vary: Accept-Encoding`, strong `ETag`, `304 Not Modified`, and immutable cache headers.
func (e *Engine) ServeAsset(w http.ResponseWriter, r *http.Request, themeID string, relAsset string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	cleanRel := strings.TrimPrefix(path.Clean("/"+relAsset), "/")
	e.mu.RLock()
	th, ok := e.themes[themeID]
	var asset cachedAsset
	var found bool
	if ok {
		asset, found = th.assets[cleanRel]
	}
	e.mu.RUnlock()

	if !found {
		http.NotFound(w, r)
		return
	}
	serveCachedAsset(w, r, asset, r.URL.Query().Get("v") != "")
}

func serveCachedAsset(w http.ResponseWriter, r *http.Request, asset cachedAsset, immutable bool) {
	w.Header().Set("Content-Type", asset.contentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Vary", "Accept-Encoding")
	w.Header().Set("ETag", asset.etag)
	if immutable {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "public, max-age=3600")
	}

	if r.Header.Get("If-None-Match") == asset.etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	useGzip := len(asset.gzipped) > 0 && acceptsGzip(r.Header.Get("Accept-Encoding"))
	payload := asset.raw
	if useGzip {
		w.Header().Set("Content-Encoding", "gzip")
		payload = asset.gzipped
	}
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(payload)
}

// GzippedAssetSize returns the pre-compressed Gzip byte size of a theme asset (used in budget tests).
func (e *Engine) GzippedAssetSize(themeID string, relAsset string) (int, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	th, ok := e.themes[themeID]
	if !ok {
		return 0, false
	}
	a, ok := th.assets[relAsset]
	if !ok {
		return 0, false
	}
	return len(a.gzipped), true
}

func (e *Engine) compileThemeFS(fsys fs.FS, fallbackFS fs.FS, isBuiltin bool) (*loadedTheme, error) {
	rawManifest, err := fs.ReadFile(fsys, "theme.json")
	if err != nil {
		return nil, err
	}
	var mf Manifest
	if err := json.Unmarshal(rawManifest, &mf); err != nil {
		return nil, err
	}
	mf.IsBuiltin = isBuiltin
	if mf.ID == "" {
		return nil, fmt.Errorf("theme.json missing id")
	}

	assets := make(map[string]cachedAsset)
	loadAssetsFrom := func(srcFS fs.FS) {
		if srcFS == nil {
			return
		}
		_ = fs.WalkDir(srcFS, "assets", func(p string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil || d.IsDir() {
				return nil
			}
			raw, rErr := fs.ReadFile(srcFS, p)
			if rErr != nil {
				return nil
			}
			rel := strings.TrimPrefix(p, "assets/")
			assets[rel] = buildCachedAsset(raw, rel)
			return nil
		})
	}
	loadAssetsFrom(fallbackFS)
	loadAssetsFrom(fsys)

	// Load optional preview image (configured `preview` path or root `preview.{svg,png,webp,jpg}`)
	var previewAsset cachedAsset
	var hasPreview bool
	previewCandidates := []string{}
	if mf.Preview != "" {
		previewCandidates = append(previewCandidates, strings.TrimPrefix(path.Clean("/"+mf.Preview), "/"))
	}
	previewCandidates = append(previewCandidates, "preview.svg", "preview.png", "preview.webp", "preview.jpg", "assets/preview.svg", "assets/preview.png")
	for _, cand := range previewCandidates {
		if rawPrev, pErr := fs.ReadFile(fsys, cand); pErr == nil && len(rawPrev) > 0 {
			previewAsset = buildCachedAsset(rawPrev, cand)
			hasPreview = true
			break
		}
	}

	// Load per-theme JSON language packs (`locales/*.json`)
	themeLocales := make(map[string]map[string]string)
	loadLocalesFrom := func(srcFS fs.FS) {
		if srcFS == nil {
			return
		}
		entries, dErr := fs.ReadDir(srcFS, "locales")
		if dErr != nil {
			return
		}
		for _, ent := range entries {
			if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".json") {
				continue
			}
			code := strings.TrimSuffix(ent.Name(), ".json")
			rawLoc, rErr := fs.ReadFile(srcFS, "locales/"+ent.Name())
			if rErr != nil {
				continue
			}
			var rawMap map[string]any
			if jErr := json.Unmarshal(rawLoc, &rawMap); jErr != nil {
				continue
			}
			if themeLocales[code] == nil {
				themeLocales[code] = make(map[string]string, len(rawMap))
			}
			for k, v := range rawMap {
				if k == "_meta" || strings.HasPrefix(k, "$") {
					continue
				}
				if s, isStr := v.(string); isStr {
					themeLocales[code][k] = s
				}
			}
		}
	}
	loadLocalesFrom(fallbackFS)
	loadLocalesFrom(fsys)

	funcs := e.templateFuncMap(mf.ID, assets, themeLocales)
	base := template.New("root").Funcs(funcs)

	// Parse shared layout, macro, and modular partial templates in deterministic order (`fallbackFS` then `fsys` override)
	parseSharedFrom := func(srcFS fs.FS) error {
		if srcFS == nil {
			return nil
		}
		var sharedPaths []string
		_ = fs.WalkDir(srcFS, "templates", func(p string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil || d.IsDir() || !strings.HasSuffix(p, ".html") {
				return nil
			}
			rel := strings.TrimPrefix(p, "templates/")
			if isSharedTemplateRel(rel) {
				sharedPaths = append(sharedPaths, p)
			}
			return nil
		})
		sort.Strings(sharedPaths)
		for _, sp := range sharedPaths {
			b, rErr := fs.ReadFile(srcFS, sp)
			if rErr != nil {
				return rErr
			}
			if _, pErr := base.Parse(string(b)); pErr != nil {
				return fmt.Errorf("parse %s: %w", sp, pErr)
			}
		}
		return nil
	}
	if err := parseSharedFrom(fallbackFS); err != nil {
		return nil, err
	}
	if err := parseSharedFrom(fsys); err != nil {
		return nil, err
	}

	pageNames := make(map[string]struct{})
	hasOwnPage := make(map[string]bool)
	collectPages := func(srcFS fs.FS, isOwn bool) {
		if srcFS == nil {
			return
		}
		if tplEntries, dErr := fs.ReadDir(srcFS, "templates"); dErr == nil {
			for _, ent := range tplEntries {
				name := ent.Name()
				if ent.IsDir() || !strings.HasSuffix(name, ".html") || isSharedTemplateRel(name) {
					continue
				}
				pageNames[name] = struct{}{}
				if isOwn {
					hasOwnPage[name] = true
				}
			}
		}
	}
	collectPages(fallbackFS, false)
	collectPages(fsys, true)
	if len(pageNames) == 0 {
		return nil, fmt.Errorf("theme %s: no page templates found", mf.ID)
	}

	readTemplateFile := func(relPath string) ([]byte, error) {
		if b, rErr := fs.ReadFile(fsys, relPath); rErr == nil {
			return b, nil
		}
		if fallbackFS != nil {
			return fs.ReadFile(fallbackFS, relPath)
		}
		return nil, fs.ErrNotExist
	}

	compiledPages := make(map[string]*template.Template, len(pageNames))
	for name := range pageNames {
		rawTpl, rErr := readTemplateFile("templates/" + name)
		if rErr != nil {
			return nil, rErr
		}
		cloned, cErr := base.Clone()
		if cErr != nil {
			return nil, cErr
		}
		if _, pErr := cloned.New(name).Parse(string(rawTpl)); pErr != nil {
			return nil, fmt.Errorf("parse page %s: %w", name, pErr)
		}
		compiledPages[name] = cloned
	}

	return &loadedTheme{
		manifest:   mf,
		pages:      compiledPages,
		hasOwnPage: hasOwnPage,
		assets:     assets,
		locales:    themeLocales,
		preview:    previewAsset,
		hasPreview: hasPreview,
	}, nil
}

func isSharedTemplateRel(rel string) bool {
	if strings.Contains(rel, "/") {
		return strings.HasPrefix(rel, "partials/") || strings.HasPrefix(rel, "common/") || strings.HasPrefix(rel, "macros/")
	}
	return rel == "layout.html" ||
		rel == "partials.html" ||
		strings.HasPrefix(rel, "_") ||
		strings.HasPrefix(rel, "macro-") ||
		strings.HasPrefix(rel, "macro_")
}

func buildCachedAsset(raw []byte, rel string) cachedAsset {
	sum := sha256.Sum256(raw)
	short := hex.EncodeToString(sum[:4])
	etag := `"` + hex.EncodeToString(sum[:12]) + `"`

	var gzBuf bytes.Buffer
	gzW, _ := gzip.NewWriterLevel(&gzBuf, gzip.BestCompression)
	_, _ = gzW.Write(raw)
	_ = gzW.Close()

	ctype := mime.TypeByExtension(filepath.Ext(rel))
	if ctype == "" {
		ctype = "application/octet-stream"
	}
	return cachedAsset{
		raw:         raw,
		gzipped:     gzBuf.Bytes(),
		contentType: ctype,
		etag:        etag,
		shortHash:   short,
	}
}

func pickLocalizedMapValue(m map[string]string, locale string, fallback string) string {
	if len(m) == 0 {
		return fallback
	}
	norm := strings.TrimSpace(locale)
	if v, ok := m[norm]; ok && strings.TrimSpace(v) != "" {
		return v
	}
	base := strings.ToLower(norm)
	if idx := strings.IndexAny(base, "-_"); idx > 0 {
		base = base[:idx]
	}
	for k, v := range m {
		kl := strings.ToLower(k)
		if (kl == strings.ToLower(norm) || strings.HasPrefix(kl, base+"-") || kl == base) && strings.TrimSpace(v) != "" {
			return v
		}
	}
	for _, pref := range []string{"zh-CN", "en-US"} {
		if v, ok := m[pref]; ok && strings.TrimSpace(v) != "" {
			return v
		}
	}
	for _, v := range m {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return fallback
}

func renderThemePreviewSVG(mf Manifest) string {
	title := pickLocalizedMapValue(mf.Name, "zh-CN", mf.ID)
	bg, fg, accent := "#f6f5f0", "#1f5c48", "#e5a93c"
	if mf.ID != DefaultThemeID {
		sum := sha256.Sum256([]byte("preview:" + mf.ID))
		palettes := [][3]string{
			{"#f7f3eb", "#7c5228", "#c8955c"},
			{"#eef4f7", "#235775", "#6ca6c7"},
			{"#f3eff6", "#5c3d75", "#a482bf"},
			{"#eef5f2", "#226354", "#6bb3a0"},
		}
		p := palettes[int(sum[0])%len(palettes)]
		bg, fg, accent = p[0], p[1], p[2]
	}
	return fmt.Sprintf(
		`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 480 270" role="img"><rect width="480" height="270" fill="%s"/><rect x="24" y="20" width="432" height="34" rx="8" fill="#ffffff" opacity="0.88"/><circle cx="44" cy="37" r="7" fill="%s"/><rect x="60" y="32" width="96" height="10" rx="5" fill="%s" opacity="0.75"/><rect x="24" y="68" width="286" height="178" rx="12" fill="#ffffff" opacity="0.92"/><rect x="324" y="68" width="132" height="178" rx="12" fill="#ffffff" opacity="0.82"/><rect x="44" y="90" width="180" height="14" rx="6" fill="%s"/><rect x="44" y="116" width="242" height="8" rx="4" fill="#8b948e" opacity="0.45"/><rect x="44" y="132" width="218" height="8" rx="4" fill="#8b948e" opacity="0.35"/><rect x="44" y="164" width="76" height="22" rx="11" fill="%s" opacity="0.28"/><text x="44" y="226" fill="%s" font-size="15" font-weight="700" font-family="system-ui,sans-serif">%s · v%s</text></svg>`,
		bg, fg, fg, fg, accent, fg, html.EscapeString(title), html.EscapeString(mf.Version),
	)
}

func renderDeterministicCoverSVG(slug string, isProtected bool) template.HTML {
	sum := sha256.Sum256([]byte("volo-cover:" + slug))
	rotA := 6 + int(sum[0])%6
	rotB := -(5 + int(sum[1])%5)
	lockBadge := ""
	if isProtected {
		lockBadge = `<circle cx="320" cy="180" r="34" fill="var(--volo-surface, #fff)" stroke="var(--volo-accent, #246b51)" stroke-width="2.5"/><text x="320" y="191" text-anchor="middle" font-size="28" font-family="sans-serif">🔒</text>`
	}
	svg := fmt.Sprintf(
		`<svg class="studio-cover-art" xmlns="http://www.w3.org/2000/svg" viewBox="0 0 640 360" aria-hidden="true" focusable="false"><circle class="studio-art-wash" cx="490" cy="70" r="110"/><circle class="studio-art-wash" cx="115" cy="340" r="95"/><g class="studio-art-paper" transform="rotate(%d 320 180)"><rect x="211" y="82" width="240" height="204" rx="20"/></g><g class="studio-art-sheet" transform="rotate(%d 320 180)"><rect x="185" y="70" width="246" height="208" rx="20"/><path class="studio-art-ink" d="M230 126h64m-64 40h154m-154 33h118m-118 33h86"/></g><path class="studio-art-spark" d="M474 232v28m-14-14h28M154 95v16m-8-8h16"/>%s</svg>`,
		rotA, rotB, lockBadge,
	)
	return template.HTML(svg)
}

func acceptsGzip(hdr string) bool {
	for _, part := range strings.Split(strings.ToLower(hdr), ",") {
		seg := strings.TrimSpace(strings.Split(part, ";")[0])
		if seg == "gzip" {
			return true
		}
	}
	return false
}
