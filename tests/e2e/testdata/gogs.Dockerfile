# syntax=docker/dockerfile:1

FROM golang:1.25-alpine AS builder

RUN apk add --no-cache git

WORKDIR /src
COPY . .

ARG GOGS_COMMIT
ARG GOGS_GOPROXY=https://goproxy.cn,direct
RUN --mount=type=cache,id=gogs-mcp-smoke-modules,target=/go/pkg/mod \
    --mount=type=cache,id=gogs-mcp-smoke-build,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOPROXY="$GOGS_GOPROXY" go build \
    -trimpath \
    -ldflags "-X gogs.io/gogs/internal/conf.BuildCommit=$GOGS_COMMIT" \
    -o /out/gogs .
RUN --mount=type=cache,id=gogs-mcp-smoke-modules,target=/go/pkg/mod \
    --mount=type=cache,id=gogs-mcp-smoke-build,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOPROXY="$GOGS_GOPROXY" go build \
    -trimpath \
    -o /out/gogs-e2e-bootstrap ./internal/e2ebootstrap
RUN --mount=type=cache,id=gogs-mcp-smoke-modules,target=/go/pkg/mod \
    --mount=type=cache,id=gogs-mcp-smoke-build,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOPROXY="$GOGS_GOPROXY" go build \
    -trimpath \
    -o /out/gogs-e2e-repositories ./internal/e2erepositories
RUN --mount=type=cache,id=gogs-mcp-smoke-modules,target=/go/pkg/mod \
    --mount=type=cache,id=gogs-mcp-smoke-build,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOPROXY="$GOGS_GOPROXY" go build \
    -trimpath \
    -o /out/gogs-e2e-issues ./internal/e2eissues

FROM alpine:3.22

RUN apk add --no-cache ca-certificates git

ARG GOGS_COMMIT
LABEL org.opencontainers.image.revision="$GOGS_COMMIT"

WORKDIR /app
COPY --from=builder /out/gogs /app/gogs
COPY --from=builder /out/gogs-e2e-bootstrap /app/gogs-e2e-bootstrap
COPY --from=builder /out/gogs-e2e-repositories /app/gogs-e2e-repositories
COPY --from=builder /out/gogs-e2e-issues /app/gogs-e2e-issues

EXPOSE 3000
ENTRYPOINT ["/app/gogs"]
CMD ["web", "--config", "/data/app.ini"]
