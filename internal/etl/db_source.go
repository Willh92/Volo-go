package etl

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Willh92/Volo-go/internal/backup"
	"github.com/Willh92/Volo-go/internal/config"
	"github.com/Willh92/Volo-go/internal/db/sqlx"
	"github.com/Willh92/Volo-go/internal/domain"
	"github.com/Willh92/Volo-go/internal/i18n"
	"github.com/Willh92/Volo-go/internal/repo"
)

const (
	SchemaKindSolo = "solo"
	SchemaKindVolo = "volo"
)

var validSQLIdentRe = regexp.MustCompile(`^[A-Za-z0-9_]+$`)

type legacyEntitySpec struct {
	rootKey  string
	suffixes []string
}

var legacyEntitySpecs = []legacyEntitySpec{
	{rootKey: "options", suffixes: []string{"option", "options"}},
	{rootKey: "users", suffixes: []string{"user", "users"}},
	{rootKey: "categories", suffixes: []string{"category", "categories"}},
	{rootKey: "tags", suffixes: []string{"tag", "tags"}},
	{rootKey: "category_tag", suffixes: []string{"category_tag", "category_tags", "categorytag"}},
	{rootKey: "tag_articles", suffixes: []string{"tag_article", "tag_articles", "tagarticle"}},
	{rootKey: "articles", suffixes: []string{"article", "articles"}},
	{rootKey: "articleRevisions", suffixes: []string{"article_revision", "article_revisions", "articlerevision", "articlerevisions", "revision", "revisions"}},
	{rootKey: "pages", suffixes: []string{"page", "pages"}},
	{rootKey: "comments", suffixes: []string{"comment", "comments"}},
	{rootKey: "links", suffixes: []string{"link", "links"}},
	{rootKey: "follows", suffixes: []string{"follow", "follows"}},
	{rootKey: "plugins", suffixes: []string{"plugin", "plugins"}},
}

// canonicalSoloColumnByNorm maps normalized (lowercase, underscore-stripped) SQL column names
// to exact Solo / Bolo / Volo-solo camelCase JSON keys expected by ImportLegacyArchive.
var canonicalSoloColumnByNorm = map[string]string{
	"oid":                        "oId",
	"optioncategory":             "optionCategory",
	"optionvalue":                "optionValue",
	"username":                   "userName",
	"useremail":                  "userEmail",
	"useremailverified":          "userEmailVerified",
	"userurl":                    "userURL",
	"userrole":                   "userRole",
	"useravatar":                 "userAvatar",
	"userb3key":                  "userB3Key",
	"userpasswordhash":           "userPasswordHash",
	"userstatus":                 "userStatus",
	"usercreated":                "userCreated",
	"categorytitle":              "categoryTitle",
	"categoryuri":                "categoryURI",
	"categorydescription":        "categoryDescription",
	"categoryorder":              "categoryOrder",
	"categoryoid":                "category_oId",
	"tagtitle":                   "tagTitle",
	"tagoid":                     "tag_oId",
	"articletitle":               "articleTitle",
	"articleabstract":            "articleAbstract",
	"articlecontent":             "articleContent",
	"articleauthorid":            "articleAuthorId",
	"articlecommentcount":        "articleCommentCount",
	"articleviewcount":           "articleViewCount",
	"articlepermalink":           "articlePermalink",
	"articleputtop":              "articlePutTop",
	"articlecreated":             "articleCreated",
	"articlecreatedate":          "articleCreated",
	"articleupdated":             "articleUpdated",
	"articleupdatedate":          "articleUpdated",
	"articlestatus":              "articleStatus",
	"articlecommentable":         "articleCommentable",
	"articleviewpwd":             "articleViewPwd",
	"articleimg1url":             "articleImg1URL",
	"articleaudiourl":            "articleAudioURL",
	"articleaudioduration":       "articleAudioDuration",
	"articleaudiosize":           "articleAudioSize",
	"articlescheduletime":        "articleScheduleTime",
	"articletags":                "articleTags",
	"revisiontitle":              "revisionTitle",
	"revisionabstract":           "revisionAbstract",
	"revisioncontent":            "revisionContent",
	"revisionauthorname":         "revisionAuthorName",
	"revisionnote":               "revisionNote",
	"revisioncreated":            "revisionCreated",
	"pagetitle":                  "pageTitle",
	"pagepermalink":              "pagePermalink",
	"pageorder":                  "pageOrder",
	"pageicon":                   "pageIcon",
	"pageopentarget":             "pageOpenTarget",
	"commentcontent":             "commentContent",
	"commentcreated":             "commentCreated",
	"commentdate":                "commentCreated",
	"commentname":                "commentName",
	"commentonid":                "commentOnId",
	"commentoriginalcommentid":   "commentOriginalCommentId",
	"commentoriginalcommentname": "commentOriginalCommentName",
	"commentthumbnailurl":        "commentThumbnailURL",
	"commenturl":                 "commentURL",
	"commentuserid":              "commentUserId",
	"commentstatus":              "commentStatus",
	"commentdeleted":             "commentDeleted",
	"commentclientip":            "commentClientIP",
	"commentguestkeyhash":        "commentGuestKeyHash",
	"commentnotifyemail":         "commentNotifyEmail",
	"commentemail":               "commentNotifyEmail",
	"linktitle":                  "linkTitle",
	"linkaddress":                "linkAddress",
	"linkdescription":            "linkDescription",
	"linkicon":                   "linkIcon",
	"linkorder":                  "linkOrder",
	"followtitle":                "followTitle",
	"followaddress":              "followAddress",
	"followdescription":          "followDescription",
	"followicon":                 "followIcon",
	"followorder":                "followOrder",
	"status":                     "status",
	"pluginstatus":               "status",
	"author":                     "author",
	"name":                       "name",
	"version":                    "version",
}

