package backup

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Willh92/Volo-go/internal/db/sqlx"
	"github.com/Willh92/Volo-go/internal/domain"
)

func TestMultiAdminFallbackPreservesDistinctHashesAndBatchNDJSONRestore(t *testing.T) {
	// 1. Multi-admin patchRedactedAdminRow matches by id, username, then email without clobbering all admins to fallbackAdmins[0]
	fallbacks := []domain.User{
		{
			ID:           "usr_alpha",
			Username:     "alpha",
			Email:        "alpha@example.com",
			PasswordHash: "$argon2id$alpha",
		},
		{
			ID:           "usr_bravo",
			Username:     "bravo",
			Email:        "bravo@example.com",
			PasswordHash: "$argon2id$bravo",
		},
		{
			ID:           "usr_charlie",
			Username:     "charlie",
			Email:        "charlie@example.com",
			PasswordHash: "$argon2id$charlie",
		},
	}

	rowByID := map[string]any{"id": "usr_bravo", "role": domain.RoleAdmin, "username": "renamed_bravo", "email": "other@example.com", "password_hash": ""}
	patchRedactedAdminRow(rowByID, fallbacks)
	if rowByID["password_hash"] != "$argon2id$bravo" {
		t.Fatalf("expected ID match to restore bravo hash, got %v", rowByID["password_hash"])
	}

	rowByUser := map[string]any{"id": "usr_new_c", "role": domain.RoleAdmin, "username": "CHARLIE", "email": "c2@example.com", "password_hash": ""}
	patchRedactedAdminRow(rowByUser, fallbacks)
	if rowByUser["password_hash"] != "$argon2id$charlie" {
		t.Fatalf("expected case-insensitive username match to restore charlie hash, got %v", rowByUser["password_hash"])
	}

	rowByEmail := map[string]any{"id": "usr_new_b", "role": domain.RoleAdmin, "username": "completely_new", "email": "BRAVO@EXAMPLE.COM", "password_hash": ""}
	patchRedactedAdminRow(rowByEmail, fallbacks)
	if rowByEmail["password_hash"] != "$argon2id$bravo" {
		t.Fatalf("expected case-insensitive email match to restore bravo hash, got %v", rowByEmail["password_hash"])
	}

	// 2. Multi-row batch NDJSON restore across > restoreBatchSize rows + mid-stream column set changes
	ctx := context.Background()
	store := openTestSQLiteStore(t, filepath.Join(t.TempDir(), "ndjson-batch.db"))
	if _, err := store.ExecContext(ctx, `CREATE TABLE batch_items (id TEXT PRIMARY KEY, seq INTEGER NOT NULL, label TEXT NOT NULL DEFAULT '')`); err != nil {
		t.Fatal(err)
	}

	var lines []string
	const totalRows = 455
	for i := 0; i < totalRows; i++ {
		if i == 210 {
			// Mid-stream column signature change (omitting label) to exercise batch flush boundary
			lines = append(lines, fmt.Sprintf(`{"id":"item_%03d","seq":%d}`, i, i))
		} else {
			lines = append(lines, fmt.Sprintf(`{"id":"item_%03d","seq":%d,"label":"lbl-%d"}`, i, i, i))
		}
	}

	var zbuf bytes.Buffer
	zw := zip.NewWriter(&zbuf)
	zfWriter, err := zw.Create("tables/batch_items.ndjson")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := zfWriter.Write([]byte(strings.Join(lines, "\n") + "\n")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(zbuf.Bytes()), int64(zbuf.Len()))
	if err != nil {
		t.Fatal(err)
	}

	var imported int64
	if err := store.WithTx(ctx, func(tx *sqlx.Tx) error {
		var sErr error
		imported, sErr = streamNDJSONIntoTable(ctx, tx, "batch_items", zr.File[0], nil)
		return sErr
	}); err != nil {
		t.Fatalf("streamNDJSONIntoTable batch failed: %v", err)
	}
	if imported != totalRows {
		t.Fatalf("expected %d imported rows, got %d", totalRows, imported)
	}

	var dbCount int
	if err := store.QueryRowContext(ctx, `SELECT COUNT(*) FROM batch_items`).Scan(&dbCount); err != nil || dbCount != totalRows {
		t.Fatalf("expected %d rows in DB, got %d (err=%v)", totalRows, dbCount, err)
	}
	var lbl210, lbl454 string
	_ = store.QueryRowContext(ctx, `SELECT label FROM batch_items WHERE id = 'item_210'`).Scan(&lbl210)
	_ = store.QueryRowContext(ctx, `SELECT label FROM batch_items WHERE id = 'item_454'`).Scan(&lbl454)
	if lbl210 != "" || lbl454 != "lbl-454" {
		t.Fatalf("unexpected row values after mid-stream schema flush: lbl210=%q lbl454=%q", lbl210, lbl454)
	}
}
