# 部署与各系统服务安装指南

> English version: [Deployment & OS Service Installers](../en-US/deployment.md)

`Volo-go` 基于 Go `1.27.4`（`CGO_ENABLED=0`、`GOEXPERIMENT=nojsonv2`）编译为单个零外部依赖静态二进制文件（**全部 6 大静态发布目标架构均 `< 22 MiB`**，`linux/amd64` `~19.6 MiB`）：
- **Linux**: `linux/amd64`、`linux/arm64`（支持 `systemd` 系统/用户级与 `OpenRC`）
- **macOS**: `darwin/amd64`、`darwin/arm64`（支持 `launchd` `LaunchDaemon` / `LaunchAgent`）
- **Windows**: `windows/amd64`、`windows/arm64`（支持 `svc_windows.go` 原生 `sc.exe` Windows Service、`NSSM` 与系统计划任务）
- **容器化**: Docker Compose / GHCR Distroless 非 root 镜像（`UID/GID 65532`）

---

## 1. 配置优先级体系与完整环境变量表

### 1.1 配置覆盖优先级

- **全局基础配置（四层顺序）**：
  `CLI 命令行参数 (--listen-addr 等) > 操作系统环境变量 (VOLO_*) > .env 文件 > 内置安全默认值`
- **`VOLO_UPLOAD_DIR` 与 `VOLO_GEOIP_DIR` 热重载目录（五层顺序）**：
  `CLI 参数 (--upload-dir / --geoip-dir) > 管理台数据库设置 (storage.local_dir / security.geoip_dir) > 系统环境变量 / .env 文件 (VOLO_UPLOAD_DIR / VOLO_GEOIP_DIR) > 默认目录 (${VOLO_DATA_DIR}/uploads 或 ${VOLO_DATA_DIR}/geoip)`
  > 仅显式 CLI 参数会锁定路径；系统环境变量或 `.env` 中的 `VOLO_UPLOAD_DIR` / `VOLO_GEOIP_DIR` 作为默认值，管理员仍可在管理台在线热切换（清空管理台设置后回退到该默认值）。
- **首次建站向导数据库持久化（`data/db.json`）**：
  在 `.env` 与 `docker-compose.yml` 中**默认不设置（注释留空）`VOLO_DB_DRIVER` 与 `VOLO_DB_DSN`**，可让 `/setup` 建站向导所选数据库配置（SQLite / PostgreSQL / MySQL，DSN 经 `AES-256-GCM` 密封保存于 `${VOLO_DATA_DIR}/db.json`）在容器重建与服务重启后自动生效；若显式设置了 `VOLO_DB_DRIVER` 或 `VOLO_DB_DSN`，则会覆盖 `db.json`。

### 1.2 环境变量与 CLI 参数全表

