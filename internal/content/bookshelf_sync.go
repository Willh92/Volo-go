package content

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"
	"unicode"

	nethtml "golang.org/x/net/html"

	"github.com/Willh92/Volo-go/internal/db/sqlx"
	"github.com/Willh92/Volo-go/internal/domain"
	"github.com/Willh92/Volo-go/internal/render"
	"github.com/Willh92/Volo-go/internal/visibility"
	"github.com/Willh92/Volo-go/pkg/id"
)

// BookChapterNode represents a node in the hierarchical chapter tree of a Book.
type BookChapterNode struct {
	ID        string            `json:"id"`
	Title     string            `json:"title"`
	URL       string            `json:"url"`
	Depth     int               `json:"depth"`
	SortOrder int               `json:"sort_order"`
	Children  []BookChapterNode `json:"children,omitempty"`
}

// BookDetailView pairs a Book with its nested chapter tree and flat ordered chapter list (for Prev/Next reader navigation).
type BookDetailView struct {
	domain.Book
	Tree         []BookChapterNode    `json:"tree"`
	FlatChapters []domain.BookChapter `json:"flat_chapters"`
}

// ListBooks returns all books ordered by sort_order ASC, created_at DESC with live chapter counts.
func (s *Service) ListBooks(ctx context.Context) ([]domain.Book, error) {
	books, err := sqlx.Select[domain.Book](ctx, s.store, `SELECT * FROM books ORDER BY sort_order ASC, created_at DESC`)
	if err != nil || len(books) == 0 {
		return books, err
	}
	type cntRow struct {
		BookID string `db:"book_id"`
		Cnt    int    `db:"cnt"`
	}
	counts, cErr := sqlx.Select[cntRow](ctx, s.store, `SELECT book_id, COUNT(*) AS cnt FROM book_chapters GROUP BY book_id`)
	if cErr == nil {
		cntMap := make(map[string]int, len(counts))
		for _, c := range counts {
			cntMap[c.BookID] = c.Cnt
		}
		for i := range books {
			if cnt, ok := cntMap[books[i].ID]; ok {
				if cnt > 0 || books[i].ChapterCount <= 0 {
					books[i].ChapterCount = cnt
				}
			} else if books[i].SourceType != "external" {
				books[i].ChapterCount = 0
			}
		}
	}
	return books, nil
}

// GetBookBySlug loads a Book and reconstructs its hierarchical chapter tree + flat reader sequence.
func (s *Service) GetBookBySlug(ctx context.Context, slug string) (BookDetailView, error) {
	bk, err := sqlx.Get[domain.Book](ctx, s.store, `SELECT * FROM books WHERE slug = ?`, strings.TrimSpace(slug))
	if err != nil {
		return BookDetailView{}, err
	}
	chapters, err := sqlx.Select[domain.BookChapter](
		ctx, s.store,
		`SELECT * FROM book_chapters WHERE book_id = ? ORDER BY sort_order ASC`,
		bk.ID,
	)
	if err != nil {
		return BookDetailView{}, err
	}
	if len(chapters) > 0 || bk.SourceType != "external" {
		bk.ChapterCount = len(chapters)
	}
	return BookDetailView{
		Book:         bk,
		Tree:         buildChapterTree(chapters),
		FlatChapters: chapters,
	}, nil
}

func buildChapterTree(flat []domain.BookChapter) []BookChapterNode {
	if len(flat) == 0 {
		return nil
	}
	byParent := make(map[string][]domain.BookChapter)
	for _, ch := range flat {
		byParent[strings.TrimSpace(ch.ParentID)] = append(byParent[strings.TrimSpace(ch.ParentID)], ch)
	}
	var walk func(parentID string) []BookChapterNode
	walk = func(parentID string) []BookChapterNode {
		kids := byParent[parentID]
		if len(kids) == 0 {
			return nil
		}
		out := make([]BookChapterNode, 0, len(kids))
		for _, k := range kids {
			out = append(out, BookChapterNode{
				ID:        k.ID,
				Title:     k.Title,
				URL:       k.URL,
				Depth:     k.Depth,
				SortOrder: k.SortOrder,
				Children:  walk(k.ID),
			})
		}
		return out
	}
	return walk("")
}

func (s *Service) lookupBookCard(ctx context.Context, slug string) (render.BookCardInfo, bool) {
	bk, err := sqlx.Get[domain.Book](ctx, s.store, `SELECT * FROM books WHERE slug = ? OR id = ?`, slug, slug)
	if err != nil {
		return render.BookCardInfo{}, false
	}
	var liveCnt int
	if cErr := s.store.QueryRowContext(ctx, `SELECT COUNT(*) FROM book_chapters WHERE book_id = ?`, bk.ID).Scan(&liveCnt); cErr == nil {
		if liveCnt > 0 || bk.ChapterCount <= 0 {
			bk.ChapterCount = liveCnt
		}
	}
	return render.BookCardInfo{
		Slug:         bk.Slug,
		Title:        bk.Title,
		Description:  bk.Description,
		CoverURL:     bk.CoverURL,
		SourceURL:    bk.SourceURL,
		ChapterCount: bk.ChapterCount,
	}, true
}

