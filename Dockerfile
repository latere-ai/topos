# SPDX-FileCopyrightText: 2026 Latere AI
# SPDX-License-Identifier: Apache-2.0

# The developer image: compiles toposd inside the image so `docker build .`
# from a checkout is enough. The release image of spec 028 copies a binary
# the pipeline already built and attested; its runtime stage is this one.

FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags "-X latere.ai/x/topos/internal/version.Version=${VERSION} -X latere.ai/x/topos/internal/version.Commit=${COMMIT} -X latere.ai/x/topos/internal/version.Date=${DATE}" \
      -o /out/toposd ./cmd/toposd

# toposd forks no binary of its own on the server roles, so the runtime
# stage is distroless: CA roots for the OIDC issuers and the cores it dials,
# nothing else. A runner that drives git or the host machine uses its own
# image (spec 016).
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/toposd /usr/local/bin/toposd
EXPOSE 8080 8081
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/toposd"]
