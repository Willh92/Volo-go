(function () {
  var SVGS = {
    neko: '<svg viewBox="0 0 64 64" width="38" height="38" fill="none" stroke="currentColor" stroke-width="2.6" stroke-linecap="round" stroke-linejoin="round"><path d="M14 26 L18 11 L28 20 H36 L46 11 L50 26 C53 33 52 46 32 49 C12 46 11 33 14 26 Z"/><circle cx="24" cy="33" r="2.2" fill="currentColor"/><circle cx="40" cy="33" r="2.2" fill="currentColor"/><path d="M29 39 Q32 42 35 39"/></svg>',
    owl: '<svg viewBox="0 0 64 64" width="38" height="38" fill="none" stroke="currentColor" stroke-width="2.6" stroke-linecap="round" stroke-linejoin="round"><rect x="15" y="14" width="34" height="36" rx="16"/><circle cx="25" cy="29" r="5"/><circle cx="39" cy="29" r="5"/><path d="M30 35 L32 39 L34 35 Z" fill="currentColor"/></svg>',
    bot: '<svg viewBox="0 0 64 64" width="38" height="38" fill="none" stroke="currentColor" stroke-width="2.6" stroke-linecap="round" stroke-linejoin="round"><line x1="32" y1="9" x2="32" y2="17"/><circle cx="32" cy="7" r="2"/><rect x="14" y="17" width="36" height="30" rx="8"/><rect x="22" y="27" width="6" height="6" rx="1" fill="currentColor"/><rect x="36" y="27" width="6" height="6" rx="1" fill="currentColor"/><line x1="25" y1="39" x2="39" y2="39"/></svg>'
  };

  var collapseTimer = 0;
  var hideTimer = 0;
  var dismissed = false;

  function resolveLabels(root) {
    var lang = String((document.documentElement && document.documentElement.lang) || "").toLowerCase();
    var isEN = lang.indexOf("en") === 0;
    var isTW = lang === "zh-tw" || lang.indexOf("zh-hant") === 0;
    var greeting = isEN
      ? (root.getAttribute("data-greeting-en") || root.getAttribute("data-greeting") || "Welcome! Click me for a head-pat~")
      : isTW
        ? (root.getAttribute("data-greeting-tw") || root.getAttribute("data-greeting") || "歡迎來到獨立部落格，點我可以摸摸頭～")
        : (root.getAttribute("data-greeting-zh") || root.getAttribute("data-greeting") || "欢迎来到独立博客，点我可以摸摸头～");
    return {
      dock: isEN ? "Studio Mascot" : "看板娘助手",
      btn: isEN ? "Pat mascot" : "摸摸看板娘",
      close: isEN ? "Dismiss message" : "收起提示",
      rateLimited: isEN ? "Gentle pats please~" : isTW ? "摸太快啦，休息一下～" : "摸太快啦，歇会儿再摸～",
      greeting: greeting
    };
  }

  function collapseBubble(bubble) {
    if (!bubble || bubble.hidden) return;
    clearTimeout(collapseTimer);
    clearTimeout(hideTimer);
    bubble.classList.add("is-collapsed");
    hideTimer = setTimeout(function () {
      if (bubble.classList.contains("is-collapsed")) bubble.hidden = true;
    }, 200);
  }

  function showBubble(bubble, textEl, text, autoHideMs) {
    if (!bubble || !textEl) return;
    clearTimeout(collapseTimer);
    clearTimeout(hideTimer);
    if (text) textEl.textContent = text;
    bubble.hidden = false;
    requestAnimationFrame(function () {
      bubble.classList.remove("is-collapsed");
    });
    if (autoHideMs > 0) {
      collapseTimer = setTimeout(function () {
        collapseBubble(bubble);
      }, autoHideMs);
    }
  }

  function mount() {
    var root = document.getElementById("volo-kanbanniang-root");
    if (!root || root.getAttribute("data-mounted") === "1") return;
    root.setAttribute("data-mounted", "1");
    if (!root.parentElement || !root.parentElement.hasAttribute("data-volo-persist")) {
      root.setAttribute("data-volo-persist", "kanbanniang");
    }

    var labels = resolveLabels(root);
    root.setAttribute("aria-label", root.getAttribute("aria-label") || labels.dock);

    var ch = root.getAttribute("data-character") || "neko";
    var greeting = root.getAttribute("data-greeting") || labels.greeting;
    var patCount = parseInt(root.getAttribute("data-pats") || "0", 10) || 0;

    var bubble = document.createElement("div");
    bubble.className = "volo-kbn-bubble";
    var textSpan = document.createElement("span");
    textSpan.className = "volo-kbn-bubble__text";
    textSpan.textContent = greeting;

    var closeBtn = document.createElement("button");
    closeBtn.type = "button";
    closeBtn.className = "volo-kbn-close";
    var initCloseLbl = root.getAttribute("data-close-label") || labels.close;
    closeBtn.setAttribute("aria-label", initCloseLbl);
    closeBtn.title = initCloseLbl;
    closeBtn.textContent = "×";
    closeBtn.addEventListener("click", function (ev) {
      ev.stopPropagation();
      dismissed = true;
      collapseBubble(bubble);
    });

    bubble.appendChild(textSpan);
    bubble.appendChild(closeBtn);

    var btn = document.createElement("button");
    btn.type = "button";
    btn.className = "volo-kbn-btn";
    btn.setAttribute("aria-label", root.getAttribute("data-btn-label") || labels.btn);
    btn.innerHTML = SVGS[ch] || SVGS.neko;

    btn.addEventListener("mouseenter", function () {
      if (dismissed) return;
      showBubble(bubble, textSpan, "", 0);
    });
    btn.addEventListener("mouseleave", function () {
      if (!bubble.hidden) {
        clearTimeout(collapseTimer);
        collapseTimer = setTimeout(function () { collapseBubble(bubble); }, 5000);
      }
    });

    btn.addEventListener("click", function () {
      dismissed = false;
      var curLabels = resolveLabels(root);
      fetch("/plugins/kanbanniang/api/pat", {
        method: "POST",
        headers: { Accept: "application/json" }
      })
        .then(function (r) {
          return r.json().catch(function () { return {}; }).then(function (d) {
            return { status: r.status, ok: r.ok, d: d };
          });
        })
        .then(function (res) {
          if (res.status === 429) {
            showBubble(bubble, textSpan, curLabels.rateLimited, 3500);
            return;
          }
          if (res.d && typeof res.d.total_pats === "number") {
            patCount = res.d.total_pats;
          } else if (res.ok) {
            patCount += 1;
          }
          root.setAttribute("data-pats", String(patCount));
          showBubble(bubble, textSpan, "✨ +" + patCount, 5000);
        })
        .catch(function () {
          patCount += 1;
          root.setAttribute("data-pats", String(patCount));
          showBubble(bubble, textSpan, "✨ +" + patCount, 5000);
        });
    });

    root.appendChild(bubble);
    root.appendChild(btn);

    collapseTimer = setTimeout(function () {
      collapseBubble(bubble);
    }, 5000);
    syncMobileOffset();
  }

  function syncMobileOffset() {
    var root = document.getElementById("volo-kanbanniang-root");
    if (!root) return;
    var pos = root.getAttribute("data-position") || (root.classList.contains("volo-kbn-right") ? "right" : "left");
    var fab = document.querySelector(".book-mobile-toc-fab");
    var fabOnScreen = !!(
      fab &&
      !fab.hidden &&
      (window.innerWidth || 1024) <= 900 &&
      pos !== "right" &&
      window.getComputedStyle(fab).display !== "none"
    );
    root.classList.toggle("is-book-fab-offset", fabOnScreen);
    var podcastOpen =
      document.documentElement.getAttribute("data-podcast-open") === "1" ||
      (document.body && document.body.classList.contains("has-podcast-mini"));
    if (fabOnScreen && !podcastOpen) {
      root.style.bottom = "calc(68px + env(safe-area-inset-bottom, 0px))";
    } else {
      root.style.removeProperty("bottom");
    }
  }

  function onAfterNavigate() {
    var root = document.getElementById("volo-kanbanniang-root");
    if (!root) return;
    if (root.getAttribute("data-mounted") !== "1") {
      mount();
      return;
    }
    var labels = resolveLabels(root);
    root.setAttribute("aria-label", labels.dock);
    var btn = root.querySelector(".volo-kbn-btn");
    if (btn) btn.setAttribute("aria-label", labels.btn);
    var closeBtn = root.querySelector(".volo-kbn-close");
    if (closeBtn) {
      closeBtn.setAttribute("aria-label", labels.close);
      closeBtn.title = labels.close;
    }
    var bubble = root.querySelector(".volo-kbn-bubble");
    var textEl = root.querySelector(".volo-kbn-bubble__text");
    if (textEl && (!bubble || bubble.hidden)) {
      textEl.textContent = labels.greeting;
    }
    /* Keep patCount intact across PJAX; collapse speech bubble on book reader so TOC/controls are unobstructed */
    if (document.querySelector("[data-book-reader]")) {
      collapseBubble(bubble);
    }
    syncMobileOffset();
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", mount, { once: true });
  } else {
    mount();
  }
  if (!window.__voloKanbanniangPJAXBound) {
    window.__voloKanbanniangPJAXBound = true;
    document.addEventListener("volo:pjax:after", onAfterNavigate);
    document.addEventListener("volo:page-load", onAfterNavigate);
    window.addEventListener("scroll", syncMobileOffset, { passive: true });
    window.addEventListener("resize", syncMobileOffset, { passive: true });
  }
})();