var legacyTimestampCanonKeys = map[string]bool{
	"userCreated":         true,
	"articleCreated":      true,
	"articleUpdated":      true,
	"articleScheduleTime": true,
	"revisionCreated":     true,
	"commentCreated":      true,
}

// SourceDBInspection describes an existing database discovered during Setup Wizard connectivity testing or direct migration.
type SourceDBInspection struct {
	Dialect       string            `json:"dialect"`
	SchemaKind    string            `json:"schema_kind"` // "solo" or "volo"
	TablePrefix   string            `json:"table_prefix"`
	MatchedTables map[string]string `json:"matched_tables"`
	Users         int64             `json:"users"`
	Articles      int64             `json:"articles"`
	Comments      int64             `json:"comments"`
	Categories    int64             `json:"categories"`
	Tags          int64             `json:"tags"`
}

// DiscoverCatalogTables queries the database catalog for base tables in the active schema,
// filtering strictly to safe SQL identifiers so catalog names can be quoted safely.
func DiscoverCatalogTables(ctx context.Context, store *sqlx.Store) (map[string]string, error) {
	if store == nil || store.Dialect() == nil {
		return nil, errors.New("etl: nil source database store")
	}
	var query string
	switch store.Dialect().Name() {
	case "sqlite":
		query = `SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`
	case "postgres":
		query = `SELECT table_name FROM information_schema.tables WHERE table_schema = current_schema() AND table_type = 'BASE TABLE'`
	case "mysql":
		query = `SELECT table_name FROM information_schema.tables WHERE table_schema = DATABASE() AND table_type = 'BASE TABLE'`
	default:
		return nil, fmt.Errorf("etl: unsupported source dialect %q", store.Dialect().Name())
	}

	rows, err := store.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("etl: query source catalog tables: %w", err)
	}
	defer rows.Close()

	byLower := make(map[string]string)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		name = strings.TrimSpace(name)
		if name == "" || !validSQLIdentRe.MatchString(name) {
			continue
		}
		lower := strings.ToLower(name)
		if _, exists := byLower[lower]; !exists {
			byLower[lower] = name
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return byLower, nil
}

