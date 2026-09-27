Fetches a web page or file over http or https and returns it as text, with the final URL, the HTTP status and the content type first. An HTML page is converted to its readable text: scripts and styles are dropped, headings are marked with `#` and list items with `-`.

Use it to read documentation, a release note, an API reference, or any page the task names.

Inputs:
- `url`: the http or https URL to fetch.

Limits:
- The fetch has 30 seconds, follows at most 5 redirects, and reads at most 10 MiB of the body; a longer body is cut and the cut is marked.
- Only text is returned. An image, an archive or another binary body is an error.
- A status of 400 or more is an error; its body is still shown.
- The fetch runs on the machine and reaches only what the machine's network allows. On a machine without network access the tool reports that it is not available.
