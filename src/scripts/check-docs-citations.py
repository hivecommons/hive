#!/usr/bin/env python3
"""check-docs-citations.py — verify repo file:line citations in src/docs/.

The docs contain many inline source citations such as `src/pkg/foo.go:123`,
`pkg/foo.go:123-130`, or `.github/workflows/docker.yml:85,280`. This checker
keeps those citations from drifting silently: every repo-local citation it can
resolve must point at an existing file and every referenced line/range must be
inside that file. It deliberately ignores URLs, fenced code blocks, and
unresolvable basename-only references that commonly point at external modules or
example issue titles.

Usage: src/scripts/check-docs-citations.py [--fix] [docs-dir]
       (default docs-dir: src/docs)

--fix delegates to the older api-reference citation fixer, which has enough
route/path context to rewrite moved route registrations. Generic docs citations
verify existence/range, plus a lightweight content anchor when the citing line
names a backticked symbol near the citation.
"""
from __future__ import annotations

import argparse
import re
import subprocess
import sys
from dataclasses import dataclass
from pathlib import Path

SOURCE_EXTS = (
    "go", "sh", "py", "md", "yaml", "yml", "json", "toml", "mod", "sum",
    "ts", "tsx", "js", "jsx", "css", "html", "service", "container",
    "timer", "conf", "env", "txt",
)
REPO_LOCAL_PREFIXES = (
    ".github/", "bin/", "changelog.d/", "cmd/", "deploy/", "docs/", "pkg/",
    "scripts/", "src/", "test/",
)
CITATION_RE = re.compile(
    r"(?<![\w./@:-])"
    r"((?:\.github/|src/|docs/|bin/|changelog\.d/|[A-Za-z0-9_./-]+/)"
    r"[A-Za-z0-9_.-]+\.(?:" + "|".join(re.escape(ext) for ext in SOURCE_EXTS) + r"))"
    r":(\d+(?:[-–]\d+)?(?:,\d+(?:[-–]\d+)?)*)"
)
FENCE_RE = re.compile(r"^\s*(```|~~~)")
BACKTICK_RE = re.compile(r"`([^`\n]+)`")
ANCHOR_SLACK_LINES = 2
ANCHOR_CUE_RE = re.compile(r"\b(descending|looping\s+until|where|contains?|containing|shows?)\b", re.IGNORECASE)


@dataclass(frozen=True)
class Citation:
    doc: Path
    line_no: int
    cited_path: str
    line_spec: str
    line: str
    match_end: int


def parse_line_spec(spec: str) -> list[tuple[int, int]]:
    ranges: list[tuple[int, int]] = []
    for part in spec.replace("–", "-").split(","):
        if "-" in part:
            start_s, end_s = part.split("-", 1)
            start, end = int(start_s), int(end_s)
        else:
            start = end = int(part)
        ranges.append((start, end))
    return ranges


def is_probably_repo_local(path: str) -> bool:
    return path.startswith(REPO_LOCAL_PREFIXES) or "/" in path


def source_lines(path: Path) -> int:
    with path.open("r", encoding="utf-8", errors="ignore") as fh:
        return sum(1 for _ in fh)


def source_line_window(path: Path, ranges: list[tuple[int, int]], slack: int = ANCHOR_SLACK_LINES) -> str:
    if not ranges:
        return ""

    wanted: set[int] = set()
    with path.open("r", encoding="utf-8", errors="ignore") as fh:
        lines = fh.readlines()
    total = len(lines)
    for start, end in ranges:
        for line_no in range(max(1, start - slack), min(total, end + slack) + 1):
            wanted.add(line_no)
    return "\n".join(lines[line_no - 1] for line_no in sorted(wanted) if 1 <= line_no <= total)


def is_symbol_anchor(text: str) -> bool:
    token = text.strip()
    if not token:
        return False
    if "/" in token or "\\" in token:
        return False
    if CITATION_RE.search(token):
        return False
    if re.fullmatch(r":?\d+(?:[-–]\d+)?(?:,\d+(?:[-–]\d+)?)*", token):
        return False
    if re.search(r"\s", token) and ":" not in token and "(" not in token:
        return False
    return bool(re.search(r"[A-Za-z_][A-Za-z0-9_]*|[A-Za-z_][A-Za-z0-9_]*\s*:", token))


def citation_anchors(line: str, match_end: int) -> list[str]:
    # Support explicit explanatory anchors that follow the citation, e.g.
    # ``path/to/file.go:10-20`, where `SymbolName` appears``. Requiring a cue
    # word keeps older prose with many unrelated backticks in range-only mode.
    after = line[match_end:]
    if after.startswith("`"):
        after = after[1:]
    after = after[:240]
    if not ANCHOR_CUE_RE.search(after):
        return []

    anchors: list[str] = []
    for match in BACKTICK_RE.finditer(after):
        token = match.group(1).strip()
        if is_symbol_anchor(token) and token not in anchors:
            anchors.append(token)
    return anchors


