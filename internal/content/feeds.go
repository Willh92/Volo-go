package content

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"html"
	"net/http"
	"strings"
	"time"

	"github.com/Willh92/Volo-go/internal/db/sqlx"
	"github.com/Willh92/Volo-go/internal/domain"
	"github.com/Willh92/Volo-go/internal/render"
	"github.com/Willh92/Volo-go/internal/visibility"
)

func (s *Service) collectPublicFeedItems(ctx context.Context, publicBaseURL string, locale string, audioOnly bool, maxItems int) ([]visibility.FeedItem, error) {
	if maxItems <= 0 {
		maxItems = 20
	}
	if maxItems > 100 {
		maxItems = 100
	}
	snap := s.settings.Snapshot()
	fullOutput := strings.EqualFold(snap.Feed.OutputMode, "full")

	where := `status = ? AND password_ciphertext = ''`
	args := []any{domain.ArticleStatusPublished}
	if audioOnly {
		where += ` AND audio_url <> ''`
	}
	q, pageArgs := s.store.Dialect().PaginateSQL(
		`SELECT * FROM articles WHERE `+where+` ORDER BY published_at DESC`,
		maxItems, 0,
	)
	raws, err := sqlx.Select[domain.RawArticle](ctx, s.store, q, append(args, pageArgs...)...)
	if err != nil {
		return nil, err
	}

	catsMap, _ := s.loadCategoriesMap(ctx)
	authorByID := make(map[string]string)
	var authorIDs []any
	seenAuthor := make(map[string]struct{})
	for _, r := range raws {
		aid := strings.TrimSpace(r.AuthorID)
		if aid != "" {
			if _, ok := seenAuthor[aid]; !ok {
				seenAuthor[aid] = struct{}{}
				authorIDs = append(authorIDs, aid)
			}
		}
	}
	if len(authorIDs) > 0 {
		ph := make([]string, len(authorIDs))
		for i := range authorIDs {
			ph[i] = "?"
		}
		users, _ := sqlx.Select[domain.User](
			ctx, s.store,
			`SELECT id, display_name, username FROM users WHERE id IN (`+strings.Join(ph, ",")+`)`,
			authorIDs...,
		)
		for _, u := range users {
			name := u.DisplayName
			if name == "" {
				name = u.Username
			}
			authorByID[u.ID] = name
		}
	}

	items := make([]visibility.FeedItem, 0, len(raws))
	for _, raw := range raws {
		author := authorByID[raw.AuthorID]
		if author == "" {
			author = snap.Site.Title
		}
		catName := ""
		if c, ok := catsMap[raw.CategoryID]; ok {
			catName = c.Name
		}
		if fi, ok := s.guard.ToFeedItem(raw, fullOutput, locale, publicBaseURL, author, catName); ok {
			items = append(items, fi)
		}
	}
	return items, nil
}

