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

type mediaRefScope struct {
	query string
	args  []any
}

func mediaReferenceScopes() []mediaRefScope {
	return []mediaRefScope{
		{query: `SELECT content_md, summary_md, cover_url, audio_url FROM articles`},
		{query: `SELECT content_md, summary_md FROM article_revisions`},
		{query: `SELECT content_md FROM pages`},
		{query: `SELECT cover_url, description FROM series`},
		{query: `SELECT cover_url, description, source_url FROM books`},
		{query: `SELECT url FROM book_chapters`},
		{query: `SELECT icon_url, url, description FROM links`},
		{query: `SELECT avatar_url, bio FROM users`},
		{query: `SELECT avatar_url, content_md FROM comments WHERE is_deleted = ?`, args: []any{false}},
		{query: `SELECT value_json FROM settings`},
		{query: `SELECT config_json FROM plugins`},
		{query: `SELECT icon_url, description FROM follows`},
		{query: `SELECT icon, url FROM nav_items`},
		{query: `SELECT target_path FROM redirects`},
	}
}

type orphanNullString struct {
	val string
}

func (ns *orphanNullString) Scan(src any) error {
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

// AssetCandidateKeys returns normalized URL, path, and object_key lookup keys for a MediaAsset.
func AssetCandidateKeys(a domain.MediaAsset) []string {
	var keys []string
	add := func(k string) {
		if k == "" {
			return
		}
		for _, existing := range keys {
			if existing == k {
				return
			}
		}
		keys = append(keys, k)
	}
	pubURL := strings.TrimSpace(a.PublicURL)
	objKey := strings.TrimLeft(strings.TrimSpace(a.ObjectKey), "/")
	if pubURL != "" {
		cleanPub := stripQueryAndFragment(pubURL)
		add(pubURL)
		add(cleanPub)
		if pathPart := extractURLPath(cleanPub); pathPart != "" {
			add(pathPart)
			add(strings.TrimLeft(pathPart, "/"))
		}
	}
	if objKey != "" {
		add(objKey)
		add("/uploads/" + objKey)
		add("uploads/" + objKey)
	}
	return keys
}

func emitNormalizedTokensFromBlob(blob string, emit func(string)) {
	if blob == "" {
		return
	}
	if strings.Index(blob, `\/`) >= 0 {
		blob = strings.ReplaceAll(blob, `\/`, "/")
	}
	emitExpanded := func(raw string) {
		if raw == "" {
			return
		}
		tok := trimTokenPunctuation(raw)
		if tok == "" {
			return
		}
		emit(tok)

		clean := stripQueryAndFragment(tok)
		if clean != tok {
			emit(clean)
			clean = trimTokenPunctuation(clean)
			emit(clean)
		}
		if pathPart := extractURLPath(clean); pathPart != "" {
			emit(pathPart)
			emit(strings.TrimLeft(pathPart, "/"))
			if tail := lastThreeSlashSegments(pathPart); tail != "" {
				emit(tail)
			}
		} else {
			emit(strings.TrimLeft(clean, "/"))
			if tail := lastThreeSlashSegments(clean); tail != "" {
				emit(tail)
			}
		}
		if upIdx := strings.Index(clean, "/uploads/"); upIdx >= 0 {
			sub := clean[upIdx:]
			emit(sub)
			emit(strings.TrimLeft(sub, "/"))
		}
		// Legacy (Solo) relative/absolute image routes such as `image/x.png`, `/posts/image/x.png`
		// or `https://host/images/2019/x.png` are served by the legacy image shim from either
		// <upload>/<prefix>/<rest> or <upload>/<rest>, so also emit the de-prefixed object key.
		base := clean
		if pathPart := extractURLPath(clean); pathPart != "" {
			base = pathPart
		}
		if rest := stripLegacyImagePrefix(base); rest != "" {
			emit(rest)
			emit("/uploads/" + rest)
		}
	}

	if trimmedWhole := strings.TrimSpace(blob); trimmedWhole != "" && !strings.ContainsAny(trimmedWhole, " \t\r\n") {
		emitExpanded(trimmedWhole)
	}
	start := -1
	for i := 0; i < len(blob); i++ {
		if isOrphanTokenDelimiter(blob[i]) {
			if start >= 0 {
				emitTokenAndParenSubtokens(blob[start:i], emitExpanded)
				start = -1
			}
			continue
		}
		if start < 0 {
			start = i
		}
	}
	if start >= 0 {
		emitTokenAndParenSubtokens(blob[start:], emitExpanded)
	}
}

func streamAllReferenceRows(ctx context.Context, store *sqlx.Store, onRow func(cols []orphanNullString) bool) error {
	if store == nil {
		return nil
	}
	for _, sc := range mediaReferenceScopes() {
		stopEarly := false
		err := func() error {
			rows, qErr := store.QueryContext(ctx, sc.query, sc.args...)
			if qErr != nil {
				return qErr
			}
			defer rows.Close()
			colNames, cErr := rows.Columns()
			if cErr != nil {
				return cErr
			}
			vals := make([]orphanNullString, len(colNames))
			ptrs := make([]any, len(colNames))
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
				if !onRow(vals) {
					stopEarly = true
					return nil
				}
			}
			return rows.Err()
		}()
		if err != nil {
			return err
		}
		if stopEarly {
			return nil
		}
	}
	return nil
}

