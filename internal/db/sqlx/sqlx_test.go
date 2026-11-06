package sqlx_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/Willh92/Volo-go/internal/db/sqlx"
)

type sampleRecord struct {
	ID        string `db:"id"`
	Title     string `db:"title"`
	ViewCount int64  `db:"view_count"`
	IsActive  bool   `db:"is_active"`
}

func TestSQLX_ReadOnlyPoolZeroWriteSideEffectsAndWAL(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "split-wal.db")
	store, err := sqlx.Open(ctx, sqlx.Options{
		Driver: "sqlite",
		DSN:    dbPath,
	})
	if err != nil {
		t.Fatalf("sqlx.Open: %v", err)
	}
	defer store.Close()

	if store.ReadDB() == store.WriteDB() {
		t.Fatalf("expected file-backed SQLite to separate WriteDB and ReadDB pools")
	}

	var journalMode string
	if err := store.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journalMode); err != nil {
		t.Fatalf("query journal_mode: %v", err)
	}
	if journalMode != "wal" {
		t.Fatalf("expected WAL journal_mode, got %q", journalMode)
	}

	if _, err := store.ExecContext(ctx, `CREATE TABLE items (id TEXT PRIMARY KEY, title TEXT, view_count INTEGER, is_active INTEGER)`); err != nil {
		t.Fatalf("create table on WriteDB: %v", err)
	}

	// Read pool MUST physically reject any write side-effect (`PRAGMA query_only = ON`)!
	if _, err := store.ReadDB().ExecContext(ctx, `INSERT INTO items (id, title, view_count, is_active) VALUES ('x', 'bad', 1, 1)`); err == nil {
		t.Fatalf("CRITICAL: ReadDB permitted an INSERT statement despite query_only(ON)!")
	}
}

func TestSQLX_WithTxAtomicCommitAndMandatoryRollbackOnErrorOrPanic(t *testing.T) {
	ctx := context.Background()
	store, err := sqlx.Open(ctx, sqlx.Options{
		Driver: "sqlite",
		DSN:    filepath.Join(t.TempDir(), "tx.db"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	_, err = store.ExecContext(ctx, `CREATE TABLE ledger_a (id TEXT PRIMARY KEY, val TEXT); CREATE TABLE ledger_b (id TEXT PRIMARY KEY, val TEXT);`)
	if err != nil {
		t.Fatal(err)
	}

	// 1. Multi-table error halfway through -> MUST roll back ledger_a write completely!
	txErr := store.WithTx(ctx, func(tx *sqlx.Tx) error {
		if _, e := tx.ExecContext(ctx, `INSERT INTO ledger_a (id, val) VALUES (?, ?)`, "k1", "partial"); e != nil {
			return e
		}
		return errors.New("simulated downstream failure")
	})
	if txErr == nil {
		t.Fatalf("expected WithTx error")
	}
	var cntA int
	_ = store.QueryRowContext(ctx, `SELECT COUNT(*) FROM ledger_a`).Scan(&cntA)
	if cntA != 0 {
		t.Fatalf("WithTx failed to roll back ledger_a on error: count=%d", cntA)
	}

	// 2. Panic inside WithTx -> MUST roll back AND re-panic cleanly!
	didPanic := false
	func() {
		defer func() {
			if recover() != nil {
				didPanic = true
			}
		}()
		_ = store.WithTx(ctx, func(tx *sqlx.Tx) error {
			_, _ = tx.ExecContext(ctx, `INSERT INTO ledger_a (id, val) VALUES (?, ?)`, "k2", "panic-row")
			panic("boom")
		})
	}()
	if !didPanic {
		t.Fatalf("expected panic to propagate after rollback")
	}
	_ = store.QueryRowContext(ctx, `SELECT COUNT(*) FROM ledger_a`).Scan(&cntA)
	if cntA != 0 {
		t.Fatalf("WithTx failed to roll back ledger_a on panic: count=%d", cntA)
	}

	// 3. Clean multi-table commit
	if err := store.WithTx(ctx, func(tx *sqlx.Tx) error {
		if _, e := tx.ExecContext(ctx, `INSERT INTO ledger_a (id, val) VALUES (?, ?)`, "ok1", "a"); e != nil {
			return e
		}
		_, e := tx.ExecContext(ctx, `INSERT INTO ledger_b (id, val) VALUES (?, ?)`, "ok1", "b")
		return e
	}); err != nil {
		t.Fatalf("clean WithTx commit failed: %v", err)
	}
	var cntB int
	_ = store.QueryRowContext(ctx, `SELECT COUNT(*) FROM ledger_a`).Scan(&cntA)
	_ = store.QueryRowContext(ctx, `SELECT COUNT(*) FROM ledger_b`).Scan(&cntB)
	if cntA != 1 || cntB != 1 {
		t.Fatalf("expected committed counts (1, 1), got (%d, %d)", cntA, cntB)
	}
}

func TestSQLX_CaseInsensitiveMappingNullCoercionAndUpsert(t *testing.T) {
	ctx := context.Background()
	store, err := sqlx.Open(ctx, sqlx.Options{
		Driver: "sqlite",
		DSN:    filepath.Join(t.TempDir(), "scan.db"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	_, err = store.ExecContext(ctx, `CREATE TABLE records (id TEXT PRIMARY KEY, title TEXT, view_count INTEGER, is_active TEXT)`)
	if err != nil {
		t.Fatal(err)
	}

	upsertQ := store.Dialect().UpsertSQL(
		"records",
		[]string{"id", "title", "view_count", "is_active"},
		[]string{"id"},
		[]string{"title", "view_count", "is_active"},
	)
	if _, err := store.ExecContext(ctx, upsertQ, "r1", "Initial", 10, "true"); err != nil {
		t.Fatalf("initial upsert: %v", err)
	}
	if _, err := store.ExecContext(ctx, upsertQ, "r1", "Updated", 42, "1"); err != nil {
		t.Fatalf("update upsert: %v", err)
	}
	// Insert a row with NULLs and mixed-case boolean
	if _, err := store.ExecContext(ctx, `INSERT INTO records (id, title, view_count, is_active) VALUES ('r2', NULL, NULL, 'FALSE')`); err != nil {
		t.Fatal(err)
	}

	// Query using mixed-case column aliases (simulating MySQL/Postgres/TiDB driver alias differences) + extra unmapped column
	rows, err := sqlx.Select[sampleRecord](ctx, store,
		`SELECT id AS "ID", title AS "TiTlE", view_count AS "VIEW_COUNT", is_active AS "Is_Active", 'ignored' AS "EXTRA_COL" FROM records ORDER BY id ASC`)
	if err != nil {
		t.Fatalf("sqlx.Select mixed-case failed: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(rows))
	}
	if rows[0].ID != "r1" || rows[0].Title != "Updated" || rows[0].ViewCount != 42 || !rows[0].IsActive {
		t.Fatalf("unexpected row[0]: %+v", rows[0])
	}
	if rows[1].ID != "r2" || rows[1].Title != "" || rows[1].ViewCount != 0 || rows[1].IsActive {
		t.Fatalf("unexpected NULL-coerced row[1]: %+v", rows[1])
	}
}
