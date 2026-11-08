package render

import (
	"strings"
	"testing"
)

func TestReadableHeadingIDsAndGitHubShowcaseTable(t *testing.T) {
	eng := NewEngine()

	md := strings.Join([]string{
		"## 架构设计与概览",
		"",
		"![GitHub Stats](/github-stats/octocat.svg)",
		"",
		"## 架构设计与概览",
		"",
		"| 仓库 | 项目简介 | Stars | Forks | 编程语言 |",
		"| --- | --- | :---: | :---: | --- |",
		"| [hello-world](https://github.com/octocat/hello-world) | 纯 Go 零外部依赖独立博客引擎，内置多方言与双栏工作室编辑器 | 128 | 16 | Go |",
		"",
		"| 指标 | 数值 |",
		"| --- | --- |",
		"| Idle RSS | 18 MiB |",
	}, "\n")

	out := eng.Render(md, Options{Mode: RenderModeWeb, Locale: "zh-CN"})

	// 1. Readable CJK heading IDs + collision suffix + TOC synchronization
	if len(out.TOC) != 2 {
		t.Fatalf("expected 2 TOC items, got %+v", out.TOC)
	}
	if out.TOC[0].ID != "架构设计与概览" || out.TOC[1].ID != "架构设计与概览-2" {
		t.Fatalf("unexpected TOC IDs: %+v", out.TOC)
	}
	if !strings.Contains(out.HTML, `<h2 id="架构设计与概览">`) || !strings.Contains(out.HTML, `<h2 id="架构设计与概览-2">`) {
		t.Fatalf("expected readable CJK heading IDs in HTML, got: %s", out.HTML)
	}

	// 2. `/github-stats/{username}.svg` wrapped in external profile link
	if !strings.Contains(out.HTML, `href="https://github.com/octocat"`) || !strings.Contains(out.HTML, `data-github-profile="1"`) {
		t.Fatalf("expected github-stats SVG wrapped in profile link, got: %s", out.HTML)
	}

	// 3. 5-column GitHub showcase table transformed with `.volo-github-repos-wrap`, `.volo-github-repos-table`, `.volo-github-repo-desc[title]`
	if !strings.Contains(out.HTML, `<div class="volo-github-repos-wrap"><table class="volo-github-repos-table">`) {
		t.Fatalf("expected GitHub repos table wrapper & class, got: %s", out.HTML)
	}
	if !strings.Contains(out.HTML, `class="volo-github-repo-desc-cell"`) || !strings.Contains(out.HTML, `class="volo-github-repo-desc" title="纯 Go 零外部依赖独立博客引擎，内置多方言与双栏工作室编辑器"`) {
		t.Fatalf("expected GitHub repo 2-line clamp div with title attribute, got: %s", out.HTML)
	}
	if !strings.Contains(out.HTML, `href="https://github.com/octocat/hello-world"`) {
		t.Fatalf("expected repo link preserved, got: %s", out.HTML)
	}

	// 4. General prose table wrapped in `.volo-table-scroll`
	if !strings.Contains(out.HTML, `<div class="volo-table-scroll"><table>`) {
		t.Fatalf("expected general prose table wrapped in .volo-table-scroll, got: %s", out.HTML)
	}
}
