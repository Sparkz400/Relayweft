from common import fail, test_count, unit_tests_pass

from inventory.csvparse import parse_line, parse_file

unit_tests_pass()
for line, want in [("a,b,,", ["a", "b", "", ""]), ("a,", ["a", ""]), ("a,b", ["a", "b"]), ("", [])]:
    got = parse_line(line)
    if got != want:
        fail(f"parse_line({line!r}) = {got!r}, want {want!r}")
if test_count() <= 5:
    fail("no new test was added")
print("ok")