// InspectSourceDatabase auto-detects whether store is a Solo/Bolo/Volo-solo database or a native Volo-go database,
// resolves the table prefix, and counts core entities without modifying the source database.
func InspectSourceDatabase(ctx context.Context, store *sqlx.Store, prefixHint string) (*SourceDBInspection, error) {
	catalog, err := DiscoverCatalogTables(ctx, store)
	if err != nil {
		return nil, err
	}
	if len(catalog) == 0 {
		return nil, errors.New("etl: source database contains no tables")
	}

	dialectName := store.Dialect().Name()
	trimmedHint := strings.TrimSpace(prefixHint)

	// 1. Check for native Volo-go schema when no custom Solo prefix was forced.
	if trimmedHint == "" && isNativeVoloCatalog(ctx, store, catalog) {
		insp := &SourceDBInspection{
			Dialect:       dialectName,
			SchemaKind:    SchemaKindVolo,
			TablePrefix:   "",
			MatchedTables: map[string]string{},
		}
		for _, t := range []string{"users", "articles", "comments", "categories", "tags", "settings"} {
			if exact, ok := catalog[t]; ok {
				insp.MatchedTables[t] = exact
			}
		}
		insp.Users, _ = countCatalogTableRows(ctx, store, catalog["users"])
		insp.Articles, _ = countCatalogTableRows(ctx, store, catalog["articles"])
		insp.Comments, _ = countCatalogTableRows(ctx, store, catalog["comments"])
		insp.Categories, _ = countCatalogTableRows(ctx, store, catalog["categories"])
		insp.Tags, _ = countCatalogTableRows(ctx, store, catalog["tags"])
		return insp, nil
	}

	// 2. Resolve Solo / Bolo / Volo-solo table prefix and matched entity tables.
	resolvedPrefix, matched := matchSoloTables(catalog, trimmedHint)
	if len(matched) == 0 {
		if trimmedHint != "" {
			return nil, fmt.Errorf("etl: no legacy Solo/Bolo tables found matching prefix %q", trimmedHint)
		}
		return nil, errors.New("etl: no recognizable Solo/Bolo or Volo-go tables found in source database")
	}

	// Verify at least one matched core table has a recognizable Solo/Bolo column (`oId`, `articleTitle`, `optionValue`, `userName`).
	if !verifySoloColumnSignature(ctx, store, matched) {
		return nil, errors.New("etl: source database tables do not match Solo/Bolo or Volo-go schema")
	}

	insp := &SourceDBInspection{
		Dialect:       dialectName,
		SchemaKind:    SchemaKindSolo,
		TablePrefix:   resolvedPrefix,
		MatchedTables: matched,
	}
	if tbl, ok := matched["users"]; ok {
		insp.Users, _ = countCatalogTableRows(ctx, store, tbl)
	}
	if tbl, ok := matched["articles"]; ok {
		insp.Articles, _ = countCatalogTableRows(ctx, store, tbl)
	}
	if tbl, ok := matched["comments"]; ok {
		insp.Comments, _ = countCatalogTableRows(ctx, store, tbl)
	}
	if tbl, ok := matched["categories"]; ok {
		insp.Categories, _ = countCatalogTableRows(ctx, store, tbl)
	}
	if tbl, ok := matched["tags"]; ok {
		insp.Tags, _ = countCatalogTableRows(ctx, store, tbl)
	}
	return insp, nil
}

func isNativeVoloCatalog(ctx context.Context, store *sqlx.Store, catalog map[string]string) bool {
	for _, req := range []string{"schema_migrations", "settings", "articles", "users"} {
		if _, ok := catalog[req]; !ok {
			return false
		}
	}
	cols, err := listTableColumnsLower(ctx, store, catalog["articles"])
	if err != nil {
		return false
	}
	return cols["content_md"] && cols["slug"]
}

func verifySoloColumnSignature(ctx context.Context, store *sqlx.Store, matched map[string]string) bool {
	checks := []struct {
		rootKey string
		expect  string
	}{
		{"articles", "articletitle"},
		{"options", "optionvalue"},
		{"users", "username"},
		{"categories", "categorytitle"},
		{"tags", "tagtitle"},
	}
	for _, c := range checks {
		tbl, ok := matched[c.rootKey]
		if !ok {
			continue
		}
		cols, err := listTableColumnsLower(ctx, store, tbl)
		if err != nil {
			continue
		}
		for col := range cols {
			norm := strings.ReplaceAll(col, "_", "")
			if norm == "oid" || norm == c.expect {
				return true
			}
		}
	}
	return false
}

func listTableColumnsLower(ctx context.Context, store *sqlx.Store, exactCatalogTable string) (map[string]bool, error) {
	if exactCatalogTable == "" || !validSQLIdentRe.MatchString(exactCatalogTable) {
		return nil, errors.New("etl: invalid catalog table identifier")
	}
	q := "SELECT * FROM " + quoteSQLIdent(store.Dialect().Name(), exactCatalogTable) + " WHERE 1 = 0"
	rows, err := store.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(cols))
	for _, c := range cols {
		out[strings.ToLower(strings.TrimSpace(c))] = true
	}
	return out, nil
}

