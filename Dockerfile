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

# module 代理可覆盖：默认走国内镜像(goproxy.cn)避开 proxy.golang.org 被墙/EOF；
# 境外/CI 环境用 `--build-arg GOPROXY=https://proxy.golang.org,direct` 即可切回官方。
# ,direct 兜底：镜像缺某模块时直连源仓库，不至于卡死。
ARG GOPROXY=https://goproxy.cn,direct
ENV GOPROXY=${GOPROXY}

# 缓存依赖层
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# goose 版本钉死为与 go.mod 一致的 v3.27.1（原 @latest 每次都要向 proxy 拉 @v/list
# 做版本解析——既不可复现，又正是本次 EOF 的触发点）。goose v3 本就是本项目库依赖，
# 该版本已在 go mod download 时进缓存，install CLI 子包无需再联网解析。
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
    -o /out/squyrrl-api ./cmd/api \
 && go install github.com/pressly/goose/v3/cmd/goose@v3.27.1 \
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
