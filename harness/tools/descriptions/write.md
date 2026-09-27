Writes a whole file on the machine, replacing any content it had, and creates the parent directories it needs.

Use it to create a new file, or to replace a file whose content changes almost entirely. For a change to part of a file, use `edit`, which sends only the text that changes.

Inputs:
- `path`: the file. An absolute path is used as given; a relative path resolves against the working directory.
- `content`: the whole new content of the file.

Rules:
- A file that does not exist is created.
- A file that exists is written only when this thread has read or written it at its current content. When the file was never read, or changed since this thread last read it, the write is refused with `<path> changed since it was last read; read it again before writing.` Read the file with `read`, then write again.
- A file outside the working directory and its other roots, and a file on the credential deny-list, is refused.
