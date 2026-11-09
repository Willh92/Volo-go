package content

import (
	"bytes"
	"compress/gzip"
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/Willh92/Volo-go/internal/db/sqlx"
	"github.com/Willh92/Volo-go/internal/domain"
	"github.com/Willh92/Volo-go/internal/render"
	"github.com/Willh92/Volo-go/internal/visibility"
)

const (
	maxFeedCacheBytes        = 8 << 20 // 8 MiB ceiling so /llms-full.txt up to 2 MiB + gzip can be cached safely
	maxFeedCacheEntries      = 24
	maxArticleHTMLCacheBytes = 2 << 20
	maxCachedRecentCmts      = 20
	maxCachedChromeTags      = 200
)

type redirectEntry struct {
	id         string // Non-empty for `redirects` table rows (records hit_count); empty for legacy_permalink
	targetPath string
	statusCode int
}

type redirectTableSnapshot struct {
	dataRev uint64
	rules   map[string]redirectEntry
}

type publicChromeSnapshot struct {
	dataRev            uint64
	settingsRev        uint64
	rawNavItems        []domain.NavItem
	categories         []CategoryWithCount
	catsByID           map[string]domain.Category
	tags               []TagWithCount
	recentCmts         []RecentCommentItem
	archiveBuckets     []ArchiveMonth
	archiveTZ          string
	archiveStats       ArchiveStats
	owner              PublicAuthorProfile
	authors            []PublicAuthorProfile
	authorsByID        map[string]PublicAuthorProfile
	stats              PublicSiteStats
	recentCards        []visibility.PublicArticleCard
	mostViewedCards    []visibility.PublicArticleCard
	mostCommentedCards []visibility.PublicArticleCard
}

// CachedDocument holds a revision-coherent public XML/Markdown distribution document
// along with its pre-compressed Gzip representation and strong HTTP ETag.
type CachedDocument struct {
	Raw         []byte
	Gzip        []byte
	ETag        string
	ContentType string
}

type feedCacheEntry struct {
	dataRev     uint64
	settingsRev uint64
	seq         uint64
	doc         CachedDocument
	cost        int
}

type tzArchiveCacheEntry struct {
	dataRev uint64
	stats   ArchiveStats
}

type cachedArticleDetail struct {
	key            string
	cost           int
	html           string
	summary        string
	coverURL       string
	ogImageURL     string
	licenseText    string
	toc            []render.TOCItem
	wordCount      int
	readingMinutes int
	needsMath      bool
	needsMermaid   bool
}

type serviceCacheState struct {
	redirects atomic.Pointer[redirectTableSnapshot]

	chromeMu sync.Mutex
	chrome   atomic.Pointer[publicChromeSnapshot]

	archiveTZMu sync.RWMutex
	archiveByTZ map[string]tzArchiveCacheEntry

	feedMu   sync.RWMutex
	feedDocs map[string]feedCacheEntry
	feedUsed int
	feedSeq  uint64

	articleHTMLMu    sync.Mutex
	articleHTMLLL    *list.List
	articleHTMLItems map[string]*list.Element
	articleHTMLUsed  int
}

func newServiceCacheState() *serviceCacheState {
	return &serviceCacheState{
		archiveByTZ:      make(map[string]tzArchiveCacheEntry, 4),
		feedDocs:         make(map[string]feedCacheEntry, 12),
		articleHTMLLL:    list.New(),
		articleHTMLItems: make(map[string]*list.Element, 128),
	}
}

func (c *serviceCacheState) getArticleHTML(key string) (cachedArticleDetail, bool) {
	if c == nil {
		return cachedArticleDetail{}, false
	}
	c.articleHTMLMu.Lock()
	defer c.articleHTMLMu.Unlock()
	if el, ok := c.articleHTMLItems[key]; ok {
		c.articleHTMLLL.MoveToFront(el)
		val := el.Value.(*cachedArticleDetail)
		out := *val
		if len(val.toc) > 0 {
			out.toc = append([]render.TOCItem(nil), val.toc...)
		}
		return out, true
	}
	return cachedArticleDetail{}, false
}

