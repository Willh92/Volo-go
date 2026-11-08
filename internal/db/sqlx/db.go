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
	"sync/atomic"
	"time"

	_ "github.com/go-sql-driver/mysql"
	_ "modernc.org/sqlite"

	"github.com/Willh92/Volo-go/internal/db/dialect"
)

var sqliteMemSeq atomic.Uint64

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

type storeState struct {
	dialect     dialect.Dialect
	writeDB     *sql.DB
	readDB      *sql.DB
	splitPools  bool
	sqliteInMem bool
	readOnly    bool
	closeOnce   sync.Once
}

func (st *storeState) close() error {
	if st == nil {
		return nil
	}
	var firstErr error
	st.closeOnce.Do(func() {
		if st.dialect != nil && st.dialect.Name() == "sqlite" && !st.sqliteInMem && !st.readOnly && st.writeDB != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_, _ = st.writeDB.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)")
			cancel()
		}
		if st.splitPools && st.readDB != nil {
			if err := st.readDB.Close(); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		if st.writeDB != nil {
			if err := st.writeDB.Close(); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	})
	return firstErr
}

// Store manages dialect-aware read/write connection pools, enforcing:
// 1. Read-only queries run with zero DB write side-effects (enforced via query_only=ON on SQLite ReadDB).
// 2. Multi-statement writes execute inside WithTx with guaranteed rollback on error or panic.
// 3. Case-insensitive struct column scanning across SQLite, PostgreSQL, MySQL, MariaDB, and TiDB.
type Store struct {
	state          atomic.Pointer[storeState]
	dataRev        atomic.Uint64
	sqliteWriteSem chan struct{}
	semOnce        sync.Once
}

func (s *Store) writeSem() chan struct{} {
	s.semOnce.Do(func() {
		if s.sqliteWriteSem == nil {
			s.sqliteWriteSem = make(chan struct{}, 1)
		}
	})
	return s.sqliteWriteSem
}

