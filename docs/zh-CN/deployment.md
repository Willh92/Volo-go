# 部署与各系统服务安装指南

> English version: [Deployment & OS Service Installers](../en-US/deployment.md)

`Volo-go` 编译产物为单个零外部依赖静态二进制文件（`CGO_ENABLED=0`），支持直接在 **Linux（`systemd` / `OpenRC`）、macOS（`launchd`）、Windows（`NSSM` / 系统计划任务）** 以及 **Docker / Kubernetes** 中投产运行。

---

## 1. 四层 `.env` 配置优先级与完整变量表

配置严格遵循以下四层覆盖顺序（高优先级覆盖低优先级）：

`CLI 命令行参数 (--listen-addr 等) > 系统环境变量 (VOLO_*) > .env 文件 > 内置安全默认值`

| 环境变量 | CLI 参数 | 默认值 | 说明 |
| :--- | :--- | :--- | :--- |
| `VOLO_LISTEN_ADDR` | `--listen-addr` | `0.0.0.0:8080` | TCP 监听地址（反代本机部署推荐设为 `127.0.0.1:8080`） |
| `VOLO_LISTEN_SOCKET` | `--listen-socket` | *(空)* | Unix Domain Socket 路径（如 `/run/volo/volo.sock`，设置后优先于 TCP） |
| `VOLO_LISTEN_SOCKET_MODE` | `--listen-socket-mode` | `0660` | Unix Socket 权限位（八进制） |
| `VOLO_PUBLIC_URL` | `--public-url` | `http://localhost:8080` | 站点规范公开 URL（用于 Canonical、RSS、Sitemap、WebAuthn RP ID 与安全 Cookie） |
| `VOLO_TRUSTED_PROXIES` | `--trusted-proxies` | `127.0.0.1/32,::1/128` | 受信反向代理 IP/CIDR 白名单（逗号分隔，防伪造 `X-Forwarded-For`） |
| `VOLO_ADMIN_PATH` | `--admin-path` | `/console` | 自定义隐身管理台路径前缀（修改后访问默认 `/console` 直接返回无特征 `404`） |
| `VOLO_SECRET_KEY` | `--secret-key` | *(自动生成)* | 主密钥（建议 `>= 32` 字节随机串；未设置时自动在 `VOLO_DATA_DIR/.secret.key` 生成 `0600` 密钥文件） |
| `VOLO_DB_DRIVER` | `--db-driver` | `sqlite` | 数据库驱动：`sqlite`、`postgres`、`mysql` |
| `VOLO_DB_DSN` | `--db-dsn` | `./data/volo.db` | 数据库连接串（支持 SQLite 文件路径、PostgreSQL URI、MySQL DSN） |
| `VOLO_DB_MAX_OPEN_CONNS` | — | `25` | 数据库最大打开连接数（SQLite 写池恒定自动锁定为 `1`） |
| `VOLO_DB_MAX_IDLE_CONNS` | — | `10` | 数据库最大空闲连接数 |
| `VOLO_GEO_ACCESS_ENABLED` | `--geo-access-enabled` | `true` | GeoIP 区域访问控制总开关（误封锁死时设为 `false` 重启即可紧急熔断恢复） |
| `VOLO_SHUTDOWN_TIMEOUT` | `--shutdown-timeout` | `15s` | 优雅停机排空连接与刷写缓存的超时时长 |
| `VOLO_DEFAULT_LOCALE` | `--default-locale` | `zh-CN` | 默认界面语言（`zh-CN` 或 `en-US`） |
| `VOLO_DATA_DIR` | `--data-dir` | `./data` | 核心数据目录 |
| `VOLO_UPLOAD_DIR` | `--upload-dir` | `./data/uploads` | 本地媒体上传存储目录 |
| `VOLO_BACKUP_DIR` | `--backup-dir` | `./data/backups` | 自动与手动备份归档目录 |
| `VOLO_GEOIP_DIR` | `--geoip-dir` | `./data/geoip` | 离线 GeoIP 数据库目录（放入 `GeoLite2-City.mmdb` 或 `ip2region.xdb` 自动生效） |
| `VOLO_PLUGINS_DIR` | `--plugins-dir` | `./plugins` | 外部插件挂载目录 |
| `VOLO_THEMES_DIR` | `--themes-dir` | `./themes` | 外部主题挂载目录 |
| `VOLO_LOCALES_DIR` | `--locales-dir` | `./locales` | 外部多语言覆盖包目录 |

---

## 2. Linux 系统服务安装（`systemd` 与 `OpenRC`）

仓库内置 [`scripts/install-service-linux.sh`](../../scripts/install-service-linux.sh)，自动识别 Debian / Ubuntu / RHEL / Rocky / Arch（`systemd`）以及 Alpine / Gentoo（`OpenRC`），自动创建无登录权限的 `volo` 系统账户、初始化 `0750` 数据目录、生成随机 256-bit 主密钥写入 `0640` `/etc/volo/volo.env`，并开启 `ProtectSystem=strict` / `NoNewPrivileges=true` 等内核级沙箱加固。

### 2.1 标准 TCP 回环模式（配合本机 Nginx / Caddy / Traefik）

```bash
# 先用 --dry-run 预览将要执行的命令与生成的单元文件（无需 root）
bash scripts/install-service-linux.sh install \
  --dry-run \
  --binary ./volo \
  --listen-addr 127.0.0.1:8080 \
  --public-url https://blog.example.com \
  --admin-path /studio-secret

# 确认无误后正式安装并启动开机自启服务
sudo bash scripts/install-service-linux.sh install \
  --binary ./volo \
  --listen-addr 127.0.0.1:8080 \
  --public-url https://blog.example.com \
  --admin-path /studio-secret
```

