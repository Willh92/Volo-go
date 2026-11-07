package content

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/Willh92/Volo-go/internal/db/sqlx"
	"github.com/Willh92/Volo-go/internal/domain"
	"github.com/Willh92/Volo-go/internal/guest"
	"github.com/Willh92/Volo-go/internal/render"
	"github.com/Willh92/Volo-go/internal/security"
	"github.com/Willh92/Volo-go/internal/visibility"
	"github.com/Willh92/Volo-go/pkg/id"
	"github.com/Willh92/Volo-go/pkg/plugin"
)

const GuestEditWindow = 30 * time.Minute

var (
	ErrCommentsClosed       = errors.New("comments: commenting is disabled")
	ErrRegisteredOnly       = errors.New("comments: reader sign-in is required to comment")
	ErrCommentEditForbidden = errors.New("comments: not authorized to modify this comment")
	ErrCommentRejected      = errors.New("comments: rejected by anti-spam policy")
)

// PublicCommentView is the zero-PII comment model exposed to visitors and comment-widget.js.
type PublicCommentView struct {
	ID          string              `json:"id"`
	ParentID    string              `json:"parent_id,omitempty"`
	ReplyToName string              `json:"reply_to_name,omitempty"`
	AuthorName  string              `json:"author_name"`
	AuthorURL   string              `json:"author_url,omitempty"`
	AvatarURL   string              `json:"avatar_url"`
	AvatarSeed  string              `json:"avatar_seed"`
	IsAuthor    bool                `json:"is_author,omitempty"`
	ContentHTML string              `json:"content_html"`
	ContentMD   string              `json:"content_md,omitempty"` // Only populated when CanEdit==true!
	GeoRegion   string              `json:"geo_region,omitempty"`
	IPMasked    string              `json:"ip_masked,omitempty"`    // Only populated for active admin!
	CanBanIP    bool                `json:"can_ban_ip,omitempty"`   // Only populated for active admin!
	CanModerate bool                `json:"can_moderate,omitempty"` // Only populated for active admin!
	Status      string              `json:"status"`
	CanEdit     bool                `json:"can_edit"`
	CreatedAt   int64               `json:"created_at"`
	Replies     []PublicCommentView `json:"replies,omitempty"`
}

// RecentCommentItem is displayed in the volo-studio sidebar (strictly excluding protected articles!).
type RecentCommentItem struct {
	ID          string `json:"id"`
	AuthorName  string `json:"author_name"`
	Excerpt     string `json:"excerpt"`
	TargetTitle string `json:"target_title"`
	TargetURL   string `json:"target_url"`
	CreatedAt   int64  `json:"created_at"`
}

// CreateCommentInput carries user/guest comment submission fields.
type CreateCommentInput struct {
	TargetType   string `json:"target_type"` // article | page
	TargetID     string `json:"target_id"`
	ParentID     string `json:"parent_id"`
	AuthorName   string `json:"author_name"`
	AuthorURL    string `json:"author_url"`
	NotifyEmail  string `json:"notify_email"`
	ContentMD    string `json:"content_md"`
	EnvFPSignal  string `json:"env_fp"`
	PoWChallenge string `json:"pow_challenge"`
	PoWSolution  string `json:"pow_solution"`
	Honeypot     string `json:"website_hp"` // Invisible honeypot field
	ClientIP     string `json:"-"`
	GeoRegion    string `json:"-"`
	Locale       string `json:"-"`
}

