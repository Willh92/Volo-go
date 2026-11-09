package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	"github.com/Willh92/Volo-go/internal/media"
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
	s.mediaRefCachedRev = 0
	s.mediaRefCountsByAssetID = nil
	s.mediaRefOrphanCounts = nil
	s.mediaRefCacheMu.Unlock()
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
func (s *AppServer) collectMediaRefCountsCached(ctx context.Context) map[string]int {
	if s == nil || s.deps.Store == nil {
		return map[string]int{}
	}
	curRev := s.deps.Store.DataRevision()
	s.mediaRefCacheMu.Lock()
	if s.mediaRefCountsByAssetID != nil && curRev > 0 && s.mediaRefCachedRev == curRev {
		cached := s.mediaRefCountsByAssetID
		s.mediaRefCacheMu.Unlock()
		return cached
	}
	s.mediaRefCacheMu.Unlock()

	var allAssets []domain.MediaAsset
	_ = sqlx.SelectInto(ctx, s.deps.Store, &allAssets,
		`SELECT id, storage_driver, object_key, public_url FROM media_assets`)

	refCounts, err := media.CountAssetReferences(ctx, s.deps.Store, allAssets)
	if err != nil || refCounts == nil {
		refCounts = map[string]int{}
	}

	s.mediaRefCacheMu.Lock()
	if s.mediaRefCachedRev != curRev {
		s.mediaRefOrphanCounts = make(map[string]int)
	}
	s.mediaRefCachedRev = curRev
	s.mediaRefCountsByAssetID = refCounts
	s.mediaRefCacheMu.Unlock()
	return refCounts
}

func (s *AppServer) countAssetsReferences(ctx context.Context, assets []domain.MediaAsset) []int {
	counts := make([]int, len(assets))
	if len(assets) == 0 || s == nil || s.deps.Store == nil {
		return counts
	}
	cachedByKey := s.collectMediaRefCountsCached(ctx)
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
	refCounts := s.collectMediaRefCountsCached(ctx)
	curRev := s.deps.Store.DataRevision()
	if canCache && curRev > 0 {
		s.mediaRefCacheMu.Lock()
		if s.mediaRefCachedRev == curRev && s.mediaRefOrphanCounts != nil {
			if cnt, ok := s.mediaRefOrphanCounts[cacheScopeKey]; ok {
				s.mediaRefCacheMu.Unlock()
				return cnt
			}
		}
		s.mediaRefCacheMu.Unlock()
	}

	var keyRows []struct {
		ID        string `db:"id"`
		ObjectKey string `db:"object_key"`
		PublicURL string `db:"public_url"`
	}
	_ = sqlx.SelectInto(ctx, s.deps.Store, &keyRows, `SELECT id, object_key, public_url FROM media_assets WHERE `+whereSQL, args...)
	orphanCnt := 0
	for _, kr := range keyRows {
		mk := mediaAssetRefLookupKey(domain.MediaAsset{
			ID:        kr.ID,
			ObjectKey: kr.ObjectKey,
			PublicURL: kr.PublicURL,
		})
		if refCounts[mk] == 0 {
			orphanCnt++
		}
	}

	if canCache && curRev > 0 {
		s.mediaRefCacheMu.Lock()
		if s.mediaRefCachedRev == curRev {
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
		type mediaKeyRow struct {
			ID            string `db:"id"`
			StorageDriver string `db:"storage_driver"`
			ObjectKey     string `db:"object_key"`
			PublicURL     string `db:"public_url"`
			SizeBytes     int64  `db:"size_bytes"`
		}
		var keyRows []mediaKeyRow
		_ = sqlx.SelectInto(ctx, s.deps.Store, &keyRows,
			`SELECT id, storage_driver, object_key, public_url, size_bytes FROM media_assets WHERE `+whereSQL+` ORDER BY `+orderClause, args...)
		if len(keyRows) == 0 {
			return []MediaAssetView{}, 0, allTotal, 0, 0
		}
		probeAssets := make([]domain.MediaAsset, len(keyRows))
		for i, kr := range keyRows {
			probeAssets[i] = domain.MediaAsset{
				ID:            kr.ID,
				StorageDriver: kr.StorageDriver,
				ObjectKey:     kr.ObjectKey,
				PublicURL:     kr.PublicURL,
			}
		}
		refCounts := s.countAssetsReferences(ctx, probeAssets)
		type matchedKey struct {
			id  string
			rc  int
			orp bool
		}
		matched := make([]matchedKey, 0, len(keyRows))
		orphanCnt := 0
		var totalBytes int64
		wantUsed := usage == "used"
		for i, kr := range keyRows {
			rc := refCounts[i]
			isOrp := rc == 0
			if isOrp {
				orphanCnt++
			}
			if (wantUsed && rc > 0) || (!wantUsed && isOrp) {
				totalBytes += kr.SizeBytes
				matched = append(matched, matchedKey{id: kr.ID, rc: rc, orp: isOrp})
			}
		}
		total := len(matched)
		start := (page - 1) * perPage
		if start >= total {
			return []MediaAssetView{}, total, allTotal, orphanCnt, totalBytes
		}
		end := start + perPage
		if end > total {
			end = total
		}
		pageMatched := matched[start:end]
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

	offset := (page - 1) * perPage
	baseSQL := `SELECT * FROM media_assets WHERE ` + whereSQL + ` ORDER BY ` + orderClause
	paginatedSQL, pageArgs := s.deps.Store.Dialect().PaginateSQL(baseSQL, perPage, offset)
	fullArgs := make([]any, 0, len(args)+len(pageArgs))
	fullArgs = append(fullArgs, args...)
	fullArgs = append(fullArgs, pageArgs...)

	var rows []domain.MediaAsset
	_ = sqlx.SelectInto(ctx, s.deps.Store, &rows, paginatedSQL, fullArgs...)

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