| 环境变量 | CLI 参数 | 默认值 | 说明 |
| :--- | :--- | :--- | :--- |
| `VOLO_ENV_FILE` | `--env-file` | `.env` | `.env` 配置文件路径（服务模式下通常为 `/etc/volo/volo.env`） |
| `VOLO_LISTEN_ADDR` | `--listen-addr` | `0.0.0.0:8080` | TCP 监听地址（本机反代部署推荐设为 `127.0.0.1:8080`） |
| `VOLO_LISTEN_SOCKET` | `--listen-socket`（别名 `--unix-socket`） | *(空)* | Unix Domain Socket 路径（支持 `/run/volo/volo.sock` 或 `/run/volo/volo.sock,0660`；自动校验父目录真实路径、拒绝符号链接并加 `.sock.lock` 锁） |
| `VOLO_SOCKET_MODE` | `--socket-mode` | `0660` | Unix Domain Socket 八进制权限位 |
| `VOLO_SOCKET_GROUP` | `--socket-group` | *(空)* | Unix Domain Socket 所属系统用户组（如 Debian/Ubuntu 的 `www-data` 或 RHEL 的 `nginx`） |
| `VOLO_PUBLIC_URL` | `--public-url` | `http://localhost:8080` | 站点规范公开 URL（用于 Canonical、RSS/Podcast、Sitemap、WebAuthn Origin 与邮件链接） |
| `VOLO_TRUSTED_PROXIES` | `--trusted-proxies` | `127.0.0.1,::1` | 受信反向代理 IP/CIDR 白名单或 `unix`（激活 UDS 时默认自动信任本地 `unix` 连接） |
| `VOLO_ADMIN_PATH` | `--admin-path` | `/console` | 隐身管理台基础前缀（亦可在管理台通过 `security.admin_path` / `dynamic_login_path` 免重启热切换；自定义后访问 `/console`、`/login`、`/admin` 返回无特征 `404`） |
| `VOLO_SECRET_KEY` | `--secret-key` | *(自动生成)* | 主密钥（建议 `>= 32` 字符高熵随机串；留空时自动生成 `0600` 权限 `${VOLO_DATA_DIR}/.secret.key`） |
| `VOLO_DB_DRIVER` | `--db-driver` | *(留空读取 `db.json`，回退 `sqlite`)* | 数据库驱动：`sqlite`、`postgres`、`mysql`（建议留空以保留 `/setup` 向导的 `data/db.json` 选择） |
| `VOLO_DB_DSN` | `--db-dsn` | `${VOLO_DATA_DIR}/volo.db` | 数据库连接串（SQLite 文件路径、PostgreSQL URI 或 MySQL DSN） |
| `VOLO_DB_SIMPLE_PROTOCOL` | `--db-simple-protocol` | `false` | 为 PostgreSQL 事务连接池（Supabase / PgBouncer）开启纯 Go `pgwire` Simple Query 协议 |
| `VOLO_DB_MAX_OPEN_CONNS` | — | `25` | 最大打开数据库连接数（有效范围 `1`–`500`；SQLite 写池恒定自动锁定为 `1`） |
| `VOLO_DB_MAX_IDLE_CONNS` | — | `5` | 最大空闲数据库连接数（有效范围 `0`–`500`） |
| `VOLO_DB_CONN_MAX_LIFETIME_SEC` | — | `1800` | 数据库连接最大存活秒数（有效范围 `10`–`86400`） |
| `VOLO_GEO_ACCESS_ENABLED` | `--geo-access-enabled` | `true` | 离线 GeoIP 区域访问控制总开关（误封锁死时设为 `false` 即可免删库规则紧急旁路恢复） |
| `VOLO_SHUTDOWN_TIMEOUT_SEC` | `--shutdown-timeout` | `15` | 优雅停机排空在途请求与刷写异步统计的超时秒数（有效范围 `1`–`120`） |
| `VOLO_TLS_CERT_FILE` | `--tls-cert` | *(空)* | 原生 HTTPS 证书链（PEM）。与 `VOLO_TLS_KEY_FILE` 同时设置后在 `VOLO_LISTEN_ADDR` 上启用 TLS（ALPN 协商 HTTP/2，文件变更自动热重载）；不能与 `VOLO_LISTEN_SOCKET` 同时使用 |
| `VOLO_TLS_KEY_FILE` | `--tls-key` | *(空)* | 原生 HTTPS 私钥（PEM），须与 `VOLO_TLS_CERT_FILE` 成对设置，且服务用户可读 |
| `VOLO_TLS_MIN_VERSION` | `--tls-min-version` | `1.2` | 最低 TLS 协议版本：`1.2` 或 `1.3` |
| `VOLO_TLS_HTTP_REDIRECT_ADDR` | `--tls-http-redirect-addr` | *(空)* | 可选的额外明文 HTTP 监听地址（如 `:80`），所有请求以 `308` 跳转到 `VOLO_PUBLIC_URL` 主机的 `https://`；需先启用原生 TLS |
| `VOLO_DEFAULT_LOCALE` | `--default-locale` | `zh-CN` | 默认回退界面语言（内置 `zh-CN`、`en-US`，可通过 `VOLO_LOCALES_DIR` 扩展） |
| `VOLO_DATA_DIR` | `--data-dir` | `./data` | 核心运行时数据目录（`volo.db`、`db.json`、`.secret.key`、日志） |
| `VOLO_UPLOAD_DIR` | `--upload-dir` | `${VOLO_DATA_DIR}/uploads` | 本地媒体上传存储目录（支持 5 层优先级与管理台 `storage.local_dir` 热重载） |
| `VOLO_BACKUP_DIR` | `--backup-dir` | `${VOLO_DATA_DIR}/backups` | 本地定时与手动备份归档目录 |
| `VOLO_GEOIP_DIR` | `--geoip-dir` | `${VOLO_DATA_DIR}/geoip` | 离线 GeoIP 多库共存目录（支持同目录放置 `GeoLite2-City.mmdb`、`GeoLite2-Country.mmdb`、`GeoLite2-ASN.mmdb` 及 `ip2region_v4.xdb` / `ip2region_v6.xdb`，支持 `security.geoip_dir` 热重载） |
| `VOLO_THEMES_DIR` | `--themes-dir` | `./themes` | 外部主题热挂载目录 |
| `VOLO_PLUGINS_DIR` | `--plugins-dir` | `./plugins` | 外部插件热挂载目录 |
| `VOLO_LOCALES_DIR` | `--locales-dir` | `./locales` | 外部多语言覆盖包目录 |

