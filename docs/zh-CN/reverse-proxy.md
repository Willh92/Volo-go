# 反向代理与 CDN 配置指南

> English version: [Reverse Proxy & CDN Guide](../en-US/reverse-proxy.md)

生产环境强烈建议在 `Volo-go` 前置反向代理（Nginx、Caddy、Traefik、Apache 或 Cloudflare Tunnel）以终结 TLS 证书并开启 HTTP/2 & HTTP/3。所有可直接复制使用的配置文件均位于仓库 [`deploy/reverse-proxy/`](../../deploy/reverse-proxy/) 目录。

---

## 1. 架构选型：TCP 回环 vs Unix Domain Socket

| 接入模式 | `.env` 配置 | 适用场景 | 性能与安全优势 |
| :--- | :--- | :--- | :--- |
| **TCP 本机回环** | `VOLO_LISTEN_ADDR=127.0.0.1:8080`<br>`VOLO_LISTEN_SOCKET=` | Docker 跨容器网络、Windows、macOS、通用面板（1Panel / 宝塔） | 仅监听本机回环网卡，公网扫描器无法绕过反代直连 8080 端口 |
| **Unix Domain Socket (UDS)** | `VOLO_LISTEN_SOCKET=/run/volo/volo.sock`<br>`VOLO_SOCKET_MODE=0660`<br>`VOLO_SOCKET_GROUP=www-data` | Linux 同机部署 Nginx / Caddy 追求极致低延迟 | 绕过内核 TCP/IP 协议栈与临时端口开销，通过 POSIX 套接字权限（`0660`）与独立用户组（`www-data` / `nginx`）严格隔离访问，同时保持 `/var/lib/volo` 归属 `volo:volo` |

---

## 2. 核心安全必读：`VOLO_TRUSTED_PROXIES` 真实 IP 防伪造机制

许多应用盲目信任请求头中的第一个 `X-Forwarded-For` 或 `X-Real-IP`，攻击者只需伪造请求头即可轻松绕过登录限流、离线 GeoIP 与 WAF 封禁。

`Volo-go` 的 `security.ResolveClientIP` 采用严格的 **受信对端门禁 + 从右向左逐跳剥离白名单算法**：
1. 只有当直连对端命中 `VOLO_TRUSTED_PROXIES` CIDR 白名单（或激活 Unix Domain Socket 且策略包含 `unix`）时，才会解析转发头；
2. 解析 `X-Forwarded-For` 时**从最右侧向左侧逆序扫描**，跳过所有属于 `VOLO_TRUSTED_PROXIES` 的代理跳点，取**第一个不在受信代理网段内的外部地址**作为真实访客 IP（本机单层 Nginx/Caddy 反代亦可直接透传 `$remote_addr` 至 `X-Real-IP`）；
3. **严禁**在公网直接暴露端口时配置 `0.0.0.0/0`！

### 常见拓扑的 `VOLO_TRUSTED_PROXIES` 标准值

- **本机 Nginx / Caddy / Apache（TCP 回环）**：
  ```dotenv
  VOLO_TRUSTED_PROXIES=127.0.0.1/32,::1/128
  ```
- **本机 Nginx / Caddy（Unix Domain Socket 模式）**：
  ```dotenv
  VOLO_TRUSTED_PROXIES=127.0.0.1/32,::1/128,unix
  ```
- **Docker Compose 同网络容器反代（如 Traefik / Nginx Proxy Manager）**：
  ```dotenv
  VOLO_TRUSTED_PROXIES=127.0.0.1/32,::1/128,172.16.0.0/12
  ```
- **前置 Cloudflare CDN -> 本机 Nginx/Caddy -> Volo-go**：
  由于请求经过两跳代理（Cloudflare 边缘节点 + 本机反代），需同时信任本机回环与 Cloudflare 官方回源网段：
  ```dotenv
  VOLO_TRUSTED_PROXIES=127.0.0.1/32,::1/128,173.245.48.0/20,103.21.244.0/22,103.22.200.0/22,103.31.4.0/22,141.101.64.0/18,108.162.192.0/18,190.93.240.0/20,188.114.96.0/20,197.234.240.0/22,198.41.128.0/17,162.158.0.0/15,104.16.0.0/13,104.24.0.0/14,172.64.0.0/13,131.0.72.0/22,2400:cb00::/32,2606:4700::/32,2803:f800::/32,2405:b500::/32,2405:8100::/32,2a06:98c0::/29,2c0f:f248::/32
  ```