func countCatalogTableRows(ctx context.Context, store *sqlx.Store, exactCatalogTable string) (int64, error) {
	if exactCatalogTable == "" || !validSQLIdentRe.MatchString(exactCatalogTable) {
		return 0, nil
	}
	q := "SELECT COUNT(*) FROM " + quoteSQLIdent(store.Dialect().Name(), exactCatalogTable)
	var n int64
	if err := store.QueryRowContext(ctx, q).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

func quoteSQLIdent(dialectName, ident string) string {
	if dialectName == "mysql" {
		return "`" + strings.ReplaceAll(ident, "`", "``") + "`"
	}
	return `"` + strings.ReplaceAll(ident, `"`, `""`) + `"`
}

func matchSoloTables(catalog map[string]string, prefixHint string) (string, map[string]string) {
	var candidates []string
	if prefixHint != "" {
		low := strings.ToLower(strings.TrimSpace(prefixHint))
		candidates = append(candidates, low)
		if !strings.HasSuffix(low, "_") {
			candidates = append(candidates, low+"_")
		} else {
			candidates = append(candidates, strings.TrimSuffix(low, "_"))
		}
	} else {
		seen := map[string]bool{
			"b3_solo_": true,
			"solo_":    true,
			"bolo_":    true,
			"volo_":    true,
			"":         true,
		}
		candidates = []string{"b3_solo_", "solo_", "bolo_", "volo_", ""}
		anchorSuffixes := []string{"article", "articles", "option", "options", "user", "users"}
		var discovered []string
		for lowerName := range catalog {
			for _, suf := range anchorSuffixes {
				if strings.HasSuffix(lowerName, "_"+suf) {
					pfx := lowerName[:len(lowerName)-len(suf)]
					if !seen[pfx] {
						seen[pfx] = true
						discovered = append(discovered, pfx)
					}
				}
			}
		}
		sort.Strings(discovered)
		candidates = append(candidates, discovered...)
	}

	bestPrefix := ""
	var bestMatched map[string]string
	bestScore := 0

	for _, pfx := range candidates {
		matched := make(map[string]string)
		score := 0
		hasCore := false
		for _, spec := range legacyEntitySpecs {
			for _, suf := range spec.suffixes {
				if exact, ok := catalog[pfx+suf]; ok {
					matched[spec.rootKey] = exact
					score++
					if spec.rootKey == "articles" || spec.rootKey == "options" || spec.rootKey == "users" {
						hasCore = true
					}
					break
				}
			}
		}
		if hasCore && score > bestScore {
			bestScore = score
			bestPrefix = pfx
			bestMatched = matched
		}
	}
	return bestPrefix, bestMatched
}

// StreamLegacyDBToJSON streams a Legacy Solo / Bolo / Volo-solo relational database row-by-row
// into the canonical JSON backup layout consumed by ImportLegacyArchive.
func StreamLegacyDBToJSON(ctx context.Context, store *sqlx.Store, insp *SourceDBInspection, w io.Writer) error {
	if insp == nil || insp.SchemaKind != SchemaKindSolo {
		return errors.New("etl: source database is not a Solo/Bolo schema")
	}
	cw := &countingLimitWriter{w: bufio.NewWriterSize(w, 64*1024), limit: maxLegacyJSONBytes}
	if _, err := cw.WriteString("{"); err != nil {
		return err
	}

	firstTable := true
	for _, spec := range legacyEntitySpecs {
		exactTable, ok := insp.MatchedTables[spec.rootKey]
		if !ok || exactTable == "" {
			continue
		}
		if !firstTable {
			if _, err := cw.WriteString(","); err != nil {
				return err
			}
		}
		firstTable = false

		keyJSON, _ := json.Marshal(spec.rootKey)
		if _, err := cw.Write(keyJSON); err != nil {
			return err
		}
		if _, err := cw.WriteString(":["); err != nil {
			return err
		}
		if err := streamSingleLegacyTable(ctx, store, spec.rootKey, exactTable, cw); err != nil {
			return err
		}
		if _, err := cw.WriteString("]"); err != nil {
			return err
		}
	}

	if _, err := cw.WriteString("}"); err != nil {
		return err
	}
	return cw.Flush()
}

type countingLimitWriter struct {
	w       *bufio.Writer
	written int64
	limit   int64
}

func (c *countingLimitWriter) Write(p []byte) (int, error) {
	if c.written+int64(len(p)) > c.limit {
		return 0, fmt.Errorf("etl: source database stream exceeds %d MiB safety limit", c.limit/(1024*1024))
	}
	n, err := c.w.Write(p)
	c.written += int64(n)
	return n, err
}

func (c *countingLimitWriter) WriteString(s string) (int, error) {
	if c.written+int64(len(s)) > c.limit {
		return 0, fmt.Errorf("etl: source database stream exceeds %d MiB safety limit", c.limit/(1024*1024))
	}
	n, err := c.w.WriteString(s)
	c.written += int64(n)
	return n, err
}

func (c *countingLimitWriter) Flush() error {
	return c.w.Flush()
}

func streamSingleLegacyTable(ctx context.Context, store *sqlx.Store, rootKey, exactCatalogTable string, cw *countingLimitWriter) error {
	if !validSQLIdentRe.MatchString(exactCatalogTable) {
		return errors.New("etl: invalid source catalog table name")
	}
	query := "SELECT * FROM " + quoteSQLIdent(store.Dialect().Name(), exactCatalogTable)
	rows, err := store.QueryContext(ctx, query)
	if err != nil {
		return fmt.Errorf("etl: stream table %q: %w", exactCatalogTable, err)
	}
	defer rows.Close()

	rawCols, err := rows.Columns()
	if err != nil {
		return err
	}
	canonCols := make([]string, len(rawCols))
	hasCanon := make(map[string]bool, len(rawCols))
	for i, rc := range rawCols {
		canon := canonicalLegacyColumnKey(rootKey, rc)
		canonCols[i] = canon
		hasCanon[canon] = true
	}
	// Avoid overwriting primary commentNotifyEmail with empty Solo v2 commentEmail if both columns exist.
	for i, rc := range rawCols {
		norm := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(rc), "_", ""))
		if norm == "commentemail" && hasCanon["commentNotifyEmail"] && countMatchingNorm(rawCols, "commentnotifyemail") > 0 {
			canonCols[i] = "_legacy_comment_email_alt"
		}
	}

	rawVals := make([]any, len(rawCols))
	scanPtrs := make([]any, len(rawCols))
	for i := range rawVals {
		scanPtrs[i] = &rawVals[i]
	}

	rowMap := make(map[string]any, len(rawCols))
	firstRow := true
	for rows.Next() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		for i := range rawVals {
			rawVals[i] = nil
		}
		if err := rows.Scan(scanPtrs...); err != nil {
			return fmt.Errorf("etl: scan table %q: %w", exactCatalogTable, err)
		}
		clear(rowMap)
		var fallbackCommentEmail any
		for i, key := range canonCols {
			if key == "" {
				continue
			}
			val := normalizeSQLCellValue(key, rawVals[i])
			if val == nil {
				continue
			}
			if key == "_legacy_comment_email_alt" {
				fallbackCommentEmail = val
				continue
			}
			rowMap[key] = val
		}
		if fallbackCommentEmail != nil {
			if cur, ok := rowMap["commentNotifyEmail"]; !ok || strings.TrimSpace(fmt.Sprint(cur)) == "" {
				rowMap["commentNotifyEmail"] = fallbackCommentEmail
			}
		}

		encoded, err := json.Marshal(rowMap)
		if err != nil {
			return err
		}
		if !firstRow {
			if _, err := cw.WriteString(","); err != nil {
				return err
			}
		}
		firstRow = false
		if _, err := cw.Write(encoded); err != nil {
			return err
		}
	}
	return rows.Err()
}

