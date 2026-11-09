# syntax=docker/dockerfile:1.7

# ------------------------------------------------------------------------------
# Stage 1: Build Preact + Signals + CodeMirror 6 Admin Console SPA
# ------------------------------------------------------------------------------
FROM --platform=$BUILDPLATFORM node:24-bookworm-slim AS admin-builder
WORKDIR /src/web/admin
COPY web/admin/package.json web/admin/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY internal/i18n/ /src/internal/i18n/
COPY internal/theme/ /src/internal/theme/
COPY web/admin/ ./
RUN rm -rf /src/internal/adminui/dist && mkdir -p /src/internal/adminui/dist && npm run build

# ------------------------------------------------------------------------------
# Stage 2: Compile Pure-Go Static Binary (CGO_ENABLED=0)
# ------------------------------------------------------------------------------
FROM --platform=$BUILDPLATFORM golang:1.27-bookworm AS go-builder
ARG TARGETOS=linux
ARG TARGETARCH=amd64
ARG VERSION=v1.0.0

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN rm -rf internal/adminui/dist
COPY --from=admin-builder /src/internal/adminui/dist ./internal/adminui/dist
COPY --from=admin-builder /src/internal/i18n/builtin_gz ./internal/i18n/builtin_gz
COPY --from=admin-builder /src/internal/theme/builtin/volo-studio/assets_gz ./internal/theme/builtin/volo-studio/assets_gz
COPY --from=admin-builder /src/internal/theme/builtin/volo-studio/templates_gz ./internal/theme/builtin/volo-studio/templates_gz

RUN CGO_ENABLED=0 GOEXPERIMENT=nojsonv2 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -buildvcs=false -trimpath \
    -gcflags="all=-l" \
    -ldflags="-s -w -funcalign=8 -X main.Version=${VERSION}" \
    -o /out/volo ./cmd/volo

# Prepare non-root runtime directory skeleton (uid:gid 65532:65532 for distroless:nonroot)
RUN mkdir -p \
      /volo-root/data/uploads \
      /volo-root/data/backups \
      /volo-root/data/geoip \
      /volo-root/plugins \
      /volo-root/themes \
      /volo-root/locales && \
    chown -R 65532:65532 /volo-root

# ------------------------------------------------------------------------------
# Stage 3: Minimal Distroless Non-Root Runtime
# ------------------------------------------------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot
LABEL org.opencontainers.image.title="Volo-go" \
      org.opencontainers.image.description="Pure-Go single-binary sovereign blog engine" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.source="https://github.com/Willh92/Volo-go"
WORKDIR /app

COPY --from=go-builder --chown=65532:65532 /volo-root/ /app/
COPY --chown=65532:65532 plugins/ /app/plugins/
COPY --chown=65532:65532 themes/ /app/themes/
COPY --chown=65532:65532 locales/ /app/locales/
COPY --chown=65532:65532 LICENSE THIRD_PARTY_NOTICES.md README.md CHANGELOG.md /app/
COPY --from=go-builder /out/volo /usr/local/bin/volo

ENV VOLO_LISTEN_ADDR=0.0.0.0:8080 \
    VOLO_DATA_DIR=/app/data \
    VOLO_UPLOAD_DIR=/app/data/uploads \
    VOLO_BACKUP_DIR=/app/data/backups \
    VOLO_GEOIP_DIR=/app/data/geoip \
    VOLO_PLUGINS_DIR=/app/plugins \
    VOLO_THEMES_DIR=/app/themes \
    VOLO_LOCALES_DIR=/app/locales

EXPOSE 8080
VOLUME ["/app/data"]

HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
  CMD ["/usr/local/bin/volo", "healthcheck"]

USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/volo"]
CMD ["serve"]
