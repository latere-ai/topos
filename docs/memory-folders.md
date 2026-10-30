# Memory folders: what a session keeps across sessions

An installation that keeps a person's files in a files service can keep
what its agents should remember there too, as files: a folder for the
person, a folder for each group of sessions. The authorizer names those
folders for a session, the session's file tools reach each one at
`/memory/<name>/`, and the next session with the same folder reads what
the last one wrote. The runner carries every call to the files service
with the session's own token, so no sandbox holds a credential, and a
session that only talks, remembers, and lists or searches its memory
with `bash` never opens a sandbox. This page is
the contract an installation's authorizer and its files service follow.
The field names are the wire's.

## Naming a session's memory folders

The authorizer names the folders in the limits of its allow of
`session.create` and `session.fork`:

```json
{
  "memory": [
    {"name": "user", "url": "https://files.example.com/v1", "owner": "https://id.example.com|ada",
     "path": "files/memory/you", "access": "read_only"},
    {"name": "project", "url": "https://files.example.com/v1", "owner": "https://id.example.com|ada",
     "path": "files/memory/projects/p1", "access": "read_write"}
  ]
}
```

| Member | Meaning |
|---|---|
| `name` | the folder's directory under `/memory/`: a lowercase letter, then up to 31 lowercase letters, digits and dashes, unique in the list |
| `url` | the files API's base URL, `http` or `https`, with no user, query or fragment |
| `owner` | the owner the files API keeps the folder under, 1 to 512 bytes with no control character |
| `path` | the folder's path under the owner, 1 to 512 bytes, with no empty, `.` or `..` segment and no control character |
| `access` | `read_only`: the session reads the folder and never changes it; `read_write`: it also writes and forgets files there |

