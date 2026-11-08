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
	"io"
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

//go:embed builtin/volo-studio/theme.json builtin/volo-studio/preview.svg builtin/volo-studio/templates_gz/* builtin/volo-studio/assets_gz/*
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
	Type        string            `json:"type"` // "text" | "string" | "textarea" | "url" | "color" | "image" | "boolean" | "number" | "select" | "sortable_list"
	Group       map[string]string `json:"group,omitempty"`
	Label       map[string]string `json:"label"`
	Description map[string]string `json:"description,omitempty"`
	Placeholder map[string]string `json:"placeholder,omitempty"`
	Required    bool              `json:"required,omitempty"`
	Min         *float64          `json:"min,omitempty"`
	Max         *float64          `json:"max,omitempty"`
	Step        *float64          `json:"step,omitempty"`
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
	PageTemplates    []ConfigOption    `json:"page_templates,omitempty"`
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
	Group       string                  `json:"group,omitempty"`
	Label       string                  `json:"label"`
	Description string                  `json:"description,omitempty"`
	Placeholder string                  `json:"placeholder,omitempty"`
	Required    bool                    `json:"required,omitempty"`
	Min         *float64                `json:"min,omitempty"`
	Max         *float64                `json:"max,omitempty"`
	Step        *float64                `json:"step,omitempty"`
	Default     any                     `json:"default,omitempty"`
	Options     []LocalizedConfigOption `json:"options,omitempty"`
}

// LocalizedTheme extends Manifest with locale-resolved labels and preview endpoint URL.
type LocalizedTheme struct {
	Manifest
	DisplayName            string                  `json:"display_name"`
	DisplayDescription     string                  `json:"display_description"`
	PreviewURL             string                  `json:"preview_url"`
	LocalizedPageTemplates []LocalizedConfigOption `json:"localized_page_templates,omitempty"`
	LocalizedSchema        []LocalizedConfigField  `json:"localized_schema,omitempty"`
}

// OIDCProviderView describes an active OIDC SSO provider button on login views.
type OIDCProviderView struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	StartURL    string `json:"start_url"`
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

// SeriesEpisodeItem represents one episode row inside an ArticleSeriesContext.
type SeriesEpisodeItem struct {
	ID        string
	Slug      string
	Title     string
	Order     int
	IsCurrent bool
}

// ArticleSeriesContext describes the current article's position inside a Series.
type ArticleSeriesContext struct {
	Series       domain.Series
	Index        int
	Total        int
	Episodes     []SeriesEpisodeItem
	PrevInSeries *visibility.PublicNeighbor
	NextInSeries *visibility.PublicNeighbor
}

// ArchivePostEntry holds one compact post row for the paginated /archives page.
type ArchivePostEntry struct {
	Slug         string
	Title        string
	CategoryName string
	CategorySlug string
	PublishedAt  int64
	MonthDay     string
	YearMonth    string
}

// ArchiveMonthGroup groups archive posts under a YYYY-MM bucket.
type ArchiveMonthGroup struct {
	YearMonth string
	Month     string
	Count     int
	Posts     []ArchivePostEntry
}

// ArchiveYearGroup groups archive month buckets under a calendar year.
type ArchiveYearGroup struct {
	Year   string
	Count  int
	Months []ArchiveMonthGroup
}

// ArchiveYearStat summarizes the post count for a year filter chip on /archives.
type ArchiveYearStat struct {
	Year  string
	Count int
}

