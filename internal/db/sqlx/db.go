package sqlx

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/pgconn"
	_ "modernc.org/sqlite"

	"github.com/Willh92/Volo-go/internal/db/dialect"
)

// Queryer is implemented by both *Store (read-only path) and *Tx (transactional path).
type Queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	Dialect() dialect.Dialect
}

// Executor extends Queryer with write capability.
type Executor interface {
	Queryer
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// Options configures database connection opening across SQLite, PostgreSQL, and MySQL.
type Options struct {
	Driver         string
	DSN            string
	SimpleProtocol bool
	MaxOpenConns   int
	MaxIdleConns   int
	ConnLifetime   time.Duration
}

// Store manages dialect-aware read/write connection pools, enforcing:
// 1. Read-only queries run with zero DB write side-effects (enforced via query_only=ON on SQLite ReadDB).
// 2. Multi-statement writes execute inside WithTx with guaranteed rollback on error or panic.
// 3. Case-insensitive struct column scanning across SQLite, PostgreSQL, MySQL, MariaDB, and TiDB.
type Store struct {
	dialect     dialect.Dialect
	writeDB     *sql.DB
	readDB      *sql.DB
	splitPools  bool
	sqliteInMem bool
	closeOnce   sync.Once
}

// Open initializes the pure-Go database pool(s) for the configured driver.
func Open(ctx context.Context, opts Options) (*Store, error) {
	d, err := dialect.For(opts.Driver)
	if err != nil {
		return nil, err
	}

	switch d.Name() {
	case "sqlite":
		return openSQLite(ctx, d, opts)
	case "postgres":
		return openPostgres(ctx, d, opts)
	case "mysql":
		return openMySQL(ctx, d, opts)
	default:
		return nil, fmt.Errorf("sqlx: unsupported dialect %q", d.Name())
	}
}

func openSQLite(ctx context.Context, d dialect.Dialect, opts Options) (*Store, error) {
	dsn := strings.TrimSpace(opts.DSN)
	isMem := dsn == ":memory:" || strings.Contains(dsn, "mode=memory")

	if !isMem {
		cleanPath := strings.TrimPrefix(dsn, "file:")
		if idx := strings.IndexByte(cleanPath, '?'); idx >= 0 {
			cleanPath = cleanPath[:idx]
		}
		if dir := filepath.Dir(cleanPath); dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0o750); err != nil {
				return nil, fmt.Errorf("sqlx: create sqlite parent dir %q: %w", dir, err)
			}
		}
	}

	writeURI := buildSQLiteURI(dsn, false, isMem)
	wdb, err := sql.Open("sqlite", writeURI)
	if err != nil {
		return nil, fmt.Errorf("sqlx: open sqlite writer: %w", err)
	}
	// Single writer eliminates SQLITE_BUSY write lock contention completely.
	wdb.SetMaxOpenConns(1)
	wdb.SetMaxIdleConns(1)
	wdb.SetConnMaxLifetime(0)

	if err := wdb.PingContext(ctx); err != nil {
		_ = wdb.Close()
		return nil, fmt.Errorf("sqlx: ping sqlite writer: %w", err)
	}

	if isMem {
		return &Store{
			dialect:     d,
			writeDB:     wdb,
			readDB:      wdb,
			splitPools:  false,
			sqliteInMem: true,
		}, nil
	}

	readURI := buildSQLiteURI(dsn, true, false)
	rdb, err := sql.Open("sqlite", readURI)
	if err != nil {
		_ = wdb.Close()
		return nil, fmt.Errorf("sqlx: open sqlite reader: %w", err)
	}
	readers := runtime.NumCPU()
	if readers < 4 {
		readers = 4
	}
	rdb.SetMaxOpenConns(readers)
	rdb.SetMaxIdleConns(readers)
	rdb.SetConnMaxLifetime(0)

	if err := rdb.PingContext(ctx); err != nil {
		_ = rdb.Close()
		_ = wdb.Close()
		return nil, fmt.Errorf("sqlx: ping sqlite reader: %w", err)
	}

	return &Store{
		dialect:    d,
		writeDB:    wdb,
		readDB:     rdb,
		splitPools: true,
	}, nil
}

