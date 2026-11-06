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
  var pos = String(cfg.position || "right");
  if (!ALLOWED_POSITIONS[pos]) pos = "right";
  var isEN = String(locale || "").toLowerCase().indexOf("en") === 0;
  var greeting = isEN
    ? cfg.greeting_en || "Welcome! Click me for a head-pat~"
    : cfg.greeting_zh || "欢迎来到独立博客，点我可以摸摸头～";
  return {
    character: ch,
    position: pos,
    greeting: String(greeting).slice(0, 140),
    hideOnMobile: cfg.hide_on_mobile !== false,
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
    var title = String(locale).toLowerCase().indexOf("en") === 0 ? "Mascot Head-Pats" : "看板娘互动统计";
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
      '" data-character="' +
      escapeAttr(st.character) +
      '" data-greeting="' +
      escapeAttr(st.greeting) +
      '" data-pats="' +
      pats +
      '" aria-label="Studio Mascot"></aside>'
    );
  }
  return "";
}

function onRequest(req) {
  var sub = String((req && req.path) || "").replace(/^\/+/, "");
  if (req.method === "POST" && (sub === "pat" || sub === "")) {
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
