package content

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/Willh92/Volo-go/internal/config"
	"github.com/Willh92/Volo-go/internal/db/dialect"
	"github.com/Willh92/Volo-go/internal/db/sqlx"
	"github.com/Willh92/Volo-go/internal/domain"
	"github.com/Willh92/Volo-go/internal/guest"
	"github.com/Willh92/Volo-go/internal/i18n"
	"github.com/Willh92/Volo-go/internal/media"
	"github.com/Willh92/Volo-go/internal/render"
	"github.com/Willh92/Volo-go/internal/repo"
	"github.com/Willh92/Volo-go/internal/security"
	"github.com/Willh92/Volo-go/internal/visibility"
	"github.com/Willh92/Volo-go/pkg/id"
	"github.com/Willh92/Volo-go/pkg/plugin"
	"github.com/Willh92/Volo-go/pkg/wordfilter"
)

// CategoryWithCount pairs a Category with its published public article count.
type CategoryWithCount struct {
	domain.Category
	ArticleCount int `json:"article_count"`
}

// TagWithCount pairs a Tag with its published public article count.
type TagWithCount struct {
	domain.Tag
	ArticleCount int `json:"article_count"`
}

// ArchiveMonth groups published article counts by YYYY-MM (strictly excluding standalone pages).
type ArchiveMonth struct {
	YearMonth string `json:"year_month"` // YYYY-MM
	Count     int    `json:"count"`
}

// ArchiveYear groups published article counts by calendar year YYYY (strictly excluding standalone pages).
type ArchiveYear struct {
	Year  string `json:"year"` // YYYY
	Count int    `json:"count"`
}

// ArchiveStats holds revision-coherent total, calendar-year (YYYY), and calendar-month (YYYY-MM) published post counts.
type ArchiveStats struct {
	TotalArticles int            `json:"total_articles"`
	Years         []ArchiveYear  `json:"years"`
	Months        []ArchiveMonth `json:"months"`
	YearCounts    map[string]int `json:"year_counts"`
	MonthCounts   map[string]int `json:"month_counts"`
}

// PublicNavNode represents a localized top-bar navigation item (and optional children).
type PublicNavNode struct {
	ID           string          `json:"id"`
	Title        string          `json:"title"`
	URL          string          `json:"url"`
	OpenInNewTab bool            `json:"open_in_new_tab"`
	Icon         string          `json:"icon,omitempty"`
	Children     []PublicNavNode `json:"children,omitempty"`
}

// PublicPageView is the rendered standalone Markdown page model (`/{slug}`).
type PublicPageView struct {
	ID                string                            `json:"id"`
	Title             string                            `json:"title"`
	Slug              string                            `json:"slug"`
	Template          string                            `json:"template"`
	URL               string                            `json:"url"`
	HTML              string                            `json:"html"`
	PlainSummary      string                            `json:"plain_summary"`
	AllowComment      bool                              `json:"allow_comment"`
	ShowTOC           bool                              `json:"show_toc"`
	TOC               []render.TOCItem                  `json:"toc,omitempty"`
	Lang              string                            `json:"lang"`
	TranslationGroup  string                            `json:"translation_group,omitempty"`
	Translations      []visibility.TranslationAlternate `json:"translations,omitempty"`
	ViewCount         int64                             `json:"view_count"`
	CommentCount      int                               `json:"comment_count"`
	UpdatedAt         int64                             `json:"updated_at"`
	NeedsMath         bool                              `json:"needs_math"`
	NeedsMermaid      bool                              `json:"needs_mermaid"`
	HasLockedSections bool                              `json:"has_locked_sections,omitempty"`
}

// SeriesView pairs a Series with its ordered public article cards and total published episode count.
type SeriesView struct {
	domain.Series
	Articles      []visibility.PublicArticleCard `json:"articles"`
	TotalArticles int                            `json:"total_articles"`
}

// FollowGroup pairs an RSS Follow source with its recent items.
type FollowGroup struct {
	domain.Follow
	Items []domain.FollowItem `json:"items"`
}

// PublicAuthorProfile exposes strictly non-sensitive author/owner metadata for theme headers, sidebars, and bylines.
type PublicAuthorProfile struct {
	ID           string `json:"id"`
	Username     string `json:"username"`
	DisplayName  string `json:"display_name"`
	AvatarURL    string `json:"avatar_url"`
	Bio          string `json:"bio,omitempty"`
	WebsiteURL   string `json:"website_url,omitempty"`
	Role         string `json:"role"`
	ArticleCount int    `json:"article_count"`
}

// PublicSiteStats holds site-wide counters for theme footers, sidebars, and activity pages.
type PublicSiteStats struct {
	ArticleCount  int   `json:"article_count"`
	CommentCount  int   `json:"comment_count"`
	ViewCount     int64 `json:"view_count"`
	CategoryCount int   `json:"category_count"`
	TagCount      int   `json:"tag_count"`
	SeriesCount   int   `json:"series_count"`
	BookCount     int   `json:"book_count"`
	UptimeDays    int   `json:"uptime_days"`
}

// PublicChromeExtras bundles cached site owner, staff authors, site stats, archive buckets, and sidebar article lists.
type PublicChromeExtras struct {
	Owner              PublicAuthorProfile
	Authors            []PublicAuthorProfile
	Stats              PublicSiteStats
	ArchiveBuckets     []ArchiveMonth
	ArchiveYearBuckets []ArchiveYear
	RecentPosts        []visibility.PublicArticleCard
	MostViewedPosts    []visibility.PublicArticleCard
	MostCommentedPosts []visibility.PublicArticleCard
}

// ServiceOptions configures the Content Service.
type ServiceOptions struct {
	Store          *sqlx.Store
	Keys           *config.KeyRing
	Settings       *repo.SettingsManager
	Passports      *guest.PassportManager
	PoW            *guest.PoWVerifier
	Guard          *visibility.Guard
	Search         *visibility.SearchIndex
	Renderer       *render.Engine
	Plugins        *plugin.Manager
	Catalog        *i18n.Catalog
	OutboundDialer *security.OutboundSafeDialer
}

// Service coordinates public & editorial content, books, feeds, comments, backlinks, and zero-PII analytics.
type Service struct {
	store     *sqlx.Store
	keys      *config.KeyRing
	settings  *repo.SettingsManager
	passports *guest.PassportManager
	pow       *guest.PoWVerifier
	guard     *visibility.Guard
	search    *visibility.SearchIndex
	renderer  *render.Engine
	plugins   *plugin.Manager
	catalog   *i18n.Catalog
	dialer    *security.OutboundSafeDialer

	views  *viewAnalyticsBuffer
	caches *serviceCacheState

	uvMu      sync.Mutex
	uvDate    string
	uvVisited map[uint64]struct{}

	commentHTMLMu   sync.RWMutex
	commentHTMLCurr map[string]string
	commentHTMLPrev map[string]string

	mediaMu      sync.RWMutex
	mediaDims    map[string][2]int
	mediaDimsRev atomic.Uint64

	bookChapterMu    sync.Mutex
	bookChapterCache map[string]cachedBookChapter

	onArticlePublished atomic.Pointer[func(context.Context, domain.RawArticle)]
	renderCacheSaltFn  atomic.Pointer[func() string]
	kwFilter           atomic.Pointer[cachedKeywordFilter]
}

type cachedKeywordFilter struct {
	rev     uint64
	matcher *wordfilter.Matcher
}

// NewService constructs a Content Service.
func NewService(opts ServiceOptions) *Service {
	rnd := opts.Renderer
	if rnd == nil {
		rnd = render.Default()
	}
	guard := opts.Guard
	if guard == nil {
		guard = visibility.NewGuard(opts.Keys, opts.Settings, opts.Passports, opts.Catalog, rnd)
	}
	searchIdx := opts.Search
	if searchIdx == nil {
		searchIdx = visibility.NewSearchIndex(opts.Store, rnd, visibility.DefaultSearchMaxBytes)
	}
	if searchIdx != nil {
		searchIdx.SetSettings(opts.Settings)
	}
	dialer := opts.OutboundDialer
	if dialer == nil {
		dialer = &security.OutboundSafeDialer{Timeout: 10 * time.Second}
	}
	srv := &Service{
		store:           opts.Store,
		keys:            opts.Keys,
		settings:        opts.Settings,
		passports:       opts.Passports,
		pow:             opts.PoW,
		guard:           guard,
		search:          searchIdx,
		renderer:        rnd,
		plugins:         opts.Plugins,
		catalog:         opts.Catalog,
		dialer:          dialer,
		views:           newViewAnalyticsBuffer(opts.Store),
		caches:          newServiceCacheState(),
		uvVisited:       make(map[uint64]struct{}),
		commentHTMLCurr: make(map[string]string, 256),
		commentHTMLPrev: make(map[string]string),
		mediaDims:       make(map[string][2]int),
	}
	if opts.Plugins != nil {
		srv.SetRenderCacheSaltFunc(opts.Plugins.RenderHooksStateHash)
		guard.SetCustomDirectiveFunc(func(name string, attrs map[string]string, body string) (string, bool) {
			return opts.Plugins.DispatchRenderDirective(context.Background(), name, attrs, body)
		})
	}
	return srv
}

func (s *Service) Guard() *visibility.Guard             { return s.guard }
func (s *Service) SearchIndex() *visibility.SearchIndex { return s.search }
func (s *Service) Renderer() *render.Engine             { return s.renderer }

// SetArticlePublishedCallback registers a host callback invoked when scheduled articles transition to published.
func (s *Service) SetArticlePublishedCallback(fn func(context.Context, domain.RawArticle)) {
	if fn == nil {
		s.onArticlePublished.Store(nil)
		return
	}
	s.onArticlePublished.Store(&fn)
}

// SetRenderCacheSaltFunc registers a dynamic digest callback mixed into articleHTMLCache and Guard render cache keys,
// keeping articleHTMLCache active even when plugin render hooks are enabled.
func (s *Service) SetRenderCacheSaltFunc(fn func() string) {
	if s == nil {
		return
	}
	if fn == nil {
		s.renderCacheSaltFn.Store(nil)
	} else {
		s.renderCacheSaltFn.Store(&fn)
	}
	if s.guard != nil {
		s.guard.SetRenderCacheSaltFunc(fn)
	}
}

func (s *Service) renderCacheSalt() string {
	if s == nil {
		return ""
	}
	if ptr := s.renderCacheSaltFn.Load(); ptr != nil && *ptr != nil {
		return (*ptr)()
	}
	return ""
}

// FlushAnalytics flushes all buffered view_count, redirect hit_count, and analytics_daily deltas to the database.
func (s *Service) FlushAnalytics(ctx context.Context) error {
	if s == nil || s.views == nil {
		return nil
	}
	return s.views.Flush(ctx)
}

// ResetAnalyticsBuffer clears buffered view/redirect counters, daily UV dedup state,
// rendered comment HTML cache, and runtime caches (used across DB swap / restore).
func (s *Service) ResetAnalyticsBuffer() {
	if s == nil {
		return
	}
	if s.views != nil {
		s.views.Reset()
	}
	s.uvMu.Lock()
	s.uvDate = ""
	clear(s.uvVisited)
	s.uvMu.Unlock()
	s.commentHTMLMu.Lock()
	clear(s.commentHTMLCurr)
	clear(s.commentHTMLPrev)
	s.commentHTMLMu.Unlock()
	s.PurgeRuntimeCaches()
}

// Close stops background analytics flushing and deterministically flushes all buffered counters before shutdown.
func (s *Service) Close(ctx context.Context) error {
	if s == nil || s.views == nil {
		return nil
	}
	return s.views.Close(ctx)
}

// WarmCaches preloads the 8 MiB two-stage search index, media intrinsic dimensions, redirect table, and chrome snapshot.
func (s *Service) WarmCaches(ctx context.Context) error {
	_ = s.RefreshMediaDimsCache(ctx)
	_, _ = s.getRedirectSnapshot(ctx)
	_, _ = s.getChromeSnapshot(ctx)
	return s.search.Rebuild(ctx)
}

const maxCachedMediaDims = 5000