// ListTargetComments returns the nested 2-level public comment tree for an article or standalone page.
// Surface 4 Enforcement: if target is a password-protected article and request has NOT unlocked it,
// ListTargetComments returns visibility.ErrProtectedForbidden (HTTP 403)!
func (s *Service) ListTargetComments(
	ctx context.Context,
	r *http.Request,
	viewer *domain.User,
	targetType string,
	targetID string,
) ([]PublicCommentView, error) {
	tType := strings.TrimSpace(targetType)
	tID := strings.TrimSpace(targetID)

	var postAuthorID string
	switch tType {
	case domain.TargetTypeArticle:
		raw, err := s.GetRawArticleByID(ctx, tID)
		if err != nil {
			return nil, err
		}
		if !s.guard.CanAccessArticleComments(r, raw, viewer) {
			return nil, visibility.ErrProtectedForbidden
		}
		postAuthorID = raw.AuthorID
	case domain.TargetTypePage:
		pg, err := sqlx.Get[domain.Page](ctx, s.store, `SELECT id, author_id, is_published FROM pages WHERE id = ?`, tID)
		if err != nil || !pg.IsPublished {
			return nil, sql.ErrNoRows
		}
		postAuthorID = pg.AuthorID
	default:
		return nil, errors.New("comments: invalid target_type")
	}

	var callerGuestHash string
	if r != nil && s.passports != nil {
		if gp, ok := s.passports.ResolveOrHeal(nil, r, false); ok && gp != nil {
			callerGuestHash = gp.GuestIDHash
		}
	}
	callerUserID := ""
	if viewer != nil && viewer.Status == domain.UserStatusActive {
		callerUserID = viewer.ID
	}

	rows, err := sqlx.Select[domain.Comment](
		ctx, s.store,
		`SELECT * FROM comments
		 WHERE target_type = ? AND target_id = ? AND is_deleted = ?
		 ORDER BY created_at ASC, id ASC`,
		tType, tID, false,
	)
	if err != nil {
		return nil, err
	}

	nowMS := time.Now().UnixMilli()
	roots := make([]PublicCommentView, 0)
	rootIdxByID := make(map[string]int)

	for _, c := range rows {
		isAdmin := viewer != nil && viewer.Role == domain.RoleAdmin && viewer.Status == domain.UserStatusActive
		isOwn := canModifyComment(c, callerUserID, callerGuestHash, nowMS)
		if c.Status != domain.CommentStatusApproved && !isOwn && !isAdmin {
			continue
		}
		view := s.toPublicCommentView(c, postAuthorID, isOwn || isAdmin, viewer)
		if strings.TrimSpace(c.ParentID) == "" {
			rootIdxByID[c.ID] = len(roots)
			roots = append(roots, view)
		} else if idx, exists := rootIdxByID[c.ParentID]; exists {
			roots[idx].Replies = append(roots[idx].Replies, view)
		} else {
			// Flatten deeper replies onto top level with @ReplyToName preserved (clean 2-level design).
			roots = append(roots, view)
		}
	}
	// Top-level comments sort newest-first (DESC) while replies inside each thread stay chronological (ASC).
	for i, j := 0, len(roots)-1; i < j; i, j = i+1, j-1 {
		roots[i], roots[j] = roots[j], roots[i]
	}
	return roots, nil
}

// ListRecentPublicComments fetches recent approved comments strictly excluding any password-protected articles (Surface 4).
func (s *Service) ListRecentPublicComments(ctx context.Context, limit int) ([]RecentCommentItem, error) {
	if limit <= 0 {
		limit = 8
	}
	if limit <= maxCachedRecentCmts {
		if snap, err := s.getChromeSnapshot(ctx); err == nil && snap != nil {
			if len(snap.recentCmts) <= limit {
				return snap.recentCmts, nil
			}
			return snap.recentCmts[:limit], nil
		}
	}
	return s.queryRecentPublicComments(ctx, limit)
}

func (s *Service) queryRecentPublicComments(ctx context.Context, limit int) ([]RecentCommentItem, error) {
	if limit <= 0 {
		limit = 8
	}
	type joinedRecent struct {
		ID          string `db:"id"`
		AuthorName  string `db:"author_name"`
		ContentMD   string `db:"content_md"`
		TargetTitle string `db:"target_title"`
		TargetSlug  string `db:"target_slug"`
		TargetType  string `db:"target_type"`
		CreatedAt   int64  `db:"created_at"`
	}
	baseSQL := `
		SELECT c.id, c.author_name, c.content_md, a.title AS target_title, a.slug AS target_slug, c.target_type, c.created_at
		FROM comments c
		INNER JOIN articles a ON c.target_type = 'article' AND a.id = c.target_id
		WHERE c.status = ? AND c.is_deleted = ? AND a.status = ? AND a.password_ciphertext = ''
		ORDER BY c.created_at DESC`
	q, pageArgs := s.store.Dialect().PaginateSQL(baseSQL, limit, 0)
	rows, err := sqlx.Select[joinedRecent](ctx, s.store, q, append([]any{domain.CommentStatusApproved, false, domain.ArticleStatusPublished}, pageArgs...)...)
	if err != nil {
		return nil, err
	}
	out := make([]RecentCommentItem, 0, len(rows))
	for _, r := range rows {
		rendered := s.renderer.Render(r.ContentMD, render.Options{Mode: render.RenderModeWeb})
		out = append(out, RecentCommentItem{
			ID:          r.ID,
			AuthorName:  r.AuthorName,
			Excerpt:     truncateForSummary(rendered.PlainText, 64),
			TargetTitle: r.TargetTitle,
			TargetURL:   visibility.ArticlePath(r.TargetSlug) + "#comment-" + r.ID,
			CreatedAt:   r.CreatedAt,
		})
	}
	return out, nil
}

