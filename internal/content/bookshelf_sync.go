package content

import (
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

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

// ListBooks returns all books ordered by sort_order ASC, created_at DESC.
func (s *Service) ListBooks(ctx context.Context) ([]domain.Book, error) {
	return sqlx.Select[domain.Book](ctx, s.store, `SELECT * FROM books ORDER BY sort_order ASC, created_at DESC`)
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
		if title == "" || rawHref == "" {
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
		if !isSafeChapterURL(resolved) {
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
		if title == "" || href == "" || strings.HasPrefix(strings.ToLower(href), "javascript:") || strings.HasPrefix(strings.ToLower(href), "mailto:") {
			continue
		}
		resolved := resolveRelativeURL(baseURL, href)
		if !isSafeChapterURL(resolved) || seen[resolved] {
			continue
		}
		u, err := url.Parse(resolved)
		if err != nil || !strings.EqualFold(u.Host, baseURL.Host) {
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
		return ref
	}
	u, err := url.Parse(ref)
	if err != nil {
		return ""
	}
	return base.ResolveReference(u).String()
}

func isSafeChapterURL(raw string) bool {
	s := strings.TrimSpace(raw)
	if strings.HasPrefix(s, "/") && !strings.HasPrefix(s, "//") {
		return true
	}
	u, err := url.Parse(s)
	if err != nil {
		return false
	}
	return u.Scheme == "http" || u.Scheme == "https"
}