// LinkCategoryGroup holds a deterministic ordered category section on /links.
type LinkCategoryGroup struct {
	Category string
	Items    []domain.Link
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
	fsys       fs.FS
	fallbackFS fs.FS
	pageNames  map[string]struct{}
	hasOwnPage map[string]bool

	mu          sync.Mutex
	assetsReady bool
	assets      map[string]cachedAsset
	preview     cachedAsset
	hasPreview  bool

	baseReady  bool
	sharedSrcs []string
	bases      map[string]*template.Template
	locales    map[string]map[string]string
	pages      map[string]*template.Template
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
	RawQuery        string
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
	OIDCProviders   []OIDCProviderView
	Owner           content.PublicAuthorProfile
	Authors         []content.PublicAuthorProfile
	Stats           content.PublicSiteStats
	NavItems        []content.PublicNavNode
	NavMenu         NavMenu
	NavMenuResolved bool

	HeroTitle     string
	HeroSubtitle  string
	HeroBackURL   string
	HeroBackLabel string

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
	IsStandalonePage bool
	StandalonePage   content.PublicPageView

	// Taxonomies, Books, Links, Follows
	TaxonomyKind           string
	Categories             []content.CategoryWithCount
	Tags                   []content.TagWithCount
	TagQuery               string
	TagSort                string
	ArchiveBuckets         []content.ArchiveMonth
	SelectedArchiveYear    string
	ArchiveYears           []ArchiveYearStat
	ArchiveGroups          []ArchiveYearGroup
	SeriesList             []domain.Series
	Books                     []domain.Book
	BookDetail                content.BookDetailView
	ActiveChapter             *domain.BookChapter
	ActiveChapterHTML         string
	ActiveChapterNeedsMath    bool
	ActiveChapterNeedsMermaid bool
	BookIframeAllowSameOrigin bool
	PrevChapter               *domain.BookChapter
	NextChapter               *domain.BookChapter
	ReaderNextURL             string
	ReaderAuthError           string
	BookLocked                bool
	BookUnlockError           string
	BookArticleID             string
	BookArticleSlug           string
	LinksByCategory           map[string][]domain.Link
	LinkGroups                []LinkCategoryGroup
	FollowGroups              []content.FollowGroup

	// Sidebar & Global Lists
	SidebarCategories  []content.CategoryWithCount
	SidebarTags        []content.TagWithCount
	SidebarArchives    []content.ArchiveMonth
	RecentPosts        []visibility.PublicArticleCard
	MostViewedPosts    []visibility.PublicArticleCard
	MostCommentedPosts []visibility.PublicArticleCard
	RecentComments     []content.RecentCommentItem

	// Plugin Widget Slots
	HeadWidgets          []plugin.RenderedSlot
	HeaderWidgets        []plugin.RenderedSlot
	ArticleBeforeWidgets []plugin.RenderedSlot
	ArticleAfterWidgets  []plugin.RenderedSlot
	CommentBeforeWidgets []plugin.RenderedSlot
	CommentAfterWidgets  []plugin.RenderedSlot
	SidebarWidgets       []plugin.RenderedSlot
	FooterWidgets        []plugin.RenderedSlot

	// Error Page
	StatusCode   int
	ErrorMessage string
}

// Engine manages embedded `volo-studio` and hot-reloadable external `./themes/<id>` packages.
type Engine struct {
	catalog        *i18n.Catalog
	themesDir      string
	cdnPrefixFn    func() string
	vendorPrefixFn func() string

	mu     sync.RWMutex
	themes map[string]*loadedTheme
}

// SetAssetCDNPrefixFunc registers an optional dynamic getter for Reading.AssetCDNPrefix.
func (e *Engine) SetAssetCDNPrefixFunc(fn func() string) {
	e.mu.Lock()
	e.cdnPrefixFn = fn
	e.mu.Unlock()
}

// SetVendorCDNPrefixFunc registers an optional dynamic getter for Reading.VendorCDNPrefix.
func (e *Engine) SetVendorCDNPrefixFunc(fn func() string) {
	e.mu.Lock()
	e.vendorPrefixFn = fn
	e.mu.Unlock()
}

func (e *Engine) assetCDNPrefix() string {
	e.mu.RLock()
	fn := e.cdnPrefixFn
	e.mu.RUnlock()
	if fn == nil {
		return ""
	}
	return strings.TrimRight(strings.TrimSpace(fn()), "/")
}

