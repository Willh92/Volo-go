var PLUGIN_VERSION = "2.0.2";
var MODEL_NAME_RE = /^[A-Za-z0-9_-]+\/[A-Za-z0-9_-]+$/;
var ALLOWED_POSITIONS = { left: true, right: true };
var RESOURCE_KEYS = {
  model: true,
  textures: true,
  physics: true,
  pose: true,
  file: true,
  sound: true,
};

var cachedManifestRaw = null;
var cachedModelNames = null;
var descriptorCache = {};
var ASSET_VERIFY_TTL_MS = 300000;
var HTML_ESC_RE = /[&"<>]/;

function escapeHtml(s) {
  var str = typeof s === "string" ? s : String(s || "");
  if (!HTML_ESC_RE.test(str)) return str;
  return str
    .replace(/&/g, "&amp;")
    .replace(/"/g, "&quot;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;");
}

var LOCALE_STRINGS = {
  "zh-CN": {
    greeting: "欢迎来到这个文字空间，点我可以摸摸头～",
    ariaLabel: "看板娘助手",
    dashboardTitle: "看板娘互动统计",
    rateLimited: "摸太快啦，歇会儿再摸～",
    changeTip: "换一套新装，继续陪你阅读。",
    copyTip: "如果转载，也请记得注明出处。",
    toolHome: "返回首页",
    toolRSS: "订阅博客",
    toolChat: "说句话",
    toolChange: "切换模型与换装",
    toolPhoto: "保存图片",
    toolGithub: "查看项目主页",
    toolClose: "关闭看板娘",
  },
  "zh-TW": {
    greeting: "歡迎來到這個文字空間，點我可以摸摸頭～",
    ariaLabel: "看板娘助手",
    dashboardTitle: "看板娘互動統計",
    rateLimited: "摸太快啦，休息一下～",
    changeTip: "換一套新裝，繼續陪你閱讀。",
    copyTip: "如果轉載，也請記得註明出處。",
    toolHome: "返回首頁",
    toolRSS: "訂閱部落格",
    toolChat: "說句話",
    toolChange: "切換模型與換裝",
    toolPhoto: "儲存圖片",
    toolGithub: "查看專案主頁",
    toolClose: "關閉看板娘",
  },
  "en-US": {
    greeting: "Welcome to this writing space! Click me for a head-pat~",
    ariaLabel: "Studio Mascot",
    dashboardTitle: "Mascot Head-Pats",
    rateLimited: "Gentle pats please~",
    changeTip: "Changed into a new outfit to keep reading with you~",
    copyTip: "Copied! Please remember to credit the source when sharing.",
    toolHome: "Back to Home",
    toolRSS: "Subscribe via RSS",
    toolChat: "Say Something",
    toolChange: "Switch Model / Outfit",
    toolPhoto: "Take Screenshot",
    toolGithub: "View Project",
    toolClose: "Hide Mascot",
  },
  "ja-JP": {
    greeting: "個人ブログへようこそ！クリックすると頭をなでられます～",
    ariaLabel: "スタジオ看板娘",
    dashboardTitle: "看板娘なでなで統計",
    rateLimited: "ゆっくりなでてね～",
    changeTip: "新しい衣装に着替えました。一緒に読書を続けましょう～",
    copyTip: "転載の際は出典の明記をお願いします。",
    toolHome: "ホームへ戻る",
    toolRSS: "RSSを購読",
    toolChat: "ひとこと話す",
    toolChange: "モデル・衣装切替",
    toolPhoto: "スクリーンショット保存",
    toolGithub: "プロジェクトを見る",
    toolClose: "看板娘を閉じる",
  },
};

function normalizeLocaleKey(locale) {
  var loc = String(locale || "").toLowerCase();
  if (loc.indexOf("en") === 0) return "en-US";
  if (loc.indexOf("ja") === 0) return "ja-JP";
  if (loc === "zh-tw" || loc.indexOf("zh-hant") === 0) return "zh-TW";
  return "zh-CN";
}

function sanitizeProjectUrl(raw) {
  var s = String(raw || "").trim();
  if (/^https?:\/\/[^\s"'<>]+$/i.test(s)) {
    return s;
  }
  return "https://github.com/Willh92/Volo-go";
}

function containsDotDotSegment(relPath) {
  var parts = relPath.split("/");
  for (var i = 0; i < parts.length; i++) {
    if (parts[i] === "..") return true;
  }
  return false;
}

function assertLocalAssetExists(relPath) {
  if (typeof relPath !== "string") {
    throw new Error("Invalid model resource");
  }
  if (relPath.trim() === "") {
    throw new Error("Invalid model resource");
  }
  if (
    relPath.charAt(0) === "/" ||
    relPath.indexOf(":") >= 0 ||
    relPath.indexOf("\\") >= 0 ||
    relPath.indexOf("?") >= 0 ||
    relPath.indexOf("#") >= 0 ||
    relPath.indexOf("%") >= 0 ||
    containsDotDotSegment(relPath)
  ) {
    throw new Error("Non-local model resource");
  }
  if (!volo.assets || !volo.assets.exists("assets/" + relPath)) {
    throw new Error("Missing bundled model resource");
  }
}

function validateDescriptorValue(value, key, outPaths) {
  if (Array.isArray(value)) {
    for (var i = 0; i < value.length; i++) {
      validateDescriptorValue(value[i], key, outPaths);
    }
    return;
  }
  if (value && typeof value === "object") {
    var keys = Object.keys(value);
    for (var k = 0; k < keys.length; k++) {
      var childKey = keys[k];
      validateDescriptorValue(value[childKey], childKey, outPaths);
    }
    return;
  }
  if (RESOURCE_KEYS[key]) {
    assertLocalAssetExists(value);
    if (outPaths) {
      outPaths.push(value);
    }
  }
}

function allAssetPathsExist(paths) {
  if (!volo.assets) return false;
  for (var i = 0; i < paths.length; i++) {
    if (!volo.assets.exists("assets/" + paths[i])) {
      return false;
    }
  }
  return true;
}

function listBundledModels(verifyAll) {
  var rawManifest = volo.assets.readText("assets/model-list.json");
  if (cachedModelNames && rawManifest === cachedManifestRaw) {
    if (!verifyAll) {
      return cachedModelNames;
    }
    var allStillPresent = true;
    for (var c = 0; c < cachedModelNames.length; c++) {
      if (!volo.assets.exists("assets/model/" + cachedModelNames[c] + "/index.json")) {
        allStillPresent = false;
        break;
      }
    }
    if (allStillPresent) {
      return cachedModelNames;
    }
  }

  var parsed = JSON.parse(rawManifest);
  if (!Array.isArray(parsed)) {
    throw new Error("Invalid model-list.json");
  }
  var names = [];
  var seen = {};
  for (var i = 0; i < parsed.length; i++) {
    var item = parsed[i];
    if (typeof item !== "string" || !MODEL_NAME_RE.test(item)) {
      throw new Error("Invalid model name");
    }
    var indexRel = "model/" + item + "/index.json";
    if (!volo.assets.exists("assets/" + indexRel)) {
      continue;
    }
    if (!seen[item]) {
      seen[item] = true;
      names.push(item);
    }
  }
  if (names.length === 0) {
    throw new Error("No bundled models");
  }
  cachedManifestRaw = rawManifest;
  cachedModelNames = names;
  return names;
}

// Bundled models point directly to .webp in index.json/textures.json.
// Retain optional .webp hint discovery only for custom .png model packs that include companion .webp files.
function buildWebpHints(textures) {
  if (!Array.isArray(textures) || textures.length === 0) {
    return null;
  }
  var hasPng = false;
  for (var p = 0; p < textures.length; p++) {
    if (typeof textures[p] === "string" && /\.png$/i.test(textures[p])) {
      hasPng = true;
      break;
    }
  }
  if (!hasPng) {
    return null;
  }
  var hints = [];
  var hasAny = false;
  for (var i = 0; i < textures.length; i++) {
    var rel = textures[i];
    if (typeof rel === "string" && /\.png$/i.test(rel)) {
      var webpRel = rel.replace(/\.png$/i, ".webp");
      if (
        !containsDotDotSegment(webpRel) &&
        volo.assets.exists("assets/" + webpRel)
      ) {
        hints.push(webpRel);
        hasAny = true;
        continue;
      }
    }
    hints.push(null);
  }
  return hasAny ? hints : null;
}

function shallowCopyObject(src) {
  var out = {};
  var keys = Object.keys(src);
  for (var i = 0; i < keys.length; i++) {
    var k = keys[i];
    out[k] = src[k];
  }
  return out;
}

function pickModelName(names, requested, excludeName) {
  if (
    typeof requested === "string" &&
    requested !== "" &&
    requested !== "random" &&
    requested !== "default" &&
    names.indexOf(requested) >= 0
  ) {
    return requested;
  }
  if (names.length > 1 && excludeName && names.indexOf(excludeName) >= 0) {
    var pool = [];
    for (var p = 0; p < names.length; p++) {
      if (names[p] !== excludeName) pool.push(names[p]);
    }
    return pool[Math.floor(Math.random() * pool.length)];
  }
  return names[Math.floor(Math.random() * names.length)];
}

function selectModelDescriptorJson(requested, excludeName) {
  var names = listBundledModels(false);
  var chosenName = pickModelName(names, requested, excludeName);
  var dirRel = "model/" + chosenName + "/";
  var rawIndex;
  try {
    rawIndex = volo.assets.readText("assets/" + dirRel + "index.json");
  } catch (readErr) {
    var isRandomReq =
      !requested || requested === "random" || requested === "default";
    if (!isRandomReq) {
      throw readErr;
    }
    names = listBundledModels(true);
    chosenName = pickModelName(names, requested, excludeName);
    dirRel = "model/" + chosenName + "/";
    rawIndex = volo.assets.readText("assets/" + dirRel + "index.json");
  }

  var nowMs = Date.now();
  var cached = descriptorCache[chosenName];

  if (cached && cached.rawIndex === rawIndex) {
    if (!cached.variants) {
      if (nowMs - cached.verifiedAtMs <= ASSET_VERIFY_TTL_MS) {
        return cached.serializedStatic;
      }
      if (allAssetPathsExist(cached.basePaths)) {
        cached.verifiedAtMs = nowMs;
        return cached.serializedStatic;
      }
      delete descriptorCache[chosenName];
      throw new Error("Missing bundled model resource");
    }

    var currentRawTex = volo.assets.readText("assets/" + dirRel + "textures.json");
    if (currentRawTex === cached.rawTextures) {
      if (nowMs - cached.verifiedAtMs > ASSET_VERIFY_TTL_MS) {
        if (!allAssetPathsExist(cached.basePaths)) {
          delete descriptorCache[chosenName];
          throw new Error("Missing bundled model resource");
        }
        cached.verifiedAtMs = nowMs;
      }
      var vIdx = Math.floor(Math.random() * cached.variants.length);
      var cachedVarJson = cached.serializedVariants[vIdx];
      if (cachedVarJson) {
        if (nowMs - cached.variantVerifiedAtMs[vIdx] <= ASSET_VERIFY_TTL_MS) {
          return cachedVarJson;
        }
        if (allAssetPathsExist(cached.variants[vIdx])) {
          cached.variantVerifiedAtMs[vIdx] = nowMs;
          return cachedVarJson;
        }
        delete descriptorCache[chosenName];
        throw new Error("Missing bundled model resource");
      }
      var chosenVar = cached.variants[vIdx];
      validateDescriptorValue(chosenVar, "textures", null);
      var fastVar = shallowCopyObject(cached.baseModel);
      fastVar.textures = chosenVar;
      fastVar.name = chosenName;
      fastVar.asset_base = "/plugins/kanbanniang/assets/";
      fastVar.asset_version = PLUGIN_VERSION;
      var varHints = buildWebpHints(chosenVar);
      if (varHints) fastVar.textures_webp = varHints;
      var serializedVar = JSON.stringify(fastVar);
      cached.serializedVariants[vIdx] = serializedVar;
      cached.variantVerifiedAtMs[vIdx] = nowMs;
      return serializedVar;
    }
  }

  var baseModel = JSON.parse(rawIndex);
  if (!baseModel || typeof baseModel !== "object" || Array.isArray(baseModel)) {
    throw new Error("Invalid model index.json");
  }

  if (Array.isArray(baseModel.textures) && baseModel.textures.length > 0) {
    var staticPaths = [];
    validateDescriptorValue(baseModel, "", staticPaths);
    var outStatic = shallowCopyObject(baseModel);
    outStatic.name = chosenName;
    outStatic.asset_base = "/plugins/kanbanniang/assets/";
    outStatic.asset_version = PLUGIN_VERSION;
    var outStaticHints = buildWebpHints(outStatic.textures);
    if (outStaticHints) outStatic.textures_webp = outStaticHints;
    var serializedStatic = JSON.stringify(outStatic);
    descriptorCache[chosenName] = {
      rawIndex: rawIndex,
      rawTextures: null,
      basePaths: staticPaths,
      verifiedAtMs: nowMs,
      serializedStatic: serializedStatic,
      variants: null,
    };
    return serializedStatic;
  }

  var rawTextures = volo.assets.readText("assets/" + dirRel + "textures.json");
  var parsedVariants = JSON.parse(rawTextures);
  if (!Array.isArray(parsedVariants) || parsedVariants.length === 0) {
    throw new Error("Model has no textures");
  }
  var normalizedVariants = [];
  for (var v = 0; v < parsedVariants.length; v++) {
    var entry = parsedVariants[v];
    var arr = Array.isArray(entry) ? entry.slice() : [entry];
    if (arr.length === 0) {
      throw new Error("Model variant has empty textures");
    }
    normalizedVariants.push(arr);
  }

  baseModel.textures = [];
  var basePaths = [];
  validateDescriptorValue(baseModel, "", basePaths);

  var pickIdx = Math.floor(Math.random() * normalizedVariants.length);
  var pickedTextures = normalizedVariants[pickIdx];
  validateDescriptorValue(pickedTextures, "textures", null);

  var outVar = shallowCopyObject(baseModel);
  outVar.textures = pickedTextures;
  outVar.name = chosenName;
  outVar.asset_base = "/plugins/kanbanniang/assets/";
  outVar.asset_version = PLUGIN_VERSION;
  var outVarHints = buildWebpHints(pickedTextures);
  if (outVarHints) outVar.textures_webp = outVarHints;
  var firstSerialized = JSON.stringify(outVar);

  var serializedVariants = {};
  var variantVerifiedAtMs = {};
  serializedVariants[pickIdx] = firstSerialized;
  variantVerifiedAtMs[pickIdx] = nowMs;

  descriptorCache[chosenName] = {
    rawIndex: rawIndex,
    rawTextures: rawTextures,
    baseModel: baseModel,
    basePaths: basePaths,
    verifiedAtMs: nowMs,
    variants: normalizedVariants,
    serializedVariants: serializedVariants,
    variantVerifiedAtMs: variantVerifiedAtMs,
  };
  return firstSerialized;
}

function safeSettings(locale) {
  var cfg = (volo.config && volo.config.all()) || {};
  var rawModel = String(cfg.model || cfg.character || "random").trim();
  if (rawModel !== "random" && !MODEL_NAME_RE.test(rawModel)) {
    rawModel = "random";
  }
  var pos = String(cfg.position || "left");
  if (!ALLOWED_POSITIONS[pos]) pos = "left";

  var locKey = normalizeLocaleKey(locale);
  var msg = LOCALE_STRINGS[locKey] || LOCALE_STRINGS["zh-CN"];

  var zhCN = String(cfg.greeting_zh || LOCALE_STRINGS["zh-CN"].greeting).slice(0, 160);
  var zhTW = String(
    cfg.greeting_tw || cfg.greeting_zh || LOCALE_STRINGS["zh-TW"].greeting
  ).slice(0, 160);
  var enUS = String(cfg.greeting_en || LOCALE_STRINGS["en-US"].greeting).slice(0, 160);
  var jaJP = String(cfg.greeting_ja || LOCALE_STRINGS["ja-JP"].greeting).slice(0, 160);
  var greeting =
    locKey === "en-US"
      ? enUS
      : locKey === "ja-JP"
        ? jaJP
        : locKey === "zh-TW"
          ? zhTW
          : zhCN;

  return {
    locale: locKey,
    model: rawModel,
    position: pos,
    sakuraBackground: cfg.sakura_background !== false,
    idleChat: cfg.idle_chat !== false,
    muteSound: cfg.mute_sound === true,
    hideOnMobile: cfg.hide_on_mobile !== false,
    projectUrl: sanitizeProjectUrl(cfg.project_url),
    greeting: greeting,
    greetingZh: zhCN,
    greetingTw: zhTW,
    greetingEn: enUS,
    greetingJa: jaJP,
    msg: msg,
  };
}

function renderToolbarButton(toolKey, svgId, titleText, viewBox, pathD) {
  return (
    '<button type="button" data-kbn-tool="' +
    toolKey +
    '" title="' +
    escapeHtml(titleText) +
    '" aria-label="' +
    escapeHtml(titleText) +
    '"><svg id="' +
    svgId +
    '" viewBox="' +
    viewBox +
    '" width="18" height="18" aria-hidden="true"><path d="' +
    pathD +
    '"></path></svg></button>'
  );
}

var LOCALE_TOOLS_HTML = (function () {
  var out = {};
  var keys = ["zh-CN", "zh-TW", "en-US", "ja-JP"];
  for (var i = 0; i < keys.length; i++) {
    var k = keys[i];
    var m = LOCALE_STRINGS[k];
    out[k] =
      renderToolbarButton(
        "home",
        "soloKanbanniangHome",
        m.toolHome,
        "0 0 32 32",
        "M32 18.967l-16-12.42-16 12.42v-5.064l16-12.42 16 12.42zM28 18.516v12h-8v-8h-8v8h-8v-12l12-9z"
      ) +
      renderToolbarButton(
        "rss",
        "soloKanbanniangRSS",
        m.toolRSS,
        "0 0 1024 1024",
        "M320.364 768q0 45.728-32 77.728t-77.728 32-77.728-32-32-77.728 32-77.728 77.728-32 77.728 32 32 77.728zM612.94 838.272q1.152 16-9.728 27.424-10.272 12-26.848 12h-77.152q-14.272 0-24.576-9.44t-11.424-23.712q-12.576-130.848-105.44-223.712t-223.712-105.44q-14.272-1.152-23.712-11.424t-9.44-24.576V402.24q0-16.576 12-26.848 9.728-9.728 24.576-9.728h2.848q91.424 7.424 174.848 46.016t148 103.712q65.152 64.576 103.712 148t46.016 174.848z m292.576 1.152q1.152 15.424-10.272 26.848-10.272 11.424-26.272 11.424h-81.728q-14.848 0-25.44-10.016t-11.136-24.288q-6.848-122.848-57.728-233.44t-132.288-192-192-132.288-233.44-58.272q-14.272-0.576-24.288-11.136t-10.016-24.864V109.664q0-16 11.424-26.272 10.272-10.272 25.152-10.272h1.728q149.728 7.424 286.56 68.576t243.136 168q106.848 106.272 168 243.136t68.576 286.56z"
      ) +
      renderToolbarButton(
        "chat",
        "soloKanbanniangChat",
        m.toolChat,
        "0 0 1024 1024",
        "M802.42709 96.163153H219.476155c-84.48109 0-154.896836 71.746044-154.896836 157.840888v393.119449c0 86.072331 70.415746 157.819398 154.896836 157.819399h214.038818V925.470963s22.526039 40.168862 64.767096 5.734608c30.965246-25.819039 126.721123-91.828428 171.775248-123.385145h132.369773c84.502579 0 154.896836-83.21526 154.896836-157.839865V251.125481c0-86.094844-70.394257-154.962328-154.896836-154.962328zM301.144176 518.002714c-39.427988 0-70.416769-31.576159-70.416769-71.746044 0-40.168862 30.988782-71.746044 70.416769-71.746044 39.426965 0 70.393233 31.577183 70.393234 71.746044 0 40.169885-30.966269 71.746044-70.393234 71.746044z m208.411657 0c-39.450501 0-70.415746-31.576159-70.415746-71.746044 0-40.168862 30.965246-71.746044 70.415746-71.746044 39.405475 0 70.394257 31.577183 70.394257 71.746044 0 40.169885-30.988782 71.746044-70.394257 71.746044z m211.203236 0c-39.426965 0-70.416769-31.576159-70.416769-71.746044 0-40.168862 30.988782-71.746044 70.416769-71.746044s70.415746 31.577183 70.415746 71.746044c-0.001023 40.169885-30.988782 71.746044-70.415746 71.746044z"
      ) +
      renderToolbarButton(
        "change",
        "soloKanbanniangChange",
        m.toolChange,
        "0 0 1024 1024",
        "M936.672 193.216l-226.88-64c-8.704-2.528-18.112-1.12-25.824 3.776-7.68 4.864-12.896 12.736-14.432 21.728C655.712 236.928 595.328 288 512 288c-71.424 0-142.464-103.296-163.776-143.104-7.136-13.28-22.528-19.84-37.024-15.68l-224 64C73.472 197.152 64 209.728 64 224v256a31.93 31.93 0 0 0 11.712 24.736c7.392 6.08 17.152 8.512 26.56 6.624L224 487.04V832c0 52.928 43.072 96 96 96h384c52.928 0 96-43.072 96-96V519.04l121.728 24.352c9.44 1.92 19.2-0.544 26.56-6.624C955.68 530.656 960 521.6 960 512V224c0-14.336-9.536-26.912-23.328-30.784zM672 800H352c-17.664 0-32-14.304-32-32s14.336-32 32-32h320c17.696 0 32 14.304 32 32s-14.304 32-32 32z"
      ) +
      renderToolbarButton(
        "photo",
        "soloKanbanniangPhoto",
        m.toolPhoto,
        "0 0 1024 1024",
        "M898.048 258.048q23.552-1.024 46.592 9.216t40.96 27.136 28.672 39.424 10.752 46.08l0 390.144q0 24.576-10.752 47.104t-28.672 40.448-40.96 28.16-47.616 10.24l-697.344 0q-24.576 0-48.64-10.24t-42.496-27.648-29.696-40.448-11.264-48.64l0-381.952q0-22.528 10.752-45.568t28.672-41.472 39.936-30.208 44.544-11.776l63.488 0 13.312-83.968q3.072-20.48 18.432-32.768t34.816-12.288l456.704 0q19.456 0 34.304 10.752t16.896 34.304l14.336 83.968 54.272 0zM548.864 712.704q40.96 0 77.824-15.872t63.488-42.496 42.496-62.976 15.872-77.312-15.872-77.312-42.496-62.976-63.488-42.496-77.824-15.872-77.312 15.872-63.488 42.496-43.008 62.976-15.872 77.312 15.872 77.312 43.008 62.976 63.488 42.496 77.312 15.872z"
      ) +
      renderToolbarButton(
        "github",
        "soloKanbanniangGithub",
        m.toolGithub,
        "0 0 1024 1024",
        "M1024 524.8c0 114.346667-32.554667 217.216-97.706667 308.565333-65.066667 91.306667-149.162667 154.538667-252.288 189.610667-11.989333 2.304-20.778667 0.682667-26.325333-4.778667a27.605333 27.605333 0 0 1-8.362667-20.48v-144.213333c0-44.16-11.52-76.501333-34.645333-97.024 25.344-2.730667 48.085333-6.826667 68.309333-12.288a268.629333 268.629333 0 0 0 62.72-26.666667 187.434667 187.434667 0 0 0 53.973334-45.44c14.421333-18.005333 26.197333-41.898667 35.328-71.765333 9.088-29.824 13.653333-64.128 13.653333-102.826667 0-55.125333-17.536-102.058667-52.650667-140.8 16.426667-41.429333 14.677333-87.893333-5.333333-139.392-12.458667-4.096-30.464-1.578667-54.016 7.509334a355.328 355.328 0 0 0-61.312 30.08L640 271.274667a462.336 462.336 0 0 0-128-17.749334c-43.989333 0-86.656 5.930667-128 17.749334a589.824 589.824 0 0 0-28.330667-18.432c-11.776-7.253333-30.336-16.042667-55.68-26.325334-25.344-10.24-44.416-13.312-57.301333-9.216-19.584 51.498667-21.12 97.962667-4.693333 139.434667-35.114667 38.698667-52.650667 85.632-52.650667 140.757333 0 38.698667 4.565333 72.874667 13.653333 102.485334 9.130667 29.610667 20.778667 53.546667 34.986667 71.765333 14.250667 18.218667 32.128 33.493333 53.674667 45.781333 21.546667 12.288 42.453333 21.205333 62.677333 26.666667 20.224 5.461333 43.008 9.557333 68.309333 12.288-17.749333 16.384-28.629333 39.850667-32.64 70.4a130.005333 130.005333 0 0 1-29.994666 10.24c-10.666667 2.261333-23.338667 3.413333-37.973334 3.413333-14.72 0-29.269333-4.906667-43.690666-14.677333-14.464-9.813333-26.794667-24.064-36.992-42.709333a109.226667 109.226667 0 0 0-32.341334-35.541334c-13.141333-9.130667-24.106667-14.592-33.024-16.426666l-13.312-2.048c-9.344 0-15.786667 1.024-19.328 3.072-3.584 2.090667-4.693333 4.693333-3.328 7.893333 1.28 3.157333 3.328 6.4 5.973334 9.557333 2.688 3.2 5.546667 5.930667 8.661333 8.192l4.693333 3.413334c9.770667 4.565333 19.413333 13.226667 29.013334 25.984 9.514667 12.757333 16.512 24.362667 20.992 34.858666l6.656 15.701334c5.76 17.322667 15.530667 31.317333 29.312 42.026666 13.781333 10.666667 28.672 17.536 44.672 20.48 16 2.986667 31.445333 4.565333 46.336 4.821334 14.890667 0.213333 27.221333-0.597333 36.992-2.389334l15.36-2.730666c0 17.28 0.085333 37.546667 0.298666 60.8l0.341334 36.906666a27.050667 27.050667 0 0 1-8.661334 20.48c-5.76 5.461333-14.677333 7.082667-26.666666 4.778667-103.125333-35.072-187.221333-98.261333-252.330667-189.610667C32.554667 742.058667 0 639.146667 0 524.8c0-95.232 22.869333-183.04 68.693333-263.466667A516.266667 516.266667 0 0 1 254.976 70.4C333.44 23.466667 419.114667 0 512 0c92.885333 0 178.56 23.466667 256.981333 70.4a516.266667 516.266667 0 0 1 186.368 190.976C1001.130667 341.802667 1024 429.653333 1024 524.842667z"
      ) +
      renderToolbarButton(
        "close",
        "soloKanbanniangClose",
        m.toolClose,
        "0 0 1024 1024",
        "M517.573 1018.675C238.266 1018.675 11.898 792.271 11.898 513c0-279.307 226.368-505.71 505.675-505.71 279.271 0 505.674 226.403 505.674 505.71 0 279.271-226.368 505.675-505.674 505.675zM754.728 339.25c13.753-13.753 9.681-40.187-9.073-58.94l-2.143-2.108c-18.754-18.754-45.152-22.862-58.905-9.073L516.394 437.378 335.964 256.912c-13.967-13.967-40.722-9.823-59.726 9.216l-2.143 2.144c-19.04 19.004-23.112 45.759-9.181 59.655l180.43 180.465-176.072 176.036c-13.788 13.824-9.716 40.151 9.073 58.94l2.108 2.108c18.754 18.754 45.152 22.898 58.905 9.073L515.465 578.442l170.392 170.428c13.967 13.931 40.687 9.859 59.691-9.18l2.179-2.18c19.04-18.968 23.148-45.723 9.18-59.69L586.515 507.392 754.728 339.25z"
      );
  }
  return out;
})();

function onEnable() {
  volo.log.info("Live2D mascot companion enabled (v" + PLUGIN_VERSION + ")");
}

function renderSlot(slotName, slotCtx) {
  var locale = (slotCtx && slotCtx.locale) || "zh-CN";
  var st = safeSettings(locale);
  var pats = Number(volo.kv.getInt("total_pats") || 0);

  if (slotName === "admin.dashboard_card") {
    return (
      '<div class="kbn-admin-stat"><strong>' +
      escapeHtml(st.msg.dashboardTitle) +
      ":</strong> <span>" +
      pats +
      "</span> (" +
      escapeHtml(st.model) +
      ")</div>"
    );
  }

  if (slotName === "public.footer") {
    var cls =
      "volo-kbn-dock solo-kanbanniang volo-kbn-" +
      st.position +
      (st.hideOnMobile ? " volo-kbn-hide-mobile" : "");

    var toolsHtml = LOCALE_TOOLS_HTML[st.locale] || LOCALE_TOOLS_HTML["zh-CN"];
    var initialDescAttr = "";
    if (st.model && st.model !== "random") {
      try {
        var descJson = selectModelDescriptorJson(st.model, "");
        if (descJson) {
          initialDescAttr =
            ' data-initial-descriptor="' + escapeHtml(descJson) + '"';
        }
      } catch (err) {}
    }

    return (
      '<aside id="volo-kanbanniang-root" class="' +
      cls +
      '" hidden data-volo-persist="kanbanniang" data-version="' +
      PLUGIN_VERSION +
      '" data-static-base="/plugins/kanbanniang/assets" data-api-base="/plugins/kanbanniang/api" data-position="' +
      st.position +
      '" data-model="' +
      st.model +
      '"' +
      initialDescAttr +
      ' data-sakura="' +
      (st.sakuraBackground ? "1" : "0") +
      '" data-idle-chat="' +
      (st.idleChat ? "1" : "0") +
      '" data-mute-sound="' +
      (st.muteSound ? "1" : "0") +
      '" data-hide-mobile="' +
      (st.hideOnMobile ? "1" : "0") +
      '" data-project-url="' +
      escapeHtml(st.projectUrl) +
      '" data-greeting="' +
      escapeHtml(st.greeting) +
      '" data-greeting-zh="' +
      escapeHtml(st.greetingZh) +
      '" data-greeting-tw="' +
      escapeHtml(st.greetingTw) +
      '" data-greeting-en="' +
      escapeHtml(st.greetingEn) +
      '" data-greeting-ja="' +
      escapeHtml(st.greetingJa) +
      '" data-rate-limited="' +
      escapeHtml(st.msg.rateLimited) +
      '" data-change-tip="' +
      escapeHtml(st.msg.changeTip) +
      '" data-copy-tip="' +
      escapeHtml(st.msg.copyTip) +
      '" data-pats="' +
      pats +
      '" aria-label="' +
      escapeHtml(st.msg.ariaLabel) +
      '">' +
      '<div class="solo-kanbanniang__tip" role="status" aria-live="polite"></div>' +
      '<canvas id="soloKanbanniang" width="280" height="250"></canvas>' +
      '<div class="solo-kanbanniang__tool">' +
      toolsHtml +
      "</div>" +
      "</aside>"
    );
  }

  return "";
}

function jsonRawNoStore(status, rawBody) {
  return {
    status: status,
    headers: {
      "Content-Type": "application/json; charset=utf-8",
      "Cache-Control": "no-store",
    },
    body: rawBody,
  };
}

function jsonNoStore(status, payload) {
  return jsonRawNoStore(status, JSON.stringify(payload));
}

function onRequest(req) {
  var sub = String((req && req.path) || "")
    .replace(/^\/+/, "")
    .replace(/\/+$/, "");
  var query = (req && req.query) || {};

  if (req.method === "GET" && sub === "model") {
    var reqModel =
      typeof query.model === "string" &&
      query.model !== "" &&
      query.model !== "default"
        ? query.model
        : safeSettings(query.locale || "zh-CN").model;
    try {
      return jsonRawNoStore(200, selectModelDescriptorJson(reqModel, ""));
    } catch (err) {
      return jsonNoStore(503, {
        sc: false,
        msg: "Bundled model resources are unavailable",
      });
    }
  }

  if (
    req.method === "GET" &&
    (sub === "absoluteRandomModel" || sub === "random-model")
  ) {
    try {
      return jsonRawNoStore(
        200,
        selectModelDescriptorJson("", String(query.exclude || ""))
      );
    } catch (err) {
      return jsonNoStore(503, {
        sc: false,
        msg: "Bundled model resources are unavailable",
      });
    }
  }

  if (req.method === "GET" && sub === "list") {
    if (!req.is_admin) {
      return jsonNoStore(401, { sc: false, msg: "Unauthorized" });
    }
    try {
      var names = listBundledModels(true);
      return jsonNoStore(200, {
        sc: true,
        msg: names.join(";"),
        models: names,
      });
    } catch (err) {
      return jsonNoStore(503, {
        sc: false,
        msg: "Bundled model resources are unavailable",
      });
    }
  }

  if (req.method === "POST" && (sub === "pat" || sub === "")) {
    var ipKey = "pat:" + String((req && req.client_ip_masked) || "anon");
    if (
      volo.rateLimit &&
      typeof volo.rateLimit.allow === "function" &&
      !volo.rateLimit.allow(ipKey, 20, 60)
    ) {
      return jsonNoStore(429, { ok: false, error: "rate limit exceeded" });
    }
    var total =
      typeof volo.kv.incrAsync === "function"
        ? volo.kv.incrAsync("total_pats", 1)
        : volo.kv.incr("total_pats", 1, 0);
    return jsonNoStore(200, { ok: true, total_pats: total });
  }

  if (req.method === "GET" && (sub === "state" || sub === "")) {
    var st = safeSettings(query.locale || "zh-CN");
    return jsonNoStore(200, {
      ok: true,
      model: st.model,
      character: st.model,
      position: st.position,
      total_pats: Number(volo.kv.getInt("total_pats") || 0),
    });
  }

  return jsonNoStore(404, { error: "not found" });
}

module.exports = {
  onEnable: onEnable,
  renderSlot: renderSlot,
  onRequest: onRequest,
};