// CreateComment validates access, Surface 4 unlock state, GuestPassport, PoW/Honeypot, offline DFA + plugin AntiSpam,
// encrypts optional notify_email via AES-256-GCM, and atomically persists the comment.
func (s *Service) CreateComment(
	ctx context.Context,
	w http.ResponseWriter,
	r *http.Request,
	viewer *domain.User,
	in CreateCommentInput,
) (PublicCommentView, string, error) {
	snap := s.settings.Snapshot()
	if snap.Reading.CommentAccessMode == domain.CommentAccessOff {
		return PublicCommentView{}, "", ErrCommentsClosed
	}
	isLoggedUser := viewer != nil && viewer.Status == domain.UserStatusActive
	if snap.Reading.CommentAccessMode == domain.CommentAccessRegistered && !isLoggedUser {
		return PublicCommentView{}, "", ErrRegisteredOnly
	}

	tType := strings.TrimSpace(in.TargetType)
	tID := strings.TrimSpace(in.TargetID)
	var postAuthorID string

	switch tType {
	case domain.TargetTypeArticle:
		raw, err := s.GetRawArticleByID(ctx, tID)
		if err != nil || raw.Status != domain.ArticleStatusPublished {
			return PublicCommentView{}, "", sql.ErrNoRows
		}
		if !raw.AllowComment {
			return PublicCommentView{}, "", ErrCommentsClosed
		}
		if !s.guard.CanAccessArticleComments(r, raw, viewer) {
			return PublicCommentView{}, "", visibility.ErrProtectedForbidden
		}
		postAuthorID = raw.AuthorID
	case domain.TargetTypePage:
		pg, err := sqlx.Get[domain.Page](ctx, s.store, `SELECT * FROM pages WHERE id = ?`, tID)
		if err != nil || !pg.IsPublished {
			return PublicCommentView{}, "", sql.ErrNoRows
		}
		if !pg.AllowComment {
			return PublicCommentView{}, "", ErrCommentsClosed
		}
		postAuthorID = pg.AuthorID
	default:
		return PublicCommentView{}, "", errors.New("comments: unsupported target_type")
	}

	contentMD := strings.TrimSpace(in.ContentMD)
	if contentMD == "" || len([]rune(contentMD)) > 4000 {
		return PublicCommentView{}, "", errors.New("comments: content length must be between 1 and 4000 characters")
	}

	// Resolve identity: authenticated user OR self-healing GuestPassport + PoW verification.
	var (
		userID        string
		guestIDHash   string
		passportToken string
		authorName    string
		authorURL     string
		avatarURL     string
	)
	if isLoggedUser {
		userID = viewer.ID
		authorName = viewer.DisplayName
		if authorName == "" {
			authorName = viewer.Username
		}
		authorURL = viewer.WebsiteURL
		avatarURL = viewer.AvatarURL
		if avatarURL == "" {
			avatarURL = "/avatars/" + viewer.ID + ".svg"
		}
	} else {
		if s.pow != nil {
			if err := s.pow.Verify(in.PoWChallenge, in.PoWSolution, in.Honeypot); err != nil {
				return PublicCommentView{}, "", err
			}
		}
		gp, _ := s.passports.ResolveOrHeal(w, r, true)
		guestIDHash = gp.GuestIDHash
		passportToken = gp.SignedToken
		authorName = strings.TrimSpace(in.AuthorName)
		if authorName == "" || len([]rune(authorName)) > 48 {
			return PublicCommentView{}, "", errors.New("comments: author_name is required (1..48 chars)")
		}
		if cleanURL, ok := sanitizeCommentAuthorURL(in.AuthorURL); ok {
			authorURL = cleanURL
		}
		avatarURL = "/avatars/" + gp.GuestID + ".svg"
	}

	var envFPHash string
	if s.keys != nil && strings.TrimSpace(in.EnvFPSignal) != "" {
		envFPHash = guest.ComputeEnvFPHash(s.keys, in.EnvFPSignal)
	}

	// Resolve parent & reply_to_name for 2-level replies.
	parentID := strings.TrimSpace(in.ParentID)
	replyToName := ""
	if parentID != "" {
		parent, pErr := sqlx.Get[domain.Comment](
			ctx, s.store,
			`SELECT id, parent_id, author_name, target_type, target_id, is_deleted FROM comments WHERE id = ?`,
			parentID,
		)
		if pErr != nil || parent.IsDeleted || parent.TargetType != tType || parent.TargetID != tID {
			return PublicCommentView{}, "", errors.New("comments: parent comment not found")
		}
		replyToName = parent.AuthorName
		if parent.ParentID != "" {
			// Flatten onto 2nd level under root comment while keeping @ReplyToName!
			parentID = parent.ParentID
		}
	}

	ipMasked := ""
	if parsedIP, ipErr := netip.ParseAddr(strings.TrimSpace(in.ClientIP)); ipErr == nil {
		ipMasked = security.MaskIP(parsedIP)
	}

	// Offline keyword filter + hot-swappable plugin AntiSpamProvider!
	status := domain.CommentStatusApproved
	if viewer == nil || (viewer.Role != domain.RoleAdmin && viewer.Role != domain.RoleEditor) {
		switch snap.Reading.CommentModerationMode {
		case domain.CommentModerationAll:
			status = domain.CommentStatusPending
		case domain.CommentModerationSmart:
			if containsBlockedKeyword(contentMD, authorName, snap.Reading.BlockedKeywords, snap.Reading.AllowedKeywords) {
				status = domain.CommentStatusPending
			}
		}
		if s.plugins != nil {
			verdict := s.plugins.Providers().EvaluateComment(ctx, plugin.CommentCheckInput{
				TargetType: tType,
				TargetID:   tID,
				AuthorName: authorName,
				AuthorURL:  authorURL,
				ContentMD:  contentMD,
				IPMasked:   ipMasked,
				GeoRegion:  in.GeoRegion,
				Locale:     in.Locale,
				IsGuest:    !isLoggedUser,
			})
			if verdict.Action == plugin.SpamActionReject {
				return PublicCommentView{}, "", ErrCommentRejected
			}
			if verdict.Action == plugin.SpamActionModerate {
				status = domain.CommentStatusPending
			}
		}
	}

	commentID := id.Next()
	emailCipher := ""
	if trimmedEmail := strings.TrimSpace(in.NotifyEmail); trimmedEmail != "" && strings.Contains(trimmedEmail, "@") && s.keys != nil {
		sealed, sErr := s.keys.SealString(trimmedEmail, "comments.notify_email:"+commentID)
		if sErr == nil {
			emailCipher = sealed
		}
	}

	now := time.Now().UnixMilli()
	record := domain.Comment{
		ID:                    commentID,
		TargetType:            tType,
		TargetID:              tID,
		ParentID:              parentID,
		ReplyToName:           replyToName,
		UserID:                userID,
		GuestIDHash:           guestIDHash,
		EnvFPHash:             envFPHash,
		AuthorName:            authorName,
		AuthorURL:             authorURL,
		AvatarURL:             avatarURL,
		NotifyEmailCiphertext: emailCipher,
		NotifyLocale:          in.Locale,
		ClientIP:              in.ClientIP,
		IPMasked:              ipMasked,
		GeoRegion:             in.GeoRegion,
		ContentMD:             contentMD,
		Status:                status,
		IsDeleted:             false,
		CreatedAt:             now,
		UpdatedAt:             now,
	}

	if s.plugins != nil {
		hookOut, hookErr := s.plugins.DispatchHook(ctx, plugin.HookCommentBeforeCreate, map[string]any{
			"id":          record.ID,
			"target_type": record.TargetType,
			"target_id":   record.TargetID,
			"author_name": record.AuthorName,
			"content_md":  record.ContentMD,
			"status":      record.Status,
		})
		if hookErr != nil {
			var rej *plugin.HookRejectedError
			if errors.As(hookErr, &rej) {
				return PublicCommentView{}, "", ErrCommentRejected
			}
		} else if resMap, ok := hookOut.(map[string]any); ok {
			if nextMD, sOk := resMap["content_md"].(string); sOk {
				trimmedNext := strings.TrimSpace(nextMD)
				if trimmedNext == "" || len([]rune(trimmedNext)) > 4000 {
					return PublicCommentView{}, "", ErrCommentRejected
				}
				record.ContentMD = trimmedNext
			}
			if nextStatus, sOk := resMap["status"].(string); sOk && nextStatus == domain.CommentStatusPending {
				record.Status = domain.CommentStatusPending
			}
		}
	}

	err := s.store.WithTx(ctx, func(tx *sqlx.Tx) error {
		if _, iErr := tx.ExecContext(
			ctx,
			`INSERT INTO comments (
				id, target_type, target_id, parent_id, reply_to_name, user_id, guest_id_hash, env_fp_hash,
				author_name, author_url, avatar_url, notify_email_ciphertext, notify_locale,
				client_ip, ip_masked, geo_region, content_md, status, is_deleted, created_at, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			record.ID, record.TargetType, record.TargetID, record.ParentID, record.ReplyToName,
			record.UserID, record.GuestIDHash, record.EnvFPHash, record.AuthorName, record.AuthorURL,
			record.AvatarURL, record.NotifyEmailCiphertext, record.NotifyLocale, record.ClientIP,
			record.IPMasked, record.GeoRegion, record.ContentMD, record.Status, record.IsDeleted,
			record.CreatedAt, record.UpdatedAt,
		); iErr != nil {
			return iErr
		}
		if record.Status == domain.CommentStatusApproved {
			return syncApprovedCommentCountTx(ctx, tx, record.TargetType, record.TargetID)
		}
		return nil
	})
	if err != nil {
		return PublicCommentView{}, "", err
	}

	if s.plugins != nil {
		_, _ = s.plugins.DispatchHook(ctx, plugin.HookCommentCreated, map[string]any{
			"id":          record.ID,
			"target_type": record.TargetType,
			"target_id":   record.TargetID,
			"author_name": record.AuthorName,
			"status":      record.Status,
		})
	}

	return s.toPublicCommentView(record, postAuthorID, true, viewer), passportToken, nil
}

// UpdateOwnComment edits a comment ONLY if caller owns it via authenticated UserID OR valid
// signed GuestPassport within GuestEditWindow (30 minutes). NEVER accepts unsigned `env_fp`!
func (s *Service) UpdateOwnComment(
	ctx context.Context,
	r *http.Request,
	viewer *domain.User,
	commentID string,
	newContentMD string,
) (PublicCommentView, error) {
	trimmed := strings.TrimSpace(newContentMD)
	if trimmed == "" || len([]rune(trimmed)) > 4000 {
		return PublicCommentView{}, errors.New("comments: content length must be between 1 and 4000 characters")
	}

	existing, err := sqlx.Get[domain.Comment](ctx, s.store, `SELECT * FROM comments WHERE id = ? AND is_deleted = ?`, strings.TrimSpace(commentID), false)
	if err != nil {
		return PublicCommentView{}, err
	}

	var postAuthorID string
	// Surface 4: also verify protected article remains unlocked!
	if existing.TargetType == domain.TargetTypeArticle {
		raw, aErr := s.GetRawArticleByID(ctx, existing.TargetID)
		if aErr != nil || !s.guard.CanAccessArticleComments(r, raw, viewer) {
			return PublicCommentView{}, visibility.ErrProtectedForbidden
		}
		postAuthorID = raw.AuthorID
	} else if existing.TargetType == domain.TargetTypePage {
		_ = s.store.QueryRowContext(ctx, `SELECT author_id FROM pages WHERE id = ?`, existing.TargetID).Scan(&postAuthorID)
	}

	var callerGuestHash, callerUserID string
	if viewer != nil && viewer.Status == domain.UserStatusActive {
		callerUserID = viewer.ID
	}
	if r != nil && s.passports != nil {
		if gp, ok := s.passports.ResolveOrHeal(nil, r, false); ok && gp != nil {
			callerGuestHash = gp.GuestIDHash
		}
	}

	nowMS := time.Now().UnixMilli()
	isAdmin := viewer != nil && viewer.Role == domain.RoleAdmin && viewer.Status == domain.UserStatusActive
	if !isAdmin && !canModifyComment(existing, callerUserID, callerGuestHash, nowMS) {
		return PublicCommentView{}, ErrCommentEditForbidden
	}

	existing.ContentMD = trimmed
	existing.UpdatedAt = nowMS
	if _, err := s.store.ExecContext(ctx, `UPDATE comments SET content_md = ?, updated_at = ? WHERE id = ?`, trimmed, nowMS, existing.ID); err != nil {
		return PublicCommentView{}, err
	}
	return s.toPublicCommentView(existing, postAuthorID, true, viewer), nil
}

// DeleteOwnComment soft-deletes a comment ONLY if caller owns it via authenticated UserID OR valid
// signed GuestPassport within 30 minutes (or is admin). NEVER accepts unsigned `env_fp`!
func (s *Service) DeleteOwnComment(ctx context.Context, r *http.Request, viewer *domain.User, commentID string) error {
	existing, err := sqlx.Get[domain.Comment](ctx, s.store, `SELECT * FROM comments WHERE id = ? AND is_deleted = ?`, strings.TrimSpace(commentID), false)
	if err != nil {
		return err
	}

	var callerGuestHash, callerUserID string
	if viewer != nil && viewer.Status == domain.UserStatusActive {
		callerUserID = viewer.ID
	}
	if r != nil && s.passports != nil {
		if gp, ok := s.passports.ResolveOrHeal(nil, r, false); ok && gp != nil {
			callerGuestHash = gp.GuestIDHash
		}
	}

	nowMS := time.Now().UnixMilli()
	isAdmin := viewer != nil && viewer.Role == domain.RoleAdmin && viewer.Status == domain.UserStatusActive
	if !isAdmin && !canModifyComment(existing, callerUserID, callerGuestHash, nowMS) {
		return ErrCommentEditForbidden
	}

	return s.store.WithTx(ctx, func(tx *sqlx.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE comments SET is_deleted = ?, updated_at = ? WHERE id = ?`, true, nowMS, existing.ID); err != nil {
			return err
		}
		return syncApprovedCommentCountTx(ctx, tx, existing.TargetType, existing.TargetID)
	})
}