func (e *Engine) vendorCDNPrefix() string {
	e.mu.RLock()
	fn := e.vendorPrefixFn
	e.mu.RUnlock()
	if fn != nil {
		if v := strings.TrimRight(strings.TrimSpace(fn()), "/"); v != "" {
			return v
		}
	}
	return domain.DefaultVendorCDNPrefix
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

// Reload discovers built-in `volo-studio` and any valid external themes in themesDir,
// validating manifests and template layout cheaply while deferring template AST and gzip asset compilation until first use.
func (e *Engine) Reload() error {
	next := make(map[string]*loadedTheme)

	subFS, err := fs.Sub(builtinThemeFS, "builtin/volo-studio")
	if err != nil {
		return fmt.Errorf("theme: open embedded volo-studio: %w", err)
	}
	builtinLoaded, err := e.inspectThemeFS(subFS, nil, true)
	if err != nil {
		return fmt.Errorf("theme: inspect embedded volo-studio: %w", err)
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
				extLoaded, cErr := e.inspectThemeFS(os.DirFS(dirPath), subFS, false)
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

func (e *Engine) inspectThemeFS(fsys fs.FS, fallbackFS fs.FS, isBuiltin bool) (*loadedTheme, error) {
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
	validSchema, err := validateThemeConfigSchema(mf.ConfigSchema)
	if err != nil {
		return nil, err
	}
	mf.ConfigSchema = validSchema

	pageNames := make(map[string]struct{})
	hasOwnPage := make(map[string]bool)
	collectPages := func(srcFS fs.FS, isOwn bool) {
		walkThemeTemplates(srcFS, func(rel string) {
			if strings.Contains(rel, "/") || isSharedTemplateRel(rel) {
				return
			}
			pageNames[rel] = struct{}{}
			if isOwn {
				hasOwnPage[rel] = true
			}
		})
	}
	collectPages(fallbackFS, false)
	collectPages(fsys, true)
	if !hasOwnPage["index.html"] && hasOwnPage["home.html"] {
		pageNames["index.html"] = struct{}{}
		hasOwnPage["index.html"] = true
	}
	if len(pageNames) == 0 {
		return nil, fmt.Errorf("theme %s: no page templates found", mf.ID)
	}

	return &loadedTheme{
		manifest:   mf,
		fsys:       fsys,
		fallbackFS: fallbackFS,
		pageNames:  pageNames,
		hasOwnPage: hasOwnPage,
		pages:      make(map[string]*template.Template),
	}, nil
}

func walkThemeTemplates(srcFS fs.FS, visit func(rel string)) {
	if srcFS == nil {
		return
	}
	seen := make(map[string]struct{})
	_ = fs.WalkDir(srcFS, "templates", func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() || !strings.HasSuffix(p, ".html") {
			return nil
		}
		rel := strings.TrimPrefix(p, "templates/")
		seen[rel] = struct{}{}
		visit(rel)
		return nil
	})
	_ = fs.WalkDir(srcFS, "templates_gz", func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() || !strings.HasSuffix(p, ".html.gz") {
			return nil
		}
		rel := strings.TrimSuffix(strings.TrimPrefix(p, "templates_gz/"), ".gz")
		if _, dup := seen[rel]; dup {
			return nil
		}
		seen[rel] = struct{}{}
		visit(rel)
		return nil
	})
}

func readThemeTemplateFileExact(srcFS fs.FS, rel string) ([]byte, error) {
	if srcFS == nil {
		return nil, fs.ErrNotExist
	}
	if b, err := fs.ReadFile(srcFS, "templates/"+rel); err == nil {
		return b, nil
	}
	gz, err := fs.ReadFile(srcFS, "templates_gz/"+rel+".gz")
	if err != nil {
		return nil, err
	}
	gr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		return nil, err
	}
	defer gr.Close()
	return io.ReadAll(io.LimitReader(gr, maxExternalAssetSingleBytes))
}

func readThemeTemplateFile(srcFS fs.FS, rel string) ([]byte, error) {
	b, err := readThemeTemplateFileExact(srcFS, rel)
	if err == nil {
		return b, nil
	}
	if rel == "index.html" {
		if hb, hErr := readThemeTemplateFileExact(srcFS, "home.html"); hErr == nil {
			return hb, nil
		}
	}
	return nil, err
}

const (
	maxExternalAssetSingleBytes = 5 << 20  // 5 MiB
	maxExternalAssetTotalBytes  = 20 << 20 // 20 MiB
	maxPooledRenderBufCap       = 64 << 10 // 64 KiB
)

var renderBufPool = sync.Pool{
	New: func() any {
		return bytes.NewBuffer(make([]byte, 0, 16<<10))
	},
}

func readBoundedThemeFile(fsys fs.FS, name string, enforceLimit bool, maxSingle int64, totalUsed *int64, maxTotal int64) ([]byte, bool) {
	if !enforceLimit {
		raw, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, false
		}
		return raw, true
	}
	f, err := fsys.Open(name)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	if info, sErr := f.Stat(); sErr == nil {
		if info.IsDir() || info.Size() > maxSingle || *totalUsed+info.Size() > maxTotal {
			return nil, false
		}
	}
	rem := maxTotal - *totalUsed
	limit := maxSingle
	if rem < limit {
		limit = rem
	}
	if limit < 0 {
		return nil, false
	}
	raw, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(raw)) > limit {
		return nil, false
	}
	*totalUsed += int64(len(raw))
	return raw, true
}

