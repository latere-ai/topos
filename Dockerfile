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
# The helper a Cella machine uploads into each sandbox, one static build
# per platform a sandbox may run on (spec 009).
RUN for arch in amd64 arm64; do \
      CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath -o /out/helpers/topos-machine-linux-$arch ./cmd/topos-machine; \
    done

# >>> shared runtime base <<<
# toposd forks no binary of its own on the server roles, so the runtime
# stage is distroless: CA roots for the OIDC issuers and the cores it dials,
# and the helper builds it uploads, nothing else. A runner that drives git
# or the host machine uses its own image (spec 016). Dockerfile.ci carries
# this stage byte for byte, so the released image differs from this one in
# where the binaries came from and in nothing else.
FROM gcr.io/distroless/static-debian12:nonroot
EXPOSE 8080 8081
USER nonroot:nonroot
# <<< shared runtime base >>>
COPY --from=build /out/toposd /usr/local/bin/toposd
COPY --from=build /out/helpers/ /usr/local/lib/topos/
ENTRYPOINT ["/usr/local/bin/toposd"]
