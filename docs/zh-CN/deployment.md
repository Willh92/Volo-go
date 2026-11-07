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

### 2. Linux 系统服务安装与极简在线更新（`systemd` 系统/用户级与 `OpenRC`）

仓库内置统一跨平台服务管理器 [`scripts/install-service.sh`](../../scripts/install-service.sh)（同时保留 `install-service-linux.sh` / `install-service-macos.sh` 兼容入口）：
- **零参数心智负担**：执行 `install` 时若本地未找到二进制自动从 GitHub Releases 下载；执行 `upgrade`（或 `update`）时**无需附加任何参数**即可自动拉取最新发行版、强制校验 `SHA256SUMS.txt`、执行 `volo version` 预检、保留已有服务路径与 `/etc/volo/volo.env` 并平滑重启；
- **全套进阶能力**：支持 `systemd` 系统级与 `--user-mode` 非 root 用户级服务、Alpine/Gentoo `OpenRC`、Unix Domain Socket 权限组、内核级沙箱加固（`ProtectSystem=strict` / `NoNewPrivileges=true`）、`--mirror` 镜像加速、`--bin` / `--archive` 离线安装及 `--dry-run` 预演。

### 2.1 一键远程安装与日常零参数更新（推荐）

```bash
# 远程一行安装并启动开机自启服务（默认监听 127.0.0.1:8080）
curl -fsSL https://raw.githubusercontent.com/Willh92/Volo-go/main/scripts/install-service.sh | sudo sh

# 或传入公开域名与自定义隐身后台路径
curl -fsSL https://raw.githubusercontent.com/Willh92/Volo-go/main/scripts/install-service.sh \
  | sudo sh -s -- install --public-url https://blog.example.com --admin-path /studio-secret

# 日常一键在线更新（零附加参数，100% 保留 /etc/volo/volo.env 与 /var/lib/volo 数据）
sudo ./scripts/install-service.sh upgrade
# 或直接远程执行更新（可选追加 --version v1.0.0 或 --mirror https://ghfast.top）：
curl -fsSL https://raw.githubusercontent.com/Willh92/Volo-go/main/scripts/install-service.sh | sudo sh -s -- upgrade
```

### 2.2 进阶定制：Unix Domain Socket、非 root 用户级与离线安装

```bash
# 配合本机 Nginx / Caddy 的零 TCP 开销 Unix Domain Socket 模式
sudo ./scripts/install-service.sh install \
  --socket-group www-data \
  --supplementary-groups www-data \
  --unix-socket /run/volo/volo.sock \
  --unix-socket-perm 0660 \
  --public-url https://blog.example.com

# 非 root 当前用户级服务（systemd --user）
./scripts/install-service.sh install --user-mode --port 8080

# 指定本地编译二进制（--bin）或离线发行包（--archive），配合 --dry-run 预览生成的单元文件
./scripts/install-service.sh install --dry-run --bin ./volo --public-url https://blog.example.com
sudo ./scripts/install-service.sh upgrade --bin ./volo
```

### 2.3 服务全生命周期管理与健康诊断

```bash
sudo ./scripts/install-service.sh status      # 查看运行状态
sudo ./scripts/install-service.sh logs        # 实时追踪运行日志
sudo ./scripts/install-service.sh restart     # 重启服务（亦支持 start / stop）
sudo /usr/local/bin/volo doctor --env-file /etc/volo/volo.env
sudo ./scripts/install-service.sh uninstall   # 卸载服务（默认保留数据；加 --purge-data 彻底清理）
```

也可直接使用 [`deploy/systemd/volo.service`](../../deploy/systemd/volo.service) 或 [`deploy/openrc/volo.initd`](../../deploy/openrc/volo.initd) 手工定制。

---

## 3. macOS 系统服务安装与更新（`launchd`）

同一份 [`scripts/install-service.sh`](../../scripts/install-service.sh) 原生支持 Intel (`darwin/amd64`) 与 Apple Silicon (`darwin/arm64`)：非 `root` 执行自动注册为当前用户 `LaunchAgent`（免 `sudo`），`sudo` 执行（或加 `--system`）注册为开机自启 `LaunchDaemon`：

```bash
# 模式 A：当前用户 LaunchAgent（免 sudo，自动安装至 ~/.local/bin/volo 与 ~/Library/Application Support/Volo）
./scripts/install-service.sh install --public-url https://blog.example.com

# 模式 B：系统级 LaunchDaemon（开机无需登录即后台启动）
sudo ./scripts/install-service.sh install --system --public-url https://blog.example.com

# 零参数在线更新 / 状态 / 实时日志 / 卸载
./scripts/install-service.sh upgrade
./scripts/install-service.sh status
./scripts/install-service.sh logs
./scripts/install-service.sh uninstall
```

---

## 4. Windows 系统服务安装与更新（PowerShell）

在管理员权限 PowerShell 中运行 [`scripts/install-service.ps1`](../../scripts/install-service.ps1)（同时保留 `install-service-windows.ps1` 兼容入口）：
- 本地存在 `volo.exe` 时自动采用本地二进制，否则自动从 GitHub Releases 下载并强制校验 `SHA256SUMS.txt`；
- 若 PATH 中已安装 `nssm.exe` 自动注册标准 Windows 服务，否则零依赖注册为 `NT AUTHORITY\NetworkService` 最小权限开机自启计划任务，并通过 `icacls.exe` 锁死 `C:\ProgramData\Volo` 的 NTFS ACL。

```powershell
# 安装并启动自启服务（支持 -Version <tag>、-Mirror "https://ghfast.top"、-BinaryPath .\volo.exe、-DryRun）
powershell -ExecutionPolicy Bypass -File .\scripts\install-service.ps1 `
  -Action Install `
  -HostAddr "127.0.0.1" -Port 8080 `
  -PublicUrl "https://blog.example.com" `
  -AdminPath "/studio-secret"

# 零附加参数一键在线升级（自动停服解锁、预检 volo.exe version、原子替换并保留 volo.env 与 data\）
powershell -ExecutionPolicy Bypass -File .\scripts\install-service.ps1 -Action Upgrade

# 状态查询 / 重启 / 卸载
powershell -ExecutionPolicy Bypass -File .\scripts\install-service.ps1 -Action Status
powershell -ExecutionPolicy Bypass -File .\scripts\install-service.ps1 -Action Restart
powershell -ExecutionPolicy Bypass -File .\scripts\install-service.ps1 -Action Uninstall
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
