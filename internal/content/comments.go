package content

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"sort"
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
	"github.com/Willh92/Volo-go/pkg/wordfilter"
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
	_ [0]func()

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
	_ [0]func()

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
	_ [0]func()

	ID          string `json:"id"`
	AuthorName  string `json:"author_name"`
	Excerpt     string `json:"excerpt"`
	TargetTitle string `json:"target_title"`
	TargetURL   string `json:"target_url"`
	CreatedAt   int64  `json:"created_at"`
}

// CreateCommentInput carries user/guest comment submission fields.
type CreateCommentInput struct {
	_ [0]func()

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
		var pg domain.Page
		err := sqlx.GetInto(ctx, s.store, &pg, `SELECT id, author_id, is_published FROM pages WHERE id = ?`, tID)
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

const publicCommentSelectCols = `id, parent_id, reply_to_name, user_id, guest_id_hash, env_fp_hash, author_name, author_url, avatar_url, ip_masked, CASE WHEN client_ip <> '' THEN 1 ELSE 0 END AS has_client_ip, geo_region, content_md, status, created_at, updated_at`

type publicCommentRow struct {
	_ [0]func()

	ID          string `db:"id"`
	ParentID    string `db:"parent_id"`
	ReplyToName string `db:"reply_to_name"`
	UserID      string `db:"user_id"`
	GuestIDHash string `db:"guest_id_hash"`
	EnvFPHash   string `db:"env_fp_hash"`
	AuthorName  string `db:"author_name"`
	AuthorURL   string `db:"author_url"`
	AvatarURL   string `db:"avatar_url"`
	IPMasked    string `db:"ip_masked"`
	HasClientIP int    `db:"has_client_ip"`
	GeoRegion   string `db:"geo_region"`
	ContentMD   string `db:"content_md"`
	Status      string `db:"status"`
	CreatedAt   int64  `db:"created_at"`
	UpdatedAt   int64  `db:"updated_at"`
}

func (r publicCommentRow) toDomainComment() domain.Comment {
	cIP := ""
	if r.HasClientIP != 0 {
		cIP = "present"
	}
	return domain.Comment{
		ID:          r.ID,
		ParentID:    r.ParentID,
		ReplyToName: r.ReplyToName,
		UserID:      r.UserID,
		GuestIDHash: r.GuestIDHash,
		EnvFPHash:   r.EnvFPHash,
		AuthorName:  r.AuthorName,
		AuthorURL:   r.AuthorURL,
		AvatarURL:   r.AvatarURL,
		ClientIP:    cIP,
		IPMasked:    r.IPMasked,
		GeoRegion:   r.GeoRegion,
		ContentMD:   r.ContentMD,
		Status:      r.Status,
		CreatedAt:   r.CreatedAt,
		UpdatedAt:   r.UpdatedAt,
	}
}

func (s *Service) resolveCommentViewerIdentity(r *http.Request, viewer *domain.User) (isAdmin bool, callerUserID string, callerGuestHash string) {
	if viewer != nil && viewer.Status == domain.UserStatusActive {
		callerUserID = viewer.ID
		isAdmin = viewer.Role == domain.RoleAdmin
	}
	if r != nil && s.passports != nil {
		if gp, ok := s.passports.ResolveOrHeal(nil, r, false); ok && gp != nil {
			callerGuestHash = gp.GuestIDHash
		}
	}
	return isAdmin, callerUserID, callerGuestHash
}

func buildCommentVisibilityWhere(tType, tID string, isAdmin bool, callerUserID, callerGuestHash string, nowMS int64) (string, []any) {
	where := `target_type = ? AND target_id = ? AND is_deleted = ?`
	args := []any{tType, tID, false}
	if !isAdmin {
		visOr := []string{`status = ?`}
		args = append(args, domain.CommentStatusApproved)
		if callerUserID != "" {
			visOr = append(visOr, `user_id = ?`)
			args = append(args, callerUserID)
		}
		if callerGuestHash != "" {
			visOr = append(visOr, `(guest_id_hash = ? AND created_at >= ?)`)
			args = append(args, callerGuestHash, nowMS-GuestEditWindow.Milliseconds())
		}
		where += ` AND (` + strings.Join(visOr, " OR ") + `)`
	}
	return where, args
}

func (s *Service) hydrateRootThreadsReplies(
	ctx context.Context,
	visWhere string,
	visArgs []any,
	rootRows []publicCommentRow,
	postAuthorID string,
	isAdmin bool,
	callerUserID string,
	callerGuestHash string,
	nowMS int64,
	viewer *domain.User,
) ([]PublicCommentView, error) {
	if len(rootRows) == 0 {
		return []PublicCommentView{}, nil
	}
	roots := make([]PublicCommentView, len(rootRows))
	rootIdxForID := make(map[string]int, len(rootRows)*2)
	frontier := make([]string, 0, len(rootRows))

	for i, rr := range rootRows {
		dc := rr.toDomainComment()
		isOwn := canModifyComment(dc, callerUserID, callerGuestHash, nowMS)
		roots[i] = s.toPublicCommentView(dc, postAuthorID, isOwn || isAdmin, viewer)
		rootIdxForID[rr.ID] = i
		frontier = append(frontier, rr.ID)
	}

	repliesByRoot := make([][]publicCommentRow, len(rootRows))
	multiDepth := false

	for depth := 0; depth < 8 && len(frontier) > 0; depth++ {
		if depth > 0 {
			multiDepth = true
		}
		var nextFrontier []string
		const batchSize = 250
		for start := 0; start < len(frontier); start += batchSize {
			end := start + batchSize
			if end > len(frontier) {
				end = len(frontier)
			}
			chunk := frontier[start:end]
			placeholders := make([]string, len(chunk))
			qArgs := make([]any, 0, len(visArgs)+len(chunk))
			qArgs = append(qArgs, visArgs...)
			for i, pid := range chunk {
				placeholders[i] = "?"
				qArgs = append(qArgs, pid)
			}
			replySQL := `SELECT ` + publicCommentSelectCols + ` FROM comments WHERE ` + visWhere +
				` AND parent_id IN (` + strings.Join(placeholders, ",") + `) ORDER BY created_at ASC, id ASC`
			var repRows []publicCommentRow
			err := sqlx.SelectInto(ctx, s.store, &repRows, replySQL, qArgs...)
			if err != nil {
				return nil, err
			}
			for _, rep := range repRows {
				rIdx, ok := rootIdxForID[strings.TrimSpace(rep.ParentID)]
				if !ok {
					continue
				}
				if _, seen := rootIdxForID[rep.ID]; seen {
					continue
				}
				rootIdxForID[rep.ID] = rIdx
				repliesByRoot[rIdx] = append(repliesByRoot[rIdx], rep)
				nextFrontier = append(nextFrontier, rep.ID)
			}
		}
		frontier = nextFrontier
	}

	for rIdx, repList := range repliesByRoot {
		if len(repList) == 0 {
			continue
		}
		if multiDepth && len(repList) > 1 {
			sort.Slice(repList, func(i, j int) bool {
				if repList[i].CreatedAt != repList[j].CreatedAt {
					return repList[i].CreatedAt < repList[j].CreatedAt
				}
				return repList[i].ID < repList[j].ID
			})
		}
		views := make([]PublicCommentView, 0, len(repList))
		for _, rep := range repList {
			dc := rep.toDomainComment()
			isOwn := canModifyComment(dc, callerUserID, callerGuestHash, nowMS)
			views = append(views, s.toPublicCommentView(dc, postAuthorID, isOwn || isAdmin, viewer))
		}
		roots[rIdx].Replies = views
	}
	return roots, nil
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
	tType := strings.TrimSpace(targetType)
	tID := strings.TrimSpace(targetID)
	postAuthorID, err := s.authorizeCommentTarget(ctx, r, viewer, tType, tID)
	if err != nil {
		return nil, err
	}
	isAdmin, callerUserID, callerGuestHash := s.resolveCommentViewerIdentity(r, viewer)
	nowMS := time.Now().UnixMilli()
	visWhere, visArgs := buildCommentVisibilityWhere(tType, tID, isAdmin, callerUserID, callerGuestHash, nowMS)

	rootSQL := `SELECT ` + publicCommentSelectCols + ` FROM comments WHERE ` + visWhere + ` AND parent_id = '' ORDER BY created_at DESC, id DESC`
	var rootRows []publicCommentRow
	err = sqlx.SelectInto(ctx, s.store, &rootRows, rootSQL, visArgs...)
	if err != nil {
		return nil, err
	}
	return s.hydrateRootThreadsReplies(ctx, visWhere, visArgs, rootRows, postAuthorID, isAdmin, callerUserID, callerGuestHash, nowMS, viewer)
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
	tType := strings.TrimSpace(targetType)
	tID := strings.TrimSpace(targetID)
	postAuthorID, err := s.authorizeCommentTarget(ctx, r, viewer, tType, tID)
	if err != nil {
		return PaginatedCommentsResult{}, err
	}
	if perPage <= 0 {
		perPage = 20
	}
	if perPage > 100 {
		perPage = 100
	}

	isAdmin, callerUserID, callerGuestHash := s.resolveCommentViewerIdentity(r, viewer)
	nowMS := time.Now().UnixMilli()
	visWhere, visArgs := buildCommentVisibilityWhere(tType, tID, isAdmin, callerUserID, callerGuestHash, nowMS)

	var totalComments, totalRoots int
	if err := s.store.QueryRowContext(
		ctx,
		`SELECT COUNT(*), COALESCE(SUM(CASE WHEN parent_id = '' THEN 1 ELSE 0 END), 0) FROM comments WHERE `+visWhere,
		visArgs...,
	).Scan(&totalComments, &totalRoots); err != nil {
		return PaginatedCommentsResult{}, err
	}

	if totalComments == 0 {
		return PaginatedCommentsResult{
			Comments:      []PublicCommentView{},
			Page:          1,
			PerPage:       perPage,
			TotalRoots:    0,
			TotalComments: 0,
			TotalPages:    1,
			HasMore:       false,
		}, nil
	}

	rootPredicate := ` AND parent_id = ''`
	if totalRoots == 0 {
		// Rare fallback when all root ancestors were soft-deleted leaving orphaned replies.
		totalRoots = totalComments
		rootPredicate = ``
	}

	totalPages := (totalRoots + perPage - 1) / perPage
	if totalPages < 1 {
		totalPages = 1
	}

	if focusID := strings.TrimSpace(focusCommentID); focusID != "" {
		type ancestorRow struct {
			_ [0]func()

			ID        string `db:"id"`
			ParentID  string `db:"parent_id"`
			CreatedAt int64  `db:"created_at"`
		}
		curID := focusID
		var rootID string
		var rootCreatedAt int64
		for hop := 0; hop < 8 && curID != ""; hop++ {
			aArgs := make([]any, 0, 1+len(visArgs))
			aArgs = append(aArgs, curID)
			aArgs = append(aArgs, visArgs...)
			var anc ancestorRow
			aErr := sqlx.GetInto(ctx, s.store, &anc, `SELECT id, parent_id, created_at FROM comments WHERE id = ? AND `+visWhere,
				aArgs...)

			if aErr != nil {
				break
			}
			if rootPredicate == `` || strings.TrimSpace(anc.ParentID) == "" {
				rootID = anc.ID
				rootCreatedAt = anc.CreatedAt
				break
			}
			curID = strings.TrimSpace(anc.ParentID)
		}
		if rootID != "" {
			var rankBefore int
			rankArgs := make([]any, 0, len(visArgs)+3)
			rankArgs = append(rankArgs, visArgs...)
			rankArgs = append(rankArgs, rootCreatedAt, rootCreatedAt, rootID)
			rankSQL := `SELECT COUNT(*) FROM comments WHERE ` + visWhere + rootPredicate +
				` AND (created_at > ? OR (created_at = ? AND id > ?))`
			if rErr := s.store.QueryRowContext(ctx, rankSQL, rankArgs...).Scan(&rankBefore); rErr == nil {
				page = (rankBefore / perPage) + 1
			}
		}
	}

	if page < 1 {
		page = 1
	}
	if page > totalPages {
		page = totalPages
	}

	offset := (page - 1) * perPage
	rootBaseSQL := `SELECT ` + publicCommentSelectCols + ` FROM comments WHERE ` + visWhere + rootPredicate + ` ORDER BY created_at DESC, id DESC`
	rootPageSQL, pageArgs := s.store.Dialect().PaginateSQL(rootBaseSQL, perPage, offset)
	rootArgs := make([]any, 0, len(visArgs)+len(pageArgs))
	rootArgs = append(rootArgs, visArgs...)
	rootArgs = append(rootArgs, pageArgs...)
	var rootRows []publicCommentRow
	err = sqlx.SelectInto(ctx, s.store, &rootRows, rootPageSQL, rootArgs...)
	if err != nil {
		return PaginatedCommentsResult{}, err
	}

	var slice []PublicCommentView
	if rootPredicate == `` {
		slice = make([]PublicCommentView, 0, len(rootRows))
		for _, rr := range rootRows {
			dc := rr.toDomainComment()
			isOwn := canModifyComment(dc, callerUserID, callerGuestHash, nowMS)
			slice = append(slice, s.toPublicCommentView(dc, postAuthorID, isOwn || isAdmin, viewer))
		}
	} else {
		slice, err = s.hydrateRootThreadsReplies(ctx, visWhere, visArgs, rootRows, postAuthorID, isAdmin, callerUserID, callerGuestHash, nowMS, viewer)
		if err != nil {
			return PaginatedCommentsResult{}, err
		}
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
		_ [0]func()

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
	var rows []joinedRecent
	err := sqlx.SelectInto(ctx, s.store, &rows, q,
		append([]any{domain.CommentStatusApproved, false, domain.ArticleStatusPublished, true}, pageArgs...)...)

	if err != nil {
		return nil, err
	}
	out := make([]RecentCommentItem, 0, len(rows))
	for _, r := range rows {
		plain := render.ExtractPlainTextFromHTML(s.renderer.RenderComment(r.ContentMD))
		targetURL := visibility.ArticlePath(r.TargetSlug) + "#comment-" + r.ID
		if r.TargetType == domain.TargetTypePage {
			targetURL = "/" + r.TargetSlug + "#comment-" + r.ID
		}
		out = append(out, RecentCommentItem{
			ID:          r.ID,
			AuthorName:  r.AuthorName,
			Excerpt:     truncateForSummary(plain, 64),
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
		var pg domain.Page
		err := sqlx.GetInto(ctx, s.store, &pg, `SELECT * FROM pages WHERE id = ?`, tID)
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
		var parent domain.Comment
		pErr := sqlx.GetInto(ctx, s.store, &parent, `SELECT id, parent_id, author_name, target_type, target_id, is_deleted FROM comments WHERE id = ?`,
			parentID)

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
			if s.matchesBuiltinCommentFilter(snap, contentMD, authorName) {
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
	var existing domain.Comment
	err := sqlx.GetInto(ctx, s.store, &existing, `SELECT * FROM comments WHERE id = ? AND is_deleted = ?`, strings.TrimSpace(commentID), false)
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
		switch snap.Reading.CommentModerationMode {
		case domain.CommentModerationAll:
			nextStatus = domain.CommentStatusPending
		case domain.CommentModerationSmart:
			if s.matchesBuiltinCommentFilter(snap, trimmed, existing.AuthorName) {
				nextStatus = domain.CommentStatusPending
				keywordTriggered = true
			}
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
	var existing domain.Comment
	err := sqlx.GetInto(ctx, s.store, &existing, `SELECT * FROM comments WHERE id = ? AND is_deleted = ?`, strings.TrimSpace(commentID), false)
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
		_ [0]func()

		Kind string `db:"kind"`
		Cnt  int    `db:"cnt"`
	}
	var rows []row
	err := sqlx.SelectInto(ctx, s.store, &rows, `SELECT kind, COUNT(*) AS cnt FROM reactions WHERE target_type = ? AND target_id = ? GROUP BY kind`,
		targetType, targetID)

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

const maxCommentHTMLGenEntries = 2048

func (s *Service) putCommentHTMLCacheLocked(key string, html string) {
	if s.commentHTMLCurr == nil {
		s.commentHTMLCurr = make(map[string]string, 256)
	}
	if _, exists := s.commentHTMLCurr[key]; !exists && len(s.commentHTMLCurr) >= maxCommentHTMLGenEntries {
		s.commentHTMLPrev = s.commentHTMLCurr
		s.commentHTMLCurr = make(map[string]string, 256)
	}
	s.commentHTMLCurr[key] = html
}

func (s *Service) renderCommentMarkdownHTML(c domain.Comment) string {
	if c.ID != "" {
		cacheKey := c.ID + ":" + strconv.FormatInt(c.UpdatedAt, 10)
		s.commentHTMLMu.RLock()
		if cached, ok := s.commentHTMLCurr[cacheKey]; ok {
			s.commentHTMLMu.RUnlock()
			return cached
		}
		_, inPrev := s.commentHTMLPrev[cacheKey]
		s.commentHTMLMu.RUnlock()

		if inPrev {
			s.commentHTMLMu.Lock()
			if cached, ok := s.commentHTMLCurr[cacheKey]; ok {
				s.commentHTMLMu.Unlock()
				return cached
			}
			if cached, ok := s.commentHTMLPrev[cacheKey]; ok {
				s.putCommentHTMLCacheLocked(cacheKey, cached)
				s.commentHTMLMu.Unlock()
				return cached
			}
			s.commentHTMLMu.Unlock()
		}

		html := s.renderer.RenderComment(c.ContentMD)
		s.commentHTMLMu.Lock()
		s.putCommentHTMLCacheLocked(cacheKey, html)
		s.commentHTMLMu.Unlock()
		return html
	}
	return s.renderer.RenderComment(c.ContentMD)
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

func (s *Service) matchesBuiltinCommentFilter(snap domain.SettingsSnapshot, content string, author string) bool {
	if !snap.Reading.BuiltinSpamFilter {
		return false
	}
	// Built-in heuristic: > 4 external http(s) links in a single comment triggers moderation.
	if countExternalLinksLower(content) > 4 {
		return true
	}
	m := s.getOrBuildKeywordMatcher(snap)
	if m == nil || m.WordCount() == 0 {
		return false
	}
	return m.Contains(content) || (strings.TrimSpace(author) != "" && m.Contains(author))
}

func (s *Service) getOrBuildKeywordMatcher(snap domain.SettingsSnapshot) *wordfilter.Matcher {
	var rev uint64
	if s.settings != nil {
		rev = s.settings.Revision()
	}
	if rev > 0 {
		if cached := s.kwFilter.Load(); cached != nil && cached.rev == rev {
			return cached.matcher
		}
	}
	var m *wordfilter.Matcher
	if len(snap.Reading.BlockedKeywords) > 0 {
		m = wordfilter.Compile(wordfilter.Options{
			Deny:             snap.Reading.BlockedKeywords,
			Allow:            snap.Reading.AllowedKeywords,
			IgnoreSeparators: true,
		})
	}
	if rev > 0 {
		s.kwFilter.Store(&cachedKeywordFilter{
			rev:     rev,
			matcher: m,
		})
	}
	return m
}

func countExternalLinksLower(s string) int {
	if len(s) < len("http://") {
		return 0
	}
	low := strings.ToLower(s)
	return strings.Count(low, "http://") + strings.Count(low, "https://")
}
