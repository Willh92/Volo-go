package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"image"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Willh92/Volo-go/internal/db/sqlx"
	"github.com/Willh92/Volo-go/internal/domain"
	"github.com/Willh92/Volo-go/internal/theme"
	"github.com/Willh92/Volo-go/pkg/id"
)

// MediaAssetView enriches MediaAsset with live reference counts and orphan status for the Admin Media Library.
type MediaAssetView struct {
	domain.MediaAsset
	RefCount int  `json:"ref_count"`
	IsOrphan bool `json:"is_orphan"`
}

func isSupportedReconcileExt(ext string) bool {
	switch strings.ToLower(ext) {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".avif", ".mp3", ".m4a", ".ogg", ".wav", ".pdf", ".zip":
		return true
	default:
		return false
	}
}

// stripImageMetadataBytes losslessly strips EXIF/XMP/IPTC/text metadata chunks from JPEG, PNG, and WebP
// byte slices without re-encoding pixel data. Returns (cleanedBytes, true) when metadata was removed.
func stripImageMetadataBytes(raw []byte, ext string) ([]byte, bool) {
	if len(raw) < 12 {
		return raw, false
	}
	// JPEG: SOI 0xFF 0xD8
	if raw[0] == 0xFF && raw[1] == 0xD8 {
		out := make([]byte, 0, len(raw))
		out = append(out, 0xFF, 0xD8)
		pos := 2
		removed := false
		for pos < len(raw) {
			if raw[pos] != 0xFF {
				return raw, false
			}
			for pos < len(raw) && raw[pos] == 0xFF {
				pos++
			}
			if pos >= len(raw) {
				break
			}
			marker := raw[pos]
			pos++
			if marker == 0x00 {
				return raw, false
			}
			if marker == 0xD9 { // EOI
				out = append(out, 0xFF, 0xD9)
				return out, removed
			}
			if marker == 0x01 || (marker >= 0xD0 && marker <= 0xD7) {
				out = append(out, 0xFF, marker)
				continue
			}
			if pos+2 > len(raw) {
				return raw, false
			}
			segLen := int(binary.BigEndian.Uint16(raw[pos : pos+2]))
			if segLen < 2 || pos+segLen > len(raw) {
				return raw, false
			}
			if marker == 0xDA { // SOS: copy header + entropy-coded scan stream to EOF
				out = append(out, 0xFF, marker)
				out = append(out, raw[pos:]...)
				return out, removed
			}
			payload := raw[pos+2 : pos+segLen]
			isICC := marker == 0xE2 && bytes.HasPrefix(payload, []byte("ICC_PROFILE\x00"))
			if !isICC && (marker == 0xFE || (marker >= 0xE1 && marker <= 0xEF)) {
				removed = true
				pos += segLen
				continue
			}
			out = append(out, 0xFF, marker)
			out = append(out, raw[pos:pos+segLen]...)
			pos += segLen
		}
		return out, removed
	}

	// PNG: \x89PNG\r\n\x1a\n
	if bytes.Equal(raw[:8], []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}) {
		out := make([]byte, 0, len(raw))
		out = append(out, raw[:8]...)
		pos := 8
		removed := false
		for pos+12 <= len(raw) {
			cLen := int(binary.BigEndian.Uint32(raw[pos : pos+4]))
			if cLen < 0 || pos+12+cLen > len(raw) {
				return raw, false
			}
			cType := string(raw[pos+4 : pos+8])
			totalChunk := 12 + cLen
			if cType == "eXIf" || cType == "tEXt" || cType == "zTXt" || cType == "iTXt" {
				removed = true
			} else {
				out = append(out, raw[pos:pos+totalChunk]...)
			}
			pos += totalChunk
			if cType == "IEND" {
				break
			}
		}
		return out, removed
	}

	// WebP: RIFF....WEBP
	if string(raw[0:4]) == "RIFF" && string(raw[8:12]) == "WEBP" {
		out := make([]byte, 12, len(raw))
		copy(out, raw[:12])
		pos := 12
		removed := false
		for pos+8 <= len(raw) {
			fourCC := string(raw[pos : pos+4])
			cSize := int(binary.LittleEndian.Uint32(raw[pos+4 : pos+8]))
			if cSize < 0 {
				return raw, false
			}
			padded := cSize
			if padded%2 == 1 {
				padded++
			}
			if pos+8+padded > len(raw) {
				if pos+8+cSize == len(raw) {
					padded = cSize
				} else {
					return raw, false
				}
			}
			if fourCC == "EXIF" || fourCC == "XMP " {
				removed = true
			} else {
				out = append(out, raw[pos:pos+8+padded]...)
			}
			pos += 8 + padded
		}
		if removed {
			if len(out) >= 24 && string(out[12:16]) == "VP8X" {
				out[20] &^= 0x0C // clear EXIF (0x08) and XMP (0x04) feature flags
			}
			binary.LittleEndian.PutUint32(out[4:8], uint32(len(out)-8))
			return out, true
		}
		return raw, false
	}

	_ = ext
	return raw, false
}

