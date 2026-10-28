#!/usr/bin/env python3
"""Hermetic tests for the CI infra classifier, rerun-once guard and rate
alert (#9664). No network, no gh: GitHub is a fake.

    python3 .github/scripts/test-ci-infra.py
"""

import contextlib
import io
import json
import os
import re
import subprocess
import sys
import tempfile
import unittest
import zipfile

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)

import ci_infra_actions as actions  # noqa: E402
import ci_infra_classify as classifier  # noqa: E402

FIXTURES = os.path.join(HERE, "testdata", "ci-infra")
WORKFLOWS = os.path.join(HERE, "..", "workflows")
RERUN_WORKFLOW = os.path.join(WORKFLOWS, "ci-infra-rerun.yml")
REPO = "hivecommons/hive"

def read(path):
    with open(path, encoding="utf-8", errors="replace") as fh:
        return fh.read()

def fixture(name):
    log, ann_path = read(os.path.join(FIXTURES, name + ".log"))
    ann = read(ann_path) if os.path.exists(ann_path) else ""
    return log, ann

def expected_fixtures():
    rows = {}
    for line in read(os
