Finds files on the machine by name pattern and returns their absolute paths, the most recently modified first. It needs no binary on the machine, honors `.gitignore`, and skips `.git`.

Use it to find files by name or extension, and to list what a directory holds. Prefer it to running find or ls with `bash`.

Inputs:
- `pattern`: a glob matched against each file's path under `path`. `*` matches within one path segment, `**` matches any number of segments, zero included, `?` matches one character, and `[abc]` one of a set. For example `**/*.go` finds every Go file, and `cmd/*/main.go` the main file of each command.
- `path`: the directory to search. Default the working directory.

At most 1000 paths are returned; when more match, narrow the pattern or the path.
