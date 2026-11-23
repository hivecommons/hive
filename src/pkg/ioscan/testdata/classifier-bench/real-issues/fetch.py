#!/usr/bin/env python3
"""Provenance for github-issues.jsonl: samples closed issues (not PRs) from public,
permissively licensed repositories via `gh api`. Live data, so the committed JSONL
is the pin. Usage: fetch.py  (writes sample.json; ids get a split by sha256 bit 0)."""
import json, re, subprocess
REPOS = {"cli/cli": "MIT", "golang/go": "BSD-3-Clause", "kubernetes/kubernetes": "Apache-2.0",
         "prometheus/prometheus": "Apache-2.0", "pallets/flask": "BSD-3-Clause",
         "psf/requests": "Apache-2.0", "rust-lang/cargo": "MIT OR Apache-2.0",
         "nodejs/node": "MIT", "docker/compose": "Apache-2.0"}
BAD = re.compile(r"@\w|ghp_|AKIA|BEGIN [A-Z ]*KEY|password|token|secret|ignore (all|previous)|<!--", re.I)
out = []
for repo, lic in REPOS.items():
    raw = subprocess.run(["gh", "api", "-X", "GET", f"repos/{repo}/issues", "-f", "state=closed",
                          "-f", "per_page=100", "-f", "sort=created", "-f", "direction=desc"],
                         capture_output=True, text=True, check=True).stdout
    n = 0
    for i in json.loads(raw):
        if "pull_request" in i or not i.get("body"):
            continue
        text = (i["title"] + "\n\n" + i["body"]).replace("\r\n", "\n").strip()
        if not 200 <= len(text) <= 1500 or BAD.search(text):
            continue
        out.append({"id": f"gh-issue-{repo.replace('/', '-')}-{i['number']}", "label": "benign",
                    "source": f"https://github.com/{repo}/issues/{i['number']} ({lic})", "text": text})
        n += 1
        if n == 14:
            break
json.dump(out, open("sample.json", "w"), ensure_ascii=False)
