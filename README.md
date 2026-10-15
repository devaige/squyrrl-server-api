# squyrrl-server-api

Squyrrl 主 API 单体（Go）。**独立仓库**，可单独开发、CI 与部署；作为 submodule 挂载在父仓库 [`squyrrl`](https://github.com/devaige/squyrrl) 的 `server/api`。

全栈文档在父仓库 `docs/readme/`（本仓库不重复维护）：
[服务端代码与检查](https://github.com/devaige/squyrrl/blob/main/docs/readme/02-server-code.md) ·
[生产部署](https://github.com/devaige/squyrrl/blob/main/docs/readme/03-server-prod.md) ·
[环境变量字典](https://github.com/devaige/squyrrl/blob/main/docs/readme/08-env-vars.md)

## 检查

部署测试直接使用正式环境。本地运行编译、静态检查与单元测试：

```bash
make vet
make test
make build
```

数据库集成测试仅在显式提供一次性测试库时运行，见父仓库服务端代码与检查手册。

## 生产部署

```bash
cp .env.prod.example .env.prod && chmod 600 .env.prod   # 首次，填 CHANGE_ME
docker compose -f docker-compose.prod.yml --env-file .env.prod up -d --build
```

`.github/workflows/deploy.yml` 在 push main 时自动重建部署；数据库迁移由 API 启动时执行。`nginx.conf` 是宿主反代配置（`api.squyrrl.com` → `127.0.0.1:10260`），安装方式见文件头注释。`nginx-cn.conf` 装在大陆服务器上（`api.squyrrl.cn` → 源站的中继 server 块），见父仓库 `docs/readme/16-cn-relay.md`。

## 跨服务约定

- 服务间走 external network `squyrrl`，tgbot / admin 用容器名 `api:8080` 直连，流量不出宿主。
- `SQUYRRL_TG_INTERNAL_TOKEN` / `SQUYRRL_INTERNAL_TOKEN` 是**跨仓库共享值**：改这里必须同步改 `squyrrl-server-thirdpart-telegram` / `squyrrl-server-web-admin` 的 `.env.prod`，否则对应服务全线 401（api 侧 fail-closed，不会启动失败）。