### 2.2 零 TCP 开销 Unix Domain Socket 模式（配合本机 Nginx / Caddy）

将服务运行组指定为反向代理所在用户组（Debian/Ubuntu 通常为 `www-data`，RHEL/Rocky/Alpine 通常为 `nginx` 或 `caddy`）：

```bash
sudo bash scripts/install-service-linux.sh install \
  --binary ./volo \
  --group www-data \
  --listen-socket /run/volo/volo.sock \
  --socket-mode 0660 \
  --public-url https://blog.example.com
```

### 2.3 日常升级、状态检查与卸载

```bash
# 平滑升级二进制并重启服务（完整保留 /etc/volo/volo.env 与 /var/lib/volo 数据）
sudo bash scripts/install-service-linux.sh upgrade --binary ./volo

# 查看运行状态与健康诊断
sudo bash scripts/install-service-linux.sh status
sudo /usr/local/bin/volo doctor --env-file /etc/volo/volo.env

# 卸载服务（默认保留数据目录；如确认彻底清空数据可追加 --purge-data）
sudo bash scripts/install-service-linux.sh uninstall
```

也可直接使用 [`deploy/systemd/volo.service`](../../deploy/systemd/volo.service) 或 [`deploy/openrc/volo.initd`](../../deploy/openrc/volo.initd) 手工定制。

---

## 3. macOS 系统服务安装（`launchd`）

[`scripts/install-service-macos.sh`](../../scripts/install-service-macos.sh) 原生支持 Intel (`darwin/amd64`) 与 Apple Silicon (`darwin/arm64`)，提供两种模式：

```bash
# 模式 A：当前用户 LaunchAgent（推荐 Mac mini 家庭服务器，免 sudo，登录后常驻自启）
bash scripts/install-service-macos.sh install \
  --binary ./volo \
  --listen-addr 127.0.0.1:8080 \
  --public-url https://blog.example.com

# 模式 B：系统级 LaunchDaemon（开机无需用户登录即在后台启动，需 sudo）
sudo bash scripts/install-service-macos.sh install --system \
  --binary ./volo \
  --listen-addr 127.0.0.1:8080 \
  --public-url https://blog.example.com

# 查看状态 / 平滑升级 / 卸载
bash scripts/install-service-macos.sh status
bash scripts/install-service-macos.sh upgrade --binary ./volo
bash scripts/install-service-macos.sh uninstall
```

---

## 4. Windows 系统服务安装（PowerShell）

在管理员权限 PowerShell 中运行 [`scripts/install-service-windows.ps1`](../../scripts/install-service-windows.ps1)：
- 若系统 PATH 中已安装 `nssm.exe`，自动注册为标准 Windows 服务（`SERVICE_AUTO_START`）；
- 若未安装 `nssm.exe`，零依赖自动注册为 `NT AUTHORITY\NETWORK SERVICE` 最小权限开机自启计划任务（异常退出自动拉起），并通过 `icacls.exe` 锁死 `C:\ProgramData\Volo\volo.env` 的 NTFS 读取权限。

```powershell
# 预览安装步骤（DryRun）
powershell -ExecutionPolicy Bypass -File .\scripts\install-service-windows.ps1 `
  -Action Install -BinaryPath .\volo.exe -DryRun

# 正式安装并启动服务（默认监听 127.0.0.1:8080 供 IIS / Caddy / Nginx 反代）
powershell -ExecutionPolicy Bypass -File .\scripts\install-service-windows.ps1 `
  -Action Install `
  -BinaryPath .\volo.exe `
  -ListenAddr "127.0.0.1:8080" `
  -PublicUrl "https://blog.example.com" `
  -AdminPath "/studio-secret"

# 查询状态 / 平滑升级 / 卸载
powershell -ExecutionPolicy Bypass -File .\scripts\install-service-windows.ps1 -Action Status
powershell -ExecutionPolicy Bypass -File .\scripts\install-service-windows.ps1 -Action Upgrade -BinaryPath .\volo.exe
powershell -ExecutionPolicy Bypass -File .\scripts\install-service-windows.ps1 -Action Uninstall
```

---

## 5. Docker 与多数据库引擎连接示例

### 5.1 Docker Compose（Distroless 非 root 镜像）

```bash
cp .env.example .env
# 编辑 .env 设置 VOLO_PUBLIC_URL 与 VOLO_ADMIN_PATH
docker compose up -d --build
```

容器镜像基于 `gcr.io/distroless/static-debian12:nonroot`（UID `65532`），内置 `HEALTHCHECK CMD ["/usr/local/bin/volo", "healthcheck"]`，无需 `curl` 或 `sh` 即可原生探活。

### 5.2 三大数据库引擎 DSN 写法

1. **SQLite（默认推荐，单机数万篇文章零运维）**：
   ```dotenv
   VOLO_DB_DRIVER=sqlite
   VOLO_DB_DSN=/var/lib/volo/data/volo.db
   ```
2. **PostgreSQL / Supabase / Neon / CockroachDB**：
   ```dotenv
   VOLO_DB_DRIVER=postgres
   VOLO_DB_DSN=postgres://volo:strong_password@127.0.0.1:5432/volo?sslmode=verify-full
   ```
3. **MySQL 8+ / MariaDB / TiDB**：
   ```dotenv
   VOLO_DB_DRIVER=mysql
   VOLO_DB_DSN=volo:strong_password@tcp(127.0.0.1:3306)/volo?charset=utf8mb4&parseTime=true&loc=UTC
   ```

> **下一步**：部署服务后，请阅读 [反向代理与 CDN 配置指南](./reverse-proxy.md) 完成 HTTPS、真实 IP 透传与音频流媒体转发配置。
