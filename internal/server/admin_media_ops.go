package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"io/fs"
	"math"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Willh92/Volo-go/internal/adminui"
	"github.com/Willh92/Volo-go/internal/db/sqlx"
	"github.com/Willh92/Volo-go/internal/domain"
	"github.com/Willh92/Volo-go/internal/extpkg"
	"github.com/Willh92/Volo-go/internal/media"
	"github.com/Willh92/Volo-go/internal/security"
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
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".avif", ".mp3", ".m4a", ".ogg", ".wav", ".mp4", ".webm", ".pdf", ".zip":
		return true
	default:
		return false
	}
}

func defaultReconcileMIMEType(ext string, sample []byte) string {
	switch strings.ToLower(ext) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".avif":
		return "image/avif"
	case ".mp3":
		return "audio/mpeg"
	case ".m4a":
		return "audio/mp4"
	case ".ogg":
		return "audio/ogg"
	case ".wav":
		return "audio/wav"
	case ".weba":
		return "audio/webm"
	case ".mp4":
		return "video/mp4"
	case ".webm":
		return "video/webm"
	case ".pdf":
		return "application/pdf"
	case ".zip":
		return "application/zip"
	}
	mt := mime.TypeByExtension(ext)
	if mt == "" && len(sample) > 0 {
		mt = http.DetectContentType(sample)
	}
	if idx := strings.IndexByte(mt, ';'); idx > 0 {
		mt = strings.TrimSpace(mt[:idx])
	}
	return strings.ToLower(strings.TrimSpace(mt))
}

