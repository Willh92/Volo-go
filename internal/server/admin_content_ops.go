package server

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Willh92/Volo-go/internal/db/sqlx"
	"github.com/Willh92/Volo-go/internal/domain"
	"github.com/Willh92/Volo-go/internal/render"
	"github.com/Willh92/Volo-go/internal/visibility"
	"github.com/Willh92/Volo-go/pkg/id"
	"github.com/Willh92/Volo-go/pkg/plugin"
)

const (
	maxBatchImportArchiveBytes      = 32 << 20 // 32 MiB upload archive cap
	maxBatchImportUncompressedBytes = 64 << 20 // 64 MiB Zip Bomb uncompressed cap
	maxBatchImportEntries           = 500
)

var nonSlugCharsRe = regexp.MustCompile(`[^\p{L}\p{N}_-]+`)

// --- Batch Markdown / ZIP Importer (with Zip Bomb & Path Traversal Defense) ---

type markdownImportDoc struct {
	Filename string
	Content  string
}

type parsedFrontmatter struct {
	Title    string
	Slug     string
	DateMS   int64
	Category string
	Tags     []string
	Summary  string
	Draft    bool
	Lang     string
	Cover    string
	BodyMD   string
}

func (s *AppServer) ensureTempDir() string {
	base := ""
	if s != nil && s.deps.Config != nil {
		base = strings.TrimSpace(s.deps.Config.DataDir)
	}
	if base == "" {
		return ""
	}
	tmpDir := filepath.Join(base, "tmp")
	if err := os.MkdirAll(tmpDir, 0o700); err != nil {
		return base
	}
	return tmpDir
}

func (s *AppServer) handleAdminBatchImportArticles(w http.ResponseWriter, r *http.Request, actor *domain.User) {
	files, _, err := readMultipartInMemoryFiles(w, r, maxBatchImportArchiveBytes, maxBatchImportArchiveBytes, maxBatchImportEntries)
	if err != nil {
		writeErrJSON(w, http.StatusBadRequest, "upload exceeds 32MB limit or invalid multipart form")
		return
	}
	if len(files) == 0 {
		writeErrJSON(w, http.StatusBadRequest, "no .md or .zip files uploaded")
		return
	}

	var docs []markdownImportDoc
	var totalBytes int64

	for _, uf := range files {
		if len(docs) >= maxBatchImportEntries {
			writeErrJSON(w, http.StatusBadRequest, "too many files in batch import (max 500)")
			return
		}
		ext := strings.ToLower(filepath.Ext(uf.Filename))
		switch ext {
		case ".md", ".markdown":
			if totalBytes+int64(len(uf.Data)) > maxBatchImportUncompressedBytes {
				writeErrJSON(w, http.StatusBadRequest, "uncompressed size exceeds 64MB batch import budget")
				return
			}
			totalBytes += int64(len(uf.Data))
			docs = append(docs, markdownImportDoc{Filename: filepath.Base(uf.Filename), Content: string(uf.Data)})

		case ".zip":
			if int64(len(uf.Data)) > maxBatchImportArchiveBytes {
				writeErrJSON(w, http.StatusBadRequest, "ZIP archive exceeds 32MB")
				return
			}
			extracted, used, zErr := extractMarkdownDocsFromZipBytes(uf.Data, maxBatchImportUncompressedBytes-totalBytes, maxBatchImportEntries-len(docs))
			if zErr != nil {
				writeErrJSON(w, http.StatusBadRequest, zErr.Error())
				return
			}
			totalBytes += used
			docs = append(docs, extracted...)

		default:
			writeErrJSON(w, http.StatusBadRequest, "unsupported file extension: "+ext)
			return
		}
	}

	if len(docs) == 0 {
		writeErrJSON(w, http.StatusBadRequest, "no valid Markdown documents found")
		return
	}

	importedSlugs := make([]string, 0, len(docs))
	defaultLocale := s.deps.Settings.Snapshot().Site.DefaultLocale
	now := time.Now().UnixMilli()

	for _, doc := range docs {
		fm := parseMarkdownFrontmatter(doc.Filename, doc.Content)
		if fm.Lang == "" {
			fm.Lang = defaultLocale
		}
		pubAt := fm.DateMS
		if pubAt <= 0 {
			pubAt = now
		}
		status := domain.ArticleStatusPublished
		if fm.Draft {
			status = domain.ArticleStatusDraft
		}
		slug := sanitizeSlug(fm.Slug)
		if slug == "" {
			slug = sanitizeSlug(fm.Title)
		}
		slug = s.ensureUniqueArticleSlug(r.Context(), slug, "")
		words, mins := computeReadingMetrics(fm.BodyMD)

		var cleanCover string
		if rawCover := strings.TrimSpace(fm.Cover); rawCover != "" {
			if u, ok := sanitizeRedirectTargetURL(rawCover); ok {
				cleanCover = u
			}
		}

		var savedRaw domain.RawArticle
		err := s.deps.Store.WithTx(r.Context(), func(tx *sqlx.Tx) error {
			var catID string
			if actor.Role == domain.RoleAdmin {
				var cErr error
				catID, cErr = s.resolveOrCreateCategoryTx(r.Context(), tx, fm.Category, now)
				if cErr != nil {
					return cErr
				}
			} else if cleanCat := strings.TrimSpace(fm.Category); cleanCat != "" {
				catSlug := sanitizeSlug(cleanCat)
				if cat, gErr := sqlx.Get[domain.Category](r.Context(), tx, `SELECT id FROM categories WHERE LOWER(name) = LOWER(?) OR slug = ?`, cleanCat, catSlug); gErr == nil {
					catID = cat.ID
				}
			}
			savedRaw = domain.RawArticle{
				ID:             id.Next(),
				AuthorID:       actor.ID,
				CategoryID:     catID,
				Title:          fm.Title,
				Slug:           slug,
				SummaryMD:      fm.Summary,
				ContentMD:      fm.BodyMD,
				CoverURL:       cleanCover,
				Status:         status,
				PublishedAt:    pubAt,
				AllowComment:   true,
				HasContentLock: render.ContainsLockDirective(fm.BodyMD),
				Lang:           fm.Lang,
				WordCount:      words,
				ReadingMinutes: mins,
				CreatedAt:      now,
				UpdatedAt:      now,
			}
			if _, iErr := tx.ExecContext(r.Context(),
				`INSERT INTO articles (
					id, author_id, category_id, title, slug, legacy_permalink, summary_md, content_md,
					cover_url, status, published_at, scheduled_at, is_pinned, allow_comment,
					password_ciphertext, has_content_lock, audio_url, audio_duration_sec, audio_size_bytes, audio_mime,
					lang, translation_group, license_override, word_count, reading_minutes, view_count, comment_count,
					created_at, updated_at
				) VALUES (?, ?, ?, ?, ?, '', ?, ?, ?, ?, ?, 0, ?, ?, '', ?, '', 0, 0, '', ?, '', '', ?, ?, 0, 0, ?, ?)`,
				savedRaw.ID, savedRaw.AuthorID, savedRaw.CategoryID, savedRaw.Title, savedRaw.Slug,
				savedRaw.SummaryMD, savedRaw.ContentMD, savedRaw.CoverURL, savedRaw.Status, savedRaw.PublishedAt,
				false, true, savedRaw.HasContentLock, savedRaw.Lang, savedRaw.WordCount, savedRaw.ReadingMinutes,
				savedRaw.CreatedAt, savedRaw.UpdatedAt,
			); iErr != nil {
				return iErr
			}
			if err := s.bindArticleTagsByNamesTx(r.Context(), tx, savedRaw.ID, fm.Tags, now); err != nil {
				return err
			}
			return s.deps.Content.ReindexArticleBacklinksTx(r.Context(), tx, savedRaw)
		})
		if err != nil {
			writeErrJSON(w, http.StatusInternalServerError, err.Error())
			return
		}
		if savedRaw.Status == domain.ArticleStatusPublished {
			s.deps.Content.SearchIndex().Upsert(savedRaw)
			if s.deps.Plugins != nil {
				s.deps.Plugins.DispatchArticleHook(r.Context(), plugin.HookArticlePublished, articleHookPayloadFromRaw(savedRaw))
			}
			if s.deps.NotifyArticlePublished != nil {
				s.deps.NotifyArticlePublished(r.Context(), savedRaw)
			}
		}
		importedSlugs = append(importedSlugs, savedRaw.Slug)
	}

	s.invalidateMediaRefCache()
	s.recordAuditLog(r, actor, "article.batch_import", "article", fmt.Sprintf("count=%d", len(importedSlugs)), map[string]any{"slugs": importedSlugs})
	writeJSON(w, http.StatusOK, map[string]any{
		"imported_count": len(importedSlugs),
		"slugs":          importedSlugs,
	})
}