func countMatchingNorm(cols []string, targetNorm string) int {
	n := 0
	for _, c := range cols {
		if strings.ToLower(strings.ReplaceAll(strings.TrimSpace(c), "_", "")) == targetNorm {
			n++
		}
	}
	return n
}

func canonicalLegacyColumnKey(rootKey, rawCol string) string {
	trimmed := strings.TrimSpace(rawCol)
	if trimmed == "" {
		return ""
	}
	norm := strings.ToLower(strings.ReplaceAll(trimmed, "_", ""))
	if norm == "articleoid" {
		if rootKey == "tag_articles" {
			return "article_oId"
		}
		return "articleOId"
	}
	if canon, ok := canonicalSoloColumnByNorm[norm]; ok {
		return canon
	}
	return trimmed
}

func normalizeSQLCellValue(canonKey string, v any) any {
	if v == nil {
		return nil
	}
	switch x := v.(type) {
	case time.Time:
		if x.IsZero() {
			return int64(0)
		}
		return x.UnixMilli()
	case []byte:
		// MySQL BIT(1) columns return single raw byte 0x00 or 0x01.
		if len(x) == 1 && (x[0] == 0 || x[0] == 1) {
			return x[0] == 1
		}
		if !utf8.Valid(x) {
			return cleanUTF8(string(x))
		}
		s := string(x)
		if legacyTimestampCanonKeys[canonKey] {
			if ms, ok := parseSQLTimestampMillis(s); ok {
				return ms
			}
		}
		return s
	case string:
		s := cleanUTF8(x)
		if legacyTimestampCanonKeys[canonKey] {
			if ms, ok := parseSQLTimestampMillis(s); ok {
				return ms
			}
		}
		return s
	default:
		return x
	}
}

