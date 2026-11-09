package sqlx

import (
	"context"
	"database/sql/driver"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestInterpolateSimpleSQL_NaNInfCommentsAndDollarQuotes(t *testing.T) {
	query := `SELECT -- $1 comment
		/* block $2 */ $$dollar $3$$, $tag$$4$tag$, $1, $2, $3, $4`
	args := []driver.NamedValue{
		{Ordinal: 1, Value: math.NaN()},
		{Ordinal: 2, Value: math.Inf(1)},
		{Ordinal: 3, Value: math.Inf(-1)},
		{Ordinal: 4, Value: "O'Reilly"},
	}
	got, err := interpolateSimpleSQL(query, args)
	if err != nil {
		t.Fatalf("interpolateSimpleSQL failed: %v", err)
	}
	for _, must := range []string{"'NaN'::float8", "'Infinity'::float8", "'-Infinity'::float8", "'O''Reilly'", "$$dollar $3$$", "$tag$$4$tag$"} {
		if !strings.Contains(got, must) {
			t.Fatalf("interpolated SQL missing %q:\n%s", must, got)
		}
	}
}

func TestSQLiteWriteMutexSerializesConcurrentTransactionsZeroBusy(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, Options{
		Driver: "sqlite",
		DSN:    filepath.Join(t.TempDir(), "concurrent-tx.db"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if _, err := store.ExecContext(ctx, `CREATE TABLE counters (id INT PRIMARY KEY, n INT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ExecContext(ctx, `INSERT INTO counters (id, n) VALUES (1, 0)`); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errCh := make(chan error, 40)
	for g := 0; g < 24; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if txErr := store.WithTx(ctx, func(tx *Tx) error {
				_, e := tx.ExecContext(ctx, `UPDATE counters SET n = n + 1 WHERE id = 1`)
				return e
			}); txErr != nil {
				errCh <- txErr
			}
		}()
	}
	for r := 0; r < 12; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var cur int
			if qErr := store.QueryRowContext(ctx, `SELECT n FROM counters WHERE id = 1`).Scan(&cur); qErr != nil {
				errCh <- fmt.Errorf("concurrent read failed: %w", qErr)
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for e := range errCh {
		t.Fatalf("unexpected concurrent SQLite error: %v", e)
	}

	var finalN int
	if err := store.QueryRowContext(ctx, `SELECT n FROM counters WHERE id = 1`).Scan(&finalN); err != nil {
		t.Fatal(err)
	}
	if finalN != 24 {
		t.Fatalf("expected serialized counter=24, got %d", finalN)
	}
}

func TestRound3_NegativeLiteralNeverFusesIntoSQLCommentAndNULRejected(t *testing.T) {
	q := `SELECT -$1, -$2 WHERE x < -$1 AND y = $3`
	args := []driver.NamedValue{
		{Ordinal: 1, Value: int64(-5)},
		{Ordinal: 2, Value: float64(-3.25)},
		{Ordinal: 3, Value: "kept"},
	}
	got, err := interpolateSimpleSQL(q, args)
	if err != nil {
		t.Fatalf("interpolateSimpleSQL failed: %v", err)
	}
	want := `SELECT -(-5), -(-3.25) WHERE x < -(-5) AND y = 'kept'`
	if got != want {
		t.Fatalf("interpolated SQL mismatch:\ngot:  %s\nwant: %s", got, want)
	}
	if strings.Contains(got, "--") {
		t.Fatalf("CRITICAL: negative literal fused into SQL comment: %s", got)
	}

	// Negative zero float64 (-0.0) must also be parenthesized
	negZero := math.Copysign(0, -1)
	litZero, err := formatSimpleLiteral(negZero)
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(litZero, "-") {
		t.Fatalf("negative zero formatted with leading minus without parens: %q", litZero)
	}

	// encodePGParams must reject NUL bytes
	if _, err := encodePGParams([]driver.NamedValue{{Ordinal: 1, Value: "bad\x00str"}}); err == nil {
		t.Fatalf("expected encodePGParams to reject NUL byte in string")
	}
}

func TestRound3_ParsePGConnConfigAndBeginTxSQL(t *testing.T) {
	cfg1, simple1, err := parsePGConnConfig("postgres://u:p@127.0.0.1:5432/db?sslmode=disable&prefer_simple_protocol=true", false)
	if err != nil {
		t.Fatalf("parsePGConnConfig url: %v", err)
	}
	if !simple1 {
		t.Fatalf("expected prefer_simple_protocol=true to enable simple protocol")
	}
	if _, leaked := cfg1.Params["prefer_simple_protocol"]; leaked {
		t.Fatalf("prefer_simple_protocol leaked into Params")
	}

	cfg2, simple2, err := parsePGConnConfig("host=127.0.0.1 user=u dbname=db default_query_exec_mode=simple_protocol", false)
	if err != nil {
		t.Fatalf("parsePGConnConfig dsn: %v", err)
	}
	if !simple2 {
		t.Fatalf("expected default_query_exec_mode=simple_protocol to enable simple protocol")
	}
	if _, leaked := cfg2.Params["default_query_exec_mode"]; leaked {
		t.Fatalf("default_query_exec_mode leaked into Params")
	}

	sqlSerRO, err := buildPGBeginSQL(driver.TxOptions{Isolation: driver.IsolationLevel(6), ReadOnly: true}) // sql.LevelSerializable == 6
	if err != nil || sqlSerRO != "BEGIN ISOLATION LEVEL SERIALIZABLE READ ONLY" {
		t.Fatalf("unexpected serializable RO begin SQL: %q, err=%v", sqlSerRO, err)
	}
	sqlRRRW, err := buildPGBeginSQL(driver.TxOptions{Isolation: driver.IsolationLevel(4), ReadOnly: false}) // sql.LevelRepeatableRead == 4
	if err != nil || sqlRRRW != "BEGIN ISOLATION LEVEL REPEATABLE READ READ WRITE" {
		t.Fatalf("unexpected repeatable read RW begin SQL: %q, err=%v", sqlRRRW, err)
	}
	if _, err := buildPGBeginSQL(driver.TxOptions{Isolation: driver.IsolationLevel(7)}); err == nil { // sql.LevelLinearizable == 7
		t.Fatalf("expected error for unsupported LevelLinearizable")
	}

	// wrapPGErr returns driver.ErrBadConn when conn is nil/closed and ctx is active
	badErr := wrapPGErr(nil, context.Background(), fmt.Errorf("broken pipe"))
	if badErr == nil || !strings.Contains(badErr.Error(), driver.ErrBadConn.Error()) {
		t.Fatalf("expected wrapPGErr to wrap driver.ErrBadConn, got %v", badErr)
	}
}

func TestRound3_NullCoalescingAllNumericWidthsMaxUint64AndTime(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, Options{Driver: "sqlite", DSN: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	type scanTarget struct {
		F32 float32   `db:"f32"`
		F64 float64   `db:"f64"`
		I8  int8      `db:"i8"`
		I16 int16     `db:"i16"`
		U   uint      `db:"u"`
		U8  uint8     `db:"u8"`
		U16 uint16    `db:"u16"`
		U32 uint32    `db:"u32"`
		U64 uint64    `db:"u64"`
		TS  time.Time `db:"ts"`
	}
	nullRes, err := Get[scanTarget](ctx, store, `SELECT NULL AS f32, NULL AS f64, NULL AS i8, NULL AS i16, NULL AS u, NULL AS u8, NULL AS u16, NULL AS u32, NULL AS u64, NULL AS ts`)
	if err != nil {
		t.Fatalf("Get NULLs failed: %v", err)
	}
	if nullRes != (scanTarget{}) || !nullRes.TS.IsZero() {
		t.Fatalf("expected all zero values on NULL scan, got %+v", nullRes)
	}

	maxRes, err := Get[scanTarget](ctx, store, `SELECT 1.5 AS f32, 2.75 AS f64, -128 AS i8, 32767 AS i16, 42 AS u, 255 AS u8, 65535 AS u16, 4294967295 AS u32, '18446744073709551615' AS u64, '2026-11-08T03:08:16Z' AS ts`)
	if err != nil {
		t.Fatalf("Get MaxUint64 failed: %v", err)
	}
	if maxRes.U64 != math.MaxUint64 || maxRes.I8 != -128 || maxRes.U16 != 65535 || maxRes.F32 != 1.5 || maxRes.TS.Year() != 2026 {
		t.Fatalf("unexpected populated scan result: %+v", maxRes)
	}
}

func TestRound3_SQLiteInMemoryReadInsideWithTxAndContextCancelSemaphore(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, Options{Driver: "sqlite", DSN: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// Verify second :memory: store is isolated from first :memory: store
	store2, err := Open(ctx, Options{Driver: "sqlite", DSN: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	defer store2.Close()

	if _, err := store.ExecContext(ctx, `CREATE TABLE items (id TEXT PRIMARY KEY, val TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ExecContext(ctx, `INSERT INTO items (id, val) VALUES ('seed', 'v0')`); err != nil {
		t.Fatal(err)
	}
	// store2 must not see items table
	var dummy int
	if err := store2.QueryRowContext(ctx, `SELECT COUNT(*) FROM items`).Scan(&dummy); err == nil {
		t.Fatalf("expected two :memory: stores to be isolated, but store2 saw store1's table!")
	}

	// Read via store.QueryRowContext INSIDE store.WithTx must NOT deadlock!
	err = store.WithTx(ctx, func(tx *Tx) error {
		if _, e := tx.ExecContext(ctx, `INSERT INTO items (id, val) VALUES ('in_tx', 'v1')`); e != nil {
			return e
		}
		var seedVal string
		if e := store.QueryRowContext(ctx, `SELECT val FROM items WHERE id = 'seed'`).Scan(&seedVal); e != nil {
			return fmt.Errorf("store.QueryRowContext inside WithTx failed: %w", e)
		}
		if seedVal != "v0" {
			return fmt.Errorf("unexpected seedVal=%q", seedVal)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WithTx with nested store.QueryRowContext failed: %v", err)
	}

	// Verify sqliteWriteSem respects ctx cancellation when contended (plus zero-value Store lazy fallback)
	var zeroStore Store
	if err := zeroStore.acquireWriteLock(ctx); err != nil {
		t.Fatalf("zeroStore acquireWriteLock: %v", err)
	}
	canceledCtx, cancel := context.WithCancel(ctx)
	cancel()
	if err := zeroStore.acquireWriteLock(canceledCtx); err == nil {
		t.Fatalf("expected acquireWriteLock to fail immediately on canceled context while held")
	}
	zeroStore.releaseWriteLock()
}