// ToggleReaction toggles an emoji/like reaction deduplicated by actorHash (`user:<id>` or `envfp:<hash>`).
func (s *Service) ToggleReaction(ctx context.Context, targetType string, targetID string, actorHash string, kind string) (map[string]int, bool, error) {
	cleanKind := strings.ToLower(strings.TrimSpace(kind))
	switch cleanKind {
	case "like", "clap", "insight", "rocket", "heart":
	default:
		return nil, false, errors.New("reactions: unsupported reaction kind")
	}
	if actorHash == "" {
		return nil, false, errors.New("reactions: missing actor identifier")
	}

	added := false
	err := s.store.WithTx(ctx, func(tx *sqlx.Tx) error {
		res, dErr := tx.ExecContext(
			ctx,
			`DELETE FROM reactions WHERE target_type = ? AND target_id = ? AND actor_hash = ? AND kind = ?`,
			targetType, targetID, actorHash, cleanKind,
		)
		if dErr != nil {
			return dErr
		}
		if n, _ := res.RowsAffected(); n > 0 {
			added = false
			return nil
		}
		upsert := tx.Dialect().UpsertSQL(
			"reactions",
			[]string{"target_type", "target_id", "actor_hash", "kind", "created_at"},
			[]string{"target_type", "target_id", "actor_hash", "kind"},
			[]string{},
		)
		_, iErr := tx.ExecContext(ctx, upsert, targetType, targetID, actorHash, cleanKind, time.Now().UnixMilli())
		added = iErr == nil
		return iErr
	})
	if err != nil {
		return nil, false, err
	}
	counts, err := s.GetReactionCounts(ctx, targetType, targetID)
	return counts, added, err
}