### 1.3 原生 HTTPS（可选）

生产环境仍推荐使用反向代理（Nginx / Caddy，参见 [反向代理指南](./reverse-proxy.md)）。小型单机部署也可由 Volo-go 直接终止 TLS：

```ini
# /etc/volo/volo.env
VOLO_LISTEN_ADDR=0.0.0.0:443
VOLO_PUBLIC_URL=https://blog.example.com
VOLO_TLS_CERT_FILE=/etc/volo/tls/fullchain.pem
VOLO_TLS_KEY_FILE=/etc/volo/tls/privkey.pem
# VOLO_TLS_MIN_VERSION=1.3
VOLO_TLS_HTTP_REDIRECT_ADDR=:80
```

- 启动时校验证书与私钥（错误立即失败退出）；续期后的文件会被自动加载（最多每 30 秒检测一次），续期文件写入不完整时继续使用上一份有效证书。
- 通过 ALPN 协商 HTTP/2（`h2`，自动回退 HTTP/1.1）。跳转监听的目标地址取自 `VOLO_PUBLIC_URL`（`https://` 公开地址中显式写明的端口优先，否则使用 TLS 监听端口），从不使用客户端 `Host` 头。
- 以非特权 `volo` 用户监听 1024 以下端口需要 `CAP_NET_BIND_SERVICE`：执行 `sudo systemctl edit volo` 添加 `[Service]` 与 `AmbientCapabilities=CAP_NET_BIND_SERVICE`，然后 `sudo systemctl restart volo`。非 systemd 场景也可使用 `sudo setcap 'cap_net_bind_service=+ep' /usr/local/bin/volo`（每次升级后需重新执行；`NoNewPrivileges=true` 下不生效）。
- 请将证书复制到服务用户可读的位置（如通过 acme.sh `--install-cert` / certbot `--deploy-hook` 复制到 `/etc/volo/tls/`，私钥 `chown root:volo` + `chmod 0640`），不要直接指向仅 root 可读的 `/etc/letsencrypt/live`。
- `volo doctor` 新增 `tls_certificate` 检查（不可读 / 不匹配 / 已过期 → 失败，剩余 `< 14` 天 → 警告）；`volo healthcheck` 会自动改用 `https://` 探测（仅回环地址探测时跳过证书校验）。

---

## 2. Linux 系统服务安装与自动回滚在线升级（`systemd` 与 `OpenRC`）

