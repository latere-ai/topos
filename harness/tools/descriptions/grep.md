Searches file contents on the machine with a regular expression. It needs no binary on the machine, honors `.gitignore`, and skips `.git` and binary files.

Use it to find where something is defined or used. Prefer it to running grep or rg with `bash`.

Inputs:
- `pattern`: an RE2 regular expression, the syntax of Go's regexp package. Escape the characters that are special, such as `\(` and `\.`.
- `path`: the directory or file to search. Default the working directory.
- `glob`: search only files matching this glob. A pattern without a slash, such as `*.go`, matches file names at any depth; one with a slash, such as `src/**/*.ts`, matches the path under `path`.
- `output_mode`: `files_with_matches` (default) lists the files that match; `content` shows each matching line as `path:line:text`, with context lines as `path-line-text`; `count` shows `path:count` per file.
- `context`: lines of context before and after each match, in `content` mode.
- `case_insensitive`: match regardless of case.
- `multiline`: let `.` match a newline, so a match may span lines.
- `head_limit`: at most this many output lines. Default 250.

Start with `files_with_matches` to see where a pattern occurs, then use `content` with `path` or `glob` narrowed to read the matches. When the output says more results follow, narrow the search rather than raising `head_limit` far.
