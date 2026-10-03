import unittest

from inventory.report import format_report
from inventory.stock import Stock


class StockTest(unittest.TestCase):
    def test_add_remove(self):
        s = Stock()
        s.add("bolt", 10)
        s.remove("bolt", 3)
        self.assertEqual(s.quantity("bolt"), 7)

    def test_low(self):
        s = Stock()
        s.add("bolt", 10)
        s.add("nut", 2)
        self.assertEqual(s.low(), ["nut"])
        self.assertIn("LOW", format_report(s))


if __name__ == "__main__":
    unittest.main()
