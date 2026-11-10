package content

import (
	"context"
	"database/sql"
	"errors"
	"html"
	"log/slog"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Willh92/Volo-go/internal/db/sqlx"
	"github.com/Willh92/Volo-go/internal/domain"
	"github.com/Willh92/Volo-go/internal/repo"
)

// deletedCommentPlaceholderKey is the public i18n key rendered inside soft-deleted comment tombstones.
const deletedCommentPlaceholderKey = "comment.deleted_placeholder"

// Comment trash janitor cadence: first sweep shortly after boot, then every 6 hours.
const (
	commentTrashJanitorStartDelay = 90 * time.Second
	commentTrashJanitorInterval   = 6 * time.Hour
	commentTrashPurgeBatch        = 200
	maxCommentAuthorNameRunes     = 48
	maxCommentURLBytes            = 512
)

var (
	// ErrCommentNotInTrash reports a restore/purge request against a comment that is not soft-deleted.
	ErrCommentNotInTrash = errors.New("comments: comment is not in trash")
	// ErrInvalidCommentIdentity reports an invalid admin edit of a comment's nickname/avatar/website.
	ErrInvalidCommentIdentity = errors.New("comments: invalid author identity")
)

// commentTrashScrubSet lists every privacy-sensitive column that is wiped when a trashed comment is
// permanently deleted but must survive as a placeholder (it still has live replies): client IP, masked IP,
// geo region, the user-agent/environment fingerprint digest and the encrypted notify email.
const commentTrashScrubSet = `client_ip = '', ip_masked = '', geo_region = '', env_fp_hash = '', notify_email_ciphertext = ''`

// SoftDeleteCommentTx moves a comment into the trash (is_deleted = true) without touching any other column,
// so a later restore is lossless. Privacy scrubbing happens only on permanent deletion (purgeTrashedCommentTx,
// via manual purge, "empty trash" or the retention janitor). Callers recount target totals themselves.
func SoftDeleteCommentTx(ctx context.Context, tx *sqlx.Tx, commentID string, nowMS int64) error {
	_, err := tx.ExecContext(ctx,
		`UPDATE comments SET is_deleted = ?, updated_at = ? WHERE id = ?`,
		true, nowMS, strings.TrimSpace(commentID))
	return err
}

func fallbackDeletedCommentPlaceholderText(locale string) string {
	l := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(locale)), "_", "-")
	switch {
	case strings.HasPrefix(l, "zh-tw"), strings.HasPrefix(l, "zh-hk"), strings.HasPrefix(l, "zh-mo"), strings.HasPrefix(l, "zh-hant"):
		return "該評論已刪除"
	case strings.HasPrefix(l, "zh"):
		return "该评论已删除"
	case strings.HasPrefix(l, "ja"):
		return "このコメントは削除されました"
	default:
		return "This comment has been deleted"
	}
}

func (s *Service) deletedCommentPlaceholderText(locale string) string {
	resolved := locale
	if s != nil && s.catalog != nil {
		siteDefault := ""
		if s.settings != nil {
			siteDefault = s.settings.Snapshot().Site.DefaultLocale
		}
		resolved = s.catalog.Match(locale, siteDefault)
		if txt := strings.TrimSpace(s.catalog.T(resolved, deletedCommentPlaceholderKey)); txt != "" && txt != deletedCommentPlaceholderKey {
			return txt
		}
	}
	return fallbackDeletedCommentPlaceholderText(resolved)
}

// deletedCommentPlaceholderView renders a zero-PII tombstone for a soft-deleted comment that still has
// visible replies: no author, no link, no avatar override, no content, no actions.
func (s *Service) deletedCommentPlaceholderView(r publicCommentRow, locale string) PublicCommentView {
	return PublicCommentView{
		ID:          r.ID,
		ParentID:    r.ParentID,
		AuthorName:  "",
		AuthorURL:   "",
		AvatarURL:   "",
		AvatarSeed:  "deleted",
		IsDeleted:   true,
		ContentHTML: `<p class="vc-deleted-placeholder">` + html.EscapeString(s.deletedCommentPlaceholderText(locale)) + `</p>`,
		Status:      "deleted",
		CanEdit:     false,
		CreatedAt:   r.CreatedAt,
	}
}

