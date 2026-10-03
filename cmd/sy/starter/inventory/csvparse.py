"""Minimal CSV line parsing."""


def parse_line(line):
    """Split one CSV line into fields.

    Surrounding whitespace of the line is ignored.
    """
    line = line.strip()
    if not line:
        return []
    fields = line.split(",")
    while fields and fields[-1] == "":
        fields.pop()
    return fields


def parse_file(path):
    """Parse a CSV file with a header line into a list of dicts."""
    with open(path, encoding="utf-8") as f:
        lines = [l for l in f.read().splitlines() if l.strip()]
    if not lines:
        return []
    header = parse_line(lines[0])
    rows = []
    for l in lines[1:]:
        values = parse_line(l)
        values += [""] * (len(header) - len(values))
        rows.append(dict(zip(header, values)))
    return rows
