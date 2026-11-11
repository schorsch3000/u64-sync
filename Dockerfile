# syntax=docker/dockerfile:1
# Build on the native platform and cross-compile: no QEMU needed for multi-arch images.
FROM --platform=$BUILDPLATFORM golang:1.27 AS build
ARG TARGETOS TARGETARCH TARGETVARIANT VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Pure Go (modernc sqlite): CGO off gives a fully static binary that runs on scratch.
RUN GOOS=$TARGETOS GOARCH=$TARGETARCH GOARM=${TARGETVARIANT#v} CGO_ENABLED=0 \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/u64-sync ./cmd/u64-sync \
 && mkdir /out/data && chown 65534:65534 /out/data

FROM scratch
COPY --from=build /out/u64-sync /u64-sync
# Scratch has no mkdir/chown at runtime, so /data is created here, owned by "nobody".
# Without a mount the container works as-is; with a bind mount run it as the directory's owner.
COPY --from=build --chown=65534:65534 /out/data /data
ENV U64_DATA_DIR=/data \
    U64_WEB_LISTEN=0.0.0.0:8064
USER 65534:65534
EXPOSE 8064
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 CMD ["/u64-sync", "-healthcheck"]
ENTRYPOINT ["/u64-sync"]
CMD ["-config", "/config.yaml"]