func (s *Service) listBookCards(ctx context.Context) []render.BookCardInfo {
	bks, err := s.ListBooks(ctx)
	if err != nil {
		return nil
	}
	out := make([]render.BookCardInfo, 0, len(bks))
	for _, bk := range bks {
		out = append(out, render.BookCardInfo{
			Slug:         bk.Slug,
			Title:        bk.Title,
			Description:  bk.Description,
			CoverURL:     bk.CoverURL,
			SourceURL:    bk.SourceURL,
			ChapterCount: bk.ChapterCount,
		})
	}
	return out
}

// SyncBookTOC synchronizes a Book's chapter tree from its configured source:
// - `series`: populates chapters from local published, non-protected articles in `series_articles`
// - `gitbook` / `mdbook` / `vuepress` / `auto`: crawls remote SUMMARY.md or sidebar HTML via SSRF-safe OutboundSafeDialer
func (s *Service) SyncBookTOC(ctx context.Context, bookID string) (int, error) {
	bk, err := sqlx.Get[domain.Book](ctx, s.store, `SELECT * FROM books WHERE id = ?`, strings.TrimSpace(bookID))
	if err != nil {
		return 0, err
	}

	var chapters []domain.BookChapter
	switch strings.ToLower(strings.TrimSpace(bk.SourceType)) {
	case "manual":
		if err := s.store.QueryRowContext(ctx, `SELECT COUNT(*) FROM book_chapters WHERE book_id = ?`, bk.ID).Scan(&bk.ChapterCount); err != nil {
			return 0, err
		}
		return bk.ChapterCount, nil

	case "series":
		if strings.TrimSpace(bk.SeriesID) == "" {
			return 0, errors.New("bookshelf: series_id is required when source_type='series'")
		}
		arts, sErr := sqlx.Select[domain.RawArticle](
			ctx, s.store,
			`SELECT a.id, a.title, a.slug, a.password_ciphertext
			 FROM series_articles sa
			 INNER JOIN articles a ON a.id = sa.article_id
			 WHERE sa.series_id = ? AND a.status = ? AND a.password_ciphertext = ''
			 ORDER BY sa.sort_order ASC, a.published_at ASC`,
			bk.SeriesID, domain.ArticleStatusPublished,
		)
		if sErr != nil {
			return 0, sErr
		}
		for idx, a := range arts {
			chapters = append(chapters, domain.BookChapter{
				ID:        id.NextPrefixed("ch"),
				BookID:    bk.ID,
				ParentID:  "",
				Title:     a.Title,
				URL:       visibility.ArticlePath(a.Slug),
				Depth:     0,
				SortOrder: idx + 1,
			})
		}

	default:
		crawled, cErr := s.crawlRemoteBookTOC(ctx, bk)
		if cErr != nil {
			return 0, cErr
		}
		chapters = crawled
	}

	now := time.Now().UnixMilli()
	err = s.store.WithTx(ctx, func(tx *sqlx.Tx) error {
		if _, dErr := tx.ExecContext(ctx, `DELETE FROM book_chapters WHERE book_id = ?`, bk.ID); dErr != nil {
			return dErr
		}
		for _, ch := range chapters {
			if _, iErr := tx.ExecContext(
				ctx,
				`INSERT INTO book_chapters (id, book_id, parent_id, title, url, depth, sort_order) VALUES (?, ?, ?, ?, ?, ?, ?)`,
				ch.ID, bk.ID, ch.ParentID, ch.Title, ch.URL, ch.Depth, ch.SortOrder,
			); iErr != nil {
				return iErr
			}
		}
		_, uErr := tx.ExecContext(
			ctx,
			`UPDATE books SET chapter_count = ?, synced_at = ?, updated_at = ? WHERE id = ?`,
			len(chapters), now, now, bk.ID,
		)
		return uErr
	})
	if err != nil {
		return 0, err
	}
	s.invalidateBookChapterCache(bk.ID)
	return len(chapters), nil
}