---

## 3. 各主流反向代理完整配置

### 3.1 Nginx（TCP 回环模式）

模板文件：[`deploy/reverse-proxy/nginx-tcp.conf`](../../deploy/reverse-proxy/nginx-tcp.conf)

关键配置要点：
1. `client_max_body_size 128m;`：允许上传高清图片、`<video>` 视频、播客音频（MP3/M4A）以及历史/跨库备份 ZIP 包；
2. `/uploads/` 路径设置 `proxy_buffering off;` 并透传 `Range` 与 `If-Range` 请求头，确保 `Volo-go` 的 `http.ServeContent` 零拷贝 `206 Partial Content` 音视频拖动进度条秒开且不占满反代磁盘缓冲；
3. 正确传递 `Host`、`X-Real-IP`、`X-Forwarded-For` 与 `X-Forwarded-Proto`。

```nginx
upstream volo_backend_tcp {
    server 127.0.0.1:8080;
    keepalive 32;
}

server {
    listen 443 ssl;
    listen [::]:443 ssl;
    http2 on;
    server_name blog.example.com;

    ssl_certificate     /etc/letsencrypt/live/blog.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/blog.example.com/privkey.pem;
    client_max_body_size 128m;

    location ^~ /uploads/ {
        proxy_pass              http://volo_backend_tcp;
        proxy_http_version      1.1;
        proxy_set_header        Connection "";
        proxy_set_header        Host              $host;
        proxy_set_header        X-Real-IP         $remote_addr;
        proxy_set_header        X-Forwarded-For   $proxy_add_x_forwarded_for;
        proxy_set_header        X-Forwarded-Proto $scheme;
        proxy_set_header        Range             $http_range;
        proxy_set_header        If-Range          $http_if_range;
        proxy_buffering         off;
    }

    location / {
        proxy_pass              http://volo_backend_tcp;
        proxy_http_version      1.1;
        proxy_set_header        Connection "";
        proxy_set_header        Host              $host;
        proxy_set_header        X-Real-IP         $remote_addr;
        proxy_set_header        X-Forwarded-For   $proxy_add_x_forwarded_for;
        proxy_set_header        X-Forwarded-Proto $scheme;
    }
}
```

### 3.2 Nginx（Unix Domain Socket 零 TCP 开销模式）

模板文件：[`deploy/reverse-proxy/nginx-unix-socket.conf`](../../deploy/reverse-proxy/nginx-unix-socket.conf)

1. 使用安装脚本通过 `--socket-mode` 与 `--socket-group` 将套接字组权限授予 Nginx（Debian/Ubuntu 为 `www-data`，RHEL/Rocky 为 `nginx`），同时保持 `/var/lib/volo` 与 `/etc/volo/volo.env` 隔离在 `volo:volo` 名下：
   ```bash
   sudo ./scripts/install-service.sh install \
     --unix-socket /run/volo/volo.sock \
     --socket-mode 0660 \
     --socket-group www-data \
     --public-url https://blog.example.com
   ```
   对应 `/etc/volo/volo.env` 配置项为：
   ```dotenv
   VOLO_LISTEN_SOCKET=/run/volo/volo.sock
   VOLO_SOCKET_MODE=0660
   VOLO_SOCKET_GROUP=www-data
   VOLO_PUBLIC_URL=https://blog.example.com
   VOLO_TRUSTED_PROXIES=127.0.0.1/32,::1/128,unix
   ```
2. Nginx `upstream` 指向 `server unix:/run/volo/volo.sock;` 即可。若出现 `502 Bad Gateway (Permission denied)`，请执行 `id www-data` 与 `ls -l /run/volo/volo.sock` 检查用户组权限及 SELinux 上下文。

### 3.3 Caddy v2（自动申请续期 HTTPS + HTTP/3）

模板文件：[`deploy/reverse-proxy/Caddyfile`](../../deploy/reverse-proxy/Caddyfile)

Caddy v2 默认自动透传标准 `X-Forwarded-For` 与 `X-Forwarded-Proto`，极简配置如下：

```caddyfile
blog.example.com {
    request_body {
        max_size 128MB
    }

    # TCP 模式：
    reverse_proxy 127.0.0.1:8080 {
        health_uri /healthz
        health_interval 30s
    }

    # 如使用 Unix Domain Socket，替换为：
    # reverse_proxy unix//run/volo/volo.sock
}
```

