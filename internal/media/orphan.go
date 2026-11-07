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

func (p *Pipeline) streamReferencedAssetIndexes(ctx context.Context, assets []domain.MediaAsset) ([]bool, error) {
	referenced := make([]bool, len(assets))
	candidateToIdxs := make(map[string][]int, len(assets)*3)
	matchedKeys := make(map[string]bool)
	remaining := 0

	addCandidate := func(key string, idx int) {
		if key == "" {
			return
		}
		list := candidateToIdxs[key]
		for _, existing := range list {
			if existing == idx {
				return
			}
		}
		candidateToIdxs[key] = append(list, idx)
	}

	for i, a := range assets {
		pubURL := strings.TrimSpace(a.PublicURL)
		objKey := strings.TrimLeft(strings.TrimSpace(a.ObjectKey), "/")
		if pubURL == "" && objKey == "" {
			continue
		}
		remaining++
		if pubURL != "" {
			cleanPub := stripQueryAndFragment(pubURL)
			addCandidate(pubURL, i)
			addCandidate(cleanPub, i)
			if pathPart := extractURLPath(cleanPub); pathPart != "" {
				addCandidate(pathPart, i)
				addCandidate(strings.TrimLeft(pathPart, "/"), i)
			}
		}
		if objKey != "" {
			addCandidate(objKey, i)
			addCandidate("/uploads/"+objKey, i)
			addCandidate("uploads/"+objKey, i)
		}
	}
	if remaining == 0 {
		return referenced, nil
	}

	markKey := func(k string) {
		if k == "" || matchedKeys[k] {
			return
		}
		idxs, ok := candidateToIdxs[k]
		if !ok {
			return
		}
		matchedKeys[k] = true
		for _, idx := range idxs {
			if !referenced[idx] {
				referenced[idx] = true
				remaining--
			}
		}
	}

	checkToken := func(raw string) {
		if raw == "" || remaining == 0 {
			return
		}
		if strings.Index(raw, `\/`) >= 0 {
			raw = strings.ReplaceAll(raw, `\/`, "/")
		}
		tok := trimTokenPunctuation(raw)
		if tok == "" {
			return
		}
		markKey(tok)

		clean := stripQueryAndFragment(tok)
		if clean != tok {
			markKey(clean)
			clean = trimTokenPunctuation(clean)
			markKey(clean)
		}
		if pathPart := extractURLPath(clean); pathPart != "" {
			markKey(pathPart)
			markKey(strings.TrimLeft(pathPart, "/"))
			if tail := lastThreeSlashSegments(pathPart); tail != "" {
				markKey(tail)
			}
		} else {
			markKey(strings.TrimLeft(clean, "/"))
			if tail := lastThreeSlashSegments(clean); tail != "" {
				markKey(tail)
			}
		}
		if upIdx := strings.Index(clean, "/uploads/"); upIdx > 0 {
			sub := clean[upIdx:]
			markKey(sub)
			markKey(strings.TrimLeft(sub, "/"))
		}
	}

	// Single linear pass per text blob: split candidate URL/path/key tokens without O(N_assets) substring scans.
	matchBlob := func(blob string) {
		if blob == "" || remaining == 0 {
			return
		}
		start := -1
		for i := 0; i < len(blob); i++ {
			if isOrphanTokenDelimiter(blob[i]) {
				if start >= 0 {
					emitTokenAndParenSubtokens(blob[start:i], checkToken)
					if remaining == 0 {
						return
					}
					start = -1
				}
				continue
			}
			if start < 0 {
				start = i
			}
		}
		if start >= 0 && remaining > 0 {
			emitTokenAndParenSubtokens(blob[start:], checkToken)
		}
	}

	scanScope := func(q string, args ...any) error {
		if remaining == 0 {
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
			if remaining == 0 {
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
		if remaining == 0 {
			break
		}
		if err := scanScope(sc.query, sc.args...); err != nil {
			return nil, err
		}
	}
	return referenced, nil
}

func isOrphanTokenDelimiter(b byte) bool {
	if b <= ' ' {
		return true
	}
	switch b {
	case '"', '\'', '`', '<', '>', '[', ']', '{', '}', ',', ';', '|':
		return true
	default:
		return false
	}
}

func emitTokenAndParenSubtokens(seg string, fn func(string)) {
	fn(seg)
	if strings.IndexByte(seg, '(') < 0 && strings.IndexByte(seg, ')') < 0 {
		return
	}
	subStart := -1
	for i := 0; i < len(seg); i++ {
		if seg[i] == '(' || seg[i] == ')' {
			if subStart >= 0 {
				fn(seg[subStart:i])
				subStart = -1
			}
			continue
		}
		if subStart < 0 {
			subStart = i
		}
	}
	if subStart >= 0 {
		fn(seg[subStart:])
	}
}

func trimTokenPunctuation(s string) string {
	start := 0
	end := len(s)
	// Preserve balanced inner parens like photo(1).jpg when stripping outer markdown parens.
	for start < end {
		switch s[start] {
		case '(', '.', ':', '!', '?', '\\':
			start++
		default:
			goto trimRight
		}
	}
trimRight:
	openCount := 0
	closeCount := 0
	for i := start; i < end; i++ {
		if s[i] == '(' {
			openCount++
		} else if s[i] == ')' {
			closeCount++
		}
	}
	for end > start {
		c := s[end-1]
		if c == ')' {
			if closeCount > openCount {
				closeCount--
				end--
				continue
			}
			break
		}
		switch c {
		case '.', ':', '!', '?', '\\':
			end--
		default:
			return s[start:end]
		}
	}
	return s[start:end]
}

func stripQueryAndFragment(s string) string {
	if idx := strings.IndexByte(s, '#'); idx >= 0 {
		s = s[:idx]
	}
	if idx := strings.IndexByte(s, '?'); idx >= 0 {
		s = s[:idx]
	}
	return s
}

func extractURLPath(s string) string {
	low := strings.ToLower(s)
	var rest string
	switch {
	case strings.HasPrefix(low, "https://"):
		rest = s[len("https://"):]
	case strings.HasPrefix(low, "http://"):
		rest = s[len("http://"):]
	case strings.HasPrefix(s, "//"):
		rest = s[2:]
	default:
		return ""
	}
	slash := strings.IndexByte(rest, '/')
	if slash < 0 {
		return ""
	}
	return rest[slash:]
}

func lastThreeSlashSegments(s string) string {
	trimmed := strings.Trim(s, "/")
	if trimmed == "" {
		return ""
	}
	slashes := 0
	for i := len(trimmed) - 1; i >= 0; i-- {
		if trimmed[i] == '/' {
			slashes++
			if slashes == 2 {
				// Found the last 2 slashes -> 3 segments (e.g. "2026/11/01j...pdf"); check if preceded by another slash.
				for j := i - 1; j >= 0; j-- {
					if trimmed[j] == '/' {
						return trimmed[j+1:]
					}
				}
				return trimmed
			}
		}
	}
	return ""
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
