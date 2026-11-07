package visibility

import (
	"context"
	"sort"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/Willh92/Volo-go/internal/db/sqlx"
	"github.com/Willh92/Volo-go/internal/domain"
	"github.com/Willh92/Volo-go/internal/render"
)

const (
	DefaultSearchMaxBytes   = 8 * 1024 * 1024 // 8 MiB RAM budget (~14,000+ articles in RAM)
	maxIndexedExcerptRunes  = 600
	fallbackSQLMatchLimit   = 30
)

// SearchHit represents a zero-leak public search result for Ctrl/Cmd+K instant search.
type SearchHit struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Slug        string `json:"slug"`
	URL         string `json:"url"`
	Snippet     string `json:"snippet"`
	PublishedAt int64  `json:"published_at"`
	Score       int    `json:"score"`
}

type searchDoc struct {
	id          string
	title       string
	slug        string
	publicPlain string
	publishedAt int64
	isPinned    bool
	byteCost    int
}

func docComesBefore(a, b searchDoc) bool {
	if a.isPinned != b.isPinned {
		return a.isPinned
	}
	if a.publishedAt != b.publishedAt {
		return a.publishedAt > b.publishedAt
	}
	return a.id < b.id
}

// SearchIndex implements Surface 5 (Two-Stage Low-Memory Instant Search):
// 1. Excludes all password-protected articles (`password_ciphertext != ''`).
// 2. Strips all `:::lock ... :::` blocks BEFORE indexing or snippet extraction, storing a compact 600-rune excerpt.
// 3. Caps resident index memory strictly at maxBytes (default 8 MiB, holding 14,000+ articles);
//    if an archive exceeds maxBytes (50,000+ posts), Stage 2 runs a lightweight metadata SQL fallback
//    (`title / slug / summary_md`, LIMIT 30) verified after `StripAllLockBlocks` without invoking full Markdown renders.
type SearchIndex struct {
	mu       sync.RWMutex
	store    *sqlx.Store
	renderer *render.Engine
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

// Rebuild streams published, non-password-protected articles into the bounded RAM index (`is_pinned DESC, published_at DESC`).
func (idx *SearchIndex) Rebuild(ctx context.Context) error {
	rows, err := idx.store.QueryContext(
		ctx,
		`SELECT id, title, slug, summary_md, content_md, published_at, is_pinned, password_ciphertext, has_content_lock
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
		if sErr := rows.Scan(&r.ID, &r.Title, &r.Slug, &r.SummaryMD, &r.ContentMD, &r.PublishedAt, &r.IsPinned, &r.PasswordCiphertext, &r.HasContentLock); sErr != nil {
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

	idx.mu.Lock()
	idx.docs = nextDocs
	idx.used = usedBytes
	idx.complete = allFit
	idx.mu.Unlock()
	return nil
}

// Stats returns current resident byte usage, indexed document count, and whether 100% of public posts fit in RAM.
func (idx *SearchIndex) Stats() (usedBytes int, docCount int, complete bool) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	return idx.used, len(idx.docs), idx.complete
}

// Remove deletes an article ID from the in-memory search index if present.
func (idx *SearchIndex) Remove(articleID string) {
	if strings.TrimSpace(articleID) == "" {
		return
	}
	idx.mu.Lock()
	defer idx.mu.Unlock()
	for i, d := range idx.docs {
		if d.id == articleID {
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
	doc := idx.buildSafeDoc(r)
	idx.mu.Lock()
	defer idx.mu.Unlock()
	for i, existing := range idx.docs {
		if existing.id == r.ID {
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

	cost := len(r.ID) + len(r.Title) + len(r.Slug) + len(excerpt) + 32
	return searchDoc{
		id:          r.ID,
		title:       r.Title,
		slug:        r.Slug,
		publicPlain: excerpt,
		publishedAt: r.PublishedAt,
		isPinned:    r.IsPinned,
		byteCost:    cost,
	}
}

func buildLightweightOverflowDoc(id, title, slug, summaryMD string, publishedAt int64, isPinned bool) searchDoc {
	safeSummary := strings.Join(strings.Fields(render.StripAllLockBlocks(summaryMD)), " ")
	excerpt := truncateRunes(safeSummary, maxIndexedExcerptRunes)
	return searchDoc{
		id:          id,
		title:       title,
		slug:        slug,
		publicPlain: excerpt,
		publishedAt: publishedAt,
		isPinned:    isPinned,
	}
}

// Search queries the index and returns up to limit verified public hits.
// When `complete == false` (archive > 8 MiB / 50,000+ posts), fallback SQL matches lightweight
// `title / slug / summary_md` (`LIMIT 30`) and verifies every token after `StripAllLockBlocks`
// with ZERO `renderer.Render` calls!
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

	idx.mu.RLock()
	docs := idx.docs
	complete := idx.complete
	idx.mu.RUnlock()

	var seen map[string]bool
	if !complete {
		seen = make(map[string]bool, len(docs))
	}
	hits := make([]SearchHit, 0, min(limit*2, 32))

	for _, doc := range docs {
		if seen != nil {
			seen[doc.id] = true
		}
		if hit, ok := matchDoc(doc, tokens); ok {
			hits = append(hits, hit)
		}
	}

	if !complete && idx.store != nil {
		var sb strings.Builder
		sb.WriteString(`SELECT id, title, slug, summary_md, published_at, is_pinned
			FROM articles
			WHERE status = ? AND password_ciphertext = ''`)
		args := make([]any, 0, 1+len(tokens)*3)
		args = append(args, domain.ArticleStatusPublished)
		for _, tok := range tokens {
			pat := "%" + tok + "%"
			sb.WriteString(` AND (LOWER(title) LIKE ? OR LOWER(slug) LIKE ? OR LOWER(summary_md) LIKE ?)`)
			args = append(args, pat, pat, pat)
		}
		sb.WriteString(` ORDER BY is_pinned DESC, published_at DESC`)
		qSQL, pageArgs := idx.store.Dialect().PaginateSQL(sb.String(), fallbackSQLMatchLimit, 0)
		args = append(args, pageArgs...)

		rows, err := idx.store.QueryContext(ctx, qSQL, args...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()

		for rows.Next() {
			var (
				id, title, slug, summaryMD string
				publishedAt                int64
				isPinned                   bool
			)
			if sErr := rows.Scan(&id, &title, &slug, &summaryMD, &publishedAt, &isPinned); sErr != nil {
				return nil, sErr
			}
			if seen[id] {
				continue
			}
			doc := buildLightweightOverflowDoc(id, title, slug, summaryMD, publishedAt, isPinned)
			if hit, ok := matchDoc(doc, tokens); ok {
				hits = append(hits, hit)
			}
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}

	sort.Slice(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		return hits[i].PublishedAt > hits[j].PublishedAt
	})
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits, nil
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
	return SearchHit{
		ID:          doc.id,
		Title:       doc.title,
		Slug:        doc.slug,
		URL:         ArticlePath(doc.slug),
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