// RefreshMediaDimsCache caches (width, height) of recent media_assets (bounded to 5000) so rendered <img> tags get zero-CLS dimensions.
func (s *Service) RefreshMediaDimsCache(ctx context.Context) error {
	q, pageArgs := s.store.Dialect().PaginateSQL(
		`SELECT public_url, width, height FROM media_assets WHERE width > 0 AND height > 0 ORDER BY created_at DESC`,
		maxCachedMediaDims, 0,
	)
	var assets []domain.MediaAsset
	err := sqlx.SelectInto(ctx, s.store, &assets, q, pageArgs...)
	if err != nil {
		return err
	}
	next := make(map[string][2]int, len(assets))
	for _, a := range assets {
		if a.PublicURL != "" {
			next[a.PublicURL] = [2]int{a.Width, a.Height}
		}
	}
	s.mediaMu.Lock()
	changed := len(s.mediaDims) != len(next)
	if !changed {
		for k, v := range next {
			if cur, ok := s.mediaDims[k]; !ok || cur != v {
				changed = true
				break
			}
		}
	}
	s.mediaDims = next
	s.mediaMu.Unlock()
	if changed {
		s.mediaDimsRev.Add(1)
		if s.guard != nil {
			s.guard.PurgeCache()
		}
	}
	return nil
}

// PutMediaDims incrementally inserts or updates a single media asset's dimensions in O(1) without scanning media_assets.
func (s *Service) PutMediaDims(publicURL string, width int, height int) {
	u := strings.TrimSpace(publicURL)
	if u == "" || width <= 0 || height <= 0 {
		return
	}
	s.mediaMu.Lock()
	if s.mediaDims == nil {
		s.mediaDims = make(map[string][2]int, 64)
	}
	prev, existed := s.mediaDims[u]
	next := [2]int{width, height}
	changed := !existed || prev != next
	if changed {
		s.mediaDims[u] = next
	}
	s.mediaMu.Unlock()
	if changed {
		s.mediaDimsRev.Add(1)
		if s.guard != nil {
			s.guard.PurgeCache()
		}
	}
}

// LookupMediaDims returns cached intrinsic pixel dimensions for a local/S3 media URL.
func (s *Service) LookupMediaDims(src string) (int, int, bool) {
	clean := strings.TrimSpace(src)
	if clean == "" {
		return 0, 0, false
	}
	if idx := strings.IndexByte(clean, '#'); idx >= 0 {
		clean = clean[:idx]
	}
	basePath := clean
	rawQuery := ""
	if qIdx := strings.IndexByte(clean, '?'); qIdx >= 0 {
		basePath = clean[:qIdx]
		rawQuery = clean[qIdx+1:]
	}
	s.mediaMu.RLock()
	wh, ok := s.mediaDims[basePath]
	if !ok && basePath != clean {
		wh, ok = s.mediaDims[clean]
	}
	s.mediaMu.RUnlock()
	if !ok {
		return 0, 0, false
	}
	if rawQuery != "" {
		tw, th := media.PredictTransformedDims(wh[0], wh[1], rawQuery)
		return tw, th, true
	}
	return wh[0], wh[1], true
}

// PublishDueScheduled promotes any scheduled articles whose scheduled_at <= nowMs to published.
func (s *Service) PublishDueScheduled(ctx context.Context) (int, error) {
	now := time.Now().UnixMilli()
	var due []domain.RawArticle
	err := sqlx.SelectInto(ctx, s.store, &due,
		`SELECT * FROM articles WHERE status = ? AND scheduled_at > 0 AND scheduled_at <= ?`,
		domain.ArticleStatusScheduled, now,
	)
	if err != nil || len(due) == 0 {
		return 0, err
	}
	err = s.store.WithTx(ctx, func(tx *sqlx.Tx) error {
		for i := range due {
			pubAt := due[i].ScheduledAt
			if pubAt <= 0 {
				pubAt = now
			}
			due[i].Status = domain.ArticleStatusPublished
			due[i].PublishedAt = pubAt
			due[i].UpdatedAt = now
			if _, uErr := tx.ExecContext(
				ctx,
				`UPDATE articles SET status = ?, published_at = ?, updated_at = ? WHERE id = ?`,
				domain.ArticleStatusPublished, pubAt, now, due[i].ID,
			); uErr != nil {
				return uErr
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	_ = s.search.Rebuild(ctx)
	cbPtr := s.onArticlePublished.Load()
	for _, a := range due {
		if s.plugins != nil {
			isProtected := strings.TrimSpace(a.PasswordCiphertext) != ""
			_ = s.plugins.DispatchArticleHook(ctx, plugin.HookArticlePublished, plugin.ArticleHookPayload{
				ID:                  a.ID,
				Title:               a.Title,
				Slug:                a.Slug,
				Lang:                a.Lang,
				SummaryMD:           a.SummaryMD,
				ContentMD:           a.ContentMD,
				CoverURL:            a.CoverURL,
				AudioURL:            a.AudioURL,
				IsPasswordProtected: isProtected,
				HasContentLock:      a.HasContentLock,
			})
		}
		if cbPtr != nil && *cbPtr != nil {
			(*cbPtr)(ctx, a)
		}
	}
	return len(due), nil
}

// ResolveRedirect checks custom & legacy 301/302 rules in `redirects` AND `articles.legacy_permalink`
// via a revision-coherent O(1) in-memory redirect table, guaranteeing zero 404 dead links and zero SQL on hot reads!
func (s *Service) ResolveRedirect(ctx context.Context, reqPath string) (target string, statusCode int, matched bool) {
	clean := strings.TrimSpace(reqPath)
	if clean == "" || clean == "/" {
		return "", 0, false
	}
	snap, err := s.getRedirectSnapshot(ctx)
	if err != nil || snap == nil {
		return "", 0, false
	}
	entry, ok := snap.rules[clean]
	if !ok || entry.targetPath == "" || entry.targetPath == clean {
		return "", 0, false
	}
	if entry.id != "" {
		s.views.RecordRedirectHit(entry.id)
	}
	return entry.targetPath, entry.statusCode, true
}

// ArticleListFilter specifies filtering and pagination for ListPublicCards.
type ArticleListFilter struct {
	Page            int
	PageSize        int
	CategorySlug    string
	TagSlug         string
	YearMonth       string // YYYY-MM
	Locale          string
	IncludeFullHTML bool
	Request         *http.Request
	Viewer          *domain.User
}

// articleCardSelectColumns projects all RawArticle metadata columns while skipping multi-MB content_md
// whenever summary_md, cover_url, word_count, and reading_minutes are already present (or when password-protected).
func articleCardSelectColumns(alias string) string {
	p := ""
	if strings.TrimSpace(alias) != "" {
		p = strings.TrimSpace(alias) + "."
	}
	return fmt.Sprintf(
		`%sid, %sauthor_id, %scategory_id, %stitle, %sslug, %slegacy_permalink, `+
			`%ssummary_md, `+
			`CASE WHEN %spassword_ciphertext <> '' OR (TRIM(%ssummary_md) <> '' AND TRIM(%scover_url) <> '' AND %sword_count > 0 AND %sreading_minutes > 0) THEN '' ELSE %scontent_md END AS content_md, `+
			`%scover_url, %sstatus, %spublished_at, %sscheduled_at, %sis_pinned, %sallow_comment, `+
			`%spassword_ciphertext, %shas_content_lock, %saudio_url, %saudio_duration_sec, `+
			`%saudio_size_bytes, %saudio_mime, %slang, %stranslation_group, %slicense_override, `+
			`%sword_count, %sreading_minutes, %sview_count, %scomment_count, %screated_at, %supdated_at`,
		p, p, p, p, p, p,
		p,
		p, p, p, p, p, p,
		p, p, p, p, p, p,
		p, p, p, p,
		p, p, p, p, p,
		p, p, p, p, p, p,
	)
}

// ListPublicCards queries published articles, enriches categories/tags in bulk, and passes every row
// through visibility.Guard.ToCard (Surface 1 & 2 protection).
func (s *Service) ListPublicCards(ctx context.Context, f ArticleListFilter) ([]visibility.PublicArticleCard, int, error) {
	limit, offset := dialect.NormalizePage(f.Page, f.PageSize, 15, 100)

	where := []string{"a.status = ?"}
	args := []any{domain.ArticleStatusPublished}

	if slug := strings.TrimSpace(f.CategorySlug); slug != "" {
		var cat domain.Category
		err := sqlx.GetInto(ctx, s.store, &cat, `SELECT id FROM categories WHERE slug = ?`, slug)
		if errors.Is(err, sql.ErrNoRows) {
			return []visibility.PublicArticleCard{}, 0, nil
		}
		if err != nil {
			return nil, 0, err
		}
		where = append(where, "a.category_id = ?")
		args = append(args, cat.ID)
	}

	if tagSlug := strings.TrimSpace(f.TagSlug); tagSlug != "" {
		var tag domain.Tag
		err := sqlx.GetInto(ctx, s.store, &tag, `SELECT id FROM tags WHERE slug = ?`, tagSlug)
		if errors.Is(err, sql.ErrNoRows) {
			return []visibility.PublicArticleCard{}, 0, nil
		}
		if err != nil {
			return nil, 0, err
		}
		where = append(where, "EXISTS (SELECT 1 FROM article_tags at WHERE at.article_id = a.id AND at.tag_id = ?)")
		args = append(args, tag.ID)
	}

	if ym := strings.TrimSpace(f.YearMonth); ym != "" {
		startMS, endMS, ok := parseYearMonthBounds(ym, s.siteLocation())
		if !ok {
			return []visibility.PublicArticleCard{}, 0, nil
		}
		where = append(where, "((a.published_at >= ? AND a.published_at < ?) OR (a.published_at <= 0 AND a.created_at >= ? AND a.created_at < ?))")
		args = append(args, startMS, endMS, startMS, endMS)
	}

	whereClause := strings.Join(where, " AND ")
	var total int
	if err := s.store.QueryRowContext(ctx, "SELECT COUNT(*) FROM articles a WHERE "+whereClause, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	if total == 0 {
		return []visibility.PublicArticleCard{}, 0, nil
	}

	selectCols := articleCardSelectColumns("a")
	if f.IncludeFullHTML {
		selectCols = "a.*"
	}
	baseSQL := "SELECT " + selectCols + " FROM articles a WHERE " + whereClause + " ORDER BY a.is_pinned DESC, a.published_at DESC"
	pageSQL, pageArgs := s.store.Dialect().PaginateSQL(baseSQL, limit, offset)
	queryArgs := append(append([]any{}, args...), pageArgs...)

	var raws []domain.RawArticle
	err := sqlx.SelectInto(ctx, s.store, &raws, pageSQL, queryArgs...)
	if err != nil {
		return nil, 0, err
	}
	cards, err := s.hydrateCards(ctx, raws, f.Locale)
	if err != nil {
		return nil, 0, err
	}
	if f.IncludeFullHTML && s.guard != nil {
		for i := range cards {
			s.PopulateCardFullHTML(ctx, f.Request, f.Viewer, raws[i], &cards[i])
		}
	}
	return cards, total, nil
}

// PopulateCardFullHTML renders sanitized FullHTML into card ONLY when raw is unlocked and free of content locks.
func (s *Service) PopulateCardFullHTML(ctx context.Context, r *http.Request, viewer *domain.User, raw domain.RawArticle, card *visibility.PublicArticleCard) {
	if s == nil || s.guard == nil || card == nil {
		return
	}
	rawForRender := raw
	if s.plugins != nil && strings.TrimSpace(raw.PasswordCiphertext) == "" && !raw.HasContentLock &&
		!render.ContainsLockDirective(raw.ContentMD) && !render.ContainsLockDirective(raw.SummaryMD) {
		preRes := s.plugins.DispatchArticleHook(ctx, plugin.HookRenderPreMarkdown, plugin.ArticleHookPayload{
			ID:                  raw.ID,
			Title:               raw.Title,
			Slug:                raw.Slug,
			Lang:                raw.Lang,
			Status:              string(raw.Status),
			SummaryMD:           raw.SummaryMD,
			ContentMD:           raw.ContentMD,
			IsPasswordProtected: false,
			HasContentLock:      false,
		})
		if preRes.ContentMD != "" {
			rawForRender.ContentMD = preRes.ContentMD
		}
		if preRes.SummaryMD != "" {
			rawForRender.SummaryMD = preRes.SummaryMD
		}
	}
	s.guard.PopulateCardFullHTML(ctx, r, viewer, rawForRender, card)
	if s.plugins != nil && card.FullHTML != "" && !card.IsPasswordProtected && !card.HasContentLock {
		hookRes := s.plugins.DispatchArticleHook(ctx, plugin.HookRenderPostHTML, plugin.ArticleHookPayload{
			ID:                  raw.ID,
			Title:               raw.Title,
			Slug:                raw.Slug,
			Lang:                raw.Lang,
			Status:              string(raw.Status),
			HTML:                card.FullHTML,
			IsPasswordProtected: false,
			HasContentLock:      false,
		})
		if hookRes.HTML != "" {
			card.FullHTML = s.SanitizeHTMLFragment(hookRes.HTML)
		}
	}
}

func (s *Service) hydrateCards(ctx context.Context, raws []domain.RawArticle, locale string) ([]visibility.PublicArticleCard, error) {
	if len(raws) == 0 {
		return []visibility.PublicArticleCard{}, nil
	}
	chromeSnap, _ := s.getChromeSnapshot(ctx)
	var catsByID map[string]domain.Category
	var authorsByID map[string]PublicAuthorProfile
	var fallbackOwner PublicAuthorProfile
	if chromeSnap != nil {
		catsByID = chromeSnap.catsByID
		authorsByID = chromeSnap.authorsByID
		fallbackOwner = chromeSnap.owner
	}
	tagsByArticle, _ := s.loadTagsForArticles(ctx, raws)

	out := make([]visibility.PublicArticleCard, 0, len(raws))
	for _, r := range raws {
		r.ViewCount += s.views.PendingArticleViews(r.ID)
		var catPtr *domain.Category
		if c, ok := catsByID[r.CategoryID]; ok {
			cCopy := c
			catPtr = &cCopy
		}
		card := s.guard.ToCard(r, locale, catPtr, tagsByArticle[r.ID], s.LookupMediaDims)
		if au, ok := authorsByID[r.AuthorID]; ok {
			card.AuthorName = au.DisplayName
			card.AuthorAvatarURL = au.AvatarURL
		} else if fallbackOwner.DisplayName != "" {
			card.AuthorName = fallbackOwner.DisplayName
			card.AuthorAvatarURL = fallbackOwner.AvatarURL
		}
		out = append(out, card)
	}
	return out, nil
}

// GetPublishedRawArticleBySlug fetches a published RawArticle by slug.
func (s *Service) GetPublishedRawArticleBySlug(ctx context.Context, slug string) (domain.RawArticle, error) {
	var raw domain.RawArticle
	err := sqlx.GetInto(
		ctx, s.store, &raw,
		`SELECT * FROM articles WHERE slug = ? AND status = ?`,
		strings.TrimSpace(slug), domain.ArticleStatusPublished,
	)
	return raw, err
}

// GetRawArticleBySlugAnyStatus fetches a RawArticle regardless of publication status (used only when gated by Staff ownership or valid ShareToken).
func (s *Service) GetRawArticleBySlugAnyStatus(ctx context.Context, slug string) (domain.RawArticle, error) {
	var raw domain.RawArticle
	err := sqlx.GetInto(
		ctx, s.store, &raw,
		`SELECT * FROM articles WHERE slug = ?`,
		strings.TrimSpace(slug),
	)
	return raw, err
}

// GetRawArticleByID fetches a RawArticle by ID.
func (s *Service) GetRawArticleByID(ctx context.Context, articleID string) (domain.RawArticle, error) {
	var raw domain.RawArticle
	err := sqlx.GetInto(ctx, s.store, &raw, `SELECT * FROM articles WHERE id = ?`, strings.TrimSpace(articleID))
	return raw, err
}

func (s *Service) buildArticleHTMLCacheKey(raw domain.RawArticle, locale string, publicBaseURL string, authorName string, showLineNumbers bool, enableLicense bool, salt string) string {
	dRev, sRev := s.currentRevisions()
	mRev := s.mediaDimsRev.Load()
	h := sha256.New()
	_, _ = h.Write([]byte(raw.Title))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(raw.SummaryMD))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(raw.ContentMD))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(raw.CoverURL))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(raw.AudioURL))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(raw.LicenseOverride))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(authorName))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(publicBaseURL))
	digest := hex.EncodeToString(h.Sum(nil)[:12])
	return fmt.Sprintf(
		"arthtml|%s|%d|d=%d|s=%d|m=%d|%s|loc=%s|ln=%t|lic=%t|salt=%s",
		raw.ID, raw.UpdatedAt, dRev, sRev, mRev, digest, locale, showLineNumbers, enableLicense, salt,
	)
}

