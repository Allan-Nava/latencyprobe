# syntax=docker/dockerfile:1
#
# latencyprobe from a clone: `docker build -t latencyprobe .`
#
# A static binary and a CA bundle, nothing else — no shell, non-root. CI builds
# this image and runs it against llhls-origin. Released images come from
# Dockerfile.release instead, which packages the binary goreleaser already built,
# so an image and its release archive are the same build.

# The toolchain runs natively and Go cross-compiles to the target: there is no
# cgo, so nothing needs emulating under QEMU.
FROM --platform=$BUILDPLATFORM golang:1.25 AS build
WORKDIR /src
# No `go mod download`: go.mod has no dependencies, so copying the tree is the
# whole dependency step.
COPY . .
ARG VERSION=dev
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/latencyprobe ./cmd/latencyprobe

FROM scratch
# scratch has no trust store, and without one every https:// playlist fails TLS
# inside the container — which would read as a broken origin, the one thing this
# tool must never report falsely.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/latencyprobe /latencyprobe
# Numeric: scratch has no /etc/passwd. 65532 is distroless's nonroot, so both
# images run as the same user.
USER 65532:65532
ENTRYPOINT ["/latencyprobe"]
CMD ["--help"]