// BuildRSSXML renders a standards-compliant RSS 2.0 document (`/rss.xml`) strictly enforcing Surface 6.
func (s *Service) BuildRSSXML(ctx context.Context, publicBaseURL string, locale string) ([]byte, error) {
	snap := s.settings.Snapshot()
	base := strings.TrimRight(publicBaseURL, "/")
	items, err := s.collectPublicFeedItems(ctx, base, locale, false, snap.Feed.ItemCount)
	if err != nil {
		return nil, err
	}

	lastBuild := time.Now().UTC().Format(time.RFC1123Z)
	if len(items) > 0 {
		ts := items[0].UpdatedAt
		if ts <= 0 {
			ts = items[0].PublishedAt
		}
		if ts > 0 {
			lastBuild = time.UnixMilli(ts).UTC().Format(time.RFC1123Z)
		}
	}

	var b bytes.Buffer
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<rss version="2.0" xmlns:content="http://purl.org/rss/1.0/modules/content/" xmlns:atom="http://www.w3.org/2005/Atom">` + "\n")
	b.WriteString("  <channel>\n")
	writeXMLTag(&b, "    ", "title", snap.Site.Title)
	writeXMLTag(&b, "    ", "link", base+"/")
	writeXMLTag(&b, "    ", "description", snap.Site.Description)
	writeXMLTag(&b, "    ", "language", snap.Site.DefaultLocale)
	fmt.Fprintf(&b, "    <atom:link href=%q rel=\"self\" type=\"application/rss+xml\"/>\n", base+"/rss.xml")
	writeXMLTag(&b, "    ", "lastBuildDate", lastBuild)

	for _, it := range items {
		b.WriteString("    <item>\n")
		writeXMLTag(&b, "      ", "title", it.Title)
		writeXMLTag(&b, "      ", "link", it.URL)
		fmt.Fprintf(&b, "      <guid isPermaLink=\"true\">%s</guid>\n", xmlEscape(it.URL))
		writeXMLTag(&b, "      ", "pubDate", time.UnixMilli(it.PublishedAt).UTC().Format(time.RFC1123Z))
		writeXMLTag(&b, "      ", "author", it.AuthorName)
		if it.CategoryName != "" {
			writeXMLTag(&b, "      ", "category", it.CategoryName)
		}
		writeXMLTag(&b, "      ", "description", it.Summary)
		fmt.Fprintf(&b, "      <content:encoded><![CDATA[%s]]></content:encoded>\n", safeCDATA(it.HTML))
		if it.AudioURL != "" {
			mimeType := it.AudioMIME
			if mimeType == "" {
				mimeType = "audio/mpeg"
			}
			fmt.Fprintf(&b, "      <enclosure url=%q length=\"%d\" type=%q/>\n", it.AudioURL, it.AudioSizeBytes, mimeType)
		}
		b.WriteString("    </item>\n")
	}

	b.WriteString("  </channel>\n</rss>\n")
	return b.Bytes(), nil
}

// GetCachedRSS returns a revision-coherent, pre-compressed RSS 2.0 document with strong ETag.
func (s *Service) GetCachedRSS(ctx context.Context, publicBaseURL string, locale string) (CachedDocument, error) {
	key := "rss|" + strings.TrimRight(publicBaseURL, "/") + "|" + locale
	return s.getOrBuildCachedDoc(ctx, key, "application/rss+xml; charset=utf-8", func(c context.Context) ([]byte, error) {
		return s.BuildRSSXML(c, publicBaseURL, locale)
	})
}

// GetCachedAtom returns a revision-coherent, pre-compressed Atom 1.0 document with strong ETag.
func (s *Service) GetCachedAtom(ctx context.Context, publicBaseURL string, locale string) (CachedDocument, error) {
	key := "atom|" + strings.TrimRight(publicBaseURL, "/") + "|" + locale
	return s.getOrBuildCachedDoc(ctx, key, "application/atom+xml; charset=utf-8", func(c context.Context) ([]byte, error) {
		return s.BuildAtomXML(c, publicBaseURL, locale)
	})
}

// GetCachedPodcast returns a revision-coherent, pre-compressed iTunes Podcast RSS document with strong ETag.
func (s *Service) GetCachedPodcast(ctx context.Context, publicBaseURL string, locale string) (CachedDocument, error) {
	key := "podcast|" + strings.TrimRight(publicBaseURL, "/") + "|" + locale
	return s.getOrBuildCachedDoc(ctx, key, "application/rss+xml; charset=utf-8", func(c context.Context) ([]byte, error) {
		return s.BuildPodcastXML(c, publicBaseURL, locale)
	})
}

// GetCachedSitemap returns a revision-coherent, pre-compressed Sitemap XML document with strong ETag.
func (s *Service) GetCachedSitemap(ctx context.Context, publicBaseURL string) (CachedDocument, error) {
	key := "sitemap|" + strings.TrimRight(publicBaseURL, "/")
	return s.getOrBuildCachedDoc(ctx, key, "application/xml; charset=utf-8", func(c context.Context) ([]byte, error) {
		return s.BuildSitemapXML(c, publicBaseURL)
	})
}

// GetCachedLLMSTxt returns a revision-coherent, pre-compressed /llms.txt or /llms-full.txt document with strong ETag.
func (s *Service) GetCachedLLMSTxt(ctx context.Context, publicBaseURL string, full bool) (CachedDocument, error) {
	kind := "llms"
	if full {
		kind = "llms-full"
	}
	key := kind + "|" + strings.TrimRight(publicBaseURL, "/")
	return s.getOrBuildCachedDoc(ctx, key, "text/plain; charset=utf-8", func(c context.Context) ([]byte, error) {
		return s.BuildLLMSTxt(c, publicBaseURL, full)
	})
}

// BuildAtomXML renders an RFC 4287 Atom 1.0 feed (`/atom.xml`) strictly enforcing Surface 6.
func (s *Service) BuildAtomXML(ctx context.Context, publicBaseURL string, locale string) ([]byte, error) {
	snap := s.settings.Snapshot()
	base := strings.TrimRight(publicBaseURL, "/")
	items, err := s.collectPublicFeedItems(ctx, base, locale, false, snap.Feed.ItemCount)
	if err != nil {
		return nil, err
	}

	updated := time.Now().UTC().Format(time.RFC3339)
	if len(items) > 0 && items[0].UpdatedAt > 0 {
		updated = time.UnixMilli(items[0].UpdatedAt).UTC().Format(time.RFC3339)
	}

	var b bytes.Buffer
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<feed xmlns="http://www.w3.org/2005/Atom">` + "\n")
	writeXMLTag(&b, "  ", "title", snap.Site.Title)
	writeXMLTag(&b, "  ", "subtitle", snap.Site.Subtitle)
	fmt.Fprintf(&b, "  <link href=%q rel=\"self\" type=\"application/atom+xml\"/>\n", base+"/atom.xml")
	fmt.Fprintf(&b, "  <link href=%q rel=\"alternate\" type=\"text/html\"/>\n", base+"/")
	writeXMLTag(&b, "  ", "id", base+"/")
	writeXMLTag(&b, "  ", "updated", updated)

	for _, it := range items {
		b.WriteString("  <entry>\n")
		writeXMLTag(&b, "    ", "title", it.Title)
		fmt.Fprintf(&b, "    <link href=%q rel=\"alternate\"/>\n", it.URL)
		writeXMLTag(&b, "    ", "id", it.URL)
		writeXMLTag(&b, "    ", "published", time.UnixMilli(it.PublishedAt).UTC().Format(time.RFC3339))
		upd := it.UpdatedAt
		if upd <= 0 {
			upd = it.PublishedAt
		}
		writeXMLTag(&b, "    ", "updated", time.UnixMilli(upd).UTC().Format(time.RFC3339))
		b.WriteString("    <author><name>" + xmlEscape(it.AuthorName) + "</name></author>\n")
		writeXMLTag(&b, "    ", "summary", it.Summary)
		fmt.Fprintf(&b, "    <content type=\"html\"><![CDATA[%s]]></content>\n", safeCDATA(it.HTML))
		b.WriteString("  </entry>\n")
	}
	b.WriteString("</feed>\n")
	return b.Bytes(), nil
}

