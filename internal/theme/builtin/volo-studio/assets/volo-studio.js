/* Volo Studio Runtime — Appearance & Language Popovers, PJAX Progress Bar, Reading Progress, Sticky TOC, Lightbox, Code Header & Reader Actions */
(function () {
  "use strict";

  var MODES = ["auto", "system", "light", "dark"];
  var ACCENTS = ["forest", "ocean", "teal", "amber", "plum", "rose"];
  var activeFetchCtrl = null;
  var pjaxTimer = null;
  var tocCleanups = [];

  function resolveEffectiveTheme(mode) {
    if (mode === "light" || mode === "dark") return mode;
    if (mode === "system") {
      return window.matchMedia && window.matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light";
    }
    var h = new Date().getHours();
    return h >= 7 && h < 19 ? "light" : "dark";
  }

  function applyTheme(mode, accent) {
    var root = document.documentElement;
    var savedMode = "";
    var savedAccent = "";
    try {
      savedMode = localStorage.getItem("volo_theme_mode") || "";
      savedAccent = localStorage.getItem("volo_accent") || "";
    } catch (_) {}
    var chosenMode = mode || savedMode || root.getAttribute("data-default-mode") || "auto";
    var chosenAccent = accent || savedAccent || root.getAttribute("data-default-accent") || root.getAttribute("data-accent") || "forest";
    if (ACCENTS.indexOf(chosenAccent) === -1) chosenAccent = "forest";

    root.setAttribute("data-mode", chosenMode);
    root.setAttribute("data-theme", resolveEffectiveTheme(chosenMode));
    root.setAttribute("data-accent", chosenAccent);

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
      el.classList.toggle("is-selected", el.getAttribute("data-volo-mode") === chosenMode);
    });
    document.querySelectorAll("[data-volo-accent]").forEach(function (el) {
      el.classList.toggle("is-selected", el.getAttribute("data-volo-accent") === chosenAccent);
    });
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
    var article = document.querySelector(".studio-article");
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

  /* Broken Image Fallbacks (Brand Avatar, Article Covers, Nav Icons) */
  function handleBrokenImg(img) {
    if (!img || img.dataset.fallbackDone) return;
    if (img.hasAttribute("data-brand-logo")) {
      img.dataset.fallbackDone = "1";
      img.hidden = true;
      var mark = img.parentElement && img.parentElement.querySelector(".studio-mark");
      if (mark) mark.hidden = false;
    } else if (img.hasAttribute("data-cover-img")) {
      img.dataset.fallbackDone = "1";
      img.hidden = true;
      var fb = img.parentElement && img.parentElement.querySelector(".studio-cover-fallback");
      if (fb) fb.hidden = false;
    } else if (img.hasAttribute("data-hide-on-error")) {
      img.dataset.fallbackDone = "1";
      img.style.display = "none";
    }
  }

  document.addEventListener("error", function (ev) {
    var t = ev.target;
    if (t && t.tagName === "IMG") handleBrokenImg(t);
  }, true);

  function checkBrokenImages(scope) {
    (scope || document).querySelectorAll("img[data-brand-logo], img[data-cover-img], img[data-hide-on-error]").forEach(function (img) {
      if (img.complete && img.naturalWidth === 0) handleBrokenImg(img);
    });
  }

  /* Code Block Header Bar + Copy Button */
  function initCodeCopy(scope) {
    var copyLabel = document.documentElement.getAttribute("data-i18n-copy") || "Copy";
    var copiedLabel = document.documentElement.getAttribute("data-i18n-copied") || "Copied";
    (scope || document).querySelectorAll(".volo-prose pre").forEach(function (pre) {
      if (pre.parentElement && pre.parentElement.classList.contains("volo-code-block")) return;
      if (pre.querySelector(".volo-copy-btn")) return;

      var code = pre.querySelector("code");
      var lang = "";
      if (code && code.className) {
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
        var txt = code ? code.textContent : pre.textContent;
        if (navigator.clipboard) {
          navigator.clipboard.writeText(txt || "").then(function () {
            btn.textContent = copiedLabel;
            setTimeout(function () { btn.textContent = copyLabel; }, 1500);
          });
        }
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

  /* Sticky Collapsible Article TOC + Scroll-Spy */
  function initArticleTOC(scope) {
    while (tocCleanups.length) {
      var fn = tocCleanups.pop();
      try { fn(); } catch (_) {}
    }
    var root = scope || document;
    var tocCard = root.querySelector("#volo-toc-card") || document.getElementById("volo-toc-card");
    var toggleBtn = root.querySelector("#volo-toc-toggle") || document.getElementById("volo-toc-toggle");
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
      var el = document.getElementById(id);
      if (el) {
        seen[id] = true;
        headings.push(el);
      }
    });
    if (!headings.length) return;

    function updateSpy() {
      var activeID = headings[0].id;
      for (var i = 0; i < headings.length; i++) {
        if (headings[i].getBoundingClientRect().top <= 118) {
          activeID = headings[i].id;
        } else {
          break;
        }
      }
      links.forEach(function (a) {
        var match = a.getAttribute("data-toc-id") === activeID;
        a.classList.toggle("is-active", match);
      });
    }

    window.addEventListener("scroll", updateSpy, { passive: true });
    tocCleanups.push(function () { window.removeEventListener("scroll", updateSpy); });
    updateSpy();
  }

  function initPodcastAndUnlock(scope) {
    var root = scope || document;
    root.querySelectorAll("[data-audio-seek]").forEach(function (btn) {
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
    if (speedBtn) {
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

  function closePopovers() {
    var themeMenu = document.getElementById("volo-theme-menu");
    var themeBtn = document.getElementById("volo-theme-btn");
    if (themeMenu) themeMenu.hidden = true;
    if (themeBtn) themeBtn.setAttribute("aria-expanded", "false");

    var langMenu = document.getElementById("volo-lang-menu");
    var langBtn = document.getElementById("volo-lang-btn");
    if (langMenu) langMenu.hidden = true;
    if (langBtn) langBtn.setAttribute("aria-expanded", "false");

    document.querySelectorAll("details.studio-nav-more[open]").forEach(function (d) {
      d.removeAttribute("open");
    });
  }

  function initSearchAndLightbox() {
    var modal = document.getElementById("volo-search-modal");
    var input = document.getElementById("volo-search-input");
    var list = document.getElementById("volo-search-results");
    var lightbox = document.getElementById("volo-lightbox");
    var lightboxImg = document.getElementById("volo-lightbox-img");

    function openSearch() {
      if (!modal || !input) return;
      closePopovers();
      modal.hidden = false;
      input.focus();
    }
    function closeSearch() {
      if (modal) modal.hidden = true;
    }
    function closeLightbox() {
      if (lightbox) lightbox.hidden = true;
    }

    document.addEventListener("click", function (ev) {
      if (ev.target.closest("#volo-search-btn")) {
        openSearch();
        return;
      }
      if (modal && ev.target === modal) {
        closeSearch();
        return;
      }
      if (lightbox && !lightbox.hidden && (ev.target === lightbox || ev.target === lightboxImg)) {
        closeLightbox();
        return;
      }
      var proseImg = ev.target.closest(".volo-prose img");
      if (proseImg && !proseImg.closest("a") && lightbox && lightboxImg) {
        var src = proseImg.getAttribute("src");
        if (src) {
          lightboxImg.src = src;
          lightboxImg.alt = proseImg.getAttribute("alt") || "";
          lightbox.hidden = false;
        }
      }
    });

    document.addEventListener("keydown", function (ev) {
      if ((ev.metaKey || ev.ctrlKey) && ev.key.toLowerCase() === "k") {
        if (modal) {
          ev.preventDefault();
          if (modal.hidden) openSearch(); else closeSearch();
        }
      } else if (ev.key === "Escape") {
        closeSearch();
        closeLightbox();
        closePopovers();
      }
    });

    if (modal && input && list) {
      var timer = null;
      input.addEventListener("input", function () {
        clearTimeout(timer);
        var q = input.value.trim();
        if (!q) { list.innerHTML = ""; return; }
        timer = setTimeout(function () {
          fetch("/api/v1/search?q=" + encodeURIComponent(q))
            .then(function (r) { return r.json(); })
            .then(function (d) {
              var hits = (d && d.hits) || [];
              if (!hits.length) {
                list.innerHTML = '<div style="padding:14px;color:var(--volo-muted)">' + (modal.getAttribute("data-empty") || "No results") + "</div>";
                return;
              }
              list.innerHTML = hits.map(function (h) {
                return '<a class="volo-search-hit" href="' + encodeURI(h.url) + '"><strong>' + escapeHTML(h.title) + '</strong><div style="font-size:12.5px;color:var(--volo-muted)">' + escapeHTML(h.snippet || "") + "</div></a>";
              }).join("");
            }).catch(function () {});
        }, 160);
      });
    }
  }

  function initBackToTop() {
    var topBtn = document.getElementById("studioBackToTop");
    function onScroll() {
      if (topBtn) topBtn.hidden = window.scrollY < 320;
      syncReadingProgress();
    }
    window.addEventListener("scroll", onScroll, { passive: true });
    if (topBtn) {
      topBtn.addEventListener("click", function () {
        window.scrollTo({ top: 0, behavior: "smooth" });
      });
    }
    onScroll();
  }

  function escapeHTML(s) {
    return String(s).replace(/[&<>"']/g, function (c) {
      return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c];
    });
  }

  function navigatePJAX(url, push) {
    var main = document.getElementById("studio-main");
    if (!main || document.documentElement.getAttribute("data-pjax") === "false") {
      window.location.href = url;
      return;
    }
    if (activeFetchCtrl) activeFetchCtrl.abort();
    activeFetchCtrl = new AbortController();
    closePopovers();
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
        document.title = doc.title || document.title;
        main.innerHTML = nextMain.innerHTML;
        main.classList.remove("is-transitioning");
        void main.offsetWidth;
        main.classList.add("is-transitioning");
        if (push) history.pushState({ volo: true }, "", url);
        if (url.indexOf("#") !== -1) {
          var hashID = decodeURIComponent(url.split("#")[1] || "");
          var targetEl = hashID && document.getElementById(hashID);
          if (targetEl) targetEl.scrollIntoView({ behavior: "smooth" });
          else window.scrollTo(0, 0);
        } else {
          window.scrollTo(0, 0);
        }
        syncActiveNav(new URL(url, window.location.origin).pathname);
        hydratePage(main);
        finishPJAXBar();
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
  }

  function hydratePage(scope) {
    checkBrokenImages(scope);
    initCodeCopy(scope);
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

    if (themeBtn) {
      if (langMenu) langMenu.hidden = true;
      var lb = document.getElementById("volo-lang-btn");
      if (lb) lb.setAttribute("aria-expanded", "false");
      if (themeMenu) {
        var nextOpen = themeMenu.hidden;
        themeMenu.hidden = !nextOpen;
        themeBtn.setAttribute("aria-expanded", nextOpen ? "true" : "false");
      } else {
        var cur = document.documentElement.getAttribute("data-mode") || "auto";
        var next = MODES[(MODES.indexOf(cur) + 1) % MODES.length];
        try { localStorage.setItem("volo_theme_mode", next); } catch (_) {}
        applyTheme(next, null);
      }
      return;
    }

    if (langBtn && langMenu) {
      if (themeMenu) themeMenu.hidden = true;
      var tb = document.getElementById("volo-theme-btn");
      if (tb) tb.setAttribute("aria-expanded", "false");
      var openLang = langMenu.hidden;
      langMenu.hidden = !openLang;
      langBtn.setAttribute("aria-expanded", openLang ? "true" : "false");
      return;
    }

    var modeChoice = ev.target.closest("[data-volo-mode]");
    if (modeChoice) {
      var m = modeChoice.getAttribute("data-volo-mode") || "auto";
      try { localStorage.setItem("volo_theme_mode", m); } catch (_) {}
      applyTheme(m, null);
      if (themeMenu) themeMenu.hidden = true;
      return;
    }

    var accentChoice = ev.target.closest("[data-volo-accent]");
    if (accentChoice) {
      var ac = accentChoice.getAttribute("data-volo-accent") || "forest";
      try { localStorage.setItem("volo_accent", ac); } catch (_) {}
      applyTheme(null, ac);
      return;
    }

    if (themeMenu && !themeMenu.hidden && !ev.target.closest(".volo-appearance")) {
      themeMenu.hidden = true;
      var tb2 = document.getElementById("volo-theme-btn");
      if (tb2) tb2.setAttribute("aria-expanded", "false");
    }
    if (langMenu && !langMenu.hidden && !ev.target.closest(".volo-lang-pop")) {
      langMenu.hidden = true;
      var lb2 = document.getElementById("volo-lang-btn");
      if (lb2) lb2.setAttribute("aria-expanded", "false");
    }
    document.querySelectorAll("details.studio-nav-more[open]").forEach(function (d) {
      if (!d.contains(ev.target)) d.removeAttribute("open");
    });

    var langSel = ev.target.closest("[data-set-lang]");
    if (langSel) {
      var code = langSel.getAttribute("data-set-lang");
      document.cookie = "volo_lang=" + encodeURIComponent(code) + "; path=/; max-age=31536000; SameSite=Lax";
      window.location.reload();
      return;
    }

    var logoutBtn = ev.target.closest("#volo-reader-logout");
    if (logoutBtn) {
      ev.preventDefault();
      fetch("/api/v1/reader/logout", { method: "POST" }).then(function () { window.location.reload(); });
      return;
    }

    var link = ev.target.closest("a[href]");
    if (!link || ev.defaultPrevented || ev.metaKey || ev.ctrlKey || ev.shiftKey || ev.altKey) return;
    if (link.target && link.target !== "_self") return;
    if (link.hasAttribute("download") || link.hasAttribute("data-no-pjax")) return;
    var u;
    try { u = new URL(link.href, window.location.origin); } catch (_) { return; }
    if (u.origin !== window.location.origin) return;
    if (u.pathname === window.location.pathname && u.hash) {
      var mobileToc = link.closest("details.volo-mobile-toc");
      if (mobileToc) mobileToc.removeAttribute("open");
      return;
    }
    if (/^\/(api|uploads|themes|plugins|images|logout|admin-index\.do|admin\/index\.do|rss\.xml|atom\.xml|podcast\.xml|sitemap\.xml|llms)/.test(u.pathname)) return;
    var modal = document.getElementById("volo-search-modal");
    if (modal) modal.hidden = true;
    ev.preventDefault();
    navigatePJAX(u.pathname + u.search + u.hash, true);
  });

  window.addEventListener("popstate", function () {
    navigatePJAX(window.location.pathname + window.location.search + window.location.hash, false);
  });

  applyTheme(null, null);
  if (window.matchMedia) {
    window.matchMedia("(prefers-color-scheme: dark)").addEventListener("change", function () {
      if ((document.documentElement.getAttribute("data-mode") || "auto") === "system") applyTheme("system", null);
    });
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", function () {
      checkBrokenImages(document);
      initSearchAndLightbox();
      initBackToTop();
      hydratePage(document);
    });
  } else {
    checkBrokenImages(document);
    initSearchAndLightbox();
    initBackToTop();
    hydratePage(document);
  }
})();
