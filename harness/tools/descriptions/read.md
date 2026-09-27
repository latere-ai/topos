Reads a file from the machine and returns its lines numbered from 1, in the form `     1	text`: the line number, a tab, then the line. The numbers are not part of the file; leave them out when you copy text into `edit`.

Use it to look at a file before you change it, to check what a command wrote, and to read the output log of a background job or a spill file named in an earlier result.

Inputs:
- `path`: the file. An absolute path is used as given; a relative path resolves against the working directory.
- `offset`: the first line to show, counted from 1. Default 1.
- `limit`: how many lines to show. Default 2000.

Limits:
- A line longer than 2000 characters is cut, and the cut is marked.
- When more lines follow the ones shown, the result ends with the range shown and the offset to read on from. Read a long file in parts with `offset` and `limit` instead of all at once.
- PNG, JPEG, GIF and WebP images up to 5 MiB are returned as an image.
- A binary file and a directory are errors. Use `glob` to list a directory.

Rules:
- `write` and `edit` change a file only after this thread has read it at its current content, so read a file before changing it, and read it again when something else may have changed it.
