package render

import (
	"strings"
	"testing"
)

func TestAlignmentWrappersRenderInnerMarkdown(t *testing.T) {
	cases := []struct {
		name, src string
		want      []string
		notWant   []string
	}{
		{
			name:    "div align without blank lines",
			src:     "<div align=center>\n[![GitHub Repo](/images/github_repo.jpg)](https://github.com/willh92)\n</div>\nafter **bold**\n",
			want:    []string{`<div align="center">`, `<a href="https://github.com/willh92"`, `<img src="/images/github_repo.jpg"`, `<strong>bold</strong>`},
			notWant: []string{"[![GitHub Repo]"},
		},
		{
			name:    "p align promoted to div",
			src:     "<p align=\"right\">\n*x*\n</p>\n",
			want:    []string{`<div align="right">`, `<em>x</em>`},
			notWant: []string{`<p align`},
		},
		{
			name:    "indented html left untouched",
			src:     "<div align=\"center\">\n    <img src=\"/a.png\">\n</div>\n",
			want:    []string{`<div align="center">`, `<img src="/a.png"`},
			notWant: []string{"<pre", "<code"},
		},
		{
			name:    "fenced code is not rewritten",
			src:     "```html\n<div align=center>\n*x*\n</div>\n```\n",
			notWant: []string{`<div align="center">`, "<em>"},
		},
		{
			name:    "align attribute value is whitelisted",
			src:     "<div align=\"javascript:x\">a</div>\n<img align=\"right\" src=\"/a.png\">\n",
			want:    []string{`<img align="right"`},
			notWant: []string{"javascript"},
		},
	}
	for _, tc := range cases {
		out := Default().Render(tc.src, Options{}).HTML
		for _, w := range tc.want {
			if !strings.Contains(out, w) {
				t.Errorf("%s: missing %q in\n%s", tc.name, w, out)
			}
		}
		for _, nw := range tc.notWant {
			if strings.Contains(out, nw) {
				t.Errorf("%s: unexpected %q in\n%s", tc.name, nw, out)
			}
		}
	}
}

func TestTrustedMarkdownAlignmentWrappers(t *testing.T) {
	out := Default().RenderTrustedMarkdownHTML("<p align=\"center\">\n**公告** [link](https://example.com)\n</p>\n")
	for _, w := range []string{`<div align="center">`, `<strong>公告</strong>`, `<a href="https://example.com"`} {
		if !strings.Contains(out, w) {
			t.Errorf("missing %q in\n%s", w, out)
		}
	}
	if strings.Contains(out, "**公告**") {
		t.Errorf("markdown inside alignment wrapper was not rendered:\n%s", out)
	}
}
