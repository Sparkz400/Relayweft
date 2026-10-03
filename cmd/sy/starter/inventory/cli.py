"""python -m inventory.cli FILE.csv: print a stock report."""

import sys

from .csvparse import parse_file
from .report import format_report
from .stock import Stock


def main(argv=None):
    argv = sys.argv[1:] if argv is None else argv
    if len(argv) != 1:
        print("usage: python -m inventory.cli FILE.csv", file=sys.stderr)
        return 2
    stock = Stock.from_rows(parse_file(argv[0]))
    print(format_report(stock))
    return 0


if __name__ == "__main__":
    sys.exit(main())
