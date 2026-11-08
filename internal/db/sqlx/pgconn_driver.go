package sqlx

import (
	"context"
	"database/sql/driver"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

const (
	pgOIDBool        uint32 = 16
	pgOIDBytea       uint32 = 17
	pgOIDInt8        uint32 = 20
	pgOIDInt2        uint32 = 21
	pgOIDInt4        uint32 = 23
	pgOIDFloat4      uint32 = 700
	pgOIDFloat8      uint32 = 701
	pgOIDDate        uint32 = 1082
	pgOIDTimestamp   uint32 = 1114
	pgOIDTimestamptz uint32 = 1184
)

// pgConnConnector implements database/sql/driver.Connector directly atop github.com/jackc/pgx/v5/pgconn.
// It uses unnamed extended-protocol ExecParams (PgBouncer / Supabase / Neon transaction-pooler safe)
// or explicit Simple Query Protocol without linking pgtype's 5,200+ unused geometry/multirange codecs.
type pgConnConnector struct {
	cfg            *pgconn.Config
	simpleProtocol bool
}

type pgConnDriver struct{}

func (d pgConnDriver) Open(name string) (driver.Conn, error) {
	cfg, err := pgconn.ParseConfig(name)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	c, err := pgconn.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &pgDriverConn{conn: c}, nil
}

func (c *pgConnConnector) Connect(ctx context.Context) (driver.Conn, error) {
	raw, err := pgconn.ConnectConfig(ctx, c.cfg.Copy())
	if err != nil {
		return nil, err
	}
	return &pgDriverConn{
		conn:           raw,
		simpleProtocol: c.simpleProtocol,
	}, nil
}

func (c *pgConnConnector) Driver() driver.Driver {
	return pgConnDriver{}
}

type pgDriverConn struct {
	conn           *pgconn.PgConn
	simpleProtocol bool
}

func (c *pgDriverConn) Prepare(query string) (driver.Stmt, error) {
	return &pgDriverStmt{conn: c, query: query}, nil
}

func (c *pgDriverConn) Close() error {
	if c.conn == nil || c.conn.IsClosed() {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return c.conn.Close(ctx)
}

func (c *pgDriverConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

func (c *pgDriverConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	beginSQL := "BEGIN"
	if opts.ReadOnly {
		beginSQL = "BEGIN READ ONLY"
	}
	if _, err := c.ExecContext(ctx, beginSQL, nil); err != nil {
		return nil, err
	}
	return &pgDriverTx{conn: c}, nil
}

func (c *pgDriverConn) Ping(ctx context.Context) error {
	if c.conn == nil || c.conn.IsClosed() {
		return driver.ErrBadConn
	}
	return c.conn.Ping(ctx)
}

func (c *pgDriverConn) IsValid() bool {
	return c.conn != nil && !c.conn.IsClosed() && !c.conn.IsBusy()
}

func (c *pgDriverConn) ResetSession(_ context.Context) error {
	if !c.IsValid() || c.conn.TxStatus() != 'I' {
		return driver.ErrBadConn
	}
	return nil
}

func (c *pgDriverConn) requireStandardConformingStrings() error {
	if c.conn == nil {
		return driver.ErrBadConn
	}
	if c.conn.ParameterStatus("standard_conforming_strings") != "on" {
		return errors.New("sqlx: postgres simple-protocol parameter interpolation requires standard_conforming_strings=on")
	}
	return nil
}

func (c *pgDriverConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if c.conn == nil || c.conn.IsClosed() {
		return nil, driver.ErrBadConn
	}
	if c.simpleProtocol || len(args) == 0 {
		if len(args) > 0 {
			if err := c.requireStandardConformingStrings(); err != nil {
				return nil, err
			}
		}
		sqlText, err := interpolateSimpleSQL(query, args)
		if err != nil {
			return nil, err
		}
		mrr := c.conn.Exec(ctx, sqlText)
		results, err := mrr.ReadAll()
		if err != nil {
			return nil, err
		}
		var rowsAffected int64
		if len(results) > 0 {
			rowsAffected = results[len(results)-1].CommandTag.RowsAffected()
		}
		return driver.RowsAffected(rowsAffected), nil
	}

	params, err := encodePGParams(args)
	if err != nil {
		return nil, err
	}
	rr := c.conn.ExecParams(ctx, query, params, nil, nil, nil)
	tag, err := rr.Close()
	if err != nil {
		return nil, err
	}
	return driver.RowsAffected(tag.RowsAffected()), nil
}

func (c *pgDriverConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if c.conn == nil || c.conn.IsClosed() {
		return nil, driver.ErrBadConn
	}
	if c.simpleProtocol {
		if len(args) > 0 {
			if err := c.requireStandardConformingStrings(); err != nil {
				return nil, err
			}
		}
		sqlText, err := interpolateSimpleSQL(query, args)
		if err != nil {
			return nil, err
		}
		mrr := c.conn.Exec(ctx, sqlText)
		for mrr.NextResult() {
			rr := mrr.ResultReader()
			fields := rr.FieldDescriptions()
			if len(fields) > 0 {
				return newPGRows(rr, mrr, fields), nil
			}
			tag, cErr := rr.Close()
			if cErr != nil {
				_ = mrr.Close()
				return nil, cErr
			}
			_ = tag
		}
		if err := mrr.Close(); err != nil {
			return nil, err
		}
		return &pgRows{closed: true}, nil
	}

	params, err := encodePGParams(args)
	if err != nil {
		return nil, err
	}
	rr := c.conn.ExecParams(ctx, query, params, nil, nil, nil)
	fields := rr.FieldDescriptions()
	if len(fields) == 0 {
		tag, err := rr.Close()
		if err != nil {
			return nil, err
		}
		return &pgRows{closed: true, commandTag: tag}, nil
	}
	return newPGRows(rr, nil, fields), nil
}

type pgDriverTx struct {
	conn *pgDriverConn
}

func (tx *pgDriverTx) Commit() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := tx.conn.ExecContext(ctx, "COMMIT", nil)
	return err
}

func (tx *pgDriverTx) Rollback() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := tx.conn.ExecContext(ctx, "ROLLBACK", nil)
	return err
}

type pgDriverStmt struct {
	conn  *pgDriverConn
	query string
}

func (s *pgDriverStmt) Close() error  { return nil }
func (s *pgDriverStmt) NumInput() int { return -1 }

func (s *pgDriverStmt) Exec(args []driver.Value) (driver.Result, error) {
	return s.conn.ExecContext(context.Background(), s.query, valuesToNamed(args))
}

func (s *pgDriverStmt) Query(args []driver.Value) (driver.Rows, error) {
	return s.conn.QueryContext(context.Background(), s.query, valuesToNamed(args))
}

type pgRows struct {
	cols       []string
	oids       []uint32
	rr         *pgconn.ResultReader
	mrr        *pgconn.MultiResultReader
	commandTag pgconn.CommandTag
	closeErr   error
	closed     bool
}

func newPGRows(rr *pgconn.ResultReader, mrr *pgconn.MultiResultReader, fields []pgconn.FieldDescription) *pgRows {
	cols := make([]string, len(fields))
	oids := make([]uint32, len(fields))
	for i, f := range fields {
		cols[i] = f.Name
		oids[i] = f.DataTypeOID
	}
	return &pgRows{
		cols: cols,
		oids: oids,
		rr:   rr,
		mrr:  mrr,
	}
}

func (r *pgRows) Columns() []string { return r.cols }

func (r *pgRows) Close() error {
	return r.closeReader()
}

func (r *pgRows) CommandTag() pgconn.CommandTag {
	return r.commandTag
}

func (r *pgRows) closeReader() error {
	if r.closed {
		return r.closeErr
	}
	r.closed = true
	var firstErr error
	if r.rr != nil {
		tag, err := r.rr.Close()
		r.commandTag = tag
		r.rr = nil
		if err != nil {
			firstErr = err
		}
	}
	if r.mrr != nil {
		if err := r.mrr.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		r.mrr = nil
	}
	r.closeErr = firstErr
	return r.closeErr
}

func (r *pgRows) Next(dest []driver.Value) error {
	if r.closed || r.rr == nil {
		return io.EOF
	}
	if !r.rr.NextRow() {
		if err := r.closeReader(); err != nil {
			return err
		}
		return io.EOF
	}
	row := r.rr.Values()
	for i := range dest {
		if i >= len(row) || row[i] == nil {
			dest[i] = nil
			continue
		}
		// Copy column bytes out of pgconn's reusable buffer before decoding.
		rawCopy := make([]byte, len(row[i]))
		copy(rawCopy, row[i])
		var oid uint32
		if i < len(r.oids) {
			oid = r.oids[i]
		}
		val, err := decodePGTextValue(oid, rawCopy)
		if err != nil {
			return err
		}
		dest[i] = val
	}
	return nil
}

func encodePGParams(args []driver.NamedValue) ([][]byte, error) {
	out := make([][]byte, len(args))
	for i, a := range args {
		if a.Value == nil {
			out[i] = nil
			continue
		}
		switch v := a.Value.(type) {
		case int64:
			out[i] = []byte(strconv.FormatInt(v, 10))
		case float64:
			switch {
			case math.IsNaN(v):
				out[i] = []byte("NaN")
			case math.IsInf(v, 1):
				out[i] = []byte("Infinity")
			case math.IsInf(v, -1):
				out[i] = []byte("-Infinity")
			default:
				out[i] = []byte(strconv.FormatFloat(v, 'g', -1, 64))
			}
		case bool:
			if v {
				out[i] = []byte("true")
			} else {
				out[i] = []byte("false")
			}
		case []byte:
			buf := make([]byte, 2+len(v)*2)
			buf[0], buf[1] = '\\', 'x'
			hex.Encode(buf[2:], v)
			out[i] = buf
		case string:
			out[i] = []byte(v)
		case time.Time:
			out[i] = []byte(v.UTC().Format(time.RFC3339Nano))
		default:
			return nil, fmt.Errorf("sqlx: unsupported postgres param type %T", a.Value)
		}
	}
	return out, nil
}

func decodePGTextValue(oid uint32, raw []byte) (driver.Value, error) {
	s := string(raw)
	switch oid {
	case pgOIDBool:
		return s == "t" || s == "true" || s == "1", nil
	case pgOIDInt2, pgOIDInt4, pgOIDInt8:
		return strconv.ParseInt(s, 10, 64)
	case pgOIDFloat4, pgOIDFloat8:
		return strconv.ParseFloat(s, 64)
	case pgOIDBytea:
		if strings.HasPrefix(s, `\x`) {
			return hex.DecodeString(s[2:])
		}
		cp := make([]byte, len(raw))
		copy(cp, raw)
		return cp, nil
	case pgOIDDate, pgOIDTimestamp, pgOIDTimestamptz:
		for _, layout := range []string{
			"2006-01-02 15:04:05.999999999-07:00",
			"2006-01-02 15:04:05.999999999-07",
			"2006-01-02 15:04:05.999999999+00",
			"2006-01-02 15:04:05.999999999",
			time.RFC3339Nano,
			"2006-01-02",
		} {
			if tm, err := time.Parse(layout, s); err == nil {
				return tm, nil
			}
		}
		return s, nil
	default:
		return s, nil
	}
}

func interpolateSimpleSQL(query string, args []driver.NamedValue) (string, error) {
	if len(args) == 0 {
		return query, nil
	}
	lit := make([]string, len(args))
	for i, a := range args {
		if a.Value == nil {
			lit[i] = "NULL"
			continue
		}
		switch v := a.Value.(type) {
		case int64:
			lit[i] = strconv.FormatInt(v, 10)
		case float64:
			switch {
			case math.IsNaN(v):
				lit[i] = "'NaN'::float8"
			case math.IsInf(v, 1):
				lit[i] = "'Infinity'::float8"
			case math.IsInf(v, -1):
				lit[i] = "'-Infinity'::float8"
			default:
				lit[i] = strconv.FormatFloat(v, 'g', -1, 64)
			}
		case bool:
			if v {
				lit[i] = "TRUE"
			} else {
				lit[i] = "FALSE"
			}
		case []byte:
			lit[i] = `'\x` + hex.EncodeToString(v) + `'::bytea`
		case string:
			if strings.IndexByte(v, 0) >= 0 {
				return "", errors.New("sqlx: NUL byte forbidden in postgres string literal")
			}
			lit[i] = "'" + strings.ReplaceAll(v, "'", "''") + "'"
		case time.Time:
			lit[i] = "'" + v.UTC().Format(time.RFC3339Nano) + "'"
		default:
			return "", fmt.Errorf("sqlx: unsupported simple-protocol arg %T", a.Value)
		}
	}

	var b strings.Builder
	b.Grow(len(query) + 32)
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

		// 2. Nested block comment: /* ... */
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

		// 3. Single-quoted literal: '...'
		if ch == '\'' {
			isEscapeStr := i > 0 && (query[i-1] == 'E' || query[i-1] == 'e') &&
				(i == 1 || !isSimpleSQLIdentByte(query[i-2]))
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

		// 5. Dollar-quoted string literal: $$...$$ or $tag$...$tag$
		if ch == '$' {
			if tag, ok := matchSimplePGDollarTag(query[i:]); ok {
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
			if i+1 < len(query) && query[i+1] >= '1' && query[i+1] <= '9' {
				j := i + 1
				for j < len(query) && query[j] >= '0' && query[j] <= '9' {
					j++
				}
				if j == len(query) || !isSimpleSQLIdentByte(query[j]) {
					idx, _ := strconv.Atoi(query[i+1 : j])
					if idx >= 1 && idx <= len(lit) {
						b.WriteString(lit[idx-1])
						i = j - 1
						continue
					}
				}
			}
		}

		b.WriteByte(ch)
	}
	return b.String(), nil
}

func isSimpleSQLIdentByte(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') || b == '_' || b >= 0x80
}

func matchSimplePGDollarTag(s string) (string, bool) {
	if len(s) < 2 || s[0] != '$' {
		return "", false
	}
	if s[1] == '$' {
		return "$$", true
	}
	first := s[1]
	if !((first >= 'a' && first <= 'z') || (first >= 'A' && first <= 'Z') || first == '_' || first >= 0x80) {
		return "", false
	}
	for j := 2; j < len(s); j++ {
		c := s[j]
		if c == '$' {
			return s[:j+1], true
		}
		if !isSimpleSQLIdentByte(c) {
			return "", false
		}
	}
	return "", false
}

func valuesToNamed(vals []driver.Value) []driver.NamedValue {
	out := make([]driver.NamedValue, len(vals))
	for i, v := range vals {
		out[i] = driver.NamedValue{Ordinal: i + 1, Value: v}
	}
	return out
}
