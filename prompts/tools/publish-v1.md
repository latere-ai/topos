Publishes a folder of the machine as a web app at a public address, and releases it.

With a folder, it puts the folder's files online as a preview of this session's app. The first call creates the app, named after the session; every later call publishes to the same app. It waits until the preview is built and returns its address, or why the build failed with the end of the build log. The app host serves static files: a folder with an index.html is served as it is, and a project with a build script, such as a Vite or Astro project, is built first. It runs no server process.

With `release` set to true, it puts the last ready preview at the app's own address.

Use it whenever the person wants to open, share or deploy a page or a site. Never start a web server on the machine for them instead: nothing on the machine can be reached from outside it.

Inputs:
- `path`: the folder to publish; a relative path resolves against the working directory. Default: the working directory.
- `release`: true to release the last ready preview instead of publishing a folder.

Limits:
- A call waits at most 10 minutes for a build or a release. When it is not done by then, call publish again the same way to keep waiting; nothing new is pushed while the folder is unchanged.
- `node_modules/` is never published, and the folder's own `.gitignore` applies.
