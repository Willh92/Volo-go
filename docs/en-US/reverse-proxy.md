# Reverse Proxy & CDN Guide

> 中文版本：[反向代理与 CDN 配置指南](../zh-CN/reverse-proxy.md)

For production deployments, place `Volo-go` behind a TLS-terminating reverse proxy (Nginx, Caddy, Traefik, Apache httpd, or Cloudflare Tunnel) to serve HTTPS with HTTP/2 & HTTP/3. Ready-to-use configuration templates are located in [`deploy/reverse-proxy/`](../../deploy/reverse-proxy/).

---

## 1. Transport Choice: TCP Loopback vs. Unix Domain Socket

| Mode | `.env` Settings | Best For | Security & Performance Advantages |
| :--- | :--- | :--- | :--- |
| **TCP Loopback** | `VOLO_LISTEN_ADDR=127.0.0.1:8080`<br>`VOLO_LISTEN_SOCKET=` | Docker networks, Windows, macOS, control panels (1Panel / Nginx Proxy Manager) | Binds strictly to loopback so external scanners cannot bypass your reverse proxy on port `8080` |
| **Unix Domain Socket (UDS)** | `VOLO_LISTEN_SOCKET=/run/volo/volo.sock`<br>`VOLO_LISTEN_SOCKET_MODE=0660` | Same-host Linux Nginx / Caddy deployments | Eliminates TCP/IP stack overhead and ephemeral port exhaustion; enforces POSIX group permissions (`0660`) |

---

## 2. Critical Security: `VOLO_TRUSTED_PROXIES` Anti-Spoofing

Naive applications trust the leftmost IP in `X-Forwarded-For`, allowing attackers to spoof `X-Forwarded-For: 1.2.3.4` and trivially bypass login rate limits and IP bans.

`Volo-go` (`security.ResolveClientIP`) implements **right-to-left hop stripping**:
1. Forwarding headers are inspected **only** when the immediate peer IP matches `VOLO_TRUSTED_PROXIES` (or connects over a local Unix Domain Socket);
2. `X-Forwarded-For` is scanned **from rightmost to leftmost**, stripping hops inside `VOLO_TRUSTED_PROXIES` and selecting the **first untrusted external IP** as the true client IP;
3. **Never** set `VOLO_TRUSTED_PROXIES=0.0.0.0/0` on an internet-exposed listener!

### Standard `VOLO_TRUSTED_PROXIES` Values by Topology

- **Local Nginx / Caddy / Apache directly facing visitors**:
  ```dotenv
  VOLO_TRUSTED_PROXIES=127.0.0.1/32,::1/128
  ```
- **Docker Compose reverse proxy container (Traefik / Caddy / NPM)**:
  ```dotenv
  VOLO_TRUSTED_PROXIES=127.0.0.1/32,::1/128,172.16.0.0/12
  ```
- **Cloudflare CDN -> Local Nginx/Caddy -> Volo-go (Two Proxy Hops)**:
  Trust both local loopback and Cloudflare's published edge CIDRs so right-to-left traversal resolves the real visitor IP:
  ```dotenv
  VOLO_TRUSTED_PROXIES=127.0.0.1/32,::1/128,173.245.48.0/20,103.21.244.0/22,103.22.200.0/22,103.31.4.0/22,141.101.64.0/18,108.162.192.0/18,190.93.240.0/20,188.114.96.0/20,197.234.240.0/22,198.41.128.0/17,162.158.0.0/15,104.16.0.0/13,104.24.0.0/14,172.64.0.0/13,131.0.72.0/22,2400:cb00::/32,2606:4700::/32,2803:f800::/32,2405:b500::/32,2405:8100::/32,2a06:98c0::/29,2c0f:f248::/32
  ```

---

## 3. Production Reverse Proxy Configurations

### 3.1 Nginx (TCP Loopback Mode)

Template: [`deploy/reverse-proxy/nginx-tcp.conf`](../../deploy/reverse-proxy/nginx-tcp.conf)

Key settings:
1. `client_max_body_size 128m;` to permit high-res photos, podcast episodes (MP3/M4A), and backup `.zip` uploads;
2. `proxy_buffering off;` + `Range` / `If-Range` pass-through on `/uploads/` so `http.ServeContent` delivers instant zero-copy `206 Partial Content` audio scrubbing without buffering large media files onto Nginx's disk;
3. Standard `Host`, `X-Real-IP`, `X-Forwarded-For`, and `X-Forwarded-Proto` headers.

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

### 3.2 Nginx (Unix Domain Socket Mode)

Template: [`deploy/reverse-proxy/nginx-unix-socket.conf`](../../deploy/reverse-proxy/nginx-unix-socket.conf)

