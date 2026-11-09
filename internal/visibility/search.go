package visibility

import (
	"context"
	"net/url"
	"sort"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/Willh92/Volo-go/internal/db/dialect"
	"github.com/Willh92/Volo-go/internal/db/sqlx"
	"github.com/Willh92/Volo-go/internal/domain"
	"github.com/Willh92/Volo-go/internal/render"
	"github.com/Willh92/Volo-go/internal/repo"
)

const (
	DefaultSearchMaxBytes  = 8 * 1024 * 1024 // 8 MiB RAM budget
	maxIndexedExcerptRunes = 2400
	maxOverflowScanRunes   = 16384
	fallbackSQLMatchLimit  = 30
)

// SearchHit represents a zero-leak public search result for Ctrl/Cmd+K instant search and paginated search pages.
type SearchHit struct {
	ID          string `json:"id"`
	Kind        string `json:"kind,omitempty"` // article | page
	Title       string `json:"title"`
	Slug        string `json:"slug"`
	URL         string `json:"url"`
	Snippet     string `json:"snippet"`
	PublishedAt int64  `json:"published_at"`
	Score       int    `json:"score"`
}

type searchDoc struct {
	id          string
	kind        string
	title       string
	slug        string
	url         string
	lang        string
	publicPlain string
	publishedAt int64
	isPinned    bool
	byteCost    int
}

func pagePath(slug string) string {
	return "/" + url.PathEscape(strings.TrimPrefix(strings.TrimSpace(slug), "/"))
}

func docSeenKey(kind, id string) string {
	if kind == "" {
		kind = "article"
	}
	return kind + ":" + id
}

func docComesBefore(a, b searchDoc) bool {
	if a.isPinned != b.isPinned {
		return a.isPinned
	}
	if a.publishedAt != b.publishedAt {
		return a.publishedAt > b.publishedAt
	}
	if a.kind != b.kind {
		return a.kind < b.kind
	}
	return a.id < b.id
}

// SearchIndex implements Surface 5 (Two-Stage Low-Memory Instant Search):
//  1. Excludes all password-protected articles (`password_ciphertext != ”`).
//  2. Strips all `:::lock ... :::` blocks BEFORE indexing or snippet extraction, storing a 2400-rune excerpt.
//  3. Indexes both published articles and published standalone pages (`/{slug}`).
//  4. Caps resident index memory strictly at maxBytes (default 8 MiB); when exceeded, Stage 2 SQL fallback
//     searches unlocked articles (`password_ciphertext = ”` and `has_content_lock = false` for `content_md`)
//     and published standalone pages, verifying tokens after `StripAllLockBlocks`.
type SearchIndex struct {
	mu       sync.RWMutex
	store    *sqlx.Store
	renderer *render.Engine
	settings *repo.SettingsManager
	maxBytes int
	used     int
	complete bool
	docs     []searchDoc
}

// NewSearchIndex constructs a memory-bounded two-stage SearchIndex.
func NewSearchIndex(store *sqlx.Store, renderer *render.Engine, maxBytes int) *SearchIndex {
	if renderer == nil {
		renderer = render.Default()
	}
	if maxBytes <= 0 {
		maxBytes = DefaultSearchMaxBytes
	}
	return &SearchIndex{
		store:    store,
		renderer: renderer,
		maxBytes: maxBytes,
	}
}

// SetSettings attaches SettingsManager so article hit URLs reflect live PostPermalinkStyle changes.
func (idx *SearchIndex) SetSettings(settings *repo.SettingsManager) {
	if idx == nil {
		return
	}
	idx.mu.Lock()
	idx.settings = settings
	idx.mu.Unlock()
}

