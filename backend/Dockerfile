# 多阶段构建：纯 Go 静态二进制（modernc.org/sqlite 无 cgo），运行镜像极小。
FROM golang:1.23-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/alarm-api . && \
    CGO_ENABLED=0 go build -o /out/verify ./cmd/verify

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/alarm-api /out/verify /usr/local/bin/
EXPOSE 8080
ENTRYPOINT ["alarm-api"]