// GetReactionCounts returns reaction counts grouped by kind.
func (s *Service) GetReactionCounts(ctx context.Context, targetType string, targetID string) (map[string]int, error) {
	type row struct {
		Kind string `db:"kind"`
		Cnt  int    `db:"cnt"`
	}
	rows, err := sqlx.Select[row](
		ctx, s.store,
		`SELECT kind, COUNT(*) AS cnt FROM reactions WHERE target_type = ? AND target_id = ? GROUP BY kind`,
		targetType, targetID,
	)
	if err != nil {
		return nil, err
	}
	out := make(map[string]int, len(rows))
	for _, r := range rows {
		out[r.Kind] = r.Cnt
	}
	return out, nil
}

func canModifyComment(c domain.Comment, callerUserID string, callerGuestHash string, nowMS int64) bool {
	if callerUserID != "" && c.UserID != "" && c.UserID == callerUserID {
		return true
	}
	if callerGuestHash != "" && c.GuestIDHash != "" && c.GuestIDHash == callerGuestHash {
		if nowMS-c.CreatedAt <= GuestEditWindow.Milliseconds() {
			return true
		}
	}
	return false
}

func (s *Service) toPublicCommentView(c domain.Comment, postAuthorID string, canEdit bool, viewer *domain.User) PublicCommentView {
	out := s.renderer.Render(c.ContentMD, render.Options{
		Mode: render.RenderModeWeb,
	})
	seed := strings.TrimSpace(c.UserID)
	if seed == "" && len(c.GuestIDHash) >= 16 {
		seed = c.GuestIDHash[:16]
	}
	if seed == "" && len(c.EnvFPHash) >= 16 {
		seed = c.EnvFPHash[:16]
	}
	if seed == "" {
		seed = c.ID
	}
	ownerID := ""
	if snap := s.caches.chrome.Load(); snap != nil {
		ownerID = snap.owner.ID
	}
	isAuthor := c.UserID != "" && ((postAuthorID != "" && c.UserID == postAuthorID) || (ownerID != "" && c.UserID == ownerID))
	avatarURL := strings.TrimSpace(c.AvatarURL)
	if avatarURL == "" {
		avatarURL = "/avatars/" + seed + ".svg"
	}
	view := PublicCommentView{
		ID:          c.ID,
		ParentID:    c.ParentID,
		ReplyToName: c.ReplyToName,
		AuthorName:  c.AuthorName,
		AuthorURL:   c.AuthorURL,
		AvatarURL:   avatarURL,
		AvatarSeed:  seed,
		IsAuthor:    isAuthor,
		ContentHTML: out.HTML,
		GeoRegion:   c.GeoRegion,
		Status:      c.Status,
		CanEdit:     canEdit,
		CreatedAt:   c.CreatedAt,
	}
	if canEdit {
		view.ContentMD = c.ContentMD
	}
	if viewer != nil && viewer.Role == domain.RoleAdmin && viewer.Status == domain.UserStatusActive {
		view.IPMasked = c.IPMasked
		view.CanBanIP = strings.TrimSpace(c.ClientIP) != ""
		view.CanModerate = true
	}
	return view
}

