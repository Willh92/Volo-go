package content

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

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
	ID               string                         `json:"id"`
	Title            string                         `json:"title"`
	Slug             string                         `json:"slug"`
	URL              string                         `json:"url"`
	HTML             string                         `json:"html"`
	PlainSummary     string                         `json:"plain_summary"`
	AllowComment     bool                           `json:"allow_comment"`
	ShowTOC          bool                           `json:"show_toc"`
	TOC              []render.TOCItem               `json:"toc,omitempty"`
	Lang             string                         `json:"lang"`
	TranslationGroup string                         `json:"translation_group,omitempty"`
	Translations     []visibility.TranslationAlternate `json:"translations,omitempty"`
	ViewCount        int64                          `json:"view_count"`
	CommentCount     int                            `json:"comment_count"`
	UpdatedAt        int64                          `json:"updated_at"`
	NeedsMath        bool                           `json:"needs_math"`
	NeedsMermaid     bool                           `json:"needs_mermaid"`
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

	uvMu      sync.Mutex
	uvDate    string
	uvVisited map[string]struct{}

	mediaMu   sync.RWMutex
	mediaDims map[string][2]int
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
		store:     opts.Store,
		keys:      opts.Keys,
		settings:  opts.Settings,
		passports: opts.Passports,
		pow:       opts.PoW,
		guard:     guard,
		search:    searchIdx,
		renderer:  rnd,
		plugins:   opts.Plugins,
		catalog:   opts.Catalog,
		dialer:    dialer,
		uvVisited: make(map[string]struct{}),
		mediaDims: make(map[string][2]int),
	}
}

func (s *Service) Guard() *visibility.Guard        { return s.guard }
func (s *Service) SearchIndex() *visibility.SearchIndex { return s.search }
func (s *Service) Renderer() *render.Engine        { return s.renderer }

// WarmCaches preloads the 8 MiB two-stage search index and media intrinsic dimensions map.
func (s *Service) WarmCaches(ctx context.Context) error {
	_ = s.RefreshMediaDimsCache(ctx)
	return s.search.Rebuild(ctx)
}

