package theme

import (
	"archive/zip"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/Willh92/Volo-go/internal/extpkg"
)

const (
	maxThemeZipFiles      = 512
	maxThemeExportEntries = 500
	maxThemeTotalBytes    = 32 << 20 // 32 MiB
	maxThemeSingleBytes   = 8 << 20  // 8 MiB
)

var validThemeIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{1,62}$`)

var themeExportCopyBufPool = sync.Pool{
	New: func() any {
		buf := make([]byte, 32*1024)
		return &buf
	},
}

type installThemeZipOptions struct {
	overwrite           bool
	expectedThemeID     string
	expectedPrevVersion string
	disallowDowngrade   bool
}

// InstallZip safely extracts, validates, and compiles a theme `.zip` package into `themesDir/<id>`,
// supporting both flat archives (`theme.json` at root) and single-folder archives (`<folder>/theme.json`).
func (e *Engine) InstallZip(r io.ReaderAt, size int64, overwrite bool) (Manifest, error) {
	return e.InstallZipContext(context.Background(), r, size, overwrite)
}

// InstallZipContext safely extracts, validates, and installs a theme `.zip` package with pre-commit
// context cancellation and post-swap transactional rollback if engine reload fails.
func (e *Engine) InstallZipContext(ctx context.Context, r io.ReaderAt, size int64, overwrite bool) (Manifest, error) {
	return e.installZipInternal(ctx, r, size, installThemeZipOptions{
		overwrite: overwrite,
	})
}

func sanitizeThemeZipEntry(zf *zip.File) (clean string, skip bool, err error) {
	mode := zf.Mode()
	if mode&os.ModeSymlink != 0 {
		return "", false, errors.New("theme: symbolic links are forbidden inside theme archive")
	}
	isDir := zf.FileInfo().IsDir()
	if !isDir && !mode.IsRegular() {
		return "", false, fmt.Errorf("theme: non-regular entry %q is forbidden inside theme archive", zf.Name)
	}

	raw := zf.Name
	if isDir {
		if strings.HasSuffix(raw, "/") && !strings.HasPrefix(raw, "/") {
			raw = strings.TrimSuffix(raw, "/")
		}
		if raw == "__MACOSX" || strings.HasPrefix(raw, "__MACOSX/") {
			return "", true, nil
		}
	} else if strings.HasPrefix(raw, "__MACOSX/") {
		return "", true, nil
	}

	clean, err = extpkg.SanitizeZipPath(raw)
	if err != nil {
		return "", false, fmt.Errorf("theme: invalid zip entry %q: %w", zf.Name, err)
	}
	return clean, false, nil
}

func (e *Engine) installZipInternal(ctx context.Context, r io.ReaderAt, size int64, opts installThemeZipOptions) (Manifest, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return Manifest{}, err
	}
	if strings.TrimSpace(e.themesDir) == "" {
		return Manifest{}, errors.New("theme: external themes directory is not configured")
	}
	if size <= 0 || size > maxThemeTotalBytes {
		return Manifest{}, fmt.Errorf("theme: zip archive size must be between 1 byte and %d MiB", maxThemeTotalBytes>>20)
	}

	zr, err := zip.NewReader(r, size)
	if err != nil {
		return Manifest{}, fmt.Errorf("theme: invalid zip archive: %w", err)
	}
	if len(zr.File) == 0 || len(zr.File) > maxThemeZipFiles {
		return Manifest{}, fmt.Errorf("theme: zip archive must contain 1..%d entries", maxThemeZipFiles)
	}

	if err := os.MkdirAll(e.themesDir, 0o755); err != nil {
		return Manifest{}, fmt.Errorf("theme: create themes directory: %w", err)
	}

	// Validate all archive paths & modes upfront and detect root prefix from location of `theme.json`.
	rootPrefix := ""
	foundManifest := false
	for _, zf := range zr.File {
		clean, skip, sErr := sanitizeThemeZipEntry(zf)
		if sErr != nil {
			return Manifest{}, sErr
		}
		if skip || zf.FileInfo().IsDir() {
			continue
		}
		if clean == "theme.json" {
			rootPrefix = ""
			foundManifest = true
		} else if !foundManifest {
			parts := strings.Split(clean, "/")
			if len(parts) == 2 && parts[1] == "theme.json" {
				rootPrefix = parts[0] + "/"
				foundManifest = true
			}
		}
	}
	if !foundManifest {
		return Manifest{}, errors.New("theme: theme.json not found at archive root or top-level directory")
	}

	stageDir, err := os.MkdirTemp(e.themesDir, ".theme-stage-*")
	if err != nil {
		return Manifest{}, fmt.Errorf("theme: create staging directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(stageDir) }()

	var totalExtracted int64
	for _, zf := range zr.File {
		if err := ctx.Err(); err != nil {
			return Manifest{}, err
		}
		clean, skip, sErr := sanitizeThemeZipEntry(zf)
		if sErr != nil {
			return Manifest{}, sErr
		}
		if skip {
			continue
		}
		if rootPrefix != "" {
			if clean+"/" == rootPrefix {
				continue
			}
			if !strings.HasPrefix(clean, rootPrefix) {
				continue
			}
			clean = strings.TrimPrefix(clean, rootPrefix)
			if clean == "" {
				continue
			}
		}
		if strings.HasSuffix(strings.ToLower(clean), ".properties") {
			return Manifest{}, errors.New("theme: legacy .properties files are not supported; use locales/*.json")
		}
		if zf.FileInfo().IsDir() {
			if err := os.MkdirAll(filepath.Join(stageDir, filepath.FromSlash(clean)), 0o755); err != nil {
				return Manifest{}, err
			}
			continue
		}
		if zf.UncompressedSize64 > maxThemeSingleBytes {
			return Manifest{}, fmt.Errorf("theme: file %s exceeds %d MiB single-file limit", clean, maxThemeSingleBytes>>20)
		}

		destPath := filepath.Join(stageDir, filepath.FromSlash(clean))
		relCheck, relErr := filepath.Rel(stageDir, destPath)
		if relErr != nil || strings.HasPrefix(relCheck, "..") {
			return Manifest{}, errors.New("theme: path traversal rejected")
		}
		if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
			return Manifest{}, err
		}

		rc, oErr := zf.Open()
		if oErr != nil {
			return Manifest{}, oErr
		}
		outFile, cErr := os.OpenFile(destPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if cErr != nil {
			_ = rc.Close()
			return Manifest{}, cErr
		}
		written, wErr := io.Copy(outFile, io.LimitReader(rc, maxThemeSingleBytes+1))
		_ = outFile.Close()
		_ = rc.Close()
		if wErr != nil {
			return Manifest{}, wErr
		}
		if written > maxThemeSingleBytes {
			return Manifest{}, fmt.Errorf("theme: file %s exceeds %d MiB single-file limit", clean, maxThemeSingleBytes>>20)
		}
		totalExtracted += written
		if totalExtracted > maxThemeTotalBytes {
			return Manifest{}, fmt.Errorf("theme: total extracted bytes exceed %d MiB limit", maxThemeTotalBytes>>20)
		}
	}

	if err := ensureStagedHomeFallbackAlias(stageDir); err != nil {
		return Manifest{}, fmt.Errorf("theme: prepare home template alias: %w", err)
	}
	subFS, err := fs.Sub(builtinThemeFS, "builtin/volo-studio")
	if err != nil {
		return Manifest{}, err
	}
	e.mu.RLock()
	bt := e.themes[DefaultThemeID]
	e.mu.RUnlock()
	mf, err := e.validateStagedThemeFS(os.DirFS(stageDir), subFS, bt)
	if err != nil {
		return Manifest{}, fmt.Errorf("theme: validation failed: %w", err)
	}
	id := strings.TrimSpace(mf.ID)
	if !validThemeIDRe.MatchString(id) {
		return Manifest{}, fmt.Errorf("theme: invalid id %q (must match [a-z0-9][a-z0-9_-]{1,62})", id)
	}
	if id == DefaultThemeID {
		return Manifest{}, fmt.Errorf("theme: %q is reserved for the built-in theme", DefaultThemeID)
	}
	if opts.expectedThemeID != "" && id != opts.expectedThemeID {
		return Manifest{}, fmt.Errorf("theme: archive theme.json id %q does not match expected %q", id, opts.expectedThemeID)
	}
	if !e.effectiveOutboundDialer().AllowLoopbackForTests && mf.Update != nil && mf.Update.URL != "" {
		if _, uErr := extpkg.ValidatePublicHTTPURL(mf.Update.URL, false); uErr != nil {
			return Manifest{}, fmt.Errorf("theme %q: update.url: %w", id, uErr)
		}
	}
	if !extpkg.IsHostCompatible(e.effectiveHostVersion(), mf.MinHostVersion) {
		return Manifest{}, fmt.Errorf(
			"theme %q v%s requires host Volo-go >= %s (current: %s)",
			id, mf.Version, mf.MinHostVersion, e.effectiveHostVersion(),
		)
	}

	// Final pre-commit cancellation gate before atomic directory swap.
	if err := ctx.Err(); err != nil {
		return Manifest{}, err
	}

	e.installMu.Lock()
	defer e.installMu.Unlock()

	existingMf, hasExisting := e.getManifest(id)
	if opts.expectedPrevVersion != "" && (!hasExisting || existingMf.Version != opts.expectedPrevVersion) {
		return Manifest{}, fmt.Errorf("theme %q version changed concurrently (expected %s, found %s)", id, opts.expectedPrevVersion, existingMf.Version)
	}
	if opts.disallowDowngrade && hasExisting && strings.TrimSpace(existingMf.Version) != "" {
		if cmp, ok := extpkg.CompareSemVer(mf.Version, existingMf.Version); !ok || cmp <= 0 {
			return Manifest{}, fmt.Errorf("theme %q downgrade or same-version install rejected (%s -> %s)", id, existingMf.Version, mf.Version)
		}
	}

	finalDir := filepath.Join(e.themesDir, id)
	backupDir := filepath.Join(e.themesDir, ".theme-old-"+id)
	hadBackup := false

	if _, statErr := os.Stat(finalDir); statErr == nil {
		if !opts.overwrite {
			return Manifest{}, fmt.Errorf("theme %q already exists", id)
		}
		_ = os.RemoveAll(backupDir)
		if err := os.Rename(finalDir, backupDir); err != nil {
			return Manifest{}, fmt.Errorf("theme: prepare overwrite: %w", err)
		}
		hadBackup = true
		if err := os.Rename(stageDir, finalDir); err != nil {
			_ = os.Rename(backupDir, finalDir)
			return Manifest{}, fmt.Errorf("theme: activate staged directory: %w", err)
		}
	} else {
		if err := os.Rename(stageDir, finalDir); err != nil {
			return Manifest{}, fmt.Errorf("theme: install directory: %w", err)
		}
	}

	if err := e.reloadLocked(); err != nil {
		_ = os.RemoveAll(finalDir)
		if hadBackup {
			if rErr := os.Rename(backupDir, finalDir); rErr != nil {
				slog.Error("theme: failed to restore backup directory during rollback", "theme_id", id, "err", rErr)
			}
		}
		_ = e.reloadLocked()
		e.invalidateUpdateStatus(id)
		return Manifest{}, fmt.Errorf("theme %q install/upgrade failed (safely rolled back): %w", id, err)
	}

	if hadBackup {
		_ = os.RemoveAll(backupDir)
	}
	e.invalidateUpdateStatus(id)
	return mf, nil
}

// Uninstall removes an external theme directory from `themesDir` and reloads the engine.
func (e *Engine) Uninstall(themeID string) error {
	id := strings.TrimSpace(themeID)
	if id == "" || id == DefaultThemeID {
		return errors.New("theme: built-in theme cannot be uninstalled")
	}
	if !validThemeIDRe.MatchString(id) {
		return errors.New("theme: invalid theme id")
	}
	if strings.TrimSpace(e.themesDir) == "" {
		return errors.New("theme: external themes directory is not configured")
	}

	e.installMu.Lock()
	defer e.installMu.Unlock()

	target := filepath.Join(e.themesDir, id)
	rel, err := filepath.Rel(e.themesDir, target)
	if err != nil || strings.HasPrefix(rel, "..") || strings.ContainsRune(rel, filepath.Separator) {
		return errors.New("theme: invalid theme path")
	}
	if _, statErr := os.Stat(target); os.IsNotExist(statErr) {
		return fmt.Errorf("theme %q not found", id)
	}
	if err := os.RemoveAll(target); err != nil {
		return fmt.Errorf("theme: remove directory: %w", err)
	}
	e.invalidateUpdateStatus(id)
	return e.reloadLocked()
}

// ensureStagedHomeFallbackAlias validates `templates/home.html` when `templates/index.html` is omitted
// so external theme archives using `home.html` as their homepage alias pass staged verification cleanly.
func ensureStagedHomeFallbackAlias(stageDir string) error {
	indexPath := filepath.Join(stageDir, "templates", "index.html")
	indexGzPath := filepath.Join(stageDir, "templates_gz", "index.html.gz")
	if _, err := os.Stat(indexPath); err == nil {
		return nil
	}
	if _, err := os.Stat(indexGzPath); err == nil {
		return nil
	}
	homePath := filepath.Join(stageDir, "templates", "home.html")
	st, err := os.Lstat(homePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if !st.Mode().IsRegular() {
		return errors.New("theme: templates/home.html must be a regular file")
	}
	return nil
}

type themeExportSource struct {
	srcPath string
	rawSize int64
	isGzip  bool
}

func resolveExportTargetPath(p string) (relOut string, isGzip bool) {
	for _, pair := range [][2]string{
		{"assets_gz/", "assets/"},
		{"templates_gz/", "templates/"},
		{"locales_gz/", "locales/"},
	} {
		if strings.HasPrefix(p, pair[0]) && strings.HasSuffix(p, ".gz") {
			return pair[1] + strings.TrimSuffix(strings.TrimPrefix(p, pair[0]), ".gz"), true
		}
	}
	if strings.HasSuffix(p, ".gz") {
		return strings.TrimSuffix(p, ".gz"), true
	}
	return p, false
}

// ExportZip streams the specified built-in or external theme package as a standard `.zip` archive.
func (e *Engine) ExportZip(w io.Writer, themeID string) error {
	return e.ExportZipContext(context.Background(), themeID, w)
}

// ExportZipContext streams the specified built-in or external theme package as a standard `.zip` archive
// using a bounded 2-pass walk and pooled 32 KiB copy buffer without buffering file contents in RAM.
func (e *Engine) ExportZipContext(ctx context.Context, themeID string, w io.Writer) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	id := strings.TrimSpace(themeID)
	if id == "" {
		return errors.New("theme: missing theme id")
	}
	e.mu.RLock()
	th, ok := e.themes[id]
	e.mu.RUnlock()
	if !ok || th == nil || th.fsys == nil {
		return fmt.Errorf("theme %q not found", id)
	}

	sources := make(map[string]themeExportSource)
	var rawSelectedBytes int64

	// Pass 1: collect regular file relative paths + metadata and enforce entry count & byte bounds.
	err := fs.WalkDir(th.fsys, ".", func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if p == "." {
			return nil
		}
		baseName := path.Base(p)
		if strings.HasPrefix(baseName, ".") {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("theme: symbolic link %q cannot be exported", p)
		}
		if d.IsDir() {
			return nil
		}
		info, iErr := d.Info()
		if iErr != nil {
			return iErr
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("theme: non-regular file %q cannot be exported", p)
		}

		relOut, isGzip := resolveExportTargetPath(p)
		cleanOut, sErr := extpkg.SanitizeZipPath(relOut)
		if sErr != nil {
			return fmt.Errorf("theme: invalid export path %q: %w", relOut, sErr)
		}
		if !isGzip && info.Size() > maxThemeSingleBytes {
			return fmt.Errorf("theme: entry %s exceeds %d bytes", cleanOut, maxThemeSingleBytes)
		}

		if prev, exists := sources[cleanOut]; exists {
			if isGzip || !prev.isGzip {
				return nil
			}
			rawSelectedBytes -= prev.rawSize
		} else if len(sources) >= maxThemeExportEntries {
			return fmt.Errorf("theme: export exceeds maximum %d file entries", maxThemeExportEntries)
		}

		rawSelectedBytes += info.Size()
		if rawSelectedBytes > maxThemeTotalBytes {
			return fmt.Errorf("theme: export exceeds %d MiB total limit", maxThemeTotalBytes>>20)
		}
		sources[cleanOut] = themeExportSource{
			srcPath: p,
			rawSize: info.Size(),
			isGzip:  isGzip,
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("theme: walk %s: %w", id, err)
	}
	if len(sources) == 0 {
		return fmt.Errorf("theme %q has no exportable files", id)
	}

	sortedKeys := make([]string, 0, len(sources))
	for k := range sources {
		sortedKeys = append(sortedKeys, k)
	}
	sort.Strings(sortedKeys)

	bufPtr := themeExportCopyBufPool.Get().(*[]byte)
	defer themeExportCopyBufPool.Put(bufPtr)
	copyBuf := *bufPtr

	// Pass 2: stream each selected file into zip.Writer using the pooled 32 KiB buffer.
	var totalUncompressed int64
	zw := zip.NewWriter(w)
	for _, relOut := range sortedKeys {
		if err := ctx.Err(); err != nil {
			_ = zw.Close()
			return err
		}
		src := sources[relOut]
		f, oErr := th.fsys.Open(src.srcPath)
		if oErr != nil {
			_ = zw.Close()
			return oErr
		}
		st, sErr := f.Stat()
		if sErr != nil {
			_ = f.Close()
			_ = zw.Close()
			return sErr
		}
		if !st.Mode().IsRegular() {
			_ = f.Close()
			_ = zw.Close()
			return fmt.Errorf("theme: non-regular file %q cannot be exported", src.srcPath)
		}

		var reader io.Reader = f
		var gzReader *gzip.Reader
		if src.isGzip {
			gr, gErr := gzip.NewReader(f)
			if gErr != nil {
				_ = f.Close()
				_ = zw.Close()
				return fmt.Errorf("decompress %s: %w", src.srcPath, gErr)
			}
			gzReader = gr
			reader = gr
		}

		hdr := &zip.FileHeader{
			Name:   relOut,
			Method: zip.Deflate,
		}
		hdr.SetMode(0o644)
		fw, cErr := zw.CreateHeader(hdr)
		if cErr != nil {
			if gzReader != nil {
				_ = gzReader.Close()
			}
			_ = f.Close()
			_ = zw.Close()
			return cErr
		}

		written, wErr := io.CopyBuffer(fw, io.LimitReader(reader, maxThemeSingleBytes+1), copyBuf)
		if gzReader != nil {
			_ = gzReader.Close()
		}
		_ = f.Close()
		if wErr != nil {
			_ = zw.Close()
			return fmt.Errorf("read %s: %w", src.srcPath, wErr)
		}
		if written > maxThemeSingleBytes {
			_ = zw.Close()
			return fmt.Errorf("theme entry %s exceeds %d bytes", src.srcPath, maxThemeSingleBytes)
		}
		totalUncompressed += written
		if totalUncompressed > maxThemeTotalBytes {
			_ = zw.Close()
			return fmt.Errorf("theme export %s exceeds %d MiB uncompressed limit", id, maxThemeTotalBytes>>20)
		}
	}
	return zw.Close()
}
