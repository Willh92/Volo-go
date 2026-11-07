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

// countAssetsReferencesStreaming streams one database row at a time across all content & configuration
// tables so reference counting never retains all article/revision Markdown bodies in memory.
func (s *AppServer) countAssetsReferencesStreaming(ctx context.Context, assets []domain.MediaAsset) []int {
	counts := make([]int, len(assets))
	if len(assets) == 0 {
		return counts
	}

	type needlePair struct {
		pubURL string
		objKey string
	}
	needles := make([]needlePair, len(assets))
	for i, a := range assets {
		needles[i] = needlePair{
			pubURL: strings.TrimSpace(a.PublicURL),
			objKey: strings.TrimLeft(strings.TrimSpace(a.ObjectKey), "/"),
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
			for idx, np := range needles {
				matched := false
				for _, col := range vals {
					if col.val == "" {
						continue
					}
					if (np.pubURL != "" && strings.Contains(col.val, np.pubURL)) ||
						(np.objKey != "" && strings.Contains(col.val, np.objKey)) {
						matched = true
						break
					}
				}
				if matched {
					counts[idx]++
				}
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

func (s *AppServer) listMediaWithRefs(ctx context.Context, limit int) ([]MediaAssetView, int) {
	if limit <= 0 {
		limit = 500
	}
	baseSQL := `SELECT * FROM media_assets ORDER BY created_at DESC`
	q, pageArgs := s.deps.Store.Dialect().PaginateSQL(baseSQL, limit, 0)
	rows, _ := sqlx.Select[domain.MediaAsset](ctx, s.deps.Store, q, pageArgs...)
	if len(rows) == 0 {
		return []MediaAssetView{}, 0
	}
	refCounts := s.countAssetsReferencesStreaming(ctx, rows)
	out := make([]MediaAssetView, 0, len(rows))
	orphanCnt := 0
	for i, a := range rows {
		rc := refCounts[i]
		isOrp := rc == 0
		if isOrp {
			orphanCnt++
		}
		out = append(out, MediaAssetView{
			MediaAsset: a,
			RefCount:   rc,
			IsOrphan:   isOrp,
		})
	}
	return out, orphanCnt
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
