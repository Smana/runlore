# syntax=docker/dockerfile:1

# ---- build ----
# golang:1.26
FROM golang:1.26@sha256:079e59808d2d252516e27e3f3a9c003740dee7f75e55aa71528766d52bcfc16a AS build
WORKDIR /src
COPY go.mod go.sum ./
# Cache the module download and the compiler cache across builds (BuildKit cache
# mounts, persisted via cache-to in CI) so an unchanged dependency set or source
# tree doesn't recompile from scratch.
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download
COPY . .
ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/lore ./cmd/lore

# ---- runtime (distroless, nonroot) ----
# gcr.io/distroless/static:nonroot
FROM gcr.io/distroless/static:nonroot@sha256:d29e660cc75a5b6b1334e03c5c81ccf9bc0884a002c6000dbf0fb96034814478
LABEL org.opencontainers.image.source="https://github.com/Smana/runlore"
COPY --from=build /out/lore /usr/local/bin/lore
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/lore"]
CMD ["serve"]