class Resolver:
    def __init__(self, repo_root: Path, src_root: Path) -> None:
        self.repo_root = repo_root
        self.src_root = src_root
        self.files = [p for p in repo_root.rglob("*") if p.is_file() and ".git" not in p.parts]

    def resolve(self, cited: str, doc: Path) -> tuple[Path | None, str | None]:
        raw = Path(cited)
        for candidate in (self.repo_root / raw, self.src_root / raw, doc.parent / raw):
            if candidate.is_file():
                return candidate.resolve(), None

        if "/" in cited:
            matches = [p for p in self.files if p.as_posix().endswith(cited)]
            if len(matches) == 1:
                return matches[0].resolve(), None
            if len(matches) > 1:
                return None, "ambiguous path suffix: " + ", ".join(m.as_posix() for m in matches[:5])
            return None, "file does not exist"

        # Basename-only references are often external package source references
        # or example issue-title text; skip them unless a future doc makes the
        # path explicit enough to resolve safely.
        return None, "unresolved basename-only reference (skipped)"


def iter_citations(docs_dir: Path) -> list[Citation]:
    citations: list[Citation] = []
    for doc in sorted(docs_dir.rglob("*.md")):
        in_fence = False
        with doc.open("r", encoding="utf-8", errors="ignore") as fh:
            for line_no, line in enumerate(fh, 1):
                if FENCE_RE.match(line):
                    in_fence = not in_fence
                    continue
                if in_fence:
                    continue
                for match in CITATION_RE.finditer(line):
                    cited_path = match.group(1)
                    if is_probably_repo_local(cited_path):
                        citations.append(Citation(doc, line_no, cited_path, match.group(2), line, match.end()))
    return citations


def run_api_reference_fix(script_dir: Path, docs_dir: Path) -> int:
    api_doc = docs_dir / "api-reference.md"
    checker = script_dir / "check-api-reference-citations.sh"
    if not api_doc.is_file() or not checker.is_file():
        return 0
    return subprocess.call(["bash", str(checker), "--fix", str(api_doc)])


def main(argv: list[str]) -> int:
    parser = argparse.ArgumentParser(description="Verify repo-local file:line citations in src/docs Markdown.")
    parser.add_argument("docs_dir", nargs="?", default=None)
    parser.add_argument("--fix", action="store_true", help="run safe fixers where available (currently api-reference.md)")
    args = parser.parse_args(argv)

    script_dir = Path(__file__).resolve().parent
    src_root = script_dir.parent
    repo_root = src_root.parent
    docs_dir = Path(args.docs_dir) if args.docs_dir else src_root / "docs"
    if not docs_dir.is_absolute():
        docs_dir = (Path.cwd() / docs_dir).resolve()

    if not docs_dir.is_dir():
        print(f"cannot find docs directory at {docs_dir}", file=sys.stderr)
        return 2

    fix_rc = run_api_reference_fix(script_dir, docs_dir) if args.fix else 0

    resolver = Resolver(repo_root, src_root)
    checked = skipped = failed = 0
    for citation in iter_citations(docs_dir):
        target, error = resolver.resolve(citation.cited_path, citation.doc)
        display_doc = citation.doc.relative_to(repo_root) if citation.doc.is_relative_to(repo_root) else citation.doc
        if target is None:
            if error and error.startswith("unresolved basename-only"):
                skipped += 1
                continue
            print(f"DRIFT {display_doc}:{citation.line_no} {citation.cited_path}:{citation.line_spec} ({error})", file=sys.stderr)
            failed += 1
            continue

        ranges = parse_line_spec(citation.line_spec)
        total_lines = source_lines(target)
        bad_ranges = []
        for start, end in ranges:
            if start < 1 or end < start or end > total_lines:
                bad_ranges.append((start, end))
        if bad_ranges:
            rel_target = target.relative_to(repo_root) if target.is_relative_to(repo_root) else target
            ranges = ", ".join(f"{s}" if s == e else f"{s}-{e}" for s, e in bad_ranges)
            print(
                f"DRIFT {display_doc}:{citation.line_no} {citation.cited_path}:{citation.line_spec} "
                f"-> {rel_target} has {total_lines} lines; out of range: {ranges}",
                file=sys.stderr,
            )
            failed += 1
            continue

        anchors = citation_anchors(citation.line, citation.match_end)
        if anchors:
            window = source_line_window(target, ranges)
            if not any(anchor in window for anchor in anchors):
                rel_target = target.relative_to(repo_root) if target.is_relative_to(repo_root) else target
                print(
                    f"DRIFT {display_doc}:{citation.line_no} {citation.cited_path}:{citation.line_spec} "
                    f"-> {rel_target} lacks anchor(s) near cited range: {', '.join(anchors)}",
                    file=sys.stderr,
                )
                failed += 1
                continue
            checked += 1
        else:
            checked += 1

    if failed == 0 and fix_rc == 0:
        msg = f"docs citations: {checked} repo-local file:line reference(s) verified"
        if skipped:
            msg += f" ({skipped} basename-only reference(s) skipped)"
        print(msg + ".")
        return 0

    print(f"docs citations: {failed} drifted citation(s); {checked} verified; {skipped} skipped.", file=sys.stderr)
    return 1


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
