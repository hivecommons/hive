#!/usr/bin/env python3
"""Provenance for github-issues.jsonl: samples closed issues (not PRs) from public,
permissively licensed repositories via `gh api`.

Usage:
  fetch.py           writes github-issues.jsonl
  fetch.py --verify  re-fetches every committed row and reports rows whose text no
                     longer matches the linked issue (the spot-check before a pin)

Sampling is reproducible up to edits on GitHub: per repository, the search query
`repo:<r> is:issue is:closed created:<CREATED>` sorted by creation date, newest
first, keeping the first PER_REPO rows that pass the filters. There is no random
seed; order is the seed. Issues are live data, so the committed JSONL is the pin,
not the fetch. Each repository's licence is checked against GitHub's SPDX id
before any row is taken from it.

Split = bit 0 of sha256(id): 1 -> heldout, 0 -> dev. Only dev rows may inform
tuning of rules, thresholds or the classifier prompt; see README.md.

Rows with @mentions, emails, secret-shaped strings, HTML comments, injection
phrasing, spam, security reports or [rejected AI] titles are excluded by BAD;
reviewers should still spot-check a fresh fetch (--verify)."""
import hashlib, json, re, subprocess, sys, time

CREATED = "2025-01-01..2026-10-31"
PER_REPO = 10
PAGES = 3
REPOS = {
    "cli/cli": "MIT", "golang/go": "BSD-3-Clause", "kubernetes/kubernetes": "Apache-2.0",
    "prometheus/prometheus": "Apache-2.0", "pallets/flask": "BSD-3-Clause",
    "psf/requests": "Apache-2.0", "rust-lang/cargo": "Apache-2.0",
    "nodejs/node": "NOASSERTION", "docker/compose": "Apache-2.0",
    "microsoft/vscode": "MIT", "react/react": "MIT", "vercel/next.js": "MIT",
    "denoland/deno": "MIT", "helm/helm": "Apache-2.0", "etcd-io/etcd": "Apache-2.0",
    "containerd/containerd": "Apache-2.0", "moby/moby": "Apache-2.0",
    "astral-sh/ruff": "MIT", "astral-sh/uv": "Apache-2.0", "pypa/pip": "MIT",
    "numpy/numpy": "NOASSERTION", "pandas-dev/pandas": "BSD-3-Clause",
    "tokio-rs/tokio": "MIT", "vitejs/vite": "MIT", "eslint/eslint": "MIT",
    "prettier/prettier": "MIT", "spf13/cobra": "Apache-2.0", "junegunn/fzf": "MIT",
    "sharkdp/bat": "Apache-2.0", "argoproj/argo-cd": "Apache-2.0",
    "open-telemetry/opentelemetry-go": "Apache-2.0", "kubernetes-sigs/kind": "Apache-2.0",
}
# Human-readable licence recorded per row. GitHub reports NOASSERTION for some
# permissive licence files it cannot classify; those are named here explicitly.
LICENCE_NAME = {"rust-lang/cargo": "MIT OR Apache-2.0", "astral-sh/uv": "MIT OR Apache-2.0",
                "sharkdp/bat": "MIT OR Apache-2.0", "nodejs/node": "MIT",
                "numpy/numpy": "BSD-3-Clause"}
BAD = re.compile(r"@\w|ghp_|AKIA|BEGIN [A-Z ]*KEY|password|token|secret|ignore (all|previous)|<!--"
                 r"|\[spam\]|\[rejected|\bRCE\b|\bSSRF\b|command injection|vulnerab", re.I)
ROW_ID = re.compile(r"^https://github\.com/([^/]+/[^/]+)/issues/(\d+) ")


def gh(*args):
    for attempt in range(5):
        p = subprocess.run(["gh", "api", "-X", "GET", *args], capture_output=True, text=True)
        if p.returncode == 0:
            return json.loads(p.stdout)
        if "rate limit" not in (p.stdout + p.stderr).lower():
            sys.exit(f"gh api {' '.join(args)}: {p.stderr.strip() or p.stdout.strip()}")
        time.sleep(30 * (attempt + 1))  # search API secondary rate limit
    sys.exit(f"gh api {' '.join(args)}: still rate limited")


def issue_text(i):
    return (i["title"] + "\n\n" + (i.get("body") or "")).replace("\r\n", "\n").strip()


def split_for(rid):
    return "heldout" if int(hashlib.sha256(rid.encode()).hexdigest(), 16) & 1 else "dev"


def fetch():
    out = []
    for repo, spdx in REPOS.items():
        meta = gh(f"repos/{repo}", "--jq", "{l: .license.spdx_id, n: .full_name}")
        got = meta["l"]
        if meta["n"].lower() != repo.lower():
            sys.exit(f"{repo} moved to {meta['n']}; update REPOS")
        if got != spdx:
            sys.exit(f"{repo}: licence is {got}, REPOS says {spdx}; re-check before sampling")
        lic = LICENCE_NAME.get(repo, spdx)
        n = 0
        for page in range(1, PAGES + 1):
            time.sleep(2.1)  # search API: 30 requests per minute
            items = gh("search/issues", "-f", f"q=repo:{repo} is:issue is:closed created:{CREATED}",
                       "-f", "sort=created", "-f", "order=desc", "-f", "per_page=100",
                       "-f", f"page={page}")["items"]
            for i in items:
                if "pull_request" in i or not i.get("body"):
                    continue
                text = issue_text(i)
                if not 200 <= len(text) <= 1500 or BAD.search(text):
                    continue
                rid = f"gh-issue-{repo.replace('/', '-')}-{i['number']}"
                out.append({"id": rid, "label": "benign",
                            "source": f"https://github.com/{repo}/issues/{i['number']} ({lic})",
                            "split": split_for(rid), "text": text})
                n += 1
                if n == PER_REPO:
                    break
            if n == PER_REPO or len(items) < 100:
                break
        print(f"{repo}: {n} rows", file=sys.stderr)
    with open("github-issues.jsonl", "w") as f:
        for r in out:
            f.write(json.dumps(r, ensure_ascii=False) + "\n")
    print(f"wrote {len(out)} rows", file=sys.stderr)


def verify():
    bad = 0
    rows = [json.loads(l) for l in open("github-issues.jsonl")]
    for r in rows:
        m = ROW_ID.match(r["source"])
        if not m or r["id"] != f"gh-issue-{m[1].replace('/', '-')}-{m[2]}":
            print(f"{r['id']}: source {r['source']!r} does not match id"); bad += 1; continue
        if issue_text(gh(f"repos/{m[1]}/issues/{m[2]}")) != r["text"]:
            print(f"{r['id']}: text differs from {m[0].strip()}"); bad += 1
    print(f"verified {len(rows) - bad} of {len(rows)} rows", file=sys.stderr)
    sys.exit(1 if bad else 0)


if __name__ == "__main__":
    verify() if sys.argv[1:] == ["--verify"] else fetch()