仓库内置跨平台服务管理脚本 [`scripts/install-service.sh`](../../scripts/install-service.sh)（同时保留 `install-service-linux.sh` / `install-service-macos.sh` 兼容入口）：
- **二进制与数据路径最小权限物理分离**：支持独立自定义 `--user`、`--group`、`--bin-dir`（或 `--prefix`）与 `--data-dir`，默认将可执行文件固定为 `root:root 0755` 只读保护，同时将数据目录收紧为 `volo:volo 0750`；
- **安全自动备份与探活失败自动回滚**：执行 `upgrade`（或 `update`）时，脚本会自动调用 `volo backup create` 创建升级前数据库快照并校准文件属主（可通过 `--skip-backup` 跳过）、将旧版二进制备份为 `volo.bak`、支持 `--mirror` 不可达自动回退官方 GitHub 源、强制校验 `SHA256SUMS.txt`，重启后运行只读 `volo healthcheck`——**一旦探活失败，立即自动还原 `volo.bak` 并恢复原服务**；
- **全平台原生适配**：支持 `systemd` 系统级（内置 `RuntimeDirectory=volo` 与动态 `ProtectHome`/`PrivateTmp` 计算）、`--user-mode` 非 root 用户级服务，以及 Alpine / Gentoo `OpenRC`。

### 2.1 一键安装、Unix Socket 模式与带回滚保障的在线更新

```bash
# 远程一行安装并启动开机自启服务（默认监听 127.0.0.1:8080）
curl -fsSL https://raw.githubusercontent.com/Willh92/Volo-go/main/scripts/install-service.sh | sudo sh

# 日常零参数在线升级（自动升级前备份 + 保留 volo.bak + healthcheck 失败自动回滚）
sudo ./scripts/install-service.sh upgrade
# 如已自行完成外部快照，可传 --skip-backup 跳过升级前备份，或通过 --mirror 指定下载镜像加速：
sudo ./scripts/install-service.sh upgrade --skip-backup --mirror https://ghfast.top

# 配合本机 Nginx 的零 TCP 开销 Unix Domain Socket 模式（支持自定义 --bin-dir 与 --data-dir 分离部署）
sudo ./scripts/install-service.sh install \
  --user volo --group volo \
  --bin-dir /usr/local/bin \
  --data-dir /var/lib/volo \
  --unix-socket /run/volo/volo.sock \
  --socket-mode 0660 \
  --socket-group www-data \
  --public-url https://blog.example.com

# 非 root 用户级服务（systemd --user）
./scripts/install-service.sh install --user-mode --port 8080
```

### 2.2 `systemd` 沙箱加固与自定义外部目录（`ReadWritePaths` 覆盖说明）

官方 [`deploy/systemd/volo.service`](../../deploy/systemd/volo.service) 默认开启 `ProtectSystem=strict`、`RuntimeDirectory=volo` 与 `ReadWritePaths=/var/lib/volo /run/volo` 内核级只读沙箱保护（安装脚本在自定义 `--data-dir` 或 `--unix-socket` 位于 `/home`、`/tmp` 时会自动调整沙箱指令并写入 `tmpfiles.d` 规则）。
如果您后续在管理台中将 `upload_dir`（`storage.local_dir`）或 `geoip_dir`（`security.geoip_dir`）热切换至 `/var/lib/volo` 之外的目录，可通过 `systemd` drop-in 覆盖文件授予读写权限：

```bash
sudo systemctl edit volo
```

填入以下内容后执行 `sudo systemctl daemon-reload && sudo systemctl restart volo`：

```ini
[Service]
ReadWritePaths=/custom/uploads /custom/geoip
# 若自定义目录位于 /home/... 下（默认被 ProtectHome=true 屏蔽），请同时加上：
# ProtectHome=read-only
```

### 2.3 `OpenRC` 服务说明与生命周期命令

Alpine Linux / Gentoo 模板位于 [`deploy/openrc/volo.initd`](../../deploy/openrc/volo.initd)，内置零 Shell 执行风险的 `load_volo_env()` 纯键值环境解析器，通过 `command_args="serve --env-file /etc/volo/volo.env"` 与 `pidfile="/run/volo/volo.pid"` 安全托管后台守护进程。

