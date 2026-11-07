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
  var lastPathnameAndSearch = window.location.pathname + window.location.search;

  function prefersReducedMotion() {
    return !!(window.matchMedia && window.matchMedia("(prefers-reduced-motion: reduce)").matches);
  }

  function resolveEffectiveTheme(mode) {
    if (mode === "light" || mode === "dark") return mode;
    if (mode === "system") {
      return window.matchMedia && window.matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light";
    }
    var h = new Date().getHours();
    return h >= 7 && h < 19 ? "light" : "dark";
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
      if (labelEl) {
        var shortAttr = toggleBtn.getAttribute("data-label-" + chosenMode);
        if (shortAttr) labelEl.textContent = shortAttr;
      }
    }
    document.querySelectorAll("[data-volo-mode]").forEach(function (el) {
      var selected = el.getAttribute("data-volo-mode") === chosenMode;
      el.classList.toggle("is-selected", selected);
      el.setAttribute("aria-checked", selected ? "true" : "false");
    });

    if (prevTheme && prevTheme !== nextTheme && window.mermaid) {
      var rendered = Array.prototype.slice.call(document.querySelectorAll("pre.mermaid[data-mermaid-src]"));
      if (rendered.length) renderMermaidNodes(rendered);
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

  function syncBodyScrollLock() {
    var locked =
      isOverlayActive(document.getElementById("volo-search-modal")) ||
      isOverlayActive(document.getElementById("volo-lightbox")) ||
      isOverlayActive(document.getElementById("volo-toc-drawer")) ||
      isOverlayActive(document.querySelector(".volo-comment-composer")) ||
      isOverlayActive(document.querySelector(".vc-ban-modal"));
    document.documentElement.style.overflow = locked ? "hidden" : "";
    if (document.body) document.body.style.overflow = locked ? "hidden" : "";
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

  /* Interruptible Cubic-Out Smooth Scroll Engine */
  function cancelSmoothScroll() {
    if (scrollAnimRAF) {
      cancelAnimationFrame(scrollAnimRAF);
      scrollAnimRAF = 0;
    }
  }

  window.addEventListener("wheel", cancelSmoothScroll, { passive: true });
  window.addEventListener("touchstart", cancelSmoothScroll, { passive: true });
  window.addEventListener("keydown", function (ev) {
    if (["ArrowUp", "ArrowDown", "PageUp", "PageDown", " ", "Home", "End"].indexOf(ev.key) !== -1) {
      cancelSmoothScroll();
    }
  });

  function getHeaderOffset() {
    var header = document.querySelector(".studio-header");
    return (header ? header.getBoundingClientRect().height : 72) + 20;
  }

  function smoothScrollToY(targetY) {
    cancelSmoothScroll();
    var maxScroll = Math.max(0, document.documentElement.scrollHeight - window.innerHeight);
    var destY = Math.min(maxScroll, Math.max(0, Math.round(targetY)));
    var startY = window.scrollY || window.pageYOffset || 0;
    var diff = destY - startY;

    if (prefersReducedMotion() || Math.abs(diff) < 4) {
      window.scrollTo(0, destY);
      return;
    }

    var duration = Math.min(560, Math.max(280, Math.round(Math.abs(diff) * 0.22)));
    var startTime = null;

    function step(ts) {
      if (startTime === null) startTime = ts;
      var elapsed = ts - startTime;
      var t = Math.min(1, elapsed / duration);
      var eased = 1 - Math.pow(1 - t, 3);
      window.scrollTo(0, Math.round(startY + diff * eased));
      if (t < 1) {
        scrollAnimRAF = requestAnimationFrame(step);
      } else {
        scrollAnimRAF = 0;
      }
    }
    scrollAnimRAF = requestAnimationFrame(step);
  }

  function findHeadingTarget(rawID) {
    if (!rawID) return null;
    var direct = document.getElementById(rawID);
    if (direct) return direct;
    try {
      var decoded = decodeURIComponent(rawID);
      return document.getElementById(decoded);
    } catch (_) {
      return null;
    }
  }

  function setActiveTOCLink(activeID) {
    if (!activeID) return;
    document.querySelectorAll(".volo-toc-link[data-toc-id]").forEach(function (a) {
      var match = a.getAttribute("data-toc-id") === activeID;
      a.classList.toggle("is-active", match);
      if (match) {
        var nav = a.closest(".volo-toc__list, .volo-toc-nav");
        if (nav && nav.scrollHeight > nav.clientHeight) {
          var itemTop = a.offsetTop;
          var itemBottom = itemTop + a.offsetHeight;
          var viewTop = nav.scrollTop;
          var viewBottom = viewTop + nav.clientHeight;
          if (itemTop < viewTop + 20 || itemBottom > viewBottom - 20) {
            nav.scrollTop = Math.max(0, itemTop - Math.round(nav.clientHeight * 0.35));
          }
        }
      }
    });
  }

  function smoothScrollToHash(rawID, skipHistory) {
    if (!rawID) {
      smoothScrollToY(0);
      return false;
    }
    var targetEl = findHeadingTarget(rawID);
    if (!targetEl) return false;
    var destY = (window.scrollY || 0) + targetEl.getBoundingClientRect().top - getHeaderOffset();
    if (!skipHistory) {
      try {
        history.replaceState(history.state, "", "#" + encodeURIComponent(targetEl.id));
      } catch (_) {}
    }
    setActiveTOCLink(targetEl.id);
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
      if (!lang && code && code.className) {
        var m = code.className.match(/language-([a-zA-Z0-9_+-]+)/);
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
        clone.querySelectorAll(".ln, .lnt").forEach(function (n) { n.remove(); });
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

  /* On-Demand KaTeX & Mermaid Lazy Loader (Zero External Requests When Unused) */
  function getCDNPrefix() {
    var raw = (document.documentElement.getAttribute("data-cdn-prefix") || "").trim().replace(/\/+$/, "");
    return raw || "https://cdn.jsdelivr.net/npm";
  }

  function loadStyleOnce(id, href) {
    if (document.getElementById(id)) return;
    var link = document.createElement("link");
    link.id = id;
    link.rel = "stylesheet";
    link.href = href;
    document.head.appendChild(link);
  }

  function loadScriptOnce(src) {
    return new Promise(function (resolve, reject) {
      var existing = document.querySelector('script[data-volo-lazy="' + src + '"]');
      if (existing) {
        if (existing.dataset.loaded === "1") return resolve();
        existing.addEventListener("load", function () { resolve(); }, { once: true });
        existing.addEventListener("error", function () { reject(new Error("script error")); }, { once: true });
        return;
      }
      var s = document.createElement("script");
      s.src = src;
      s.async = true;
      s.setAttribute("data-volo-lazy", src);
      s.addEventListener("load", function () {
        s.dataset.loaded = "1";
        resolve();
      }, { once: true });
      s.addEventListener("error", function () {
        s.remove();
        reject(new Error("script error"));
      }, { once: true });
      document.head.appendChild(s);
    });
  }

  function ensureKaTeX() {
    if (window.renderMathInElement) return Promise.resolve();
    if (!katexPromise) {
      var cdn = getCDNPrefix();
      loadStyleOnce("volo-katex-css", cdn + "/katex@0.16.11/dist/katex.min.css");
      katexPromise = loadScriptOnce(cdn + "/katex@0.16.11/dist/katex.min.js")
        .then(function () {
          return loadScriptOnce(cdn + "/katex@0.16.11/dist/contrib/auto-render.min.js");
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
      var cdn = getCDNPrefix();
      mermaidPromise = loadScriptOnce(cdn + "/mermaid@10/dist/mermaid.min.js")
        .then(function () { return window.mermaid; })
        .catch(function (err) {
          mermaidPromise = null;
          throw err;
        });
    }
    return mermaidPromise;
  }

  function renderMermaidNodes(nodes) {
    if (!window.mermaid || !nodes || !nodes.length) return;
    nodes.forEach(function (pre) {
      if (!pre.dataset.mermaidSrc) {
        pre.dataset.mermaidSrc = pre.textContent || "";
      } else if (pre.getAttribute("data-processed")) {
        pre.removeAttribute("data-processed");
        pre.textContent = pre.dataset.mermaidSrc;
      }
    });
    var isDark = document.documentElement.getAttribute("data-theme") === "dark";
    try {
      window.mermaid.initialize({
        startOnLoad: false,
        theme: isDark ? "dark" : "default",
        securityLevel: "strict"
      });
      var p = window.mermaid.run({ nodes: nodes });
      if (p && typeof p.catch === "function") p.catch(function () {});
    } catch (_) {}
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

    var mermaidNodes = Array.prototype.slice.call(root.querySelectorAll("pre.mermaid"));
    var hasMermaidFlag = mermaidNodes.length > 0 || !!(
      (root.querySelector && root.querySelector('[data-needs-mermaid="1"]')) ||
      (root.getAttribute && root.getAttribute("data-needs-mermaid") === "1")
    );
    if (hasMermaidFlag && mermaidNodes.length > 0) {
      ensureMermaid().then(function () {
        renderMermaidNodes(mermaidNodes);
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

    var root = scope || document;
    var tocSlot = root.querySelector("#volo-toc-slot") || document.getElementById("volo-toc-slot");
    var tocCard = root.querySelector("#volo-toc-card") || document.getElementById("volo-toc-card");
    var toggleBtn = root.querySelector("#volo-toc-toggle") || document.getElementById("volo-toc-toggle");
    var mobileBtn = document.getElementById("volo-mobile-toc-btn");
    var mobileDrawer = document.getElementById("volo-toc-drawer");

    if (mobileBtn) {
      mobileBtn.hidden = !mobileDrawer;
      mobileBtn.setAttribute("aria-expanded", "false");
    }

    if (tocCard && toggleBtn) {
      var collapsed = false;
      try { collapsed = sessionStorage.getItem("volo_toc_collapsed") === "1"; } catch (_) {}
      tocCard.classList.toggle("is-collapsed", collapsed);
      toggleBtn.setAttribute("aria-expanded", collapsed ? "false" : "true");
      var onToggle = function () {
        var next = !tocCard.classList.contains("is-collapsed");
        tocCard.classList.toggle("is-collapsed", next);
        toggleBtn.setAttribute("aria-expanded", next ? "false" : "true");
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
    function updateSpyAndDock() {
      spyPending = false;

      if (headings.length) {
        var threshold = getHeaderOffset() + 18;
        var activeID = headings[0].id;
        for (var i = 0; i < headings.length; i++) {
          if (headings[i].getBoundingClientRect().top <= threshold) {
            activeID = headings[i].id;
          } else {
            break;
          }
        }
        setActiveTOCLink(activeID);
      }

      if (tocCard && tocSlot) {
        if (window.innerWidth <= 900) {
          tocCard.classList.remove("is-docked");
          tocCard.style.left = "";
          tocCard.style.top = "";
          tocSlot.style.minHeight = "";
          return;
        }
        var rail = tocSlot.closest(".studio-side-rail");
        var sidebar = rail && rail.querySelector(".studio-sidebar");
        var slotRect = tocSlot.getBoundingClientRect();
        var triggerPassed = sidebar
          ? sidebar.getBoundingClientRect().bottom <= 78
          : slotRect.top <= 90;

        if (triggerPassed) {
          if (!tocCard.classList.contains("is-docked")) {
            tocSlot.style.minHeight = tocCard.offsetHeight + "px";
            tocCard.classList.add("is-docked");
          }
          tocCard.style.setProperty("--toc-slot-width", Math.round(slotRect.width) + "px");
          tocCard.style.left = Math.round(slotRect.left) + "px";
          var footer = document.querySelector(".studio-footer");
          if (footer) {
            var footerTop = footer.getBoundingClientRect().top;
            var cardH = tocCard.offsetHeight;
            var maxTop = footerTop - cardH - 24;
            tocCard.style.top = (maxTop < 90 ? Math.round(maxTop) : 90) + "px";
          } else {
            tocCard.style.top = "90px";
          }
        } else if (tocCard.classList.contains("is-docked")) {
          tocCard.classList.remove("is-docked");
          tocCard.style.left = "";
          tocCard.style.top = "";
          tocSlot.style.minHeight = "";
        }
      }
    }

    function requestSpy() {
      if (!spyPending) {
        spyPending = true;
        requestAnimationFrame(updateSpyAndDock);
      }
    }

    window.addEventListener("scroll", requestSpy, { passive: true });
    window.addEventListener("resize", requestSpy, { passive: true });
    tocCleanups.push(function () {
      window.removeEventListener("scroll", requestSpy);
      window.removeEventListener("resize", requestSpy);
      if (tocCard) {
        tocCard.classList.remove("is-docked");
        tocCard.style.left = "";
        tocCard.style.top = "";
      }
      if (tocSlot) tocSlot.style.minHeight = "";
    });
    updateSpyAndDock();
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
    if (slug && activeID && activeURL) {
      writeBookProgress(slug, { chapterId: activeID, chapterTitle: activeTitle, url: activeURL, ts: Date.now() });
    }

    reader.querySelectorAll("[data-chapter-id]").forEach(function (link) {
      link.addEventListener("click", function () {
        var cid = link.getAttribute("data-chapter-id") || "";
        var ctitle = link.getAttribute("data-chapter-title") || "";
        var href = link.getAttribute("href") || "";
        if (slug && cid && href && href.charAt(0) === "/") {
          writeBookProgress(slug, { chapterId: cid, chapterTitle: ctitle, url: href, ts: Date.now() });
        }
      });
    });

    if (activeID && window.innerWidth <= 900 && reader.dataset.mobileTocInit !== "1") {
      reader.dataset.mobileTocInit = "1";
      reader.classList.add("is-toc-collapsed");
    }

    var tocBtn = reader.querySelector(".js-book-toggle-toc");
    if (tocBtn) {
      var syncTocBtn = function () {
        var isCol = reader.classList.contains("is-toc-collapsed");
        tocBtn.setAttribute("aria-expanded", isCol ? "false" : "true");
        tocBtn.textContent = isCol
          ? (tocBtn.getAttribute("data-expand-label") || tocBtn.textContent)
          : (tocBtn.getAttribute("data-collapse-label") || tocBtn.textContent);
      };
      syncTocBtn();
      tocBtn.addEventListener("click", function () {
        reader.classList.toggle("is-toc-collapsed");
        syncTocBtn();
      });
    }

    var fsBtn = reader.querySelector(".js-book-fullscreen");
    if (fsBtn) {
      fsBtn.addEventListener("click", function () {
        var full = document.body.classList.toggle("is-book-fullscreen");
        fsBtn.textContent = full
          ? (fsBtn.getAttribute("data-exit-label") || fsBtn.textContent)
          : (fsBtn.getAttribute("data-enter-label") || fsBtn.textContent);
      });
    }
  }

  function initPodcastAndUnlock(scope) {
    var root = scope || document;
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
      form.addEventListener("submit", function (ev) {
        ev.preventDefault();
        var aid = form.getAttribute("data-article-id");
        var input = form.querySelector("input[type='password']") || form.querySelector("input[name='password']");
        var msg = form.querySelector(".js-unlock-msg");
        if (!aid || !input) return;
        fetch("/api/v1/articles/" + encodeURIComponent(aid) + "/unlock", {
          method: "POST",
          headers: { "Content-Type": "application/json", "X-Volo-Env-FP": collectEnvFP() },
          body: JSON.stringify({ password: input.value })
        }).then(function (r) {
          if (r.ok) {
            window.location.reload();
          } else if (msg) {
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
        fetch("/api/v1/reactions", {
          method: "POST",
          headers: { "Content-Type": "application/json", "X-Volo-Env-FP": collectEnvFP() },
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
    initBookshelfAndReader(root);
  }

  function initReaderAuthForms(root) {
    var loginForm = root.querySelector("#volo-reader-login-form");
    if (loginForm) {
      loginForm.addEventListener("submit", function (ev) {
        ev.preventDefault();
        var fd = new FormData(loginForm);
        var statusEl = loginForm.querySelector(".js-auth-msg");
        fetch("/api/v1/reader/login", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ ident: fd.get("ident"), password: fd.get("password") })
        }).then(function (r) {
          return r.json().then(function (d) { return { ok: r.ok, d: d }; });
        }).then(function (res) {
          if (res.ok) window.location.href = "/";
          else if (statusEl) statusEl.textContent = (res.d && res.d.error) || "Sign-in failed";
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
        }).then(function (r) { return r.json(); }).then(function (d) {
          if (statusEl) statusEl.textContent = d.message || d.error || "";
        });
      });
      regForm.addEventListener("submit", function (ev) {
        ev.preventDefault();
        var fd = new FormData(regForm);
        var statusEl = regForm.querySelector(".js-auth-msg");
        fetch("/api/v1/reader/register", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({
            username: fd.get("username"),
            email: fd.get("email"),
            password: fd.get("password"),
            code: fd.get("code")
          })
        }).then(function (r) {
          return r.json().then(function (d) { return { ok: r.ok, d: d }; });
        }).then(function (res) {
          if (res.ok) window.location.href = "/";
          else if (statusEl) statusEl.textContent = (res.d && res.d.error) || "Registration failed";
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

  /* Instant Search Modal (Ctrl+K, /, ArrowUp/Down/Enter, <mark>, Filter Pills) & Lightbox */
  function initSearchAndLightbox() {
    var modal = document.getElementById("volo-search-modal");
    var input = document.getElementById("volo-search-input");
    var list = document.getElementById("volo-search-results");
    var filterBar = document.getElementById("volo-search-filters");
    var lightbox = document.getElementById("volo-lightbox");
    var lightboxImg = document.getElementById("volo-lightbox-img");
    var lightboxCap = document.getElementById("volo-lightbox-caption");
    var activeIdx = -1;

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

    function runQuery(q) {
      if (!modal || !list) return;
      activeIdx = -1;
      if (!q) {
        list.innerHTML = "";
        return;
      }
      fetch("/api/v1/search?q=" + encodeURIComponent(q))
        .then(function (r) { return r.json(); })
        .then(function (d) {
          var hits = (d && d.hits) || [];
          if (!hits.length) {
            list.innerHTML = '<div class="volo-search-empty">' + escapeHTML(modal.getAttribute("data-empty") || "No results") + "</div>";
            return;
          }
          list.innerHTML = hits.map(function (h, i) {
            return '<a class="volo-search-hit' + (i === 0 ? " is-active" : "") + '" role="option" href="' + encodeURI(h.url) + '">' +
              "<strong>" + highlightTerms(h.title || "", q) + "</strong>" +
              (h.snippet ? '<div class="volo-search-hit__snip">' + highlightTerms(h.snippet, q) + "</div>" : "") +
              "</a>";
          }).join("");
          activeIdx = 0;
        })
        .catch(function () {});
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

      if (ev.target.closest("#volo-mobile-toc-btn")) {
        var drawer = document.getElementById("volo-toc-drawer");
        var mBtn = document.getElementById("volo-mobile-toc-btn");
        if (drawer) {
          if (drawer.hidden || drawer.classList.contains("is-closing")) {
            closePopovers();
            openAnimated(drawer, mBtn);
          } else {
            closeMobileTOC();
          }
        }
        return;
      }
      var drawerEl = document.getElementById("volo-toc-drawer");
      if (ev.target.closest("[data-toc-close]") || (drawerEl && ev.target === drawerEl)) {
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
        if (document.body.classList.contains("is-book-fullscreen")) {
          document.body.classList.remove("is-book-fullscreen");
          var fsBtn = document.querySelector(".js-book-fullscreen");
          if (fsBtn) fsBtn.textContent = fsBtn.getAttribute("data-enter-label") || fsBtn.textContent;
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
        if (!q) { list.innerHTML = ""; activeIdx = -1; return; }
        timer = setTimeout(function () { runQuery(q); }, 150);
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

  function navigatePJAX(rawURL, push) {
    var url = withPreviewParams(rawURL);
    var main = document.getElementById("studio-main");
    if (!main || document.documentElement.getAttribute("data-pjax") === "false") {
      window.location.href = url;
      return;
    }
    if (activeFetchCtrl) activeFetchCtrl.abort();
    activeFetchCtrl = new AbortController();
    closePopovers();
    closeAnimated(document.getElementById("volo-search-modal"), null, 180);
    closeAnimated(document.getElementById("volo-lightbox"), null, 180);
    closeAnimated(document.getElementById("volo-toc-drawer"), document.getElementById("volo-mobile-toc-btn"), 180);
    startPJAXBar();
    window.dispatchEvent(new CustomEvent("volo:before-navigate", { detail: { url: url } }));

    fetch(url, { signal: activeFetchCtrl.signal, headers: { "X-Volo-PJAX": "1" } })
      .then(function (res) {
        if (!res.ok || (res.headers.get("content-type") || "").indexOf("text/html") === -1) {
          window.location.href = url;
          return null;
        }
        return res.text();
      })
      .then(function (html) {
        if (!html) return;
        var doc = new DOMParser().parseFromString(html, "text/html");
        var nextMain = doc.getElementById("studio-main");
        if (!nextMain) { window.location.href = url; return; }

        var nextAccent = doc.documentElement.getAttribute("data-accent");
        if (nextAccent) document.documentElement.setAttribute("data-accent", nextAccent);

        document.title = doc.title || document.title;
        main.innerHTML = nextMain.innerHTML;
        main.classList.remove("is-transitioning");
        if (!prefersReducedMotion()) {
          void main.offsetWidth;
          main.classList.add("is-transitioning");
        }

        var parsed = new URL(url, window.location.origin);
        if (push) history.pushState({ volo: true }, "", url);
        lastPathnameAndSearch = parsed.pathname + parsed.search;

        syncActiveNav(parsed.pathname);
        hydratePage(main);

        if (parsed.hash) {
          if (!smoothScrollToHash(parsed.hash.slice(1), true)) {
            window.scrollTo(0, 0);
          }
        } else {
          cancelSmoothScroll();
          window.scrollTo(0, 0);
        }

        finishPJAXBar();
        syncBodyScrollLock();
        window.dispatchEvent(new CustomEvent("volo:after-navigate", { detail: { url: url } }));
      })
      .catch(function (err) {
        finishPJAXBar();
        if (err && err.name !== "AbortError") window.location.href = url;
      });
  }

  function syncActiveNav(pathname) {
    document.querySelectorAll(".studio-nav__link, .studio-nav-more__item").forEach(function (a) {
      var href = a.getAttribute("href") || "";
      if (!href) return;
      a.classList.toggle("is-active", href === pathname || (href !== "/" && pathname.indexOf(href) === 0));
    });
    var moreBtn = document.getElementById("volo-nav-more-btn");
    if (moreBtn) {
      var moreActive = ["/series", "/books", "/links", "/follows", "/dynamic"].some(function (p) {
        return pathname.indexOf(p) === 0;
      });
      moreBtn.classList.toggle("is-active", moreActive);
    }
  }

  function hydratePage(scope) {
    checkBrokenImages(scope);
    initCodeCopy(scope);
    initEnhancements(scope);
    initShareBar(scope);
    inspectGitHubDescriptions(scope);
    initArticleTOC(scope);
    initPodcastAndUnlock(scope);
    syncReadingProgress();
    if (window.VoloCommentWidget && typeof window.VoloCommentWidget.mount === "function") {
      window.VoloCommentWidget.mount();
    }
  }

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
        var safeTitle = shareTitle.replace(/([\[\]])/g, "\\$1");
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

  var resizeTimer = null;
  window.addEventListener("resize", function () {
    clearTimeout(resizeTimer);
    resizeTimer = setTimeout(function () { inspectGitHubDescriptions(document); }, 180);
  }, { passive: true });

  applyTheme(null);
  if (window.matchMedia) {
    window.matchMedia("(prefers-color-scheme: dark)").addEventListener("change", function () {
      if ((document.documentElement.getAttribute("data-mode") || "auto") === "system") applyTheme("system");
    });
  }

  function boot() {
    var mainEl = document.getElementById("studio-main");
    if (mainEl) {
      mainEl.addEventListener("animationend", function (ev) {
        if (ev.target === mainEl) mainEl.classList.remove("is-transitioning");
      });
    }
    checkBrokenImages(document);
    initSearchAndLightbox();
    initBackToTop();
    hydratePage(document);
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", boot);
  } else {
    boot();
  }
})();
