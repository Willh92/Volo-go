package media

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/Willh92/Volo-go/internal/db/sqlx"
	"github.com/Willh92/Volo-go/internal/domain"
)

// ScanOrphans inspects ALL 9 reference scopes across the database before marking any MediaAsset as orphaned:
//  1. articles (content_md, summary_md, cover_url, audio_url)
//  2. article_revisions (content_md, summary_md)
//  3. pages (content_md, cover_url)
//  4. series (cover_url, description)
//  5. books (cover_url, description)
//  6. links (logo_url)
//  7. users (avatar_url, bio)
//  8. comments (content_md, avatar_url)
//  9. settings (value_json across site/seo/feed/podcast) + plugins_state (config_json)
//
// When deleteOrphans is true, orphaned assets are safely deleted (with `filepath.EvalSymlinks`
// containment enforcement for local files so no path outside UploadDir can ever be unlinked).
func (p *Pipeline) ScanOrphans(ctx context.Context, deleteOrphans bool) ([]domain.MediaAsset, error) {
	assets, err := sqlx.Select[domain.MediaAsset](ctx, p.store, `SELECT * FROM media_assets ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	if len(assets) == 0 {
		return []domain.MediaAsset{}, nil
	}

	referenced, err := p.streamReferencedAssetIndexes(ctx, assets)
	if err != nil {
		return nil, err
	}

	orphans := make([]domain.MediaAsset, 0)
	for i, a := range assets {
		if referenced[i] {
			continue
		}
		orphans = append(orphans, a)
	}

	if deleteOrphans && len(orphans) > 0 {
		for _, orp := range orphans {
			_ = p.deleteAssetRecord(ctx, orp)
		}
	}
	return orphans, nil
}

type assetMatchNeedle struct {
	idx    int
	pubURL string
	objKey string
}

func (p *Pipeline) streamReferencedAssetIndexes(ctx context.Context, assets []domain.MediaAsset) ([]bool, error) {
	referenced := make([]bool, len(assets))
	pending := make([]assetMatchNeedle, 0, len(assets))
	for i, a := range assets {
		pubURL := strings.TrimSpace(a.PublicURL)
		objKey := strings.TrimLeft(strings.TrimSpace(a.ObjectKey), "/")
		if pubURL == "" && objKey == "" {
			continue
		}
		pending = append(pending, assetMatchNeedle{
			idx:    i,
			pubURL: pubURL,
			objKey: objKey,
		})
	}
	if len(pending) == 0 {
		return referenced, nil
	}

	matchBlob := func(blob string) {
		if blob == "" || len(pending) == 0 {
			return
		}
		writeIdx := 0
		for _, cand := range pending {
			hit := (cand.pubURL != "" && strings.Contains(blob, cand.pubURL)) ||
				(cand.objKey != "" && strings.Contains(blob, cand.objKey))
			if hit {
				referenced[cand.idx] = true
				continue
			}
			pending[writeIdx] = cand
			writeIdx++
		}
		pending = pending[:writeIdx]
	}

	scanScope := func(q string, args ...any) error {
		if len(pending) == 0 {
			return nil
		}
		rows, err := p.store.QueryContext(ctx, q, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		cols, err := rows.Columns()
		if err != nil {
			return err
		}
		strVals := make([]string, len(cols))
		scanPtrs := make([]any, len(cols))
		for i := range strVals {
			scanPtrs[i] = &strVals[i]
		}
		for rows.Next() {
			for i := range strVals {
				strVals[i] = ""
			}
			if err := rows.Scan(scanPtrs...); err != nil {
				continue
			}
			for _, v := range strVals {
				if v != "" {
					matchBlob(v)
				}
			}
			if len(pending) == 0 {
				return nil
			}
		}
		return rows.Err()
	}

	scopes := []struct {
		query string
		args  []any
	}{
		{query: `SELECT content_md, summary_md, cover_url, audio_url FROM articles`},
		{query: `SELECT content_md, summary_md FROM article_revisions`},
		{query: `SELECT content_md FROM pages`},
		{query: `SELECT cover_url, description FROM series`},
		{query: `SELECT cover_url, description FROM books`},
		{query: `SELECT icon_url, description FROM links`},
		{query: `SELECT avatar_url, bio FROM users`},
		{query: `SELECT content_md, avatar_url FROM comments WHERE is_deleted = ?`, args: []any{false}},
		{query: `SELECT value_json FROM settings`},
		{query: `SELECT config_json FROM plugins`},
		{query: `SELECT icon_url, description FROM follows`},
		{query: `SELECT icon FROM nav_items`},
	}
	for _, sc := range scopes {
		if len(pending) == 0 {
			break
		}
		if err := scanScope(sc.query, sc.args...); err != nil {
			return nil, err
		}
	}
	return referenced, nil
}

// safeDeleteLocalUpload verifies via `filepath.EvalSymlinks` that target resides strictly inside uploadDir
// before unlinking, preventing symlink escape or tampered `object_key` traversal!
func safeDeleteLocalUpload(uploadDir string, objectKey string) error {
	cleanUploadRoot, err := filepath.EvalSymlinks(uploadDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	rel := filepath.Clean(filepath.FromSlash(strings.TrimLeft(objectKey, "/")))
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return errors.New("refusing to delete invalid relative object key")
	}
	candidate := filepath.Join(cleanUploadRoot, rel)
	info, lstatErr := os.Lstat(candidate)
	if lstatErr != nil {
		if os.IsNotExist(lstatErr) {
			return nil
		}
		return lstatErr
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return errors.New("refusing to unlink symbolic link inside upload directory")
	}
	resolved, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return err
	}
	prefix := cleanUploadRoot + string(os.PathSeparator)
	if !strings.HasPrefix(resolved, prefix) {
		return errors.New("upload path escapes UploadDir containment boundary")
	}
	return os.Remove(resolved)
}
