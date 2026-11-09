var DEFAULT_UPDATE_URL =
  "https://raw.githubusercontent.com/houbb/sensitive-word-data/main/src/main/resources/sensitive_word_dict.txt";
var DEFAULT_RESERVED_NICKNAMES = [
  "管理员",
  "系统管理员",
  "官方客服",
  "网站管理员",
  "站长",
  "博主",
  "superadmin",
  "webmaster",
];
var CRON_JOB_NAME = "sync_dict";
var REMOTE_KV_PREFIX = "remote";
var BUNDLED_HOUBB_ASSET = "dicts/houbb_base.txt.gz";
var BUNDLED_ALLOW_ASSET = "dicts/allowlist.txt";

function escapeHTML(s) {
  if (volo && typeof volo.escapeHTML === "function") {
    return volo.escapeHTML(String(s || ""));
  }
  return String(s || "")
    .replace(/&/g, "&amp;")
    .replace(/"/g, "&quot;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;");
}

function fnv1aStep(h, str) {
  var s = String(str || "");
  for (var i = 0; i < s.length; i++) {
    h = h ^ s.charCodeAt(i);
    h = (h + ((h << 1) + (h << 4) + (h << 7) + (h << 8) + (h << 24))) >>> 0;
  }
  return h;
}

function hashWordList(val) {
  var h = 0x811c9dc5;
  if (Array.isArray(val)) {
    for (var i = 0; i < val.length; i++) {
      if (i > 0) {
        h = h ^ 10;
        h = (h + ((h << 1) + (h << 4) + (h << 7) + (h << 8) + (h << 24))) >>> 0;
      }
      h = fnv1aStep(h, val[i]);
    }
    return h.toString(16);
  }
  return fnv1aStep(h, val).toString(16);
}

function normalizeWordList(rawVal, fallbackList) {
  if (rawVal === undefined || rawVal === null) {
    return fallbackList;
  }
  if (Array.isArray(rawVal)) {
    return rawVal;
  }
  return String(rawVal || "");
}

function safeConfig(overrideCfg) {
  var raw = overrideCfg || (volo.config && volo.config.all()) || {};
  var act = String(raw.action || "reject").toLowerCase();
  if (act !== "reject" && act !== "moderate") {
    act = "reject";
  }
  var hours = Number(raw.update_interval_hours);
  if (!(hours >= 1 && hours <= 720)) {
    hours = 24;
  }
  var updateUrl = String(
    raw.update_url === undefined ? DEFAULT_UPDATE_URL : raw.update_url || ""
  ).trim();
  var reserved = normalizeWordList(raw.reserved_nicknames, DEFAULT_RESERVED_NICKNAMES);

  return {
    action: act,
    customRejectMessage: String(raw.custom_reject_message || "").trim().slice(0, 200),
    maskExisting: raw.mask_existing !== false,
    checkNickname: raw.check_nickname !== false,
    customNicknameMessage: String(raw.custom_nickname_message || "").trim().slice(0, 200),
    reservedNicknames: reserved,
    catPolitics: raw.cat_politics !== false,
    catNarcotics: raw.cat_narcotics !== false,
    catPorn: raw.cat_porn !== false,
    catGambling: raw.cat_gambling !== false,
    catCrime: raw.cat_crime !== false,
    catGeneral: raw.cat_general !== false,
    ignoreSeparators: raw.ignore_separators !== false,
    ignoreRepeat: raw.ignore_repeat !== false,
    customDeny: normalizeWordList(raw.custom_deny, []),
    customAllow: normalizeWordList(raw.custom_allow, []),
    autoUpdate: raw.auto_update === true,
    updateUrl: updateUrl,
    updateIntervalHours: hours,
  };
}

function resolveCommentMessage(locale, customMsg) {
  if (customMsg) {
    return customMsg;
  }
  var loc = String(locale || "zh-CN").toLowerCase();
  if (loc.indexOf("en") === 0) {
    return "Your comment contains restricted words. Please revise and try again.";
  }
  if (loc.indexOf("ja") === 0) {
    return "コメントに制限された語句が含まれています。内容を修正して再度お試しください。";
  }
  if (loc === "zh-tw" || loc.indexOf("zh-hant") === 0) {
    return "評論包含不適合發表的詞語，請修改後再試。";
  }
  return "评论包含不适合发表的词语，请修改后再试。";
}

function resolveNicknameMessage(locale, customMsg) {
  if (customMsg) {
    return customMsg;
  }
  var loc = String(locale || "zh-CN").toLowerCase();
  if (loc.indexOf("en") === 0) {
    return "This nickname contains restricted or reserved words. Please choose another one.";
  }
  if (loc.indexOf("ja") === 0) {
    return "このニックネームには使用できない語句または予約語が含まれています。別の名前をお試しください。";
  }
  if (loc === "zh-tw" || loc.indexOf("zh-hant") === 0) {
    return "暱稱包含不適合使用或受保護的詞語，請更換後再試。";
  }
  return "昵称包含不适合使用或受保护的词语，请更换后再试。";
}

function readRemoteMeta() {
  if (!volo.kv) return null;
  var raw = volo.kv.get(REMOTE_KV_PREFIX + ":meta");
  if (!raw) return null;
  try {
    var parsed = JSON.parse(raw);
    if (parsed && typeof parsed === "object") {
      return parsed;
    }
  } catch (_e) {}
  return null;
}

function buildSignatures(cfg) {
  var rev = volo.wordFilter.revision();
  var flags =
    (cfg.catPolitics ? "1" : "0") +
    (cfg.catNarcotics ? "1" : "0") +
    (cfg.catPorn ? "1" : "0") +
    (cfg.catGambling ? "1" : "0") +
    (cfg.catCrime ? "1" : "0") +
    (cfg.catGeneral ? "1" : "0") +
    (cfg.ignoreSeparators ? "1" : "0") +
    (cfg.ignoreRepeat ? "1" : "0");
  var allowHash = hashWordList(cfg.customAllow);
  return {
    commentSig:
      "c:" + rev + ":" + flags + ":" + hashWordList(cfg.customDeny) + ":" + allowHash,
    reservedSig:
      "r:" +
      rev +
      ":" +
      (cfg.ignoreSeparators ? "1" : "0") +
      (cfg.ignoreRepeat ? "1" : "0") +
      ":" +
      hashWordList(cfg.reservedNicknames) +
      ":" +
      allowHash,
  };
}

function syncNativeSanitizer(cfg, sigs) {
  if (!volo.wordFilter || typeof volo.wordFilter.setSanitizer !== "function") {
    return;
  }
  if (!cfg.maskExisting) {
    volo.wordFilter.setSanitizer({
      enabled: false,
      content_sig: "",
      nickname_sig: "",
      check_nickname: false,
    });
    return;
  }
  volo.wordFilter.setSanitizer({
    enabled: true,
    content_sig: sigs.commentSig,
    nickname_sig: cfg.checkNickname ? sigs.reservedSig : "",
    check_nickname: cfg.checkNickname,
  });
}

// ensureCompiled lazily builds the shared Go-native 16B/state BFS-CSR Aho-Corasick automatons
// on first actual check without materializing large dictionary strings inside the JS VM.
function ensureCompiled(cfg) {
  var sigs = buildSignatures(cfg);
  var needComment = !volo.wordFilter.has(sigs.commentSig);
  var needReserved = cfg.checkNickname && !volo.wordFilter.has(sigs.reservedSig);
  if (!needComment && !needReserved) {
    syncNativeSanitizer(cfg, sigs);
    return sigs;
  }

  if (needComment) {
    volo.wordFilter.compile(sigs.commentSig, {
      ignore_separators: cfg.ignoreSeparators,
      ignore_repeat: cfg.ignoreRepeat,
      houbb_asset: BUNDLED_HOUBB_ASSET,
      remote_kv_prefix: REMOTE_KV_PREFIX,
      remote_mode: "auto",
      allow_assets: [BUNDLED_ALLOW_ASSET],
      enabled_categories: {
        politics: cfg.catPolitics,
        narcotics: cfg.catNarcotics,
        porn: cfg.catPorn,
        gambling: cfg.catGambling,
        crime: cfg.catCrime,
        general: cfg.catGeneral,
      },
      deny: cfg.customDeny,
      allow: cfg.customAllow,
    });
  }

  if (needReserved) {
    var reservedGroup = Array.isArray(cfg.reservedNicknames)
      ? { category: "impersonation", words: cfg.reservedNicknames }
      : { category: "impersonation", text: cfg.reservedNicknames };
    volo.wordFilter.compile(sigs.reservedSig, {
      ignore_separators: cfg.ignoreSeparators,
      ignore_repeat: cfg.ignoreRepeat,
      allow_assets: [BUNDLED_ALLOW_ASSET],
      groups: [reservedGroup],
      allow: cfg.customAllow,
    });
  }

  syncNativeSanitizer(cfg, sigs);
  return sigs;
}

function ensureSanitizer() {
  var cfg = safeConfig();
  if (!cfg.maskExisting) {
    syncNativeSanitizer(cfg, { commentSig: "", reservedSig: "" });
    return { enabled: false };
  }
  var sigs = ensureCompiled(cfg);
  return { enabled: true, content_sig: sigs.commentSig };
}

function matchTextFast(sig, text) {
  if (typeof volo.wordFilter.contains === "function") {
    return volo.wordFilter.contains(sig, text);
  }
  var hit = volo.wordFilter.match(sig, text);
  return Boolean(hit && hit.matched);
}

function matchNickname(sigs, nickname) {
  var name = String(nickname || "").trim();
  if (!name) {
    return false;
  }
  if (matchTextFast(sigs.commentSig, name)) {
    return true;
  }
  return matchTextFast(sigs.reservedSig, name);
}

function recordIntercept(isNickname) {
  if (!volo.kv) {
    return;
  }
  try {
    if (typeof volo.kv.incrAsync === "function") {
      volo.kv.incrAsync("stats:blocked_total", 1);
      if (isNickname) {
        volo.kv.incrAsync("stats:blocked_nickname", 1);
      }
      return;
    }
    if (typeof volo.kv.incr === "function") {
      volo.kv.incr("stats:blocked_total", 1, 0);
      if (isNickname) {
        volo.kv.incr("stats:blocked_nickname", 1, 0);
      }
    }
  } catch (_e) {}
}

function checkSpam(input) {
  var cfg = safeConfig();
  var sigs = ensureCompiled(cfg);
  var locale = (input && input.locale) || "zh-CN";
  var isNicknameTarget = input && String(input.target_type || "") === "nickname";
  var shouldScreenAuthorName =
    isNicknameTarget || !input || input.is_guest === undefined || Boolean(input.is_guest);

  if (cfg.checkNickname && shouldScreenAuthorName && input && input.author_name) {
    if (matchNickname(sigs, input.author_name)) {
      recordIntercept(true);
      return {
        action: isNicknameTarget ? "reject" : cfg.action,
        score: 1.0,
        reason: resolveNicknameMessage(locale, cfg.customNicknameMessage),
      };
    }
  }

  if (isNicknameTarget) {
    return { action: "pass", score: 0 };
  }

  var authorUrl = String((input && input.author_url) || "").trim();
  if (authorUrl && matchTextFast(sigs.commentSig, authorUrl)) {
    recordIntercept(false);
    return {
      action: cfg.action,
      score: 1.0,
      reason: resolveCommentMessage(locale, cfg.customRejectMessage),
    };
  }

  var body = String((input && input.content_md) || "");
  if (body && matchTextFast(sigs.commentSig, body)) {
    recordIntercept(false);
    return {
      action: cfg.action,
      score: 1.0,
      reason: resolveCommentMessage(locale, cfg.customRejectMessage),
    };
  }

  return { action: "pass", score: 0 };
}

function checkNickname(input) {
  var cfg = safeConfig();
  if (!cfg.checkNickname) {
    return { action: "pass", score: 0 };
  }
  var sigs = ensureCompiled(cfg);
  var locale = (input && input.locale) || "zh-CN";
  var cand = String((input && input.nickname) || "");
  var hit = matchNickname(sigs, cand);
  if (hit) {
    recordIntercept(true);
    return {
      action: "reject",
      score: 1.0,
      reason: resolveNicknameMessage(locale, cfg.customNicknameMessage),
    };
  }
  return { action: "pass", score: 0 };
}

function syncRemoteDictionary(force) {
  var cfg = safeConfig();
  if (!cfg.updateUrl) {
    return { ok: false, error: "update_url is empty" };
  }
  if (!volo.wordFilter || typeof volo.wordFilter.syncRemote !== "function") {
    return { ok: false, error: "native syncRemote capability unavailable" };
  }

  try {
    var res = volo.wordFilter.syncRemote({
      url: cfg.updateUrl,
      force: !!force,
      kv_prefix: REMOTE_KV_PREFIX,
      base_tag_asset: BUNDLED_HOUBB_ASSET,
      replace_threshold: 10000,
    });
    if (res && res.ok) {
      if (!res.not_modified) {
        volo.log.info(
          "synced & converted remote dictionary (" +
            res.mode +
            "): " +
            res.word_count +
            " words, " +
            res.tagged_count +
            " tagged, " +
            res.dropped_count +
            " noisy entries dropped across " +
            res.chunks +
            " compressed chunk(s)"
        );
      }
      return res;
    }
    var errMsg = String((res && res.error) || "sync failed");
    volo.log.warn("remote dictionary sync failed: " + errMsg);
    return { ok: false, error: errMsg };
  } catch (err) {
    var msg = String((err && err.message) || err || "sync failed").slice(0, 240);
    if (volo.kv) {
      try {
        volo.kv.set(
          REMOTE_KV_PREFIX + ":last_error",
          new Date().toISOString() + " " + msg,
          0
        );
      } catch (_e) {}
    }
    volo.log.warn("remote dictionary sync error: " + msg);
    return { ok: false, error: msg };
  }
}

function syncSchedule(cfg) {
  if (!volo.cron) {
    return;
  }
  if (!cfg.autoUpdate || !cfg.updateUrl) {
    volo.cron.unregister(CRON_JOB_NAME);
    return;
  }
  var intervalSec = Math.max(
    3600,
    Math.min(720 * 3600, Math.round(cfg.updateIntervalHours * 3600))
  );
  var meta = readRemoteMeta();
  var needsInitialSync = !meta || meta.url !== cfg.updateUrl || !meta.synced_at;
  volo.cron.register(CRON_JOB_NAME, {
    interval_seconds: intervalSec,
    handler: "onCronSync",
    immediate: needsInitialSync,
  });
}

function onEnable() {
  var cfg = safeConfig();
  // Keep startup idle RSS near zero: schedule cron if enabled, and compile lazily on first check.
  syncSchedule(cfg);
}

function onConfigChange(newCfg) {
  // Release previous matcher memory immediately before lazy recompilation under updated config.
  volo.wordFilter.clear();
  var cfg = safeConfig(newCfg);
  syncSchedule(cfg);
}

function onDisable() {
  if (volo.cron) {
    volo.cron.unregister(CRON_JOB_NAME);
  }
  volo.wordFilter.clear();
}

function onUninstall() {
  if (volo.cron) {
    volo.cron.unregister(CRON_JOB_NAME);
  }
  volo.wordFilter.clear();
  if (volo.kv && typeof volo.kv.clear === "function") {
    try {
      volo.kv.clear();
    } catch (_e) {}
  }
}

function onCronSync() {
  syncRemoteDictionary(false);
}

function renderSlot(slotName, slotCtx) {
  if (slotName !== "admin.dashboard_card") {
    return "";
  }
  var cfg = safeConfig();
  var sigs = buildSignatures(cfg);
  var stats = volo.wordFilter.stats(sigs.commentSig) || {};
  var blockedTotal = Number((volo.kv && volo.kv.getInt("stats:blocked_total")) || 0);
  var locale = String((slotCtx && slotCtx.locale) || "zh-CN").toLowerCase();
  var isEN = locale.indexOf("en") === 0;
  var isJA = locale.indexOf("ja") === 0;
  var isTW = locale === "zh-tw" || locale.indexOf("zh-hant") === 0;

  var title = isEN
    ? "Sensitive Word Guard"
    : isJA
      ? "NGワード・なりすまし防止ガード"
      : isTW
        ? "敏感詞與暱稱衛士"
        : "敏感词与昵称卫士";
  var rulesText = stats.compiled
    ? Number(stats.word_count || 0) +
      (isEN ? " rules" : isJA ? " 件のルール" : isTW ? " 條規則" : " 条规则")
    : isEN
      ? "64k+ rules (standby)"
      : isJA
        ? "6.4万+ 件（オンデマンド読込）"
        : isTW
          ? "6.4萬+ 條規則（按需載入）"
          : "6.4万+ 条规则（按需加载）";
  var blockedLabel = isEN
    ? "blocked"
    : isJA
      ? "件ブロック"
      : isTW
        ? "次攔截"
        : "次拦截";
  var syncBadge = cfg.autoUpdate
    ? isEN
      ? "Auto-Sync ON"
      : isJA
        ? "自動同期オン"
        : isTW
          ? "自動更新已開啟"
          : "自动更新已开启"
    : isEN
      ? "Offline Ready"
      : isJA
        ? "オフライン待機"
        : isTW
          ? "離線就緒"
          : "离线就绪";

  return (
    '<div class="sw-admin-stat"><strong>' +
    escapeHTML(title) +
    ":</strong> <span>" +
    escapeHTML(rulesText) +
    " · " +
    blockedTotal +
    " " +
    escapeHTML(blockedLabel) +
    " (" +
    escapeHTML(syncBadge) +
    ")</span></div>"
  );
}

function onRequest(req) {
  var sub = String((req && req.path) || "").replace(/^\/+/, "");
  var jsonHeaders = { "Content-Type": "application/json; charset=utf-8" };

  if (req.method === "POST" && sub === "sync") {
    if (!req.is_admin) {
      return {
        status: 403,
        headers: jsonHeaders,
        body: JSON.stringify({ ok: false, error: "admin authentication required" }),
      };
    }
    var syncRes = syncRemoteDictionary(true);
    return {
      status: syncRes.ok ? 200 : 502,
      headers: jsonHeaders,
      body: JSON.stringify({
        ok: Boolean(syncRes && syncRes.ok),
        sync: syncRes,
        word_count: Number((syncRes && syncRes.word_count) || 0),
        allow_count: Number((syncRes && syncRes.allow_count) || 0),
      }),
    };
  }

  if (req.method === "DELETE" && sub === "remote-cache") {
    if (!req.is_admin) {
      return {
        status: 403,
        headers: jsonHeaders,
        body: JSON.stringify({ ok: false, error: "admin authentication required" }),
      };
    }
    var purged = 0;
    if (volo.kv && typeof volo.kv.deletePrefix === "function") {
      purged = Number(volo.kv.deletePrefix(REMOTE_KV_PREFIX + ":")) || 0;
    }
    volo.wordFilter.clear();
    return {
      status: 200,
      headers: jsonHeaders,
      body: JSON.stringify({ ok: true, deleted_keys: purged }),
    };
  }

  if (req.method === "GET" && (sub === "status" || sub === "")) {
    if (!req.is_admin) {
      return {
        status: 403,
        headers: jsonHeaders,
        body: JSON.stringify({ ok: false, error: "admin authentication required" }),
      };
    }
    var cfg = safeConfig();
    var sigs = buildSignatures(cfg);
    var mainStats = volo.wordFilter.stats(sigs.commentSig) || {};
    var reservedStats = cfg.checkNickname
      ? volo.wordFilter.stats(sigs.reservedSig) || {}
      : { word_count: 0 };
    var meta = readRemoteMeta();
    var payload = {
      ok: true,
      compiled: Boolean(mainStats.compiled),
      action: cfg.action,
      mask_existing: cfg.maskExisting,
      check_nickname: cfg.checkNickname,
      auto_update: cfg.autoUpdate,
      word_count: Number(mainStats.word_count || 0),
      allow_count: Number(mainStats.allow_count || 0),
      reserved_nickname_count: Number(reservedStats.word_count || 0),
      blocked_total: Number((volo.kv && volo.kv.getInt("stats:blocked_total")) || 0),
      blocked_nickname: Number(
        (volo.kv && volo.kv.getInt("stats:blocked_nickname")) || 0
      ),
      remote_sync: meta
        ? {
            synced_at: meta.synced_at || "",
            checked_at: meta.checked_at || "",
            line_count: Number(meta.word_count || meta.line_count) || 0,
            tagged_count: Number(meta.tagged_count) || 0,
            dropped_count: Number(meta.dropped_count) || 0,
            chunks: Number(meta.chunks) || 0,
            mode: meta.mode || "merge",
          }
        : null,
      cron_jobs: volo.cron ? volo.cron.list() : [],
    };
    if (volo.kv) {
      var lastErr = volo.kv.get(REMOTE_KV_PREFIX + ":last_error");
      if (lastErr) {
        payload.last_error = lastErr;
      }
    }
    return {
      status: 200,
      headers: jsonHeaders,
      body: JSON.stringify(payload),
    };
  }

  return {
    status: 404,
    headers: jsonHeaders,
    body: JSON.stringify({ error: "not found" }),
  };
}

module.exports = {
  onEnable: onEnable,
  onDisable: onDisable,
  onUninstall: onUninstall,
  onConfigChange: onConfigChange,
  onCronSync: onCronSync,
  ensureSanitizer: ensureSanitizer,
  checkSpam: checkSpam,
  checkNickname: checkNickname,
  renderSlot: renderSlot,
  onRequest: onRequest,
};
