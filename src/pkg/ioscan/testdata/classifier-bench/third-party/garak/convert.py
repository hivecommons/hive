#!/usr/bin/env python3
"""Deterministically convert the pinned garak latent-injection templates into
classifier-bench JSONL. Usage: convert.py <dir>  (writes <dir>/garak-latent.jsonl)
Templates are copied verbatim from the pinned latentinjection.py; the only edit
is substituting garak's own PAYLOAD_MARKER with each payload from the pinned
domains_latentinjection.json. No text is written or paraphrased here."""
import ast, hashlib, json, sys, os
d = sys.argv[1]
tree = ast.parse(open(os.path.join(d, "latentinjection.py")).read())
payloads = json.load(open(os.path.join(d, "domains_latentinjection.json")))["payloads"]
CLASSES = ["FactSnippetMixin", "LatentInjectionReportFull", "LatentInjectionResumeFull", "LatentWhois"]
def render(n):
    if isinstance(n, ast.Constant): return n.value
    if isinstance(n, ast.BinOp) and isinstance(n.op, ast.Add):
        a, b = render(n.left), render(n.right)
        return None if a is None or b is None else a + b
    if not isinstance(n, ast.JoinedStr): return None
    out = []
    for v in n.values:
        if isinstance(v, ast.Constant): out.append(v.value)
        elif isinstance(v, ast.FormattedValue) and getattr(v.value, "id", "") == "PAYLOAD_MARKER": out.append("\0")
        else: return None
    return "".join(out)
rows = []
for c in tree.body:
    if isinstance(c, ast.ClassDef) and c.name in CLASSES:
        for s in c.body:
            if isinstance(s, ast.Assign) and getattr(s.targets[0], "id", "") == "injection_instructions":
                for i, e in enumerate(s.value.elts):
                    t = render(e)
                    if t is None or "\0" not in t: continue
                    for j, p in enumerate(payloads):
                        rid = f"garak-latent-{c.name}-{i}-{j}"
                        h = int(hashlib.sha256(rid.encode()).hexdigest(), 16)
                        rows.append({"id": rid, "label": "attack", "split": "heldout" if h % 2 else "dev",
                                     "source": "NVIDIA/garak@725160beb8137a8294c79b1f78c68a756367f861 (Apache-2.0)",
                                     "text": t.replace("\0", p)})
with open(os.path.join(d, "garak-latent.jsonl"), "w") as f:
    for r in rows: f.write(json.dumps(r, ensure_ascii=False, sort_keys=True) + "\n")
