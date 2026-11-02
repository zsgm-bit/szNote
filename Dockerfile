# ---- 构建阶段：编译 Go 单二进制（纯标准库，无需联网拉依赖）----
FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod main.go ./
COPY static ./static
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /sznote .

# ---- 运行阶段：极小镜像 ----
FROM alpine:3.20
LABEL org.opencontainers.image.title="szNote" \
      org.opencontainers.image.description="自托管网页笔记服务（AGPL-3.0 或商业许可）" \
      org.opencontainers.image.licenses="AGPL-3.0-or-later" \
      org.opencontainers.image.url="https://github.com/zsgm-bit/szNote"
RUN apk add --no-cache ca-certificates tzdata
WORKDIR /app
COPY --from=build /sznote /app/sznote
# 许可文本随镜像分发（AGPL-3.0 合规）；对应源码见 https://github.com/zsgm-bit/szNote
COPY LICENSE COMMERCIAL-LICENSE.md /app/
# 内置一份默认配置；实际部署用 volume 挂载覆盖（见 docker-compose.yml）
COPY config.json /app/config.json
VOLUME /app/data
EXPOSE 9720
ENTRYPOINT ["/app/sznote"]
