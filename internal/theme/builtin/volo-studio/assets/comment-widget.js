/* Volo-go Zero-Dependency Comment Widget (< 10 KiB Gzipped) */
(function () {
  "use strict";
  var win = window, doc = document, encU = encodeURIComponent;
  var LS_KEY = "volo_gp_v1", PROF_KEY = "volo.comment.profile.v1", IDB_NAME = "volo_identity", IDB_STORE = "kv";

  if (!win.__voloAvatarErrBound) {
    win.__voloAvatarErrBound = true;
    doc.addEventListener("error", function (ev) {
      var img = ev.target;
      if (!img || img.tagName !== "IMG" || !img.hasAttribute("data-avatar-seed") || img.dataset.fallbackApplied === "1") return;
      var fb = "/avatars/" + encU(img.getAttribute("data-avatar-seed") || "guest") + ".svg";
      img.dataset.fallbackApplied = "1";
      if ((img.getAttribute("src") || "") !== fb) img.src = fb;
    }, true);
  }

  function idbOp(mode, fn) {
    return new Promise(function (res) {
      if (!win.indexedDB) return res("");
      var r = indexedDB.open(IDB_NAME, 1);
      r.onupgradeneeded = function () { r.result.createObjectStore(IDB_STORE); };
      r.onsuccess = function () {
        try { fn(r.result.transaction(IDB_STORE, mode).objectStore(IDB_STORE), res); } catch (_) { res(""); }
      };
      r.onerror = function () { res(""); };
    });
  }

  function savePassport(tok) {
    if (!tok) return;
    try { localStorage.setItem(LS_KEY, tok); } catch (_) {}
    idbOp("readwrite", function (st, res) { st.put(tok, LS_KEY); res(""); });
  }

  function loadPassport() {
    var ls = "";
    try { ls = localStorage.getItem(LS_KEY) || ""; } catch (_) {}
    return idbOp("readonly", function (st, res) {
      var r = st.get(LS_KEY);
      r.onsuccess = function () { res(r.result || ""); };
      r.onerror = function () { res(""); };
    }).then(function (iv) {
      var tok = ls || iv || "";
      if (tok) {
        if (!ls || !iv) savePassport(tok);
        return tok;
      }
      return fetch("/api/v1/guest/passport", { credentials: "same-origin" })
        .then(function (r) { return r.ok ? r.json() : null; })
        .then(function (d) { var m = (d && d.passport_token) || ""; savePassport(m); return m; })
        .catch(function () { return ""; });
    });
  }

  function solvePoWMain(chal, bits) {
    var te = new TextEncoder(), i = 0;
    return (function step() {
      var s = "" + i++;
      return crypto.subtle.digest("SHA-256", te.encode(chal + ":" + s)).then(function (buf) {
        var h = new Uint8Array(buf), z = 0;
        for (var j = 0; j < h.length; j++) {
          if (!h[j]) { z += 8; continue; }
          for (var k = 7; k >= 0; k--) { if (!(h[j] & (1 << k))) z++; else { j = 99; break; } }
        }
        return z >= bits ? s : step();
      });
    })();
  }

  function solvePoW(chal, bits) {
    if (win.Worker && win.Blob && win.URL) {
      return new Promise(function (res) {
        var src = "onmessage=async e=>{let{c,b}=e.data,t=new TextEncoder();for(let i=0;;i++){let s=''+i,h=new Uint8Array(await crypto.subtle.digest('SHA-256',t.encode(c+':'+s))),z=0;for(let j=0;j<h.length;j++){if(!h[j]){z+=8;continue}for(let k=7;k>=0;k--)if(!(h[j]&1<<k))z++;else{j=99;break}}if(z>=b){postMessage(s);return}}}";
        try {
          var u = URL.createObjectURL(new Blob([src], { type: "text/javascript" })), w = new Worker(u);
          var fin = function (fn) { w.terminate(); URL.revokeObjectURL(u); fn(); };
          w.onmessage = function (ev) { fin(function () { res(ev.data); }); };
          w.onerror = function () { fin(function () { solvePoWMain(chal, bits).then(res); }); };
          w.postMessage({ c: chal, b: bits });
        } catch (_) { solvePoWMain(chal, bits).then(res); }
      });
    }
    return solvePoWMain(chal, bits);
  }

  var ESC_MAP = { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" };
  function esc(s) { return String(s || "").replace(/[&<>"']/g, function (c) { return ESC_MAP[c]; }); }

  function fmtTime(ms) {
    var d = new Date(ms), p = function (n) { return n < 10 ? "0" + n : "" + n; };
    return d.getFullYear() + "-" + p(d.getMonth() + 1) + "-" + p(d.getDate()) + " " + p(d.getHours()) + ":" + p(d.getMinutes());
  }

  function decorateCode(raw, copyLbl) {
    return String(raw || "").replace(/<pre(\b[^>]*)>(\s*<code\b[\s\S]*?<\/code>\s*)<\/pre>/gi, function (_, a, code) {
      return '<div class="vc-code-wrap"><button type="button" class="volo-copy-btn vc-code-copy js-vc-copy">' + esc(copyLbl) + "</button><pre" + a + ">" + code + "</pre></div>";
    });
  }

  function renderPreviewMD(md, emptyLbl, copyLbl) {
    var tr = String(md || "").trim();
    if (!tr) return '<div class="vc-empty">' + esc(emptyLbl) + "</div>";
    var blks = [];
    var html = esc(tr).replace(/```([a-zA-Z0-9_+-]*)\n([\s\S]*?)```/g, function (_, _l, c) {
      return "\x00B" + (blks.push("<pre><code>" + c + "</code></pre>") - 1) + "\x00";
    }).split(/\n{2,}/).map(function (p) {
      if (/^\x00B\d+\x00$/.test(p.trim())) return p.trim();
      var inl = p.replace(/`([^`\n]+)`/g, "<code>$1</code>")
        .replace(/\*\*([^*]+)\*\*/g, "<strong>$1</strong>")
        .replace(/\*([^*\n]+)\*/g, "<em>$1</em>")
        .replace(/\[([^\]]+)\]\((https?:\/\/[^\s)]+)\)/g, '<a href="$2" target="_blank" rel="nofollow ugc noopener">$1</a>');
      return /^&gt;\s?/.test(inl)
        ? "<blockquote>" + inl.replace(/^&gt;\s?/gm, "").replace(/\n/g, "<br>") + "</blockquote>"
        : "<p>" + inl.replace(/\n/g, "<br>") + "</p>";
    }).join("").replace(/\x00B(\d+)\x00/g, function (_, i) { return blks[+i] || ""; });
    return decorateCode(html, copyLbl);
  }

  function applyMdTool(ta, act) {
    var s = ta.selectionStart || 0, e = ta.selectionEnd || 0, v = ta.value || "", sel = v.slice(s, e), r = "", o = 0;
    var nl = s > 0 && v.charAt(s - 1) !== "\n" ? "\n" : "";
    if (act === "bold") { r = "**" + (sel || "bold") + "**"; o = sel ? r.length : 2; }
    else if (act === "italic") { r = "*" + (sel || "italic") + "*"; o = sel ? r.length : 1; }
    else if (act === "code") { r = "`" + (sel || "code") + "`"; o = sel ? r.length : 1; }
    else if (act === "quote") { r = nl + "> " + sel; o = r.length; }
    else if (act === "link") { r = "[" + (sel || "link") + "](https://)"; o = r.length - 1; }
    else if (act === "fence") { r = nl + "```\n" + sel + "\n```"; o = r.length - 4; }
    ta.value = v.slice(0, s) + r + v.slice(e);
    ta.focus();
    ta.setSelectionRange(s + o, s + o);
    ta.dispatchEvent(new Event("input"));
  }

  function hashCommentID() {
    var h = (win.location.hash || "").slice(1);
    try { h = decodeURIComponent(h); } catch (_) {}
    return h.indexOf("comment-") === 0 ? h.slice(8) : "";
  }

  function loginHref() {
    var p = (win.location.pathname || "/") + (win.location.search || "") + "#volo-comments";
    return (p.charAt(0) !== "/" || p.indexOf("//") === 0 || p.indexOf("/\\") === 0) ? "/reader/login" : "/reader/login?next=" + encU(p);
  }

  function copyStr(txt) {
    if (navigator.clipboard && navigator.clipboard.writeText) return navigator.clipboard.writeText(txt);
    return new Promise(function (ok, fail) {
      try {
        var t = doc.createElement("textarea");
        t.value = txt; t.style.cssText = "position:fixed;opacity:0";
        doc.body.appendChild(t); t.select(); doc.execCommand("copy"); t.remove(); ok();
      } catch (e) { fail(e); }
    });
  }

  function mount() {
    var el = doc.getElementById("volo-comments");
    if (!el || el.getAttribute("data-mounted") === "1") return;
    el.setAttribute("data-mounted", "1");

    var rootEl = doc.documentElement;
    var attr = function (k, d) { return el.getAttribute(k) || d || ""; };
    var dL = function (k, d) { return attr("data-l-" + k, d); };
    var tType = attr("data-target-type", "article"), tID = attr("data-target-id");
    var isLogged = attr("data-is-logged") === "true";
    var viewerID = attr("data-viewer-id"), viewerName = attr("data-viewer-name", "Reader"), viewerRole = attr("data-viewer-role");
    var viewerAvatar = attr("data-viewer-avatar") || ("/avatars/" + encU(viewerID || viewerName) + ".svg");
    var csrfToken = attr("data-csrf-token");
    var mode = attr("data-comment-mode") || attr("data-access-mode", "all");
    var copyLbl = dL("copy", rootEl.getAttribute("data-i18n-copy") || "Copy");
    var copiedLbl = dL("copied", rootEl.getAttribute("data-i18n-copied") || "Copied");
    var netErrLbl = dL("net-err", "Request failed");

    var gpToken = "", replyParentID = "", replyParentName = "", editingID = "", banTargetID = "", banHours = 0;
    var curPage = 1, totalPages = 1;
    var canCompose = mode !== "off" && !(mode === "registered_only" && !isLogged);
    var seekID = hashCommentID(), seekSteps = 0, initialScrollPending = true, statusBanner = null;

    function jReq(url, method, body, withCSRF) {
      var h = { "Content-Type": "application/json" };
      if (gpToken) h["X-Volo-Guest-Passport"] = gpToken;
      if (win.__voloEnvFP) h["X-Volo-Env-FP"] = win.__voloEnvFP();
      if (withCSRF && csrfToken) h["X-CSRF-Token"] = csrfToken;
      var o = { method: method || "GET", headers: h };
      if (body) o.body = JSON.stringify(body);
      return fetch(url, o).then(function (r) {
        return r.json().catch(function () { return {}; }).then(function (d) { return { ok: r.ok, d: d || {} }; });
      });
    }

    function countTree(items) {
      var n = 0;
      (items || []).forEach(function (c) { n += 1 + countTree(c.replies); });
      return n;
    }

    function hasDescendant(list, id) {
      return !!id && (list || []).some(function (x) { return String(x.id) === id || hasDescendant(x.replies, id); });
    }

    function renderItem(c, depth, parentAuthor) {
      var seed = c.avatar_seed || c.id || "guest";
      var av = c.avatar_url || ("/avatars/" + encU(seed) + ".svg");
      var pName = c.reply_to_name || (depth >= 1 ? parentAuthor : "");
      var at = pName ? '<span class="vc-reply-at">@' + esc(pName) + "</span>" : "";
      var rawURL = c.author_url ? String(c.author_url).trim() : "";
      var who = /^https?:\/\//i.test(rawURL)
        ? '<a href="' + esc(rawURL) + '" rel="nofollow ugc noopener" target="_blank">' + esc(c.author_name) + "</a>"
        : "<strong>" + esc(c.author_name) + "</strong>";
      var cid = esc(c.id), delLbl = dL("del", "Delete");
      var mkAct = function (cls, ex, lbl) { return '<button type="button" class="vc-act-btn ' + cls + '" data-id="' + cid + '"' + ex + ">" + esc(lbl) + "</button>"; };

      var acts = [];
      if (canCompose) acts.push(mkAct("js-vc-reply", ' data-name="' + esc(c.author_name) + '"', dL("reply", "Reply")));
      if (c.can_edit) {
        acts.push(mkAct("js-vc-edit", ' data-md="' + esc(c.content_md || "") + '"', dL("edit", "Edit")));
        acts.push(mkAct("vc-act-btn--danger js-vc-del", "", delLbl));
      }
      if (c.can_moderate && csrfToken) {
        if (c.status === "pending") acts.push(mkAct("js-vc-mod-approve", "", dL("approve", "Approve")));
        if (!c.can_edit) acts.push(mkAct("vc-act-btn--danger js-vc-mod-del", "", delLbl));
        if (c.can_ban_ip) acts.push(mkAct("vc-act-btn--danger js-vc-mod-ban", ' data-ip="' + esc(c.ip_masked || "") + '"', dL("ban", "Ban IP")));
      }

      var reps = c.replies || [], kids = "";
      if (reps.length) {
        var dNext = (depth || 0) + 1, dAttr = ' data-depth="' + Math.min(dNext, 3) + '"';
        var mapR = function (a) { return a.map(function (r) { return renderItem(r, dNext, c.author_name || ""); }).join(""); };
        kids = (!depth && reps.length > 3 && !hasDescendant(reps.slice(3), hashCommentID()))
          ? '<div class="vc-replies"' + dAttr + ">" + mapR(reps.slice(0, 3)) + '<div class="js-vc-more-box" hidden>' + mapR(reps.slice(3)) +
            '</div><button type="button" class="vc-more-replies js-vc-show-more">' + esc(dL("more-replies", "+{count}").replace("{count}", "" + (reps.length - 3))) + "</button></div>"
          : '<div class="vc-replies"' + dAttr + ">" + mapR(reps) + "</div>";
      }

      var bodyHTML = decorateCode(c.content_html || "", copyLbl);
      if (at) {
        bodyHTML = /^\s*<p(?:\s[^>]*)?>/i.test(bodyHTML) ? bodyHTML.replace(/^(\s*<p(?:\s[^>]*)?>)/i, "$1" + at + " ") : at + " " + bodyHTML;
      }

      return '<div class="vc-item" id="comment-' + cid + '">' +
        '<img class="vc-avatar" src="' + esc(av) + '" data-avatar-seed="' + esc(seed) + '" alt="" width="38" height="38" loading="lazy">' +
        '<div class="vc-body"><div class="vc-meta">' + who +
        (c.is_author ? '<span class="vc-author-badge">' + esc(dL("author", "Author")) + "</span>" : "") +
        (c.status === "pending" ? '<span class="vc-pending-badge vc-badge--pending">' + esc(dL("pending-badge", "Pending")) + "</span>" : "") +
        "<span>· " + fmtTime(c.created_at) + "</span>" +
        (c.geo_region ? '<span class="vc-geo">' + esc(c.geo_region) + "</span>" : "") +
        (c.ip_masked ? '<span class="vc-ip-badge">' + esc(c.ip_masked) + "</span>" : "") +
        '</div><div class="vc-content volo-prose">' + bodyHTML + "</div>" +
        (acts.length ? '<div class="vc-actions">' + acts.join("") + "</div>" : "") + "</div>" + kids + "</div>";
    }

    function showBanner(txt, isErr) {
      if (!statusBanner) statusBanner = el.querySelector(".js-vc-status");
      if (!statusBanner) return;
      statusBanner.textContent = txt || "";
      statusBanner.classList.toggle("volo-banner--err", !!isErr);
      statusBanner.hidden = !txt;
    }

    function refreshList(reqPage, stepping) {
      if (reqPage) curPage = reqPage;
      var qs = "target_type=" + encU(tType) + "&target_id=" + encU(tID) + "&page=" + curPage + "&per_page=20";
      if (seekID && !stepping) qs += "&focus_comment=" + encU(seekID) + "&target_comment_id=" + encU(seekID);

      jReq("/api/v1/comments?" + qs).then(function (res) {
        if (!res.ok) throw 0;
        var d = res.d, box = el.querySelector(".js-vc-list");
        if (!box) return;
        var arr = d.comments || [];
        curPage = d.page || curPage || 1;
        totalPages = d.total_pages || 1;
        var totalCnt = typeof d.total_comments === "number" ? d.total_comments : countTree(arr);
        var titleEl = el.querySelector(".vc-head__title");
        if (titleEl) titleEl.textContent = dL("title", "Comments").replace(/\(\d+\)/, "(" + totalCnt + ")");
        var pager = totalPages > 1
          ? '<nav class="vc-pager" aria-label="Comments pagination"><button type="button" class="pagination__item js-vc-page" data-page="' + (curPage - 1) + '"' + (curPage <= 1 ? " disabled" : "") + ">" + esc(dL("prev", "←")) +
            '</button><span class="vc-pager__info">' + esc(dL("page", "{page}/{total}").replace("{page}", "" + curPage).replace("{total}", "" + totalPages)) +
            '</span><button type="button" class="pagination__item js-vc-page" data-page="' + (curPage + 1) + '"' + (curPage >= totalPages ? " disabled" : "") + ">" + esc(dL("next", "→")) + "</button></nav>"
          : "";
        box.innerHTML = (arr.length ? arr.map(function (c) { return renderItem(c, 0); }).join("") : (mode === "off" ? "" : '<div class="vc-empty">' + esc(dL("empty", "")) + "</div>")) + pager;
        if (statusBanner && statusBanner.classList.contains("volo-banner--err")) showBanner("", false);

        if (seekID) {
          if (!doc.getElementById("comment-" + seekID) && curPage < totalPages && seekSteps < 20) {
            seekSteps++; refreshList(curPage + 1, true); return;
          }
          seekID = "";
        }
        if (initialScrollPending) { initialScrollPending = false; requestAnimationFrame(focusHashComment); }
      }).catch(function () {
        var box = el.querySelector(".js-vc-list");
        if (box && !box.innerHTML) box.innerHTML = '<div class="vc-list-err">' + esc(netErrLbl) + "</div>";
        else showBanner(netErrLbl, true);
      });
    }

    el._stepToHash = function (cid) {
      if (!cid || totalPages <= 1) return;
      seekID = cid; seekSteps = 0; initialScrollPending = true;
      refreshList(1, false);
    };

    (mode === "off" ? Promise.resolve("") : loadPassport()).then(function (tok) {
      gpToken = tok;
      var prof = {};
      if (mode !== "off") { try { prof = JSON.parse(localStorage.getItem(PROF_KEY) || "{}") || {}; } catch (_) {} }
      var claimHTML = (mode !== "off" && isLogged && gpToken) ? '<button type="button" class="studio-icon-btn js-vc-claim">' + esc(dL("claim", "Claim")) + "</button>" : "";

      var topPrompt = "", drawerHTML = "", cancelLbl = dL("cancel", "Cancel");
      if (mode === "off") {
        topPrompt = '<div class="volo-banner vc-closed-banner">' + esc(dL("closed", "Comments are currently closed.")) + "</div>";
      } else if (mode === "registered_only" && !isLogged) {
        topPrompt = '<div class="volo-banner vc-regonly-banner"><span>' + esc(dL("regonly", "Sign in to comment")) + '</span> <a class="vc-login-link" href="' + esc(loginHref()) + '">' + esc(dL("login", "Sign In")) + " →</a></div>";
      } else {
        topPrompt = '<button type="button" class="vc-trigger-bar js-vc-open"><span>' + esc(dL("trigger", "Write…")) + '</span><span class="vc-trigger-bar__kbd">' + esc(dL("shortcut", "Ctrl+Enter")) + "</span></button>";
        var idHTML = isLogged
          ? '<div class="vc-identity"><img class="vc-identity__avatar" src="' + esc(viewerAvatar) + '" data-avatar-seed="' + esc(viewerID || viewerName) + '" alt="" width="28" height="28"><span>' + esc(viewerName) + "</span>" +
            (viewerRole === "admin" ? '<span class="vc-author-badge">' + esc(dL("author", "Author")) + "</span>" : "") + "</div>"
          : '<div class="vc-guest-chip js-vc-guest-chip"' + (prof.name ? "" : " hidden") + '><span class="js-vc-guest-label">' + esc(dL("guest-as", "{name}").replace("{name}", prof.name || "")) + '</span><span>·</span><button type="button" class="js-vc-toggle-guest">' + esc(dL("edit-profile", "Edit")) + "</button></div>";
        var gRow = isLogged ? "" : '<div class="vc-row js-vc-guest-row"' + (prof.name ? " hidden" : "") + ">" +
          '<input class="volo-input vc-input" name="author_name" required maxlength="48" placeholder="' + esc(dL("nick", "Name")) + '" value="' + esc(prof.name || "") + '">' +
          '<input class="volo-input vc-input" name="notify_email" type="email" placeholder="' + esc(dL("email", "Email")) + '" value="' + esc(prof.email || "") + '">' +
          '<input class="volo-input vc-input" name="author_url" type="url" placeholder="' + esc(dL("site", "Website")) + '" value="' + esc(prof.url || "") + '"></div>';
        var tools = [["bold", "B"], ["italic", "<em>I</em>"], ["code", "&lt;/&gt;"], ["quote", "❝"], ["link", "🔗"], ["fence", "{ }"]]
          .map(function (t) { return '<button type="button" class="vc-tool-btn" data-md-act="' + t[0] + '">' + t[1] + "</button>"; }).join("");

        drawerHTML = '<div class="vc-backdrop js-vc-backdrop" hidden></div><div class="volo-comment-composer js-vc-drawer" role="dialog" aria-modal="true" hidden><form class="vc-form js-vc-form">' +
          '<div class="vc-composer-top">' + idHTML + '<div class="vc-context-pill js-vc-ctx" hidden><span class="js-vc-ctx-text"></span><button type="button" class="js-vc-clear-ctx" aria-label="Clear">×</button></div>' +
          '<button type="button" class="studio-icon-btn studio-icon-btn--square js-vc-close" aria-label="Close">✕</button></div>' + gRow +
          '<input class="vc-hp" name="website_hp" tabindex="-1" autocomplete="off"><div><div class="vc-toolbar"><div class="vc-md-tools">' + tools + '</div><div class="vc-tabs">' +
          '<button type="button" class="vc-tab is-active" data-vc-tab="write">' + esc(dL("write", "Write")) + '</button><button type="button" class="vc-tab" data-vc-tab="preview">' + esc(dL("preview", "Preview")) + "</button></div></div>" +
          '<textarea class="vc-textarea js-vc-ta" name="content_md" required maxlength="4000" placeholder="' + esc(dL("ph", "")) + '"></textarea><div class="vc-preview volo-prose js-vc-preview" hidden></div></div>' +
          '<div class="vc-composer-foot"><div class="vc-composer-meta"><span class="js-vc-cnt">0 / 4000</span><span class="vc-msg js-vc-msg"></span></div><div class="vc-composer-btns">' +
          '<button type="button" class="studio-icon-btn js-vc-close">' + esc(cancelLbl) + '</button><button type="submit" class="volo-btn js-vc-submit">' + esc(dL("submit", "Post")) + "</button></div></div></form></div>";
      }

      var banModalHTML = csrfToken
        ? '<div class="vc-ban-modal js-vc-ban-modal" hidden><div class="vc-ban-modal__panel" role="dialog" aria-modal="true"><h4 class="vc-ban-modal__title js-vc-ban-title">' + esc(dL("ban-title", "Ban IP")) + '</h4><div class="vc-ban-durations">' +
          '<button type="button" class="vc-dur-btn" data-hours="24">' + esc(dL("ban-24h", "24h")) + '</button><button type="button" class="vc-dur-btn" data-hours="168">' + esc(dL("ban-7d", "7d")) + '</button><button type="button" class="vc-dur-btn is-active" data-hours="0">' + esc(dL("ban-forever", "Permanent")) + "</button></div>" +
          '<label class="vc-ban-check"><input type="checkbox" class="js-vc-ban-reject" checked><span>' + esc(dL("ban-reject", "")) + '</span></label><input type="text" class="volo-input vc-input js-vc-ban-note" maxlength="160" placeholder="' + esc(dL("ban-note", "")) + '">' +
          '<div class="vc-ban-foot"><button type="button" class="studio-icon-btn js-vc-ban-cancel">' + esc(cancelLbl) + '</button><button type="button" class="volo-btn vc-btn-danger js-vc-ban-ok">' + esc(dL("ban-confirm", "Confirm")) + "</button></div></div></div>"
        : "";

      el.innerHTML = '<div class="vc-head"><h3 class="vc-head__title">' + esc(dL("title", "Comments")) + "</h3>" + claimHTML + "</div>" + topPrompt +
        '<div class="js-vc-status volo-banner" hidden></div><div class="js-vc-list"></div>' + drawerHTML + banModalHTML;

      var q = function (s) { return el.querySelector(s); };
      var drawer = q(".js-vc-drawer"), backdrop = q(".js-vc-backdrop"), form = q(".js-vc-form"), ta = q(".js-vc-ta"), previewBox = q(".js-vc-preview");
      var cntEl = q(".js-vc-cnt"), ctxPill = q(".js-vc-ctx"), ctxText = q(".js-vc-ctx-text"), submitBtn = q(".js-vc-submit"), msgEl = q(".js-vc-msg"), banModal = q(".js-vc-ban-modal");
      statusBanner = q(".js-vc-status");
      var syncLock = function () { if (win.__voloSyncScrollLock) win.__voloSyncScrollLock(); };
      var submitLbl = dL("submit", "Post"), saveLbl = dL("save", "Save");

      refreshList(1, false);

      function confirmTap(btn) {
        if (btn.getAttribute("data-confirm") === "1") {
          clearTimeout(btn._cfTimer); btn.removeAttribute("data-confirm");
          if (btn._origTxt) btn.textContent = btn._origTxt;
          return true;
        }
        btn._origTxt = btn.textContent; btn.setAttribute("data-confirm", "1"); btn.textContent = dL("confirm", "Confirm?");
        btn._cfTimer = setTimeout(function () { btn.removeAttribute("data-confirm"); btn.textContent = btn._origTxt; }, 3000);
        return false;
      }

      function actReq(url, method, body, withCSRF, onOk) {
        jReq(url, method, body, withCSRF).then(function (res) {
          if (!res.ok) showBanner(res.d.error || netErrLbl, true); else onOk(res.d);
        }).catch(function () { showBanner(netErrLbl, true); });
      }

      function syncCtxUI() {
        if (!ctxPill || !ctxText || !submitBtn) return;
        ctxPill.hidden = !(editingID || replyParentID);
        if (editingID) ctxText.textContent = dL("editing", "Editing");
        else if (replyParentID) ctxText.textContent = dL("replying", "@{name}").replace("{name}", replyParentName);
        submitBtn.textContent = editingID ? saveLbl : submitLbl;
      }

      function setTab(which) {
        if (!ta || !previewBox) return;
        var isPrev = which === "preview";
        el.querySelectorAll("[data-vc-tab]").forEach(function (b) { b.classList.toggle("is-active", b.getAttribute("data-vc-tab") === which); });
        ta.hidden = isPrev; previewBox.hidden = !isPrev;
        if (isPrev) previewBox.innerHTML = renderPreviewMD(ta.value, dL("preview-empty", ""), copyLbl);
      }

      function toggleDrawer(open) {
        if (!drawer || !backdrop || (!open && drawer.hidden)) return;
        var pair = [backdrop, drawer];
        if (open) {
          pair.forEach(function (n) { n.hidden = false; n.classList.remove("is-closing"); });
          void drawer.offsetWidth;
          pair.forEach(function (n) { n.classList.add("is-open"); });
          syncLock(); setTab("write");
          setTimeout(function () { if (ta) ta.focus(); }, 30);
        } else {
          pair.forEach(function (n) { n.classList.remove("is-open"); n.classList.add("is-closing"); });
          syncLock();
          setTimeout(function () { pair.forEach(function (n) { n.classList.remove("is-closing"); n.hidden = true; }); syncLock(); }, 200);
        }
      }

      function setFormErr(txt) {
        if (msgEl) { msgEl.textContent = txt || ""; msgEl.classList.toggle("is-err", !!txt); }
      }

      if (ta && cntEl) {
        ta.addEventListener("input", function () { cntEl.textContent = (ta.value || "").length + " / 4000"; setFormErr(""); });
        ta.addEventListener("keydown", function (ev) {
          if ((ev.ctrlKey || ev.metaKey) && ev.key === "Enter" && form) {
            ev.preventDefault();
            if (form.requestSubmit) form.requestSubmit(); else if (submitBtn) submitBtn.click();
          }
        });
      }

      el._closeOverlays = function () {
        if (banModal && !banModal.hidden) { banModal.hidden = true; syncLock(); }
        else toggleDrawer(false);
      };

      if (form) {
        form.addEventListener("submit", function (ev) {
          ev.preventDefault();
          if (!submitBtn || !ta) return;
          var fd = new FormData(form), content = String(ta.value || "").trim();
          if (!content) return;
          if (!isLogged && !editingID && !String(fd.get("author_name") || "").trim()) {
            var gRow = q(".js-vc-guest-row"); if (gRow) gRow.hidden = false;
            var nIn = form.querySelector("input[name='author_name']"); if (nIn) nIn.focus();
            return;
          }
          submitBtn.disabled = true; setFormErr("");

          if (editingID) {
            jReq("/api/v1/comments/" + encU(editingID), "PATCH", { content_md: content }, false).then(function (res) {
              if (!res.ok) { setFormErr(res.d.error || netErrLbl); return; }
              editingID = ""; ta.value = ""; ta.dispatchEvent(new Event("input")); toggleDrawer(false); refreshList(curPage, false);
            }).catch(function () { setFormErr(netErrLbl); }).finally(function () { submitBtn.disabled = false; syncCtxUI(); });
            return;
          }

          var doPost = function (chal, sol) {
            var nameVal = String(fd.get("author_name") || "").trim(), emailVal = String(fd.get("notify_email") || "").trim(), urlVal = String(fd.get("author_url") || "").trim(), wasReply = !!replyParentID;
            jReq("/api/v1/comments", "POST", {
              target_type: tType, target_id: tID, parent_id: replyParentID, author_name: nameVal, notify_email: emailVal, author_url: urlVal,
              content_md: content, website_hp: fd.get("website_hp") || "", pow_challenge: chal, pow_solution: sol, env_fp: win.__voloEnvFP ? win.__voloEnvFP() : ""
            }, false).then(function (res) {
              if (!res.ok) { setFormErr(res.d.error || netErrLbl); return; }
              if (res.d.passport_token) { gpToken = res.d.passport_token; savePassport(gpToken); }
              if (!isLogged && nameVal) {
                try { localStorage.setItem(PROF_KEY, JSON.stringify({ name: nameVal, email: emailVal, url: urlVal })); } catch (_) {}
                var chip = q(".js-vc-guest-chip"), chipLbl = q(".js-vc-guest-label"), gRow = q(".js-vc-guest-row");
                if (chip && chipLbl) { chipLbl.textContent = dL("guest-as", "{name}").replace("{name}", nameVal); chip.hidden = false; }
                if (gRow) gRow.hidden = true;
              }
              ta.value = ""; replyParentID = replyParentName = ""; ta.dispatchEvent(new Event("input")); toggleDrawer(false);
              showBanner(res.d.comment && res.d.comment.status === "pending" ? dL("pending", "") : "", false);
              refreshList(wasReply ? curPage : 1, false);
            }).catch(function () { setFormErr(netErrLbl); }).finally(function () { submitBtn.disabled = false; syncCtxUI(); });
          };

          if (isLogged) doPost("", "");
          else {
            submitBtn.textContent = dL("pow", "…");
            fetch("/api/v1/pow/challenge").then(function (r) { if (!r.ok) throw 0; return r.json(); })
              .then(function (c) { return solvePoW(c.challenge, c.difficulty_bits).then(function (sol) { doPost(c.challenge, sol); }); })
              .catch(function () { submitBtn.disabled = false; syncCtxUI(); setFormErr(netErrLbl); });
          }
        });
      }

      el.addEventListener("click", function (ev) {
        var cl = function (s) { return ev.target.closest(s); };
        var refreshCur = function () { refreshList(curPage, false); };

        var cpBtn = cl(".js-vc-copy");
        if (cpBtn) {
          var cn = cpBtn.parentElement && cpBtn.parentElement.querySelector("pre code, pre");
          if (cn) copyStr(cn.textContent || "").then(function () {
            cpBtn.textContent = copiedLbl; cpBtn.classList.add("is-copied");
            clearTimeout(cpBtn._t); cpBtn._t = setTimeout(function () { cpBtn.textContent = copyLbl; cpBtn.classList.remove("is-copied"); }, 1600);
          }).catch(function () {});
          return;
        }
        if (cl(".js-vc-open")) { toggleDrawer(true); return; }
        if (cl(".js-vc-close, .js-vc-backdrop")) { toggleDrawer(false); return; }
        if (cl(".js-vc-clear-ctx")) {
          replyParentID = replyParentName = "";
          if (editingID && ta) ta.value = "";
          editingID = ""; if (ta) ta.dispatchEvent(new Event("input")); syncCtxUI();
          return;
        }
        if (cl(".js-vc-toggle-guest")) {
          var gRow = q(".js-vc-guest-row");
          if (gRow && !(gRow.hidden = !gRow.hidden)) { var ni = gRow.querySelector("input[name='author_name']"); if (ni) ni.focus(); }
          return;
        }
        var pBtn = cl(".js-vc-page");
        if (pBtn) {
          if (!pBtn.disabled) { refreshList(+pBtn.getAttribute("data-page") || 1, false); if (win.__voloScrollToEl) win.__voloScrollToEl(el, false); }
          return;
        }
        var smBtn = cl(".js-vc-show-more");
        if (smBtn) { if (smBtn.previousElementSibling) smBtn.previousElementSibling.hidden = false; smBtn.remove(); return; }
        var mdBtn = cl("[data-md-act]");
        if (mdBtn && ta) { setTab("write"); applyMdTool(ta, mdBtn.getAttribute("data-md-act")); return; }
        var tabBtn = cl("[data-vc-tab]");
        if (tabBtn) { setTab(tabBtn.getAttribute("data-vc-tab")); return; }
        var rBtn = cl(".js-vc-reply");
        if (rBtn) { editingID = ""; replyParentID = rBtn.getAttribute("data-id") || ""; replyParentName = rBtn.getAttribute("data-name") || ""; syncCtxUI(); toggleDrawer(true); return; }
        var eBtn = cl(".js-vc-edit");
        if (eBtn && ta) { replyParentID = replyParentName = ""; editingID = eBtn.getAttribute("data-id") || ""; ta.value = eBtn.getAttribute("data-md") || ""; ta.dispatchEvent(new Event("input")); syncCtxUI(); toggleDrawer(true); return; }
        var dBtn = cl(".js-vc-del");
        if (dBtn) { if (confirmTap(dBtn)) actReq("/api/v1/comments/" + encU(dBtn.getAttribute("data-id")), "DELETE", null, false, refreshCur); return; }
        var apBtn = cl(".js-vc-mod-approve");
        if (apBtn) { actReq("/api/admin/comments/" + encU(apBtn.getAttribute("data-id")), "PATCH", { status: "approved" }, true, refreshCur); return; }
        var mdDel = cl(".js-vc-mod-del");
        if (mdDel) { if (confirmTap(mdDel)) actReq("/api/admin/comments/" + encU(mdDel.getAttribute("data-id")), "DELETE", null, true, refreshCur); return; }
        var banBtn = cl(".js-vc-mod-ban");
        if (banBtn && banModal) {
          banTargetID = banBtn.getAttribute("data-id") || "";
          var ip = banBtn.getAttribute("data-ip") || "", bt = banModal.querySelector(".js-vc-ban-title"), bn = banModal.querySelector(".js-vc-ban-note");
          if (bt) bt.textContent = dL("ban-title", "Ban IP") + (ip ? " (" + ip + ")" : "");
          if (bn) bn.value = "";
          banModal.hidden = false; syncLock(); return;
        }
        var durBtn = cl(".vc-dur-btn");
        if (durBtn && banModal) {
          banHours = +durBtn.getAttribute("data-hours") || 0;
          banModal.querySelectorAll(".vc-dur-btn").forEach(function (b) { b.classList.toggle("is-active", b === durBtn); });
          return;
        }
        if (cl(".js-vc-ban-cancel") || (banModal && ev.target === banModal)) { if (banModal) { banModal.hidden = true; syncLock(); } return; }
        if (cl(".js-vc-ban-ok") && banModal && banTargetID) {
          var rej = banModal.querySelector(".js-vc-ban-reject"), noteIn = banModal.querySelector(".js-vc-ban-note");
          banModal.hidden = true; syncLock();
          actReq("/api/admin/comments/" + encU(banTargetID) + "/ban-ip", "POST", {
            note: noteIn ? noteIn.value.trim() : "", expires_hours: banHours, reject_comment: rej ? !!rej.checked : true
          }, true, refreshCur);
          return;
        }
        var cBtn = cl(".js-vc-claim");
        if (cBtn) actReq("/api/v1/reader/claim-guest-comments", "POST", null, false, function (d) { cBtn.textContent = d.message || "OK"; refreshCur(); });
      });
    });
  }

  doc.addEventListener("keydown", function (ev) {
    if (ev.key === "Escape") { var cur = doc.getElementById("volo-comments"); if (cur && cur._closeOverlays) cur._closeOverlays(); }
  });

  function focusHashComment() {
    var h = (win.location.hash || "").slice(1);
    if (!h) return;
    try { h = decodeURIComponent(h); } catch (_) {}
    if (h === "comments") h = "volo-comments";
    var isCmt = h.indexOf("comment-") === 0;
    if (h !== "volo-comments" && !isCmt) return;
    var node = doc.getElementById(h);
    if (!node) {
      if (isCmt) { var sec = doc.getElementById("volo-comments"); if (sec && sec._stepToHash) sec._stepToHash(h.slice(8)); }
      return;
    }
    var box = node.closest && node.closest(".js-vc-more-box[hidden]");
    if (box) { box.hidden = false; var b = box.nextElementSibling; if (b && b.classList.contains("js-vc-show-more")) b.remove(); }
    if (isCmt) {
      node.classList.add("is-target");
      clearTimeout(node._hlT); node._hlT = setTimeout(function () { node.classList.remove("is-target"); }, 3200);
    }
    if (win.__voloScrollToEl) win.__voloScrollToEl(node, isCmt);
    else node.scrollIntoView({ behavior: "smooth", block: "center" });
  }

  win.addEventListener("hashchange", focusHashComment);
  win.VoloCommentWidget = { mount: mount };
  if (doc.readyState === "loading") doc.addEventListener("DOMContentLoaded", mount); else mount();
})();
