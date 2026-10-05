"""Shared helpers for the rw bench starter checks (not part of the tasks)."""

import os
import subprocess
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
sys.path.insert(0, ROOT)


def fail(msg):
    print("CHECK FAILED:", msg)
    sys.exit(1)


def unit_tests_pass():
    """The project's own tests must still pass."""
    r = subprocess.run([sys.executable, "-m", "unittest", "discover", "-s", "tests", "-t", "."],
                       cwd=ROOT, capture_output=True, text=True)
    if r.returncode != 0:
        fail("the unit tests fail:\n" + r.stderr[-2000:])


def test_count():
    n = 0
    for dirpath, _, files in os.walk(os.path.join(ROOT, "tests")):
        for f in files:
            if f.startswith("test") and f.endswith(".py"):
                with open(os.path.join(dirpath, f), encoding="utf-8") as fh:
                    n += fh.read().count("def test")
    return n
