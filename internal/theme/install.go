package theme

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
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

	subFS, err := fs.Sub(builtinThemeFS, "builtin/volo-studio")
	if err != nil {
		return Manifest{}, err
	}
	e.mu.RLock()
	var builtinAssets map[string]cachedAsset
	if bt := e.themes[DefaultThemeID]; bt != nil {
		builtinAssets = bt.assets
	}
	e.mu.RUnlock()
	compiled, err := e.compileThemeFS(os.DirFS(stageDir), subFS, builtinAssets, false, nil, nil)
	if err != nil {
		return Manifest{}, fmt.Errorf("theme: validation failed: %w", err)
	}
	id := strings.TrimSpace(compiled.manifest.ID)
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
	return compiled.manifest, nil
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