func extractMarkdownDocsFromZipBytes(zipData []byte, maxTotalBytes int64, maxFiles int) ([]markdownImportDoc, int64, error) {
	zr, err := zip.NewReader(bytes.NewReader(zipData), int64(len(zipData)))
	if err != nil {
		return nil, 0, fmt.Errorf("invalid zip archive: %w", err)
	}

	if len(zr.File) > maxBatchImportEntries*2 {
		return nil, 0, errors.New("zip archive contains too many entries")
	}

	var docs []markdownImportDoc
	var used int64

	for _, zf := range zr.File {
		name := strings.ReplaceAll(zf.Name, "\\", "/")
		if strings.HasPrefix(name, "__MACOSX/") || strings.Contains(name, "/.") || strings.HasPrefix(name, ".") {
			continue
		}
		// Reject symlinks and path traversal (`..` or leading `/`)
		if zf.Mode()&os.ModeSymlink != 0 {
			return nil, 0, errors.New("zip archive contains forbidden symbolic link")
		}
		cleaned := path.Clean(strings.TrimPrefix(name, "/"))
		if strings.HasPrefix(cleaned, "..") || strings.Contains(cleaned, "../") || strings.Contains(name, "..") {
			return nil, 0, errors.New("zip archive contains path traversal sequence")
		}
		if zf.FileInfo().IsDir() {
			continue
		}
		ext := strings.ToLower(path.Ext(cleaned))
		if ext != ".md" && ext != ".markdown" {
			continue
		}
		if len(docs) >= maxFiles {
			return nil, 0, errors.New("zip archive exceeds maximum Markdown file count (500)")
		}
		if zf.UncompressedSize64 > uint64(maxBatchImportUncompressedBytes) {
			return nil, 0, errors.New("zip bomb detected: declared uncompressed size exceeds 64MB limit")
		}
		rc, oErr := zf.Open()
		if oErr != nil {
			return nil, 0, oErr
		}
		raw, rErr := readBoundedStream(rc, maxTotalBytes-used)
		_ = rc.Close()
		if rErr != nil {
			return nil, 0, rErr
		}
		used += int64(len(raw))
		docs = append(docs, markdownImportDoc{
			Filename: path.Base(cleaned),
			Content:  string(raw),
		})
	}
	return docs, used, nil
}

