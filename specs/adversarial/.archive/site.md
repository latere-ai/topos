---
title: Landing site
status: current
updated: 2026-07-05
author: changkun
---

# Landing site

`agon.latere.ai` is the project's landing page. It is the only thing this repo
deploys; the engine is a library and the CLI ships as `latere agon` elsewhere.

## Pieces

- **`cmd/agon-web/main.go`** - a stdlib-only HTTP server (no dependencies beyond
  the standard library). It serves `GET /healthz` and `/readyz` (both `no-store`
  "ok"), mounts the embedded SPA, listens on `:8080` (override with `-addr` or
  `AGON_ADDR`), and shuts down gracefully on SIGINT/SIGTERM.
- **`internal/web`** - embeds and serves the built SPA. `MountSPA` registers the
  static handlers (`/assets/` immutable for a year, `/fonts/` and `/static/`
  stale-while-revalidate); `SPAFallback` serves top-level files (favicon, og.svg,
  robots.txt, sitemap.xml) and falls back to `index.html` for client-side routes.
  The build output is embedded from `internal/web/spa/dist/`.
- **`frontend/`** - the Vue 3 + Vite (vite-ssg) source. Landing copy is data in
  `frontend/src/content/en.ts` and `zh.ts`, kept in structural parity by a vitest
  check. The build emits compiled `.js` next to each source and Vite loads those,
  so a copy edit is `edit .ts` then `bun run build` and commit the regenerated
  `.js`. See `CONTRIBUTING.md`.

## Deploy

`Dockerfile.web` builds the frontend (Bun) into the embed dir, then builds
`cmd/agon-web` into a distroless image `ghcr.io/latere-ai/agon-web`.

`.github/workflows/site.yml` deploys on a `v*` tag only; pushing to `main` never
deploys. On a tag it builds and pushes the image, applies `deploy/prod/` and rolls
the `agon-web` deployment on the `latere` Kubernetes namespace, smoke-tests `/`,
`/healthz`, and `/readyz` at `agon.latere.ai`, then creates the GitHub release with
the smoke evidence. Since agon ships no binary, `site.yml` is the only
tag-triggered workflow and the sole release creator.
