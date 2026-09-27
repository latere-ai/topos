from app.net import fetch


def users(base):
    return fetch(base + "/users")