func (c *serviceCacheState) putArticleHTML(entry cachedArticleDetail) {
	if c == nil {
		return
	}
	tocBytes := 0
	for _, t := range entry.toc {
		tocBytes += len(t.ID) + len(t.Text) + 24
	}
	cost := len(entry.key) + len(entry.html) + len(entry.summary) + len(entry.coverURL) + len(entry.ogImageURL) + len(entry.licenseText) + tocBytes + 160
	if cost <= 0 || cost > maxArticleHTMLCacheBytes/4 {
		return
	}
	cp := entry
	cp.cost = cost
	if len(entry.toc) > 0 {
		cp.toc = append([]render.TOCItem(nil), entry.toc...)
	}

	c.articleHTMLMu.Lock()
	defer c.articleHTMLMu.Unlock()
	if existing, ok := c.articleHTMLItems[cp.key]; ok {
		old := existing.Value.(*cachedArticleDetail)
		c.articleHTMLUsed -= old.cost
		existing.Value = &cp
		c.articleHTMLUsed += cp.cost
		c.articleHTMLLL.MoveToFront(existing)
	} else {
		el := c.articleHTMLLL.PushFront(&cp)
		c.articleHTMLItems[cp.key] = el
		c.articleHTMLUsed += cp.cost
	}
	for c.articleHTMLUsed > maxArticleHTMLCacheBytes && c.articleHTMLLL.Len() > 0 {
		back := c.articleHTMLLL.Back()
		if back == nil {
			break
		}
		ev := back.Value.(*cachedArticleDetail)
		delete(c.articleHTMLItems, ev.key)
		c.articleHTMLLL.Remove(back)
		c.articleHTMLUsed -= ev.cost
		if c.articleHTMLUsed < 0 {
			c.articleHTMLUsed = 0
		}
	}
}

func (s *Service) currentRevisions() (dataRev uint64, settingsRev uint64) {
	if s.store != nil {
		dataRev = s.store.DataRevision()
	}
	if s.settings != nil {
		settingsRev = s.settings.Revision()
	}
	return dataRev, settingsRev
}

// PurgeRuntimeCaches clears in-memory redirect, chrome, feed, article HTML, and visibility render caches.
func (s *Service) PurgeRuntimeCaches() {
	if s == nil {
		return
	}
	if s.caches != nil {
		s.caches.redirects.Store(nil)
		s.caches.chrome.Store(nil)
		s.caches.archiveTZMu.Lock()
		clear(s.caches.archiveByTZ)
		s.caches.archiveTZMu.Unlock()
		s.caches.feedMu.Lock()
		clear(s.caches.feedDocs)
		s.caches.feedUsed = 0
		s.caches.feedMu.Unlock()
		s.caches.articleHTMLMu.Lock()
		s.caches.articleHTMLLL.Init()
		clear(s.caches.articleHTMLItems)
		s.caches.articleHTMLUsed = 0
		s.caches.articleHTMLMu.Unlock()
	}
	if s.guard != nil {
		s.guard.PurgeCache()
	}
}

func (s *Service) getRedirectSnapshot(ctx context.Context) (*redirectTableSnapshot, error) {
	curRev := s.store.DataRevision()
	if snap := s.caches.redirects.Load(); snap != nil && snap.dataRev == curRev {
		return snap, nil
	}

	var rulesRows []domain.RedirectRule
	err := sqlx.SelectInto(ctx, s.store, &rulesRows,
		`SELECT id, source_path, target_path, status_code FROM redirects`,
	)
	if err != nil {
		return nil, err
	}
	var legacyArts []domain.RawArticle
	err = sqlx.SelectInto(ctx, s.store, &legacyArts,
		`SELECT slug, legacy_permalink FROM articles WHERE legacy_permalink <> '' AND status = ?`,
		domain.ArticleStatusPublished,
	)
	if err != nil {
		return nil, err
	}

	rules := make(map[string]redirectEntry, len(rulesRows)+len(legacyArts))
	// Populate legacy permalinks first so explicit `redirects` table overrides win on conflict.
	for _, a := range legacyArts {
		src := strings.TrimSpace(a.LegacyPermalink)
		dest := visibility.ArticlePath(a.Slug)
		if src != "" && a.Slug != "" && src != dest {
			rules[src] = redirectEntry{
				targetPath: dest,
				statusCode: http.StatusMovedPermanently,
			}
		}
	}
	for _, r := range rulesRows {
		src := strings.TrimSpace(r.SourcePath)
		dest := strings.TrimSpace(r.TargetPath)
		if src == "" || dest == "" || src == dest {
			continue
		}
		code := r.StatusCode
		if code != http.StatusMovedPermanently && code != http.StatusFound {
			code = http.StatusMovedPermanently
		}
		rules[src] = redirectEntry{
			id:         r.ID,
			targetPath: dest,
			statusCode: code,
		}
	}

	next := &redirectTableSnapshot{
		dataRev: curRev,
		rules:   rules,
	}
	s.caches.redirects.Store(next)
	return next, nil
}

