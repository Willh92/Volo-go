#!/usr/bin/env python3
"""Generate concise bilingual GitHub Release notes for Volo-go.

Priority:
1. Extract the matching `## [<version>]` section from `CHANGELOG.zh-CN.md` and
   `CHANGELOG.md`, condensing each top-level bullet into a crisp single-line
   highlight so the GitHub Release page stays concise and scannable.
2. Fall back to non-merge, non-bot git commit subjects since the previous `v*`
   tag when a tag is pushed before `CHANGELOG` is updated.
"""

from __future__ import annotations

import argparse
import pathlib
import re
import subprocess
import sys

BOT_PATTERNS = (
    "dependabot",
    "github-actions[bot]",
    "chore(deps)",
    "build(deps)",
)


def run_git(args: list[str]) -> str:
    try:
        res = subprocess.run(
            ["git", *args],
            check=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.DEVNULL,
            text=True,
        )
        return res.stdout.strip()
    except Exception:
        return ""


def find_previous_tag(current_tag: str) -> str:
    raw = run_git(["tag", "--list", "v*", "--sort=-v:refname"])
    if not raw:
        return ""
    tags = [t.strip() for t in raw.splitlines() if t.strip()]
    if current_tag in tags:
        idx = tags.index(current_tag)
        for cand in tags[idx + 1 :]:
            if cand != current_tag:
                return cand
        return ""
    for cand in tags:
        if cand != current_tag:
            return cand
    return ""


def extract_version_block(md_text: str, ver_no_v: str) -> list[str]:
    header_re = re.compile(
        rf"^##\s+\[?v?{re.escape(ver_no_v)}\]?(?:\s+-\s+.*|\s*)$",
        re.IGNORECASE,
    )
    next_sec_re = re.compile(r"^(?:##\s+\[?v?\d+\.\d+|---\s*$)")
    lines = md_text.splitlines()
    collecting = False
    out: list[str] = []
    for line in lines:
        if not collecting:
            if header_re.match(line.strip()):
                collecting = True
            continue
        if next_sec_re.match(line.strip()):
            break
        out.append(line)
    return out


def strip_long_parentheticals(text: str, min_inner_len: int) -> str:
    out: list[str] = []
    i = 0
    n = len(text)
    in_code = False
    while i < n:
        ch = text[i]
        if ch == "`":
            in_code = not in_code
            out.append(ch)
            i += 1
            continue
        if not in_code and ch in "（(":
            close_ch = "）" if ch == "（" else ")"
            depth = 1
            j = i + 1
            sub_code = False
            while j < n and depth > 0:
                c2 = text[j]
                if c2 == "`":
                    sub_code = not sub_code
                elif not sub_code:
                    if c2 == ch:
                        depth += 1
                    elif c2 == close_ch:
                        depth -= 1
                j += 1
            if depth == 0 and (j - i - 2) >= min_inner_len:
                if out and out[-1] == " " and j < n and text[j] in " ,.;:，。；：":
                    out.pop()
                i = j
                continue
        out.append(ch)
        i += 1
    return re.sub(r"\s+", " ", "".join(out)).strip()


