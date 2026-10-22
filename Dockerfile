# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.26.10-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY cmd/ ./cmd/
COPY internal/ ./internal/
COPY storage/ ./storage/
COPY LICENSE ./LICENSE

ARG TARGETOS
ARG TARGETARCH
ARG VERSION
ARG COMMIT
# Match P7-A's explicit metadata contract; no fallback to unknown for images.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    set -eu; \
    test "$TARGETOS" = linux; \
    case "$TARGETARCH" in amd64|arm64) ;; *) exit 1 ;; esac; \
    export LC_ALL=C; \
    case "$VERSION" in ''|unknown|*..*|*[!A-Za-z0-9._-]*|[!A-Za-z0-9]*) exit 1 ;; esac; \
    test "${#VERSION}" -le 64; \
    case "$COMMIT" in *[!0-9a-fA-F]*) exit 1 ;; esac; \
    test "${#COMMIT}" -eq 40; \
    CGO_ENABLED=0 GOOS=linux GOARCH="$TARGETARCH" \
      go build -mod=readonly -trimpath -buildvcs=false \
      -ldflags="-X main.version=$VERSION -X main.commit=$COMMIT" \
      -o /out/uptime ./cmd/uptime; \
    mkdir -m 0700 /out/data

# Debian 13 is the maintained Distroless static line; includes system CA roots.
FROM gcr.io/distroless/static-debian13:nonroot
ARG VERSION
ARG COMMIT
LABEL org.opencontainers.image.title="LibreCosta Uptime" \
      org.opencontainers.image.source="https://github.com/librecosta/uptime" \
      org.opencontainers.image.licenses="MIT" \
      org.opencontainers.image.version=$VERSION \
      org.opencontainers.image.revision=$COMMIT
COPY --from=build /out/uptime /usr/local/bin/uptime
COPY --from=build /src/LICENSE /usr/share/licenses/librecosta-uptime/LICENSE
COPY --from=build --chown=65532:65532 /out/data /data
USER 65532:65532
WORKDIR /data
EXPOSE 18580
STOPSIGNAL SIGTERM
ENTRYPOINT ["/usr/local/bin/uptime"]
CMD ["serve", "--config", "/etc/uptime/uptime.yaml"]
