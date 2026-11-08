package dialect

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// TypeMap abstracts physical SQL DDL column types across SQLite, PostgreSQL, and MySQL/TiDB.
type TypeMap struct {
	Varchar32   string
	Varchar64   string
	Varchar128  string
	Varchar255  string
	Varchar512  string
	Text        string
	LongText    string
	Int         string
	BigInt      string
	Bool        string
	Blob        string
	BoolTrueLit string
	BoolFalseLit string
}

// Dialect defines the cross-database SQL adaptation contract.
type Dialect interface {
	Name() string
	Rebind(query string) string
	UpsertSQL(table string, cols []string, conflictCols []string, updateCols []string) string
	PaginateSQL(baseQuery string, limit int, offset int) (string, []any)
	Types() TypeMap
	QuoteIdent(ident string) string
}

// NormalizePage clamps page (>=1) and pageSize (1..maxSize) and returns (limit, offset).
func NormalizePage(page int, pageSize int, defaultSize int, maxSize int) (limit int, offset int) {
	if defaultSize <= 0 {
		defaultSize = 20
	}
	if maxSize <= 0 {
		maxSize = 100
	}
	if page < 1 {
		page = 1
	}
	if pageSize <= 0 {
		pageSize = defaultSize
	}
	if pageSize > maxSize {
		pageSize = maxSize
	}
	return pageSize, (page - 1) * pageSize
}

// For returns the Dialect implementation for "sqlite", "postgres", or "mysql".
func For(driver string) (Dialect, error) {
	switch strings.ToLower(strings.TrimSpace(driver)) {
	case "sqlite", "sqlite3":
		return sqliteDialect{}, nil
	case "postgres", "postgresql", "pg", "pgx":
		return postgresDialect{}, nil
	case "mysql", "mariadb", "tidb":
		return mysqlDialect{}, nil
	default:
		return nil, fmt.Errorf("dialect: unsupported database driver %q", driver)
	}
}

type sqliteDialect struct{}

func (sqliteDialect) Name() string               { return "sqlite" }
func (sqliteDialect) Rebind(query string) string { return query }
func (sqliteDialect) QuoteIdent(id string) string {
	return `"` + strings.ReplaceAll(id, `"`, `""`) + `"`
}
func (sqliteDialect) Types() TypeMap {
	return TypeMap{
		Varchar32:    " TEXT",
		Varchar64:    " TEXT",
		Varchar128:   " TEXT",
		Varchar255:   " TEXT",
		Varchar512:   " TEXT",
		Text:         " TEXT",
		LongText:     " TEXT",
		Int:          " INTEGER",
		BigInt:       " INTEGER",
		Bool:         " INTEGER",
		Blob:         " BLOB",
		BoolTrueLit:  "1",
		BoolFalseLit: "0",
	}
}
func (d sqliteDialect) UpsertSQL(table string, cols []string, conflictCols []string, updateCols []string) string {
	return buildConflictUpsert(d, table, cols, conflictCols, updateCols)
}
func (d sqliteDialect) PaginateSQL(baseQuery string, limit int, offset int) (string, []any) {
	return strings.TrimRight(strings.TrimSpace(baseQuery), ";") + " LIMIT ? OFFSET ?", []any{limit, offset}
}

type postgresDialect struct{}

var (
	pgRebindCache sync.Map // map[string]string
	pgRebindCount atomic.Int32
)

const maxPGRebindCacheEntries = 2048

func (postgresDialect) Name() string { return "postgres" }
func (postgresDialect) QuoteIdent(id string) string {
	return `"` + strings.ReplaceAll(id, `"`, `""`) + `"`
}
func (postgresDialect) Types() TypeMap {
	return TypeMap{
		Varchar32:    " VARCHAR(32)",
		Varchar64:    " VARCHAR(64)",
		Varchar128:   " VARCHAR(128)",
		Varchar255:   " VARCHAR(255)",
		Varchar512:   " VARCHAR(512)",
		Text:         " TEXT",
		LongText:     " TEXT",
		Int:          " INTEGER",
		BigInt:       " BIGINT",
		Bool:         " BOOLEAN",
		Blob:         " BYTEA",
		BoolTrueLit:  "TRUE",
		BoolFalseLit: "FALSE",
	}
}

