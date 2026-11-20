import assert from "node:assert/strict";

function summarizeBody(res) {
  if (!res) return "<no response>";
  const raw = res.text ?? (res.data ? JSON.stringify(res.data) : "");
  const trimmed = String(raw).trim();
  if (trimmed.length <= 400) return trimmed;
  return `${trimmed.slice(0, 400)}... [truncated]`;
}

function getHeaderCaseInsensitive(headers, name) {
  if (!headers) return null;
  if (typeof headers.get === "function") {
    return headers.get(name);
  }
  const lower = name.toLowerCase();
  for (const [k, v] of Object.entries(headers)) {
    if (k.toLowerCase() === lower) {
      return Array.isArray(v) ? v.join(", ") : String(v);
    }
  }
  return null;
}

function getSetCookieHeaders(headers) {
  if (!headers) return [];
  if (typeof headers.getSetCookie === "function") {
    return headers.getSetCookie();
  }
  if (typeof headers.get === "function") {
    const val = headers.get("set-cookie");
    return val ? [String(val)] : [];
  }
  for (const [k, v] of Object.entries(headers)) {
    if (k.toLowerCase() === "set-cookie") {
      if (Array.isArray(v)) {
        return v.map((item) => String(item));
      }
      return v != null ? [String(v)] : [];
    }
  }
  return [];
}

/**
 * Assert HTTP status matches an expected code or array of acceptable codes.
 */
export function assertStatus(res, expectedStatus, contextMsg = "") {
  const allowed = Array.isArray(expectedStatus) ? expectedStatus : [expectedStatus];
  const prefix = contextMsg ? `${contextMsg}: ` : "";
  assert.ok(
    allowed.includes(res?.status),
    `${prefix}Expected HTTP status ${allowed.join(" | ")}, got ${res?.status}. Body: ${summarizeBody(res)}`,
  );
}

/**
 * Assert that an object contains all required keys (with non-undefined values).
 */
export function assertJsonHasKeys(obj, keys, contextMsg = "") {
  const prefix = contextMsg ? `${contextMsg}: ` : "";
  assert.ok(
    obj !== null && typeof obj === "object" && !Array.isArray(obj),
    `${prefix}Expected JSON object, got ${obj === null ? "null" : typeof obj}`,
  );
  const missing = keys.filter((k) => !(k in obj) || obj[k] === undefined);
  assert.equal(
    missing.length,
    0,
    `${prefix}Missing JSON keys [${missing.join(", ")}] in ${JSON.stringify(Object.keys(obj))}`,
  );
}

/**
 * Assert HTTP redirect status and Location header substring/pattern match.
 */
export function assertRedirect(res, expectedStatus, locationPart, contextMsg = "") {
  assertStatus(res, expectedStatus, contextMsg || "Redirect check");
  const location = getHeaderCaseInsensitive(res.headers, "location") || "";
  const prefix = contextMsg ? `${contextMsg}: ` : "";
  if (locationPart instanceof RegExp) {
    assert.match(
      location,
      locationPart,
      `${prefix}Expected Location header matching ${locationPart}, got "${location}"`,
    );
  } else {
    assert.ok(
      location.includes(String(locationPart)),
      `${prefix}Expected Location header to include "${locationPart}", got "${location}"`,
    );
  }
}

/**
 * Assert a response header exists and includes the expected substring (case-insensitive by default).
 */
export function assertHeaderIncludes(res, headerName, substr, contextMsg = "") {
  const value = getHeaderCaseInsensitive(res.headers, headerName);
  const prefix = contextMsg ? `${contextMsg}: ` : "";
  assert.ok(
    value !== null,
    `${prefix}Expected header "${headerName}" to be present`,
  );
  assert.ok(
    value.toLowerCase().includes(String(substr).toLowerCase()),
    `${prefix}Expected header "${headerName}" ("${value}") to include "${substr}"`,
  );
}

/**
 * Assert a Set-Cookie header for cookieName was sent and satisfies optional attributes.
 */
