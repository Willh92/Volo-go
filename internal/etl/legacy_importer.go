package etl

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Willh92/Volo-go/internal/config"
	"github.com/Willh92/Volo-go/internal/db/sqlx"
	"github.com/Willh92/Volo-go/internal/domain"
	"github.com/Willh92/Volo-go/internal/i18n"
	"github.com/Willh92/Volo-go/internal/repo"
	"github.com/Willh92/Volo-go/pkg/id"
)

const (
	maxLegacyJSONBytes   = 512 * 1024 * 1024 // 512 MiB stream safety limit
	chunkOptionCategory  = "voloDocumentChunk"
	chunkManifestPrefix  = `{"storage":"chunks"`
	defaultMappedAdminID = "1000000000001"
)

var (
	soloJSONNameRe = regexp.MustCompile(`(?i)^(data\.json|solo-[a-z0-9_.-]*\.json)$`)
	hexGenRe       = regexp.MustCompile(`^[a-f0-9]{32}$`)
	nonSlugCharsRe = regexp.MustCompile(`[^a-z0-9\p{Han}-]+`)
	multiDashRe    = regexp.MustCompile(`-+`)
	htmlTagStripRe = regexp.MustCompile(`<[^>]+>`)
)

// ImportOptions configures streaming legacy bolo-solo archive ETL, URL/domain rewriting, and setup overrides.
type ImportOptions struct {
	DryRun                 bool
	LegacyMediaDir         string // Optional path to legacy ./image directory to register/copy
	TargetUploadDir        string
	AdminPath              string // Default "/console"
	OldURLPrefixes         []string
	NewSiteURL             string
	SiteTitle              string
	SiteSubtitle           string
	DefaultLocale          string
	TimeZone               string
	Accent                 string
	ForceLocalStorage      bool
	AdminUsername          string
	AdminEmail             string
	AdminDisplayName       string
	AdminPasswordHash      string
	PreserveExistingAdmins bool
}

// ImportReport summarizes everything migrated, normalized, rewritten, and any multi-category conflicts or warnings.
type ImportReport struct {
	SourceVersion     string   `json:"source_version"`
	DryRun            bool     `json:"dry_run"`
	TargetDialect     string   `json:"target_dialect,omitempty"`
	Users             int      `json:"users"`
	Categories        int      `json:"categories"`
	Tags              int      `json:"tags"`
	Articles          int      `json:"articles"`
	Revisions         int      `json:"revisions"`
	NavItems          int      `json:"nav_items"`
	Comments          int      `json:"comments"`
	Links             int      `json:"links"`
	Follows           int      `json:"follows"`
	Series            int      `json:"series"`
	Books             int      `json:"books"`
	BookChapters      int      `json:"book_chapters"`
	MediaFiles        int      `json:"media_files"`
	URLsRewritten     int      `json:"urls_rewritten"`
	AnalyticsDays     int      `json:"analytics_days"`
	Webhooks          int      `json:"webhooks"`
	RedirectsCreated  int      `json:"redirects_created"`
	CategoryConflicts []string `json:"category_conflicts,omitempty"`
	Warnings          []string `json:"warnings,omitempty"`
}

type legacyOptionRow struct {
	OID      string
	Category string
	Value    string
}

type legacyCategoryMeta struct {
	ID    string
	Order int
	Name  string
}

// ImportLegacyArchive streams a legacy bolo-solo ZIP (containing data.json or solo-*.json) or a raw .json file,
// decomposing chunked option documents, deriving direct article categories, preserving .html permalinks via 301s,
// rewriting legacy origins/media paths, and writing normalized first-class records inside a rollback-safe transaction.
func ImportLegacyArchive(
	ctx context.Context,
	store *sqlx.Store,
	settingsMgr *repo.SettingsManager,
	keyRing *config.KeyRing,
	inputPath string,
	opts ImportOptions,
) (*ImportReport, error) {
	openStream, closeContainer, err := openLegacyJSONSource(inputPath)
	if err != nil {
		return nil, err
	}
	defer closeContainer()

	report := &ImportReport{DryRun: opts.DryRun}
	if store != nil && store.Dialect() != nil {
		report.TargetDialect = store.Dialect().Name()
	}

	mediaCandidates, extractedImages, closeMediaArchive, err := collectBundledMediaCandidates(inputPath, opts.LegacyMediaDir, report)
	if err != nil {
		return nil, err
	}
	defer closeMediaArchive()

	// -------------------------------------------------------------------------
	// Pass 1: Stream metadata, relational index maps, article permalink map, and chunked option rows
	// -------------------------------------------------------------------------
	pass1State, err := runPass1(ctx, openStream, opts, report)
	if err != nil {
		return nil, err
	}

	rewriter := newURLRewriter(pass1State, opts, extractedImages)

	// Build normalized settings & chunk-derived entities in memory (small metadata only)
	decomposed, err := decomposeOptions(pass1State, settingsMgr.Snapshot(), keyRing, opts, rewriter, report)
	if err != nil {
		return nil, err
	}

	if opts.DryRun {
		report.MediaFiles = len(mediaCandidates)
		if err := countPass2Entities(ctx, openStream, pass1State, rewriter, report); err != nil {
			return nil, err
		}
		report.URLsRewritten = rewriter.count
		return report, nil
	}

	var existingAdmins []domain.User
	_ = sqlx.SelectInto(ctx, store, &existingAdmins,
		`SELECT * FROM users WHERE role = ?`, domain.RoleAdmin)
	if opts.PreserveExistingAdmins && len(existingAdmins) > 0 {
		if len(pass1State.users) == 1 && pass1State.users[0].ID == defaultMappedAdminID && pass1State.users[0].Email == "admin@local.volo" {
			pass1State.users = nil
			pass1State.defaultAdminID = existingAdmins[0].ID
		} else if pass1State.defaultAdminID == "" {
			pass1State.defaultAdminID = existingAdmins[0].ID
		}
	}

	// -------------------------------------------------------------------------
	// Pass 2: Stream articles, revisions, comments, pages->nav_items, links, follows, and bundled media
	// directly into normalized tables inside an atomic transaction!
	// -------------------------------------------------------------------------
	var createdMediaFiles []string
	err = store.WithTx(ctx, func(tx *sqlx.Tx) error {
		if err := insertPass1EntitiesTx(ctx, tx, pass1State, decomposed, existingAdmins, opts.PreserveExistingAdmins, report); err != nil {
			return err
		}
		if err := persistBundledMediaTx(ctx, tx, mediaCandidates, opts.TargetUploadDir, pass1State.defaultAdminID, report, &createdMediaFiles); err != nil {
			return err
		}
		return streamPass2IntoTx(ctx, tx, openStream, pass1State, decomposed.seriesArticles, keyRing, opts, rewriter, report)
	})
	if err != nil {
		for _, p := range createdMediaFiles {
			_ = os.Remove(p)
		}
		return nil, err
	}
	report.URLsRewritten = rewriter.count

	// Apply decomposed atomic settings domains and hot-reload SettingsManager
	if err := persistDecomposedSettings(ctx, settingsMgr, decomposed); err != nil {
		return nil, err
	}

	return report, nil
}

func openLegacyJSONSource(inputPath string) (func() (io.ReadCloser, error), func(), error) {
	f, err := os.Open(inputPath)
	if err != nil {
		return nil, nil, fmt.Errorf("etl: open %q: %w", inputPath, err)
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, nil, err
	}

	if strings.HasSuffix(strings.ToLower(inputPath), ".json") {
		_ = f.Close()
		return func() (io.ReadCloser, error) {
			rawF, oErr := os.Open(inputPath)
			if oErr != nil {
				return nil, oErr
			}
			return stripBOMReadCloser(rawF), nil
		}, func() {}, nil
	}

	zr, err := zip.NewReader(f, st.Size())
	if err != nil {
		_ = f.Close()
		return nil, nil, fmt.Errorf("etl: invalid zip archive %q: %w", inputPath, err)
	}
	if len(zr.File) > maxLegacyZipEntries {
		_ = f.Close()
		return nil, nil, fmt.Errorf("etl: zip archive entry count %d exceeds %d safety limit", len(zr.File), maxLegacyZipEntries)
	}

	var targetEntry *zip.File
	for _, zf := range zr.File {
		base := path.Base(strings.TrimPrefix(zf.Name, "/"))
		if zf.FileInfo().IsDir() || strings.HasPrefix(base, ".") {
			continue
		}
		if strings.EqualFold(base, "data.json") {
			targetEntry = zf
			break
		}
		if soloJSONNameRe.MatchString(base) && targetEntry == nil {
			targetEntry = zf
		}
	}
	if targetEntry == nil {
		_ = f.Close()
		return nil, nil, errors.New("etl: no data.json or solo-*.json found inside archive")
	}

	return func() (io.ReadCloser, error) {
		rc, oErr := targetEntry.Open()
		if oErr != nil {
			return nil, oErr
		}
		return stripBOMReadCloser(rc), nil
	}, func() { _ = f.Close() }, nil
}

type limitedBOMReadCloser struct {
	io.Reader
	closer io.Closer
}

func (l *limitedBOMReadCloser) Close() error { return l.closer.Close() }

func stripBOMReadCloser(rc io.ReadCloser) io.ReadCloser {
	br := bufio.NewReaderSize(io.LimitReader(rc, maxLegacyJSONBytes), 32*1024)
	if peek, err := br.Peek(3); err == nil && bytes.Equal(peek, []byte{0xEF, 0xBB, 0xBF}) {
		_, _ = br.Discard(3)
	}
	return &limitedBOMReadCloser{Reader: br, closer: rc}
}

type pass1Data struct {
	options             map[string]legacyOptionRow
	chunks              map[string]string
	users               []domain.User
	userIDMap           map[string]string // maps e.g. "default" -> "1000000000001"
	defaultAdminID      string
	categories          []domain.Category
	catIDMap            map[string]string
	catMeta             map[string]legacyCategoryMeta
	tags                []domain.Tag
	tagIDMap            map[string]string
	tagIDSet            map[string]bool
	tagByName           map[string]string
	catTagMap           map[string][]string // raw/mapped tag_oId -> []raw/mapped category_oId
	artTagsMap          map[string][]string // mapped article_oId -> []mapped tag_oId
	articleIDMap        map[string]string   // raw article_oId -> <=32 char article_id
	articleSlugMap      map[string]string   // mapped article_id -> slug
	permalinkMap        map[string]string   // legacy permalink -> "/posts/" + slug
	articlePrimaryCat   map[string]string   // mapped article_id -> derived primary category_id
	commentMailFallback map[string]string   // comment_oId -> email (from legacy mailUserContext)
	linkHealth          map[string]legacyLinkHealthItem
}

func (st *pass1Data) resolveArticleID(rawID string) string {
	rawID = strings.TrimSpace(rawID)
	if rawID == "" {
		return ""
	}
	if mapped, ok := st.articleIDMap[rawID]; ok {
		return mapped
	}
	return rawID
}

type legacyLinkHealthItem struct {
	OK        bool   `json:"ok"`
	Status    string `json:"status"`
	HTTPCode  int    `json:"httpCode"`
	CodeAlt   int    `json:"status_num"`
	LatencyMS int    `json:"latencyMs"`
	CheckedAt int64  `json:"checkedAt"`
	Error     string `json:"error"`
}

