# syntax=docker/dockerfile:1
FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# 一次编译全部二进制（工具进镜像，容器内可直接跑脚本）。全部 -trimpath -s -w。
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/r2api ./cmd/server \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/signin_bin ./cmd/signin \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/login_bin ./cmd/login \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/credit_bin ./cmd/credit \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/trial_bin ./cmd/trial \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/acct_bin ./cmd/acct \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/activity_bin ./cmd/activity \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/stats_bin ./cmd/stats

FROM alpine:3.20
# python3：login.sh 的 JSON 解析与落盘；bash：shell 脚本体。
RUN apk add --no-cache wget ca-certificates tzdata python3 bash \
 && adduser -D -u 10001 app \
 && mkdir -p /app/auths /app/data \
 && chown -R app:app /app
WORKDIR /app
# 脚本置入 + 去 CRLF（Windows 检出可能性）在切到 app 之前以 root 完成——
# app 对 root 所有文件无写权限，sed -i 需要写权限。
COPY --from=build /out/r2api /app/r2api
COPY --from=build /out/signin_bin /app/signin_bin
COPY --from=build /out/login_bin /app/login_bin
COPY --from=build /out/credit_bin /app/credit_bin
COPY --from=build /out/trial_bin /app/trial_bin
COPY --from=build /out/acct_bin /app/acct_bin
COPY --from=build /out/activity_bin /app/activity_bin
COPY --from=build /out/stats_bin /app/stats_bin
COPY login.sh signin.sh acct.sh checkin.sh credit.sh trial.sh /app/
RUN sed -i 's/\r$//' /app/*.sh && chmod 755 /app/*.sh
# 镜像不带真实配置：落 example 作为默认（生产由挂载卷 /app/config.json 覆盖）
COPY config.example.json /app/config.json
USER app
EXPOSE 7863
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s \
  CMD wget -qO- http://127.0.0.1:7863/healthz || exit 1
ENTRYPOINT ["/app/r2api", "-config", "/app/config.json"]
