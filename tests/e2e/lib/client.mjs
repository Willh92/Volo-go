import crypto from "node:crypto";

const BASE32_ALPHABET = "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567";

function decodeBase32(input) {
  const clean = String(input || "")
    .toUpperCase()
    .replace(/[\s=-]+/g, "");
  let bits = 0;
  let value = 0;
  const out = [];

  for (let i = 0; i < clean.length; i += 1) {
    const idx = BASE32_ALPHABET.indexOf(clean[i]);
    if (idx === -1) {
      throw new Error(`Invalid Base32 character '${clean[i]}' in TOTP secret`);
    }
    value = (value << 5) | idx;
    bits += 5;
    if (bits >= 8) {
      bits -= 8;
      out.push((value >>> bits) & 0xff);
    }
  }
  return Buffer.from(out);
}

/**
 * Generate a 6-digit RFC 6238 TOTP code (HMAC-SHA1, 30s period).
 */
export function generateTOTP(secretBase32, offsetSteps = 0, nowMs = Date.now()) {
  const key = decodeBase32(secretBase32);
  const counter = BigInt(Math.floor(nowMs / 1000 / 30) + offsetSteps);
  const counterBuf = Buffer.alloc(8);
  counterBuf.writeBigUInt64BE(counter, 0);

  const hmac = crypto.createHmac("sha1", key).update(counterBuf).digest();
  const offset = hmac[hmac.length - 1] & 0x0f;
  const binary =
    ((hmac[offset] & 0x7f) << 24) |
    ((hmac[offset + 1] & 0xff) << 16) |
    ((hmac[offset + 2] & 0xff) << 8) |
    (hmac[offset + 3] & 0xff);
  return String(binary % 1_000_000).padStart(6, "0");
}

function countLeadingZeroBits(buf) {
  let total = 0;
  for (let i = 0; i < buf.length; i += 1) {
    const b = buf[i];
    if (b === 0) {
      total += 8;
      continue;
    }
    total += Math.clz32(b) - 24;
    break;
  }
  return total;
}

/**
 * Solve a Volo guest PoW challenge (SHA-256(challenge + ":" + nonce) >= difficultyBits leading zero bits).
 */
export function solvePoW(challenge, difficultyBits) {
  for (let nonce = 0; ; nonce += 1) {
    const cand = String(nonce);
    const digest = crypto
      .createHash("sha256")
      .update(`${challenge}:${cand}`)
      .digest();
    if (countLeadingZeroBits(digest) >= difficultyBits) {
      return cand;
    }
  }
}

function isPlainObjectOrArray(val) {
  if (val === null || typeof val !== "object") return false;
  if (Array.isArray(val)) return true;
  const proto = Object.getPrototypeOf(val);
  return proto === Object.prototype || proto === null;
}

function parseSetCookieLine(rawLine, cookieMap) {
  if (!rawLine) return;
  const segments = rawLine.split(";").map((s) => s.trim());
  const pair = segments[0];
  if (!pair) return;
  const eqIdx = pair.indexOf("=");
  if (eqIdx <= 0) return;

  const name = pair.slice(0, eqIdx).trim();
  const value = pair.slice(eqIdx + 1).trim();

  let expired = value === "";
  for (let i = 1; i < segments.length; i += 1) {
    const attr = segments[i];
    const attrEq = attr.indexOf("=");
    const attrKey = (attrEq >= 0 ? attr.slice(0, attrEq) : attr).trim().toLowerCase();
    const attrVal = attrEq >= 0 ? attr.slice(attrEq + 1).trim() : "";
    if (attrKey === "max-age") {
      const n = Number(attrVal);
      if (Number.isFinite(n) && n <= 0) {
        expired = true;
      }
    } else if (attrKey === "expires") {
      const ts = Date.parse(attrVal);
      if (Number.isFinite(ts) && ts <= Date.now()) {
        expired = true;
      }
    }
  }

  if (expired) {
    cookieMap.delete(name);
  } else {
    cookieMap.set(name, value);
  }
}