func runPass1(ctx context.Context, openStream func() (io.ReadCloser, error), opts ImportOptions, report *ImportReport) (*pass1Data, error) {
	rc, err := openStream()
	if err != nil {
		return nil, err
	}
	defer rc.Close()

	st := &pass1Data{
		options:             make(map[string]legacyOptionRow),
		chunks:              make(map[string]string),
		userIDMap:           make(map[string]string),
		catIDMap:            make(map[string]string),
		catMeta:             make(map[string]legacyCategoryMeta),
		tagIDMap:            make(map[string]string),
		tagIDSet:            make(map[string]bool),
		tagByName:           make(map[string]string),
		catTagMap:           make(map[string][]string),
		artTagsMap:          make(map[string][]string),
		articleIDMap:        make(map[string]string),
		articleSlugMap:      make(map[string]string),
		permalinkMap:        make(map[string]string),
		articlePrimaryCat:   make(map[string]string),
		commentMailFallback: make(map[string]string),
		linkHealth:          make(map[string]legacyLinkHealthItem),
	}

	dec := json.NewDecoder(rc)
	dec.UseNumber()

	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return nil, errors.New("etl: expected root JSON object in legacy backup")
	}

	usedCatSlugs := make(map[string]bool)
	usedCatIDs := make(map[string]bool)
	usedTagSlugs := make(map[string]bool)
	usedUserIDs := make(map[string]bool)
	usedUsernames := make(map[string]bool)
	usedEmails := make(map[string]bool)
	usedArtIDs := make(map[string]bool)
	usedArtSlugs := make(map[string]bool)

	type rawPair struct{ a, b string }
	var rawCatTags []rawPair
	var rawArtTags []rawPair

	for dec.More() {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		keyTok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		rootKey, _ := keyTok.(string)

		switch rootKey {
		case "options":
			if err := streamJSONArray(dec, func(raw map[string]any) error {
				oid := strVal(raw["oId"])
				cat := strVal(raw["optionCategory"])
				val := strVal(raw["optionValue"])
				if oid == "" {
					return nil
				}
				if cat == chunkOptionCategory {
					st.chunks[oid] = val
					return nil
				}
				st.options[oid] = legacyOptionRow{OID: oid, Category: cat, Value: val}
				return nil
			}); err != nil {
				return nil, err
			}

		case "users":
			if err := streamJSONArray(dec, func(raw map[string]any) error {
				origID := strings.TrimSpace(strVal(raw["oId"]))
				if origID == "" {
					return nil
				}
				if _, alreadySeen := st.userIDMap[origID]; alreadySeen {
					return nil
				}
				role := mapLegacyRole(strVal(raw["userRole"]))
				mappedID := origID
				if origID == "default" || len(origID) > 32 || usedUserIDs[origID] {
					if !usedUserIDs[defaultMappedAdminID] && (origID == "default" || role == domain.RoleAdmin) {
						mappedID = defaultMappedAdminID
					} else {
						mappedID = id.Next()
						for usedUserIDs[mappedID] {
							mappedID = id.Next()
						}
					}
				}
				usedUserIDs[mappedID] = true
				st.userIDMap[origID] = mappedID

				uname := truncRunes(strings.TrimSpace(strVal(raw["userName"])), 56)
				if uname == "" {
					uname = "user_" + mappedID
				}
				uname = dedupString(uname, usedUsernames)

				email := truncRunes(strings.ToLower(strings.TrimSpace(strVal(raw["userEmail"]))), 180)
				if email == "" {
					email = strings.ToLower(uname) + "@local.volo"
				}
				email = dedupEmail(email, usedEmails)

				if role == domain.RoleAdmin && st.defaultAdminID == "" {
					st.defaultAdminID = mappedID
				}

				legacyHash := extractLegacyPasswordHash(raw, role)
				status := domain.UserStatusActive
				if intVal(raw["userStatus"]) != 0 {
					status = domain.UserStatusDisabled
				}
				createdAt := int64Val(raw["userCreated"])
				if createdAt <= 0 {
					createdAt = parseMillisFallback(mappedID)
				}
				verifiedAt := int64(0)
				if boolVal(raw["userEmailVerified"]) {
					verifiedAt = createdAt
				}
				avatar := sanitizeLegacyAvatar(strVal(raw["userAvatar"]))

				st.users = append(st.users, domain.User{
					ID:              mappedID,
					Username:        uname,
					Email:           email,
					DisplayName:     uname,
					AvatarURL:       avatar,
					Bio:             "",
					WebsiteURL:      strVal(raw["userURL"]),
					Role:            role,
					PasswordHash:    "",
					LegacyHash:      legacyHash,
					Status:          status,
					EmailVerifiedAt: verifiedAt,
					PreferredLocale: "",
					CreatedAt:       createdAt,
					UpdatedAt:       createdAt,
				})
				return nil
			}); err != nil {
				return nil, err
			}

		case "categories":
			if err := streamJSONArray(dec, func(raw map[string]any) error {
				origCID := strings.TrimSpace(strVal(raw["oId"]))
				title := strings.TrimSpace(strVal(raw["categoryTitle"]))
				if origCID == "" || title == "" {
					return nil
				}
				if _, seen := st.catIDMap[origCID]; seen {
					return nil
				}
				cid := origCID
				if len(cid) > 32 || usedCatIDs[cid] {
					cid = id.Next()
				}
				usedCatIDs[cid] = true
				st.catIDMap[origCID] = cid
				uri := strings.Trim(strings.TrimSpace(strVal(raw["categoryURI"])), "/")
				slug := dedupSlug(slugify(firstNonEmpty(uri, title, cid)), usedCatSlugs)
				order := intVal(raw["categoryOrder"])
				now := parseMillisFallback(cid)

				st.categories = append(st.categories, domain.Category{
					ID:          cid,
					Name:        title,
					Slug:        slug,
					Description: strVal(raw["categoryDescription"]),
					Icon:        "",
					SortOrder:   order,
					CreatedAt:   now,
					UpdatedAt:   now,
				})
				st.catMeta[cid] = legacyCategoryMeta{ID: cid, Order: order, Name: title}
				return nil
			}); err != nil {
				return nil, err
			}

		case "tags":
			if err := streamJSONArray(dec, func(raw map[string]any) error {
				origTID := strings.TrimSpace(strVal(raw["oId"]))
				title := strings.TrimSpace(strVal(raw["tagTitle"]))
				if origTID == "" || title == "" {
					return nil
				}
				if _, seen := st.tagIDMap[origTID]; seen {
					return nil
				}
				if existingTID, dup := st.tagByName[strings.ToLower(title)]; dup {
					st.tagIDMap[origTID] = existingTID
					return nil
				}
				tid := origTID
				if len(tid) > 32 || st.tagIDSet[tid] {
					tid = id.Next()
				}
				slug := dedupSlug(slugify(firstNonEmpty(title, tid)), usedTagSlugs)
				st.tags = append(st.tags, domain.Tag{
					ID:        tid,
					Name:      title,
					Slug:      slug,
					CreatedAt: parseMillisFallback(tid),
				})
				st.tagIDMap[origTID] = tid
				st.tagIDSet[tid] = true
				st.tagByName[strings.ToLower(title)] = tid
				return nil
			}); err != nil {
				return nil, err
			}

		case "category_tag", "category_tags":
			if err := streamJSONArray(dec, func(raw map[string]any) error {
				cid := strings.TrimSpace(strVal(raw["category_oId"]))
				tid := strings.TrimSpace(strVal(raw["tag_oId"]))
				if cid != "" && tid != "" {
					rawCatTags = append(rawCatTags, rawPair{a: cid, b: tid})
				}
				return nil
			}); err != nil {
				return nil, err
			}

		case "tag_articles", "tag_article":
			if err := streamJSONArray(dec, func(raw map[string]any) error {
				aid := strings.TrimSpace(strVal(raw["article_oId"]))
				tid := strings.TrimSpace(strVal(raw["tag_oId"]))
				if aid != "" && tid != "" {
					rawArtTags = append(rawArtTags, rawPair{a: aid, b: tid})
				}
				return nil
			}); err != nil {
				return nil, err
			}

		case "articles":
			if err := streamArticleHeaders(dec, func(rawOID, rawPermalink, rawTitle string) error {
				origAID := strings.TrimSpace(rawOID)
				if origAID == "" {
					return nil
				}
				if _, seen := st.articleIDMap[origAID]; seen {
					return nil
				}
				mappedAID := origAID
				if len(mappedAID) > 32 || usedArtIDs[mappedAID] {
					mappedAID = id.Next()
				}
				usedArtIDs[mappedAID] = true
				st.articleIDMap[origAID] = mappedAID

				origPermalink := normalizeLegacyPath(rawPermalink)
				title := firstNonEmpty(strings.TrimSpace(rawTitle), "Untitled "+mappedAID)
				slugBase := deriveSlugFromPermalinkOrTitle(origPermalink, title, mappedAID)
				slug := truncRunes(dedupSlug(slugBase, usedArtSlugs), 190)
				st.articleSlugMap[mappedAID] = slug
				modernPath := "/posts/" + slug
				if origPermalink != "" && origPermalink != modernPath {
					if _, exists := st.permalinkMap[origPermalink]; !exists {
						st.permalinkMap[origPermalink] = modernPath
					}
				}
				return nil
			}); err != nil {
				return nil, err
			}

		default:
			if err := skipJSONValue(dec); err != nil {
				return nil, err
			}
		}
	}

	// Resolve category_tag and tag_articles through normalized <=32 char IDs
	for _, p := range rawCatTags {
		cid := st.catIDMap[p.a]
		if cid == "" {
			cid = p.a
		}
		tid := st.tagIDMap[p.b]
		if tid == "" {
			tid = p.b
		}
		st.catTagMap[tid] = append(st.catTagMap[tid], cid)
	}
	for _, p := range rawArtTags {
		aid := st.resolveArticleID(p.a)
		tid := st.tagIDMap[p.b]
		if tid == "" {
			tid = p.b
		}
		st.artTagsMap[aid] = append(st.artTagsMap[aid], tid)
	}

	// Ensure at least one active administrator exists
	if len(st.users) == 0 {
		now := time.Now().UnixMilli()
		st.defaultAdminID = defaultMappedAdminID
		st.users = append(st.users, domain.User{
			ID:          defaultMappedAdminID,
			Username:    "admin",
			Email:       "admin@local.volo",
			DisplayName: "Admin",
			Role:        domain.RoleAdmin,
			Status:      domain.UserStatusActive,
			CreatedAt:   now,
			UpdatedAt:   now,
		})
	} else if st.defaultAdminID == "" {
		st.users[0].Role = domain.RoleAdmin
		st.defaultAdminID = st.users[0].ID
	}

	// Apply optional administrator account overrides from Setup Wizard
	adminIdx := -1
	for i := range st.users {
		if st.users[i].ID == st.defaultAdminID || (adminIdx < 0 && st.users[i].Role == domain.RoleAdmin) {
			adminIdx = i
			if st.users[i].ID == st.defaultAdminID {
				break
			}
		}
	}
	if adminIdx >= 0 {
		oldUname := st.users[adminIdx].Username
		if newUname := truncRunes(strings.TrimSpace(opts.AdminUsername), 64); newUname != "" {
			for i := range st.users {
				if i != adminIdx && strings.EqualFold(st.users[i].Username, newUname) {
					st.users[i].Username = truncRunes(st.users[i].Username+"_legacy", 64)
				}
			}
			st.users[adminIdx].Username = newUname
			if strings.TrimSpace(opts.AdminDisplayName) == "" && st.users[adminIdx].DisplayName == oldUname {
				st.users[adminIdx].DisplayName = newUname
			}
		}
		if newEmail := truncRunes(strings.ToLower(strings.TrimSpace(opts.AdminEmail)), 190); newEmail != "" {
			for i := range st.users {
				if i != adminIdx && strings.EqualFold(st.users[i].Email, newEmail) {
					st.users[i].Email = truncRunes("legacy+"+st.users[i].Email, 190)
				}
			}
			st.users[adminIdx].Email = newEmail
		}
		if newDisp := truncRunes(strings.TrimSpace(opts.AdminDisplayName), 128); newDisp != "" {
			st.users[adminIdx].DisplayName = newDisp
		}
		if strings.TrimSpace(opts.AdminPasswordHash) != "" {
			st.users[adminIdx].PasswordHash = strings.TrimSpace(opts.AdminPasswordHash)
			st.users[adminIdx].LegacyHash = ""
			st.users[adminIdx].Status = domain.UserStatusActive
		}
	}

	// Derive direct primary category for every article from tag_articles + category_tag
	for aid, tids := range st.artTagsMap {
		candidateSet := make(map[string]legacyCategoryMeta)
		for _, tid := range tids {
			for _, cid := range st.catTagMap[tid] {
				if meta, ok := st.catMeta[cid]; ok {
					candidateSet[cid] = meta
				}
			}
		}
		if len(candidateSet) == 0 {
			continue
		}
		candidates := make([]legacyCategoryMeta, 0, len(candidateSet))
		for _, m := range candidateSet {
			candidates = append(candidates, m)
		}
		sort.Slice(candidates, func(i, j int) bool {
			if candidates[i].Order != candidates[j].Order {
				return candidates[i].Order < candidates[j].Order
			}
			return candidates[i].ID < candidates[j].ID
		})
		st.articlePrimaryCat[aid] = candidates[0].ID
		if len(candidates) > 1 {
			names := make([]string, len(candidates))
			for i, c := range candidates {
				names[i] = c.Name
			}
			report.CategoryConflicts = append(report.CategoryConflicts,
				fmt.Sprintf("article %s matched [%s] -> bound primary category %q", aid, strings.Join(names, ", "), candidates[0].Name))
		}
	}
	sort.Strings(report.CategoryConflicts)

	// Parse legacy mailUserContext ("<commentOId>:<userName>:<email>;...")
	if muc, ok := st.options["mailUserContext"]; ok && muc.Value != "" {
		for _, entry := range strings.Split(muc.Value, ";") {
			parts := strings.Split(strings.TrimSpace(entry), ":")
			if len(parts) >= 3 && parts[0] != "" && strings.Contains(parts[2], "@") {
				st.commentMailFallback[parts[0]] = strings.TrimSpace(parts[2])
			}
		}
	}

	return st, nil
}

// reassembleOptionDoc reconstructs a potentially chunked option JSON payload strictly
// in chunk index order 0..count-1 (immune to lexicographical ".10" before ".2" ordering!),
// checking both st.chunks and st.options and recording a warning instead of aborting on missing chunks.
func (st *pass1Data) reassembleOptionDoc(rootID string, report *ImportReport) (string, bool) {
	root, ok := st.options[rootID]
	if !ok {
		return "", false
	}
	val := strings.TrimSpace(root.Value)
	if !strings.HasPrefix(val, "{") || !strings.Contains(val, `"storage"`) || !strings.Contains(val, `"chunks"`) {
		return root.Value, true
	}
	var manifest struct {
		Storage    string `json:"storage"`
		Generation string `json:"generation"`
		Count      int    `json:"count"`
	}
	if err := json.Unmarshal([]byte(val), &manifest); err != nil {
		if report != nil {
			report.Warnings = append(report.Warnings, fmt.Sprintf("skipped malformed chunk manifest for %q", rootID))
		}
		return "", false
	}
	if manifest.Storage != "chunks" || !hexGenRe.MatchString(manifest.Generation) || manifest.Count < 1 || manifest.Count > 4096 {
		if report != nil {
			report.Warnings = append(report.Warnings, fmt.Sprintf("skipped invalid chunk manifest for %q", rootID))
		}
		return "", false
	}
	var sb strings.Builder
	for i := 0; i < manifest.Count; i++ {
		chunkKey := fmt.Sprintf("%s.%s.%d", rootID, manifest.Generation, i)
		part, exists := st.chunks[chunkKey]
		if !exists {
			if optRow, optExists := st.options[chunkKey]; optExists {
				part = optRow.Value
				exists = true
			}
		}
		if !exists {
			if report != nil {
				report.Warnings = append(report.Warnings, fmt.Sprintf("skipped incomplete chunked option %q (missing chunk %q)", rootID, chunkKey))
			}
			return "", false
		}
		sb.WriteString(part)
	}
	return sb.String(), true
}

type decomposedState struct {
	settings       domain.SettingsSnapshot
	storageSecret  string
	smtpSecret     string
	backupSecrets  domain.BackupSecrets
	series         []domain.Series
	seriesArticles []domain.SeriesArticle
	books          []domain.Book
	bookChapters   []domain.BookChapter
	analyticsDaily []domain.AnalyticsDaily
	analyticsTop   []domain.AnalyticsTopItem
	webhooks       []domain.Webhook
	ipBans         []domain.SecurityIPBan
	geoRules       []domain.SecurityGeoRule
}

