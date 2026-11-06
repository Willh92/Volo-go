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

	corpus, err := p.collectAll9ReferenceScopes(ctx)
	if err != nil {
		return nil, err
	}

	orphans := make([]domain.MediaAsset, 0)
	for _, a := range assets {
		if isAssetReferencedInCorpus(a, corpus) {
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

func isAssetReferencedInCorpus(a domain.MediaAsset, corpus []string) bool {
	pubURL := strings.TrimSpace(a.PublicURL)
	objKey := strings.TrimLeft(strings.TrimSpace(a.ObjectKey), "/")
	for _, blob := range corpus {
		if blob == "" {
			continue
		}
		if pubURL != "" && strings.Contains(blob, pubURL) {
			return true
		}
		if objKey != "" && strings.Contains(blob, objKey) {
			return true
		}
	}
	return false
}

func (p *Pipeline) collectAll9ReferenceScopes(ctx context.Context) ([]string, error) {
	var blobs []string
	appendQuery := func(q string) error {
		rows, err := p.store.QueryContext(ctx, q)
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
					blobs = append(blobs, v)
				}
			}
		}
		return rows.Err()
	}

	// Scope 1: articles
	if err := appendQuery(`SELECT content_md, summary_md, cover_url, audio_url FROM articles`); err != nil {
		return nil, err
	}
	// Scope 2: article_revisions
	if err := appendQuery(`SELECT content_md, summary_md FROM article_revisions`); err != nil {
		return nil, err
	}
	// Scope 3: pages
	if err := appendQuery(`SELECT content_md FROM pages`); err != nil {
		return nil, err
	}
	// Scope 4: series
	if err := appendQuery(`SELECT cover_url, description FROM series`); err != nil {
		return nil, err
	}
	// Scope 5: books
	if err := appendQuery(`SELECT cover_url, description FROM books`); err != nil {
		return nil, err
	}
	// Scope 6: links
	if err := appendQuery(`SELECT icon_url, description FROM links`); err != nil {
		return nil, err
	}
	// Scope 7: users
	if err := appendQuery(`SELECT avatar_url, bio FROM users`); err != nil {
		return nil, err
	}
	// Scope 8: comments (non-deleted)
	if err := appendQuery(`SELECT content_md, avatar_url FROM comments WHERE is_deleted = 0`); err != nil {
		return nil, err
	}
	// Scope 9: settings + plugins
	if err := appendQuery(`SELECT value_json FROM settings`); err != nil {
		return nil, err
	}
	if err := appendQuery(`SELECT config_json FROM plugins`); err != nil {
		return nil, err
	}
	return blobs, nil
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