// reconcileLocalUploads scans UploadDir on explicit operator request (`POST /api/admin/media/reconcile`)
// and registers untracked valid media files in `media_assets`, normalizing EXIF orientation & stripping
// image metadata on disk via `media.NormalizeLocalImageBytes` and returning the number of newly imported rows.
func (s *AppServer) reconcileLocalUploads(ctx context.Context) int {
	if s.deps.Config == nil || strings.TrimSpace(s.deps.Config.GetUploadDir()) == "" {
		return 0
	}
	rootAbs, err := filepath.Abs(s.deps.Config.GetUploadDir())
	if err != nil {
		return 0
	}
	st, err := os.Stat(rootAbs)
	if err != nil || !st.IsDir() {
		return 0
	}

	existingKeys := make(map[string]struct{})
	var rows []struct {
		ObjectKey string `db:"object_key"`
		PublicURL string `db:"public_url"`
	}
	err = sqlx.SelectInto(ctx, s.deps.Store, &rows, `SELECT object_key, public_url FROM media_assets`)
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
		var (
			mimeType     string
			width        int
			height       int
			sumHex       string
			exifStripped bool
		)

		isRasterImage := ext == ".jpg" || ext == ".jpeg" || ext == ".png" || ext == ".webp" || ext == ".gif"
		if isRasterImage {
			if fileSize > (32 << 20) {
				return nil
			}
			rawBytes, rErr := os.ReadFile(fullPath)
			if rErr != nil {
				return nil
			}
			cleanBytes, normMIME, w, h, changed, nErr := media.NormalizeLocalImageBytes(rawBytes, ext)
			if nErr != nil {
				return nil
			}
			mimeType = strings.ToLower(strings.TrimSpace(normMIME))
			width, height = w, h
			exifStripped = ext != ".gif"
			finalBytes := rawBytes
			if changed && len(cleanBytes) > 0 {
				tmpPath := fullPath + ".volostrip." + strings.ToLower(id.Next())
				if wErr := os.WriteFile(tmpPath, cleanBytes, info.Mode().Perm()); wErr == nil {
					if renErr := os.Rename(tmpPath, fullPath); renErr == nil {
						finalBytes = cleanBytes
					} else {
						_ = os.Remove(tmpPath)
					}
				}
			}
			fileSize = int64(len(finalBytes))
			sum := sha256.Sum256(finalBytes)
			sumHex = hex.EncodeToString(sum[:])
		} else {
			f, oErr := os.Open(fullPath)
			if oErr != nil {
				return nil
			}
			var head [512]byte
			n, _ := io.ReadFull(f, head[:])
			mimeType = defaultReconcileMIMEType(ext, head[:n])
			_, _ = f.Seek(0, io.SeekStart)
			hasher := sha256.New()
			written, _ := io.Copy(hasher, f)
			_ = f.Close()
			if written > 0 {
				fileSize = written
			}
			sumHex = hex.EncodeToString(hasher.Sum(nil))
		}

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

func (s *AppServer) invalidateMediaRefCache() {
	if s == nil {
		return
	}
	s.mediaRefCacheMu.Lock()
	s.mediaRefInvalidateGen++
	s.mediaRefCachedRev = 0
	s.mediaRefCountsByAssetID = nil
	s.mediaRefOrphanCounts = nil
	s.mediaRefCacheMu.Unlock()
}

func (s *AppServer) ensureMediaRefFetchSem() chan struct{} {
	s.mediaRefFetchOnce.Do(func() {
		if s.mediaRefFetchSem == nil {
			s.mediaRefFetchSem = make(chan struct{}, 1)
		}
	})
	return s.mediaRefFetchSem
}

func mediaAssetRefLookupKey(a domain.MediaAsset) string {
	mk := strings.TrimSpace(a.ID)
	if mk == "" {
		mk = strings.TrimSpace(a.PublicURL)
	}
	if mk == "" {
		mk = strings.TrimLeft(strings.TrimSpace(a.ObjectKey), "/")
	}
	return mk
}

// collectMediaRefCountsCached computes per-asset reference counts via `media.CountAssetReferences`,
// caching only compact `map[assetID]int` keyed by `store.DataRevision()` so prose tokens are never retained on heap.
// Concurrent misses at the same revision are deduplicated via a context-aware singleflight semaphore, and
// monotonic revision + invalidation generation guards prevent stale scans from overwriting newer state.
func (s *AppServer) collectMediaRefCountsCached(ctx context.Context) (map[string]int, uint64) {
	if s == nil || s.deps.Store == nil {
		return map[string]int{}, 0
	}
	curRev := s.deps.Store.DataRevision()
	s.mediaRefCacheMu.Lock()
	if s.mediaRefCountsByAssetID != nil && curRev > 0 && s.mediaRefCachedRev == curRev {
		cached := s.mediaRefCountsByAssetID
		s.mediaRefCacheMu.Unlock()
		return cached, curRev
	}
	s.mediaRefCacheMu.Unlock()

	sem := s.ensureMediaRefFetchSem()
	select {
	case sem <- struct{}{}:
		defer func() { <-sem }()
	case <-ctx.Done():
		return map[string]int{}, 0
	}

	curRev = s.deps.Store.DataRevision()
	s.mediaRefCacheMu.Lock()
	if s.mediaRefCountsByAssetID != nil && curRev > 0 && s.mediaRefCachedRev == curRev {
		cached := s.mediaRefCountsByAssetID
		s.mediaRefCacheMu.Unlock()
		return cached, curRev
	}
	gen := s.mediaRefInvalidateGen
	s.mediaRefCacheMu.Unlock()

	var allAssets []domain.MediaAsset
	selErr := sqlx.SelectInto(ctx, s.deps.Store, &allAssets,
		`SELECT id, storage_driver, object_key, public_url FROM media_assets`)
	if selErr != nil {
		return map[string]int{}, 0
	}

	refCounts, err := media.CountAssetReferences(ctx, s.deps.Store, allAssets)
	if err != nil {
		return map[string]int{}, 0
	}
	if refCounts == nil {
		refCounts = map[string]int{}
	}

	latestRev := s.deps.Store.DataRevision()
	var committedRev uint64
	if curRev > 0 && latestRev == curRev {
		s.mediaRefCacheMu.Lock()
		if s.mediaRefInvalidateGen == gen && curRev >= s.mediaRefCachedRev {
			if s.mediaRefCachedRev != curRev {
				s.mediaRefOrphanCounts = make(map[string]int)
			}
			s.mediaRefCachedRev = curRev
			s.mediaRefCountsByAssetID = refCounts
			committedRev = curRev
		}
		s.mediaRefCacheMu.Unlock()
	}
	return refCounts, committedRev
}

func (s *AppServer) countAssetsReferences(ctx context.Context, assets []domain.MediaAsset) []int {
	counts := make([]int, len(assets))
	if len(assets) == 0 || s == nil || s.deps.Store == nil {
		return counts
	}
	cachedByKey, _ := s.collectMediaRefCountsCached(ctx)
	var missing []domain.MediaAsset
	var missingIdx []int
	for i, a := range assets {
		mk := mediaAssetRefLookupKey(a)
		if cnt, ok := cachedByKey[mk]; ok {
			counts[i] = cnt
		} else {
			missing = append(missing, a)
			missingIdx = append(missingIdx, i)
		}
	}
	if len(missing) > 0 {
		if fallback, err := media.CountAssetReferences(ctx, s.deps.Store, missing); err == nil {
			for j, idx := range missingIdx {
				counts[idx] = fallback[mediaAssetRefLookupKey(missing[j])]
			}
		}
	}
	return counts
}

func (s *AppServer) cachedOrComputeOrphanCount(
	ctx context.Context,
	whereSQL string,
	args []any,
	cacheScopeKey string,
	canCache bool,
) int {
	refCounts, refRev := s.collectMediaRefCountsCached(ctx)
	if canCache && refRev > 0 {
		s.mediaRefCacheMu.Lock()
		if s.mediaRefCachedRev == refRev && s.mediaRefOrphanCounts != nil {
			if cnt, ok := s.mediaRefOrphanCounts[cacheScopeKey]; ok {
				s.mediaRefCacheMu.Unlock()
				return cnt
			}
		}
		s.mediaRefCacheMu.Unlock()
	}

	orphanCnt := 0
	if canCache && cacheScopeKey == "global" && refRev > 0 {
		// Global unfiltered scope: refCounts already contains every media_assets row at refRev.
		for _, rc := range refCounts {
			if rc == 0 {
				orphanCnt++
			}
		}
	} else if rows, qErr := s.deps.Store.QueryContext(ctx, `SELECT id, object_key, public_url FROM media_assets WHERE `+whereSQL, args...); qErr == nil {
		defer rows.Close()
		for rows.Next() {
			var rowID, objKey, pubURL string
			if scanErr := rows.Scan(&rowID, &objKey, &pubURL); scanErr != nil {
				continue
			}
			mk := mediaAssetRefLookupKey(domain.MediaAsset{
				ID:        rowID,
				ObjectKey: objKey,
				PublicURL: pubURL,
			})
			if refCounts[mk] == 0 {
				orphanCnt++
			}
		}
	}

	if canCache && refRev > 0 && s.deps.Store.DataRevision() == refRev {
		s.mediaRefCacheMu.Lock()
		if s.mediaRefCachedRev == refRev {
			if s.mediaRefOrphanCounts == nil {
				s.mediaRefOrphanCounts = make(map[string]int)
			}
			s.mediaRefOrphanCounts[cacheScopeKey] = orphanCnt
		}
		s.mediaRefCacheMu.Unlock()
	}
	return orphanCnt
}

func (s *AppServer) mediaTimeLocation() *time.Location {
	if s != nil && s.deps.Settings != nil {
		return theme.LoadSiteLocation(s.deps.Settings.Snapshot().Site.TimeZone)
	}
	return time.UTC
}

func (s *AppServer) queryAdminMediaPage(
	ctx context.Context,
	actor *domain.User,
	qStr string,
	mimeFilter string,
	monthFilter string,
	usageFilter string,
	orphanOnly bool,
	withRefs bool,
	sortBy string,
	page int,
	perPage int,
) ([]MediaAssetView, int, int, int, int64) {
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
	scopeKey := "global"

	var allTotal int
	if actor != nil && actor.Role != domain.RoleAdmin {
		where = append(where, "uploader_id = ?")
		args = append(args, actor.ID)
		scopeKey = "uploader:" + actor.ID
		_ = s.deps.Store.QueryRowContext(ctx, `SELECT COUNT(*) FROM media_assets WHERE uploader_id = ?`, actor.ID).Scan(&allTotal)
	} else {
		_ = s.deps.Store.QueryRowContext(ctx, `SELECT COUNT(*) FROM media_assets`).Scan(&allTotal)
	}

	hasExtraFilters := false
	qStr = strings.TrimSpace(qStr)
	if qStr != "" {
		hasExtraFilters = true
		pat := "%" + escapeLikePattern(qStr) + "%"
		where = append(where, "(LOWER(original_name) LIKE LOWER(?) ESCAPE '!' OR LOWER(object_key) LIKE LOWER(?) ESCAPE '!' OR LOWER(public_url) LIKE LOWER(?) ESCAPE '!' OR LOWER(alt_text) LIKE LOWER(?) ESCAPE '!')")
		args = append(args, pat, pat, pat, pat)
	}

	mf := strings.ToLower(strings.TrimSpace(mimeFilter))
	if mf != "" && mf != "all" {
		hasExtraFilters = true
		switch mf {
		case "image", "images":
			where = append(where, "mime_type LIKE 'image/%'")
		case "audio":
			where = append(where, "mime_type LIKE 'audio/%'")
		case "video", "videos":
			where = append(where, "mime_type LIKE 'video/%'")
		case "other", "file", "files":
			where = append(where, "(mime_type NOT LIKE 'image/%' AND mime_type NOT LIKE 'audio/%' AND mime_type NOT LIKE 'video/%')")
		default:
			where = append(where, "mime_type LIKE ? ESCAPE '!'")
			args = append(args, escapeLikePattern(mf)+"%")
		}
	}

	monthClean := strings.TrimSpace(monthFilter)
	if len(monthClean) == 7 && monthClean[4] == '-' {
		if tStart, err := time.ParseInLocation("2006-01", monthClean, s.mediaTimeLocation()); err == nil {
			hasExtraFilters = true
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
		refCounts, _ := s.collectMediaRefCountsCached(ctx)
		rows, qErr := s.deps.Store.QueryContext(ctx,
			`SELECT id, storage_driver, object_key, public_url, size_bytes FROM media_assets WHERE `+whereSQL+` ORDER BY `+orderClause, args...)
		if qErr != nil {
			return []MediaAssetView{}, 0, allTotal, 0, 0
		}

		type matchedKey struct {
			id  string
			rc  int
			orp bool
		}
		wantUsed := usage == "used"
		start := math.MaxInt
		end := math.MaxInt
		if page >= 1 && perPage > 0 && (page-1) <= math.MaxInt/perPage {
			start = (page - 1) * perPage
			end = start + perPage
			if end < start {
				end = math.MaxInt
			}
		}
		capHint := 50
		if perPage > 0 && perPage <= 200 {
			capHint = perPage
		} else if perPage > 200 {
			capHint = 200
		}
		pageMatched := make([]matchedKey, 0, capHint)
		total := 0
		orphanCnt := 0
		var totalBytes int64

		for rows.Next() {
			var rowID, drv, objKey, pubURL string
			var sizeBytes int64
			if scanErr := rows.Scan(&rowID, &drv, &objKey, &pubURL, &sizeBytes); scanErr != nil {
				continue
			}
			asset := domain.MediaAsset{
				ID:            rowID,
				StorageDriver: drv,
				ObjectKey:     objKey,
				PublicURL:     pubURL,
			}
			mk := mediaAssetRefLookupKey(asset)
			rc, ok := refCounts[mk]
			if !ok {
				if fb, fbErr := media.CountAssetReferences(ctx, s.deps.Store, []domain.MediaAsset{asset}); fbErr == nil {
					rc = fb[mk]
				}
			}
			isOrp := rc == 0
			if isOrp {
				orphanCnt++
			}
			if (wantUsed && rc > 0) || (!wantUsed && isOrp) {
				totalBytes += sizeBytes
				if total >= start && total < end {
					pageMatched = append(pageMatched, matchedKey{id: rowID, rc: rc, orp: isOrp})
				}
				total++
			}
		}
		_ = rows.Close()

		if len(pageMatched) == 0 {
			return []MediaAssetView{}, total, allTotal, orphanCnt, totalBytes
		}
		placeholders := make([]string, len(pageMatched))
		idArgs := make([]any, len(pageMatched))
		for i, m := range pageMatched {
			placeholders[i] = "?"
			idArgs[i] = m.id
		}
		var fullRows []domain.MediaAsset
		_ = sqlx.SelectInto(ctx, s.deps.Store, &fullRows,
			`SELECT * FROM media_assets WHERE id IN (`+strings.Join(placeholders, ",")+`)`, idArgs...)
		byID := make(map[string]domain.MediaAsset, len(fullRows))
		for _, r := range fullRows {
			byID[r.ID] = r
		}
		out := make([]MediaAssetView, 0, len(pageMatched))
		for _, m := range pageMatched {
			if full, ok := byID[m.id]; ok {
				out = append(out, MediaAssetView{
					MediaAsset: full,
					RefCount:   m.rc,
					IsOrphan:   m.orp,
				})
			}
		}
		return out, total, allTotal, orphanCnt, totalBytes
	}

	var total int
	var totalBytes int64
	_ = s.deps.Store.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(size_bytes), 0) FROM media_assets WHERE `+whereSQL, args...).Scan(&total, &totalBytes)
	if total == 0 {
		return []MediaAssetView{}, 0, allTotal, 0, 0
	}

	var rows []domain.MediaAsset
	if (page-1) <= (total-1)/perPage {
		baseSQL := `SELECT * FROM media_assets WHERE ` + whereSQL + ` ORDER BY ` + orderClause
		paginatedSQL, fullArgs := s.paginateQuery(baseSQL, args, page, perPage)
		_ = sqlx.SelectInto(ctx, s.deps.Store, &rows, paginatedSQL, fullArgs...)
	}

	orphanCnt := 0
	var refCounts []int
	if withRefs {
		if len(rows) > 0 {
			refCounts = s.countAssetsReferences(ctx, rows)
		}
		orphanCnt = s.cachedOrComputeOrphanCount(ctx, whereSQL, args, scopeKey, !hasExtraFilters)
	}

	if len(rows) == 0 {
		return []MediaAssetView{}, total, allTotal, orphanCnt, totalBytes
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
	return out, total, allTotal, orphanCnt, totalBytes
}

func (s *AppServer) listMediaWithRefs(ctx context.Context, limit int) ([]MediaAssetView, int) {
	items, _, _, orphanCnt, _ := s.queryAdminMediaPage(ctx, nil, "", "", "", "all", false, true, "newest", 1, limit)
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

	var rows []domain.MediaAsset
	err := sqlx.SelectInto(ctx, s.deps.Store, &rows, `SELECT * FROM media_assets ORDER BY created_at DESC`)
	if err != nil {
		return nil, 0, err
	}
	refCounts := s.countAssetsReferences(ctx, rows)
	orphans := make([]domain.MediaAsset, 0)
	var freed int64
	for i, a := range rows {
		if refCounts[i] == 0 {
			orphans = append(orphans, a)
			if deleteOrphans {
				if a.StorageDriver == "local" && a.ObjectKey != "" && s.deps.Config != nil {
					if curDir := s.deps.Config.GetUploadDir(); curDir != "" {
						_ = safeUnlinkLocalAsset(curDir, a.ObjectKey)
						if defDir := s.deps.Config.GetDefaultUploadDir(); defDir != "" && defDir != curDir {
							_ = safeUnlinkLocalAsset(defDir, a.ObjectKey)
						}
					}
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
type aboutProjectCacheState struct {
	mu            sync.Mutex
	inflight      chan struct{}
	fetchedAt     time.Time
	repoFetchedAt time.Time
	repoOK        bool
	relFetchedAt  time.Time
	relOK         bool
	stars         int
	forks         int
	openIssues    int
	repoFullName  string
	repoDesc      string
	repoLang      string
	htmlURL       string
	latestTag     string
	latestName    string
	latestHTMLURL string
	latestPubAt   string
	liveOK        bool
}

// SetAboutGitHubURLsForTest overrides the GitHub API endpoints used by /api/admin/about/project-info in tests.
func (s *AppServer) SetAboutGitHubURLsForTest(repoURL, releaseURL string) {
	if s == nil {
		return
	}
	s.aboutCache.mu.Lock()
	defer s.aboutCache.mu.Unlock()
	s.aboutRepoAPIURL = strings.TrimSpace(repoURL)
	s.aboutReleaseAPIURL = strings.TrimSpace(releaseURL)
}

// SetAboutClockForTest overrides the clock used by /api/admin/about/project-info cache TTL checks in tests.
func (s *AppServer) SetAboutClockForTest(fn func() time.Time) {
	if s == nil {
		return
	}
	s.aboutCache.mu.Lock()
	defer s.aboutCache.mu.Unlock()
	s.aboutNowFn = fn
}

func isAboutPartStale(attemptedAt time.Time, ok, forceRefresh bool, now time.Time) bool {
	if attemptedAt.IsZero() {
		return true
	}
	ttl := 30 * time.Minute
	if !ok {
		ttl = 2 * time.Minute
	}
	if forceRefresh {
		ttl = 10 * time.Second
	}
	return now.Sub(attemptedAt) > ttl
}

func (s *AppServer) handleAdminAboutProjectInfo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	ver := s.effectiveAppVersion()
	repoURL := "https://github.com/Willh92/Volo-go"

	forceRefresh := r.URL.Query().Get("refresh") == "1" || r.URL.Query().Get("refresh") == "true"
	offline := r.URL.Query().Get("offline") == "1"

	var (
		fetchedAt     time.Time
		stars         int
		forks         int
		openIssues    int
		repoFullName  string
		repoDesc      string
		repoLang      string
		htmlURL       string
		latestTag     string
		latestName    string
		latestHTMLURL string
		latestPubAt   string
		liveOK        bool
	)
	snapshotLocked := func() {
		fetchedAt = s.aboutCache.fetchedAt
		stars = s.aboutCache.stars
		forks = s.aboutCache.forks
		openIssues = s.aboutCache.openIssues
		repoFullName = s.aboutCache.repoFullName
		repoDesc = s.aboutCache.repoDesc
		repoLang = s.aboutCache.repoLang
		htmlURL = s.aboutCache.htmlURL
		latestTag = s.aboutCache.latestTag
		latestName = s.aboutCache.latestName
		latestHTMLURL = s.aboutCache.latestHTMLURL
		latestPubAt = s.aboutCache.latestPubAt
		liveOK = s.aboutCache.liveOK
	}

	for attempts := 0; attempts < 2; attempts++ {
		s.aboutCache.mu.Lock()
		nowFn := s.aboutNowFn
		now := time.Now()
		if nowFn != nil {
			now = nowFn()
		}
		needRepo := isAboutPartStale(s.aboutCache.repoFetchedAt, s.aboutCache.repoOK, forceRefresh, now)
		needRel := isAboutPartStale(s.aboutCache.relFetchedAt, s.aboutCache.relOK, forceRefresh, now)
		if (!needRepo && !needRel) || offline || s.deps.OutboundDialer == nil {
			s.aboutCache.mu.Unlock()
			break
		}
		if ch := s.aboutCache.inflight; ch != nil {
			s.aboutCache.mu.Unlock()
			select {
			case <-ch:
			case <-r.Context().Done():
				return
			}
			continue
		}
		ch := make(chan struct{})
		s.aboutCache.inflight = ch
		repoEndpoint := s.aboutRepoAPIURL
		if repoEndpoint == "" {
			repoEndpoint = "https://api.github.com/repos/Willh92/Volo-go"
		}
		relEndpoint := s.aboutReleaseAPIURL
		if relEndpoint == "" {
			relEndpoint = "https://api.github.com/repos/Willh92/Volo-go/releases/latest"
		}
		dialer := s.deps.OutboundDialer
		baseCtx := s.bgCtx
		if baseCtx == nil {
			baseCtx = context.Background()
		}
		s.aboutCache.mu.Unlock()

		s.bgWG.Add(1)
		go func(needRepo, needRel bool, repoEndpoint, relEndpoint string, dialer *security.OutboundSafeDialer, baseCtx context.Context, nowFn func() time.Time, ch chan struct{}) {
			defer s.bgWG.Done()
			ctx, cancel := context.WithTimeout(baseCtx, 6*time.Second)
			defer cancel()

			cli := dialer.NewHTTPClient(6 * time.Second)
			var (
				wg         sync.WaitGroup
				repoOK     bool
				relOK      bool
				nextStars  int
				nextForks  int
				nextIssues int
				nextFull   string
				nextDesc   string
				nextLang   string
				nextHTML   string
				nextTag    string
				nextName   string
				nextRelURL string
				nextPubAt  string
			)

			defer func() {
				fetchNow := time.Now()
				if nowFn != nil {
					fetchNow = nowFn()
				}
				s.aboutCache.mu.Lock()
				if needRepo {
					s.aboutCache.repoFetchedAt = fetchNow
					s.aboutCache.repoOK = repoOK
					if repoOK {
						s.aboutCache.stars = nextStars
						s.aboutCache.forks = nextForks
						s.aboutCache.openIssues = nextIssues
						if nextFull != "" {
							s.aboutCache.repoFullName = nextFull
						}
						if nextDesc != "" {
							s.aboutCache.repoDesc = nextDesc
						}
						if nextLang != "" {
							s.aboutCache.repoLang = nextLang
						}
						if nextHTML != "" {
							s.aboutCache.htmlURL = nextHTML
						}
					}
				}
				if needRel {
					s.aboutCache.relFetchedAt = fetchNow
					s.aboutCache.relOK = relOK
					if relOK {
						s.aboutCache.latestTag = nextTag
						s.aboutCache.latestName = nextName
						if nextRelURL != "" {
							s.aboutCache.latestHTMLURL = nextRelURL
						}
						s.aboutCache.latestPubAt = nextPubAt
					}
				}
				s.aboutCache.fetchedAt = fetchNow
				s.aboutCache.liveOK = s.aboutCache.repoOK && s.aboutCache.relOK
				s.aboutCache.inflight = nil
				close(ch)
				s.aboutCache.mu.Unlock()
			}()

			if needRepo {
				wg.Add(1)
				go func() {
					defer wg.Done()
					req, err := http.NewRequestWithContext(ctx, http.MethodGet, repoEndpoint, nil)
					if err != nil {
						return
					}
					req.Header.Set("Accept", "application/vnd.github+json")
					req.Header.Set("User-Agent", "Volo-go/"+ver)
					resp, rErr := cli.Do(req)
					if rErr != nil {
						return
					}
					defer resp.Body.Close()
					var meta struct {
						FullName        string `json:"full_name"`
						Description     string `json:"description"`
						Language        string `json:"language"`
						StargazersCount int    `json:"stargazers_count"`
						ForksCount      int    `json:"forks_count"`
						OpenIssuesCount int    `json:"open_issues_count"`
						HTMLURL         string `json:"html_url"`
					}
					if resp.StatusCode == http.StatusOK && json.NewDecoder(io.LimitReader(resp.Body, 65536)).Decode(&meta) == nil {
						nextStars = meta.StargazersCount
						nextForks = meta.ForksCount
						nextIssues = meta.OpenIssuesCount
						nextFull = strings.TrimSpace(meta.FullName)
						nextDesc = strings.TrimSpace(meta.Description)
						nextLang = strings.TrimSpace(meta.Language)
						nextHTML = extpkg.SanitizeHTTPURL(meta.HTMLURL)
						repoOK = true
					}
				}()
			}

			if needRel {
				wg.Add(1)
				go func() {
					defer wg.Done()
					relReq, relErr := http.NewRequestWithContext(ctx, http.MethodGet, relEndpoint, nil)
					if relErr != nil {
						return
					}
					relReq.Header.Set("Accept", "application/vnd.github+json")
					relReq.Header.Set("User-Agent", "Volo-go/"+ver)
					resp, rErr := cli.Do(relReq)
					if rErr != nil {
						return
					}
					defer resp.Body.Close()
					var rel struct {
						TagName     string `json:"tag_name"`
						Name        string `json:"name"`
						HTMLURL     string `json:"html_url"`
						PublishedAt string `json:"published_at"`
					}
					if resp.StatusCode == http.StatusOK && json.NewDecoder(io.LimitReader(resp.Body, 65536)).Decode(&rel) == nil && strings.TrimSpace(rel.TagName) != "" {
						nextTag = strings.TrimSpace(rel.TagName)
						nextName = strings.TrimSpace(rel.Name)
						nextRelURL = extpkg.SanitizeHTTPURL(rel.HTMLURL)
						nextPubAt = strings.TrimSpace(rel.PublishedAt)
						relOK = true
					}
				}()
			}

			wg.Wait()
		}(needRepo, needRel, repoEndpoint, relEndpoint, dialer, baseCtx, nowFn, ch)

		select {
		case <-ch:
		case <-r.Context().Done():
			return
		}
		break
	}

	s.aboutCache.mu.Lock()
	snapshotLocked()
	s.aboutCache.mu.Unlock()

	if repoFullName == "" {
		repoFullName = "Willh92/Volo-go"
	}
	if htmlURL == "" {
		htmlURL = repoURL
	}
	if repoLang == "" {
		repoLang = "Go"
	}
	if latestTag == "" {
		latestTag = ver
	}
	if latestHTMLURL == "" {
		latestHTMLURL = repoURL + "/releases"
	}
	hasUpdate := latestTag != "" && CompareSemVer(latestTag, ver) > 0
	adminGzipKB := math.Round(float64(adminui.Default().TotalGzipBytes())/1024.0*10) / 10
	checkedAtMS := int64(0)
	if !fetchedAt.IsZero() {
		checkedAtMS = fetchedAt.UnixMilli()
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"project_name":   "Volo-go",
		"version":        ver,
		"latest_version": latestTag,
		"has_update":     hasUpdate,
		"go_version":     runtime.Version(),
		"os_arch":        runtime.GOOS + "/" + runtime.GOARCH,
		"db_dialect":     s.deps.Store.Dialect().Name(),
		"admin_gzip_kb":  adminGzipKB,
		"online":         liveOK,
		"checked_at":     checkedAtMS,
		"repo": map[string]any{
			"full_name":   repoFullName,
			"html_url":    htmlURL,
			"description": repoDesc,
			"stars":       stars,
			"forks":       forks,
			"open_issues": openIssues,
			"language":    repoLang,
			"license":     "Apache-2.0",
		},
		"latest_release": map[string]any{
			"tag_name":     latestTag,
			"name":         latestName,
			"html_url":     latestHTMLURL,
			"published_at": latestPubAt,
		},
		"repo_url":     htmlURL,
		"issues_url":   repoURL + "/issues",
		"releases_url": latestHTMLURL,
		"license":      "Apache-2.0",
		"stars":        stars,
		"forks":        forks,
		"open_issues":  openIssues,
		"live_fetched": liveOK,
	})
}
