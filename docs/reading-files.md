# Reading a session's files

A session holds files of three kinds: the files a person sent with a
message, the files an agent's answer showed or linked, and the files of
the working directory its machine works in. This page is for whoever
writes a client that shows them. Everything here is on the wire of
`/v1`; the field names are the API's, and `api/openapi.yaml` states each
route under `getBlob` and `getFile`.

## Where the log names them

| Files | Named by | Read with |
|---|---|---|
| A person's files | each of a `user.message`'s `attachments`: `{name, media_type, size, blob, path}` | the blob route, by `blob` |
| What an answer showed or linked | each of the `files` of the `files.kept` that follows the `agent.message`: `{path, resolved, blob, media_type, size, width, height}` | the blob route, by `blob` |
| What the agent wrote or changed | a `write` or `edit` `tool.result`'s `meta`: `{path, sha256, size}` | the files route, by `path` |

The first two are kept with the session and stay readable after it
ends. The working directory is the machine's, and it is read only while
the machine runs; `session.machine` names it as `machine.workdir`.

## A kept file

`GET /v1/sessions/{id}/blobs/{digest}` answers a file the session keeps,
by the digest its record names:

```http
GET /v1/sessions/ses_01.../blobs/sha256:3362...e5e7

HTTP/1.1 200 OK
Content-Type: application/pdf
Content-Length: 11702886
Content-Disposition: attachment
X-Content-Type-Options: nosniff
Content-Security-Policy: sandbox; default-src 'none'
Cache-Control: private, max-age=31536000, immutable
ETag: "sha256:3362...e5e7"
```

| Header | What it says |
|---|---|
| `Content-Type` | For bytes that are a PNG, JPEG, GIF or WebP picture, that image type. For any other file, the `media_type` its record gave it when it was kept: the first `user.message` attachment or `files.kept` entry that names the digest and was not removed. A blob no event records, such as the agent's bundle, is labeled with the type its leading bytes give. |
| `Content-Length` | The file's length in bytes. |
| `Content-Disposition` | `inline` for those four picture formats, `attachment` for everything else, an SVG and an HTML page included. |
| `X-Content-Type-Options`, `Content-Security-Policy` | `nosniff` and `sandbox; default-src 'none'` on every answer, so a file opened at this address never runs as a page of the API's origin, whatever its type. |
| `Cache-Control`, `ETag` | The bytes never change under their digest, so keep them as long as you like; `private` keeps them out of shared caches, since the answer depends on who may read the session. |

`HEAD` answers the same headers without the bytes, so a client can show
a file's type and size, or refuse one too large for it, before it reads
it.

The type is the one the file was kept with, so the answer and the record
say the same thing: a person's file has the type its message named, else
the one its leading bytes give; a file an answer linked, the type its
name's extension names, else the one its leading bytes give; a picture
an answer showed, the format of its bytes. A label is not a reason to draw a file: an HTML page or an SVG is
labeled `text/html` or `image/svg+xml` and is still a download. A client
that shows one draws it in a sandbox of its own.

A file whose every record was removed, because a person removed the
message or the answer that named it, is `not_found`, and so is a digest
the session does not hold. For a session that keeps its files in a
folder of a files store, the file is read from that folder:
`files_unavailable` when the files service fails the read, and
`not_found` when the folder no longer holds it.

## A file of the working directory

`GET /v1/sessions/{id}/files?path=` answers one file of the working
directory as it is now, by its absolute path, as a `tool.result`'s
`meta.path` names it, or by a path relative to the working directory.
The answer is a download: `Content-Type` from the file's extension or its
leading bytes, `Content-Length`, `Content-Disposition: attachment` with
its name, the same `nosniff` and sandboxing policy, and
`Cache-Control: no-store`, since the file may change. `HEAD` answers the
headers alone.

The read never starts a machine. While the session's machine is not
running it is `file_unavailable`; the file can be read again once the
session runs its machine, as its next turn does. A file past 32 MiB is
`file_too_large`. A path outside the working directory, a directory, or
a file the credential deny-list names is `invalid_request`.

## Which files the working directory holds

Topos has no route that lists a working directory. A client learns which
files a session made from its log:

- the `files.kept` entries name the files its answers showed or linked;
- a `write` or `edit` `tool.result`'s `meta.path` and `meta.size` name
  each file the agent wrote or changed with those tools, while a file a
  command wrote is named by nothing but the answer that links it;
- a session that keeps its files in a folder of a files store keeps a
  copy of its working directory there, at `work/<path>`, and a turn that
  changed it ends with a `work.saved` whose `written` entries,
  `{path, size, mode, sha256}`, and `removed` paths say what changed.
  The copy holds the latest state alone, the files service lists and
  reads it, and the blob route does not answer its files.
