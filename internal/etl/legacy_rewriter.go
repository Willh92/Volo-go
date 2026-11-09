package etl

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	_ "golang.org/x/image/webp"

	"github.com/Willh92/Volo-go/internal/config"
	"github.com/Willh92/Volo-go/internal/db/sqlx"
	"github.com/Willh92/Volo-go/pkg/id"
)

const (
	maxLegacyZipEntries   = 250_000
	maxSingleLegacyMediaB = 64 << 20  // 64 MiB per bundled media file
	maxTotalLegacyMediaB  = 512 << 20 // 512 MiB total unpacked media budget
)

var (
	absURLInTextRe = regexp.MustCompile(`https?://[^\s"'<>)\]]+`)
	relURLInAttrRe = regexp.MustCompile(`(\]\(\s*|(?:href|src|poster)\s*=\s*["'])(/[^)\s"'>]+)`)
)

func cleanUTF8(s string) string {
	if s == "" {
		return ""
	}
	if strings.IndexByte(s, 0) >= 0 {
		s = strings.ReplaceAll(s, "\x00", "")
	}
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "")
	}
	return s
}

func truncRunes(s string, maxRunes int) string {
	s = cleanUTF8(s)
	if maxRunes <= 0 || len(s) <= maxRunes {
		return s
	}
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	runes := []rune(s)
	return string(runes[:maxRunes])
}

func isForbiddenMediaExt(ext string) bool {
	switch strings.ToLower(strings.TrimSpace(ext)) {
	case ".svg", ".html", ".htm", ".xml", ".xhtml", ".js", ".mjs":
		return true
	default:
		return false
	}
}

func hasDotDotSegment(slashPath string) bool {
	for _, seg := range strings.Split(slashPath, "/") {
		if seg == ".." {
			return true
		}
	}
	return false
}

type bundledMediaCandidate struct {
	_ [0]func()

	relKey     string // normalized slash path under uploads/, e.g. "2023/05/demo.png"
	legacyPath string // e.g. "/image/2023/05/demo.png" or "/images/foo.png"
	origName   string
	open       func() (io.ReadCloser, error)
}

func collectBundledMediaCandidates(inputPath string, legacyMediaDir string, report *ImportReport) ([]bundledMediaCandidate, map[string]string, func(), error) {
	var list []bundledMediaCandidate
	extractedImages := make(map[string]string)
	seenRel := make(map[string]bool)
	closeFn := func() {}

	addCandidate := func(prefixKind, relRaw string, openFn func() (io.ReadCloser, error)) {
		rel := strings.TrimLeft(strings.TrimSpace(filepath.ToSlash(relRaw)), "/")
		if rel == "" || hasDotDotSegment(rel) {
			return
		}
		cleaned := path.Clean(rel)
		if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
			return
		}
		base := path.Base(cleaned)
		if strings.HasPrefix(base, ".") {
			return
		}
		ext := strings.ToLower(path.Ext(base))
		if isForbiddenMediaExt(ext) {
			if report != nil {
				report.Warnings = append(report.Warnings, fmt.Sprintf("skipped active markup/script media file %q during import", cleaned))
			}
			return
		}
		if seenRel[cleaned] {
			return
		}
		seenRel[cleaned] = true
		pubURL := "/uploads/" + cleaned
		legacyPath := "/" + prefixKind + "/" + cleaned
		if prefixKind == "images" || prefixKind == "upload" {
			extractedImages[legacyPath] = pubURL
		}
		list = append(list, bundledMediaCandidate{
			relKey:     cleaned,
			legacyPath: legacyPath,
			origName:   base,
			open:       openFn,
		})
	}

	if !strings.HasSuffix(strings.ToLower(inputPath), ".json") {
		f, err := os.Open(inputPath)
		if err == nil {
			keepOpen := false
			if st, sErr := f.Stat(); sErr == nil {
				if zr, zErr := zip.NewReader(f, st.Size()); zErr == nil {
					if len(zr.File) > maxLegacyZipEntries {
						_ = f.Close()
						return nil, nil, closeFn, fmt.Errorf("etl: archive entry count %d exceeds %d safety limit", len(zr.File), maxLegacyZipEntries)
					}
					keepOpen = true
					closeFn = func() { _ = f.Close() }
					for _, zf := range zr.File {
						if zf.FileInfo().IsDir() {
							continue
						}
						if zf.Mode()&os.ModeSymlink != 0 || !zf.Mode().IsRegular() {
							continue
						}
						slashName := strings.TrimLeft(strings.TrimPrefix(filepath.ToSlash(zf.Name), "./"), "/")
						if hasDotDotSegment(slashName) {
							continue
						}
						var prefixKind, rest string
						switch {
						case strings.HasPrefix(slashName, "image/"):
							prefixKind, rest = "image", strings.TrimPrefix(slashName, "image/")
						case strings.HasPrefix(slashName, "images/"):
							prefixKind, rest = "images", strings.TrimPrefix(slashName, "images/")
						case strings.HasPrefix(slashName, "upload/"):
							prefixKind, rest = "upload", strings.TrimPrefix(slashName, "upload/")
						case strings.HasPrefix(slashName, "uploads/"):
							prefixKind, rest = "uploads", strings.TrimPrefix(slashName, "uploads/")
						default:
							continue
						}
						zfItem := zf
						addCandidate(prefixKind, rest, func() (io.ReadCloser, error) {
							return zfItem.Open()
						})
					}
				}
			}
			if !keepOpen {
				_ = f.Close()
			}
		}
	}

	if dir := strings.TrimSpace(legacyMediaDir); dir != "" {
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, wErr error) error {
				if wErr != nil || d.IsDir() {
					return nil
				}
				if d.Type()&os.ModeSymlink != 0 || !d.Type().IsRegular() {
					return nil
				}
				rel, rErr := filepath.Rel(dir, p)
				if rErr != nil {
					return nil
				}
				diskPath := p
				addCandidate("image", filepath.ToSlash(rel), func() (io.ReadCloser, error) {
					return os.Open(diskPath)
				})
				return nil
			})
		}
	}

	return list, extractedImages, closeFn, nil
}

