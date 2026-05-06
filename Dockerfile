# syntax=docker/dockerfile:1.7

FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
        -o /out/claude-proxy ./cmd/claude-proxy

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build /out/claude-proxy /usr/local/bin/claude-proxy

# /data holds proxy.db (sticky bindings, credentials)
# /creds is where the operator mounts .credentials.json files for import
VOLUME ["/data"]

EXPOSE 8787

ENTRYPOINT ["/usr/local/bin/claude-proxy"]
CMD ["serve", "--addr", "0.0.0.0:8787", "--db", "/data/proxy.db"]