// reconcileLocalUploads scans UploadDir on explicit operator request (`POST /api/admin/media/reconcile`)
// and registers untracked valid media files in `media_assets`, stripping image EXIF/metadata on disk and
// returning the number of newly imported rows.
func (s *AppServer) reconcileLocalUploads(ctx context.Context) int {
	if s.deps.Config == nil || strings.TrimSpace(s.deps.Config.UploadDir) == "" {
		return 0
	}
	rootAbs, err := filepath.Abs(s.deps.Config.UploadDir)
	if err != nil {
		return 0
	}
	st, err := os.Stat(rootAbs)
	if err != nil || !st.IsDir() {
		return 0
	}

	existingKeys := make(map[string]struct{})
	rows, err := sqlx.Select[struct {
		ObjectKey string `db:"object_key"`
		PublicURL string `db:"public_url"`
	}](ctx, s.deps.Store, `SELECT object_key, public_url FROM media_assets`)
	if err == nil {
		for _, r := range rows {
			k := strings.TrimLeft(strings.TrimSpace(r.ObjectKey), "/")
			if k != "" {
				existingKeys[k] = struct{}{}
			}
			if strings.HasPrefix(r.PublicURL, "/uploads/") {
				uk := strings.TrimPrefix(r.PublicURL, "/uploads/")
				if uk != "" {
					existingKeys[uk] = struct{}{}
				}
			}
		}
	}

	imported := 0
	_ = filepath.WalkDir(rootAbs, func(fullPath string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		name := d.Name()
		if strings.HasPrefix(name, ".") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		rel, rErr := filepath.Rel(rootAbs, fullPath)
		if rErr != nil || rel == "." {
			return nil
		}
		relSlash := filepath.ToSlash(rel)
		if d.IsDir() {
			if relSlash == "images" || strings.HasPrefix(relSlash, "images/") {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 || !d.Type().IsRegular() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(name))
		if !isSupportedReconcileExt(ext) {
			return nil
		}
		if _, exists := existingKeys[relSlash]; exists {
			return nil
		}
		info, iErr := d.Info()
		if iErr != nil || info.Size() <= 0 || info.Size() > (100<<20) {
			return nil
		}

		fileSize := info.Size()
		exifStripped := false
		isStripCandidate := ext == ".jpg" || ext == ".jpeg" || ext == ".png" || ext == ".webp"
		if isStripCandidate && fileSize <= (32<<20) {
			if rawBytes, rErr := os.ReadFile(fullPath); rErr == nil {
				if cleaned, changed := stripImageMetadataBytes(rawBytes, ext); changed {
					tmpPath := fullPath + ".volostrip." + strings.ToLower(id.Next())
					if wErr := os.WriteFile(tmpPath, cleaned, info.Mode().Perm()); wErr == nil {
						if renErr := os.Rename(tmpPath, fullPath); renErr == nil {
							fileSize = int64(len(cleaned))
							exifStripped = true
						} else {
							_ = os.Remove(tmpPath)
						}
					}
				} else {
					exifStripped = true
				}
			}
		}

		f, oErr := os.Open(fullPath)
		if oErr != nil {
			return nil
		}

		var head [512]byte
		n, _ := io.ReadFull(f, head[:])
		mimeType := mime.TypeByExtension(ext)
		if mimeType == "" && n > 0 {
			mimeType = http.DetectContentType(head[:n])
		}
		if idx := strings.IndexByte(mimeType, ';'); idx > 0 {
			mimeType = strings.TrimSpace(mimeType[:idx])
		}

		var width, height int
		if strings.HasPrefix(mimeType, "image/") {
			_, _ = f.Seek(0, io.SeekStart)
			if cfg, _, dErr := image.DecodeConfig(f); dErr == nil {
				width, height = cfg.Width, cfg.Height
			}
		}

		_, _ = f.Seek(0, io.SeekStart)
		hasher := sha256.New()
		written, _ := io.Copy(hasher, f)
		_ = f.Close()
		if written > 0 {
			fileSize = written
		}
		sumHex := hex.EncodeToString(hasher.Sum(nil))

		createdAt := info.ModTime().UnixMilli()
		if createdAt <= 0 {
			createdAt = time.Now().UnixMilli()
		}
		_, insErr := s.deps.Store.ExecContext(ctx,
			`INSERT INTO media_assets (id, storage_driver, object_key, public_url, original_name, alt_text, mime_type, size_bytes, width, height, sha256, exif_stripped, uploader_id, created_at)
			 VALUES (?, 'local', ?, ?, ?, '', ?, ?, ?, ?, ?, ?, '', ?)`,
			id.Next(), relSlash, "/uploads/"+relSlash, name, mimeType, fileSize, width, height, sumHex, exifStripped, createdAt)
		if insErr == nil {
			existingKeys[relSlash] = struct{}{}
			imported++
		}
		return nil
	})
	if imported > 0 && s.deps.Content != nil {
		_ = s.deps.Content.RefreshMediaDimsCache(ctx)
	}
	return imported
}

type sqlNullString struct {
	val string
}

func (ns *sqlNullString) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		ns.val = ""
	case string:
		ns.val = v
	case []byte:
		ns.val = string(v)
	default:
		ns.val = ""
	}
	return nil
}