// hasLiveCommentDescendants walks the reply tree below commentID (bounded depth) and reports whether any
// descendant is still live (not soft-deleted).
func hasLiveCommentDescendants(ctx context.Context, q sqlx.Queryer, targetType, targetID, commentID string) (bool, error) {
	type childRow struct {
		ID        string `db:"id"`
		IsDeleted bool   `db:"is_deleted"`
	}
	frontier := []string{commentID}
	seen := map[string]bool{commentID: true}
	for depth := 0; depth < 16 && len(frontier) > 0; depth++ {
		var next []string
		for start := 0; start < len(frontier); start += 200 {
			end := min(start+200, len(frontier))
			chunk := frontier[start:end]
			args := make([]any, 0, len(chunk)+2)
			args = append(args, targetType, targetID)
			for _, id := range chunk {
				args = append(args, id)
			}
			var rows []childRow
			if err := sqlx.SelectInto(ctx, q, &rows,
				`SELECT id, is_deleted FROM comments WHERE target_type = ? AND target_id = ? AND parent_id IN (`+
					strings.TrimSuffix(strings.Repeat("?,", len(chunk)), ",")+`)`, args...); err != nil {
				return false, err
			}
			for _, row := range rows {
				if !row.IsDeleted {
					return true, nil
				}
				if !seen[row.ID] {
					seen[row.ID] = true
					next = append(next, row.ID)
				}
			}
		}
		frontier = next
	}
	return false, nil
}

// isPurgedCommentPlaceholder reports whether c is the minimal, content-free tombstone left behind by a purge
// (or by legacy pre-trash deletes that cleared content_md). Such rows cannot be restored and are hidden from
// the admin trash listing; they disappear for good once their last live reply is gone.
func isPurgedCommentPlaceholder(c domain.Comment) bool {
	return c.IsDeleted && strings.TrimSpace(c.ContentMD) == ""
}

// purgeTrashedCommentTx permanently removes a trashed comment and its reactions. When live replies still
// hang below it, the row is instead reduced to a minimal, content- and identity-free placeholder (kept=true)
// so the public thread keeps its structure; surviving direct replies stop echoing the old "@nickname".
func purgeTrashedCommentTx(ctx context.Context, tx *sqlx.Tx, c domain.Comment) (kept bool, err error) {
	live, err := hasLiveCommentDescendants(ctx, tx, c.TargetType, c.TargetID, c.ID)
	if err != nil {
		return false, err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM reactions WHERE target_type = ? AND target_id = ?`, "comment", c.ID); err != nil {
		return false, err
	}
	if live {
		if name := strings.TrimSpace(c.AuthorName); name != "" {
			if _, err = tx.ExecContext(ctx,
				`UPDATE comments SET reply_to_name = '' WHERE target_type = ? AND target_id = ? AND parent_id = ? AND reply_to_name = ?`,
				c.TargetType, c.TargetID, c.ID, c.AuthorName); err != nil {
				return false, err
			}
		}
		_, err = tx.ExecContext(ctx,
			`UPDATE comments SET content_md = '', author_name = '', author_url = '', avatar_url = '', reply_to_name = '', `+
				`user_id = '', guest_id_hash = '', `+commentTrashScrubSet+` WHERE id = ? AND is_deleted = ?`,
			c.ID, true)
		return true, err
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM comments WHERE id = ? AND is_deleted = ?`, c.ID, true)
	return false, err
}

// RestoreCommentTx moves a trashed comment back to the live tree inside tx (callers recount target totals).
// When its parent was permanently purged meanwhile, the comment is promoted to a root so it never becomes an
// invisible orphan. Content-free purge placeholders are not restorable (ErrCommentNotInTrash).
func RestoreCommentTx(ctx context.Context, tx *sqlx.Tx, c domain.Comment, nowMS int64) (domain.Comment, error) {
	if !c.IsDeleted || isPurgedCommentPlaceholder(c) {
		return c, ErrCommentNotInTrash
	}
	parentID := strings.TrimSpace(c.ParentID)
	replyTo := c.ReplyToName
	if parentID != "" {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM comments WHERE id = ?`, parentID).Scan(&n); err != nil {
			return c, err
		}
		if n == 0 {
			parentID, replyTo = "", ""
		}
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE comments SET is_deleted = ?, parent_id = ?, reply_to_name = ?, updated_at = ? WHERE id = ?`,
		false, parentID, replyTo, nowMS, c.ID); err != nil {
		return c, err
	}
	c.ParentID, c.ReplyToName = parentID, replyTo
	c.IsDeleted = false
	c.UpdatedAt = nowMS
	return c, nil
}

