Publishes a folder of the machine as a web app at a public address, and releases it.

With a folder, it puts the folder's files online as a preview of an app. The first call creates this session's own app, named after the session; every later call publishes to the same app. When the session was given apps to keep building, each is a checkout of its source in the working directory, named with its slug in your context: name it with `app` to publish its checkout, which commits it on your branch and pushes it. It waits until the preview is built and returns its address, or why the build failed with the end of the build log. The app host serves static files: a folder with an index.html is served as it is, and a project with a build script, such as a Vite or Astro project, is built first. It runs no server process.

With `release` set to true, one call puts a change live: it publishes the folder as above, waits for the build, and releases it to the app's own address. The person approves that one call. This is the way to put a change live; publish without `release` first only when the person wants to look at a preview before it goes live. When the build fails, nothing is released and the result says why. A release is refused while the address serves a version your work does not include, such as another session's release: merge that version, then release again.

Use it whenever the person wants to open, share or deploy a page or a site. Never start a web server on the machine for them instead: nothing on the machine can be reached from outside it.

Inputs:
- `path`: the folder to publish; a relative path resolves against the working directory. Default: the checkout of the app `app` names, else the folder last published to this session's own app, else the working directory.
- `app`: the slug of the app to publish to or release: one the session was given, or one this session created. Default: the app whose checkout `path` is in, else this session's own app. A release that names neither `app` nor `path` takes the app this session has a preview of, else the one app it has published; when there are several, name one.
- `release`: true to publish and release in one call.

Limits:
- A call waits at most 10 minutes for a build, and a release at most 10 minutes more for the release itself. When it is not done by then, call publish again the same way to keep waiting; nothing new is pushed while the folder is unchanged.
- `node_modules/` is never published, and the folder's own `.gitignore` applies.
