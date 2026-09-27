from app.net import fetch, legacy_fetch


def orders(base):
    return fetch(base + "/orders")


def order(base, n):
    body = legacy_fetch(base + "/orders/" + str(n))
    return body.decode()


def refunds(base):
    return legacy_fetch(base + "/refunds")