export function assertCookieSet(res, cookieName, opts = {}) {
  const lines = getSetCookieHeaders(res?.headers);
  const matching = lines.filter((line) =>
    line.trim().startsWith(`${cookieName}=`),
  );
  const prefix = opts.contextMsg ? `${opts.contextMsg}: ` : "";
  assert.ok(
    matching.length > 0,
    `${prefix}Expected Set-Cookie for "${cookieName}", got [${lines.join(" | ")}]`,
  );

  const line = matching[matching.length - 1];
  const segments = line.split(";").map((s) => s.trim());
  const firstEq = segments[0].indexOf("=");
  const value = firstEq >= 0 ? segments[0].slice(firstEq + 1) : "";
  const attrs = segments.slice(1).map((s) => s.toLowerCase());

  if (opts.httpOnly !== undefined) {
    const hasHttpOnly = attrs.includes("httponly");
    assert.equal(
      hasHttpOnly,
      opts.httpOnly,
      `${prefix}Cookie "${cookieName}" HttpOnly mismatch in "${line}"`,
    );
  }
  if (opts.sameSite !== undefined) {
    const expectedAttr = `samesite=${String(opts.sameSite).toLowerCase()}`;
    assert.ok(
      attrs.includes(expectedAttr),
      `${prefix}Cookie "${cookieName}" expected ${expectedAttr} in "${line}"`,
    );
  }
  if (opts.path !== undefined) {
    const expectedPath = `path=${opts.path}`.toLowerCase();
    assert.ok(
      attrs.includes(expectedPath),
      `${prefix}Cookie "${cookieName}" expected ${expectedPath} in "${line}"`,
    );
  }
  if (opts.deleted === true) {
    const maxAgeZero = attrs.some((a) => {
      if (!a.startsWith("max-age=")) return false;
      const n = Number(a.slice("max-age=".length));
      return Number.isFinite(n) && n <= 0;
    });
    assert.ok(
      value === "" || maxAgeZero,
      `${prefix}Expected cookie "${cookieName}" to be deleted, got "${line}"`,
    );
  }
  if (opts.nonEmpty === true) {
    assert.ok(value.length > 0, `${prefix}Expected non-empty value for cookie "${cookieName}"`);
  }
  return line;
}

function decodeHtmlEntities(str) {
  const named = {
    colon: ":",
    sol: "/",
    tab: "\t",
    newline: "\n",
    nbsp: " ",
    amp: "&",
    lt: "<",
    gt: ">",
    quot: '"',
    apos: "'",
  };
  return str
    .replace(/&#(\d+);?/g, (full, dec) => {
      const code = Number.parseInt(dec, 10);
      return Number.isFinite(code) && code >= 0 && code <= 0x10ffff
        ? String.fromCodePoint(code)
        : full;
    })
    .replace(/&#x([0-9a-f]+);?/gi, (full, hex) => {
      const code = Number.parseInt(hex, 16);
      return Number.isFinite(code) && code >= 0 && code <= 0x10ffff
        ? String.fromCodePoint(code)
        : full;
    })
    .replace(/&([a-z]+);/gi, (full, name) => {
      const ch = named[name.toLowerCase()];
      return ch !== undefined ? ch : full;
    });
}

/**
 * Assert rendered HTML fragment contains no dangerous XSS vectors.
 */
export function assertNoXSS(html, contextMsg = "") {
  const source = String(html || "");
  const decoded = decodeHtmlEntities(source);
  const decodedCompact = decoded.replace(/[\u0000-\u001f\u007f]+/g, "");
  const prefix = contextMsg ? `${contextMsg}: ` : "";

  const rawPatterns = [
    { name: "<script> tag", re: /<script\b/i },
    { name: "inline event handler", re: /\bon[a-z]+\s*=/i },
  ];
  for (const pat of rawPatterns) {
    const match = source.match(pat.re);
    assert.equal(
      match,
      null,
      `${prefix}Unsafe XSS vector (${pat.name}) detected in HTML: ${match ? match[0] : ""}`,
    );
  }

  const schemePatterns = [
    { name: "javascript: URL", re: /javascript\s*:/i },
    { name: "vbscript: URL", re: /vbscript\s*:/i },
    { name: "data:text/html URL", re: /data\s*:\s*text\/html/i },
  ];
  for (const pat of schemePatterns) {
    for (const candidate of [source, decoded, decodedCompact]) {
      const match = candidate.match(pat.re);
      assert.equal(
        match,
        null,
        `${prefix}Unsafe XSS vector (${pat.name}) detected in HTML: ${match ? match[0] : ""}`,
      );
    }
  }
}

/**
 * Fetch GET /api/admin/settings/{domain} and verify expected fields persisted.
 */
export async function assertSettingPersisted(
  client,
  domain,
  expectedSubset,
  contextMsg = "",
) {
  const res = await client.get(`/api/admin/settings/${domain}`);
  assertStatus(
    res,
    200,
    contextMsg || `GET /api/admin/settings/${domain}`,
  );
  const stored = res.data?.value;
  assert.ok(
    stored && typeof stored === "object",
    `${contextMsg || domain}: Expected settings.value object, got ${JSON.stringify(res.data)}`,
  );

  for (const [key, expectedVal] of Object.entries(expectedSubset)) {
    assert.deepStrictEqual(
      stored[key],
      expectedVal,
      `${contextMsg ? `${contextMsg}: ` : ""}Setting ${domain}.${key} mismatch`,
    );
  }
  return res.data;
}

/**
 * Assert an SSRF-protected endpoint rejected a non-public/forbidden target.
 */
export function assertSSRFRejected(res, contextMsg = "") {
  const prefix = contextMsg ? `${contextMsg}: ` : "";
  assertStatus(res, [400, 403], contextMsg || "SSRF rejection");
  assert.notEqual(
    res.data?.ok,
    true,
    `${prefix}Expected ok !== true on SSRF rejection, got ${JSON.stringify(res.data)}`,
  );
  const errMsg = String(res.data?.error || res.text || "").trim();
  assert.ok(errMsg.length > 0, `${prefix}Expected non-empty SSRF error message`);
}
