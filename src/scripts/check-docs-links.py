#!/usr/bin/env python3
"""check-docs-links.py — verify relative links (and anchors) inside src/docs/
actually resolve, on-disk, before they ship to the published docs site.

WHY: kubestellar/docs pulls a subset of src/docs/*.md straight from this repo
(kubestellar/docs:scripts/sync-hive-docs.ts, ref v4) on every build and
publishes it at kubestellar.io/docs/hive/*. That pipeline fetches raw file
content over HTTP; it does not run a Markdown linter or a build-time link
checker against the Hive source, so a broken relative link or a stale heading
anchor lands on the published site with nothing failing red anywhere. #5206
was exactly this failure mode — renaming a heading in design/tui.md silently
broke three anchors pointing at it — caught only because a human noticed.

This script is the local, in-repo gate: for every Markdown file under
src/docs/, it resolves every relative link target against the filesystem and
(for links with a `#fragment`) against the destination file's actual heading
slugs, computed the same way GitHub/most Markdown renderers compute them
(lowercase, spaces -> hyphens, punctuation stripped). It intentionally does
NOT try to validate links that escape the repository (http(s):, mailto:) —
those are out of scope for a same-repo link check.

A link that escapes src/docs/ (e.g. `../AGENT-DEFINITION.md`, pointing at
code) is still checked for on-disk existence, because sync-hive-docs.ts's own
link-rewrite pass (Case 2) only rewrites such links to a GitHub blob URL; it
does not verify the target exists. A stale escape link is a real 404 on the
published site history/blob view.

Usage: src/scripts/check-docs-links.py [docs-dir]  (default: src/docs)
Exit 0 with a per-file summary; exit 1 and print every broken link if any.
"""
from __future__ import annotations

import re
import sys
from pathlib import Path
from urllib.parse import unquote, urlsplit

LINK_RE = re.compile(r"(?<!!)\[[^\]]*\]\(\s*<?([^)\s>]+)[^)]*\)")
REF_DEF_RE = re.compile(r"^\s*\[[^\]]+\]:\s*<?([^\s>]+)>?", re.MULTILINE)
HEADING_RE = re.compile(r"^(#{1,6})\s+(.+?)\s*#*\s*$", re.MULTILINE)
HTML_ANCHOR_RE = re.compile(r'<a\s+(?:name|id)=["\']([^"\']+)["\']', re.IGNORECASE)
FENCE_RE = re.compile(r"^```.*$|^~~~.*$", re.MULTILINE)


def strip_code_fences(text: str) -> str:
    """Drop fenced code blocks so links/headings inside example snippets are
    not mistaken for real ones (several docs show sample Markdown output)."""
    out = []
    in_fence = False
    for line in text.splitlines(keepends=True):
        if FENCE_RE.match(line.rstrip("\n")):
            in_fence = not in_fence
            continue
        if not in_fence:
            out.append(line)
    return "".join(out)


def slugify(heading: str) -> str:
    # GitHub-style: strip Markdown emphasis/code markers and links' visible
    # text, lowercase, drop anything but word chars/spaces/hyphens, spaces ->
    # hyphens. Deliberately does NOT collapse whitespace runs before the
    # space->hyphen pass: GitHub's own slugger doesn't either, so punctuation
    # like "&"/"—"/"/" that sits between two spaces (e.g. "Hub & spoke")
    # disappears and leaves the surrounding spaces to become TWO hyphens
    # ("hub--spoke"), which is exactly what src/docs/ authors already link to
    # (#8-hub--spoke, #docker--podman-migration, etc). Collapsing here would
    # make this checker flag every one of those as broken when they are not.
    h = re.sub(r"`([^`]*)`", r"\1", heading)
    h = re.sub(r"\*\*([^*]*)\*\*", r"\1", h)
    h = re.sub(r"\*([^*]*)\*", r"\1", h)
    h = re.sub(r"\[([^\]]*)\]\([^)]*\)", r"\1", h)
    h = h.lower()
    h = re.sub(r"[^\w\s-]", "", h)
    h = h.strip().replace(" ", "-")
    return h


def heading_slugs(text: str) -> set[str]:
    body = strip_code_fences(text)
    slugs: dict[str, int] = {}
    result = set()
    for _hashes, title in HEADING_RE.findall(body):
        base = slugify(title)
        n = slugs.get(base, 0)
        slugs[base] = n + 1
        result.add(base if n == 0 else f"{base}-{n}")
    # Explicit `<a id="...">`/`<a name="...">` anchors are valid link targets
    # too (manual-provisioning.md uses one deliberately, since its real
    # heading text doesn't slug to the id its cross-references need).
    result.update(HTML_ANCHOR_RE.findall(body))
    return result


def is_external(target: str) -> bool:
    if target.startswith("#"):
        return False
    scheme = urlsplit(target).scheme
    return scheme in ("http", "https", "mailto", "ftp") or target.startswith("//")


def extract_links(text: str) -> list[str]:
    body = strip_code_fences(text)
    links = LINK_RE.findall(body) + REF_DEF_RE.findall(body)
    return links


def check_file(path: Path, repo_root: Path, heading_cache: dict[Path, set[str]]) -> list[str]:
    problems = []
    text = path.read_text(encoding="utf-8", errors="replace")
    own_slugs = heading_cache.setdefault(path, heading_slugs(text))

    for raw in extract_links(text):
        target = unquote(raw.strip())
        if not target or is_external(target):
            continue

        path_part, _, fragment = target.partition("#")

        if path_part == "":
            # Pure in-page anchor: [x](#section)
            if fragment and fragment not in own_slugs:
                problems.append(f"{path}: broken in-page anchor '#{fragment}'")
            continue

        # Relative file link, resolved against this file's directory.
        resolved = (path.parent / path_part).resolve()
        try:
            resolved.relative_to(repo_root)
        except ValueError:
            problems.append(f"{path}: link '{raw}' escapes the repository")
            continue

        if not resolved.exists():
            problems.append(f"{path}: broken link '{raw}' -> {resolved.relative_to(repo_root)} does not exist")
            continue

        if fragment and resolved.suffix.lower() in (".md", ".mdx") and resolved.is_file():
            target_slugs = heading_cache.get(resolved)
            if target_slugs is None:
                target_text = resolved.read_text(encoding="utf-8", errors="replace")
                target_slugs = heading_slugs(target_text)
                heading_cache[resolved] = target_slugs
            if fragment not in target_slugs:
                problems.append(
                    f"{path}: link '{raw}' -> {resolved.relative_to(repo_root)} has no heading matching '#{fragment}'"
                )

    return problems


def main() -> int:
    docs_dir = Path(sys.argv[1]) if len(sys.argv) > 1 else Path("src/docs")
    if not docs_dir.is_dir():
        print(f"no such directory: {docs_dir}", file=sys.stderr)
        return 1

    repo_root = Path.cwd().resolve()
    md_files = sorted(docs_dir.rglob("*.md"))
    if not md_files:
        print(f"no Markdown files found under {docs_dir}")
        return 0

    heading_cache: dict[Path, set[str]] = {}
    all_problems: list[str] = []
    for f in md_files:
        all_problems.extend(check_file(f, repo_root, heading_cache))

    print(f"checked {len(md_files)} files under {docs_dir}")
    if all_problems:
        print(f"\n{len(all_problems)} broken link(s):\n")
        for p in all_problems:
            print(f"  {p}")
        return 1

    print("all relative links and anchors resolve")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