def trim_outside_code(text: str, max_len: int, is_zh: bool) -> str:
    s = strip_long_parentheticals(re.sub(r"\s+", " ", text).strip(), 18 if is_zh else 26)
    if not s:
        return ""

    # Prefer stopping at the first natural sentence/clause boundary outside backticks/parens.
    in_code = False
    paren_depth = 0
    first_clause_end = -1
    sentence_end = -1
    for i, ch in enumerate(s):
        if ch == "`":
            in_code = not in_code
            continue
        if in_code:
            continue
        if ch in "（(":
            paren_depth += 1
            continue
        if ch in "）)" and paren_depth > 0:
            paren_depth -= 1
            continue
        if paren_depth > 0:
            continue

        if is_zh:
            if ch == "。" and i >= 8:
                sentence_end = i + 1
                break
            if ch == "；" and i >= 12 and first_clause_end < 0:
                first_clause_end = i
        else:
            if ch == ";" and i >= 20 and first_clause_end < 0:
                first_clause_end = i
            if ch == "." and i >= 20 and (i + 1 == len(s) or s[i + 1] == " "):
                sentence_end = i + 1
                break

    if sentence_end > 0 and sentence_end <= max_len:
        return s[:sentence_end].strip()
    if 0 < first_clause_end <= max_len:
        suffix = "。" if is_zh else "."
        return s[:first_clause_end].rstrip("，,;；.。") + suffix
    if len(s) <= max_len:
        return s

    # Safe hard-truncate outside inline code spans and unclosed parentheticals.
    cut = max_len
    if s[:cut].count("`") % 2 == 1:
        next_tick = s.find("`", cut)
        prev_tick = s.rfind("`", 0, cut)
        if 0 <= next_tick <= max_len + 24:
            cut = next_tick + 1
        elif prev_tick > 0:
            cut = prev_tick

    in_code = False
    open_stack: list[int] = []
    for idx, ch in enumerate(s[:cut]):
        if ch == "`":
            in_code = not in_code
            continue
        if in_code:
            continue
        if ch in "（(":
            open_stack.append(idx)
        elif ch in "）)" and open_stack:
            open_stack.pop()
    if open_stack and open_stack[0] >= 10:
        cut = open_stack[0]
        return s[:cut].rstrip(" ，,;；:：-—") + ("。" if is_zh else ".")

    if not is_zh:
        sp = s.rfind(" ", max_len // 2, cut)
        if sp > 0 and s[:sp].count("`") % 2 == 0:
            cut = sp
    return s[:cut].rstrip(" ，,;；:：-—(（") + "…"


def summarize_changelog_block(block_lines: list[str], is_zh: bool, max_items: int = 14) -> list[str]:
    entries: list[tuple[str, str, list[str]]] = []
    cur_title = ""
    cur_inline = ""
    cur_subs: list[str] = []

    def flush() -> None:
        nonlocal cur_title, cur_inline, cur_subs
        if cur_title or cur_inline:
            entries.append((cur_title, cur_inline, cur_subs))
        cur_title = ""
        cur_inline = ""
        cur_subs = []

    top_re = re.compile(r"^-\s+(.*)$")
    sub_re = re.compile(r"^\s{2,}-\s+(.*)$")
    bold_lead_re = re.compile(r"^\*\*(.+?)\*\*\s*(?:[：:]\s*(.*))?$")

    for raw in block_lines:
        if raw.strip().startswith("###"):
            continue
        m_top = top_re.match(raw)
        if m_top:
            flush()
            body = m_top.group(1).strip()
            m_bold = bold_lead_re.match(body)
            if m_bold:
                cur_title = m_bold.group(1).strip()
                cur_inline = (m_bold.group(2) or "").strip()
            else:
                cur_inline = body
            continue

        m_sub = sub_re.match(raw)
        if m_sub and (cur_title or cur_inline):
            sub_body = m_sub.group(1).strip()
            m_sub_bold = bold_lead_re.match(sub_body)
            if m_sub_bold and m_sub_bold.group(2):
                cur_subs.append(f"{m_sub_bold.group(1).strip()}: {m_sub_bold.group(2).strip()}" if not is_zh else f"{m_sub_bold.group(1).strip()}（{m_sub_bold.group(2).strip()}）")
            elif m_sub_bold:
                cur_subs.append(m_sub_bold.group(1).strip())
            else:
                cur_subs.append(sub_body)

    flush()

    max_desc = 96 if is_zh else 155
    sep = "：" if is_zh else ": "
    bullets: list[str] = []
    for title, inline_desc, subs in entries[:max_items]:
        raw_desc = inline_desc if inline_desc else (subs[0] if subs else "")
        short_desc = trim_outside_code(raw_desc, max_desc, is_zh=is_zh)
        if title and short_desc:
            bullets.append(f"- **{title}**{sep}{short_desc}")
        elif title:
            bullets.append(f"- **{title}**")
        elif short_desc:
            bullets.append(f"- {short_desc}")
    return bullets


def fallback_git_commits(prev_tag: str, max_items: int = 12) -> list[str]:
    rev_spec = f"{prev_tag}..HEAD" if prev_tag else "-n 12"
    cmd = ["log", "--no-merges", "--pretty=format:%s (%h)"]
    if prev_tag:
        cmd.append(rev_spec)
    else:
        cmd.extend(["-n", str(max_items)])
    raw = run_git(cmd)
    bullets: list[str] = []
    seen: set[str] = set()
    for line in raw.splitlines():
        s = line.strip()
        if not s:
            continue
        low = s.lower()
        if any(pat in low for pat in BOT_PATTERNS):
            continue
        if s in seen:
            continue
        seen.add(s)
        bullets.append(f"- {s}")
        if len(bullets) >= max_items:
            break
    if not bullets:
        head_line = run_git(["show", "-s", "--format=%s (%h)", "HEAD"])
        if head_line:
            bullets.append(f"- {head_line}")
    return bullets


def main() -> int:
    parser = argparse.ArgumentParser(description="Generate concise Volo-go GitHub release notes.")
    parser.add_argument("--version", required=True, help="Release tag, e.g. v1.0.1")
    parser.add_argument("--repo", default="Willh92/Volo-go", help="GitHub owner/repo")
    parser.add_argument("--root", default=".", help="Repository root path")
    parser.add_argument("--output", required=True, help="Output markdown file path")
    args = parser.parse_args()

    tag = args.version.strip()
    if not tag.startswith("v"):
        tag = f"v{tag}"
    ver_no_v = tag[1:]
    repo = args.repo.strip() or "Willh92/Volo-go"
    root = pathlib.Path(args.root)

    zh_file = root / "CHANGELOG.zh-CN.md"
    en_file = root / "CHANGELOG.md"

    zh_bullets: list[str] = []
    en_bullets: list[str] = []

    if zh_file.is_file():
        zh_block = extract_version_block(zh_file.read_text(encoding="utf-8"), ver_no_v)
        zh_bullets = summarize_changelog_block(zh_block, is_zh=True)

    if en_file.is_file():
        en_block = extract_version_block(en_file.read_text(encoding="utf-8"), ver_no_v)
        en_bullets = summarize_changelog_block(en_block, is_zh=False)

    prev_tag = find_previous_tag(tag)
    sections: list[str] = []

    if zh_bullets:
        sections.append("## 更新摘要\n\n" + "\n".join(zh_bullets))
    if en_bullets:
        sections.append("## Release Highlights\n\n" + "\n".join(en_bullets))

    if not zh_bullets and not en_bullets:
        commit_bullets = fallback_git_commits(prev_tag)
        if commit_bullets:
            sections.append("## 更新内容 / What's Changed\n\n" + "\n".join(commit_bullets))
        else:
            sections.append(f"## Volo-go {tag}\n\n- Maintenance & stability release `{tag}`.")

    links = [
        f"[CHANGELOG.zh-CN.md](https://github.com/{repo}/blob/{tag}/CHANGELOG.zh-CN.md)",
        f"[CHANGELOG.md](https://github.com/{repo}/blob/{tag}/CHANGELOG.md)",
    ]
    if prev_tag:
        links.append(f"[Compare `{prev_tag}...{tag}`](https://github.com/{repo}/compare/{prev_tag}...{tag})")

    ghcr_image = f"ghcr.io/{repo.lower()}:{ver_no_v}"
    footer = (
        "---\n\n"
        f"- **完整更新日志 / Full Changelog**: {' · '.join(links)}\n"
        f"- **容器镜像 / Container**: `docker pull {ghcr_image}`\n"
        "- **校验值 / Checksums**: 见附件 `SHA256SUMS.txt`"
    )
    sections.append(footer)

    out_path = pathlib.Path(args.output)
    out_path.write_text("\n\n".join(sections) + "\n", encoding="utf-8")
    return 0


if __name__ == "__main__":
    sys.exit(main())