func readBoundedStream(r io.Reader, maxBytes int64) ([]byte, error) {
	if maxBytes <= 0 {
		return nil, errors.New("uncompressed size exceeds 64MB batch import budget")
	}
	buf, err := io.ReadAll(io.LimitReader(r, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(buf)) > maxBytes {
		return nil, errors.New("zip bomb or oversized Markdown stream exceeds 64MB limit")
	}
	return buf, nil
}

func parseMarkdownFrontmatter(filename string, raw string) parsedFrontmatter {
	norm := strings.ReplaceAll(strings.TrimPrefix(raw, "\uFEFF"), "\r\n", "\n")
	baseTitle := strings.TrimSuffix(filename, filepath.Ext(filename))
	out := parsedFrontmatter{
		Title:  baseTitle,
		BodyMD: norm,
	}

	if strings.HasPrefix(norm, "---\n") {
		if endIdx := strings.Index(norm[4:], "\n---\n"); endIdx >= 0 {
			fmBlock := norm[4 : 4+endIdx]
			out.BodyMD = strings.TrimSpace(norm[4+endIdx+5:])
			var currentListKey string
			for _, line := range strings.Split(fmBlock, "\n") {
				trimmed := strings.TrimSpace(line)
				if trimmed == "" || strings.HasPrefix(trimmed, "#") {
					continue
				}
				if strings.HasPrefix(trimmed, "- ") && currentListKey == "tags" {
					val := trimYAMLQuotes(strings.TrimSpace(strings.TrimPrefix(trimmed, "- ")))
					if val != "" {
						out.Tags = append(out.Tags, val)
					}
					continue
				}
				colon := strings.IndexByte(line, ':')
				if colon <= 0 {
					continue
				}
				key := strings.ToLower(strings.TrimSpace(line[:colon]))
				valRaw := strings.TrimSpace(line[colon+1:])
				currentListKey = ""
				if valRaw == "" {
					currentListKey = key
					continue
				}
				val := trimYAMLQuotes(valRaw)
				switch key {
				case "title":
					if val != "" {
						out.Title = val
					}
				case "slug", "url":
					out.Slug = strings.Trim(val, "/")
				case "category", "categories":
					out.Category = strings.Trim(strings.Split(strings.Trim(valRaw, "[]"), ",")[0], ` "'`)
				case "tags":
					for _, p := range strings.Split(strings.Trim(valRaw, "[]"), ",") {
						tv := trimYAMLQuotes(strings.TrimSpace(p))
						if tv != "" {
							out.Tags = append(out.Tags, tv)
						}
					}
				case "summary", "description", "excerpt":
					out.Summary = val
				case "draft":
					out.Draft = strings.EqualFold(val, "true") || val == "1"
				case "lang", "locale":
					out.Lang = val
				case "cover", "image":
					out.Cover = val
				case "date", "published", "created":
					if ts := parseFlexibleTimeMS(val); ts > 0 {
						out.DateMS = ts
					}
				}
			}
		}
	}

	// Fallback title from first H1 if filename was generic
	if out.Title == "" || strings.EqualFold(out.Title, "index") || strings.EqualFold(out.Title, "readme") {
		for _, line := range strings.Split(out.BodyMD, "\n") {
			t := strings.TrimSpace(line)
			if strings.HasPrefix(t, "# ") {
				out.Title = strings.TrimSpace(strings.TrimPrefix(t, "# "))
				break
			}
		}
	}
	if out.Title == "" {
		out.Title = "Untitled"
	}
	return out
}

func trimYAMLQuotes(s string) string {
	if len(s) >= 2 && ((s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'')) {
		return s[1 : len(s)-1]
	}
	return s
}

func parseFlexibleTimeMS(raw string) int64 {
	layouts := []string{
		time.RFC3339,
		"2006-01-02 15:04:05",
		"2006-01-02 15:04",
		"2006-01-02",
		time.RFC1123Z,
		time.RFC1123,
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, strings.TrimSpace(raw)); err == nil {
			return t.UnixMilli()
		}
	}
	return 0
}

func sanitizeSlug(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	s = strings.Trim(s, "/")
	if idx := strings.LastIndex(s, "/"); idx >= 0 {
		s = s[idx+1:]
	}
	s = strings.TrimSuffix(s, ".html")
	s = strings.TrimSuffix(s, ".md")
	s = nonSlugCharsRe.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if utf8.RuneCountInString(s) > 80 {
		r := []rune(s)
		s = strings.Trim(string(r[:80]), "-")
	}
	if s == "" {
		s = "post-" + strings.ToLower(id.Next())[:8]
	}
	return s
}

func computeReadingMetrics(md string) (words int, minutes int) {
	stripped := strings.TrimSpace(md)
	if stripped == "" {
		return 0, 1
	}
	for _, r := range stripped {
		if r > 127 {
			words++
		}
	}
	asciiFields := len(strings.Fields(stripped))
	if asciiFields > words/2 {
		words += asciiFields / 2
	}
	if words < 1 {
		words = utf8.RuneCountInString(stripped)
	}
	minutes = (words + 299) / 300
	if minutes < 1 {
		minutes = 1
	}
	return words, minutes
}

// --- Friend Link Concurrent SSRF-Safe Health Patrol ---

func (s *AppServer) newLinkPatrolHTTPClient() *http.Client {
	client := &http.Client{Timeout: 8 * time.Second}
	if s.deps.OutboundDialer != nil {
		client = s.deps.OutboundDialer.NewHTTPClient(8 * time.Second)
	}
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 4 {
			return errors.New("too many redirects")
		}
		if s.deps.OutboundDialer != nil {
			if _, err := s.deps.OutboundDialer.ValidateOutboundURL(req.URL.String()); err != nil {
				return err
			}
		}
		return nil
	}
	return client
}

func (s *AppServer) checkSingleFriendLink(ctx context.Context, lk domain.Link) domain.Link {
	return s.checkSingleFriendLinkWithClient(ctx, s.newLinkPatrolHTTPClient(), lk)
}

