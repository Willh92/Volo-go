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
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Willh92/Volo-go/internal/db/migrate"
	"github.com/Willh92/Volo-go/internal/db/sqlx"
	"github.com/Willh92/Volo-go/internal/domain"
)

const (
	BackupFormatVersion     = "volo-backup-v1"
	maxRestoreZipEntries    = 250_000
	maxSingleEntryBytes     = 512 * 1024 * 1024  // 512 MiB per stream guard against zip bombs
	maxTotalRestoreBytes    = 2048 * 1024 * 1024 // 2 GiB total unpacked cap
	maxExtensionSingleBytes = 32 * 1024 * 1024   // 32 MiB per extension file
	maxExtensionTotalBytes  = 256 * 1024 * 1024  // 256 MiB total unpacked extensions cap
	restoreBatchSize        = 200
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
	Format             string       `json:"format"`
	SchemaVersion      int          `json:"schema_version"`
	SourceDialect      string       `json:"source_dialect"`
	CreatedAt          int64        `json:"created_at"`
	IncludesMedia      bool         `json:"includes_media"`
	IncludesExtensions bool         `json:"includes_extensions,omitempty"`
	Files              []FileDigest `json:"files"`
}

// CreateOptions configures streaming backup creation.
type CreateOptions struct {
	IncludeMedia bool
	UploadDir    string
	PluginsDir   string
	ThemesDir    string
}

// RestoreOptions configures streaming cross-database backup restoration.
type RestoreOptions struct {
	DryRun             bool
	UploadDir          string
	PluginsDir         string
	ThemesDir          string
	MaxEntries         int
	MaxSingleFileBytes int64
	MaxTotalBytes      int64
}

// RestoreSummary reports row counts, media files, and extension files restored (or validated in DryRun).
type RestoreSummary struct {
	SourceDialect  string           `json:"source_dialect"`
	Tables         map[string]int64 `json:"tables"`
	MediaFiles     int              `json:"media_files"`
	ExtensionFiles int              `json:"extension_files,omitempty"`
	DryRun         bool             `json:"dry_run"`
}

