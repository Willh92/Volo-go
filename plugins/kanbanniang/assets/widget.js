/* Volo-go Live2D Kanbanniang Controller
 * Pure-WebP bundled textures & sakura animations (with custom PNG pack compatibility),
 * zero-inline CSP data-* bootstrap, touch toolbar toggle, viewport-clamped speech bubble,
 * WebGL context recovery, PJAX persistence, pointer drag, tips.json, and KV head-pats.
 */
(function () {
  "use strict";

  var ALPHA_WEBP_URI =
    "data:image/webp;base64,UklGRh4AAABXRUJQVlA4TBEAAAAvAUAAEAfQs5Y0qoCBiOh/AAA=";
  var ANIM_WEBP_URI =
    "data:image/webp;base64,UklGRoQAAABXRUJQVlA4WAoAAAASAAAAAQAAAQAAQU5JTQYAAAD/////AABBTk1GKAAAAAAAAAAAAAEAAAEAAGQAAAJWUDhMDwAAAC8BQAAQBxD9jwIGIqL/AQBBTk1GKAAAAAAAAAAAAAEAAAEAAGQAAAJWUDhMDwAAAC8BQAAQBxDR/wIGIqL/AQA=";
  var OVERLAY_OPEN_SELECTOR =
    "[data-book-reader], .lightbox-overlay.is-open, #volo-lightbox.is-open, [data-search-modal]:not([hidden]), #volo-search-modal:not([hidden]), #volo-toc-drawer:not([hidden]), .studio-reader-drawer.is-open, .volo-comment-composer.is-open, dialog[open], body.modal-open";

  var host =
    (typeof document.getElementById === "function" &&
      (document.getElementById("volo-kanbanniang-root") ||
        document.getElementById("volo-kanbanniang-dock"))) ||
    document.querySelector(".solo-kanbanniang");

  if (!host) return;

  if (
    !host.parentElement ||
    !host.parentElement.hasAttribute("data-volo-persist")
  ) {
    host.setAttribute("data-volo-persist", "kanbanniang");
  }

  if (
    window.VoloKanban &&
    typeof window.VoloKanban.isAlive === "function" &&
    window.VoloKanban.isAlive() &&
    host.isConnected &&
    host.getAttribute("data-mounted") === "1"
  ) {
    if (typeof window.VoloKanban.syncLocale === "function") {
      window.VoloKanban.syncLocale();
    }
    return;
  }
  if (window.VoloKanban && typeof window.VoloKanban.destroy === "function") {
    window.VoloKanban.destroy();
  }

  host.setAttribute("data-mounted", "1");

  function safeSameOriginMountPath(raw, fallback) {
    var s = String(raw || "").trim();
    if (!s || s.indexOf("://") !== -1 || s.indexOf("//") === 0) {
      return fallback;
    }
    var parts = s.split("/").filter(Boolean);
    if (!parts.length) return fallback;
    return "/" + parts.map(encodeURIComponent).join("/");
  }

  var version = host.getAttribute("data-version") || "2.0.4";
  var staticBase = safeSameOriginMountPath(
    host.getAttribute("data-static-base"),
    "/plugins/kanbanniang/assets"
  );
  var apiBase = safeSameOriginMountPath(
    host.getAttribute("data-api-base"),
    "/plugins/kanbanniang/api"
  );
  var tipEl = host.querySelector(".solo-kanbanniang__tip");

  var LOAD_TIMEOUT_MS = 25000;

  var alive = true;
  var started = false;
  var ready = false;
  var loadFailed = false;
  var canvasTainted = false;
  var initialGreetShown = false;
  var tipsLoaded = false;
  var currentModel = "";
  var failedModel = "";
  var lastDescriptor = null;
  var warmBootPromise = null;
  var retryBtn = null;
  var loadSeq = 0;
  var pendingChangeTip = false;
  var patCount = parseInt(host.getAttribute("data-pats") || "0", 10) || 0;

  var messageTimer = 0;
  var messageUntilMs = 0;
  var seasonTimer = 0;
  var closeTimer = 0;
  var loadTimer = 0;
  var chatTimer = 0;
  var idleHandle = 0;
  var idleIsRaf = false;
  var overlaySyncRaf = 0;
  var dragState = null;
  var lastHoverTarget = null;
  var sakuraPreImg = null;
  var removers = [];
  var canvasRemovers = [];
  var abortCtrl = typeof AbortController === "function" ? new AbortController() : null;
  var preloadBytesMap = Object.create(null);
  var preloadImagesMap = Object.create(null);
  var activePreloadImgs = typeof Set === "function" ? new Set() : null;

  var tips = {
    mouseover: [],
    click: [],
    seasons: [],
    chat: ["谢谢你来看望这个文字空间。"]
  };

  function sGet(key) {
    try {
      return window.sessionStorage.getItem(key);
    } catch (err) {
      return null;
    }
  }

  function sSet(key, val) {
    try {
      window.sessionStorage.setItem(key, String(val));
    } catch (err) {}
  }

  function isClosedInSession() {
    return (
      sGet("volo.kanban.closed") === "1" || sGet("soloKanbanniang") === "close"
    );
  }

  function viewportWidth() {
    return typeof window.innerWidth === "number"
      ? window.innerWidth
      : document.documentElement
        ? document.documentElement.clientWidth
        : 1024;
  }

  function viewportHeight() {
    return typeof window.innerHeight === "number"
      ? window.innerHeight
      : document.documentElement
        ? document.documentElement.clientHeight
        : 768;
  }

  function shouldPauseForMobile() {
    return host.getAttribute("data-hide-mobile") !== "0" && viewportWidth() <= 800;
  }

  function prefersReducedMotion() {
    try {
      return !!(
        window.matchMedia &&
        window.matchMedia("(prefers-reduced-motion: reduce)").matches
      );
    } catch (err) {
      return false;
    }
  }

  function isTouchOrCoarse(ev) {
    if (ev && (ev.pointerType === "touch" || ev.pointerType === "pen")) {
      return true;
    }
    try {
      return !!(
        window.matchMedia &&
        window.matchMedia("(hover: none), (pointer: coarse)").matches
      );
    } catch (err) {
      return false;
    }
  }

  function isOverlayBlocking() {
    try {
      if (document.body && document.body.classList.contains("modal-open")) {
        return true;
      }
      return !!document.querySelector(OVERLAY_OPEN_SELECTOR);
    } catch (err) {
      return false;
    }
  }

  function clampTipToViewport() {
    if (!tipEl || !host) return;
    tipEl.classList.remove("is-flipped-below", "is-clamped-viewport");
    tipEl.style.removeProperty("--kbn-tip-top");
    var hostRect = host.getBoundingClientRect();
    if (!hostRect || (hostRect.width === 0 && hostRect.height === 0)) return;
    var tipHeight = tipEl.offsetHeight || 72;
    var neededSpace = tipHeight + 14;
    if (hostRect.top >= neededSpace) {
      return;
    }
    var spaceBelow = viewportHeight() - hostRect.bottom;
    if (spaceBelow >= neededSpace) {
      tipEl.classList.add("is-flipped-below");
      return;
    }
    var safeTop = Math.round(8 - hostRect.top);
    if (hostRect.height > tipHeight + 8) {
      safeTop = Math.min(safeTop, Math.round(hostRect.height - tipHeight - 4));
    }
    tipEl.style.setProperty("--kbn-tip-top", safeTop + "px");
    tipEl.classList.add("is-clamped-viewport");
  }

  function listen(target, type, fn, opts) {
    if (!target || !target.addEventListener) return;
    target.addEventListener(type, fn, opts || false);
    removers.push(function () {
      try {
        target.removeEventListener(type, fn, opts || false);
      } catch (err) {}
    });
  }

  function sanitizeHttpUrl(raw) {
    var s = String(raw || "").trim();
    if (/^https?:\/\/[^\s"'<>]+$/i.test(s)) return s;
    return "https://github.com/Willh92/Volo-go";
  }

  function resolveLocaleStrings() {
    var lang = String(
      (document.documentElement && document.documentElement.lang) || ""
    ).toLowerCase();
    var isEN = lang.indexOf("en") === 0;
    var isJA = lang.indexOf("ja") === 0;
    var isTW = lang === "zh-tw" || lang.indexOf("zh-hant") === 0;

    var greeting = isEN
      ? host.getAttribute("data-greeting-en") ||
        host.getAttribute("data-greeting") ||
        "Welcome! Click me for a head-pat~"
      : isJA
        ? host.getAttribute("data-greeting-ja") ||
          host.getAttribute("data-greeting") ||
          "個人ブログへようこそ！クリックすると頭をなでられます～"
        : isTW
          ? host.getAttribute("data-greeting-tw") ||
            host.getAttribute("data-greeting") ||
            "歡迎來到這個文字空間，點我可以摸摸頭～"
          : host.getAttribute("data-greeting-zh") ||
            host.getAttribute("data-greeting") ||
            "欢迎来到这个文字空间，点我可以摸摸头～";

    return {
      ariaLabel: isEN
        ? "Studio Mascot"
        : isJA
          ? "スタジオ看板娘"
          : "看板娘助手",
      greeting: greeting,
      rateLimited: isEN
        ? "Gentle pats please~"
        : isJA
          ? "ゆっくりなでてね～"
          : isTW
            ? "摸太快啦，休息一下～"
            : host.getAttribute("data-rate-limited") || "摸太快啦，歇会儿再摸～",
      changeTip: isEN
        ? "Changed into a new outfit to keep reading with you~"
        : isJA
          ? "新しい衣装に着替えました。一緒に読書を続けましょう～"
          : isTW
            ? "換一套新裝，繼續陪你閱讀。"
            : host.getAttribute("data-change-tip") || "换一套新装，继续陪你阅读。",
      copyTip: isEN
        ? "Copied! Please remember to credit the source when sharing."
        : isJA
          ? "転載の際は出典の明記をお願いします。"
          : isTW
            ? "如果轉載，也請記得註明出處。"
            : host.getAttribute("data-copy-tip") || "如果转载，也请记得注明出处。",
      retryLabel: isEN
        ? "Load failed · Retry"
        : isJA
          ? "読み込み失敗 · 再試行"
          : isTW
            ? "載入失敗 · 點擊重試"
            : "加载失败 · 点击重试",
      loadErrorTip: isEN
        ? "Failed to load model. Click to retry or switch outfit~"
        : isJA
          ? "モデルの読み込みに失敗しました。クリックで再試行、または右側のボタンで切替できます～"
          : isTW
            ? "模型載入未完成，點擊重試或點右側換裝～"
            : "模型加载未完成，点击重试或点右侧换装～",
      tools: {
        home: isEN ? "Back to Home" : isJA ? "ホームへ戻る" : isTW ? "返回首頁" : "返回首页",
        rss: isEN ? "Subscribe via RSS" : isJA ? "RSSを購読" : isTW ? "訂閱部落格" : "订阅博客",
        chat: isEN ? "Say Something" : isJA ? "ひとこと話す" : isTW ? "說句話" : "说句话",
        change: isEN ? "Switch Model / Outfit" : isJA ? "モデル・衣装切替" : isTW ? "切換模型與換裝" : "切换模型与换装",
        photo: isEN ? "Take Screenshot" : isJA ? "スクリーンショット保存" : isTW ? "儲存圖片" : "保存图片",
        github: isEN ? "View Project" : isJA ? "プロジェクトを見る" : isTW ? "查看專案主頁" : "查看项目主页",
        close: isEN ? "Hide Mascot" : isJA ? "看板娘を閉じる" : isTW ? "關閉看板娘" : "关闭看板娘"
      }
    };
  }

  function ensureRetryButton() {
    if (retryBtn) return retryBtn;
    if (!host || typeof host.querySelector !== "function") return null;
    retryBtn = host.querySelector("[data-kbn-retry]");
    if (
      !retryBtn &&
      typeof document.createElement === "function" &&
      typeof host.appendChild === "function"
    ) {
      retryBtn = document.createElement("button");
      retryBtn.type = "button";
      retryBtn.className = "solo-kanbanniang__retry";
      retryBtn.setAttribute("data-kbn-retry", "1");
      retryBtn.hidden = true;
      var loc = resolveLocaleStrings();
      retryBtn.textContent = loc.retryLabel;
      retryBtn.setAttribute("aria-label", loc.retryLabel);
      listen(retryBtn, "click", function (ev) {
        if (ev && typeof ev.stopPropagation === "function") {
          ev.stopPropagation();
        }
        retryLoad();
      });
      host.appendChild(retryBtn);
    }
    return retryBtn;
  }

  function syncLocale() {
    if (!host) return;
    var loc = resolveLocaleStrings();
    host.setAttribute("aria-label", loc.ariaLabel);
    var keys = ["home", "rss", "chat", "change", "photo", "github", "close"];
    for (var i = 0; i < keys.length; i++) {
      var k = keys[i];
      var btn = host.querySelector('[data-kbn-tool="' + k + '"]');
      if (btn && loc.tools[k]) {
        btn.title = loc.tools[k];
        btn.setAttribute("aria-label", loc.tools[k]);
      }
    }
    if (retryBtn) {
      retryBtn.textContent = loc.retryLabel;
      retryBtn.setAttribute("aria-label", loc.retryLabel);
    }
    if (isOverlayBlocking()) {
      hideMessage();
    }
    syncOffsetsAndPosition();
  }

  function syncOverlayState() {
    overlaySyncRaf = 0;
    if (!alive) return;
    var blocked = isOverlayBlocking();
    if (blocked) {
      hideMessage();
    }
    if (
      window.VoloLive2D &&
      typeof window.VoloLive2D.setSuspended === "function"
    ) {
      window.VoloLive2D.setSuspended(blocked);
    }
  }

  function scheduleOverlaySync() {
    if (!alive || overlaySyncRaf) return;
    if (typeof window.requestAnimationFrame === "function") {
      overlaySyncRaf = window.requestAnimationFrame(syncOverlayState);
    } else {
      overlaySyncRaf = window.setTimeout(syncOverlayState, 16);
    }
  }

  function resolveTipText(raw) {
    for (var depth = 0; depth < 5 && raw != null; depth++) {
      if (typeof raw === "string") {
        var trimmed = raw.trim();
        return trimmed && trimmed.indexOf("[object ") === -1 ? trimmed : "";
      }
      if (Array.isArray(raw)) {
        if (raw.length === 0) return "";
        raw = raw[Math.floor(Math.random() * raw.length)];
        continue;
      }
      if (typeof raw === "object" && raw.text != null) {
        raw = raw.text;
        continue;
      }
      return "";
    }
    return "";
  }

  function pickRuleTip(rules, targetEl) {
    if (!rules) return "";
    if (typeof rules === "string") return resolveTipText(rules);
    if (!Array.isArray(rules) || rules.length === 0) return "";

    var candidates = [];
    for (var i = 0; i < rules.length; i++) {
      var item = rules[i];
      if (!item) continue;
      if (typeof item === "string") {
        candidates.push(item);
        continue;
      }
      if (typeof item === "object" && item.text != null) {
        if (!item.selector) {
          candidates.push(item.text);
          continue;
        }
        if (targetEl && typeof targetEl.closest === "function") {
          try {
            if (targetEl.closest(item.selector)) {
              return resolveTipText(item.text);
            }
          } catch (err) {}
        } else if (String(item.selector).indexOf("soloKanbanniang") !== -1) {
          candidates.push(item.text);
        }
      }
    }
    return candidates.length > 0 ? resolveTipText(candidates) : "";
  }

  function extractHoverLabel(el) {
    if (!el) return "";
    var txt = String(el.textContent || "")
      .replace(/\s+/g, " ")
      .trim();
    if (!txt && typeof el.getAttribute === "function") {
      txt = String(
        el.getAttribute("aria-label") || el.getAttribute("title") || ""
      )
        .replace(/\s+/g, " ")
        .trim();
    }
    if (!txt && typeof el.querySelector === "function") {
      var img = el.querySelector("img[alt]");
      if (img) {
        txt = String(img.getAttribute("alt") || "")
          .replace(/\s+/g, " ")
          .trim();
      }
    }
    return txt.slice(0, 42);
  }

  function hideMessage() {
    if (!tipEl) return;
    clearTimeout(messageTimer);
    messageUntilMs = 0;
    tipEl.style.opacity = "0";
    tipEl.classList.remove("is-visible");
  }

  function showMessage(text, durationMs) {
    if (!alive || (!ready && !loadFailed) || !tipEl || isClosedInSession()) {
      return;
    }
    if (isOverlayBlocking()) {
      hideMessage();
      return;
    }
    var resolved = resolveTipText(text);
    if (!resolved) return;
    var dur = durationMs || 4000;
    tipEl.textContent = resolved;
    clampTipToViewport();
    tipEl.style.opacity = "1";
    tipEl.classList.add("is-visible");
    clearTimeout(messageTimer);
    messageUntilMs = Date.now() + dur;
    messageTimer = window.setTimeout(function () {
      messageUntilMs = 0;
      if (tipEl) {
        tipEl.style.opacity = "0";
        tipEl.classList.remove("is-visible");
      }
    }, dur);
  }

  function probeImage2x2(dataUri) {
    return new Promise(function (resolve) {
      var settled = false;
      function done(ok) {
        if (!settled) {
          settled = true;
          resolve(ok);
        }
      }
      var timer = window.setTimeout(function () {
        done(false);
      }, 600);
      var img = new Image();
      img.onload = function () {
        window.clearTimeout(timer);
        done(img.width === 2 && img.height === 2);
      };
      img.onerror = function () {
        window.clearTimeout(timer);
        done(false);
      };
      img.src = dataUri;
    });
  }

  function ensureCapabilities() {
    if (!window.__voloKbnCapsPromise) {
      var hasWebGL = !!window.WebGLRenderingContext;
      var cachedCaps = sGet("volo.kanban.caps");
      if (cachedCaps && cachedCaps.indexOf(",") === 1) {
        var parts = cachedCaps.split(",");
        window.__voloKbnCapsPromise = Promise.resolve({
          webglWebp: hasWebGL && parts[0] === "1",
          animWebp: parts[1] === "1"
        });
      } else {
        window.__voloKbnCapsPromise = Promise.all([
          hasWebGL ? probeImage2x2(ALPHA_WEBP_URI) : Promise.resolve(false),
          probeImage2x2(ANIM_WEBP_URI)
        ]).then(function (res) {
          var caps = { webglWebp: !!res[0], animWebp: !!res[1] };
          sSet(
            "volo.kanban.caps",
            (caps.webglWebp ? "1" : "0") + "," + (caps.animWebp ? "1" : "0")
          );
          return caps;
        });
      }
    }
    return window.__voloKbnCapsPromise;
  }

  function clearSakuraPreload() {
    if (sakuraPreImg) {
      sakuraPreImg.onload = sakuraPreImg.onerror = null;
      try {
        sakuraPreImg.removeAttribute("src");
      } catch (err) {}
      sakuraPreImg = null;
    }
  }

  function clearPreload() {
    preloadBytesMap = Object.create(null);
    preloadImagesMap = Object.create(null);
    if (activePreloadImgs) {
      activePreloadImgs.forEach(function (im) {
        if (im) {
          im.onload = im.onerror = null;
          try {
            im.removeAttribute("src");
          } catch (err) {}
        }
      });
      activePreloadImgs.clear();
    }
  }

  function resolveDescriptorAssetUrl(descriptor, relPath) {
    if (!relPath) return "";
    if (/^https?:\/\/|^\/\//i.test(relPath)) return relPath;
    var baseRaw =
      (descriptor && descriptor.asset_base) || staticBase + "/";
    var homeDir = baseRaw;
    try {
      homeDir = new URL(baseRaw, window.location.href).href;
    } catch (err) {}
    var url = homeDir + relPath;
    var ver = descriptor && descriptor.asset_version;
    if (ver && url.indexOf("?") < 0) {
      url += "?v=" + encodeURIComponent(ver);
    }
    return url;
  }

  function prefetchBytes(url) {
    if (!url || preloadBytesMap[url] || typeof window.fetch !== "function") {
      return;
    }
    var opts = { credentials: "same-origin" };
    if (abortCtrl) opts.signal = abortCtrl.signal;
    var p = window.fetch(url, opts).then(function (resp) {
      if (!resp.ok) throw new Error("Asset HTTP " + resp.status);
      return resp.arrayBuffer();
    });
    p.catch(function () {});
    preloadBytesMap[url] = p;
  }

  function prefetchImage(url) {
    if (!url || preloadImagesMap[url]) return;
    var p = new Promise(function (resolve, reject) {
      var img = new Image();
      if (activePreloadImgs) activePreloadImgs.add(img);
      img.crossOrigin = "Anonymous";
      img.decoding = "async";
      img.onload = function () {
        if (activePreloadImgs) activePreloadImgs.delete(img);
        img.onload = img.onerror = null;
        resolve(img);
      };
      img.onerror = function () {
        if (activePreloadImgs) activePreloadImgs.delete(img);
        img.onload = img.onerror = null;
        try {
          img.removeAttribute("src");
        } catch (err) {}
        reject(new Error("Texture preload failed"));
      };
      img.src = url;
    });
    p.catch(function () {});
    preloadImagesMap[url] = p;
  }

  function prefetchDescriptor(descriptor, useWebp) {
    if (!descriptor || typeof descriptor !== "object") return;
    if (descriptor.model) {
      prefetchBytes(resolveDescriptorAssetUrl(descriptor, descriptor.model));
    }
    if (descriptor.physics) {
      prefetchBytes(resolveDescriptorAssetUrl(descriptor, descriptor.physics));
    }
    if (descriptor.pose) {
      prefetchBytes(resolveDescriptorAssetUrl(descriptor, descriptor.pose));
    }
    if (
      descriptor.motions &&
      Array.isArray(descriptor.motions.idle) &&
      descriptor.motions.idle.length > 0
    ) {
      var idleList = descriptor.motions.idle;
      var idleIdx =
        typeof descriptor._initIdleIdx === "number" &&
        descriptor._initIdleIdx >= 0 &&
        descriptor._initIdleIdx < idleList.length
          ? descriptor._initIdleIdx
          : Math.floor(Math.random() * idleList.length);
      descriptor._initIdleIdx = idleIdx;
      if (idleList[idleIdx] && idleList[idleIdx].file) {
        prefetchBytes(
          resolveDescriptorAssetUrl(descriptor, idleList[idleIdx].file)
        );
      }
    }
    if (Array.isArray(descriptor.textures)) {
      for (var i = 0; i < descriptor.textures.length; i++) {
        var webpRel =
          useWebp &&
          Array.isArray(descriptor.textures_webp) &&
          descriptor.textures_webp[i]
            ? descriptor.textures_webp[i]
            : null;
        var texRel = webpRel || descriptor.textures[i];
        if (texRel) {
          prefetchImage(resolveDescriptorAssetUrl(descriptor, texRel));
        }
      }
    }
  }

  window.__voloKbnPreload = {
    hasBytes: function (url) {
      return !!preloadBytesMap[url];
    },
    takeBytes: function (url) {
      var p = preloadBytesMap[url] || null;
      if (p) delete preloadBytesMap[url];
      return p;
    },
    hasImage: function (url) {
      return !!preloadImagesMap[url];
    },
    takeImage: function (url) {
      var p = preloadImagesMap[url] || null;
      if (p) delete preloadImagesMap[url];
      return p;
    },
    clear: clearPreload
  };

  function loadSDK() {
    if (typeof window.loadlive2d === "function") {
      return Promise.resolve();
    }
    if (!window.__voloKanbanSDK) {
      window.__voloKanbanSDK = new Promise(function (resolve, reject) {
        var script = document.createElement("script");
        script.src =
          staticBase + "/live2d.js?v=" + encodeURIComponent(version);
        script.async = true;
        script.onload = function () {
          if (typeof window.loadlive2d === "function") resolve();
          else reject(new Error("Live2D runtime failed to initialize"));
        };
        script.onerror = function () {
          if (script.parentNode) script.parentNode.removeChild(script);
          reject(new Error("Local Live2D script is unavailable"));
        };
        document.head.appendChild(script);
      }).catch(function (err) {
        window.__voloKanbanSDK = null;
        throw err;
      });
    }
    return window.__voloKanbanSDK;
  }

  function consumeInitialDescriptor() {
    var raw = host.getAttribute("data-initial-descriptor");
    if (!raw) return null;
    host.removeAttribute("data-initial-descriptor");
    try {
      var parsed = JSON.parse(raw);
      if (parsed && typeof parsed === "object" && parsed.model) {
        return Promise.resolve(parsed);
      }
    } catch (err) {}
    return null;
  }

  function fetchModelDescriptor(isRandomSwitch, explicitModel) {
    if (!isRandomSwitch && !explicitModel) {
      var inlined = consumeInitialDescriptor();
      if (inlined) return inlined;
    }
    var endpoint = isRandomSwitch ? "/absoluteRandomModel" : "/model";
    var targetModel =
      explicitModel || host.getAttribute("data-model") || "random";
    var qs = isRandomSwitch
      ? "?exclude=" +
        encodeURIComponent(currentModel || failedModel || "") +
        "&t=" +
        Date.now()
      : "?model=" +
        encodeURIComponent(targetModel) +
        (explicitModel ? "&t=" + Date.now() : "");
    var fetchOpts = { headers: { Accept: "application/json" } };
    if (abortCtrl) fetchOpts.signal = abortCtrl.signal;
    return fetch(apiBase + endpoint + qs, fetchOpts)
      .then(function (resp) {
        if (!resp.ok) throw new Error("Model API returned " + resp.status);
        return resp.json();
      })
      .then(function (data) {
        if (!data || typeof data !== "object" || !data.model) {
          throw new Error("Invalid model descriptor");
        }
        return data;
      });
  }

  function refreshSakuraBackground() {
    if (!alive || !host) return;
    if (host.getAttribute("data-sakura") === "0" || prefersReducedMotion()) {
      host.style.backgroundImage = "none";
      host.classList.remove("has-bg");
      return;
    }
    var idx = Math.floor(Math.random() * 11);
    var webpUrl =
      staticBase +
      "/background/sakura" +
      idx +
      ".webp?v=" +
      encodeURIComponent(version);

    ensureCapabilities().then(function (caps) {
      if (!alive || !host) return;
      if (!caps.animWebp) {
        host.style.backgroundImage = "none";
        host.classList.remove("has-bg");
        return;
      }
      clearSakuraPreload();
      var pre = new Image();
      sakuraPreImg = pre;
      pre.decoding = "async";
      pre.onload = function () {
        if (sakuraPreImg === pre) sakuraPreImg = null;
        pre.onload = pre.onerror = null;
        if (alive && host && !prefersReducedMotion()) {
          host.style.backgroundImage = 'url("' + webpUrl + '")';
          host.classList.add("has-bg");
        }
      };
      pre.onerror = function () {
        if (sakuraPreImg === pre) sakuraPreImg = null;
        pre.onload = pre.onerror = null;
        if (caps) caps.animWebp = false;
        if (alive && host) {
          host.style.backgroundImage = "none";
          host.classList.remove("has-bg");
        }
      };
      pre.src = webpUrl;
    });
  }

  function matchSeasonalTip(seasons) {
    if (!Array.isArray(seasons) || seasons.length === 0) return "";
    var now = new Date();
    var m = now.getMonth() + 1;
    var d = now.getDate();
    var curVal = m * 100 + d;
    for (var i = 0; i < seasons.length; i++) {
      var item = seasons[i];
      if (!item || typeof item.date !== "string" || !item.text) continue;
      var parts = item.date.split("-");
      var startMD = parts[0].trim().split("/");
      if (startMD.length !== 2) continue;
      var sVal = parseInt(startMD[0], 10) * 100 + parseInt(startMD[1], 10);
      var eVal = sVal;
      if (parts.length > 1) {
        var endMD = parts[1].trim().split("/");
        if (endMD.length === 2) {
          eVal = parseInt(endMD[0], 10) * 100 + parseInt(endMD[1], 10);
        }
      }
      if (curVal >= sVal && curVal <= eVal) {
        var year = now.getFullYear();
        return String(item.text)
          .replace(/\{years\}/g, String(year - 1949))
          .replace(/\{year\}/g, String(year));
      }
    }
    return "";
  }

  function ensureTipsLoaded() {
    if (tipsLoaded || !alive) return;
    tipsLoaded = true;
    var fetchOpts = { headers: { Accept: "application/json" } };
    if (abortCtrl) fetchOpts.signal = abortCtrl.signal;
    fetch(
      staticBase + "/tips.json?v=" + encodeURIComponent(version),
      fetchOpts
    )
      .then(function (r) {
        if (!r.ok) throw new Error("tips unavailable");
        return r.json();
      })
      .then(function (data) {
        if (!alive || !data) return;
        tips = data;
        if (ready && sGet("volo.kanban.seasonShown") !== "1") {
          var seasonal = matchSeasonalTip(tips.seasons);
          if (seasonal) {
            sSet("volo.kanban.seasonShown", "1");
            var waitMs = Math.max(0, messageUntilMs - Date.now());
            if (waitMs > 0) {
              window.clearTimeout(seasonTimer);
              seasonTimer = window.setTimeout(function () {
                seasonTimer = 0;
                showMessage(seasonal, 6000);
              }, waitMs + 250);
            } else {
              showMessage(seasonal, 6000);
            }
          }
        }
      })
      .catch(function () {});

    if (host.getAttribute("data-idle-chat") !== "0") {
      chatTimer = window.setInterval(function () {
        if (
          alive &&
          ready &&
          !document.hidden &&
          !isOverlayBlocking()
        ) {
          showMessage(tips.chat, 4000);
        }
      }, 30000);
    }
  }

  function syncOffsetsAndPosition() {
    if (!host) return;
    var rawX = sGet("volo.kanban.x");
    var rawY = sGet("volo.kanban.y");
    if (rawX === null) rawX = sGet("soloKanbanniangX");
    if (rawY === null) rawY = sGet("soloKanbanniangY");

    if (rawX !== null && rawY !== null) {
      var x = Number(rawX);
      var y = Number(rawY);
      if (isFinite(x) && isFinite(y)) {
        var clampedX = Math.max(0, Math.min(x, viewportWidth() - 284));
        var clampedY = Math.max(0, Math.min(y, viewportHeight() - 254));
        host.setAttribute("data-dragged", "1");
        host.style.left = clampedX + "px";
        host.style.top = clampedY + "px";
        host.style.right = "auto";
        host.style.bottom = "auto";
        clampTipToViewport();
        return;
      }
    }

    host.removeAttribute("data-dragged");
    host.style.removeProperty("left");
    host.style.removeProperty("top");
    host.style.removeProperty("right");

    var pos =
      host.getAttribute("data-position") ||
      (host.classList.contains("volo-kbn-right") ? "right" : "left");
    var fab = document.querySelector(".book-mobile-toc-fab");
    var fabOnScreen = !!(
      fab &&
      !fab.hidden &&
      viewportWidth() <= 900 &&
      pos !== "right" &&
      window.getComputedStyle(fab).display !== "none"
    );
    host.classList.toggle("is-book-fab-offset", fabOnScreen);
    var podcastOpen =
      document.documentElement.getAttribute("data-podcast-open") === "1" ||
      (document.body && document.body.classList.contains("has-podcast-mini"));
    if (fabOnScreen && !podcastOpen) {
      host.style.bottom = "calc(68px + env(safe-area-inset-bottom, 0px))";
    } else {
      host.style.removeProperty("bottom");
    }
    clampTipToViewport();
  }

  function clearCanvasRemovers() {
    var list = canvasRemovers.splice(0);
    for (var i = 0; i < list.length; i++) {
      list[i]();
    }
  }

  function clearErrorUI() {
    loadFailed = false;
    if (host) {
      host.classList.remove("is-error");
      delete host.dataset.error;
    }
    if (retryBtn) {
      retryBtn.hidden = true;
    }
  }

  function pause() {
    loadSeq += 1;
    warmBootPromise = null;
    pendingChangeTip = false;
    ready = false;
    loadFailed = false;
    failedModel = "";
    started = false;
    lastHoverTarget = null;
    dragState = null;
    clearPreload();
    clearSakuraPreload();
    if (host) {
      host.hidden = true;
      host.classList.remove("is-tools-open", "is-loading", "is-error");
      host.style.removeProperty("transform");
      host.style.removeProperty("will-change");
      delete host.dataset.ready;
      delete host.dataset.error;
    }
    if (retryBtn) {
      retryBtn.hidden = true;
    }
    window.clearTimeout(loadTimer);
    window.clearTimeout(seasonTimer);
    seasonTimer = 0;
    hideMessage();
    if (window.loadlive2d && typeof window.loadlive2d.dispose === "function") {
      clearCanvasRemovers();
      canvasTainted = true;
      window.loadlive2d.dispose();
    }
  }

  function enterLoadError(targetModelName) {
    if (!alive || !started || shouldPauseForMobile() || isClosedInSession()) {
      return;
    }
    loadSeq += 1;
    warmBootPromise = null;
    pendingChangeTip = false;
    ready = false;
    loadFailed = true;
    if (targetModelName) {
      failedModel = targetModelName;
    } else if (!failedModel && currentModel) {
      failedModel = currentModel;
    }
    clearPreload();
    window.clearTimeout(loadTimer);
    if (window.loadlive2d && typeof window.loadlive2d.dispose === "function") {
      clearCanvasRemovers();
      canvasTainted = true;
      window.loadlive2d.dispose();
    }
    if (host) {
      host.hidden = false;
      host.classList.remove("is-closed", "is-closing", "is-loading");
      host.classList.add("is-error");
      delete host.dataset.ready;
      host.dataset.error = "true";
    }
    var btn = ensureRetryButton();
    if (btn) {
      btn.hidden = false;
    }
    syncOffsetsAndPosition();
    if (!isOverlayBlocking()) {
      showMessage(resolveLocaleStrings().loadErrorTip, 6000);
    }
  }

  function retryLoad() {
    if (!alive || isClosedInSession() || shouldPauseForMobile()) return;
    if (!started) {
      start();
      return;
    }
    change(false, failedModel);
  }

  function listenCanvas(canvasEl, type, fn) {
    if (!canvasEl || !canvasEl.addEventListener) return;
    canvasEl.addEventListener(type, fn, false);
    canvasRemovers.push(function () {
      try {
        canvasEl.removeEventListener(type, fn, false);
      } catch (err) {}
    });
  }

  function bindCanvasContextRecovery(canvasEl) {
    if (!canvasEl || canvasEl.__voloCtxBound) return;
    canvasEl.__voloCtxBound = true;
    listenCanvas(canvasEl, "webglcontextlost", function (ev) {
      if (!alive || !started) return;
      if (ev && typeof ev.preventDefault === "function") {
        ev.preventDefault();
      }
      ready = false;
      if (host) {
        delete host.dataset.ready;
      }
      window.clearTimeout(loadTimer);
    });
    listenCanvas(canvasEl, "webglcontextrestored", function () {
      if (!alive || !started || shouldPauseForMobile() || isClosedInSession()) {
        return;
      }
      clearErrorUI();
      host.classList.add("is-loading");
      window.clearTimeout(loadTimer);
      loadTimer = window.setTimeout(function () {
        enterLoadError(currentModel);
      }, LOAD_TIMEOUT_MS);
      if (lastDescriptor && typeof window.loadlive2d === "function") {
        window.loadlive2d("soloKanbanniang", lastDescriptor);
      } else {
        change(false, currentModel);
      }
    });
  }

  function ensureFreshCanvas() {
    var canvasEl = host.querySelector("#soloKanbanniang");
    if (canvasEl && canvasTainted && canvasEl.parentNode) {
      clearCanvasRemovers();
      var freshCanvas = canvasEl.cloneNode(false);
      canvasEl.parentNode.replaceChild(freshCanvas, canvasEl);
      canvasEl = freshCanvas;
      canvasTainted = false;
    }
    bindCanvasContextRecovery(canvasEl);
    return canvasEl;
  }

  function destroy() {
    if (!alive) return;
    alive = false;
    if (idleHandle) {
      if (idleIsRaf && typeof window.cancelAnimationFrame === "function") {
        window.cancelAnimationFrame(idleHandle);
      } else {
        window.clearTimeout(idleHandle);
      }
      idleHandle = 0;
    }
    if (overlaySyncRaf) {
      if (typeof window.cancelAnimationFrame === "function") {
        window.cancelAnimationFrame(overlaySyncRaf);
      } else {
        window.clearTimeout(overlaySyncRaf);
      }
      overlaySyncRaf = 0;
    }
    if (abortCtrl) {
      try {
        abortCtrl.abort();
      } catch (err) {}
    }
    window.clearTimeout(closeTimer);
    window.clearTimeout(seasonTimer);
    window.clearInterval(chatTimer);
    clearCanvasRemovers();
    var list = removers.splice(0);
    for (var i = 0; i < list.length; i++) {
      list[i]();
    }
    pause();
  }

  function change(isRandomSwitch, explicitModel) {
    if (!alive || !started) return;
    var seq = ++loadSeq;
    ready = false;
    clearErrorUI();
    delete host.dataset.ready;
    clearPreload();
    host.hidden = false;
    host.classList.remove("is-closed", "is-closing");
    host.classList.add("is-loading");
    window.clearTimeout(loadTimer);
    loadTimer = window.setTimeout(function () {
      if (seq === loadSeq) enterLoadError(explicitModel || "");
    }, LOAD_TIMEOUT_MS);

    var prepared = Promise.all([
      ensureCapabilities(),
      fetchModelDescriptor(!!isRandomSwitch, explicitModel)
    ]).then(function (pair) {
      var caps = pair[0];
      var descriptor = pair[1];
      if (
        seq === loadSeq &&
        alive &&
        started &&
        !shouldPauseForMobile() &&
        descriptor
      ) {
        prefetchDescriptor(descriptor, !!caps.webglWebp);
      }
      return pair;
    });

    Promise.all([prepared, loadSDK()])
      .then(function (res) {
        if (seq !== loadSeq) return;
        var caps = res[0][0];
        var descriptor = res[0][1];
        if (!alive || !started || shouldPauseForMobile()) return;
        window.VoloLive2D = window.VoloLive2D || {};
        window.VoloLive2D.useWebp = !!caps.webglWebp;
        window.VoloLive2D.muted = host.getAttribute("data-mute-sound") === "1";
        window.VoloLive2D.suspended = isOverlayBlocking();
        ensureFreshCanvas();
        lastDescriptor = descriptor || lastDescriptor;
        currentModel = (descriptor && descriptor.name) || currentModel;
        failedModel = (descriptor && descriptor.name) || explicitModel || currentModel;
        window.loadlive2d("soloKanbanniang", descriptor);
      })
      .catch(function () {
        if (seq === loadSeq) enterLoadError(explicitModel || "");
      });
  }

  function warmInitialAssets(expectedSeq) {
    if (!warmBootPromise) {
      var sdkPromise = loadSDK();
      var prepared = Promise.all([
        ensureCapabilities(),
        fetchModelDescriptor(false)
      ]).then(function (pair) {
        var caps = pair[0];
        var descriptor = pair[1];
        if (
          expectedSeq === loadSeq &&
          alive &&
          !shouldPauseForMobile() &&
          !isClosedInSession() &&
          descriptor
        ) {
          prefetchDescriptor(descriptor, !!caps.webglWebp);
        }
        return pair;
      });
      warmBootPromise = Promise.all([prepared, sdkPromise]).catch(function (err) {
        warmBootPromise = null;
        throw err;
      });
    }
    return warmBootPromise;
  }

  function start() {
    if (!alive || started || shouldPauseForMobile() || isClosedInSession()) {
      return;
    }
    var seq = warmBootPromise ? loadSeq : ++loadSeq;
    started = true;
    clearErrorUI();
    syncOffsetsAndPosition();
    host.classList.remove("is-closed", "is-closing");
    host.classList.add("is-loading");
    host.hidden = false;
    window.clearTimeout(loadTimer);
    loadTimer = window.setTimeout(function () {
      if (seq === loadSeq) enterLoadError("");
    }, LOAD_TIMEOUT_MS);

    var bootPromise = warmInitialAssets(seq);
    warmBootPromise = null;

    bootPromise
      .then(function (res) {
        if (seq !== loadSeq) return;
        if (!alive || shouldPauseForMobile() || !host.isConnected) {
          started = false;
          host.classList.remove("is-loading");
          host.hidden = true;
          clearPreload();
          window.clearTimeout(loadTimer);
          return;
        }
        var caps = res[0][0];
        var descriptor = res[0][1];
        window.VoloLive2D = window.VoloLive2D || {};
        window.VoloLive2D.useWebp = !!caps.webglWebp;
        window.VoloLive2D.muted = host.getAttribute("data-mute-sound") === "1";
        window.VoloLive2D.suspended = isOverlayBlocking();

        ensureFreshCanvas();

        lastDescriptor = descriptor || null;
        currentModel = (descriptor && descriptor.name) || "";
        failedModel = currentModel;
        window.loadlive2d("soloKanbanniang", descriptor);
      })
      .catch(function () {
        if (seq === loadSeq) enterLoadError("");
      });
  }

  function scheduleIdleStart() {
    if (shouldPauseForMobile() || isClosedInSession()) {
      host.hidden = true;
      return;
    }
    var seq = ++loadSeq;
    warmInitialAssets(seq).catch(function () {});
    if (typeof window.requestAnimationFrame === "function") {
      idleIsRaf = true;
      idleHandle = window.requestAnimationFrame(function () {
        idleHandle = 0;
        start();
      });
    } else {
      idleIsRaf = false;
      idleHandle = window.setTimeout(function () {
        idleHandle = 0;
        start();
      }, 16);
    }
  }

  function navigateHome() {
    try {
      var a = document.createElement("a");
      a.href = "/";
      a.style.display = "none";
      document.body.appendChild(a);
      var ev = new MouseEvent("click", {
        bubbles: true,
        cancelable: true,
        view: window
      });
      var notPrevented = a.dispatchEvent(ev);
      if (a.parentNode) a.parentNode.removeChild(a);
      if (notPrevented) {
        window.location.assign("/");
      }
    } catch (err) {
      window.location.assign("/");
    }
  }

  function sendHeadPat(targetEl) {
    var loc = resolveLocaleStrings();
    fetch(apiBase + "/pat", {
      method: "POST",
      headers: { Accept: "application/json" }
    })
      .then(function (r) {
        return r
          .json()
          .catch(function () {
            return {};
          })
          .then(function (d) {
            return { status: r.status, ok: r.ok, d: d };
          });
      })
      .then(function (res) {
        if (!alive) return;
        if (res.status === 429) {
          showMessage(loc.rateLimited, 3500);
          return;
        }
        if (res.d && typeof res.d.total_pats === "number") {
          patCount = res.d.total_pats;
        } else if (res.ok) {
          patCount += 1;
        }
        host.setAttribute("data-pats", String(patCount));
        var quote = pickRuleTip(tips.click, targetEl);
        if (quote) {
          showMessage(quote + " (✨ +" + patCount + ")", 4500);
        } else {
          showMessage("✨ +" + patCount, 4000);
        }
      })
      .catch(function () {
        if (!alive) return;
        patCount += 1;
        host.setAttribute("data-pats", String(patCount));
        var quote = pickRuleTip(tips.click, targetEl);
        if (quote) {
          showMessage(quote + " (✨ +" + patCount + ")", 4500);
        } else {
          showMessage("✨ +" + patCount, 4000);
        }
      });
  }

  function bindTool(toolName, handler) {
    var btn = host.querySelector('[data-kbn-tool="' + toolName + '"]');
    if (btn) {
      listen(btn, "click", function (ev) {
        ev.stopPropagation();
        handler();
      });
    }
  }

  bindTool("home", navigateHome);
  bindTool("rss", function () {
    window.location.assign("/rss.xml");
  });
  bindTool("chat", function () {
    showMessage(tips.chat, 4500);
    refreshSakuraBackground();
  });
  bindTool("change", function () {
    pendingChangeTip = true;
    if (!started) {
      start();
      return;
    }
    change(true);
  });
  bindTool("photo", function () {
    if (ready && window.Live2D) {
      window.Live2D.captureName = "volo-kanbanniang.png";
      window.Live2D.captureFrame = true;
    }
  });
  bindTool("github", function () {
    var url = sanitizeHttpUrl(host.getAttribute("data-project-url"));
    window.open(url, "_blank", "noopener,noreferrer");
  });
  bindTool("close", function () {
    sSet("volo.kanban.closed", "1");
    sSet("soloKanbanniang", "close");
    host.classList.add("is-closing");
    closeTimer = window.setTimeout(function () {
      host.classList.remove("is-closing");
      host.classList.add("is-closed");
      pause();
    }, 180);
  });

  listen(window, "volo:kanban-ready", function (ev) {
    if (!alive || !started || shouldPauseForMobile() || isClosedInSession()) {
      return;
    }
    ready = true;
    failedModel = "";
    clearErrorUI();
    window.clearTimeout(loadTimer);
    host.dataset.ready = "true";
    if (ev && ev.detail && ev.detail.model) {
      currentModel = ev.detail.model;
    }
    host.hidden = false;
    host.classList.remove("is-closed", "is-closing", "is-loading", "is-error");
    syncOffsetsAndPosition();
    if (window.VoloLive2D) {
      if (typeof window.VoloLive2D.drawNow === "function") {
        window.VoloLive2D.drawNow();
      } else if (typeof window.VoloLive2D.invalidateRect === "function") {
        window.VoloLive2D.invalidateRect();
      }
    }
    window.dispatchEvent(new CustomEvent("volo:kanban-moved"));
    refreshSakuraBackground();
    ensureTipsLoaded();

    if (!initialGreetShown) {
      initialGreetShown = true;
      pendingChangeTip = false;
      if (!isOverlayBlocking()) {
        showMessage(resolveLocaleStrings().greeting, 5000);
      }
    } else if (pendingChangeTip) {
      pendingChangeTip = false;
      if (!isOverlayBlocking()) {
        showMessage(resolveLocaleStrings().changeTip, 4500);
      }
    }
  });

  listen(window, "volo:kanban-error", function (ev) {
    if (!alive || !started || shouldPauseForMobile() || isClosedInSession()) {
      return;
    }
    if (ev && ev.detail && ev.detail.reason === "webgl") {
      canvasTainted = true;
    }
    var errModel = (ev && ev.detail && ev.detail.model) || failedModel || currentModel;
    enterLoadError(errModel);
  });

  bindCanvasContextRecovery(host.querySelector("#soloKanbanniang"));

  listen(
    window,
    "resize",
    function () {
      if (shouldPauseForMobile()) {
        if (started) pause();
      } else {
        syncOffsetsAndPosition();
        start();
      }
    },
    { passive: true }
  );

  listen(document, "volo:pjax:after", function () {
    syncLocale();
    scheduleOverlaySync();
  });
  listen(document, "volo:page-load", function () {
    syncLocale();
    scheduleOverlaySync();
  });
  listen(document, "click", scheduleOverlaySync, { passive: true });
  listen(document, "close", scheduleOverlaySync, true);

  if (typeof MutationObserver === "function") {
    var rootObserver = new MutationObserver(scheduleOverlaySync);
    if (document.documentElement) {
      rootObserver.observe(document.documentElement, {
        attributes: true,
        attributeFilter: ["class", "data-podcast-open"]
      });
    }
    if (document.body) {
      rootObserver.observe(document.body, {
        attributes: true,
        attributeFilter: ["class"]
      });
    }
    removers.push(function () {
      rootObserver.disconnect();
    });
  }

  listen(
    document,
    "pointerdown",
    function (ev) {
      if (!host || !host.classList.contains("is-tools-open")) return;
      var target = ev && ev.target;
      if (!target || !host.contains(target)) {
        host.classList.remove("is-tools-open");
      }
    },
    { passive: true }
  );

  listen(document, "keydown", function (ev) {
    if (ev && ev.key === "Escape" && host) {
      host.classList.remove("is-tools-open");
    }
    scheduleOverlaySync();
  });

  listen(document, "copy", function () {
    showMessage(resolveLocaleStrings().copyTip, 4000);
  });

  listen(
    document,
    "mouseover",
    function (ev) {
      if (
        !ready ||
        dragState ||
        document.hidden ||
        !tips ||
        !Array.isArray(tips.mouseover) ||
        tips.mouseover.length === 0
      ) {
        return;
      }
      var target = ev && ev.target;
      if (
        !target ||
        target === lastHoverTarget ||
        typeof target.closest !== "function"
      ) {
        return;
      }
      lastHoverTarget = target;
      if (isOverlayBlocking()) return;
      var list = tips.mouseover;
      for (var i = 0; i < list.length; i++) {
        var entry = list[i];
        if (!entry || !entry.selector || entry.text == null) continue;
        var matched = null;
        try {
          matched = target.closest(entry.selector);
        } catch (err) {
          continue;
        }
        if (
          matched &&
          (!ev.relatedTarget || !matched.contains(ev.relatedTarget))
        ) {
          var tpl = resolveTipText(entry.text);
          if (!tpl) break;
          if (tpl.indexOf("{text}") !== -1) {
            var labelText = extractHoverLabel(matched);
            if (!labelText) break;
            tpl = tpl.replace(/\{text\}/g, labelText);
          }
          showMessage(tpl, 3200);
          break;
        }
      }
    },
    { passive: true }
  );

  listen(host, "pointerdown", function (ev) {
    if (ev.button !== 0 && ev.pointerType === "mouse") return;
    if (ev.target && typeof ev.target.closest === "function") {
      if (ev.target.closest("button")) return;
    }
    var rect = host.getBoundingClientRect();
    var w = rect.width || 284;
    var h = rect.height || 254;
    dragState = {
      id: ev.pointerId,
      startX: ev.clientX,
      startY: ev.clientY,
      startLeft: rect.left,
      startTop: rect.top,
      maxW: Math.max(0, viewportWidth() - w),
      maxH: Math.max(0, viewportHeight() - h),
      nextX: rect.left,
      nextY: rect.top,
      moved: false,
      isCanvas: !!(ev.target && ev.target.id === "soloKanbanniang")
    };
  });

  listen(host, "pointermove", function (ev) {
    if (!dragState || dragState.id !== ev.pointerId) return;
    var rawDx = ev.clientX - dragState.startX;
    var rawDy = ev.clientY - dragState.startY;
    if (!dragState.moved && rawDx * rawDx + rawDy * rawDy <= 25) {
      return;
    }
    if (!dragState.moved) {
      dragState.moved = true;
      if (typeof host.setPointerCapture === "function") {
        try {
          host.setPointerCapture(ev.pointerId);
        } catch (err) {}
      }
      host.setAttribute("data-dragged", "1");
      host.style.left = dragState.startLeft + "px";
      host.style.top = dragState.startTop + "px";
      host.style.right = "auto";
      host.style.bottom = "auto";
      host.style.willChange = "transform";
    }
    var clampedX = Math.max(
      0,
      Math.min(dragState.startLeft + rawDx, dragState.maxW)
    );
    var clampedY = Math.max(
      0,
      Math.min(dragState.startTop + rawDy, dragState.maxH)
    );
    dragState.nextX = clampedX;
    dragState.nextY = clampedY;
    var tx = clampedX - dragState.startLeft;
    var ty = clampedY - dragState.startTop;
    host.style.transform = "translate3d(" + tx + "px," + ty + "px,0)";
  });

  function finishPointer(ev, isCancel) {
    if (!dragState || dragState.id !== ev.pointerId) return;
    var state = dragState;
    dragState = null;
    if (
      typeof host.hasPointerCapture === "function" &&
      typeof host.releasePointerCapture === "function"
    ) {
      try {
        if (host.hasPointerCapture(ev.pointerId)) {
          host.releasePointerCapture(ev.pointerId);
        }
      } catch (err) {}
    }
    if (state.moved) {
      var finalX = Math.round(state.nextX);
      var finalY = Math.round(state.nextY);
      host.style.removeProperty("transform");
      host.style.removeProperty("will-change");
      host.style.left = finalX + "px";
      host.style.top = finalY + "px";
      host.style.right = "auto";
      host.style.bottom = "auto";
      sSet("volo.kanban.x", finalX);
      sSet("volo.kanban.y", finalY);
      sSet("soloKanbanniangX", finalX);
      sSet("soloKanbanniangY", finalY);
      clampTipToViewport();
      window.dispatchEvent(new CustomEvent("volo:kanban-moved"));
      return;
    }
    if (!isCancel) {
      if (loadFailed) {
        retryLoad();
        return;
      }
      if (isTouchOrCoarse(ev)) {
        host.classList.toggle("is-tools-open");
      }
      if (state.isCanvas && ready) {
        if (
          window.VoloLive2D &&
          typeof window.VoloLive2D.tapClient === "function"
        ) {
          window.VoloLive2D.tapClient(ev.clientX, ev.clientY);
        }
        sendHeadPat(ev.target);
      }
    }
  }

  listen(host, "pointerup", function (ev) {
    finishPointer(ev, false);
  });
  listen(host, "pointercancel", function (ev) {
    finishPointer(ev, true);
  });
  listen(host, "lostpointercapture", function (ev) {
    finishPointer(ev, true);
  });

  window.VoloKanban = {
    destroy: destroy,
    change: change,
    retry: retryLoad,
    showMessage: showMessage,
    syncLocale: syncLocale,
    isReady: function () {
      return ready;
    },
    hasError: function () {
      return loadFailed;
    },
    isAlive: function () {
      return alive;
    }
  };

  syncLocale();
  if (document.readyState === "loading") {
    listen(document, "DOMContentLoaded", scheduleIdleStart, { once: true });
  } else {
    scheduleIdleStart();
  }
})();