// RestoreComment moves a trashed comment back to the live tree and recounts its target (see RestoreCommentTx).
func (s *Service) RestoreComment(ctx context.Context, commentID string) (domain.Comment, error) {
	var c domain.Comment
	if err := sqlx.GetInto(ctx, s.store, &c, `SELECT * FROM comments WHERE id = ?`, strings.TrimSpace(commentID)); err != nil {
		return domain.Comment{}, err
	}
	if !c.IsDeleted || isPurgedCommentPlaceholder(c) {
		return c, ErrCommentNotInTrash
	}
	nowMS := time.Now().UnixMilli()
	var restored domain.Comment
	err := s.store.WithTx(ctx, func(tx *sqlx.Tx) error {
		var rErr error
		restored, rErr = RestoreCommentTx(ctx, tx, c, nowMS)
		if rErr != nil {
			return rErr
		}
		return syncApprovedCommentCountTx(ctx, tx, c.TargetType, c.TargetID)
	})
	if err != nil {
		return domain.Comment{}, err
	}
	return restored, nil
}

// PurgeComment permanently deletes a trashed comment (see purgeTrashedCommentTx for placeholder semantics).
func (s *Service) PurgeComment(ctx context.Context, commentID string) (keptPlaceholder bool, err error) {
	var c domain.Comment
	if err := sqlx.GetInto(ctx, s.store, &c, `SELECT * FROM comments WHERE id = ?`, strings.TrimSpace(commentID)); err != nil {
		return false, err
	}
	if !c.IsDeleted {
		return false, ErrCommentNotInTrash
	}
	err = s.store.WithTx(ctx, func(tx *sqlx.Tx) error {
		var pErr error
		keptPlaceholder, pErr = purgeTrashedCommentTx(ctx, tx, c)
		return pErr
	})
	return keptPlaceholder, err
}

// PurgeTrashedCommentsTx purges the given (already authorized) trashed comments inside tx, skipping live ones.
// kept counts rows newly reduced to placeholders; rows that already were minimal placeholders are not re-counted.
func PurgeTrashedCommentsTx(ctx context.Context, tx *sqlx.Tx, comments []domain.Comment) (purged int, kept int, err error) {
	for _, c := range comments {
		if !c.IsDeleted {
			continue
		}
		k, pErr := purgeTrashedCommentTx(ctx, tx, c)
		if pErr != nil {
			return purged, kept, pErr
		}
		if k {
			if !(isPurgedCommentPlaceholder(c) && c.AuthorName == "") {
				kept++
			}
		} else {
			purged++
		}
	}
	return purged, kept, nil
}

// purgeTrash sweeps trashed comments matching extraWhere in keyset-paginated batches.
func (s *Service) purgeTrash(ctx context.Context, extraWhere string, extraArgs []any, background bool) (purged int, kept int, err error) {
	lastID := ""
	for {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return purged, kept, ctxErr
		}
		args := append([]any{true, lastID}, extraArgs...)
		baseSQL := `SELECT id, target_type, target_id, parent_id, author_name, content_md, is_deleted FROM comments WHERE is_deleted = ? AND id > ?` + extraWhere + ` ORDER BY id ASC`
		q, pageArgs := s.store.Dialect().PaginateSQL(baseSQL, commentTrashPurgeBatch, 0)
		var batch []domain.Comment
		if err := sqlx.SelectInto(ctx, s.store, &batch, q, append(args, pageArgs...)...); err != nil {
			return purged, kept, err
		}
		if len(batch) == 0 {
			return purged, kept, nil
		}
		run := s.store.WithTx
		if background {
			// Trash rows are invisible (or content-free placeholders) publicly: skip DataRevision bumps.
			run = s.store.WithBgTx
		}
		if txErr := run(ctx, func(tx *sqlx.Tx) error {
			p, k, pErr := PurgeTrashedCommentsTx(ctx, tx, batch)
			purged += p
			kept += k
			return pErr
		}); txErr != nil {
			return purged, kept, txErr
		}
		lastID = batch[len(batch)-1].ID
		if len(batch) < commentTrashPurgeBatch {
			return purged, kept, nil
		}
	}
}

