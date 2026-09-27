from app.api import orders


def test_orders(server):
    assert orders.orders(server.url) is not None
