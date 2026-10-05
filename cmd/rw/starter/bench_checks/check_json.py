import json
import os
import subprocess
import sys

from common import ROOT, fail, unit_tests_pass

unit_tests_pass()
r = subprocess.run([sys.executable, "-m", "inventory.cli", "--json", "data.csv"], cwd=ROOT, capture_output=True, text=True)
if r.returncode != 0:
    fail("--json failed: " + r.stderr[-1000:])
try:
    data = json.loads(r.stdout)
except ValueError:
    fail("--json did not print JSON: " + r.stdout[:500])
items = data.get("items") if isinstance(data, dict) else data
if not isinstance(items, list) or len(items) != 3:
    fail("want a list of 3 items (or {\"items\": [...]}), got: " + r.stdout[:500])
by = {i.get("name"): i for i in items}
if by.get("nut", {}).get("qty") != 3 or by.get("nut", {}).get("low") is not True or by.get("bolt", {}).get("low") is not False:
    fail("each item needs name, qty (int) and low (bool): " + r.stdout[:500])
r = subprocess.run([sys.executable, "-m", "inventory.cli", "data.csv"], cwd=ROOT, capture_output=True, text=True)
if r.returncode != 0 or "LOW" not in r.stdout:
    fail("the plain report broke")
print("ok")