func decomposeOptions(
	st *pass1Data,
	base domain.SettingsSnapshot,
	kr *config.KeyRing,
	opts ImportOptions,
	rewriter *urlRewriter,
	report *ImportReport,
) (*decomposedState, error) {
	out := &decomposedState{settings: base}
	out.settings.Site.Initialized = true

	getOpt := func(k string) string {
		if r, ok := st.options[k]; ok {
			return strings.TrimSpace(r.Value)
		}
		return ""
	}
	if v := getOpt("version"); v != "" {
		report.SourceVersion = v
	}

	// 1. Site & Appearance settings
	if v := getOpt("blogTitle"); v != "" {
		out.settings.Site.Title = v
	}
	if v := getOpt("blogSubtitle"); v != "" {
		out.settings.Site.Subtitle = v
	}
	if v := getOpt("localeString"); v != "" {
		out.settings.Site.DefaultLocale = i18n.NormalizeLocale(v)
	}
	if v := getOpt("timeZoneId"); v != "" {
		out.settings.Site.TimeZone = v
	}
	if v := getOpt("noticeBoard"); v != "" {
		out.settings.Site.NoticeBoard = sanitizeLegacySiteInjectionHTML(rewriter.rewriteText(v))
	}
	if v := getOpt("footerContent"); v != "" {
		out.settings.Site.FooterText = sanitizeLegacySiteInjectionHTML(rewriter.rewriteText(v))
	}
	if v := firstNonEmpty(getOpt("htmlHead"), getOpt("head"), getOpt("blog_custom_head")); v != "" {
		out.settings.Site.CustomHeadMeta = sanitizeLegacySiteInjectionHTML(rewriter.rewriteText(v))
	}
	if v := getOpt("faviconURL"); v != "" && !isLegacyExternalAsset(v) {
		out.settings.Site.FaviconURL = rewriter.rewriteURL(v)
	}
	if v := getOpt("showLoginEntry"); v != "" {
		out.settings.Site.ShowAdminLoginEntry = parseLegacyBool(v, false)
	}
	if cv := getOpt("customVars"); cv != "" {
		out.settings.Site.CustomVars = parsePipeKeyValues(cv)
	}
	if signsRaw := getOpt("signs"); signsRaw != "" {
		if tpl := convertLegacySignsToSafeTemplate(signsRaw); tpl != "" {
			out.settings.Site.LicenseTemplate = tpl
		}
	}
	if usiteRaw, ok := st.reassembleOptionDoc("usite", report); ok && usiteRaw != "" {
		out.settings.Site.SocialLinks = convertDynamicSocialLinks(usiteRaw)
	}

	// Apply explicit setup/import overrides over legacy options
	if v := strings.TrimSpace(opts.SiteTitle); v != "" {
		out.settings.Site.Title = cleanUTF8(v)
	}
	if strings.TrimSpace(out.settings.Site.Title) == "" {
		out.settings.Site.Title = "Volo Studio"
	}
	if v := strings.TrimSpace(opts.SiteSubtitle); v != "" {
		out.settings.Site.Subtitle = cleanUTF8(v)
	}
	if v := strings.TrimSpace(opts.DefaultLocale); v != "" {
		if norm := i18n.NormalizeLocale(v); norm != "" {
			out.settings.Site.DefaultLocale = norm
		}
	}
	if v := strings.TrimSpace(opts.TimeZone); v != "" {
		out.settings.Site.TimeZone = cleanUTF8(v)
	}

	// Pre-sanitize FaviconURL, LogoURL, and SocialLinks so PatchDomain("site") cannot fail post-commit.
	if rawFav := strings.TrimSpace(out.settings.Site.FaviconURL); rawFav != "" {
		if cleanFav, ok := sanitizeLegacySiteURL(rawFav, false); ok {
			out.settings.Site.FaviconURL = cleanFav
		} else {
			out.settings.Site.FaviconURL = ""
			if report != nil {
				report.Warnings = append(report.Warnings, fmt.Sprintf("cleared invalid legacy faviconURL %q", rawFav))
			}
		}
	}
	if rawLogo := strings.TrimSpace(out.settings.Site.LogoURL); rawLogo != "" {
		if cleanLogo, ok := sanitizeLegacySiteURL(rawLogo, false); ok {
			out.settings.Site.LogoURL = cleanLogo
		} else {
			out.settings.Site.LogoURL = ""
			if report != nil {
				report.Warnings = append(report.Warnings, fmt.Sprintf("cleared invalid legacy logoURL %q", rawLogo))
			}
		}
	}
	if len(out.settings.Site.SocialLinks) > 0 {
		validLinks := make([]domain.SocialLink, 0, len(out.settings.Site.SocialLinks))
		for _, sl := range out.settings.Site.SocialLinks {
			rewritten := rewriter.rewriteURL(sl.URL)
			if cleanU, ok := sanitizeLegacySiteURL(rewritten, true); ok && cleanU != "" {
				sl.URL = cleanU
				validLinks = append(validLinks, sl)
			} else if report != nil && strings.TrimSpace(sl.URL) != "" {
				report.Warnings = append(report.Warnings, fmt.Sprintf("dropped invalid social link %q (%q)", sl.Label, sl.URL))
			}
		}
		out.settings.Site.SocialLinks = validLinks
	}

	// Skin fallback -> pure volo-studio + accent inheritance
	skin := getOpt("skinDirName")
	if skin != "" && !strings.EqualFold(skin, "volo-studio") {
		report.Warnings = append(report.Warnings,
			fmt.Sprintf("retired legacy skin %q automatically migrated to volo-studio", skin))
	}
	out.settings.Appearance.ActiveTheme = "volo-studio"
	if acc := getOpt("appearanceAccent"); isValidAccent(acc) {
		out.settings.Appearance.Accent = acc
	}
	if acc := strings.ToLower(strings.TrimSpace(opts.Accent)); isValidAccent(acc) {
		out.settings.Appearance.Accent = acc
	}

	// Rewrite user & category fields collected in Pass 1
	for i := range st.users {
		if st.users[i].AvatarURL != "" {
			st.users[i].AvatarURL = rewriter.rewriteURL(st.users[i].AvatarURL)
		}
		if st.users[i].WebsiteURL != "" {
			st.users[i].WebsiteURL = rewriter.rewriteURL(st.users[i].WebsiteURL)
		}
	}
	for i := range st.categories {
		if st.categories[i].Description != "" {
			st.categories[i].Description = rewriter.rewriteText(st.categories[i].Description)
		}
	}

	// 2. Reading & Comment Governance settings
	switch getOpt("articleListStyle") {
	case "titleOnly":
		out.settings.Reading.ListStyle = "title_only"
	case "titleAndContent":
		out.settings.Reading.ListStyle = "full"
	default:
		out.settings.Reading.ListStyle = "summary"
	}
	if n := parseIntBound(getOpt("articleListDisplayCount"), 1, 100); n > 0 {
		out.settings.Reading.PostsPerPage = n
	}
	if n := parseIntBound(getOpt("recentArticleDisplayCount"), 1, 50); n > 0 {
		out.settings.Reading.RecentPostsCount = n
	}
	if n := parseIntBound(getOpt("recentCommentDisplayCount"), 1, 50); n > 0 {
		out.settings.Reading.RecentCommentsCount = n
	}
	if n := parseIntBound(getOpt("mostUsedTagDisplayCount"), 1, 100); n > 0 {
		out.settings.Reading.PopularTagsCount = n
	}
	if n := parseIntBound(getOpt("relevantArticlesDisplayCount"), 0, 20); n >= 0 && getOpt("relevantArticlesDisplayCount") != "" {
		out.settings.Reading.RelatedPostsCount = n
	}
	if v := getOpt("enableReadingStats"); v != "" {
		out.settings.Reading.EnableReadingStats = parseLegacyBool(v, true)
	}
	if v := getOpt("enableInstantSearch"); v != "" {
		out.settings.Reading.EnableInstantSearch = parseLegacyBool(v, true)
	}
	if v := getOpt("showCodeBlockLn"); v != "" {
		out.settings.Reading.ShowCodeLineNumbers = parseLegacyBool(v, false)
	}
	if v := getOpt("enableArticleUpdateHint"); v != "" && !parseLegacyBool(v, true) {
		out.settings.Reading.OutdatedNoticeDays = 0
	} else if d := parseIntBound(getOpt("articleOutdatedDays"), 0, 3650); getOpt("articleOutdatedDays") != "" {
		out.settings.Reading.OutdatedNoticeDays = d
	}
	if v := getOpt("allowRegister"); v != "" {
		out.settings.Reading.AllowReaderRegister = parseLegacyBool(v, false)
	}
	commentable := parseLegacyBool(getOpt("commentable"), true)
	interactive := parseLegacyBool(getOpt("interactive"), true)
	if !commentable || !interactive {
		out.settings.Reading.CommentAccessMode = domain.CommentAccessOff
	} else if m := getOpt("commentAccessMode"); m == "registered" || m == "off" || m == "all" {
		out.settings.Reading.CommentAccessMode = m
	}
	switch getOpt("commentModerationMode") {
	case "all":
		out.settings.Reading.CommentModerationMode = domain.CommentModerationAll
	case "smart", "guest":
		out.settings.Reading.CommentModerationMode = domain.CommentModerationSmart
	case "off":
		out.settings.Reading.CommentModerationMode = domain.CommentModerationOff
	}
	if v := getOpt("commentFilterBuiltin"); v != "" {
		out.settings.Reading.BuiltinSpamFilter = parseLegacyBool(v, true)
	}
	if v := getOpt("spam"); v != "" {
		out.settings.Reading.BlockedKeywords = splitWordList(v)
	}
	if v := getOpt("commentFilterAllow"); v != "" {
		out.settings.Reading.AllowedKeywords = splitWordList(v)
	}

	// 3. Feed & SEO & GitHub Showcase
	if getOpt("feedOutputMode") == "fullContent" {
		out.settings.Feed.OutputMode = "full"
	}
	if n := parseIntBound(getOpt("feedOutputCnt"), 1, 100); n > 0 {
		out.settings.Feed.ItemCount = n
	}
	out.settings.Feed.Podcast.Title = out.settings.Site.Title
	out.settings.Feed.Podcast.Language = out.settings.Site.DefaultLocale

	if v := getOpt("metaKeywords"); v != "" {
		out.settings.SEO.MetaKeywords = v
	}
	if v := getOpt("metaDescription"); v != "" {
		out.settings.SEO.MetaDescription = v
		if out.settings.Site.Description == base.Site.Description {
			out.settings.Site.Description = v
		}
	}
	if v := getOpt("indexNowEnabled"); v != "" {
		out.settings.SEO.IndexNowEnabled = parseLegacyBool(v, false)
	}
	if v := strings.TrimSpace(getOpt("indexNowKey")); isValidLegacyIndexNowKey(v) {
		out.settings.SEO.IndexNowKey = v
	}
	if v := getOpt("myGitHubID"); v != "" {
		out.settings.SEO.GitHubUsername = v
	}
	if ghRaw, ok := st.reassembleOptionDoc("githubRepos", report); ok && ghRaw != "" {
		dispRaw, _ := st.reassembleOptionDoc("githubRepositoryDisplay", report)
		parseLegacyGitHubRepos(ghRaw, dispRaw, &out.settings.SEO)
	}

	// 4. Storage settings (decomposing legacy delimited tuChuangConfig)
	if tc := getOpt("tuChuangConfig"); tc != "" {
		parseLegacyStorageConfig(tc, &out.settings.Storage, &out.storageSecret, report)
	}
	if v := getOpt("imageWebpAdaptive"); v != "" {
		out.settings.Storage.AdaptiveWebP = parseLegacyBool(v, true)
	}
	if opts.ForceLocalStorage {
		out.settings.Storage.ActiveDriver = "local"
	}

	// 5. SMTP Notifications
	parseLegacySMTP(getOpt, &out.settings.Notifications, &out.smtpSecret)

	// 6. Security settings, IP Bans & Geo Access Rules
	if v := getOpt("riskControlEnabled"); v != "" {
		out.settings.Security.RiskControlEnabled = parseLegacyBool(v, true)
	}
	if v := getOpt("wafPower"); v != "" {
		out.settings.Security.WAFEnabled = parseLegacyBool(v, true)
	}
	if n := parseIntBound(getOpt("wafCurrentLimitSecond"), 1, 86400); n > 0 {
		out.settings.Security.WAFWindowSeconds = n
	}
	if n := parseIntBound(getOpt("wafCurrentLimitTimes"), 1, 100000); n > 0 {
		out.settings.Security.WAFMaxRequests = n
	}
	if bansRaw, ok := st.reassembleOptionDoc("voloIpBans", report); ok && bansRaw != "" {
		out.ipBans = parseLegacyIPBans(bansRaw)
	}
	if geoRaw, ok := st.reassembleOptionDoc("voloGeoAccess", report); ok && geoRaw != "" {
		out.geoRules = parseLegacyGeoAccess(geoRaw, &out.settings.Security)
	}

	// 7. Local & Remote Backup settings
	if v := getOpt("localBackupAuto"); v != "" {
		out.settings.Backup.LocalAutoEnabled = parseLegacyBool(v, false)
	}
	if n := parseIntBound(getOpt("localBackupKeepCount"), 1, 365); n > 0 {
		out.settings.Backup.LocalKeepCount = n
	}
	if rbRaw, ok := st.reassembleOptionDoc("voloRemoteBackup", report); ok && rbRaw != "" {
		parseLegacyRemoteBackup(rbRaw, &out.settings.Backup, &out.backupSecrets)
	}

	// 8. Friend Link Health Cache ("voloLinkHealth")
	if lhRaw, ok := st.reassembleOptionDoc("voloLinkHealth", report); ok && lhRaw != "" {
		_ = json.Unmarshal([]byte(lhRaw), &st.linkHealth)
	}

	// 9. Chunked Option Payloads -> First-Class Normalized Tables!
	// 9a. voloSeries -> series & series_articles
	if serRaw, ok := st.reassembleOptionDoc("voloSeries", report); ok && serRaw != "" {
		out.series, out.seriesArticles = parseLegacySeries(serRaw)
		for i := range out.series {
			out.series[i].Description = rewriter.rewriteText(out.series[i].Description)
			out.series[i].CoverURL = rewriter.rewriteURL(out.series[i].CoverURL)
		}
		for i := range out.seriesArticles {
			out.seriesArticles[i].ArticleID = st.resolveArticleID(out.seriesArticles[i].ArticleID)
		}
		report.Series = len(out.series)
	}

	// 9b. voloBooks + voloBookToc.<id> -> books & book_chapters
	if booksRaw, ok := st.reassembleOptionDoc("voloBooks", report); ok && booksRaw != "" {
		books, chapters := parseLegacyBooksAndTOCs(booksRaw, st, report)
		for i := range books {
			books[i].Description = rewriter.rewriteText(books[i].Description)
			books[i].CoverURL = rewriter.rewriteURL(books[i].CoverURL)
			books[i].SourceURL = rewriter.rewriteURL(books[i].SourceURL)
		}
		for i := range chapters {
			chapters[i].URL = rewriter.rewriteURL(chapters[i].URL)
		}
		out.books = books
		out.bookChapters = chapters
		report.Books = len(books)
		report.BookChapters = len(chapters)
	}

	// 9c. voloDailyStats -> analytics_daily & analytics_top_items
	if statsRaw, ok := st.reassembleOptionDoc("voloDailyStats", report); ok && statsRaw != "" {
		out.analyticsDaily, out.analyticsTop = parseLegacyAnalytics(statsRaw, st)
		report.AnalyticsDays = len(out.analyticsDaily)
	}

	// 9d. voloWebhooks -> webhooks (with AES-256-GCM secret encryption)
	if whRaw, ok := st.reassembleOptionDoc("voloWebhooks", report); ok && whRaw != "" {
		whs, wErr := parseLegacyWebhooks(whRaw, kr, report)
		if wErr != nil {
			return nil, wErr
		}
		out.webhooks = whs
		report.Webhooks = len(whs)
	}

	return out, nil
}