// RenderArticleDetail builds the full sanitized PublicArticleView + Prev/Next neighbors + public inbound backlinks.
func (s *Service) RenderArticleDetail(
	ctx context.Context,
	w http.ResponseWriter,
	r *http.Request,
	raw domain.RawArticle,
	viewer *domain.User,
	locale string,
	publicBaseURL string,
) (visibility.PublicArticleView, *visibility.PublicNeighbor, *visibility.PublicNeighbor, []visibility.PublicNeighbor, error) {
	snap := s.settings.Snapshot()
	chromeSnap, _ := s.getChromeSnapshot(ctx)

	var authorName, authorAvatar, authorBio, authorWeb string
	if chromeSnap != nil {
		if au, ok := chromeSnap.authorsByID[raw.AuthorID]; ok {
			authorName = au.DisplayName
			authorAvatar = au.AvatarURL
			authorBio = au.Bio
			authorWeb = au.WebsiteURL
		}
	}
	if authorName == "" {
		var u domain.User
		if err := sqlx.GetInto(ctx, s.store, &u, `SELECT display_name, username, avatar_url, bio, website_url FROM users WHERE id = ?`, raw.AuthorID); err == nil {
			authorName = u.DisplayName
			if authorName == "" {
				authorName = u.Username
			}
			authorAvatar = u.AvatarURL
			if authorAvatar == "" && u.Username != "" {
				authorAvatar = "/avatars/" + u.Username
			}
			authorBio = u.Bio
			authorWeb = u.WebsiteURL
		}
	}
	if authorName == "" {
		authorName = snap.Site.Title
	}

	var catPtr *domain.Category
	if raw.CategoryID != "" {
		if chromeSnap != nil {
			if c, ok := chromeSnap.catsByID[raw.CategoryID]; ok {
				cCopy := c
				catPtr = &cCopy
			}
		}
		if catPtr == nil {
			var c domain.Category
			if err := sqlx.GetInto(ctx, s.store, &c, `SELECT * FROM categories WHERE id = ?`, raw.CategoryID); err == nil {
				catPtr = &c
			}
		}
	}
	tagsMap, _ := s.loadTagsForArticles(ctx, []domain.RawArticle{raw})
	alts := s.listArticleTranslations(ctx, raw)
	raw.ViewCount += s.views.PendingArticleViews(raw.ID)

	_, isWholeProt := s.guard.DecryptArticlePassword(raw)
	hasShareTok := r != nil && (strings.TrimSpace(r.URL.Query().Get("preview")) != "" || strings.TrimSpace(r.Header.Get("X-Volo-Share-Token")) != "")
	hasAnyLock := isWholeProt || strings.TrimSpace(raw.PasswordCiphertext) != "" || raw.HasContentLock ||
		render.ContainsLockDirective(raw.ContentMD) || render.ContainsLockDirective(raw.SummaryMD)
	hasSaltFn := s.renderCacheSaltFn.Load() != nil
	canCacheArticleHTML := s.caches != nil &&
		raw.Status == domain.ArticleStatusPublished &&
		!hasAnyLock &&
		!hasShareTok &&
		!strings.Contains(raw.ContentMD, ":::book") &&
		(s.plugins == nil || hasSaltFn)

	var detailKey string
	if canCacheArticleHTML {
		salt := s.renderCacheSalt()
		detailKey = s.buildArticleHTMLCacheKey(raw, locale, publicBaseURL, authorName, snap.Reading.ShowCodeLineNumbers, snap.Site.EnableLicenseNotice, salt)
		if cached, ok := s.caches.getArticleHTML(detailKey); ok {
			view := visibility.PublicArticleView{
				ID:               raw.ID,
				AuthorID:         raw.AuthorID,
				AuthorName:       authorName,
				AuthorAvatarURL:  authorAvatar,
				AuthorBio:        authorBio,
				AuthorWebsiteURL: authorWeb,
				CategoryID:       raw.CategoryID,
				Tags:             tagsMap[raw.ID],
				Title:            raw.Title,
				Slug:             raw.Slug,
				URL:              s.guard.ArticleURL(raw),
				Summary:          cached.summary,
				HTML:             cached.html,
				CoverURL:         cached.coverURL,
				OGImageURL:       cached.ogImageURL,
				IsPinned:         raw.IsPinned,
				HasUpdated:       raw.PublishedAt > 0 && raw.UpdatedAt > raw.PublishedAt+60_000,
				AllowComment:     raw.AllowComment,
				AudioURL:         raw.AudioURL,
				AudioDurationSec: raw.AudioDurationSec,
				AudioSizeBytes:   raw.AudioSizeBytes,
				AudioMIME:        raw.AudioMIME,
				Lang:             raw.Lang,
				TranslationGroup: raw.TranslationGroup,
				Translations:     alts,
				LicenseText:      cached.licenseText,
				TOC:              cached.toc,
				WordCount:        cached.wordCount,
				ReadingMinutes:   cached.readingMinutes,
				ViewCount:        raw.ViewCount,
				CommentCount:     raw.CommentCount,
				PublishedAt:      raw.PublishedAt,
				UpdatedAt:        raw.UpdatedAt,
				NeedsMath:        cached.needsMath,
				NeedsMermaid:     cached.needsMermaid,
			}
			if catPtr != nil {
				view.CategoryName = catPtr.Name
				view.CategorySlug = catPtr.Slug
			}
			prev, next := s.getNeighbors(ctx, raw)
			backlinks := s.ListInboundBacklinks(ctx, raw)
			return view, prev, next, backlinks, nil
		}
	}

	rawForRender := raw
	if s.plugins != nil && strings.TrimSpace(raw.PasswordCiphertext) == "" && !raw.HasContentLock && !render.ContainsLockDirective(raw.ContentMD) {
		preRes := s.plugins.DispatchArticleHook(ctx, plugin.HookRenderPreMarkdown, plugin.ArticleHookPayload{
			ID:                  raw.ID,
			Title:               raw.Title,
			Slug:                raw.Slug,
			Lang:                raw.Lang,
			Status:              string(raw.Status),
			SummaryMD:           raw.SummaryMD,
			ContentMD:           raw.ContentMD,
			IsPasswordProtected: false,
			HasContentLock:      false,
		})
		if preRes.ContentMD != "" {
			rawForRender.ContentMD = preRes.ContentMD
		}
		if preRes.SummaryMD != "" {
			rawForRender.SummaryMD = preRes.SummaryMD
		}
	}

	var customDirective func(string, map[string]string, string) (string, bool)
	if s.plugins != nil {
		customDirective = func(name string, attrs map[string]string, body string) (string, bool) {
			return s.plugins.DispatchRenderDirective(ctx, name, attrs, body)
		}
	}

	view := s.guard.ToDetail(w, r, rawForRender, viewer, visibility.DetailRenderDeps{
		Locale:               locale,
		AuthorName:           authorName,
		AuthorAvatarURL:      authorAvatar,
		AuthorBio:            authorBio,
		AuthorWebsiteURL:     authorWeb,
		Category:             catPtr,
		Tags:                 tagsMap[raw.ID],
		Translations:         alts,
		DefaultOGImageURL:    snap.SEO.DefaultOGImageURL,
		SiteTitle:            snap.Site.Title,
		PublicBaseURL:        publicBaseURL,
		LicenseTemplate:      snap.Site.LicenseTemplate,
		DisableLicenseNotice: !snap.Site.EnableLicenseNotice,
		ShowLineNumbers:      snap.Reading.ShowCodeLineNumbers,
		LookupBook:           func(bSlug string) (render.BookCardInfo, bool) { return s.lookupBookCard(ctx, bSlug) },
		ListBooks:            func() []render.BookCardInfo { return s.listBookCards(ctx) },
		LookupMediaDims:      s.LookupMediaDims,
		CustomDirective:      customDirective,
	})

	// Surface 10: Only run `render.post_html` plugin hook when the article is public & free of locks!
	if s.plugins != nil && !view.IsPasswordProtected && !view.HasContentLock && view.HTML != "" {
		hookRes := s.plugins.DispatchArticleHook(ctx, plugin.HookRenderPostHTML, plugin.ArticleHookPayload{
			ID:                  raw.ID,
			Title:               raw.Title,
			Slug:                raw.Slug,
			Lang:                raw.Lang,
			Status:              string(raw.Status),
			HTML:                view.HTML,
			IsPasswordProtected: false,
			HasContentLock:      false,
		})
		if hookRes.HTML != "" {
			// Re-sanitize plugin-post-processed HTML via bluemonday so plugins cannot inject <script> XSS!
			view.HTML = s.SanitizeHTMLFragment(hookRes.HTML)
		}
	}

	if canCacheArticleHTML && detailKey != "" && !view.IsPasswordProtected && !view.RequiresPassword && !view.HasContentLock && !view.HasLockedSections {
		s.caches.putArticleHTML(cachedArticleDetail{
			key:            detailKey,
			html:           view.HTML,
			summary:        view.Summary,
			coverURL:       view.CoverURL,
			ogImageURL:     view.OGImageURL,
			licenseText:    view.LicenseText,
			toc:            view.TOC,
			wordCount:      view.WordCount,
			readingMinutes: view.ReadingMinutes,
			needsMath:      view.NeedsMath,
			needsMermaid:   view.NeedsMermaid,
		})
	}

	prev, next := s.getNeighbors(ctx, raw)
	backlinks := s.ListInboundBacklinks(ctx, raw)
	return view, prev, next, backlinks, nil
}

