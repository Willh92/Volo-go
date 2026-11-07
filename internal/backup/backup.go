package backup

import (
	"archive/zip"
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Willh92/Volo-go/internal/db/migrate"
	"github.com/Willh92/Volo-go/internal/db/sqlx"
	"github.com/Willh92/Volo-go/internal/domain"
)

const (
	BackupFormatVersion  = "volo-backup-v1"
	maxRestoreZipEntries = 10000
	maxSingleEntryBytes  = 512 * 1024 * 1024  // 512 MiB per stream guard against zip bombs
	maxTotalRestoreBytes = 2048 * 1024 * 1024 // 2 GiB total unpacked cap
	restoreBatchSize     = 200
)

func isBlockedRestoreUploadExt(p string) bool {
	switch strings.ToLower(path.Ext(p)) {
	case ".svg", ".html", ".htm", ".xml", ".xhtml", ".js", ".mjs":
		return true
	default:
		return false
	}
}

// FileDigest records integrity & row metrics for a single stream inside manifest.json.
type FileDigest struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
	Rows   int64  `json:"rows,omitempty"`
}

// Manifest is stored as manifest.json inside every native Volo-go backup ZIP.
type Manifest struct {
	Format        string       `json:"format"`
	SchemaVersion int          `json:"schema_version"`
	SourceDialect string       `json:"source_dialect"`
	CreatedAt     int64        `json:"created_at"`
	IncludesMedia bool         `json:"includes_media"`
	Files         []FileDigest `json:"files"`
}

// CreateOptions configures streaming backup creation.
type CreateOptions struct {
	IncludeMedia bool
	UploadDir    string
}

// RestoreOptions configures streaming cross-database backup restoration.
type RestoreOptions struct {
	DryRun    bool
	UploadDir string
}

// RestoreSummary reports row counts and media files restored (or validated in DryRun).
type RestoreSummary struct {
	SourceDialect string           `json:"source_dialect"`
	Tables        map[string]int64 `json:"tables"`
	MediaFiles    int              `json:"media_files"`
	DryRun        bool             `json:"dry_run"`
}

// CreateToFile streams a complete native backup archive directly to outputZipPath via a temporary file.
func CreateToFile(ctx context.Context, store *sqlx.Store, outputZipPath string, opts CreateOptions) (*Manifest, error) {
	if err := os.MkdirAll(filepath.Dir(outputZipPath), 0o750); err != nil {
		return nil, fmt.Errorf("backup: mkdir parent: %w", err)
	}
	tmpPath := fmt.Sprintf("%s.tmp.%d", outputZipPath, os.Getpid())
	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("backup: create temp archive: %w", err)
	}
	manifest, err := Create(ctx, store, f, opts)
	if err != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		return nil, err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		return nil, err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return nil, err
	}
	if err := os.Rename(tmpPath, outputZipPath); err != nil {
		_ = os.Remove(tmpPath)
		return nil, err
	}
	return manifest, nil
}

