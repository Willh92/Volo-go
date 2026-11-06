/* Volo-go Zero-Dependency Lightweight Comment Widget (< 10KB Gzipped)
 * Features: 3-Tier GuestPassport Self-Healing (Cookie + localStorage + IndexedDB),
 * Peppered EnvFP Header, WebWorker SHA-256 PoW + Honeypot, 2-Level Replies, 30m Self-Edit/Delete & Reader Claim.
 */
(function () {
  "use strict";

  var LS_KEY = "volo_gp_v1";
  var IDB_NAME = "volo_identity";
  var IDB_STORE = "kv";

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

  /* 3-Tier GuestPassport Resolution & Mutual Repair */
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

  /* SHA-256 Leading-Zero-Bits PoW Solver (Blob WebWorker + Main-Thread SubtleCrypto Fallback) */
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

  function mount() {
    var el = document.getElementById("volo-comments");
    if (!el || el.getAttribute("data-mounted") === "1") return;
    el.setAttribute("data-mounted", "1");

    var tType = el.getAttribute("data-target-type") || "article";
    var tID = el.getAttribute("data-target-id") || "";
    var isReader = el.getAttribute("data-is-logged") === "true";
    var mode = el.getAttribute("data-access-mode") || "all";
    var labels = {
      title: el.getAttribute("data-l-title") || "Comments",
      nick: el.getAttribute("data-l-nick") || "Name",
      email: el.getAttribute("data-l-email") || "Email (private)",
      site: el.getAttribute("data-l-site") || "Website",
      ph: el.getAttribute("data-l-ph") || "Write a comment...",
      submit: el.getAttribute("data-l-submit") || "Post",
      pow: el.getAttribute("data-l-pow") || "Verifying...",
      reply: el.getAttribute("data-l-reply") || "Reply",
      edit: el.getAttribute("data-l-edit") || "Edit",
      del: el.getAttribute("data-l-del") || "Delete",
      claim: el.getAttribute("data-l-claim") || "Claim historical guest comments"
    };

    var replyParentID = "";
    var replyParentName = "";
    var gpToken = "";

    function headers() {
      var h = { "Content-Type": "application/json" };
      if (gpToken) h["X-Volo-Guest-Passport"] = gpToken;
      if (window.__voloEnvFP) h["X-Volo-Env-FP"] = window.__voloEnvFP();
      return h;
    }

    function renderItem(c) {
      var at = c.reply_to_name ? '<span style="color:var(--volo-accent);margin-right:6px">@' + esc(c.reply_to_name) + "</span>" : "";
      var authorLink = c.author_url ? '<a href="' + esc(c.author_url) + '" rel="nofollow ugc noopener" target="_blank">' + esc(c.author_name) + "</a>" : "<strong>" + esc(c.author_name) + "</strong>";
      var badge = c.is_author ? ' <span class="volo-badge">Author</span>' : "";
      var geo = c.geo_region ? " · " + esc(c.geo_region) : "";
      var acts = '<button type="button" class="studio-icon-btn js-vc-reply" data-id="' + esc(c.id) + '" data-name="' + esc(c.author_name) + '" style="height:26px;font-size:.76rem">' + esc(labels.reply) + "</button>";
      if (c.can_edit) {
        acts += ' <button type="button" class="studio-icon-btn js-vc-edit" data-id="' + esc(c.id) + '" data-md="' + esc(c.content_md || "") + '" style="height:26px;font-size:.76rem">' + esc(labels.edit) + '</button>' +
          ' <button type="button" class="studio-icon-btn js-vc-del" data-id="' + esc(c.id) + '" style="height:26px;font-size:.76rem">' + esc(labels.del) + "</button>";
      }
      var kids = (c.replies || []).length ? '<div class="vc-replies">' + c.replies.map(renderItem).join("") + "</div>" : "";
      return '<div class="vc-item" id="comment-' + esc(c.id) + '">' +
        '<img class="vc-avatar" src="' + esc(c.avatar_url) + '" alt="" width="38" height="38" loading="lazy">' +
        '<div style="flex:1;min-width:0">' +
        '<div style="font-size:.86rem;color:var(--volo-muted)">' + authorLink + badge + " · " + fmtTime(c.created_at) + geo + "</div>" +
        '<div class="volo-prose" style="font-size:.95rem;margin:6px 0">' + at + (c.content_html || "") + "</div>" +
        "<div>" + acts + "</div>" + kids + "</div></div>";
    }

    function refreshList() {
      fetch("/api/v1/comments?target_type=" + encodeURIComponent(tType) + "&target_id=" + encodeURIComponent(tID), { headers: headers() })
        .then(function (r) { return r.ok ? r.json() : null; })
        .then(function (d) {
          var box = el.querySelector(".js-vc-list");
          if (!box || !d) return;
          box.innerHTML = (d.comments || []).map(renderItem).join("");
        });
    }

    loadPassport().then(function (tok) {
      gpToken = tok;
      if (mode === "off") return;
      var formHTML = "";
      if (mode === "registered_only" && !isReader) {
        formHTML = '<div class="volo-banner"><a href="/reader/login">' + esc(el.getAttribute("data-l-regonly") || "Sign in to comment") + "</a></div>";
      } else {
        var guestInputs = isReader ? "" :
          '<div class="vc-row">' +
          '<input class="volo-input" name="author_name" required maxlength="48" placeholder="' + esc(labels.nick) + '">' +
          '<input class="volo-input" name="notify_email" type="email" placeholder="' + esc(labels.email) + '">' +
          '<input class="volo-input" name="author_url" type="url" placeholder="' + esc(labels.site) + '">' +
          "</div>";
        var claimBar = (isReader && gpToken) ?
          '<div style="margin-bottom:10px"><button type="button" class="studio-icon-btn js-vc-claim">' + esc(labels.claim) + "</button></div>" : "";
        formHTML = claimBar +
          '<form class="vc-form js-vc-form">' +
          '<div class="js-vc-reply-hint" hidden style="font-size:.85rem;color:var(--volo-accent)"></div>' +
          guestInputs +
          '<input class="vc-hp" name="website_hp" tabindex="-1" autocomplete="off">' +
          '<textarea class="vc-textarea" name="content_md" required maxlength="4000" placeholder="' + esc(labels.ph) + '"></textarea>' +
          '<div style="display:flex;align-items:center;justify-content:space-between;gap:10px">' +
          '<span class="js-vc-msg" style="font-size:.85rem;color:var(--volo-muted)"></span>' +
          '<button type="submit" class="volo-btn js-vc-submit">' + esc(labels.submit) + "</button></div></form>";
      }
      el.innerHTML = '<h3 style="margin-top:0">' + esc(labels.title) + "</h3>" + formHTML + '<div class="js-vc-list"></div>';
      refreshList();

      var form = el.querySelector(".js-vc-form");
      if (form) {
        form.addEventListener("submit", function (ev) {
          ev.preventDefault();
          var btn = form.querySelector(".js-vc-submit");
          var msg = form.querySelector(".js-vc-msg");
          var fd = new FormData(form);
          btn.disabled = true;
          var doPost = function (chal, sol) {
            fetch("/api/v1/comments", {
              method: "POST",
              headers: headers(),
              body: JSON.stringify({
                target_type: tType,
                target_id: tID,
                parent_id: replyParentID,
                author_name: fd.get("author_name") || "",
                notify_email: fd.get("notify_email") || "",
                author_url: fd.get("author_url") || "",
                content_md: fd.get("content_md") || "",
                website_hp: fd.get("website_hp") || "",
                pow_challenge: chal,
                pow_solution: sol,
                env_fp: window.__voloEnvFP ? window.__voloEnvFP() : ""
              })
            }).then(function (r) { return r.json().then(function (d) { return { ok: r.ok, d: d }; }); })
              .then(function (res) {
                btn.disabled = false;
                btn.textContent = labels.submit;
                if (!res.ok) { if (msg) msg.textContent = (res.d && res.d.error) || "Failed"; return; }
                if (res.d && res.d.passport_token) { gpToken = res.d.passport_token; savePassport(gpToken); }
                form.querySelector("textarea").value = "";
                replyParentID = ""; replyParentName = "";
                var rh = form.querySelector(".js-vc-reply-hint");
                if (rh) rh.hidden = true;
                if (msg) msg.textContent = (res.d.comment && res.d.comment.status === "pending") ? (el.getAttribute("data-l-pending") || "Submitted for review") : "";
                refreshList();
              });
          };
          if (isReader) {
            doPost("", "");
          } else {
            btn.textContent = labels.pow;
            fetch("/api/v1/pow/challenge").then(function (r) { return r.json(); }).then(function (c) {
              return solvePoW(c.challenge, c.difficulty_bits).then(function (sol) { doPost(c.challenge, sol); });
            }).catch(function () { btn.disabled = false; btn.textContent = labels.submit; });
          }
        });
      }

      el.addEventListener("click", function (ev) {
        var rBtn = ev.target.closest(".js-vc-reply");
        if (rBtn && form) {
          replyParentID = rBtn.getAttribute("data-id") || "";
          replyParentName = rBtn.getAttribute("data-name") || "";
          var rh = form.querySelector(".js-vc-reply-hint");
          if (rh) { rh.hidden = false; rh.textContent = "@" + replyParentName; }
          form.querySelector("textarea").focus();
          return;
        }
        var dBtn = ev.target.closest(".js-vc-del");
        if (dBtn) {
          fetch("/api/v1/comments/" + encodeURIComponent(dBtn.getAttribute("data-id")), { method: "DELETE", headers: headers() })
            .then(function (r) { if (r.ok) refreshList(); });
          return;
        }
        var eBtn = ev.target.closest(".js-vc-edit");
        if (eBtn) {
          var cid = eBtn.getAttribute("data-id");
          var next = window.prompt(labels.edit, eBtn.getAttribute("data-md") || "");
          if (next && next.trim()) {
            fetch("/api/v1/comments/" + encodeURIComponent(cid), {
              method: "PATCH",
              headers: headers(),
              body: JSON.stringify({ content_md: next })
            }).then(function (r) { if (r.ok) refreshList(); });
          }
          return;
        }
        var cBtn = ev.target.closest(".js-vc-claim");
        if (cBtn) {
          fetch("/api/v1/reader/claim-guest-comments", { method: "POST", headers: headers() })
            .then(function (r) { return r.json(); })
            .then(function (d) { cBtn.textContent = (d && d.message) || "OK"; refreshList(); });
        }
      });
    });
  }

  window.VoloCommentWidget = { mount: mount };
  if (document.readyState !== "loading") mount();
})();