// SanitizeHTMLFragment sanitizes an HTML fragment using the engine's strict XSS policy.
func (s *Service) SanitizeHTMLFragment(htmlFragment string) string {
	return s.renderer.SanitizeHTML(htmlFragment)
}

func (s *Service) getNeighbors(ctx context.Context, cur domain.RawArticle) (prev *visibility.PublicNeighbor, next *visibility.PublicNeighbor) {
	olderSQL, olderArgs := s.store.Dialect().PaginateSQL(
		`SELECT id, title, slug, published_at, password_ciphertext
		 FROM articles
		 WHERE status = ? AND (published_at < ? OR (published_at = ? AND id < ?))
		 ORDER BY published_at DESC, id DESC`,
		1, 0,
	)
	var olderRows []domain.RawArticle
	err := sqlx.SelectInto(ctx, s.store, &olderRows, olderSQL,
		append([]any{domain.ArticleStatusPublished, cur.PublishedAt, cur.PublishedAt, cur.ID}, olderArgs...)...,
	)
	if err == nil && len(olderRows) > 0 {
		n := s.guard.ToNeighbor(olderRows[0])
		prev = &n
	}

	newerSQL, newerArgs := s.store.Dialect().PaginateSQL(
		`SELECT id, title, slug, published_at, password_ciphertext
		 FROM articles
		 WHERE status = ? AND (published_at > ? OR (published_at = ? AND id > ?))
		 ORDER BY published_at ASC, id ASC`,
		1, 0,
	)
	var newerRows []domain.RawArticle
	err = sqlx.SelectInto(ctx, s.store, &newerRows, newerSQL,
		append([]any{domain.ArticleStatusPublished, cur.PublishedAt, cur.PublishedAt, cur.ID}, newerArgs...)...,
	)
	if err == nil && len(newerRows) > 0 {
		n := s.guard.ToNeighbor(newerRows[0])
		next = &n
	}
	return prev, next
}

// ListInboundBacklinks enforces Surface 8 (bounded to 50 rows):
// - Password-protected target articles expose ZERO backlinks publicly.
// - Password-protected source articles NEVER appear as backlink sources.
func (s *Service) ListInboundBacklinks(ctx context.Context, target domain.RawArticle) []visibility.PublicNeighbor {
	if !s.guard.IsPublicIndexable(target) {
		return nil
	}
	baseSQL := `SELECT a.id, a.title, a.slug, a.status, a.published_at, a.password_ciphertext
		 FROM article_backlinks b
		 INNER JOIN articles a ON a.id = b.source_article_id
		 WHERE b.target_article_id = ? AND a.status = ? AND a.password_ciphertext = ''
		 ORDER BY a.published_at DESC`
	pageSQL, pageArgs := s.store.Dialect().PaginateSQL(baseSQL, 50, 0)
	var rows []domain.RawArticle
	err := sqlx.SelectInto(ctx, s.store, &rows, pageSQL,
		append([]any{target.ID, domain.ArticleStatusPublished}, pageArgs...)...,
	)
	if err != nil || len(rows) == 0 {
		return nil
	}
	out := make([]visibility.PublicNeighbor, 0, len(rows))
	for _, r := range rows {
		if s.guard.IsPublicIndexable(r) {
			out = append(out, s.guard.ToNeighbor(r))
		}
	}
	return out
}

