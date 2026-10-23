#!/usr/bin/env bash
# check-security-docs-readability.sh — fail when a security document is harder
# to read than a 20-year-old non-expert can follow.
#
# Every file in the fixed list below must score a Flesch-Kincaid grade of 11
# or lower. Grade is 0.39 * (words / sentences) + 11.8 * (syllables / words)
# - 15.59, computed over prose only: fenced code, tables, headings, URLs,
# inline code and link targets are dropped first so that a long Markdown
# table or a URL does not inflate the score. The syllable counter is the
# usual vowel-group heuristic, good to about +/-0.5 grade on English prose,
# which is why the threshold is 11 and the writing target is 9-10.
#
# Rules the authors follow to stay under the line: sentences average 20 words
# or fewer, one idea per sentence, every term or acronym gets a short gloss on
# first use, "stops / blocks / checks" rather than "mitigates / enforces /
# validates", and tables for anything list-like.
#
# Pure Python standard library; no third-party packages. Run from the repo
# root. Pass `--verbose` to print the score for every file, not only failures.

set -euo pipefail

cd "$(dirname "$0")/../.."

MAX_GRADE="${MAX_GRADE:-11}"

FILES=(
  SECURITY.md
  src/docs/security-self-assessment.md
  src/docs/security-response.md
  src/docs/security-threat-model.md
  src/docs/security-model.md
  src/docs/securing-your-hive.md
  src/docs/ioscan.md
  src/docs/ioscan-red-team.md
)

python3 - "$MAX_GRADE" "$@" "${FILES[@]}" <<'PY'
import re
import sys

max_grade = float(sys.argv[1])
args = sys.argv[2:]
verbose = "--verbose" in args
files = [a for a in args if a != "--verbose"]


def prose(text):
    text = re.sub(r"```.*?```", "", text, flags=re.S)
    text = re.sub(r"^\|.*$", "", text, flags=re.M)
    text = re.sub(r"^#+ .*$", "", text, flags=re.M)
    text = re.sub(r"\[([^\]]*)\]\([^)]*\)", r"\1", text)
    text = re.sub(r"<https?://[^>]+>|https?://\S+", "", text)
    text = re.sub(r"`[^`]*`", "code", text)
    text = re.sub(r"[*_>#]", "", text)
    return text


def syllables(word):
    word = re.sub(r"[^a-z]", "", word.lower())
    if not word:
        return 0
    if len(word) <= 3:
        return 1
    word = re.sub(r"(?:[^laeiouy]es|ed|[^laeiouy]e)$", "", word)
    word = re.sub(r"^y", "", word)
    return max(1, len(re.findall(r"[aeiouy]{1,2}", word)))


def grade(text):
    body = prose(text)
    sentences = [s for s in re.split(r"(?<=[.!?])\s+|\n\s*\n", body) if len(s.split()) >= 3]
    words = [w for s in sentences for w in s.split() if re.search(r"[A-Za-z]", w)]
    if not sentences or not words:
        return None, 0.0, 0
    syl = sum(syllables(w) for w in words)
    wps = len(words) / len(sentences)
    return 0.39 * wps + 11.8 * syl / len(words) - 15.59, wps, len(words)


failed = 0
for path in files:
    try:
        text = open(path, encoding="utf-8").read()
    except OSError as exc:
        print(f"FAIL {path}: {exc}")
        failed += 1
        continue
    g, wps, n = grade(text)
    if g is None:
        print(f"FAIL {path}: no prose found")
        failed += 1
        continue
    status = "ok  " if g <= max_grade else "FAIL"
    if status == "FAIL":
        failed += 1
    if verbose or status == "FAIL":
        print(f"{status} {path}: grade {g:.1f} (max {max_grade:g}), {wps:.1f} words/sentence, {n} prose words")

if failed:
    print(f"\n{failed} security doc(s) score above grade {max_grade:g}. Shorten sentences, swap long words for short ones, and gloss any term on first use.")
    sys.exit(1)
print(f"all {len(files)} security docs read at grade {max_grade:g} or below")
PY