// CreateToFile streams a complete native backup archive directly to outputZipPath via a temporary file.
func CreateToFile(ctx context.Context, store *sqlx.Store, outputZipPath string, opts CreateOptions) (*Manifest, error) {
	if err := os.MkdirAll(filepath.Dir(outputZipPath), 0o750); err != nil {
		return nil, fmt.Errorf("backup: mkdir parent: %w", err)
	}
	f, err := os.CreateTemp(filepath.Dir(outputZipPath), ".volo-backup-*.tmp")
	if err != nil {
		return nil, fmt.Errorf("backup: create temp archive: %w", err)
	}
	tmpPath := f.Name()
	_ = f.Chmod(0o600)
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

	if strings.TrimSpace(opts.PluginsDir) != "" {
		pluginDigests, err := streamExtensionsDirToZip(ctx, zw, opts.PluginsDir, "extensions/plugins")
		if err != nil {
			_ = zw.Close()
			return nil, fmt.Errorf("backup: export plugins: %w", err)
		}
		if len(pluginDigests) > 0 {
			manifest.IncludesExtensions = true
			manifest.Files = append(manifest.Files, pluginDigests...)
		}
	}

	if strings.TrimSpace(opts.ThemesDir) != "" {
		themeDigests, err := streamExtensionsDirToZip(ctx, zw, opts.ThemesDir, "extensions/themes")
		if err != nil {
			_ = zw.Close()
			return nil, fmt.Errorf("backup: export themes: %w", err)
		}
		if len(themeDigests) > 0 {
			manifest.IncludesExtensions = true
			manifest.Files = append(manifest.Files, themeDigests...)
		}
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

func shouldSkipBackupTempOrHidden(name string) bool {
	lower := strings.ToLower(strings.TrimSpace(name))
	if lower == "" || strings.HasPrefix(lower, ".") {
		return true
	}
	if strings.HasPrefix(lower, ".tmp") || strings.HasPrefix(lower, ".bak") ||
		strings.HasSuffix(lower, ".tmp") || strings.HasSuffix(lower, ".bak") {
		return true
	}
	return false
}

func isValidExtensionID(id string) bool {
	if len(id) == 0 || len(id) > 64 || strings.HasPrefix(id, ".") {
		return false
	}
	if shouldSkipBackupTempOrHidden(id) {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			continue
		}
		if i > 0 && (c == '-' || c == '_' || c == '.') {
			continue
		}
		return false
	}
	return !strings.Contains(id, "..")
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
		linfo, sErr := os.Lstat(p)
		if sErr != nil {
			return sErr
		}
		if linfo.Mode()&os.ModeSymlink != 0 || d.Type()&os.ModeSymlink != 0 {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if p != realRoot && shouldSkipBackupTempOrHidden(filepath.Base(p)) {
			if linfo.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if linfo.IsDir() {
			return nil
		}
		if !linfo.Mode().IsRegular() {
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

func streamExtensionsDirToZip(ctx context.Context, zw *zip.Writer, rootDir string, zipPrefix string) ([]FileDigest, error) {
	info, err := os.Lstat(rootDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("refusing symlinked extensions directory %q", rootDir)
	}
	if !info.IsDir() {
		return nil, nil
	}
	realRoot, err := filepath.EvalSymlinks(rootDir)
	if err != nil {
		return nil, err
	}

	topEntries, err := os.ReadDir(realRoot)
	if err != nil {
		return nil, err
	}

	var relFiles []string
	for _, top := range topEntries {
		extID := top.Name()
		if !isValidExtensionID(extID) {
			continue
		}
		extDir := filepath.Join(realRoot, extID)
		extInfo, sErr := os.Lstat(extDir)
		if sErr != nil || extInfo.Mode()&os.ModeSymlink != 0 || !extInfo.IsDir() {
			continue
		}

		walkErr := filepath.WalkDir(extDir, func(p string, d fs.DirEntry, wErr error) error {
			if wErr != nil {
				return wErr
			}
			linfo, lErr := os.Lstat(p)
			if lErr != nil {
				return lErr
			}
			if linfo.Mode()&os.ModeSymlink != 0 || d.Type()&os.ModeSymlink != 0 {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if p != extDir && shouldSkipBackupTempOrHidden(filepath.Base(p)) {
				if linfo.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if linfo.IsDir() {
				return nil
			}
			if !linfo.Mode().IsRegular() {
				return nil
			}
			rel, rErr := filepath.Rel(extDir, p)
			if rErr != nil {
				return rErr
			}
			relSlash := filepath.ToSlash(rel)
			if strings.HasPrefix(relSlash, ".") || strings.Contains(relSlash, "/.") {
				return nil
			}
			relFiles = append(relFiles, extID+"/"+relSlash)
			return nil
		})
		if walkErr != nil {
			return nil, walkErr
		}
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
		linfo, lErr := os.Lstat(srcPath)
		if lErr != nil || linfo.Mode()&os.ModeSymlink != 0 || !linfo.Mode().IsRegular() {
			continue
		}
		f, oErr := os.Open(srcPath)
		if oErr != nil {
			return nil, oErr
		}
		entryPath := zipPrefix + "/" + rel
		hdr := &zip.FileHeader{
			Name:     entryPath,
			Method:   zip.Deflate,
			Modified: linfo.ModTime().UTC(),
		}
		hdr.SetMode(0o644)
		zf, cErr := zw.CreateHeader(hdr)
		if cErr != nil {
			_ = f.Close()
			return nil, cErr
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

func sanitizeBackupZipEntryName(raw string, isDir bool) (string, error) {
	if raw == "" || strings.ContainsRune(raw, 0) || strings.ContainsRune(raw, '\\') || strings.ContainsRune(raw, ':') {
		return "", fmt.Errorf("backup: unsafe archive entry path %q rejected", raw)
	}
	if strings.HasPrefix(raw, "/") {
		return "", fmt.Errorf("backup: absolute archive entry path %q rejected", raw)
	}
	trimmed := raw
	if isDir {
		trimmed = strings.TrimSuffix(trimmed, "/")
		if trimmed == "" {
			return ".", nil
		}
	} else if strings.HasSuffix(trimmed, "/") {
		return "", fmt.Errorf("backup: trailing slash on regular entry %q rejected", raw)
	}

	parts := strings.Split(trimmed, "/")
	for _, seg := range parts {
		if seg == "" || seg == "." || seg == ".." {
			return "", fmt.Errorf("backup: path traversal entry %q rejected", raw)
		}
	}
	clean := path.Clean(trimmed)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("backup: path traversal entry %q rejected", raw)
	}
	return clean, nil
}

func parseExtensionEntryPath(entryPath string) (kind string, extID string, relSlash string, skip bool, err error) {
	var rest string
	switch {
	case strings.HasPrefix(entryPath, "extensions/plugins/"):
		kind = "plugins"
		rest = strings.TrimPrefix(entryPath, "extensions/plugins/")
	case strings.HasPrefix(entryPath, "extensions/themes/"):
		kind = "themes"
		rest = strings.TrimPrefix(entryPath, "extensions/themes/")
	default:
		return "", "", "", false, fmt.Errorf("backup: invalid extension archive path %q", entryPath)
	}
	extID, relSlash, hasSlash := strings.Cut(rest, "/")
	if !hasSlash || relSlash == "" || !isValidExtensionID(extID) {
		return "", "", "", false, fmt.Errorf("backup: invalid extension identifier or path in %q", entryPath)
	}
	for _, seg := range strings.Split(relSlash, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", "", "", false, fmt.Errorf("backup: unsafe extension path %q", entryPath)
		}
		if shouldSkipBackupTempOrHidden(seg) {
			return kind, extID, relSlash, true, nil
		}
	}
	return kind, extID, relSlash, false, nil
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

	entryCap := maxRestoreZipEntries
	if opts.MaxEntries > 0 {
		entryCap = opts.MaxEntries
	}
	if len(zr.File) > entryCap {
		return nil, fmt.Errorf("backup: archive exceeds maximum %d entries", entryCap)
	}

	singleCap := int64(maxSingleEntryBytes)
	if opts.MaxSingleFileBytes > 0 {
		singleCap = opts.MaxSingleFileBytes
	}
	totalCap := int64(maxTotalRestoreBytes)
	if opts.MaxTotalBytes > 0 {
		totalCap = opts.MaxTotalBytes
	}

	fileIndex := make(map[string]*zip.File, len(zr.File))
	for _, zf := range zr.File {
		mode := zf.Mode()
		isDir := zf.FileInfo().IsDir()
		if mode&os.ModeSymlink != 0 || (!isDir && !mode.IsRegular()) {
			return nil, fmt.Errorf("backup: non-regular or symlink entry %q rejected", zf.Name)
		}
		clean, cErr := sanitizeBackupZipEntryName(zf.Name, isDir)
		if cErr != nil {
			return nil, cErr
		}
		if isDir {
			continue
		}
		if _, dup := fileIndex[clean]; dup {
			return nil, fmt.Errorf("backup: duplicate archive entry %q rejected", zf.Name)
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

	// Pass 1: Verify SHA-256 checksums, path safety, and byte quotas BEFORE touching DB or disk!
	var totalBytes int64
	var extensionBytes int64
	expectedMap := make(map[string]FileDigest, len(manifest.Files))
	for _, fd := range manifest.Files {
		cleanDecl, dErr := sanitizeBackupZipEntryName(fd.Path, false)
		if dErr != nil || cleanDecl != fd.Path {
			return nil, fmt.Errorf("backup: invalid manifest file path %q", fd.Path)
		}
		if strings.HasPrefix(fd.Path, "uploads/") && isBlockedRestoreUploadExt(fd.Path) {
			return nil, fmt.Errorf("backup: active script/markup media entry %q rejected", fd.Path)
		}
		entryMax := singleCap
		isExt := strings.HasPrefix(fd.Path, "extensions/")
		if isExt {
			if _, _, _, _, eErr := parseExtensionEntryPath(fd.Path); eErr != nil {
				return nil, eErr
			}
			if maxExtensionSingleBytes < entryMax {
				entryMax = maxExtensionSingleBytes
			}
		}

		zf, exists := fileIndex[fd.Path]
		if !exists {
			return nil, fmt.Errorf("backup: missing declared entry %q", fd.Path)
		}
		if zf.UncompressedSize64 > uint64(entryMax) {
			return nil, fmt.Errorf("backup: entry %q uncompressed header size exceeds limit %d", fd.Path, entryMax)
		}
		actualHash, actualBytes, err := verifyZipEntrySHA256(zf, entryMax)
		if err != nil {
			return nil, fmt.Errorf("backup: verify %q: %w", fd.Path, err)
		}
		totalBytes += actualBytes
		if totalBytes > totalCap {
			return nil, errors.New("backup: archive exceeds total unpacked safety limit")
		}
		if isExt {
			extensionBytes += actualBytes
			extTotalCap := int64(maxExtensionTotalBytes)
			if totalCap < extTotalCap {
				extTotalCap = totalCap
			}
			if extensionBytes > extTotalCap {
				return nil, errors.New("backup: extension entries exceed unpacked safety limit")
			}
		}
		if !strings.EqualFold(actualHash, fd.SHA256) || actualBytes != fd.Bytes {
			return nil, fmt.Errorf("backup: integrity check failed for %q (sha256/size mismatch)", fd.Path)
		}
		expectedMap[fd.Path] = fd
	}

	// Fail closed if the ZIP archive contains ANY undeclared table, media, or extension stream missing from manifest.json!
	for clean, zf := range fileIndex {
		if clean == "manifest.json" || zf == nil || zf.FileInfo().IsDir() {
			continue
		}
		if strings.HasPrefix(clean, "tables/") || strings.HasPrefix(clean, "uploads/") ||
			strings.HasPrefix(clean, "media/") || strings.HasPrefix(clean, "extensions/") {
			if _, declared := expectedMap[clean]; !declared {
				return nil, fmt.Errorf("backup: unsigned extra archive entry %q missing from manifest.json", clean)
			}
		}
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
			} else if strings.HasPrefix(p, "extensions/") {
				if _, _, _, skip, pErr := parseExtensionEntryPath(p); pErr == nil && !skip {
					summary.ExtensionFiles++
				}
			}
		}
		return summary, nil
	}

	// Preserve existing target admin credentials if the incoming backup has empty password hashes
	var existingAdmins []domain.User
	_ = sqlx.SelectInto(ctx, store, &existingAdmins,
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
			if _, verified := expectedMap[entryName]; !verified {
				continue
			}
			zf, exists := fileIndex[entryName]
			if !exists || zf == nil || zf.FileInfo().IsDir() {
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

	var extractedDiskBytes int64

	// Pass 3: Stream media uploads if present and UploadDir provided
	if manifest.IncludesMedia && strings.TrimSpace(opts.UploadDir) != "" {
		realUploadRoot, rErr := prepareSafeRestoreDir(opts.UploadDir)
		if rErr != nil {
			return nil, fmt.Errorf("backup: prepare upload dir: %w", rErr)
		}
		buf := make([]byte, 32*1024)
		for _, fd := range manifest.Files {
			if !strings.HasPrefix(fd.Path, "uploads/") {
				continue
			}
			zf := fileIndex[fd.Path]
			if zf == nil || zf.FileInfo().IsDir() {
				continue
			}
			rel := strings.TrimPrefix(fd.Path, "uploads/")
			if shouldSkipRelativeRestorePath(rel) {
				continue
			}
			n, wErr := extractVerifiedZipFile(realUploadRoot, rel, zf, singleCap, totalCap-extractedDiskBytes, buf)
			if wErr != nil {
				return nil, wErr
			}
			extractedDiskBytes += n
			summary.MediaFiles++
		}
	}

	// Pass 4: Stream external plugins and themes if target directories are configured
	if strings.TrimSpace(opts.PluginsDir) != "" {
		nFiles, nBytes, pErr := restoreExtensionKind(manifest.Files, fileIndex, "plugins", opts.PluginsDir, singleCap, totalCap-extractedDiskBytes)
		if pErr != nil {
			return nil, pErr
		}
		extractedDiskBytes += nBytes
		summary.ExtensionFiles += nFiles
	}
	if strings.TrimSpace(opts.ThemesDir) != "" {
		nFiles, nBytes, tErr := restoreExtensionKind(manifest.Files, fileIndex, "themes", opts.ThemesDir, singleCap, totalCap-extractedDiskBytes)
		if tErr != nil {
			return nil, tErr
		}
		extractedDiskBytes += nBytes
		summary.ExtensionFiles += nFiles
	}

	return summary, nil
}

func prepareSafeRestoreDir(dir string) (string, error) {
	if linfo, err := os.Lstat(dir); err == nil && linfo.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("refusing symlinked restore directory %q", dir)
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(dir)
}

func shouldSkipRelativeRestorePath(relSlash string) bool {
	for _, seg := range strings.Split(relSlash, "/") {
		if shouldSkipBackupTempOrHidden(seg) {
			return true
		}
	}
	return false
}

func ensureNoSymlinkAncestors(realRoot string, target string) error {
	rel, err := filepath.Rel(realRoot, target)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return fmt.Errorf("backup: target %q escapes root %q", target, realRoot)
	}
	cur := realRoot
	for _, seg := range strings.Split(rel, string(os.PathSeparator)) {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("backup: invalid path segment %q", seg)
		}
		cur = filepath.Join(cur, seg)
		if linfo, sErr := os.Lstat(cur); sErr == nil {
			if linfo.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("backup: refusing to traverse or overwrite symlink %q", cur)
			}
		}
	}
	return nil
}

func extractVerifiedZipFile(
	realRoot string,
	relSlash string,
	zf *zip.File,
	singleLimit int64,
	remainingTotal int64,
	buf []byte,
) (int64, error) {
	if remainingTotal < 0 {
		return 0, errors.New("backup: archive exceeds total unpacked safety limit")
	}
	target := filepath.Join(realRoot, filepath.FromSlash(relSlash))
	if !strings.HasPrefix(target, realRoot+string(os.PathSeparator)) {
		return 0, fmt.Errorf("backup: unsafe target path %q", zf.Name)
	}
	if err := ensureNoSymlinkAncestors(realRoot, target); err != nil {
		return 0, err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
		return 0, err
	}
	if err := ensureNoSymlinkAncestors(realRoot, target); err != nil {
		return 0, err
	}

	maxAllowed := singleLimit
	if remainingTotal < maxAllowed {
		maxAllowed = remainingTotal
	}
	rc, err := zf.Open()
	if err != nil {
		return 0, err
	}
	out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		_ = rc.Close()
		return 0, err
	}
	written, copyErr := io.CopyBuffer(out, io.LimitReader(rc, maxAllowed+1), buf)
	_ = out.Close()
	_ = rc.Close()
	if copyErr != nil {
		_ = os.Remove(target)
		return 0, copyErr
	}
	if written > maxAllowed {
		_ = os.Remove(target)
		return 0, fmt.Errorf("backup: extracted stream %q exceeds unpacked byte cap", zf.Name)
	}
	return written, nil
}

func restoreExtensionKind(
	files []FileDigest,
	fileIndex map[string]*zip.File,
	wantKind string,
	destDir string,
	singleCap int64,
	remainingTotal int64,
) (int, int64, error) {
	prefix := "extensions/" + wantKind + "/"
	hasMatching := false
	for _, fd := range files {
		if strings.HasPrefix(fd.Path, prefix) {
			hasMatching = true
			break
		}
	}
	if !hasMatching {
		return 0, 0, nil
	}

	realDestRoot, err := prepareSafeRestoreDir(destDir)
	if err != nil {
		return 0, 0, fmt.Errorf("backup: prepare %s dir: %w", wantKind, err)
	}

	entryMax := singleCap
	if maxExtensionSingleBytes < entryMax {
		entryMax = maxExtensionSingleBytes
	}

	buf := make([]byte, 32*1024)
	var restoredCount int
	var restoredBytes int64
	for _, fd := range files {
		if !strings.HasPrefix(fd.Path, prefix) {
			continue
		}
		kind, extID, relSlash, skip, pErr := parseExtensionEntryPath(fd.Path)
		if pErr != nil {
			return restoredCount, restoredBytes, pErr
		}
		if skip || kind != wantKind {
			continue
		}
		zf := fileIndex[fd.Path]
		if zf == nil || zf.FileInfo().IsDir() {
			continue
		}
		n, wErr := extractVerifiedZipFile(realDestRoot, extID+"/"+relSlash, zf, entryMax, remainingTotal-restoredBytes, buf)
		if wErr != nil {
			return restoredCount, restoredBytes, wErr
		}
		restoredBytes += n
		restoredCount++
	}
	return restoredCount, restoredBytes, nil
}

func streamNDJSONIntoTable(ctx context.Context, tx *sqlx.Tx, table string, zf *zip.File, fallbackAdmins []domain.User) (int64, error) {
	rc, err := zf.Open()
	if err != nil {
		return 0, err
	}
	defer rc.Close()

	// Discover the target table's canonical schema columns so keys omitted on row 1
	// are never dropped on subsequent rows and unknown/legacy keys are safely ignored.
	validCols := make(map[string]string)
	schemaRows, err := tx.QueryContext(ctx, "SELECT * FROM "+tx.Dialect().QuoteIdent(table)+" WHERE 1=0")
	if err != nil {
		return 0, err
	}
	tableColNames, colErr := schemaRows.Columns()
	_ = schemaRows.Close()
	if colErr != nil {
		return 0, colErr
	}
	for _, colName := range tableColNames {
		validCols[strings.ToLower(colName)] = colName
	}

	dec := json.NewDecoder(io.LimitReader(rc, maxSingleEntryBytes))
	dec.UseNumber()

	type colMeta struct {
		headerPrefix string
		rowTuple     string
	}
	metaCache := make(map[string]colMeta)
	fullBatchSQLCache := make(map[string]string)
	isMySQL := tx.Dialect().Name() == "mysql"
	var mysqlUniqueSeen map[string]map[string]bool
	if isMySQL && len(mysqlUniqueColumnsForTable(table)) > 0 {
		mysqlUniqueSeen = make(map[string]map[string]bool)
	}
	var count int64

	var (
		pendingCols []string
		pendingSig  string
		pendingMeta colMeta
		pendingArgs []any
		pendingRows int
		batchCap    int
	)

	flushBatch := func() error {
		if pendingRows == 0 {
			return nil
		}
		var insertSQL string
		if pendingRows == batchCap {
			if cached, ok := fullBatchSQLCache[pendingSig]; ok {
				insertSQL = cached
			}
		}
		if insertSQL == "" {
			var b strings.Builder
			b.Grow(len(pendingMeta.headerPrefix) + pendingRows*(len(pendingMeta.rowTuple)+2))
			b.WriteString(pendingMeta.headerPrefix)
			for i := 0; i < pendingRows; i++ {
				if i > 0 {
					b.WriteString(", ")
				}
				b.WriteString(pendingMeta.rowTuple)
			}
			insertSQL = b.String()
			if pendingRows == batchCap {
				fullBatchSQLCache[pendingSig] = insertSQL
			}
		}
		if _, err := tx.ExecContext(ctx, insertSQL, pendingArgs...); err != nil {
			return err
		}
		count += int64(pendingRows)
		pendingArgs = pendingArgs[:0]
		pendingRows = 0
		return nil
	}

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
		if mysqlUniqueSeen != nil {
			disambiguateMySQLUniqueRow(table, row, mysqlUniqueSeen)
		}

		cols := make([]string, 0, len(row))
		rowVals := make(map[string]any, len(row))
		for k, v := range row {
			if canonicalCol, ok := validCols[strings.ToLower(k)]; ok {
				if _, dup := rowVals[canonicalCol]; !dup {
					cols = append(cols, canonicalCol)
				}
				rowVals[canonicalCol] = v
			}
		}
		if len(cols) == 0 {
			continue
		}
		sort.Strings(cols)

		colSig := strings.Join(cols, "\x00")
		if pendingRows > 0 && colSig != pendingSig {
			if err := flushBatch(); err != nil {
				return count, err
			}
		}
		if pendingRows == 0 {
			meta, cached := metaCache[colSig]
			if !cached {
				quotedCols := make([]string, len(cols))
				placeholders := make([]string, len(cols))
				for i, c := range cols {
					quotedCols[i] = tx.Dialect().QuoteIdent(c)
					placeholders[i] = "?"
				}
				meta = colMeta{
					headerPrefix: fmt.Sprintf(
						"INSERT INTO %s (%s) VALUES ",
						tx.Dialect().QuoteIdent(table),
						strings.Join(quotedCols, ", "),
					),
					rowTuple: "(" + strings.Join(placeholders, ", ") + ")",
				}
				metaCache[colSig] = meta
			}
			pendingCols = cols
			pendingSig = colSig
			pendingMeta = meta
			batchCap = min(restoreBatchSize, max(1, 900/len(cols)))
			if cap(pendingArgs) < batchCap*len(cols) {
				pendingArgs = make([]any, 0, batchCap*len(cols))
			}
		}

		for _, c := range pendingCols {
			pendingArgs = append(pendingArgs, coerceJSONValForDB(table, c, rowVals[c], tx.Dialect().Name()))
		}
		pendingRows++
		if pendingRows >= batchCap {
			if err := flushBatch(); err != nil {
				return count, err
			}
		}
	}
	if err := flushBatch(); err != nil {
		return count, err
	}
	return count, nil
}

func mysqlUniqueColumnsForTable(table string) []string {
	switch table {
	case "tags":
		return []string{"name", "slug"}
	case "redirects":
		return []string{"source_path"}
	case "users":
		return []string{"username", "email"}
	case "categories", "articles", "pages", "series", "books":
		return []string{"slug"}
	default:
		return nil
	}
}

func mysqlMaxColRunes(table string, col string) int {
	switch table + "." + col {
	case "users.username":
		return 64
	case "tags.name", "tags.slug", "categories.slug":
		return 128
	case "redirects.source_path":
		return 512
	default:
		return 190
	}
}

func truncateRunesForMySQL(s string, maxRunes int) string {
	if maxRunes <= 0 || utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	r := []rune(s)
	return string(r[:maxRunes])
}

// mysqlUnicodeCIFoldKey approximates MySQL `utf8mb4_unicode_ci` (UCA case + accent + ligature + PAD SPACE folding)
// in pure Go stdlib so case/accent-colliding keys from SQLite/Postgres are deterministically disambiguated.
func mysqlUnicodeCIFoldKey(s string) string {
	s = strings.TrimRight(s, " ")
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if unicode.Is(unicode.Mn, r) {
			continue
		}
		if r >= 0xFF01 && r <= 0xFF5E {
			r -= 0xFEE0
		}
		r = unicode.ToLower(r)
		switch r {
		case 'à', 'á', 'â', 'ã', 'ä', 'å', 'ā', 'ă', 'ą':
			b.WriteByte('a')
		case 'æ':
			b.WriteString("ae")
		case 'ç', 'ć', 'ĉ', 'ċ', 'č':
			b.WriteByte('c')
		case 'ď', 'đ', 'ð':
			b.WriteByte('d')
		case 'è', 'é', 'ê', 'ë', 'ē', 'ĕ', 'ė', 'ę', 'ě':
			b.WriteByte('e')
		case 'ĝ', 'ğ', 'ġ', 'ģ':
			b.WriteByte('g')
		case 'ĥ', 'ħ':
			b.WriteByte('h')
		case 'ì', 'í', 'î', 'ĩ', 'ï', 'ī', 'ĭ', 'į', 'ı':
			b.WriteByte('i')
		case 'ĵ':
			b.WriteByte('j')
		case 'ķ':
			b.WriteByte('k')
		case 'ĺ', 'ļ', 'ľ', 'ŀ', 'ł':
			b.WriteByte('l')
		case 'ñ', 'ń', 'ņ', 'ň':
			b.WriteByte('n')
		case 'ò', 'ó', 'ô', 'õ', 'ö', 'ø', 'ō', 'ŏ', 'ő':
			b.WriteByte('o')
		case 'œ':
			b.WriteString("oe")
		case 'ŕ', 'ŗ', 'ř':
			b.WriteByte('r')
		case 'ś', 'ŝ', 'ş', 'š', 'ſ':
			b.WriteByte('s')
		case 'ß':
			b.WriteString("ss")
		case 'ţ', 'ť', 'ŧ':
			b.WriteByte('t')
		case 'ù', 'ú', 'û', 'ũ', 'ü', 'ū', 'ŭ', 'ů', 'ű', 'ų':
			b.WriteByte('u')
		case 'ŵ':
			b.WriteByte('w')
		case 'ý', 'ÿ', 'ŷ':
			b.WriteByte('y')
		case 'ź', 'ż', 'ž':
			b.WriteByte('z')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func disambiguateMySQLUniqueRow(table string, row map[string]any, seen map[string]map[string]bool) {
	for _, col := range mysqlUniqueColumnsForTable(table) {
		rawVal, ok := row[col]
		if !ok || rawVal == nil {
			continue
		}
		str, ok := rawVal.(string)
		if !ok {
			continue
		}
		colSeen := seen[col]
		if colSeen == nil {
			colSeen = make(map[string]bool)
			seen[col] = colSeen
		}
		maxRunes := mysqlMaxColRunes(table, col)
		candidate := truncateRunesForMySQL(str, maxRunes)
		fold := mysqlUnicodeCIFoldKey(candidate)
		if !colSeen[fold] {
			colSeen[fold] = true
			if candidate != str {
				row[col] = candidate
			}
			continue
		}
		base := strings.TrimRight(str, " ")
		for n := 2; ; n++ {
			suffix := "-" + strconv.Itoa(n)
			switch {
			case col == "email" && strings.Contains(base, "@"):
				local, dom, _ := strings.Cut(base, "@")
				tag := "+" + strconv.Itoa(n) + "@" + dom
				candidate = truncateRunesForMySQL(local, maxRunes-utf8.RuneCountInString(tag)) + tag
			case col == "name":
				tag := " (" + strconv.Itoa(n) + ")"
				candidate = truncateRunesForMySQL(base, maxRunes-utf8.RuneCountInString(tag)) + tag
			default:
				candidate = truncateRunesForMySQL(base, maxRunes-utf8.RuneCountInString(suffix)) + suffix
			}
			fold = mysqlUnicodeCIFoldKey(candidate)
			if !colSeen[fold] {
				colSeen[fold] = true
				row[col] = candidate
				break
			}
		}
	}
}

func patchRedactedAdminRow(row map[string]any, fallbackAdmins []domain.User) {
	if len(fallbackAdmins) == 0 {
		return
	}
	role, _ := row["role"].(string)
	pw, _ := row["password_hash"].(string)
	leg, _ := row["legacy_hash"].(string)
	if role != domain.RoleAdmin || pw != "" || leg != "" {
		return
	}
	rowID, _ := row["id"].(string)
	rowUser, _ := row["username"].(string)
	rowEmail, _ := row["email"].(string)
	matched := fallbackAdmins[0]
	found := false
	if strings.TrimSpace(rowID) != "" {
		for _, fb := range fallbackAdmins {
			if fb.ID == rowID {
				matched = fb
				found = true
				break
			}
		}
	}
	if !found && strings.TrimSpace(rowUser) != "" {
		for _, fb := range fallbackAdmins {
			if strings.EqualFold(strings.TrimSpace(fb.Username), strings.TrimSpace(rowUser)) {
				matched = fb
				found = true
				break
			}
		}
	}
	if !found && strings.TrimSpace(rowEmail) != "" {
		for _, fb := range fallbackAdmins {
			if strings.EqualFold(strings.TrimSpace(fb.Email), strings.TrimSpace(rowEmail)) {
				matched = fb
				found = true
				break
			}
		}
	}
	row["password_hash"] = matched.PasswordHash
	row["legacy_hash"] = matched.LegacyHash
}

func coerceJSONValForDB(table string, col string, val any, dialectName string) any {
	if val == nil {
		return nil
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
		"webhook_deliveries.success",
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