1. Install the service sharing Nginx's group (`www-data` on Debian/Ubuntu, `nginx` on RHEL/Rocky):
   ```bash
   sudo bash scripts/install-service-linux.sh install \
     --group www-data \
     --listen-socket /run/volo/volo.sock \
     --socket-mode 0660 \
     --public-url https://blog.example.com
   ```
2. Point Nginx's upstream to `server unix:/run/volo/volo.sock;`. If you encounter `502 Bad Gateway (Permission denied)`, check `id www-data` and `ls -l /run/volo/volo.sock`.

### 3.3 Caddy v2 (Automatic HTTPS + HTTP/3)

Template: [`deploy/reverse-proxy/Caddyfile`](../../deploy/reverse-proxy/Caddyfile)

Caddy v2 automatically forwards `X-Forwarded-For` and `X-Forwarded-Proto`:

```caddyfile
blog.example.com {
    request_body {
        max_size 128MB
    }

    # TCP Loopback:
    reverse_proxy 127.0.0.1:8080 {
        health_uri /healthz
        health_interval 30s
    }

    # Or Unix Domain Socket:
    # reverse_proxy unix//run/volo/volo.sock
}
```

### 3.4 Traefik v3 & Apache httpd 2.4

- **Traefik v3**: Use [`deploy/reverse-proxy/traefik-dynamic.yml`](../../deploy/reverse-proxy/traefik-dynamic.yml) and include your Docker bridge CIDR (`172.16.0.0/12`) in `VOLO_TRUSTED_PROXIES`.
- **Apache httpd 2.4+**: Enable modules via `sudo a2enmod proxy proxy_http headers ssl rewrite http2` and load [`deploy/reverse-proxy/apache-vhost.conf`](../../deploy/reverse-proxy/apache-vhost.conf). Keep `ProxyPreserveHost On` and `RequestHeader set X-Forwarded-Proto "https"` so WebAuthn Passkeys and `Secure` session cookies validate the origin cleanly.

### 3.5 Web Control Panels (1Panel / Nginx Proxy Manager / Plesk)

When creating a reverse proxy site pointing to `http://127.0.0.1:8080`:
1. **Disable forced full-page HTML caching** in the panel: `Volo-go` already serves microsecond in-memory caches and emits `Cache-Control: private, no-store` on password-protected posts. Overriding upstream `Cache-Control` headers can leak or break unlock states;
2. Increase max upload body size (`client_max_body_size`) to `128m`;
3. Ensure `VOLO_PUBLIC_URL=https://your-domain` matches the exact browser origin.

---

## 4. Cloudflare CDN & Cloudflare Tunnel (`cloudflared`) Best Practices

1. **SSL/TLS Mode**: Set Cloudflare SSL/TLS to **Full (Strict)** when terminating TLS on Nginx/Caddy (avoid `Flexible` to prevent HTTPS redirect loops).
2. **Zero-Inbound-Port Cloudflare Tunnel**:
   - Bind `Volo-go` to `127.0.0.1:8080` and point local `cloudflared` to `http://127.0.0.1:8080`;
   - Set `VOLO_PUBLIC_URL=https://blog.example.com` and `VOLO_TRUSTED_PROXIES=127.0.0.1/32,::1/128`.
3. **Recommended CDN Cache Rules**:
   - **Cache Eligible**: `/uploads/*`, `/themes/*`, `/avatars/*` (respect origin `Cache-Control` and `ETag`);
   - **Revalidate via Origin `ETag`**: `/rss.xml`, `/atom.xml`, `/podcast.xml`, `/sitemap.xml`;
   - **Bypass Cache**: `/api/*`, `/setup*`, your custom `VOLO_ADMIN_PATH` prefix, and any `?preview=` query URLs.

---

## 5. Post-Deployment Verification Checklist

Run these four checks to verify your reverse proxy pipeline end-to-end:

```bash
# 1. Verify /healthz liveness and read-only DB probe
curl -fsS https://blog.example.com/healthz

# 2. Verify BREACH-safe Gzip compression and Vary negotiation on public HTML
curl -sI -H "Accept-Encoding: gzip" https://blog.example.com/ | grep -iE "content-encoding|vary"

# 3. Verify strong ETag and zero-body 304 Not Modified on RSS feeds
ETAG=$(curl -sI https://blog.example.com/rss.xml | awk -F': ' 'tolower($1)=="etag"{print $2}' | tr -d '\r')
curl -s -o /dev/null -w "%{http_code}\n" -H "If-None-Match: ${ETAG}" https://blog.example.com/rss.xml

# 4. Run built-in environment & security diagnostics
volo doctor --env-file /etc/volo/volo.env
```