// Rebuild streams published, non-password-protected articles and published standalone pages into the bounded RAM index.
func (idx *SearchIndex) Rebuild(ctx context.Context) error {
	rows, err := idx.store.QueryContext(
		ctx,
		`SELECT id, title, slug, summary_md, content_md, lang, published_at, is_pinned, password_ciphertext, has_content_lock
		 FROM articles
		 WHERE status = ? AND password_ciphertext = ''
		 ORDER BY is_pinned DESC, published_at DESC`,
		domain.ArticleStatusPublished,
	)
	if err != nil {
		return err
	}
	defer rows.Close()

	var nextDocs []searchDoc
	usedBytes := 0
	allFit := true

	for rows.Next() {
		var r domain.RawArticle
		if sErr := rows.Scan(&r.ID, &r.Title, &r.Slug, &r.SummaryMD, &r.ContentMD, &r.Lang, &r.PublishedAt, &r.IsPinned, &r.PasswordCiphertext, &r.HasContentLock); sErr != nil {
			return sErr
		}
		if strings.TrimSpace(r.PasswordCiphertext) != "" {
			continue
		}
		doc := idx.buildSafeDoc(r)
		if usedBytes+doc.byteCost > idx.maxBytes {
			allFit = false
			break
		}
		usedBytes += doc.byteCost
		nextDocs = append(nextDocs, doc)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	if allFit {
		pageRows, pErr := idx.store.QueryContext(
			ctx,
			`SELECT id, title, slug, content_md, lang, created_at, updated_at
			 FROM pages
			 WHERE is_published = ?
			 ORDER BY updated_at DESC`,
			true,
		)
		if pErr != nil {
			return pErr
		}
		defer pageRows.Close()

		for pageRows.Next() {
			var p domain.Page
			if sErr := pageRows.Scan(&p.ID, &p.Title, &p.Slug, &p.ContentMD, &p.Lang, &p.CreatedAt, &p.UpdatedAt); sErr != nil {
				return sErr
			}
			doc := idx.buildSafePageDoc(p)
			if usedBytes+doc.byteCost > idx.maxBytes {
				allFit = false
				break
			}
			usedBytes += doc.byteCost
			nextDocs = append(nextDocs, doc)
		}
		if err := pageRows.Err(); err != nil {
			return err
		}
	}

	sort.Slice(nextDocs, func(i, j int) bool {
		return docComesBefore(nextDocs[i], nextDocs[j])
	})

	idx.mu.Lock()
	idx.docs = nextDocs
	idx.used = usedBytes
	idx.complete = allFit
	idx.mu.Unlock()
	return nil
}

// Stats returns current resident byte usage, indexed document count, and whether 100% of public posts/pages fit in RAM.
func (idx *SearchIndex) Stats() (usedBytes int, docCount int, complete bool) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	return idx.used, len(idx.docs), idx.complete
}

// Remove deletes an article ID from the in-memory search index if present.
func (idx *SearchIndex) Remove(articleID string) {
	idx.removeByKindAndID("article", articleID)
}

// RemovePage deletes a standalone page ID from the in-memory search index if present.
func (idx *SearchIndex) RemovePage(pageID string) {
	idx.removeByKindAndID("page", pageID)
}

func (idx *SearchIndex) removeByKindAndID(kind, docID string) {
	if strings.TrimSpace(docID) == "" {
		return
	}
	idx.mu.Lock()
	defer idx.mu.Unlock()
	for i, d := range idx.docs {
		dKind := d.kind
		if dKind == "" {
			dKind = "article"
		}
		if dKind == kind && d.id == docID {
			idx.used -= d.byteCost
			if idx.used < 0 {
				idx.used = 0
			}
			idx.docs = append(idx.docs[:i], idx.docs[i+1:]...)
			return
		}
	}
}

// UpsertArticle is an alias for Upsert keeping `idx.docs` sorted by `(is_pinned DESC, published_at DESC)`.
func (idx *SearchIndex) UpsertArticle(r domain.RawArticle) {
	idx.Upsert(r)
}

