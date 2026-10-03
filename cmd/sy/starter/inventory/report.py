"""Human-readable stock reports."""

from .stock import LOW_STOCK


def report_lines(stock):
    lines = []
    for name in sorted(stock.items):
        qty = stock.items[name]
        flag = "  LOW" if qty < LOW_STOCK else ""
        lines.append(f"{name:<20}{qty:>6}{flag}")
    return lines


def format_report(stock):
    return "\n".join(report_lines(stock))