func persistBundledMediaTx(
	ctx context.Context,
	tx *sqlx.Tx,
	candidates []bundledMediaCandidate,
	targetUploadDir string,
	uploaderID string,
	report *ImportReport,
	createdFiles *[]string,
) error {
	if len(candidates) == 0 {
		return nil
	}
	uploadDir := strings.TrimSpace(targetUploadDir)
	if uploadDir == "" {
		return nil
	}
	if err := os.MkdirAll(uploadDir, 0o750); err != nil {
		return fmt.Errorf("etl: mkdir upload dir: %w", err)
	}
	cleanUploadRoot, err := filepath.Abs(uploadDir)
	if err != nil {
		return err
	}

	var totalWritten int64
	nowMs := time.Now().UnixMilli()
	buf := make([]byte, 32*1024)

	for _, cand := range candidates {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if len(cand.relKey) > 190 {
			continue
		}
		destPath := filepath.Join(cleanUploadRoot, filepath.FromSlash(cand.relKey))
		relCheck, relErr := filepath.Rel(cleanUploadRoot, destPath)
		if relErr != nil || relCheck == ".." || strings.HasPrefix(relCheck, ".."+string(os.PathSeparator)) {
			continue
		}

		rc, err := cand.open()
		if err != nil {
			if report != nil {
				report.Warnings = append(report.Warnings, fmt.Sprintf("skipped unreadable media %q: %v", cand.relKey, err))
			}
			continue
		}

		// Pre-sniff up to 512 bytes BEFORE touching disk so disguised SVG/HTML/XML/JS payloads never create files!
		var headBuf [512]byte
		headN := 0
		var preReadErr error
		for headN < len(headBuf) {
			n, rErr := rc.Read(headBuf[headN:])
			headN += n
			if rErr == io.EOF {
				break
			}
			if rErr != nil {
				preReadErr = rErr
				break
			}
		}
		if preReadErr != nil {
			_ = rc.Close()
			return fmt.Errorf("etl: read media header %q: %w", cand.relKey, preReadErr)
		}

		headSlice := headBuf[:headN]
		mimeType := inferLegacyMediaMIME(cand.origName, headSlice)
		sniffMIME := ""
		if headN > 0 {
			sniffMIME = http.DetectContentType(headSlice)
		}
		if isForbiddenMediaMIME(mimeType) || isForbiddenMediaMIME(sniffMIME) || hasDisguisedActiveMarkup(headSlice) {
			_ = rc.Close()
			if report != nil {
				badMIME := sniffMIME
				if badMIME == "" || !isForbiddenMediaMIME(badMIME) {
					badMIME = mimeType
				}
				report.Warnings = append(report.Warnings, fmt.Sprintf("skipped media %q with active MIME/markup (%s)", cand.relKey, badMIME))
			}
			continue
		}

		_, statErr := os.Lstat(destPath)
		existedBefore := statErr == nil

		if err := os.MkdirAll(filepath.Dir(destPath), 0o750); err != nil {
			_ = rc.Close()
			return err
		}
		outFile, err := os.OpenFile(destPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
		if err != nil {
			_ = rc.Close()
			return err
		}

		hasher := sha256.New()
		var written int64
		var copyErr error
		if headN > 0 {
			written = int64(headN)
			if written > maxSingleLegacyMediaB || totalWritten+written > maxTotalLegacyMediaB {
				copyErr = errors.New("bundled media size budget exceeded")
			} else {
				_, _ = hasher.Write(headSlice)
				if _, wErr := outFile.Write(headSlice); wErr != nil {
					copyErr = wErr
				}
			}
		}
		for copyErr == nil {
			n, rErr := rc.Read(buf)
			if n > 0 {
				written += int64(n)
				if written > maxSingleLegacyMediaB || totalWritten+written > maxTotalLegacyMediaB {
					copyErr = errors.New("bundled media size budget exceeded")
					break
				}
				_, _ = hasher.Write(buf[:n])
				if _, wErr := outFile.Write(buf[:n]); wErr != nil {
					copyErr = wErr
					break
				}
			}
			if rErr == io.EOF {
				break
			}
			if rErr != nil {
				copyErr = rErr
				break
			}
		}
		_ = outFile.Close()
		_ = rc.Close()

		if copyErr != nil {
			if !existedBefore {
				_ = os.Remove(destPath)
			}
			return fmt.Errorf("etl: extract media %q: %w", cand.relKey, copyErr)
		}
		if !existedBefore && createdFiles != nil {
			*createdFiles = append(*createdFiles, destPath)
		}
		totalWritten += written

		var width, height int
		if strings.HasPrefix(mimeType, "image/") {
			if imgF, iErr := os.Open(destPath); iErr == nil {
				if cfg, _, cErr := image.DecodeConfig(imgF); cErr == nil && cfg.Width > 0 && cfg.Height > 0 {
					width, height = cfg.Width, cfg.Height
				}
				_ = imgF.Close()
			}
		}

		pubURL := "/uploads/" + cand.relKey
		shaHex := hex.EncodeToString(hasher.Sum(nil))
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO media_assets (id, storage_driver, object_key, public_url, original_name, alt_text, mime_type, size_bytes, width, height, sha256, exif_stripped, uploader_id, created_at)
			 VALUES (?, 'local', ?, ?, ?, '', ?, ?, ?, ?, ?, ?, ?, ?)`,
			id.Next(), truncRunes(cand.relKey, 190), truncRunes(pubURL, 512), truncRunes(cand.origName, 255),
			truncRunes(mimeType, 64), written, width, height, shaHex, false, truncRunes(uploaderID, 32), nowMs,
		); err != nil {
			return fmt.Errorf("etl: insert media_asset %q: %w", cand.relKey, err)
		}
		report.MediaFiles++
	}
	return nil
}

func inferLegacyMediaMIME(filename string, head []byte) string {
	switch strings.ToLower(path.Ext(filename)) {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".avif":
		return "image/avif"
	case ".ico":
		return "image/x-icon"
	case ".mp3":
		return "audio/mpeg"
	case ".m4a":
		return "audio/mp4"
	case ".ogg":
		return "audio/ogg"
	case ".wav":
		return "audio/wav"
	case ".pdf":
		return "application/pdf"
	case ".zip":
		return "application/zip"
	}
	if len(head) > 0 {
		return http.DetectContentType(head)
	}
	return "application/octet-stream"
}

func isForbiddenMediaMIME(ct string) bool {
	low := strings.ToLower(ct)
	return strings.Contains(low, "svg") ||
		strings.Contains(low, "text/html") ||
		strings.Contains(low, "xhtml") ||
		strings.Contains(low, "xml") ||
		strings.Contains(low, "javascript")
}

func hasDisguisedActiveMarkup(head []byte) bool {
	if len(head) == 0 {
		return false
	}
	trimmed := strings.TrimSpace(strings.TrimPrefix(string(head), "\xEF\xBB\xBF"))
	low := strings.ToLower(trimmed)
	if strings.HasPrefix(low, "<svg") ||
		strings.HasPrefix(low, "<?xml") ||
		strings.HasPrefix(low, "<!doctype html") ||
		strings.HasPrefix(low, "<html") ||
		strings.HasPrefix(low, "<script") ||
		strings.HasPrefix(low, "<iframe") {
		return true
	}
	return strings.Contains(low, "<svg") || strings.Contains(low, "<script")
}

// urlRewriter rewrites legacy site origins, CDN prefixes, .html permalinks, and /image/* paths
// across structured URLs and Markdown/HTML bodies while counting total URLs rewritten.
type urlRewriter struct {
	_ [0]func()

	adminPath       string
	newSiteURL      string
	oldPrefixes     []string
	permalinkMap    map[string]string
	extractedImages map[string]string
	count           int
}

func newURLRewriter(st *pass1Data, opts ImportOptions, extractedImages map[string]string) *urlRewriter {
	adminPath := strings.TrimSpace(opts.AdminPath)
	if adminPath == "" {
		adminPath = config.DefaultAdminPath
	}
	newSite := normalizeOriginPrefix(opts.NewSiteURL)

	seenPrefix := make(map[string]bool)
	var prefixes []string
	addRawPrefix := func(raw string) {
		for _, tok := range strings.FieldsFunc(raw, func(r rune) bool {
			return r == ',' || r == ';' || r == '\n' || r == '\r' || r == '\t' || r == ' '
		}) {
			norm := normalizeOriginPrefix(tok)
			if norm == "" {
				continue
			}
			low := strings.ToLower(norm)
			if newSite != "" && strings.EqualFold(norm, newSite) {
				// Even if same domain, still register if needed? If old == new, keep so <old><legacyPermalink> still rewrites!
			}
			if !seenPrefix[low] {
				seenPrefix[low] = true
				prefixes = append(prefixes, norm)
			}
		}
	}

	if st != nil {
		if sp, ok := st.options["servePath"]; ok {
			addRawPrefix(sp.Value)
		}
		if ssp, ok := st.options["staticServePath"]; ok {
			addRawPrefix(ssp.Value)
		}
	}
	for _, p := range opts.OldURLPrefixes {
		addRawPrefix(p)
	}
	sort.Slice(prefixes, func(i, j int) bool {
		return len(prefixes[i]) > len(prefixes[j])
	})

	pm := make(map[string]string)
	if st != nil && st.permalinkMap != nil {
		pm = st.permalinkMap
	}
	return &urlRewriter{
		adminPath:       adminPath,
		newSiteURL:      newSite,
		oldPrefixes:     prefixes,
		permalinkMap:    pm,
		extractedImages: extractedImages,
	}
}

func normalizeOriginPrefix(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}
	u.RawQuery = ""
	u.Fragment = ""
	u.Path = strings.TrimRight(u.Path, "/")
	return strings.TrimRight(u.String(), "/")
}

func splitPathQueryFrag(u string) (string, string) {
	idx := strings.IndexAny(u, "?#")
	if idx < 0 {
		return u, ""
	}
	return u[:idx], u[idx:]
}

func (r *urlRewriter) lookupPermalink(relPath string) (string, bool) {
	if relPath == "" {
		return "", false
	}
	if m, ok := r.permalinkMap[relPath]; ok {
		return m, true
	}
	trimmed := strings.TrimRight(relPath, "/")
	if trimmed != "" && trimmed != relPath {
		if m, ok := r.permalinkMap[trimmed]; ok {
			return m, true
		}
	}
	return "", false
}

func legacySystemRoute(relPath string, adminPath string) (string, bool) {
	switch relPath {
	case "/admin-index.do", "/admin/index.do", "/start":
		return adminPath, true
	case "/archives.html", "/archive.html", "/archive", "/archivedate.html":
		return "/archives", true
	case "/tags.html":
		return "/tags", true
	case "/categories.html":
		return "/categories", true
	case "/links.html":
		return "/links", true
	case "/follows.html":
		return "/follows", true
	case "/dynamic.html":
		return "/dynamic", true
	case "/rss.do":
		return "/rss.xml", true
	}
	for _, pfx := range []string{"/archives/", "/archive/", "/archivedate/"} {
		if strings.HasPrefix(relPath, pfx) {
			sub := strings.Trim(strings.TrimSuffix(relPath[len(pfx):], ".html"), "/")
			parts := strings.Split(sub, "/")
			if len(parts) == 2 && len(parts[0]) == 4 && (len(parts[1]) == 1 || len(parts[1]) == 2) {
				if yr, e1 := strconv.Atoi(parts[0]); e1 == nil && yr >= 1900 && yr <= 9999 {
					if mo, e2 := strconv.Atoi(parts[1]); e2 == nil && mo >= 1 && mo <= 12 {
						return fmt.Sprintf("/archives/%04d-%02d", yr, mo), true
					}
				}
			}
		}
	}
	return "", false
}

func isInternalSitePath(relPath string) bool {
	if relPath == "/" {
		return false
	}
	for _, pfx := range []string{
		"/posts/", "/uploads/", "/image/", "/articles/", "/category/", "/categories",
		"/tags", "/archives", "/links", "/follows", "/books", "/series", "/pages/",
		"/rss.xml", "/atom.xml", "/podcast.xml", "/sitemap.xml", "/avatars/", "/github", "/my-github-repos",
	} {
		if relPath == pfx || strings.HasPrefix(relPath, pfx) {
			return true
		}
	}
	return false
}

func (r *urlRewriter) matchOldPrefix(u string) (string, string, bool) {
	lowU := strings.ToLower(u)
	for _, pfx := range r.oldPrefixes {
		lowPfx := strings.ToLower(pfx)
		if !strings.HasPrefix(lowU, lowPfx) {
			continue
		}
		rem := u[len(pfx):]
		if rem == "" || rem[0] == '/' || rem[0] == '?' || rem[0] == '#' {
			return pfx, rem, true
		}
	}
	return "", "", false
}

func (r *urlRewriter) rewriteURL(raw string) string {
	u := strings.TrimSpace(raw)
	if u == "" {
		return ""
	}

	if matchedPfx, rem, ok := r.matchOldPrefix(u); ok {
		absBase := r.newSiteURL
		if rem == "" {
			if absBase != "" && absBase != matchedPfx {
				r.count++
				return absBase
			}
			return u
		}
		if !strings.HasPrefix(rem, "/") {
			// "?..." or "#..." directly on root origin
			if absBase != "" && absBase+rem != u {
				r.count++
				return absBase + rem
			}
			return u
		}
		remPath, remSuffix := splitPathQueryFrag(rem)
		if canon, sysOK := legacySystemRoute(remPath, r.adminPath); sysOK {
			target := canon + remSuffix
			if canon != r.adminPath && absBase != "" {
				target = absBase + canon + remSuffix
			}
			if target != u {
				r.count++
			}
			return target
		}
		if modern, pmOK := r.lookupPermalink(remPath); pmOK {
			target := absBase + modern + remSuffix
			if target != u {
				r.count++
			}
			return target
		}
		if strings.HasPrefix(remPath, "/image/") && len(remPath) > len("/image/") {
			target := absBase + "/uploads/" + strings.TrimPrefix(remPath, "/image/") + remSuffix
			if target != u {
				r.count++
			}
			return target
		}
		if upPath, exOK := r.extractedImages[remPath]; exOK {
			target := absBase + upPath + remSuffix
			if target != u {
				r.count++
			}
			return target
		}
		if absBase != "" {
			target := absBase + rem
			if target != u {
				r.count++
			}
			return target
		}
		if isInternalSitePath(remPath) {
			target := remPath + remSuffix
			if target != u {
				r.count++
			}
			return target
		}
		return u
	}

	if strings.HasPrefix(u, "/") && !strings.HasPrefix(u, "//") {
		relPath, relSuffix := splitPathQueryFrag(u)
		if strings.HasPrefix(u, "/admin-index.do#") || strings.HasPrefix(u, "/admin/index.do#") {
			if u != r.adminPath {
				r.count++
			}
			return r.adminPath
		}
		if canon, sysOK := legacySystemRoute(relPath, r.adminPath); sysOK {
			target := canon + relSuffix
			if target != u {
				r.count++
			}
			return target
		}
		if modern, pmOK := r.lookupPermalink(relPath); pmOK {
			target := modern + relSuffix
			if target != u {
				r.count++
			}
			return target
		}
		if strings.HasPrefix(relPath, "/image/") && len(relPath) > len("/image/") {
			target := "/uploads/" + strings.TrimPrefix(relPath, "/image/") + relSuffix
			if target != u {
				r.count++
			}
			return target
		}
		if upPath, exOK := r.extractedImages[relPath]; exOK {
			target := upPath + relSuffix
			if target != u {
				r.count++
			}
			return target
		}
	}

	return u
}

func (r *urlRewriter) rewriteText(text string) string {
	if text == "" {
		return ""
	}
	out := absURLInTextRe.ReplaceAllStringFunc(text, func(match string) string {
		trimmed := match
		trailing := ""
		for len(trimmed) > 0 {
			last := trimmed[len(trimmed)-1]
			if last == '.' || last == ',' || last == ';' || last == ':' || last == '!' {
				trailing = string(last) + trailing
				trimmed = trimmed[:len(trimmed)-1]
			} else {
				break
			}
		}
		return r.rewriteURL(trimmed) + trailing
	})
	out = relURLInAttrRe.ReplaceAllStringFunc(out, func(match string) string {
		sub := relURLInAttrRe.FindStringSubmatch(match)
		if len(sub) != 3 {
			return match
		}
		return sub[1] + r.rewriteURL(sub[2])
	})
	return out
}

// streamArticleHeaders scans an "articles" JSON array in Pass 1 extracting ONLY (oId, articlePermalink, articleTitle)
// and skipping large articleContent bodies at token level so heap usage remains near-zero.
func streamArticleHeaders(dec *json.Decoder, onHeader func(oid, permalink, title string) error) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if tok == nil {
		return nil
	}
	if tok != json.Delim('[') {
		return fmt.Errorf("etl: expected '[' for articles, got %v", tok)
	}
	for dec.More() {
		objTok, err := dec.Token()
		if err != nil {
			return err
		}
		if objTok != json.Delim('{') {
			return fmt.Errorf("etl: expected '{' in articles array, got %v", objTok)
		}
		var oid, permalink, title string
		for dec.More() {
			kTok, err := dec.Token()
			if err != nil {
				return err
			}
			k, _ := kTok.(string)
			switch k {
			case "oId":
				var v any
				if err := dec.Decode(&v); err != nil {
					return err
				}
				oid = strVal(v)
			case "articlePermalink":
				var v any
				if err := dec.Decode(&v); err != nil {
					return err
				}
				permalink = strVal(v)
			case "articleTitle":
				var v any
				if err := dec.Decode(&v); err != nil {
					return err
				}
				title = strVal(v)
			default:
				if err := skipJSONTokens(dec); err != nil {
					return err
				}
			}
		}
		if _, err := dec.Token(); err != nil { // consume '}'
			return err
		}
		if err := onHeader(oid, permalink, title); err != nil {
			return err
		}
	}
	_, err = dec.Token() // consume ']'
	return err
}

func skipJSONTokens(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	if delim == '{' || delim == '[' {
		depth := 1
		for depth > 0 {
			t, err := dec.Token()
			if err != nil {
				return err
			}
			if d, isD := t.(json.Delim); isD {
				switch d {
				case '{', '[':
					depth++
				case '}', ']':
					depth--
				}
			}
		}
	}
	return nil
}
