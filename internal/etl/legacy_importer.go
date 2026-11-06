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

// ImportOptions configures streaming legacy bolo-solo archive ETL.
type ImportOptions struct {
	DryRun          bool
	LegacyMediaDir  string // Optional path to legacy ./image directory to register/copy
	TargetUploadDir string
}

// ImportReport summarizes everything migrated, normalized, and any multi-category conflicts or warnings.
type ImportReport struct {
	SourceVersion     string   `json:"source_version"`
	DryRun            bool     `json:"dry_run"`
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
// and writing normalized first-class records inside a rollback-safe transaction.
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

	// -------------------------------------------------------------------------
	// Pass 1: Stream metadata, relational index maps, and chunked option rows
	// -------------------------------------------------------------------------
	pass1State, err := runPass1(ctx, openStream, report)
	if err != nil {
		return nil, err
	}

	// Build normalized settings & chunk-derived entities in memory (small metadata only)
	decomposed, err := decomposeOptions(pass1State, settingsMgr.Snapshot(), keyRing, report)
	if err != nil {
		return nil, err
	}

	if opts.DryRun {
		if err := countPass2Entities(ctx, openStream, pass1State, report); err != nil {
			return nil, err
		}
		return report, nil
	}

	existingAdmins, _ := sqlx.Select[domain.User](ctx, store,
		`SELECT * FROM users WHERE role = ?`, domain.RoleAdmin)

	// -------------------------------------------------------------------------
	// Pass 2: Stream articles, revisions, comments, pages->nav_items, links, follows
	// directly into normalized tables inside an atomic transaction!
	// -------------------------------------------------------------------------
	err = store.WithTx(ctx, func(tx *sqlx.Tx) error {
		if err := insertPass1EntitiesTx(ctx, tx, pass1State, decomposed, existingAdmins, report); err != nil {
			return err
		}
		return streamPass2IntoTx(ctx, tx, openStream, pass1State, keyRing, report)
	})
	if err != nil {
		return nil, err
	}

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
	catMeta             map[string]legacyCategoryMeta
	tags                []domain.Tag
	tagIDSet            map[string]bool
	tagByName           map[string]string
	catTagMap           map[string][]string // tag_oId -> []category_oId
	artTagsMap          map[string][]string // article_oId -> []tag_oId
	articlePrimaryCat   map[string]string   // article_oId -> derived primary category_id
	commentMailFallback map[string]string   // comment_oId -> email (from legacy mailUserContext)
	linkHealth          map[string]legacyLinkHealthItem
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

func runPass1(ctx context.Context, openStream func() (io.ReadCloser, error), report *ImportReport) (*pass1Data, error) {
	rc, err := openStream()
	if err != nil {
		return nil, err
	}
	defer rc.Close()

	st := &pass1Data{
		options:             make(map[string]legacyOptionRow),
		chunks:              make(map[string]string),
		userIDMap:           make(map[string]string),
		catMeta:             make(map[string]legacyCategoryMeta),
		tagIDSet:            make(map[string]bool),
		tagByName:           make(map[string]string),
		catTagMap:           make(map[string][]string),
		artTagsMap:          make(map[string][]string),
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
	usedTagSlugs := make(map[string]bool)
	usedUsernames := make(map[string]bool)
	usedEmails := make(map[string]bool)

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
				origID := strVal(raw["oId"])
				if origID == "" {
					return nil
				}
				mappedID := origID
				if origID == "default" || len(origID) > 32 {
					mappedID = defaultMappedAdminID
					if _, taken := st.userIDMap[origID]; !taken {
						st.userIDMap[origID] = mappedID
					}
				} else {
					st.userIDMap[origID] = mappedID
				}

				uname := strings.TrimSpace(strVal(raw["userName"]))
				if uname == "" {
					uname = "user_" + mappedID
				}
				uname = dedupString(uname, usedUsernames)

				email := strings.ToLower(strings.TrimSpace(strVal(raw["userEmail"])))
				if email == "" {
					email = strings.ToLower(uname) + "@local.volo"
				}
				email = dedupEmail(email, usedEmails)

				role := mapLegacyRole(strVal(raw["userRole"]))
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
				cid := strVal(raw["oId"])
				title := strings.TrimSpace(strVal(raw["categoryTitle"]))
				if cid == "" || title == "" {
					return nil
				}
				uri := strings.Trim(strings.TrimSpace(strVal(raw["categoryURI"])), "/")
				slug := dedupSlug(slugify(firstNonEmpty(uri, title, cid)), usedCatSlugs)
				order := int(int64Val(raw["categoryOrder"]))
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
				tid := strVal(raw["oId"])
				title := strings.TrimSpace(strVal(raw["tagTitle"]))
				if tid == "" || title == "" {
					return nil
				}
				if _, dup := st.tagByName[strings.ToLower(title)]; dup {
					return nil
				}
				slug := dedupSlug(slugify(firstNonEmpty(title, tid)), usedTagSlugs)
				st.tags = append(st.tags, domain.Tag{
					ID:        tid,
					Name:      title,
					Slug:      slug,
					CreatedAt: parseMillisFallback(tid),
				})
				st.tagIDSet[tid] = true
				st.tagByName[strings.ToLower(title)] = tid
				return nil
			}); err != nil {
				return nil, err
			}

		case "category_tag", "category_tags":
			if err := streamJSONArray(dec, func(raw map[string]any) error {
				cid := strVal(raw["category_oId"])
				tid := strVal(raw["tag_oId"])
				if cid != "" && tid != "" {
					st.catTagMap[tid] = append(st.catTagMap[tid], cid)
				}
				return nil
			}); err != nil {
				return nil, err
			}

		case "tag_articles", "tag_article":
			if err := streamJSONArray(dec, func(raw map[string]any) error {
				aid := strVal(raw["article_oId"])
				tid := strVal(raw["tag_oId"])
				if aid != "" && tid != "" {
					st.artTagsMap[aid] = append(st.artTagsMap[aid], tid)
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

	if st.defaultAdminID == "" && len(st.users) > 0 {
		st.defaultAdminID = st.users[0].ID
	}
	if st.defaultAdminID == "" {
		st.defaultAdminID = defaultMappedAdminID
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
// in chunk index order 0..count-1 (immune to lexicographical ".10" before ".2" ordering!).
func (st *pass1Data) reassembleOptionDoc(rootID string) (string, bool, error) {
	root, ok := st.options[rootID]
	if !ok {
		return "", false, nil
	}
	val := root.Value
	if !strings.HasPrefix(val, chunkManifestPrefix) {
		return val, true, nil
	}
	var manifest struct {
		Storage    string `json:"storage"`
		Generation string `json:"generation"`
		Count      int    `json:"count"`
	}
	if err := json.Unmarshal([]byte(val), &manifest); err != nil {
		return "", false, fmt.Errorf("etl: parse chunk manifest for %q: %w", rootID, err)
	}
	if manifest.Storage != "chunks" || !hexGenRe.MatchString(manifest.Generation) || manifest.Count < 1 || manifest.Count > 256 {
		return "", false, fmt.Errorf("etl: invalid chunk manifest for %q", rootID)
	}
	var sb strings.Builder
	for i := 0; i < manifest.Count; i++ {
		chunkKey := fmt.Sprintf("%s.%s.%d", rootID, manifest.Generation, i)
		part, exists := st.chunks[chunkKey]
		if !exists {
			return "", false, fmt.Errorf("etl: missing chunk %q while reassembling %q", chunkKey, rootID)
		}
		sb.WriteString(part)
	}
	return sb.String(), true, nil
}

type decomposedState struct {
	settings           domain.SettingsSnapshot
	storageSecret      string
	smtpSecret         string
	backupSecrets      domain.BackupSecrets
	series             []domain.Series
	seriesArticles     []domain.SeriesArticle
	books              []domain.Book
	bookChapters       []domain.BookChapter
	analyticsDaily     []domain.AnalyticsDaily
	analyticsTop       []domain.AnalyticsTopItem
	webhooks           []domain.Webhook
	ipBans             []domain.SecurityIPBan
	geoRules           []domain.SecurityGeoRule
}

func decomposeOptions(st *pass1Data, base domain.SettingsSnapshot, kr *config.KeyRing, report *ImportReport) (*decomposedState, error) {
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
		out.settings.Site.NoticeBoard = v
	}
	if v := getOpt("footerContent"); v != "" {
		out.settings.Site.FooterText = v
	}
	if v := getOpt("htmlHead"); v != "" {
		out.settings.Site.CustomHeadMeta = v
	}
	if v := getOpt("faviconURL"); v != "" && !isLegacyExternalAsset(v) {
		out.settings.Site.FaviconURL = v
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
	if usiteRaw, ok, err := st.reassembleOptionDoc("usite"); err == nil && ok && usiteRaw != "" {
		out.settings.Site.SocialLinks = convertDynamicSocialLinks(usiteRaw)
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
	if v := getOpt("indexNowKey"); v != "" {
		out.settings.SEO.IndexNowKey = v
	}
	if v := getOpt("myGitHubID"); v != "" {
		out.settings.SEO.GitHubUsername = v
	}
	if ghRaw, ok, err := st.reassembleOptionDoc("githubRepos"); err == nil && ok && ghRaw != "" {
		dispRaw, _, _ := st.reassembleOptionDoc("githubRepositoryDisplay")
		parseLegacyGitHubRepos(ghRaw, dispRaw, &out.settings.SEO)
	}

	// 4. Storage settings (decomposing legacy delimited tuChuangConfig)
	if tc := getOpt("tuChuangConfig"); tc != "" {
		parseLegacyStorageConfig(tc, &out.settings.Storage, &out.storageSecret, report)
	}
	if v := getOpt("imageWebpAdaptive"); v != "" {
		out.settings.Storage.AdaptiveWebP = parseLegacyBool(v, true)
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
	if bansRaw, ok, err := st.reassembleOptionDoc("voloIpBans"); err == nil && ok && bansRaw != "" {
		out.ipBans = parseLegacyIPBans(bansRaw)
	}
	if geoRaw, ok, err := st.reassembleOptionDoc("voloGeoAccess"); err == nil && ok && geoRaw != "" {
		out.geoRules = parseLegacyGeoAccess(geoRaw, &out.settings.Security)
	}

	// 7. Local & Remote Backup settings
	if v := getOpt("localBackupAuto"); v != "" {
		out.settings.Backup.LocalAutoEnabled = parseLegacyBool(v, false)
	}
	if n := parseIntBound(getOpt("localBackupKeepCount"), 1, 365); n > 0 {
		out.settings.Backup.LocalKeepCount = n
	}
	if rbRaw, ok, err := st.reassembleOptionDoc("voloRemoteBackup"); err == nil && ok && rbRaw != "" {
		parseLegacyRemoteBackup(rbRaw, &out.settings.Backup, &out.backupSecrets)
	}

	// 8. Friend Link Health Cache ("voloLinkHealth")
	if lhRaw, ok, err := st.reassembleOptionDoc("voloLinkHealth"); err == nil && ok && lhRaw != "" {
		_ = json.Unmarshal([]byte(lhRaw), &st.linkHealth)
	}

	// 9. Chunked Option Payloads -> First-Class Normalized Tables!
	// 9a. voloSeries -> series & series_articles
	if serRaw, ok, err := st.reassembleOptionDoc("voloSeries"); err != nil {
		return nil, err
	} else if ok && serRaw != "" {
		out.series, out.seriesArticles = parseLegacySeries(serRaw)
		report.Series = len(out.series)
	}

	// 9b. voloBooks + voloBookToc.<id> -> books & book_chapters
	if booksRaw, ok, err := st.reassembleOptionDoc("voloBooks"); err != nil {
		return nil, err
	} else if ok && booksRaw != "" {
		books, chapters, bErr := parseLegacyBooksAndTOCs(booksRaw, st)
		if bErr != nil {
			return nil, bErr
		}
		out.books = books
		out.bookChapters = chapters
		report.Books = len(books)
		report.BookChapters = len(chapters)
	}

	// 9c. voloDailyStats -> analytics_daily & analytics_top_items
	if statsRaw, ok, err := st.reassembleOptionDoc("voloDailyStats"); err != nil {
		return nil, err
	} else if ok && statsRaw != "" {
		out.analyticsDaily, out.analyticsTop = parseLegacyAnalytics(statsRaw)
		report.AnalyticsDays = len(out.analyticsDaily)
	}

	// 9d. voloWebhooks -> webhooks (with AES-256-GCM secret encryption)
	if whRaw, ok, err := st.reassembleOptionDoc("voloWebhooks"); err != nil {
		return nil, err
	} else if ok && whRaw != "" {
		whs, wErr := parseLegacyWebhooks(whRaw, kr)
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
	report *ImportReport,
) error {
	// Clear target tables in reverse FK dependency order for clean idempotent import
	tablesToClear := []string{
		"analytics_top_items", "analytics_daily", "webhooks", "security_geo_rules", "security_ip_bans",
		"redirects", "follows", "links", "comments", "book_chapters", "books", "series_articles", "series",
		"nav_items", "article_backlinks", "article_revisions", "article_tags", "articles", "tags", "categories", "users",
	}
	for _, tbl := range tablesToClear {
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+tx.Dialect().QuoteIdent(tbl)); err != nil {
			return fmt.Errorf("etl: clear %s: %w", tbl, err)
		}
	}

	for _, u := range st.users {
		if u.Role == domain.RoleAdmin && u.PasswordHash == "" && u.LegacyHash == "" && len(existingAdmins) > 0 {
			u.PasswordHash = existingAdmins[0].PasswordHash
			u.LegacyHash = existingAdmins[0].LegacyHash
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO users (id, username, email, display_name, avatar_url, bio, website_url, role, password_hash, legacy_hash, status, email_verified_at, preferred_locale, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			u.ID, u.Username, u.Email, u.DisplayName, u.AvatarURL, u.Bio, u.WebsiteURL,
			u.Role, u.PasswordHash, u.LegacyHash, u.Status, u.EmailVerifiedAt, u.PreferredLocale, u.CreatedAt, u.UpdatedAt,
		); err != nil {
			return fmt.Errorf("etl: insert user %s: %w", u.ID, err)
		}
		report.Users++
	}

	for _, c := range st.categories {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO categories (id, name, slug, description, icon, sort_order, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			c.ID, c.Name, c.Slug, c.Description, c.Icon, c.SortOrder, c.CreatedAt, c.UpdatedAt,
		); err != nil {
			return fmt.Errorf("etl: insert category %s: %w", c.ID, err)
		}
		report.Categories++
	}

	for _, tg := range st.tags {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO tags (id, name, slug, created_at) VALUES (?, ?, ?, ?)`,
			tg.ID, tg.Name, tg.Slug, tg.CreatedAt,
		); err != nil {
			return fmt.Errorf("etl: insert tag %s: %w", tg.ID, err)
		}
		report.Tags++
	}

	for _, s := range dec.series {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO series (id, title, slug, description, cover_url, sort_order, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			s.ID, s.Title, s.Slug, s.Description, s.CoverURL, s.SortOrder, s.CreatedAt, s.UpdatedAt,
		); err != nil {
			return fmt.Errorf("etl: insert series %s: %w", s.ID, err)
		}
	}
	for _, sa := range dec.seriesArticles {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO series_articles (series_id, article_id, sort_order) VALUES (?, ?, ?)`,
			sa.SeriesID, sa.ArticleID, sa.SortOrder,
		); err != nil {
			return fmt.Errorf("etl: insert series_article: %w", err)
		}
	}

	for _, bk := range dec.books {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO books (id, title, slug, description, cover_url, source_type, source_url, series_id, embed_mode, chapter_count, synced_at, sort_order, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			bk.ID, bk.Title, bk.Slug, bk.Description, bk.CoverURL, bk.SourceType, bk.SourceURL,
			bk.SeriesID, bk.EmbedMode, bk.ChapterCount, bk.SyncedAt, bk.SortOrder, bk.CreatedAt, bk.UpdatedAt,
		); err != nil {
			return fmt.Errorf("etl: insert book %s: %w", bk.ID, err)
		}
	}
	for _, ch := range dec.bookChapters {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO book_chapters (id, book_id, parent_id, title, url, depth, sort_order)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			ch.ID, ch.BookID, ch.ParentID, ch.Title, ch.URL, ch.Depth, ch.SortOrder,
		); err != nil {
			return fmt.Errorf("etl: insert book_chapter %s: %w", ch.ID, err)
		}
	}

	for _, ad := range dec.analyticsDaily {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO analytics_daily (stat_date, pv, uv, updated_at) VALUES (?, ?, ?, ?)`,
			ad.StatDate, ad.PV, ad.UV, ad.UpdatedAt,
		); err != nil {
			return fmt.Errorf("etl: insert analytics_daily %s: %w", ad.StatDate, err)
		}
	}
	for _, top := range dec.analyticsTop {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO analytics_top_items (stat_date, kind, item_key, hits) VALUES (?, ?, ?, ?)`,
			top.StatDate, top.Kind, top.ItemKey, top.Hits,
		); err != nil {
			return fmt.Errorf("etl: insert analytics_top_items: %w", err)
		}
	}

	for _, wh := range dec.webhooks {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO webhooks (id, name, provider, url, secret_ciphertext, chat_id, events_json, is_enabled, last_status_code, last_error, last_triggered_at, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			wh.ID, wh.Name, wh.Provider, wh.URL, wh.SecretCiphertext, wh.ChatID, wh.EventsJSON,
			wh.IsEnabled, 0, "", 0, wh.CreatedAt, wh.UpdatedAt,
		); err != nil {
			return fmt.Errorf("etl: insert webhook %s: %w", wh.ID, err)
		}
	}

	for _, ban := range dec.ipBans {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO security_ip_bans (id, cidr, source, reason_code, reason, note, occurrences, expires_at, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			ban.ID, ban.CIDR, ban.Source, ban.ReasonCode, ban.Reason, ban.Note, ban.Occurrences, ban.ExpiresAt, ban.CreatedAt, ban.UpdatedAt,
		); err != nil {
			return fmt.Errorf("etl: insert ip_ban %s: %w", ban.CIDR, err)
		}
	}

	for _, gr := range dec.geoRules {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO security_geo_rules (id, name, is_enabled, action, on_miss, countries, regions, sort_order, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			gr.ID, gr.Name, gr.IsEnabled, gr.Action, gr.OnMiss, gr.Countries, gr.Regions, gr.SortOrder, gr.CreatedAt, gr.UpdatedAt,
		); err != nil {
			return fmt.Errorf("etl: insert geo_rule %s: %w", gr.Name, err)
		}
	}

	return nil
}

func streamPass2IntoTx(
	ctx context.Context,
	tx *sqlx.Tx,
	openStream func() (io.ReadCloser, error),
	st *pass1Data,
	kr *config.KeyRing,
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

	usedArticleSlugs := make(map[string]bool)
	usedRedirects := make(map[string]bool)
	defaultLocale := i18n.NormalizeLocale(st.options["localeString"].Value)
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
				aid := strVal(raw["oId"])
				if aid == "" {
					return nil
				}
				origPermalink := normalizeLegacyPath(strVal(raw["articlePermalink"]))
				if origPermalink == "/my-github-repos" {
					// Cleanly skip auto-generated legacy GitHub HTML pseudo-article
					report.Warnings = append(report.Warnings, "skipped legacy auto-generated /my-github-repos pseudo-article (replaced by native GitHub showcase)")
					return nil
				}

				authorID := st.userIDMap[strVal(raw["articleAuthorId"])]
				if authorID == "" {
					authorID = st.defaultAdminID
				}
				catID := st.articlePrimaryCat[aid]
				title := firstNonEmpty(strings.TrimSpace(strVal(raw["articleTitle"])), "Untitled "+aid)
				slugBase := deriveSlugFromPermalinkOrTitle(origPermalink, title, aid)
				slug := dedupSlug(slugBase, usedArticleSlugs)
				modernPath := "/posts/" + slug

				contentMD := strVal(raw["articleContent"])
				summaryMD := strVal(raw["articleAbstract"])
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
				pwdCipher, sErr := kr.SealString(viewPwd, "articles:password:"+aid)
				if sErr != nil {
					return sErr
				}
				hasLock := strings.Contains(contentMD, ":::lock")
				words, mins := estimateReadingStats(contentMD)

				audioURL := strings.TrimSpace(strVal(raw["articleAudioURL"]))
				audioMime := ""
				if audioURL != "" {
					audioMime = inferAudioMIME(audioURL)
				}

				coverURL := strVal(raw["articleImg1URL"])
				if isLegacyExternalAsset(coverURL) {
					coverURL = ""
				}

				if _, err := tx.ExecContext(ctx,
					`INSERT INTO articles (
						id, author_id, category_id, title, slug, legacy_permalink, summary_md, content_md,
						cover_url, status, published_at, scheduled_at, is_pinned, allow_comment,
						password_ciphertext, has_content_lock, audio_url, audio_duration_sec, audio_size_bytes,
						audio_mime, lang, translation_group, license_override, word_count, reading_minutes,
						view_count, comment_count, created_at, updated_at
					) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '', '', ?, ?, ?, ?, ?, ?)`,
					aid, authorID, catID, title, slug, origPermalink, summaryMD, contentMD,
					coverURL, status, publishedAt, schedAt, boolVal(raw["articlePutTop"]), boolValDefault(raw["articleCommentable"], true),
					pwdCipher, hasLock, audioURL, int(int64Val(raw["articleAudioDuration"])), int64Val(raw["articleAudioSize"]),
					audioMime, defaultLocale, words, mins,
					int64Val(raw["articleViewCount"]), int(int64Val(raw["articleCommentCount"])), createdAt, updatedAt,
				); err != nil {
					return fmt.Errorf("etl: insert article %s: %w", aid, err)
				}
				report.Articles++

				// Bind free-form tags (from tag_articles + fallback comma-separated articleTags)
				boundTags := make(map[string]bool)
				for _, tid := range st.artTagsMap[aid] {
					if st.tagIDSet[tid] && !boundTags[tid] {
						boundTags[tid] = true
						_, _ = tx.ExecContext(ctx, `INSERT INTO article_tags (article_id, tag_id) VALUES (?, ?)`, aid, tid)
					}
				}
				for _, rawTagName := range strings.Split(strVal(raw["articleTags"]), ",") {
					tName := strings.TrimSpace(rawTagName)
					if tid, ok := st.tagByName[strings.ToLower(tName)]; ok && !boundTags[tid] {
						boundTags[tid] = true
						_, _ = tx.ExecContext(ctx, `INSERT INTO article_tags (article_id, tag_id) VALUES (?, ?)`, aid, tid)
					}
				}

				// Register 301 Redirect Guardian for historical permalink (including .html)!
				if origPermalink != "" && origPermalink != modernPath && !usedRedirects[origPermalink] && len(origPermalink) <= 190 {
					usedRedirects[origPermalink] = true
					if _, err := tx.ExecContext(ctx,
						`INSERT INTO redirects (id, source_path, target_path, status_code, is_system_legacy, hit_count, created_at)
						 VALUES (?, ?, ?, 301, ?, 0, ?)`,
						id.Next(), origPermalink, modernPath, true, createdAt,
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
				rid := firstNonEmpty(strVal(raw["oId"]), id.Next())
				aid := strVal(raw["articleOId"])
				if aid == "" {
					return nil
				}
				if _, err := tx.ExecContext(ctx,
					`INSERT INTO article_revisions (id, article_id, title, summary_md, content_md, author_name, note, created_at)
					 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
					rid, aid, strVal(raw["revisionTitle"]), strVal(raw["revisionAbstract"]),
					strVal(raw["revisionContent"]), strVal(raw["revisionAuthorName"]),
					strVal(raw["revisionNote"]), int64Val(raw["revisionCreated"]),
				); err != nil {
					return fmt.Errorf("etl: insert revision %s: %w", rid, err)
				}
				report.Revisions++
				return nil
			}); err != nil {
				return err
			}

		case "pages":
			// Legacy "page" table held ONLY header navigation items -> split cleanly into nav_items!
			if err := streamJSONArray(dec, func(raw map[string]any) error {
				nid := firstNonEmpty(strVal(raw["oId"]), id.Next())
				title := strings.TrimSpace(strVal(raw["pageTitle"]))
				targetURL := strings.TrimSpace(strVal(raw["pagePermalink"]))
				if title == "" || targetURL == "" {
					return nil
				}
				newTab := strings.EqualFold(strVal(raw["pageOpenTarget"]), "_blank")
				icon := strVal(raw["pageIcon"])
				if isLegacyExternalAsset(icon) {
					icon = ""
				}
				ts := parseMillisFallback(nid)
				if _, err := tx.ExecContext(ctx,
					`INSERT INTO nav_items (id, parent_id, title, i18n_titles_json, url, open_in_new_tab, icon, sort_order, is_enabled, created_at, updated_at)
					 VALUES (?, '', ?, '{}', ?, ?, ?, ?, ?, ?, ?)`,
					nid, title, targetURL, newTab, icon, int(int64Val(raw["pageOrder"])), true, ts, ts,
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
				cid := strVal(raw["oId"])
				onID := strVal(raw["commentOnId"])
				if cid == "" || onID == "" {
					return nil
				}
				rawUID := strVal(raw["commentUserId"])
				mappedUID := st.userIDMap[rawUID]

				notifyEmail := strings.TrimSpace(strVal(raw["commentNotifyEmail"]))
				if notifyEmail == "" {
					notifyEmail = st.commentMailFallback[cid]
				}
				emailCipher, sErr := kr.SealString(notifyEmail, "comments:email:"+cid)
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

				if _, err := tx.ExecContext(ctx,
					`INSERT INTO comments (
						id, target_type, target_id, parent_id, reply_to_name, user_id, guest_id_hash, env_fp_hash,
						author_name, author_url, avatar_url, notify_email_ciphertext, notify_locale,
						client_ip, ip_masked, geo_region, content_md, status, is_deleted, created_at, updated_at
					) VALUES (?, 'article', ?, ?, ?, ?, ?, '', ?, ?, ?, ?, ?, ?, ?, '', ?, ?, ?, ?, ?)`,
					cid, onID, strVal(raw["commentOriginalCommentId"]), strVal(raw["commentOriginalCommentName"]),
					mappedUID, strVal(raw["commentGuestKeyHash"]),
					firstNonEmpty(strVal(raw["commentName"]), "Guest"), strVal(raw["commentURL"]), avatar,
					emailCipher, defaultLocale, clientIP, ipMasked, strVal(raw["commentContent"]),
					status, isDeleted, createdAt, createdAt,
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
				lid := firstNonEmpty(strVal(raw["oId"]), id.Next())
				addr := strings.TrimSpace(strVal(raw["linkAddress"]))
				if addr == "" {
					return nil
				}
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
				}
				if _, err := tx.ExecContext(ctx,
					`INSERT INTO links (id, title, url, description, icon_url, category_name, sort_order, health_status, http_code, latency_ms, health_error, last_checked_at, created_at, updated_at)
					 VALUES (?, ?, ?, ?, ?, '', ?, ?, ?, ?, ?, ?, ?, ?)`,
					lid, firstNonEmpty(strVal(raw["linkTitle"]), addr), addr, strVal(raw["linkDescription"]),
					icon, int(int64Val(raw["linkOrder"])), hStatus, code, lh.LatencyMS, lh.Error, lh.CheckedAt, ts, ts,
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
				fid := firstNonEmpty(strVal(raw["oId"]), id.Next())
				addr := strings.TrimSpace(strVal(raw["followAddress"]))
				if addr == "" {
					return nil
				}
				ts := parseMillisFallback(fid)
				icon := strVal(raw["followIcon"])
				if isLegacyExternalAsset(icon) {
					icon = ""
				}
				if _, err := tx.ExecContext(ctx,
					`INSERT INTO follows (id, title, site_url, feed_url, description, icon_url, sort_order, last_synced_at, sync_error, created_at, updated_at)
					 VALUES (?, ?, ?, ?, ?, ?, ?, 0, '', ?, ?)`,
					fid, firstNonEmpty(strVal(raw["followTitle"]), addr), addr, addr,
					strVal(raw["followDescription"]), icon, int(int64Val(raw["followOrder"])), ts, ts,
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
	return nil
}

func countPass2Entities(ctx context.Context, openStream func() (io.ReadCloser, error), st *pass1Data, report *ImportReport) error {
	report.Users = len(st.users)
	report.Categories = len(st.categories)
	report.Tags = len(st.tags)

	rc, err := openStream()
	if err != nil {
		return err
	}
	defer rc.Close()

	dec := json.NewDecoder(rc)
	if _, err := dec.Token(); err != nil {
		return err
	}
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
				if normalizeLegacyPath(strVal(raw["articlePermalink"])) != "/my-github-repos" {
					report.Articles++
				}
				return nil
			})
		case "articleRevisions", "revisions", "article_revision":
			_ = streamJSONArray(dec, func(_ map[string]any) error { report.Revisions++; return nil })
		case "pages":
			_ = streamJSONArray(dec, func(_ map[string]any) error { report.NavItems++; return nil })
		case "comments":
			_ = streamJSONArray(dec, func(_ map[string]any) error { report.Comments++; return nil })
		case "links":
			_ = streamJSONArray(dec, func(_ map[string]any) error { report.Links++; return nil })
		case "follows":
			_ = streamJSONArray(dec, func(_ map[string]any) error { report.Follows++; return nil })
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

func parseLegacyBooksAndTOCs(raw string, st *pass1Data) ([]domain.Book, []domain.BookChapter, error) {
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
		return nil, nil, fmt.Errorf("etl: parse voloBooks: %w", err)
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
		if tocRaw, ok, err := st.reassembleOptionDoc("voloBookToc." + b.ID); err != nil {
			return nil, nil, err
		} else if ok && tocRaw != "" {
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
	return booksOut, chapsOut, nil
}

func parseLegacyAnalytics(raw string) ([]domain.AnalyticsDaily, []domain.AnalyticsTopItem) {
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
		for aid, hits := range bucket.Articles {
			if aid != "" && hits > 0 {
				topOut = append(topOut, domain.AnalyticsTopItem{StatDate: d, Kind: "article", ItemKey: aid, Hits: hits})
			}
		}
		for ref, hits := range bucket.Referers {
			if ref != "" && hits > 0 {
				topOut = append(topOut, domain.AnalyticsTopItem{StatDate: d, Kind: "referer", ItemKey: ref, Hits: hits})
			}
		}
	}
	return dailyOut, topOut
}

func parseLegacyWebhooks(raw string, kr *config.KeyRing) ([]domain.Webhook, error) {
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
		return nil, fmt.Errorf("etl: parse voloWebhooks: %w", err)
	}
	var out []domain.Webhook
	now := time.Now().UnixMilli()
	for _, w := range arr {
		if strings.TrimSpace(w.URL) == "" {
			continue
		}
		wid := firstNonEmpty(strings.TrimSpace(w.ID), id.NextPrefixed("wh"))
		sealed, err := kr.SealString(w.Secret, "webhooks:secret:"+wid)
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
		if !strings.HasPrefix(v, "http://") && !strings.HasPrefix(v, "https://") {
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
				hiddenSet[h] = true
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
		Updated     string `json:"githubrepoUpdated"`
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
			Hidden:      hiddenSet[r.FullName] || hiddenSet[r.Name],
			UpdatedAt:   r.Updated,
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

func estimateReadingStats(md string) (words int, minutes int) {
	inLock := false
	for _, line := range strings.Split(md, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, ":::lock") {
			inLock = true
			continue
		}
		if inLock {
			if trimmed == ":::" {
				inLock = false
			}
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
		return x
	case json.Number:
		return x.String()
	case bool:
		if x {
			return "true"
		}
		return "false"
	default:
		return fmt.Sprint(x)
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

func intVal(v any) int { return int(int64Val(v)) }

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
