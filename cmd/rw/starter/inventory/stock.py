"""Stock keeping."""

LOW_STOCK = 5


class Stock:
    def __init__(self):
        self.items = {}

    def add(self, name, qty):
        if qty <= 0:
            raise ValueError("quantity must be positive")
        self.items[name] = self.items.get(name, 0) + qty

    def remove(self, name, qty):
        if qty <= 0:
            raise ValueError("quantity must be positive")
        self.items[name] = self.items.get(name, 0) - qty

    def quantity(self, name):
        return self.items.get(name, 0)

    def low(self):
        return sorted(n for n, q in self.items.items() if q < LOW_STOCK)

    @classmethod
    def from_rows(cls, rows):
        s = cls()
        for r in rows:
            s.add(r["name"], int(r["qty"]))
        return s
