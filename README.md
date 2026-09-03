# squyrrl-server-api

Squyrrl 主 API 单体（Go）。**独立仓库**，可单独开发、CI 与部署；作为 submodule 挂载在父仓库 [`squyrrl`](https://github.com/devaige/squyrrl) 的 `server/api`。

全栈文档在父仓库 `docs/readme/`（本仓库不重复维护）：
[服务端开发](https://github.com/devaige/squyrrl/blob/main/docs/readme/02-server-dev.md) ·
[生产部署](https://github.com/devaige/squyrrl/blob/main/docs/readme/03-server-prod.md) ·
[环境变量字典](https://github.com/devaige/squyrrl/blob/main/docs/readme/08-env-vars.md)

## 本地开发

本仓库的 dev compose 自带 postgres + minio，是全栈里唯一持有这两项依赖的仓库 —— 因此**本机开发要先起本仓库**，tgbot / admin 的 dev compose 以 external 方式加入这里创建的 `squyrrl` 网络。

```bash
cp .env.example .env
docker compose -f docker-compose.dev.yml up -d --build
docker compose -f docker-compose.dev.yml logs -f api
```

原生跑（不走容器，便于断点调试）：

```bash
make infra   # 只起 postgres / minio
make run     # go run ./cmd/api，读同目录 .env
```

`.env` 里写的是宿主地址（`localhost:55432` / `:59000`），容器化启动时由 `docker-compose.dev.yml` 的 `environment` 覆盖为服务名 —— 一份模板同时服务两种跑法。

## 生产部署

```bash
cp .env.prod.example .env.prod && chmod 600 .env.prod   # 首次，填 CHANGE_ME
docker compose -f docker-compose.prod.yml --env-file .env.prod up -d --build
```

`.github/workflows/deploy.yml` 在 push main 时自动做同样的事并跑数据库迁移。`nginx.conf` 是宿主反代配置（`api.squyrrl.app` → `127.0.0.1:8080`），安装方式见文件头注释。

## 跨服务约定

- 服务间走 external network `squyrrl`，tgbot / admin 用容器名 `api:8080` 直连，流量不出宿主。
- `SQUYRRL_TG_INTERNAL_TOKEN` / `SQUYRRL_INTERNAL_TOKEN` 是**跨仓库共享值**：改这里必须同步改 `squyrrl-server-thirdpart-telegram` / `squyrrl-server-web-admin` 的 `.env.prod`，否则对应服务全线 401（api 侧 fail-closed，不会启动失败）。