// ReindexArticleBacklinksTx updates `article_backlinks` for sourceArticle inside tx,
// strictly enforcing Surface 8 (encrypted articles write zero outbound backlinks; `:::lock` blocks are stripped first).
func (s *Service) ReindexArticleBacklinksTx(ctx context.Context, tx *sqlx.Tx, source domain.RawArticle) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM article_backlinks WHERE source_article_id = ?`, source.ID); err != nil {
		return err
	}
	if !s.guard.IsPublicIndexable(source) {
		return nil
	}
	slugs := render.ExtractOutboundPostSlugs(source.ContentMD)
	if len(slugs) == 0 {
		return nil
	}
	now := time.Now().UnixMilli()
	upsert := tx.Dialect().UpsertSQL(
		"article_backlinks",
		[]string{"source_article_id", "target_article_id", "updated_at"},
		[]string{"source_article_id", "target_article_id"},
		[]string{"updated_at"},
	)
	for _, slug := range slugs {
		if slug == source.Slug {
			continue
		}
		var target domain.RawArticle
		err := sqlx.GetInto(ctx, tx, &target, `SELECT id, password_ciphertext FROM articles WHERE slug = ?`, slug)
		if err != nil || target.ID == "" || target.ID == source.ID || strings.TrimSpace(target.PasswordCiphertext) != "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, upsert, source.ID, target.ID, now); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) listArticleTranslations(ctx context.Context, cur domain.RawArticle) []visibility.TranslationAlternate {
	grp := strings.TrimSpace(cur.TranslationGroup)
	if grp == "" {
		return nil
	}
	var rows []domain.RawArticle
	err := sqlx.SelectInto(ctx, s.store, &rows,
		`SELECT title, slug, lang, published_at, password_ciphertext
		 FROM articles
		 WHERE translation_group = ? AND status = ? AND id <> ?
		 ORDER BY lang ASC`,
		grp, domain.ArticleStatusPublished, cur.ID,
	)
	if err != nil || len(rows) == 0 {
		return nil
	}
	out := make([]visibility.TranslationAlternate, 0, len(rows))
	for _, r := range rows {
		if strings.TrimSpace(r.PasswordCiphertext) != "" {
			continue
		}
		out = append(out, visibility.TranslationAlternate{
			Lang:  r.Lang,
			Title: r.Title,
			Slug:  r.Slug,
			URL:   s.guard.ArticleURL(r),
		})
	}
	return out
}

// --- Standalone Pages (`pages` table — strictly decoupled from blog timeline & archives) ---

// GetPublicPageBySlug renders a published Standalone Page (`/{slug}`) for an anonymous visitor.
func (s *Service) GetPublicPageBySlug(ctx context.Context, slug string, locale string) (PublicPageView, error) {
	return s.GetPublicPageBySlugForViewer(ctx, nil, slug, locale, nil)
}

// GetPublicPageBySlugForViewer renders a published Standalone Page (`/{slug}`), strictly evaluating
// per-block `:::lock` directives against the viewer/passport instead of hardcoding `Unlocked: true`!
func (s *Service) GetPublicPageBySlugForViewer(
	ctx context.Context,
	r *http.Request,
	slug string,
	locale string,
	viewer *domain.User,
) (PublicPageView, error) {
	var p domain.Page
	err := sqlx.GetInto(ctx, s.store, &p,
		`SELECT * FROM pages WHERE slug = ? AND is_published = ?`,
		strings.TrimSpace(slug), true,
	)
	if err != nil {
		return PublicPageView{}, err
	}
	snap := s.settings.Snapshot()

	isPageStaff := viewer != nil && viewer.Status == domain.UserStatusActive &&
		(viewer.Role == domain.RoleAdmin || (viewer.Role == domain.RoleEditor && viewer.ID == p.AuthorID))
	isReaderLoggedIn := viewer != nil && viewer.Status == domain.UserStatusActive

	var unlockedPwds map[string]bool
	if r != nil && s.guard != nil {
		byPagePrefix := s.guard.ResolveInlineUnlockedPasswords(r, "page:"+p.ID, p.ContentMD)
		byRawID := s.guard.ResolveInlineUnlockedPasswords(r, p.ID, p.ContentMD)
		if len(byPagePrefix) > 0 || len(byRawID) > 0 {
			unlockedPwds = make(map[string]bool, len(byPagePrefix)+len(byRawID))
			for k, v := range byPagePrefix {
				unlockedPwds[k] = v
			}
			for k, v := range byRawID {
				unlockedPwds[k] = v
			}
		}
	}

	contentMD := p.ContentMD
	hasLockDirective := render.ContainsLockDirective(contentMD)
	if s.plugins != nil && !hasLockDirective {
		preRes := s.plugins.DispatchArticleHook(ctx, plugin.HookRenderPreMarkdown, plugin.ArticleHookPayload{
			ID:                  p.ID,
			Title:               p.Title,
			Slug:                p.Slug,
			Lang:                p.Lang,
			Status:              domain.ArticleStatusPublished,
			ContentMD:           contentMD,
			IsPasswordProtected: false,
			HasContentLock:      false,
		})
		if preRes.ContentMD != "" {
			contentMD = preRes.ContentMD
		}
	}

	var pageCustomDirective func(string, map[string]string, string) (string, bool)
	if s.plugins != nil {
		pageCustomDirective = func(name string, attrs map[string]string, body string) (string, bool) {
			return s.plugins.DispatchRenderDirective(ctx, name, attrs, body)
		}
	}

	rendered := s.renderer.Render(contentMD, render.Options{
		Mode:                render.RenderModeWeb,
		Locale:              locale,
		Unlocked:            false,
		AuthorOrShareBypass: isPageStaff,
		IsReaderLoggedIn:    isReaderLoggedIn,
		UnlockedPasswords:   unlockedPwds,
		ShowLineNumbers:     snap.Reading.ShowCodeLineNumbers,
		LookupBook:          func(bSlug string) (render.BookCardInfo, bool) { return s.lookupBookCard(ctx, bSlug) },
		ListBooks:           func() []render.BookCardInfo { return s.listBookCards(ctx) },
		LookupMediaDims:     s.LookupMediaDims,
		CustomDirective:     pageCustomDirective,
	})

	htmlOut := rendered.HTML
	if s.plugins != nil && !rendered.HasLockBlock && !rendered.HasUnresolvedLock && htmlOut != "" {
		postRes := s.plugins.DispatchArticleHook(ctx, plugin.HookRenderPostHTML, plugin.ArticleHookPayload{
			ID:                  p.ID,
			Title:               p.Title,
			Slug:                p.Slug,
			Lang:                p.Lang,
			Status:              domain.ArticleStatusPublished,
			HTML:                htmlOut,
			IsPasswordProtected: false,
			HasContentLock:      false,
		})
		if postRes.HTML != "" {
			htmlOut = s.SanitizeHTMLFragment(postRes.HTML)
		}
	}

	plainSource := rendered.PlainText
	if rendered.HasLockBlock {
		safeOut := s.renderer.Render(render.StripAllLockBlocks(contentMD), render.Options{Mode: render.RenderModeWeb})
		plainSource = safeOut.PlainText
	}

	var alts []visibility.TranslationAlternate
	if strings.TrimSpace(p.TranslationGroup) != "" {
		var peers []domain.Page
		pErr := sqlx.SelectInto(ctx, s.store, &peers,
			`SELECT title, slug, lang FROM pages WHERE translation_group = ? AND is_published = ? AND id <> ? ORDER BY lang ASC`,
			p.TranslationGroup, true, p.ID,
		)
		if pErr == nil {
			for _, pr := range peers {
				alts = append(alts, visibility.TranslationAlternate{
					Lang:  pr.Lang,
					Title: pr.Title,
					Slug:  pr.Slug,
					URL:   "/" + pr.Slug,
				})
			}
		}
	}

	var toc []render.TOCItem
	if p.ShowTOC {
		toc = rendered.TOC
	}
	tpl := strings.TrimSpace(p.Template)
	if tpl == "" {
		tpl = "default"
	}
	return PublicPageView{
		ID:                p.ID,
		Title:             p.Title,
		Slug:              p.Slug,
		Template:          tpl,
		URL:               "/" + p.Slug,
		HTML:              htmlOut,
		PlainSummary:      truncateForSummary(plainSource, 160),
		AllowComment:      p.AllowComment,
		ShowTOC:           p.ShowTOC,
		TOC:               toc,
		Lang:              p.Lang,
		TranslationGroup:  p.TranslationGroup,
		Translations:      alts,
		ViewCount:         p.ViewCount + s.views.PendingPageViews(p.ID),
		CommentCount:      p.CommentCount,
		UpdatedAt:         p.UpdatedAt,
		NeedsMath:         rendered.NeedsMath,
		NeedsMermaid:      rendered.NeedsMermaid,
		HasLockedSections: rendered.HasUnresolvedLock,
	}, nil
}

// ListPublicNavItems builds the localized top navigation menu from revision-coherent `nav_items` snapshot.
func (s *Service) ListPublicNavItems(ctx context.Context, locale string) ([]PublicNavNode, error) {
	snap, err := s.getChromeSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	items := snap.rawNavItems
	byParent := make(map[string][]domain.NavItem, len(items))
	for _, it := range items {
		parent := strings.TrimSpace(it.ParentID)
		byParent[parent] = append(byParent[parent], it)
	}
	var build func(parentID string) []PublicNavNode
	build = func(parentID string) []PublicNavNode {
		list := byParent[parentID]
		if len(list) == 0 {
			return nil
		}
		out := make([]PublicNavNode, 0, len(list))
		for _, it := range list {
			title := it.Title
			hasExplicit := false
			if strings.TrimSpace(it.I18nTitlesJSON) != "" {
				var m map[string]string
				if json.Unmarshal([]byte(it.I18nTitlesJSON), &m) == nil {
					if locTitle := lookupExactOrLangTitle(m, locale); locTitle != "" {
						title = locTitle
						hasExplicit = true
					}
				}
			}
			if !hasExplicit {
				if inferred := inferBuiltinNavItemTitle(it.Title, it.URL, locale); inferred != "" {
					title = inferred
				} else if strings.TrimSpace(it.I18nTitlesJSON) != "" {
					var m map[string]string
					if json.Unmarshal([]byte(it.I18nTitlesJSON), &m) == nil {
						if locTitle := plugin.ResolveLocalized(m, locale); locTitle != "" {
							title = locTitle
						}
					}
				}
			}
			out = append(out, PublicNavNode{
				ID:           it.ID,
				Title:        title,
				URL:          it.URL,
				OpenInNewTab: it.OpenInNewTab,
				Icon:         it.Icon,
				Children:     build(it.ID),
			})
		}
		return out
	}
	return build(""), nil
}

func lookupExactOrLangTitle(m map[string]string, locale string) string {
	if len(m) == 0 {
		return ""
	}
	loc := strings.TrimSpace(locale)
	if v := strings.TrimSpace(m[loc]); v != "" {
		return v
	}
	for k, v := range m {
		if strings.EqualFold(strings.TrimSpace(k), loc) && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	locLower := strings.ToLower(loc)
	switch {
	case strings.HasPrefix(locLower, "en"):
		if v := strings.TrimSpace(m["en-US"]); v != "" {
			return v
		}
		if v := strings.TrimSpace(m["en"]); v != "" {
			return v
		}
	case strings.HasPrefix(locLower, "ja"):
		if v := strings.TrimSpace(m["ja-JP"]); v != "" {
			return v
		}
		if v := strings.TrimSpace(m["ja"]); v != "" {
			return v
		}
	case strings.Contains(locLower, "tw") || strings.Contains(locLower, "hk") || strings.Contains(locLower, "hant"):
		if v := strings.TrimSpace(m["zh-TW"]); v != "" {
			return v
		}
		if v := strings.TrimSpace(m["zh-HK"]); v != "" {
			return v
		}
	case strings.HasPrefix(locLower, "zh"):
		if v := strings.TrimSpace(m["zh-CN"]); v != "" {
			return v
		}
		if v := strings.TrimSpace(m["zh"]); v != "" {
			return v
		}
	}
	return ""
}

var commonCustomNavLocales = map[string][4]string{
	// key -> {zh-CN, zh-TW, en-US, ja-JP}
	"关于":      {"关于", "關於", "About", "概要"},
	"關於":      {"关于", "關於", "About", "概要"},
	"概要":      {"关于", "關於", "About", "概要"},
	"关于我":     {"关于我", "關於我", "About", "概要"},
	"关于本站":    {"关于本站", "關於本站", "About", "概要"},
	"about":   {"关于", "關於", "About", "概要"},
	"我的开源":    {"我的开源", "我的開源", "Open Source", "オープンソース"},
	"我的開源":    {"我的开源", "我的開源", "Open Source", "オープンソース"},
	"オープンソース": {"我的开源", "我的開源", "Open Source", "オープンソース"},
	"open source": {"我的开源", "我的開源", "Open Source", "オープンソース"},
	"开源":      {"开源", "開源", "Open Source", "オープンソース"},
	"开源项目":    {"开源项目", "開源專案", "Open Source", "オープンソース"},
	"友链":      {"友链", "友鏈", "Links", "リンク"},
	"友情链接":    {"友情链接", "友情連結", "Links", "リンク"},
	"留言":      {"留言", "留言", "Guestbook", "ゲストブック"},
	"留言板":     {"留言板", "留言板", "Guestbook", "ゲストブック"},
	"书单":      {"书单", "書單", "Bookshelf", "本棚"},
	"书架":      {"书架", "書架", "Bookshelf", "本棚"},
	"读书":      {"读书", "讀書", "Reading", "読書"},
	"相册":      {"相册", "相冊", "Gallery", "ギャラリー"},
	"订阅":      {"订阅", "訂閱", "Subscribe", "購読"},
	"动态":      {"动态", "動態", "Stream", "ストリーム"},
	"归档":      {"归档", "歸檔", "Archives", "アーカイブ"},
	"分类":      {"分类", "分類", "Categories", "カテゴリー"},
	"标签":      {"标签", "標籤", "Tags", "タグ"},
	"专题":      {"专题", "專題", "Series", "シリーズ"},
	"作品":      {"作品", "作品", "Projects", "プロジェクト"},
	"项目":      {"项目", "專案", "Projects", "プロジェクト"},
}

func inferBuiltinNavItemTitle(rawTitle, rawURL, locale string) string {
	key := strings.ToLower(strings.TrimSpace(rawTitle))
	entry, ok := commonCustomNavLocales[key]
	if !ok {
		cleanPath := strings.Trim(strings.ToLower(strings.TrimSpace(rawURL)), "/")
		switch cleanPath {
		case "about":
			entry, ok = commonCustomNavLocales["关于"]
		case "posts/my-github-repos", "my-github-repos", "github":
			entry, ok = commonCustomNavLocales["我的开源"]
		case "archives":
			entry, ok = commonCustomNavLocales["归档"]
		case "links":
			entry, ok = commonCustomNavLocales["友链"]
		case "bookshelf":
			entry, ok = commonCustomNavLocales["书架"]
		case "follows":
			entry, ok = commonCustomNavLocales["订阅"]
		case "dynamic":
			entry, ok = commonCustomNavLocales["动态"]
		case "categories":
			entry, ok = commonCustomNavLocales["分类"]
		case "tags":
			entry, ok = commonCustomNavLocales["标签"]
		case "series":
			entry, ok = commonCustomNavLocales["专题"]
		}
	}
	if !ok {
		return ""
	}
	loc := strings.ToLower(strings.TrimSpace(locale))
	switch {
	case strings.HasPrefix(loc, "en"):
		return entry[2]
	case strings.HasPrefix(loc, "ja"):
		return entry[3]
	case strings.Contains(loc, "tw") || strings.Contains(loc, "hk") || strings.Contains(loc, "hant"):
		return entry[1]
	default:
		return entry[0]
	}
}

// ListCategoriesWithCounts returns categories ordered by sort_order with published article counts.
func (s *Service) ListCategoriesWithCounts(ctx context.Context) ([]CategoryWithCount, error) {
	snap, err := s.getChromeSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]CategoryWithCount, len(snap.categories))
	copy(out, snap.categories)
	return out, nil
}

// ListCategoriesWithCountsPage returns a SQL-paginated slice of categories ordered by sort_order with published article counts.
func (s *Service) ListCategoriesWithCountsPage(ctx context.Context, page, perPage int) ([]CategoryWithCount, int, error) {
	limit, offset := dialect.NormalizePage(page, perPage, 24, 200)
	var total int
	if err := s.store.QueryRowContext(ctx, `SELECT COUNT(*) FROM categories`).Scan(&total); err != nil {
		return nil, 0, err
	}
	if total == 0 {
		return []CategoryWithCount{}, 0, nil
	}
	pageSQL, pageArgs := s.store.Dialect().PaginateSQL(
		`SELECT * FROM categories ORDER BY sort_order ASC, created_at ASC`,
		limit, offset,
	)
	var cats []domain.Category
	err := sqlx.SelectInto(ctx, s.store, &cats, pageSQL, pageArgs...)
	if err != nil {
		return nil, 0, err
	}
	if len(cats) == 0 {
		return []CategoryWithCount{}, total, nil
	}

	placeholders := make([]string, len(cats))
	cntArgs := make([]any, 0, len(cats)+1)
	cntArgs = append(cntArgs, domain.ArticleStatusPublished)
	for i, c := range cats {
		placeholders[i] = "?"
		cntArgs = append(cntArgs, c.ID)
	}
	type countRow struct {
		CategoryID string `db:"category_id"`
		Cnt        int    `db:"cnt"`
	}
	cntSQL := fmt.Sprintf(
		`SELECT category_id, COUNT(*) AS cnt FROM articles WHERE status = ? AND category_id IN (%s) GROUP BY category_id`,
		strings.Join(placeholders, ","),
	)
	var counts []countRow
	_ = sqlx.SelectInto(ctx, s.store, &counts, cntSQL, cntArgs...)
	cntMap := make(map[string]int, len(counts))
	for _, c := range counts {
		cntMap[c.CategoryID] = c.Cnt
	}

	out := make([]CategoryWithCount, 0, len(cats))
	for _, c := range cats {
		out = append(out, CategoryWithCount{
			Category:     c,
			ArticleCount: cntMap[c.ID],
		})
	}
	return out, total, nil
}

func (s *Service) queryCategoriesWithCounts(ctx context.Context) ([]CategoryWithCount, error) {
	var cats []domain.Category
	err := sqlx.SelectInto(ctx, s.store, &cats, `SELECT * FROM categories ORDER BY sort_order ASC, created_at ASC`)
	if err != nil {
		return nil, err
	}
	type countRow struct {
		CategoryID string `db:"category_id"`
		Cnt        int    `db:"cnt"`
	}
	var counts []countRow
	_ = sqlx.SelectInto(ctx, s.store, &counts,
		`SELECT category_id, COUNT(*) AS cnt FROM articles WHERE status = ? AND category_id <> '' GROUP BY category_id`,
		domain.ArticleStatusPublished,
	)
	cntMap := make(map[string]int, len(counts))
	for _, c := range counts {
		cntMap[c.CategoryID] = c.Cnt
	}
	out := make([]CategoryWithCount, 0, len(cats))
	for _, c := range cats {
		out = append(out, CategoryWithCount{
			Category:     c,
			ArticleCount: cntMap[c.ID],
		})
	}
	return out, nil
}

// ListTagsWithCounts returns tags ordered by article count DESC, name ASC.
func (s *Service) ListTagsWithCounts(ctx context.Context, limit int) ([]TagWithCount, error) {
	snap, err := s.getChromeSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	if (limit <= 0 || limit > maxCachedChromeTags) && len(snap.tags) >= maxCachedChromeTags {
		return s.queryTagsWithCountsLimited(ctx, limit)
	}
	src := snap.tags
	if limit > 0 && len(src) > limit {
		src = src[:limit]
	}
	out := make([]TagWithCount, len(src))
	copy(out, src)
	return out, nil
}

// ListTagsWithCountsPage filters tags by optional query q (name or slug LIKE) and paginates in SQL.
func (s *Service) ListTagsWithCountsPage(ctx context.Context, q string, page, perPage int) ([]TagWithCount, int, error) {
	limit, offset := dialect.NormalizePage(page, perPage, 60, 200)
	cleanQ := strings.TrimSpace(q)

	var whereSQL string
	var whereArgs []any
	if cleanQ != "" {
		pat := "%" + strings.ToLower(cleanQ) + "%"
		whereSQL = " WHERE LOWER(t.name) LIKE ? OR LOWER(t.slug) LIKE ?"
		whereArgs = []any{pat, pat}
	}

	var total int
	if err := s.store.QueryRowContext(ctx, "SELECT COUNT(*) FROM tags t"+whereSQL, whereArgs...).Scan(&total); err != nil {
		return nil, 0, err
	}
	if total == 0 {
		return []TagWithCount{}, 0, nil
	}

	type tagCountRow struct {
		ID           string `db:"id"`
		Name         string `db:"name"`
		Slug         string `db:"slug"`
		CreatedAt    int64  `db:"created_at"`
		ArticleCount int    `db:"article_count"`
	}
	baseSQL := `
		SELECT t.id, t.name, t.slug, t.created_at, COALESCE(cnts.cnt, 0) AS article_count
		FROM tags t
		LEFT JOIN (
			SELECT atg.tag_id AS tag_id, COUNT(*) AS cnt
			FROM article_tags atg
			INNER JOIN articles a ON a.id = atg.article_id
			WHERE a.status = ?
			GROUP BY atg.tag_id
		) cnts ON cnts.tag_id = t.id` + whereSQL + `
		ORDER BY COALESCE(cnts.cnt, 0) DESC, t.name ASC`
	pageSQL, pageArgs := s.store.Dialect().PaginateSQL(baseSQL, limit, offset)
	args := make([]any, 0, 1+len(whereArgs)+len(pageArgs))
	args = append(args, domain.ArticleStatusPublished)
	args = append(args, whereArgs...)
	args = append(args, pageArgs...)

	var rows []tagCountRow
	err := sqlx.SelectInto(ctx, s.store, &rows, pageSQL, args...)
	if err != nil {
		return nil, 0, err
	}
	out := make([]TagWithCount, 0, len(rows))
	for _, r := range rows {
		out = append(out, TagWithCount{
			Tag: domain.Tag{
				ID:        r.ID,
				Name:      r.Name,
				Slug:      r.Slug,
				CreatedAt: r.CreatedAt,
			},
			ArticleCount: r.ArticleCount,
		})
	}
	return out, total, nil
}

func (s *Service) queryTagsWithCountsLimited(ctx context.Context, limit int) ([]TagWithCount, error) {
	type tagCountRow struct {
		ID           string `db:"id"`
		Name         string `db:"name"`
		Slug         string `db:"slug"`
		CreatedAt    int64  `db:"created_at"`
		ArticleCount int    `db:"article_count"`
	}
	baseSQL := `
		SELECT t.id, t.name, t.slug, t.created_at, COALESCE(cnts.cnt, 0) AS article_count
		FROM tags t
		LEFT JOIN (
			SELECT atg.tag_id AS tag_id, COUNT(*) AS cnt
			FROM article_tags atg
			INNER JOIN articles a ON a.id = atg.article_id
			WHERE a.status = ?
			GROUP BY atg.tag_id
		) cnts ON cnts.tag_id = t.id
		ORDER BY COALESCE(cnts.cnt, 0) DESC, t.name ASC`
	args := []any{domain.ArticleStatusPublished}
	q := baseSQL
	if limit > 0 {
		var pageArgs []any
		q, pageArgs = s.store.Dialect().PaginateSQL(baseSQL, limit, 0)
		args = append(args, pageArgs...)
	}
	var rows []tagCountRow
	err := sqlx.SelectInto(ctx, s.store, &rows, q, args...)
	if err != nil {
		return nil, err
	}
	out := make([]TagWithCount, 0, len(rows))
	for _, r := range rows {
		out = append(out, TagWithCount{
			Tag: domain.Tag{
				ID:        r.ID,
				Name:      r.Name,
				Slug:      r.Slug,
				CreatedAt: r.CreatedAt,
			},
			ArticleCount: r.ArticleCount,
		})
	}
	return out, nil
}

func cloneArchiveStats(src ArchiveStats) ArchiveStats {
	years := make([]ArchiveYear, len(src.Years))
	copy(years, src.Years)
	months := make([]ArchiveMonth, len(src.Months))
	copy(months, src.Months)
	yearCounts := make(map[string]int, len(src.YearCounts))
	for k, v := range src.YearCounts {
		yearCounts[k] = v
	}
	monthCounts := make(map[string]int, len(src.MonthCounts))
	for k, v := range src.MonthCounts {
		monthCounts[k] = v
	}
	return ArchiveStats{
		TotalArticles: src.TotalArticles,
		Years:         years,
		Months:        months,
		YearCounts:    yearCounts,
		MonthCounts:   monthCounts,
	}
}

// ListArchiveMonths returns cached YYYY-MM archive buckets computed from `articles.published_at`
// (replacing legacy `archivedate` tables and strictly excluding Standalone Pages!).
func (s *Service) ListArchiveMonths(ctx context.Context) ([]ArchiveMonth, error) {
	snap, err := s.getChromeSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]ArchiveMonth, len(snap.archiveBuckets))
	copy(out, snap.archiveBuckets)
	return out, nil
}

// ArchiveMonthStats returns revision-coherent total, calendar-year (YYYY), and calendar-month (YYYY-MM) counts
// for published articles in siteTZ (or the configured site timezone when siteTZ is empty).
func (s *Service) ArchiveMonthStats(ctx context.Context, siteTZ string) (ArchiveStats, error) {
	var loc *time.Location
	if trimmed := strings.TrimSpace(siteTZ); trimmed != "" {
		loc = domain.ResolveSiteLocation(trimmed)
	} else {
		loc = s.siteLocation()
	}

	if snap, err := s.getChromeSnapshot(ctx); err == nil && snap != nil && snap.archiveTZ == loc.String() {
		return cloneArchiveStats(snap.archiveStats), nil
	}

	dRev := uint64(0)
	if s.store != nil {
		dRev = s.store.DataRevision()
	}
	tzKey := loc.String()
	if s.caches != nil {
		s.caches.archiveTZMu.RLock()
		if hit, ok := s.caches.archiveByTZ[tzKey]; ok && hit.dataRev == dRev {
			s.caches.archiveTZMu.RUnlock()
			return cloneArchiveStats(hit.stats), nil
		}
		s.caches.archiveTZMu.RUnlock()
	}

	stats, err := s.queryArchiveStatsForLocation(ctx, loc)
	if err != nil {
		return ArchiveStats{}, err
	}
	if s.caches != nil {
		s.caches.archiveTZMu.Lock()
		if len(s.caches.archiveByTZ) >= 16 {
			clear(s.caches.archiveByTZ)
		}
		s.caches.archiveByTZ[tzKey] = tzArchiveCacheEntry{
			dataRev: dRev,
			stats:   stats,
		}
		s.caches.archiveTZMu.Unlock()
	}
	return cloneArchiveStats(stats), nil
}

func (s *Service) queryArchiveMonths(ctx context.Context) ([]ArchiveMonth, error) {
	stats, err := s.queryArchiveStatsForLocation(ctx, s.siteLocation())
	if err != nil {
		return nil, err
	}
	return stats.Months, nil
}

func (s *Service) queryArchiveStatsForLocation(ctx context.Context, loc *time.Location) (ArchiveStats, error) {
	if loc == nil {
		loc = s.siteLocation()
	}
	type pubRow struct {
		PublishedAt int64 `db:"published_at"`
	}
	var rows []pubRow
	err := sqlx.SelectInto(ctx, s.store, &rows,
		`SELECT CASE WHEN published_at > 0 THEN published_at ELSE created_at END AS published_at FROM articles WHERE status = ? ORDER BY published_at DESC`,
		domain.ArticleStatusPublished,
	)
	if err != nil {
		return ArchiveStats{}, err
	}
	yearCounts := make(map[string]int)
	monthCounts := make(map[string]int)
	var yearOrder []string
	var monthOrder []string
	for _, r := range rows {
		ym := time.UnixMilli(r.PublishedAt).In(loc).Format("2006-01")
		yr := ym[:4]
		if yearCounts[yr] == 0 {
			yearOrder = append(yearOrder, yr)
		}
		yearCounts[yr]++
		if monthCounts[ym] == 0 {
			monthOrder = append(monthOrder, ym)
		}
		monthCounts[ym]++
	}
	years := make([]ArchiveYear, 0, len(yearOrder))
	for _, yr := range yearOrder {
		years = append(years, ArchiveYear{Year: yr, Count: yearCounts[yr]})
	}
	months := make([]ArchiveMonth, 0, len(monthOrder))
	for _, ym := range monthOrder {
		months = append(months, ArchiveMonth{YearMonth: ym, Count: monthCounts[ym]})
	}
	return ArchiveStats{
		TotalArticles: len(rows),
		Years:         years,
		Months:        months,
		YearCounts:    yearCounts,
		MonthCounts:   monthCounts,
	}, nil
}

// ListSeries returns all Series ordered by sort_order.
func (s *Service) ListSeries(ctx context.Context) ([]domain.Series, error) {
	var out []domain.Series
	err := sqlx.SelectInto(ctx, s.store, &out, `SELECT * FROM series ORDER BY sort_order ASC, created_at DESC`)
	return out, err
}

// ListSeriesPage filters Series by optional query q and paginates in SQL.
func (s *Service) ListSeriesPage(ctx context.Context, q string, page, perPage int) ([]domain.Series, int, error) {
	limit, offset := dialect.NormalizePage(page, perPage, 18, 100)
	cleanQ := strings.TrimSpace(q)

	var whereSQL string
	var whereArgs []any
	if cleanQ != "" {
		pat := "%" + strings.ToLower(cleanQ) + "%"
		whereSQL = " WHERE LOWER(title) LIKE ? OR LOWER(slug) LIKE ? OR LOWER(description) LIKE ?"
		whereArgs = []any{pat, pat, pat}
	}

	var total int
	if err := s.store.QueryRowContext(ctx, "SELECT COUNT(*) FROM series"+whereSQL, whereArgs...).Scan(&total); err != nil {
		return nil, 0, err
	}
	if total == 0 {
		return []domain.Series{}, 0, nil
	}

	baseSQL := "SELECT * FROM series" + whereSQL + " ORDER BY sort_order ASC, created_at DESC"
	pageSQL, pageArgs := s.store.Dialect().PaginateSQL(baseSQL, limit, offset)
	queryArgs := append(append([]any{}, whereArgs...), pageArgs...)

	var items []domain.Series
	err := sqlx.SelectInto(ctx, s.store, &items, pageSQL, queryArgs...)
	if err != nil {
		return nil, 0, err
	}
	return items, total, nil
}

// GetSeriesBySlug returns a Series and its ordered, Guard-redacted PublicArticleCards.
func (s *Service) GetSeriesBySlug(ctx context.Context, slug string, locale string) (SeriesView, error) {
	var ser domain.Series
	err := sqlx.GetInto(ctx, s.store, &ser, `SELECT * FROM series WHERE slug = ?`, strings.TrimSpace(slug))
	if err != nil {
		return SeriesView{}, err
	}
	var raws []domain.RawArticle
	err = sqlx.SelectInto(ctx, s.store, &raws,
		`SELECT `+articleCardSelectColumns("a")+` FROM series_articles sa
		 INNER JOIN articles a ON a.id = sa.article_id
		 WHERE sa.series_id = ? AND a.status = ?
		 ORDER BY sa.sort_order ASC, a.published_at ASC`,
		ser.ID, domain.ArticleStatusPublished,
	)
	if err != nil {
		return SeriesView{}, err
	}
	cards, err := s.hydrateCards(ctx, raws, locale)
	if err != nil {
		return SeriesView{}, err
	}
	return SeriesView{Series: ser, Articles: cards, TotalArticles: len(cards)}, nil
}

// GetSeriesBySlugPage returns a Series, total published episode count, and a SQL-paginated slice of Guard-redacted episode cards,
// projecting card columns so multi-MB content_md bodies are never loaded unnecessarily.
func (s *Service) GetSeriesBySlugPage(ctx context.Context, slug string, page, perPage int, locale ...string) (SeriesView, error) {
	var ser domain.Series
	err := sqlx.GetInto(ctx, s.store, &ser, `SELECT * FROM series WHERE slug = ?`, strings.TrimSpace(slug))
	if err != nil {
		return SeriesView{}, err
	}
	var total int
	if err := s.store.QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM series_articles sa
		 INNER JOIN articles a ON a.id = sa.article_id
		 WHERE sa.series_id = ? AND a.status = ?`,
		ser.ID, domain.ArticleStatusPublished,
	).Scan(&total); err != nil {
		return SeriesView{}, err
	}
	if total == 0 {
		return SeriesView{Series: ser, Articles: []visibility.PublicArticleCard{}, TotalArticles: 0}, nil
	}

	limit, offset := dialect.NormalizePage(page, perPage, 20, 100)
	baseSQL := `SELECT ` + articleCardSelectColumns("a") + ` FROM series_articles sa
		 INNER JOIN articles a ON a.id = sa.article_id
		 WHERE sa.series_id = ? AND a.status = ?
		 ORDER BY sa.sort_order ASC, a.published_at ASC`
	pageSQL, pageArgs := s.store.Dialect().PaginateSQL(baseSQL, limit, offset)
	queryArgs := append([]any{ser.ID, domain.ArticleStatusPublished}, pageArgs...)

	var raws []domain.RawArticle
	err = sqlx.SelectInto(ctx, s.store, &raws, pageSQL, queryArgs...)
	if err != nil {
		return SeriesView{}, err
	}
	loc := ""
	if len(locale) > 0 {
		loc = strings.TrimSpace(locale[0])
	}
	if loc == "" {
		loc = s.defaultLocale()
	}
	cards, err := s.hydrateCards(ctx, raws, loc)
	if err != nil {
		return SeriesView{}, err
	}
	return SeriesView{Series: ser, Articles: cards, TotalArticles: total}, nil
}

