package sqlx

import (
	"context"
	"database/sql/driver"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
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
	if !c.IsValid() {
		return driver.ErrBadConn
	}
	return nil
}

func (c *pgDriverConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if c.conn == nil || c.conn.IsClosed() {
		return nil, driver.ErrBadConn
	}
	if c.simpleProtocol || len(args) == 0 {
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
		sqlText, err := interpolateSimpleSQL(query, args)
		if err != nil {
			return nil, err
		}
		mrr := c.conn.Exec(ctx, sqlText)
		results, err := mrr.ReadAll()
		if err != nil {
			return nil, err
		}
		if len(results) == 0 {
			return &pgBufferedRows{}, nil
		}
		last := results[len(results)-1]
		return newPGBufferedRows(last.FieldDescriptions, last.Rows), nil
	}

	params, err := encodePGParams(args)
	if err != nil {
		return nil, err
	}
	rr := c.conn.ExecParams(ctx, query, params, nil, nil, nil)
	res := rr.Read()
	if res.Err != nil {
		return nil, res.Err
	}
	return newPGBufferedRows(res.FieldDescriptions, res.Rows), nil
}

type pgDriverTx struct {
	conn *pgDriverConn
}

func (tx *pgDriverTx) Commit() error {
	_, err := tx.conn.ExecContext(context.Background(), "COMMIT", nil)
	return err
}

func (tx *pgDriverTx) Rollback() error {
	_, err := tx.conn.ExecContext(context.Background(), "ROLLBACK", nil)
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

type pgBufferedRows struct {
	cols   []string
	oids   []uint32
	rows   [][][]byte
	cursor int
}

func newPGBufferedRows(fields []pgconn.FieldDescription, rawRows [][][]byte) *pgBufferedRows {
	cols := make([]string, len(fields))
	oids := make([]uint32, len(fields))
	for i, f := range fields {
		cols[i] = f.Name
		oids[i] = f.DataTypeOID
	}
	return &pgBufferedRows{
		cols: cols,
		oids: oids,
		rows: rawRows,
	}
}

func (r *pgBufferedRows) Columns() []string { return r.cols }
func (r *pgBufferedRows) Close() error      { return nil }

func (r *pgBufferedRows) Next(dest []driver.Value) error {
	if r.cursor >= len(r.rows) {
		return io.EOF
	}
	row := r.rows[r.cursor]
	r.cursor++
	for i := range dest {
		if i >= len(row) || row[i] == nil {
			dest[i] = nil
			continue
		}
		val, err := decodePGTextValue(r.oids[i], row[i])
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
			out[i] = []byte(strconv.FormatFloat(v, 'g', -1, 64))
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
			lit[i] = strconv.FormatFloat(v, 'g', -1, 64)
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
	inSingle := false
	inDouble := false
	for i := 0; i < len(query); i++ {
		ch := query[i]
		if ch == '\'' && !inDouble {
			inSingle = !inSingle
			b.WriteByte(ch)
			continue
		}
		if ch == '"' && !inSingle {
			inDouble = !inDouble
			b.WriteByte(ch)
			continue
		}
		if ch == '$' && !inSingle && !inDouble && i+1 < len(query) && query[i+1] >= '1' && query[i+1] <= '9' {
			j := i + 1
			for j < len(query) && query[j] >= '0' && query[j] <= '9' {
				j++
			}
			idx, _ := strconv.Atoi(query[i+1 : j])
			if idx >= 1 && idx <= len(lit) {
				b.WriteString(lit[idx-1])
				i = j - 1
				continue
			}
		}
		b.WriteByte(ch)
	}
	return b.String(), nil
}

func valuesToNamed(vals []driver.Value) []driver.NamedValue {
	out := make([]driver.NamedValue, len(vals))
	for i, v := range vals {
		out[i] = driver.NamedValue{Ordinal: i + 1, Value: v}
	}
	return out
}