// RefreshMediaDimsCache caches (width, height) of known media_assets so rendered <img> tags get zero-CLS dimensions.
func (s *Service) RefreshMediaDimsCache(ctx context.Context) error {
	assets, err := sqlx.Select[domain.MediaAsset](
		ctx, s.store,
		`SELECT public_url, width, height FROM media_assets WHERE width > 0 AND height > 0`,
	)
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
	s.mediaDims = next
	s.mediaMu.Unlock()
	return nil
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
		for _, a := range due {
			pubAt := a.ScheduledAt
			if pubAt <= 0 {
				pubAt = now
			}
			if _, uErr := tx.ExecContext(
				ctx,
				`UPDATE articles SET status = ?, published_at = ?, updated_at = ? WHERE id = ?`,
				domain.ArticleStatusPublished, pubAt, now, a.ID,
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
	return len(due), nil
}

// ResolveRedirect checks custom & legacy 301/302 rules in `redirects` AND `articles.legacy_permalink`,
// guaranteeing zero 404 dead links for migrated `.html` URLs!
func (s *Service) ResolveRedirect(ctx context.Context, reqPath string) (target string, statusCode int, matched bool) {
	clean := strings.TrimSpace(reqPath)
	if clean == "" || clean == "/" {
		return "", 0, false
	}
	rule, err := sqlx.Get[domain.RedirectRule](
		ctx, s.store,
		`SELECT id, source_path, target_path, status_code FROM redirects WHERE source_path = ?`,
		clean,
	)
	if err == nil && rule.TargetPath != "" && rule.TargetPath != clean {
		code := rule.StatusCode
		if code != http.StatusMovedPermanently && code != http.StatusFound {
			code = http.StatusMovedPermanently
		}
		// Best-effort async hit counter update on WriteDB outside read transaction.
		_, _ = s.store.ExecContext(ctx, `UPDATE redirects SET hit_count = hit_count + 1 WHERE id = ?`, rule.ID)
		return rule.TargetPath, code, true
	}

	// Fallback check against articles.legacy_permalink (e.g., `/articles/2023/05/01/1682900000.html`).
	art, aErr := sqlx.Get[domain.RawArticle](
		ctx, s.store,
		`SELECT slug FROM articles WHERE legacy_permalink = ? AND status = ?`,
		clean, domain.ArticleStatusPublished,
	)
	if aErr == nil && art.Slug != "" {
		dest := visibility.ArticlePath(art.Slug)
		if dest != clean {
			return dest, http.StatusMovedPermanently, true
		}
	}
	return "", 0, false
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
	catsByID, _ := s.loadCategoriesMap(ctx)
	tagsByArticle, _ := s.loadTagsForArticles(ctx, raws)

	out := make([]visibility.PublicArticleCard, 0, len(raws))
	for _, r := range raws {
		var catPtr *domain.Category
		if c, ok := catsByID[r.CategoryID]; ok {
			cCopy := c
			catPtr = &cCopy
		}
		out = append(out, s.guard.ToCard(r, locale, catPtr, tagsByArticle[r.ID], s.LookupMediaDims))
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

	var authorName string
	if u, err := sqlx.Get[domain.User](ctx, s.store, `SELECT display_name, username FROM users WHERE id = ?`, raw.AuthorID); err == nil {
		authorName = u.DisplayName
		if authorName == "" {
			authorName = u.Username
		}
	}
	if authorName == "" {
		authorName = snap.Site.Title
	}

	var catPtr *domain.Category
	if raw.CategoryID != "" {
		if c, err := sqlx.Get[domain.Category](ctx, s.store, `SELECT * FROM categories WHERE id = ?`, raw.CategoryID); err == nil {
			catPtr = &c
		}
	}
	tagsMap, _ := s.loadTagsForArticles(ctx, []domain.RawArticle{raw})
	alts := s.listArticleTranslations(ctx, raw)

	view := s.guard.ToDetail(w, r, raw, viewer, visibility.DetailRenderDeps{
		Locale:            locale,
		AuthorName:        authorName,
		Category:          catPtr,
		Tags:              tagsMap[raw.ID],
		Translations:      alts,
		DefaultOGImageURL: snap.SEO.DefaultOGImageURL,
		SiteTitle:         snap.Site.Title,
		PublicBaseURL:     publicBaseURL,
		LicenseTemplate:   snap.Site.LicenseTemplate,
		ShowLineNumbers:   snap.Reading.ShowCodeLineNumbers,
		LookupBook:        func(bSlug string) (render.BookCardInfo, bool) { return s.lookupBookCard(ctx, bSlug) },
		ListBooks:         func() []render.BookCardInfo { return s.listBookCards(ctx) },
		LookupMediaDims:   s.LookupMediaDims,
	})

	// Surface 10: Only run `render.post_html` plugin hook when the article is public & free of locks!
	if s.plugins != nil && !view.IsPasswordProtected && !view.HasContentLock && view.HTML != "" {
		hookRes := s.plugins.DispatchArticleHook(ctx, plugin.HookRenderPostHTML, plugin.ArticleHookPayload{
			ID:                  raw.ID,
			Title:               raw.Title,
			Slug:                raw.Slug,
			Lang:                raw.Lang,
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

// GetPublicPageBySlug renders a published Standalone Page (`/{slug}`).
func (s *Service) GetPublicPageBySlug(ctx context.Context, slug string, locale string) (PublicPageView, error) {
	p, err := sqlx.Get[domain.Page](
		ctx, s.store,
		`SELECT * FROM pages WHERE slug = ? AND is_published = ?`,
		strings.TrimSpace(slug), true,
	)
	if err != nil {
		return PublicPageView{}, err
	}
	snap := s.settings.Snapshot()
	rendered := s.renderer.Render(p.ContentMD, render.Options{
		Mode:            render.RenderModeWeb,
		Locale:          locale,
		Unlocked:        true,
		ShowLineNumbers: snap.Reading.ShowCodeLineNumbers,
		LookupBook:      func(bSlug string) (render.BookCardInfo, bool) { return s.lookupBookCard(ctx, bSlug) },
		ListBooks:       func() []render.BookCardInfo { return s.listBookCards(ctx) },
		LookupMediaDims: s.LookupMediaDims,
	})

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
	return PublicPageView{
		ID:               p.ID,
		Title:            p.Title,
		Slug:             p.Slug,
		URL:              "/" + p.Slug,
		HTML:             rendered.HTML,
		PlainSummary:     truncateForSummary(rendered.PlainText, 160),
		AllowComment:     p.AllowComment,
		ShowTOC:          p.ShowTOC,
		TOC:              toc,
		Lang:             p.Lang,
		TranslationGroup: p.TranslationGroup,
		Translations:     alts,
		ViewCount:        p.ViewCount,
		CommentCount:     p.CommentCount,
		UpdatedAt:        p.UpdatedAt,
		NeedsMath:        rendered.NeedsMath,
		NeedsMermaid:     rendered.NeedsMermaid,
	}, nil
}

// ListPublicNavItems builds the localized top navigation menu from `nav_items`.
func (s *Service) ListPublicNavItems(ctx context.Context, locale string) ([]PublicNavNode, error) {
	items, err := sqlx.Select[domain.NavItem](
		ctx, s.store,
		`SELECT * FROM nav_items WHERE is_enabled = ? ORDER BY sort_order ASC, created_at ASC`,
		true,
	)
	if err != nil {
		return nil, err
	}
	byParent := make(map[string][]domain.NavItem)
	for _, it := range items {
		byParent[strings.TrimSpace(it.ParentID)] = append(byParent[strings.TrimSpace(it.ParentID)], it)
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
	tags, err := sqlx.Select[domain.Tag](ctx, s.store, `SELECT * FROM tags ORDER BY name ASC`)
	if err != nil {
		return nil, err
	}
	type countRow struct {
		TagID string `db:"tag_id"`
		Cnt   int    `db:"cnt"`
	}
	counts, _ := sqlx.Select[countRow](
		ctx, s.store,
		`SELECT at.tag_id AS tag_id, COUNT(*) AS cnt
		 FROM article_tags at
		 INNER JOIN articles a ON a.id = at.article_id
		 WHERE a.status = ?
		 GROUP BY at.tag_id`,
		domain.ArticleStatusPublished,
	)
	cntMap := make(map[string]int, len(counts))
	for _, c := range counts {
		cntMap[c.TagID] = c.Cnt
	}
	out := make([]TagWithCount, 0, len(tags))
	for _, t := range tags {
		out = append(out, TagWithCount{
			Tag:          t,
			ArticleCount: cntMap[t.ID],
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ArticleCount != out[j].ArticleCount {
			return out[i].ArticleCount > out[j].ArticleCount
		}
		return out[i].Name < out[j].Name
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// ListArchiveMonths computes YYYY-MM archive buckets directly from `articles.published_at`
// (replacing legacy `archivedate` tables and strictly excluding Standalone Pages!).
func (s *Service) ListArchiveMonths(ctx context.Context) ([]ArchiveMonth, error) {
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

// ListFollowsWithItems returns RSS Follow subscriptions and their latest items.
func (s *Service) ListFollowsWithItems(ctx context.Context, itemsPerFollow int) ([]FollowGroup, error) {
	if itemsPerFollow <= 0 {
		itemsPerFollow = 5
	}
	follows, err := sqlx.Select[domain.Follow](ctx, s.store, `SELECT * FROM follows ORDER BY sort_order ASC, created_at ASC`)
	if err != nil {
		return nil, err
	}
	allItems, _ := sqlx.Select[domain.FollowItem](ctx, s.store, `SELECT * FROM follow_items ORDER BY published_at DESC`)
	byFollow := make(map[string][]domain.FollowItem)
	for _, it := range allItems {
		if len(byFollow[it.FollowID]) < itemsPerFollow {
			byFollow[it.FollowID] = append(byFollow[it.FollowID], it)
		}
	}
	out := make([]FollowGroup, 0, len(follows))
	for _, f := range follows {
		out = append(out, FollowGroup{Follow: f, Items: byFollow[f.ID]})
	}
	return out, nil
}

// RecordView increments view_count and zero-PII daily PV/UV + top referer/article counters.
func (s *Service) RecordView(ctx context.Context, targetType string, targetID string, actorFPHash string, refererHost string) {
	today := time.Now().In(s.siteLocation()).Format("2006-01-02")
	now := time.Now().UnixMilli()

	isNewUV := false
	if actorFPHash != "" {
		s.uvMu.Lock()
		if s.uvDate != today {
			s.uvDate = today
			s.uvVisited = make(map[string]struct{})
		}
		if len(s.uvVisited) < 65536 {
			if _, seen := s.uvVisited[actorFPHash]; !seen {
				s.uvVisited[actorFPHash] = struct{}{}
				isNewUV = true
			}
		}
		s.uvMu.Unlock()
	}

	uvDelta := int64(0)
	if isNewUV {
		uvDelta = 1
	}

	_ = s.store.WithTx(ctx, func(tx *sqlx.Tx) error {
		if targetType == domain.TargetTypeArticle && targetID != "" {
			_, _ = tx.ExecContext(ctx, `UPDATE articles SET view_count = view_count + 1 WHERE id = ?`, targetID)
			if err := upsertDailyTopItemTx(ctx, tx, today, "article", targetID); err != nil {
				return err
			}
		} else if targetType == domain.TargetTypePage && targetID != "" {
			_, _ = tx.ExecContext(ctx, `UPDATE pages SET view_count = view_count + 1 WHERE id = ?`, targetID)
		}
		if cleanRef := strings.ToLower(strings.TrimSpace(refererHost)); cleanRef != "" {
			_ = upsertDailyTopItemTx(ctx, tx, today, "referer", cleanRef)
		}

		res, err := tx.ExecContext(
			ctx,
			`UPDATE analytics_daily SET pv = pv + 1, uv = uv + ?, updated_at = ? WHERE stat_date = ?`,
			uvDelta, now, today,
		)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			upsert := tx.Dialect().UpsertSQL(
				"analytics_daily",
				[]string{"stat_date", "pv", "uv", "updated_at"},
				[]string{"stat_date"},
				[]string{"updated_at"},
			)
			_, err = tx.ExecContext(ctx, upsert, today, int64(1), uvDelta, now)
			return err
		}
		return nil
	})
}

func upsertDailyTopItemTx(ctx context.Context, tx *sqlx.Tx, statDate string, kind string, itemKey string) error {
	res, err := tx.ExecContext(
		ctx,
		`UPDATE analytics_top_items SET hits = hits + 1 WHERE stat_date = ? AND kind = ? AND item_key = ?`,
		statDate, kind, itemKey,
	)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return nil
	}
	upsert := tx.Dialect().UpsertSQL(
		"analytics_top_items",
		[]string{"stat_date", "kind", "item_key", "hits"},
		[]string{"stat_date", "kind", "item_key"},
		[]string{},
	)
	_, err = tx.ExecContext(ctx, upsert, statDate, kind, itemKey, int64(1))
	return err
}

func (s *Service) loadCategoriesMap(ctx context.Context) (map[string]domain.Category, error) {
	cats, err := sqlx.Select[domain.Category](ctx, s.store, `SELECT * FROM categories`)
	if err != nil {
		return nil, err
	}
	m := make(map[string]domain.Category, len(cats))
	for _, c := range cats {
		m[c.ID] = c
	}
	return m, nil
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

func (s *Service) siteLocation() *time.Location {
	if s.settings != nil {
		tz := strings.TrimSpace(s.settings.Snapshot().Site.TimeZone)
		if tz != "" {
			if loc, err := time.LoadLocation(tz); err == nil {
				return loc
			}
		}
	}
	return time.FixedZone("CST", 8*3600)
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
	rs := []rune(strings.TrimSpace(s))
	if len(rs) <= maxRunes {
		return string(rs)
	}
	return strings.TrimSpace(string(rs[:maxRunes])) + "…"
}

// Helper ensuring `id` import is actively referenced for entity creation helpers in content package.
var _ = id.Next
