import unittest

from inventory.csvparse import parse_line


class ParseLineTest(unittest.TestCase):
    def test_simple(self):
        self.assertEqual(parse_line("a,b,c"), ["a", "b", "c"])

    def test_empty(self):
        self.assertEqual(parse_line("   "), [])

    def test_inner_empty(self):
        self.assertEqual(parse_line("a,,c"), ["a", "", "c"])


if __name__ == "__main__":
    unittest.main()
