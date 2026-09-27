import urllib.request


def legacy_fetch(url):
    """Fetches url the old way; use fetch instead."""
    with urllib.request.urlopen(url) as r:
        return r.read()


def fetch(url, timeout=10):
    with urllib.request.urlopen(url, timeout=timeout) as r:
        return r.read()