func buildSQLiteURI(raw string, readOnly bool, isMem bool) string {
	if isMem {
		if strings.Contains(raw, "?") {
			return raw + "&_pragma=foreign_keys(ON)&_pragma=busy_timeout(5000)"
		}
		if raw == ":memory:" {
			return "file::memory:?cache=shared&_pragma=foreign_keys(ON)&_pragma=busy_timeout(5000)"
		}
		return raw + "?_pragma=foreign_keys(ON)&_pragma=busy_timeout(5000)"
	}

	pathPart := strings.TrimPrefix(raw, "file:")
	var existingQuery string
	if idx := strings.IndexByte(pathPart, '?'); idx >= 0 {
		existingQuery = pathPart[idx+1:]
		pathPart = pathPart[:idx]
	}
	q, _ := url.ParseQuery(existingQuery)
	pragmas := []string{
		"journal_mode(WAL)",
		"synchronous(NORMAL)",
		"busy_timeout(5000)",
		"foreign_keys(ON)",
		"temp_store(MEMORY)",
	}
	if readOnly {
		pragmas = append(pragmas, "query_only(ON)")
	} else {
		q.Set("_txlock", "immediate")
	}
	for _, p := range pragmas {
		q.Add("_pragma", p)
	}
	return "file:" + pathPart + "?" + q.Encode()
}

func openPostgres(ctx context.Context, d dialect.Dialect, opts Options) (*Store, error) {
	if strings.TrimSpace(opts.DSN) == "" {
		return nil, errors.New("sqlx: VOLO_DB_DSN is required for postgres driver")
	}
	cfg, err := pgconn.ParseConfig(opts.DSN)
	if err != nil {
		return nil, fmt.Errorf("sqlx: parse postgres DSN: %w", err)
	}
	db := sql.OpenDB(&pgConnConnector{
		cfg:            cfg,
		simpleProtocol: opts.SimpleProtocol,
	})
	applyPoolSettings(db, opts)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sqlx: ping postgres: %w", err)
	}
	return &Store{dialect: d, writeDB: db, readDB: db}, nil
}

func openMySQL(ctx context.Context, d dialect.Dialect, opts Options) (*Store, error) {
	dsn := ensureMySQLParams(opts.DSN)
	if dsn == "" {
		return nil, errors.New("sqlx: VOLO_DB_DSN is required for mysql driver")
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("sqlx: open mysql: %w", err)
	}
	applyPoolSettings(db, opts)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sqlx: ping mysql: %w", err)
	}
	return &Store{dialect: d, writeDB: db, readDB: db}, nil
}

func ensureMySQLParams(dsn string) string {
	dsn = strings.TrimSpace(dsn)
	if dsn == "" {
		return ""
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	var extras []string
	if !strings.Contains(dsn, "charset=") {
		extras = append(extras, "charset=utf8mb4")
	}
	if !strings.Contains(dsn, "parseTime=") {
		extras = append(extras, "parseTime=true")
	}
	if !strings.Contains(dsn, "loc=") {
		extras = append(extras, "loc=UTC")
	}
	if len(extras) == 0 {
		return dsn
	}
	return dsn + sep + strings.Join(extras, "&")
}

func applyPoolSettings(db *sql.DB, opts Options) {
	maxOpen := opts.MaxOpenConns
	if maxOpen <= 0 {
		maxOpen = 25
	}
	maxIdle := opts.MaxIdleConns
	if maxIdle <= 0 {
		maxIdle = 5
	}
	life := opts.ConnLifetime
	if life <= 0 {
		life = 30 * time.Minute
	}
	db.SetMaxOpenConns(maxOpen)
	db.SetMaxIdleConns(maxIdle)
	db.SetConnMaxLifetime(life)
}

func (s *Store) Dialect() dialect.Dialect { return s.dialect }
func (s *Store) WriteDB() *sql.DB         { return s.writeDB }
func (s *Store) ReadDB() *sql.DB          { return s.readDB }

// Ping verifies both read and write database pools are healthy.
func (s *Store) Ping(ctx context.Context) error {
	if err := s.readDB.PingContext(ctx); err != nil {
		return err
	}
	if s.splitPools {
		return s.writeDB.PingContext(ctx)
	}
	return nil
}

// Close performs a clean WAL checkpoint (if SQLite file DB) and closes all database pools.
func (s *Store) Close() error {
	var firstErr error
	s.closeOnce.Do(func() {
		if s.dialect.Name() == "sqlite" && !s.sqliteInMem {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_, _ = s.writeDB.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)")
			cancel()
		}
		if s.splitPools {
			if err := s.readDB.Close(); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		if err := s.writeDB.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	})
	return firstErr
}

// QueryContext executes a read-only query against ReadDB with automatic ? -> $N rebinding.
func (s *Store) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return s.readDB.QueryContext(ctx, s.dialect.Rebind(query), args...)
}