func (s *Service) getChromeSnapshot(ctx context.Context) (*publicChromeSnapshot, error) {
	dRev, sRev := s.currentRevisions()
	if snap := s.caches.chrome.Load(); snap != nil && snap.dataRev == dRev && snap.settingsRev == sRev {
		return snap, nil
	}

	s.caches.chromeMu.Lock()
	defer s.caches.chromeMu.Unlock()

	dRev, sRev = s.currentRevisions()
	if snap := s.caches.chrome.Load(); snap != nil && snap.dataRev == dRev && snap.settingsRev == sRev {
		return snap, nil
	}

	var navRows []domain.NavItem
	err := sqlx.SelectInto(ctx, s.store, &navRows,
		`SELECT * FROM nav_items WHERE is_enabled = ? ORDER BY sort_order ASC, created_at ASC`,
		true,
	)
	if err != nil {
		return nil, err
	}
	cats, err := s.queryCategoriesWithCounts(ctx)
	if err != nil {
		return nil, err
	}
	catsByID := make(map[string]domain.Category, len(cats))
	for _, c := range cats {
		catsByID[c.ID] = c.Category
	}
	tags, err := s.queryTagsWithCountsLimited(ctx, maxCachedChromeTags)
	if err != nil {
		return nil, err
	}
	recentCmts, _ := s.queryRecentPublicComments(ctx, maxCachedRecentCmts)
	siteLoc := s.siteLocation()
	archiveStats, _ := s.queryArchiveStatsForLocation(ctx, siteLoc)
	owner, authors, authorsByID := s.queryPublicAuthors(ctx)
	stats := s.queryPublicSiteStats(ctx, len(cats))

	cardCols := articleCardSelectColumns("")
	recentSQL, recentArgs := s.store.Dialect().PaginateSQL(
		`SELECT `+cardCols+` FROM articles WHERE status = ? ORDER BY published_at DESC`, maxCachedRecentCmts, 0,
	)
	var recentRaw []domain.RawArticle
	_ = sqlx.SelectInto(ctx, s.store, &recentRaw, recentSQL, append([]any{domain.ArticleStatusPublished}, recentArgs...)...)

	viewedSQL, viewedArgs := s.store.Dialect().PaginateSQL(
		`SELECT `+cardCols+` FROM articles WHERE status = ? ORDER BY view_count DESC, published_at DESC`, maxCachedRecentCmts, 0,
	)
	var mostViewedRaw []domain.RawArticle
	_ = sqlx.SelectInto(ctx, s.store, &mostViewedRaw, viewedSQL, append([]any{domain.ArticleStatusPublished}, viewedArgs...)...)

	cmtedSQL, cmtedArgs := s.store.Dialect().PaginateSQL(
		`SELECT `+cardCols+` FROM articles WHERE status = ? ORDER BY comment_count DESC, published_at DESC`, maxCachedRecentCmts, 0,
	)
	var mostCommentedRaw []domain.RawArticle
	_ = sqlx.SelectInto(ctx, s.store, &mostCommentedRaw, cmtedSQL, append([]any{domain.ArticleStatusPublished}, cmtedArgs...)...)

	seenArt := make(map[string]struct{}, len(recentRaw)+len(mostViewedRaw)+len(mostCommentedRaw))
	uniqueRaw := make([]domain.RawArticle, 0, len(recentRaw)+len(mostViewedRaw)+len(mostCommentedRaw))
	for _, list := range [][]domain.RawArticle{recentRaw, mostViewedRaw, mostCommentedRaw} {
		for _, r := range list {
			if _, ok := seenArt[r.ID]; !ok {
				seenArt[r.ID] = struct{}{}
				uniqueRaw = append(uniqueRaw, r)
			}
		}
	}
	tagsByArt, _ := s.loadTagsForArticles(ctx, uniqueRaw)
	defaultLoc := "zh-CN"
	if s.settings != nil {
		if loc := strings.TrimSpace(s.settings.Snapshot().Site.DefaultLocale); loc != "" {
			defaultLoc = loc
		}
	}
	buildSnapshotCards := func(src []domain.RawArticle) []visibility.PublicArticleCard {
		cards := make([]visibility.PublicArticleCard, 0, len(src))
		for _, r := range src {
			var catPtr *domain.Category
			if c, ok := catsByID[r.CategoryID]; ok {
				cCopy := c
				catPtr = &cCopy
			}
			card := s.guard.ToCard(r, defaultLoc, catPtr, tagsByArt[r.ID], s.LookupMediaDims)
			if au, ok := authorsByID[r.AuthorID]; ok {
				card.AuthorName = au.DisplayName
				card.AuthorAvatarURL = au.AvatarURL
			} else if owner.DisplayName != "" {
				card.AuthorName = owner.DisplayName
				card.AuthorAvatarURL = owner.AvatarURL
			}
			cards = append(cards, card)
		}
		return cards
	}

	next := &publicChromeSnapshot{
		dataRev:            dRev,
		settingsRev:        sRev,
		rawNavItems:        navRows,
		categories:         cats,
		catsByID:           catsByID,
		tags:               tags,
		recentCmts:         recentCmts,
		archiveBuckets:     archiveStats.Months,
		archiveTZ:          siteLoc.String(),
		archiveStats:       archiveStats,
		owner:              owner,
		authors:            authors,
		authorsByID:        authorsByID,
		stats:              stats,
		recentCards:        buildSnapshotCards(recentRaw),
		mostViewedCards:    buildSnapshotCards(mostViewedRaw),
		mostCommentedCards: buildSnapshotCards(mostCommentedRaw),
	}
	s.caches.chrome.Store(next)
	return next, nil
}