func (s *AppServer) checkSingleFriendLinkWithClient(ctx context.Context, client *http.Client, lk domain.Link) domain.Link {
	start := time.Now()
	status := "ok"
	code := 0
	errMsg := ""

	reqCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	req, rErr := http.NewRequestWithContext(reqCtx, http.MethodHead, lk.URL, nil)
	if rErr != nil {
		cancel()
		status = "broken"
		errMsg = rErr.Error()
	} else {
		req.Header.Set("User-Agent", "Volo-go-LinkPatrol/1.0")
		resp, dErr := client.Do(req)
		if dErr != nil || (resp != nil && (resp.StatusCode == http.StatusMethodNotAllowed || resp.StatusCode == http.StatusForbidden)) {
			if resp != nil {
				_ = resp.Body.Close()
			}
			reqGet, gErr := http.NewRequestWithContext(reqCtx, http.MethodGet, lk.URL, nil)
			if gErr == nil {
				reqGet.Header.Set("User-Agent", "Volo-go-LinkPatrol/1.0")
				resp, dErr = client.Do(reqGet)
			}
		}
		if dErr != nil {
			if strings.Contains(strings.ToLower(dErr.Error()), "ssrf") || strings.Contains(strings.ToLower(dErr.Error()), "blocked") {
				status = "blocked"
			} else {
				status = "broken"
			}
			errMsg = dErr.Error()
		} else if resp != nil {
			code = resp.StatusCode
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			_ = resp.Body.Close()
			switch {
			case code >= 200 && code < 300:
				if resp.Request != nil && resp.Request.URL != nil && resp.Request.URL.String() != lk.URL {
					status = "redirect"
				} else {
					status = "ok"
				}
			case code >= 300 && code < 400:
				status = "redirect"
			default:
				status = "broken"
				errMsg = fmt.Sprintf("HTTP %d", code)
			}
		}
		cancel()
	}

	lat := int(time.Since(start).Milliseconds())
	if len(errMsg) > 240 {
		errMsg = errMsg[:240]
	}
	now := time.Now().UnixMilli()
	_, _ = s.deps.Store.ExecContext(ctx,
		`UPDATE links SET health_status = ?, http_code = ?, latency_ms = ?, health_error = ?, last_checked_at = ?, updated_at = ? WHERE id = ?`,
		status, code, lat, errMsg, now, now, lk.ID)
	lk.HealthStatus = status
	lk.HTTPCode = code
	lk.LatencyMS = lat
	lk.HealthError = errMsg
	lk.LastCheckedAt = now
	lk.UpdatedAt = now
	return lk
}

func (s *AppServer) patrolAllFriendLinks(ctx context.Context) (int, error) {
	links, err := sqlx.Select[domain.Link](ctx, s.deps.Store, `SELECT * FROM links ORDER BY sort_order ASC`)
	if err != nil {
		return 0, err
	}
	if len(links) == 0 {
		return 0, nil
	}

	client := s.newLinkPatrolHTTPClient()
	sem := make(chan struct{}, 6)
	var wg sync.WaitGroup
	for _, lk := range links {
		lk := lk
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			_ = s.checkSingleFriendLinkWithClient(ctx, client, lk)
		}()
	}
	wg.Wait()
	return len(links), nil
}

// --- Follow RSS / Atom Reader Sync (SSRF-Safe) ---

type parsedFeedEntry struct {
	GUID      string
	Title     string
	URL       string
	Summary   string
	Author    string
	Published int64
}

func (s *AppServer) newFollowSyncHTTPClient() *http.Client {
	if s.deps.OutboundDialer != nil {
		return s.deps.OutboundDialer.NewHTTPClient(10 * time.Second)
	}
	return &http.Client{Timeout: 10 * time.Second}
}

func (s *AppServer) syncSingleFollowFeed(ctx context.Context, fw domain.Follow) (int, domain.Follow, error) {
	return s.syncSingleFollowFeedWithClient(ctx, s.newFollowSyncHTTPClient(), fw)
}

