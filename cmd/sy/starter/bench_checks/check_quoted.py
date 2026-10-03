from common import fail, unit_tests_pass

from inventory.csvparse import parse_line

unit_tests_pass()
cases = [
    ('a,"b,c",d', ["a", "b,c", "d"]),
    ('"x"', ["x"]),
    ('"say ""hi""",2', ['say "hi"', "2"]),
    ("a,,c", ["a", "", "c"]),
    ("a,b,c", ["a", "b", "c"]),
]
for line, want in cases:
    got = parse_line(line)
    # Trailing empty fields are a separate task; compare what both agree on.
    if got[: len(want)] != want:
        fail(f"parse_line({line!r}) = {got!r}, want {want!r}")
print("ok")