// CollectReferencedTokenCounts scans all content, navigation, chapter, redirect, comment, setting,
// and plugin tables and returns per-token referencing-row counts.
func CollectReferencedTokenCounts(ctx context.Context, store *sqlx.Store) (map[string]int, error) {
	counts := make(map[string]int, 256)
	rowSeen := make(map[string]struct{}, 32)
	err := streamAllReferenceRows(ctx, store, func(cols []orphanNullString) bool {
		clear(rowSeen)
		for _, col := range cols {
			if col.val == "" {
				continue
			}
			emitNormalizedTokensFromBlob(col.val, func(k string) {
				if k == "" || len(k) > 512 {
					return
				}
				if _, seen := rowSeen[k]; !seen {
					rowSeen[k] = struct{}{}
					counts[k]++
				}
			})
		}
		return true
	})
	if err != nil {
		return nil, err
	}
	return counts, nil
}

// CollectAllReferencedURLTokens scans all content, navigation, chapter, redirect, comment, setting,
// and plugin tables and returns the set of normalized referenced URL/path/key tokens.
func CollectAllReferencedURLTokens(ctx context.Context, store *sqlx.Store) (map[string]struct{}, error) {
	tokens := make(map[string]struct{}, 256)
	err := streamAllReferenceRows(ctx, store, func(cols []orphanNullString) bool {
		for _, col := range cols {
			if col.val == "" {
				continue
			}
			emitNormalizedTokensFromBlob(col.val, func(k string) {
				if k != "" && len(k) <= 512 {
					tokens[k] = struct{}{}
				}
			})
		}
		return true
	})
	if err != nil {
		return nil, err
	}
	return tokens, nil
}

// AssetRefCountFromTokens computes the referencing row count for an asset from a token-count map.
func AssetRefCountFromTokens(tokenCounts map[string]int, a domain.MediaAsset) int {
	if len(tokenCounts) == 0 {
		return 0
	}
	best := 0
	for _, k := range AssetCandidateKeys(a) {
		if c := tokenCounts[k]; c > best {
			best = c
		}
	}
	return best
}

// CountAssetReferences computes exact per-asset referencing row counts (keyed by asset.ID, or
// PublicURL/ObjectKey fallback when ID is empty) across all 14 database tables in a single streaming pass.
func CountAssetReferences(ctx context.Context, store *sqlx.Store, assets []domain.MediaAsset) (map[string]int, error) {
	counts := make(map[string]int, len(assets))
	if len(assets) == 0 || store == nil {
		return counts, nil
	}

	candidateToIdxs := make(map[string][]int, len(assets)*3)
	type spaceNeedle struct {
		idx  int
		keys []string
	}
	var spaceAssets []spaceNeedle

	assetMapKey := func(a domain.MediaAsset) string {
		if id := strings.TrimSpace(a.ID); id != "" {
			return id
		}
		if pub := strings.TrimSpace(a.PublicURL); pub != "" {
			return pub
		}
		return strings.TrimLeft(strings.TrimSpace(a.ObjectKey), "/")
	}

	for i, a := range assets {
		if mk := assetMapKey(a); mk != "" {
			counts[mk] = 0
		}
		keys := AssetCandidateKeys(a)
		hasSpace := false
		for _, k := range keys {
			candidateToIdxs[k] = append(candidateToIdxs[k], i)
			if strings.ContainsAny(k, " \t") {
				hasSpace = true
			}
		}
		if hasSpace {
			spaceAssets = append(spaceAssets, spaceNeedle{idx: i, keys: keys})
		}
	}
	if len(candidateToIdxs) == 0 {
		return counts, nil
	}

	idxCounts := make([]int, len(assets))
	rowSeenGen := make([]uint32, len(assets))
	var rowGen uint32

	markIdx := func(idx int) {
		if rowSeenGen[idx] != rowGen {
			rowSeenGen[idx] = rowGen
			idxCounts[idx]++
		}
	}

	err := streamAllReferenceRows(ctx, store, func(cols []orphanNullString) bool {
		rowGen++
		if rowGen == 0 {
			clear(rowSeenGen)
			rowGen = 1
		}
		for _, col := range cols {
			if col.val == "" {
				continue
			}
			emitNormalizedTokensFromBlob(col.val, func(k string) {
				if idxs, ok := candidateToIdxs[k]; ok {
					for _, idx := range idxs {
						markIdx(idx)
					}
				}
			})
			if len(spaceAssets) > 0 {
				norm := col.val
				if strings.Index(norm, `\/`) >= 0 {
					norm = strings.ReplaceAll(norm, `\/`, "/")
				}
				for _, sp := range spaceAssets {
					if rowSeenGen[sp.idx] == rowGen {
						continue
					}
					for _, k := range sp.keys {
						if k != "" && strings.Contains(norm, k) {
							markIdx(sp.idx)
							break
						}
					}
				}
			}
		}
		return true
	})
	if err != nil {
		return nil, err
	}

	for i, a := range assets {
		if mk := assetMapKey(a); mk != "" {
			counts[mk] = idxCounts[i]
		}
	}
	return counts, nil
}