// Rebind rewrites ? placeholders to PostgreSQL $1, $2, ... while safely skipping
// single-line comments (--...\n), nested block comments (/*...*/), single-quoted string
// literals ('...'), double-quoted identifiers ("..."), and dollar-quoted literals ($tag$...$tag$).
func (postgresDialect) Rebind(query string) string {
	if strings.IndexByte(query, '?') < 0 {
		return query
	}
	if len(query) <= 4096 {
		if cached, ok := pgRebindCache.Load(query); ok {
			return cached.(string)
		}
	}
	var b strings.Builder
	b.Grow(len(query) + 16)

	argIdx := 1
	for i := 0; i < len(query); i++ {
		ch := query[i]

		// 1. Single-line comment: -- ... \n
		if ch == '-' && i+1 < len(query) && query[i+1] == '-' {
			nl := strings.IndexByte(query[i:], '\n')
			if nl < 0 {
				b.WriteString(query[i:])
				break
			}
			b.WriteString(query[i : i+nl+1])
			i += nl
			continue
		}

		// 2. Block comment (supports PostgreSQL nested /* ... */ comments)
		if ch == '/' && i+1 < len(query) && query[i+1] == '*' {
			depth := 1
			b.WriteString("/*")
			i += 2
			for i < len(query) && depth > 0 {
				if i+1 < len(query) && query[i] == '/' && query[i+1] == '*' {
					depth++
					b.WriteString("/*")
					i += 2
					continue
				}
				if i+1 < len(query) && query[i] == '*' && query[i+1] == '/' {
					depth--
					b.WriteString("*/")
					i += 2
					continue
				}
				b.WriteByte(query[i])
				i++
			}
			i--
			continue
		}

		// 3. Single-quoted literal: '...' (with '' escapes, plus E'...' backslash escapes)
		if ch == '\'' {
			isEscapeStr := i > 0 && (query[i-1] == 'E' || query[i-1] == 'e') &&
				(i == 1 || !isSQLIdentByte(query[i-2]))
			b.WriteByte('\'')
			i++
			for i < len(query) {
				c := query[i]
				b.WriteByte(c)
				if isEscapeStr && c == '\\' && i+1 < len(query) {
					i++
					b.WriteByte(query[i])
					i++
					continue
				}
				if c == '\'' {
					if i+1 < len(query) && query[i+1] == '\'' {
						b.WriteByte('\'')
						i += 2
						continue
					}
					break
				}
				i++
			}
			continue
		}

		// 4. Double-quoted identifier: "..."
		if ch == '"' {
			b.WriteByte('"')
			i++
			for i < len(query) {
				c := query[i]
				b.WriteByte(c)
				if c == '"' {
					if i+1 < len(query) && query[i+1] == '"' {
						b.WriteByte('"')
						i += 2
						continue
					}
					break
				}
				i++
			}
			continue
		}

		// 5. PostgreSQL dollar-quoted string literal: $$...$$ or $tag$...$tag$
		if ch == '$' {
			if tag, ok := matchPGDollarQuoteTag(query[i:]); ok {
				endRel := strings.Index(query[i+len(tag):], tag)
				if endRel < 0 {
					b.WriteString(query[i:])
					break
				}
				totalLen := len(tag) + endRel + len(tag)
				b.WriteString(query[i : i+totalLen])
				i += totalLen - 1
				continue
			}
		}

		if ch == '?' {
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(argIdx))
			argIdx++
		} else {
			b.WriteByte(ch)
		}
	}

	out := b.String()
	if len(query) <= 4096 && pgRebindCount.Load() < maxPGRebindCacheEntries {
		if _, loaded := pgRebindCache.LoadOrStore(query, out); !loaded {
			pgRebindCount.Add(1)
		}
	}
	return out
}

func isSQLIdentByte(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') || b == '_' || b >= 0x80
}

func matchPGDollarQuoteTag(s string) (string, bool) {
	if len(s) < 2 || s[0] != '$' {
		return "", false
	}
	if s[1] == '$' {
		return "$$", true
	}
	// Tag cannot start with an ASCII digit (otherwise $1 is a positional parameter).
	first := s[1]
	if !((first >= 'a' && first <= 'z') || (first >= 'A' && first <= 'Z') || first == '_' || first >= 0x80) {
		return "", false
	}
	for j := 2; j < len(s); j++ {
		c := s[j]
		if c == '$' {
			return s[:j+1], true
		}
		if !isSQLIdentByte(c) {
			return "", false
		}
	}
	return "", false
}

