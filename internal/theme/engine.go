package theme

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
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
	"time"

	"github.com/Willh92/Volo-go/internal/content"
	"github.com/Willh92/Volo-go/internal/domain"
	"github.com/Willh92/Volo-go/internal/i18n"
	"github.com/Willh92/Volo-go/internal/visibility"
	"github.com/Willh92/Volo-go/pkg/plugin"
)

//go:embed builtin/volo-studio/theme.json builtin/volo-studio/templates/*.html builtin/volo-studio/assets/*
var builtinThemeFS embed.FS

const DefaultThemeID = "volo-studio"

// Manifest describes a built-in or external theme (`theme.json`).
type Manifest struct {
	ID               string            `json:"id"`
	Name             map[string]string `json:"name"`
	Version          string            `json:"version"`
	Author           string            `json:"author"`
	Description      map[string]string `json:"description"`
	Accents          []string          `json:"accents"`
	SupportsDarkMode bool              `json:"supports_dark_mode"`
	SupportsPJAX     bool              `json:"supports_pjax"`
	IsBuiltin        bool              `json:"is_builtin"`
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
	manifest Manifest
	pages    map[string]*template.Template
	assets   map[string]cachedAsset
}

// PageContext holds all data passed into theme templates.
type PageContext struct {
	ThemeID         string
	EnablePJAX      bool
	Locale          string
	TextDir         string
	Locales         []i18n.LocaleInfo
	CurrentPath     string
	BasePath        string
	CanonicalURL    string
	PageTitle       string
	MetaDescription string
	OGImage         string
	JSONLD          string
	NoIndex         bool
	IsArticle       bool
	AdminPath       string

	Site       domain.SiteSettings
	Reading    domain.ReadingSettings
	Feed       domain.FeedSettings
	SEO        domain.SEOSettings
	Appearance domain.AppearanceSettings

	Viewer   *domain.User
	NavItems []content.PublicNavNode

	HeroTitle    string
	HeroSubtitle string

	// List & Pagination
	ArticleCards []visibility.PublicArticleCard
	Page         int
	TotalPages   int

	// Article Detail
	Detail        visibility.PublicArticleView
	PrevPost      *visibility.PublicNeighbor
	NextPost      *visibility.PublicNeighbor
	Backlinks     []visibility.PublicNeighbor
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

	// Sidebar
	SidebarCategories []content.CategoryWithCount
	SidebarTags       []content.TagWithCount
	RecentComments    []content.RecentCommentItem

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
	builtinLoaded, err := e.compileThemeFS(subFS, true)
	if err != nil {
		return fmt.Errorf("theme: compile embedded volo-studio: %w", err)
	}
	next[builtinLoaded.manifest.ID] = builtinLoaded

	if e.themesDir != "" {
		entries, err := os.ReadDir(e.themesDir)
		if err == nil {
			for _, ent := range entries {
				if !ent.IsDir() {
					continue
				}
				dirPath := filepath.Join(e.themesDir, ent.Name())
				if st, lErr := os.Lstat(dirPath); lErr != nil || st.Mode()&os.ModeSymlink != 0 {
					continue
				}
				extLoaded, cErr := e.compileThemeFS(os.DirFS(dirPath), false)
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

// Has reports whether themeID is currently available.
func (e *Engine) Has(themeID string) bool {
	e.mu.RLock()
	_, ok := e.themes[strings.TrimSpace(themeID)]
	e.mu.RUnlock()
	return ok
}

// ResolveActiveTheme picks `?skin=<id>` when viewer is Admin/Editor, otherwise Appearance.ActiveTheme,
// falling back cleanly to `volo-studio` if an external theme was removed.
func (e *Engine) ResolveActiveTheme(r *http.Request, viewer *domain.User, configuredID string) string {
	if r != nil && viewer != nil && viewer.Status == domain.UserStatusActive &&
		(viewer.Role == domain.RoleAdmin || viewer.Role == domain.RoleEditor) {
		if skin := strings.TrimSpace(r.URL.Query().Get("skin")); skin != "" && e.Has(skin) {
			return skin
		}
	}
	if cfg := strings.TrimSpace(configuredID); cfg != "" && e.Has(cfg) {
		return cfg
	}
	return DefaultThemeID
}

// Render executes pageName (`index.html`, `article.html`, etc.) inside `layout` and writes HTML to w.
func (e *Engine) Render(w http.ResponseWriter, status int, themeID string, pageName string, data PageContext) error {
	e.mu.RLock()
	th, ok := e.themes[themeID]
	if !ok {
		th = e.themes[DefaultThemeID]
		themeID = DefaultThemeID
	}
	tmpl, pageOK := th.pages[pageName]
	supportsPJAX := th.manifest.SupportsPJAX
	e.mu.RUnlock()

	if !pageOK {
		return fmt.Errorf("theme %s: missing page template %s", themeID, pageName)
	}
	data.ThemeID = themeID
	data.EnablePJAX = supportsPJAX

	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "layout", data); err != nil {
		return fmt.Errorf("theme %s render %s: %w", themeID, pageName, err)
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if data.NoIndex {
		w.Header().Set("X-Robots-Tag", "noindex, nofollow")
	}
	w.WriteHeader(status)
	_, err := w.Write(buf.Bytes())
	return err
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

	w.Header().Set("Content-Type", asset.contentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Vary", "Accept-Encoding")
	w.Header().Set("ETag", asset.etag)
	if r.URL.Query().Get("v") != "" {
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

func (e *Engine) compileThemeFS(fsys fs.FS, isBuiltin bool) (*loadedTheme, error) {
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
	_ = fs.WalkDir(fsys, "assets", func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() {
			return nil
		}
		raw, rErr := fs.ReadFile(fsys, p)
		if rErr != nil {
			return nil
		}
		rel := strings.TrimPrefix(p, "assets/")
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
		assets[rel] = cachedAsset{
			raw:         raw,
			gzipped:     gzBuf.Bytes(),
			contentType: ctype,
			etag:        etag,
			shortHash:   short,
		}
		return nil
	})

	funcs := e.templateFuncMap(mf.ID, assets)
	base := template.New("root").Funcs(funcs)
	for _, sharedName := range []string{"templates/layout.html", "templates/partials.html"} {
		if b, rErr := fs.ReadFile(fsys, sharedName); rErr == nil {
			if _, pErr := base.Parse(string(b)); pErr != nil {
				return nil, fmt.Errorf("parse %s: %w", sharedName, pErr)
			}
		}
	}

	tplEntries, err := fs.ReadDir(fsys, "templates")
	if err != nil {
		return nil, err
	}
	compiledPages := make(map[string]*template.Template)
	for _, ent := range tplEntries {
		name := ent.Name()
		if ent.IsDir() || !strings.HasSuffix(name, ".html") || name == "layout.html" || name == "partials.html" {
			continue
		}
		rawTpl, rErr := fs.ReadFile(fsys, "templates/"+name)
		if rErr != nil {
			return nil, rErr
		}
		cloned, cErr := base.Clone()
		if cErr != nil {
			return nil, cErr
		}
		if _, pErr := cloned.Parse(string(rawTpl)); pErr != nil {
			return nil, fmt.Errorf("parse page %s: %w", name, pErr)
		}
		compiledPages[name] = cloned
	}

	return &loadedTheme{
		manifest: mf,
		pages:    compiledPages,
		assets:   assets,
	}, nil
}

func (e *Engine) templateFuncMap(themeID string, assets map[string]cachedAsset) template.FuncMap {
	return template.FuncMap{
		"t": func(locale string, key string, vars ...map[string]any) string {
			if e.catalog == nil {
				return key
			}
			return e.catalog.T(locale, key, vars...)
		},
		"assetURL": func(tid string, rel string) string {
			if tid == "" {
				tid = themeID
			}
			if a, ok := assets[rel]; ok && a.shortHash != "" {
				return fmt.Sprintf("/themes/%s/assets/%s?v=%s", tid, rel, a.shortHash)
			}
			return fmt.Sprintf("/themes/%s/assets/%s", tid, rel)
		},
		"safeHTML": func(s string) template.HTML {
			return template.HTML(s)
		},
		"safeJS": func(s string) template.JS {
			return template.JS(s)
		},
		"fmtDate": func(ms int64) string {
			if ms <= 0 {
				return ""
			}
			return time.UnixMilli(ms).Format("2006-01-02")
		},
		"fmtISO": func(ms int64) string {
			if ms <= 0 {
				return ""
			}
			return time.UnixMilli(ms).UTC().Format(time.RFC3339)
		},
		"hasPrefix": strings.HasPrefix,
		"add":       func(a, b int) int { return a + b },
		"sub":       func(a, b int) int { return a - b },
		"mul":       func(a, b int) int { return a * b },
		"dict": func(values ...any) map[string]any {
			m := make(map[string]any, len(values)/2)
			for i := 0; i+1 < len(values); i += 2 {
				if k, ok := values[i].(string); ok {
					m[k] = values[i+1]
				}
			}
			return m
		},
		"coverSVG": renderDeterministicCoverSVG,
	}
}

func renderDeterministicCoverSVG(slug string, isProtected bool) template.HTML {
	sum := sha256.Sum256([]byte("volo-cover:" + slug))
	palettes := [][3]string{
		{"#e7efe9", "#246b51", "#8cc0a8"},
		{"#e7eff4", "#2d6583", "#8ebcd6"},
		{"#e6f1f1", "#286c70", "#84c2c5"},
		{"#f3ece0", "#855c27", "#cca777"},
		{"#efe9f3", "#70517d", "#b89cc7"},
		{"#f4eaed", "#87525f", "#cfa0ac"},
	}
	p := palettes[int(sum[0])%len(palettes)]
	cx := 60 + int(sum[1])%200
	cy := 40 + int(sum[2])%120
	label := ""
	if isProtected {
		label = `<text x="160" y="114" text-anchor="middle" fill="` + p[1] + `" font-size="26" font-family="sans-serif">🔒</text>`
	}
	svg := fmt.Sprintf(
		`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 320 214" role="img" aria-hidden="true"><rect width="320" height="214" fill="%s"/><circle cx="%d" cy="%d" r="76" fill="%s" opacity="0.35"/><path d="M0 176 Q 110 128 215 168 T 320 152 L 320 214 L 0 214 Z" fill="%s" opacity="0.22"/>%s</svg>`,
		p[0], cx, cy, p[2], p[1], label,
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