func (e *Engine) ensureAssets(th *loadedTheme) map[string]cachedAsset {
	if th == nil {
		return nil
	}
	th.mu.Lock()
	defer th.mu.Unlock()
	if th.assetsReady {
		return th.assets
	}

	var gzBuf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&gzBuf, gzip.BestCompression)
	if err != nil {
		th.assets = make(map[string]cachedAsset)
		th.assetsReady = true
		return th.assets
	}

	enforceBounds := !th.manifest.IsBuiltin
	var totalBytes int64

	assets := make(map[string]cachedAsset)
	if th.fsys != nil {
		_ = fs.WalkDir(th.fsys, "assets_gz", func(p string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil || d.IsDir() || !strings.HasSuffix(p, ".gz") {
				return nil
			}
			gzBytes, ok := readBoundedThemeFile(th.fsys, p, enforceBounds, maxExternalAssetSingleBytes, &totalBytes, maxExternalAssetTotalBytes)
			if !ok || len(gzBytes) == 0 {
				return nil
			}
			rel := strings.TrimSuffix(strings.TrimPrefix(p, "assets_gz/"), ".gz")
			assets[rel] = buildPrecompressedAsset(gzBytes, rel)
			return nil
		})
		_ = fs.WalkDir(th.fsys, "assets", func(p string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil || d.IsDir() {
				return nil
			}
			raw, ok := readBoundedThemeFile(th.fsys, p, enforceBounds, maxExternalAssetSingleBytes, &totalBytes, maxExternalAssetTotalBytes)
			if !ok {
				return nil
			}
			rel := strings.TrimPrefix(p, "assets/")
			assets[rel] = buildCachedAssetWithWriter(raw, rel, zw, &gzBuf)
			return nil
		})
	}

	var previewAsset cachedAsset
	var hasPreview bool
	var previewCandidates []string
	if th.manifest.Preview != "" {
		previewCandidates = append(previewCandidates, strings.TrimPrefix(path.Clean("/"+th.manifest.Preview), "/"))
	}
	previewCandidates = append(previewCandidates, "preview.svg", "preview.png", "preview.webp", "preview.jpg", "assets/preview.svg", "assets/preview.png")
	if th.fsys != nil {
		for _, cand := range previewCandidates {
			if rawPrev, ok := readBoundedThemeFile(th.fsys, cand, enforceBounds, maxExternalAssetSingleBytes, &totalBytes, maxExternalAssetTotalBytes); ok && len(rawPrev) > 0 {
				previewAsset = buildCachedAssetWithWriter(rawPrev, cand, zw, &gzBuf)
				hasPreview = true
				break
			}
		}
	}

	th.assets = assets
	th.preview = previewAsset
	th.hasPreview = hasPreview
	th.assetsReady = true
	return assets
}

