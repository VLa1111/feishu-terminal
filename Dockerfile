# ---- 构建阶段：编译出静态二进制 ----
FROM golang:1.24-alpine AS build
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_TIME=unknown
WORKDIR /src
ENV CGO_ENABLED=0 \
    GOOS=linux
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -trimpath \
    -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.buildTime=${BUILD_TIME}" \
    -o /out/feishu-terminal .

# ---- 运行阶段：最小镜像，装 zsh + git + tmux ----
FROM alpine:3.20
# ca-certificates: 连 wss://open.feishu.cn 的 TLS 校验需要
# zsh:            bot 专用提示符（地址栏）依赖
# git:           提示符里显示当前 git 分支
# tmux:           持久会话后端（每个聊天一个 tmux 会话）
# tzdata:        让 TZ 时区设置（docker-compose 里的 Asia/Shanghai）生效
RUN apk add --no-cache ca-certificates zsh git tmux tzdata \
    && addgroup -S bot \
    && adduser -S -G bot bot

WORKDIR /app
COPY --from=build /out/feishu-terminal /app/feishu-terminal
COPY zsh /app/zsh
RUN chown -R bot:bot /app

# 给 shell 一个终端类型，避免部分命令（less/top 等）因缺 TERM 报错
ENV TERM=xterm

USER bot
ENTRYPOINT ["/app/feishu-terminal"]