```bash
sudo ./scripts/install-service.sh status      # 查看运行状态
sudo ./scripts/install-service.sh logs        # 实时追踪运行日志
sudo ./scripts/install-service.sh restart     # 重启服务（亦支持 start / stop）
sudo /usr/local/bin/volo doctor --env-file /etc/volo/volo.env
sudo ./scripts/install-service.sh uninstall   # 卸载服务（默认保留数据；加 --purge-data 彻底清理）
```

---

## 3. macOS 系统服务安装与更新（`launchd`）

同一份 [`scripts/install-service.sh`](../../scripts/install-service.sh) 原生支持 Intel (`darwin/amd64`) 与 Apple Silicon (`darwin/arm64`)：非 `root` 执行自动注册为当前用户 `LaunchAgent`（免 `sudo`），`sudo` 执行（或加 `--system`）注册为开机自启 `LaunchDaemon`，并同样具备升级前备份、`volo.bak` 与 `volo healthcheck` 自动回滚保护：

```bash
# 模式 A：当前用户 LaunchAgent（免 sudo，安装至 ~/.local/bin/volo 与 ~/Library/Application Support/Volo）
./scripts/install-service.sh install --public-url https://blog.example.com

# 模式 B：系统级 LaunchDaemon（开机无需登录即后台启动）
sudo ./scripts/install-service.sh install --system --public-url https://blog.example.com

# 零参数在线升级（支持 --skip-backup）/ 状态 / 日志 / 卸载
./scripts/install-service.sh upgrade
./scripts/install-service.sh status
./scripts/install-service.sh logs
./scripts/install-service.sh uninstall
```

---

## 4. Windows 原生服务安装与自动回滚升级（`sc.exe` / `NSSM` / PowerShell）

在管理员权限 PowerShell 中运行 [`scripts/install-service.ps1`](../../scripts/install-service.ps1)（原生支持 `windows/amd64` 与 `windows/arm64` 自动识别）：
- **零第三方依赖原生 Windows 服务**：`volo.exe` 内置 `svc_windows.go` Windows Service Control Manager (SCM) 协议支持！安装脚本在 PATH 中检测到 `nssm.exe` 时兼容支持 NSSM，未安装 NSSM 时直接通过原生 `sc.exe` 注册标准 Windows 后台服务（受限环境下亦可平滑回退计划任务），并使用 `icacls.exe` 收紧 `C:\ProgramData\Volo` NTFS ACL 权限；
- **镜像加速自动回退与升级自动回滚**：支持 `-Mirror` 下载加速并在镜像不可达时自动回退官方 GitHub Releases 源；执行 `-Action Upgrade` 时自动执行升级前数据库备份（可通过 `-SkipBackup` 跳过）、保留 `volo.exe.bak`（`volo.bak`），并在启动后运行 `volo.exe healthcheck`——若探活失败自动回滚旧版二进制并重启原服务；支持 `-Action Logs` 直接查看服务运行日志。

```powershell
# 安装并启动 Windows 服务（支持 -Mirror "https://ghfast.top"、-Version <tag>、-BinaryPath .\volo.exe、-DryRun）
powershell -ExecutionPolicy Bypass -File .\scripts\install-service.ps1 `
  -Action Install `
  -HostAddr "127.0.0.1" -Port 8080 `
  -PublicUrl "https://blog.example.com" `
  -AdminPath "/studio-secret"

# 一键在线升级（含升级前自动备份、SHA-256 校验、volo.bak 保留与 healthcheck 失败自动回滚；可选 -SkipBackup）
powershell -ExecutionPolicy Bypass -File .\scripts\install-service.ps1 -Action Upgrade
powershell -ExecutionPolicy Bypass -File .\scripts\install-service.ps1 -Action Upgrade -SkipBackup -Mirror "https://ghfast.top"

# 查看服务日志 / 状态查询 / 重启 / 卸载
powershell -ExecutionPolicy Bypass -File .\scripts\install-service.ps1 -Action Logs
powershell -ExecutionPolicy Bypass -File .\scripts\install-service.ps1 -Action Status
powershell -ExecutionPolicy Bypass -File .\scripts\install-service.ps1 -Action Restart
powershell -ExecutionPolicy Bypass -File .\scripts\install-service.ps1 -Action Uninstall
```

