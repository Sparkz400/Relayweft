from common import fail, test_count, unit_tests_pass

from inventory.stock import Stock

unit_tests_pass()
s = Stock()
s.add("bolt", 3)
try:
    s.remove("bolt", 5)
except ValueError:
    pass
else:
    fail("removing more than is in stock did not raise ValueError")
if s.quantity("bolt") != 3:
    fail("a failed remove changed the quantity")
s.remove("bolt", 3)
if s.quantity("bolt") != 0:
    fail("removing exactly the stock should leave 0")
if test_count() <= 5:
    fail("no new test was added")
print("ok")
