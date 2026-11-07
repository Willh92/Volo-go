/* Volo-go Zero-Dependency Comment Widget (< 10KB Gzipped)
 * 3-Tier GuestPassport, EnvFP, WebWorker SHA-256 PoW, Slide-Up Markdown Composer, Guest Profile Memory & Admin Governance.
 */
(function () {
  "use strict";

  var LS_KEY = "volo_gp_v1";
  var PROF_KEY = "volo.comment.profile.v1";
  var IDB_NAME = "volo_identity";
  var IDB_STORE = "kv";

  if (!window.__voloAvatarErrBound) {
    window.__voloAvatarErrBound = true;
    document.addEventListener("error", function (ev) {
      var img = ev.target;
      if (!img || img.tagName !== "IMG" || !img.hasAttribute("data-avatar-seed")) return;
      if (img.dataset.fallbackApplied === "1") return;
      var seed = img.getAttribute("data-avatar-seed") || "guest";
      var fb = "/avatars/" + encodeURIComponent(seed) + ".svg";
      img.dataset.fallbackApplied = "1";
      if ((img.getAttribute("src") || "") !== fb) img.src = fb;
    }, true);
  }

  function idbOpen() {
    return new Promise(function (resolve) {
      if (!window.indexedDB) return resolve(null);
      var req = indexedDB.open(IDB_NAME, 1);
      req.onupgradeneeded = function () { req.result.createObjectStore(IDB_STORE); };
      req.onsuccess = function () { resolve(req.result); };
      req.onerror = function () { resolve(null); };
    });
  }

  function idbGet(key) {
    return idbOpen().then(function (db) {
      if (!db) return "";
      return new Promise(function (resolve) {
        try {
          var r = db.transaction(IDB_STORE, "readonly").objectStore(IDB_STORE).get(key);
          r.onsuccess = function () { resolve(r.result || ""); };
          r.onerror = function () { resolve(""); };
        } catch (_) { resolve(""); }
      });
    });
  }

  function idbSet(key, val) {
    return idbOpen().then(function (db) {
      if (!db || !val) return;
      try { db.transaction(IDB_STORE, "readwrite").objectStore(IDB_STORE).put(val, key); } catch (_) {}
    });
  }

  function loadPassport() {
    var ls = "";
    try { ls = localStorage.getItem(LS_KEY) || ""; } catch (_) {}
    return idbGet(LS_KEY).then(function (idbVal) {
      var tok = ls || idbVal || "";
      if (tok) {
        if (!ls) { try { localStorage.setItem(LS_KEY, tok); } catch (_) {} }
        if (!idbVal) idbSet(LS_KEY, tok);
        return tok;
      }
      return fetch("/api/v1/guest/passport", { credentials: "same-origin" })
        .then(function (r) { return r.ok ? r.json() : null; })
        .then(function (d) {
          var minted = (d && d.passport_token) || "";
          savePassport(minted);
          return minted;
        })
        .catch(function () { return ""; });
    });
  }

  function savePassport(tok) {
    if (!tok) return;
    try { localStorage.setItem(LS_KEY, tok); } catch (_) {}
    idbSet(LS_KEY, tok);
  }

  function readGuestProfile() {
    try { return JSON.parse(localStorage.getItem(PROF_KEY) || "{}") || {}; } catch (_) { return {}; }
  }

  function saveGuestProfile(p) {
    try { localStorage.setItem(PROF_KEY, JSON.stringify(p)); } catch (_) {}
  }

  function countZeroBits(bytes) {
    var total = 0;
    for (var i = 0; i < bytes.length; i++) {
      var v = bytes[i];
      if (v === 0) { total += 8; continue; }
      for (var b = 7; b >= 0; b--) {
        if ((v & (1 << b)) === 0) total++; else return total;
      }
    }
    return total;
  }

  function solvePoWMain(challenge, bits) {
    var enc = new TextEncoder();
    var idx = 0;
    function step() {
      var cand = String(idx++);
      return crypto.subtle.digest("SHA-256", enc.encode(challenge + ":" + cand)).then(function (buf) {
        if (countZeroBits(new Uint8Array(buf)) >= bits) return cand;
        return step();
      });
    }
    return step();
  }

  function solvePoW(challenge, bits) {
    if (window.Worker && window.Blob && window.URL) {
      return new Promise(function (resolve) {
        var src = "self.onmessage=async function(e){var c=e.data.c,b=e.data.b,enc=new TextEncoder();for(var i=0;;i++){var s=String(i),h=new Uint8Array(await crypto.subtle.digest('SHA-256',enc.encode(c+':'+s))),z=0;for(var j=0;j<h.length;j++){if(h[j]===0){z+=8;continue}for(var k=7;k>=0;k--){if((h[j]&(1<<k))===0)z++;else{j=99;break}}}if(z>=b){self.postMessage(s);return}}};";
        try {
          var blobURL = URL.createObjectURL(new Blob([src], { type: "application/javascript" }));
          var w = new Worker(blobURL);
          w.onmessage = function (ev) { w.terminate(); URL.revokeObjectURL(blobURL); resolve(ev.data); };
          w.onerror = function () { w.terminate(); URL.revokeObjectURL(blobURL); solvePoWMain(challenge, bits).then(resolve); };
          w.postMessage({ c: challenge, b: bits });
        } catch (_) {
          solvePoWMain(challenge, bits).then(resolve);
        }
      });
    }
    return solvePoWMain(challenge, bits);
  }

  function esc(s) {
    return String(s || "").replace(/[&<>"']/g, function (c) {
      return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c];
    });
  }

  function fmtTime(ms) {
    var d = new Date(ms);
    var p = function (n) { return n < 10 ? "0" + n : String(n); };
    return d.getFullYear() + "-" + p(d.getMonth() + 1) + "-" + p(d.getDate()) + " " + p(d.getHours()) + ":" + p(d.getMinutes());
  }

  function renderPreviewMD(md, emptyLabel) {
    var trimmed = String(md || "").trim();
    if (!trimmed) return '<div class="vc-empty">' + esc(emptyLabel) + "</div>";
    var blocks = [];
    var safe = esc(trimmed).replace(/```([a-zA-Z0-9_+-]*)\n([\s\S]*?)```/g, function (_, _lang, code) {
      var idx = blocks.push("<pre><code>" + code + "</code></pre>") - 1;
      return "\x00BLK" + idx + "\x00";
    });
    var html = safe.split(/\n{2,}/).map(function (para) {
      if (/^\x00BLK\d+\x00$/.test(para.trim())) return para.trim();
      var inline = para
        .replace(/`([^`\n]+)`/g, "<code>$1</code>")
        .replace(/\*\*([^*]+)\*\*/g, "<strong>$1</strong>")
        .replace(/\*([^*\n]+)\*/g, "<em>$1</em>")
        .replace(/\[([^\]]+)\]\((https?:\/\/[^\s)]+)\)/g, '<a href="$2" target="_blank" rel="nofollow ugc noopener">$1</a>');
      if (/^&gt;\s?/.test(inline)) {
        return "<blockquote>" + inline.replace(/^&gt;\s?/gm, "").replace(/\n/g, "<br>") + "</blockquote>";
      }
      return "<p>" + inline.replace(/\n/g, "<br>") + "</p>";
    }).join("");
    return html.replace(/\x00BLK(\d+)\x00/g, function (_, i) { return blocks[Number(i)] || ""; });
  }

  function applyMdTool(ta, act) {
    var s = ta.selectionStart || 0;
    var e = ta.selectionEnd || 0;
    var v = ta.value || "";
    var sel = v.slice(s, e);
    var rep = "";
    var curOffset = 0;
    if (act === "bold") { rep = "**" + (sel || "bold") + "**"; curOffset = sel ? rep.length : 2; }
    else if (act === "italic") { rep = "*" + (sel || "italic") + "*"; curOffset = sel ? rep.length : 1; }
    else if (act === "code") { rep = "`" + (sel || "code") + "`"; curOffset = sel ? rep.length : 1; }
    else if (act === "quote") { rep = (s > 0 && v.charAt(s - 1) !== "\n" ? "\n> " : "> ") + (sel || ""); curOffset = rep.length; }
    else if (act === "link") { rep = "[" + (sel || "link") + "](https://)"; curOffset = rep.length - 1; }
    else if (act === "fence") { rep = (s > 0 && v.charAt(s - 1) !== "\n" ? "\n```\n" : "```\n") + (sel || "") + "\n```"; curOffset = rep.length - 4; }
    ta.value = v.slice(0, s) + rep + v.slice(e);
    ta.focus();
    ta.setSelectionRange(s + curOffset, s + curOffset);
    ta.dispatchEvent(new Event("input"));
  }

  function mount() {
    var el = document.getElementById("volo-comments");
    if (!el || el.getAttribute("data-mounted") === "1") return;
    el.setAttribute("data-mounted", "1");

    var attr = function (k, d) { return el.getAttribute(k) || d || ""; };
    var tType = attr("data-target-type", "article");
    var tID = attr("data-target-id", "");
    var isLogged = attr("data-is-logged") === "true";
    var viewerID = attr("data-viewer-id");
    var viewerName = attr("data-viewer-name", "Reader");
    var viewerRole = attr("data-viewer-role");
    var viewerAvatar = attr("data-viewer-avatar") || ("/avatars/" + encodeURIComponent(viewerID || viewerName) + ".svg");
    var csrfToken = attr("data-csrf-token");
    var mode = attr("data-access-mode", "all");

    var L = {
      title: attr("data-l-title", "Comments"),
      trigger: attr("data-l-trigger", "Write a comment..."),
      nick: attr("data-l-nick", "Name"),
      email: attr("data-l-email", "Email"),
      site: attr("data-l-site", "Website"),
      ph: attr("data-l-ph", "Write a comment..."),
      submit: attr("data-l-submit", "Post"),
      save: attr("data-l-save", "Save"),
      cancel: attr("data-l-cancel", "Cancel"),
      pow: attr("data-l-pow", "Verifying..."),
      pending: attr("data-l-pending", "Submitted for review"),
      pendingBadge: attr("data-l-pending-badge", "Pending"),
      regonly: attr("data-l-regonly", "Sign in to comment"),
      reply: attr("data-l-reply", "Reply"),
      edit: attr("data-l-edit", "Edit"),
      del: attr("data-l-del", "Delete"),
      author: attr("data-l-author", "Author"),
      guestAs: attr("data-l-guest-as", "Commenting as {name}"),
      editProf: attr("data-l-edit-profile", "Edit profile"),
      write: attr("data-l-write", "Write"),
      preview: attr("data-l-preview", "Preview"),
      previewEmpty: attr("data-l-preview-empty", "Nothing to preview"),
      editing: attr("data-l-editing", "Editing comment"),
      replying: attr("data-l-replying", "Reply @{name}"),
      empty: attr("data-l-empty", "No comments yet."),
      shortcut: attr("data-l-shortcut", "Ctrl + Enter"),
      approve: attr("data-l-approve", "Approve"),
      ban: attr("data-l-ban", "Ban IP"),
      banTitle: attr("data-l-ban-title", "Ban Comment IP"),
      ban24h: attr("data-l-ban-24h", "24h"),
      ban7d: attr("data-l-ban-7d", "7d"),
      banForever: attr("data-l-ban-forever", "Permanent"),
      banReject: attr("data-l-ban-reject", "Also reject comment"),
      banNote: attr("data-l-ban-note", "Note"),
      banConfirm: attr("data-l-ban-confirm", "Confirm Ban"),
      claim: attr("data-l-claim", "Claim guest comments")
    };

    var gpToken = "";
    var replyParentID = "";
    var replyParentName = "";
    var editingID = "";
    var banTargetID = "";
    var banHours = 0;
    var canCompose = mode !== "off" && !(mode === "registered_only" && !isLogged);

    function headers(withCSRF) {
      var h = { "Content-Type": "application/json" };
      if (gpToken) h["X-Volo-Guest-Passport"] = gpToken;
      if (window.__voloEnvFP) h["X-Volo-Env-FP"] = window.__voloEnvFP();
      if (withCSRF && csrfToken) h["X-CSRF-Token"] = csrfToken;
      return h;
    }

    function countTree(items) {
      var n = 0;
      (items || []).forEach(function (c) { n += 1 + countTree(c.replies); });
      return n;
    }

    function renderItem(c) {
      var seed = c.avatar_seed || c.id || "guest";
      var av = c.avatar_url || ("/avatars/" + encodeURIComponent(seed) + ".svg");
      var at = c.reply_to_name ? '<span class="vc-reply-at">@' + esc(c.reply_to_name) + "</span>" : "";
      var who = c.author_url
        ? '<a href="' + esc(c.author_url) + '" rel="nofollow ugc noopener" target="_blank">' + esc(c.author_name) + "</a>"
        : "<strong>" + esc(c.author_name) + "</strong>";
      var authorBadge = c.is_author ? '<span class="vc-author-badge">' + esc(L.author) + "</span>" : "";
      var pendBadge = c.status === "pending" ? '<span class="vc-pending-badge">' + esc(L.pendingBadge) + "</span>" : "";
      var geo = c.geo_region ? '<span class="vc-geo">' + esc(c.geo_region) + "</span>" : "";
      var ip = c.ip_masked ? '<span class="vc-ip-badge">' + esc(c.ip_masked) + "</span>" : "";

      var acts = [];
      if (canCompose) {
        acts.push('<button type="button" class="vc-act-btn js-vc-reply" data-id="' + esc(c.id) + '" data-name="' + esc(c.author_name) + '">' + esc(L.reply) + "</button>");
      }
      if (c.can_edit) {
        acts.push('<button type="button" class="vc-act-btn js-vc-edit" data-id="' + esc(c.id) + '" data-md="' + esc(c.content_md || "") + '">' + esc(L.edit) + "</button>");
        acts.push('<button type="button" class="vc-act-btn vc-act-btn--danger js-vc-del" data-id="' + esc(c.id) + '">' + esc(L.del) + "</button>");
      }
      if (c.can_moderate && csrfToken) {
        if (c.status === "pending") {
          acts.push('<button type="button" class="vc-act-btn js-vc-mod-approve" data-id="' + esc(c.id) + '">' + esc(L.approve) + "</button>");
        }
        if (!c.can_edit) {
          acts.push('<button type="button" class="vc-act-btn vc-act-btn--danger js-vc-mod-del" data-id="' + esc(c.id) + '">' + esc(L.del) + "</button>");
        }
        if (c.can_ban_ip) {
          acts.push('<button type="button" class="vc-act-btn vc-act-btn--danger js-vc-mod-ban" data-id="' + esc(c.id) + '" data-ip="' + esc(c.ip_masked || "") + '">' + esc(L.ban) + "</button>");
        }
      }

      var kids = (c.replies || []).length ? '<div class="vc-replies">' + c.replies.map(renderItem).join("") + "</div>" : "";
      return '<div class="vc-item" id="comment-' + esc(c.id) + '">' +
        '<img class="vc-avatar" src="' + esc(av) + '" data-avatar-seed="' + esc(seed) + '" alt="" width="38" height="38" loading="lazy">' +
        '<div class="vc-body">' +
        '<div class="vc-meta">' + who + authorBadge + pendBadge + "<span>· " + fmtTime(c.created_at) + "</span>" + geo + ip + "</div>" +
        '<div class="vc-content volo-prose">' + at + (c.content_html || "") + "</div>" +
        (acts.length ? '<div class="vc-actions">' + acts.join("") + "</div>" : "") +
        kids + "</div></div>";
    }

    function refreshList() {
      fetch("/api/v1/comments?target_type=" + encodeURIComponent(tType) + "&target_id=" + encodeURIComponent(tID), { headers: headers(false) })
        .then(function (r) { return r.ok ? r.json() : null; })
        .then(function (d) {
          var box = el.querySelector(".js-vc-list");
          if (!box || !d) return;
          var arr = d.comments || [];
          var titleEl = el.querySelector(".vc-head__title");
          if (titleEl) {
            titleEl.textContent = L.title.replace(/\(\d+\)/, "(" + countTree(arr) + ")");
          }
          box.innerHTML = arr.length ? arr.map(renderItem).join("") : '<div class="vc-empty">' + esc(L.empty) + "</div>";
        });
    }

    loadPassport().then(function (tok) {
      gpToken = tok;
      if (mode === "off") return;

      var prof = readGuestProfile();
      var claimHTML = (isLogged && gpToken)
        ? '<button type="button" class="studio-icon-btn js-vc-claim">' + esc(L.claim) + "</button>"
        : "";

      var topPrompt = "";
      var drawerHTML = "";
      if (mode === "registered_only" && !isLogged) {
        topPrompt = '<div class="volo-banner"><a href="/reader/login">' + esc(L.regonly) + "</a></div>";
      } else {
        topPrompt = '<button type="button" class="vc-trigger-bar js-vc-open">' +
          "<span>" + esc(L.trigger) + "</span>" +
          '<span class="vc-trigger-bar__kbd">' + esc(L.shortcut) + "</span></button>";

        var identityHTML = isLogged
          ? '<div class="vc-identity"><img class="vc-identity__avatar" src="' + esc(viewerAvatar) + '" data-avatar-seed="' + esc(viewerID || viewerName) + '" alt="" width="28" height="28">' +
            "<span>" + esc(viewerName) + "</span>" +
            (viewerRole === "admin" ? '<span class="vc-author-badge">' + esc(L.author) + "</span>" : "") + "</div>"
          : '<div class="vc-guest-chip js-vc-guest-chip"' + (prof.name ? "" : " hidden") + ">" +
            '<span class="js-vc-guest-label">' + esc(L.guestAs.replace("{name}", prof.name || "")) + "</span>" +
            '<span>·</span><button type="button" class="js-vc-toggle-guest">' + esc(L.editProf) + "</button></div>";

        var guestRow = isLogged ? "" :
          '<div class="vc-row js-vc-guest-row"' + (prof.name ? " hidden" : "") + ">" +
          '<input class="volo-input" name="author_name" required maxlength="48" placeholder="' + esc(L.nick) + '" value="' + esc(prof.name || "") + '">' +
          '<input class="volo-input" name="notify_email" type="email" placeholder="' + esc(L.email) + '" value="' + esc(prof.email || "") + '">' +
          '<input class="volo-input" name="author_url" type="url" placeholder="' + esc(L.site) + '" value="' + esc(prof.url || "") + '">' +
          "</div>";

        drawerHTML = '<div class="vc-backdrop js-vc-backdrop" hidden></div>' +
          '<div class="volo-comment-composer js-vc-drawer" role="dialog" aria-modal="true" hidden>' +
          '<form class="vc-form js-vc-form">' +
          '<div class="vc-composer-top">' + identityHTML +
          '<div class="vc-context-pill js-vc-ctx" hidden><span class="js-vc-ctx-text"></span><button type="button" class="js-vc-clear-ctx" aria-label="Clear">×</button></div>' +
          '<button type="button" class="studio-icon-btn studio-icon-btn--square js-vc-close" aria-label="Close">✕</button></div>' +
          guestRow +
          '<input class="vc-hp" name="website_hp" tabindex="-1" autocomplete="off">' +
          "<div>" +
          '<div class="vc-toolbar"><div class="vc-md-tools">' +
          '<button type="button" class="vc-tool-btn" data-md-act="bold" title="Bold">B</button>' +
          '<button type="button" class="vc-tool-btn" data-md-act="italic" title="Italic"><em>I</em></button>' +
          '<button type="button" class="vc-tool-btn" data-md-act="code" title="Inline Code">&lt;/&gt;</button>' +
          '<button type="button" class="vc-tool-btn" data-md-act="quote" title="Quote">❝</button>' +
          '<button type="button" class="vc-tool-btn" data-md-act="link" title="Link">🔗</button>' +
          '<button type="button" class="vc-tool-btn" data-md-act="fence" title="Code Block">{ }</button>' +
          '</div><div class="vc-tabs">' +
          '<button type="button" class="vc-tab is-active" data-vc-tab="write">' + esc(L.write) + "</button>" +
          '<button type="button" class="vc-tab" data-vc-tab="preview">' + esc(L.preview) + "</button>" +
          "</div></div>" +
          '<textarea class="vc-textarea js-vc-ta" name="content_md" required maxlength="4000" placeholder="' + esc(L.ph) + '"></textarea>' +
          '<div class="vc-preview volo-prose js-vc-preview" hidden></div>' +
          "</div>" +
          '<div class="vc-composer-foot">' +
          '<div class="vc-composer-meta"><span class="js-vc-cnt">0 / 4000</span><span class="vc-msg js-vc-msg"></span></div>' +
          '<div class="vc-composer-btns">' +
          '<button type="button" class="studio-icon-btn js-vc-close">' + esc(L.cancel) + "</button>" +
          '<button type="submit" class="volo-btn js-vc-submit">' + esc(L.submit) + "</button>" +
          "</div></div></form></div>";
      }

      var banModalHTML = csrfToken
        ? '<div class="vc-ban-modal js-vc-ban-modal" hidden>' +
          '<div class="vc-ban-modal__panel" role="dialog" aria-modal="true">' +
          '<h4 class="vc-ban-modal__title js-vc-ban-title">' + esc(L.banTitle) + "</h4>" +
          '<div class="vc-ban-durations">' +
          '<button type="button" class="vc-dur-btn" data-hours="24">' + esc(L.ban24h) + "</button>" +
          '<button type="button" class="vc-dur-btn" data-hours="168">' + esc(L.ban7d) + "</button>" +
          '<button type="button" class="vc-dur-btn is-active" data-hours="0">' + esc(L.banForever) + "</button>" +
          "</div>" +
          '<label class="vc-ban-check"><input type="checkbox" class="js-vc-ban-reject" checked><span>' + esc(L.banReject) + "</span></label>" +
          '<input type="text" class="volo-input js-vc-ban-note" maxlength="160" placeholder="' + esc(L.banNote) + '">' +
          '<div class="vc-ban-foot">' +
          '<button type="button" class="studio-icon-btn js-vc-ban-cancel">' + esc(L.cancel) + "</button>" +
          '<button type="button" class="volo-btn vc-btn-danger js-vc-ban-ok">' + esc(L.banConfirm) + "</button>" +
          "</div></div></div>"
        : "";

      el.innerHTML = '<div class="vc-head"><h3 class="vc-head__title">' + esc(L.title) + "</h3>" + claimHTML + "</div>" +
        topPrompt + '<div class="js-vc-status volo-banner" hidden></div><div class="js-vc-list"></div>' + drawerHTML + banModalHTML;

      refreshList();

      var drawer = el.querySelector(".js-vc-drawer");
      var backdrop = el.querySelector(".js-vc-backdrop");
      var form = el.querySelector(".js-vc-form");
      var ta = el.querySelector(".js-vc-ta");
      var previewBox = el.querySelector(".js-vc-preview");
      var cntEl = el.querySelector(".js-vc-cnt");
      var ctxPill = el.querySelector(".js-vc-ctx");
      var ctxText = el.querySelector(".js-vc-ctx-text");
      var submitBtn = el.querySelector(".js-vc-submit");
      var msgEl = el.querySelector(".js-vc-msg");
      var statusBanner = el.querySelector(".js-vc-status");
      var banModal = el.querySelector(".js-vc-ban-modal");

      function syncCtxUI() {
        if (!ctxPill || !ctxText || !submitBtn) return;
        if (editingID) {
          ctxPill.hidden = false;
          ctxText.textContent = L.editing;
          submitBtn.textContent = L.save;
        } else if (replyParentID) {
          ctxPill.hidden = false;
          ctxText.textContent = L.replying.replace("{name}", replyParentName);
          submitBtn.textContent = L.submit;
        } else {
          ctxPill.hidden = true;
          submitBtn.textContent = L.submit;
        }
      }

      function setTab(which) {
        if (!ta || !previewBox) return;
        var isPrev = which === "preview";
        el.querySelectorAll("[data-vc-tab]").forEach(function (b) {
          b.classList.toggle("is-active", b.getAttribute("data-vc-tab") === which);
        });
        ta.hidden = isPrev;
        previewBox.hidden = !isPrev;
        if (isPrev) previewBox.innerHTML = renderPreviewMD(ta.value, L.previewEmpty);
      }

      function openDrawer() {
        if (!drawer || !backdrop) return;
        backdrop.hidden = false;
        drawer.hidden = false;
        backdrop.classList.remove("is-closing");
        drawer.classList.remove("is-closing");
        void drawer.offsetWidth;
        backdrop.classList.add("is-open");
        drawer.classList.add("is-open");
        setTab("write");
        setTimeout(function () { if (ta) ta.focus(); }, 30);
      }

      function closeDrawer() {
        if (!drawer || !backdrop || drawer.hidden) return;
        backdrop.classList.remove("is-open");
        drawer.classList.remove("is-open");
        backdrop.classList.add("is-closing");
        drawer.classList.add("is-closing");
        setTimeout(function () {
          backdrop.classList.remove("is-closing");
          drawer.classList.remove("is-closing");
          backdrop.hidden = true;
          drawer.hidden = true;
        }, 200);
      }

      function clearCtx() {
        replyParentID = "";
        replyParentName = "";
        if (editingID && ta) ta.value = "";
        editingID = "";
        if (ta) ta.dispatchEvent(new Event("input"));
        syncCtxUI();
      }

      if (ta && cntEl) {
        ta.addEventListener("input", function () {
          cntEl.textContent = (ta.value || "").length + " / 4000";
          if (msgEl) { msgEl.textContent = ""; msgEl.classList.remove("is-err"); }
        });
        ta.addEventListener("keydown", function (ev) {
          if ((ev.ctrlKey || ev.metaKey) && ev.key === "Enter" && form) {
            ev.preventDefault();
            if (form.requestSubmit) form.requestSubmit();
            else if (submitBtn) submitBtn.click();
          }
        });
      }

      el._closeOverlays = function () {
        if (banModal && !banModal.hidden) { banModal.hidden = true; return; }
        if (drawer && !drawer.hidden) closeDrawer();
      };

      if (form) {
        form.addEventListener("submit", function (ev) {
          ev.preventDefault();
          if (!submitBtn || !ta) return;
          var fd = new FormData(form);
          var content = String(ta.value || "").trim();
          if (!content) return;

          if (!isLogged && !editingID) {
            var gName = String(fd.get("author_name") || "").trim();
            if (!gName) {
              var gRow = el.querySelector(".js-vc-guest-row");
              if (gRow) gRow.hidden = false;
              var nIn = form.querySelector("input[name='author_name']");
              if (nIn) nIn.focus();
              return;
            }
          }

          submitBtn.disabled = true;
          if (msgEl) { msgEl.textContent = ""; msgEl.classList.remove("is-err"); }

          if (editingID) {
            fetch("/api/v1/comments/" + encodeURIComponent(editingID), {
              method: "PATCH",
              headers: headers(false),
              body: JSON.stringify({ content_md: content })
            }).then(function (r) {
              return r.json().catch(function () { return {}; }).then(function (d) { return { ok: r.ok, d: d }; });
            }).then(function (res) {
              submitBtn.disabled = false;
              if (!res.ok) {
                if (msgEl) { msgEl.textContent = (res.d && res.d.error) || "Failed"; msgEl.classList.add("is-err"); }
                return;
              }
              editingID = "";
              ta.value = "";
              ta.dispatchEvent(new Event("input"));
              syncCtxUI();
              closeDrawer();
              refreshList();
            });
            return;
          }

          var doPost = function (chal, sol) {
            var nameVal = String(fd.get("author_name") || "").trim();
            var emailVal = String(fd.get("notify_email") || "").trim();
            var urlVal = String(fd.get("author_url") || "").trim();
            fetch("/api/v1/comments", {
              method: "POST",
              headers: headers(false),
              body: JSON.stringify({
                target_type: tType,
                target_id: tID,
                parent_id: replyParentID,
                author_name: nameVal,
                notify_email: emailVal,
                author_url: urlVal,
                content_md: content,
                website_hp: fd.get("website_hp") || "",
                pow_challenge: chal,
                pow_solution: sol,
                env_fp: window.__voloEnvFP ? window.__voloEnvFP() : ""
              })
            }).then(function (r) {
              return r.json().then(function (d) { return { ok: r.ok, d: d }; });
            }).then(function (res) {
              submitBtn.disabled = false;
              syncCtxUI();
              if (!res.ok) {
                if (msgEl) { msgEl.textContent = (res.d && res.d.error) || "Failed"; msgEl.classList.add("is-err"); }
                return;
              }
              if (res.d && res.d.passport_token) { gpToken = res.d.passport_token; savePassport(gpToken); }
              if (!isLogged && nameVal) {
                saveGuestProfile({ name: nameVal, email: emailVal, url: urlVal });
                var chip = el.querySelector(".js-vc-guest-chip");
                var chipLbl = el.querySelector(".js-vc-guest-label");
                var gRow = el.querySelector(".js-vc-guest-row");
                if (chip && chipLbl) {
                  chipLbl.textContent = L.guestAs.replace("{name}", nameVal);
                  chip.hidden = false;
                }
                if (gRow) gRow.hidden = true;
              }
              ta.value = "";
              replyParentID = "";
              replyParentName = "";
              ta.dispatchEvent(new Event("input"));
              syncCtxUI();
              closeDrawer();
              if (statusBanner) {
                var isPend = res.d && res.d.comment && res.d.comment.status === "pending";
                statusBanner.textContent = isPend ? L.pending : "";
                statusBanner.hidden = !isPend;
              }
              refreshList();
            });
          };

          if (isLogged) {
            doPost("", "");
          } else {
            submitBtn.textContent = L.pow;
            fetch("/api/v1/pow/challenge")
              .then(function (r) { return r.json(); })
              .then(function (c) { return solvePoW(c.challenge, c.difficulty_bits).then(function (sol) { doPost(c.challenge, sol); }); })
              .catch(function () { submitBtn.disabled = false; syncCtxUI(); });
          }
        });
      }

      el.addEventListener("click", function (ev) {
        if (ev.target.closest(".js-vc-open")) {
          openDrawer();
          return;
        }
        if (ev.target.closest(".js-vc-close") || ev.target.closest(".js-vc-backdrop")) {
          closeDrawer();
          return;
        }
        if (ev.target.closest(".js-vc-clear-ctx")) {
          clearCtx();
          return;
        }
        if (ev.target.closest(".js-vc-toggle-guest")) {
          var gRow = el.querySelector(".js-vc-guest-row");
          if (gRow) {
            gRow.hidden = !gRow.hidden;
            if (!gRow.hidden) {
              var ni = gRow.querySelector("input[name='author_name']");
              if (ni) ni.focus();
            }
          }
          return;
        }

        var mdBtn = ev.target.closest("[data-md-act]");
        if (mdBtn && ta) {
          setTab("write");
          applyMdTool(ta, mdBtn.getAttribute("data-md-act"));
          return;
        }

        var tabBtn = ev.target.closest("[data-vc-tab]");
        if (tabBtn) {
          setTab(tabBtn.getAttribute("data-vc-tab"));
          return;
        }

        var rBtn = ev.target.closest(".js-vc-reply");
        if (rBtn) {
          editingID = "";
          replyParentID = rBtn.getAttribute("data-id") || "";
          replyParentName = rBtn.getAttribute("data-name") || "";
          syncCtxUI();
          openDrawer();
          return;
        }

        var eBtn = ev.target.closest(".js-vc-edit");
        if (eBtn && ta) {
          replyParentID = "";
          replyParentName = "";
          editingID = eBtn.getAttribute("data-id") || "";
          ta.value = eBtn.getAttribute("data-md") || "";
          ta.dispatchEvent(new Event("input"));
          syncCtxUI();
          openDrawer();
          return;
        }

        var dBtn = ev.target.closest(".js-vc-del");
        if (dBtn) {
          fetch("/api/v1/comments/" + encodeURIComponent(dBtn.getAttribute("data-id")), { method: "DELETE", headers: headers(false) })
            .then(function (r) { if (r.ok) refreshList(); });
          return;
        }

        var apBtn = ev.target.closest(".js-vc-mod-approve");
        if (apBtn) {
          fetch("/api/admin/comments/" + encodeURIComponent(apBtn.getAttribute("data-id")), {
            method: "PATCH",
            headers: headers(true),
            body: JSON.stringify({ status: "approved" })
          }).then(function (r) { if (r.ok) refreshList(); });
          return;
        }

        var mdDel = ev.target.closest(".js-vc-mod-del");
        if (mdDel) {
          fetch("/api/admin/comments/" + encodeURIComponent(mdDel.getAttribute("data-id")), {
            method: "DELETE",
            headers: headers(true)
          }).then(function (r) { if (r.ok) refreshList(); });
          return;
        }

        var banBtn = ev.target.closest(".js-vc-mod-ban");
        if (banBtn && banModal) {
          banTargetID = banBtn.getAttribute("data-id") || "";
          var ipTxt = banBtn.getAttribute("data-ip") || "";
          var bTitle = banModal.querySelector(".js-vc-ban-title");
          if (bTitle) bTitle.textContent = L.banTitle + (ipTxt ? " (" + ipTxt + ")" : "");
          banModal.hidden = false;
          return;
        }

        var durBtn = ev.target.closest(".vc-dur-btn");
        if (durBtn && banModal) {
          banHours = parseInt(durBtn.getAttribute("data-hours") || "0", 10) || 0;
          banModal.querySelectorAll(".vc-dur-btn").forEach(function (b) { b.classList.toggle("is-active", b === durBtn); });
          return;
        }

        if (ev.target.closest(".js-vc-ban-cancel") || (banModal && ev.target === banModal)) {
          if (banModal) banModal.hidden = true;
          return;
        }

        if (ev.target.closest(".js-vc-ban-ok") && banModal && banTargetID) {
          var rej = banModal.querySelector(".js-vc-ban-reject");
          var noteIn = banModal.querySelector(".js-vc-ban-note");
          fetch("/api/admin/comments/" + encodeURIComponent(banTargetID) + "/ban-ip", {
            method: "POST",
            headers: headers(true),
            body: JSON.stringify({
              note: noteIn ? noteIn.value.trim() : "",
              expires_hours: banHours,
              reject_comment: rej ? !!rej.checked : true
            })
          }).then(function (r) {
            banModal.hidden = true;
            if (r.ok) refreshList();
          });
          return;
        }

        var cBtn = ev.target.closest(".js-vc-claim");
        if (cBtn) {
          fetch("/api/v1/reader/claim-guest-comments", { method: "POST", headers: headers(false) })
            .then(function (r) { return r.json(); })
            .then(function (d) { cBtn.textContent = (d && d.message) || "OK"; refreshList(); });
        }
      });
    });
  }

  document.addEventListener("keydown", function (ev) {
    if (ev.key === "Escape") {
      var cur = document.getElementById("volo-comments");
      if (cur && cur._closeOverlays) cur._closeOverlays();
    }
  });

  window.VoloCommentWidget = { mount: mount };
  if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", mount);
  else mount();
})();