// ScanOrphans inspects ALL 14 reference scopes across the database before marking any MediaAsset as orphaned.
// When deleteOrphans is true, orphaned assets are safely deleted (with `filepath.EvalSymlinks`
// containment enforcement for local files so no path outside UploadDir can ever be unlinked).
func (p *Pipeline) ScanOrphans(ctx context.Context, deleteOrphans bool) ([]domain.MediaAsset, error) {
	var assets []domain.MediaAsset
	err := sqlx.SelectInto(ctx, p.store, &assets, `SELECT * FROM media_assets ORDER BY created_at DESC`)
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

	for i, a := range assets {
		keys := AssetCandidateKeys(a)
		if len(keys) == 0 {
			continue
		}
		remaining++
		for _, k := range keys {
			candidateToIdxs[k] = append(candidateToIdxs[k], i)
		}
	}
	if remaining == 0 {
		return referenced, nil
	}

	err := streamAllReferenceRows(ctx, p.store, func(cols []orphanNullString) bool {
		for _, col := range cols {
			if col.val == "" {
				continue
			}
			emitNormalizedTokensFromBlob(col.val, func(k string) {
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
			})
			if remaining == 0 {
				return false
			}
		}
		return remaining > 0
	})
	if err != nil {
		return nil, err
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

// stripLegacyImagePrefix mirrors the prefixes accepted by the legacy image shim
// (`/image/`, `/images/`, `/upload/`, optionally under `/posts/`, also as relative `./image/...`)
// and returns the remaining object-key-like tail, or "" when s has no such prefix.
func stripLegacyImagePrefix(s string) string {
	p := s
	for {
		switch {
		case strings.HasPrefix(p, "./"):
			p = p[2:]
		case strings.HasPrefix(p, "../"):
			p = p[3:]
		case strings.HasPrefix(p, "/"):
			p = p[1:]
		default:
			goto trimmed
		}
	}
trimmed:
	if len(p) > len("posts/") && strings.EqualFold(p[:len("posts/")], "posts/") {
		p = p[len("posts/"):]
	}
	for _, pfx := range []string{"images/", "image/", "upload/"} {
		if len(p) > len(pfx) && strings.EqualFold(p[:len(pfx)], pfx) {
			rest := strings.TrimLeft(p[len(pfx):], "/")
			if rest == "" || strings.Contains(rest, "..") {
				return ""
			}
			return rest
		}
	}
	return ""
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

// SafeUnlinkLocalFile verifies via `filepath.EvalSymlinks` that relPath resides strictly inside rootDir
// before unlinking, preventing symlink escape or path traversal outside rootDir.
func SafeUnlinkLocalFile(rootDir string, relPath string) error {
	cleanUploadRoot, err := filepath.EvalSymlinks(rootDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	rel := filepath.Clean(filepath.FromSlash(strings.TrimLeft(relPath, "/")))
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

func safeDeleteLocalUpload(uploadDir string, objectKey string) error {
	return SafeUnlinkLocalFile(uploadDir, objectKey)
}