func insertPass1EntitiesTx(
	ctx context.Context,
	tx *sqlx.Tx,
	st *pass1Data,
	dec *decomposedState,
	existingAdmins []domain.User,
	preserveExistingAdmins bool,
	report *ImportReport,
) error {
	// Clear target tables in reverse FK dependency order for clean idempotent import
	tablesToClear := []string{
		"analytics_top_items", "analytics_daily",
		"webhook_deliveries", "webhooks",
		"security_geo_rules", "security_ip_bans",
		"reactions", "media_assets", "redirects",
		"follow_items", "follows", "links", "comments",
		"book_chapters", "books",
		"series_articles", "series", "nav_items",
		"article_backlinks", "article_revisions", "article_tags", "articles",
		"pages", "tags", "categories", "users",
	}
	for _, tbl := range tablesToClear {
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+tx.Dialect().QuoteIdent(tbl)); err != nil {
			return fmt.Errorf("etl: clear %s: %w", tbl, err)
		}
	}

	usedIDs := make(map[string]bool, len(existingAdmins)+len(st.users))
	usedUsernames := make(map[string]bool, len(existingAdmins)+len(st.users))
	usedEmails := make(map[string]bool, len(existingAdmins)+len(st.users))
	if preserveExistingAdmins && len(existingAdmins) > 0 {
		for _, adm := range existingAdmins {
			uid := truncRunes(adm.ID, 32)
			uname := truncRunes(adm.Username, 64)
			uemail := truncRunes(adm.Email, 190)
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO users (id, username, email, display_name, avatar_url, bio, website_url, role, password_hash, legacy_hash, status, email_verified_at, preferred_locale, created_at, updated_at)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				uid, uname, uemail, truncRunes(adm.DisplayName, 128),
				truncRunes(adm.AvatarURL, 512), cleanUTF8(adm.Bio), truncRunes(adm.WebsiteURL, 512),
				domain.RoleAdmin, truncRunes(adm.PasswordHash, 255), truncRunes(adm.LegacyHash, 255),
				truncRunes(adm.Status, 32), adm.EmailVerifiedAt, truncRunes(adm.PreferredLocale, 32), adm.CreatedAt, adm.UpdatedAt,
			); err != nil {
				return fmt.Errorf("etl: preserve existing admin %s: %w", uid, err)
			}
			usedIDs[uid] = true
			usedUsernames[strings.ToLower(uname)] = true
			usedEmails[strings.ToLower(uemail)] = true
			report.Users++
		}
	}

	for _, u := range st.users {
		uid := truncRunes(u.ID, 32)
		if usedIDs[uid] {
			continue
		}
		if preserveExistingAdmins && len(existingAdmins) > 0 {
			if u.Role == domain.RoleAdmin {
				u.Role = domain.RoleEditor
			}
			u.PasswordHash = ""
			u.LegacyHash = ""
			uname := truncRunes(u.Username, 64)
			if usedUsernames[strings.ToLower(uname)] {
				uname = truncRunes(uname+"_legacy_"+uid, 64)
			}
			u.Username = uname
			uemail := truncRunes(u.Email, 190)
			if usedEmails[strings.ToLower(uemail)] {
				uemail = truncRunes("legacy+"+uid+"@local.volo", 190)
			}
			u.Email = uemail
		} else if u.Role == domain.RoleAdmin && u.PasswordHash == "" && u.LegacyHash == "" && len(existingAdmins) > 0 {
			u.PasswordHash = existingAdmins[0].PasswordHash
			u.LegacyHash = existingAdmins[0].LegacyHash
		}
		usedIDs[uid] = true
		usedUsernames[strings.ToLower(truncRunes(u.Username, 64))] = true
		usedEmails[strings.ToLower(truncRunes(u.Email, 190))] = true
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO users (id, username, email, display_name, avatar_url, bio, website_url, role, password_hash, legacy_hash, status, email_verified_at, preferred_locale, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			uid, truncRunes(u.Username, 64), truncRunes(u.Email, 190), truncRunes(u.DisplayName, 128),
			truncRunes(u.AvatarURL, 512), cleanUTF8(u.Bio), truncRunes(u.WebsiteURL, 512),
			truncRunes(u.Role, 32), truncRunes(u.PasswordHash, 255), truncRunes(u.LegacyHash, 255),
			truncRunes(u.Status, 32), u.EmailVerifiedAt, truncRunes(u.PreferredLocale, 32), u.CreatedAt, u.UpdatedAt,
		); err != nil {
			return fmt.Errorf("etl: insert user %s: %w", u.ID, err)
		}
		report.Users++
	}

	for _, c := range st.categories {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO categories (id, name, slug, description, icon, sort_order, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			truncRunes(c.ID, 32), truncRunes(c.Name, 128), truncRunes(c.Slug, 128),
			cleanUTF8(c.Description), truncRunes(c.Icon, 255), c.SortOrder, c.CreatedAt, c.UpdatedAt,
		); err != nil {
			return fmt.Errorf("etl: insert category %s: %w", c.ID, err)
		}
		report.Categories++
	}

	for _, tg := range st.tags {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO tags (id, name, slug, created_at) VALUES (?, ?, ?, ?)`,
			truncRunes(tg.ID, 32), truncRunes(tg.Name, 128), truncRunes(tg.Slug, 128), tg.CreatedAt,
		); err != nil {
			return fmt.Errorf("etl: insert tag %s: %w", tg.ID, err)
		}
		report.Tags++
	}

	importedSeriesIDs := make(map[string]bool, len(dec.series))
	for _, s := range dec.series {
		sid := truncRunes(s.ID, 32)
		if importedSeriesIDs[sid] {
			continue
		}
		importedSeriesIDs[sid] = true
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO series (id, title, slug, description, cover_url, sort_order, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			sid, truncRunes(s.Title, 255), truncRunes(s.Slug, 190), cleanUTF8(s.Description),
			truncRunes(s.CoverURL, 512), s.SortOrder, s.CreatedAt, s.UpdatedAt,
		); err != nil {
			return fmt.Errorf("etl: insert series %s: %w", sid, err)
		}
	}

	importedBookIDs := make(map[string]bool, len(dec.books))
	for _, bk := range dec.books {
		bid := truncRunes(bk.ID, 32)
		if importedBookIDs[bid] {
			continue
		}
		importedBookIDs[bid] = true
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO books (id, title, slug, description, cover_url, source_type, source_url, series_id, embed_mode, chapter_count, synced_at, sort_order, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			bid, truncRunes(bk.Title, 255), truncRunes(bk.Slug, 190), cleanUTF8(bk.Description),
			truncRunes(bk.CoverURL, 512), truncRunes(bk.SourceType, 32), truncRunes(bk.SourceURL, 512),
			truncRunes(bk.SeriesID, 32), truncRunes(bk.EmbedMode, 32), bk.ChapterCount, bk.SyncedAt, bk.SortOrder, bk.CreatedAt, bk.UpdatedAt,
		); err != nil {
			return fmt.Errorf("etl: insert book %s: %w", bid, err)
		}
	}
	for _, ch := range dec.bookChapters {
		bid := truncRunes(ch.BookID, 32)
		if !importedBookIDs[bid] {
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO book_chapters (id, book_id, parent_id, title, url, depth, sort_order)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			truncRunes(ch.ID, 32), bid, truncRunes(ch.ParentID, 32), truncRunes(ch.Title, 255),
			truncRunes(ch.URL, 512), ch.Depth, ch.SortOrder,
		); err != nil {
			return fmt.Errorf("etl: insert book_chapter %s: %w", ch.ID, err)
		}
	}

	seenDates := make(map[string]bool, len(dec.analyticsDaily))
	for _, ad := range dec.analyticsDaily {
		sd := truncRunes(ad.StatDate, 32)
		if seenDates[sd] {
			continue
		}
		seenDates[sd] = true
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO analytics_daily (stat_date, pv, uv, updated_at) VALUES (?, ?, ?, ?)`,
			sd, ad.PV, ad.UV, ad.UpdatedAt,
		); err != nil {
			return fmt.Errorf("etl: insert analytics_daily %s: %w", sd, err)
		}
	}
	seenTopKeys := make(map[string]bool, len(dec.analyticsTop))
	for _, top := range dec.analyticsTop {
		sd := truncRunes(top.StatDate, 32)
		kd := truncRunes(top.Kind, 32)
		ik := truncRunes(top.ItemKey, 512)
		tk := sd + "|" + kd + "|" + ik
		if seenTopKeys[tk] {
			continue
		}
		seenTopKeys[tk] = true
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO analytics_top_items (stat_date, kind, item_key, hits) VALUES (?, ?, ?, ?)`,
			sd, kd, ik, top.Hits,
		); err != nil {
			return fmt.Errorf("etl: insert analytics_top_items: %w", err)
		}
	}

	seenWebhookIDs := make(map[string]bool, len(dec.webhooks))
	for _, wh := range dec.webhooks {
		wid := truncRunes(wh.ID, 32)
		if seenWebhookIDs[wid] {
			wid = id.NextPrefixed("wh")
		}
		seenWebhookIDs[wid] = true
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO webhooks (id, name, provider, url, secret_ciphertext, chat_id, events_json, is_enabled, last_status_code, last_error, last_triggered_at, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			wid, truncRunes(wh.Name, 128), truncRunes(wh.Provider, 32), truncRunes(wh.URL, 512),
			wh.SecretCiphertext, truncRunes(wh.ChatID, 128), wh.EventsJSON,
			wh.IsEnabled, 0, "", 0, wh.CreatedAt, wh.UpdatedAt,
		); err != nil {
			return fmt.Errorf("etl: insert webhook %s: %w", wid, err)
		}
	}

	for _, ban := range dec.ipBans {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO security_ip_bans (id, cidr, source, reason_code, reason, note, occurrences, expires_at, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			truncRunes(ban.ID, 32), truncRunes(ban.CIDR, 64), truncRunes(ban.Source, 32),
			truncRunes(ban.ReasonCode, 64), cleanUTF8(ban.Reason), cleanUTF8(ban.Note),
			ban.Occurrences, ban.ExpiresAt, ban.CreatedAt, ban.UpdatedAt,
		); err != nil {
			return fmt.Errorf("etl: insert ip_ban %s: %w", ban.CIDR, err)
		}
	}

	for _, gr := range dec.geoRules {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO security_geo_rules (id, name, is_enabled, action, on_miss, countries, regions, sort_order, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			truncRunes(gr.ID, 32), truncRunes(gr.Name, 128), gr.IsEnabled,
			truncRunes(gr.Action, 32), truncRunes(gr.OnMiss, 32), cleanUTF8(gr.Countries), cleanUTF8(gr.Regions),
			gr.SortOrder, gr.CreatedAt, gr.UpdatedAt,
		); err != nil {
			return fmt.Errorf("etl: insert geo_rule %s: %w", gr.Name, err)
		}
	}

	return nil
}

type deferredRevision struct {
	id         string
	articleID  string
	title      string
	summaryMD  string
	contentMD  string
	authorName string
	note       string
	createdAt  int64
}

func streamPass2IntoTx(
	ctx context.Context,
	tx *sqlx.Tx,
	openStream func() (io.ReadCloser, error),
	st *pass1Data,
	seriesArticles []domain.SeriesArticle,
	kr *config.KeyRing,
	opts ImportOptions,
	rewriter *urlRewriter,
	report *ImportReport,
) error {
	rc, err := openStream()
	if err != nil {
		return err
	}
	defer rc.Close()

	dec := json.NewDecoder(rc)
	dec.UseNumber()

	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return errors.New("etl: invalid root JSON in pass 2")
	}

	importedArticleIDs := make(map[string]bool)
	usedArticleSlugs := make(map[string]bool)
	usedRedirects := make(map[string]bool)
	usedNavIDs := make(map[string]bool)
	usedCommentIDs := make(map[string]bool)
	usedLinkIDs := make(map[string]bool)
	usedFollowIDs := make(map[string]bool)
	var pendingRevs []deferredRevision

	upsertArtTagSQL := tx.Dialect().UpsertSQL("article_tags", []string{"article_id", "tag_id"}, []string{"article_id", "tag_id"}, nil)

	defaultLocale := i18n.NormalizeLocale(firstNonEmpty(opts.DefaultLocale, st.options["localeString"].Value))
	if defaultLocale == "" {
		defaultLocale = "zh-CN"
	}
	nowMs := time.Now().UnixMilli()

	for dec.More() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		keyTok, err := dec.Token()
		if err != nil {
			return err
		}
		rootKey, _ := keyTok.(string)

		switch rootKey {
		case "articles":
			if err := streamJSONArray(dec, func(raw map[string]any) error {
				origAID := strings.TrimSpace(strVal(raw["oId"]))
				if origAID == "" {
					return nil
				}
				aid := st.resolveArticleID(origAID)
				if aid == "" || importedArticleIDs[aid] {
					return nil
				}
				origPermalink := normalizeLegacyPath(strVal(raw["articlePermalink"]))

				authorID := st.userIDMap[strings.TrimSpace(strVal(raw["articleAuthorId"]))]
				if authorID == "" {
					authorID = st.defaultAdminID
				}
				catID := st.articlePrimaryCat[aid]
				title := firstNonEmpty(strings.TrimSpace(strVal(raw["articleTitle"])), "Untitled "+aid)

				slug := st.articleSlugMap[aid]
				if slug == "" || usedArticleSlugs[slug] {
					slugBase := deriveSlugFromPermalinkOrTitle(origPermalink, title, aid)
					slug = dedupSlug(slugBase, usedArticleSlugs)
				} else {
					usedArticleSlugs[slug] = true
				}
				slug = truncRunes(slug, 190)
				modernPath := "/posts/" + slug

				contentMD := rewriter.rewriteText(strVal(raw["articleContent"]))
				summaryMD := rewriter.rewriteText(strVal(raw["articleAbstract"]))
				createdAt := int64Val(raw["articleCreated"])
				if createdAt <= 0 {
					createdAt = parseMillisFallback(aid)
				}
				updatedAt := int64Val(raw["articleUpdated"])
				if updatedAt <= 0 {
					updatedAt = createdAt
				}
				schedAt := int64Val(raw["articleScheduleTime"])
				statusInt := intVal(raw["articleStatus"])

				status := domain.ArticleStatusPublished
				publishedAt := createdAt
				if statusInt != 0 {
					if schedAt > nowMs {
						status = domain.ArticleStatusScheduled
						publishedAt = 0
					} else {
						status = domain.ArticleStatusDraft
						publishedAt = 0
					}
				}

				viewPwd := strVal(raw["articleViewPwd"])
				pwdCipher, sErr := kr.SealString(viewPwd, config.ArticlePasswordAAD(aid))
				if sErr != nil {
					return sErr
				}
				hasLock := containsLockDirectiveETL(contentMD)
				words, mins := estimateReadingStats(contentMD)

				audioURL := rewriter.rewriteURL(strings.TrimSpace(strVal(raw["articleAudioURL"])))
				audioMime := ""
				if audioURL != "" {
					audioMime = inferAudioMIME(audioURL)
				}

				coverURL := strVal(raw["articleImg1URL"])
				if isLegacyExternalAsset(coverURL) {
					coverURL = ""
				} else if coverURL != "" {
					coverURL = rewriter.rewriteURL(coverURL)
				}

				if _, err := tx.ExecContext(ctx,
					`INSERT INTO articles (
						id, author_id, category_id, title, slug, legacy_permalink, summary_md, content_md,
						cover_url, status, published_at, scheduled_at, is_pinned, allow_comment,
						password_ciphertext, has_content_lock, audio_url, audio_duration_sec, audio_size_bytes,
						audio_mime, lang, translation_group, license_override, word_count, reading_minutes,
						view_count, comment_count, created_at, updated_at
					) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '', '', ?, ?, ?, ?, ?, ?)`,
					truncRunes(aid, 32), truncRunes(authorID, 32), truncRunes(catID, 32),
					truncRunes(title, 255), slug, truncRunes(origPermalink, 512), summaryMD, contentMD,
					truncRunes(coverURL, 512), truncRunes(status, 32), publishedAt, schedAt,
					boolVal(raw["articlePutTop"]), boolValDefault(raw["articleCommentable"], true),
					pwdCipher, hasLock, truncRunes(audioURL, 512), intVal(raw["articleAudioDuration"]), int64Val(raw["articleAudioSize"]),
					truncRunes(audioMime, 64), truncRunes(defaultLocale, 32), words, mins,
					int64Val(raw["articleViewCount"]), intVal(raw["articleCommentCount"]), createdAt, updatedAt,
				); err != nil {
					return fmt.Errorf("etl: insert article %s: %w", aid, err)
				}
				importedArticleIDs[aid] = true
				report.Articles++

				// Bind free-form tags (from tag_articles + fallback comma-separated articleTags)
				boundTags := make(map[string]bool)
				for _, tid := range st.artTagsMap[aid] {
					if st.tagIDSet[tid] && !boundTags[tid] {
						boundTags[tid] = true
						if _, err := tx.ExecContext(ctx, upsertArtTagSQL, truncRunes(aid, 32), truncRunes(tid, 32)); err != nil {
							return fmt.Errorf("etl: bind article_tag (%s, %s): %w", aid, tid, err)
						}
					}
				}
				for _, rawTagName := range strings.Split(strVal(raw["articleTags"]), ",") {
					tName := strings.TrimSpace(rawTagName)
					if tid, ok := st.tagByName[strings.ToLower(tName)]; ok && !boundTags[tid] {
						boundTags[tid] = true
						if _, err := tx.ExecContext(ctx, upsertArtTagSQL, truncRunes(aid, 32), truncRunes(tid, 32)); err != nil {
							return fmt.Errorf("etl: bind article_tag (%s, %s): %w", aid, tid, err)
						}
					}
				}

				// Register 301 Redirect Guardian for historical permalink (including .html)!
				if origPermalink != "" && origPermalink != modernPath && !usedRedirects[origPermalink] && utf8.RuneCountInString(origPermalink) <= 512 {
					usedRedirects[origPermalink] = true
					if _, err := tx.ExecContext(ctx,
						`INSERT INTO redirects (id, source_path, target_path, status_code, is_system_legacy, hit_count, created_at)
						 VALUES (?, ?, ?, 301, ?, 0, ?)`,
						id.Next(), origPermalink, truncRunes(modernPath, 512), true, createdAt,
					); err != nil {
						return fmt.Errorf("etl: insert redirect %s -> %s: %w", origPermalink, modernPath, err)
					}
					report.RedirectsCreated++
				}
				return nil
			}); err != nil {
				return err
			}

		case "articleRevisions", "revisions", "article_revision":
			if err := streamJSONArray(dec, func(raw map[string]any) error {
				rid := firstNonEmpty(strings.TrimSpace(strVal(raw["oId"])), id.Next())
				if len(rid) > 32 {
					rid = id.Next()
				}
				aid := st.resolveArticleID(strings.TrimSpace(strVal(raw["articleOId"])))
				if aid == "" {
					return nil
				}
				pendingRevs = append(pendingRevs, deferredRevision{
					id:         rid,
					articleID:  aid,
					title:      strVal(raw["revisionTitle"]),
					summaryMD:  rewriter.rewriteText(strVal(raw["revisionAbstract"])),
					contentMD:  rewriter.rewriteText(strVal(raw["revisionContent"])),
					authorName: strVal(raw["revisionAuthorName"]),
					note:       strVal(raw["revisionNote"]),
					createdAt:  int64Val(raw["revisionCreated"]),
				})
				return nil
			}); err != nil {
				return err
			}

		case "pages":
			// Legacy "page" table held ONLY header navigation items -> split cleanly into nav_items!
			if err := streamJSONArray(dec, func(raw map[string]any) error {
				nid := firstNonEmpty(strings.TrimSpace(strVal(raw["oId"])), id.Next())
				if len(nid) > 32 || usedNavIDs[nid] {
					nid = id.Next()
				}
				title := strings.TrimSpace(strVal(raw["pageTitle"]))
				targetURL := rewriter.rewriteURL(strings.TrimSpace(strVal(raw["pagePermalink"])))
				if title == "" || targetURL == "" {
					return nil
				}
				usedNavIDs[nid] = true
				newTab := strings.EqualFold(strVal(raw["pageOpenTarget"]), "_blank")
				icon := strings.TrimSpace(strVal(raw["pageIcon"]))
				if isLegacyExternalAsset(icon) {
					icon = ""
				} else if icon != "" {
					icon = rewriter.rewriteURL(icon)
				}
				ts := parseMillisFallback(nid)
				if _, err := tx.ExecContext(ctx,
					`INSERT INTO nav_items (id, parent_id, title, i18n_titles_json, url, open_in_new_tab, icon, sort_order, is_enabled, created_at, updated_at)
					 VALUES (?, '', ?, '{}', ?, ?, ?, ?, ?, ?, ?)`,
					truncRunes(nid, 32), truncRunes(title, 128), truncRunes(targetURL, 512),
					newTab, truncRunes(icon, 255), intVal(raw["pageOrder"]), true, ts, ts,
				); err != nil {
					return fmt.Errorf("etl: insert nav_item %s: %w", nid, err)
				}
				report.NavItems++
				return nil
			}); err != nil {
				return err
			}

		case "comments":
			if err := streamJSONArray(dec, func(raw map[string]any) error {
				cid := strings.TrimSpace(strVal(raw["oId"]))
				origOnID := strings.TrimSpace(strVal(raw["commentOnId"]))
				if cid == "" || origOnID == "" || usedCommentIDs[cid] {
					return nil
				}
				origCID := cid
				if len(cid) > 32 {
					cid = id.Next()
				}
				onID := st.resolveArticleID(origOnID)
				usedCommentIDs[origCID] = true
				usedCommentIDs[cid] = true
				rawUID := strings.TrimSpace(strVal(raw["commentUserId"]))
				mappedUID := st.userIDMap[rawUID]

				notifyEmail := strings.TrimSpace(strVal(raw["commentNotifyEmail"]))
				if notifyEmail == "" {
					notifyEmail = st.commentMailFallback[origCID]
				}
				emailCipher, sErr := kr.SealString(notifyEmail, config.CommentNotifyEmailAAD(cid))
				if sErr != nil {
					return sErr
				}

				clientIP := strings.TrimSpace(strVal(raw["commentClientIP"]))
				ipMasked := maskIPAddress(clientIP)
				status := domain.CommentStatusApproved
				switch intVal(raw["commentStatus"]) {
				case 1:
					status = domain.CommentStatusPending
				case 2:
					status = domain.CommentStatusRejected
				}
				isDeleted := boolVal(raw["commentDeleted"])
				createdAt := int64Val(raw["commentCreated"])
				if createdAt <= 0 {
					createdAt = parseMillisFallback(cid)
				}
				avatar := sanitizeLegacyAvatar(strVal(raw["commentThumbnailURL"]))
				if avatar != "" {
					avatar = rewriter.rewriteURL(avatar)
				}
				authorURL := rewriter.rewriteURL(strVal(raw["commentURL"]))
				cmtBody := rewriter.rewriteText(strVal(raw["commentContent"]))
				parentID := strings.TrimSpace(strVal(raw["commentOriginalCommentId"]))
				if len(parentID) > 32 {
					parentID = truncRunes(parentID, 32)
				}

				if _, err := tx.ExecContext(ctx,
					`INSERT INTO comments (
						id, target_type, target_id, parent_id, reply_to_name, user_id, guest_id_hash, env_fp_hash,
						author_name, author_url, avatar_url, notify_email_ciphertext, notify_locale,
						client_ip, ip_masked, geo_region, content_md, status, is_deleted, created_at, updated_at
					) VALUES (?, 'article', ?, ?, ?, ?, ?, '', ?, ?, ?, ?, ?, ?, ?, '', ?, ?, ?, ?, ?)`,
					truncRunes(cid, 32), truncRunes(onID, 32), parentID, truncRunes(strVal(raw["commentOriginalCommentName"]), 128),
					truncRunes(mappedUID, 32), truncRunes(strVal(raw["commentGuestKeyHash"]), 64),
					truncRunes(firstNonEmpty(strVal(raw["commentName"]), "Guest"), 128),
					truncRunes(authorURL, 512), truncRunes(avatar, 512),
					emailCipher, truncRunes(defaultLocale, 32), truncRunes(clientIP, 64), truncRunes(ipMasked, 64), cmtBody,
					truncRunes(status, 32), isDeleted, createdAt, createdAt,
				); err != nil {
					return fmt.Errorf("etl: insert comment %s: %w", cid, err)
				}
				report.Comments++
				return nil
			}); err != nil {
				return err
			}

		case "links":
			if err := streamJSONArray(dec, func(raw map[string]any) error {
				lid := firstNonEmpty(strings.TrimSpace(strVal(raw["oId"])), id.Next())
				if len(lid) > 32 || usedLinkIDs[lid] {
					lid = id.Next()
				}
				addr := rewriter.rewriteURL(strings.TrimSpace(strVal(raw["linkAddress"])))
				if addr == "" {
					return nil
				}
				usedLinkIDs[lid] = true
				lh := st.linkHealth[lid]
				hStatus := "unknown"
				if lh.Status != "" {
					hStatus = lh.Status
				} else if lh.CheckedAt > 0 {
					if lh.OK {
						hStatus = "ok"
					} else {
						hStatus = "broken"
					}
				}
				code := lh.HTTPCode
				if code == 0 {
					code = lh.CodeAlt
				}
				ts := parseMillisFallback(lid)
				icon := strVal(raw["linkIcon"])
				if isLegacyExternalAsset(icon) {
					icon = ""
				} else if icon != "" {
					icon = rewriter.rewriteURL(icon)
				}
				desc := rewriter.rewriteText(strVal(raw["linkDescription"]))
				if _, err := tx.ExecContext(ctx,
					`INSERT INTO links (id, title, url, description, icon_url, category_name, sort_order, health_status, http_code, latency_ms, health_error, last_checked_at, created_at, updated_at)
					 VALUES (?, ?, ?, ?, ?, '', ?, ?, ?, ?, ?, ?, ?, ?)`,
					truncRunes(lid, 32), truncRunes(firstNonEmpty(strVal(raw["linkTitle"]), addr), 255),
					truncRunes(addr, 512), desc, truncRunes(icon, 512),
					intVal(raw["linkOrder"]), truncRunes(hStatus, 32), code, lh.LatencyMS,
					truncRunes(lh.Error, 255), lh.CheckedAt, ts, ts,
				); err != nil {
					return fmt.Errorf("etl: insert link %s: %w", lid, err)
				}
				report.Links++
				return nil
			}); err != nil {
				return err
			}

		case "follows":
			if err := streamJSONArray(dec, func(raw map[string]any) error {
				fid := firstNonEmpty(strings.TrimSpace(strVal(raw["oId"])), id.Next())
				if len(fid) > 32 || usedFollowIDs[fid] {
					fid = id.Next()
				}
				addr := rewriter.rewriteURL(strings.TrimSpace(strVal(raw["followAddress"])))
				if addr == "" {
					return nil
				}
				usedFollowIDs[fid] = true
				ts := parseMillisFallback(fid)
				icon := strVal(raw["followIcon"])
				if isLegacyExternalAsset(icon) {
					icon = ""
				} else if icon != "" {
					icon = rewriter.rewriteURL(icon)
				}
				desc := rewriter.rewriteText(strVal(raw["followDescription"]))
				if _, err := tx.ExecContext(ctx,
					`INSERT INTO follows (id, title, site_url, feed_url, description, icon_url, sort_order, last_synced_at, sync_error, created_at, updated_at)
					 VALUES (?, ?, ?, ?, ?, ?, ?, 0, '', ?, ?)`,
					truncRunes(fid, 32), truncRunes(firstNonEmpty(strVal(raw["followTitle"]), addr), 255),
					truncRunes(addr, 512), truncRunes(addr, 512), desc, truncRunes(icon, 512),
					intVal(raw["followOrder"]), ts, ts,
				); err != nil {
					return fmt.Errorf("etl: insert follow %s: %w", fid, err)
				}
				report.Follows++
				return nil
			}); err != nil {
				return err
			}

		default:
			if err := skipJSONValue(dec); err != nil {
				return err
			}
		}
	}

	// Insert deferred FK-dependent revisions and series_articles only for valid imported articles!
	usedRevIDs := make(map[string]bool, len(pendingRevs))
	for _, rev := range pendingRevs {
		if !importedArticleIDs[rev.articleID] {
			continue
		}
		rid := truncRunes(rev.id, 32)
		if usedRevIDs[rid] {
			rid = id.Next()
		}
		usedRevIDs[rid] = true
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO article_revisions (id, article_id, title, summary_md, content_md, author_name, note, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			rid, truncRunes(rev.articleID, 32), truncRunes(rev.title, 255),
			rev.summaryMD, rev.contentMD, truncRunes(rev.authorName, 128), truncRunes(rev.note, 255), rev.createdAt,
		); err != nil {
			return fmt.Errorf("etl: insert revision %s: %w", rid, err)
		}
		report.Revisions++
	}

	var existingSeriesRows []struct {
		ID string `db:"id"`
	}
	_ = sqlx.SelectInto(ctx, tx, &existingSeriesRows, `SELECT id FROM series`)
	validSeries := make(map[string]bool, len(existingSeriesRows))
	for _, r := range existingSeriesRows {
		validSeries[r.ID] = true
	}
	seenSeriesArt := make(map[string]bool, len(seriesArticles))
	for _, sa := range seriesArticles {
		sid := truncRunes(sa.SeriesID, 32)
		aid := truncRunes(sa.ArticleID, 32)
		if !validSeries[sid] || !importedArticleIDs[aid] {
			continue
		}
		key := sid + ":" + aid
		if seenSeriesArt[key] {
			continue
		}
		seenSeriesArt[key] = true
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO series_articles (series_id, article_id, sort_order) VALUES (?, ?, ?)`,
			sid, aid, sa.SortOrder,
		); err != nil {
			return fmt.Errorf("etl: insert series_article: %w", err)
		}
	}

	return nil
}

