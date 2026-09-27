Replaces text in a file on the machine: `old_string` becomes `new_string`.

Use it for a change to part of a file. Use `write` to create a file or to replace one entirely.

Inputs:
- `path`: the file. An absolute path is used as given; a relative path resolves against the working directory.
- `old_string`: the exact text to replace, with its whitespace and indentation as the file has them. Copy it from the output of `read` without the line number and tab that start each line.
- `new_string`: the text to put in its place. It must differ from `old_string`.
- `replace_all`: replace every occurrence instead of exactly one. Default false.

Rules:
- Without `replace_all`, `old_string` must occur exactly once in the file. When it does not occur, read the file again and copy the text exactly. When it occurs more than once, include enough surrounding lines to make it unique, or set `replace_all` when every occurrence should change.
- The file must exist, and this thread must have read or written it at its current content. When it changed since this thread last read it, the edit is refused with `<path> changed since it was last read; read it again before writing.` Read the file, then edit again.
- Several changes to one file are several `edit` calls, each with its own unique `old_string`.
