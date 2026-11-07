package sqlx_test

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/Willh92/Volo-go/internal/db/dialect"
	"github.com/Willh92/Volo-go/internal/db/sqlx"
)

func TestDataRevisionCoherenceVsBackgroundTelemetry(t *testing.T) {
	ctx := context.Background()
	store, err := sqlx.Open(ctx, sqlx.Options{
		Driver: "sqlite",
		DSN:    filepath.Join(t.TempDir(), "rev.db"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	r0 := store.DataRevision()
	if _, err := store.ExecContext(ctx, `CREATE TABLE counters (id TEXT PRIMARY KEY, n INTEGER)`); err != nil {
		t.Fatal(err)
	}
	r1 := store.DataRevision()
	if r1 <= r0 {
		t.Fatalf("expected ExecContext to bump DataRevision (%d -> %d)", r0, r1)
	}

	if err := store.WithTx(ctx, func(tx *sqlx.Tx) error {
		_, e := tx.ExecContext(ctx, `INSERT INTO counters (id, n) VALUES ('a', 1)`)
		return e
	}); err != nil {
		t.Fatal(err)
	}
	r2 := store.DataRevision()
	if r2 <= r1 {
		t.Fatalf("expected committed WithTx to bump DataRevision (%d -> %d)", r1, r2)
	}

	// Background telemetry writes (`ExecBgContext` / `WithBgTx`) MUST NOT invalidate content/feed DataRevision!
	if _, err := store.ExecBgContext(ctx, `UPDATE counters SET n = n + 1 WHERE id = 'a'`); err != nil {
		t.Fatal(err)
	}
	if err := store.WithBgTx(ctx, func(tx *sqlx.Tx) error {
		_, e := tx.ExecContext(ctx, `UPDATE counters SET n = n + 5 WHERE id = 'a'`)
		return e
	}); err != nil {
		t.Fatal(err)
	}
	if got := store.DataRevision(); got != r2 {
		t.Fatalf("expected background telemetry writes to preserve DataRevision=%d, got %d", r2, got)
	}
}

func BenchmarkZeroClosureSelect(b *testing.B) {
	ctx := context.Background()
	store, err := sqlx.Open(ctx, sqlx.Options{
		Driver: "sqlite",
		DSN:    filepath.Join(b.TempDir(), "bench.db"),
	})
	if err != nil {
		b.Fatal(err)
	}
	defer store.Close()

	if _, err := store.ExecContext(ctx, `CREATE TABLE items (id TEXT PRIMARY KEY, title TEXT, view_count INTEGER, is_active INTEGER)`); err != nil {
		b.Fatal(err)
	}
	if err := store.WithTx(ctx, func(tx *sqlx.Tx) error {
		for i := 0; i < 25; i++ {
			if _, e := tx.ExecContext(ctx, `INSERT INTO items (id, title, view_count, is_active) VALUES (?, ?, ?, ?)`,
				fmt.Sprintf("id-%02d", i), fmt.Sprintf("Benchmark Item %d", i), int64(i*10), 1); e != nil {
				return e
			}
		}
		return nil
	}); err != nil {
		b.Fatal(err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rows, err := sqlx.Select[sampleRecord](ctx, store, `SELECT id, title, view_count, is_active FROM items ORDER BY id ASC`)
		if err != nil || len(rows) != 25 {
			b.Fatalf("unexpected select result: len=%d err=%v", len(rows), err)
		}
	}
}

func BenchmarkPostgresRebindCached(b *testing.B) {
	pg, err := dialect.For("postgres")
	if err != nil {
		b.Fatal(err)
	}
	const q = `SELECT * FROM articles WHERE status = ? AND category_id = ? AND published_at >= ? AND published_at < ? ORDER BY is_pinned DESC, published_at DESC LIMIT ? OFFSET ?`
	_ = pg.Rebind(q)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		out := pg.Rebind(q)
		if out == "" {
			b.Fatal("empty rebind")
		}
	}
}