func (s *Service) defaultLocale() string {
	if s.settings != nil {
		if loc := strings.TrimSpace(s.settings.Snapshot().Site.DefaultLocale); loc != "" {
			return loc
		}
	}
	return "zh-CN"
}

// ListLinks returns friend links ordered by sort_order.
func (s *Service) ListLinks(ctx context.Context) ([]domain.Link, error) {
	var out []domain.Link
	err := sqlx.SelectInto(ctx, s.store, &out, `SELECT * FROM links ORDER BY sort_order ASC, created_at ASC`)
	return out, err
}

// ListPublicLinksPage returns distinct ordered link categories (for jump pills) and a SQL-paginated slice of links.
func (s *Service) ListPublicLinksPage(ctx context.Context, category string, page, perPage int) (items []domain.Link, categories []string, total int, err error) {
	limit, offset := dialect.NormalizePage(page, perPage, 36, 200)

	type catRow struct {
		CategoryName string `db:"category_name"`
	}
	var catRows []catRow
	err = sqlx.SelectInto(ctx, s.store, &catRows,
		`SELECT TRIM(category_name) AS category_name FROM links WHERE TRIM(category_name) <> '' GROUP BY TRIM(category_name) ORDER BY MIN(sort_order) ASC, TRIM(category_name) ASC`,
	)
	if err != nil {
		return nil, nil, 0, err
	}
	categories = make([]string, 0, len(catRows))
	for _, r := range catRows {
		if r.CategoryName != "" {
			categories = append(categories, r.CategoryName)
		}
	}

	cleanCat := strings.TrimSpace(category)
	var whereSQL string
	var whereArgs []any
	if cleanCat != "" {
		whereSQL = " WHERE TRIM(category_name) = ?"
		whereArgs = []any{cleanCat}
	}

	if err := s.store.QueryRowContext(ctx, "SELECT COUNT(*) FROM links"+whereSQL, whereArgs...).Scan(&total); err != nil {
		return nil, nil, 0, err
	}
	if total == 0 {
		return []domain.Link{}, categories, 0, nil
	}

	baseSQL := "SELECT * FROM links" + whereSQL + " ORDER BY sort_order ASC, created_at ASC"
	pageSQL, pageArgs := s.store.Dialect().PaginateSQL(baseSQL, limit, offset)
	queryArgs := append(append([]any{}, whereArgs...), pageArgs...)

	err = sqlx.SelectInto(ctx, s.store, &items, pageSQL, queryArgs...)
	if err != nil {
		return nil, nil, 0, err
	}
	return items, categories, total, nil
}

