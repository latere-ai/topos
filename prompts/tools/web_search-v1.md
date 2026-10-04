Searches the web and returns a list of results, each with its title, its URL and a snippet of the page's text.

Use it to find what you do not have an address for: a current fact, a release, a project's documentation, an error message others have met. When you know the address, fetch it with `web_fetch` instead. A snippet is only part of a page: fetch a result with `web_fetch` to read it in full before you rely on details it does not show.

How to search:
- Write the query as a person would type it into a search engine: the few words that matter, a name, a version, an exact error.
- Search when the answer depends on what you do not know, not to confirm what you already know. Each search may be charged to the person you work for.
- Cite the URL of each result you rely on in your answer.

Inputs:
- `query`: what to search for, at most 400 characters.
- `max_results`: how many results to return, 1 to 10. The default is 5.

When a search is refused, the result says why in a sentence written for the person. Tell them what it says, and do not search again until what it names has changed.