func (s *Service) crawlRemoteBookTOC(ctx context.Context, bk domain.Book) ([]domain.BookChapter, error) {
	baseURLStr := strings.TrimSpace(bk.SourceURL)
	if baseURLStr == "" {
		return nil, errors.New("bookshelf: source_url is required for remote TOC sync")
	}
	baseURL, err := s.dialer.ValidateOutboundURL(baseURLStr)
	if err != nil {
		return nil, err
	}
	client := s.dialer.NewHTTPClient(10 * time.Second)

	// 1. If URL ends with .md or if sibling SUMMARY.md exists (GitBook / mdBook convention), parse Markdown list!
	if strings.HasSuffix(strings.ToLower(baseURL.Path), ".md") {
		body, finalBase, fErr := fetchBoundedText(ctx, client, baseURL.String())
		if fErr != nil {
			return nil, fErr
		}
		return parseSummaryMarkdownTOC(bk.ID, body, finalBase), nil
	}

	summaryCandidate := resolveRelativeURL(baseURL, "SUMMARY.md")
	if mdBody, finalBase, sErr := fetchBoundedText(ctx, client, summaryCandidate); sErr == nil && strings.Contains(mdBody, "](") {
		parsed := parseSummaryMarkdownTOC(bk.ID, mdBody, finalBase)
		if len(parsed) > 0 {
			return parsed, nil
		}
	}

	// 2. Fallback to parsing HTML <nav> / sidebar anchor list (mdBook index.html / GitBook / VuePress / VitePress).
	htmlBody, finalBase, err := fetchBoundedText(ctx, client, baseURL.String())
	if err != nil {
		return nil, err
	}
	parsed := parseHTMLSidebarTOC(bk.ID, htmlBody, finalBase)
	if len(parsed) == 0 {
		return nil, errors.New("bookshelf: no chapter links discovered at source_url")
	}
	return parsed, nil
}

func fetchBoundedText(ctx context.Context, client *http.Client, targetURL string) (string, *url.URL, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
	if err != nil {
		return "", nil, err
	}
	req.Header.Set("User-Agent", "Volo-go-BookshelfSync/1.0")
	resp, err := client.Do(req)
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", nil, fmt.Errorf("bookshelf: HTTP %d from %s", resp.StatusCode, targetURL)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return "", nil, err
	}
	return string(raw), resp.Request.URL, nil
}

var mdSummaryBulletRe = regexp.MustCompile(`^(\s*)[-*+]\s+\[([^\]]+)\]\(([^)]+)\)`)

func parseSummaryMarkdownTOC(bookID string, md string, baseURL *url.URL) []domain.BookChapter {
	lines := strings.Split(strings.ReplaceAll(md, "\r\n", "\n"), "\n")
	var out []domain.BookChapter
	stackByDepth := make(map[int]string) // depth -> chapterID

	for _, line := range lines {
		m := mdSummaryBulletRe.FindStringSubmatch(line)
		if len(m) < 4 {
			continue
		}
		indent := len(strings.ReplaceAll(m[1], "\t", "  "))
		depth := indent / 2
		if depth > 4 {
			depth = 4
		}
		title := strings.TrimSpace(m[2])
		rawHref := strings.TrimSpace(m[3])
		if title == "" || rawHref == "" || isDangerousHrefScheme(rawHref) {
			continue
		}
		// Convert Markdown `.md` chapter link to `.html` when hosted on mdBook/GitBook web root.
		webHref := rawHref
		if strings.HasSuffix(strings.ToLower(webHref), "/readme.md") {
			webHref = webHref[:len(webHref)-len("readme.md")]
		} else if strings.EqualFold(webHref, "readme.md") {
			webHref = "./"
		} else if strings.HasSuffix(strings.ToLower(webHref), ".md") {
			webHref = webHref[:len(webHref)-3] + ".html"
		}

		resolved := resolveRelativeURL(baseURL, webHref)
		if !isSafeSameHostChapterURL(resolved, baseURL) {
			continue
		}

		parentID := ""
		if depth > 0 {
			for d := depth - 1; d >= 0; d-- {
				if pid, ok := stackByDepth[d]; ok {
					parentID = pid
					break
				}
			}
		}
		chID := id.NextPrefixed("ch")
		stackByDepth[depth] = chID
		out = append(out, domain.BookChapter{
			ID:        chID,
			BookID:    bookID,
			ParentID:  parentID,
			Title:     title,
			URL:       resolved,
			Depth:     depth,
			SortOrder: len(out) + 1,
		})
	}
	return out
}

var (
	navSectionRe = regexp.MustCompile(`(?is)<(?:nav|aside)\b[^>]*>(.*?)</(?:nav|aside)>`)
	htmlAnchorRe = regexp.MustCompile(`(?is)<a\b[^>]*\bhref=["']([^"'#]+)["'][^>]*>(.*?)</a>`)
	innerTagRe   = regexp.MustCompile(`<[^>]+>`)
)

