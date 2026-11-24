# syntax=docker/dockerfile:1.7
# Base images pinned by digest — tags are mutable and this image ships
# signed with SLSA provenance; Dependabot keeps the digests current.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine@sha256:70bd7ddea8b6cbe5d6943cde279d6b67feb10cc924fb9100d1d8757c1575b280 AS build

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown

WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

ENV CGO_ENABLED=0
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath \
      -ldflags "-s -w \
        -X github.com/llmspan/llmspan/internal/buildinfo.Version=${VERSION} \
        -X github.com/llmspan/llmspan/internal/buildinfo.Commit=${COMMIT} \
        -X github.com/llmspan/llmspan/internal/buildinfo.Date=${BUILD_DATE}" \
      -o /out/llmspan ./cmd/llmspan

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab

ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown

# OCI labels drive GHCR repo linking and image provenance display.
LABEL org.opencontainers.image.title="llmspan" \
      org.opencontainers.image.source="https://github.com/llmspan/llmspan" \
      org.opencontainers.image.description="OpenTelemetry proxy for LLM API traffic" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}" \
      org.opencontainers.image.created="${BUILD_DATE}"

COPY --from=build /out/llmspan /usr/local/bin/llmspan
# 4000 = default plaintext-loopback listen; 4443 = documented TLS listen.
EXPOSE 4000 4443
# Distroless has no curl — the binary probes itself. Override the URL
# when listen is non-default (TLS :4443, custom bind).
ENV LLMSPAN_HEALTHCHECK_URL=http://127.0.0.1:4000/healthz
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD ["/usr/local/bin/llmspan", "healthcheck"]
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/llmspan"]
CMD ["up"]