// ListFollowsWithItems returns RSS Follow subscriptions and their latest items using 2 bounded batch queries.
func (s *Service) ListFollowsWithItems(ctx context.Context, itemsPerFollow int) ([]FollowGroup, error) {
	var follows []domain.Follow
	err := sqlx.SelectInto(ctx, s.store, &follows, `SELECT * FROM follows ORDER BY sort_order ASC, created_at ASC`)
	if err != nil || len(follows) == 0 {
		return []FollowGroup{}, err
	}
	return s.hydrateFollowGroups(ctx, follows, itemsPerFollow), nil
}

// ListFollowsWithItemsPage returns a paginated slice of RSS Follow subscriptions and their latest items.
func (s *Service) ListFollowsWithItemsPage(ctx context.Context, page int, perPage int, itemsPerFollow int) ([]FollowGroup, int, error) {
	limit, offset := dialect.NormalizePage(page, perPage, 18, 100)
	var total int
	if err := s.store.QueryRowContext(ctx, `SELECT COUNT(*) FROM follows`).Scan(&total); err != nil {
		return nil, 0, err
	}
	if total == 0 {
		return []FollowGroup{}, 0, nil
	}
	pageSQL, pageArgs := s.store.Dialect().PaginateSQL(
		`SELECT * FROM follows ORDER BY sort_order ASC, created_at ASC`,
		limit, offset,
	)
	var follows []domain.Follow
	err := sqlx.SelectInto(ctx, s.store, &follows, pageSQL, pageArgs...)
	if err != nil {
		return nil, 0, err
	}
	return s.hydrateFollowGroups(ctx, follows, itemsPerFollow), total, nil
}

func (s *Service) hydrateFollowGroups(ctx context.Context, follows []domain.Follow, itemsPerFollow int) []FollowGroup {
	if len(follows) == 0 {
		return []FollowGroup{}
	}
	if itemsPerFollow <= 0 {
		itemsPerFollow = 5
	}
	placeholders := make([]string, len(follows))
	args := make([]any, len(follows))
	for i, f := range follows {
		placeholders[i] = "?"
		args[i] = f.ID
	}
	q := fmt.Sprintf(
		`SELECT * FROM follow_items WHERE follow_id IN (%s) ORDER BY published_at DESC`,
		strings.Join(placeholders, ","),
	)
	var allItems []domain.FollowItem
	_ = sqlx.SelectInto(ctx, s.store, &allItems, q, args...)
	byFollow := make(map[string][]domain.FollowItem, len(follows))
	for _, it := range allItems {
		if len(byFollow[it.FollowID]) < itemsPerFollow {
			byFollow[it.FollowID] = append(byFollow[it.FollowID], it)
		}
	}
	out := make([]FollowGroup, 0, len(follows))
	for _, f := range follows {
		out = append(out, FollowGroup{Follow: f, Items: byFollow[f.ID]})
	}
	return out
}

// RecordView coalesces view_count and zero-PII daily PV/UV + top referer/article counters
// into the non-blocking write-behind analytics buffer, eliminating per-GET SQLite write-lock contention.
func (s *Service) RecordView(ctx context.Context, targetType string, targetID string, actorFPHash string, refererHost string) {
	s.RecordViewWithGeo(ctx, targetType, targetID, actorFPHash, refererHost, "")
}

// RecordViewWithGeo coalesces view_count, zero-PII daily PV/UV, validated top referer host, and optional country code
// (`kind = 'country'`) into the non-blocking write-behind analytics buffer.
func (s *Service) RecordViewWithGeo(
	_ context.Context,
	targetType string,
	targetID string,
	actorFPHash string,
	refererHost string,
	countryCode string,
	siteTZ ...string,
) {
	if s == nil || s.views == nil {
		return
	}
	var tzOverride string
	if len(siteTZ) > 0 {
		tzOverride = strings.TrimSpace(siteTZ[0])
	} else if targetType != "" && targetType != domain.TargetTypeArticle && targetType != domain.TargetTypePage {
		// Support 6-arg shorthand (ctx, articleID, visitorHash, refererHost, countryCode, siteTZ).
		tzOverride = strings.TrimSpace(countryCode)
		countryCode = refererHost
		refererHost = actorFPHash
		actorFPHash = targetID
		targetID = targetType
		targetType = domain.TargetTypeArticle
	}

	loc := s.siteLocation()
	if tzOverride != "" {
		loc = domain.ResolveSiteLocation(tzOverride)
	}
	today := time.Now().In(loc).Format("2006-01-02")

	isNewUV := false
	if actorFPHash != "" {
		sum := sha256.Sum256([]byte(actorFPHash))
		fpKey := binary.LittleEndian.Uint64(sum[:8])
		s.uvMu.Lock()
		if s.uvDate != today {
			s.uvDate = today
			s.uvVisited = make(map[uint64]struct{})
		}
		if len(s.uvVisited) < 65536 {
			if _, seen := s.uvVisited[fpKey]; !seen {
				s.uvVisited[fpKey] = struct{}{}
				isNewUV = true
			}
		}
		s.uvMu.Unlock()
	}

	uvDelta := int64(0)
	if isNewUV {
		uvDelta = 1
	}

	s.views.RecordViewWithGeo(targetType, targetID, today, uvDelta, refererHost, countryCode)
}

// PruneAnalyticsTopItems deletes `analytics_top_items` rows (`kind IN ('referer', 'country')`)
// whose stat_date is older than keepDays (defaulting to 180 days when keepDays <= 0).
func (s *Service) PruneAnalyticsTopItems(ctx context.Context, keepDays int) error {
	if s == nil || s.store == nil {
		return nil
	}
	if keepDays <= 0 {
		keepDays = 180
	}
	cutoff := time.Now().In(s.siteLocation()).AddDate(0, 0, -keepDays).Format("2006-01-02")
	return s.store.WithBgTx(ctx, func(tx *sqlx.Tx) error {
		_, err := tx.ExecContext(
			ctx,
			`DELETE FROM analytics_top_items WHERE kind IN ('referer', 'country') AND stat_date < ?`,
			cutoff,
		)
		return err
	})
}

func (s *Service) loadCategoriesMap(ctx context.Context) (map[string]domain.Category, error) {
	snap, err := s.getChromeSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	return snap.catsByID, nil
}

func (s *Service) loadTagsForArticles(ctx context.Context, raws []domain.RawArticle) (map[string][]domain.Tag, error) {
	out := make(map[string][]domain.Tag)
	if len(raws) == 0 {
		return out, nil
	}
	placeholders := make([]string, len(raws))
	args := make([]any, len(raws))
	for i, r := range raws {
		placeholders[i] = "?"
		args[i] = r.ID
	}
	type joinedTag struct {
		ArticleID string `db:"article_id"`
		ID        string `db:"id"`
		Name      string `db:"name"`
		Slug      string `db:"slug"`
		CreatedAt int64  `db:"created_at"`
	}
	q := fmt.Sprintf(
		`SELECT at.article_id, t.id, t.name, t.slug, t.created_at
		 FROM article_tags at
		 INNER JOIN tags t ON t.id = at.tag_id
		 WHERE at.article_id IN (%s)
		 ORDER BY t.name ASC`,
		strings.Join(placeholders, ","),
	)
	var rows []joinedTag
	err := sqlx.SelectInto(ctx, s.store, &rows, q, args...)
	if err != nil {
		return out, err
	}
	for _, r := range rows {
		out[r.ArticleID] = append(out[r.ArticleID], domain.Tag{
			ID:        r.ID,
			Name:      r.Name,
			Slug:      r.Slug,
			CreatedAt: r.CreatedAt,
		})
	}
	return out, nil
}

