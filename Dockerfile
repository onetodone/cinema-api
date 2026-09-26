# syntax=docker/dockerfile:1

ARG GO_VERSION=1.27

# --- build: compile every binary in ./cmd as a static executable ---
FROM golang:${GO_VERSION}-alpine AS build
WORKDIR /src
ENV CGO_ENABLED=0

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go build -trimpath -ldflags="-s -w" -o /out/ ./cmd/...

# --- runtime: no shell, no package manager, non-root user ---
FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build /out/ /app/
USER nonroot:nonroot
EXPOSE 8080
# Other binaries (the worker from Sprint 4) run from the same image with a different entrypoint.
ENTRYPOINT ["/app/api"]
