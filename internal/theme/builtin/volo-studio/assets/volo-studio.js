/* Volo Studio Runtime — 4-Mode + 6-Accent Appearance Popover, Persistent PJAX, Instant Search, Podcast & Reader Actions */
(function () {
  "use strict";

  var MODES = ["auto", "system", "light", "dark"];
  var ACCENTS = ["forest", "ocean", "teal", "amber", "plum", "rose"];
  var activeFetchCtrl = null;

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

  function initCodeCopy(scope) {
    var copyLabel = document.documentElement.getAttribute("data-i18n-copy") || "Copy";
    var copiedLabel = document.documentElement.getAttribute("data-i18n-copied") || "Copied";
    (scope || document).querySelectorAll(".volo-prose pre").forEach(function (pre) {
      if (pre.querySelector(".volo-copy-btn")) return;
      var btn = document.createElement("button");
      btn.type = "button";
      btn.className = "volo-copy-btn";
      btn.textContent = copyLabel;
      btn.addEventListener("click", function () {
        var code = pre.querySelector("code");
        var txt = code ? code.textContent : pre.textContent;
        if (navigator.clipboard) {
          navigator.clipboard.writeText(txt || "").then(function () {
            btn.textContent = copiedLabel;
            setTimeout(function () { btn.textContent = copyLabel; }, 1500);
          });
        }
      });
      pre.appendChild(btn);
    });
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
        var input = form.querySelector("input[name='password']");
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

  function initSearchModal() {
    var modal = document.getElementById("volo-search-modal");
    var input = document.getElementById("volo-search-input");
    var list = document.getElementById("volo-search-results");
    if (!modal || !input || !list) return;

    function openSearch() {
      modal.hidden = false;
      input.focus();
    }
    function closeSearch() {
      modal.hidden = true;
    }

    document.addEventListener("click", function (ev) {
      var openTrigger = ev.target.closest("#volo-search-btn");
      if (openTrigger) { openSearch(); return; }
      if (ev.target === modal) closeSearch();
    });

    document.addEventListener("keydown", function (ev) {
      if ((ev.metaKey || ev.ctrlKey) && ev.key.toLowerCase() === "k") {
        ev.preventDefault();
        if (modal.hidden) openSearch(); else closeSearch();
      } else if (ev.key === "Escape") {
        if (!modal.hidden) closeSearch();
        var menu = document.getElementById("volo-theme-menu");
        if (menu) menu.hidden = true;
      }
    });

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

  function initBackToTop() {
    var topBtn = document.getElementById("studioBackToTop");
    if (!topBtn) return;
    function syncTopBtn() {
      topBtn.hidden = window.scrollY < 320;
    }
    window.addEventListener("scroll", syncTopBtn, { passive: true });
    topBtn.addEventListener("click", function () {
      window.scrollTo({ top: 0, behavior: "smooth" });
    });
    syncTopBtn();
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
        if (push) history.pushState({ volo: true }, "", url);
        window.scrollTo(0, 0);
        syncActiveNav(new URL(url, window.location.origin).pathname);
        hydratePage(main);
        window.dispatchEvent(new CustomEvent("volo:after-navigate", { detail: { url: url } }));
      })
      .catch(function (err) {
        if (err && err.name !== "AbortError") window.location.href = url;
      });
  }

  function syncActiveNav(pathname) {
    document.querySelectorAll(".studio-nav__link").forEach(function (a) {
      var href = a.getAttribute("href") || "";
      a.classList.toggle("is-active", href === pathname || (href !== "/" && pathname.indexOf(href) === 0));
    });
  }

  function hydratePage(scope) {
    initCodeCopy(scope);
    initPodcastAndUnlock(scope);
    if (window.VoloCommentWidget && typeof window.VoloCommentWidget.mount === "function") {
      window.VoloCommentWidget.mount();
    }
  }

  document.addEventListener("click", function (ev) {
    var menu = document.getElementById("volo-theme-menu");
    var themeBtn = ev.target.closest("#volo-theme-btn");
    if (themeBtn) {
      if (menu) {
        menu.hidden = !menu.hidden;
      } else {
        var cur = document.documentElement.getAttribute("data-mode") || "auto";
        var next = MODES[(MODES.indexOf(cur) + 1) % MODES.length];
        try { localStorage.setItem("volo_theme_mode", next); } catch (_) {}
        applyTheme(next, null);
      }
      return;
    }
    var modeChoice = ev.target.closest("[data-volo-mode]");
    if (modeChoice) {
      var m = modeChoice.getAttribute("data-volo-mode") || "auto";
      try { localStorage.setItem("volo_theme_mode", m); } catch (_) {}
      applyTheme(m, null);
      if (menu) menu.hidden = true;
      return;
    }
    var accentChoice = ev.target.closest("[data-volo-accent]");
    if (accentChoice) {
      var ac = accentChoice.getAttribute("data-volo-accent") || "forest";
      try { localStorage.setItem("volo_accent", ac); } catch (_) {}
      applyTheme(null, ac);
      return;
    }
    if (menu && !menu.hidden && !ev.target.closest(".volo-appearance")) {
      menu.hidden = true;
    }

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
    if (u.pathname === window.location.pathname && u.hash) return;
    if (/^\/(api|uploads|themes|plugins|rss\.xml|atom\.xml|podcast\.xml|sitemap\.xml|llms)/.test(u.pathname)) return;
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
      initSearchModal();
      initBackToTop();
      hydratePage(document);
    });
  } else {
    initSearchModal();
    initBackToTop();
    hydratePage(document);
  }
})();