// BuildPodcastXML renders an Apple Podcasts & Spotify compliant iTunes RSS feed (`/podcast.xml`).
func (s *Service) BuildPodcastXML(ctx context.Context, publicBaseURL string, locale string) ([]byte, error) {
	snap := s.settings.Snapshot()
	pod := snap.Feed.Podcast
	base := strings.TrimRight(publicBaseURL, "/")

	items, err := s.collectPublicFeedItems(ctx, base, locale, true, 50)
	if err != nil {
		return nil, err
	}

	chanTitle := strings.TrimSpace(pod.Title)
	if chanTitle == "" {
		chanTitle = snap.Site.Title
	}
	chanSummary := strings.TrimSpace(pod.Summary)
	if chanSummary == "" {
		chanSummary = snap.Site.Description
	}
	chanAuthor := strings.TrimSpace(pod.Author)
	if chanAuthor == "" {
		chanAuthor = snap.Site.Title
	}
	lang := strings.TrimSpace(pod.Language)
	if lang == "" {
		lang = snap.Site.DefaultLocale
	}
	coverURL := strings.TrimSpace(pod.CoverImageURL)
	if strings.HasPrefix(coverURL, "/") {
		coverURL = base + coverURL
	}
	explicitStr := "false"
	if pod.Explicit {
		explicitStr = "true"
	}

	var b bytes.Buffer
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<rss version="2.0" xmlns:itunes="http://www.itunes.com/dtds/podcast-1.0.dtd" xmlns:content="http://purl.org/rss/1.0/modules/content/" xmlns:atom="http://www.w3.org/2005/Atom">` + "\n")
	b.WriteString("  <channel>\n")
	writeXMLTag(&b, "    ", "title", chanTitle)
	writeXMLTag(&b, "    ", "link", base+"/")
	writeXMLTag(&b, "    ", "language", lang)
	writeXMLTag(&b, "    ", "description", chanSummary)
	fmt.Fprintf(&b, "    <atom:link href=%q rel=\"self\" type=\"application/rss+xml\"/>\n", base+"/podcast.xml")
	writeXMLTag(&b, "    ", "itunes:author", chanAuthor)
	writeXMLTag(&b, "    ", "itunes:summary", chanSummary)
	writeXMLTag(&b, "    ", "itunes:explicit", explicitStr)
	if pod.Subtitle != "" {
		writeXMLTag(&b, "    ", "itunes:subtitle", pod.Subtitle)
	}
	if pod.Copyright != "" {
		writeXMLTag(&b, "    ", "copyright", pod.Copyright)
	}
	if coverURL != "" {
		fmt.Fprintf(&b, "    <itunes:image href=%q/>\n", coverURL)
	}
	if pod.OwnerName != "" || pod.OwnerEmail != "" {
		b.WriteString("    <itunes:owner>\n")
		writeXMLTag(&b, "      ", "itunes:name", pod.OwnerName)
		writeXMLTag(&b, "      ", "itunes:email", pod.OwnerEmail)
		b.WriteString("    </itunes:owner>\n")
	}
	cat := strings.TrimSpace(pod.Category)
	if cat == "" {
		cat = "Technology"
	}
	if strings.TrimSpace(pod.SubCategory) != "" {
		fmt.Fprintf(&b, "    <itunes:category text=%q>\n      <itunes:category text=%q/>\n    </itunes:category>\n", cat, pod.SubCategory)
	} else {
		fmt.Fprintf(&b, "    <itunes:category text=%q/>\n", cat)
	}

	for _, it := range items {
		mimeType := it.AudioMIME
		if mimeType == "" {
			mimeType = "audio/mpeg"
		}
		b.WriteString("    <item>\n")
		writeXMLTag(&b, "      ", "title", it.Title)
		writeXMLTag(&b, "      ", "link", it.URL)
		fmt.Fprintf(&b, "      <guid isPermaLink=\"true\">%s</guid>\n", xmlEscape(it.URL))
		writeXMLTag(&b, "      ", "pubDate", time.UnixMilli(it.PublishedAt).UTC().Format(time.RFC1123Z))
		writeXMLTag(&b, "      ", "description", it.Summary)
		fmt.Fprintf(&b, "      <content:encoded><![CDATA[%s]]></content:encoded>\n", safeCDATA(it.HTML))
		fmt.Fprintf(&b, "      <enclosure url=%q length=\"%d\" type=%q/>\n", it.AudioURL, it.AudioSizeBytes, mimeType)
		writeXMLTag(&b, "      ", "itunes:author", it.AuthorName)
		writeXMLTag(&b, "      ", "itunes:explicit", explicitStr)
		if it.AudioDurationSec > 0 {
			writeXMLTag(&b, "      ", "itunes:duration", formatITunesDuration(it.AudioDurationSec))
		}
		b.WriteString("    </item>\n")
	}
	b.WriteString("  </channel>\n</rss>\n")
	return b.Bytes(), nil
}

// BuildSitemapXML generates `/sitemap.xml` with multilingual `xhtml:link rel="alternate" hreflang`
// while strictly excluding all password-protected articles (Surface 7).
func (s *Service) BuildSitemapXML(ctx context.Context, publicBaseURL string) ([]byte, error) {
	base := strings.TrimRight(publicBaseURL, "/")

	arts, err := sqlx.Select[domain.RawArticle](
		ctx, s.store,
		`SELECT id, slug, lang, translation_group, published_at, updated_at, status, password_ciphertext
		 FROM articles
		 WHERE status = ? AND password_ciphertext = ''
		 ORDER BY published_at DESC`,
		domain.ArticleStatusPublished,
	)
	if err != nil {
		return nil, err
	}
	pages, err := sqlx.Select[domain.Page](
		ctx, s.store,
		`SELECT id, slug, lang, translation_group, updated_at FROM pages WHERE is_published = ? ORDER BY slug ASC`,
		true,
	)
	if err != nil {
		return nil, err
	}

	artGroups := make(map[string][]domain.RawArticle)
	for _, a := range arts {
		if s.guard.IsPublicIndexable(a) && strings.TrimSpace(a.TranslationGroup) != "" {
			artGroups[a.TranslationGroup] = append(artGroups[a.TranslationGroup], a)
		}
	}
	pageGroups := make(map[string][]domain.Page)
	for _, p := range pages {
		if strings.TrimSpace(p.TranslationGroup) != "" {
			pageGroups[p.TranslationGroup] = append(pageGroups[p.TranslationGroup], p)
		}
	}

	var b bytes.Buffer
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9" xmlns:xhtml="http://www.w3.org/1999/xhtml">` + "\n")

	// Home & structural index routes
	for _, route := range []string{"/", "/archives", "/categories", "/tags", "/series", "/books"} {
		b.WriteString("  <url>\n")
		writeXMLTag(&b, "    ", "loc", base+route)
		b.WriteString("  </url>\n")
	}

	for _, a := range arts {
		if !s.guard.IsPublicIndexable(a) {
			continue
		}
		loc := base + visibility.ArticlePath(a.Slug)
		upd := a.UpdatedAt
		if upd <= 0 {
			upd = a.PublishedAt
		}
		b.WriteString("  <url>\n")
		writeXMLTag(&b, "    ", "loc", loc)
		if upd > 0 {
			writeXMLTag(&b, "    ", "lastmod", time.UnixMilli(upd).UTC().Format("2006-01-02"))
		}
		if grp := artGroups[a.TranslationGroup]; len(grp) > 1 {
			for _, peer := range grp {
				lang := peer.Lang
				if lang == "" {
					lang = "x-default"
				}
				fmt.Fprintf(&b, "    <xhtml:link rel=\"alternate\" hreflang=%q href=%q/>\n", lang, base+visibility.ArticlePath(peer.Slug))
			}
		}
		b.WriteString("  </url>\n")
	}

	for _, p := range pages {
		loc := base + "/" + p.Slug
		b.WriteString("  <url>\n")
		writeXMLTag(&b, "    ", "loc", loc)
		if p.UpdatedAt > 0 {
			writeXMLTag(&b, "    ", "lastmod", time.UnixMilli(p.UpdatedAt).UTC().Format("2006-01-02"))
		}
		if grp := pageGroups[p.TranslationGroup]; len(grp) > 1 {
			for _, peer := range grp {
				lang := peer.Lang
				if lang == "" {
					lang = "x-default"
				}
				fmt.Fprintf(&b, "    <xhtml:link rel=\"alternate\" hreflang=%q href=%q/>\n", lang, base+"/"+peer.Slug)
			}
		}
		b.WriteString("  </url>\n")
	}

	b.WriteString("</urlset>\n")
	return b.Bytes(), nil
}

