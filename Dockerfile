# 第一阶段：使用开发环境镜像进行构建，设置别名builder
FROM golang:1.23-alpine AS builder

# 设置工作目录
WORKDIR /app

# 复制所有文件到工作目录
COPY . .

# 编译应用程序
RUN chmod +x ./build.sh && ./build.sh

# 第二阶段：使用小体积的基础镜像 打包最终镜像
FROM gcr.io/distroless/static-debian12 


WORKDIR /app

# 从构建阶段复制编译好的可执行文件
COPY --from=builder /app/rss2telegram .

# 设置时区
ENV TZ=Asia/Shanghai

# 运行可执行文件
CMD ["./rss2telegram"]