func parseHTMLSidebarTOC(bookID string, htmlDoc string, baseURL *url.URL) []domain.BookChapter {
	searchScope := htmlDoc
	if navMatches := navSectionRe.FindAllStringSubmatch(htmlDoc, -1); len(navMatches) > 0 {
		var joined strings.Builder
		for _, nm := range navMatches {
			joined.WriteString(nm[1])
			joined.WriteByte('\n')
		}
		searchScope = joined.String()
	}

	matches := htmlAnchorRe.FindAllStringSubmatch(searchScope, 500)
	seen := make(map[string]bool)
	var out []domain.BookChapter

	for _, m := range matches {
		if len(m) < 3 {
			continue
		}
		href := strings.TrimSpace(html.UnescapeString(m[1]))
		title := strings.TrimSpace(html.UnescapeString(innerTagRe.ReplaceAllString(m[2], "")))
		if title == "" || href == "" || isDangerousHrefScheme(href) {
			continue
		}
		resolved := resolveRelativeURL(baseURL, href)
		if !isSafeSameHostChapterURL(resolved, baseURL) || seen[resolved] {
			continue
		}
		seen[resolved] = true
		out = append(out, domain.BookChapter{
			ID:        id.NextPrefixed("ch"),
			BookID:    bookID,
			ParentID:  "",
			Title:     title,
			URL:       resolved,
			Depth:     0,
			SortOrder: len(out) + 1,
		})
		if len(out) >= 300 {
			break
		}
	}
	return out
}

func resolveRelativeURL(base *url.URL, ref string) string {
	if base == nil {
		return ""
	}
	u, err := url.Parse(ref)
	if err != nil {
		return ""
	}
	return base.ResolveReference(u).String()
}

