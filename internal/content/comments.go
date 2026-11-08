package content

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Willh92/Volo-go/internal/config"
	"github.com/Willh92/Volo-go/internal/db/dialect"
	"github.com/Willh92/Volo-go/internal/db/sqlx"
	"github.com/Willh92/Volo-go/internal/domain"
	"github.com/Willh92/Volo-go/internal/guest"
	"github.com/Willh92/Volo-go/internal/render"
	"github.com/Willh92/Volo-go/internal/repo"
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

// PaginatedCommentsResult holds a paginated slice of top-level comment threads (with full replies per thread).
type PaginatedCommentsResult struct {
	Comments      []PublicCommentView `json:"comments"`
	Page          int                 `json:"page"`
	PerPage       int                 `json:"per_page"`
	TotalRoots    int                 `json:"total_roots"`
	TotalComments int                 `json:"total_comments"`
	TotalPages    int                 `json:"total_pages"`
	HasMore       bool                `json:"has_more"`
}

// RecentCommentItem is displayed in the volo-studio sidebar and /dynamic page (strictly excluding protected/unpublished targets!).
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

func (s *Service) authorizeCommentTarget(
	ctx context.Context,
	r *http.Request,
	viewer *domain.User,
	targetType string,
	targetID string,
) (string, error) {
	tType := strings.TrimSpace(targetType)
	tID := strings.TrimSpace(targetID)

	switch tType {
	case domain.TargetTypeArticle:
		raw, err := s.GetRawArticleByID(ctx, tID)
		if err != nil {
			return "", err
		}
		if raw.Status != domain.ArticleStatusPublished && !s.guard.IsAuthorOrAdmin(raw, viewer) {
			return "", sql.ErrNoRows
		}
		if !s.guard.CanAccessArticleComments(r, raw, viewer) {
			return "", visibility.ErrProtectedForbidden
		}
		return raw.AuthorID, nil
	case domain.TargetTypePage:
		pg, err := sqlx.Get[domain.Page](ctx, s.store, `SELECT id, author_id, is_published FROM pages WHERE id = ?`, tID)
		if err != nil {
			return "", err
		}
		isPageStaff := viewer != nil && viewer.Status == domain.UserStatusActive &&
			(viewer.Role == domain.RoleAdmin || (viewer.Role == domain.RoleEditor && viewer.ID == pg.AuthorID))
		if !pg.IsPublished && !isPageStaff {
			return "", sql.ErrNoRows
		}
		return pg.AuthorID, nil
	default:
		return "", errors.New("comments: invalid target_type")
	}
}