// QueryRowContext executes a single-row read-only query against ReadDB with automatic ? -> $N rebinding.
func (s *Store) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return s.readDB.QueryRowContext(ctx, s.dialect.Rebind(query), args...)
}

// ExecContext executes a single atomic write statement against WriteDB with automatic ? -> $N rebinding.
func (s *Store) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return s.writeDB.ExecContext(ctx, s.dialect.Rebind(query), args...)
}

// Tx wraps *sql.Tx with dialect-aware placeholder rebinding.
type Tx struct {
	raw     *sql.Tx
	dialect dialect.Dialect
}

func (t *Tx) Dialect() dialect.Dialect { return t.dialect }

func (t *Tx) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return t.raw.ExecContext(ctx, t.dialect.Rebind(query), args...)
}

func (t *Tx) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return t.raw.QueryContext(ctx, t.dialect.Rebind(query), args...)
}

func (t *Tx) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return t.raw.QueryRowContext(ctx, t.dialect.Rebind(query), args...)
}

// WithTx runs fn inside a database transaction on WriteDB.
// If fn returns an error or panics, the transaction is rolled back immediately.
func (s *Store) WithTx(ctx context.Context, fn func(tx *Tx) error) (err error) {
	rawTx, err := s.writeDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlx: begin tx: %w", err)
	}
	wrapped := &Tx{raw: rawTx, dialect: s.dialect}
	defer func() {
		if p := recover(); p != nil {
			_ = rawTx.Rollback()
			panic(p)
		}
		if err != nil {
			_ = rawTx.Rollback()
		}
	}()

	if err = fn(wrapped); err != nil {
		return err
	}
	if commitErr := rawTx.Commit(); commitErr != nil {
		err = fmt.Errorf("sqlx: commit tx: %w", commitErr)
		return err
	}
	return nil
}

// Select executes a query and scans all rows into destSlicePtr (*[]T) using case-insensitive
// matching against `db:"column_name"` struct tags or lowercase field names.
func Select[T any](ctx context.Context, q Queryer, query string, args ...any) ([]T, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	lowerCols := make([]string, len(cols))
	for i, c := range cols {
		lowerCols[i] = strings.ToLower(c)
	}

	var zero T
	rt := reflect.TypeOf(zero)
	if rt == nil || rt.Kind() != reflect.Struct {
		return nil, errors.New("sqlx.Select requires struct element type T")
	}
	fieldMap := buildStructFieldIndex(rt)

	var out []T
	for rows.Next() {
		var item T
		rv := reflect.ValueOf(&item).Elem()
		scanTargets, postProcess := prepareScanTargets(rv, lowerCols, fieldMap)
		if err := rows.Scan(scanTargets...); err != nil {
			return nil, err
		}
		if err := postProcess(); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// Get executes a query and scans the first row into T, returning sql.ErrNoRows if empty.
func Get[T any](ctx context.Context, q Queryer, query string, args ...any) (T, error) {
	var zero T
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return zero, err
	}
	defer rows.Close()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return zero, err
		}
		return zero, sql.ErrNoRows
	}
	cols, err := rows.Columns()
	if err != nil {
		return zero, err
	}
	lowerCols := make([]string, len(cols))
	for i, c := range cols {
		lowerCols[i] = strings.ToLower(c)
	}

	rt := reflect.TypeOf(zero)
	if rt == nil || rt.Kind() != reflect.Struct {
		return zero, errors.New("sqlx.Get requires struct type T")
	}
	fieldMap := buildStructFieldIndex(rt)

	var item T
	rv := reflect.ValueOf(&item).Elem()
	scanTargets, postProcess := prepareScanTargets(rv, lowerCols, fieldMap)
	if err := rows.Scan(scanTargets...); err != nil {
		return zero, err
	}
	if err := postProcess(); err != nil {
		return zero, err
	}
	return item, rows.Err()
}

