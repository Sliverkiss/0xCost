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
EXPOSE 8080
USER nonroot
ENTRYPOINT ["/mimo2api"]
