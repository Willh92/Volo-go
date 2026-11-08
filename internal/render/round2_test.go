package render

import (
	"strings"
	"testing"
)

func TestMultipleUnlockedLockBlocksPreserveDistinctInnerCodePlaceholders(t *testing.T) {
	eng := NewEngine()
	md := strings.Join([]string{
		":::lock type=\"user\"",
		"First unlocked section with $a_1 + b_1$:",
		"",
		"```go",
		"const FirstBlock = \"ALPHA_DISTINCT_TOKEN\"",
		"```",
		":::",
		"",
		":::lock type=\"user\"",
		"Second unlocked section with $x_2 + y_2$:",
		"",
		"```go",
		"const SecondBlock = \"BETA_DISTINCT_TOKEN\"",
		"```",
		":::",
	}, "\n")

	out := eng.Render(md, Options{
		Mode:             RenderModeWeb,
		Locale:           "en-US",
		IsReaderLoggedIn: true,
	})
	if !strings.Contains(out.HTML, "ALPHA_DISTINCT_TOKEN") || !strings.Contains(out.HTML, "BETA_DISTINCT_TOKEN") {
		t.Fatalf("INNER placeholder collision lost unlocked lock block code content:\n%s", out.HTML)
	}
	if !strings.Contains(out.HTML, "a_1 + b_1") || !strings.Contains(out.HTML, "x_2 + y_2") {
		t.Fatalf("INNER placeholder collision lost unlocked lock block math content:\n%s", out.HTML)
	}
}

func TestIsSafeImageSrcBlocksProtocolRelativeURLs(t *testing.T) {
	if isSafeImageSrc("//evil.example/tracker.png") {
		t.Fatal("expected protocol-relative //evil.example URL to be rejected")
	}
	if isSafeImageSrc(`/\evil.example/tracker.png`) {
		t.Fatal(`expected /\evil.example URL to be rejected`)
	}
	if !isSafeImageSrc("/uploads/2026/11/photo.webp") {
		t.Fatal("expected local /uploads/ path to be accepted")
	}
	if !isSafeImageSrc("https://cdn.example.com/photo.webp") {
		t.Fatal("expected https:// URL to be accepted")
	}
}
