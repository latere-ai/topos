# Memory folders: what a session keeps across sessions

An installation that keeps a person's files in a files service can keep
what its agents should remember there too, as files: a folder for the
person, a folder for each group of sessions. The authorizer names those
folders for a session, the session's file tools reach each one at
`/memory/<name>/`, and the next session with the same folder reads what
the last one wrote. The runner carries every call to the files service
with the session's own token, so no sandbox holds a credential, and a
session that only talks and remembers never opens a sandbox. This page is
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
so does a list on a server with no `TOPOS_FILES_AUDIENCE`, which reaches
no files service. A fork's folders are its own allow's, not the session's
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
sandbox's. Commands do not reach the folders, and `bash` is unchanged.

Every request of the session names the folders in its attachments block,
the same bytes on every turn:

```text
<attachments>
Memory folders, kept across sessions. read, write, edit, glob and grep reach them and commands do not; write a file empty to forget it:
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
| a listing, for `glob` and `grep` | `GET <url>/files/<owner>/<path>?list=1&limit=1000&cursor=<next_cursor>`, at most 2,000 files a folder |
| a write of a file the session read | `PUT` with `If-Match: <the ETag it read>` |
| a write of a new file | `PUT` with `If-None-Match: *` |
| a forget | `DELETE` with `If-Match: <the ETag it read>`, without `permanent=1`, so the file goes to the trash |

A Markdown file is written as `text/markdown; charset=utf-8`. A rename and
a command under `/memory/` are refused, and a change to a `read_only`
folder is refused before any request. A file holds at most 1 MiB; an
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
`progressive` the rules' verdict stands without asking it.

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
