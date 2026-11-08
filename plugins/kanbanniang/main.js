var ALLOWED_CHARACTERS = { neko: true, owl: true, bot: true };
var ALLOWED_POSITIONS = { right: true, left: true };

function escapeAttr(s) {
  return String(s || "")
    .replace(/&/g, "&amp;")
    .replace(/"/g, "&quot;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;");
}

function safeSettings(locale) {
  var cfg = volo.config.all() || {};
  var ch = String(cfg.character || "neko");
  if (!ALLOWED_CHARACTERS[ch]) ch = "neko";
  var pos = String(cfg.position || "left");
  if (!ALLOWED_POSITIONS[pos]) pos = "left";
  var loc = String(locale || "").toLowerCase();
  var isEN = loc.indexOf("en") === 0;
  var isTW = loc === "zh-tw" || loc.indexOf("zh-hant") === 0;
  var zhCN = String(cfg.greeting_zh || "欢迎来到独立博客，点我可以摸摸头～").slice(0, 140);
  var zhTW = String(cfg.greeting_tw || cfg.greeting_zh || "歡迎來到獨立部落格，點我可以摸摸頭～").slice(0, 140);
  var enUS = String(cfg.greeting_en || "Welcome! Click me for a head-pat~").slice(0, 140);
  var greeting = isEN ? enUS : isTW ? zhTW : zhCN;
  return {
    character: ch,
    position: pos,
    greeting: greeting,
    greetingZh: zhCN,
    greetingTw: zhTW,
    greetingEn: enUS,
    hideOnMobile: cfg.hide_on_mobile !== false,
    ariaLabel: isEN ? "Studio Mascot" : "看板娘助手",
    btnLabel: isEN ? "Pat mascot" : "摸摸看板娘",
    closeLabel: isEN ? "Dismiss message" : "收起提示",
  };
}

function onEnable() {
  volo.log.info("Studio mascot companion enabled");
}

function renderSlot(slotName, slotCtx) {
  var locale = (slotCtx && slotCtx.locale) || "zh-CN";
  var st = safeSettings(locale);
  var pats = Number(volo.kv.getInt("total_pats") || 0);

  if (slotName === "admin.dashboard_card") {
    var loc = String(locale).toLowerCase();
    var title = loc.indexOf("en") === 0
      ? "Mascot Head-Pats"
      : loc === "zh-tw" || loc.indexOf("zh-hant") === 0
        ? "看板娘互動統計"
        : "看板娘互动统计";
    return (
      '<div class="kbn-admin-stat"><strong>' +
      escapeAttr(title) +
      ":</strong> <span>" +
      pats +
      "</span> (" +
      escapeAttr(st.character) +
      ")</div>"
    );
  }

  if (slotName === "public.footer") {
    var cls =
      "volo-kbn-dock volo-kbn-" +
      st.position +
      (st.hideOnMobile ? " volo-kbn-hide-mobile" : "");
    return (
      '<aside id="volo-kanbanniang-root" class="' +
      cls +
      '" data-position="' +
      escapeAttr(st.position) +
      '" data-character="' +
      escapeAttr(st.character) +
      '" data-greeting="' +
      escapeAttr(st.greeting) +
      '" data-greeting-zh="' +
      escapeAttr(st.greetingZh) +
      '" data-greeting-tw="' +
      escapeAttr(st.greetingTw) +
      '" data-greeting-en="' +
      escapeAttr(st.greetingEn) +
      '" data-pats="' +
      pats +
      '" data-btn-label="' +
      escapeAttr(st.btnLabel) +
      '" data-close-label="' +
      escapeAttr(st.closeLabel) +
      '" aria-label="' +
      escapeAttr(st.ariaLabel) +
      '"></aside>'
    );
  }
  return "";
}

function onRequest(req) {
  var sub = String((req && req.path) || "").replace(/^\/+/, "");
  if (req.method === "POST" && (sub === "pat" || sub === "")) {
    var ipKey = "pat:" + String((req && req.client_ip_masked) || "anon");
    if (volo.rateLimit && typeof volo.rateLimit.allow === "function" && !volo.rateLimit.allow(ipKey, 20, 60)) {
      return {
        status: 429,
        headers: { "Content-Type": "application/json; charset=utf-8" },
        body: JSON.stringify({ ok: false, error: "rate limit exceeded" }),
      };
    }
    var total = volo.kv.incr("total_pats", 1, 0);
    return {
      status: 200,
      headers: { "Content-Type": "application/json; charset=utf-8" },
      body: JSON.stringify({ ok: true, total_pats: total }),
    };
  }
  if (req.method === "GET" && (sub === "state" || sub === "")) {
    var st = safeSettings((req.query && req.query.locale) || "zh-CN");
    return {
      status: 200,
      headers: { "Content-Type": "application/json; charset=utf-8" },
      body: JSON.stringify({
        ok: true,
        character: st.character,
        position: st.position,
        total_pats: Number(volo.kv.getInt("total_pats") || 0),
      }),
    };
  }
  return {
    status: 404,
    headers: { "Content-Type": "application/json; charset=utf-8" },
    body: JSON.stringify({ error: "not found" }),
  };
}

module.exports = {
  onEnable: onEnable,
  renderSlot: renderSlot,
  onRequest: onRequest,
};
