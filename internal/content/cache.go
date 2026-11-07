package content

import (
	"bytes"
	"compress/gzip"
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
	"github.com/Willh92/Volo-go/internal/visibility"
)

const (
	maxFeedCacheBytes   = 2 * 1024 * 1024 // <= 2 MiB ceiling for pre-compressed RSS/Atom/Podcast/Sitemap/LLMs artifacts
	maxCachedRecentCmts = 20
)

var tzLocationCache sync.Map // map[string]*time.Location

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
	dataRev          uint64
	settingsRev      uint64
	rawNavItems      []domain.NavItem
	categories       []CategoryWithCount
	catsByID         map[string]domain.Category
	tags             []TagWithCount
	recentCmts       []RecentCommentItem
	archiveBuckets   []ArchiveMonth
	owner            PublicAuthorProfile
	authors          []PublicAuthorProfile
	authorsByID      map[string]PublicAuthorProfile
	stats            PublicSiteStats
	recentRaw        []domain.RawArticle
	mostViewedRaw    []domain.RawArticle
	mostCommentedRaw []domain.RawArticle
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
	doc         CachedDocument
	cost        int
}

type serviceCacheState struct {
	redirects atomic.Pointer[redirectTableSnapshot]

	chromeMu sync.Mutex
	chrome   atomic.Pointer[publicChromeSnapshot]

	feedMu   sync.RWMutex
	feedDocs map[string]feedCacheEntry
	feedUsed int
}

func newServiceCacheState() *serviceCacheState {
	return &serviceCacheState{
		feedDocs: make(map[string]feedCacheEntry, 12),
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

func (s *Service) getRedirectSnapshot(ctx context.Context) (*redirectTableSnapshot, error) {
	curRev := s.store.DataRevision()
	if snap := s.caches.redirects.Load(); snap != nil && snap.dataRev == curRev {
		return snap, nil
	}

	rulesRows, err := sqlx.Select[domain.RedirectRule](
		ctx, s.store,
		`SELECT id, source_path, target_path, status_code FROM redirects`,
	)
	if err != nil {
		return nil, err
	}
	legacyArts, err := sqlx.Select[domain.RawArticle](
		ctx, s.store,
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

	navRows, err := sqlx.Select[domain.NavItem](
		ctx, s.store,
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
	tags, err := s.queryAllTagsWithCounts(ctx)
	if err != nil {
		return nil, err
	}
	recentCmts, _ := s.queryRecentPublicComments(ctx, maxCachedRecentCmts)
	archiveBuckets, _ := s.queryArchiveMonths(ctx)
	owner, authors, authorsByID := s.queryPublicAuthors(ctx)
	stats := s.queryPublicSiteStats(ctx, len(cats), len(tags))

	recentSQL, recentArgs := s.store.Dialect().PaginateSQL(
		`SELECT * FROM articles WHERE status = ? ORDER BY published_at DESC`, maxCachedRecentCmts, 0,
	)
	recentRaw, _ := sqlx.Select[domain.RawArticle](ctx, s.store, recentSQL, append([]any{domain.ArticleStatusPublished}, recentArgs...)...)

	viewedSQL, viewedArgs := s.store.Dialect().PaginateSQL(
		`SELECT * FROM articles WHERE status = ? ORDER BY view_count DESC, published_at DESC`, maxCachedRecentCmts, 0,
	)
	mostViewedRaw, _ := sqlx.Select[domain.RawArticle](ctx, s.store, viewedSQL, append([]any{domain.ArticleStatusPublished}, viewedArgs...)...)

	cmtedSQL, cmtedArgs := s.store.Dialect().PaginateSQL(
		`SELECT * FROM articles WHERE status = ? ORDER BY comment_count DESC, published_at DESC`, maxCachedRecentCmts, 0,
	)
	mostCommentedRaw, _ := sqlx.Select[domain.RawArticle](ctx, s.store, cmtedSQL, append([]any{domain.ArticleStatusPublished}, cmtedArgs...)...)

	next := &publicChromeSnapshot{
		dataRev:          dRev,
		settingsRev:      sRev,
		rawNavItems:      navRows,
		categories:       cats,
		catsByID:         catsByID,
		tags:             tags,
		recentCmts:       recentCmts,
		archiveBuckets:   archiveBuckets,
		owner:            owner,
		authors:          authors,
		authorsByID:      authorsByID,
		stats:            stats,
		recentRaw:        recentRaw,
		mostViewedRaw:    mostViewedRaw,
		mostCommentedRaw: mostCommentedRaw,
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
		// Evict stale-revision entries first, or reset if budget exceeded.
		for k, v := range s.caches.feedDocs {
			if v.dataRev != dRev || v.settingsRev != sRev {
				s.caches.feedUsed -= v.cost
				delete(s.caches.feedDocs, k)
			}
		}
		if s.caches.feedUsed+cost > maxFeedCacheBytes || len(s.caches.feedDocs) >= 24 {
			clear(s.caches.feedDocs)
			s.caches.feedUsed = 0
		}
		if prev, exists := s.caches.feedDocs[cacheKey]; exists {
			s.caches.feedUsed -= prev.cost
		}
		s.caches.feedDocs[cacheKey] = feedCacheEntry{
			dataRev:     dRev,
			settingsRev: sRev,
			doc:         doc,
			cost:        cost,
		}
		s.caches.feedUsed += cost
		s.caches.feedMu.Unlock()
	}

	return doc, nil
}