// Upsert inserts or updates a published, non-password-protected article in the bounded RAM index,
// maintaining `(is_pinned DESC, published_at DESC)` sort order.
func (idx *SearchIndex) Upsert(r domain.RawArticle) {
	if r.Status != domain.ArticleStatusPublished || strings.TrimSpace(r.PasswordCiphertext) != "" {
		idx.Remove(r.ID)
		return
	}
	idx.upsertDoc(idx.buildSafeDoc(r))
}

// UpsertPage inserts or updates a published standalone page in the bounded RAM index.
func (idx *SearchIndex) UpsertPage(p domain.Page) {
	if !p.IsPublished {
		idx.RemovePage(p.ID)
		return
	}
	idx.upsertDoc(idx.buildSafePageDoc(p))
}

func (idx *SearchIndex) upsertDoc(doc searchDoc) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	for i, existing := range idx.docs {
		eKind := existing.kind
		if eKind == "" {
			eKind = "article"
		}
		if eKind == doc.kind && existing.id == doc.id {
			idx.used -= existing.byteCost
			if idx.used < 0 {
				idx.used = 0
			}
			idx.docs = append(idx.docs[:i], idx.docs[i+1:]...)
			break
		}
	}
	if idx.used+doc.byteCost > idx.maxBytes {
		idx.complete = false
		return
	}
	idx.used += doc.byteCost
	insertAt := sort.Search(len(idx.docs), func(i int) bool {
		return docComesBefore(doc, idx.docs[i])
	})
	idx.docs = append(idx.docs, searchDoc{})
	copy(idx.docs[insertAt+1:], idx.docs[insertAt:])
	idx.docs[insertAt] = doc
}

func (idx *SearchIndex) buildSafeDoc(r domain.RawArticle) searchDoc {
	// Strictly strip ALL `:::lock ... :::` (including unclosed locks) BEFORE extracting plain text!
	safeSummary := render.StripAllLockBlocks(r.SummaryMD)
	safeContent := render.StripAllLockBlocks(r.ContentMD)
	combined := strings.TrimSpace(safeSummary + "\n" + safeContent)

	out := idx.renderer.Render(combined, render.Options{
		Mode:     render.RenderModeWeb,
		Unlocked: false,
	})
	excerpt := truncateRunes(out.PlainText, maxIndexedExcerptRunes)
	docURL := ArticlePath(r.Slug)

	cost := len(r.ID) + len(r.Title) + len(r.Slug) + len(docURL) + len(r.Lang) + len(excerpt) + 48
	return searchDoc{
		id:          r.ID,
		kind:        "article",
		title:       r.Title,
		slug:        r.Slug,
		url:         docURL,
		lang:        r.Lang,
		publicPlain: excerpt,
		publishedAt: r.PublishedAt,
		isPinned:    r.IsPinned,
		byteCost:    cost,
	}
}

func (idx *SearchIndex) buildSafePageDoc(p domain.Page) searchDoc {
	safeContent := render.StripAllLockBlocks(p.ContentMD)
	out := idx.renderer.Render(safeContent, render.Options{
		Mode:     render.RenderModeWeb,
		Unlocked: false,
	})
	excerpt := truncateRunes(out.PlainText, maxIndexedExcerptRunes)
	docURL := pagePath(p.Slug)
	pubAt := p.UpdatedAt
	if pubAt <= 0 {
		pubAt = p.CreatedAt
	}

	cost := len(p.ID) + len(p.Title) + len(p.Slug) + len(docURL) + len(p.Lang) + len(excerpt) + 48
	return searchDoc{
		id:          p.ID,
		kind:        "page",
		title:       p.Title,
		slug:        p.Slug,
		url:         docURL,
		lang:        p.Lang,
		publicPlain: excerpt,
		publishedAt: pubAt,
		isPinned:    false,
		byteCost:    cost,
	}
}