func parseSQLTimestampMillis(raw string) (int64, bool) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0, false
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n, true
	}
	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05",
		"2006-01-02",
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UnixMilli(), true
		}
	}
	return 0, false
}

// ImportSourceDatabase inspects sourceStore and migrates either a Legacy Solo/Bolo/Volo-solo database
// or an existing native Volo-go database directly into targetStore.
func ImportSourceDatabase(
	ctx context.Context,
	sourceStore *sqlx.Store,
	targetStore *sqlx.Store,
	settingsMgr *repo.SettingsManager,
	keyRing *config.KeyRing,
	prefixHint string,
	opts ImportOptions,
) (*ImportReport, *SourceDBInspection, error) {
	insp, err := InspectSourceDatabase(ctx, sourceStore, prefixHint)
	if err != nil {
		return nil, nil, err
	}

	if insp.SchemaKind == SchemaKindVolo {
		rep, err := migrateNativeVoloDatabase(ctx, sourceStore, targetStore, settingsMgr, insp, opts)
		return rep, insp, err
	}

	spoolFile, err := os.CreateTemp("", "volo-legacy-db-*.json")
	if err != nil {
		return nil, insp, fmt.Errorf("etl: create temporary stream file: %w", err)
	}
	spoolPath := spoolFile.Name()
	_ = spoolFile.Chmod(0o600)
	defer func() { _ = os.Remove(spoolPath) }()

	if err := StreamLegacyDBToJSON(ctx, sourceStore, insp, spoolFile); err != nil {
		_ = spoolFile.Close()
		return nil, insp, err
	}
	if err := spoolFile.Close(); err != nil {
		return nil, insp, err
	}

	rep, err := ImportLegacyArchive(ctx, targetStore, settingsMgr, keyRing, spoolPath, opts)
	if err != nil {
		return nil, insp, err
	}
	if strings.TrimSpace(rep.SourceVersion) == "" {
		rep.SourceVersion = "legacy-db:" + insp.Dialect
	} else {
		rep.SourceVersion = rep.SourceVersion + " (db:" + insp.Dialect + ")"
	}
	return rep, insp, nil
}

func migrateNativeVoloDatabase(
	ctx context.Context,
	sourceStore *sqlx.Store,
	targetStore *sqlx.Store,
	settingsMgr *repo.SettingsManager,
	insp *SourceDBInspection,
	opts ImportOptions,
) (*ImportReport, error) {
	spoolZip, err := os.CreateTemp("", "volo-native-db-migrate-*.zip")
	if err != nil {
		return nil, fmt.Errorf("etl: create temporary backup spool: %w", err)
	}
	spoolPath := spoolZip.Name()
	_ = spoolZip.Chmod(0o600)
	defer func() { _ = os.Remove(spoolPath) }()

	includeMedia := strings.TrimSpace(opts.LegacyMediaDir) != ""
	if _, err := backup.Create(ctx, sourceStore, spoolZip, backup.CreateOptions{
		IncludeMedia: includeMedia,
		UploadDir:    opts.LegacyMediaDir,
	}); err != nil {
		_ = spoolZip.Close()
		return nil, fmt.Errorf("etl: stream source Volo-go database: %w", err)
	}
	if err := spoolZip.Close(); err != nil {
		return nil, err
	}

	rf, err := os.Open(spoolPath)
	if err != nil {
		return nil, err
	}
	defer rf.Close()
	rst, err := rf.Stat()
	if err != nil {
		return nil, err
	}

	summary, err := backup.Restore(ctx, targetStore, rf, rst.Size(), backup.RestoreOptions{
		DryRun:    opts.DryRun,
		UploadDir: opts.TargetUploadDir,
	})
	if err != nil {
		return nil, fmt.Errorf("etl: restore into target database: %w", err)
	}

	rep := &ImportReport{
		SourceVersion:    "volo-go-db (" + insp.Dialect + ")",
		DryRun:           opts.DryRun,
		TargetDialect:    targetStore.Dialect().Name(),
		Users:            clampInt64ToInt(summary.Tables["users"]),
		Categories:       clampInt64ToInt(summary.Tables["categories"]),
		Tags:             clampInt64ToInt(summary.Tables["tags"]),
		Articles:         clampInt64ToInt(summary.Tables["articles"]),
		Revisions:        clampInt64ToInt(summary.Tables["article_revisions"]),
		NavItems:         clampInt64ToInt(summary.Tables["nav_items"]),
		Comments:         clampInt64ToInt(summary.Tables["comments"]),
		Links:            clampInt64ToInt(summary.Tables["links"]),
		Follows:          clampInt64ToInt(summary.Tables["follows"]),
		Series:           clampInt64ToInt(summary.Tables["series"]),
		Books:            clampInt64ToInt(summary.Tables["books"]),
		BookChapters:     clampInt64ToInt(summary.Tables["book_chapters"]),
		MediaFiles:       summary.MediaFiles,
		AnalyticsDays:    clampInt64ToInt(summary.Tables["analytics_daily"]),
		Webhooks:         clampInt64ToInt(summary.Tables["webhooks"]),
		RedirectsCreated: clampInt64ToInt(summary.Tables["redirects"]),
	}
	if opts.DryRun {
		return rep, nil
	}

	if err := applyPostNativeMigrationOverrides(ctx, targetStore, settingsMgr, opts, rep); err != nil {
		return nil, err
	}
	return rep, nil
}

