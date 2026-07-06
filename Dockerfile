# =============================================================================
# Squyrrl API — 多阶段构建
#
# 阶段 1（builder）：alpine + Go 工具链，编译 cmd/api 与 goose 迁移工具。
# 阶段 2（runtime）：debian-slim + 二进制（CGO_ENABLED=0 静态链接）。
#
# 服务端已不代抓网页（archive 功能连同 chromedp 一并移除），runtime 只需
# ca-certificates + tini，可进一步瘦身到 distroless / static，留作后续优化。
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

# ca-certificates（出站 HTTPS 到 R2 / Resend / 上游解析 API）+ tini PID 1
RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates \
        tini \
 && rm -rf /var/lib/apt/lists/*

ENV SQUYRRL_HTTP_ADDR=:8080

WORKDIR /app
COPY --from=builder /out/squyrrl-api /usr/local/bin/squyrrl-api
COPY --from=builder /out/goose       /usr/local/bin/goose
COPY internal/infra/db/migrations    /app/migrations

EXPOSE 8080
USER nobody:nogroup

ENTRYPOINT ["/usr/bin/tini", "--"]
CMD ["/usr/local/bin/squyrrl-api"]