// BuildLLMSTxt generates `/llms.txt` (index) or `/llms-full.txt` (concatenated public Markdown)
// strictly enforcing Surface 9 (password-protected articles 100% excluded, `:::lock` blocks stripped).
func (s *Service) BuildLLMSTxt(ctx context.Context, publicBaseURL string, full bool) ([]byte, error) {
	snap := s.settings.Snapshot()
	base := strings.TrimRight(publicBaseURL, "/")

	var (
		arts  []domain.RawArticle
		pages []domain.Page
		err   error
	)
	if !full {
		artSQL, artArgs := s.store.Dialect().PaginateSQL(
			`SELECT id, author_id, category_id, title, slug, summary_md, cover_url, status, is_pinned,
			        password_ciphertext, has_content_lock, audio_url, lang, word_count, reading_minutes,
			        view_count, comment_count, published_at, updated_at
			 FROM articles
			 WHERE status = ? AND password_ciphertext = ''
			 ORDER BY is_pinned DESC, published_at DESC`,
			1000, 0,
		)
		arts, err = sqlx.Select[domain.RawArticle](
			ctx, s.store, artSQL,
			append([]any{domain.ArticleStatusPublished}, artArgs...)...,
		)
		if err != nil {
			return nil, err
		}
		pages, _ = sqlx.Select[domain.Page](
			ctx, s.store,
			`SELECT id, title, slug FROM pages WHERE is_published = ? ORDER BY slug ASC`,
			true,
		)
	} else {
		artSQL, artArgs := s.store.Dialect().PaginateSQL(
			`SELECT * FROM articles
			 WHERE status = ? AND password_ciphertext = ''
			 ORDER BY is_pinned DESC, published_at DESC`,
			200, 0,
		)
		arts, err = sqlx.Select[domain.RawArticle](
			ctx, s.store, artSQL,
			append([]any{domain.ArticleStatusPublished}, artArgs...)...,
		)
		if err != nil {
			return nil, err
		}
		pages, _ = sqlx.Select[domain.Page](
			ctx, s.store,
			`SELECT * FROM pages WHERE is_published = ? ORDER BY slug ASC`,
			true,
		)
	}

	var b strings.Builder
	b.WriteString("# " + snap.Site.Title + "\n\n")
	if snap.Site.Description != "" {
		b.WriteString("> " + snap.Site.Description + "\n\n")
	}

	b.WriteString("## Articles\n\n")
	for _, a := range arts {
		if a.Status != domain.ArticleStatusPublished || strings.TrimSpace(a.PasswordCiphertext) != "" {
			continue
		}
		if !full {
			cleanSummaryMD := strings.TrimSpace(render.StripAllLockBlocks(a.SummaryMD))
			summary := a.Title
			if cleanSummaryMD != "" {
				rendered := s.renderer.Render(cleanSummaryMD, render.Options{Mode: render.RenderModeWeb})
				if plain := truncateForSummary(rendered.PlainText, 180); plain != "" {
					summary = plain
				}
			}
			fmt.Fprintf(&b, "- [%s](%s%s.md): %s\n", a.Title, base, visibility.ArticlePath(a.Slug), summary)
		} else {
			md, ok := s.guard.ToSafeRawMarkdown(a)
			if !ok {
				continue
			}
			fmt.Fprintf(&b, "---\nURL: %s%s\n\n%s\n", base, visibility.ArticlePath(a.Slug), md)
		}
	}

	if len(pages) > 0 {
		b.WriteString("\n## Standalone Pages\n\n")
		for _, p := range pages {
			if !full {
				fmt.Fprintf(&b, "- [%s](%s/%s)\n", p.Title, base, p.Slug)
			} else {
				cleanPageMD := strings.TrimSpace(render.StripAllLockBlocks(p.ContentMD))
				fmt.Fprintf(&b, "---\nURL: %s/%s\n\n# %s\n\n%s\n", base, p.Slug, p.Title, cleanPageMD)
			}
		}
	}
	return []byte(b.String()), nil
}

