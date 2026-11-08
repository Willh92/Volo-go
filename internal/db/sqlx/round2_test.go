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