### 3.4 Traefik v3 与 Apache httpd 2.4

- **Traefik v3**：直接使用 [`deploy/reverse-proxy/traefik-dynamic.yml`](../../deploy/reverse-proxy/traefik-dynamic.yml)，确保容器网段（如 `172.16.0.0/12`）加入 `VOLO_TRUSTED_PROXIES`。
- **Apache httpd 2.4+**：启用模块 `sudo a2enmod proxy proxy_http headers ssl rewrite http2` 后加载 [`deploy/reverse-proxy/apache-vhost.conf`](../../deploy/reverse-proxy/apache-vhost.conf)，务必保留 `ProxyPreserveHost On` 与 `RequestHeader set X-Forwarded-Proto "https"`，否则 WebAuthn Passkey 与 `Secure` Cookie 校验会因协议不一致拒绝签发。

### 3.5 1Panel / 宝塔面板（Baota）反代注意事项

在可视化面板中创建“反向代理站点”指向 `http://127.0.0.1:8080` 时，请注意以下 3 点：
1. **关闭面板自带的强制全站静态 HTML 缓存**：`Volo-go` 已内置微秒级渲染缓存与 `Cache-Control: private, no-store` 加密文章保护头，若面板无视源站响应头强行缓存 `.html` 或 `/posts/*`，会导致密码解锁状态或动态隐身后台路径（`security.admin_path`）异常；
2. **调大上传限制**：在站点配置中将 `client_max_body_size` 调至 `128m`；
3. **WebAuthn 域名一致**：确保 `.env` 中的 `VOLO_PUBLIC_URL=https://你的域名` 与浏览器地址栏域名完全一致（含 `https://`）。

---

## 4. Cloudflare CDN 与 Cloudflare Tunnel (`cloudflared`) 最佳实践

1. **SSL/TLS 加密模式**：若使用 Nginx/Caddy，请在 Cloudflare 控制台将 SSL/TLS 模式设为 **Full (Strict)**（切勿使用 Flexible，否则会导致 HTTPS 重定向死循环）。
2. **Cloudflare Tunnel 零公网端口部署**：
   - `Volo-go` 监听 `127.0.0.1:8080`，`cloudflared` 同机指向 `http://127.0.0.1:8080`；
   - `.env` 设置 `VOLO_PUBLIC_URL=https://blog.example.com` 与 `VOLO_TRUSTED_PROXIES=127.0.0.1/32,::1/128`。
3. **CDN 边缘缓存规则建议（Cache Rules）**：
   - **允许边缘缓存**：`/uploads/*`、`/themes/*`、`/plugins/*`、`/avatars/*`（尊重源站 `Cache-Control` 与 `ETag`）；
   - **遵循源站协商缓存**：`/rss.xml`、`/atom.xml`、`/podcast.xml`、`/sitemap.xml` 及自动分片 `/sitemap-*.xml`（源站自带强 `ETag` 与 `max-age=60`）；
   - **必须绕过边缘缓存（Bypass Cache）**：`/api/*`、`/setup*`、`VOLO_ADMIN_PATH` / 动态隐身后台前缀，以及任何携带 `?preview=` 参数的请求（`Volo-go` 源站已自动对私密/解锁页面下发 `Cache-Control: private, no-store`）。

---

## 5. 反代连通性一键自检清单

配置完成后，可在终端依次执行以下 4 条命令验证反代链路完全健康：

```bash
# 1. 验证健康探针与数据库就绪状态（返回 {"status":"ok","database":"ok","uptime_sec":...}）
curl -fsS https://blog.example.com/healthz

# 2. 验证公开首页 Gzip 压缩与 Vary 协商头
curl -sI -H "Accept-Encoding: gzip" https://blog.example.com/ | grep -iE "content-encoding|vary"

# 3. 验证订阅源强 ETag 与 304 零流量命中
ETAG=$(curl -sI https://blog.example.com/rss.xml | awk -F': ' 'tolower($1)=="etag"{print $2}' | tr -d '\r')
curl -s -o /dev/null -w "%{http_code}\n" -H "If-None-Match: ${ETAG}" https://blog.example.com/rss.xml

# 4. 运行内置医生命令检查配置、GeoIP 多库挂载、权限与安全告警
volo doctor --env-file /etc/volo/volo.env
```
