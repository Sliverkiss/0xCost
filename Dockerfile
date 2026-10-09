# syntax=docker/dockerfile:1
# Multi-stage build (SDD §6): static Go binary → distroless non-root runtime.
# CGO_ENABLED=0 guarantees the binary runs on the distroless/static image;
# -trimpath -ldflags="-s -w" strip symbol tables for a small, reproducible
# artifact. Native arm64 (no cross-arch emulation needed on this host).

FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/mimo2api ./cmd/mimo2api

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/mimo2api /mimo2api
VOLUME /data
# The container port is 21223 — the deployment's own port, the one Caddy proxies to
# (127.0.0.1:21223). The image and compose agree instead of mapping two different
# numbers, and nothing in this deployment listens on the hosted platform's 3000.
EXPOSE 21223
ENV MIMO2API_PORT=21223
USER nonroot
ENTRYPOINT ["/mimo2api"]