function isDelOptsObject(val) {
  if (!val || typeof val !== "object" || Array.isArray(val)) return false;
  const reqOptionKeys = [
    "headers",
    "omitCsrf",
    "omitCookies",
    "redirect",
    "bearer",
    "consoleHeader",
    "body",
    "signal",
  ];
  return reqOptionKeys.some((k) => k in val);
}

/**
 * Create an isolated HTTP client with cookie jar, pinned User-Agent,
 * CSRF token injection, Bearer PAT support, and Volo auth helpers.
 */
export function createClient(baseUrl, opts = {}) {
  const normalizedBaseUrl = String(baseUrl).replace(/\/+$/, "");
  const cookies = new Map();
  if (opts.cookies instanceof Map) {
    for (const [k, v] of opts.cookies.entries()) cookies.set(k, v);
  }

  let csrfToken = opts.csrfToken || "";
  let bearerToken = opts.bearerToken || "";
  const userAgent = opts.userAgent || "Volo-E2E-Client/1.0";
  const defaultHeaders = { ...(opts.headers || {}) };
  const defaultConsoleHeader = Boolean(opts.consoleHeader);

  function updateCookiesFromHeaders(headers) {
    if (!headers) return;
    const setCookies =
      typeof headers.getSetCookie === "function"
        ? headers.getSetCookie()
        : headers.get("set-cookie")
          ? [headers.get("set-cookie")]
          : [];
    for (const line of setCookies) {
      parseSetCookieLine(line, cookies);
    }
  }

  function serializeCookies() {
    if (cookies.size === 0) return "";
    return Array.from(cookies.entries())
      .map(([k, v]) => `${k}=${v}`)
      .join("; ");
  }

  async function request(method, reqPath, body = undefined, reqOpts = {}) {
    const upperMethod = String(method || "GET").toUpperCase();
    const targetUrl = /^https?:\/\//i.test(reqPath)
      ? reqPath
      : `${normalizedBaseUrl}${reqPath.startsWith("/") ? "" : "/"}${reqPath}`;

    const headers = new Headers(defaultHeaders);
    if (!headers.has("user-agent")) {
      headers.set("User-Agent", reqOpts.userAgent || userAgent);
    }

    if (reqOpts.headers) {
      const extra = new Headers(reqOpts.headers);
      extra.forEach((v, k) => headers.set(k, v));
    }

    const activeBearer = reqOpts.bearer !== undefined ? reqOpts.bearer : bearerToken;
    const usingBearer = Boolean(activeBearer);
    if (usingBearer && !headers.has("authorization")) {
      headers.set("Authorization", `Bearer ${activeBearer}`);
    }

    if (!reqOpts.omitCookies && !headers.has("cookie")) {
      const cookieStr = serializeCookies();
      if (cookieStr) {
        headers.set("Cookie", cookieStr);
      }
    }

    const wantConsole = reqOpts.consoleHeader ?? defaultConsoleHeader;
    if (wantConsole && !headers.has("x-volo-console")) {
      headers.set("X-Volo-Console", "1");
    }

    const isMutating = ["POST", "PUT", "PATCH", "DELETE"].includes(upperMethod);
    if (isMutating && !reqOpts.omitCsrf && !usingBearer && csrfToken && !headers.has("x-csrf-token")) {
      headers.set("X-CSRF-Token", csrfToken);
    }

    let payload = body;
    if (body !== undefined && isPlainObjectOrArray(body)) {
      payload = JSON.stringify(body);
      if (!headers.has("content-type")) {
        headers.set("Content-Type", "application/json");
      }
    }

    const fetchInit = {
      method: upperMethod,
      headers,
      redirect: reqOpts.redirect || "manual",
      signal: reqOpts.signal,
    };
    if (payload !== undefined && upperMethod !== "GET" && upperMethod !== "HEAD") {
      fetchInit.body = payload;
    }

    const res = await fetch(targetUrl, fetchInit);
    updateCookiesFromHeaders(res.headers);

    const buf = Buffer.from(await res.arrayBuffer());
    const text = buf.toString("utf8");
    const contentType = (res.headers.get("content-type") || "").toLowerCase();
    let data = null;
    if (contentType.includes("application/json") || contentType.includes("+json")) {
      try {
        data = text ? JSON.parse(text) : null;
      } catch {
        data = null;
      }
    }

    return {
      status: res.status,
      ok: res.ok,
      headers: res.headers,
      data,
      text,
      buffer: buf,
      url: res.url,
    };
  }

  const client = {
    baseUrl: normalizedBaseUrl,
    userAgent,
    cookies,
    get csrfToken() {
      return csrfToken;
    },
    set csrfToken(value) {
      csrfToken = value || "";
    },
    get bearerToken() {
      return bearerToken;
    },
    set bearerToken(value) {
      bearerToken = value || "";
    },
    request,
    api: request,
    get(reqPath, reqOpts) {
      return request("GET", reqPath, undefined, reqOpts);
    },
    post(reqPath, body, reqOpts) {
      return request("POST", reqPath, body, reqOpts);
    },
    put(reqPath, body, reqOpts) {
      return request("PUT", reqPath, body, reqOpts);
    },
    patch(reqPath, body, reqOpts) {
      return request("PATCH", reqPath, body, reqOpts);
    },
    del(reqPath, arg2, arg3) {
      if (arg3 !== undefined) {
        return request("DELETE", reqPath, arg2, arg3);
      }
      if (isDelOptsObject(arg2)) {
        return request("DELETE", reqPath, arg2.body, arg2);
      }
      return request("DELETE", reqPath, arg2, {});
    },
    async setupComplete(payload) {
      const res = await request("POST", "/api/setup/complete", payload);
      if (res.ok && res.data?.csrf_token) {
        csrfToken = res.data.csrf_token;
      }
      return res;
    },
    async login(ident, password, reqOpts = {}) {
      const res = await request("POST", "/api/admin/auth/login", { ident, password }, reqOpts);
      if (res.ok && !res.data?.requires_2fa && res.data?.csrf_token) {
        csrfToken = res.data.csrf_token;
      }
      return res;
    },
    async verify2FA(challengeToken, code, rememberDevice = false, reqOpts = {}) {
      const res = await request(
        "POST",
        "/api/admin/auth/2fa",
        {
          challenge_token: challengeToken,
          code,
          remember_device: rememberDevice,
        },
        reqOpts,
      );
      if (res.ok && res.data?.csrf_token) {
        csrfToken = res.data.csrf_token;
      }
      return res;
    },
    async logout(reqOpts = {}) {
      const res = await request("POST", "/api/admin/auth/logout", undefined, reqOpts);
      if (res.ok) {
        csrfToken = "";
      }
      return res;
    },
    async me(withConsoleHeader = true, reqOpts = {}) {
      const res = await request("GET", "/api/admin/me", undefined, {
        ...reqOpts,
        consoleHeader: withConsoleHeader,
      });
      if (res.ok && res.data?.csrf_token) {
        csrfToken = res.data.csrf_token;
      }
      return res;
    },
    async createPAT(name, scopes, currentPassword, expiresInDays = 30) {
      const res = await request("POST", "/api/admin/security/tokens", {
        name,
        scopes,
        expires_in_days: expiresInDays,
        current_password: currentPassword,
      });
      return {
        res,
        plaintextToken: res.data?.plaintext_token || "",
        token: res.data?.token || null,
      };
    },
    withBearer(token) {
      return createClient(normalizedBaseUrl, {
        bearerToken: token,
        userAgent,
        headers: defaultHeaders,
      });
    },
    clearSession() {
      cookies.clear();
      csrfToken = "";
      bearerToken = "";
    },
  };

  return client;
}