func (s *Store) acquireWriteLock(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	sem := s.writeSem()
	select {
	case sem <- struct{}{}:
		if err := ctx.Err(); err != nil {
			<-sem
			return err
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Store) releaseWriteLock() {
	<-s.writeSem()
}

func newStoreFromState(st *storeState) *Store {
	s := &Store{
		sqliteWriteSem: make(chan struct{}, 1),
	}
	s.writeSem()
	s.state.Store(st)
	s.dataRev.Store(1)
	return s
}

// Open initializes the pure-Go database pool(s) for the configured driver.
func Open(ctx context.Context, opts Options) (*Store, error) {
	d, err := dialect.For(opts.Driver)
	if err != nil {
		return nil, err
	}

	var st *Store
	switch d.Name() {
	case "sqlite":
		st, err = openSQLite(ctx, d, opts)
	case "postgres":
		st, err = openPostgres(ctx, d, opts)
	case "mysql":
		st, err = openMySQL(ctx, d, opts)
	default:
		return nil, fmt.Errorf("sqlx: unsupported dialect %q", d.Name())
	}
	if err != nil {
		return nil, err
	}
	st.dataRev.Store(1)
	return st, nil
}

// OpenReadOnly initializes a read-only database pool for inspecting or migrating an existing source database.
func OpenReadOnly(ctx context.Context, opts Options) (*Store, error) {
	d, err := dialect.For(opts.Driver)
	if err != nil {
		return nil, err
	}
	if opts.MaxOpenConns <= 0 {
		opts.MaxOpenConns = 2
	}
	if opts.MaxIdleConns <= 0 {
		opts.MaxIdleConns = 1
	}
	var st *Store
	switch d.Name() {
	case "sqlite":
		st, err = openSQLiteReadOnly(ctx, d, opts)
	case "postgres":
		st, err = openPostgres(ctx, d, opts)
	case "mysql":
		st, err = openMySQL(ctx, d, opts)
	default:
		return nil, fmt.Errorf("sqlx: unsupported dialect %q", d.Name())
	}
	if err != nil {
		return nil, err
	}
	if s := st.state.Load(); s != nil {
		s.readOnly = true
	}
	st.dataRev.Store(1)
	return st, nil
}

func openSQLiteReadOnly(ctx context.Context, d dialect.Dialect, opts Options) (*Store, error) {
	dsn := strings.TrimSpace(opts.DSN)
	if dsn == "" {
		return nil, errors.New("sqlx: sqlite DSN is required for read-only source")
	}
	isMem := dsn == ":memory:" || strings.Contains(dsn, "mode=memory") || strings.HasPrefix(dsn, "file::memory:")
	var uri string
	if isMem {
		uri = buildSQLiteURI(dsn, true, true)
	} else {
		pathPart := strings.TrimPrefix(dsn, "file:")
		if idx := strings.IndexByte(pathPart, '?'); idx >= 0 {
			pathPart = pathPart[:idx]
		}
		if strings.Contains(pathPart, "..") {
			return nil, errors.New("sqlx: sqlite path must not contain '..' traversal")
		}
		cleanPath := filepath.Clean(pathPart)
		info, err := os.Stat(cleanPath)
		if err != nil {
			return nil, fmt.Errorf("sqlx: source sqlite file not accessible: %w", err)
		}
		if info.IsDir() {
			return nil, errors.New("sqlx: source sqlite path points to a directory")
		}
		q := url.Values{}
		q.Set("mode", "ro")
		q.Add("_pragma", "query_only(ON)")
		q.Add("_pragma", "busy_timeout(5000)")
		q.Add("_pragma", "cache_size(-1024)")
		uri = "file:" + cleanPath + "?" + q.Encode()
	}
	rdb, err := sql.Open("sqlite", uri)
	if err != nil {
		return nil, fmt.Errorf("sqlx: open read-only sqlite: %w", err)
	}
	rdb.SetMaxOpenConns(2)
	rdb.SetMaxIdleConns(1)
	rdb.SetConnMaxIdleTime(30 * time.Second)
	if err := rdb.PingContext(ctx); err != nil {
		_ = rdb.Close()
		return nil, fmt.Errorf("sqlx: ping read-only sqlite: %w", err)
	}
	return newStoreFromState(&storeState{
		dialect:     d,
		writeDB:     rdb,
		readDB:      rdb,
		splitPools:  false,
		sqliteInMem: isMem,
		readOnly:    true,
	}), nil
}

func openSQLite(ctx context.Context, d dialect.Dialect, opts Options) (*Store, error) {
	dsn := strings.TrimSpace(opts.DSN)
	if dsn == ":memory:" {
		dsn = fmt.Sprintf("file:volo_mem_%d?mode=memory&cache=shared", sqliteMemSeq.Add(1))
	}
	isMem := strings.Contains(dsn, "mode=memory") || strings.HasPrefix(dsn, "file::memory:")
	isSharedMem := isMem && strings.Contains(dsn, "cache=shared")

	if !isMem {
		cleanPath := strings.TrimPrefix(dsn, "file:")
		if idx := strings.IndexByte(cleanPath, '?'); idx >= 0 {
			cleanPath = cleanPath[:idx]
		}
		if strings.Contains(cleanPath, "..") {
			return nil, errors.New("sqlx: sqlite path must not contain '..' traversal")
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
	// Single writer eliminates SQLITE_BUSY write lock contention and holds named shared-memory DB alive until Close().
	wdb.SetMaxOpenConns(1)
	wdb.SetMaxIdleConns(1)
	wdb.SetConnMaxIdleTime(0)
	wdb.SetConnMaxLifetime(0)

	if err := wdb.PingContext(ctx); err != nil {
		_ = wdb.Close()
		return nil, fmt.Errorf("sqlx: ping sqlite writer: %w", err)
	}

	if isMem && !isSharedMem {
		return newStoreFromState(&storeState{
			dialect:     d,
			writeDB:     wdb,
			readDB:      wdb,
			splitPools:  false,
			sqliteInMem: true,
		}), nil
	}

	readURI := buildSQLiteURI(dsn, true, isMem)
	rdb, err := sql.Open("sqlite", readURI)
	if err != nil {
		_ = wdb.Close()
		return nil, fmt.Errorf("sqlx: open sqlite reader: %w", err)
	}
	readers := min(4, max(2, runtime.NumCPU()))
	rdb.SetMaxOpenConns(readers)
	rdb.SetMaxIdleConns(1)
	rdb.SetConnMaxIdleTime(60 * time.Second)
	rdb.SetConnMaxLifetime(0)

	if err := rdb.PingContext(ctx); err != nil {
		_ = rdb.Close()
		_ = wdb.Close()
		return nil, fmt.Errorf("sqlx: ping sqlite reader: %w", err)
	}

	return newStoreFromState(&storeState{
		dialect:     d,
		writeDB:     wdb,
		readDB:      rdb,
		splitPools:  true,
		sqliteInMem: isMem,
	}), nil
}

func buildSQLiteURI(raw string, readOnly bool, isMem bool) string {
	if isMem {
		base := raw
		if base == ":memory:" {
			base = "file::memory:?cache=shared"
		}
		sep := "?"
		if strings.Contains(base, "?") {
			sep = "&"
		}
		suffix := "_pragma=foreign_keys(ON)&_pragma=busy_timeout(5000)"
		if readOnly {
			suffix += "&_pragma=query_only(ON)&_pragma=read_uncommitted(ON)"
		}
		return base + sep + suffix
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
		"cache_size(-1024)", // Bounded ~1 MiB page cache per active connection within < 25 MiB VmRSS budget
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
	cfg, simpleProto, err := parsePGConnConfig(opts.DSN, opts.SimpleProtocol)
	if err != nil {
		return nil, fmt.Errorf("sqlx: parse postgres DSN: %w", err)
	}
	db := sql.OpenDB(&pgConnConnector{
		cfg:            cfg,
		simpleProtocol: simpleProto,
	})
	applyPoolSettings(db, opts)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sqlx: ping postgres: %w", err)
	}
	return newStoreFromState(&storeState{dialect: d, writeDB: db, readDB: db}), nil
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
	return newStoreFromState(&storeState{dialect: d, writeDB: db, readDB: db}), nil
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

func (s *Store) Dialect() dialect.Dialect {
	if st := s.state.Load(); st != nil {
		return st.dialect
	}
	return nil
}

func (s *Store) WriteDB() *sql.DB {
	if st := s.state.Load(); st != nil {
		return st.writeDB
	}
	return nil
}

func (s *Store) ReadDB() *sql.DB {
	if st := s.state.Load(); st != nil {
		return st.readDB
	}
	return nil
}

// Swap atomically switches this Store's underlying pools and dialect to other's pools,
// increments DataRevision so all read caches invalidate immediately, and closes the previous pools.
func (s *Store) Swap(other *Store) {
	if s == nil || other == nil || s == other {
		return
	}
	next := other.state.Load()
	if next == nil {
		return
	}
	prev := s.state.Swap(next)
	s.dataRev.Add(1)
	if prev != nil && prev != next {
		_ = prev.close()
	}
}

// DataRevision returns a monotonically increasing sequence incremented whenever content/config
// rows mutate via ExecContext or WithTx. Read-side caches compare this in 1ns lock-free!
func (s *Store) DataRevision() uint64 {
	if s == nil {
		return 0
	}
	return s.dataRev.Load()
}

// BumpRevision explicitly increments DataRevision for external callers if needed.
func (s *Store) BumpRevision() {
	if s != nil {
		s.dataRev.Add(1)
	}
}

// Ping verifies both read and write database pools are healthy.
func (s *Store) Ping(ctx context.Context) error {
	st := s.state.Load()
	if st == nil {
		return errors.New("sqlx: store not initialized")
	}
	if err := st.readDB.PingContext(ctx); err != nil {
		return err
	}
	if st.splitPools {
		return st.writeDB.PingContext(ctx)
	}
	return nil
}

// Close performs a clean WAL checkpoint (if SQLite file DB) and closes all database pools.
func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	return s.state.Load().close()
}

// QueryContext executes a read-only query against ReadDB with automatic ? -> $N rebinding.
func (s *Store) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	st := s.state.Load()
	return st.readDB.QueryContext(ctx, st.dialect.Rebind(query), args...)
}

// QueryRowContext executes a single-row read-only query against ReadDB with automatic ? -> $N rebinding.
func (s *Store) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	st := s.state.Load()
	return st.readDB.QueryRowContext(ctx, st.dialect.Rebind(query), args...)
}

// ExecContext executes a single atomic write statement against WriteDB with automatic ? -> $N rebinding
// and bumps DataRevision so read-side snapshots invalidate immediately.
func (s *Store) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	st := s.state.Load()
	if st.dialect != nil && st.dialect.Name() == "sqlite" {
		if err := s.acquireWriteLock(ctx); err != nil {
			return nil, err
		}
		defer s.releaseWriteLock()
	}
	res, err := st.writeDB.ExecContext(ctx, st.dialect.Rebind(query), args...)
	if err == nil {
		s.dataRev.Add(1)
	}
	return res, err
}

// ExecBgContext executes a background telemetry/session-timestamp update on WriteDB without
// invalidating content/feed/chrome caches.
func (s *Store) ExecBgContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	st := s.state.Load()
	if st.dialect != nil && st.dialect.Name() == "sqlite" {
		if err := s.acquireWriteLock(ctx); err != nil {
			return nil, err
		}
		defer s.releaseWriteLock()
	}
	return st.writeDB.ExecContext(ctx, st.dialect.Rebind(query), args...)
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

// WithTx runs fn inside a database transaction on WriteDB and bumps DataRevision on commit.
// If fn returns an error or panics, the transaction is rolled back immediately.
func (s *Store) WithTx(ctx context.Context, fn func(tx *Tx) error) error {
	return s.runTx(ctx, true, fn)
}

// WithBgTx runs fn inside a database transaction on WriteDB for background counter flushes
// (PV/UV/view_count/hit_count) without invalidating content/feed/chrome snapshots.
func (s *Store) WithBgTx(ctx context.Context, fn func(tx *Tx) error) error {
	return s.runTx(ctx, false, fn)
}

func (s *Store) runTx(ctx context.Context, bumpRev bool, fn func(tx *Tx) error) (err error) {
	st := s.state.Load()
	if st.dialect != nil && st.dialect.Name() == "sqlite" {
		if err := s.acquireWriteLock(ctx); err != nil {
			return err
		}
		defer s.releaseWriteLock()
	}
	rawTx, err := st.writeDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlx: begin tx: %w", err)
	}
	wrapped := &Tx{raw: rawTx, dialect: st.dialect}
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
	if bumpRev {
		s.dataRev.Add(1)
	}
	return nil
}

