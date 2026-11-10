package render

import (
	"strings"
	"testing"
)

var sampleBenchmarkMarkdown = strings.Repeat(strings.Join([]string{
	"## Pure-Go Sovereign Publishing Engine",
	"",
	"Volo-go compiles into a single static binary (`CGO_ENABLED=0`) under 21 MiB with live RSS under 35 MiB.",
	"",
	"| Capability | Implementation | Memory Budget |",
	"| --- | --- | --- |",
	"| SQLite WAL | `modernc.org/sqlite` split read/write pools | `4 MiB` page cache |",
	"| Markdown | `goldmark` + `chroma` + `bluemonday` | `4 MiB` render LRU |",
	"",
	"```go",
	"func ServeRange(w http.ResponseWriter, r *http.Request, rs io.ReadSeeker, mod time.Time) {",
	"    http.ServeContent(w, r, \"episode.mp3\", mod, rs)",
	"}",
	"```",
	"",
	"Listen at [04:12] and cross-reference [[architecture-overview]].",
	"",
}, "\n"), 4)

func BenchmarkMarkdownRenderWeb(b *testing.B) {
	eng := NewEngine()
	opts := Options{
		Mode:            RenderModeWeb,
		Locale:          "en-US",
		HasAudio:        true,
		ShowLineNumbers: true,
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		out := eng.Render(sampleBenchmarkMarkdown, opts)
		if out.HTML == "" {
			b.Fatal("empty render output")
		}
	}
}