func countPass2Entities(
	ctx context.Context,
	openStream func() (io.ReadCloser, error),
	st *pass1Data,
	rewriter *urlRewriter,
	report *ImportReport,
) error {
	report.Users = len(st.users)
	report.Categories = len(st.categories)
	report.Tags = len(st.tags)

	rc, err := openStream()
	if err != nil {
		return err
	}
	defer rc.Close()

	dec := json.NewDecoder(rc)
	dec.UseNumber()
	if _, err := dec.Token(); err != nil {
		return err
	}
	usedRedirects := make(map[string]bool)
	seenArt := make(map[string]bool)

	for dec.More() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		kt, err := dec.Token()
		if err != nil {
			return err
		}
		key, _ := kt.(string)
		switch key {
		case "articles":
			_ = streamJSONArray(dec, func(raw map[string]any) error {
				origAID := strings.TrimSpace(strVal(raw["oId"]))
				if origAID == "" {
					return nil
				}
				aid := st.resolveArticleID(origAID)
				if seenArt[aid] {
					return nil
				}
				seenArt[aid] = true
				report.Articles++
				_ = rewriter.rewriteText(strVal(raw["articleContent"]))
				_ = rewriter.rewriteText(strVal(raw["articleAbstract"]))
				_ = rewriter.rewriteURL(strVal(raw["articleAudioURL"]))
				if cov := strVal(raw["articleImg1URL"]); cov != "" && !isLegacyExternalAsset(cov) {
					_ = rewriter.rewriteURL(cov)
				}
				origPermalink := normalizeLegacyPath(strVal(raw["articlePermalink"]))
				slug := st.articleSlugMap[aid]
				modernPath := "/posts/" + slug
				if origPermalink != "" && slug != "" && origPermalink != modernPath && !usedRedirects[origPermalink] && utf8.RuneCountInString(origPermalink) <= 512 {
					usedRedirects[origPermalink] = true
					report.RedirectsCreated++
				}
				return nil
			})
		case "articleRevisions", "revisions", "article_revision":
			_ = streamJSONArray(dec, func(raw map[string]any) error {
				aid := st.resolveArticleID(strings.TrimSpace(strVal(raw["articleOId"])))
				if aid != "" {
					report.Revisions++
					_ = rewriter.rewriteText(strVal(raw["revisionAbstract"]))
					_ = rewriter.rewriteText(strVal(raw["revisionContent"]))
				}
				return nil
			})
		case "pages":
			_ = streamJSONArray(dec, func(raw map[string]any) error {
				title := strings.TrimSpace(strVal(raw["pageTitle"]))
				targetURL := rewriter.rewriteURL(strings.TrimSpace(strVal(raw["pagePermalink"])))
				if title != "" && targetURL != "" {
					report.NavItems++
					if ic := strings.TrimSpace(strVal(raw["pageIcon"])); ic != "" && !isLegacyExternalAsset(ic) {
						_ = rewriter.rewriteURL(ic)
					}
				}
				return nil
			})
		case "comments":
			_ = streamJSONArray(dec, func(raw map[string]any) error {
				if strings.TrimSpace(strVal(raw["oId"])) != "" && strings.TrimSpace(strVal(raw["commentOnId"])) != "" {
					report.Comments++
					_ = rewriter.rewriteURL(strVal(raw["commentURL"]))
					_ = rewriter.rewriteText(strVal(raw["commentContent"]))
				}
				return nil
			})
		case "links":
			_ = streamJSONArray(dec, func(raw map[string]any) error {
				if addr := rewriter.rewriteURL(strings.TrimSpace(strVal(raw["linkAddress"]))); addr != "" {
					report.Links++
					_ = rewriter.rewriteText(strVal(raw["linkDescription"]))
				}
				return nil
			})
		case "follows":
			_ = streamJSONArray(dec, func(raw map[string]any) error {
				if addr := rewriter.rewriteURL(strings.TrimSpace(strVal(raw["followAddress"]))); addr != "" {
					report.Follows++
					_ = rewriter.rewriteText(strVal(raw["followDescription"]))
				}
				return nil
			})
		default:
			_ = skipJSONValue(dec)
		}
	}
	return nil
}

