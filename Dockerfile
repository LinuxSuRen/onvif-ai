# syntax=docker/dockerfile:1

# ---- 前端构建：Vue 3 + Vite ----
FROM node:22-alpine AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

# ---- 后端构建：纯静态编译（CGO 关闭）----
FROM golang:1.24-alpine AS build
ARG VERSION=dev
ARG COMMIT=unknown
WORKDIR /src
COPY go.mod go.sum ./
# go.mod 以 replace 指向 ./third_party/aac-go，mod download 前必须先就位
COPY third_party/ ./third_party/
RUN go mod download
COPY cmd/ ./cmd/
COPY internal/ ./internal/
# go:embed 需要前端构建产物（embed.go + dist）参与编译
COPY web/ ./web/
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" \
    -o /out/onvif-ai ./cmd/server

# ---- 运行时 ----
# alpine 而非 scratch：需要 ca-certificates（LLM/TTS 走 HTTPS）与常用调试工具
FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata && adduser -D -u 10001 onvif
WORKDIR /app
COPY --from=build /out/onvif-ai ./onvif-ai
COPY .env.example ./
USER onvif
ENV PORT=8080
EXPOSE 8080
# WS-Discovery 依赖 UDP 3702 组播，建议 --network host（Linux）
ENTRYPOINT ["./onvif-ai"]
