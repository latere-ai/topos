Writes a whole file on the machine, replacing any content it had, and creates the parent directories it needs.

Use it to create a new file, or to replace a file whose content changes almost entirely. For a change to part of a file, use `edit`, which sends only the text that changes.

Inputs:
- `path`: the file. An absolute path is used as given; a relative path resolves against the working directory.
- `content`: the whole new content of the file.

Rules:
- A file that does not exist is created.
- A file that exists is written only when this thread has read or written it at its current content, so read an existing file with `read` before you write it. A file this thread never read is refused with `<path> exists and this thread has not read it; read it before writing to it.`, and one that changed since this thread last read it with `<path> changed since it was last read; read it again before writing.` Read the file, then write again.
- A file outside the working directory and its other roots, and a file on the credential deny-list, is refused.
