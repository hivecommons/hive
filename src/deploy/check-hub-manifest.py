#!/usr/bin/env python3
"""Check a hub Deployment manifest (#19344, #19345).

Usage: check-hub-manifest.py FILE [FILE...]   (use "-" for stdin, e.g.
`kubectl -n hive-hub get deploy hive-hub -o yaml | check-hub-manifest.py -`)

Fails (exit 1) when a Deployment named hive-hub:
  - has a container other than the expected set (a stray "hive-hub" container
    next to the real "hub" container ran with the GitHub App credentials), or
  - sets a known secret-bearing env name as a literal `value:` or from
    anything other than valueFrom.secretKeyRef (configMapKeyRef, fieldRef,
    or no source at all), in containers or initContainers.

Keep EXPECTED_CONTAINERS in sync with hubContainerName in
src/pkg/hub/saas_sha_poller.go.
"""
import sys

import yaml

HUB_DEPLOYMENT = "hive-hub"
EXPECTED_CONTAINERS = {"hub"}
SECRET_ENV_NAMES = {
    "HIVE_HUB_OAUTH_CLIENT_SECRET",
    "HIVE_NPS_RELAY_PULL_SECRET",
}


def check_doc(doc):
    """Return a list of problem strings for one parsed YAML document."""
    if isinstance(doc, dict) and doc.get("kind") == "List":
        return [p for item in doc.get("items") or [] for p in check_doc(item)]
    if not isinstance(doc, dict) or doc.get("kind") != "Deployment":
        return []
    if (doc.get("metadata") or {}).get("name") != HUB_DEPLOYMENT:
        return []
    problems = []
    pod = ((doc.get("spec") or {}).get("template") or {}).get("spec") or {}
    for c in pod.get("containers") or []:
        name = c.get("name")
        if name not in EXPECTED_CONTAINERS:
            problems.append(
                "unexpected container %r (expected only %s)"
                % (name, sorted(EXPECTED_CONTAINERS))
            )
    for c in (pod.get("containers") or []) + (pod.get("initContainers") or []):
        for env in c.get("env") or []:
            if env.get("name") not in SECRET_ENV_NAMES:
                continue
            if not (env.get("valueFrom") or {}).get("secretKeyRef") or "value" in env:
                problems.append(
                    "container %r sets %s without valueFrom.secretKeyRef"
                    % (c.get("name"), env["name"])
                )
    names = {c.get("name") for c in pod.get("containers") or []}
    for missing in sorted(EXPECTED_CONTAINERS - names):
        problems.append("expected container %r is missing" % missing)
    return problems


def main(argv):
    if not argv:
        print(__doc__)
        return 2
    failed = False
    for path in argv:
        text = sys.stdin.read() if path == "-" else open(path).read()
        for doc in yaml.safe_load_all(text):
            for p in check_doc(doc):
                print("%s: %s" % (path, p))
                failed = True
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