func (s *Service) getOrBuildCachedDoc(
	ctx context.Context,
	cacheKey string,
	contentType string,
	buildFn func(context.Context) ([]byte, error),
) (CachedDocument, error) {
	dRev, sRev := s.currentRevisions()

	s.caches.feedMu.RLock()
	if hit, ok := s.caches.feedDocs[cacheKey]; ok && hit.dataRev == dRev && hit.settingsRev == sRev {
		s.caches.feedMu.RUnlock()
		return hit.doc, nil
	}
	s.caches.feedMu.RUnlock()

	raw, err := buildFn(ctx)
	if err != nil {
		return CachedDocument{}, err
	}

	sum := sha256.Sum256(raw)
	etag := fmt.Sprintf(`"v%d-%d-%s"`, dRev, sRev, hex.EncodeToString(sum[:8]))

	var gzBytes []byte
	if len(raw) >= 512 && len(raw) <= maxFeedCacheBytes/2 {
		var buf bytes.Buffer
		zw, _ := gzip.NewWriterLevel(&buf, gzip.DefaultCompression)
		_, _ = zw.Write(raw)
		_ = zw.Close()
		if buf.Len() < len(raw) {
			gzBytes = buf.Bytes()
		}
	}

	doc := CachedDocument{
		Raw:         raw,
		Gzip:        gzBytes,
		ETag:        etag,
		ContentType: contentType,
	}

	cost := len(cacheKey) + len(raw) + len(gzBytes) + 64
	if cost <= maxFeedCacheBytes/2 {
		s.caches.feedMu.Lock()
		// Evict stale-revision entries first.
		for k, v := range s.caches.feedDocs {
			if v.dataRev != dRev || v.settingsRev != sRev {
				s.caches.feedUsed -= v.cost
				delete(s.caches.feedDocs, k)
			}
		}
		if prev, exists := s.caches.feedDocs[cacheKey]; exists {
			s.caches.feedUsed -= prev.cost
			delete(s.caches.feedDocs, cacheKey)
		}
		if s.caches.feedUsed < 0 {
			s.caches.feedUsed = 0
		}
		// Single-entry eviction: delete one oldest entry at a time instead of wiping all cached feeds.
		for len(s.caches.feedDocs) >= maxFeedCacheEntries || (len(s.caches.feedDocs) > 0 && s.caches.feedUsed+cost > maxFeedCacheBytes) {
			var evictKey string
			var evictSeq uint64
			first := true
			for k, v := range s.caches.feedDocs {
				if first || v.seq < evictSeq || (v.seq == evictSeq && k < evictKey) {
					evictKey = k
					evictSeq = v.seq
					first = false
				}
			}
			if first {
				break
			}
			s.caches.feedUsed -= s.caches.feedDocs[evictKey].cost
			if s.caches.feedUsed < 0 {
				s.caches.feedUsed = 0
			}
			delete(s.caches.feedDocs, evictKey)
		}
		s.caches.feedSeq++
		s.caches.feedDocs[cacheKey] = feedCacheEntry{
			dataRev:     dRev,
			settingsRev: sRev,
			seq:         s.caches.feedSeq,
			doc:         doc,
			cost:        cost,
		}
		s.caches.feedUsed += cost
		s.caches.feedMu.Unlock()
	}

	return doc, nil
}
