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
	var raws []domain.RawArticle
	err := sqlx.SelectInto(ctx, s.store, &raws, q, append(args, pageArgs...)...)
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
		var users []domain.User
		_ = sqlx.SelectInto(ctx, s.store, &users,
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

// GetCachedJSONFeed returns a revision-coherent, pre-compressed JSON Feed 1.1 document with strong ETag.
func (s *Service) GetCachedJSONFeed(ctx context.Context, publicBaseURL string, locale string) (CachedDocument, error) {
	key := "jsonfeed|" + strings.TrimRight(publicBaseURL, "/") + "|" + locale
	return s.getOrBuildCachedDoc(ctx, key, "application/feed+json; charset=utf-8", func(c context.Context) ([]byte, error) {
		return s.BuildJSONFeed(c, publicBaseURL, locale)
	})
}

// BuildJSONFeed renders a JSON Feed 1.1 document (`/feed.json`) strictly enforcing Surface 6.
func (s *Service) BuildJSONFeed(ctx context.Context, publicBaseURL string, locale string) ([]byte, error) {
	snap := s.settings.Snapshot()
	base := strings.TrimRight(publicBaseURL, "/")
	items, err := s.collectPublicFeedItems(ctx, base, locale, false, snap.Feed.ItemCount)
	if err != nil {
		return nil, err
	}
	type jsonAuthor struct {
		Name string `json:"name,omitempty"`
	}
	type jsonItem struct {
		ID            string       `json:"id"`
		URL           string       `json:"url"`
		Title         string       `json:"title"`
		Summary       string       `json:"summary,omitempty"`
		ContentHTML   string       `json:"content_html,omitempty"`
		DatePublished string       `json:"date_published,omitempty"`
		DateModified  string       `json:"date_modified,omitempty"`
		Authors       []jsonAuthor `json:"authors,omitempty"`
	}
	outItems := make([]jsonItem, 0, len(items))
	for _, it := range items {
		upd := it.UpdatedAt
		if upd <= 0 {
			upd = it.PublishedAt
		}
		ji := jsonItem{
			ID:            it.URL,
			URL:           it.URL,
			Title:         it.Title,
			Summary:       it.Summary,
			ContentHTML:   it.HTML,
			DatePublished: time.UnixMilli(it.PublishedAt).UTC().Format(time.RFC3339),
			DateModified:  time.UnixMilli(upd).UTC().Format(time.RFC3339),
		}
		if strings.TrimSpace(it.AuthorName) != "" {
			ji.Authors = []jsonAuthor{{Name: it.AuthorName}}
		}
		outItems = append(outItems, ji)
	}
	doc := map[string]any{
		"version":       "https://jsonfeed.org/version/1.1",
		"title":         snap.Site.Title,
		"home_page_url": base + "/",
		"feed_url":      base + "/feed.json",
		"description":   snap.Site.Description,
		"language":      snap.Site.DefaultLocale,
		"items":         outItems,
	}
	return json.MarshalIndent(doc, "", "  ")
}

// GetCachedPodcast returns a revision-coherent, pre-compressed iTunes Podcast RSS document with strong ETag.
func (s *Service) GetCachedPodcast(ctx context.Context, publicBaseURL string, locale string) (CachedDocument, error) {
	key := "podcast|" + strings.TrimRight(publicBaseURL, "/") + "|" + locale
	return s.getOrBuildCachedDoc(ctx, key, "application/rss+xml; charset=utf-8", func(c context.Context) ([]byte, error) {
		return s.BuildPodcastXML(c, publicBaseURL, locale)
	})
}

// GetCachedSitemap returns a revision-coherent, pre-compressed Sitemap XML (or sitemapindex) document with strong ETag.
func (s *Service) GetCachedSitemap(ctx context.Context, publicBaseURL string) (CachedDocument, error) {
	return s.GetCachedSitemapShard(ctx, publicBaseURL, 0)
}

// GetCachedSitemapShard returns a revision-coherent, pre-compressed Sitemap XML or numbered shard document with strong ETag.
func (s *Service) GetCachedSitemapShard(ctx context.Context, publicBaseURL string, shard int) (CachedDocument, error) {
	base := strings.TrimRight(publicBaseURL, "/")
	key := "sitemap|" + base
	if shard >= 1 {
		key = fmt.Sprintf("sitemap|%s|shard=%d", base, shard)
	}
	return s.getOrBuildCachedDoc(ctx, key, "application/xml; charset=utf-8", func(c context.Context) ([]byte, error) {
		return s.BuildSitemapShardXML(c, publicBaseURL, shard)
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

// SitemapShardSize caps the maximum published articles + standalone pages per sitemap shard before emitting a <sitemapindex>.
const SitemapShardSize = 10000

// BuildSitemapXML generates `/sitemap.xml` (or a Sitemap 0.9 `<sitemapindex>` when total items > SitemapShardSize)
// with multilingual `xhtml:link rel="alternate" hreflang` while strictly excluding all password-protected articles (Surface 7).
func (s *Service) BuildSitemapXML(ctx context.Context, publicBaseURL string) ([]byte, error) {
	return s.BuildSitemapShardXML(ctx, publicBaseURL, 0)
}

// BuildSitemapShardXML generates either `/sitemap.xml` (shard <= 0: `<urlset>` when total <= SitemapShardSize,
// `<sitemapindex>` pointing to `/sitemap-1.xml` .. `/sitemap-{N}.xml` when total > SitemapShardSize)
// or an individual `/sitemap-{shard}.xml` `<urlset>` (shard >= 1) bounded by SQL LIMIT/OFFSET.
func (s *Service) BuildSitemapShardXML(ctx context.Context, publicBaseURL string, shard int) ([]byte, error) {
	base := strings.TrimRight(publicBaseURL, "/")

	var artTotal, pageTotal int
	if err := s.store.QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM articles WHERE status = ? AND password_ciphertext = ''`,
		domain.ArticleStatusPublished,
	).Scan(&artTotal); err != nil {
		return nil, err
	}
	if err := s.store.QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM pages WHERE is_published = ?`,
		true,
	).Scan(&pageTotal); err != nil {
		return nil, err
	}
	total := artTotal + pageTotal

	if shard <= 0 && total > SitemapShardSize {
		numShards := (total + SitemapShardSize - 1) / SitemapShardSize
		var maxArtUpd, maxPageUpd int64
		_ = s.store.QueryRowContext(
			ctx,
			`SELECT COALESCE(MAX(CASE WHEN updated_at > 0 THEN updated_at ELSE published_at END), 0)
			 FROM articles WHERE status = ? AND password_ciphertext = ''`,
			domain.ArticleStatusPublished,
		).Scan(&maxArtUpd)
		_ = s.store.QueryRowContext(
			ctx,
			`SELECT COALESCE(MAX(updated_at), 0) FROM pages WHERE is_published = ?`,
			true,
		).Scan(&maxPageUpd)
		maxUpd := maxArtUpd
		if maxPageUpd > maxUpd {
			maxUpd = maxPageUpd
		}
		lastmod := ""
		if maxUpd > 0 {
			lastmod = time.UnixMilli(maxUpd).UTC().Format("2006-01-02")
		}

		var b bytes.Buffer
		b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
		b.WriteString(`<sitemapindex xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">` + "\n")
		for i := 1; i <= numShards; i++ {
			b.WriteString("  <sitemap>\n")
			writeXMLTag(&b, "    ", "loc", fmt.Sprintf("%s/sitemap-%d.xml", base, i))
			if lastmod != "" {
				writeXMLTag(&b, "    ", "lastmod", lastmod)
			}
			b.WriteString("  </sitemap>\n")
		}
		b.WriteString("</sitemapindex>\n")
		return b.Bytes(), nil
	}

	var (
		arts                 []domain.RawArticle
		pages                []domain.Page
		includeStructuralNav bool
	)

	if shard <= 0 {
		includeStructuralNav = true
		var err error
		err = sqlx.SelectInto(ctx, s.store, &arts,
			`SELECT id, slug, lang, translation_group, published_at, updated_at, status, password_ciphertext
			 FROM articles
			 WHERE status = ? AND password_ciphertext = ''
			 ORDER BY published_at DESC`,
			domain.ArticleStatusPublished,
		)
		if err != nil {
			return nil, err
		}
		err = sqlx.SelectInto(ctx, s.store, &pages,
			`SELECT id, slug, lang, translation_group, updated_at FROM pages WHERE is_published = ? ORDER BY slug ASC`,
			true,
		)
		if err != nil {
			return nil, err
		}
	} else {
		includeStructuralNav = shard == 1
		offset := (shard - 1) * SitemapShardSize
		rem := SitemapShardSize

		if offset < artTotal {
			artLimit := rem
			if offset+artLimit > artTotal {
				artLimit = artTotal - offset
			}
			artSQL, artArgs := s.store.Dialect().PaginateSQL(
				`SELECT id, slug, lang, translation_group, published_at, updated_at, status, password_ciphertext
				 FROM articles
				 WHERE status = ? AND password_ciphertext = ''
				 ORDER BY published_at DESC`,
				artLimit, offset,
			)
			var err error
			err = sqlx.SelectInto(ctx, s.store, &arts, artSQL,
				append([]any{domain.ArticleStatusPublished}, artArgs...)...,
			)
			if err != nil {
				return nil, err
			}
			rem -= artLimit
		}

		if rem > 0 && pageTotal > 0 {
			pageOffset := 0
			if offset > artTotal {
				pageOffset = offset - artTotal
			}
			if pageOffset < pageTotal {
				pgSQL, pgArgs := s.store.Dialect().PaginateSQL(
					`SELECT id, slug, lang, translation_group, updated_at FROM pages WHERE is_published = ? ORDER BY slug ASC`,
					rem, pageOffset,
				)
				var err error
				err = sqlx.SelectInto(ctx, s.store, &pages, pgSQL,
					append([]any{true}, pgArgs...)...,
				)
				if err != nil {
					return nil, err
				}
			}
		}
	}

	artGroups := s.resolveSitemapArticleGroups(ctx, arts, shard > 0)
	pageGroups := s.resolveSitemapPageGroups(ctx, pages, shard > 0)

	var b bytes.Buffer
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9" xmlns:xhtml="http://www.w3.org/1999/xhtml">` + "\n")

	if includeStructuralNav {
		// Home & structural index routes
		for _, route := range []string{"/", "/archives", "/categories", "/tags", "/series", "/books"} {
			b.WriteString("  <url>\n")
			writeXMLTag(&b, "    ", "loc", base+route)
			b.WriteString("  </url>\n")
		}
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

func (s *Service) resolveSitemapArticleGroups(ctx context.Context, arts []domain.RawArticle, fetchCrossShard bool) map[string][]domain.RawArticle {
	groups := make(map[string][]domain.RawArticle)
	if !fetchCrossShard {
		for _, a := range arts {
			if s.guard.IsPublicIndexable(a) && strings.TrimSpace(a.TranslationGroup) != "" {
				groups[a.TranslationGroup] = append(groups[a.TranslationGroup], a)
			}
		}
		return groups
	}
	seen := make(map[string]struct{})
	var groupNames []string
	for _, a := range arts {
		g := strings.TrimSpace(a.TranslationGroup)
		if g != "" && s.guard.IsPublicIndexable(a) {
			if _, ok := seen[g]; !ok {
				seen[g] = struct{}{}
				groupNames = append(groupNames, g)
			}
		}
	}
	const batch = 200
	for start := 0; start < len(groupNames); start += batch {
		end := start + batch
		if end > len(groupNames) {
			end = len(groupNames)
		}
		chunk := groupNames[start:end]
		ph := make([]string, len(chunk))
		qArgs := make([]any, 0, 1+len(chunk))
		qArgs = append(qArgs, domain.ArticleStatusPublished)
		for i, g := range chunk {
			ph[i] = "?"
			qArgs = append(qArgs, g)
		}
		var peers []domain.RawArticle
		err := sqlx.SelectInto(ctx, s.store, &peers,
			`SELECT id, slug, lang, translation_group, status, password_ciphertext
			 FROM articles
			 WHERE status = ? AND password_ciphertext = '' AND translation_group IN (`+strings.Join(ph, ",")+`)
			 ORDER BY published_at DESC`,
			qArgs...,
		)
		if err == nil {
			for _, pr := range peers {
				if s.guard.IsPublicIndexable(pr) && strings.TrimSpace(pr.TranslationGroup) != "" {
					groups[pr.TranslationGroup] = append(groups[pr.TranslationGroup], pr)
				}
			}
		}
	}
	return groups
}

func (s *Service) resolveSitemapPageGroups(ctx context.Context, pages []domain.Page, fetchCrossShard bool) map[string][]domain.Page {
	groups := make(map[string][]domain.Page)
	if !fetchCrossShard {
		for _, p := range pages {
			if strings.TrimSpace(p.TranslationGroup) != "" {
				groups[p.TranslationGroup] = append(groups[p.TranslationGroup], p)
			}
		}
		return groups
	}
	seen := make(map[string]struct{})
	var groupNames []string
	for _, p := range pages {
		g := strings.TrimSpace(p.TranslationGroup)
		if g != "" {
			if _, ok := seen[g]; !ok {
				seen[g] = struct{}{}
				groupNames = append(groupNames, g)
			}
		}
	}
	const batch = 200
	for start := 0; start < len(groupNames); start += batch {
		end := start + batch
		if end > len(groupNames) {
			end = len(groupNames)
		}
		chunk := groupNames[start:end]
		ph := make([]string, len(chunk))
		qArgs := make([]any, 0, 1+len(chunk))
		qArgs = append(qArgs, true)
		for i, g := range chunk {
			ph[i] = "?"
			qArgs = append(qArgs, g)
		}
		var peers []domain.Page
		err := sqlx.SelectInto(ctx, s.store, &peers,
			`SELECT id, slug, lang, translation_group FROM pages
			 WHERE is_published = ? AND translation_group IN (`+strings.Join(ph, ",")+`)
			 ORDER BY slug ASC`,
			qArgs...,
		)
		if err == nil {
			for _, pr := range peers {
				if strings.TrimSpace(pr.TranslationGroup) != "" {
					groups[pr.TranslationGroup] = append(groups[pr.TranslationGroup], pr)
				}
			}
		}
	}
	return groups
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
		err = sqlx.SelectInto(ctx, s.store, &arts, artSQL,
			append([]any{domain.ArticleStatusPublished}, artArgs...)...,
		)
		if err != nil {
			return nil, err
		}
		pgSQL, pgArgs := s.store.Dialect().PaginateSQL(
			`SELECT id, title, slug FROM pages WHERE is_published = ? ORDER BY slug ASC`,
			500, 0,
		)
		_ = sqlx.SelectInto(ctx, s.store, &pages, pgSQL,
			append([]any{true}, pgArgs...)...,
		)
	} else {
		artSQL, artArgs := s.store.Dialect().PaginateSQL(
			`SELECT * FROM articles
			 WHERE status = ? AND password_ciphertext = ''
			 ORDER BY is_pinned DESC, published_at DESC`,
			200, 0,
		)
		err = sqlx.SelectInto(ctx, s.store, &arts, artSQL,
			append([]any{domain.ArticleStatusPublished}, artArgs...)...,
		)
		if err != nil {
			return nil, err
		}
		pgSQL, pgArgs := s.store.Dialect().PaginateSQL(
			`SELECT * FROM pages WHERE is_published = ? ORDER BY slug ASC`,
			100, 0,
		)
		_ = sqlx.SelectInto(ctx, s.store, &pages, pgSQL,
			append([]any{true}, pgArgs...)...,
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
	return s.BuildGitHubStatsSVGWithTheme(username, "auto")
}

// BuildGitHubStatsSVGWithTheme generates a GitHub stats SVG card supporting "auto"/"" (CSS custom properties + prefers-color-scheme), "dark", or "light".
func (s *Service) BuildGitHubStatsSVGWithTheme(username string, themeMode string) []byte {
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

	mode := strings.ToLower(strings.TrimSpace(themeMode))
	lightVars := `--gh-bg:#f6f7f3;--gh-border:#dfe6de;--gh-title:#293d36;--gh-label:#60756a;--gh-val:#246b51`
	darkVars := `--gh-bg:#141b18;--gh-border:#27352f;--gh-title:#e7efe9;--gh-label:#93a89e;--gh-val:#5ec299`
	classRules := `.gh-card{fill:var(--gh-bg);stroke:var(--gh-border)}.gh-line{stroke:var(--gh-border)}.gh-title{fill:var(--gh-title)}.gh-label{fill:var(--gh-label)}.gh-val{fill:var(--gh-val)}`

	var styleCSS string
	switch mode {
	case "dark":
		styleCSS = `:root{color-scheme:dark;` + darkVars + `}` + classRules
	case "light":
		styleCSS = `:root{color-scheme:light;` + lightVars + `}` + classRules
	default:
		styleCSS = `:root{color-scheme:light dark;` + lightVars + `}@media (prefers-color-scheme: dark){:root{` + darkVars + `}}` + classRules
	}

	svg := fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" width="440" height="148" viewBox="0 0 440 148" role="img" aria-label="GitHub Stats for %s">
  <style>%s</style>
  <rect class="gh-card" x="1" y="1" width="438" height="146" rx="14" stroke-width="1.5"/>
  <text class="gh-title" x="24" y="38" font-family="-apple-system,BlinkMacSystemFont,'Segoe UI',sans-serif" font-size="16" font-weight="600">@%s · GitHub Open Source</text>
  <line class="gh-line" x1="24" y1="54" x2="416" y2="54" stroke-width="1"/>
  <g font-family="-apple-system,BlinkMacSystemFont,'Segoe UI',sans-serif">
    <text class="gh-label" x="24" y="84" font-size="11">PUBLIC REPOS</text>
    <text class="gh-val" x="24" y="114" font-size="22" font-weight="700">%d</text>
    <text class="gh-label" x="172" y="84" font-size="11">TOTAL STARS</text>
    <text class="gh-val" x="172" y="114" font-size="22" font-weight="700">%d</text>
    <text class="gh-label" x="312" y="84" font-size="11">TOTAL FORKS</text>
    <text class="gh-val" x="312" y="114" font-size="22" font-weight="700">%d</text>
  </g>
</svg>`, html.EscapeString(user), styleCSS, html.EscapeString(user), totalRepos, totalStars, totalForks)
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