At most 4 folders. A list that breaks a rule refuses the request with
`authorizer_unavailable`, naming the member in the developer detail, and
so does a folder of a files service on a server with no
`TOPOS_FILES_AUDIENCE`, which reaches none. A folder can also hold the
person's past conversations in place of files; see
[A folder of past conversations](#a-folder-of-past-conversations). A fork's folders are its own allow's, not the session's
it continues. A run of a rollout is isolated and gets none.

The session answers its folders by name and access alone, so a client
never reads the address or the owner:

```json
"memory": [{"name": "user", "access": "read_only"}, {"name": "project", "access": "read_write"}]
```

## What the session does with them

`read`, `write`, `edit`, `glob` and `grep` reach a file at
`/memory/<name>/<path>`. A call whose every path is under `/memory/` runs
on the runner, not in the session's sandbox, and opens no sandbox. A
relative path, or a path that climbs out of `/memory` once cleaned, is the
sandbox's.

`bash` reaches them too, for a command line that only works with files
there:

```sh
cd /memory/user && ls && grep -rn tide . && cat plan.md
```

A line whose every program is `ls`, `cat`, `head`, `tail`, `wc`, `grep`,
`find`, `sort`, `uniq`, `cut`, `mkdir`, `rm`, `mv`, `cp`, `touch` or a
shell builtin, and whose every path is under `/memory/`, runs on the
runner and opens no sandbox. It answers as the sandbox's `/bin/sh` would,
with GNU's output and messages, and its reads and writes are the file
tools' calls below, with the same preconditions. The runner decides this
before the line runs. Any other line, one that runs another program, uses
a path outside the folders, or runs in the background, runs in the
sandbox as before, and the sandbox does not see the folders. A line that
empties a file forgets it, and `touch` makes no empty file. A read-only
folder answers a change with `Read-only file system`.

A line on the runner runs within bounds, since it shares the runner
with every other session: 50,000 steps, 10 seconds, and what it may hold,
read and list. A line that reaches one stops where it is with
`/bin/sh: 2: <the bound>` and exit code 2, keeping what it changed
before; it does not move to the sandbox. A line longer than 64 KiB,
nested deeper than 64, with a command of more than 16 expansions, more
than 32 pipes or a pattern of more than 8 `*`, and a line that turns on
`set -x`, runs in the sandbox. A line keeps at most 1 MiB of output.

The runner logs which call opened a session's sandbox, `a tool call
opened the session's machine` with the tool and the reason, and counts
bash calls by where they ran in `latere.topos.bash.runs` and the calls
that opened a sandbox in `latere.topos.machine.opens`, so an installation
sees how many sandboxes its sessions' memory still opens.

Every request of the session names the folders in its attachments block,
the same bytes on every turn:

```text
<attachments>
Memory folders, kept across sessions. read, write, edit, glob and grep reach them, and so does a command that uses only ls, cat, head, tail, wc, grep, find, sort, uniq, cut, mkdir, rm, mv, cp, touch and the shell's builtins; any other program does not see them. Write a file empty to forget it:
- /memory/user/ (read only)
- /memory/project/ (read and write)
</attachments>
```

A session with apps names them first, in the same block. What a memory is
for, how its files are named, and the index the agent reads first are the
installation's: put them in a `context` part of the same allow. The
session reads nothing of a folder until the agent does.

## The files API

Each request carries the session's own token for `TOPOS_FILES_AUDIENCE`
and the workload `session` as its bearer. The token never reaches the
sandbox. A file's address is
`<url>/files/<owner>/<path>/<file>`, the owner and each path segment
escaped.

| Call | Request |
|---|---|
| a file's size and kind | `HEAD`; a refusal is read again with `GET` for its error body |
| a read | `GET`, following a redirect to a signed address without the bearer; the answer's `ETag` is kept |
| a listing, for `glob`, `grep` and a `bash` line | `GET <url>/files/<owner>/<path>?list=1&limit=1000&cursor=<next_cursor>`, at most 2,000 files a folder |
| a write of a file the session read | `PUT` with `If-Match: <the ETag it read>` |
| a write of a new file | `PUT` with `If-None-Match: *` |
| a `bash` write of a file the session never read | `PUT` with `If-Match: <the ETag the listing or a HEAD answers now>` |
| a forget | `DELETE` with `If-Match: <the ETag it read>`, without `permanent=1`, so the file goes to the trash |

A Markdown file is written as `text/markdown; charset=utf-8`. A `bash`
`mv` writes the file at its new path and forgets the old one, and a
change to a `read_only` folder is refused before any request. A file holds at most 1 MiB; an
installation bounds it lower by refusing the write.

`glob` and `grep` read the folder's listing, then the files they search.
A file read once is read again from the runner's copy while its checksum
is unchanged. A folder past 2,000 files is searched in its first 2,000,
and the result ends with the line
`[... a memory folder holds more than 2000 files; only the first 2000 were searched, and the rest were not read]`.

## Two sessions, one folder

Every write and forget is conditional on the version the session last
read, so two sessions writing one folder lose nothing. A `412` overwrites
nothing, and the agent reads
`<path> changed since it was last read; read it again before writing.`,
the same text a file changed under it in a sandbox gives. The agent reads
the file again and writes on the new version.

A files service that does not honor `If-Match` on `DELETE` forgets the
file whatever version it holds: the trash keeps it, and the person can
restore it.

## A folder of past conversations

A folder may hold the person's other conversations, served by the server
from its own log, so the agent finds and reads what was said before with
the same `glob`, `grep` and `read`. It names `sessions` in place of
`url`, `owner` and `path`:

```json
{"name": "chats", "access": "read_only", "sessions": {"metadata": {"project": "p1"}}}
```

| Member | Meaning |
|---|---|
| `sessions` | the folder holds conversations; `url`, `owner` and `path` are absent |
| `sessions.metadata` | at most one entry: a string keeps the sessions whose metadata holds the key with that value, `null` keeps those without the key, and none keeps every one. The key and value follow the metadata rules |
| `access` | `read_only`, and no other |

The folder holds the sessions of the session's own initiator, of the
agents of its context, that the entry keeps: one per fork tree, the
tree's newest, never a run of a rollout and never the reading session's
own tree. They are listed newest first, at most 2,000, as any folder is.
No `TOPOS_FILES_AUDIENCE` is needed; the runner reaches the server, over
its lease when it runs on its own.

Each conversation is a file `/memory/<name>/<yyyy-mm-dd>-<session id>.md`,
named by the day it started in UTC, so the name holds while its title
changes:

```markdown
# Tide tables

Session ses_01JA, started 2026-10-28 09:00 UTC, last changed 2026-10-28 10:12 UTC.

## User, 2026-10-28 09:00 UTC

What time is high tide at Kiel tomorrow?

## Agent, 2026-10-28 09:01 UTC

High tide at Kiel tomorrow is at 14:12.
```

A file holds what the person and the agent said on the session's own
thread, as session search reads it: messages and answers, never a tool's
output, a thinking block, a subagent's thread or a redacted event. A file
holds at most 256 KiB; a longer conversation keeps its messages in order
up to the bound, and its last line says how many were left out. A grep
reads a conversation again only once it has changed.

Every list asks `session.list`, and every conversation listed or read
asks `session.read`, as the session's initiator in its context. Both name
the reading session as their origin, and a read names the conversation's
metadata:

```json
{"origin": {"kind": "memory", "session_id": "ses_01JB", "folder": "chats"},
 "metadata": {"project": "p1"}}
```

A denied read leaves the conversation out of the folder and reads as a
file that is not there. A denied list is the call's result, with the
deny's reason as `refusal`:
`/memory/chats: This session may not read these conversations now.` An
authorizer that reads `origin` decides, at every call, whether the person
lets their conversations be read at all and which ones. A folder of
conversations is read only: a write, an edit or a forget there is refused
before anything is asked.

## Refusals

A `4xx` with an error body
`{"error": {"code": "...", "message": "...", "details": {"detail": "..."}}}`
is the call's result as `<path>: <message> <words>`, where `<words>`
follow `<reason>: ` at the start of `details.detail`, or after the action
the store names first, as Arca writes `file.write: <reason>: <words>`. The
result's meta
carries `refusal`: the reason, or the code when the detail names none. A
detail that names no reason is left out of the text. Write the words for
the agent, which reads them and acts on them:

```json
{"error": {"code": "forbidden", "message": "You do not have permission to do this.",
  "details": {"detail": "memory_name: a memory file is named with lowercase letters, digits and dashes, ending in .md."}}}
```

is

```text
/memory/project/Notes.txt: You do not have permission to do this. a memory file is named with lowercase letters, digits and dashes, ending in .md.
```

with `"refusal": "memory_name"`. A `5xx`, an answer that does not decode,
or no answer within 30 seconds is `Memory could not be reached: <detail>.`
A change to a `read_only` folder is
`<path> is in /memory/<name>/, which this session reads and does not change.`

## How a call is decided

A call whose every path is under `/memory/` scores 0.1 with the feature
`memory` in place of its tool's own. In `confirm` and `progressive` it is
allowed, with the reason `a memory the person can review`; in `plan` a
read is allowed and a write blocked, as every write is. Name `memory` in
`always_confirm` to ask the person about every memory call, reads
included, or a pattern such as `write(/memory/project/*)` to ask about
some. A decision service never changes a memory call's verdict: in
`progressive` the rules' verdict stands without asking it. A `bash` call
over the folders is decided as any command is.

## What a client shows

A write or an edit under `/memory/` records the change in its
`agent.tool_result`'s meta, beside `path`:

```json
{"path": "/memory/project/color.md", "sha256": "…", "size": 26,
 "memory": {"folder": "project", "path": "color.md", "action": "saved"}}
```

`action` is `saved`, or `forgotten` for a write of no content or an edit
that leaves the file empty, whose result reads `Forgot <path>.` and whose
meta has no `sha256`. Show a saved memory with a way to open the folder
where the person reviews it, and a forgotten one with the way back from
the trash.