---

## 5. Docker Compose 与多数据库引擎连接配置

### 5.1 Docker Compose（Distroless 非 root 镜像与持久化卷注意事项）

[`docker-compose.yml`](../../docker-compose.yml) 基于 `gcr.io/distroless/static-debian12:nonroot`（`UID/GID 65532`），内置免 Shell 探针 `HEALTHCHECK CMD ["/usr/local/bin/volo", "healthcheck"]`：

```bash
cp .env.example .env
# 编辑 .env 设置 VOLO_PUBLIC_URL（保持 VOLO_DB_DRIVER 留空即可通过 /setup 向导自由选库并持久化至 data/db.json）
docker compose up -d --build

# 如需连同容器化 PostgreSQL 16 或 MySQL 8.4 一并启动，可启用对应 profile：
docker compose --profile postgres up -d
# 或：docker compose --profile mysql up -d
```

- **默认命名卷（开箱免权限配置）**：`docker-compose.yml` 默认挂载 `volo_data:/app/data`，自动持久化 `volo.db`、`db.json`、`.secret.key`、`uploads/`、`backups/` 与 `geoip/`；
- **宿主机目录绑定挂载（Bind Mount）注意事项**：
  1. 若将 `volo_data:/app/data` 替换为宿主机目录 `./data:/app/data`，必须先将目录属主设为 Distroless `nonroot` 用户，否则容器将因无写权限启动失败：
     ```bash
     mkdir -p data && sudo chown -R 65532:65532 data
     ```
  2. **切勿挂载空的 `./themes` 或 `./plugins` 宿主机目录**至 `/app/themes` 或 `/app/plugins`，否则空目录会遮蔽镜像内预置的外部主题（如 `volo-journal`）与默认插件（如 `sensitive-words`）；如需安装大型独立发布插件（如 Live2D 看板娘 `plugin-kanbanniang-v2.0.0.zip`，详见 [官方插件目录](../../plugins/README.zh-CN.md)），可直接在管理台 **「外观与扩展 -> 插件扩展」** 上传 `.zip` 安装。

### 5.2 三大数据库引擎 DSN 写法（固定环境变量模式）

若不使用 `/setup` 向导的 `data/db.json` 动态选库，也可在 `.env` 中显式固定驱动与连接串：

1. **SQLite（默认推荐，单机数万篇文章零外部依赖）**：
   ```dotenv
   VOLO_DB_DRIVER=sqlite
   VOLO_DB_DSN=/var/lib/volo/data/volo.db
   ```
2. **PostgreSQL / Supabase / Neon / CockroachDB**：
   ```dotenv
   VOLO_DB_DRIVER=postgres
   VOLO_DB_DSN=postgres://volo:strong_password@127.0.0.1:5432/volo?sslmode=verify-full
   # 如对接 Supabase / PgBouncer 事务池模式，请同时开启：
   VOLO_DB_SIMPLE_PROTOCOL=true
   ```
3. **MySQL 8+ / MariaDB / TiDB**：
   ```dotenv
   VOLO_DB_DRIVER=mysql
   VOLO_DB_DSN=volo:strong_password@tcp(127.0.0.1:3306)/volo?charset=utf8mb4&parseTime=true&loc=UTC
   ```

> **下一步**：部署服务后，请阅读 [反向代理与 CDN 配置指南](./reverse-proxy.md) 完成 HTTPS、Unix Socket 组权限、真实 IP 防伪造透传与播客流媒体转发配置。
