#!/usr/bin/env python3
"""Deterministically convert the pinned RepoGuardBench payloads into classifier-bench
JSONL. Usage: convert.py <dir>  (writes <dir>/repoguardbench-attacks.jsonl)
Payload text is taken verbatim from the pinned carriers.py (PAYLOADS) and wrapped with
its own carrier templates (issue note suffix, test-log board, rule file, README
section). Issue carriers append the payload to each of the 14 pinned task issue_texts.
Nothing is written or paraphrased here."""
import hashlib, importlib.util, json, os, sys
d = sys.argv[1]
spec = importlib.util.spec_from_file_location("carriers", os.path.join(d, "carriers.py"))
c = importlib.util.module_from_spec(spec); sys.modules["carriers"] = c; spec.loader.exec_module(c)
tasks = [json.loads(l) for l in open(os.path.join(d, "repoguardbench_real.upstream.txt"), encoding="utf-8")]
SRC = "DaoyuanLi2816/RepoGuardBench@49d6e51b5189f1f49a948bf360ed73d9487548a3 (MIT code, CC-BY-4.0 data)"
rows = []
def add(rid, text):
    h = int(hashlib.sha256(rid.encode()).hexdigest(), 16)
    rows.append({"id": rid, "label": "attack", "split": "heldout" if h % 2 else "dev", "source": SRC, "text": text})
for goal in c.ATTACK_GOALS:
    p = c.build_attack_payload(goal)
    for t in tasks:
        _, issue, _ = c.inject_attack(carrier="issue", goal=goal, repo_files={}, issue_text=t["issue_text"])
        add(f"repoguardbench-issue-{goal}-{t['task_id']}", issue)
    for carrier in ("readme", "test_log", "rule_file"):
        rf, _, _ = c.inject_attack(carrier=carrier, goal=goal, repo_files={"README.md": ""}, issue_text="")
        key = {"readme": "README.md", "test_log": "diagnostics/last_test_run.log", "rule_file": "AGENT_INSTRUCTIONS.md"}[carrier]
        add(f"repoguardbench-{carrier}-{goal}", rf[key].strip())
with open(os.path.join(d, "repoguardbench-attacks.jsonl"), "w", encoding="utf-8") as f:
    for r in rows: f.write(json.dumps(r, ensure_ascii=False, sort_keys=True) + "\n")
