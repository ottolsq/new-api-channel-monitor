FROM golang:1.26.1-alpine@sha256:2389ebfa5b7f43eeafbd6be0c3700cc46690ef842ad962f6c5bd6be49ed82039 AS builder

ENV GO111MODULE=on CGO_ENABLED=0 GOPROXY=https://mirrors.aliyun.com/goproxy/,direct

WORKDIR /build
COPY go.mod go.sum ./
RUN export GOPROXY=https://goproxy.cn,direct && \
    go clean -modcache && \
    go mod download

COPY . .
RUN go build -ldflags "-s -w" -o channel-monitor .

FROM alpine:latest

RUN apk add --no-cache ca-certificates tzdata

WORKDIR /app
COPY --from=builder /build/channel-monitor /app/channel-monitor

# 默认从工作目录读取 .env 配置（可被 docker -e / --env-file 覆盖），数据卷持久化 monitor.db
ENTRYPOINT ["/app/channel-monitor"]