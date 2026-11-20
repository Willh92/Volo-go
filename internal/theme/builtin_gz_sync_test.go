package theme

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func gunzipFileBytes(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	gr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("gzip.NewReader(%s): %v", path, err)
	}
	defer gr.Close()
	out, err := io.ReadAll(gr)
	if err != nil {
		t.Fatalf("io.ReadAll(%s): %v", path, err)
	}
	return out
}

func TestBuiltinVoloStudioGzParity(t *testing.T) {
	baseDir := filepath.Join("builtin", "volo-studio")

	// 1. theme.json <-> theme.json.gz
	srcManifestRaw, err := os.ReadFile(filepath.Join(baseDir, "theme.json"))
	if err != nil {
		t.Fatalf("read theme.json: %v", err)
	}
	gzManifestRaw := gunzipFileBytes(t, filepath.Join(baseDir, "theme.json.gz"))
	var srcManifestObj, gzManifestObj any
	if err := json.Unmarshal(srcManifestRaw, &srcManifestObj); err != nil {
		t.Fatalf("unmarshal source theme.json: %v", err)
	}
	if err := json.Unmarshal(gzManifestRaw, &gzManifestObj); err != nil {
		t.Fatalf("unmarshal gzipped theme.json.gz: %v", err)
	}
	if !reflect.DeepEqual(srcManifestObj, gzManifestObj) {
		t.Fatalf("theme.json and theme.json.gz differ semantically")
	}

	// 2. preview.svg <-> preview.svg.gz
	srcPreview, err := os.ReadFile(filepath.Join(baseDir, "preview.svg"))
	if err != nil {
		t.Fatalf("read preview.svg: %v", err)
	}
	gzPreview := gunzipFileBytes(t, filepath.Join(baseDir, "preview.svg.gz"))
	if !strings.Contains(string(srcPreview), "<svg") || !strings.Contains(string(gzPreview), "<svg") {
		t.Fatalf("expected both preview.svg and preview.svg.gz to contain <svg")
	}

	// 3. 1-to-1 templates/*.html <-> templates_gz/*.html.gz
	checkDirPair := func(srcDir, gzDir string, allowedExt func(string) bool) map[string][]byte {
		srcEntries, err := os.ReadDir(srcDir)
		if err != nil {
			t.Fatalf("ReadDir(%s): %v", srcDir, err)
		}
		gzEntries, err := os.ReadDir(gzDir)
		if err != nil {
			t.Fatalf("ReadDir(%s): %v", gzDir, err)
		}
		var srcNames, gzNames []string
		for _, e := range srcEntries {
			if !e.IsDir() && allowedExt(e.Name()) {
				srcNames = append(srcNames, e.Name())
			}
		}
		for _, e := range gzEntries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".gz") {
				gzNames = append(gzNames, strings.TrimSuffix(e.Name(), ".gz"))
			}
		}
		sort.Strings(srcNames)
		sort.Strings(gzNames)
		if !reflect.DeepEqual(srcNames, gzNames) {
			t.Fatalf("1-to-1 parity mismatch between %s %v and %s %v", srcDir, srcNames, gzDir, gzNames)
		}
		decompressed := make(map[string][]byte, len(gzNames))
		for _, name := range gzNames {
			decompressed[name] = gunzipFileBytes(t, filepath.Join(gzDir, name+".gz"))
		}
		return decompressed
	}

	tpls := checkDirPair(
		filepath.Join(baseDir, "templates"),
		filepath.Join(baseDir, "templates_gz"),
		func(name string) bool { return strings.HasSuffix(name, ".html") },
	)
	assets := checkDirPair(
		filepath.Join(baseDir, "assets"),
		filepath.Join(baseDir, "assets_gz"),
		func(name string) bool { return strings.HasSuffix(name, ".css") || strings.HasSuffix(name, ".js") },
	)

	// 4. Key structural markers in decompressed .gz match source edits (catches stale .gz artifacts).
	bookSrc, err := os.ReadFile(filepath.Join(baseDir, "templates", "book_reader.html"))
	if err != nil {
		t.Fatalf("read book_reader.html: %v", err)
	}
	bookGz := string(tpls["book_reader.html"])
	for _, want := range []string{
		"book-reader-toc__volume-summary",
		"book-reader-toc__volume-chevron",
		"book-reader-toc__volume-items",
		"cpageURL",
		"bookTOCWindow",
	} {
		if !strings.Contains(string(bookSrc), want) || !strings.Contains(bookGz, want) {
			t.Fatalf("expected book_reader.html and book_reader.html.gz to both contain %q", want)
		}
	}
	for _, forbidden := range []string{`book-reader-toc__volume-sum"`, `book-reader-toc__volume-list"`} {
		if strings.Contains(string(bookSrc), forbidden) || strings.Contains(bookGz, forbidden) {
			t.Fatalf("unexpected stale class %q in book_reader.html / .gz", forbidden)
		}
	}

	taxGz := string(tpls["taxonomies.html"])
	for _, want := range []string{`action="/categories"`, `data-server-filter="1"`, `js-jump-pills-toggle`} {
		if !strings.Contains(taxGz, want) {
			t.Fatalf("expected taxonomies.html.gz to contain %q", want)
		}
	}
	linksGz := string(tpls["links.html"])
	if !strings.Contains(linksGz, `action="/links"`) || !strings.Contains(linksGz, `data-server-filter="1"`) {
		t.Fatalf("expected links.html.gz to contain server-side /links search form")
	}

	cssGz := string(assets["volo-studio.css"])
	for _, want := range []string{"--studio-shell-max", "overflow-wrap:anywhere", "max-content", "pagination--compact"} {
		if !strings.Contains(cssGz, want) {
			t.Fatalf("expected volo-studio.css.gz to contain %q", want)
		}
	}
	cwGz := string(assets["comment-widget.js"])
	if !strings.Contains(cwGz, "execCommand") || !strings.Contains(cwGz, "data-copied") {
		t.Fatalf("expected comment-widget.js.gz to contain execCommand fallback and data-copied")
	}
	jsGz := string(assets["volo-studio.js"])
	if !strings.Contains(jsGz, "data-server-filter") || !strings.Contains(jsGz, "vc-code-wrap") {
		t.Fatalf("expected volo-studio.js.gz to contain data-server-filter and vc-code-wrap")
	}
}