func isMediaRefDelim(b byte) bool {
	switch b {
	case ' ', '\t', '\n', '\r', '"', '\'', '(', ')', '<', '>', '[', ']', '{', '}', ',', ';', '?', '#', '|', '`':
		return true
	default:
		return false
	}
}

func (s *AppServer) invalidateMediaRefCache() {
	if s == nil {
		return
	}
	s.mediaRefCacheMu.Lock()
	s.mediaRefCachedAt = time.Time{}
	s.mediaRefCachedTokens = nil
	s.mediaRefCacheMu.Unlock()
}

// collectReferencedMediaTokensCached scans all 12 content & configuration tables for media URL/key tokens
// and caches per-token row reference counts for 10 seconds (invalidated immediately on mutations).
func (s *AppServer) collectReferencedMediaTokensCached(ctx context.Context) map[string]int {
	if s == nil || s.deps.Store == nil {
		return map[string]int{}
	}
	s.mediaRefCacheMu.Lock()
	if s.mediaRefCachedTokens != nil && time.Since(s.mediaRefCachedAt) < 10*time.Second {
		cached := s.mediaRefCachedTokens
		s.mediaRefCacheMu.Unlock()
		return cached
	}
	s.mediaRefCacheMu.Unlock()

	tokenCounts := make(map[string]int, 256)
	rowSeen := make(map[string]struct{}, 16)

	recordKey := func(k string) {
		if k == "" || len(k) > 512 {
			return
		}
		if _, exists := rowSeen[k]; !exists {
			rowSeen[k] = struct{}{}
			tokenCounts[k]++
		}
	}

	collectToken := func(tok string) {
		if tok == "" {
			return
		}
		if strings.Contains(tok, "/") || strings.Contains(tok, ".") {
			recordKey(tok)
			trimmed := strings.TrimLeft(tok, "/")
			if trimmed != "" {
				recordKey(trimmed)
			}
		}
		if uIdx := strings.Index(tok, "/uploads/"); uIdx >= 0 {
			subPub := tok[uIdx:]
			recordKey(subPub)
			subKey := subPub[len("/uploads/"):]
			if subKey != "" {
				recordKey(subKey)
			}
		} else if strings.HasPrefix(tok, "http://") || strings.HasPrefix(tok, "https://") {
			if schemeEnd := strings.Index(tok, "://"); schemeEnd > 0 {
				rest := tok[schemeEnd+3:]
				if slash := strings.IndexByte(rest, '/'); slash >= 0 && slash+1 < len(rest) {
					recordKey(rest[slash+1:])
				}
			}
		}
	}

	scanColumn := func(raw string) {
		if raw == "" {
			return
		}
		str := raw
		if strings.Contains(str, `\/`) {
			str = strings.ReplaceAll(str, `\/`, "/")
		}
		collectToken(strings.TrimSpace(str))
		n := len(str)
		start := -1
		for i := 0; i < n; i++ {
			if isMediaRefDelim(str[i]) {
				if start >= 0 {
					if i-start >= 3 {
						collectToken(str[start:i])
					}
					start = -1
				}
			} else if start < 0 {
				start = i
			}
		}
		if start >= 0 && n-start >= 3 {
			collectToken(str[start:n])
		}
	}

	scanQuery := func(q string, args ...any) {
		rows, err := s.deps.Store.QueryContext(ctx, q, args...)
		if err != nil {
			return
		}
		defer rows.Close()
		cols, err := rows.Columns()
		if err != nil || len(cols) == 0 {
			return
		}
		vals := make([]sqlNullString, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		for rows.Next() {
			for i := range vals {
				vals[i].val = ""
			}
			if err := rows.Scan(ptrs...); err != nil {
				continue
			}
			clear(rowSeen)
			for _, col := range vals {
				scanColumn(col.val)
			}
		}
	}

	scanQuery(`SELECT content_md, summary_md, cover_url, audio_url FROM articles`)
	scanQuery(`SELECT content_md, summary_md FROM article_revisions`)
	scanQuery(`SELECT content_md FROM pages`)
	scanQuery(`SELECT cover_url, description FROM series`)
	scanQuery(`SELECT cover_url, description FROM books`)
	scanQuery(`SELECT icon_url, description FROM links`)
	scanQuery(`SELECT avatar_url, bio FROM users`)
	scanQuery(`SELECT content_md, avatar_url FROM comments WHERE is_deleted = ?`, false)
	scanQuery(`SELECT value_json FROM settings`)
	scanQuery(`SELECT config_json FROM plugins`)
	scanQuery(`SELECT icon_url, description FROM follows`)
	scanQuery(`SELECT icon FROM nav_items`)

	s.mediaRefCacheMu.Lock()
	s.mediaRefCachedAt = time.Now()
	s.mediaRefCachedTokens = tokenCounts
	s.mediaRefCacheMu.Unlock()
	return tokenCounts
}

func assetRefCountFromTokens(tokenCounts map[string]int, a domain.MediaAsset) int {
	pub := strings.TrimSpace(a.PublicURL)
	obj := strings.TrimLeft(strings.TrimSpace(a.ObjectKey), "/")
	rc := 0
	if pub != "" {
		if c := tokenCounts[pub]; c > rc {
			rc = c
		}
	}
	if obj != "" {
		if c := tokenCounts[obj]; c > rc {
			rc = c
		}
	}
	return rc
}

// countAssetsReferencesStreaming computes reference counts across all content & configuration tables,
// utilizing the 10-second token cache for standard assets and falling back to a direct row scan only
// if an asset path contains whitespace characters.
func (s *AppServer) countAssetsReferencesStreaming(ctx context.Context, assets []domain.MediaAsset) []int {
	counts := make([]int, len(assets))
	if len(assets) == 0 {
		return counts
	}

	hasWhitespaceKey := false
	for _, a := range assets {
		if strings.ContainsAny(strings.TrimSpace(a.PublicURL), " \t") || strings.ContainsAny(strings.TrimSpace(a.ObjectKey), " \t") {
			hasWhitespaceKey = true
			break
		}
	}
	if !hasWhitespaceKey {
		tokenCounts := s.collectReferencedMediaTokensCached(ctx)
		for i, a := range assets {
			counts[i] = assetRefCountFromTokens(tokenCounts, a)
		}
		return counts
	}

	byPubURL := make(map[string][]int, len(assets))
	byObjKey := make(map[string][]int, len(assets))
	type spaceNeedle struct {
		idx    int
		pubURL string
		objKey string
	}
	var spaceAssets []spaceNeedle

	for i, a := range assets {
		pub := strings.TrimSpace(a.PublicURL)
		obj := strings.TrimLeft(strings.TrimSpace(a.ObjectKey), "/")
		if pub != "" {
			byPubURL[pub] = append(byPubURL[pub], i)
		}
		if obj != "" {
			byObjKey[obj] = append(byObjKey[obj], i)
		}
		if strings.ContainsAny(pub, " \t") || strings.ContainsAny(obj, " \t") {
			spaceAssets = append(spaceAssets, spaceNeedle{idx: i, pubURL: pub, objKey: obj})
		}
	}

	rowSeen := make([]uint32, len(assets))
	var rowGen uint32

	markIndices := func(idxs []int) {
		for _, idx := range idxs {
			if rowSeen[idx] != rowGen {
				rowSeen[idx] = rowGen
				counts[idx]++
			}
		}
	}

	matchToken := func(tok string) {
		if tok == "" {
			return
		}
		if idxs, ok := byPubURL[tok]; ok {
			markIndices(idxs)
		}
		trimmed := strings.TrimLeft(tok, "/")
		if trimmed != "" {
			if idxs, ok := byObjKey[trimmed]; ok {
				markIndices(idxs)
			}
		}
		if uIdx := strings.Index(tok, "/uploads/"); uIdx >= 0 {
			subPub := tok[uIdx:]
			if idxs, ok := byPubURL[subPub]; ok {
				markIndices(idxs)
			}
			subKey := subPub[len("/uploads/"):]
			if subKey != "" {
				if idxs, ok := byObjKey[subKey]; ok {
					markIndices(idxs)
				}
			}
		} else if strings.HasPrefix(tok, "http://") || strings.HasPrefix(tok, "https://") {
			if schemeEnd := strings.Index(tok, "://"); schemeEnd > 0 {
				rest := tok[schemeEnd+3:]
				if slash := strings.IndexByte(rest, '/'); slash >= 0 && slash+1 < len(rest) {
					pathKey := rest[slash+1:]
					if idxs, ok := byObjKey[pathKey]; ok {
						markIndices(idxs)
					}
				}
			}
		}
	}

	scanColumn := func(raw string) {
		if raw == "" {
			return
		}
		str := raw
		if strings.Contains(str, `\/`) {
			str = strings.ReplaceAll(str, `\/`, "/")
		}
		whole := strings.TrimSpace(str)
		matchToken(whole)

		n := len(str)
		start := -1
		for i := 0; i < n; i++ {
			if isMediaRefDelim(str[i]) {
				if start >= 0 {
					if i-start >= 3 {
						matchToken(str[start:i])
					}
					start = -1
				}
			} else if start < 0 {
				start = i
			}
		}
		if start >= 0 && n-start >= 3 {
			matchToken(str[start:n])
		}

		for _, sp := range spaceAssets {
			if rowSeen[sp.idx] == rowGen {
				continue
			}
			if (sp.pubURL != "" && strings.Contains(str, sp.pubURL)) ||
				(sp.objKey != "" && strings.Contains(str, sp.objKey)) {
				rowSeen[sp.idx] = rowGen
				counts[sp.idx]++
			}
		}
	}

	scanQuery := func(q string, args ...any) {
		rows, err := s.deps.Store.QueryContext(ctx, q, args...)
		if err != nil {
			return
		}
		defer rows.Close()
		cols, err := rows.Columns()
		if err != nil || len(cols) == 0 {
			return
		}
		vals := make([]sqlNullString, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		for rows.Next() {
			for i := range vals {
				vals[i].val = ""
			}
			if err := rows.Scan(ptrs...); err != nil {
				continue
			}
			rowGen++
			if rowGen == 0 {
				clear(rowSeen)
				rowGen = 1
			}
			for _, col := range vals {
				scanColumn(col.val)
			}
		}
	}

	scanQuery(`SELECT content_md, summary_md, cover_url, audio_url FROM articles`)
	scanQuery(`SELECT content_md, summary_md FROM article_revisions`)
	scanQuery(`SELECT content_md FROM pages`)
	scanQuery(`SELECT cover_url, description FROM series`)
	scanQuery(`SELECT cover_url, description FROM books`)
	scanQuery(`SELECT icon_url, description FROM links`)
	scanQuery(`SELECT avatar_url, bio FROM users`)
	scanQuery(`SELECT content_md, avatar_url FROM comments WHERE is_deleted = ?`, false)
	scanQuery(`SELECT value_json FROM settings`)
	scanQuery(`SELECT config_json FROM plugins`)
	scanQuery(`SELECT icon_url, description FROM follows`)
	scanQuery(`SELECT icon FROM nav_items`)
	return counts
}

func (s *AppServer) mediaTimeLocation() *time.Location {
	if s != nil && s.deps.Settings != nil {
		return theme.LoadSiteLocation(s.deps.Settings.Snapshot().Site.TimeZone)
	}
	return time.UTC
}

func (s *AppServer) queryAdminMediaPage(
	ctx context.Context,
	qStr string,
	mimeFilter string,
	monthFilter string,
	usageFilter string,
	orphanOnly bool,
	withRefs bool,
	sortBy string,
	page int,
	perPage int,
) ([]MediaAssetView, int, int, int64) {
	if page < 1 {
		page = 1
	}
	if perPage <= 0 {
		perPage = 50
	}
	if perPage > 200 {
		perPage = 200
	}

	where := []string{"1=1"}
	var args []any

	qStr = strings.TrimSpace(qStr)
	if qStr != "" {
		pat := "%" + escapeLikePattern(qStr) + "%"
		where = append(where, "(LOWER(original_name) LIKE LOWER(?) ESCAPE '!' OR LOWER(object_key) LIKE LOWER(?) ESCAPE '!' OR LOWER(public_url) LIKE LOWER(?) ESCAPE '!' OR LOWER(alt_text) LIKE LOWER(?) ESCAPE '!')")
		args = append(args, pat, pat, pat, pat)
	}

	mf := strings.ToLower(strings.TrimSpace(mimeFilter))
	if mf != "" && mf != "all" {
		switch mf {
		case "image", "images":
			where = append(where, "LOWER(mime_type) LIKE 'image/%'")
		case "audio":
			where = append(where, "LOWER(mime_type) LIKE 'audio/%'")
		case "other", "file", "files":
			where = append(where, "(LOWER(mime_type) NOT LIKE 'image/%' AND LOWER(mime_type) NOT LIKE 'audio/%')")
		default:
			where = append(where, "LOWER(mime_type) LIKE LOWER(?) ESCAPE '!'")
			args = append(args, escapeLikePattern(mf)+"%")
		}
	}

	monthClean := strings.TrimSpace(monthFilter)
	if len(monthClean) == 7 && monthClean[4] == '-' {
		if tStart, err := time.ParseInLocation("2006-01", monthClean, s.mediaTimeLocation()); err == nil {
			tEnd := tStart.AddDate(0, 1, 0)
			where = append(where, "(created_at >= ? AND created_at < ?)")
			args = append(args, tStart.UnixMilli(), tEnd.UnixMilli())
		}
	}

	orderClause := "created_at DESC"
	switch strings.ToLower(strings.TrimSpace(sortBy)) {
	case "size":
		orderClause = "size_bytes DESC, created_at DESC"
	case "name":
		orderClause = "original_name ASC, created_at DESC"
	case "oldest":
		orderClause = "created_at ASC"
	}

	whereSQL := strings.Join(where, " AND ")
	usage := strings.ToLower(strings.TrimSpace(usageFilter))
	if orphanOnly && usage == "" {
		usage = "unused"
	}

	if usage == "unused" || usage == "orphan" || usage == "used" {
		rows, _ := sqlx.Select[domain.MediaAsset](ctx, s.deps.Store,
			`SELECT * FROM media_assets WHERE `+whereSQL+` ORDER BY `+orderClause, args...)
		if len(rows) == 0 {
			return []MediaAssetView{}, 0, 0, 0
		}
		refCounts := s.countAssetsReferencesStreaming(ctx, rows)
		filtered := make([]MediaAssetView, 0, len(rows))
		orphanCnt := 0
		var totalBytes int64
		wantUsed := usage == "used"
		for i, a := range rows {
			rc := refCounts[i]
			isOrp := rc == 0
			if isOrp {
				orphanCnt++
			}
			if (wantUsed && rc > 0) || (!wantUsed && isOrp) {
				totalBytes += a.SizeBytes
				filtered = append(filtered, MediaAssetView{
					MediaAsset: a,
					RefCount:   rc,
					IsOrphan:   isOrp,
				})
			}
		}
		total := len(filtered)
		start := (page - 1) * perPage
		if start >= total {
			return []MediaAssetView{}, total, orphanCnt, totalBytes
		}
		end := start + perPage
		if end > total {
			end = total
		}
		return filtered[start:end], total, orphanCnt, totalBytes
	}

	var total int
	var totalBytes int64
	_ = s.deps.Store.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(size_bytes), 0) FROM media_assets WHERE `+whereSQL, args...).Scan(&total, &totalBytes)
	if total == 0 {
		return []MediaAssetView{}, 0, 0, 0
	}

	offset := (page - 1) * perPage
	baseSQL := `SELECT * FROM media_assets WHERE ` + whereSQL + ` ORDER BY ` + orderClause
	paginatedSQL, pageArgs := s.deps.Store.Dialect().PaginateSQL(baseSQL, perPage, offset)
	fullArgs := make([]any, 0, len(args)+len(pageArgs))
	fullArgs = append(fullArgs, args...)
	fullArgs = append(fullArgs, pageArgs...)

	rows, _ := sqlx.Select[domain.MediaAsset](ctx, s.deps.Store, paginatedSQL, fullArgs...)

	orphanCnt := 0
	var refCounts []int
	if withRefs {
		if len(rows) > 0 {
			refCounts = s.countAssetsReferencesStreaming(ctx, rows)
		}
		// Compute total orphan count across matching rows via lightweight key projection + cached token map
		tokenCounts := s.collectReferencedMediaTokensCached(ctx)
		keyRows, _ := sqlx.Select[struct {
			ObjectKey string `db:"object_key"`
			PublicURL string `db:"public_url"`
		}](ctx, s.deps.Store, `SELECT object_key, public_url FROM media_assets WHERE `+whereSQL, args...)
		for _, kr := range keyRows {
			if assetRefCountFromTokens(tokenCounts, domain.MediaAsset{ObjectKey: kr.ObjectKey, PublicURL: kr.PublicURL}) == 0 {
				orphanCnt++
			}
		}
	}

	if len(rows) == 0 {
		return []MediaAssetView{}, total, orphanCnt, totalBytes
	}

	out := make([]MediaAssetView, 0, len(rows))
	for i, a := range rows {
		rc := 0
		isOrp := false
		if withRefs {
			rc = refCounts[i]
			isOrp = rc == 0
		}
		out = append(out, MediaAssetView{
			MediaAsset: a,
			RefCount:   rc,
			IsOrphan:   isOrp,
		})
	}
	return out, total, orphanCnt, totalBytes
}

func (s *AppServer) listMediaWithRefs(ctx context.Context, limit int) ([]MediaAssetView, int) {
	items, _, orphanCnt, _ := s.queryAdminMediaPage(ctx, "", "", "", "all", false, true, "newest", 1, limit)
	return items, orphanCnt
}

func (s *AppServer) scanOrCleanOrphans(ctx context.Context, deleteOrphans bool) ([]domain.MediaAsset, int64, error) {
	if s.deps.Media != nil {
		orphans, err := s.deps.Media.ScanOrphans(ctx, deleteOrphans)
		if err != nil {
			return nil, 0, err
		}
		var freed int64
		if deleteOrphans {
			for _, o := range orphans {
				freed += o.SizeBytes
			}
		}
		if orphans == nil {
			orphans = []domain.MediaAsset{}
		}
		return orphans, freed, nil
	}

	rows, err := sqlx.Select[domain.MediaAsset](ctx, s.deps.Store, `SELECT * FROM media_assets ORDER BY created_at DESC`)
	if err != nil {
		return nil, 0, err
	}
	refCounts := s.countAssetsReferencesStreaming(ctx, rows)
	orphans := make([]domain.MediaAsset, 0)
	var freed int64
	for i, a := range rows {
		if refCounts[i] == 0 {
			orphans = append(orphans, a)
			if deleteOrphans {
				if a.StorageDriver == "local" && a.ObjectKey != "" && s.deps.Config != nil && s.deps.Config.UploadDir != "" {
					_ = safeUnlinkLocalAsset(s.deps.Config.UploadDir, a.ObjectKey)
				}
				_, _ = s.deps.Store.ExecContext(ctx, `DELETE FROM media_assets WHERE id = ?`, a.ID)
				freed += a.SizeBytes
			}
		}
	}
	return orphans, freed, nil
}

func safeUnlinkLocalAsset(uploadDir string, objectKey string) error {
	cleanUploadRoot, err := filepath.EvalSymlinks(uploadDir)
	if err != nil {
		return err
	}
	rel := filepath.Clean(filepath.FromSlash(strings.TrimLeft(objectKey, "/")))
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return nil
	}
	cand := filepath.Join(cleanUploadRoot, rel)
	info, lErr := os.Lstat(cand)
	if lErr != nil || info.Mode()&os.ModeSymlink != 0 {
		return nil
	}
	resolved, rErr := filepath.EvalSymlinks(cand)
	if rErr != nil {
		return rErr
	}
	if !strings.HasPrefix(resolved, cleanUploadRoot+string(os.PathSeparator)) {
		return nil
	}
	return os.Remove(resolved)
}

// CompareSemVer compares two semantic version strings numerically (`v1.10.0 > v1.9.0`, `v1.2.0 > v1.2.0-rc.1`).
// Returns 1 if a > b, -1 if a < b, and 0 if equal or non-comparable.
func CompareSemVer(a, b string) int {
	parse := func(raw string) ([3]int, string, bool) {
		s := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(raw), "v"))
		s = strings.TrimPrefix(s, "V")
		if idx := strings.IndexByte(s, '+'); idx >= 0 {
			s = s[:idx]
		}
		pre := ""
		if idx := strings.IndexByte(s, '-'); idx >= 0 {
			pre = s[idx+1:]
			s = s[:idx]
		}
		parts := strings.Split(s, ".")
		if len(parts) == 0 || len(parts) > 3 {
			return [3]int{}, "", false
		}
		var nums [3]int
		for i := 0; i < len(parts); i++ {
			n, err := strconv.Atoi(parts[i])
			if err != nil || n < 0 {
				return [3]int{}, "", false
			}
			nums[i] = n
		}
		return nums, pre, true
	}
	na, preA, okA := parse(a)
	nb, preB, okB := parse(b)
	if !okA || !okB {
		return 0
	}
	for i := 0; i < 3; i++ {
		if na[i] > nb[i] {
			return 1
		}
		if na[i] < nb[i] {
			return -1
		}
	}
	if preA == "" && preB != "" {
		return 1
	}
	if preA != "" && preB == "" {
		return -1
	}
	if preA == preB {
		return 0
	}
	if preA > preB {
		return 1
	}
	return -1
}

// Cached GitHub repository metadata for Admin About page (`/api/admin/about/project-info`)
var aboutProjectCache struct {
	mu        sync.Mutex
	fetchMu   sync.Mutex
	fetchedAt time.Time
	stars     int
	forks     int
	latestTag string
	htmlURL   string
	liveOK    bool
}

func (s *AppServer) handleAdminAboutProjectInfo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	ver := strings.TrimSpace(s.deps.Version)
	if ver == "" {
		ver = "v1.0.0"
	}
	repoURL := "https://github.com/Willh92/Volo-go"

	forceRefresh := r.URL.Query().Get("refresh") == "1" || r.URL.Query().Get("refresh") == "true"
	cacheTTL := 30 * time.Minute
	if forceRefresh {
		cacheTTL = 10 * time.Second
	}

	aboutProjectCache.mu.Lock()
	stale := time.Since(aboutProjectCache.fetchedAt) > cacheTTL
	stars := aboutProjectCache.stars
	forks := aboutProjectCache.forks
	latestTag := aboutProjectCache.latestTag
	liveOK := aboutProjectCache.liveOK
	aboutProjectCache.mu.Unlock()

	if stale && r.URL.Query().Get("offline") != "1" && s.deps.OutboundDialer != nil {
		aboutProjectCache.fetchMu.Lock()
		aboutProjectCache.mu.Lock()
		stillStale := time.Since(aboutProjectCache.fetchedAt) > cacheTTL
		stars = aboutProjectCache.stars
		forks = aboutProjectCache.forks
		latestTag = aboutProjectCache.latestTag
		liveOK = aboutProjectCache.liveOK
		aboutProjectCache.mu.Unlock()

		if stillStale {
			ctx, cancel := context.WithTimeout(r.Context(), 3500*time.Millisecond)
			cli := s.deps.OutboundDialer.NewHTTPClient(3500 * time.Millisecond)
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/repos/Willh92/Volo-go", nil)
			if err == nil {
				req.Header.Set("Accept", "application/vnd.github+json")
				req.Header.Set("User-Agent", "Volo-go/"+ver)
				if resp, rErr := cli.Do(req); rErr == nil {
					var meta struct {
						StargazersCount int    `json:"stargazers_count"`
						ForksCount      int    `json:"forks_count"`
						HTMLURL         string `json:"html_url"`
					}
					if resp.StatusCode == http.StatusOK && json.NewDecoder(io.LimitReader(resp.Body, 65536)).Decode(&meta) == nil {
						stars = meta.StargazersCount
						forks = meta.ForksCount
						liveOK = true
					}
					_ = resp.Body.Close()
				}
			}
			relReq, relErr := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/repos/Willh92/Volo-go/releases/latest", nil)
			if relErr == nil {
				relReq.Header.Set("Accept", "application/vnd.github+json")
				relReq.Header.Set("User-Agent", "Volo-go/"+ver)
				if resp, rErr := cli.Do(relReq); rErr == nil {
					var rel struct {
						TagName string `json:"tag_name"`
					}
					if resp.StatusCode == http.StatusOK && json.NewDecoder(io.LimitReader(resp.Body, 65536)).Decode(&rel) == nil && strings.TrimSpace(rel.TagName) != "" {
						latestTag = strings.TrimSpace(rel.TagName)
						liveOK = true
					}
					_ = resp.Body.Close()
				}
			}
			cancel()

			aboutProjectCache.mu.Lock()
			aboutProjectCache.fetchedAt = time.Now()
			aboutProjectCache.stars = stars
			aboutProjectCache.forks = forks
			aboutProjectCache.latestTag = latestTag
			aboutProjectCache.liveOK = liveOK
			aboutProjectCache.mu.Unlock()
		}
		aboutProjectCache.fetchMu.Unlock()
	}

	if latestTag == "" {
		latestTag = ver
	}
	hasUpdate := latestTag != "" && CompareSemVer(latestTag, ver) > 0

	writeJSON(w, http.StatusOK, map[string]any{
		"project_name":   "Volo-go",
		"version":        ver,
		"latest_version": latestTag,
		"has_update":     hasUpdate,
		"go_version":     runtime.Version(),
		"os_arch":        runtime.GOOS + "/" + runtime.GOARCH,
		"db_dialect":     s.deps.Store.Dialect().Name(),
		"repo_url":       repoURL,
		"issues_url":     repoURL + "/issues",
		"releases_url":   repoURL + "/releases",
		"license":        "Apache-2.0",
		"stars":          stars,
		"forks":          forks,
		"live_fetched":   liveOK,
	})
}
