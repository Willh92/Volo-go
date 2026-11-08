package theme

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const (
	maxThemeZipFiles     = 512
	maxThemeTotalBytes   = 32 << 20 // 32 MiB
	maxThemeSingleBytes  = 8 << 20  // 8 MiB
)

var validThemeIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{1,62}$`)

// InstallZip safely extracts, validates, and compiles a theme `.zip` package into `themesDir/<id>`,
// supporting both flat archives (`theme.json` at root) and single-folder archives (`<folder>/theme.json`).
func (e *Engine) InstallZip(r io.ReaderAt, size int64, overwrite bool) (Manifest, error) {
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

	// Detect root prefix from location of `theme.json`
	rootPrefix := ""
	foundManifest := false
	for _, zf := range zr.File {
		if zf.FileInfo().IsDir() {
			continue
		}
		clean := normalizeZipEntry(zf.Name)
		if clean == "" || strings.HasPrefix(clean, "__MACOSX/") {
			continue
		}
		if clean == "theme.json" {
			rootPrefix = ""
			foundManifest = true
			break
		}
		parts := strings.Split(clean, "/")
		if len(parts) == 2 && parts[1] == "theme.json" {
			rootPrefix = parts[0] + "/"
			foundManifest = true
			break
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
		mode := zf.Mode()
		if mode&os.ModeSymlink != 0 {
			return Manifest{}, errors.New("theme: symbolic links are forbidden inside theme archive")
		}
		clean := normalizeZipEntry(zf.Name)
		if clean == "" || strings.HasPrefix(clean, "__MACOSX/") {
			continue
		}
		if rootPrefix != "" {
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

	finalDir := filepath.Join(e.themesDir, id)
	if _, statErr := os.Stat(finalDir); statErr == nil {
		if !overwrite {
			return Manifest{}, fmt.Errorf("theme %q already exists", id)
		}
		backupDir := filepath.Join(e.themesDir, ".theme-old-"+id)
		_ = os.RemoveAll(backupDir)
		if err := os.Rename(finalDir, backupDir); err != nil {
			return Manifest{}, fmt.Errorf("theme: prepare overwrite: %w", err)
		}
		if err := os.Rename(stageDir, finalDir); err != nil {
			_ = os.Rename(backupDir, finalDir)
			return Manifest{}, fmt.Errorf("theme: activate staged directory: %w", err)
		}
		_ = os.RemoveAll(backupDir)
	} else {
		if err := os.Rename(stageDir, finalDir); err != nil {
			return Manifest{}, fmt.Errorf("theme: install directory: %w", err)
		}
	}

	if err := e.Reload(); err != nil {
		return Manifest{}, err
	}
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
	return e.Reload()
}

func normalizeZipEntry(raw string) string {
	slashed := strings.ReplaceAll(raw, `\`, "/")
	if strings.HasPrefix(slashed, "/") || strings.Contains(slashed, "../") || strings.HasSuffix(slashed, "/..") || slashed == ".." {
		return ""
	}
	cleaned := strings.TrimPrefix(path.Clean("/"+slashed), "/")
	if cleaned == "." || cleaned == "" || strings.HasPrefix(cleaned, "..") {
		return ""
	}
	return cleaned
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

// ExportZip streams the specified built-in or external theme package as a standard `.zip` archive.
func (e *Engine) ExportZip(w io.Writer, themeID string) error {
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

	entriesByPath := make(map[string][]byte)

	err := fs.WalkDir(th.fsys, ".", func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() || p == "." {
			return nil
		}
		baseName := path.Base(p)
		if strings.HasPrefix(baseName, ".") {
			return nil
		}
		data, rErr := fs.ReadFile(th.fsys, p)
		if rErr != nil {
			return rErr
		}
		for _, pair := range [][2]string{{"assets_gz/", "assets/"}, {"templates_gz/", "templates/"}} {
			if strings.HasPrefix(p, pair[0]) && strings.HasSuffix(p, ".gz") {
				relOut := pair[1] + strings.TrimSuffix(strings.TrimPrefix(p, pair[0]), ".gz")
				if _, exists := entriesByPath[relOut]; !exists {
					gr, gErr := gzip.NewReader(bytes.NewReader(data))
					if gErr == nil {
						inflated, iErr := io.ReadAll(io.LimitReader(gr, maxThemeSingleBytes))
						_ = gr.Close()
						if iErr == nil {
							entriesByPath[relOut] = inflated
							return nil
						}
					}
				}
				return nil
			}
		}
		if !strings.Contains(p, "/") && strings.HasSuffix(p, ".gz") {
			relOut := strings.TrimSuffix(p, ".gz")
			if _, exists := entriesByPath[relOut]; !exists {
				if gr, gErr := gzip.NewReader(bytes.NewReader(data)); gErr == nil {
					inflated, iErr := io.ReadAll(io.LimitReader(gr, maxThemeSingleBytes))
					_ = gr.Close()
					if iErr == nil {
						entriesByPath[relOut] = inflated
						return nil
					}
				}
			}
			return nil
		}
		entriesByPath[p] = data
		return nil
	})
	if err != nil {
		return fmt.Errorf("theme: walk %s: %w", id, err)
	}
	if len(entriesByPath) == 0 {
		return fmt.Errorf("theme %q has no exportable files", id)
	}

	sortedKeys := make([]string, 0, len(entriesByPath))
	for k := range entriesByPath {
		sortedKeys = append(sortedKeys, k)
	}
	sort.Strings(sortedKeys)

	zw := zip.NewWriter(w)
	for _, k := range sortedKeys {
		hdr := &zip.FileHeader{
			Name:   k,
			Method: zip.Deflate,
		}
		hdr.SetMode(0o644)
		fw, cErr := zw.CreateHeader(hdr)
		if cErr != nil {
			_ = zw.Close()
			return cErr
		}
		if _, wErr := fw.Write(entriesByPath[k]); wErr != nil {
			_ = zw.Close()
			return wErr
		}
	}
	return zw.Close()
}