type scanColKind uint8

const (
	scanColDiscard scanColKind = iota
	scanColBool
	scanColString
	scanColInt
	scanColUint
	scanColFloat
	scanColTime
	scanColDirect
)

var timeStructType = reflect.TypeFor[time.Time]()

type scanColSpec struct {
	kind       scanColKind
	fieldIndex []int
}

type nullTimeScanner struct {
	nt sql.NullTime
}

func (s *nullTimeScanner) Scan(value any) error {
	if value == nil {
		s.nt = sql.NullTime{}
		return nil
	}
	switch v := value.(type) {
	case time.Time:
		s.nt = sql.NullTime{Time: v, Valid: true}
		return nil
	case []byte:
		return s.parseString(string(v))
	case string:
		return s.parseString(v)
	default:
		return s.nt.Scan(value)
	}
}

func (s *nullTimeScanner) parseString(raw string) error {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		s.nt = sql.NullTime{}
		return nil
	}
	for _, layout := range []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02 15:04:05.999999999-07",
		"2006-01-02 15:04:05.999999999+00",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02",
	} {
		if tm, err := time.Parse(layout, trimmed); err == nil {
			s.nt = sql.NullTime{Time: tm, Valid: true}
			return nil
		}
	}
	return fmt.Errorf("sqlx: cannot parse %q into time.Time", raw)
}