func clampInt64ToInt(n int64) int {
	if n < 0 {
		return 0
	}
	if n > 2147483647 {
		return 2147483647
	}
	return int(n)
}

func applyPostNativeMigrationOverrides(
	ctx context.Context,
	targetStore *sqlx.Store,
	settingsMgr *repo.SettingsManager,
	opts ImportOptions,
	rep *ImportReport,
) error {
	if settingsMgr != nil {
		if _, err := settingsMgr.Reload(ctx); err != nil {
			return err
		}
		snap := settingsMgr.Snapshot()
		siteChanged := false
		site := snap.Site
		if !site.Initialized {
			site.Initialized = true
			siteChanged = true
		}
		if v := strings.TrimSpace(opts.SiteTitle); v != "" {
			site.Title = v
			siteChanged = true
		}
		if v := strings.TrimSpace(opts.SiteSubtitle); v != "" {
			site.Subtitle = v
			siteChanged = true
		}
		if v := strings.TrimSpace(opts.TimeZone); v != "" {
			site.TimeZone = v
			siteChanged = true
		}
		if v := strings.TrimSpace(opts.DefaultLocale); v != "" {
			site.DefaultLocale = i18n.NormalizeLocale(v)
			siteChanged = true
		}
		if siteChanged {
			b, err := json.Marshal(site)
			if err != nil {
				return err
			}
			if _, _, err := settingsMgr.PatchDomain(ctx, domain.SettingsDomainSite, b, 0); err != nil {
				return err
			}
		}
		if acc := strings.ToLower(strings.TrimSpace(opts.Accent)); acc != "" {
			switch acc {
			case "forest", "ocean", "teal", "amber", "plum", "rose":
				if b, mErr := json.Marshal(map[string]any{"accent": acc}); mErr == nil {
					_, _, _ = settingsMgr.PatchDomain(ctx, domain.SettingsDomainAppearance, b, 0)
				}
			}
		}
		if opts.ForceLocalStorage && snap.Storage.ActiveDriver != "local" {
			stMap := structToMap(snap.Storage)
			stMap["active_driver"] = "local"
			b, err := json.Marshal(stMap)
			if err != nil {
				return err
			}
			if _, _, err := settingsMgr.PatchDomain(ctx, domain.SettingsDomainStorage, b, 0); err != nil {
				return err
			}
		}
	}

	newBase := strings.TrimRight(strings.TrimSpace(opts.NewSiteURL), "/")
	if newBase != "" && len(opts.OldURLPrefixes) > 0 {
		sqlRewrites := []string{
			`UPDATE articles SET content_md = REPLACE(content_md, ?, ?), summary_md = REPLACE(summary_md, ?, ?), cover_url = REPLACE(cover_url, ?, ?), audio_url = REPLACE(audio_url, ?, ?) WHERE content_md LIKE ? OR summary_md LIKE ? OR cover_url LIKE ? OR audio_url LIKE ?`,
			`UPDATE pages SET content_md = REPLACE(content_md, ?, ?) WHERE content_md LIKE ?`,
			`UPDATE comments SET content_md = REPLACE(content_md, ?, ?), author_url = REPLACE(author_url, ?, ?), avatar_url = REPLACE(avatar_url, ?, ?) WHERE content_md LIKE ? OR author_url LIKE ? OR avatar_url LIKE ?`,
			`UPDATE links SET url = REPLACE(url, ?, ?), icon_url = REPLACE(icon_url, ?, ?) WHERE url LIKE ? OR icon_url LIKE ?`,
			`UPDATE follows SET site_url = REPLACE(site_url, ?, ?), feed_url = REPLACE(feed_url, ?, ?), icon_url = REPLACE(icon_url, ?, ?) WHERE site_url LIKE ? OR feed_url LIKE ? OR icon_url LIKE ?`,
		}
		var rewrittenRows int64
		for _, rawOld := range opts.OldURLPrefixes {
			oldPfx := strings.TrimRight(strings.TrimSpace(rawOld), "/")
			if oldPfx == "" || oldPfx == newBase {
				continue
			}
			pat := "%" + strings.NewReplacer("%", "\\%", "_", "\\_").Replace(oldPfx) + "%"
			if res, rErr := targetStore.ExecContext(ctx, sqlRewrites[0], oldPfx, newBase, oldPfx, newBase, oldPfx, newBase, oldPfx, newBase, pat, pat, pat, pat); rErr == nil {
				if n, _ := res.RowsAffected(); n > 0 {
					rewrittenRows += n
				}
			}
			if res, rErr := targetStore.ExecContext(ctx, sqlRewrites[1], oldPfx, newBase, pat); rErr == nil {
				if n, _ := res.RowsAffected(); n > 0 {
					rewrittenRows += n
				}
			}
			if res, rErr := targetStore.ExecContext(ctx, sqlRewrites[2], oldPfx, newBase, oldPfx, newBase, oldPfx, newBase, pat, pat, pat); rErr == nil {
				if n, _ := res.RowsAffected(); n > 0 {
					rewrittenRows += n
				}
			}
			if res, rErr := targetStore.ExecContext(ctx, sqlRewrites[3], oldPfx, newBase, oldPfx, newBase, pat, pat); rErr == nil {
				if n, _ := res.RowsAffected(); n > 0 {
					rewrittenRows += n
				}
			}
			if res, rErr := targetStore.ExecContext(ctx, sqlRewrites[4], oldPfx, newBase, oldPfx, newBase, oldPfx, newBase, pat, pat, pat); rErr == nil {
				if n, _ := res.RowsAffected(); n > 0 {
					rewrittenRows += n
				}
			}
		}
		if rep != nil && rewrittenRows > 0 {
			rep.URLsRewritten = clampInt64ToInt(rewrittenRows)
		}
	}

	if strings.TrimSpace(opts.AdminUsername) == "" &&
		strings.TrimSpace(opts.AdminEmail) == "" &&
		strings.TrimSpace(opts.AdminDisplayName) == "" &&
		strings.TrimSpace(opts.AdminPasswordHash) == "" {
		return nil
	}

	var admins []domain.User
	err := sqlx.SelectInto(ctx, targetStore, &admins,
		`SELECT * FROM users WHERE role = ? ORDER BY created_at ASC LIMIT 1`, domain.RoleAdmin)
	if err != nil || len(admins) == 0 {
		return err
	}
	admin := admins[0]
	if u := truncRunes(strings.TrimSpace(opts.AdminUsername), 64); u != "" {
		admin.Username = u
	}
	if em := truncRunes(strings.ToLower(strings.TrimSpace(opts.AdminEmail)), 190); em != "" {
		admin.Email = em
	}
	if dn := truncRunes(strings.TrimSpace(opts.AdminDisplayName), 128); dn != "" {
		admin.DisplayName = dn
	}
	if ph := strings.TrimSpace(opts.AdminPasswordHash); ph != "" {
		admin.PasswordHash = ph
		admin.LegacyHash = ""
		admin.Status = domain.UserStatusActive
	}
	admin.UpdatedAt = time.Now().UnixMilli()
	_, err = targetStore.ExecContext(ctx,
		`UPDATE users SET username = ?, email = ?, display_name = ?, password_hash = ?, legacy_hash = ?, status = ?, updated_at = ? WHERE id = ?`,
		admin.Username, admin.Email, admin.DisplayName, admin.PasswordHash, admin.LegacyHash, admin.Status, admin.UpdatedAt, admin.ID)
	return err
}