// EmptyCommentTrash permanently purges every trashed comment (placeholders with live replies are scrubbed and kept).
func (s *Service) EmptyCommentTrash(ctx context.Context) (purged int, kept int, err error) {
	return s.purgeTrash(ctx, "", nil, false)
}

// PurgeExpiredCommentTrash purges trashed comments older than reading.comment_trash_retention_days (0 = manual only).
func (s *Service) PurgeExpiredCommentTrash(ctx context.Context, now time.Time) (purged int, kept int, err error) {
	if s == nil || s.store == nil || s.settings == nil {
		return 0, 0, nil
	}
	days := s.settings.Snapshot().Reading.CommentTrashRetentionDays
	if days <= 0 {
		return 0, 0, nil
	}
	cutoff := now.Add(-time.Duration(days) * 24 * time.Hour).UnixMilli()
	return s.purgeTrash(ctx, ` AND updated_at < ?`, []any{cutoff}, true)
}

// RunCommentTrashJanitor blocks until ctx is done, sweeping expired comment trash shortly after startup
// and then every 6 hours. Intended to be launched once from the serve command's background goroutines.
func (s *Service) RunCommentTrashJanitor(ctx context.Context) {
	if s == nil || s.store == nil {
		return
	}
	timer := time.NewTimer(commentTrashJanitorStartDelay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		runCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		purged, kept, err := s.PurgeExpiredCommentTrash(runCtx, time.Now())
		cancel()
		if err != nil && !errors.Is(err, context.Canceled) {
			slog.Warn("comment trash purge failed", "component", "maintenance", "err", err)
		} else if purged > 0 || kept > 0 {
			slog.Info("purged expired comment trash", "component", "maintenance", "purged", purged, "kept_placeholders", kept)
		}
		timer.Reset(commentTrashJanitorInterval)
	}
}

// CommentIdentityPatch carries optional admin edits of a comment's guest identity fields.
type CommentIdentityPatch struct {
	AuthorName *string
	AvatarURL  *string
	AuthorURL  *string
}

// Empty reports whether no identity field is being changed.
func (p CommentIdentityPatch) Empty() bool {
	return p.AuthorName == nil && p.AvatarURL == nil && p.AuthorURL == nil
}

func isBidiControl(r rune) bool {
	return (r >= 0x202A && r <= 0x202E) || (r >= 0x2066 && r <= 0x2069) || r == 0x200E || r == 0x200F || r == 0x061C
}

// NormalizeCommentAuthorName strips control/bidi-override characters, trims, and enforces 1..48 runes.
func NormalizeCommentAuthorName(raw string) (string, error) {
	cleaned := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || isBidiControl(r) || r == utf8.RuneError {
			return -1
		}
		return r
	}, raw)
	cleaned = strings.TrimSpace(cleaned)
	if n := utf8.RuneCountInString(cleaned); n == 0 || n > maxCommentAuthorNameRunes {
		return "", ErrInvalidCommentIdentity
	}
	return cleaned, nil
}

// NormalizeCommentAvatarURL accepts "" (generated default avatar), site-relative "/..." paths, or http(s) URLs.
func NormalizeCommentAvatarURL(raw string) (string, error) {
	if strings.TrimSpace(repo.StripASCIIControls(raw)) == "" {
		return "", nil
	}
	clean, ok := repo.SanitizeAllowedURL(raw, false)
	if !ok || len(clean) > maxCommentURLBytes || strings.ContainsAny(clean, "\"'<> ") {
		return "", ErrInvalidCommentIdentity
	}
	return clean, nil
}