type rowScanState struct {
	targets []any
	bools   []any
	strs    []sql.NullString
	ints    []sql.NullInt64
	uints   []any
	floats  []sql.NullFloat64
	times   []nullTimeScanner
	discard any
}

func buildColumnPlan(rt reflect.Type, cols []string) []scanColSpec {
	fieldMap := buildStructFieldIndex(rt)
	plan := make([]scanColSpec, len(cols))
	for i, c := range cols {
		fIdx, ok := fieldMap[strings.ToLower(c)]
		if !ok {
			plan[i] = scanColSpec{kind: scanColDiscard}
			continue
		}
		ft := rt.FieldByIndex(fIdx).Type
		if ft == timeStructType {
			plan[i] = scanColSpec{kind: scanColTime, fieldIndex: fIdx}
			continue
		}
		switch ft.Kind() {
		case reflect.Bool:
			plan[i] = scanColSpec{kind: scanColBool, fieldIndex: fIdx}
		case reflect.String:
			plan[i] = scanColSpec{kind: scanColString, fieldIndex: fIdx}
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			plan[i] = scanColSpec{kind: scanColInt, fieldIndex: fIdx}
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			plan[i] = scanColSpec{kind: scanColUint, fieldIndex: fIdx}
		case reflect.Float32, reflect.Float64:
			plan[i] = scanColSpec{kind: scanColFloat, fieldIndex: fIdx}
		default:
			plan[i] = scanColSpec{kind: scanColDirect, fieldIndex: fIdx}
		}
	}
	return plan
}