// Create writes a database-agnostic streaming ZIP archive to w without buffering whole tables in RAM.
func Create(ctx context.Context, store *sqlx.Store, w io.Writer, opts CreateOptions) (*Manifest, error) {
	zw := zip.NewWriter(w)
	manifest := &Manifest{
		Format:        BackupFormatVersion,
		SchemaVersion: migrate.CurrentSchemaVersion,
		SourceDialect: store.Dialect().Name(),
		CreatedAt:     time.Now().UnixMilli(),
		IncludesMedia: opts.IncludeMedia,
	}

	for _, table := range migrate.CanonicalTables {
		select {
		case <-ctx.Done():
			_ = zw.Close()
			return nil, ctx.Err()
		default:
		}
		entryPath := "tables/" + table + ".ndjson"
		digest, err := streamTableToZip(ctx, store, zw, table, entryPath)
		if err != nil {
			_ = zw.Close()
			return nil, fmt.Errorf("backup: export table %s: %w", table, err)
		}
		manifest.Files = append(manifest.Files, digest)
	}

	if opts.IncludeMedia && strings.TrimSpace(opts.UploadDir) != "" {
		mediaDigests, err := streamMediaDirToZip(ctx, zw, opts.UploadDir)
		if err != nil {
			_ = zw.Close()
			return nil, fmt.Errorf("backup: export media: %w", err)
		}
		manifest.Files = append(manifest.Files, mediaDigests...)
	}

	mWriter, err := zw.CreateHeader(&zip.FileHeader{
		Name:     "manifest.json",
		Method:   zip.Deflate,
		Modified: time.Now().UTC(),
	})
	if err != nil {
		_ = zw.Close()
		return nil, err
	}
	enc := json.NewEncoder(mWriter)
	enc.SetIndent("", "  ")
	if err := enc.Encode(manifest); err != nil {
		_ = zw.Close()
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return manifest, nil
}

func streamTableToZip(ctx context.Context, store *sqlx.Store, zw *zip.Writer, table string, entryPath string) (FileDigest, error) {
	q := fmt.Sprintf("SELECT * FROM %s", store.Dialect().QuoteIdent(table))
	rows, err := store.QueryContext(ctx, q)
	if err != nil {
		return FileDigest{}, err
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return FileDigest{}, err
	}
	lowerCols := make([]string, len(cols))
	for i, c := range cols {
		lowerCols[i] = strings.ToLower(c)
	}

	zf, err := zw.CreateHeader(&zip.FileHeader{
		Name:     entryPath,
		Method:   zip.Deflate,
		Modified: time.Now().UTC(),
	})
	if err != nil {
		return FileDigest{}, err
	}

	hasher := sha256.New()
	cw := &countingWriter{w: io.MultiWriter(zf, hasher)}
	bw := bufio.NewWriterSize(cw, 32*1024)
	enc := json.NewEncoder(bw)
	enc.SetEscapeHTML(false)

	rawVals := make([]any, len(lowerCols))
	valPtrs := make([]any, len(lowerCols))
	for i := range rawVals {
		valPtrs[i] = &rawVals[i]
	}

	var rowCount int64
	rowMap := make(map[string]any, len(lowerCols))
	for rows.Next() {
		if err := rows.Scan(valPtrs...); err != nil {
			return FileDigest{}, err
		}
		clear(rowMap)
		for i, col := range lowerCols {
			rowMap[col] = normalizeSQLValueForJSON(table, col, rawVals[i])
		}
		if err := enc.Encode(rowMap); err != nil {
			return FileDigest{}, err
		}
		rowCount++
	}
	if err := rows.Err(); err != nil {
		return FileDigest{}, err
	}
	if err := bw.Flush(); err != nil {
		return FileDigest{}, err
	}

	return FileDigest{
		Path:   entryPath,
		SHA256: hex.EncodeToString(hasher.Sum(nil)),
		Bytes:  cw.n,
		Rows:   rowCount,
	}, nil
}

func normalizeSQLValueForJSON(table string, col string, v any) any {
	if isBooleanColumn(table, col) {
		switch x := v.(type) {
		case bool:
			return x
		case int64:
			return x != 0
		case int:
			return x != 0
		case []byte:
			s := strings.TrimSpace(string(x))
			return s == "1" || strings.EqualFold(s, "true") || strings.EqualFold(s, "t")
		case string:
			s := strings.TrimSpace(x)
			return s == "1" || strings.EqualFold(s, "true") || strings.EqualFold(s, "t")
		}
		return false
	}
	switch x := v.(type) {
	case nil:
		return nil
	case []byte:
		return string(x)
	default:
		return x
	}
}

func streamMediaDirToZip(ctx context.Context, zw *zip.Writer, uploadDir string) ([]FileDigest, error) {
	info, err := os.Lstat(uploadDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("refusing symlinked upload directory %q", uploadDir)
	}
	realRoot, err := filepath.EvalSymlinks(uploadDir)
	if err != nil {
		return nil, err
	}

	var relFiles []string
	err = filepath.WalkDir(realRoot, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.Type()&os.ModeSymlink != 0 {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(realRoot, p)
		if err != nil {
			return err
		}
		relSlash := filepath.ToSlash(rel)
		if strings.HasPrefix(relSlash, ".") || strings.Contains(relSlash, "/.") {
			return nil
		}
		relFiles = append(relFiles, relSlash)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(relFiles)

	digests := make([]FileDigest, 0, len(relFiles))
	buf := make([]byte, 32*1024)
	for _, rel := range relFiles {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		srcPath := filepath.Join(realRoot, filepath.FromSlash(rel))
		f, err := os.Open(srcPath)
		if err != nil {
			return nil, err
		}
		entryPath := "uploads/" + rel
		zf, err := zw.CreateHeader(&zip.FileHeader{
			Name:     entryPath,
			Method:   zip.Deflate,
			Modified: time.Now().UTC(),
		})
		if err != nil {
			_ = f.Close()
			return nil, err
		}
		hasher := sha256.New()
		n, copyErr := io.CopyBuffer(io.MultiWriter(zf, hasher), f, buf)
		_ = f.Close()
		if copyErr != nil {
			return nil, copyErr
		}
		digests = append(digests, FileDigest{
			Path:   entryPath,
			SHA256: hex.EncodeToString(hasher.Sum(nil)),
			Bytes:  n,
		})
	}
	return digests, nil
}

// RestoreFromFile validates and streams a native Volo-go backup ZIP into the target Store
// inside an atomic database transaction (supporting SQLite <-> PostgreSQL <-> MySQL migration).
func RestoreFromFile(ctx context.Context, store *sqlx.Store, zipPath string, opts RestoreOptions) (*RestoreSummary, error) {
	f, err := os.Open(zipPath)
	if err != nil {
		return nil, fmt.Errorf("backup: open archive %q: %w", zipPath, err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	return Restore(ctx, store, f, st.Size(), opts)
}

// Restore validates SHA-256 checksums for every archive stream before mutating the database,
// then restores all 22+ canonical tables inside a single rollback-safe transaction.
func Restore(ctx context.Context, store *sqlx.Store, ra io.ReaderAt, size int64, opts RestoreOptions) (*RestoreSummary, error) {
	zr, err := zip.NewReader(ra, size)
	if err != nil {
		return nil, fmt.Errorf("backup: invalid zip archive: %w", err)
	}

	if len(zr.File) > maxRestoreZipEntries {
		return nil, fmt.Errorf("backup: archive exceeds maximum %d entries", maxRestoreZipEntries)
	}

	fileIndex := make(map[string]*zip.File, len(zr.File))
	for _, zf := range zr.File {
		clean := path.Clean(strings.TrimPrefix(zf.Name, "/"))
		if clean == ".." || strings.HasPrefix(clean, "../") {
			return nil, fmt.Errorf("backup: path traversal entry %q rejected", zf.Name)
		}
		if !zf.FileInfo().IsDir() && !zf.Mode().IsRegular() {
			return nil, fmt.Errorf("backup: non-regular entry %q rejected", zf.Name)
		}
		fileIndex[clean] = zf
	}

	mfEntry, ok := fileIndex["manifest.json"]
	if !ok {
		return nil, errors.New("backup: missing manifest.json (if this is a legacy bolo-solo archive, use 'volo import-legacy')")
	}
	var manifest Manifest
	if err := decodeLimitedJSON(mfEntry, 4*1024*1024, &manifest); err != nil {
		return nil, fmt.Errorf("backup: parse manifest.json: %w", err)
	}
	if manifest.Format != BackupFormatVersion {
		return nil, fmt.Errorf("backup: unsupported archive format %q", manifest.Format)
	}

	// Pass 1: Verify SHA-256 checksums and total byte quota BEFORE touching DB or disk!
	var totalBytes int64
	expectedMap := make(map[string]FileDigest, len(manifest.Files))
	for _, fd := range manifest.Files {
		if strings.HasPrefix(fd.Path, "uploads/") && isBlockedRestoreUploadExt(fd.Path) {
			return nil, fmt.Errorf("backup: active script/markup media entry %q rejected", fd.Path)
		}
		zf, exists := fileIndex[fd.Path]
		if !exists {
			return nil, fmt.Errorf("backup: missing declared entry %q", fd.Path)
		}
		actualHash, actualBytes, err := verifyZipEntrySHA256(zf, maxSingleEntryBytes)
		if err != nil {
			return nil, fmt.Errorf("backup: verify %q: %w", fd.Path, err)
		}
		totalBytes += actualBytes
		if totalBytes > maxTotalRestoreBytes {
			return nil, errors.New("backup: archive exceeds total unpacked safety limit")
		}
		if !strings.EqualFold(actualHash, fd.SHA256) || actualBytes != fd.Bytes {
			return nil, fmt.Errorf("backup: integrity check failed for %q (sha256/size mismatch)", fd.Path)
		}
		expectedMap[fd.Path] = fd
	}

	summary := &RestoreSummary{
		SourceDialect: manifest.SourceDialect,
		Tables:        make(map[string]int64, len(migrate.CanonicalTables)),
		DryRun:        opts.DryRun,
	}

	if opts.DryRun {
		for _, table := range migrate.CanonicalTables {
			if fd, ok := expectedMap["tables/"+table+".ndjson"]; ok {
				summary.Tables[table] = fd.Rows
			}
		}
		for p := range expectedMap {
			if strings.HasPrefix(p, "uploads/") {
				summary.MediaFiles++
			}
		}
		return summary, nil
	}

	// Preserve existing target admin credentials if the incoming backup has empty password hashes
	existingAdmins, _ := sqlx.Select[domain.User](ctx, store,
		`SELECT * FROM users WHERE role = ?`, domain.RoleAdmin)

	// Pass 2: Atomic transactional DB restore (reverse delete, forward streaming insert)
	err = store.WithTx(ctx, func(tx *sqlx.Tx) error {
		for i := len(migrate.CanonicalTables) - 1; i >= 0; i-- {
			tbl := migrate.CanonicalTables[i]
			if _, delErr := tx.ExecContext(ctx, "DELETE FROM "+tx.Dialect().QuoteIdent(tbl)); delErr != nil {
				return fmt.Errorf("clear table %s: %w", tbl, delErr)
			}
		}

		for _, tbl := range migrate.CanonicalTables {
			entryName := "tables/" + tbl + ".ndjson"
			zf, exists := fileIndex[entryName]
			if !exists {
				continue
			}
			n, insErr := streamNDJSONIntoTable(ctx, tx, tbl, zf, existingAdmins)
			if insErr != nil {
				return fmt.Errorf("restore table %s: %w", tbl, insErr)
			}
			summary.Tables[tbl] = n
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	// Pass 3: Stream media uploads if present and UploadDir provided
	if manifest.IncludesMedia && strings.TrimSpace(opts.UploadDir) != "" {
		if err := os.MkdirAll(opts.UploadDir, 0o750); err != nil {
			return nil, fmt.Errorf("backup: mkdir upload dir: %w", err)
		}
		realUploadRoot, err := filepath.EvalSymlinks(opts.UploadDir)
		if err != nil {
			return nil, err
		}
		buf := make([]byte, 32*1024)
		for _, fd := range manifest.Files {
			if !strings.HasPrefix(fd.Path, "uploads/") {
				continue
			}
			rel := strings.TrimPrefix(fd.Path, "uploads/")
			target := filepath.Join(realUploadRoot, filepath.FromSlash(rel))
			if !strings.HasPrefix(target, realUploadRoot+string(os.PathSeparator)) {
				return nil, fmt.Errorf("backup: unsafe media path %q", fd.Path)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
				return nil, err
			}
			rc, err := fileIndex[fd.Path].Open()
			if err != nil {
				return nil, err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
			if err != nil {
				_ = rc.Close()
				return nil, err
			}
			_, copyErr := io.CopyBuffer(out, io.LimitReader(rc, maxSingleEntryBytes), buf)
			_ = out.Close()
			_ = rc.Close()
			if copyErr != nil {
				return nil, copyErr
			}
			summary.MediaFiles++
		}
	}

	return summary, nil
}

func streamNDJSONIntoTable(ctx context.Context, tx *sqlx.Tx, table string, zf *zip.File, fallbackAdmins []domain.User) (int64, error) {
	rc, err := zf.Open()
	if err != nil {
		return 0, err
	}
	defer rc.Close()

	dec := json.NewDecoder(io.LimitReader(rc, maxSingleEntryBytes))
	dec.UseNumber()

	var cols []string
	var insertSQL string
	var count int64

	for {
		var row map[string]any
		if err := dec.Decode(&row); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return count, err
		}
		if len(row) == 0 {
			continue
		}

		if table == "users" {
			patchRedactedAdminRow(row, fallbackAdmins)
		}

		if len(cols) == 0 {
			cols = make([]string, 0, len(row))
			for k := range row {
				cols = append(cols, k)
			}
			sort.Strings(cols)

			quotedCols := make([]string, len(cols))
			placeholders := make([]string, len(cols))
			for i, c := range cols {
				quotedCols[i] = tx.Dialect().QuoteIdent(c)
				placeholders[i] = "?"
			}
			insertSQL = fmt.Sprintf(
				"INSERT INTO %s (%s) VALUES (%s)",
				tx.Dialect().QuoteIdent(table),
				strings.Join(quotedCols, ", "),
				strings.Join(placeholders, ", "),
			)
		}

		args := make([]any, len(cols))
		for i, c := range cols {
			args[i] = coerceJSONValForDB(table, c, row[c], tx.Dialect().Name())
		}
		if _, err := tx.ExecContext(ctx, insertSQL, args...); err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}

func patchRedactedAdminRow(row map[string]any, fallbackAdmins []domain.User) {
	if len(fallbackAdmins) == 0 {
		return
	}
	role, _ := row["role"].(string)
	pw, _ := row["password_hash"].(string)
	leg, _ := row["legacy_hash"].(string)
	if role == domain.RoleAdmin && pw == "" && leg == "" {
		row["password_hash"] = fallbackAdmins[0].PasswordHash
		row["legacy_hash"] = fallbackAdmins[0].LegacyHash
	}
}

func coerceJSONValForDB(table string, col string, val any, dialectName string) any {
	if val == nil {
		return ""
	}
	if isBooleanColumn(table, col) {
		b := false
		switch x := val.(type) {
		case bool:
			b = x
		case json.Number:
			n, _ := x.Int64()
			b = n != 0
		case string:
			b = x == "1" || strings.EqualFold(x, "true")
		}
		if dialectName == "postgres" {
			return b
		}
		if b {
			return 1
		}
		return 0
	}
	if num, ok := val.(json.Number); ok {
		if i, err := num.Int64(); err == nil {
			return i
		}
		if f, err := num.Float64(); err == nil {
			return f
		}
		return num.String()
	}
	return val
}

func isBooleanColumn(table string, col string) bool {
	switch table + "." + col {
	case "articles.is_pinned",
		"articles.allow_comment",
		"articles.has_content_lock",
		"pages.is_published",
		"pages.allow_comment",
		"pages.show_toc",
		"nav_items.open_in_new_tab",
		"nav_items.is_enabled",
		"comments.is_deleted",
		"media_assets.exif_stripped",
		"redirects.is_system_legacy",
		"webhooks.is_enabled",
		"security_geo_rules.is_enabled",
		"plugins.is_enabled":
		return true
	default:
		return false
	}
}

func verifyZipEntrySHA256(zf *zip.File, maxBytes int64) (string, int64, error) {
	rc, err := zf.Open()
	if err != nil {
		return "", 0, err
	}
	defer rc.Close()

	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(rc, maxBytes+1))
	if err != nil {
		return "", 0, err
	}
	if n > maxBytes {
		return "", 0, fmt.Errorf("entry %q exceeds max allowed size %d", zf.Name, maxBytes)
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func decodeLimitedJSON(zf *zip.File, maxBytes int64, dst any) error {
	rc, err := zf.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	dec := json.NewDecoder(io.LimitReader(rc, maxBytes))
	return dec.Decode(dst)
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (cw *countingWriter) Write(p []byte) (int, error) {
	n, err := cw.w.Write(p)
	cw.n += int64(n)
	return n, err
}
