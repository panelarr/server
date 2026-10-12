# syntax=docker/dockerfile:1
#
# Image for the Panelarr server: `make docker`, or
#   docker build \
#     --build-context meta=../meta --build-context source-api=../source-api \
#     [--build-context web=../web/dist] -t panelarr/server .
#
# The meta and source-api contexts stand in for the sibling modules go.mod
# points at until they are published. The web context is optional: without
# it the image serves a placeholder page instead of the web app.

ARG GO_VERSION=1.27.2

# Overridden by --build-context; empty otherwise.
FROM scratch AS meta
FROM scratch AS source-api
FROM scratch AS web

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS build
ARG TARGETOS TARGETARCH
ARG VERSION=dev
# go.mod's replace directives point at ../meta and ../source-api.
COPY --from=meta . /meta
COPY --from=source-api . /source-api
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY api ./api
COPY cmd ./cmd
COPY internal ./internal
COPY --from=web . ./internal/webui/dist/
RUN mkdir -p /out/config /out/data
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X github.com/panelarr/server/internal/buildinfo.Version=${VERSION}" \
      -o /out/panelarr ./cmd/panelarr

FROM gcr.io/distroless/static-debian12:nonroot
ARG VERSION=dev
LABEL org.opencontainers.image.title="server" \
      org.opencontainers.image.description="Panelarr server: libraries, matching, chapters, reader and downloads, with the web app" \
      org.opencontainers.image.source="https://github.com/panelarr/server" \
      org.opencontainers.image.licenses="GPL-3.0-or-later" \
      org.opencontainers.image.version="${VERSION}"
COPY --from=build /out/panelarr /panelarr
COPY --from=build --chown=65532:65532 /out/config /config
COPY --from=build --chown=65532:65532 /out/data /data
ENV PANELARR_LISTEN=:8900 \
    PANELARR_CONFIG=/config \
    PANELARR_DATA=/data
VOLUME ["/config", "/data"]
EXPOSE 8900
USER 65532:65532
HEALTHCHECK --interval=30s --timeout=5s --start-period=30s --retries=3 \
  CMD ["/panelarr", "healthcheck"]
ENTRYPOINT ["/panelarr"]
CMD ["serve"]