func (d postgresDialect) UpsertSQL(table string, cols []string, conflictCols []string, updateCols []string) string {
	return buildConflictUpsert(d, table, cols, conflictCols, updateCols)
}
func (d postgresDialect) PaginateSQL(baseQuery string, limit int, offset int) (string, []any) {
	q := strings.TrimRight(strings.TrimSpace(baseQuery), ";") + " LIMIT ? OFFSET ?"
	return d.Rebind(q), []any{limit, offset}
}

type mysqlDialect struct{}

func (mysqlDialect) Name() string               { return "mysql" }
func (mysqlDialect) Rebind(query string) string { return query }
func (mysqlDialect) QuoteIdent(id string) string {
	return "`" + strings.ReplaceAll(id, "`", "``") + "`"
}
func (mysqlDialect) Types() TypeMap {
	return TypeMap{
		Varchar32:    " VARCHAR(32)",
		Varchar64:    " VARCHAR(64)",
		Varchar128:   " VARCHAR(128)",
		Varchar255:   " VARCHAR(255)",
		Varchar512:   " VARCHAR(512)",
		Text:         " TEXT",
		LongText:     " LONGTEXT",
		Int:          " INT",
		BigInt:       " BIGINT",
		Bool:         " TINYINT(1)",
		Blob:         " LONGBLOB",
		BoolTrueLit:  "1",
		BoolFalseLit: "0",
	}
}

func (d mysqlDialect) PaginateSQL(baseQuery string, limit int, offset int) (string, []any) {
	return strings.TrimRight(strings.TrimSpace(baseQuery), ";") + " LIMIT ? OFFSET ?", []any{limit, offset}
}

func (d mysqlDialect) UpsertSQL(table string, cols []string, conflictCols []string, updateCols []string) string {
	quotedCols := make([]string, len(cols))
	placeholders := make([]string, len(cols))
	for i, c := range cols {
		quotedCols[i] = d.QuoteIdent(c)
		placeholders[i] = "?"
	}
	base := fmt.Sprintf(
		"INSERT INTO %s (%s) VALUES (%s)",
		d.QuoteIdent(table),
		strings.Join(quotedCols, ", "),
		strings.Join(placeholders, ", "),
	)
	if len(updateCols) == 0 {
		anchor := cols[0]
		if len(conflictCols) > 0 {
			anchor = conflictCols[0]
		}
		qAnchor := d.QuoteIdent(anchor)
		return base + fmt.Sprintf(" ON DUPLICATE KEY UPDATE %s = %s", qAnchor, qAnchor)
	}
	updates := make([]string, len(updateCols))
	for i, u := range updateCols {
		qu := d.QuoteIdent(u)
		updates[i] = fmt.Sprintf("%s = VALUES(%s)", qu, qu)
	}
	return base + " ON DUPLICATE KEY UPDATE " + strings.Join(updates, ", ")
}

func buildConflictUpsert(d Dialect, table string, cols []string, conflictCols []string, updateCols []string) string {
	quotedCols := make([]string, len(cols))
	placeholders := make([]string, len(cols))
	for i, c := range cols {
		quotedCols[i] = d.QuoteIdent(c)
		placeholders[i] = "?"
	}
	quotedConflicts := make([]string, len(conflictCols))
	for i, c := range conflictCols {
		quotedConflicts[i] = d.QuoteIdent(c)
	}
	base := fmt.Sprintf(
		"INSERT INTO %s (%s) VALUES (%s) ON CONFLICT (%s)",
		d.QuoteIdent(table),
		strings.Join(quotedCols, ", "),
		strings.Join(placeholders, ", "),
		strings.Join(quotedConflicts, ", "),
	)
	if len(updateCols) == 0 {
		sql := base + " DO NOTHING"
		return d.Rebind(sql)
	}
	updates := make([]string, len(updateCols))
	for i, u := range updateCols {
		qu := d.QuoteIdent(u)
		updates[i] = fmt.Sprintf("%s = EXCLUDED.%s", qu, qu)
	}
	sql := base + " DO UPDATE SET " + strings.Join(updates, ", ")
	return d.Rebind(sql)
}
