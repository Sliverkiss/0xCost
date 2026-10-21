# syntax=docker/dockerfile:1
FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/chatglm2api ./cmd/chatglm2api \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/acct ./cmd/acct

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata wget bash \
 && adduser -D -u 10001 app \
 && mkdir -p /app/data \
 && chown -R app:app /app
WORKDIR /app
COPY --from=build /out/chatglm2api /app/chatglm2api
COPY --from=build /out/acct /app/acct
COPY config.example.json /app/config.json
USER app
EXPOSE 7988
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s \
  CMD wget -qO- http://127.0.0.1:7988/healthz || exit 1
ENTRYPOINT ["/app/chatglm2api", "-config", "/app/config.json"]
