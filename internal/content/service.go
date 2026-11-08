package content

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
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
	"github.com/Willh92/Volo-go/internal/render"
	"github.com/Willh92/Volo-go/internal/repo"
	"github.com/Willh92/Volo-go/internal/security"
	"github.com/Willh92/Volo-go/internal/visibility"
	"github.com/Willh92/Volo-go/pkg/id"
	"github.com/Willh92/Volo-go/pkg/plugin"
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

// SeriesView pairs a Series with its ordered public article cards.
type SeriesView struct {
	domain.Series
	Articles []visibility.PublicArticleCard `json:"articles"`
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

	mediaMu   sync.RWMutex
	mediaDims map[string][2]int

	bookChapterMu    sync.Mutex
	bookChapterCache map[string]cachedBookChapter

	onArticlePublished atomic.Pointer[func(context.Context, domain.RawArticle)]
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
	dialer := opts.OutboundDialer
	if dialer == nil {
		dialer = &security.OutboundSafeDialer{Timeout: 10 * time.Second}
	}
	return &Service{
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
	assets, err := sqlx.Select[domain.MediaAsset](ctx, s.store, q, pageArgs...)
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
	if changed && s.guard != nil {
		s.guard.PurgeCache()
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
	if changed && s.guard != nil {
		s.guard.PurgeCache()
	}
}

// LookupMediaDims returns cached intrinsic pixel dimensions for a local/S3 media URL.
func (s *Service) LookupMediaDims(src string) (int, int, bool) {
	s.mediaMu.RLock()
	wh, ok := s.mediaDims[strings.TrimSpace(src)]
	s.mediaMu.RUnlock()
	if !ok {
		return 0, 0, false
	}
	return wh[0], wh[1], true
}

// PublishDueScheduled promotes any scheduled articles whose scheduled_at <= nowMs to published.
func (s *Service) PublishDueScheduled(ctx context.Context) (int, error) {
	now := time.Now().UnixMilli()
	due, err := sqlx.Select[domain.RawArticle](
		ctx, s.store,
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
	Page         int
	PageSize     int
	CategorySlug string
	TagSlug      string
	YearMonth    string // YYYY-MM
	Locale       string
}

// ListPublicCards queries published articles, enriches categories/tags in bulk, and passes every row
// through visibility.Guard.ToCard (Surface 1 & 2 protection).
func (s *Service) ListPublicCards(ctx context.Context, f ArticleListFilter) ([]visibility.PublicArticleCard, int, error) {
	limit, offset := dialect.NormalizePage(f.Page, f.PageSize, 15, 100)

	where := []string{"a.status = ?"}
	args := []any{domain.ArticleStatusPublished}

	if slug := strings.TrimSpace(f.CategorySlug); slug != "" {
		cat, err := sqlx.Get[domain.Category](ctx, s.store, `SELECT id FROM categories WHERE slug = ?`, slug)
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
		tag, err := sqlx.Get[domain.Tag](ctx, s.store, `SELECT id FROM tags WHERE slug = ?`, tagSlug)
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
		where = append(where, "a.published_at >= ? AND a.published_at < ?")
		args = append(args, startMS, endMS)
	}

	whereClause := strings.Join(where, " AND ")
	var total int
	if err := s.store.QueryRowContext(ctx, "SELECT COUNT(*) FROM articles a WHERE "+whereClause, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	if total == 0 {
		return []visibility.PublicArticleCard{}, 0, nil
	}

	baseSQL := "SELECT a.* FROM articles a WHERE " + whereClause + " ORDER BY a.is_pinned DESC, a.published_at DESC"
	pageSQL, pageArgs := s.store.Dialect().PaginateSQL(baseSQL, limit, offset)
	queryArgs := append(append([]any{}, args...), pageArgs...)

	raws, err := sqlx.Select[domain.RawArticle](ctx, s.store, pageSQL, queryArgs...)
	if err != nil {
		return nil, 0, err
	}
	cards, err := s.hydrateCards(ctx, raws, f.Locale)
	return cards, total, err
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
	return sqlx.Get[domain.RawArticle](
		ctx, s.store,
		`SELECT * FROM articles WHERE slug = ? AND status = ?`,
		strings.TrimSpace(slug), domain.ArticleStatusPublished,
	)
}

// GetRawArticleBySlugAnyStatus fetches a RawArticle regardless of publication status (used only when gated by Staff ownership or valid ShareToken).
func (s *Service) GetRawArticleBySlugAnyStatus(ctx context.Context, slug string) (domain.RawArticle, error) {
	return sqlx.Get[domain.RawArticle](
		ctx, s.store,
		`SELECT * FROM articles WHERE slug = ?`,
		strings.TrimSpace(slug),
	)
}

// GetRawArticleByID fetches a RawArticle by ID.
func (s *Service) GetRawArticleByID(ctx context.Context, articleID string) (domain.RawArticle, error) {
	return sqlx.Get[domain.RawArticle](ctx, s.store, `SELECT * FROM articles WHERE id = ?`, strings.TrimSpace(articleID))
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
		if u, err := sqlx.Get[domain.User](ctx, s.store, `SELECT display_name, username, avatar_url, bio, website_url FROM users WHERE id = ?`, raw.AuthorID); err == nil {
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
			if c, err := sqlx.Get[domain.Category](ctx, s.store, `SELECT * FROM categories WHERE id = ?`, raw.CategoryID); err == nil {
				catPtr = &c
			}
		}
	}
	tagsMap, _ := s.loadTagsForArticles(ctx, []domain.RawArticle{raw})
	alts := s.listArticleTranslations(ctx, raw)
	raw.ViewCount += s.views.PendingArticleViews(raw.ID)

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
	olderRows, err := sqlx.Select[domain.RawArticle](
		ctx, s.store, olderSQL,
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
	newerRows, err := sqlx.Select[domain.RawArticle](
		ctx, s.store, newerSQL,
		append([]any{domain.ArticleStatusPublished, cur.PublishedAt, cur.PublishedAt, cur.ID}, newerArgs...)...,
	)
	if err == nil && len(newerRows) > 0 {
		n := s.guard.ToNeighbor(newerRows[0])
		next = &n
	}
	return prev, next
}

// ListInboundBacklinks enforces Surface 8:
// - Password-protected target articles expose ZERO backlinks publicly.
// - Password-protected source articles NEVER appear as backlink sources.
func (s *Service) ListInboundBacklinks(ctx context.Context, target domain.RawArticle) []visibility.PublicNeighbor {
	if !s.guard.IsPublicIndexable(target) {
		return nil
	}
	rows, err := sqlx.Select[domain.RawArticle](
		ctx, s.store,
		`SELECT a.id, a.title, a.slug, a.published_at, a.password_ciphertext
		 FROM article_backlinks b
		 INNER JOIN articles a ON a.id = b.source_article_id
		 WHERE b.target_article_id = ? AND a.status = ? AND a.password_ciphertext = ''
		 ORDER BY a.published_at DESC`,
		target.ID, domain.ArticleStatusPublished,
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
		target, err := sqlx.Get[domain.RawArticle](ctx, tx, `SELECT id, password_ciphertext FROM articles WHERE slug = ?`, slug)
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
	rows, err := sqlx.Select[domain.RawArticle](
		ctx, s.store,
		`SELECT title, slug, lang, password_ciphertext
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
			URL:   visibility.ArticlePath(r.Slug),
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
	p, err := sqlx.Get[domain.Page](
		ctx, s.store,
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
		peers, pErr := sqlx.Select[domain.Page](
			ctx, s.store,
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
			if strings.TrimSpace(it.I18nTitlesJSON) != "" {
				var m map[string]string
				if json.Unmarshal([]byte(it.I18nTitlesJSON), &m) == nil {
					if locTitle := plugin.ResolveLocalized(m, locale); locTitle != "" {
						title = locTitle
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

func (s *Service) queryCategoriesWithCounts(ctx context.Context) ([]CategoryWithCount, error) {
	cats, err := sqlx.Select[domain.Category](ctx, s.store, `SELECT * FROM categories ORDER BY sort_order ASC, created_at ASC`)
	if err != nil {
		return nil, err
	}
	type countRow struct {
		CategoryID string `db:"category_id"`
		Cnt        int    `db:"cnt"`
	}
	counts, _ := sqlx.Select[countRow](
		ctx, s.store,
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
	rows, err := sqlx.Select[tagCountRow](ctx, s.store, q, args...)
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

func (s *Service) queryArchiveMonths(ctx context.Context) ([]ArchiveMonth, error) {
	type pubRow struct {
		PublishedAt int64 `db:"published_at"`
	}
	rows, err := sqlx.Select[pubRow](
		ctx, s.store,
		`SELECT published_at FROM articles WHERE status = ? ORDER BY published_at DESC`,
		domain.ArticleStatusPublished,
	)
	if err != nil {
		return nil, err
	}
	loc := s.siteLocation()
	counts := make(map[string]int)
	var order []string
	for _, r := range rows {
		if r.PublishedAt <= 0 {
			continue
		}
		ym := time.UnixMilli(r.PublishedAt).In(loc).Format("2006-01")
		if counts[ym] == 0 {
			order = append(order, ym)
		}
		counts[ym]++
	}
	out := make([]ArchiveMonth, 0, len(order))
	for _, ym := range order {
		out = append(out, ArchiveMonth{YearMonth: ym, Count: counts[ym]})
	}
	return out, nil
}

// ListSeries returns all Series ordered by sort_order.
func (s *Service) ListSeries(ctx context.Context) ([]domain.Series, error) {
	return sqlx.Select[domain.Series](ctx, s.store, `SELECT * FROM series ORDER BY sort_order ASC, created_at DESC`)
}

// GetSeriesBySlug returns a Series and its ordered, Guard-redacted PublicArticleCards.
func (s *Service) GetSeriesBySlug(ctx context.Context, slug string, locale string) (SeriesView, error) {
	ser, err := sqlx.Get[domain.Series](ctx, s.store, `SELECT * FROM series WHERE slug = ?`, strings.TrimSpace(slug))
	if err != nil {
		return SeriesView{}, err
	}
	raws, err := sqlx.Select[domain.RawArticle](
		ctx, s.store,
		`SELECT a.* FROM series_articles sa
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
	return SeriesView{Series: ser, Articles: cards}, nil
}

// ListLinks returns friend links ordered by sort_order.
func (s *Service) ListLinks(ctx context.Context) ([]domain.Link, error) {
	return sqlx.Select[domain.Link](ctx, s.store, `SELECT * FROM links ORDER BY sort_order ASC, created_at ASC`)
}

// ListFollowsWithItems returns RSS Follow subscriptions and their latest items using 2 bounded batch queries.
func (s *Service) ListFollowsWithItems(ctx context.Context, itemsPerFollow int) ([]FollowGroup, error) {
	follows, err := sqlx.Select[domain.Follow](ctx, s.store, `SELECT * FROM follows ORDER BY sort_order ASC, created_at ASC`)
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
	follows, err := sqlx.Select[domain.Follow](ctx, s.store, pageSQL, pageArgs...)
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
	allItems, _ := sqlx.Select[domain.FollowItem](ctx, s.store, q, args...)
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
func (s *Service) RecordView(_ context.Context, targetType string, targetID string, actorFPHash string, refererHost string) {
	today := time.Now().In(s.siteLocation()).Format("2006-01-02")

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

	s.views.RecordView(targetType, targetID, today, uvDelta, refererHost)
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
	rows, err := sqlx.Select[joinedTag](ctx, s.store, q, args...)
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

var defaultCSTLocation = time.FixedZone("CST", 8*3600)

func (s *Service) siteLocation() *time.Location {
	if s.settings != nil {
		tz := strings.TrimSpace(s.settings.Snapshot().Site.TimeZone)
		if tz != "" {
			if cached, ok := tzLocationCache.Load(tz); ok {
				return cached.(*time.Location)
			}
			if loc, err := time.LoadLocation(tz); err == nil {
				tzLocationCache.Store(tz, loc)
				return loc
			}
		}
	}
	return defaultCSTLocation
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
	rows, _ := sqlx.Select[domain.User](
		ctx, s.store,
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
	cntRows, _ := sqlx.Select[authorCnt](
		ctx, s.store,
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
	recentCards := cloneCards(snap.recentCards, sidebarPostLimit)
	viewedCards := cloneCards(snap.mostViewedCards, sidebarPostLimit)
	cmtedCards := cloneCards(snap.mostCommentedCards, sidebarPostLimit)

	authorsCopy := make([]PublicAuthorProfile, len(snap.authors))
	copy(authorsCopy, snap.authors)
	archivesCopy := make([]ArchiveMonth, len(snap.archiveBuckets))
	copy(archivesCopy, snap.archiveBuckets)

	stats := snap.stats
	stats.ViewCount += s.views.TotalPendingArticleViews()

	return PublicChromeExtras{
		Owner:              snap.owner,
		Authors:            authorsCopy,
		Stats:              stats,
		ArchiveBuckets:     archivesCopy,
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

	tagSQL, tagArgs := s.store.Dialect().PaginateSQL(
		`SELECT DISTINCT a.* FROM articles a
		 INNER JOIN article_tags at1 ON at1.article_id = a.id
		 INNER JOIN article_tags at2 ON at2.tag_id = at1.tag_id
		 WHERE at2.article_id = ? AND a.id <> ? AND a.status = ? AND a.password_ciphertext = ''
		 ORDER BY a.published_at DESC`,
		limit, 0,
	)
	if byTag, err := sqlx.Select[domain.RawArticle](
		ctx, s.store, tagSQL,
		append([]any{cur.ID, cur.ID, domain.ArticleStatusPublished}, tagArgs...)...,
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
			`SELECT * FROM articles
			 WHERE category_id = ? AND id <> ? AND status = ? AND password_ciphertext = ''
			 ORDER BY published_at DESC`,
			rem+4, 0,
		)
		if byCat, err := sqlx.Select[domain.RawArticle](
			ctx, s.store, catSQL,
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
	seen := map[string]bool{excludeID: true}
	var pool []visibility.PublicArticleCard
	for _, src := range [][]visibility.PublicArticleCard{snap.recentCards, snap.mostViewedCards} {
		for _, c := range src {
			if !seen[c.ID] && !c.IsPasswordProtected {
				seen[c.ID] = true
				c.ViewCount += s.views.PendingArticleViews(c.ID)
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
