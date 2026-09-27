Keeps this thread's todo list: each call replaces the whole list with the one given, and the result shows it back.

Use it to plan work that takes several steps, and to keep track of it as you go: write the steps as `pending` when you start, mark one `in_progress` when you begin it, and mark it `completed` as soon as it is done. Keep one item `in_progress` at a time. Skip the list for a task of one or two steps.

Inputs:
- `todos`: the whole list, at most 100 items. Each item has:
  - `id`: a short id, unique in the list, which stays the same as the item's status changes.
  - `content`: the step, in one line.
  - `status`: `pending`, `in_progress` or `completed`.

Every call sends the complete list, not only the items that changed. An empty list clears it.