// BuildGitHubStatsSVG generates a crisp, local, zero-external-request SVG stats card from cached SEOSettings.
func (s *Service) BuildGitHubStatsSVG(username string) []byte {
	snap := s.settings.Snapshot()
	user := strings.TrimSpace(username)
	if user == "" {
		user = snap.SEO.GitHubUsername
	}
	if user == "" {
		user = "developer"
	}

	totalRepos := 0
	totalStars := 0
	totalForks := 0
	for _, r := range snap.SEO.GitHubRepos {
		if r.Hidden {
			continue
		}
		totalRepos++
		totalStars += r.Stars
		totalForks += r.Forks
	}

	svg := fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" width="440" height="148" viewBox="0 0 440 148" role="img" aria-label="GitHub Stats for %s">
  <rect x="1" y="1" width="438" height="146" rx="14" fill="#f6f7f3" stroke="#dfe6de" stroke-width="1.5"/>
  <text x="24" y="38" font-family="-apple-system,BlinkMacSystemFont,'Segoe UI',sans-serif" font-size="16" font-weight="600" fill="#293d36">@%s · GitHub Open Source</text>
  <line x1="24" y1="54" x2="416" y2="54" stroke="#dfe6de" stroke-width="1"/>
  <g font-family="-apple-system,BlinkMacSystemFont,'Segoe UI',sans-serif">
    <text x="24" y="84" font-size="11" fill="#60756a">PUBLIC REPOS</text>
    <text x="24" y="114" font-size="22" font-weight="700" fill="#246b51">%d</text>
    <text x="172" y="84" font-size="11" fill="#60756a">TOTAL STARS</text>
    <text x="172" y="114" font-size="22" font-weight="700" fill="#246b51">%d</text>
    <text x="312" y="84" font-size="11" fill="#60756a">TOTAL FORKS</text>
    <text x="312" y="114" font-size="22" font-weight="700" fill="#246b51">%d</text>
  </g>
</svg>`, html.EscapeString(user), html.EscapeString(user), totalRepos, totalStars, totalForks)
	return []byte(svg)
}

// PushIndexNow submits public URLs to IndexNow via OutboundSafeDialer (Surface 7: never called for protected posts).
func (s *Service) PushIndexNow(ctx context.Context, publicBaseURL string, urls []string) error {
	snap := s.settings.Snapshot()
	if !snap.SEO.IndexNowEnabled || strings.TrimSpace(snap.SEO.IndexNowKey) == "" || len(urls) == 0 {
		return nil
	}
	base := strings.TrimRight(publicBaseURL, "/")
	host := strings.TrimPrefix(strings.TrimPrefix(base, "https://"), "http://")
	if idx := strings.IndexByte(host, '/'); idx >= 0 {
		host = host[:idx]
	}
	payload := map[string]any{
		"host":        host,
		"key":         snap.SEO.IndexNowKey,
		"keyLocation": base + "/" + snap.SEO.IndexNowKey + ".txt",
		"urlList":     urls,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	client := s.dialer.NewHTTPClient(8 * time.Second)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.indexnow.org/indexnow", bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	return nil
}

func formatITunesDuration(sec int) string {
	h := sec / 3600
	m := (sec % 3600) / 60
	s := sec % 60
	if h > 0 {
		return fmt.Sprintf("%02d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%02d:%02d", m, s)
}

func writeXMLTag(b *bytes.Buffer, indent string, tag string, val string) {
	b.WriteString(indent)
	b.WriteByte('<')
	b.WriteString(tag)
	b.WriteByte('>')
	b.WriteString(xmlEscape(val))
	b.WriteString("</")
	b.WriteString(tag)
	b.WriteString(">\n")
}

func xmlEscape(s string) string {
	var buf bytes.Buffer
	_ = xml.EscapeText(&buf, []byte(s))
	return buf.String()
}

func safeCDATA(s string) string {
	return strings.ReplaceAll(s, "]]>", "]]]]><![CDATA[>")
}
