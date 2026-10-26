# heraldo-server image. Build from the repo root: docker build -t heraldo .
FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X github.com/noahkiss/heraldo/internal/version.Version=${VERSION}" \
    -o /out/heraldo-server ./cmd/heraldo-server

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/heraldo-server /heraldo-server
EXPOSE 7842
USER nonroot
ENTRYPOINT ["/heraldo-server"]