func (e *Engine) getOrCompilePage(th *loadedTheme, defTh *loadedTheme, selected string, activeLocale string, siteTimeZone string) (*template.Template, error) {
	var fallbackAssets map[string]cachedAsset
	if th.fallbackFS != nil && defTh != nil && defTh != th {
		fallbackAssets = e.ensureAssets(defTh)
	}
	ownAssets := e.ensureAssets(th)

	normLocale := strings.TrimSpace(activeLocale)
	if normLocale == "" {
		normLocale = "zh-CN"
	}
	siteLoc := LoadSiteLocation(siteTimeZone)
	normTZ := siteLoc.String()
	baseKey := normLocale + "\x00" + normTZ
	pageKey := selected + "\x00" + baseKey

	th.mu.Lock()
	defer th.mu.Unlock()
	if t, ok := th.pages[pageKey]; ok {
		return t, nil
	}

	if !th.baseReady {
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
		loadLocalesFrom(th.fallbackFS)
		loadLocalesFrom(th.fsys)

		var sharedSrcs []string
		collectSharedFrom := func(srcFS fs.FS) error {
			if srcFS == nil {
				return nil
			}
			var sharedRels []string
			walkThemeTemplates(srcFS, func(rel string) {
				if isSharedTemplateRel(rel) {
					sharedRels = append(sharedRels, rel)
				}
			})
			sort.Strings(sharedRels)
			for _, sr := range sharedRels {
				b, rErr := readThemeTemplateFile(srcFS, sr)
				if rErr != nil {
					return rErr
				}
				sharedSrcs = append(sharedSrcs, string(b))
			}
			return nil
		}
		if err := collectSharedFrom(th.fallbackFS); err != nil {
			return nil, err
		}
		if err := collectSharedFrom(th.fsys); err != nil {
			return nil, err
		}
		th.locales = themeLocales
		th.sharedSrcs = sharedSrcs
		th.bases = make(map[string]*template.Template)
		th.baseReady = true
	}

	if th.bases == nil {
		th.bases = make(map[string]*template.Template)
	}
	base, hasBase := th.bases[baseKey]
	if !hasBase {
		if len(th.bases) >= 32 {
			clear(th.bases)
		}
		funcs := e.templateFuncMap(th.manifest.ID, ownAssets, fallbackAssets, th.locales, normLocale, siteLoc)
		base = template.New("root").Funcs(funcs)
		for _, src := range th.sharedSrcs {
			if _, pErr := base.Parse(src); pErr != nil {
				return nil, fmt.Errorf("parse shared template: %w", pErr)
			}
		}
		th.bases[baseKey] = base
	}

	var rawTpl []byte
	var rErr error
	if th.fsys != nil {
		rawTpl, rErr = readThemeTemplateFile(th.fsys, selected)
	}
	if (th.fsys == nil || rErr != nil) && th.fallbackFS != nil {
		rawTpl, rErr = readThemeTemplateFile(th.fallbackFS, selected)
	}
	if rErr != nil {
		return nil, rErr
	}

	cloned, cErr := base.Clone()
	if cErr != nil {
		return nil, cErr
	}
	if _, pErr := cloned.New(selected).Parse(string(rawTpl)); pErr != nil {
		return nil, fmt.Errorf("parse page %s: %w", selected, pErr)
	}
	if th.pages == nil {
		th.pages = make(map[string]*template.Template)
	}
	if len(th.pages) >= 256 {
		clear(th.pages)
	}
	th.pages[pageKey] = cloned
	return cloned, nil
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

func defaultPageTemplateOptions() []ConfigOption {
	return []ConfigOption{
		{Value: "default", Label: map[string]string{"zh-CN": "标准版式", "en-US": "Default Layout", "zh-TW": "標準版式"}},
		{Value: "wide", Label: map[string]string{"zh-CN": "宽幅展示", "en-US": "Wide Showcase", "zh-TW": "寬幅展示"}},
		{Value: "minimal", Label: map[string]string{"zh-CN": "极简阅读", "en-US": "Minimal Sheet", "zh-TW": "極簡閱讀"}},
	}
}

// ListLocalized returns all loaded themes with locale-resolved names, descriptions, preview URLs, and config schemas.
func (e *Engine) ListLocalized(locale string) []LocalizedTheme {
	manifests := e.List()
	var builtinPageTpls []ConfigOption
	for _, m := range manifests {
		if m.ID == DefaultThemeID && len(m.PageTemplates) > 0 {
			builtinPageTpls = m.PageTemplates
			break
		}
	}
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
				Group:       pickLocalizedMapValue(f.Group, locale, ""),
				Label:       pickLocalizedMapValue(f.Label, locale, f.Key),
				Description: pickLocalizedMapValue(f.Description, locale, ""),
				Placeholder: pickLocalizedMapValue(f.Placeholder, locale, ""),
				Required:    f.Required,
				Min:         f.Min,
				Max:         f.Max,
				Step:        f.Step,
				Default:     f.Default,
				Options:     opts,
			})
		}
		rawPageTpls := mf.PageTemplates
		if len(rawPageTpls) == 0 {
			if len(builtinPageTpls) > 0 {
				rawPageTpls = builtinPageTpls
			} else {
				rawPageTpls = defaultPageTemplateOptions()
			}
			mf.PageTemplates = rawPageTpls
		}
		locPageTpls := make([]LocalizedConfigOption, 0, len(rawPageTpls))
		for _, pt := range rawPageTpls {
			locPageTpls = append(locPageTpls, LocalizedConfigOption{
				Value: pt.Value,
				Label: pickLocalizedMapValue(pt.Label, locale, pt.Value),
			})
		}
		out = append(out, LocalizedTheme{
			Manifest:               mf,
			DisplayName:            dispName,
			DisplayDescription:     dispDesc,
			PreviewURL:             "/themes/" + mf.ID + "/preview",
			LocalizedPageTemplates: locPageTpls,
			LocalizedSchema:        schema,
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

// IsMobileRequest detects mobile phone user-agents and Client Hints (`Sec-CH-UA-Mobile: ?1`).
// Query parameters (`?device=`) are intentionally ignored here so unauthenticated requests cannot spoof device detection.
func IsMobileRequest(r *http.Request) bool {
	if r == nil {
		return false
	}
	if strings.TrimSpace(r.Header.Get("Sec-CH-UA-Mobile")) == "?1" {
		return true
	}
	ua := strings.ToLower(r.Header.Get("User-Agent"))
	if ua == "" {
		return false
	}
	if strings.Contains(ua, "ipad") || strings.Contains(ua, "tablet") {
		return false
	}
	for _, kw := range []string{"mobi", "iphone", "ipod", "android", "windows phone", "harmonyos", "ucbrowser", "micromessenger"} {
		if strings.Contains(ua, kw) {
			return true
		}
	}
	return false
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

// ResolveThemeForRequest resolves the active theme taking into account staff preview params (`?skin`, `?theme_preview`, `?device`),
// separate MobileTheme configuration on mobile requests, and desktop ActiveTheme fallback.
func (e *Engine) ResolveThemeForRequest(r *http.Request, viewer *domain.User, app domain.AppearanceSettings) string {
	isStaff := r != nil && viewer != nil && viewer.Status == domain.UserStatusActive &&
		(viewer.Role == domain.RoleAdmin || viewer.Role == domain.RoleEditor)
	if isStaff {
		q := r.URL.Query()
		for _, param := range []string{"skin", "theme_preview"} {
			if candidate := strings.TrimSpace(q.Get(param)); candidate != "" && e.Has(candidate) {
				return candidate
			}
		}
	}
	isMobile := IsMobileRequest(r)
	if isStaff {
		switch strings.ToLower(strings.TrimSpace(r.URL.Query().Get("device"))) {
		case "mobile", "phone":
			isMobile = true
		case "desktop", "pc":
			isMobile = false
		}
	}
	if mob := strings.TrimSpace(app.MobileTheme); mob != "" && mob != "same" && isMobile && e.Has(mob) {
		return mob
	}
	return e.ResolveActiveTheme(r, viewer, app.ActiveTheme)
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
	defTh := e.themes[DefaultThemeID]
	th, ok := e.themes[themeID]
	if !ok {
		th = defTh
		themeID = DefaultThemeID
	}
	if th == nil {
		e.mu.RUnlock()
		return fmt.Errorf("theme %s: not loaded", themeID)
	}

	var selected string
	for _, cand := range candidates {
		if th.hasOwnPage[cand] {
			selected = cand
			break
		}
		if cand == "index.html" && th.hasOwnPage["home.html"] {
			selected = "home.html"
			break
		}
	}
	if selected == "" {
		for _, cand := range candidates {
			if _, exists := th.pageNames[cand]; exists {
				selected = cand
				break
			}
		}
	}
	supportsPJAX := th.manifest.SupportsPJAX
	e.mu.RUnlock()

	if selected == "" {
		return fmt.Errorf("theme %s: missing page templates %v", themeID, candidates)
	}
	tmpl, err := e.getOrCompilePage(th, defTh, selected, data.Locale, data.Site.TimeZone)
	if err != nil {
		return fmt.Errorf("theme %s compile %s: %w", themeID, selected, err)
	}
	data.ThemeID = themeID
	data.EnablePJAX = supportsPJAX && data.Reading.EnablePJAX
	if data.ThemeConfig == nil {
		data.ThemeConfig = e.ResolveThemeConfig(themeID, data.Appearance.ThemeConfigs)
	}
	if !data.NavMenuResolved {
		data.NavMenu = e.ResolveNavMenu(data)
		data.NavMenuResolved = true
	}
	e.prependVendorHeadSlot(&data)

	entryTemplate := selected
	if tmpl.Lookup("content") != nil && tmpl.Lookup("layout") != nil {
		entryTemplate = "layout"
	}

	buf := renderBufPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer func() {
		if buf.Cap() <= maxPooledRenderBufCap {
			buf.Reset()
			renderBufPool.Put(buf)
		}
	}()

	if err := tmpl.ExecuteTemplate(buf, entryTemplate, data); err != nil {
		return fmt.Errorf("theme %s render %s (via %s): %w", themeID, selected, entryTemplate, err)
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if data.NoIndex {
		w.Header().Set("X-Robots-Tag", "noindex, nofollow")
	}
	w.WriteHeader(status)
	_, err = w.Write(buf.Bytes())
	return err
}

func (e *Engine) prependVendorHeadSlot(data *PageContext) {
	if data == nil {
		return
	}
	for _, hw := range data.HeadWidgets {
		if hw.PluginID == "volo-core-vendor" {
			return
		}
	}
	primary := strings.TrimRight(strings.TrimSpace(data.Reading.VendorCDNPrefix), "/")
	if primary == "" {
		primary = e.vendorCDNPrefix()
	}
	fallbacks := []string{
		primary,
		"https://fastly.jsdelivr.net/npm",
		"https://cdn.jsdelivr.net/npm",
		"https://unpkg.com",
	}
	bases := make([]string, 0, len(fallbacks))
	seen := make(map[string]struct{}, len(fallbacks))
	for _, b := range fallbacks {
		b = strings.TrimRight(strings.TrimSpace(b), "/")
		if b == "" {
			continue
		}
		if _, exists := seen[b]; exists {
			continue
		}
		seen[b] = struct{}{}
		bases = append(bases, b)
	}
	basesJSON, err := json.Marshal(bases)
	if err != nil {
		basesJSON = []byte(`["https://cdn.jsdelivr.net/npm"]`)
	}
	safeBasesJS := strings.ReplaceAll(string(basesJSON), "</", `<\/`)

	var sb strings.Builder
	needsVendor := data.Detail.NeedsMath || data.Detail.NeedsMermaid ||
		data.StandalonePage.NeedsMath || data.StandalonePage.NeedsMermaid ||
		data.ActiveChapterNeedsMath || data.ActiveChapterNeedsMermaid
	if needsVendor {
		if origin := extractOriginForPreconnect(primary); origin != "" {
			sb.WriteString(`<link rel="preconnect" href="`)
			sb.WriteString(html.EscapeString(origin))
			sb.WriteString(`" crossorigin>`)
		}
	}
	sb.WriteString(`<script>window.__VOLO_VENDOR__=(function(b){var m={};function n(s){return String(s||"").replace(/^\/+/,"");}return{bases:b,katexCss:"katex@0.16.11/dist/katex.min.css",katexJs:"katex@0.16.11/dist/katex.min.js",katexAutoRenderJs:"katex@0.16.11/dist/contrib/auto-render.min.js",mermaidJs:"mermaid@10/dist/mermaid.min.js",url:function(s){return b[0]+"/"+n(s);},loadStyle:function(id,s){if(document.getElementById(id))return Promise.resolve();var c=n(s);return new Promise(function(ok){(function go(i){if(i>=b.length){ok();return;}var l=document.createElement("link");l.id=id;l.rel="stylesheet";l.href=b[i]+"/"+c;l.onload=function(){ok();};l.onerror=function(){l.remove();go(i+1);};document.head.appendChild(l);})(0);});},loadScript:function(s){var c=n(s);if(m[c])return m[c];m[c]=new Promise(function(ok,err){(function go(i){if(i>=b.length){delete m[c];err(new Error("vendor load failed: "+c));return;}var el=document.createElement("script"),d=false,t=setTimeout(function(){if(d)return;d=true;el.remove();go(i+1);},8000);el.src=b[i]+"/"+c;el.async=true;el.onload=function(){if(d)return;d=true;clearTimeout(t);ok();};el.onerror=function(){if(d)return;d=true;clearTimeout(t);el.remove();go(i+1);};document.head.appendChild(el);})(0);});return m[c];}};})(`)
	sb.WriteString(safeBasesJS)
	sb.WriteString(`);</script>`)

	slot := plugin.RenderedSlot{
		PluginID: "volo-core-vendor",
		Slot:     "head",
		Persist:  true,
		HTML:     sb.String(),
	}
	next := make([]plugin.RenderedSlot, 0, len(data.HeadWidgets)+1)
	next = append(next, slot)
	next = append(next, data.HeadWidgets...)
	data.HeadWidgets = next
}

func extractOriginForPreconnect(prefix string) string {
	low := strings.ToLower(prefix)
	var rest string
	var scheme string
	switch {
	case strings.HasPrefix(low, "https://"):
		scheme = "https://"
		rest = prefix[len("https://"):]
	case strings.HasPrefix(low, "http://"):
		scheme = "http://"
		rest = prefix[len("http://"):]
	case strings.HasPrefix(low, "//"):
		scheme = "https://"
		rest = prefix[2:]
	default:
		return ""
	}
	if idx := strings.IndexByte(rest, '/'); idx >= 0 {
		rest = rest[:idx]
	}
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return ""
	}
	return scheme + rest
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
	_ = e.ensureAssets(th)
	th.mu.Lock()
	hasPreview := th.hasPreview
	preview := th.preview
	th.mu.Unlock()

	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; sandbox")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if hasPreview {
		serveCachedAsset(w, r, preview, false)
		return
	}
	svg := renderThemePreviewSVG(th.manifest)
	w.Header().Set("Content-Type", "image/svg+xml; charset=utf-8")
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
	defTh := e.themes[DefaultThemeID]
	e.mu.RUnlock()
	if !ok {
		http.NotFound(w, r)
		return
	}

	assets := e.ensureAssets(th)
	asset, found := assets[cleanRel]
	if !found && themeID != DefaultThemeID && defTh != nil {
		defAssets := e.ensureAssets(defTh)
		asset, found = defAssets[cleanRel]
	}

	if !found {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Access-Control-Allow-Origin", "*")
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
	if useGzip {
		w.Header().Set("Content-Encoding", "gzip")
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodHead {
			return
		}
		_, _ = w.Write(asset.gzipped)
		return
	}

	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	if asset.raw != nil {
		_, _ = w.Write(asset.raw)
		return
	}
	if len(asset.gzipped) > 0 {
		if gr, err := gzip.NewReader(bytes.NewReader(asset.gzipped)); err == nil {
			_, _ = io.Copy(w, gr)
			_ = gr.Close()
		}
	}
}

// GzippedAssetSize returns the pre-compressed Gzip byte size of a theme asset (used in budget tests).
func (e *Engine) GzippedAssetSize(themeID string, relAsset string) (int, bool) {
	e.mu.RLock()
	th, ok := e.themes[themeID]
	defTh := e.themes[DefaultThemeID]
	e.mu.RUnlock()
	if !ok {
		return 0, false
	}
	assets := e.ensureAssets(th)
	a, ok := assets[relAsset]
	if !ok && themeID != DefaultThemeID && defTh != nil {
		defAssets := e.ensureAssets(defTh)
		a, ok = defAssets[relAsset]
	}
	if !ok {
		return 0, false
	}
	return len(a.gzipped), true
}

func validateThemeConfigSchema(schema []ConfigField) ([]ConfigField, error) {
	if len(schema) == 0 {
		return nil, nil
	}
	out := make([]ConfigField, len(schema))
	for i, f := range schema {
		f.Key = strings.TrimSpace(f.Key)
		if f.Key == "" {
			return nil, fmt.Errorf("theme.json config_schema[%d] missing key", i)
		}
		ft := strings.ToLower(strings.TrimSpace(f.Type))
		if ft == "" {
			ft = "text"
		}
		switch ft {
		case "text", "string", "textarea", "url", "color", "image", "boolean", "number", "select", "sortable_list":
		default:
			return nil, fmt.Errorf("theme.json config_schema[%q] unsupported type %q", f.Key, f.Type)
		}
		f.Type = ft
		out[i] = f
	}
	return out, nil
}

func (e *Engine) validateStagedThemeFS(fsys fs.FS, fallbackFS fs.FS, fallbackTheme *loadedTheme) (Manifest, error) {
	loaded, err := e.inspectThemeFS(fsys, fallbackFS, false)
	if err != nil {
		return Manifest{}, err
	}
	names := make([]string, 0, len(loaded.pageNames))
	for name := range loaded.pageNames {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if _, err := e.getOrCompilePage(loaded, fallbackTheme, name, "zh-CN", ""); err != nil {
			return Manifest{}, err
		}
	}
	return loaded.manifest, nil
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

func buildCachedAssetWithWriter(raw []byte, rel string, zw *gzip.Writer, gzBuf *bytes.Buffer) cachedAsset {
	sum := sha256.Sum256(raw)
	short := hex.EncodeToString(sum[:4])
	etag := `"` + hex.EncodeToString(sum[:12]) + `"`

	gzBuf.Reset()
	zw.Reset(gzBuf)
	_, _ = zw.Write(raw)
	_ = zw.Close()
	gzipped := bytes.Clone(gzBuf.Bytes())

	ctype := mime.TypeByExtension(filepath.Ext(rel))
	if ctype == "" {
		ctype = "application/octet-stream"
	}
	var retainedRaw []byte
	if len(gzipped) == 0 {
		retainedRaw = raw
	}
	return cachedAsset{
		raw:         retainedRaw,
		gzipped:     gzipped,
		contentType: ctype,
		etag:        etag,
		shortHash:   short,
	}
}

func buildPrecompressedAsset(gzBytes []byte, rel string) cachedAsset {
	sum := sha256.Sum256(gzBytes)
	short := hex.EncodeToString(sum[:4])
	etag := `"` + hex.EncodeToString(sum[:12]) + `"`
	ctype := mime.TypeByExtension(filepath.Ext(rel))
	if ctype == "" {
		ctype = "application/octet-stream"
	}
	return cachedAsset{
		raw:         nil,
		gzipped:     gzBytes,
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