func persistDecomposedSettings(ctx context.Context, mgr *repo.SettingsManager, dec *decomposedState) error {
	patch := func(domainName string, payload any) error {
		b, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		_, _, err = mgr.PatchDomain(ctx, domainName, b, 0)
		return err
	}

	if err := patch(domain.SettingsDomainSite, dec.settings.Site); err != nil {
		return err
	}
	if err := patch(domain.SettingsDomainReading, dec.settings.Reading); err != nil {
		return err
	}
	if err := patch(domain.SettingsDomainFeed, dec.settings.Feed); err != nil {
		return err
	}
	if err := patch(domain.SettingsDomainSEO, dec.settings.SEO); err != nil {
		return err
	}
	if err := patch(domain.SettingsDomainAppearance, dec.settings.Appearance); err != nil {
		return err
	}
	if err := patch(domain.SettingsDomainSecurity, dec.settings.Security); err != nil {
		return err
	}

	// Storage + secret
	stMap := structToMap(dec.settings.Storage)
	if dec.storageSecret != "" {
		stMap["s3_secret_key"] = dec.storageSecret
	}
	if err := patch(domain.SettingsDomainStorage, stMap); err != nil {
		return err
	}

	// Notifications + secret
	ntMap := structToMap(dec.settings.Notifications)
	if dec.smtpSecret != "" {
		ntMap["smtp_password"] = dec.smtpSecret
	}
	if err := patch(domain.SettingsDomainNotifications, ntMap); err != nil {
		return err
	}

	// Backup + secrets
	bkMap := structToMap(dec.settings.Backup)
	if dec.backupSecrets.WebDAVPassword != "" {
		bkMap["webdav_password"] = dec.backupSecrets.WebDAVPassword
	}
	if dec.backupSecrets.S3SecretKey != "" {
		bkMap["s3_secret_key"] = dec.backupSecrets.S3SecretKey
	}
	return patch(domain.SettingsDomainBackup, bkMap)
}

// --- Helper parsers for chunked option payloads & legacy schema conversion ---

func parseLegacySeries(raw string) ([]domain.Series, []domain.SeriesArticle) {
	var arr []struct {
		ID          string   `json:"id"`
		Title       string   `json:"title"`
		Description string   `json:"description"`
		ArticleIDs  []string `json:"articleIds"`
	}
	if json.Unmarshal([]byte(raw), &arr) != nil {
		return nil, nil
	}
	usedSlugs := make(map[string]bool)
	var seriesOut []domain.Series
	var relsOut []domain.SeriesArticle
	now := time.Now().UnixMilli()

	for idx, item := range arr {
		sid := firstNonEmpty(strings.TrimSpace(item.ID), id.Next())
		title := strings.TrimSpace(item.Title)
		if title == "" {
			continue
		}
		slug := dedupSlug(slugify(firstNonEmpty(title, sid)), usedSlugs)
		seriesOut = append(seriesOut, domain.Series{
			ID:          sid,
			Title:       title,
			Slug:        slug,
			Description: item.Description,
			CoverURL:    "",
			SortOrder:   idx,
			CreatedAt:   now,
			UpdatedAt:   now,
		})
		seenArt := make(map[string]bool)
		for aIdx, aid := range item.ArticleIDs {
			aid = strings.TrimSpace(aid)
			if aid == "" || seenArt[aid] {
				continue
			}
			seenArt[aid] = true
			relsOut = append(relsOut, domain.SeriesArticle{
				SeriesID:  sid,
				ArticleID: aid,
				SortOrder: aIdx,
			})
		}
	}
	return seriesOut, relsOut
}

type legacyTocNode struct {
	Title    string          `json:"title"`
	URL      string          `json:"url"`
	Children []legacyTocNode `json:"children"`
}

func parseLegacyBooksAndTOCs(raw string, st *pass1Data, report *ImportReport) ([]domain.Book, []domain.BookChapter) {
	var arr []struct {
		ID           string `json:"id"`
		Title        string `json:"title"`
		Description  string `json:"description"`
		CoverURL     string `json:"coverUrl"`
		SourceType   string `json:"sourceType"`
		SourceURL    string `json:"sourceUrl"`
		SeriesID     string `json:"seriesId"`
		EmbedMode    string `json:"embedMode"`
		ChapterCount int    `json:"chapterCount"`
		SyncedAt     int64  `json:"syncedAt"`
	}
	if err := json.Unmarshal([]byte(raw), &arr); err != nil {
		if report != nil {
			report.Warnings = append(report.Warnings, fmt.Sprintf("malformed voloBooks option payload skipped: %v", err))
		}
		return nil, nil
	}
	usedSlugs := make(map[string]bool)
	now := time.Now().UnixMilli()
	var booksOut []domain.Book
	var chapsOut []domain.BookChapter

	for idx, b := range arr {
		bid := firstNonEmpty(strings.TrimSpace(b.ID), id.Next())
		title := strings.TrimSpace(b.Title)
		if title == "" {
			continue
		}
		slug := dedupSlug(slugify(firstNonEmpty(bid, title)), usedSlugs)

		var bookChapters []domain.BookChapter
		if tocRaw, ok := st.reassembleOptionDoc("voloBookToc."+b.ID, report); ok && tocRaw != "" {
			var nodes []legacyTocNode
			if json.Unmarshal([]byte(tocRaw), &nodes) == nil {
				orderSeq := 0
				var walk func(items []legacyTocNode, parentID string, depth int)
				walk = func(items []legacyTocNode, parentID string, depth int) {
					for _, n := range items {
						cid := id.Next()
						bookChapters = append(bookChapters, domain.BookChapter{
							ID:        cid,
							BookID:    bid,
							ParentID:  parentID,
							Title:     n.Title,
							URL:       n.URL,
							Depth:     depth,
							SortOrder: orderSeq,
						})
						orderSeq++
						if len(n.Children) > 0 && depth < 6 {
							walk(n.Children, cid, depth+1)
						}
					}
				}
				walk(nodes, "", 0)
			}
		}

		chCount := b.ChapterCount
		if len(bookChapters) > 0 {
			chCount = len(bookChapters)
		}
		booksOut = append(booksOut, domain.Book{
			ID:           bid,
			Title:        title,
			Slug:         slug,
			Description:  b.Description,
			CoverURL:     b.CoverURL,
			SourceType:   firstNonEmpty(b.SourceType, "auto"),
			SourceURL:    b.SourceURL,
			SeriesID:     b.SeriesID,
			EmbedMode:    firstNonEmpty(b.EmbedMode, "iframe"),
			ChapterCount: chCount,
			SyncedAt:     b.SyncedAt,
			SortOrder:    idx,
			CreatedAt:    now,
			UpdatedAt:    now,
		})
		chapsOut = append(chapsOut, bookChapters...)
	}
	return booksOut, chapsOut
}

func parseLegacyAnalytics(raw string, st *pass1Data) ([]domain.AnalyticsDaily, []domain.AnalyticsTopItem) {
	var doc struct {
		UpdatedAt int64 `json:"updatedAt"`
		Days      map[string]struct {
			PV       int64            `json:"pv"`
			UV       int64            `json:"uv"`
			Articles map[string]int64 `json:"articles"`
			Referers map[string]int64 `json:"referers"`
		} `json:"days"`
	}
	if json.Unmarshal([]byte(raw), &doc) != nil {
		return nil, nil
	}
	ts := doc.UpdatedAt
	if ts <= 0 {
		ts = time.Now().UnixMilli()
	}

	dates := make([]string, 0, len(doc.Days))
	for d := range doc.Days {
		dates = append(dates, d)
	}
	sort.Strings(dates)

	var dailyOut []domain.AnalyticsDaily
	var topOut []domain.AnalyticsTopItem
	for _, d := range dates {
		bucket := doc.Days[d]
		dailyOut = append(dailyOut, domain.AnalyticsDaily{
			StatDate:  d,
			PV:        bucket.PV,
			UV:        bucket.UV,
			UpdatedAt: ts,
		})
		artHits := make(map[string]int64, len(bucket.Articles))
		for aid, hits := range bucket.Articles {
			aid = strings.TrimSpace(aid)
			if aid == "" || hits <= 0 {
				continue
			}
			if st != nil {
				if resolved := st.resolveArticleID(aid); resolved != "" {
					aid = resolved
				}
			}
			artHits[truncRunes(aid, 512)] += hits
		}
		artKeys := make([]string, 0, len(artHits))
		for k := range artHits {
			artKeys = append(artKeys, k)
		}
		sort.Strings(artKeys)
		for _, k := range artKeys {
			topOut = append(topOut, domain.AnalyticsTopItem{StatDate: d, Kind: "article", ItemKey: k, Hits: artHits[k]})
		}

		refHits := make(map[string]int64, len(bucket.Referers))
		for ref, hits := range bucket.Referers {
			ref = strings.TrimSpace(ref)
			if ref == "" || hits <= 0 {
				continue
			}
			refHits[truncRunes(ref, 512)] += hits
		}
		refKeys := make([]string, 0, len(refHits))
		for k := range refHits {
			refKeys = append(refKeys, k)
		}
		sort.Strings(refKeys)
		for _, k := range refKeys {
			topOut = append(topOut, domain.AnalyticsTopItem{StatDate: d, Kind: "referer", ItemKey: k, Hits: refHits[k]})
		}
	}
	return dailyOut, topOut
}

func parseLegacyWebhooks(raw string, kr *config.KeyRing, report *ImportReport) ([]domain.Webhook, error) {
	var arr []struct {
		ID        string   `json:"id"`
		Name      string   `json:"name"`
		Provider  string   `json:"provider"`
		URL       string   `json:"url"`
		Secret    string   `json:"secret"`
		ChatID    string   `json:"chatId"`
		Events    []string `json:"events"`
		Enabled   bool     `json:"enabled"`
		UpdatedAt int64    `json:"updatedAt"`
	}
	if err := json.Unmarshal([]byte(raw), &arr); err != nil {
		if report != nil {
			report.Warnings = append(report.Warnings, fmt.Sprintf("malformed voloWebhooks option payload skipped: %v", err))
		}
		return nil, nil
	}
	var out []domain.Webhook
	now := time.Now().UnixMilli()
	for _, w := range arr {
		if strings.TrimSpace(w.URL) == "" {
			continue
		}
		wid := firstNonEmpty(strings.TrimSpace(w.ID), id.NextPrefixed("wh"))
		sealed, err := kr.SealString(w.Secret, config.WebhookSecretAAD(wid))
		if err != nil {
			return nil, err
		}
		evJSON, _ := json.Marshal(w.Events)
		ts := w.UpdatedAt
		if ts <= 0 {
			ts = now
		}
		out = append(out, domain.Webhook{
			ID:               wid,
			Name:             firstNonEmpty(w.Name, w.Provider),
			Provider:         firstNonEmpty(w.Provider, "custom"),
			URL:              strings.TrimSpace(w.URL),
			SecretCiphertext: sealed,
			ChatID:           w.ChatID,
			EventsJSON:       string(evJSON),
			IsEnabled:        w.Enabled,
			CreatedAt:        ts,
			UpdatedAt:        ts,
		})
	}
	return out, nil
}