func (s *AppServer) syncSingleFollowFeedWithClient(ctx context.Context, client *http.Client, fw domain.Follow) (int, domain.Follow, error) {
	now := time.Now().UnixMilli()
	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	recordErr := func(rawErr string) (int, domain.Follow, error) {
		msg := strings.TrimSpace(rawErr)
		if len(msg) > 250 {
			msg = msg[:250]
		}
		_, _ = s.deps.Store.ExecContext(ctx, `UPDATE follows SET last_synced_at = ?, sync_error = ? WHERE id = ?`, now, msg, fw.ID)
		fw.LastSyncedAt = now
		fw.SyncError = msg
		return 0, fw, errors.New(msg)
	}

	req, rErr := http.NewRequestWithContext(reqCtx, http.MethodGet, strings.TrimSpace(fw.FeedURL), nil)
	if rErr != nil {
		return recordErr(rErr.Error())
	}
	req.Header.Set("User-Agent", "Volo-go-FollowReader/1.0")
	resp, dErr := client.Do(req)
	if dErr != nil {
		return recordErr(dErr.Error())
	}
	rawXML, readErr := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	_ = resp.Body.Close()
	if readErr != nil || resp.StatusCode >= 400 {
		msg := fmt.Sprintf("HTTP %d", resp.StatusCode)
		if readErr != nil {
			msg = readErr.Error()
		}
		return recordErr(msg)
	}

	entries, pErr := parseRSSOrAtomFeed(rawXML)
	if pErr != nil {
		return recordErr(pErr.Error())
	}

	upsert := s.deps.Store.Dialect().UpsertSQL(
		"follow_items",
		[]string{"id", "follow_id", "guid_hash", "title", "url", "summary", "author_name", "published_at"},
		[]string{"follow_id", "guid_hash"},
		[]string{"title", "url", "summary", "author_name", "published_at"},
	)
	count := 0
	for _, ent := range entries {
		if count >= 25 {
			break
		}
		if ent.Title == "" || ent.URL == "" {
			continue
		}
		uParsed, uErr := url.Parse(ent.URL)
		if uErr != nil || (uParsed.Scheme != "http" && uParsed.Scheme != "https") {
			continue
		}
		sum := sha256.Sum256([]byte(ent.GUID))
		guidHash := hex.EncodeToString(sum[:16])
		pub := ent.Published
		if pub <= 0 {
			pub = now
		}
		_, _ = s.deps.Store.ExecContext(ctx, upsert,
			id.Next(), fw.ID, guidHash, ent.Title, ent.URL, truncateRunes(ent.Summary, 260), ent.Author, pub)
		count++
	}
	pruneSQL, prunePageArgs := s.deps.Store.Dialect().PaginateSQL(
		`SELECT id FROM follow_items WHERE follow_id = ? ORDER BY published_at DESC`, 1000, 30)
	pruneArgs := make([]any, 0, 1+len(prunePageArgs))
	pruneArgs = append(pruneArgs, fw.ID)
	pruneArgs = append(pruneArgs, prunePageArgs...)
	staleRows, _ := sqlx.Select[struct {
		ID string `db:"id"`
	}](ctx, s.deps.Store, pruneSQL, pruneArgs...)
	if len(staleRows) > 0 {
		ph := make([]string, len(staleRows))
		delArgs := make([]any, len(staleRows))
		for i, sr := range staleRows {
			ph[i] = "?"
			delArgs[i] = sr.ID
		}
		_, _ = s.deps.Store.ExecContext(ctx,
			`DELETE FROM follow_items WHERE id IN (`+strings.Join(ph, ",")+`)`, delArgs...)
	}
	_, _ = s.deps.Store.ExecContext(ctx, `UPDATE follows SET last_synced_at = ?, sync_error = '', updated_at = ? WHERE id = ?`, now, now, fw.ID)
	fw.LastSyncedAt = now
	fw.SyncError = ""
	fw.UpdatedAt = now
	return count, fw, nil
}