func isDangerousHrefScheme(raw string) bool {
	s := strings.ToLower(strings.TrimSpace(raw))
	for i := 0; i < len(s); i++ {
		if s[i] <= 0x1F || s[i] == 0x7F {
			return true
		}
	}
	return strings.HasPrefix(s, "javascript:") ||
		strings.HasPrefix(s, "data:") ||
		strings.HasPrefix(s, "vbscript:") ||
		strings.HasPrefix(s, "mailto:") ||
		strings.HasPrefix(s, "file:") ||
		strings.HasPrefix(s, `/\`)
}

func isSafeSameHostChapterURL(raw string, baseURL *url.URL) bool {
	s := strings.TrimSpace(raw)
	if s == "" || baseURL == nil || baseURL.Host == "" || isDangerousHrefScheme(s) {
		return false
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || u.User != nil {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	if !strings.EqualFold(u.Host, baseURL.Host) {
		return false
	}
	if strings.EqualFold(baseURL.Scheme, "https") && u.Scheme != "https" {
		return false
	}
	return true
}

const (
	maxBookChapterCacheEntries   = 24
	maxBookChapterCacheItemBytes = 256 << 10 // 256 KiB per chapter
	bookChapterCacheTTL          = 15 * time.Minute
)

type cachedBookChapter struct {
	bookID       string
	html         string
	needsMath    bool
	needsMermaid bool
	expiresAt    time.Time
}

func (s *Service) invalidateBookChapterCache(bookID string) {
	s.bookChapterMu.Lock()
	defer s.bookChapterMu.Unlock()
	for k, v := range s.bookChapterCache {
		if v.bookID == bookID {
			delete(s.bookChapterCache, k)
		}
	}
}

// ExtractRemoteBookChapter fetches a remote book chapter via SSRF-safe OutboundSafeDialer,
// strips redundant built-in TOC/sidebar/chrome (GitBook, HonKit, mdBook, VitePress, VuePress,
// Sphinx, MkDocs, Docusaurus), resolves relative images/media, and rewrites inter-chapter links
// to `/books/{slug}?ch={id}#frag` so all internal links and anchors work seamlessly inside Volo.
func (s *Service) ExtractRemoteBookChapter(
	ctx context.Context,
	bk BookDetailView,
	activeCh domain.BookChapter,
) (htmlOut string, needsMath bool, needsMermaid bool, ok bool) {
	rawURL := strings.TrimSpace(activeCh.URL)
	if rawURL == "" || s.dialer == nil || s.renderer == nil {
		return "", false, false, false
	}
	cacheKey := bk.ID + "|" + activeCh.ID + "|" + rawURL
	now := time.Now()

	s.bookChapterMu.Lock()
	if entry, hit := s.bookChapterCache[cacheKey]; hit {
		if now.Before(entry.expiresAt) {
			s.bookChapterMu.Unlock()
			return entry.html, entry.needsMath, entry.needsMermaid, entry.html != ""
		}
		delete(s.bookChapterCache, cacheKey)
	}
	s.bookChapterMu.Unlock()

	validatedURL, err := s.dialer.ValidateOutboundURL(rawURL)
	if err != nil {
		return "", false, false, false
	}

	fetchCtx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	client := s.dialer.NewHTTPClient(6 * time.Second)
	rawBody, finalURL, err := fetchBoundedText(fetchCtx, client, validatedURL.String())
	if err != nil || strings.TrimSpace(rawBody) == "" {
		return "", false, false, false
	}
	if finalURL == nil {
		finalURL = validatedURL
	}

	cleanedHTML, ok := extractAndRewriteBookHTML(rawBody, finalURL, bk, activeCh, s.renderer)
	if !ok || cleanedHTML == "" {
		return "", false, false, false
	}
	hasMath := strings.Contains(cleanedHTML, "math-inline") || strings.Contains(cleanedHTML, "math-display") || strings.Contains(cleanedHTML, "katex")
	hasMermaid := strings.Contains(cleanedHTML, `class="mermaid"`) || strings.Contains(cleanedHTML, `language-mermaid`)

	if len(cleanedHTML) <= maxBookChapterCacheItemBytes {
		s.bookChapterMu.Lock()
		if s.bookChapterCache == nil {
			s.bookChapterCache = make(map[string]cachedBookChapter, maxBookChapterCacheEntries)
		}
		for k, v := range s.bookChapterCache {
			if now.After(v.expiresAt) {
				delete(s.bookChapterCache, k)
			}
		}
		if len(s.bookChapterCache) >= maxBookChapterCacheEntries {
			var oldestKey string
			var oldestExp time.Time
			for k, v := range s.bookChapterCache {
				if oldestKey == "" || v.expiresAt.Before(oldestExp) {
					oldestKey = k
					oldestExp = v.expiresAt
				}
			}
			if oldestKey != "" {
				delete(s.bookChapterCache, oldestKey)
			}
		}
		s.bookChapterCache[cacheKey] = cachedBookChapter{
			bookID:       bk.ID,
			html:         cleanedHTML,
			needsMath:    hasMath,
			needsMermaid: hasMermaid,
			expiresAt:    now.Add(bookChapterCacheTTL),
		}
		s.bookChapterMu.Unlock()
	}

	return cleanedHTML, hasMath, hasMermaid, true
}

func extractAndRewriteBookHTML(
	rawDoc string,
	finalURL *url.URL,
	bk BookDetailView,
	activeCh domain.BookChapter,
	renderer *render.Engine,
) (string, bool) {
	// Support raw Markdown chapter sources (.md)
	if finalURL != nil && strings.HasSuffix(strings.ToLower(finalURL.Path), ".md") && !strings.Contains(strings.ToLower(rawDoc[:min(len(rawDoc), 256)]), "<html") {
		out := renderer.Render(rawDoc, render.Options{Mode: render.RenderModeWeb})
		rawDoc = "<html><body><article class=\"markdown-section\">" + out.HTML + "</article></body></html>"
	}

	doc, err := nethtml.Parse(strings.NewReader(rawDoc))
	if err != nil {
		return "", false
	}

	root := selectBookProseContainer(doc)
	if root == nil {
		return "", false
	}

	pruneBookChromeNodes(root)
	stripDuplicateLeadingH1(root, activeCh.Title)

	if countVisibleRunes(root, 32) < 20 && !hasBookMediaElement(root) {
		return "", false
	}

	chapterMap := buildBookChapterLookup(bk)
	activeKeys := make(map[string]bool, 4)
	if finalURL != nil {
		for _, k := range bookURLKeys(finalURL) {
			activeKeys[k] = true
		}
	}
	if u, pErr := url.Parse(strings.TrimSpace(activeCh.URL)); pErr == nil {
		for _, k := range bookURLKeys(u) {
			activeKeys[k] = true
		}
	}

	rewriteBookSubtreeURLs(root, finalURL, bk.Slug, activeCh.ID, activeKeys, chapterMap)

	var buf bytes.Buffer
	for c := root.FirstChild; c != nil; c = c.NextSibling {
		_ = nethtml.Render(&buf, c)
	}
	sanitized := strings.TrimSpace(renderer.SanitizeHTML(buf.String()))
	if sanitized == "" {
		return "", false
	}
	return sanitized, true
}

func selectBookProseContainer(doc *nethtml.Node) *nethtml.Node {
	var tier1, tier2, tier3, tier4, bodyNode *nethtml.Node
	var walk func(n *nethtml.Node)
	walk = func(n *nethtml.Node) {
		if n == nil || tier1 != nil {
			return
		}
		if n.Type == nethtml.ElementNode {
			tag := strings.ToLower(n.Data)
			idVal := strings.ToLower(getNodeAttr(n, "id"))
			cls := getNodeAttr(n, "class")
			role := strings.ToLower(getNodeAttr(n, "role"))

			if tag == "body" && bodyNode == nil {
				bodyNode = n
			}
			// Do not descend into obvious navigation/sidebar containers when hunting for main prose
			if isBookChromeContainer(tag, idVal, cls) {
				return
			}
			if hasAnyClassToken(cls,
				"markdown-section",
				"vp-doc",
				"theme-default-content",
				"theme-hope-content",
				"theme-doc-markdown",
				"md-content__inner",
				"rst-content",
				"td-content",
				"markdown-body",
			) || (tag == "main" && n.Parent != nil && strings.EqualFold(getNodeAttr(n.Parent, "id"), "content")) {
				tier1 = n
				return
			}
			if tier2 == nil && tag == "article" {
				tier2 = n
			}
			if tier3 == nil && (tag == "main" || role == "main") {
				tier3 = n
			}
			if tier4 == nil && (idVal == "content" || idVal == "main-content" || idVal == "page-wrapper" ||
				hasAnyClassToken(cls, "page-inner", "book-body", "doc-content", "content")) {
				tier4 = n
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	for _, cand := range []*nethtml.Node{tier1, tier2, tier3, tier4, bodyNode} {
		if cand != nil {
			return cand
		}
	}
	return nil
}

func isBookChromeContainer(tag, idLower, cls string) bool {
	switch tag {
	case "nav", "aside", "header", "footer", "script", "style", "noscript", "iframe", "form":
		return true
	}
	switch idLower {
	case "sidebar", "book-search-input", "book-search-results", "menu-bar", "page-toc", "toc", "table-of-contents":
		return true
	}
	return hasAnyClassToken(cls,
		"book-summary", "book-header", "navigation", "navigation-prev", "navigation-next",
		"nav-chapters", "nav-wide-wrapper", "sidebar", "VPDocAside", "VPLocalNav", "VPSidebar",
		"VPNav", "VPDocFooter", "edit-link", "page-edit", "page-nav", "pagination-nav",
		"toc", "table-of-contents", "md-sidebar", "sphinxsidebar", "wy-nav-side",
		"rst-versions", "rst-footer-buttons", "breadcrumbs", "wy-breadcrumbs", "search-results",
	)
}

func pruneBookChromeNodes(root *nethtml.Node) {
	var walk func(n *nethtml.Node)
	walk = func(n *nethtml.Node) {
		for c := n.FirstChild; c != nil; {
			next := c.NextSibling
			if c.Type == nethtml.ElementNode {
				tag := strings.ToLower(c.Data)
				idRaw := getNodeAttr(c, "id")
				nameRaw := getNodeAttr(c, "name")
				cls := getNodeAttr(c, "class")

				if isBookChromeContainer(tag, strings.ToLower(idRaw), cls) || hasNodeAttr(c, "hidden") {
					n.RemoveChild(c)
					c = next
					continue
				}
				// Strip heading permalink '#' / '¶' clutter while preserving anchor IDs on parent heading!
				if tag == "a" {
					anchorID := strings.TrimSpace(idRaw)
					if anchorID == "" {
						anchorID = strings.TrimSpace(nameRaw)
					}
					txt := strings.TrimSpace(extractNodeText(c))
					isPermalinkGlyph := hasAnyClassToken(cls, "headerlink", "header-anchor", "anchor", "anchorjs-link", "hash-link", "plugin-anchor") ||
						((txt == "#" || txt == "¶" || txt == "") && strings.HasPrefix(strings.TrimSpace(getNodeAttr(c, "href")), "#")) ||
						(txt == "" && anchorID != "" && getNodeAttr(c, "href") == "")
					if isPermalinkGlyph {
						if anchorID != "" && n.Type == nethtml.ElementNode && getNodeAttr(n, "id") == "" {
							setNodeAttr(n, "id", anchorID)
						}
						n.RemoveChild(c)
						c = next
						continue
					}
				}
			}
			walk(c)
			c = next
		}
	}
	walk(root)
}

func stripDuplicateLeadingH1(root *nethtml.Node, chapterTitle string) {
	want := normalizeTitleForMatch(chapterTitle)
	if want == "" {
		return
	}
	for c := root.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == nethtml.TextNode && strings.TrimSpace(c.Data) == "" {
			continue
		}
		if c.Type == nethtml.CommentNode {
			continue
		}
		if c.Type == nethtml.ElementNode && strings.EqualFold(c.Data, "h1") {
			if normalizeTitleForMatch(extractNodeText(c)) == want {
				// Preserve heading ID on first following element or keep an empty span anchor if needed
				if hID := strings.TrimSpace(getNodeAttr(c, "id")); hID != "" {
					c.Data = "div"
					c.Attr = []nethtml.Attribute{{Key: "id", Val: hID}}
					c.FirstChild = nil
					c.LastChild = nil
				} else {
					root.RemoveChild(c)
				}
			}
		}
		break
	}
}

func normalizeTitleForMatch(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		if unicode.IsSpace(r) || r == '#' || r == '¶' {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func buildBookChapterLookup(bk BookDetailView) map[string]string {
	m := make(map[string]string, len(bk.FlatChapters)*3+2)
	for _, ch := range bk.FlatChapters {
		u, err := url.Parse(strings.TrimSpace(ch.URL))
		if err != nil || u.Host == "" {
			continue
		}
		for _, k := range bookURLKeys(u) {
			if _, exists := m[k]; !exists {
				m[k] = ch.ID
			}
		}
	}
	if len(bk.FlatChapters) > 0 && strings.TrimSpace(bk.SourceURL) != "" {
		if su, err := url.Parse(strings.TrimSpace(bk.SourceURL)); err == nil && su.Host != "" {
			for _, k := range bookURLKeys(su) {
				if _, exists := m[k]; !exists {
					m[k] = bk.FlatChapters[0].ID
				}
			}
		}
	}
	return m
}

func bookURLKeys(u *url.URL) []string {
	if u == nil || u.Host == "" {
		return nil
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return nil
	}
	host := strings.ToLower(u.Host)
	rawPath := u.Path
	if unesc, err := url.PathUnescape(rawPath); err == nil && unesc != "" {
		rawPath = unesc
	}
	if rawPath == "" {
		rawPath = "/"
	}
	cleanP := path.Clean(rawPath)
	if !strings.HasPrefix(cleanP, "/") {
		cleanP = "/" + cleanP
	}

	seen := make(map[string]bool, 5)
	var keys []string
	add := func(p string) {
		k := scheme + "://" + host + p
		if !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}

	add(cleanP)
	lowP := strings.ToLower(cleanP)
	switch {
	case strings.HasSuffix(lowP, "/index.html"):
		dir := strings.TrimSuffix(cleanP[:len(cleanP)-len("/index.html")], "/")
		if dir == "" {
			add("/")
		} else {
			add(dir)
			add(dir + "/")
		}
	case strings.HasSuffix(lowP, "/index.htm"):
		dir := strings.TrimSuffix(cleanP[:len(cleanP)-len("/index.htm")], "/")
		if dir == "" {
			add("/")
		} else {
			add(dir)
			add(dir + "/")
		}
	case strings.HasSuffix(lowP, "/readme.md"):
		dir := strings.TrimSuffix(cleanP[:len(cleanP)-len("/readme.md")], "/")
		if dir == "" {
			add("/")
		} else {
			add(dir)
			add(dir + "/")
			add(dir + "/index.html")
		}
	case strings.HasSuffix(lowP, ".html"):
		stem := cleanP[:len(cleanP)-len(".html")]
		add(stem)
		add(stem + ".md")
	case strings.HasSuffix(lowP, ".md"):
		stem := cleanP[:len(cleanP)-len(".md")]
		add(stem + ".html")
		add(stem)
	default:
		if cleanP == "/" {
			add("/index.html")
		} else {
			trimmed := strings.TrimSuffix(cleanP, "/")
			add(trimmed)
			add(trimmed + "/")
			add(trimmed + "/index.html")
			add(trimmed + ".html")
		}
	}
	return keys
}

func rewriteBookSubtreeURLs(
	root *nethtml.Node,
	finalURL *url.URL,
	bookSlug string,
	activeChapterID string,
	activeKeys map[string]bool,
	chapterMap map[string]string,
) {
	var walk func(n *nethtml.Node)
	walk = func(n *nethtml.Node) {
		if n.Type == nethtml.ElementNode {
			tag := strings.ToLower(n.Data)
			switch tag {
			case "a":
				rawHref := strings.TrimSpace(getNodeAttr(n, "href"))
				if rawHref == "" || isDangerousHrefScheme(rawHref) {
					removeNodeAttr(n, "href")
				} else if strings.HasPrefix(rawHref, "#") {
					// Keep in-chapter #fragment; proseHTML binds it to current /books/{slug}?ch={id}#frag
					removeNodeAttr(n, "target")
					removeNodeAttr(n, "data-no-pjax")
				} else if finalURL != nil {
					if ref, err := url.Parse(rawHref); err == nil {
						abs := finalURL.ResolveReference(ref)
						frag := ""
						if abs.Fragment != "" {
							frag = "#" + abs.Fragment
						}
						matchedID := ""
						isSelf := false
						for _, k := range bookURLKeys(abs) {
							if activeKeys[k] {
								isSelf = true
							}
							if idHit, ok := chapterMap[k]; ok && matchedID == "" {
								matchedID = idHit
							}
						}
						if isSelf && frag != "" {
							setNodeAttr(n, "href", frag)
							removeNodeAttr(n, "target")
							removeNodeAttr(n, "rel")
							removeNodeAttr(n, "data-no-pjax")
						} else if matchedID != "" || isSelf {
							chID := matchedID
							if chID == "" {
								chID = activeChapterID
							}
							internalHref := fmt.Sprintf("/books/%s?ch=%s%s", url.PathEscape(bookSlug), url.QueryEscape(chID), frag)
							setNodeAttr(n, "href", internalHref)
							removeNodeAttr(n, "target")
							removeNodeAttr(n, "rel")
							removeNodeAttr(n, "data-no-pjax")
						} else if abs.Scheme == "http" || abs.Scheme == "https" {
							setNodeAttr(n, "href", abs.String())
							setNodeAttr(n, "target", "_blank")
							setNodeAttr(n, "rel", "noopener noreferrer")
							setNodeAttr(n, "data-no-pjax", "1")
						} else {
							removeNodeAttr(n, "href")
						}
					}
				}
			case "img", "source", "audio", "video":
				src := strings.TrimSpace(getNodeAttr(n, "src"))
				if (src == "" || strings.HasPrefix(strings.ToLower(src), "data:")) && tag == "img" {
					for _, altAttr := range []string{"data-src", "data-original", "data-lazy-src"} {
						if ds := strings.TrimSpace(getNodeAttr(n, altAttr)); ds != "" && !isDangerousHrefScheme(ds) {
							src = ds
							break
						}
					}
				}
				if src != "" && finalURL != nil && !strings.HasPrefix(strings.ToLower(src), "data:") {
					if ref, err := url.Parse(src); err == nil {
						abs := finalURL.ResolveReference(ref)
						if abs.Scheme == "http" || abs.Scheme == "https" {
							setNodeAttr(n, "src", abs.String())
						} else {
							removeNodeAttr(n, "src")
						}
					}
				}
				removeNodeAttr(n, "srcset")
				if tag == "img" && getNodeAttr(n, "loading") == "" {
					setNodeAttr(n, "loading", "lazy")
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(root)
}

func getNodeAttr(n *nethtml.Node, key string) string {
	for _, a := range n.Attr {
		if strings.EqualFold(a.Key, key) {
			return a.Val
		}
	}
	return ""
}

func hasNodeAttr(n *nethtml.Node, key string) bool {
	for _, a := range n.Attr {
		if strings.EqualFold(a.Key, key) {
			return true
		}
	}
	return false
}

func setNodeAttr(n *nethtml.Node, key, val string) {
	for i := range n.Attr {
		if strings.EqualFold(n.Attr[i].Key, key) {
			n.Attr[i].Val = val
			return
		}
	}
	n.Attr = append(n.Attr, nethtml.Attribute{Key: key, Val: val})
}

func removeNodeAttr(n *nethtml.Node, key string) {
	out := n.Attr[:0]
	for _, a := range n.Attr {
		if !strings.EqualFold(a.Key, key) {
			out = append(out, a)
		}
	}
	n.Attr = out
}

func hasAnyClassToken(cls string, targets ...string) bool {
	if cls == "" {
		return false
	}
	for _, tok := range strings.Fields(cls) {
		for _, want := range targets {
			if strings.EqualFold(tok, want) {
				return true
			}
		}
	}
	return false
}

func extractNodeText(n *nethtml.Node) string {
	var b strings.Builder
	var walk func(cur *nethtml.Node)
	walk = func(cur *nethtml.Node) {
		if cur.Type == nethtml.TextNode {
			b.WriteString(cur.Data)
		}
		for c := cur.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return b.String()
}

func countVisibleRunes(n *nethtml.Node, stopAt int) int {
	cnt := 0
	var walk func(cur *nethtml.Node)
	walk = func(cur *nethtml.Node) {
		if cnt >= stopAt {
			return
		}
		if cur.Type == nethtml.TextNode {
			for _, r := range cur.Data {
				if !unicode.IsSpace(r) {
					cnt++
					if cnt >= stopAt {
						return
					}
				}
			}
		}
		for c := cur.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return cnt
}

func hasBookMediaElement(n *nethtml.Node) bool {
	found := false
	var walk func(cur *nethtml.Node)
	walk = func(cur *nethtml.Node) {
		if found {
			return
		}
		if cur.Type == nethtml.ElementNode {
			switch strings.ToLower(cur.Data) {
			case "img", "pre", "table", "svg", "video", "audio":
				found = true
				return
			}
		}
		for c := cur.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return found
}
