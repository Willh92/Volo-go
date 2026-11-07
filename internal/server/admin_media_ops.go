package server

import (
	"context"
	"crypto/sha256"
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

// reconcileLocalUploads scans UploadDir on explicit operator request (`POST /api/admin/media/reconcile`)
// and registers untracked valid media files in `media_assets`, returning the number of newly imported rows.
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
		_, _ = io.Copy(hasher, f)
		_ = f.Close()
		sumHex := hex.EncodeToString(hasher.Sum(nil))

		createdAt := info.ModTime().UnixMilli()
		if createdAt <= 0 {
			createdAt = time.Now().UnixMilli()
		}
		_, insErr := s.deps.Store.ExecContext(ctx,
			`INSERT INTO media_assets (id, storage_driver, object_key, public_url, original_name, alt_text, mime_type, size_bytes, width, height, sha256, exif_stripped, uploader_id, created_at)
			 VALUES (?, 'local', ?, ?, ?, '', ?, ?, ?, ?, ?, ?, '', ?)`,
			id.Next(), relSlash, "/uploads/"+relSlash, name, mimeType, info.Size(), width, height, sumHex, false, createdAt)
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

// countAssetsReferencesStreaming streams one database row at a time across all content & configuration
// tables and matches `/uploads/...`, object keys, and external asset URLs in a single O(bytes) token pass
// rather than an O(rows * assets) substring loop.
func (s *AppServer) countAssetsReferencesStreaming(ctx context.Context, assets []domain.MediaAsset) []int {
	counts := make([]int, len(assets))
	if len(assets) == 0 {
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
		s := raw
		if strings.Contains(s, `\/`) {
			s = strings.ReplaceAll(s, `\/`, "/")
		}
		whole := strings.TrimSpace(s)
		matchToken(whole)

		n := len(s)
		start := -1
		for i := 0; i < n; i++ {
			if isMediaRefDelim(s[i]) {
				if start >= 0 {
					if i-start >= 3 {
						matchToken(s[start:i])
					}
					start = -1
				}
			} else if start < 0 {
				start = i
			}
		}
		if start >= 0 && n-start >= 3 {
			matchToken(s[start:n])
		}

		for _, sp := range spaceAssets {
			if rowSeen[sp.idx] == rowGen {
				continue
			}
			if (sp.pubURL != "" && strings.Contains(s, sp.pubURL)) ||
				(sp.objKey != "" && strings.Contains(s, sp.objKey)) {
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
		tz := strings.TrimSpace(s.deps.Settings.Snapshot().Site.TimeZone)
		if tz != "" {
			if loc, err := time.LoadLocation(tz); err == nil {
				return loc
			}
		}
	}
	return time.UTC
}

func (s *AppServer) queryAdminMediaPage(
	ctx context.Context,
	qStr string,
	mimeFilter string,
	monthFilter string,
	orphanOnly bool,
	withRefs bool,
	sortBy string,
	page int,
	perPage int,
) ([]MediaAssetView, int, int) {
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
		pat := "%" + qStr + "%"
		where = append(where, "(LOWER(original_name) LIKE LOWER(?) OR LOWER(object_key) LIKE LOWER(?) OR LOWER(public_url) LIKE LOWER(?) OR LOWER(alt_text) LIKE LOWER(?))")
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
			if strings.HasSuffix(mf, "/") {
				where = append(where, "LOWER(mime_type) LIKE LOWER(?)")
				args = append(args, mf+"%")
			} else {
				where = append(where, "LOWER(mime_type) LIKE LOWER(?)")
				args = append(args, mf+"%")
			}
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

	if orphanOnly {
		rows, _ := sqlx.Select[domain.MediaAsset](ctx, s.deps.Store,
			`SELECT * FROM media_assets WHERE `+whereSQL+` ORDER BY `+orderClause, args...)
		if len(rows) == 0 {
			return []MediaAssetView{}, 0, 0
		}
		refCounts := s.countAssetsReferencesStreaming(ctx, rows)
		orphans := make([]MediaAssetView, 0)
		for i, a := range rows {
			if refCounts[i] == 0 {
				orphans = append(orphans, MediaAssetView{
					MediaAsset: a,
					RefCount:   0,
					IsOrphan:   true,
				})
			}
		}
		total := len(orphans)
		start := (page - 1) * perPage
		if start >= total {
			return []MediaAssetView{}, total, total
		}
		end := start + perPage
		if end > total {
			end = total
		}
		return orphans[start:end], total, total
	}

	var total int
	_ = s.deps.Store.QueryRowContext(ctx, `SELECT COUNT(*) FROM media_assets WHERE `+whereSQL, args...).Scan(&total)
	if total == 0 {
		return []MediaAssetView{}, 0, 0
	}

	offset := (page - 1) * perPage
	baseSQL := `SELECT * FROM media_assets WHERE ` + whereSQL + ` ORDER BY ` + orderClause
	paginatedSQL, pageArgs := s.deps.Store.Dialect().PaginateSQL(baseSQL, perPage, offset)
	fullArgs := make([]any, 0, len(args)+len(pageArgs))
	fullArgs = append(fullArgs, args...)
	fullArgs = append(fullArgs, pageArgs...)

	rows, _ := sqlx.Select[domain.MediaAsset](ctx, s.deps.Store, paginatedSQL, fullArgs...)
	if len(rows) == 0 {
		return []MediaAssetView{}, total, 0
	}

	var refCounts []int
	if withRefs {
		refCounts = s.countAssetsReferencesStreaming(ctx, rows)
	}

	out := make([]MediaAssetView, 0, len(rows))
	orphanCnt := 0
	for i, a := range rows {
		rc := 0
		isOrp := false
		if withRefs {
			rc = refCounts[i]
			isOrp = rc == 0
			if isOrp {
				orphanCnt++
			}
		}
		out = append(out, MediaAssetView{
			MediaAsset: a,
			RefCount:   rc,
			IsOrphan:   isOrp,
		})
	}
	return out, total, orphanCnt
}

func (s *AppServer) listMediaWithRefs(ctx context.Context, limit int) ([]MediaAssetView, int) {
	items, _, orphanCnt := s.queryAdminMediaPage(ctx, "", "", "", false, true, "newest", 1, limit)
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