func newRowScanState(n int) *rowScanState {
	return &rowScanState{
		targets: make([]any, n),
		bools:   make([]any, n),
		strs:    make([]sql.NullString, n),
		ints:    make([]sql.NullInt64, n),
		uints:   make([]any, n),
		floats:  make([]sql.NullFloat64, n),
		times:   make([]nullTimeScanner, n),
	}
}

func (st *rowScanState) bind(rv reflect.Value, plan []scanColSpec) []any {
	for i, sp := range plan {
		switch sp.kind {
		case scanColDiscard:
			st.discard = nil
			st.targets[i] = &st.discard
		case scanColBool:
			st.bools[i] = nil
			st.targets[i] = &st.bools[i]
		case scanColString:
			st.strs[i] = sql.NullString{}
			st.targets[i] = &st.strs[i]
		case scanColInt:
			st.ints[i] = sql.NullInt64{}
			st.targets[i] = &st.ints[i]
		case scanColUint:
			st.uints[i] = nil
			st.targets[i] = &st.uints[i]
		case scanColFloat:
			st.floats[i] = sql.NullFloat64{}
			st.targets[i] = &st.floats[i]
		case scanColTime:
			st.times[i] = nullTimeScanner{}
			st.targets[i] = &st.times[i]
		case scanColDirect:
			st.targets[i] = rv.FieldByIndex(sp.fieldIndex).Addr().Interface()
		}
	}
	return st.targets
}

func (st *rowScanState) assign(rv reflect.Value, plan []scanColSpec) error {
	for i, sp := range plan {
		switch sp.kind {
		case scanColBool:
			b, err := coerceBool(st.bools[i])
			if err != nil {
				return err
			}
			rv.FieldByIndex(sp.fieldIndex).SetBool(b)
		case scanColString:
			fv := rv.FieldByIndex(sp.fieldIndex)
			if st.strs[i].Valid {
				fv.SetString(st.strs[i].String)
			} else {
				fv.SetString("")
			}
		case scanColInt:
			fv := rv.FieldByIndex(sp.fieldIndex)
			if st.ints[i].Valid {
				v := st.ints[i].Int64
				if fv.OverflowInt(v) {
					return fmt.Errorf("sqlx: integer %d overflows %s", v, fv.Type())
				}
				fv.SetInt(v)
			} else {
				fv.SetInt(0)
			}
		case scanColUint:
			fv := rv.FieldByIndex(sp.fieldIndex)
			u, err := coerceUint64(st.uints[i])
			if err != nil {
				return err
			}
			if fv.OverflowUint(u) {
				return fmt.Errorf("sqlx: unsigned integer %d overflows %s", u, fv.Type())
			}
			fv.SetUint(u)
		case scanColFloat:
			fv := rv.FieldByIndex(sp.fieldIndex)
			if st.floats[i].Valid {
				v := st.floats[i].Float64
				if fv.OverflowFloat(v) {
					return fmt.Errorf("sqlx: float %g overflows %s", v, fv.Type())
				}
				fv.SetFloat(v)
			} else {
				fv.SetFloat(0)
			}
		case scanColTime:
			fv := rv.FieldByIndex(sp.fieldIndex)
			if st.times[i].nt.Valid {
				fv.Set(reflect.ValueOf(st.times[i].nt.Time))
			} else {
				fv.Set(reflect.Zero(timeStructType))
			}
		}
	}
	return nil
}

