Publishes a folder of the machine as a web app at a public address, and releases it.

With a folder, it puts the folder's files online as a preview of an app. The first call creates this session's own app, named after the session; every later call publishes to the same app. When the session was given apps to keep building, each is a checkout of its source in the working directory, named with its slug in your context: name it with `app` to publish its checkout, which commits it on your branch and pushes it. It waits until the preview is built and returns its address, or why the build failed with the end of the build log. The app host serves static files: a folder with an index.html is served as it is, and a project with a build script, such as a Vite or Astro project, is built first. It runs no server process.

With `release` set to true, it puts the app's newest preview at the app's own address; a preview still building is released once it is built. A release is refused while the address serves a version your work does not include, such as another session's release: merge that version, publish, and release again.

Use it whenever the person wants to open, share or deploy a page or a site. Never start a web server on the machine for them instead: nothing on the machine can be reached from outside it.

Inputs:
- `path`: the folder to publish; a relative path resolves against the working directory. Default: the checkout of the app `app` names, else the working directory.
- `app`: the slug of the app to publish to or release: one the session was given, or one this session created. Default: the app whose checkout `path` is in, else this session's own app.
- `release`: true to release the app's newest preview instead of publishing a folder.

Limits:
- A call waits at most 10 minutes for a build or a release. When it is not done by then, call publish again the same way to keep waiting; nothing new is pushed while the folder is unchanged.
- `node_modules/` is never published, and the folder's own `.gitignore` applies.
