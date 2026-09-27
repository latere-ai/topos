import sys

from app.net import legacy_fetch

# legacy_fetch is kept here until the sync job moves to fetch.
if __name__ == "__main__":
    print(legacy_fetch(sys.argv[1]))