var fieldCache sync.Map // map[reflect.Type]map[string][]int

func buildStructFieldIndex(rt reflect.Type) map[string][]int {
	if cached, ok := fieldCache.Load(rt); ok {
		return cached.(map[string][]int)
	}
	idx := make(map[string][]int, rt.NumField())
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		if !f.IsExported() {
			continue
		}
		tag := f.Tag.Get("db")
		if tag == "-" {
			continue
		}
		if comma := strings.IndexByte(tag, ','); comma >= 0 {
			tag = tag[:comma]
		}
		name := strings.ToLower(strings.TrimSpace(tag))
		if name == "" {
			name = strings.ToLower(f.Name)
		}
		idx[name] = f.Index
	}
	fieldCache.Store(rt, idx)
	return idx
}

func prepareScanTargets(rv reflect.Value, lowerCols []string, fieldMap map[string][]int) ([]any, func() error) {
	targets := make([]any, len(lowerCols))
	var setters []func() error

	for i, col := range lowerCols {
		fIdx, ok := fieldMap[col]
		if !ok {
			var discard any
			targets[i] = &discard
			continue
		}
		fv := rv.FieldByIndex(fIdx)
		switch fv.Kind() {
		case reflect.Bool:
			// Cross-dialect bool scanner: handles SQLite INTEGER (0/1), MySQL TINYINT(1), and Postgres BOOLEAN.
			var raw any
			targets[i] = &raw
			fieldVal := fv
			setters = append(setters, func() error {
				b, err := coerceBool(raw)
				if err != nil {
					return err
				}
				fieldVal.SetBool(b)
				return nil
			})
		case reflect.String:
			var ns sql.NullString
			targets[i] = &ns
			fieldVal := fv
			setters = append(setters, func() error {
				if ns.Valid {
					fieldVal.SetString(ns.String)
				} else {
					fieldVal.SetString("")
				}
				return nil
			})
		case reflect.Int, reflect.Int64, reflect.Int32:
			var ni sql.NullInt64
			targets[i] = &ni
			fieldVal := fv
			setters = append(setters, func() error {
				if ni.Valid {
					fieldVal.SetInt(ni.Int64)
				} else {
					fieldVal.SetInt(0)
				}
				return nil
			})
		default:
			targets[i] = fv.Addr().Interface()
		}
	}

	return targets, func() error {
		for _, fn := range setters {
			if err := fn(); err != nil {
				return err
			}
		}
		return nil
	}
}

func coerceBool(v any) (bool, error) {
	switch x := v.(type) {
	case nil:
		return false, nil
	case bool:
		return x, nil
	case int64:
		return x != 0, nil
	case int:
		return x != 0, nil
	case []byte:
		return coerceBoolString(string(x))
	case string:
		return coerceBoolString(x)
	default:
		return false, fmt.Errorf("sqlx: cannot scan %T into bool", v)
	}
}

func coerceBoolString(s string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "0", "f", "false", "no", "off":
		return false, nil
	case "1", "t", "true", "yes", "on":
		return true, nil
	default:
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return false, fmt.Errorf("sqlx: invalid boolean string %q", s)
		}
		return n != 0, nil
	}
}
