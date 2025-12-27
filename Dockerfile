# 阶段 1: 编译 Web 管理器
FROM golang:1.20-alpine AS builder
WORKDIR /src
COPY main.go go.mod ./
RUN go mod tidy && go build -ldflags "-s -w" -o manager main.go

# 阶段 2: 运行镜像
FROM alpine:latest
# 安装必要依赖
# ca-certificates: 支持 HTTPS (API调用)
# tzdata: 时区支持
# gcompat: 提供 glibc 兼容性，确保用户上传的标准版 cfst 二进制文件能运行
# curl: 用于可能的网络调试
RUN apk --no-cache add ca-certificates tzdata gcompat curl

WORKDIR /app
# 复制 Web 管理器
COPY --from=builder /src/manager /app/manager
# 复制 HTML 模板
COPY index.html /app/index.html

# 暴露端口
EXPOSE 8080
# 挂载卷
VOLUME ["/app/data"]

CMD ["/app/manager"]