func syncApprovedCommentCountTx(ctx context.Context, tx *sqlx.Tx, targetType string, targetID string) error {
	var cnt int
	if err := tx.QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM comments WHERE target_type = ? AND target_id = ? AND status = ? AND is_deleted = ?`,
		targetType, targetID, domain.CommentStatusApproved, false,
	).Scan(&cnt); err != nil {
		return err
	}
	switch targetType {
	case domain.TargetTypeArticle:
		_, err := tx.ExecContext(ctx, `UPDATE articles SET comment_count = ? WHERE id = ?`, cnt, targetID)
		return err
	case domain.TargetTypePage:
		_, err := tx.ExecContext(ctx, `UPDATE pages SET comment_count = ? WHERE id = ?`, cnt, targetID)
		return err
	default:
		return nil
	}
}

func sanitizeCommentAuthorURL(raw string) (string, bool) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", false
	}
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", false
	}
	return u.String(), true
}

func containsBlockedKeyword(content string, author string, blocked []string, allowed []string) bool {
	haystack := strings.ToLower(content + " " + author)
	for _, allow := range allowed {
		a := strings.ToLower(strings.TrimSpace(allow))
		if a != "" {
			haystack = strings.ReplaceAll(haystack, a, "")
		}
	}
	for _, kw := range blocked {
		k := strings.ToLower(strings.TrimSpace(kw))
		if k != "" && strings.Contains(haystack, k) {
			return true
		}
	}
	// Built-in heuristic: > 4 external http(s) links in a single guest comment triggers moderation.
	if strings.Count(haystack, "http://")+strings.Count(haystack, "https://") > 4 {
		return true
	}
	return false
}