func parseLegacyIPBans(raw string) []domain.SecurityIPBan {
	var arr []struct {
		Range       string `json:"range"`
		Source      string `json:"source"`
		ReasonCode  string `json:"reasonCode"`
		Reason      string `json:"reason"`
		Note        string `json:"note"`
		Created     int64  `json:"created"`
		Updated     int64  `json:"updated"`
		ExpiresAt   int64  `json:"expiresAt"`
		Occurrences int    `json:"occurrences"`
	}
	if json.Unmarshal([]byte(raw), &arr) != nil {
		return nil
	}
	seen := make(map[string]bool)
	var out []domain.SecurityIPBan
	now := time.Now().UnixMilli()
	for _, b := range arr {
		r := strings.TrimSpace(b.Range)
		if r == "" || seen[r] {
			continue
		}
		seen[r] = true
		c := b.Created
		if c <= 0 {
			c = now
		}
		u := b.Updated
		if u <= 0 {
			u = c
		}
		occ := b.Occurrences
		if occ < 1 {
			occ = 1
		}
		out = append(out, domain.SecurityIPBan{
			ID:          id.Next(),
			CIDR:        r,
			Source:      firstNonEmpty(b.Source, "manual"),
			ReasonCode:  b.ReasonCode,
			Reason:      b.Reason,
			Note:        b.Note,
			Occurrences: occ,
			ExpiresAt:   b.ExpiresAt,
			CreatedAt:   c,
			UpdatedAt:   u,
		})
	}
	return out
}