func (s *Service) buildTargetCommentTree(
	ctx context.Context,
	r *http.Request,
	viewer *domain.User,
	targetType string,
	targetID string,
) ([]PublicCommentView, int, error) {
	tType := strings.TrimSpace(targetType)
	tID := strings.TrimSpace(targetID)
	postAuthorID, err := s.authorizeCommentTarget(ctx, r, viewer, tType, tID)
	if err != nil {
		return nil, 0, err
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
		return nil, 0, err
	}

	nowMS := time.Now().UnixMilli()
	roots := make([]PublicCommentView, 0)
	rootIdxByID := make(map[string]int)
	totalComments := 0

	for _, c := range rows {
		isAdmin := viewer != nil && viewer.Role == domain.RoleAdmin && viewer.Status == domain.UserStatusActive
		isOwn := canModifyComment(c, callerUserID, callerGuestHash, nowMS)
		if c.Status != domain.CommentStatusApproved && !isOwn && !isAdmin {
			continue
		}
		totalComments++
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
	return roots, totalComments, nil
}

// ListTargetComments returns the nested 2-level public comment tree for an article or standalone page.
// Surface 4 Enforcement: if target is unpublished -> sql.ErrNoRows (404); if password-protected and locked -> ErrProtectedForbidden (403)!
func (s *Service) ListTargetComments(
	ctx context.Context,
	r *http.Request,
	viewer *domain.User,
	targetType string,
	targetID string,
) ([]PublicCommentView, error) {
	roots, _, err := s.buildTargetCommentTree(ctx, r, viewer, targetType, targetID)
	return roots, err
}

// ListTargetCommentsPage returns a paginated slice of root comment threads (newest-first, with replies intact).
// If focusCommentID matches a root or reply, page automatically jumps to the page containing that thread.
func (s *Service) ListTargetCommentsPage(
	ctx context.Context,
	r *http.Request,
	viewer *domain.User,
	targetType string,
	targetID string,
	page int,
	perPage int,
	focusCommentID string,
) (PaginatedCommentsResult, error) {
	roots, totalComments, err := s.buildTargetCommentTree(ctx, r, viewer, targetType, targetID)
	if err != nil {
		return PaginatedCommentsResult{}, err
	}
	if perPage <= 0 {
		perPage = 20
	}
	if perPage > 100 {
		perPage = 100
	}
	totalRoots := len(roots)
	totalPages := 1
	if totalRoots > 0 {
		totalPages = (totalRoots + perPage - 1) / perPage
	}

	if focusID := strings.TrimSpace(focusCommentID); focusID != "" {
		for i, root := range roots {
			matched := root.ID == focusID
			if !matched {
				for _, rep := range root.Replies {
					if rep.ID == focusID {
						matched = true
						break
					}
				}
			}
			if matched {
				page = (i / perPage) + 1
				break
			}
		}
	}

	if page < 1 {
		page = 1
	}
	if page > totalPages {
		page = totalPages
	}

	var slice []PublicCommentView
	if totalRoots == 0 {
		slice = []PublicCommentView{}
	} else {
		start := (page - 1) * perPage
		end := start + perPage
		if end > totalRoots {
			end = totalRoots
		}
		slice = roots[start:end]
	}

	return PaginatedCommentsResult{
		Comments:      slice,
		Page:          page,
		PerPage:       perPage,
		TotalRoots:    totalRoots,
		TotalComments: totalComments,
		TotalPages:    totalPages,
		HasMore:       page < totalPages,
	}, nil
}

// ListRecentPublicComments fetches recent approved comments strictly excluding any password-protected or unpublished targets (Surface 4).
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

// ListRecentPublicCommentsPage fetches a paginated page of recent public comments across published public articles and standalone pages.
func (s *Service) ListRecentPublicCommentsPage(ctx context.Context, page int, perPage int) ([]RecentCommentItem, int, error) {
	limit, offset := dialect.NormalizePage(page, perPage, 20, 100)
	whereFrom := `
		FROM comments c
		LEFT JOIN articles a ON c.target_type = 'article' AND a.id = c.target_id
		LEFT JOIN pages p ON c.target_type = 'page' AND p.id = c.target_id
		WHERE c.status = ? AND c.is_deleted = ?
		  AND ((c.target_type = 'article' AND a.status = ? AND a.password_ciphertext = '')
		    OR (c.target_type = 'page' AND p.is_published = ?))`
	args := []any{domain.CommentStatusApproved, false, domain.ArticleStatusPublished, true}

	var total int
	if err := s.store.QueryRowContext(ctx, "SELECT COUNT(*) "+whereFrom, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	if total == 0 {
		return []RecentCommentItem{}, 0, nil
	}
	items, err := s.queryRecentPublicCommentsOffset(ctx, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	return items, total, nil
}

func (s *Service) queryRecentPublicComments(ctx context.Context, limit int) ([]RecentCommentItem, error) {
	if limit <= 0 {
		limit = 8
	}
	return s.queryRecentPublicCommentsOffset(ctx, limit, 0)
}

func (s *Service) queryRecentPublicCommentsOffset(ctx context.Context, limit int, offset int) ([]RecentCommentItem, error) {
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
		SELECT c.id, c.author_name, c.content_md,
		       COALESCE(a.title, p.title, '') AS target_title,
		       COALESCE(a.slug, p.slug, '') AS target_slug,
		       c.target_type, c.created_at
		FROM comments c
		LEFT JOIN articles a ON c.target_type = 'article' AND a.id = c.target_id
		LEFT JOIN pages p ON c.target_type = 'page' AND p.id = c.target_id
		WHERE c.status = ? AND c.is_deleted = ?
		  AND ((c.target_type = 'article' AND a.status = ? AND a.password_ciphertext = '')
		    OR (c.target_type = 'page' AND p.is_published = ?))
		ORDER BY c.created_at DESC`
	q, pageArgs := s.store.Dialect().PaginateSQL(baseSQL, limit, offset)
	rows, err := sqlx.Select[joinedRecent](
		ctx, s.store, q,
		append([]any{domain.CommentStatusApproved, false, domain.ArticleStatusPublished, true}, pageArgs...)...,
	)
	if err != nil {
		return nil, err
	}
	out := make([]RecentCommentItem, 0, len(rows))
	for _, r := range rows {
		rendered := s.renderer.Render(r.ContentMD, render.Options{Mode: render.RenderModeWeb})
		targetURL := visibility.ArticlePath(r.TargetSlug) + "#comment-" + r.ID
		if r.TargetType == domain.TargetTypePage {
			targetURL = "/" + r.TargetSlug + "#comment-" + r.ID
		}
		out = append(out, RecentCommentItem{
			ID:          r.ID,
			AuthorName:  r.AuthorName,
			Excerpt:     truncateForSummary(rendered.PlainText, 64),
			TargetTitle: r.TargetTitle,
			TargetURL:   targetURL,
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
		if cleanURL, ok := sanitizeCommentAuthorURL(viewer.WebsiteURL); ok {
			authorURL = cleanURL
		}
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
				if reason := strings.TrimSpace(verdict.Reason); reason != "" {
					return PublicCommentView{}, "", fmt.Errorf("%w: %s", ErrCommentRejected, reason)
				}
				return PublicCommentView{}, "", ErrCommentRejected
			}
			if verdict.Action == plugin.SpamActionModerate {
				status = domain.CommentStatusPending
			} else if verdict.Action == plugin.SpamActionAllow && snap.Reading.CommentModerationMode != domain.CommentModerationAll {
				status = domain.CommentStatusApproved
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
				if reason := strings.TrimSpace(rej.Reason); reason != "" {
					return PublicCommentView{}, "", fmt.Errorf("%w: %s", ErrCommentRejected, reason)
				}
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

	// Surface 4: reject unpublished targets (404) and locked password-protected articles (403)!
	postAuthorID, tErr := s.authorizeCommentTarget(ctx, r, viewer, existing.TargetType, existing.TargetID)
	if tErr != nil {
		return PublicCommentView{}, tErr
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
	isStaff := viewer != nil && viewer.Status == domain.UserStatusActive &&
		(viewer.Role == domain.RoleAdmin || viewer.Role == domain.RoleEditor)
	if !isAdmin && !canModifyComment(existing, callerUserID, callerGuestHash, nowMS) {
		return PublicCommentView{}, ErrCommentEditForbidden
	}

	nextStatus := existing.Status
	if !isStaff {
		snap := s.settings.Snapshot()
		keywordTriggered := false
		if snap.Reading.CommentModerationMode == domain.CommentModerationAll {
			nextStatus = domain.CommentStatusPending
		} else if containsBlockedKeyword(trimmed, existing.AuthorName, snap.Reading.BlockedKeywords, snap.Reading.AllowedKeywords) {
			nextStatus = domain.CommentStatusPending
			keywordTriggered = true
		}

		if s.plugins != nil {
			verdict := s.plugins.Providers().EvaluateComment(ctx, plugin.CommentCheckInput{
				TargetType: existing.TargetType,
				TargetID:   existing.TargetID,
				AuthorName: existing.AuthorName,
				AuthorURL:  existing.AuthorURL,
				ContentMD:  trimmed,
				IPMasked:   existing.IPMasked,
				GeoRegion:  existing.GeoRegion,
				Locale:     existing.NotifyLocale,
				IsGuest:    strings.TrimSpace(existing.UserID) == "",
			})
			if verdict.Action == plugin.SpamActionReject {
				if reason := strings.TrimSpace(verdict.Reason); reason != "" {
					return PublicCommentView{}, fmt.Errorf("%w: %s", ErrCommentRejected, reason)
				}
				return PublicCommentView{}, ErrCommentRejected
			}
			if verdict.Action == plugin.SpamActionModerate {
				nextStatus = domain.CommentStatusPending
			} else if verdict.Action == plugin.SpamActionAllow && keywordTriggered && snap.Reading.CommentModerationMode != domain.CommentModerationAll {
				nextStatus = existing.Status
			}

			hookOut, hookErr := s.plugins.DispatchHook(ctx, plugin.HookCommentBeforeCreate, map[string]any{
				"id":          existing.ID,
				"target_type": existing.TargetType,
				"target_id":   existing.TargetID,
				"author_name": existing.AuthorName,
				"content_md":  trimmed,
				"status":      nextStatus,
				"is_edit":     true,
			})
			if hookErr != nil {
				var rej *plugin.HookRejectedError
				if errors.As(hookErr, &rej) {
					if reason := strings.TrimSpace(rej.Reason); reason != "" {
						return PublicCommentView{}, fmt.Errorf("%w: %s", ErrCommentRejected, reason)
					}
					return PublicCommentView{}, ErrCommentRejected
				}
			} else if resMap, ok := hookOut.(map[string]any); ok {
				if nextMD, sOk := resMap["content_md"].(string); sOk {
					trimmedNext := strings.TrimSpace(nextMD)
					if trimmedNext == "" || len([]rune(trimmedNext)) > 4000 {
						return PublicCommentView{}, ErrCommentRejected
					}
					trimmed = trimmedNext
				}
				if hs, sOk := resMap["status"].(string); sOk && hs == domain.CommentStatusPending {
					nextStatus = domain.CommentStatusPending
				}
			}
		}
	}

	prevStatus := existing.Status
	existing.ContentMD = trimmed
	existing.Status = nextStatus
	existing.UpdatedAt = nowMS
	if err := s.store.WithTx(ctx, func(tx *sqlx.Tx) error {
		if _, uErr := tx.ExecContext(ctx, `UPDATE comments SET content_md = ?, status = ?, updated_at = ? WHERE id = ?`, trimmed, nextStatus, nowMS, existing.ID); uErr != nil {
			return uErr
		}
		if prevStatus != nextStatus {
			return syncApprovedCommentCountTx(ctx, tx, existing.TargetType, existing.TargetID)
		}
		return nil
	}); err != nil {
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

	// Surface 4: reject unpublished targets (404) and locked password-protected articles (403)!
	if _, tErr := s.authorizeCommentTarget(ctx, r, viewer, existing.TargetType, existing.TargetID); tErr != nil {
		return tErr
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
// Uses WithBgTx so visitor reactions do NOT bump DataRevision or thrash public chrome/feed caches!
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
	err := s.store.WithBgTx(ctx, func(tx *sqlx.Tx) error {
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

// DecryptCommentNotifyEmail opens a comment's encrypted notify_email using canonical and legacy fallback AADs.
func (s *Service) DecryptCommentNotifyEmail(c domain.Comment) (string, bool) {
	if s == nil || s.keys == nil || strings.TrimSpace(c.NotifyEmailCiphertext) == "" {
		return "", false
	}
	plain, err := s.keys.OpenStringFallback(
		c.NotifyEmailCiphertext,
		config.CommentNotifyEmailAAD(c.ID),
		"comments:notify_email:"+c.ID,
		"comments:email:"+c.ID,
		"comment:email",
	)
	if err != nil || !strings.Contains(plain, "@") {
		return "", false
	}
	return plain, true
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

func (s *Service) renderCommentMarkdownHTML(c domain.Comment) string {
	if c.ID != "" {
		cacheKey := c.ID + ":" + strconv.FormatInt(c.UpdatedAt, 10)
		s.commentHTMLMu.RLock()
		if cached, ok := s.commentHTML[cacheKey]; ok {
			s.commentHTMLMu.RUnlock()
			return cached
		}
		s.commentHTMLMu.RUnlock()

		html := s.renderer.Render(c.ContentMD, render.Options{Mode: render.RenderModeWeb}).HTML
		s.commentHTMLMu.Lock()
		if s.commentHTML == nil {
			s.commentHTML = make(map[string]string, 256)
		} else if len(s.commentHTML) >= 4096 {
			clear(s.commentHTML)
		}
		s.commentHTML[cacheKey] = html
		s.commentHTMLMu.Unlock()
		return html
	}
	return s.renderer.Render(c.ContentMD, render.Options{Mode: render.RenderModeWeb}).HTML
}

func (s *Service) toPublicCommentView(c domain.Comment, postAuthorID string, canEdit bool, viewer *domain.User) PublicCommentView {
	renderedHTML := s.renderCommentMarkdownHTML(c)
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
		ContentHTML: renderedHTML,
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
	s := strings.TrimSpace(repo.StripASCIIControls(raw))
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