// Select executes a query and scans all rows into []T using a precomputed zero-closure column plan
// with case-insensitive matching against `db:"column_name"` struct tags or lowercase field names.
func Select[T any](ctx context.Context, q Queryer, query string, args ...any) ([]T, error) {
	var out []T
	if err := selectRowsInto(ctx, q, &out, query, args...); err != nil {
		return nil, err
	}
	return out, nil
}

func selectRowsInto(ctx context.Context, q Queryer, slicePtr any, query string, args ...any) error {
	sliceVal := reflect.ValueOf(slicePtr).Elem()
	rt := sliceVal.Type().Elem()
	if rt.Kind() != reflect.Struct {
		return errors.New("sqlx.Select requires struct element type T")
	}
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return err
	}

	plan := buildColumnPlan(rt, cols)
	state := newRowScanState(len(cols))

	for rows.Next() {
		n := sliceVal.Len()
		sliceVal.Grow(1)
		sliceVal.SetLen(n + 1)
		rv := sliceVal.Index(n)
		if err := rows.Scan(state.bind(rv, plan)...); err != nil {
			return err
		}
		if err := state.assign(rv, plan); err != nil {
			return err
		}
	}
	return rows.Err()
}

// Get executes a query and scans the first row into T, returning sql.ErrNoRows if empty.
func Get[T any](ctx context.Context, q Queryer, query string, args ...any) (T, error) {
	var item T
	err := getRowInto(ctx, q, &item, query, args...)
	return item, err
}

func getRowInto(ctx context.Context, q Queryer, itemPtr any, query string, args ...any) error {
	rv := reflect.ValueOf(itemPtr).Elem()
	rt := rv.Type()
	if rt.Kind() != reflect.Struct {
		return errors.New("sqlx.Get requires struct type T")
	}
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return err
		}
		return sql.ErrNoRows
	}
	cols, err := rows.Columns()
	if err != nil {
		return err
	}

	plan := buildColumnPlan(rt, cols)
	state := newRowScanState(len(cols))

	if err := rows.Scan(state.bind(rv, plan)...); err != nil {
		rv.SetZero()
		return err
	}
	if err := state.assign(rv, plan); err != nil {
		rv.SetZero()
		return err
	}
	return rows.Err()
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

func coerceUint64(v any) (uint64, error) {
	switch x := v.(type) {
	case nil:
		return 0, nil
	case uint64:
		return x, nil
	case int64:
		if x < 0 {
			return 0, fmt.Errorf("sqlx: negative integer %d cannot scan into uint", x)
		}
		return uint64(x), nil
	case int:
		if x < 0 {
			return 0, fmt.Errorf("sqlx: negative integer %d cannot scan into uint", x)
		}
		return uint64(x), nil
	case float64:
		if x < 0 {
			return 0, fmt.Errorf("sqlx: negative float %g cannot scan into uint", x)
		}
		return uint64(x), nil
	case []byte:
		s := strings.TrimSpace(string(x))
		if s == "" {
			return 0, nil
		}
		return strconv.ParseUint(s, 10, 64)
	case string:
		s := strings.TrimSpace(x)
		if s == "" {
			return 0, nil
		}
		return strconv.ParseUint(s, 10, 64)
	default:
		return 0, fmt.Errorf("sqlx: cannot scan %T into uint", v)
	}
}

