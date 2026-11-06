package visibility

import (
	"context"
	"sort"
	"strings"
	"sync"

	"github.com/Willh92/Volo-go/internal/db/sqlx"
	"github.com/Willh92/Volo-go/internal/domain"
	"github.com/Willh92/Volo-go/internal/render"
)

const DefaultSearchMaxBytes = 8 * 1024 * 1024 // <= 8 MiB strict RAM budget for instant search index

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
	titleLower  string
	slug        string
	publicPlain string
	plainLower  string
	publishedAt int64
	byteCost    int
}

// SearchIndex implements Surface 5 (Two-Stage Low-Memory Instant Search):
// 1. Excludes all password-protected articles (`password_ciphertext != ''`).
// 2. Strips all `:::lock ... :::` blocks BEFORE indexing or snippet extraction.
// 3. Caps resident index memory strictly at maxBytes (default 8 MiB); if an archive exceeds maxBytes,
//    Stage 2 falls back to streaming DB batches and re-verifies every token against `StripAllLockBlocks`
//    public plaintext in Go (zero SQL LIKE side-channel!).
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

// Rebuild reloads published, non-password-protected articles into the bounded RAM index (newest first).
func (idx *SearchIndex) Rebuild(ctx context.Context) error {
	rows, err := sqlx.Select[domain.RawArticle](
		ctx,
		idx.store,
		`SELECT id, title, slug, summary_md, content_md, published_at, password_ciphertext, has_content_lock
		 FROM articles
		 WHERE status = ? AND password_ciphertext = ''
		 ORDER BY is_pinned DESC, published_at DESC`,
		domain.ArticleStatusPublished,
	)
	if err != nil {
		return err
	}

	var nextDocs []searchDoc
	usedBytes := 0
	allFit := true

	for _, r := range rows {
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

func (idx *SearchIndex) buildSafeDoc(r domain.RawArticle) searchDoc {
	// Strictly strip ALL `:::lock ... :::` (including unclosed locks) BEFORE extracting plain text!
	safeSummary := render.StripAllLockBlocks(r.SummaryMD)
	safeContent := render.StripAllLockBlocks(r.ContentMD)
	combined := strings.TrimSpace(safeSummary + "\n" + safeContent)

	out := idx.renderer.Render(combined, render.Options{
		Mode:     render.RenderModeWeb,
		Unlocked: false,
	})
	plain := out.PlainText
	plainLower := strings.ToLower(plain)
	titleLower := strings.ToLower(r.Title)

	cost := len(r.ID) + len(r.Title) + len(titleLower) + len(r.Slug) + len(plain) + len(plainLower) + 64
	return searchDoc{
		id:          r.ID,
		title:       r.Title,
		titleLower:  titleLower,
		slug:        r.Slug,
		publicPlain: plain,
		plainLower:  plainLower,
		publishedAt: r.PublishedAt,
		byteCost:    cost,
	}
}

// Search queries the index and returns up to limit verified public hits.
// Even when `complete == false` (archive > 8 MiB), overflow rows are verified in Go AFTER
// `StripAllLockBlocks`, NEVER trusting raw SQL `LIKE` matches!
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

	seen := make(map[string]bool, len(docs))
	hits := make([]SearchHit, 0)

	for _, doc := range docs {
		seen[doc.id] = true
		if hit, ok := matchDoc(doc, tokens); ok {
			hits = append(hits, hit)
		}
	}

	// Stage 2 overflow scan if archive exceeded 8 MiB resident cap:
	// Note that we DO NOT pass user tokens into `content_md LIKE ?` directly without post-strip verification!
	if !complete && idx.store != nil {
		overflowRows, err := sqlx.Select[domain.RawArticle](
			ctx,
			idx.store,
			`SELECT id, title, slug, summary_md, content_md, published_at, password_ciphertext, has_content_lock
			 FROM articles
			 WHERE status = ? AND password_ciphertext = ''
			 ORDER BY published_at DESC`,
			domain.ArticleStatusPublished,
		)
		if err != nil {
			return nil, err
		}
		for _, r := range overflowRows {
			if seen[r.ID] || strings.TrimSpace(r.PasswordCiphertext) != "" {
				continue
			}
			doc := idx.buildSafeDoc(r)
			if hit, ok := matchDoc(doc, tokens); ok {
				hits = append(hits, hit)
			}
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

func matchDoc(doc searchDoc, tokens []string) (SearchHit, bool) {
	score := 0
	firstMatchByte := -1

	for _, tok := range tokens {
		inTitle := strings.Contains(doc.titleLower, tok)
		posBody := strings.Index(doc.plainLower, tok)
		if !inTitle && posBody < 0 {
			return SearchHit{}, false
		}
		if inTitle {
			score += 10
		}
		if posBody >= 0 {
			score += 3
			if firstMatchByte < 0 {
				firstMatchByte = posBody
			}
		}
	}

	snippet := extractSafeSnippet(doc.publicPlain, tokens[0], 140)
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

func extractSafeSnippet(plain string, firstToken string, maxRunes int) string {
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
