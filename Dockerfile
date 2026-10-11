# Container images for the gateway (the default target) and the local fake
# provider (target fakeprovider), for linux/amd64 and linux/arm64:
#
#   make image          # this machine's platform, loaded into Docker
#   make image-check    # both platforms, neither loaded nor pushed
#
# Base images are pinned by the digest of their multi-platform index. Update
# a tag and its digest together:
#   docker buildx imagetools inspect <image>:<tag>

# The build stage runs on the builder's own platform and cross-compiles for
# the target one, so a multi-platform build needs no emulation.
FROM --platform=$BUILDPLATFORM golang:1.26.9@sha256:d7722066f0b60ceccb6c0643cbed1f5f9e15506ac237146c95333504d7805d89 AS build
# Fail instead of downloading another toolchain if go.mod asks for one.
ENV GOTOOLCHAIN=local
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download
COPY cmd ./cmd
COPY internal ./internal
ARG TARGETOS TARGETARCH
# A static binary without symbol tables or DWARF. Stack traces and profiles
# keep function names, which come from the runtime's own tables.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags='-s -w' -o /out/ ./cmd/gateway ./cmd/fakeprovider

# Distroless static: CA certificates, time zone data, and a non-root user,
# but no shell or package manager. The user is set numerically, so that
# Kubernetes can verify runAsNonRoot.
FROM gcr.io/distroless/static-debian13:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3 AS runtime
USER 65532:65532

# The fake provider is a local fixture for demonstrations and load tests,
# never a provider for real traffic. It listens on 127.0.0.1:9000 unless
# -addr says otherwise.
FROM runtime AS fakeprovider
COPY --from=build /out/fakeprovider /fakeprovider
ENTRYPOINT ["/fakeprovider"]

# The gateway, the default target. GATEWAY_ADDR listens on every interface of
# the container's network namespace, so the port can be published or probed.
# The metrics listener stays on loopback unless GATEWAY_METRICS_ADDR is set.
# There is no HEALTHCHECK: the image has no HTTP client to run one, and
# Kubernetes ignores it in favor of its own probes of /livez and /readyz.
FROM runtime AS gateway
COPY --from=build /out/gateway /gateway
ENV GATEWAY_ADDR=:8080
EXPOSE 8080
ENTRYPOINT ["/gateway"]