func (s *AppServer) syncAllFollowFeeds(ctx context.Context) (int, error) {
	follows, err := sqlx.Select[domain.Follow](ctx, s.deps.Store, `SELECT * FROM follows ORDER BY sort_order ASC`)
	if err != nil {
		return 0, err
	}
	if len(follows) == 0 {
		return 0, nil
	}

	client := s.newFollowSyncHTTPClient()
	totalItems := 0
	var mu sync.Mutex
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup

	for _, fw := range follows {
		fw := fw
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			count, _, syncErr := s.syncSingleFollowFeedWithClient(ctx, client, fw)
			if syncErr == nil && count > 0 {
				mu.Lock()
				totalItems += count
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return totalItems, nil
}

func parseRSSOrAtomFeed(raw []byte) ([]parsedFeedEntry, error) {
	type rssItem struct {
		Title       string `xml:"title"`
		Link        string `xml:"link"`
		GUID        string `xml:"guid"`
		Description string `xml:"description"`
		Author      string `xml:"author"`
		Creator     string `xml:"creator"`
		PubDate     string `xml:"pubDate"`
	}
	type rssDoc struct {
		Channel struct {
			Items []rssItem `xml:"item"`
		} `xml:"channel"`
	}
	var rDoc rssDoc
	if xml.Unmarshal(raw, &rDoc) == nil && len(rDoc.Channel.Items) > 0 {
		out := make([]parsedFeedEntry, 0, len(rDoc.Channel.Items))
		for _, it := range rDoc.Channel.Items {
			guid := strings.TrimSpace(it.GUID)
			if guid == "" {
				guid = strings.TrimSpace(it.Link)
			}
			author := strings.TrimSpace(it.Author)
			if author == "" {
				author = strings.TrimSpace(it.Creator)
			}
			out = append(out, parsedFeedEntry{
				GUID:      guid,
				Title:     strings.TrimSpace(it.Title),
				URL:       strings.TrimSpace(it.Link),
				Summary:   stripHTMLTagsSimple(it.Description),
				Author:    author,
				Published: parseFlexibleTimeMS(it.PubDate),
			})
		}
		return out, nil
	}

	type atomLink struct {
		Href string `xml:"href,attr"`
		Rel  string `xml:"rel,attr"`
	}
	type atomEntry struct {
		ID      string     `xml:"id"`
		Title   string     `xml:"title"`
		Links   []atomLink `xml:"link"`
		Summary string     `xml:"summary"`
		Content string     `xml:"content"`
		Updated string     `xml:"updated"`
		Author  struct {
			Name string `xml:"name"`
		} `xml:"author"`
	}
	type atomFeed struct {
		Entries []atomEntry `xml:"entry"`
	}
	var aDoc atomFeed
	if err := xml.Unmarshal(raw, &aDoc); err != nil {
		return nil, fmt.Errorf("unrecognized RSS/Atom XML: %w", err)
	}
	out := make([]parsedFeedEntry, 0, len(aDoc.Entries))
	for _, ent := range aDoc.Entries {
		href := ""
		for _, lk := range ent.Links {
			if lk.Href != "" && (lk.Rel == "" || lk.Rel == "alternate") {
				href = strings.TrimSpace(lk.Href)
				break
			}
		}
		guid := strings.TrimSpace(ent.ID)
		if guid == "" {
			guid = href
		}
		summary := ent.Summary
		if summary == "" {
			summary = ent.Content
		}
		out = append(out, parsedFeedEntry{
			GUID:      guid,
			Title:     strings.TrimSpace(ent.Title),
			URL:       href,
			Summary:   stripHTMLTagsSimple(summary),
			Author:    strings.TrimSpace(ent.Author.Name),
			Published: parseFlexibleTimeMS(ent.Updated),
		})
	}
	return out, nil
}

func stripHTMLTagsSimple(s string) string {
	var b strings.Builder
	inTag := false
	for _, r := range s {
		if r == '<' {
			inTag = true
			continue
		}
		if r == '>' {
			inTag = false
			b.WriteByte(' ')
			continue
		}
		if !inTag {
			b.WriteRune(r)
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

func truncateRunes(s string, max int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= max {
		return string(r)
	}
	return string(r[:max]) + "…"
}

// --- GitHub Public Repositories Showcase Sync & IndexNow Push ---

func (s *AppServer) refreshGitHubRepositories(ctx context.Context, usernameOverride string) ([]domain.GitHubRepoItem, error) {
	seo := s.deps.Settings.Snapshot().SEO
	uname := strings.TrimSpace(usernameOverride)
	if uname == "" {
		uname = strings.TrimSpace(seo.GitHubUsername)
	}
	if uname == "" {
		return nil, errors.New("github_username is not configured")
	}

	type ghRawRepo struct {
		Name            string `json:"name"`
		FullName        string `json:"full_name"`
		Description     string `json:"description"`
		HTMLURL         string `json:"html_url"`
		Homepage        string `json:"homepage"`
		Language        string `json:"language"`
		StargazersCount int    `json:"stargazers_count"`
		ForksCount      int    `json:"forks_count"`
		Fork            bool   `json:"fork"`
		UpdatedAt       string `json:"updated_at"`
	}

	client := s.deps.OutboundDialer.NewHTTPClient(12 * time.Second)
	var allRaw []ghRawRepo
	for page := 1; page <= 5; page++ {
		apiURL := fmt.Sprintf("https://api.github.com/users/%s/repos?per_page=100&page=%d&sort=updated", url.PathEscape(uname), page)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("User-Agent", "Volo-go-Showcase/1.0")

		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			_ = resp.Body.Close()
			return nil, fmt.Errorf("GitHub API returned HTTP %d", resp.StatusCode)
		}
		var batch []ghRawRepo
		decErr := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&batch)
		_ = resp.Body.Close()
		if decErr != nil {
			return nil, decErr
		}
		allRaw = append(allRaw, batch...)
		if len(batch) < 100 {
			break
		}
	}

	hiddenSet := make(map[string]bool)
	for _, existing := range seo.GitHubRepos {
		if existing.Hidden {
			if k := strings.ToLower(strings.TrimSpace(existing.FullName)); k != "" {
				hiddenSet[k] = true
			}
			if k := strings.ToLower(strings.TrimSpace(existing.Name)); k != "" {
				hiddenSet[k] = true
			}
		}
	}

	seen := make(map[string]bool, len(allRaw))
	items := make([]domain.GitHubRepoItem, 0, len(allRaw))
	for _, r := range allRaw {
		fullLower := strings.ToLower(strings.TrimSpace(r.FullName))
		nameLower := strings.ToLower(strings.TrimSpace(r.Name))
		if nameLower == "" || seen[fullLower] {
			continue
		}
		seen[fullLower] = true
		items = append(items, domain.GitHubRepoItem{
			Name:        r.Name,
			FullName:    r.FullName,
			Description: r.Description,
			HTMLURL:     r.HTMLURL,
			Homepage:    r.Homepage,
			Language:    r.Language,
			Stars:       r.StargazersCount,
			Forks:       r.ForksCount,
			IsFork:      r.Fork,
			Hidden:      hiddenSet[fullLower] || hiddenSet[nameLower],
			UpdatedAt:   r.UpdatedAt,
		})
	}

	sort.Slice(items, func(i, j int) bool {
		if items[i].Stars != items[j].Stars {
			return items[i].Stars > items[j].Stars
		}
		if items[i].Forks != items[j].Forks {
			return items[i].Forks > items[j].Forks
		}
		return strings.ToLower(items[i].FullName) < strings.ToLower(items[j].FullName)
	})

	patchBytes, _ := json.Marshal(map[string]any{
		"github_username":  uname,
		"github_repos":     items,
		"github_synced_at": time.Now().UnixMilli(),
	})
	_, _, err := s.deps.Settings.PatchDomain(ctx, domain.SettingsDomainSEO, patchBytes, 0)
	return items, err
}

func (s *AppServer) publishGitHubShowcaseArticle(ctx context.Context, actor *domain.User) (*domain.RawArticle, error) {
	snap := s.deps.Settings.Snapshot()
	seo := snap.SEO
	uname := strings.TrimSpace(seo.GitHubUsername)
	if uname == "" {
		return nil, errors.New("github_username is not configured")
	}

	locLower := strings.ToLower(strings.TrimSpace(snap.Site.DefaultLocale))
	isEN := strings.HasPrefix(locLower, "en")
	isZHTW := strings.HasPrefix(locLower, "zh-tw") || strings.HasPrefix(locLower, "zh-hk") || strings.HasPrefix(locLower, "zh-hant")

	visible := make([]domain.GitHubRepoItem, 0, len(seo.GitHubRepos))
	totalStars := 0
	totalForks := 0
	for _, rp := range seo.GitHubRepos {
		if rp.Hidden {
			continue
		}
		if rp.IsFork && !seo.GitHubIncludeForks {
			continue
		}
		visible = append(visible, rp)
		totalStars += rp.Stars
		totalForks += rp.Forks
	}

	var (
		artTitle   string
		summaryStr string
		introLine  string
		emptyLine  string
		tblHeader  string
		siteBadge  string
		tagNames   []string
		navTitle   string
	)
	switch {
	case isEN:
		artTitle = "My Open Source Projects on GitHub"
		summaryStr = fmt.Sprintf("A curated showcase of %d open-source repositories maintained on GitHub (@%s, %d total Stars).", len(visible), uname, totalStars)
		introLine = fmt.Sprintf("> GitHub Profile: [https://github.com/%s](https://github.com/%s) · Showcasing **%d** open-source repositories (**%d** Stars · **%d** Forks)\n\n",
			uname, uname, len(visible), totalStars, totalForks)
		emptyLine = "_No public repositories are currently selected for display. Sync and select repositories in Admin → Open Source Showcase._\n"
		tblHeader = "| Repository | Description | Stars | Forks | Language |\n"
		siteBadge = "Website ↗"
		tagNames = []string{"Open Source", "GitHub"}
		navTitle = "Open Source"
	case isZHTW:
		artTitle = "我在 GitHub 上的開源專案"
		summaryStr = fmt.Sprintf("彙整我在 GitHub (@%s) 上持續維護的 %d 個開源專案（累計 %d Stars）。", uname, len(visible), totalStars)
		introLine = fmt.Sprintf("> GitHub 主頁：[https://github.com/%s](https://github.com/%s) · 共精選展示 **%d** 個開源倉庫（累計 **%d** Stars · **%d** Forks）\n\n",
			uname, uname, len(visible), totalStars, totalForks)
		emptyLine = "_暫無勾選展示的公開倉庫，請在管理台「開源作品展示」中同步並勾選需要展示的專案。_\n"
		tblHeader = "| 倉庫 | 專案簡介 | Stars | Forks | 程式語言 |\n"
		siteBadge = "站點 ↗"
		tagNames = []string{"開源", "GitHub"}
		navTitle = "我的開源"
	default:
		artTitle = "我在 GitHub 上的开源项目"
		summaryStr = fmt.Sprintf("汇总我在 GitHub (@%s) 上持续维护的 %d 个开源项目（累计 %d Stars）。", uname, len(visible), totalStars)
		introLine = fmt.Sprintf("> GitHub 主页：[https://github.com/%s](https://github.com/%s) · 共精选展示 **%d** 个开源仓库（累计 **%d** Stars · **%d** Forks）\n\n",
			uname, uname, len(visible), totalStars, totalForks)
		emptyLine = "_暂无勾选展示的公开仓库，请在管理台「开源作品展示」中同步并勾选需要展示的项目。_\n"
		tblHeader = "| 仓库 | 项目简介 | Stars | Forks | 编程语言 |\n"
		siteBadge = "站点 ↗"
		tagNames = []string{"开源", "GitHub"}
		navTitle = "我的开源"
	}

	var md strings.Builder
	md.WriteString(fmt.Sprintf("![%s GitHub Stats](/github-stats/%s.svg)\n\n", uname, url.PathEscape(uname)))
	md.WriteString(introLine)

	if len(visible) == 0 {
		md.WriteString(emptyLine)
	} else {
		md.WriteString(tblHeader)
		md.WriteString("| :--- | :--- | ---: | ---: | :--- |\n")
		for _, rp := range visible {
			lang := strings.TrimSpace(rp.Language)
			if lang == "" {
				lang = "—"
			}
			desc := strings.ReplaceAll(strings.ReplaceAll(strings.TrimSpace(rp.Description), "|", "\\|"), "\n", " ")
			if desc == "" {
				desc = "—"
			}
			if hp := strings.TrimSpace(rp.Homepage); hp != "" && (strings.HasPrefix(hp, "http://") || strings.HasPrefix(hp, "https://")) {
				desc += fmt.Sprintf(" ([%s](%s))", siteBadge, hp)
			}
			repoLabel := rp.Name
			if rp.IsFork {
				repoLabel += " `Fork`"
			}
			repoURL := strings.TrimSpace(rp.HTMLURL)
			if repoURL == "" {
				repoURL = "https://github.com/" + strings.TrimPrefix(rp.FullName, "/")
			}
			md.WriteString(fmt.Sprintf("| [**%s**](%s) | %s | %d | %d | %s |\n",
				repoLabel, repoURL, desc, rp.Stars, rp.Forks, lang))
		}
	}

	contentStr := md.String()
	words, mins := computeReadingMetrics(contentStr)
	now := time.Now().UnixMilli()

	existing, err := sqlx.Get[domain.RawArticle](ctx, s.deps.Store,
		`SELECT * FROM articles WHERE slug IN ('github', 'my-github-repos') OR legacy_permalink IN ('/my-github-repos', '/github') ORDER BY updated_at DESC LIMIT 1`)
	isCreate := err != nil || existing.ID == ""
	wasPublished := !isCreate && existing.Status == domain.ArticleStatusPublished

	authorID := ""
	if actor != nil && actor.ID != "" {
		authorID = actor.ID
	} else {
		_ = s.deps.Store.QueryRowContext(ctx, `SELECT id FROM users WHERE role = ? LIMIT 1`, domain.RoleAdmin).Scan(&authorID)
	}

	var cur domain.RawArticle
	if isCreate {
		cur = domain.RawArticle{
			ID:              id.Next(),
			AuthorID:        authorID,
			Title:           artTitle,
			Slug:            "github",
			LegacyPermalink: "/my-github-repos",
			SummaryMD:       summaryStr,
			ContentMD:       contentStr,
			CoverURL:        "/images/github_repo.jpg",
			Status:          domain.ArticleStatusPublished,
			PublishedAt:     now,
			AllowComment:    true,
			Lang:            snap.Site.DefaultLocale,
			WordCount:       words,
			ReadingMinutes:  mins,
			CreatedAt:       now,
			UpdatedAt:       now,
		}
	} else {
		cur = existing
		cur.Title = artTitle
		cur.SummaryMD = summaryStr
		cur.ContentMD = contentStr
		if strings.TrimSpace(cur.CoverURL) == "" {
			cur.CoverURL = "/images/github_repo.jpg"
		}
		cur.Status = domain.ArticleStatusPublished
		if cur.PublishedAt <= 0 {
			cur.PublishedAt = now
		}
		cur.Lang = snap.Site.DefaultLocale
		cur.WordCount = words
		cur.ReadingMinutes = mins
		cur.UpdatedAt = now
	}

	txErr := s.deps.Store.WithTx(ctx, func(tx *sqlx.Tx) error {
		if isCreate {
			if _, iErr := tx.ExecContext(ctx,
				`INSERT INTO articles (
					id, author_id, category_id, title, slug, legacy_permalink, summary_md, content_md,
					cover_url, status, published_at, scheduled_at, is_pinned, allow_comment,
					password_ciphertext, has_content_lock, audio_url, audio_duration_sec, audio_size_bytes, audio_mime,
					lang, translation_group, license_override, word_count, reading_minutes, view_count, comment_count,
					created_at, updated_at
				) VALUES (?, ?, '', ?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?, '', ?, '', 0, 0, '', ?, '', '', ?, ?, 0, 0, ?, ?)`,
				cur.ID, cur.AuthorID, cur.Title, cur.Slug, cur.LegacyPermalink, cur.SummaryMD, cur.ContentMD,
				cur.CoverURL, cur.Status, cur.PublishedAt, cur.IsPinned, cur.AllowComment, false,
				cur.Lang, cur.WordCount, cur.ReadingMinutes, cur.CreatedAt, cur.UpdatedAt); iErr != nil {
				return iErr
			}
		} else {
			if _, uErr := tx.ExecContext(ctx,
				`UPDATE articles SET title = ?, summary_md = ?, content_md = ?, cover_url = ?, status = ?, published_at = ?, lang = ?, word_count = ?, reading_minutes = ?, updated_at = ? WHERE id = ?`,
				cur.Title, cur.SummaryMD, cur.ContentMD, cur.CoverURL, cur.Status, cur.PublishedAt, cur.Lang, cur.WordCount, cur.ReadingMinutes, cur.UpdatedAt, cur.ID); uErr != nil {
				return uErr
			}
		}
		if bErr := s.bindArticleTagsByNamesTx(ctx, tx, cur.ID, tagNames, now); bErr != nil {
			return bErr
		}
		targetURL := "/posts/" + cur.Slug
		var navCnt int
		_ = tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM nav_items WHERE url IN (?, '/posts/github', '/posts/my-github-repos', '/my-github-repos', '/github')`,
			targetURL).Scan(&navCnt)
		if navCnt == 0 {
			_, _ = tx.ExecContext(ctx,
				`INSERT INTO nav_items (id, parent_id, title, i18n_titles_json, url, open_in_new_tab, icon, sort_order, is_enabled, created_at, updated_at)
				 VALUES (?, '', ?, '{"zh-CN":"我的开源","zh-TW":"我的開源","en-US":"Open Source"}', ?, ?, '/images/github-icon-light.svg', 5, ?, ?, ?)`,
				id.Next(), navTitle, targetURL, false, true, now, now)
		}
		return s.deps.Content.ReindexArticleBacklinksTx(ctx, tx, cur)
	})
	if txErr != nil {
		return nil, txErr
	}
	s.deps.Content.SearchIndex().Upsert(cur)
	_ = s.deps.Content.WarmCaches(ctx)
	s.invalidateMediaRefCache()
	if s.deps.Plugins != nil {
		hookName := plugin.HookArticlePublished
		if wasPublished {
			hookName = plugin.HookArticleUpdated
		}
		s.deps.Plugins.DispatchArticleHook(ctx, hookName, articleHookPayloadFromRaw(cur))
	}
	if !wasPublished && s.deps.NotifyArticlePublished != nil {
		s.deps.NotifyArticlePublished(ctx, cur)
	}
	return &cur, nil
}

func (s *AppServer) pushIndexNowURLs(ctx context.Context, baseURL string) (int, error) {
	seo := s.deps.Settings.Snapshot().SEO
	if !seo.IndexNowEnabled || strings.TrimSpace(seo.IndexNowKey) == "" {
		return 0, errors.New("IndexNow is disabled or indexnow_key is empty")
	}
	parsedBase, err := url.Parse(baseURL)
	if err != nil || parsedBase.Host == "" {
		return 0, errors.New("valid public base URL required for IndexNow")
	}

	// Strictly exclude password-protected posts from IndexNow submission!
	rows, err := sqlx.Select[domain.RawArticle](ctx, s.deps.Store,
		`SELECT slug FROM articles WHERE status = ? AND password_ciphertext = '' ORDER BY updated_at DESC LIMIT 50`,
		domain.ArticleStatusPublished)
	if err != nil {
		return 0, err
	}
	urls := make([]string, 0, len(rows)+1)
	urls = append(urls, strings.TrimRight(baseURL, "/")+"/")
	for _, r := range rows {
		urls = append(urls, strings.TrimRight(baseURL, "/")+visibility.ArticlePath(r.Slug))
	}

	payload, _ := json.Marshal(map[string]any{
		"host":        parsedBase.Hostname(),
		"key":         seo.IndexNowKey,
		"keyLocation": strings.TrimRight(baseURL, "/") + "/" + seo.IndexNowKey + ".txt",
		"urlList":     urls,
	})
	client := s.deps.OutboundDialer.NewHTTPClient(8 * time.Second)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.indexnow.org/indexnow", bytes.NewReader(payload))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return 0, fmt.Errorf("IndexNow HTTP %d", resp.StatusCode)
	}
	return len(urls), nil
}
