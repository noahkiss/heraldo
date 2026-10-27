# heraldo-server image: a static binary on distroless, run as non-root, with
# the database on the /data volume. Build from the repo root:
#   docker build -t heraldo .
# The build stage runs on the build host and cross-compiles, so a
# linux/amd64 image builds fast on an arm64 machine (no cgo: pure-Go SQLite).
FROM --platform=$BUILDPLATFORM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
    -ldflags "-s -w -X github.com/noahkiss/heraldo/internal/version.Version=${VERSION}" \
    -o /out/heraldo-server ./cmd/heraldo-server \
 && mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/heraldo-server /heraldo-server
# The nonroot user (65532) owns /data, so a fresh named volume is writable.
COPY --from=build --chown=65532:65532 /out/data /data
ENV HERALDO_ADDR=:7842 \
    HERALDO_DB=/data/heraldo.db
VOLUME /data
EXPOSE 7842
USER nonroot
HEALTHCHECK --interval=30s --timeout=6s --start-period=10s CMD ["/heraldo-server", "healthcheck"]
ENTRYPOINT ["/heraldo-server"]