// NormalizeCommentAuthorURL accepts "" or an absolute http(s) URL.
func NormalizeCommentAuthorURL(raw string) (string, error) {
	if strings.TrimSpace(repo.StripASCIIControls(raw)) == "" {
		return "", nil
	}
	clean, ok := sanitizeCommentAuthorURL(raw)
	if !ok || len(clean) > maxCommentURLBytes {
		return "", ErrInvalidCommentIdentity
	}
	return clean, nil
}

// NormalizedCommentIdentity holds validated identity values; nil means "unchanged".
type NormalizedCommentIdentity struct {
	AuthorName *string
	AvatarURL  *string
	AuthorURL  *string
}

// NormalizeCommentIdentityPatch validates every provided field of p.
func NormalizeCommentIdentityPatch(p CommentIdentityPatch) (NormalizedCommentIdentity, error) {
	var out NormalizedCommentIdentity
	if p.AuthorName != nil {
		v, err := NormalizeCommentAuthorName(*p.AuthorName)
		if err != nil {
			return out, errors.New("comments: author_name must be 1..48 characters")
		}
		out.AuthorName = &v
	}
	if p.AvatarURL != nil {
		v, err := NormalizeCommentAvatarURL(*p.AvatarURL)
		if err != nil {
			return out, errors.New("comments: avatar_url must be empty, a site-relative /path or an http(s) URL")
		}
		out.AvatarURL = &v
	}
	if p.AuthorURL != nil {
		v, err := NormalizeCommentAuthorURL(*p.AuthorURL)
		if err != nil {
			return out, errors.New("comments: author_url must be empty or an http(s) URL")
		}
		out.AuthorURL = &v
	}
	return out, nil
}

// ApplyCommentIdentityTx persists validated identity edits for c. When the nickname changes, replies that
// addressed the old nickname ("@old") are re-pointed in the same transaction: direct children
// (parent_id = c.id) and, for 2nd-level comments, the flattened siblings created after c under the same
// parent (CreateComment flattens replies-to-replies onto the root while keeping reply_to_name).
func ApplyCommentIdentityTx(ctx context.Context, tx *sqlx.Tx, c domain.Comment, id NormalizedCommentIdentity, nowMS int64) (bool, error) {
	sets := make([]string, 0, 4)
	args := make([]any, 0, 6)
	nameChanged := id.AuthorName != nil && *id.AuthorName != c.AuthorName
	if nameChanged {
		sets = append(sets, `author_name = ?`)
		args = append(args, *id.AuthorName)
	}
	if id.AvatarURL != nil && *id.AvatarURL != c.AvatarURL {
		sets = append(sets, `avatar_url = ?`)
		args = append(args, *id.AvatarURL)
	}
	if id.AuthorURL != nil && *id.AuthorURL != c.AuthorURL {
		sets = append(sets, `author_url = ?`)
		args = append(args, *id.AuthorURL)
	}
	if len(sets) == 0 {
		return false, nil
	}
	sets = append(sets, `updated_at = ?`)
	args = append(args, nowMS, c.ID)
	if _, err := tx.ExecContext(ctx, `UPDATE comments SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...); err != nil {
		return false, err
	}
	if nameChanged && c.AuthorName != "" {
		parentID := strings.TrimSpace(c.ParentID)
		var err error
		if parentID == "" {
			_, err = tx.ExecContext(ctx,
				`UPDATE comments SET reply_to_name = ?, updated_at = ? WHERE target_type = ? AND target_id = ? AND reply_to_name = ? AND id <> ? AND parent_id = ?`,
				*id.AuthorName, nowMS, c.TargetType, c.TargetID, c.AuthorName, c.ID, c.ID)
		} else {
			_, err = tx.ExecContext(ctx,
				`UPDATE comments SET reply_to_name = ?, updated_at = ? WHERE target_type = ? AND target_id = ? AND reply_to_name = ? AND id <> ? AND (parent_id = ? OR (parent_id = ? AND created_at >= ?))`,
				*id.AuthorName, nowMS, c.TargetType, c.TargetID, c.AuthorName, c.ID, c.ID, parentID, c.CreatedAt)
		}
		if err != nil {
			return false, err
		}
	}
	return true, nil
}

// isNoRows is a small helper for callers mapping lookups to 404.
func isNoRows(err error) bool { return errors.Is(err, sql.ErrNoRows) }
