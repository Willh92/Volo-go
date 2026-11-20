const CONTROL_OR_ZW_RE = /[\u0000-\u001f\u007f\u200b-\u200d\ufeff]/g;
const SAFE_DATA_IMAGE_RE = /^data:image\/(?:png|jpe?g|gif|webp|avif|bmp|x-icon);base64,[A-Za-z0-9+/=]+$/i;

function stripControlChars(raw: unknown): string {
  return String(raw ?? "").replace(CONTROL_OR_ZW_RE, "").trim();
}

export function safeHttpUrl(raw: unknown): string {
  const s = stripControlChars(raw);
  if (!s || /[\s"'<>`]/.test(s) || !/^https?:\/\/[^/]/i.test(s)) return "";
  try {
    const u = new URL(s);
    return (u.protocol === "https:" || u.protocol === "http:") && u.host ? s : "";
  } catch {
    return "";
  }
}

export function safeHref(
  raw: unknown,
  opts: { allowRelative?: boolean; allowHash?: boolean; allowMailto?: boolean } = {}
): string {
  const { allowRelative = true, allowHash = true, allowMailto = false } = opts;
  const s = stripControlChars(raw);
  if (!s || s.startsWith("//") || s.startsWith("/\\") || s.startsWith("\\") || /["'<>`]/.test(s)) {
    return "";
  }
  if (allowHash && s.startsWith("#")) {
    return /^#[A-Za-z0-9._:/?&=+%~-]*$/.test(s) ? s : "";
  }
  if (allowRelative && s.startsWith("/")) {
    return !/\s/.test(s) ? s : "";
  }
  if (
    allowRelative &&
    /^(?:\.\.?\/|[A-Za-z0-9._-]+(?:\/|\?|#|$))/.test(s) &&
    !/^[A-Za-z][A-Za-z0-9+.-]*:/.test(s)
  ) {
    return !/\s/.test(s) ? s : "";
  }
  try {
    const u = new URL(s);
    if (u.protocol === "https:" || u.protocol === "http:") {
      return !/\s/.test(s) && /^https?:\/\/[^/]/i.test(s) && u.host ? s : "";
    }
    if (allowMailto && (u.protocol === "mailto:" || u.protocol === "tel:")) {
      return s;
    }
    return "";
  } catch {
    return "";
  }
}

export function safeMediaSrc(
  raw: unknown,
  opts: { allowDataImage?: boolean; allowBlob?: boolean; allowRelative?: boolean } = {}
): string {
  const { allowDataImage = false, allowBlob = false, allowRelative = true } = opts;
  const s = stripControlChars(raw);
  if (!s || /["'<>`]/.test(s)) return "";
  if (allowDataImage && SAFE_DATA_IMAGE_RE.test(s)) return s;
  if (allowBlob && /^blob:https?:\/\//i.test(s) && !/\s/.test(s)) return s;
  return safeHref(s, { allowRelative, allowHash: false, allowMailto: false });
}

export function cleanClipboardUrl(raw: string, kind: "src" | "href" = "src"): string {
  const s = stripControlChars(raw);
  if (!s) return "";
  const validate = (val: string): string =>
    kind === "href"
      ? safeHref(val, { allowRelative: true, allowHash: true, allowMailto: true })
      : safeMediaSrc(val, { allowDataImage: true, allowBlob: true, allowRelative: true });
  const first = validate(s);
  if (!first) return "";
  if (/^(?:https?:\/\/|\/(?!\/))/i.test(first)) {
    let unescaped = first;
    for (let pass = 0; pass < 2 && unescaped.includes("&amp;"); pass++) {
      unescaped = unescaped.replace(/&amp;/gi, "&");
    }
    return validate(unescaped);
  }
  return first;
}