func buildLightweightOverflowDoc(kind, id, title, slug, docURL, lang, summaryMD, contentMD string, publishedAt int64, isPinned bool) searchDoc {
	safeSummary := render.StripAllLockBlocks(summaryMD)
	safeContent := render.StripAllLockBlocks(contentMD)
	combined := strings.Join(strings.Fields(safeSummary+" "+safeContent), " ")
	excerpt := truncateRunes(combined, maxOverflowScanRunes)
	return searchDoc{
		id:          id,
		kind:        kind,
		title:       title,
		slug:        slug,
		url:         docURL,
		lang:        lang,
		publicPlain: excerpt,
		publishedAt: publishedAt,
		isPinned:    isPinned,
	}
}

func primaryLangTag(locale string) string {
	s := strings.ToLower(strings.TrimSpace(locale))
	if idx := strings.IndexAny(s, "-_"); idx > 0 {
		return s[:idx]
	}
	return s
}

func (idx *SearchIndex) collectAllHits(ctx context.Context, tokens []string, locale string, sqlLimit int) ([]SearchHit, error) {
	idx.mu.RLock()
	docs := idx.docs
	complete := idx.complete
	idx.mu.RUnlock()

	var seen map[string]bool
	if !complete {
		seen = make(map[string]bool, len(docs))
	}

	type scoredHit struct {
		hit        SearchHit
		localePref bool
	}
	wantPrimary := primaryLangTag(locale)
	scored := make([]scoredHit, 0, 32)

	for _, doc := range docs {
		if seen != nil {
			seen[docSeenKey(doc.kind, doc.id)] = true
		}
		if hit, ok := matchDoc(doc, tokens); ok {
			pref := wantPrimary != "" && primaryLangTag(doc.lang) == wantPrimary
			scored = append(scored, scoredHit{hit: hit, localePref: pref})
		}
	}

	if !complete && idx.store != nil {
		capLimit := sqlLimit
		if capLimit < fallbackSQLMatchLimit {
			capLimit = fallbackSQLMatchLimit
		}
		if capLimit > 500 {
			capLimit = 500
		}

		// Stage 2A: Unlocked published articles (content_md searched ONLY when password_ciphertext = '' AND has_content_lock = false).
		var artSB strings.Builder
		artSB.WriteString(`SELECT id, title, slug, summary_md,
			CASE WHEN has_content_lock = ? THEN content_md ELSE '' END AS content_md,
			lang, published_at, is_pinned
			FROM articles
			WHERE status = ? AND password_ciphertext = ''`)
		artArgs := make([]any, 0, 2+len(tokens)*5)
		artArgs = append(artArgs, false, domain.ArticleStatusPublished)
		for _, tok := range tokens {
			pat := "%" + tok + "%"
			artSB.WriteString(` AND (LOWER(title) LIKE ? OR LOWER(slug) LIKE ? OR LOWER(summary_md) LIKE ? OR (has_content_lock = ? AND LOWER(content_md) LIKE ?))`)
			artArgs = append(artArgs, pat, pat, pat, false, pat)
		}
		artSB.WriteString(` ORDER BY is_pinned DESC, published_at DESC`)
		artSQL, artPageArgs := idx.store.Dialect().PaginateSQL(artSB.String(), capLimit, 0)
		artArgs = append(artArgs, artPageArgs...)

		rows, err := idx.store.QueryContext(ctx, artSQL, artArgs...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var (
				id, title, slug, summaryMD, contentMD, lang string
				publishedAt                                 int64
				isPinned                                    bool
			)
			if sErr := rows.Scan(&id, &title, &slug, &summaryMD, &contentMD, &lang, &publishedAt, &isPinned); sErr != nil {
				_ = rows.Close()
				return nil, sErr
			}
			if seen[docSeenKey("article", id)] {
				continue
			}
			seen[docSeenKey("article", id)] = true
			doc := buildLightweightOverflowDoc("article", id, title, slug, ArticlePath(slug), lang, summaryMD, contentMD, publishedAt, isPinned)
			if hit, ok := matchDoc(doc, tokens); ok {
				pref := wantPrimary != "" && primaryLangTag(lang) == wantPrimary
				scored = append(scored, scoredHit{hit: hit, localePref: pref})
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, err
		}
		_ = rows.Close()

		// Stage 2B: Published standalone pages (verified after StripAllLockBlocks).
		var pgSB strings.Builder
		pgSB.WriteString(`SELECT id, title, slug, content_md, lang, created_at, updated_at
			FROM pages
			WHERE is_published = ?`)
		pgArgs := make([]any, 0, 1+len(tokens)*3)
		pgArgs = append(pgArgs, true)
		for _, tok := range tokens {
			pat := "%" + tok + "%"
			pgSB.WriteString(` AND (LOWER(title) LIKE ? OR LOWER(slug) LIKE ? OR LOWER(content_md) LIKE ?)`)
			pgArgs = append(pgArgs, pat, pat, pat)
		}
		pgSB.WriteString(` ORDER BY updated_at DESC`)
		pgSQL, pgPageArgs := idx.store.Dialect().PaginateSQL(pgSB.String(), capLimit, 0)
		pgArgs = append(pgArgs, pgPageArgs...)

		pgRows, pgErr := idx.store.QueryContext(ctx, pgSQL, pgArgs...)
		if pgErr != nil {
			return nil, pgErr
		}
		for pgRows.Next() {
			var (
				id, title, slug, contentMD, lang string
				createdAt, updatedAt             int64
			)
			if sErr := pgRows.Scan(&id, &title, &slug, &contentMD, &lang, &createdAt, &updatedAt); sErr != nil {
				_ = pgRows.Close()
				return nil, sErr
			}
			if seen[docSeenKey("page", id)] {
				continue
			}
			seen[docSeenKey("page", id)] = true
			pubAt := updatedAt
			if pubAt <= 0 {
				pubAt = createdAt
			}
			doc := buildLightweightOverflowDoc("page", id, title, slug, pagePath(slug), lang, "", contentMD, pubAt, false)
			if hit, ok := matchDoc(doc, tokens); ok {
				pref := wantPrimary != "" && primaryLangTag(lang) == wantPrimary
				scored = append(scored, scoredHit{hit: hit, localePref: pref})
			}
		}
		if err := pgRows.Err(); err != nil {
			_ = pgRows.Close()
			return nil, err
		}
		_ = pgRows.Close()
	}

	sort.Slice(scored, func(i, j int) bool {
		if scored[i].hit.Score != scored[j].hit.Score {
			return scored[i].hit.Score > scored[j].hit.Score
		}
		if scored[i].hit.PublishedAt != scored[j].hit.PublishedAt {
			return scored[i].hit.PublishedAt > scored[j].hit.PublishedAt
		}
		if scored[i].localePref != scored[j].localePref {
			return scored[i].localePref
		}
		if scored[i].hit.Kind != scored[j].hit.Kind {
			return scored[i].hit.Kind < scored[j].hit.Kind
		}
		return scored[i].hit.ID < scored[j].hit.ID
	})

	idx.mu.RLock()
	settingsMgr := idx.settings
	idx.mu.RUnlock()
	var permStyle, siteTZ string
	if settingsMgr != nil {
		snap := settingsMgr.Snapshot()
		permStyle = snap.SEO.PostPermalinkStyle
		siteTZ = snap.Site.TimeZone
	}

	hits := make([]SearchHit, len(scored))
	for i := range scored {
		h := scored[i].hit
		if permStyle != "" && (h.Kind == "" || h.Kind == "article") && h.Slug != "" {
			h.URL = FormatArticlePath(permStyle, h.Slug, h.PublishedAt, siteTZ)
		}
		hits[i] = h
	}
	return hits, nil
}

// Search queries the index and returns up to limit verified public hits.
func (idx *SearchIndex) Search(ctx context.Context, query string, limit int) ([]SearchHit, error) {
	tokens := splitQueryTokens(query)
	if len(tokens) == 0 {
		return []SearchHit{}, nil
	}
	if limit <= 0 {
		limit = 10
	}
	if limit > 50 {
		limit = 50
	}
	hits, err := idx.collectAllHits(ctx, tokens, "", max(fallbackSQLMatchLimit, limit))
	if err != nil {
		return nil, err
	}
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits, nil
}

// SearchPaged queries the two-stage public search index and returns a paginated slice of verified hits,
// total matching count, and whether further pages remain.
func (idx *SearchIndex) SearchPaged(
	ctx context.Context,
	query string,
	locale string,
	_ *domain.User,
	page int,
	perPage int,
) (hits []SearchHit, total int, hasMore bool, err error) {
	limit, offset := dialect.NormalizePage(page, perPage, 15, 50)
	tokens := splitQueryTokens(query)
	if len(tokens) == 0 {
		return []SearchHit{}, 0, false, nil
	}
	allHits, err := idx.collectAllHits(ctx, tokens, locale, offset+limit+fallbackSQLMatchLimit)
	if err != nil {
		return nil, 0, false, err
	}
	total = len(allHits)
	if offset >= total {
		return []SearchHit{}, total, false, nil
	}
	end := offset + limit
	if end > total {
		end = total
	}
	return allHits[offset:end], total, end < total, nil
}

func splitQueryTokens(q string) []string {
	raw := strings.Fields(strings.ToLower(strings.TrimSpace(q)))
	out := make([]string, 0, len(raw))
	for _, t := range raw {
		if len(t) > 0 {
			out = append(out, t)
		}
		if len(out) >= 8 {
			break
		}
	}
	return out
}

func indexFoldLowerNeedle(haystack string, lowerNeedle string) int {
	if lowerNeedle == "" {
		return 0
	}
	if len(haystack) == 0 {
		return -1
	}
	firstRune, firstWidth := utf8.DecodeRuneInString(lowerNeedle)
	isASCII := true
	for i := 0; i < len(lowerNeedle); i++ {
		if lowerNeedle[i] >= utf8.RuneSelf {
			isASCII = false
			break
		}
	}
	if isASCII {
		nLen := len(lowerNeedle)
		if len(haystack) < nLen {
			return -1
		}
		c0 := lowerNeedle[0]
		limit := len(haystack) - nLen
		for i := 0; i <= limit; i++ {
			b := haystack[i]
			if b >= 'A' && b <= 'Z' {
				b += 'a' - 'A'
			}
			if b != c0 {
				continue
			}
			match := true
			for j := 1; j < nLen; j++ {
				hb := haystack[i+j]
				if hb >= 'A' && hb <= 'Z' {
					hb += 'a' - 'A'
				}
				if hb != lowerNeedle[j] {
					match = false
					break
				}
			}
			if match {
				return i
			}
		}
		return -1
	}

	rest := lowerNeedle[firstWidth:]
	for idx, r := range haystack {
		if unicode.ToLower(r) != firstRune {
			continue
		}
		rem := haystack[idx+utf8.RuneLen(r):]
		if hasPrefixFoldLower(rem, rest) {
			return idx
		}
	}
	return -1
}

func hasPrefixFoldLower(s string, lowerPrefix string) bool {
	for len(lowerPrefix) > 0 {
		if len(s) == 0 {
			return false
		}
		sr, sw := utf8.DecodeRuneInString(s)
		pr, pw := utf8.DecodeRuneInString(lowerPrefix)
		if unicode.ToLower(sr) != pr {
			return false
		}
		s = s[sw:]
		lowerPrefix = lowerPrefix[pw:]
	}
	return true
}

func matchDoc(doc searchDoc, tokens []string) (SearchHit, bool) {
	score := 0
	firstMatchByte := -1

	for _, tok := range tokens {
		inTitle := indexFoldLowerNeedle(doc.title, tok) >= 0
		inSlug := indexFoldLowerNeedle(doc.slug, tok) >= 0
		posBody := indexFoldLowerNeedle(doc.publicPlain, tok)
		if !inTitle && !inSlug && posBody < 0 {
			return SearchHit{}, false
		}
		if inTitle {
			score += 10
		}
		if inSlug {
			score += 6
		}
		if posBody >= 0 {
			score += 3
			if firstMatchByte < 0 {
				firstMatchByte = posBody
			}
		}
	}

	snippet := extractSafeSnippetFast(doc.publicPlain, doc.publicPlain, tokens[0], firstMatchByte, 140)
	kind := doc.kind
	if kind == "" {
		kind = "article"
	}
	hitURL := doc.url
	if hitURL == "" {
		if kind == "page" {
			hitURL = pagePath(doc.slug)
		} else {
			hitURL = ArticlePath(doc.slug)
		}
	}
	return SearchHit{
		ID:          doc.id,
		Kind:        kind,
		Title:       doc.title,
		Slug:        doc.slug,
		URL:         hitURL,
		Snippet:     snippet,
		PublishedAt: doc.publishedAt,
		Score:       score,
	}, true
}

// extractSafeSnippetFast slices a UTF-8 rune-safe window around firstMatchByte with zero full-doc
// []rune or strings.ToLower allocations whenever plain and plainLower share identical UTF-8 byte offsets.
func extractSafeSnippetFast(plain string, plainLower string, firstToken string, firstMatchByte int, maxRunes int) string {
	if len(plain) <= maxRunes {
		return plain
	}
	if len(plain) != len(plainLower) {
		return extractSafeSnippetFallback(plain, firstToken, maxRunes)
	}

	if firstMatchByte < 0 || !utf8.RuneStart(plain[firstMatchByte]) {
		end := stepForwardRunes(plain, 0, maxRunes)
		if end >= len(plain) {
			return plain
		}
		return strings.TrimSpace(plain[:end]) + "…"
	}

	start := stepBackwardRunes(plain, firstMatchByte, 36)
	end := stepForwardRunes(plain, start, maxRunes)
	if start == 0 && end >= len(plain) {
		return plain
	}

	var b strings.Builder
	b.Grow((end - start) + 8)
	if start > 0 {
		b.WriteString("…")
	}
	b.WriteString(strings.TrimSpace(plain[start:end]))
	if end < len(plain) {
		b.WriteString("…")
	}
	return b.String()
}

func stepBackwardRunes(s string, idx int, count int) int {
	pos := idx
	for n := 0; n < count && pos > 0; n++ {
		_, width := utf8.DecodeLastRuneInString(s[:pos])
		pos -= width
	}
	if pos < 0 {
		return 0
	}
	return pos
}

func stepForwardRunes(s string, idx int, count int) int {
	pos := idx
	for n := 0; n < count && pos < len(s); n++ {
		_, width := utf8.DecodeRuneInString(s[pos:])
		pos += width
	}
	if pos > len(s) {
		return len(s)
	}
	return pos
}

func extractSafeSnippetFallback(plain string, firstToken string, maxRunes int) string {
	runes := []rune(plain)
	if len(runes) <= maxRunes {
		return plain
	}
	lowerRunes := []rune(strings.ToLower(plain))
	tokRunes := []rune(firstToken)
	matchIdx := indexRuneSlice(lowerRunes, tokRunes)
	if matchIdx < 0 {
		return string(runes[:maxRunes]) + "…"
	}
	start := matchIdx - 36
	if start < 0 {
		start = 0
	}
	end := start + maxRunes
	if end > len(runes) {
		end = len(runes)
	}
	var b strings.Builder
	if start > 0 {
		b.WriteString("…")
	}
	b.WriteString(strings.TrimSpace(string(runes[start:end])))
	if end < len(runes) {
		b.WriteString("…")
	}
	return b.String()
}

func indexRuneSlice(haystack []rune, needle []rune) int {
	if len(needle) == 0 || len(needle) > len(haystack) {
		return -1
	}
	limit := len(haystack) - len(needle)
	for i := 0; i <= limit; i++ {
		match := true
		for j := 0; j < len(needle); j++ {
			if haystack[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}
