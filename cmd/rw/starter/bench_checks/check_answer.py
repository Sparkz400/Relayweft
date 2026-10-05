import os

from common import ROOT, fail, unit_tests_pass

unit_tests_pass()
path = os.path.join(ROOT, "ANSWER.md")
if not os.path.exists(path):
    fail("ANSWER.md is missing")
text = open(path, encoding="utf-8").read()
for want in ["LOW_STOCK", "stock.py", "report.py", "low", "report_lines"]:
    if want not in text:
        fail(f"ANSWER.md does not mention {want}")
print("ok")
