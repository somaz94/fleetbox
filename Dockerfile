# The builder stays on the build host's platform and cross-compiles, instead of
# running the Go toolchain under QEMU for the other architecture.
FROM --platform=${BUILDPLATFORM} golang:1.27 AS builder
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG GIT_COMMIT=unknown
ARG BUILD_DATE=unknown

WORKDIR /workspace

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY cmd/ cmd/
COPY internal/ internal/

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -trimpath \
    -ldflags="-s -w \
      -X github.com/somaz94/fleetbox/cmd/cli.Version=${VERSION} \
      -X github.com/somaz94/fleetbox/cmd/cli.GitCommit=${GIT_COMMIT} \
      -X github.com/somaz94/fleetbox/cmd/cli.BuildDate=${BUILD_DATE}" \
    -o fleetbox ./cmd/

FROM gcr.io/distroless/static:nonroot
ARG VERSION=dev
ARG GIT_COMMIT=unknown
ARG BUILD_DATE=unknown

LABEL org.opencontainers.image.title="fleetbox" \
      org.opencontainers.image.description="Web console for containers outside Kubernetes: ECS services and Docker hosts in one view" \
      org.opencontainers.image.url="https://github.com/somaz94/fleetbox" \
      org.opencontainers.image.source="https://github.com/somaz94/fleetbox" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${GIT_COMMIT}" \
      org.opencontainers.image.created="${BUILD_DATE}"

COPY --from=builder /workspace/fleetbox /fleetbox
USER 65532:65532
EXPOSE 8080

ENTRYPOINT ["/fleetbox"]
CMD ["serve", "--config", "/etc/fleetbox/config/config.yaml"]
