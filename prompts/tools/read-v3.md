Reads a file from the machine and returns its lines numbered from 1, in the form `     1	text`: the line number, a tab, then the line. The numbers are not part of the file; leave them out when you copy text into `edit`.

Use it to look at a file before you change it, to check what a command wrote, and to read the output log of a background job or a spill file named in an earlier result.

Inputs:
- `path`: the file. An absolute path is used as given; a relative path resolves against the working directory.
- `offset`: the first line to show, counted from 1. Default 1.
- `limit`: how many lines to show. Default 2000.
- `pages`: for a PDF, the pages to read, such as `1-5`, `1-3,7` or `9-` for page 9 to the end. Default `1-5`.

Limits:
- A line longer than 2000 characters is cut, and the cut is marked.
- When more lines follow the ones shown, the result ends with the range shown and the offset to read on from. Read a long file in parts with `offset` and `limit` instead of all at once.
- PNG, JPEG, GIF and WebP images up to 5 MiB are returned as an image.
- Word, Excel and PowerPoint files (`.docx`, `.xlsx`, `.pptx`) and their OpenDocument counterparts (`.odt`, `.ods`, `.odp`) are returned as their text, without formatting or images: a document's paragraphs, each sheet's rows with their cells separated by tabs, each slide's text. The line numbers count lines of that text, and `edit` cannot change such a file.
- A PDF that a person attached, or one that was in the working directory as it is now at the end of an earlier turn, is read as text through a parsing service and returned as Markdown, page by page, each page opened by a `<!-- page N -->` mark, without images. The result says how many pages the PDF has and which `pages` to ask for next. Only the first 500 pages of a longer PDF are read. Reading a PDF the first time can take a few minutes; reading other pages of it again is fast. A PDF that cannot be read this way is answered as a binary file, with a sentence saying why.
- Any other binary file and a directory are errors. Use `glob` to list a directory.

Rules:
- `write` and `edit` change a file only after this thread has read it at its current content, so read a file before changing it, and read it again when something else may have changed it.
