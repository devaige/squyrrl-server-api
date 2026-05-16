# =============================================================================
# Squyrrl API — 多阶段构建
#
# 阶段 1（builder）：alpine + Go 工具链，编译 cmd/api 与 goose 迁移工具。
# 阶段 2（runtime）：debian-slim + chromium（chromedp Renderer 用）+ 二进制。
#
# 不用 distroless / scratch 是因为 chromedp Archive Renderer 需要 chromium。
# 如果未来通过 SQUYRRL_ARCHIVE_RENDERERS=light 关闭 chromedp，可以换 alpine 基础镜像。
# =============================================================================

# -------- 编译阶段 --------
FROM golang:1.26-alpine AS builder
WORKDIR /src

# 缓存依赖层
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
    -o /out/squyrrl-api ./cmd/api \
 && go install github.com/pressly/goose/v3/cmd/goose@latest \
 && cp "$(go env GOPATH)/bin/goose" /out/goose

# -------- 运行阶段 --------
FROM debian:bookworm-slim AS runtime

# chromium + 字体（chromedp Archive Renderer）+ ca-certificates + tini PID 1
RUN apt-get update && apt-get install -y --no-install-recommends \
        chromium \
        fonts-noto-cjk fonts-noto-color-emoji \
        ca-certificates \
        tini \
 && rm -rf /var/lib/apt/lists/*

ENV SQUYRRL_CHROME_PATH=/usr/bin/chromium \
    SQUYRRL_HTTP_ADDR=:8080

WORKDIR /app
COPY --from=builder /out/squyrrl-api /usr/local/bin/squyrrl-api
COPY --from=builder /out/goose       /usr/local/bin/goose
COPY internal/infra/db/migrations    /app/migrations

EXPOSE 8080
USER nobody:nogroup

ENTRYPOINT ["/usr/bin/tini", "--"]
CMD ["/usr/local/bin/squyrrl-api"]
