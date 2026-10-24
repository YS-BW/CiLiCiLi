# syntax=docker/dockerfile:1
# CiLiCiLi 后端：多阶段构建，产物为只含 Go 二进制的 alpine 镜像（约 20MB）。
FROM golang:1.27-alpine AS build
WORKDIR /src
RUN apk add --no-cache ca-certificates
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY configs ./configs
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/cilicili ./cmd/cilici

FROM alpine:3
RUN apk add --no-cache ca-certificates tzdata
ENV TZ=Asia/Shanghai \
    LISTEN=0.0.0.0:8080 \
    DATA_DIR=/data \
    SOURCES_FILE=/etc/cilicili/sources.json
COPY --from=build /out/cilicili /usr/local/bin/cilicili
COPY configs/sources.json /etc/cilicili/sources.json
VOLUME ["/data"]
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/cilicili"]
