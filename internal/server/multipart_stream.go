package server

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

const (
	maxMultipartFormFieldBytes = 16 << 10 // 16 KiB per text form field
	maxMultipartFormFields     = 64
)

type streamedMultipartFile struct {
	_ [0]func()

	TempPath     string
	OrigFilename string
	SizeBytes    int64
	FormFields   map[string]string
}

type inMemoryMultipartFile struct {
	_ [0]func()

	FieldName string
	Filename  string
	Data      []byte
}

// streamMultipartSingleFile streams a single file upload directly from r.MultipartReader()
// to one bounded temporary file on disk without r.ParseMultipartForm double-spooling.
func streamMultipartSingleFile(
	w http.ResponseWriter,
	r *http.Request,
	fieldName string,
	maxBytes int64,
	tempDir string,
	tempPrefix string,
	defaultExt string,
	allowedExts ...string,
) (*streamedMultipartFile, error) {
	if maxBytes <= 0 {
		maxBytes = 32 << 20
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes+(2<<20))
	mr, err := r.MultipartReader()
	if err != nil {
		return nil, fmt.Errorf("upload parse error: %w", err)
	}

	fields := make(map[string]string)
	var out *streamedMultipartFile
	cleanup := func() {
		if out != nil && out.TempPath != "" {
			_ = os.Remove(out.TempPath)
		}
	}

	for {
		part, pErr := mr.NextPart()
		if pErr == io.EOF {
			break
		}
		if pErr != nil {
			cleanup()
			return nil, fmt.Errorf("upload stream error: %w", pErr)
		}

		formName := part.FormName()
		fileName := part.FileName()
		isTargetFile := fileName != "" && (fieldName == "" || formName == fieldName)

		if isTargetFile && out == nil {
			ext := strings.ToLower(filepath.Ext(filepath.Base(fileName)))
			if len(allowedExts) > 0 {
				validExt := false
				for _, ae := range allowedExts {
					if ext == strings.ToLower(ae) {
						validExt = true
						break
					}
				}
				if !validExt {
					ext = defaultExt
				}
			} else if ext == "" {
				ext = defaultExt
			}
			tmpFile, cErr := os.CreateTemp(tempDir, tempPrefix+"*"+ext)
			if cErr != nil {
				_ = part.Close()
				return nil, cErr
			}
			tmpPath := tmpFile.Name()
			written, copyErr := io.Copy(tmpFile, io.LimitReader(part, maxBytes+1))
			closeErr := tmpFile.Close()
			_ = part.Close()
			if copyErr != nil {
				_ = os.Remove(tmpPath)
				return nil, copyErr
			}
			if closeErr != nil {
				_ = os.Remove(tmpPath)
				return nil, closeErr
			}
			if written <= 0 {
				_ = os.Remove(tmpPath)
				return nil, fmt.Errorf("empty file upload")
			}
			if written > maxBytes {
				_ = os.Remove(tmpPath)
				return nil, fmt.Errorf("upload exceeds maximum size of %d bytes", maxBytes)
			}
			out = &streamedMultipartFile{
				TempPath:     tmpPath,
				OrigFilename: filepath.Base(fileName),
				SizeBytes:    written,
			}
			continue
		}

		if fileName == "" && formName != "" && len(fields) < maxMultipartFormFields {
			rawVal, rErr := io.ReadAll(io.LimitReader(part, maxMultipartFormFieldBytes))
			_ = part.Close()
			if rErr != nil {
				cleanup()
				return nil, rErr
			}
			if _, exists := fields[formName]; !exists {
				fields[formName] = string(rawVal)
			}
			continue
		}
		_ = part.Close()
	}

	if out == nil {
		target := fieldName
		if target == "" {
			target = "file"
		}
		return nil, fmt.Errorf("missing multipart '%s' field", target)
	}
	out.FormFields = fields
	return out, nil
}

// readMultipartInMemoryFiles streams small bounded multipart uploads (< 32 MiB) directly into memory
// without creating temporary files via r.ParseMultipartForm.
func readMultipartInMemoryFiles(
	w http.ResponseWriter,
	r *http.Request,
	maxTotalBytes int64,
	maxSingleBytes int64,
	maxFiles int,
) ([]inMemoryMultipartFile, map[string]string, error) {
	if maxTotalBytes <= 0 {
		maxTotalBytes = 32 << 20
	}
	if maxSingleBytes <= 0 || maxSingleBytes > maxTotalBytes {
		maxSingleBytes = maxTotalBytes
	}
	if maxFiles <= 0 {
		maxFiles = 200
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxTotalBytes+(2<<20))
	mr, err := r.MultipartReader()
	if err != nil {
		return nil, nil, fmt.Errorf("upload parse error: %w", err)
	}

	var files []inMemoryMultipartFile
	fields := make(map[string]string)
	var totalBytes int64

	for {
		part, pErr := mr.NextPart()
		if pErr == io.EOF {
			break
		}
		if pErr != nil {
			return nil, nil, fmt.Errorf("upload stream error: %w", pErr)
		}

		formName := part.FormName()
		fileName := part.FileName()
		if fileName != "" {
			if len(files) >= maxFiles {
				_ = part.Close()
				return nil, nil, fmt.Errorf("too many uploaded files (max %d)", maxFiles)
			}
			rem := maxTotalBytes - totalBytes
			limit := maxSingleBytes
			if rem < limit {
				limit = rem
			}
			raw, rErr := io.ReadAll(io.LimitReader(part, limit+1))
			_ = part.Close()
			if rErr != nil {
				return nil, nil, rErr
			}
			if int64(len(raw)) > limit {
				return nil, nil, fmt.Errorf("uploaded file %q exceeds size limit", filepath.Base(fileName))
			}
			totalBytes += int64(len(raw))
			files = append(files, inMemoryMultipartFile{
				FieldName: formName,
				Filename:  filepath.Base(fileName),
				Data:      raw,
			})
			continue
		}

		if formName != "" && len(fields) < maxMultipartFormFields {
			rawVal, rErr := io.ReadAll(io.LimitReader(part, maxMultipartFormFieldBytes))
			_ = part.Close()
			if rErr != nil {
				return nil, nil, rErr
			}
			if _, exists := fields[formName]; !exists {
				fields[formName] = string(rawVal)
			}
			continue
		}
		_ = part.Close()
	}

	return files, fields, nil
}

// escapeLikePattern escapes SQL LIKE wildcard metacharacters ('!', '%', '_') for use with `ESCAPE '!'`
// consistently across SQLite, PostgreSQL, and MySQL.
func escapeLikePattern(s string) string {
	if s == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(s) + 4)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '!' || c == '%' || c == '_' {
			b.WriteByte('!')
		}
		b.WriteByte(c)
	}
	return b.String()
}
