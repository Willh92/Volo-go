/* Volo Studio Public Runtime — Server-Authoritative Accent, Animated Popovers, PJAX, Interruptible Smooth Scroll, Dockable TOC, Search, Podcast & Bookshelf */
(function () {
  "use strict";

  var MODES = ["auto", "system", "light", "dark"];
  var BOOK_PROGRESS_KEY = "volo.book.progress.v1";
  var activeFetchCtrl = null;
  var pjaxTimer = null;
  var tocCleanups = [];
  var scrollAnimRAF = 0;
  var katexPromise = null;
  var mermaidPromise = null;
  var mermaidGen = 0;
  var mermaidQueue = Promise.resolve();
  var lastPathnameAndSearch = window.location.pathname + window.location.search;

  function prefersReducedMotion() {
    return !!(window.matchMedia && window.matchMedia("(prefers-reduced-motion: reduce)").matches);
  }

  function resolveEffectiveTheme(mode) {
    if (mode === "light" || mode === "dark") return mode;
    if (mode === "system") {
      return window.matchMedia && window.matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light";
    }
    var now = new Date();
    var mins = now.getHours() * 60 + now.getMinutes();
    return mins >= 390 && mins < 1140 ? "light" : "dark";
  }

  function updateGitHubStatsTheme(scope, theme) {
    var eff = theme || document.documentElement.getAttribute("data-theme") || "light";
    (scope || document).querySelectorAll('img[src*="/github-stats/"]').forEach(function (img) {
      var raw = img.getAttribute("src") || "";
      if (!raw || raw.indexOf(".svg") === -1) return;
      try {
        var u = new URL(raw, window.location.origin);
        if (u.searchParams.get("theme") !== eff) {
          u.searchParams.set("theme", eff);
          img.setAttribute("src", raw.charAt(0) === "/" ? (u.pathname + u.search + u.hash) : u.href);
        }
      } catch (_) {}
    });
  }

  function getCSRFToken() {
    var meta = document.querySelector('meta[name="csrf-token"]');
    if (meta && meta.getAttribute("content")) return meta.getAttribute("content");
    var cm = document.getElementById("volo-comments");
    if (cm && cm.getAttribute("data-csrf-token")) return cm.getAttribute("data-csrf-token");
    return "";
  }

  /* Strictly honor server-rendered data-accent; only switch 4-mode light/dark appearance */
  function applyTheme(mode) {
    var root = document.documentElement;
    var prevTheme = root.getAttribute("data-theme") || "";
    var savedMode = "";
    try {
      savedMode = localStorage.getItem("volo_theme_mode") || "";
    } catch (_) {}
    var chosenMode = mode || savedMode || root.getAttribute("data-default-mode") || "auto";
    if (MODES.indexOf(chosenMode) === -1) chosenMode = "auto";

    var nextTheme = resolveEffectiveTheme(chosenMode);
    root.setAttribute("data-mode", chosenMode);
    root.setAttribute("data-theme", nextTheme);

    var labelEl = document.getElementById("volo-theme-label");
    var toggleBtn = document.getElementById("volo-theme-btn");
    if (toggleBtn) {
      toggleBtn.setAttribute("data-current-mode", chosenMode);
      var shortAttr = toggleBtn.getAttribute("data-label-" + chosenMode);
      if (labelEl && shortAttr) labelEl.textContent = shortAttr;
      var titleAttr = toggleBtn.getAttribute("data-title-" + chosenMode) || shortAttr;
      if (titleAttr) {
        toggleBtn.setAttribute("title", titleAttr);
        toggleBtn.setAttribute("aria-label", titleAttr);
      }
      toggleBtn.querySelectorAll(".volo-theme-icon[data-theme-icon]").forEach(function (ic) {
        ic.classList.toggle("is-active", ic.getAttribute("data-theme-icon") === chosenMode);
      });
    }
    document.querySelectorAll("[data-volo-mode]").forEach(function (el) {
      var selected = el.getAttribute("data-volo-mode") === chosenMode;
      el.classList.toggle("is-selected", selected);
      el.setAttribute("aria-checked", selected ? "true" : "false");
    });

    updateGitHubStatsTheme(document, nextTheme);
    if (prevTheme && prevTheme !== nextTheme) {
      reRenderMermaidForTheme();
    }
  }

  function collectEnvFP() {
    try {
      var tz = Intl.DateTimeFormat().resolvedOptions().timeZone || "";
      return [screen.width + "x" + screen.height, window.devicePixelRatio || 1, tz, navigator.language || "", navigator.hardwareConcurrency || 0].join("|");
    } catch (_) {
      return "";
    }
  }
  window.__voloEnvFP = collectEnvFP;

  /* Unified Body Scroll Lock for Modals, Lightbox & Drawers */
  function isOverlayActive(el) {
    return !!(el && !el.hidden && !el.classList.contains("is-closing"));
  }

  function isTocDrawerModalActive() {
    var d = document.getElementById("volo-toc-drawer");
    if (!isOverlayActive(d)) return false;
    if (document.documentElement.getAttribute("data-volo-skin") === "volo-journal" && (window.innerWidth || 1024) >= 1024) {
      return false;
    }
    return true;
  }

  function isMobileBookDrawerOpen() {
    if ((window.innerWidth || 1024) > 900) return false;
    var reader = document.querySelector("[data-book-reader]");
    return !!(reader && !reader.classList.contains("is-toc-collapsed") && reader.querySelector(".book-reader-toc"));
  }

  var bodyScrollLocked = false;
  function syncBodyScrollLock() {
    var locked =
      isOverlayActive(document.getElementById("volo-search-modal")) ||
      isOverlayActive(document.getElementById("volo-lightbox")) ||
      isTocDrawerModalActive() ||
      isMobileBookDrawerOpen() ||
      isOverlayActive(document.querySelector(".volo-comment-composer")) ||
      isOverlayActive(document.querySelector(".vc-ban-modal"));
    if (locked === bodyScrollLocked) return;
    if (locked) {
      var sbw = Math.max(0, (window.innerWidth || 0) - (document.documentElement.clientWidth || 0));
      var gtr = window.CSS && CSS.supports && CSS.supports("scrollbar-gutter: stable");
      bodyScrollLocked = true;
      document.documentElement.style.overflow = "hidden";
      if (document.body) {
        document.body.style.overflow = "hidden";
        if (!gtr && sbw > 0) document.body.style.paddingRight = sbw + "px";
      }
    } else {
      bodyScrollLocked = false;
      document.documentElement.style.overflow = "";
      if (document.body) {
        document.body.style.overflow = "";
        document.body.style.paddingRight = "";
      }
    }
  }
  window.__voloSyncScrollLock = syncBodyScrollLock;

  function clampPopoverToViewport(el) {
    if (!el || !el.classList.contains("volo-popover")) return;
    el.style.right = "";
    el.style.left = "";
    var vw = window.innerWidth || document.documentElement.clientWidth || 375;
    var rect = el.getBoundingClientRect();
    if (rect.left < 14) {
      var curRight = parseFloat(window.getComputedStyle(el).right) || 0;
      el.style.right = Math.round(curRight - (14 - rect.left)) + "px";
    } else if (rect.right > vw - 14) {
      var curRight2 = parseFloat(window.getComputedStyle(el).right) || 0;
      el.style.right = Math.round(curRight2 + (rect.right - (vw - 14))) + "px";
    }
  }

  /* Animated Popover / Modal / Drawer Transitions */
  function openAnimated(el, triggerBtn) {
    if (!el) return;
    if (el._closeTimer) {
      clearTimeout(el._closeTimer);
      el._closeTimer = null;
    }
    el.hidden = false;
    el.classList.remove("is-closing");
    if (triggerBtn) triggerBtn.setAttribute("aria-expanded", "true");
    clampPopoverToViewport(el);
    if (prefersReducedMotion()) {
      el.classList.add("is-open");
      syncBodyScrollLock();
      return;
    }
    void el.offsetWidth;
    el.classList.add("is-open");
    syncBodyScrollLock();
  }

  function closeAnimated(el, triggerBtn, durationMs) {
    if (!el || el.hidden) {
      if (triggerBtn) triggerBtn.setAttribute("aria-expanded", "false");
      syncBodyScrollLock();
      return;
    }
    if (triggerBtn) triggerBtn.setAttribute("aria-expanded", "false");
    if (el._closeTimer) {
      clearTimeout(el._closeTimer);
      el._closeTimer = null;
    }
    if (prefersReducedMotion()) {
      el.classList.remove("is-open", "is-closing");
      el.hidden = true;
      syncBodyScrollLock();
      return;
    }
    el.classList.remove("is-open");
    el.classList.add("is-closing");
    syncBodyScrollLock();
    el._closeTimer = setTimeout(function () {
      el.classList.remove("is-closing");
      el.hidden = true;
      el._closeTimer = null;
      syncBodyScrollLock();
    }, durationMs || 180);
  }

  function closePopovers() {
    closeAnimated(document.getElementById("volo-theme-menu"), document.getElementById("volo-theme-btn"), 180);
    closeAnimated(document.getElementById("volo-lang-menu"), document.getElementById("volo-lang-btn"), 180);
    closeAnimated(document.getElementById("volo-nav-more-menu"), document.getElementById("volo-nav-more-btn"), 180);
  }

  var lastActiveTocID = "";
  var pinnedTocID = "";
  var pinnedTocY = -1;

  function notifyScrollEnd() {
    window.dispatchEvent(new Event("volo:scrollend"));
  }

  /* Interruptible Cubic-Out Smooth Scroll Engine */
  function cancelSmoothScroll(silent) {
    if (scrollAnimRAF) {
      if (scrollAnimRAF > 0) cancelAnimationFrame(scrollAnimRAF);
      scrollAnimRAF = 0;
      if (!silent) notifyScrollEnd();
    }
  }

  function onUserScrollInterrupt() {
    pinnedTocID = "";
    cancelSmoothScroll(false);
  }

  window.addEventListener("wheel", onUserScrollInterrupt, { passive: true });
  window.addEventListener("touchstart", onUserScrollInterrupt, { passive: true });
  window.addEventListener("keydown", function (ev) {
    if (["ArrowUp", "ArrowDown", "PageUp", "PageDown", " ", "Home", "End"].indexOf(ev.key) !== -1) {
      onUserScrollInterrupt();
    }
  });
  window.__voloIsScrolling = function () {
    return scrollAnimRAF !== 0;
  };

  function syncHeaderHeight() {
    var header = document.querySelector(".studio-header");
    var h = header ? Math.round(header.getBoundingClientRect().height) : 64;
    if (!h || h < 40) h = 64;
    document.documentElement.style.setProperty("--volo-header-h", h + "px");
    return h;
  }

  function getHeaderOffset() {
    return syncHeaderHeight() + 20;
  }

  function smoothScrollToY(targetY, duration) {
    cancelSmoothScroll(true);
    var maxScroll = Math.max(0, document.documentElement.scrollHeight - (window.innerHeight || 0));
    var destY = Math.min(maxScroll, Math.max(0, Math.round(targetY)));
    var startY = window.scrollY || window.pageYOffset || 0;
    var diff = destY - startY;
    var dist = Math.abs(diff);

    if (prefersReducedMotion() || dist < 4) {
      window.scrollTo(0, destY);
      notifyScrollEnd();
      return;
    }

    var maxGlide = Math.min(Math.max(Math.round((window.innerHeight || 800) * 1.15), 700), 1080);
    var dur = duration
      ? Math.min(280, Math.max(160, Math.round(duration)))
      : Math.min(280, Math.max(160, Math.round(150 + Math.min(dist, 1200) * 0.1)));

    if (dist > maxGlide) {
      startY = destY - (diff > 0 ? maxGlide : -maxGlide);
      diff = destY - startY;
      dur = duration ? Math.min(280, Math.max(160, Math.round(duration))) : 240;
      scrollAnimRAF = -1;
      window.scrollTo(0, startY);
    }

    var startTime = null;

    function step(ts) {
      if (startTime === null) startTime = ts;
      var elapsed = ts - startTime;
      var t = Math.min(1, elapsed / dur);
      var eased = 1 - Math.pow(1 - t, 3);
      window.scrollTo(0, Math.round(startY + diff * eased));
      if (t < 1) {
        scrollAnimRAF = requestAnimationFrame(step);
      } else {
        scrollAnimRAF = 0;
        notifyScrollEnd();
      }
    }
    scrollAnimRAF = requestAnimationFrame(step);
  }

  function findHeadingTarget(rawID) {
    if (!rawID) return null;
    if (rawID === "comments") rawID = "volo-comments";
    var direct = document.getElementById(rawID);
    if (direct) return direct;
    try {
      var decoded = decodeURIComponent(rawID);
      if (decoded === "comments") decoded = "volo-comments";
      return document.getElementById(decoded);
    } catch (_) {
      return null;
    }
  }

  function pulseCommentTarget(el) {
    if (!el || !el.classList) return;
    var fnItem = el.matches && el.matches(".footnotes li") ? el : (el.closest ? el.closest(".footnotes li") : null);
    if (fnItem) {
      document.querySelectorAll(".footnotes li.is-target").forEach(function (n) { n.classList.remove("is-target"); });
      fnItem.classList.add("is-target");
      clearTimeout(fnItem._targetTimer);
      fnItem._targetTimer = setTimeout(function () { fnItem.classList.remove("is-target"); }, 2400);
      return;
    }
    if (!el.classList.contains("vc-item")) return;
    document.querySelectorAll(".vc-item.is-target").forEach(function (n) { n.classList.remove("is-target"); });
    el.classList.add("is-target");
    clearTimeout(el._targetTimer);
    el._targetTimer = setTimeout(function () { el.classList.remove("is-target"); }, 2600);
  }

  window.__voloScrollToEl = function (targetEl, highlight) {
    if (!targetEl) return false;
    var destY = (window.scrollY || 0) + targetEl.getBoundingClientRect().top - getHeaderOffset();
    if (highlight) pulseCommentTarget(targetEl);
    smoothScrollToY(destY);
    return true;
  };

  function setActiveTOCLink(activeID, force) {
    if (!activeID || (!force && activeID === lastActiveTocID)) return;
    lastActiveTocID = activeID;
    document.querySelectorAll(".volo-toc-link[data-toc-id]").forEach(function (a) {
      var match = a.getAttribute("data-toc-id") === activeID;
      if (a.classList.contains("is-active") !== match) {
        a.classList.toggle("is-active", match);
      }
      if (match && a.closest("#volo-toc-card, .toc-card")) {
        var nav = a.closest(".volo-toc__list, .volo-toc-nav, .toc-card");
        if (nav && nav.scrollHeight > nav.clientHeight && nav.offsetParent !== null) {
          var relTop = a.offsetParent === nav
            ? a.offsetTop
            : (a.getBoundingClientRect().top - nav.getBoundingClientRect().top + nav.scrollTop);
          var linkH = a.offsetHeight || 28;
          var relBottom = relTop + linkH;
          var viewTop = nav.scrollTop;
          var viewBottom = viewTop + nav.clientHeight;
          if (relTop < viewTop + 24 || relBottom > viewBottom - 24) {
            var nextTop = Math.max(0, relTop - Math.round((nav.clientHeight - linkH) * 0.38));
            if (typeof nav.scrollTo === "function" && !prefersReducedMotion()) {
              nav.scrollTo({ top: nextTop, behavior: "smooth" });
            } else {
              nav.scrollTop = nextTop;
            }
          }
        }
      }
    });
  }

  function smoothScrollToHash(rawID, skipHistory) {
    if (!rawID) {
      pinnedTocID = "";
      smoothScrollToY(0);
      return false;
    }
    var targetEl = findHeadingTarget(rawID);
    if (!targetEl) return false;
    var destY = (window.scrollY || 0) + targetEl.getBoundingClientRect().top - getHeaderOffset();
    var maxScroll = Math.max(0, document.documentElement.scrollHeight - (window.innerHeight || 0));
    pinnedTocID = targetEl.id;
    pinnedTocY = Math.min(maxScroll, Math.max(0, Math.round(destY)));
    if (!skipHistory) {
      try {
        var cleanHash = encodeURIComponent(targetEl.id).replace(/%3A/gi, ":");
        history.replaceState(history.state, "", "#" + cleanHash);
      } catch (_) {}
    }
    setActiveTOCLink(targetEl.id, false);
    pulseCommentTarget(targetEl);
    smoothScrollToY(destY);
    return true;
  }

  /* PJAX Top Progress Bar */
  function startPJAXBar() {
    var bar = document.getElementById("volo-pjax-bar");
    if (!bar) return;
    clearInterval(pjaxTimer);
    bar.style.transition = "none";
    bar.style.width = "0%";
    bar.classList.add("is-active");
    void bar.offsetWidth;
    bar.style.transition = "";
    var pct = 18;
    bar.style.width = pct + "%";
    pjaxTimer = setInterval(function () {
      if (pct < 86) {
        pct += (90 - pct) * 0.16;
        bar.style.width = pct.toFixed(1) + "%";
      }
    }, 180);
  }

  function finishPJAXBar() {
    var bar = document.getElementById("volo-pjax-bar");
    clearInterval(pjaxTimer);
    if (!bar) return;
    bar.style.width = "100%";
    setTimeout(function () {
      bar.classList.remove("is-active");
      setTimeout(function () { bar.style.width = "0%"; }, 240);
    }, 220);
  }

  /* Article Reading Progress Bar */
  function syncReadingProgress() {
    var bar = document.getElementById("volo-read-bar");
    if (!bar) return;
    var article = document.querySelector(".studio-layout--article .studio-article");
    if (!article) {
      bar.classList.remove("is-visible");
      bar.style.width = "0%";
      return;
    }
    var rect = article.getBoundingClientRect();
    var total = Math.max(1, article.offsetHeight - window.innerHeight * 0.65);
    var scrolled = Math.min(Math.max(0, -rect.top + 88), total);
    var pct = Math.round((scrolled / total) * 100);
    bar.classList.toggle("is-visible", window.scrollY > 60);
    bar.style.width = pct + "%";
  }

  /* Capture-Phase Broken Image Fallbacks (Brand Avatar, Covers, Geometric Avatars) */
  function handleBrokenImg(img) {
    if (!img) return;
    if (img.hasAttribute("data-brand-avatar") || img.hasAttribute("data-brand-logo")) {
      if (img.dataset.fallbackDone === "1") return;
      img.dataset.fallbackDone = "1";
      img.hidden = true;
      var wrap = img.parentElement;
      var mark = wrap && wrap.querySelector(".studio-brand__mark, .studio-mark");
      if (mark) mark.hidden = false;
      return;
    }
    if (img.hasAttribute("data-cover-img")) {
      if (img.dataset.fallbackDone === "1") return;
      img.dataset.fallbackDone = "1";
      img.hidden = true;
      var fb = img.parentElement && img.parentElement.querySelector(".studio-cover-fallback");
      if (fb) fb.hidden = false;
      return;
    }
    if (img.hasAttribute("data-avatar-seed")) {
      if (img.dataset.fallbackApplied === "1") return;
      var seed = img.getAttribute("data-avatar-seed") || "guest";
      var fallbackSrc = "/avatars/" + encodeURIComponent(seed) + ".svg";
      img.dataset.fallbackApplied = "1";
      if ((img.getAttribute("src") || "") !== fallbackSrc) {
        img.src = fallbackSrc;
      }
      return;
    }
    if (img.hasAttribute("data-hide-on-error")) {
      img.dataset.fallbackDone = "1";
      img.hidden = true;
    }
  }

  document.addEventListener("error", function (ev) {
    var t = ev.target;
    if (t && t.tagName === "IMG") handleBrokenImg(t);
  }, true);

  function checkBrokenImages(scope) {
    (scope || document).querySelectorAll("img[data-brand-avatar], img[data-brand-logo], img[data-cover-img], img[data-avatar-seed], img[data-hide-on-error]").forEach(function (img) {
      if (img.complete && img.naturalWidth === 0) handleBrokenImg(img);
    });
  }

  function copyTextToClipboard(txt) {
    if (navigator.clipboard && typeof navigator.clipboard.writeText === "function") {
      return navigator.clipboard.writeText(txt || "");
    }
    return new Promise(function (resolve, reject) {
      try {
        var ta = document.createElement("textarea");
        ta.value = txt || "";
        ta.setAttribute("readonly", "");
        ta.style.position = "fixed";
        ta.style.opacity = "0";
        document.body.appendChild(ta);
        ta.select();
        var ok = document.execCommand("copy");
        document.body.removeChild(ta);
        if (ok) resolve();
        else reject(new Error("copy failed"));
      } catch (err) {
        reject(err);
      }
    });
  }

  /* Code Block Header Bar + Clean Source Copy (Excludes Mermaid & Strips Line Numbers) */
  function initCodeCopy(scope) {
    var copyLabel = document.documentElement.getAttribute("data-i18n-copy") || "Copy";
    var copiedLabel = document.documentElement.getAttribute("data-i18n-copied") || "Copied";
    (scope || document).querySelectorAll(".volo-prose pre").forEach(function (pre) {
      if (pre.classList.contains("mermaid")) return;
      if (pre.parentElement && pre.parentElement.classList.contains("volo-code-block")) return;
      if (pre.querySelector(".volo-copy-btn")) return;

      var fig = pre.closest("[data-lang]");
      var lang = (fig && fig.getAttribute("data-lang")) || pre.getAttribute("data-lang") || "";
      var code = pre.querySelector("code");
      if (!lang) {
        var clsSrc = ((code && code.className) || "") + " " + (pre.className || "");
        var m = clsSrc.match(/(?:language|lang)-([a-zA-Z0-9_+#-]+)/);
        if (m) lang = m[1];
      }

      var wrap = document.createElement("div");
      wrap.className = "volo-code-block";
      var head = document.createElement("div");
      head.className = "volo-code-head";
      var langSpan = document.createElement("span");
      langSpan.className = "volo-code-lang";
      langSpan.textContent = lang || "code";
      var btn = document.createElement("button");
      btn.type = "button";
      btn.className = "volo-copy-btn";
      btn.textContent = copyLabel;
      btn.addEventListener("click", function () {
        var srcNode = code || pre;
        var clone = srcNode.cloneNode(true);
        clone.querySelectorAll(".ln, .lnt, .line-number, .line-numbers-rows").forEach(function (n) { n.remove(); });
        copyTextToClipboard(clone.textContent || "").then(function () {
          btn.textContent = copiedLabel;
          setTimeout(function () { btn.textContent = copyLabel; }, 1500);
        }).catch(function () {});
      });

      head.appendChild(langSpan);
      head.appendChild(btn);
      if (pre.parentNode) {
        pre.parentNode.insertBefore(wrap, pre);
        wrap.appendChild(head);
        wrap.appendChild(pre);
      }
    });
  }

  /* On-Demand KaTeX & Mermaid Lazy Loader (Backend-Unified Vendor CDN + Multi-Mirror Fallback) */
  function getVendorRuntime() {
    if (window.__VOLO_VENDOR__ && typeof window.__VOLO_VENDOR__.loadScript === "function") {
      return window.__VOLO_VENDOR__;
    }
    var bases = ["https://cdn.jsdelivr.net/npm", "https://fastly.jsdelivr.net/npm", "https://unpkg.com"];
    var loaded = {};
    function norm(sub) { return String(sub || "").replace(/^\/+/, ""); }
    function loadStyle(id, sub) {
      if (document.getElementById(id)) return Promise.resolve();
      var clean = norm(sub);
      return new Promise(function (resolve) {
        function attempt(idx) {
          if (idx >= bases.length) { resolve(); return; }
          var link = document.createElement("link");
          link.id = id;
          link.rel = "stylesheet";
          link.href = bases[idx] + "/" + clean;
          link.onload = function () { resolve(); };
          link.onerror = function () {
            link.remove();
            attempt(idx + 1);
          };
          document.head.appendChild(link);
        }
        attempt(0);
      });
    }
    function loadScript(sub) {
      var clean = norm(sub);
      if (loaded[clean]) return loaded[clean];
      loaded[clean] = new Promise(function (resolve, reject) {
        function attempt(idx) {
          if (idx >= bases.length) {
            delete loaded[clean];
            reject(new Error("vendor script load failed: " + clean));
            return;
          }
          var s = document.createElement("script");
          var done = false;
          var timer = setTimeout(function () {
            if (done) return;
            done = true;
            s.remove();
            attempt(idx + 1);
          }, 8000);
          s.src = bases[idx] + "/" + clean;
          s.async = true;
          s.onload = function () {
            if (done) return;
            done = true;
            clearTimeout(timer);
            resolve();
          };
          s.onerror = function () {
            if (done) return;
            done = true;
            clearTimeout(timer);
            s.remove();
            attempt(idx + 1);
          };
          document.head.appendChild(s);
        }
        attempt(0);
      });
      return loaded[clean];
    }
    window.__VOLO_VENDOR__ = {
      bases: bases,
      katexCss: "katex@0.16.11/dist/katex.min.css",
      katexJs: "katex@0.16.11/dist/katex.min.js",
      katexAutoRenderJs: "katex@0.16.11/dist/contrib/auto-render.min.js",
      mermaidJs: "mermaid@10/dist/mermaid.min.js",
      url: function (sub) { return bases[0] + "/" + norm(sub); },
      loadStyle: loadStyle,
      loadScript: loadScript
    };
    return window.__VOLO_VENDOR__;
  }

  function ensureKaTeX() {
    if (window.renderMathInElement) return Promise.resolve();
    if (!katexPromise) {
      var v = getVendorRuntime();
      v.loadStyle("volo-katex-css", v.katexCss || "katex@0.16.11/dist/katex.min.css");
      katexPromise = v.loadScript(v.katexJs || "katex@0.16.11/dist/katex.min.js")
        .then(function () {
          return v.loadScript(v.katexAutoRenderJs || "katex@0.16.11/dist/contrib/auto-render.min.js");
        })
        .catch(function (err) {
          katexPromise = null;
          throw err;
        });
    }
    return katexPromise;
  }

  function ensureMermaid() {
    if (window.mermaid) return Promise.resolve(window.mermaid);
    if (!mermaidPromise) {
      var v = getVendorRuntime();
      mermaidPromise = v.loadScript(v.mermaidJs || "mermaid@10/dist/mermaid.min.js")
        .then(function () { return window.mermaid; })
        .catch(function (err) {
          mermaidPromise = null;
          throw err;
        });
    }
    return mermaidPromise;
  }

  function scheduleMermaidRender(nodes, gen) {
    mermaidQueue = mermaidQueue.then(function () {
      if (gen !== undefined && gen !== mermaidGen) return;
      if (!window.mermaid || !nodes || !nodes.length) return;
      var liveNodes = nodes.filter(function (el) { return el && el.isConnected; });
      if (!liveNodes.length) return;
      liveNodes.forEach(function (el) {
        var raw = el.getAttribute("data-mermaid-src");
        if (raw === null) {
          raw = el.textContent || "";
          el.setAttribute("data-mermaid-src", raw);
        } else if (el.getAttribute("data-processed") || el.querySelector("svg")) {
          el.removeAttribute("data-processed");
          el.textContent = raw;
        }
      });
      var isDark = document.documentElement.getAttribute("data-theme") === "dark";
      try {
        window.mermaid.initialize({
          startOnLoad: false,
          theme: isDark ? "dark" : "default",
          securityLevel: "strict"
        });
        var p = window.mermaid.run({ nodes: liveNodes });
        if (p && typeof p.then === "function") {
          return p.catch(function () {});
        }
      } catch (_) {}
    }).catch(function () {});
    return mermaidQueue;
  }

  function reRenderMermaidForTheme() {
    var rendered = Array.prototype.slice.call(
      document.querySelectorAll("pre.mermaid[data-mermaid-src], div.mermaid[data-mermaid-src]")
    );
    if (!rendered.length || !window.mermaid) return;
    var gen = ++mermaidGen;
    scheduleMermaidRender(rendered, gen);
  }

  function sanitizeNextPath(raw) {
    var s = String(raw || "").trim();
    if (!s || s.charAt(0) !== "/" || s.indexOf("//") === 0 || s.indexOf("/\\") === 0) return "";
    var pathOnly = s.split("#")[0].split("?")[0];
    if (pathOnly === "/reader/login" || pathOnly === "/reader/register") return "";
    return s;
  }

  function syncHeaderLoginNextLinks() {
    var curNext = sanitizeNextPath((window.location.pathname || "/") + (window.location.search || ""));
    var nextHref = curNext ? "/reader/login?next=" + encodeURIComponent(curNext) : "/reader/login";
    document.querySelectorAll('.js-header-reader-login[data-reader-login="1"]').forEach(function (a) {
      a.setAttribute("href", nextHref);
    });
  }

  function enhanceProse(scope) {
    var root = scope || document;
    var curNext = sanitizeNextPath((window.location.pathname || "/") + (window.location.search || ""));
    if (curNext) {
      root.querySelectorAll('.volo-lock-card a[href^="/reader/login"]').forEach(function (a) {
        var h = a.getAttribute("href") || "";
        if (h === "/reader/login") {
          a.setAttribute("href", "/reader/login?next=" + encodeURIComponent(curNext));
        }
      });
    }
    root.querySelectorAll(".volo-prose table:not(.volo-github-repos-table)").forEach(function (tbl) {
      var p = tbl.parentElement;
      if (!p || p.classList.contains("volo-table-wrap") || p.classList.contains("volo-table-scroll") || p.classList.contains("volo-github-repos-wrap")) return;
      var wrap = document.createElement("div");
      wrap.className = "volo-table-wrap";
      p.insertBefore(wrap, tbl);
      wrap.appendChild(tbl);
    });
    root.querySelectorAll(".article-foot-tags").forEach(function (box) {
      if (box.querySelector(".js-foot-tags-toggle")) return;
      var chips = Array.prototype.slice.call(box.querySelectorAll("a.tag-chip"));
      if (chips.length <= 12) return;
      var extra = chips.slice(12);
      extra.forEach(function (c) {
        c.setAttribute("data-extra-tag", "");
        c.hidden = true;
      });
      var btn = document.createElement("button");
      btn.type = "button";
      btn.className = "tag-chip studio-tags-more-btn js-foot-tags-toggle";
      btn.setAttribute("aria-expanded", "false");
      btn.setAttribute("data-more-label", "+" + extra.length);
      btn.setAttribute("data-less-label", "−");
      btn.textContent = "+" + extra.length;
      box.appendChild(btn);
    });
  }

  function initEnhancements(scope) {
    var root = scope || document;
    var hasMathFlag = !!(
      (root.querySelector && root.querySelector('[data-needs-math="1"]')) ||
      (root.getAttribute && root.getAttribute("data-needs-math") === "1")
    );
    if (hasMathFlag) {
      ensureKaTeX().then(function () {
        if (!window.renderMathInElement) return;
        root.querySelectorAll(".volo-prose").forEach(function (prose) {
          if (prose.closest(".vc-item")) return;
          if (prose.dataset.mathRendered === "1") return;
          prose.dataset.mathRendered = "1";
          try {
            window.renderMathInElement(prose, {
              delimiters: [
                { left: "$$", right: "$$", display: true },
                { left: "\\[", right: "\\]", display: true },
                { left: "$", right: "$", display: false },
                { left: "\\(", right: "\\)", display: false }
              ],
              throwOnError: false,
              ignoredTags: ["script", "noscript", "style", "textarea", "pre", "code", "option"]
            });
          } catch (_) {}
        });
      }).catch(function () {});
    }

    var mermaidNodes = Array.prototype.slice.call(root.querySelectorAll("pre.mermaid, div.mermaid"));
    mermaidNodes.forEach(function (el) {
      if (el.getAttribute("data-mermaid-src") === null) {
        el.setAttribute("data-mermaid-src", el.textContent || "");
      }
    });
    var hasMermaidFlag = mermaidNodes.length > 0 || !!(
      (root.querySelector && root.querySelector('[data-needs-mermaid="1"]')) ||
      (root.getAttribute && root.getAttribute("data-needs-mermaid") === "1")
    );
    if (hasMermaidFlag && mermaidNodes.length > 0) {
      var gen = ++mermaidGen;
      ensureMermaid().then(function () {
        scheduleMermaidRender(mermaidNodes, gen);
      }).catch(function () {});
    }
  }

  function initShareBar(scope) {
    var canNative = typeof navigator !== "undefined" && typeof navigator.share === "function";
    (scope || document).querySelectorAll('[data-share-act="native"]').forEach(function (btn) {
      btn.hidden = !canNative;
    });
  }

  /* 5-Column GitHub Showcase Description Clamp & Click-to-Expand */
  function inspectGitHubDescriptions(scope) {
    (scope || document).querySelectorAll(".volo-github-repo-desc").forEach(function (el) {
      if (el.classList.contains("is-expanded")) return;
      var truncatable = el.scrollHeight > el.clientHeight + 2;
      el.classList.toggle("is-truncatable", truncatable);
      if (truncatable) {
        el.tabIndex = 0;
        el.setAttribute("role", "button");
        if (!el.hasAttribute("aria-expanded")) el.setAttribute("aria-expanded", "false");
      } else {
        el.removeAttribute("tabindex");
        el.removeAttribute("role");
        el.removeAttribute("aria-expanded");
      }
    });
  }

  /* Desktop Dockable TOC, Mobile TOC Drawer & rAF-Throttled Scroll-Spy */
  function initArticleTOC(scope) {
    while (tocCleanups.length) {
      var fn = tocCleanups.pop();
      try { fn(); } catch (_) {}
    }
    lastActiveTocID = "";
    pinnedTocID = "";
    pinnedTocY = -1;

    var root = scope || document;
    var tocCard = root.querySelector("#volo-toc-card") || document.getElementById("volo-toc-card");
    var toggleBtn = root.querySelector("#volo-toc-toggle") || document.getElementById("volo-toc-toggle");
    var tocAside = root.querySelector("#volo-toc-aside") || document.getElementById("volo-toc-aside") ||
      root.querySelector("#volo-toc-body") || document.getElementById("volo-toc-body");
    var tocList = root.querySelector("#volo-toc-list") || document.getElementById("volo-toc-list");
    var isMinimalTpl = !!(root.querySelector && root.querySelector('article[data-page-template="minimal"]'));
    var mobileBtn = document.getElementById("volo-mobile-toc-btn");
    var mobileDrawer = document.getElementById("volo-toc-drawer");

    if (mobileBtn) {
      mobileBtn.hidden = !mobileDrawer || !tocList || isMinimalTpl;
      mobileBtn.setAttribute("aria-expanded", "false");
    }
    document.querySelectorAll(".js-mobile-toc-trigger").forEach(function (b) {
      b.setAttribute("aria-expanded", "false");
    });

    var syncTocAsideA11y = function (isCollapsed) {
      if (!tocAside) return;
      tocAside.setAttribute("aria-hidden", isCollapsed ? "true" : "false");
      if ("inert" in tocAside) tocAside.inert = !!isCollapsed;
    };

    if (tocCard && toggleBtn) {
      var collapsed = false;
      try { collapsed = sessionStorage.getItem("volo_toc_collapsed") === "1"; } catch (_) {}
      tocCard.classList.toggle("is-collapsed", collapsed);
      toggleBtn.setAttribute("aria-expanded", collapsed ? "false" : "true");
      syncTocAsideA11y(collapsed);
      var onToggle = function () {
        var next = !tocCard.classList.contains("is-collapsed");
        tocCard.classList.toggle("is-collapsed", next);
        toggleBtn.setAttribute("aria-expanded", next ? "false" : "true");
        syncTocAsideA11y(next);
        try { sessionStorage.setItem("volo_toc_collapsed", next ? "1" : "0"); } catch (_) {}
      };
      toggleBtn.addEventListener("click", onToggle);
      tocCleanups.push(function () { toggleBtn.removeEventListener("click", onToggle); });
    }

    var links = Array.prototype.slice.call(document.querySelectorAll(".volo-toc-link[data-toc-id]"));
    if (!links.length) return;

    var headings = [];
    var seen = {};
    links.forEach(function (a) {
      var id = a.getAttribute("data-toc-id");
      if (!id || seen[id]) return;
      var el = findHeadingTarget(id);
      if (el) {
        seen[id] = true;
        headings.push(el);
      }
    });

    var spyPending = false;
    function updateSpy() {
      spyPending = false;
      if (scrollAnimRAF !== 0 || !headings.length) return;
      if (pinnedTocID) {
        if (Math.abs((window.scrollY || window.pageYOffset || 0) - pinnedTocY) <= 4) {
          setActiveTOCLink(pinnedTocID, false);
          return;
        }
        pinnedTocID = "";
      }
      var threshold = getHeaderOffset() + 18;
      var lo = 0;
      var hi = headings.length - 1;
      var idx = 0;
      while (lo <= hi) {
        var mid = (lo + hi) >> 1;
        if (headings[mid].getBoundingClientRect().top <= threshold) {
          idx = mid;
          lo = mid + 1;
        } else {
          hi = mid - 1;
        }
      }
      setActiveTOCLink(headings[idx].id, false);
    }

    function requestSpy() {
      if (scrollAnimRAF !== 0 || spyPending) return;
      spyPending = true;
      requestAnimationFrame(updateSpy);
    }

    function onScrollEnd() {
      spyPending = false;
      updateSpy();
    }

    window.addEventListener("scroll", requestSpy, { passive: true });
    window.addEventListener("resize", requestSpy, { passive: true });
    window.addEventListener("volo:scrollend", onScrollEnd);
    tocCleanups.push(function () {
      window.removeEventListener("scroll", requestSpy);
      window.removeEventListener("resize", requestSpy);
      window.removeEventListener("volo:scrollend", onScrollEnd);
    });
    updateSpy();
  }

  /* Auto-Linkify [mm:ss] / [hh:mm:ss] Timestamps in Prose When Podcast Audio Exists */
  function linkifyPodcastTimestamps(root) {
    var audio = root.querySelector("#volo-podcast-audio") || document.getElementById("volo-podcast-audio");
    var prose = root.querySelector(".volo-prose");
    if (!audio || !prose || prose.dataset.tsLinked === "1") return;
    prose.dataset.tsLinked = "1";

    var walker = document.createTreeWalker(prose, NodeFilter.SHOW_TEXT, null);
    var nodes = [];
    var n;
    while ((n = walker.nextNode())) {
      var p = n.parentElement;
      if (!p || p.closest("pre, code, a, button, script, style")) continue;
      if (/\[(?:\d{1,2}:)?\d{1,2}:\d{2}\]/.test(n.nodeValue || "")) nodes.push(n);
    }

    var re = /\[((?:\d{1,2}:)?\d{1,2}:\d{2})\]/g;
    nodes.forEach(function (textNode) {
      var val = textNode.nodeValue || "";
      re.lastIndex = 0;
      var frag = document.createDocumentFragment();
      var lastIdx = 0;
      var m;
      while ((m = re.exec(val)) !== null) {
        if (m.index > lastIdx) frag.appendChild(document.createTextNode(val.slice(lastIdx, m.index)));
        var raw = m[1];
        var parts = raw.split(":").map(function (x) { return parseInt(x, 10) || 0; });
        var secs = parts.length === 3 ? parts[0] * 3600 + parts[1] * 60 + parts[2] : parts[0] * 60 + parts[1];
        var btn = document.createElement("button");
        btn.type = "button";
        btn.className = "volo-audio-ts";
        btn.setAttribute("data-audio-seek", String(secs));
        btn.textContent = "▶ " + raw;
        frag.appendChild(btn);
        lastIdx = m.index + m[0].length;
      }
      if (lastIdx < val.length) frag.appendChild(document.createTextNode(val.slice(lastIdx)));
      if (textNode.parentNode) textNode.parentNode.replaceChild(frag, textNode);
    });
  }

  /* Podcast Speed, Unlock, Reactions, Reader Auth & Bookshelf Progress */
  function readBookProgress() {
    try {
      return JSON.parse(localStorage.getItem(BOOK_PROGRESS_KEY) || "{}") || {};
    } catch (_) {
      return {};
    }
  }

  function writeBookProgress(slug, data) {
    if (!slug || !data) return;
    var map = readBookProgress();
    map[slug] = data;
    try { localStorage.setItem(BOOK_PROGRESS_KEY, JSON.stringify(map)); } catch (_) {}
  }

  var pendingFilterFocus = null;
  var filterNavTimer = null;

  function scheduleServerFilterNav(selector, rawVal, preserveKeys) {
    clearTimeout(filterNavTimer);
    filterNavTimer = setTimeout(function () {
      try {
        var u = new URL(window.location.href);
        var trimmed = String(rawVal || "").trim();
        var curQ = (u.searchParams.get("q") || "").trim();
        if (trimmed === curQ) return;
        var nextParams = new URLSearchParams();
        (preserveKeys || []).forEach(function (k) {
          var v = u.searchParams.get(k);
          if (v) nextParams.set(k, v);
        });
        if (trimmed) nextParams.set("q", trimmed);
        var qs = nextParams.toString();
        var nextURL = u.pathname + (qs ? "?" + qs : "");
        var activeEl = document.querySelector(selector);
        pendingFilterFocus = {
          sel: selector,
          val: rawVal,
          pos: activeEl && typeof activeEl.selectionStart === "number" ? activeEl.selectionStart : String(rawVal || "").length
        };
        navigatePJAX(nextURL, false);
      } catch (_) {}
    }, 300);
  }

  function restorePendingFilterFocus(root) {
    if (!pendingFilterFocus) return;
    var info = pendingFilterFocus;
    pendingFilterFocus = null;
    var input = (root || document).querySelector(info.sel);
    if (!input) return;
    if (typeof info.val === "string" && input.value !== info.val) {
      input.value = info.val;
    }
    try {
      input.focus({ preventScroll: true });
      if (typeof input.setSelectionRange === "function") {
        input.setSelectionRange(info.pos, info.pos);
      }
    } catch (_) {}
  }

  function initSeriesBoxScroll(scope) {
    (scope || document).querySelectorAll(".volo-series-box").forEach(function (box) {
      var scrollCur = function () {
        var cur = box.querySelector(".volo-series-box__list .is-current");
        if (!cur) return;
        var list = box.querySelector(".volo-series-box__list");
        if (list && list.scrollHeight > list.clientHeight) {
          centerActiveLinkInContainer(list, cur);
        } else if (typeof cur.scrollIntoView === "function") {
          cur.scrollIntoView({ block: "nearest" });
        }
      };
      if (box.open) requestAnimationFrame(scrollCur);
      if (box.dataset.boundSeriesScroll !== "1") {
        box.dataset.boundSeriesScroll = "1";
        box.addEventListener("toggle", function () {
          if (box.open) scrollCur();
        });
      }
    });
  }

  function initTaxonomyAndBookFilters(scope) {
    var root = scope || document;
    var mainWrap = document.getElementById("studio-main") || root;
    var tagIn = root.querySelector(".js-tag-filter-input");
    var tagList = root.querySelector(".js-tag-filter-list, .js-tag-filter-grid");
    var tagEmpty = root.querySelector(".js-tag-filter-empty");
    var tagClear = root.querySelector(".js-tag-filter-clear");
    var tagCount = root.querySelector(".js-tag-filter-count");
    if (tagIn && tagIn.dataset.boundFilter !== "1") {
      tagIn.dataset.boundFilter = "1";
      var serverTagQ = tagClear ? (tagClear.getAttribute("data-server-query") || "") : "";
      var pageCnt = tagCount ? (parseInt(tagCount.getAttribute("data-page-count") || "0", 10) || 0) : 0;
      var totalCnt = tagCount ? (parseInt(tagCount.getAttribute("data-total-count") || "0", 10) || pageCnt) : 0;
      var hasTagPager = !!(serverTagQ || totalCnt > pageCnt || mainWrap.querySelector(".pagination"));
      var syncTagFilter = function (fromInput) {
        var q = String(tagIn.value || "").trim().toLowerCase();
        var shown = 0;
        if (tagList) {
          tagList.querySelectorAll("[data-tag-name], [data-tag-filter]").forEach(function (item) {
            var hay = (item.getAttribute("data-tag-name") || item.getAttribute("data-tag-filter") || item.textContent || "").toLowerCase();
            var match = !q || hay.indexOf(q) !== -1;
            item.hidden = !match;
            if (match) shown++;
          });
        }
        if (tagEmpty) tagEmpty.hidden = !q || !tagList || shown > 0;
        if (tagClear) tagClear.hidden = !q && !serverTagQ;
        if (tagCount) {
          if (q && tagList) {
            tagCount.textContent = shown + " / " + (totalCnt || pageCnt);
          } else if (!serverTagQ && totalCnt > pageCnt) {
            tagCount.textContent = pageCnt + " / " + totalCnt;
          } else {
            tagCount.textContent = String(pageCnt);
          }
        }
        if (fromInput && hasTagPager && !tagIn._composing) {
          scheduleServerFilterNav(".js-tag-filter-input", tagIn.value, ["sort", "skin", "theme_preview"]);
        }
      };
      tagIn.addEventListener("compositionstart", function () { tagIn._composing = true; });
      tagIn.addEventListener("compositionend", function () { tagIn._composing = false; syncTagFilter(true); });
      tagIn.addEventListener("input", function () { syncTagFilter(true); });
      if (tagClear) {
        tagClear.addEventListener("click", function (ev) {
          if (!serverTagQ) {
            ev.preventDefault();
            tagIn.value = "";
            syncTagFilter(false);
            tagIn.focus();
          }
        });
      }
      if (tagIn.value && tagIn.value !== serverTagQ) syncTagFilter(false);
    }

    root.querySelectorAll("[data-collection-root]").forEach(function (colRoot) {
      var filterIn = colRoot.querySelector(".js-collection-filter");
      if (!filterIn || filterIn.dataset.boundFilter === "1") return;
      filterIn.dataset.boundFilter = "1";
      var emptyEl = colRoot.querySelector(".js-collection-empty");
      var countEl = colRoot.querySelector(".js-collection-count");
      var groups = Array.prototype.slice.call(colRoot.querySelectorAll("[data-collection-group]"));
      var allItems = Array.prototype.slice.call(colRoot.querySelectorAll("[data-collection-item]"));
      var totalItems = countEl ? (parseInt(countEl.getAttribute("data-total") || "0", 10) || allItems.length) : allItems.length;
      var syncCollection = function () {
        var q = String(filterIn.value || "").trim().toLowerCase();
        var shown = 0;
        allItems.forEach(function (item) {
          var hay = (item.getAttribute("data-collection-item") || item.textContent || "").toLowerCase();
          var match = !q || hay.indexOf(q) !== -1;
          item.hidden = !match;
          if (match) shown++;
        });
        groups.forEach(function (grp) {
          var grpVisible = Array.prototype.some.call(grp.querySelectorAll("[data-collection-item]"), function (el) {
            return !el.hidden;
          });
          grp.hidden = !!q && !grpVisible;
        });
        if (countEl) {
          countEl.textContent = q ? (shown + " / " + totalItems) : String(totalItems);
        }
        if (emptyEl) emptyEl.hidden = !q || shown > 0;
      };
      filterIn.addEventListener("input", syncCollection);
      if (filterIn.value) syncCollection();
    });

    var bookIn = root.querySelector(".js-book-filter-input");
    var bookGrid = root.querySelector(".js-book-filter-grid");
    var bookEmpty = root.querySelector(".js-book-filter-empty");
    if (bookIn && bookIn.dataset.boundFilter !== "1") {
      bookIn.dataset.boundFilter = "1";
      var urlBookQ = "";
      try {
        urlBookQ = new URLSearchParams(window.location.search).get("q") || "";
        if (!bookIn.value && urlBookQ) bookIn.value = urlBookQ;
      } catch (_) {}
      var hasBookPager = !!(urlBookQ || mainWrap.querySelector(".pagination"));
      var syncBookFilter = function (fromInput) {
        var q = String(bookIn.value || "").trim().toLowerCase();
        var shown = 0;
        if (bookGrid) {
          bookGrid.querySelectorAll("[data-book-filter]").forEach(function (item) {
            var hay = (item.getAttribute("data-book-filter") || item.textContent || "").toLowerCase();
            var match = !q || hay.indexOf(q) !== -1;
            item.hidden = !match;
            if (match) shown++;
          });
        }
        if (bookEmpty && bookGrid) bookEmpty.hidden = !q || shown > 0;
        if (fromInput && hasBookPager && !bookIn._composing) {
          scheduleServerFilterNav(".js-book-filter-input", bookIn.value, ["skin", "theme_preview"]);
        }
      };
      bookIn.addEventListener("compositionstart", function () { bookIn._composing = true; });
      bookIn.addEventListener("compositionend", function () { bookIn._composing = false; syncBookFilter(true); });
      bookIn.addEventListener("input", function () { syncBookFilter(true); });
      if (bookIn.value && bookIn.value !== urlBookQ) syncBookFilter(false);
    }

    restorePendingFilterFocus(root);
  }

  function centerActiveLinkInContainer(navEl, linkEl) {
    if (!navEl || !linkEl) return;
    requestAnimationFrame(function () {
      var nr = navEl.getBoundingClientRect();
      var lr = linkEl.getBoundingClientRect();
      if (nr.height <= 0 || navEl.scrollHeight <= navEl.clientHeight) return;
      var relTop = lr.top - nr.top + navEl.scrollTop;
      navEl.scrollTop = Math.max(0, relTop - Math.round((navEl.clientHeight - lr.height) / 2));
    });
  }

  function syncBookFullscreenBtn(fsBtn, isFull) {
    if (!fsBtn) return;
    var nextLbl = isFull
      ? (fsBtn.getAttribute("data-exit-label") || "")
      : (fsBtn.getAttribute("data-enter-label") || "");
    var fsLbl = fsBtn.querySelector(".js-book-fs-label");
    if (fsLbl && nextLbl) fsLbl.textContent = nextLbl;
    else if (nextLbl && !fsBtn.querySelector("svg")) fsBtn.textContent = nextLbl;
    if (nextLbl) {
      fsBtn.setAttribute("title", nextLbl);
      fsBtn.setAttribute("aria-label", nextLbl);
    }
    fsBtn.setAttribute("aria-pressed", isFull ? "true" : "false");
  }

  function closeMobileBookTOC() {
    if ((window.innerWidth || 1024) > 900) return;
    var reader = document.querySelector("[data-book-reader]");
    if (!reader || reader.classList.contains("is-toc-collapsed")) return;
    reader.classList.add("is-toc-collapsed");
    var bd = reader.querySelector(".js-book-toc-backdrop");
    if (bd) {
      bd.classList.remove("is-open");
      bd.hidden = true;
    }
    reader.querySelectorAll(".js-book-toggle-toc").forEach(function (btn) {
      btn.setAttribute("aria-expanded", "false");
      var expLbl = btn.getAttribute("data-expand-label");
      var lblSpan = btn.querySelector(".js-book-toggle-label");
      if (lblSpan && expLbl) lblSpan.textContent = expLbl;
      else if (expLbl && !btn.querySelector("svg")) btn.textContent = expLbl;
    });
    var mobTocBtn = reader.querySelector(".js-book-mobile-toc");
    if (mobTocBtn) {
      mobTocBtn.hidden = !((window.innerWidth || 1024) <= 900 && (window.scrollY || 0) > 160);
    }
    syncBodyScrollLock();
  }

  function initBookshelfAndReader(root) {
    var progress = readBookProgress();
    root.querySelectorAll("[data-book-card]").forEach(function (card) {
      var slug = card.getAttribute("data-book-card") || "";
      var saved = progress[slug];
      var badge = card.querySelector(".js-book-resume");
      if (badge && saved && saved.url) {
        var base = badge.getAttribute("data-label") || badge.textContent || "";
        badge.href = saved.url;
        badge.textContent = saved.chapterTitle ? base + " · " + saved.chapterTitle : base;
        badge.hidden = false;
        var foot = badge.closest(".book-card__foot");
        if (foot) foot.hidden = false;
      }
    });

    var reader = root.querySelector("[data-book-reader]");
    if (!reader) {
      document.body.classList.remove("is-book-fullscreen");
      return;
    }
    var slug = reader.getAttribute("data-book-reader") || "";
    var activeID = reader.getAttribute("data-active-chapter-id") || "";
    var activeTitle = reader.getAttribute("data-active-chapter-title") || "";
    var activeURL = reader.getAttribute("data-active-chapter-url") || "";
    var isExternalIframe = reader.getAttribute("data-external-iframe") === "1";
    if (slug && activeID && activeURL) {
      writeBookProgress(slug, { chapterId: activeID, chapterTitle: activeTitle, url: activeURL, ts: Date.now() });
    }

    reader.querySelectorAll(".book-reader-frame").forEach(function (frame) {
      if (frame.dataset.boundLoad === "1") return;
      frame.dataset.boundLoad = "1";
      var wrap = frame.closest(".book-reader-frame-wrap");
      if (!wrap) return;
      var clearSpin = function () { wrap.classList.remove("is-loading"); };
      frame.addEventListener("load", clearSpin, { once: true });
      frame.addEventListener("error", clearSpin, { once: true });
      setTimeout(clearSpin, 3500);
    });

    var chapFilter = reader.querySelector(".js-book-chap-filter");
    var chapEmpty = reader.querySelector(".js-book-chap-empty");
    var chapNav = reader.querySelector(".book-reader-toc__nav");
    if (chapFilter && chapNav && chapFilter.dataset.boundFilter !== "1") {
      chapFilter.dataset.boundFilter = "1";
      chapFilter.addEventListener("input", function () {
        var q = String(chapFilter.value || "").trim().toLowerCase();
        var shown = 0;
        var children = Array.prototype.slice.call(chapNav.children);
        children.forEach(function (el) {
          if (el.classList.contains("book-chap-link") || el.classList.contains("book-reader-toc__link")) {
            var hay = (el.getAttribute("data-chapter-title") || el.textContent || "").toLowerCase();
            var match = !q || hay.indexOf(q) !== -1;
            el.hidden = !match;
            if (match) shown++;
          }
        });
        for (var gi = 0; gi < children.length; gi++) {
          var node = children[gi];
          if (!node.classList.contains("book-reader-toc__group")) continue;
          if (!q) {
            node.hidden = false;
            continue;
          }
          var hasVisibleChild = false;
          for (var gj = gi + 1; gj < children.length; gj++) {
            if (children[gj].classList.contains("book-reader-toc__group")) break;
            if (!children[gj].hidden) { hasVisibleChild = true; break; }
          }
          node.hidden = !hasVisibleChild;
        }
        if (chapEmpty) chapEmpty.hidden = !q || shown > 0;
      });
    }

    var prefKey = "volo_book_toc_" + (slug || "default");
    var savedPref = "";
    try { savedPref = sessionStorage.getItem(prefKey) || ""; } catch (_) {}

    if (window.innerWidth <= 900 && reader.dataset.mobileTocInit !== "1") {
      reader.dataset.mobileTocInit = "1";
      reader.classList.add("is-toc-collapsed");
    } else if (savedPref === "collapsed") {
      reader.classList.add("is-toc-collapsed");
    } else if (savedPref === "expanded" && window.innerWidth > 900) {
      reader.classList.remove("is-toc-collapsed");
    } else if (isExternalIframe) {
      reader.classList.add("is-toc-collapsed");
    }

    var backdrop = reader.querySelector(".js-book-toc-backdrop");
    var mobTocBtn = reader.querySelector(".js-book-mobile-toc");
    var tocBtns = Array.prototype.slice.call(reader.querySelectorAll(".js-book-toggle-toc"));

    var scrollBookTocActive = function () {
      if (reader.classList.contains("is-toc-collapsed")) return;
      var actCh = reader.querySelector(".book-chap-link.active, .book-reader-toc__link.is-active");
      if (!actCh) return;
      var navInner = reader.querySelector(".book-reader-toc__nav");
      var sideBox = reader.querySelector(".book-sidebar, .book-reader-toc");
      var scrollBox = (navInner && navInner.scrollHeight > navInner.clientHeight + 2) ? navInner : sideBox;
      if (scrollBox) centerActiveLinkInContainer(scrollBox, actCh);
    };

    var syncMobBookBtn = function () {
      if (!mobTocBtn) return;
      var isCol = reader.classList.contains("is-toc-collapsed");
      mobTocBtn.hidden = !(window.innerWidth <= 900 && isCol && (window.scrollY || 0) > 160);
    };

    var syncTocBtns = function () {
      var isCol = reader.classList.contains("is-toc-collapsed");
      var isMobile = window.innerWidth <= 900;
      tocBtns.forEach(function (btn) {
        btn.setAttribute("aria-expanded", isCol ? "false" : "true");
        var expLbl = btn.getAttribute("data-expand-label");
        var colLbl = btn.getAttribute("data-collapse-label");
        var lblSpan = btn.querySelector(".js-book-toggle-label");
        if (lblSpan && (expLbl || colLbl)) {
          lblSpan.textContent = isCol ? (expLbl || lblSpan.textContent) : (colLbl || lblSpan.textContent);
        } else if ((expLbl || colLbl) && !btn.querySelector("svg")) {
          btn.textContent = isCol ? (expLbl || btn.textContent) : (colLbl || btn.textContent);
        }
      });
      if (backdrop) {
        if (isMobile && !isCol) {
          backdrop.hidden = false;
          requestAnimationFrame(function () { backdrop.classList.add("is-open"); });
        } else {
          backdrop.classList.remove("is-open");
          backdrop.hidden = true;
        }
      }
      syncMobBookBtn();
      syncBodyScrollLock();
    };

    reader.querySelectorAll("[data-chapter-id]").forEach(function (link) {
      link.addEventListener("click", function () {
        var cid = link.getAttribute("data-chapter-id") || "";
        var ctitle = link.getAttribute("data-chapter-title") || "";
        var href = link.getAttribute("href") || "";
        if (slug && cid && href && href.charAt(0) === "/") {
          writeBookProgress(slug, { chapterId: cid, chapterTitle: ctitle, url: href, ts: Date.now() });
        }
        if (window.innerWidth <= 900) {
          reader.classList.add("is-toc-collapsed");
          syncTocBtns();
        }
      });
    });

    if (backdrop) {
      backdrop.addEventListener("click", function () {
        reader.classList.add("is-toc-collapsed");
        syncTocBtns();
      });
    }

    if (tocBtns.length) {
      syncTocBtns();
      tocBtns.forEach(function (btn) {
        btn.addEventListener("click", function () {
          var nextCol = reader.classList.toggle("is-toc-collapsed");
          if (window.innerWidth > 900) {
            try { sessionStorage.setItem(prefKey, nextCol ? "collapsed" : "expanded"); } catch (_) {}
          }
          syncTocBtns();
          if (!nextCol) scrollBookTocActive();
        });
      });
    } else {
      syncTocBtns();
    }
    scrollBookTocActive();

    var onBookViewportChange = function () {
      if (backdrop) {
        var showBd = window.innerWidth <= 900 && !reader.classList.contains("is-toc-collapsed");
        backdrop.hidden = !showBd;
        backdrop.classList.toggle("is-open", showBd);
      }
      syncMobBookBtn();
      syncBodyScrollLock();
    };
    window.addEventListener("scroll", syncMobBookBtn, { passive: true });
    window.addEventListener("resize", onBookViewportChange, { passive: true });
    tocCleanups.push(function () {
      window.removeEventListener("scroll", syncMobBookBtn);
      window.removeEventListener("resize", onBookViewportChange);
    });

    if (mobTocBtn) {
      mobTocBtn.addEventListener("click", function () {
        reader.classList.remove("is-toc-collapsed");
        syncTocBtns();
        scrollBookTocActive();
      });
    }

    var fsBtn = reader.querySelector(".js-book-fullscreen");
    if (fsBtn) {
      syncBookFullscreenBtn(fsBtn, document.body.classList.contains("is-book-fullscreen") || !!document.fullscreenElement);
      fsBtn.addEventListener("click", function () {
        var full = document.body.classList.toggle("is-book-fullscreen");
        syncBookFullscreenBtn(fsBtn, full);
      });
      var onFsChange = function () {
        var activeFull = document.body.classList.contains("is-book-fullscreen") || !!document.fullscreenElement;
        syncBookFullscreenBtn(fsBtn, activeFull);
      };
      document.addEventListener("fullscreenchange", onFsChange);
      tocCleanups.push(function () {
        document.removeEventListener("fullscreenchange", onFsChange);
      });
    }
  }

  function normAudioHref(el) {
    if (!el) return "";
    var raw = el.getAttribute("src") || el.currentSrc || "";
    if (!raw) return "";
    try { return new URL(raw, window.location.origin).href; } catch (_) { return raw; }
  }

  function syncPodcastOpenState() {
    var mini = document.getElementById("volo-podcast-mini");
    var open = !!(mini && !mini.hidden);
    if (open) {
      document.documentElement.setAttribute("data-podcast-open", "1");
      if (document.body) document.body.classList.add("has-podcast-mini");
    } else {
      document.documentElement.removeAttribute("data-podcast-open");
      if (document.body) document.body.classList.remove("has-podcast-mini");
    }
  }

  function dockPlayingPodcastBeforePJAX(mainEl) {
    if (!mainEl) return;
    var liveAudio = mainEl.querySelector("#volo-podcast-audio");
    if (!liveAudio || liveAudio.paused || liveAudio.ended) return;
    var srcHref = normAudioHref(liveAudio);
    if (!srcHref) return;
    var dock = document.getElementById("volo-persist-dock") || document.body;
    if (!dock) return;

    var prevMini = document.getElementById("volo-podcast-mini");
    if (prevMini) {
      var prevAud = prevMini.querySelector("audio");
      if (prevAud && prevAud !== liveAudio) { try { prevAud.pause(); } catch (_) {} }
      prevMini.remove();
    }

    var epTitle = liveAudio.getAttribute("data-episode-title") || document.title || "Podcast";
    var rawEpURL = liveAudio.getAttribute("data-episode-url") || "";
    var safeEpRel = String(window.location.pathname || "/").replace(/^[\/\\]+/, "");
    if (rawEpURL && rawEpURL.charAt(0) === "/" && rawEpURL.indexOf("//") !== 0 && rawEpURL.indexOf("/\\") !== 0) {
      safeEpRel = rawEpURL
        .split("#")[0]
        .split("?")[0]
        .split("/")
        .filter(Boolean)
        .map(function (seg) {
          try { return encodeURIComponent(decodeURIComponent(seg)); } catch (_) { return encodeURIComponent(seg); }
        })
        .join("/");
    }
    liveAudio.removeAttribute("id");
    liveAudio.className = "volo-podcast-mini__audio";

    var mini = document.createElement("div");
    mini.id = "volo-podcast-mini";
    mini.className = "volo-podcast-mini";
    mini.setAttribute("data-volo-persist", "podcast-mini");

    var head = document.createElement("div");
    head.className = "volo-podcast-mini__head";
    var titleLink = document.createElement("a");
    titleLink.className = "volo-podcast-mini__title";
    titleLink.href = window.location.origin + "/" + safeEpRel;
    titleLink.textContent = "🎙 " + epTitle;
    titleLink.title = epTitle;
    var closeBtn = document.createElement("button");
    closeBtn.type = "button";
    closeBtn.className = "volo-podcast-mini__close";
    closeBtn.setAttribute("aria-label", "Close");
    closeBtn.textContent = "✕";
    closeBtn.addEventListener("click", function () {
      try { liveAudio.pause(); } catch (_) {}
      mini.remove();
      syncPodcastOpenState();
    });
    head.appendChild(titleLink);
    head.appendChild(closeBtn);
    mini.appendChild(head);
    mini.appendChild(liveAudio);
    dock.appendChild(mini);
    syncPodcastOpenState();
    if (liveAudio.paused) {
      liveAudio.play().catch(function () {});
    }
  }

  function reconcilePodcastMiniPlayer(root) {
    var mini = document.getElementById("volo-podcast-mini");
    var miniAudio = mini ? mini.querySelector("audio") : null;
    var pageAudio = root.querySelector("#volo-podcast-audio");
    if (!mini || !miniAudio || !pageAudio) {
      syncPodcastOpenState();
      return;
    }

    if (normAudioHref(miniAudio) === normAudioHref(pageAudio)) {
      var wasPlaying = !miniAudio.paused;
      miniAudio.id = "volo-podcast-audio";
      miniAudio.className = "volo-podcast-card__player";
      pageAudio.replaceWith(miniAudio);
      mini.remove();
      syncPodcastOpenState();
      var spBtn = root.querySelector("#volo-podcast-speed");
      if (spBtn && miniAudio.playbackRate) spBtn.textContent = miniAudio.playbackRate + "x";
      if (wasPlaying && miniAudio.paused) {
        miniAudio.play().catch(function () {});
      }
    } else if (pageAudio.dataset.boundMiniStop !== "1") {
      pageAudio.dataset.boundMiniStop = "1";
      pageAudio.addEventListener("play", function () {
        var curMini = document.getElementById("volo-podcast-mini");
        if (!curMini) return;
        var curAud = curMini.querySelector("audio");
        if (curAud) { try { curAud.pause(); } catch (_) {} }
        curMini.remove();
        syncPodcastOpenState();
      });
    }
  }

  function initPodcastAndUnlock(scope) {
    var root = scope || document;
    reconcilePodcastMiniPlayer(root);
    linkifyPodcastTimestamps(root);

    root.querySelectorAll("[data-audio-seek]").forEach(function (btn) {
      if (btn.dataset.boundSeek === "1") return;
      btn.dataset.boundSeek = "1";
      btn.addEventListener("click", function () {
        var sec = parseInt(btn.getAttribute("data-audio-seek") || "0", 10);
        var audio = document.getElementById("volo-podcast-audio");
        if (audio && !isNaN(sec)) {
          audio.currentTime = sec;
          audio.play().catch(function () {});
        }
      });
    });

    var speedBtn = root.querySelector("#volo-podcast-speed");
    if (speedBtn && speedBtn.dataset.boundSpeed !== "1") {
      speedBtn.dataset.boundSpeed = "1";
      var speeds = [1, 1.25, 1.5, 2];
      speedBtn.addEventListener("click", function () {
        var audio = document.getElementById("volo-podcast-audio");
        if (!audio) return;
        var next = speeds[(speeds.indexOf(audio.playbackRate) + 1) % speeds.length] || 1;
        audio.playbackRate = next;
        speedBtn.textContent = next + "x";
      });
    }

    root.querySelectorAll(".js-volo-unlock-form").forEach(function (form) {
      var msg = form.querySelector(".js-unlock-msg");
      var defaultUnlockErr = (msg && msg.textContent.trim()) || "";
      form.addEventListener("submit", function (ev) {
        ev.preventDefault();
        var aid = form.getAttribute("data-article-id");
        var input = form.querySelector("input[type='password']") || form.querySelector("input[name='password']");
        if (!aid || !input) return;
        fetch("/api/v1/articles/" + encodeURIComponent(aid) + "/unlock", {
          method: "POST",
          headers: { "Content-Type": "application/json", "X-Volo-Env-FP": collectEnvFP() },
          body: JSON.stringify({ password: input.value })
        }).then(function (r) {
          if (r.ok) {
            window.location.reload();
            return;
          }
          r.json().catch(function () { return {}; }).then(function (d) {
            if (msg) {
              msg.textContent = (d && d.error) || form.getAttribute("data-err-unlock") || defaultUnlockErr || i18nFallback("unlockErr");
              msg.hidden = false;
            }
          });
        }).catch(function () {
          if (msg) {
            msg.textContent = form.getAttribute("data-err-network") || i18nFallback("netErr");
            msg.hidden = false;
          }
        });
      });
    });

    root.querySelectorAll(".volo-reaction-btn").forEach(function (btn) {
      btn.addEventListener("click", function () {
        var tType = btn.getAttribute("data-target-type");
        var tID = btn.getAttribute("data-target-id");
        var kind = btn.getAttribute("data-kind");
        var headers = { "Content-Type": "application/json", "X-Volo-Env-FP": collectEnvFP() };
        var csrf = getCSRFToken();
        if (csrf) headers["X-CSRF-Token"] = csrf;
        fetch("/api/v1/reactions", {
          method: "POST",
          credentials: "same-origin",
          headers: headers,
          body: JSON.stringify({ target_type: tType, target_id: tID, kind: kind })
        }).then(function (r) { return r.json(); }).then(function (d) {
          if (d && d.counts) {
            root.querySelectorAll(".volo-reaction-btn[data-target-id='" + tID + "']").forEach(function (b) {
              var k = b.getAttribute("data-kind");
              var cntEl = b.querySelector(".js-rcnt");
              if (cntEl) cntEl.textContent = String(d.counts[k] || 0);
            });
            btn.classList.toggle("is-active", !!d.added);
          }
        }).catch(function () {});
      });
    });

    initReaderAuthForms(root);
    initTaxonomyAndBookFilters(root);
    initBookshelfAndReader(root);
  }

  var I18N_FALLBACKS = {
    "zh-CN": {
      loginErr: "登录失败，请检查账号或密码",
      regErr: "注册失败，请检查填写信息",
      unlockErr: "密码错误，请重试",
      reqErr: "请求失败，请稍后重试",
      netErr: "网络错误，请检查连接后重试",
      codeSent: "验证码已发送",
      noResults: "未找到相关内容",
      loadMore: "加载更多"
    },
    "zh-TW": {
      loginErr: "登入失敗，請檢查帳號或密碼",
      regErr: "註冊失敗，請檢查填寫資訊",
      unlockErr: "密碼錯誤，請重試",
      reqErr: "請求失敗，請稍後重試",
      netErr: "網路錯誤，請檢查連線後重試",
      codeSent: "驗證碼已發送",
      noResults: "找不到相關內容",
      loadMore: "載入更多"
    },
    ja: {
      loginErr: "ログインに失敗しました。認証情報をご確認ください",
      regErr: "登録に失敗しました。入力内容をご確認ください",
      unlockErr: "パスワードが正しくありません",
      reqErr: "リクエストに失敗しました。しばらくしてから再試行してください",
      netErr: "ネットワークエラーが発生しました",
      codeSent: "確認コードを送信しました",
      noResults: "該当する記事が見つかりません",
      loadMore: "もっと見る"
    },
    en: {
      loginErr: "Invalid credentials or sign-in failed",
      regErr: "Registration failed",
      unlockErr: "Incorrect password",
      reqErr: "Request failed",
      netErr: "Network error, please try again",
      codeSent: "Verification code sent",
      noResults: "No results",
      loadMore: "Load more"
    }
  };

  function i18nFallback(key) {
    var raw = String((document.documentElement && document.documentElement.getAttribute("lang")) || "").toLowerCase();
    var dict = I18N_FALLBACKS.en;
    if (raw.indexOf("zh") === 0) {
      dict = (raw.indexOf("tw") !== -1 || raw.indexOf("hk") !== -1 || raw.indexOf("mo") !== -1 || raw.indexOf("hant") !== -1)
        ? I18N_FALLBACKS["zh-TW"]
        : I18N_FALLBACKS["zh-CN"];
    } else if (raw.indexOf("ja") === 0) {
      dict = I18N_FALLBACKS.ja;
    }
    return (dict && dict[key]) || I18N_FALLBACKS.en[key] || "";
  }

  function setAuthMsg(el, txt, isErr) {
    if (!el) return;
    el.textContent = txt || "";
    el.classList.toggle("reader-auth-msg--error", !!isErr);
    el.classList.toggle("reader-auth-msg--info", !isErr && !!txt);
  }

  function resolveFormNextURL(fd) {
    var fromField = fd ? (fd.get("next") || "") : "";
    var fromQS = "";
    try { fromQS = new URLSearchParams(window.location.search).get("next") || ""; } catch (_) {}
    return sanitizeNextPath(fromField || fromQS);
  }

  function initReaderAuthForms(root) {
    var loginForm = root.querySelector("#volo-reader-login-form");
    if (loginForm) {
      loginForm.addEventListener("submit", function (ev) {
        ev.preventDefault();
        var fd = new FormData(loginForm);
        var nextURL = resolveFormNextURL(fd);
        var statusEl = loginForm.querySelector(".js-auth-msg");
        fetch("/api/v1/reader/login", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ ident: fd.get("ident"), password: fd.get("password"), next: nextURL || undefined })
        }).then(function (r) {
          return r.json().catch(function () { return {}; }).then(function (d) { return { ok: r.ok, d: d }; });
        }).then(function (res) {
          if (res.ok) {
            var target = sanitizeNextPath((res.d && (res.d.redirect || res.d.next)) || nextURL) || "/";
            window.location.href = window.location.origin + "/" + target.replace(/^[\/\\]+/, "");
          } else {
            setAuthMsg(statusEl, (res.d && res.d.error) || loginForm.getAttribute("data-err-login") || i18nFallback("loginErr"), true);
          }
        }).catch(function () {
          setAuthMsg(statusEl, loginForm.getAttribute("data-err-network") || i18nFallback("netErr"), true);
        });
      });
    }

    var sendBtn = root.querySelector("#volo-send-code-btn");
    var regForm = root.querySelector("#volo-reader-reg-form");
    if (sendBtn && regForm) {
      sendBtn.addEventListener("click", function () {
        var emailInput = regForm.querySelector("input[name='email']");
        var statusEl = regForm.querySelector(".js-auth-msg");
        if (!emailInput || !emailInput.value) return;
        fetch("/api/v1/reader/send-code", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ email: emailInput.value })
        }).then(function (r) {
          return r.json().catch(function () { return {}; }).then(function (d) { return { ok: r.ok, d: d }; });
        }).then(function (res) {
          if (res.ok) {
            setAuthMsg(statusEl, (res.d && res.d.message) || sendBtn.getAttribute("data-msg-sent") || i18nFallback("codeSent"), false);
          } else {
            setAuthMsg(statusEl, (res.d && res.d.error) || regForm.getAttribute("data-err-code") || i18nFallback("reqErr"), true);
          }
        }).catch(function () {
          setAuthMsg(statusEl, regForm.getAttribute("data-err-network") || i18nFallback("netErr"), true);
        });
      });
      regForm.addEventListener("submit", function (ev) {
        ev.preventDefault();
        var fd = new FormData(regForm);
        var nextURL = resolveFormNextURL(fd);
        var statusEl = regForm.querySelector(".js-auth-msg");
        fetch("/api/v1/reader/register", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({
            username: fd.get("username"),
            email: fd.get("email"),
            password: fd.get("password"),
            code: fd.get("code"),
            next: nextURL || undefined
          })
        }).then(function (r) {
          return r.json().catch(function () { return {}; }).then(function (d) { return { ok: r.ok, d: d }; });
        }).then(function (res) {
          if (res.ok) {
            var target = sanitizeNextPath((res.d && (res.d.redirect || res.d.next)) || nextURL) || "/";
            window.location.href = window.location.origin + "/" + target.replace(/^[\/\\]+/, "");
          } else {
            setAuthMsg(statusEl, (res.d && res.d.error) || regForm.getAttribute("data-err-register") || i18nFallback("regErr"), true);
          }
        }).catch(function () {
          setAuthMsg(statusEl, regForm.getAttribute("data-err-network") || i18nFallback("netErr"), true);
        });
      });
    }
  }

  function escapeHTML(s) {
    return String(s || "").replace(/[&<>"']/g, function (c) {
      return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c];
    });
  }

  function highlightTerms(text, query) {
    var safe = escapeHTML(text);
    var terms = String(query || "")
      .trim()
      .split(/\s+/)
      .filter(function (t) { return t.length >= 1; })
      .map(function (t) { return escapeHTML(t).replace(/[.*+?^${}()|[\]\\]/g, "\\$&"); });
    if (!terms.length) return safe;
    try {
      var rx = new RegExp("(" + terms.join("|") + ")", "gi");
      return safe.replace(rx, "<mark>$1</mark>");
    } catch (_) {
      return safe;
    }
  }

  /* Instant Search Modal (Ctrl+K, /, ArrowUp/Down/Enter, <mark>, Filter Pills, Load More) & Lightbox */
  function initSearchAndLightbox() {
    var modal = document.getElementById("volo-search-modal");
    var input = document.getElementById("volo-search-input");
    var list = document.getElementById("volo-search-results");
    var filterBar = document.getElementById("volo-search-filters");
    var lightbox = document.getElementById("volo-lightbox");
    var lightboxImg = document.getElementById("volo-lightbox-img");
    var lightboxCap = document.getElementById("volo-lightbox-caption");
    var activeIdx = -1;
    var curQuery = "";
    var curPage = 1;
    var seenUrls = {};
    var searchSeq = 0;
    var searchLoading = false;

    if (filterBar && filterBar.querySelector(".volo-filter-pill")) {
      filterBar.hidden = false;
    }

    function openSearch() {
      if (!modal || !input) return;
      closePopovers();
      openAnimated(modal, null);
      setTimeout(function () { input.focus(); input.select(); }, 20);
    }

    function closeSearch() {
      if (modal) closeAnimated(modal, null, 180);
    }

    function openLightbox(src, altText) {
      if (!lightbox || !lightboxImg) return;
      closePopovers();
      lightboxImg.src = src;
      lightboxImg.alt = altText || "";
      if (lightboxCap) {
        var cleanAlt = (altText || "").trim();
        lightboxCap.textContent = cleanAlt;
        lightboxCap.hidden = !cleanAlt;
      }
      openAnimated(lightbox, null);
    }

    function closeLightbox() {
      if (lightbox && !lightbox.hidden) {
        closeAnimated(lightbox, null, 180);
      }
    }

    function closeMobileTOC() {
      closeAnimated(document.getElementById("volo-toc-drawer"), document.getElementById("volo-mobile-toc-btn"), 180);
      document.querySelectorAll(".js-mobile-toc-trigger").forEach(function (b) {
        b.setAttribute("aria-expanded", "false");
      });
    }

    function syncActiveHit() {
      if (!list) return;
      var items = list.querySelectorAll(".volo-search-hit");
      items.forEach(function (el, idx) {
        var on = idx === activeIdx;
        el.classList.toggle("is-active", on);
        if (on) el.scrollIntoView({ block: "nearest" });
      });
    }

    function renderHitMarkup(h, q, isActive) {
      return '<a class="volo-search-hit' + (isActive ? " is-active" : "") + '" role="option" href="' + encodeURI(h.url) + '">' +
        "<strong>" + highlightTerms(h.title || "", q) + "</strong>" +
        (h.snippet ? '<div class="volo-search-hit__snip">' + highlightTerms(h.snippet, q) + "</div>" : "") +
        "</a>";
    }

    function runQuery(q, append) {
      if (!modal || !list) return;
      var mySeq = ++searchSeq;
      if (!q) {
        curQuery = "";
        curPage = 1;
        seenUrls = {};
        activeIdx = -1;
        searchLoading = false;
        list.innerHTML = "";
        return;
      }
      var nextPage = append ? (curPage + 1) : 1;
      var curOffset = append ? list.querySelectorAll(".volo-search-hit").length : 0;
      if (!append) {
        curQuery = q;
        curPage = 1;
        seenUrls = {};
        activeIdx = -1;
      }
      searchLoading = true;
      var moreBtn = append ? list.querySelector(".js-search-load-more") : null;
      if (moreBtn) moreBtn.disabled = true;

      var url = "/api/v1/search?q=" + encodeURIComponent(q) + (append ? ("&page=" + nextPage + "&offset=" + curOffset) : "");
      fetch(url)
        .then(function (r) { return r.json(); })
        .then(function (d) {
          if (mySeq !== searchSeq) return;
          searchLoading = false;
          var rawHits = (d && d.hits) || [];
          var hits = [];
          rawHits.forEach(function (h) {
            var uKey = (h && h.url) || "";
            if (!uKey || !seenUrls[uKey]) {
              if (uKey) seenUrls[uKey] = true;
              hits.push(h);
            }
          });
          var prevMore = list.querySelector(".volo-search-more-wrap");
          if (prevMore) prevMore.remove();

          if (!append && !hits.length) {
            list.innerHTML = '<div class="volo-search-empty">' + escapeHTML(modal.getAttribute("data-empty") || i18nFallback("noResults")) + "</div>";
            return;
          }
          if (append) {
            curPage = nextPage;
            if (hits.length) {
              list.insertAdjacentHTML("beforeend", hits.map(function (h) {
                return renderHitMarkup(h, q, false);
              }).join(""));
            }
          } else {
            list.innerHTML = hits.map(function (h, i) {
              return renderHitMarkup(h, q, i === 0);
            }).join("");
            activeIdx = 0;
          }
          if (d && d.has_more && hits.length > 0) {
            var loadMoreLabel = modal.getAttribute("data-load-more") || i18nFallback("loadMore");
            list.insertAdjacentHTML(
              "beforeend",
              '<div class="volo-search-more-wrap"><button type="button" class="volo-search-more js-search-load-more">' +
                escapeHTML(loadMoreLabel) +
              "</button></div>"
            );
          }
        })
        .catch(function () {
          if (mySeq === searchSeq) searchLoading = false;
          if (moreBtn) moreBtn.disabled = false;
        });
    }

    document.addEventListener("click", function (ev) {
      if (ev.target.closest("#volo-search-btn, [data-open-search]")) {
        openSearch();
        return;
      }
      if (ev.target.closest("[data-search-close]") || (modal && ev.target === modal)) {
        closeSearch();
        return;
      }
      var moreHitBtn = ev.target.closest(".js-search-load-more");
      if (moreHitBtn && modal && !modal.hidden) {
        ev.preventDefault();
        if (curQuery && !searchLoading) runQuery(curQuery, true);
        return;
      }
      var pill = ev.target.closest(".volo-filter-pill");
      if (pill && input) {
        var term = pill.getAttribute("data-seed-cat") || pill.getAttribute("data-seed-tag") || "";
        var already = pill.classList.contains("is-active");
        document.querySelectorAll(".volo-filter-pill.is-active").forEach(function (p) { p.classList.remove("is-active"); });
        if (already) {
          input.value = "";
          runQuery("");
        } else {
          pill.classList.add("is-active");
          input.value = term;
          runQuery(term);
        }
        input.focus();
        return;
      }

      var tocTrigger = ev.target.closest("#volo-mobile-toc-btn, .js-mobile-toc-trigger");
      if (tocTrigger) {
        var drawer = document.getElementById("volo-toc-drawer");
        var mBtn = document.getElementById("volo-mobile-toc-btn");
        if (drawer) {
          if (drawer.hidden || drawer.classList.contains("is-closing")) {
            closePopovers();
            openAnimated(drawer, mBtn || tocTrigger);
            document.querySelectorAll(".js-mobile-toc-trigger").forEach(function (b) {
              b.setAttribute("aria-expanded", "true");
            });
            centerActiveLinkInContainer(
              drawer.querySelector(".volo-toc-nav, .volo-toc__list"),
              drawer.querySelector(".volo-toc-link.is-active")
            );
          } else {
            closeMobileTOC();
          }
        }
        return;
      }
      var drawerEl = document.getElementById("volo-toc-drawer");
      if (
        ev.target.closest("[data-toc-close]") ||
        (drawerEl && !drawerEl.hidden && !ev.target.closest(".volo-toc-drawer__panel") && !ev.target.closest("#volo-mobile-toc-btn, .js-mobile-toc-trigger"))
      ) {
        closeMobileTOC();
        return;
      }

      if (
        lightbox &&
        !lightbox.hidden &&
        (ev.target === lightbox ||
          ev.target === lightboxImg ||
          ev.target.closest("[data-lightbox-close]") ||
          (ev.target.classList && ev.target.classList.contains("volo-lightbox__figure")))
      ) {
        closeLightbox();
        return;
      }
      var proseImg = ev.target.closest(".volo-prose img");
      if (proseImg && !proseImg.closest("a, .volo-embed-book, .volo-book-tile, .vc-item") && lightbox && lightboxImg) {
        var src = proseImg.getAttribute("src");
        if (src) {
          openLightbox(src, proseImg.getAttribute("alt") || "");
        }
      }
    });

    document.addEventListener("keydown", function (ev) {
      var tag = (ev.target && ev.target.tagName) || "";
      var editable = tag === "INPUT" || tag === "TEXTAREA" || tag === "SELECT" || (ev.target && ev.target.isContentEditable);

      if ((ev.metaKey || ev.ctrlKey) && ev.key.toLowerCase() === "k") {
        if (modal) {
          ev.preventDefault();
          if (modal.hidden || modal.classList.contains("is-closing")) openSearch();
          else closeSearch();
        }
        return;
      }
      if (ev.key === "/" && !editable && !ev.metaKey && !ev.ctrlKey && !ev.altKey) {
        if (modal && modal.hidden) {
          ev.preventDefault();
          openSearch();
          return;
        }
      }
      if (ev.key === "Escape") {
        closeSearch();
        closeLightbox();
        closePopovers();
        closeMobileTOC();
        closeMobileBookTOC();
        if (document.body.classList.contains("is-book-fullscreen")) {
          document.body.classList.remove("is-book-fullscreen");
          syncBookFullscreenBtn(document.querySelector(".js-book-fullscreen"), false);
        }
        syncBodyScrollLock();
        return;
      }

      if (modal && !modal.hidden && list) {
        var hits = list.querySelectorAll(".volo-search-hit");
        if (!hits.length) return;
        if (ev.key === "ArrowDown") {
          ev.preventDefault();
          activeIdx = (activeIdx + 1) % hits.length;
          syncActiveHit();
        } else if (ev.key === "ArrowUp") {
          ev.preventDefault();
          activeIdx = (activeIdx - 1 + hits.length) % hits.length;
          syncActiveHit();
        } else if (ev.key === "Enter" && document.activeElement === input) {
          var chosen = hits[activeIdx >= 0 ? activeIdx : 0];
          if (chosen) {
            ev.preventDefault();
            var href = chosen.getAttribute("href");
            closeSearch();
            if (href) navigatePJAX(href, true);
          }
        }
      }
    });

    if (modal && input && list) {
      var timer = null;
      input.addEventListener("input", function () {
        clearTimeout(timer);
        document.querySelectorAll(".volo-filter-pill.is-active").forEach(function (p) { p.classList.remove("is-active"); });
        var q = input.value.trim();
        if (!q) { runQuery(""); return; }
        timer = setTimeout(function () { runQuery(q, false); }, 150);
      });
    }
  }

  function initBackToTop() {
    var topBtn = document.getElementById("studioBackToTop");
    function onScroll() {
      if (topBtn) topBtn.hidden = window.scrollY < 360;
      syncReadingProgress();
    }
    window.addEventListener("scroll", onScroll, { passive: true });
    if (topBtn) {
      topBtn.addEventListener("click", function () {
        smoothScrollToY(0);
      });
    }
    onScroll();
  }

  function withPreviewParams(rawURL) {
    try {
      var curParams = new URLSearchParams(window.location.search);
      var skin = curParams.get("skin");
      var preview = curParams.get("theme_preview");
      if (!skin && !preview) return rawURL;
      var target = new URL(rawURL, window.location.origin);
      if (target.origin !== window.location.origin) return rawURL;
      if (skin && !target.searchParams.has("skin")) target.searchParams.set("skin", skin);
      if (preview && !target.searchParams.has("theme_preview")) target.searchParams.set("theme_preview", preview);
      return target.pathname + target.search + target.hash;
    } catch (_) {
      return rawURL;
    }
  }

  function hoistStylesheets(container, removeFromBody) {
    if (!container || !container.querySelectorAll || !document.head) return;
    Array.prototype.slice.call(container.querySelectorAll('link[rel="stylesheet"][href]')).forEach(function (lnk) {
      var h = lnk.getAttribute("href") || "";
      if (!h) return;
      var exists = Array.prototype.some.call(document.head.querySelectorAll('link[rel="stylesheet"]'), function (cur) {
        return cur.getAttribute("href") === h;
      });
      if (!exists) {
        var clone = document.createElement("link");
        clone.rel = "stylesheet";
        clone.href = h;
        var media = lnk.getAttribute("media");
        if (media) clone.media = media;
        var integrity = lnk.getAttribute("integrity");
        if (integrity) clone.setAttribute("integrity", integrity);
        var crossOrigin = lnk.getAttribute("crossorigin");
        if (crossOrigin !== null) clone.setAttribute("crossorigin", crossOrigin);
        document.head.appendChild(clone);
      }
      if (removeFromBody && lnk.parentNode) {
        lnk.remove();
      }
    });
  }

  function activateContainerScripts(container, skipProseAndComments) {
    if (!container || !container.querySelectorAll) return;
    Array.prototype.slice.call(container.querySelectorAll("script")).forEach(function (oldScript) {
      if (!oldScript.parentNode) return;
      var sType = (oldScript.getAttribute("type") || "").trim().toLowerCase();
      if (sType && sType !== "text/javascript" && sType !== "application/javascript" && sType !== "module") {
        return;
      }
      if (skipProseAndComments && oldScript.closest(".volo-prose, #volo-comments") && !oldScript.closest(".volo-plugin-slot")) {
        return;
      }
      var src = oldScript.getAttribute("src") || "";
      if (src.indexOf("/assets/volo-studio.js") !== -1 || src.indexOf("/assets/comment-widget.js") !== -1) {
        return;
      }
      var fresh = document.createElement("script");
      Array.prototype.forEach.call(oldScript.attributes, function (attr) {
        fresh.setAttribute(attr.name, attr.value);
      });
      if (!src) {
        fresh.textContent = oldScript.textContent || "";
      }
      oldScript.parentNode.replaceChild(fresh, oldScript);
    });
  }

  function syncDocumentHead(doc) {
    if (!doc || !doc.head || !document.head) return;
    document.title = doc.title || document.title;
    var nextRoot = doc.documentElement;
    if (nextRoot) {
      var nextLang = nextRoot.getAttribute("lang");
      if (nextLang) document.documentElement.setAttribute("lang", nextLang);
      var nextDir = nextRoot.getAttribute("dir");
      if (nextDir) {
        document.documentElement.setAttribute("dir", nextDir);
        document.documentElement.dir = nextDir;
      } else {
        document.documentElement.removeAttribute("dir");
        document.documentElement.dir = "";
      }
      ["data-volo-skin", "data-needs-math", "data-needs-mermaid"].forEach(function (attrName) {
        var v = nextRoot.getAttribute(attrName);
        if (v) document.documentElement.setAttribute(attrName, v);
        else document.documentElement.removeAttribute(attrName);
      });
    }

    var nextThemeLinks = Array.prototype.slice.call(doc.head.querySelectorAll('link[rel="stylesheet"][href*="/themes/"]'));
    var nextHrefs = nextThemeLinks.map(function (l) { return l.getAttribute("href") || ""; });
    Array.prototype.slice.call(document.head.querySelectorAll('link[rel="stylesheet"][href*="/themes/"]')).forEach(function (curLink) {
      var h = curLink.getAttribute("href") || "";
      if (nextHrefs.indexOf(h) === -1) curLink.remove();
    });
    hoistStylesheets(doc.head, false);

    var nextCanon = doc.head.querySelector('link[rel="canonical"]');
    var curCanon = document.head.querySelector('link[rel="canonical"]');
    if (nextCanon) {
      if (!curCanon) {
        curCanon = document.createElement("link");
        curCanon.rel = "canonical";
        document.head.appendChild(curCanon);
      }
      curCanon.href = nextCanon.getAttribute("href") || "";
    } else if (curCanon) {
      curCanon.remove();
    }

    ["description", "robots", "csrf-token"].forEach(function (name) {
      var nextMeta = doc.head.querySelector('meta[name="' + name + '"]');
      var curMeta = document.head.querySelector('meta[name="' + name + '"]');
      if (nextMeta) {
        if (!curMeta) {
          curMeta = document.createElement("meta");
          curMeta.name = name;
          document.head.appendChild(curMeta);
        }
        curMeta.content = nextMeta.getAttribute("content") || "";
      } else if (curMeta) {
        curMeta.remove();
      }
    });

    var socialAndAltSel = 'meta[property^="og:"], meta[name^="twitter:"], link[rel="alternate"][hreflang]';
    document.head.querySelectorAll(socialAndAltSel).forEach(function (el) { el.remove(); });
    doc.head.querySelectorAll(socialAndAltSel).forEach(function (el) {
      document.head.appendChild(el.cloneNode(true));
    });

    document.head.querySelectorAll('script[type="application/ld+json"]').forEach(function (s) { s.remove(); });
    doc.head.querySelectorAll('script[type="application/ld+json"]').forEach(function (s) {
      var clone = document.createElement("script");
      clone.type = "application/ld+json";
      clone.textContent = s.textContent || "";
      document.head.appendChild(clone);
    });
  }

  function syncHeaderAndPersistDock(doc, mainEl) {
    var curHW = document.querySelector(".studio-header-widgets");
    var nextHW = doc.querySelector(".studio-header-widgets");
    if (nextHW && curHW) {
      curHW.innerHTML = nextHW.innerHTML;
      hoistStylesheets(curHW, true);
      activateContainerScripts(curHW, false);
    } else if (nextHW && !curHW && mainEl && mainEl.parentNode) {
      var insertedHW = nextHW.cloneNode(true);
      mainEl.parentNode.insertBefore(insertedHW, mainEl);
      hoistStylesheets(insertedHW, true);
      activateContainerScripts(insertedHW, false);
    } else if (!nextHW && curHW) {
      curHW.remove();
    }

    var curDock = document.getElementById("volo-persist-dock");
    var nextDock = doc.getElementById("volo-persist-dock");
    if (!curDock || !nextDock) return;

    Array.prototype.slice.call(curDock.children).forEach(function (ch) {
      if (ch.hasAttribute("data-volo-persist") || ch.querySelector("[data-volo-persist]") || ch.id === "volo-podcast-mini") return;
      ch.remove();
    });
    Array.prototype.slice.call(nextDock.children).forEach(function (ch) {
      var pKey = ch.getAttribute("data-volo-persist");
      if (pKey && curDock.querySelector('[data-volo-persist="' + pKey + '"]')) return;
      var innerPersist = !pKey && ch.querySelector ? ch.querySelector("[data-volo-persist]") : null;
      if (innerPersist) {
        var innerKey = innerPersist.getAttribute("data-volo-persist");
        if (innerKey && curDock.querySelector('[data-volo-persist="' + innerKey + '"]')) return;
      }
      if (ch.id && document.getElementById(ch.id)) return;
      var clonedDock = ch.cloneNode(true);
      curDock.appendChild(clonedDock);
      hoistStylesheets(clonedDock, true);
      activateContainerScripts(clonedDock, false);
    });
    syncPodcastOpenState();
  }

  function navigatePJAX(rawURL, push) {
    var targetUrl = withPreviewParams(rawURL);
    var main = document.getElementById("studio-main");
    if (!main || document.documentElement.getAttribute("data-pjax") === "false") {
      window.location.href = targetUrl;
      return;
    }
    if (activeFetchCtrl) activeFetchCtrl.abort();
    activeFetchCtrl = new AbortController();
    closePopovers();
    closeAnimated(document.getElementById("volo-search-modal"), null, 180);
    closeAnimated(document.getElementById("volo-lightbox"), null, 180);
    closeAnimated(document.getElementById("volo-toc-drawer"), document.getElementById("volo-mobile-toc-btn"), 180);
    document.querySelectorAll(".js-mobile-toc-trigger").forEach(function (b) {
      b.setAttribute("aria-expanded", "false");
    });
    closeMobileBookTOC();
    startPJAXBar();
    window.dispatchEvent(new CustomEvent("volo:before-navigate", { detail: { url: targetUrl } }));

    var finalURL = targetUrl;
    fetch(targetUrl, { signal: activeFetchCtrl.signal, headers: { "X-Volo-PJAX": "1" } })
      .then(function (res) {
        var resolvedRaw = res.url || targetUrl;
        if (!res.ok || (res.headers.get("content-type") || "").indexOf("text/html") === -1) {
          window.location.href = resolvedRaw;
          return null;
        }
        try {
          var reqParsed = new URL(targetUrl, window.location.origin);
          var resParsed = new URL(resolvedRaw, window.location.origin);
          if (resParsed.origin !== window.location.origin) {
            window.location.href = resolvedRaw;
            return null;
          }
          finalURL = resParsed.pathname + resParsed.search + (resParsed.hash || reqParsed.hash || "");
        } catch (_) {
          finalURL = resolvedRaw;
        }
        return res.text();
      })
      .then(function (html) {
        if (!html) return;
        var doc = new DOMParser().parseFromString(html, "text/html");
        var nextMain = doc.getElementById("studio-main");
        if (!nextMain) { window.location.href = finalURL; return; }

        var nextAccent = doc.documentElement.getAttribute("data-accent");
        if (nextAccent) document.documentElement.setAttribute("data-accent", nextAccent);

        dockPlayingPodcastBeforePJAX(main);
        syncDocumentHead(doc);
        syncHeaderAndPersistDock(doc, main);

        var curBrandCopy = document.querySelector(".studio-brand-copy, .studio-brand__copy");
        var nextBrandCopy = doc.querySelector(".studio-brand-copy, .studio-brand__copy");
        if (curBrandCopy && nextBrandCopy && curBrandCopy.innerHTML !== nextBrandCopy.innerHTML) {
          curBrandCopy.innerHTML = nextBrandCopy.innerHTML;
        }
        var curFooter = document.querySelector(".studio-footer");
        var nextFooter = doc.querySelector(".studio-footer");
        if (curFooter && nextFooter && curFooter.innerHTML !== nextFooter.innerHTML) {
          curFooter.innerHTML = nextFooter.innerHTML;
          hoistStylesheets(curFooter, true);
          activateContainerScripts(curFooter, false);
        }

        main.innerHTML = nextMain.innerHTML;
        hoistStylesheets(main, true);
        activateContainerScripts(main, true);
        main.classList.remove("is-transitioning");
        if (!prefersReducedMotion()) {
          void main.offsetWidth;
          main.classList.add("is-transitioning");
        }

        var parsed = new URL(finalURL, window.location.origin);
        if (push) history.pushState({ volo: true }, "", finalURL);
        else history.replaceState({ volo: true }, "", finalURL);
        lastPathnameAndSearch = parsed.pathname + parsed.search;

        syncHeaderLoginNextLinks();
        syncHeaderHeight();
        syncActiveNav(parsed.pathname);
        hydratePage(main);

        if (parsed.hash) {
          var hID = parsed.hash.slice(1);
          if (!smoothScrollToHash(hID, true)) {
            if (hID.indexOf("comment-") === 0 || hID === "comments") {
              if (!smoothScrollToHash("volo-comments", true)) window.scrollTo(0, 0);
            } else {
              window.scrollTo(0, 0);
            }
          }
        } else {
          cancelSmoothScroll();
          window.scrollTo(0, 0);
        }

        finishPJAXBar();
        syncBodyScrollLock();
        var navDetail = { url: finalURL };
        window.dispatchEvent(new CustomEvent("volo:after-navigate", { detail: navDetail }));
        window.dispatchEvent(new CustomEvent("volo:pjax:after", { detail: navDetail }));
        document.dispatchEvent(new CustomEvent("volo:pjax:after", { detail: navDetail }));
      })
      .catch(function (err) {
        finishPJAXBar();
        if (err && err.name !== "AbortError") window.location.href = targetUrl;
      });
  }

  function syncActiveNav(pathname) {
    var cleanPath = (pathname || "/").replace(/\/+$/, "") || "/";
    document.querySelectorAll(".studio-nav__link, .studio-nav-more__item").forEach(function (a) {
      var href = a.getAttribute("href") || "";
      if (!href) return;
      var cleanHref = href.split("?")[0].replace(/\/+$/, "") || "/";
      var active = cleanPath === cleanHref || (cleanHref !== "/" && cleanPath.indexOf(cleanHref + "/") === 0);
      a.classList.toggle("is-active", active);
    });
    var moreBtn = document.getElementById("volo-nav-more-btn");
    var moreMenu = document.getElementById("volo-nav-more-menu");
    if (moreBtn) {
      var sel = window.innerWidth <= 760
        ? ".studio-nav-more__item.is-active"
        : ".studio-nav-more__item.is-active:not(.studio-only-mobile)";
      moreBtn.classList.toggle("is-active", Boolean(moreMenu && moreMenu.querySelector(sel)));
    }
  }

  function hydratePage(scope) {
    checkBrokenImages(scope);
    enhanceProse(scope);
    updateGitHubStatsTheme(scope);
    initCodeCopy(scope);
    initEnhancements(scope);
    initShareBar(scope);
    inspectGitHubDescriptions(scope);
    initArticleTOC(scope);
    initSeriesBoxScroll(scope);
    initPodcastAndUnlock(scope);
    syncPodcastOpenState();
    syncReadingProgress();
    if (window.VoloCommentWidget && typeof window.VoloCommentWidget.mount === "function") {
      window.VoloCommentWidget.mount();
    }
  }

  document.addEventListener("submit", function (ev) {
    var form = ev.target;
    if (!form || !form.matches) return;
    if (form.matches("form.js-reader-logout-form, form[action='/api/v1/reader/logout']")) {
      ev.preventDefault();
      var csrfField = form.querySelector("input[name='csrf_token']");
      var csrfVal = getCSRFToken() || (csrfField && csrfField.value) || "";
      var logoutHeaders = { "Accept": "application/json" };
      if (csrfVal) logoutHeaders["X-CSRF-Token"] = csrfVal;
      fetch("/api/v1/reader/logout", {
        method: "POST",
        credentials: "same-origin",
        headers: logoutHeaders
      }).then(function (r) {
        return r.json().catch(function () { return {}; }).then(function (d) { return { ok: r.ok, d: d }; });
      }).then(function (res) {
        var nextRedir = sanitizeNextPath((res.d && (res.d.redirect || res.d.next)) || "");
        if (nextRedir) {
          window.location.href = window.location.origin + "/" + nextRedir.replace(/^[\/\\]+/, "");
        } else {
          window.location.reload();
        }
      }).catch(function () {
        window.location.reload();
      });
      return;
    }
    if (!form.matches("form.studio-tax-search")) return;
    if ((form.getAttribute("method") || "get").toLowerCase() !== "get") return;
    if (document.documentElement.getAttribute("data-pjax") === "false") return;
    ev.preventDefault();
    var act = form.getAttribute("action") || window.location.pathname;
    var u = new URL(act, window.location.origin);
    var fd = new FormData(form);
    fd.forEach(function (val, key) {
      var s = String(val || "").trim();
      if (s) u.searchParams.set(key, s);
    });
    navigatePJAX(u.pathname + u.search, true);
  });

  document.addEventListener("click", function (ev) {
    var themeMenu = document.getElementById("volo-theme-menu");
    var themeBtn = ev.target.closest("#volo-theme-btn");
    var langMenu = document.getElementById("volo-lang-menu");
    var langBtn = ev.target.closest("#volo-lang-btn");
    var moreMenu = document.getElementById("volo-nav-more-menu");
    var moreBtn = ev.target.closest("#volo-nav-more-btn");

    if (themeBtn) {
      closeAnimated(langMenu, document.getElementById("volo-lang-btn"), 180);
      closeAnimated(moreMenu, document.getElementById("volo-nav-more-btn"), 180);
      if (themeMenu) {
        if (themeMenu.hidden || themeMenu.classList.contains("is-closing")) openAnimated(themeMenu, themeBtn);
        else closeAnimated(themeMenu, themeBtn, 180);
      } else {
        var cur = document.documentElement.getAttribute("data-mode") || "auto";
        var next = MODES[(MODES.indexOf(cur) + 1) % MODES.length];
        try { localStorage.setItem("volo_theme_mode", next); } catch (_) {}
        applyTheme(next);
      }
      return;
    }

    if (langBtn && langMenu) {
      closeAnimated(themeMenu, document.getElementById("volo-theme-btn"), 180);
      closeAnimated(moreMenu, document.getElementById("volo-nav-more-btn"), 180);
      if (langMenu.hidden || langMenu.classList.contains("is-closing")) openAnimated(langMenu, langBtn);
      else closeAnimated(langMenu, langBtn, 180);
      return;
    }

    if (moreBtn && moreMenu) {
      closeAnimated(themeMenu, document.getElementById("volo-theme-btn"), 180);
      closeAnimated(langMenu, document.getElementById("volo-lang-btn"), 180);
      if (moreMenu.hidden || moreMenu.classList.contains("is-closing")) openAnimated(moreMenu, moreBtn);
      else closeAnimated(moreMenu, moreBtn, 180);
      return;
    }

    var modeChoice = ev.target.closest("[data-volo-mode]");
    if (modeChoice) {
      var m = modeChoice.getAttribute("data-volo-mode") || "auto";
      try { localStorage.setItem("volo_theme_mode", m); } catch (_) {}
      applyTheme(m);
      closeAnimated(themeMenu, document.getElementById("volo-theme-btn"), 180);
      return;
    }

    if (themeMenu && !themeMenu.hidden && !ev.target.closest(".volo-appearance")) {
      closeAnimated(themeMenu, document.getElementById("volo-theme-btn"), 180);
    }
    if (langMenu && !langMenu.hidden && !ev.target.closest(".volo-lang-pop")) {
      closeAnimated(langMenu, document.getElementById("volo-lang-btn"), 180);
    }
    if (moreMenu && !moreMenu.hidden && !ev.target.closest(".studio-nav-more")) {
      closeAnimated(moreMenu, document.getElementById("volo-nav-more-btn"), 180);
    }

    var langSel = ev.target.closest("[data-set-lang]");
    if (langSel) {
      var code = langSel.getAttribute("data-set-lang");
      document.cookie = "volo_lang=" + encodeURIComponent(code) + "; path=/; max-age=31536000; SameSite=Lax";
      window.location.reload();
      return;
    }

    var claimGuestBtn = ev.target.closest("[data-claim-guest-comments]");
    if (claimGuestBtn && !claimGuestBtn.closest("#volo-comments")) {
      ev.preventDefault();
      if (claimGuestBtn.disabled) return;
      claimGuestBtn.disabled = true;
      var claimHeaders = { "Content-Type": "application/json", "X-Volo-Env-FP": collectEnvFP() };
      var claimCSRF = getCSRFToken();
      if (claimCSRF) claimHeaders["X-CSRF-Token"] = claimCSRF;
      fetch("/api/v1/reader/claim-guest-comments", {
        method: "POST",
        credentials: "same-origin",
        headers: claimHeaders,
        body: "{}"
      }).then(function (r) {
        if (r.ok) window.location.reload();
        else claimGuestBtn.disabled = false;
      }).catch(function () {
        claimGuestBtn.disabled = false;
      });
      return;
    }

    var tagsToggle = ev.target.closest(".js-foot-tags-toggle");
    if (tagsToggle) {
      var tagBox = tagsToggle.closest(".article-foot-tags, .studio-article__foot-tags");
      if (tagBox) {
        var willExpand = tagsToggle.getAttribute("aria-expanded") !== "true";
        tagBox.querySelectorAll("[data-extra-tag]").forEach(function (el) {
          el.hidden = !willExpand;
        });
        tagsToggle.setAttribute("aria-expanded", willExpand ? "true" : "false");
        tagsToggle.textContent = willExpand
          ? (tagsToggle.getAttribute("data-less-label") || "−")
          : (tagsToggle.getAttribute("data-more-label") || "+");
      }
      return;
    }

    var pillsToggle = ev.target.closest(".js-jump-pills-toggle");
    if (pillsToggle) {
      var pillBox = pillsToggle.closest(".studio-jump-pills");
      if (pillBox) {
        var expandPills = pillsToggle.getAttribute("aria-expanded") !== "true";
        pillBox.querySelectorAll("[data-extra-pill]").forEach(function (el) {
          el.hidden = !expandPills;
        });
        pillsToggle.setAttribute("aria-expanded", expandPills ? "true" : "false");
        pillsToggle.textContent = expandPills
          ? (pillsToggle.getAttribute("data-less-label") || "−")
          : (pillsToggle.getAttribute("data-more-label") || "+");
      }
      return;
    }

    var shareBtn = ev.target.closest("[data-share-act]");
    if (shareBtn) {
      var act = shareBtn.getAttribute("data-share-act") || "";
      var rawShareURL = shareBtn.getAttribute("data-share-url") || window.location.href;
      var shareURL = rawShareURL.charAt(0) === "/" ? window.location.origin + rawShareURL : rawShareURL;
      var shareTitle = shareBtn.getAttribute("data-share-title") || document.title || "";
      var flashDone = function () {
        var lbl = shareBtn.querySelector(".js-share-label");
        var doneTxt = shareBtn.getAttribute("data-label-done");
        var defTxt = shareBtn.getAttribute("data-label-default");
        shareBtn.classList.add("is-copied");
        if (lbl && doneTxt) lbl.textContent = doneTxt;
        clearTimeout(shareBtn._shareTimer);
        shareBtn._shareTimer = setTimeout(function () {
          shareBtn.classList.remove("is-copied");
          if (lbl && defTxt) lbl.textContent = defTxt;
        }, 1600);
      };
      if (act === "copy-link") {
        copyTextToClipboard(shareURL).then(flashDone).catch(function () {});
      } else if (act === "copy-md") {
        var safeTitle = shareTitle.replace(/\\/g, "\\\\").replace(/([\[\]])/g, "\\$1");
        copyTextToClipboard("[" + safeTitle + "](" + shareURL + ")").then(flashDone).catch(function () {});
      } else if (act === "native" && typeof navigator !== "undefined" && typeof navigator.share === "function") {
        navigator.share({ title: shareTitle, url: shareURL }).catch(function () {});
      }
      return;
    }

    var ghDesc = ev.target.closest(".volo-github-repo-desc.is-truncatable");
    if (ghDesc && !ev.target.closest("a")) {
      var expanded = ghDesc.classList.toggle("is-expanded");
      ghDesc.setAttribute("aria-expanded", expanded ? "true" : "false");
      return;
    }

    var link = ev.target.closest("a[href]");
    if (!link || ev.defaultPrevented || ev.metaKey || ev.ctrlKey || ev.shiftKey || ev.altKey) return;
    if (link.target && link.target !== "_self") return;
    if (link.hasAttribute("download") || link.hasAttribute("data-no-pjax")) return;

    var rawHref = link.getAttribute("href") || "";
    if (rawHref.charAt(0) === "#") {
      if (rawHref.length > 1) {
        ev.preventDefault();
        if (link.closest("#volo-toc-drawer")) {
          closeAnimated(document.getElementById("volo-toc-drawer"), document.getElementById("volo-mobile-toc-btn"), 180);
          document.querySelectorAll(".js-mobile-toc-trigger").forEach(function (b) { b.setAttribute("aria-expanded", "false"); });
        }
        smoothScrollToHash(rawHref.slice(1), false);
      }
      return;
    }

    var u;
    try { u = new URL(link.href, window.location.origin); } catch (_) { return; }
    if (u.origin !== window.location.origin) return;

    /* Same-page hash navigation: smooth-scroll directly, NEVER trigger PJAX */
    var normTargetPS = withPreviewParams(u.pathname + u.search);
    if (normTargetPS === window.location.pathname + window.location.search && u.hash) {
      ev.preventDefault();
      if (link.closest("#volo-toc-drawer")) {
        closeAnimated(document.getElementById("volo-toc-drawer"), document.getElementById("volo-mobile-toc-btn"), 180);
        document.querySelectorAll(".js-mobile-toc-trigger").forEach(function (b) { b.setAttribute("aria-expanded", "false"); });
      }
      smoothScrollToHash(u.hash.slice(1), false);
      return;
    }

    if (/^\/(api|uploads|themes|plugins|images|logout|admin-index\.do|admin\/index\.do|rss\.xml|atom\.xml|podcast\.xml|sitemap\.xml|llms)/.test(u.pathname)) return;

    ev.preventDefault();
    navigatePJAX(u.pathname + u.search + u.hash, true);
  });

  document.addEventListener("keydown", function (ev) {
    if (ev.key === "Enter" || ev.key === " ") {
      var ghDesc = ev.target && ev.target.closest && ev.target.closest(".volo-github-repo-desc.is-truncatable");
      if (ghDesc && ev.target === ghDesc) {
        ev.preventDefault();
        var expanded = ghDesc.classList.toggle("is-expanded");
        ghDesc.setAttribute("aria-expanded", expanded ? "true" : "false");
      }
    }
  });

  /* Never trigger PJAX reload when popstate only changes #hash on the same pathname+search */
  window.addEventListener("popstate", function () {
    var curPS = window.location.pathname + window.location.search;
    if (curPS === lastPathnameAndSearch) {
      if (window.location.hash) {
        smoothScrollToHash(window.location.hash.slice(1), true);
      } else {
        smoothScrollToY(0);
      }
      return;
    }
    lastPathnameAndSearch = curPS;
    navigatePJAX(curPS + window.location.hash, false);
  });

  function reclampOpenPopovers() {
    document.querySelectorAll(".volo-popover:not([hidden])").forEach(function (el) {
      clampPopoverToViewport(el);
    });
  }

  var resizeTimer = null;
  window.addEventListener("resize", function () {
    syncHeaderHeight();
    reclampOpenPopovers();
    clearTimeout(resizeTimer);
    resizeTimer = setTimeout(function () {
      inspectGitHubDescriptions(document);
      syncActiveNav(window.location.pathname);
      syncBodyScrollLock();
    }, 180);
  }, { passive: true });

  window.addEventListener("orientationchange", function () {
    setTimeout(function () {
      syncHeaderHeight();
      reclampOpenPopovers();
      syncBodyScrollLock();
    }, 60);
  });

  applyTheme(null);
  window.addEventListener("storage", function (ev) {
    if (!ev || !ev.key || ev.key === "volo_theme_mode") applyTheme(null);
  });
  if (window.matchMedia) {
    var darkMql = window.matchMedia("(prefers-color-scheme: dark)");
    var onSystemChange = function () {
      if ((document.documentElement.getAttribute("data-mode") || "auto") === "system") applyTheme("system");
    };
    if (typeof darkMql.addEventListener === "function") {
      darkMql.addEventListener("change", onSystemChange);
    } else if (typeof darkMql.addListener === "function") {
      darkMql.addListener(onSystemChange);
    }
  }
  setInterval(function () {
    if ((document.documentElement.getAttribute("data-mode") || "auto") === "auto") {
      applyTheme("auto");
    }
  }, 60000);

  function initHeaderHeightObserver() {
    syncHeaderHeight();
    var header = document.querySelector(".studio-header");
    if (header && typeof ResizeObserver !== "undefined") {
      var ro = new ResizeObserver(function () {
        syncHeaderHeight();
      });
      ro.observe(header);
    }
  }

  function initPersistDockObserver() {
    syncPodcastOpenState();
    var dock = document.getElementById("volo-persist-dock");
    if (dock && typeof MutationObserver !== "undefined") {
      var mo = new MutationObserver(function () {
        syncPodcastOpenState();
      });
      mo.observe(dock, { childList: true, subtree: true, attributes: true, attributeFilter: ["hidden"] });
    }
  }

  function boot() {
    var mainEl = document.getElementById("studio-main");
    if (mainEl) {
      mainEl.addEventListener("animationend", function (ev) {
        if (ev.target === mainEl) mainEl.classList.remove("is-transitioning");
      });
    }
    initHeaderHeightObserver();
    initPersistDockObserver();
    syncActiveNav(window.location.pathname);
    checkBrokenImages(document);
    initSearchAndLightbox();
    initBackToTop();
    hydratePage(document);
    if (window.location.hash && window.location.hash.length > 1) {
      var initHash = window.location.hash.slice(1);
      if (!smoothScrollToHash(initHash, true) && initHash.indexOf("comment-") === 0) {
        smoothScrollToHash("volo-comments", true);
      }
    }
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", boot);
  } else {
    boot();
  }
})();