func (s *Service) siteLocation() *time.Location {
	tz := "Asia/Shanghai"
	if s.settings != nil {
		if v := strings.TrimSpace(s.settings.Snapshot().Site.TimeZone); v != "" {
			tz = v
		}
	}
	return domain.ResolveSiteLocation(tz)
}

func parseYearMonthBounds(ym string, loc *time.Location) (int64, int64, bool) {
	t, err := time.ParseInLocation("2006-01", ym, loc)
	if err != nil {
		return 0, 0, false
	}
	next := t.AddDate(0, 1, 0)
	return t.UnixMilli(), next.UnixMilli(), true
}

func truncateForSummary(s string, maxRunes int) string {
	s = strings.TrimSpace(s)
	if s == "" || maxRunes <= 0 {
		return ""
	}
	if len(s) <= maxRunes {
		return s
	}
	count := 0
	for idx := 0; idx < len(s); {
		if count == maxRunes {
			return strings.TrimSpace(s[:idx]) + "…"
		}
		_, w := utf8.DecodeRuneInString(s[idx:])
		idx += w
		count++
	}
	return s
}

func (s *Service) queryPublicAuthors(ctx context.Context) (PublicAuthorProfile, []PublicAuthorProfile, map[string]PublicAuthorProfile) {
	var rows []domain.User
	_ = sqlx.SelectInto(ctx, s.store, &rows,
		`SELECT id, username, display_name, avatar_url, bio, website_url, role
		 FROM users
		 WHERE status = ? AND (role = ? OR role = ?)
		 ORDER BY created_at ASC`,
		domain.UserStatusActive, domain.RoleAdmin, domain.RoleEditor,
	)
	type authorCnt struct {
		AuthorID string `db:"author_id"`
		Cnt      int    `db:"cnt"`
	}
	var cntRows []authorCnt
	_ = sqlx.SelectInto(ctx, s.store, &cntRows,
		`SELECT author_id, COUNT(*) AS cnt FROM articles WHERE status = ? GROUP BY author_id`,
		domain.ArticleStatusPublished,
	)
	cntMap := make(map[string]int, len(cntRows))
	for _, c := range cntRows {
		cntMap[c.AuthorID] = c.Cnt
	}

	authors := make([]PublicAuthorProfile, 0, len(rows))
	byID := make(map[string]PublicAuthorProfile, len(rows))
	var owner PublicAuthorProfile
	for _, u := range rows {
		name := strings.TrimSpace(u.DisplayName)
		if name == "" {
			name = u.Username
		}
		avatar := strings.TrimSpace(u.AvatarURL)
		if avatar == "" && u.Username != "" {
			avatar = "/avatars/" + u.Username
		}
		prof := PublicAuthorProfile{
			ID:           u.ID,
			Username:     u.Username,
			DisplayName:  name,
			AvatarURL:    avatar,
			Bio:          u.Bio,
			WebsiteURL:   u.WebsiteURL,
			Role:         u.Role,
			ArticleCount: cntMap[u.ID],
		}
		authors = append(authors, prof)
		byID[u.ID] = prof
		if owner.ID == "" && u.Role == domain.RoleAdmin {
			owner = prof
		}
	}
	if owner.ID == "" && len(authors) > 0 {
		owner = authors[0]
	}
	if owner.DisplayName == "" && s.settings != nil {
		owner.DisplayName = s.settings.Snapshot().Site.Title
		owner.AvatarURL = "/favicon.svg"
	}
	return owner, authors, byID
}

func (s *Service) queryPublicSiteStats(ctx context.Context, catCount int) PublicSiteStats {
	var artCnt, cmtCnt, tagCnt, serCnt, bookCnt int
	var artViews, pageViews, minPubAt int64
	_ = s.store.QueryRowContext(
		ctx,
		`SELECT COUNT(*), COALESCE(SUM(view_count), 0), COALESCE(MIN(published_at), 0) FROM articles WHERE status = ?`,
		domain.ArticleStatusPublished,
	).Scan(&artCnt, &artViews, &minPubAt)
	_ = s.store.QueryRowContext(
		ctx,
		`SELECT COALESCE(SUM(view_count), 0) FROM pages WHERE is_published = ?`,
		true,
	).Scan(&pageViews)
	_ = s.store.QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM comments WHERE status = ? AND is_deleted = ?`,
		domain.CommentStatusApproved, false,
	).Scan(&cmtCnt)
	_ = s.store.QueryRowContext(ctx, `SELECT COUNT(*) FROM tags`).Scan(&tagCnt)
	_ = s.store.QueryRowContext(ctx, `SELECT COUNT(*) FROM series`).Scan(&serCnt)
	_ = s.store.QueryRowContext(ctx, `SELECT COUNT(*) FROM books`).Scan(&bookCnt)

	uptimeDays := 1
	if minPubAt > 0 {
		days := int(time.Since(time.UnixMilli(minPubAt)).Hours()/24) + 1
		if days > uptimeDays {
			uptimeDays = days
		}
	}
	return PublicSiteStats{
		ArticleCount:  artCnt,
		CommentCount:  cmtCnt,
		ViewCount:     artViews + pageViews,
		CategoryCount: catCount,
		TagCount:      tagCnt,
		SeriesCount:   serCnt,
		BookCount:     bookCnt,
		UptimeDays:    uptimeDays,
	}
}

// GetPublicChromeExtras returns revision-coherent Owner, Authors, SiteStats, ArchiveBuckets, and Sidebar Article Cards with zero SQL on cache hits.
func (s *Service) GetPublicChromeExtras(ctx context.Context, locale string, sidebarPostLimit int) PublicChromeExtras {
	snap, err := s.getChromeSnapshot(ctx)
	if err != nil || snap == nil {
		return PublicChromeExtras{}
	}
	if sidebarPostLimit <= 0 {
		sidebarPostLimit = 6
	}
	if sidebarPostLimit > maxCachedRecentCmts {
		sidebarPostLimit = maxCachedRecentCmts
	}
	localeCards := s.chromeCardsForLocale(snap, locale)
	cloneCards := func(src []visibility.PublicArticleCard, n int) []visibility.PublicArticleCard {
		if len(src) > n {
			src = src[:n]
		}
		out := make([]visibility.PublicArticleCard, len(src))
		for i, c := range src {
			c.ViewCount += s.views.PendingArticleViews(c.ID)
			if c.IsPasswordProtected {
				c.Summary = s.guard.ProtectedNotice(locale)
			}
			out[i] = c
		}
		return out
	}
	recentCards := cloneCards(localeCards[0], sidebarPostLimit)
	viewedCards := cloneCards(localeCards[1], sidebarPostLimit)
	cmtedCards := cloneCards(localeCards[2], sidebarPostLimit)

	authorsCopy := make([]PublicAuthorProfile, len(snap.authors))
	copy(authorsCopy, snap.authors)
	archivesCopy := make([]ArchiveMonth, len(snap.archiveBuckets))
	copy(archivesCopy, snap.archiveBuckets)
	archiveYearsCopy := make([]ArchiveYear, len(snap.archiveStats.Years))
	copy(archiveYearsCopy, snap.archiveStats.Years)

	stats := snap.stats
	stats.ViewCount += s.views.TotalPendingArticleViews()

	return PublicChromeExtras{
		Owner:              snap.owner,
		Authors:            authorsCopy,
		Stats:              stats,
		ArchiveBuckets:     archivesCopy,
		ArchiveYearBuckets: archiveYearsCopy,
		RecentPosts:        recentCards,
		MostViewedPosts:    viewedCards,
		MostCommentedPosts: cmtedCards,
	}
}

// ListRelatedArticles returns up to limit public Guard-redacted articles sharing tags or category with cur.
// Password-protected posts never participate as source or candidate (Surface 3 & 8 zero-leak invariant).
func (s *Service) ListRelatedArticles(ctx context.Context, cur domain.RawArticle, locale string, limit int) []visibility.PublicArticleCard {
	if limit <= 0 || !s.guard.IsPublicIndexable(cur) {
		return nil
	}
	if limit > 12 {
		limit = 12
	}
	seen := map[string]bool{cur.ID: true}
	var collected []domain.RawArticle

	cardColsA := articleCardSelectColumns("a")
	tagSQL, tagArgs := s.store.Dialect().PaginateSQL(
		`SELECT `+cardColsA+` FROM articles a
		 WHERE a.id <> ? AND a.status = ? AND a.password_ciphertext = ''
		   AND EXISTS (
		     SELECT 1 FROM article_tags at1
		     INNER JOIN article_tags at2 ON at2.tag_id = at1.tag_id
		     WHERE at2.article_id = ? AND at1.article_id = a.id
		   )
		 ORDER BY a.published_at DESC`,
		limit, 0,
	)
	var byTag []domain.RawArticle
	if err := sqlx.SelectInto(
		ctx, s.store, &byTag, tagSQL,
		append([]any{cur.ID, domain.ArticleStatusPublished, cur.ID}, tagArgs...)...,
	); err == nil {
		for _, r := range byTag {
			if !seen[r.ID] && s.guard.IsPublicIndexable(r) {
				seen[r.ID] = true
				collected = append(collected, r)
			}
		}
	}

	if len(collected) < limit && strings.TrimSpace(cur.CategoryID) != "" {
		rem := limit - len(collected)
		catSQL, catArgs := s.store.Dialect().PaginateSQL(
			`SELECT `+articleCardSelectColumns("")+` FROM articles
			 WHERE category_id = ? AND id <> ? AND status = ? AND password_ciphertext = ''
			 ORDER BY published_at DESC`,
			rem+4, 0,
		)
		var byCat []domain.RawArticle
		if err := sqlx.SelectInto(
			ctx, s.store, &byCat, catSQL,
			append([]any{cur.CategoryID, cur.ID, domain.ArticleStatusPublished}, catArgs...)...,
		); err == nil {
			for _, r := range byCat {
				if len(collected) >= limit {
					break
				}
				if !seen[r.ID] && s.guard.IsPublicIndexable(r) {
					seen[r.ID] = true
					collected = append(collected, r)
				}
			}
		}
	}

	if len(collected) == 0 {
		return nil
	}
	cards, _ := s.hydrateCards(ctx, collected, locale)
	return cards
}

// ListRandomArticles returns a deterministic pseudo-random sample of public Guard-redacted articles from the in-memory chrome snapshot (zero SQL).
func (s *Service) ListRandomArticles(ctx context.Context, excludeID string, locale string, limit int) []visibility.PublicArticleCard {
	if limit <= 0 {
		return nil
	}
	snap, err := s.getChromeSnapshot(ctx)
	if err != nil || snap == nil {
		return nil
	}
	localeCards := s.chromeCardsForLocale(snap, locale)
	seen := map[string]bool{excludeID: true}
	var pool []visibility.PublicArticleCard
	for _, src := range [][]visibility.PublicArticleCard{localeCards[0], localeCards[1]} {
		for _, c := range src {
			if !seen[c.ID] && !c.IsPasswordProtected {
				seen[c.ID] = true
				c.ViewCount += s.views.PendingArticleViews(c.ID)
				if c.IsPasswordProtected {
					c.Summary = s.guard.ProtectedNotice(locale)
				}
				pool = append(pool, c)
			}
		}
	}
	if len(pool) == 0 {
		return nil
	}
	if len(pool) > limit {
		offset := 0
		for i := 0; i < len(excludeID); i++ {
			offset = (offset*31 + int(excludeID[i])) % len(pool)
		}
		picked := make([]visibility.PublicArticleCard, 0, limit)
		for i := 0; i < limit; i++ {
			picked = append(picked, pool[(offset+i)%len(pool)])
		}
		pool = picked
	}
	return pool
}

// Helper ensuring `id` import is actively referenced for entity creation helpers in content package.
var _ = id.Next