func parseLegacyGeoAccess(raw string, sec *domain.SecuritySettings) []domain.SecurityGeoRule {
	var doc struct {
		Version       int    `json:"version"`
		Active        bool   `json:"active"`
		Mode          string `json:"mode"` // legacy v1
		DefaultAction string `json:"defaultAction"`
		Unknown       string `json:"unknown"`
		Countries     string `json:"countries"`
		Regions       string `json:"regions"`
		Rules         []struct {
			Name      string `json:"name"`
			Enabled   bool   `json:"enabled"`
			Action    string `json:"action"`
			OnMiss    string `json:"onMiss"`
			Countries string `json:"countries"`
			Regions   string `json:"regions"`
		} `json:"rules"`
	}
	if json.Unmarshal([]byte(raw), &doc) != nil {
		return nil
	}
	now := time.Now().UnixMilli()
	if doc.Unknown == "deny" || doc.Unknown == "allow" {
		sec.GeoUnknownAction = doc.Unknown
	}
	if doc.Version >= 2 || len(doc.Rules) > 0 {
		sec.GeoPolicyActive = doc.Active
		if doc.DefaultAction == "deny" || doc.DefaultAction == "allow" {
			sec.GeoDefaultAction = doc.DefaultAction
		}
		var out []domain.SecurityGeoRule
		for i, r := range doc.Rules {
			out = append(out, domain.SecurityGeoRule{
				ID:        id.Next(),
				Name:      firstNonEmpty(r.Name, fmt.Sprintf("Rule %d", i+1)),
				IsEnabled: r.Enabled,
				Action:    firstNonEmpty(r.Action, "deny"),
				OnMiss:    firstNonEmpty(r.OnMiss, "next"),
				Countries: r.Countries,
				Regions:   strings.ReplaceAll(r.Regions, `\`, `/`),
				SortOrder: i,
				CreatedAt: now,
				UpdatedAt: now,
			})
		}
		return out
	}
	// Legacy v1 single-mode fallback
	if doc.Mode == "allow" || doc.Mode == "deny" {
		sec.GeoPolicyActive = true
		onMiss := "allow"
		if doc.Mode == "allow" {
			onMiss = "deny"
		}
		return []domain.SecurityGeoRule{{
			ID:        id.Next(),
			Name:      "Migrated Region Policy",
			IsEnabled: true,
			Action:    doc.Mode,
			OnMiss:    onMiss,
			Countries: doc.Countries,
			Regions:   strings.ReplaceAll(doc.Regions, `\`, `/`),
			SortOrder: 0,
			CreatedAt: now,
			UpdatedAt: now,
		}}
	}
	return nil
}

func convertDynamicSocialLinks(raw string) []domain.SocialLink {
	var m map[string]any
	if json.Unmarshal([]byte(raw), &m) != nil {
		return []domain.SocialLink{}
	}
	lowerMap := make(map[string]string, len(m))
	for k, v := range m {
		if s := strings.TrimSpace(strVal(v)); s != "" {
			lowerMap[strings.ToLower(strings.TrimSpace(k))] = s
		}
	}
	type spec struct {
		keys   []string
		label  string
		icon   string
		urlTpl string
	}
	specs := []spec{
		{[]string{"usitegithub", "github"}, "GitHub", "github", "https://github.com/%s"},
		{[]string{"usitetwitter", "usitex", "twitter", "x"}, "X / Twitter", "twitter", "https://x.com/%s"},
		{[]string{"usitetelegram", "telegram"}, "Telegram", "telegram", "https://t.me/%s"},
		{[]string{"usitezhihu", "zhihu"}, "知乎", "zhihu", "https://www.zhihu.com/people/%s"},
		{[]string{"usiteweibo", "weibo"}, "微博", "weibo", "https://weibo.com/%s"},
		{[]string{"usitelinkedin", "linkedin"}, "LinkedIn", "linkedin", "https://www.linkedin.com/in/%s"},
		{[]string{"usitestackoverflow", "stackoverflow"}, "Stack Overflow", "stackoverflow", "https://stackoverflow.com/users/%s"},
		{[]string{"usitemedium", "medium"}, "Medium", "medium", "https://medium.com/@%s"},
		{[]string{"usiteinstagram", "instagram"}, "Instagram", "instagram", "https://www.instagram.com/%s"},
		{[]string{"usitecodepen", "codepen"}, "CodePen", "codepen", "https://codepen.io/%s"},
		{[]string{"usitedribbble", "dribbble"}, "Dribbble", "dribbble", "https://dribbble.com/%s"},
		{[]string{"usitebehance", "behance"}, "Behance", "behance", "https://www.behance.net/%s"},
		{[]string{"usitesteam", "steam"}, "Steam", "steam", "https://steamcommunity.com/id/%s"},
	}
	var out []domain.SocialLink
	for _, sp := range specs {
		var v string
		for _, alias := range sp.keys {
			if found := lowerMap[alias]; found != "" {
				v = found
				break
			}
		}
		if v == "" {
			continue
		}
		u := v
		lowV := strings.ToLower(v)
		if !strings.HasPrefix(lowV, "http://") && !strings.HasPrefix(lowV, "https://") && !strings.Contains(v, ":") && !strings.HasPrefix(v, "/") {
			u = fmt.Sprintf(sp.urlTpl, strings.TrimPrefix(v, "@"))
		}
		out = append(out, domain.SocialLink{
			ID:    sp.icon,
			Label: sp.label,
			Icon:  sp.icon,
			URL:   u,
		})
	}
	return out
}

func convertLegacySignsToSafeTemplate(raw string) string {
	var signs []struct {
		OID      any    `json:"oId"`
		SignHTML string `json:"signHTML"`
	}
	if json.Unmarshal([]byte(raw), &signs) != nil {
		return ""
	}
	for _, s := range signs {
		if fmt.Sprint(s.OID) == "0" || strings.TrimSpace(s.SignHTML) == "" {
			continue
		}
		txt := strings.ReplaceAll(s.SignHTML, "<br>", " · ")
		txt = strings.ReplaceAll(txt, "<br/>", " · ")
		txt = htmlTagStripRe.ReplaceAllString(txt, "")
		txt = strings.ReplaceAll(txt, "{blog}", "{title}")
		txt = strings.TrimSpace(txt)
		if txt != "" {
			return txt
		}
	}
	return ""
}

func parseLegacyStorageConfig(raw string, st *domain.StorageSettings, secretOut *string, report *ImportReport) {
	// Split legacy multi-char delimiter without storing or emitting it anywhere in Volo-go
	delim := string([]byte{'<', '<', '>', '>'})
	parts := strings.Split(raw, delim)
	provider := strings.ToLower(strings.TrimSpace(parts[0]))

	switch provider {
	case "", "local":
		st.ActiveDriver = "local"
	case "s3":
		if len(parts) >= 9 {
			st.ActiveDriver = "s3"
			st.S3Endpoint = strings.TrimSpace(parts[1])
			st.S3AccessKey = strings.TrimSpace(parts[2])
			*secretOut = strings.TrimSpace(parts[3])
			st.S3Bucket = strings.TrimSpace(parts[4])
			st.S3Region = firstNonEmpty(strings.TrimSpace(parts[5]), "auto")
			st.S3PublicBaseURL = strings.TrimSpace(parts[6])
			st.S3KeyPrefix = strings.Trim(strings.TrimSpace(parts[7]), "/")
			st.S3PathStyle = parseLegacyBool(parts[8], false)
		}
	default:
		st.ActiveDriver = "local"
		report.Warnings = append(report.Warnings,
			fmt.Sprintf("legacy storage provider %q retired -> reset active storage driver to 'local' (configure unified S3 in Admin Storage if needed)", provider))
	}
}

func parseLegacySMTP(getOpt func(string) string, nt *domain.NotificationsSettings, secretOut *string) {
	host := getOpt("smtpHost")
	port := parseIntBound(getOpt("smtpPort"), 1, 65535)
	sec := strings.ToLower(getOpt("smtpSecurity"))

	if host == "" {
		switch strings.ToUpper(getOpt("mailBox")) {
		case "QQ":
			host, port, sec = "smtp.qq.com", 465, "tls"
		case "QQ_ENT":
			host, port, sec = "smtp.exmail.qq.com", 465, "tls"
		case "163":
			host, port, sec = "smtp.163.com", 465, "tls"
		}
	}
	user := getOpt("mailUsername")
	pass := getOpt("mailPassword")
	from := firstNonEmpty(getOpt("smtpFrom"), user)

	if host != "" && user != "" {
		nt.SMTPEnabled = true
		nt.SMTPHost = host
		if port > 0 {
			nt.SMTPPort = port
		}
		switch sec {
		case "ssl", "tls":
			nt.SMTPSecurity = "tls"
		case "starttls":
			nt.SMTPSecurity = "starttls"
		case "none":
			nt.SMTPSecurity = "plain_internal"
		}
		nt.SMTPAuth = parseLegacyBool(getOpt("smtpAuth"), true)
		nt.SMTPUsername = user
		nt.SMTPFromEmail = from
		*secretOut = pass
	}
	if v := getOpt("adminActiveSentToMailbox"); v != "" {
		nt.NotifyAdminOnComment = parseLegacyBool(v, true)
	}
}

func parseLegacyRemoteBackup(raw string, bk *domain.BackupSettings, sec *domain.BackupSecrets) {
	var doc struct {
		Enabled        bool   `json:"enabled"`
		Protocol       string `json:"protocol"`
		WebDAVURL      string `json:"webdavUrl"`
		WebDAVUser     string `json:"webdavUser"`
		WebDAVPassword string `json:"webdavPassword"`
		RemoteDir      string `json:"remoteDir"`
		S3Endpoint     string `json:"s3Endpoint"`
		S3AccessKey    string `json:"s3AccessKey"`
		S3SecretKey    string `json:"s3SecretKey"`
		S3Bucket       string `json:"s3Bucket"`
		S3Region       string `json:"s3Region"`
		S3Prefix       string `json:"s3Prefix"`
		S3PathStyle    bool   `json:"s3PathStyle"`
	}
	if json.Unmarshal([]byte(raw), &doc) != nil {
		return
	}
	bk.RemoteEnabled = doc.Enabled
	if doc.Protocol == "webdav" || doc.Protocol == "s3" {
		bk.RemoteProtocol = doc.Protocol
	}
	bk.WebDAVURL = doc.WebDAVURL
	bk.WebDAVUser = doc.WebDAVUser
	bk.WebDAVDir = firstNonEmpty(doc.RemoteDir, "volo-backups")
	bk.S3Endpoint = doc.S3Endpoint
	bk.S3AccessKey = doc.S3AccessKey
	bk.S3Bucket = doc.S3Bucket
	bk.S3Region = firstNonEmpty(doc.S3Region, "auto")
	bk.S3Prefix = firstNonEmpty(doc.S3Prefix, "volo-backups")
	bk.S3PathStyle = doc.S3PathStyle
	sec.WebDAVPassword = doc.WebDAVPassword
	sec.S3SecretKey = doc.S3SecretKey
}

func parseLegacyGitHubRepos(ghRaw string, dispRaw string, seo *domain.SEOSettings) {
	hiddenSet := make(map[string]bool)
	if dispRaw != "" {
		var disp struct {
			IncludeForks bool     `json:"includeForks"`
			Hidden       []string `json:"hidden"`
		}
		if json.Unmarshal([]byte(dispRaw), &disp) == nil {
			seo.GitHubIncludeForks = disp.IncludeForks
			for _, h := range disp.Hidden {
				if k := strings.ToLower(strings.TrimSpace(h)); k != "" {
					hiddenSet[k] = true
				}
			}
		}
	}

	type rawRepo struct {
		Name        string `json:"githubrepoName"`
		FullName    string `json:"githubrepoFullName"`
		Description string `json:"githubrepoDescription"`
		HTMLURL     string `json:"githubrepoHTMLURL"`
		Homepage    string `json:"githubrepoHomepage"`
		Language    string `json:"githubrepoLanguage"`
		Stars       int    `json:"githubrepoStargazersCount"`
		Forks       int    `json:"githubrepoForksCount"`
		Fork        bool   `json:"githubrepoFork"`
		Updated     any    `json:"githubrepoUpdated"`
	}

	var list []rawRepo
	trimmed := strings.TrimSpace(ghRaw)
	if strings.HasPrefix(trimmed, "[") {
		_ = json.Unmarshal([]byte(trimmed), &list)
	} else {
		var wrapper struct {
			Username     string    `json:"username"`
			UpdatedAt    int64     `json:"updatedAt"`
			Repositories []rawRepo `json:"repositories"`
		}
		if json.Unmarshal([]byte(trimmed), &wrapper) == nil {
			if wrapper.Username != "" && seo.GitHubUsername == "" {
				seo.GitHubUsername = wrapper.Username
			}
			seo.GitHubSyncedAt = wrapper.UpdatedAt
			list = wrapper.Repositories
		}
	}

	out := make([]domain.GitHubRepoItem, 0, len(list))
	for _, r := range list {
		if r.Name == "" {
			continue
		}
		fullLower := strings.ToLower(strings.TrimSpace(r.FullName))
		nameLower := strings.ToLower(strings.TrimSpace(r.Name))
		out = append(out, domain.GitHubRepoItem{
			Name:        r.Name,
			FullName:    r.FullName,
			Description: r.Description,
			HTMLURL:     r.HTMLURL,
			Homepage:    r.Homepage,
			Language:    r.Language,
			Stars:       r.Stars,
			Forks:       r.Forks,
			IsFork:      r.Fork,
			Hidden:      hiddenSet[fullLower] || hiddenSet[nameLower],
			UpdatedAt:   strVal(r.Updated),
		})
	}
	seo.GitHubRepos = out
}

// --- Low-level streaming & normalization utilities ---

func streamJSONArray(dec *json.Decoder, onElem func(map[string]any) error) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if tok == nil {
		return nil
	}
	if tok != json.Delim('[') {
		return fmt.Errorf("etl: expected '[' token, got %v", tok)
	}
	for dec.More() {
		var row map[string]any
		if err := dec.Decode(&row); err != nil {
			return err
		}
		if err := onElem(row); err != nil {
			return err
		}
	}
	_, err = dec.Token() // consume ']'
	return err
}

func skipJSONValue(dec *json.Decoder) error {
	var discard json.RawMessage
	return dec.Decode(&discard)
}

func extractLegacyPasswordHash(raw map[string]any, role string) string {
	pbkdf2 := strings.TrimSpace(strVal(raw["userPasswordHash"]))
	if strings.HasPrefix(pbkdf2, "pbkdf2$sha256$") {
		return pbkdf2
	}
	// Legacy double-MD5 stored in userB3Key (only when userPasswordHash is empty;
	// note that for visitorRole with userPasswordHash present, userB3Key was a random session key).
	b3k := strings.TrimSpace(strVal(raw["userB3Key"]))
	if pbkdf2 == "" && len(b3k) >= 28 && len(b3k) <= 32 && isHexString(b3k) && role != domain.RoleReader {
		return "bolomd5x2$" + b3k
	}
	return ""
}

func isHexString(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return len(s) > 0
}

func mapLegacyRole(r string) string {
	switch strings.TrimSpace(r) {
	case "adminRole":
		return domain.RoleAdmin
	case "defaultRole":
		return domain.RoleEditor
	default:
		return domain.RoleReader
	}
}

func normalizeLegacyPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return p
}

func deriveSlugFromPermalinkOrTitle(permalink string, title string, fallbackID string) string {
	if permalink != "" {
		base := path.Base(permalink)
		base = strings.TrimSuffix(base, ".html")
		base = strings.TrimSuffix(base, ".htm")
		if s := slugify(base); s != "" && s != "index" {
			return s
		}
	}
	if s := slugify(title); s != "" {
		return s
	}
	return fallbackID
}

func slugify(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	s = strings.ReplaceAll(s, "_", "-")
	s = strings.ReplaceAll(s, " ", "-")
	s = nonSlugCharsRe.ReplaceAllString(s, "-")
	s = multiDashRe.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if utf8.RuneCountInString(s) > 80 {
		runes := []rune(s)
		s = strings.Trim(string(runes[:80]), "-")
	}
	return s
}

func dedupSlug(base string, used map[string]bool) string {
	if base == "" {
		base = id.Next()
	}
	candidate := base
	for i := 2; used[candidate]; i++ {
		candidate = fmt.Sprintf("%s-%d", base, i)
	}
	used[candidate] = true
	return candidate
}

func dedupString(base string, used map[string]bool) string {
	candidate := base
	for i := 2; used[strings.ToLower(candidate)]; i++ {
		candidate = fmt.Sprintf("%s_%d", base, i)
	}
	used[strings.ToLower(candidate)] = true
	return candidate
}

func dedupEmail(base string, used map[string]bool) string {
	candidate := base
	at := strings.IndexByte(base, '@')
	for i := 2; used[candidate]; i++ {
		if at > 0 {
			candidate = fmt.Sprintf("%s+%d%s", base[:at], i, base[at:])
		} else {
			candidate = fmt.Sprintf("user_%d@local.volo", i)
		}
	}
	used[candidate] = true
	return candidate
}

func stripETLBlockquoteAndIndent(line string) (rest string, ok bool) {
	s := strings.TrimRight(line, "\r")
	for {
		spaces := 0
		i := 0
		for i < len(s) && spaces < 3 {
			if s[i] == ' ' {
				spaces++
				i++
			} else {
				break
			}
		}
		if i < len(s) && s[i] == '>' {
			s = s[i+1:]
			if len(s) > 0 && s[0] == ' ' {
				s = s[1:]
			} else if len(s) > 0 && s[0] == '\t' {
				s = s[1:]
			}
			continue
		}
		if i < len(s) && (s[i] == ' ' || s[i] == '\t') {
			return "", false
		}
		return s[i:], true
	}
}

func parseETLFenceOpen(rest string) (ch byte, length int, ok bool) {
	if len(rest) < 3 || (rest[0] != '`' && rest[0] != '~') {
		return 0, 0, false
	}
	c := rest[0]
	n := 0
	for n < len(rest) && rest[n] == c {
		n++
	}
	if n < 3 {
		return 0, 0, false
	}
	if c == '`' && strings.IndexByte(rest[n:], '`') >= 0 {
		return 0, 0, false
	}
	return c, n, true
}

func isETLFenceClose(rest string, ch byte, minLen int) bool {
	if len(rest) < minLen || rest[0] != ch {
		return false
	}
	n := 0
	for n < len(rest) && rest[n] == ch {
		n++
	}
	if n < minLen {
		return false
	}
	return strings.TrimRight(rest[n:], " \t") == ""
}

func isETLDirectiveOpen(rest string) (name string, ok bool) {
	if !strings.HasPrefix(rest, ":::") {
		return "", false
	}
	tail := strings.TrimSpace(rest[3:])
	if tail == "" {
		return "", false
	}
	fields := strings.Fields(tail)
	return strings.ToLower(fields[0]), true
}

func isETLDirectiveClose(rest string) bool {
	return strings.HasPrefix(rest, ":::") && strings.TrimRight(rest[3:], " \t") == ""
}

func containsLockDirectiveETL(md string) bool {
	var fenceChar byte
	var fenceLen int
	for _, line := range strings.Split(md, "\n") {
		rest, ok := stripETLBlockquoteAndIndent(line)
		if !ok {
			continue
		}
		if fenceChar != 0 {
			if isETLFenceClose(rest, fenceChar, fenceLen) {
				fenceChar = 0
				fenceLen = 0
			}
			continue
		}
		if fc, fl, isFence := parseETLFenceOpen(rest); isFence {
			fenceChar = fc
			fenceLen = fl
			continue
		}
		if name, isDir := isETLDirectiveOpen(rest); isDir && name == "lock" {
			return true
		}
	}
	return false
}

func sanitizeLegacySiteURL(raw string, allowMailto bool) (string, bool) {
	cleaned := strings.TrimSpace(repo.StripASCIIControls(raw))
	if cleaned == "" {
		return "", true
	}
	if strings.HasPrefix(cleaned, "//") && len(cleaned) > 2 && cleaned[2] != '/' {
		cleaned = "https:" + cleaned
	} else if !strings.Contains(cleaned, ":") && !strings.HasPrefix(cleaned, "/") && !strings.HasPrefix(cleaned, "#") {
		cleaned = "/" + cleaned
	}
	return repo.SanitizeAllowedURL(cleaned, allowMailto)
}

func estimateReadingStats(md string) (words int, minutes int) {
	inLock := false
	lockNestDepth := 0
	var fenceChar byte
	var fenceLen int
	for _, line := range strings.Split(md, "\n") {
		if rest, ok := stripETLBlockquoteAndIndent(line); ok {
			if fenceChar != 0 {
				if isETLFenceClose(rest, fenceChar, fenceLen) {
					fenceChar = 0
					fenceLen = 0
				}
				if inLock {
					continue
				}
			} else if fc, fl, isFence := parseETLFenceOpen(rest); isFence {
				fenceChar = fc
				fenceLen = fl
				if inLock {
					continue
				}
			} else if inLock {
				if isETLDirectiveClose(rest) {
					if lockNestDepth > 0 {
						lockNestDepth--
					} else {
						inLock = false
					}
				} else if _, isDir := isETLDirectiveOpen(rest); isDir {
					lockNestDepth++
				}
				continue
			} else if name, isDir := isETLDirectiveOpen(rest); isDir && name == "lock" {
				inLock = true
				lockNestDepth = 0
				continue
			}
		} else if inLock {
			continue
		}

		inWord := false
		for _, r := range line {
			if unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r) {
				words++
				inWord = false
			} else if unicode.IsLetter(r) || unicode.IsDigit(r) {
				if !inWord {
					words++
					inWord = true
				}
			} else {
				inWord = false
			}
		}
	}
	if words == 0 {
		return 0, 1
	}
	minutes = (words + 349) / 350
	if minutes < 1 {
		minutes = 1
	}
	return words, minutes
}

func inferAudioMIME(u string) string {
	ext := strings.ToLower(filepath.Ext(strings.Split(u, "?")[0]))
	switch ext {
	case ".m4a", ".mp4":
		return "audio/x-m4a"
	case ".ogg", ".oga":
		return "audio/ogg"
	case ".opus":
		return "audio/opus"
	case ".wav":
		return "audio/wav"
	case ".aac":
		return "audio/aac"
	case ".flac":
		return "audio/flac"
	default:
		return "audio/mpeg"
	}
}

func sanitizeLegacyAvatar(u string) string {
	u = strings.TrimSpace(u)
	if u == "" || isLegacyExternalAsset(u) {
		return ""
	}
	return u
}

var retiredAvatarRootDomainDigests = map[string]bool{
	"75ac1de44ecc6259": true,
	"385876a68eae0c8e": true,
	"7ac681a01a48c4dd": true,
	"5e3976e195f5e50c": true,
	"1943155e296d86ff": true,
	"4789dac9eabfc69d": true,
}

func isLegacyExternalAsset(u string) bool {
	parsed, err := url.Parse(strings.TrimSpace(u))
	if err != nil {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	parts := strings.Split(host, ".")
	if len(parts) < 2 {
		return false
	}
	root := parts[len(parts)-2] + "." + parts[len(parts)-1]
	sum := sha256.Sum256([]byte(root))
	return retiredAvatarRootDomainDigests[hex.EncodeToString(sum[:8])]
}

func maskIPAddress(ipStr string) string {
	addr, err := netip.ParseAddr(strings.TrimSpace(ipStr))
	if err != nil {
		return ""
	}
	addr = addr.Unmap()
	if addr.Is4() {
		b := addr.As4()
		return fmt.Sprintf("%d.%d.*.*", b[0], b[1])
	}
	b := addr.As16()
	return fmt.Sprintf("%x:%x:*:*:*:*:*:*", uint16(b[0])<<8|uint16(b[1]), uint16(b[2])<<8|uint16(b[3]))
}

func parseLegacyBool(raw string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "true", "on", "1", "yes":
		return true
	case "false", "off", "0", "no":
		return false
	default:
		return def
	}
}

func parsePipeKeyValues(raw string) map[string]string {
	out := make(map[string]string)
	for _, pair := range strings.Split(raw, "|") {
		if eq := strings.IndexByte(pair, '='); eq > 0 {
			k := strings.TrimSpace(pair[:eq])
			v := strings.TrimSpace(pair[eq+1:])
			if k != "" {
				out[k] = v
			}
		}
	}
	return out
}

func splitWordList(raw string) []string {
	replacer := strings.NewReplacer("，", ",", "\r\n", ",", "\n", ",", "\r", ",")
	parts := strings.Split(replacer.Replace(raw), ",")
	var out []string
	seen := make(map[string]bool)
	for _, p := range parts {
		w := strings.TrimSpace(p)
		if w != "" && !seen[w] {
			seen[w] = true
			out = append(out, w)
		}
	}
	return out
}

func isValidAccent(a string) bool {
	switch a {
	case "forest", "ocean", "teal", "amber", "plum", "rose":
		return true
	default:
		return false
	}
}

func parseIntBound(raw string, min int, max int) int {
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return 0
	}
	if n < min {
		return min
	}
	if n > max {
		return max
	}
	return n
}

func parseMillisFallback(oid string) int64 {
	if n, err := strconv.ParseInt(strings.TrimSpace(oid), 10, 64); err == nil && n > 1_000_000_000_000 {
		return n
	}
	return time.Now().UnixMilli()
}

func strVal(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return cleanUTF8(x)
	case json.Number:
		return x.String()
	case bool:
		if x {
			return "true"
		}
		return "false"
	default:
		return cleanUTF8(fmt.Sprint(x))
	}
}

func int64Val(v any) int64 {
	switch x := v.(type) {
	case nil:
		return 0
	case json.Number:
		if n, err := x.Int64(); err == nil {
			return n
		}
		if f, err := x.Float64(); err == nil {
			return int64(f)
		}
		return 0
	case float64:
		return int64(x)
	case int64:
		return x
	case int:
		return int64(x)
	case string:
		n, _ := strconv.ParseInt(strings.TrimSpace(x), 10, 64)
		return n
	default:
		return 0
	}
}

func intVal(v any) int {
	n := int64Val(v)
	if n < -2147483648 {
		return -2147483648
	}
	if n > 2147483647 {
		return 2147483647
	}
	return int(n)
}

func boolVal(v any) bool { return boolValDefault(v, false) }

func boolValDefault(v any, def bool) bool {
	switch x := v.(type) {
	case nil:
		return def
	case bool:
		return x
	case json.Number:
		return x.String() != "0"
	case string:
		return parseLegacyBool(x, def)
	default:
		return def
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func structToMap(v any) map[string]any {
	b, _ := json.Marshal(v)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	if m == nil {
		m = make(map[string]any)
	}
	return m
}

func isValidLegacyIndexNowKey(s string) bool {
	if len(s) < 8 || len(s) > 128 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-') {
			return false
		}
	}
	return true
}

var (
	legacyInjScriptBlockRe = regexp.MustCompile(`(?is)<script\b[^>]*>.*?</script\s*>`)
	legacyInjScriptTagRe   = regexp.MustCompile(`(?is)</?script\b[^>]*>`)
	legacyInjFrameBlockRe  = regexp.MustCompile(`(?is)<(?:iframe|object|embed|applet)\b[^>]*>.*?</(?:iframe|object|embed|applet)\s*>`)
	legacyInjFrameTagRe    = regexp.MustCompile(`(?is)</?(?:iframe|object|embed|applet)\b[^>]*>`)
	legacyInjEventAttrRe   = regexp.MustCompile(`(?is)\s+\bon[a-z0-9_-]+\s*=\s*(?:"[^"]*"|'[^']*'|[^\s>"']*)`)
	legacyInjJSProtoAttrRe = regexp.MustCompile(`(?is)\s+\b(?:href|src|action|formaction|data)\s*=\s*(?:"\s*(?:javascript|vbscript|data\s*:\s*text/html)[^"]*"|'\s*(?:javascript|vbscript|data\s*:\s*text/html)[^']*'|(?:\s*(?:javascript|vbscript|data\s*:\s*text/html)[^\s>"']*))`)
)

func sanitizeLegacySiteInjectionHTML(raw string) string {
	cleaned := repo.NormalizeSiteInjectionHTML(raw)
	for {
		prev := cleaned
		cleaned = legacyInjScriptBlockRe.ReplaceAllString(cleaned, "")
		cleaned = legacyInjScriptTagRe.ReplaceAllString(cleaned, "")
		cleaned = legacyInjFrameBlockRe.ReplaceAllString(cleaned, "")
		cleaned = legacyInjFrameTagRe.ReplaceAllString(cleaned, "")
		cleaned = legacyInjEventAttrRe.ReplaceAllString(cleaned, "")
		cleaned = legacyInjJSProtoAttrRe.ReplaceAllString(cleaned, "")
		cleaned = repo.NormalizeSiteInjectionHTML(cleaned)
		if cleaned == prev {
			break
		}
	}
	return cleaned
}
