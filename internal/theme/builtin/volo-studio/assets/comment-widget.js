/* Volo Comment Widget — Role Bounds, Rich Toolbar, Inline & Fullscreen Split Editor, Height Defense */
(() => {
  "use strict";
  const win = window, doc = document, encU = encodeURIComponent;
  const LS_KEY = "volo_gp_v1", PROF_KEY = "volo.comment.profile.v1", IDB_NAME = "volo_identity", IDB_STORE = "kv";
  const byId = (i) => doc.getElementById(i);
  const ga = (n, k) => n?.getAttribute(k) || "";
  const sa = (n, k, v) => n?.setAttribute(k, v);
  const qa = (n, s) => (n ? n.querySelectorAll(s) : []);
  const clsA = (n, ...c) => n?.classList.add(...c);
  const clsR = (n, ...c) => n?.classList.remove(...c);
  const clsT = (n, c, v) => n?.classList.toggle(c, v);
  const clsH = (n, c) => !!n?.classList.contains(c);
  const onEv = (n, e, f, c) => n?.addEventListener(e, f, c);
  const btnH = (cls, body, ex = "") => '<button type="button" class="' + cls + '"' + ex + ">" + body + "</button>";

  if (!win.__voloAvatarErrBound) {
    win.__voloAvatarErrBound = true;
    onEv(doc, "error", (ev) => {
      const img = ev.target;
      if (!img || img.tagName !== "IMG" || !img.hasAttribute("data-avatar-seed") || img.dataset.fallbackApplied === "1") return;
      const fb = "/avatars/" + encU(ga(img, "data-avatar-seed") || "guest") + ".svg";
      img.dataset.fallbackApplied = "1";
      if (ga(img, "src") !== fb) img.src = fb;
    }, true);
  }

  const idbOp = (mode, fn) => new Promise((res) => {
    if (!win.indexedDB) return res("");
    const r = indexedDB.open(IDB_NAME, 1);
    r.onupgradeneeded = () => r.result.createObjectStore(IDB_STORE);
    r.onsuccess = () => {
      const db = r.result, cls = () => { try { db.close(); } catch {} };
      try {
        const tx = db.transaction(IDB_STORE, mode);
        tx.oncomplete = tx.onerror = tx.onabort = cls;
        fn(tx.objectStore(IDB_STORE), res);
      } catch { cls(); res(""); }
    };
    r.onerror = () => res("");
  });

  const savePassport = (tok) => {
    if (!tok) return;
    try { localStorage.setItem(LS_KEY, tok); } catch {}
    idbOp("readwrite", (st, res) => (st.put(tok, LS_KEY), res("")));
  };
  const clearPassport = () => {
    try { localStorage.removeItem(LS_KEY); } catch {}
    idbOp("readwrite", (st, res) => (st.delete(LS_KEY), res("")));
  };
  const loadPassport = (allowMint) => {
    let ls = "";
    try { ls = localStorage.getItem(LS_KEY) || ""; } catch {}
    return idbOp("readonly", (st, res) => {
      const r = st.get(LS_KEY);
      r.onsuccess = () => res(r.result || "");
      r.onerror = () => res("");
    }).then((iv) => {
      const tok = ls || iv || "";
      if (tok) return (!ls || !iv) && savePassport(tok), tok;
      if (!allowMint) return "";
      return fetch("/api/v1/guest/passport", { credentials: "same-origin" })
        .then((r) => (r.ok ? r.json() : null))
        .then((d) => { const m = d?.passport_token || ""; savePassport(m); return m; })
        .catch(() => "");
    });
  };

  async function solvePoW(chal, bits) {
    const te = new TextEncoder();
    for (let i = 0; ; i++) {
      const s = "" + i, h = new Uint8Array(await crypto.subtle.digest("SHA-256", te.encode(chal + ":" + s)));
      let z = 0;
      for (let j = 0; j < h.length; j++) {
        if (!h[j]) { z += 8; continue; }
        for (let k = 7; k >= 0; k--) if (!(h[j] & (1 << k))) z++; else { j = 99; break; }
      }
      if (z >= bits) return s;
    }
  }

  const ESC = { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" };
  const esc = (s) => String(s || "").replace(/[&<>"']/g, (c) => ESC[c]);
  const isSafeHref = (raw) => {
    const u = String(raw || "").trim();
    return /^https?:\/\/[^\s"'<>]+$/i.test(u) || (u[0] === "/" && u[1] !== "/" && u[1] !== "\\");
  };
  const fmtTime = (ms) => {
    const d = new Date(ms), p = (n) => (n < 10 ? "0" + n : "" + n);
    return d.getFullYear() + "-" + p(d.getMonth() + 1) + "-" + p(d.getDate()) + " " + p(d.getHours()) + ":" + p(d.getMinutes());
  };
  const compactMD = (raw) => String(raw || "").replace(/\r\n?/g, "\n").replace(/\n{3,}/g, "\n\n").trim();
  const countStats = (raw) => {
    const c = compactMD(raw);
    return [c, c ? Array.from(c).length : 0, c ? c.split("\n").length : 0];
  };

  const wrapNode = (el, cls) => {
    const p = el.parentElement;
    if (!p || clsH(p, cls)) return null;
    const w = doc.createElement("div"); w.className = cls;
    p.insertBefore(w, el); w.appendChild(el); return w;
  };
  const enhanceDOM = (root, copyLbl) => {
    qa(root, "table").forEach((t) => wrapNode(t, "volo-table-scroll"));
    qa(root, "pre").forEach((pre) => {
      const w = wrapNode(pre, "vc-code-wrap");
      if (w) {
        const b = doc.createElement("button");
        b.type = "button"; b.className = "volo-copy-btn vc-code-copy js-vc-copy"; b.textContent = copyLbl;
        w.insertBefore(b, pre);
      }
    });
  };

  const mdPrev = (md, emptyLbl) => {
    const t = compactMD(md);
    return t ? "<p>" + esc(t).replace(/\n/g, "<br>") + "</p>" : '<div class="vc-empty">' + esc(emptyLbl) + "</div>";
  };

  const applyEdit = (ta, nextVal, s0, s1) => {
    ta.value = nextVal; ta.focus(); ta.setSelectionRange(s0, s1 ?? s0); ta.dispatchEvent(new Event("input"));
  };

  const mdTool = (ta, act) => {
    const s = ta.selectionStart || 0, e = ta.selectionEnd || 0, v = ta.value || "", sel = v.slice(s, e);
    const INL = { bold: ["**", "**", "bold"], italic: ["*", "*", "italic"], strike: ["~~", "~~", "text"], code: ["`", "`", "code"] }[act];
    if (INL) {
      const b = sel || INL[2], r = INL[0] + b + INL[1], st = s + INL[0].length;
      return applyEdit(ta, v.slice(0, s) + r + v.slice(e), sel ? s + r.length : st, sel ? s + r.length : st + b.length);
    }
    if (act === "link" || act === "image") {
      if (act === "link" && /^https?:\/\//i.test(sel.trim())) {
        const r = "[link](" + sel.trim() + ")";
        return applyEdit(ta, v.slice(0, s) + r + v.slice(e), s + 1, s + 5);
      }
      const pfx = act === "image" ? "![" : "[", lbl = sel || act, r = pfx + lbl + "](https://)", u = s + pfx.length + lbl.length + 2;
      return applyEdit(ta, v.slice(0, s) + r + v.slice(e), u, u + 8);
    }
    const PFX = { h3: ["### ", /^#{1,6}\s+/], quote: ["> ", /^>\s?/], ul: ["- ", /^[-*+]\s+/], ol: ["1. ", /^\d+\.\s+/] }[act];
    if (PFX) {
      const ls = v.lastIndexOf("\n", s - 1) + 1, leI = v.indexOf("\n", e), le = leI < 0 ? v.length : leI;
      const out = v.slice(ls, le).split("\n").map((ln, i) => PFX[1].test(ln) ? ln.replace(PFX[1], "") : (act === "ol" ? (i + 1) + ". " : PFX[0]) + ln).join("\n");
      return applyEdit(ta, v.slice(0, ls) + out + v.slice(le), ls + out.length);
    }
    const pre = s > 0 && v[s - 1] !== "\n" ? "\n" : "", post = e < v.length && v[e] !== "\n" ? "\n" : "";
    const BLK = { hr: [(s > 0 ? "\n\n" : "") + "---\n", 0], fence: [pre + "```\n" + sel + "\n```" + post, pre.length + 4 + sel.length], table: [pre + "| Col 1 | Col 2 |\n| --- | --- |\n|  |  |" + post, pre.length + 2] }[act];
    if (BLK) applyEdit(ta, v.slice(0, s) + BLK[0] + v.slice(e), s + (BLK[1] || BLK[0].length), act === "table" ? s + BLK[1] + 5 : undefined);
  };

  const rawHash = () => {
    const h = (win.location.hash || "").slice(1);
    try { return decodeURIComponent(h); } catch { return h; }
  };
  const hashCID = () => { const h = rawHash(); return h.startsWith("comment-") ? h.slice(8) : ""; };
  const copyStr = (txt) => navigator.clipboard?.writeText ? navigator.clipboard.writeText(txt) : Promise.reject();

  const TOOLS = [
    ["bold", "Bold", "<b>B</b>"], ["italic", "Italic", "<i>I</i>"], ["strike", "Strike", "<s>S</s>"], ["code", "Code", "&lt;/&gt;"], 0,
    ["h3", "H3", "H3"], ["quote", "Quote", "❝"], ["ul", "List", "•"], ["ol", "Ordered", "1."], ["hr", "Rule", "—"], 0,
    ["link", "Link", "🔗"], ["image", "Image", "🖼"], ["fence", "Code Block", "{ }"], ["table", "Table", "⊞"]
  ];

  function mount() {
    const el = byId("volo-comments");
    if (!el || ga(el, "data-mounted") === "1") return;
    sa(el, "data-mounted", "1");
    const rootEl = doc.documentElement, q = (s) => el.querySelector(s);
    const attr = (k, d) => ga(el, k) || d || "", dL = (k, d) => attr("data-l-" + k, d);
    const tType = attr("data-target-type", "article"), tID = attr("data-target-id"), isLogged = attr("data-is-logged") === "true";
    const viewerID = attr("data-viewer-id"), viewerName = attr("data-viewer-name", "Reader"), viewerRole = attr("data-viewer-role");
    const isStaff = viewerRole === "admin" || viewerRole === "editor", rawVA = attr("data-viewer-avatar");
    const vAvatar = isSafeHref(rawVA) ? rawVA : "/avatars/" + encU(viewerID || viewerName) + ".svg";
    const csrfToken = attr("data-csrf-token") || ga(doc.querySelector('meta[name="csrf-token"]'), "content");
    const mode = attr("data-comment-mode") || attr("data-access-mode", "all");
    const copyLbl = dL("copy", ga(rootEl, "data-i18n-copy") || "Copy"), copiedLbl = dL("copied", ga(rootEl, "data-i18n-copied") || "Copied");
    const netErrLbl = dL("net-err", "Request failed"), expandLbl = dL("expand", "Show more"), collapseLbl = dL("collapse", "Show less"), linesLbl = dL("lines", "lines");

    let minLen = Math.max(1, +attr("data-min-len", "2") || 2);
    let maxLen = !isLogged ? +attr("data-max-len-guest", "512") || 512 : isStaff ? +attr("data-max-len-staff", "4096") || 4096 : +attr("data-max-len-user", "1024") || 1024;
    let maxLines = !isLogged ? 16 : isStaff ? 64 : 24;
    let gpToken = "", repID = "", repName = "", editingID = "", banID = "", banHours = 0, claimedDone = false, viewerMuted = false;
    let curPage = 1, totPages = 1, isSubmitting = false, seekID = hashCID(), seekSteps = 0, initScroll = true, sBan = null, onBounds = null;
    const canCompose = mode !== "off" && !(mode === "registered_only" && !isLogged);

    const jReq = (url, method, body, withCSRF, signal) => {
      const h = { "Content-Type": "application/json" };
      if (gpToken) h["X-Volo-Guest-Passport"] = gpToken;
      if (win.__voloEnvFP) h["X-Volo-Env-FP"] = win.__voloEnvFP();
      if (withCSRF && csrfToken) h["X-CSRF-Token"] = csrfToken;
      const o = { method: method || "GET", headers: h, credentials: "same-origin" };
      if (body) o.body = JSON.stringify(body);
      if (signal) o.signal = signal;
      return fetch(url, o).then((r) => r.json().catch(() => ({})).then((d) => ({ ok: r.ok, d: d || {} })));
    };

    const countTree = (items) => (items || []).reduce((n, c) => n + 1 + countTree(c.replies), 0);
    const hasDesc = (list, id) => !!id && (list || []).some((x) => String(x.id) === id || hasDesc(x.replies, id));

    const checkTall = (b) => {
      if (!b || b.dataset.vcExpanded === "1" || b.querySelector(".volo-comment-shielded")) return;
      clsR(b, "is-collapsed");
      const nx = b.nextElementSibling;
      if (b.scrollHeight > 260) {
        clsA(b, "is-tall", "is-collapsed");
        if (!clsH(nx, "js-vc-expand-tall")) {
          const btn = doc.createElement("button");
          btn.type = "button"; btn.className = "volo-comment-expand-btn js-vc-expand-tall";
          sa(btn, "aria-expanded", "false"); btn.textContent = expandLbl;
          b.insertAdjacentElement("afterend", btn);
        }
      } else {
        clsR(b, "is-tall");
        if (clsH(nx, "js-vc-expand-tall")) nx.remove();
      }
    };

    const applyTall = (scope) => qa(scope, ".vc-content").forEach((b) => {
      enhanceDOM(b, copyLbl); checkTall(b);
      qa(b, "img").forEach((img) => {
        if (!img.complete && img.dataset.vcTall !== "1") {
          img.dataset.vcTall = "1"; onEv(img, "load", () => checkTall(b), { once: true });
        }
      });
    });

    const renderItem = (c, depth, parentAuthor) => {
      // Soft-deleted comments that still carry visible replies arrive as zero-PII placeholders
      // (is_deleted / status "deleted"): no author, link, badges, reply-at prefix or actions.
      const isDel = !!c.is_deleted || c.status === "deleted";
      const seed = c.avatar_seed || c.id || "guest", av = !isDel && isSafeHref(c.avatar_url) ? String(c.avatar_url).trim() : "/avatars/" + encU(seed) + ".svg";
      const pName = isDel ? "" : c.reply_to_name || (depth >= 1 ? parentAuthor : ""), at = pName ? '<span class="vc-reply-at">@' + esc(pName) + "</span>" : "";
      const rawU = !isDel && c.author_url ? String(c.author_url).trim() : "";
      const who = isDel ? "" : /^https?:\/\//i.test(rawU) ? '<a href="' + esc(rawU) + '" rel="nofollow ugc noopener" target="_blank">' + esc(c.author_name) + "</a>" : "<strong>" + esc(c.author_name) + "</strong>";
      const cid = esc(c.id), delLbl = dL("del", "Delete"), acts = [];
      const mkAct = (cls, ex, lbl) => btnH("vc-act-btn " + cls, esc(lbl), ' data-id="' + cid + '"' + ex);
      if (!isDel && canCompose && !viewerMuted) acts.push(mkAct("js-vc-reply", ' data-name="' + esc(c.author_name) + '"', dL("reply", "Reply")));
      if (!isDel && c.can_edit && !c.is_shielded && !viewerMuted) {
        acts.push(mkAct("js-vc-edit", ' data-md="' + esc(c.content_md || "") + '"', dL("edit", "Edit")), mkAct("vc-act-btn--danger js-vc-del", "", delLbl));
      }
      if (!isDel && c.can_moderate && csrfToken) {
        if (c.status === "pending") acts.push(mkAct("js-vc-mod-approve", "", dL("approve", "Approve")));
        if (!c.can_edit) acts.push(mkAct("vc-act-btn--danger js-vc-mod-del", "", delLbl));
        if (c.can_ban_ip) acts.push(mkAct("vc-act-btn--danger js-vc-mod-ban", ' data-ip="' + esc(c.ip_masked || "") + '"', dL("ban", "Ban IP")));
      }
      const reps = c.replies || [];
      let kids = "";
      if (reps.length) {
        const dN = (depth || 0) + 1, dA = ' data-depth="' + Math.min(dN, 3) + '"', mapR = (a) => a.map((r) => renderItem(r, dN, c.author_name || "")).join("");
        kids = !depth && reps.length > 3 && !hasDesc(reps.slice(3), hashCID())
          ? '<div class="vc-replies"' + dA + ">" + mapR(reps.slice(0, 3)) + '<div class="js-vc-more-box" hidden>' + mapR(reps.slice(3)) + "</div>" + btnH("vc-more-replies js-vc-show-more", esc(dL("more-replies", "+{count}").replace("{count}", "" + (reps.length - 3)))) + "</div>"
          : '<div class="vc-replies"' + dA + ">" + mapR(reps) + "</div>";
      }
      let bodyHTML = String(c.content_html || "");
      if (at) bodyHTML = /^\s*<p\b[^>]*>/i.test(bodyHTML) ? bodyHTML.replace(/^(\s*<p\b[^>]*>)/i, "$1" + at + " ") : at + " " + bodyHTML;
      return '<div class="vc-item' + (isDel ? " is-deleted" : "") + '" id="comment-' + cid + '"><img class="vc-avatar" src="' + esc(av) + '" data-avatar-seed="' + esc(seed) + '" alt="" width="38" height="38" loading="lazy"><div class="vc-body"><div class="vc-meta">' + who +
        (!isDel && c.is_author ? '<span class="vc-author-badge">' + esc(dL("author", "Author")) + "</span>" : "") +
        (!isDel && c.status === "pending" ? '<span class="vc-pending-badge vc-badge--pending">' + esc(dL("pending-badge", "Pending")) + "</span>" : "") +
        "<span>" + (isDel ? "" : "· ") + fmtTime(c.created_at) + "</span>" +
        (!isDel && c.geo_region ? '<span class="vc-geo">' + esc(c.geo_region) + "</span>" : "") +
        (!isDel && c.ip_masked ? '<span class="vc-ip-badge">' + esc(c.ip_masked) + "</span>" : "") +
        '</div><div class="vc-content vc-item-body volo-prose">' + bodyHTML + "</div>" +
        (acts.length ? '<div class="vc-actions">' + acts.join("") + "</div>" : "") +
        "</div>" + kids + "</div>";
    };

    const showBanner = (txt, isErr) => {
      if (!sBan) sBan = q(".js-vc-status");
      if (sBan) { sBan.textContent = txt || ""; clsT(sBan, "volo-banner--err", !!isErr); sBan.hidden = !txt; }
    };

    const refreshList = (reqPage, stepping) => {
      if (reqPage) curPage = reqPage;
      let qs = "target_type=" + encU(tType) + "&target_id=" + encU(tID) + "&page=" + curPage + "&per_page=20";
      if (seekID && !stepping) qs += "&focus_comment=" + encU(seekID) + "&target_comment_id=" + encU(seekID);
      jReq("/api/v1/comments?" + qs).then((res) => {
        if (!res.ok) throw 0;
        const d = res.d, box = q(".js-vc-list");
        if (d.clear_guest_passport) { gpToken = ""; clearPassport(); }
        if (d.comment_min_length > 0) minLen = d.comment_min_length;
        if (d.comment_max_length > 0) maxLen = d.comment_max_length;
        if (d.comment_max_lines > 0) maxLines = d.comment_max_lines;
        viewerMuted = !!d.viewer_muted;
        const openBtn = q(".js-vc-open");
        if (openBtn) openBtn.hidden = viewerMuted;
        if (viewerMuted && d.viewer_mute_notice) showBanner(d.viewer_mute_notice, true);
        onBounds?.();
        const cBtn = q(".js-vc-claim");
        if (cBtn && !claimedDone) {
          cBtn.hidden = !(viewerRole === "reader" && d.claimable_guest_count > 0);
          if (!cBtn.hidden) cBtn.textContent = dL("claim", "Claim").replace("{count}", d.claimable_guest_count);
        }
        if (!box) return;
        const arr = d.comments || [];
        curPage = d.page || curPage || 1; totPages = d.total_pages || 1;
        const titleEl = q(".vc-head__title");
        if (titleEl) titleEl.textContent = dL("title", "Comments").replace(/\(\d+\)/, "(" + (typeof d.total_comments === "number" ? d.total_comments : countTree(arr)) + ")");
        const pager = totPages > 1
          ? '<nav class="vc-pager" aria-label="Comments pagination">' +
            btnH("pagination__item js-vc-page", esc(dL("prev", "←")), ' data-page="' + (curPage - 1) + '"' + (curPage <= 1 ? " disabled" : "")) +
            '<span class="vc-pager__info">' + esc(dL("page", "{page}/{total}").replace("{page}", "" + curPage).replace("{total}", "" + totPages)) + "</span>" +
            btnH("pagination__item js-vc-page", esc(dL("next", "→")), ' data-page="' + (curPage + 1) + '"' + (curPage >= totPages ? " disabled" : "")) + "</nav>" : "";
        box.innerHTML = (arr.length ? arr.map((c) => renderItem(c, 0)).join("") : mode === "off" ? "" : '<div class="vc-empty">' + esc(dL("empty", "")) + "</div>") + pager;
        applyTall(box);
        if (!viewerMuted && clsH(sBan, "volo-banner--err")) showBanner("", false);
        if (seekID) {
          if (d.focus_found !== false && !byId("comment-" + seekID) && curPage < totPages && seekSteps < 20) {
            seekSteps++; refreshList(curPage + 1, true); return;
          }
          seekID = "";
        }
        if (initScroll) { initScroll = false; requestAnimationFrame(focusHash); }
      }).catch(() => {
        const box = q(".js-vc-list");
        if (box && !box.innerHTML) box.innerHTML = '<div class="vc-list-err">' + esc(netErrLbl) + "</div>";
        else showBanner(netErrLbl, true);
      });
    };

    el._stepToHash = (cid) => { if (cid && totPages > 1) { seekID = cid; seekSteps = 0; initScroll = true; refreshList(1, false); } };

    (isLogged ? (viewerRole === "reader" ? loadPassport(false) : (clearPassport(), Promise.resolve(""))) : mode === "off" ? Promise.resolve("") : loadPassport(true)).then((tok) => {
      gpToken = tok;
      let prof = {};
      if (mode !== "off") { try { prof = JSON.parse(localStorage.getItem(PROF_KEY) || "{}") || {}; } catch {} }
      const claimHTML = isLogged && viewerRole === "reader" ? btnH("studio-icon-btn js-vc-claim", esc(dL("claim", "Claim")), " hidden") : "";
      const cancelLbl = esc(dL("cancel", "Cancel"));
      let topPrompt = "", drwHTML = "";

      if (mode === "off") {
        topPrompt = '<div class="volo-banner vc-closed-banner">' + esc(dL("closed", "Comments are currently closed.")) + "</div>";
      } else if (mode === "registered_only" && !isLogged) {
        const p = (win.location.pathname || "/") + (win.location.search || "") + "#volo-comments";
        const lHref = p[0] !== "/" || p.startsWith("//") || p.startsWith("/\\") ? "/reader/login" : "/reader/login?next=" + encU(p);
        topPrompt = '<div class="volo-banner vc-regonly-banner"><span>' + esc(dL("regonly", "Sign in to comment")) + '</span> <a class="vc-login-link" href="' + esc(lHref) + '">' + esc(dL("login", "Sign In")) + " →</a></div>";
      } else {
        topPrompt = btnH("vc-trigger-bar js-vc-open", "<span>" + esc(dL("trigger", "Write…")) + '</span><span class="vc-trigger-bar__kbd">' + esc(dL("shortcut", "Ctrl+Enter")) + "</span>");
        const idHTML = isLogged
          ? '<div class="vc-identity"><img class="vc-identity__avatar" src="' + esc(vAvatar) + '" data-avatar-seed="' + esc(viewerID || viewerName) + '" alt="" width="28" height="28"><span>' + esc(viewerName) + "</span>" + (viewerRole === "admin" ? '<span class="vc-author-badge">' + esc(dL("author", "Author")) + "</span>" : "") + "</div>"
          : '<div class="vc-guest-chip js-vc-guest-chip"' + (prof.name ? "" : " hidden") + '><span class="js-vc-guest-label">' + esc(dL("guest-as", "{name}").replace("{name}", prof.name || "")) + "</span><span>·</span>" + btnH("js-vc-toggle-guest", esc(dL("edit-profile", "Edit"))) + "</div>";
        const gIn = (nm, tp, lk, fb, val, ex = "") => '<input class="volo-input vc-input" name="' + nm + '"' + (tp ? ' type="' + tp + '"' : "") + ex + ' placeholder="' + esc(dL(lk, fb)) + '" value="' + esc(val || "") + '">';
        const gRow = isLogged ? "" : '<div class="vc-row js-vc-guest-row"' + (prof.name ? " hidden" : "") + ">" + gIn("author_name", "", "nick", "Name", prof.name, ' required maxlength="48"') + gIn("notify_email", "email", "email", "Email", prof.email) + gIn("author_url", "url", "site", "Website", prof.url) + "</div>" +
          btnH("vc-id-summary js-vc-id-expand", '<span class="vc-id-summary__label js-vc-id-sum-label"></span><span class="vc-id-summary__act">' + esc(dL("edit-profile", "Edit")) + "</span>");
        const tools = TOOLS.map((t) => {
          if (!t) return '<span class="vc-tb-sep" aria-hidden="true"></span>';
          const tip = esc(dL("tb-" + (t[0] === "image" ? "image" : t[0]), t[1]));
          return btnH("vc-tool-btn", t[2], ' data-md-act="' + t[0] + '" title="' + tip + '" aria-label="' + tip + '"');
        }).join("");
        const fsLbl = esc(dL("fullscreen", "Fullscreen"));
        drwHTML = '<div class="vc-backdrop js-vc-backdrop" hidden></div><div class="volo-comment-composer js-vc-drawer" role="dialog" aria-modal="true" hidden><form class="vc-form js-vc-form"><div class="vc-composer-top"><div class="vc-composer-top__left">' + idHTML +
          '<div class="vc-context-pill js-vc-ctx" hidden><span class="js-vc-ctx-text"></span>' + btnH("js-vc-clear-ctx", "×", ' aria-label="Clear"') + '</div></div><div class="vc-composer-top__right">' +
          btnH("studio-icon-btn studio-icon-btn--square js-vc-fs", "⛶", ' aria-pressed="false" title="' + fsLbl + '" aria-label="' + fsLbl + '"') +
          btnH("studio-icon-btn studio-icon-btn--square js-vc-close", "✕", ' aria-label="' + cancelLbl + '"') + '</div></div><div class="vc-composer-body js-vc-body">' + gRow +
          '<input class="vc-hp" name="website_hp" tabindex="-1" autocomplete="off"><div class="vc-editor-shell"><div class="vc-toolbar"><div class="vc-md-tools" role="toolbar">' + tools + '</div><div class="vc-tabs" role="tablist">' +
          btnH("vc-tab is-active", esc(dL("write", "Write")), ' data-vc-tab="write"') + btnH("vc-tab vc-tab--split", esc(dL("split", "Split")), ' data-vc-tab="split"') + btnH("vc-tab", esc(dL("preview", "Preview")), ' data-vc-tab="preview"') + "</div></div>" +
          '<div class="vc-editor-panes js-vc-panes" data-pane-mode="write"><textarea class="vc-textarea js-vc-ta" name="content_md" required placeholder="' + esc(dL("ph", "")) + '"></textarea><div class="vc-preview volo-cw-preview volo-prose js-vc-preview" hidden></div></div></div></div>' +
          '<div class="vc-composer-foot"><div class="vc-composer-meta"><span class="vc-char-cnt js-vc-cnt">0 / ' + maxLen + '</span><span class="vc-msg js-vc-msg"></span></div><div class="vc-composer-btns">' +
          btnH("studio-icon-btn js-vc-close", cancelLbl) + '<button type="submit" class="volo-btn js-vc-submit">' + esc(dL("submit", "Post")) + "</button></div></div></form></div>";
      }

      const banHTML = csrfToken
        ? '<div class="vc-ban-modal js-vc-ban-modal" hidden><div class="vc-ban-modal__panel" role="dialog" aria-modal="true"><h4 class="vc-ban-modal__title js-vc-ban-title">' + esc(dL("ban-title", "Ban IP")) + '</h4><div class="vc-ban-durations">' +
          btnH("vc-dur-btn", esc(dL("ban-24h", "24h")), ' data-hours="24"') + btnH("vc-dur-btn", esc(dL("ban-7d", "7d")), ' data-hours="168"') + btnH("vc-dur-btn is-active", esc(dL("ban-forever", "Permanent")), ' data-hours="0"') + "</div>" +
          '<label class="vc-ban-check"><input type="checkbox" class="js-vc-ban-reject" checked><span>' + esc(dL("ban-reject", "")) + '</span></label><input type="text" class="volo-input vc-input js-vc-ban-note" maxlength="160" placeholder="' + esc(dL("ban-note", "")) + '">' +
          '<div class="vc-ban-foot">' + btnH("studio-icon-btn js-vc-ban-cancel", cancelLbl) + btnH("volo-btn vc-btn-danger js-vc-ban-ok", esc(dL("ban-confirm", "Confirm"))) + "</div></div></div>" : "";

      el.innerHTML = '<div class="vc-head"><h3 class="vc-head__title">' + esc(dL("title", "Comments")) + "</h3>" + claimHTML + "</div>" + topPrompt + '<div class="js-vc-status volo-banner" hidden></div><div class="js-vc-list"></div>' + drwHTML + banHTML;

      const drawer = q(".js-vc-drawer"), bdrop = q(".js-vc-backdrop"), form = q(".js-vc-form"), bodyEl = q(".js-vc-body"), ta = q(".js-vc-ta"), pBox = q(".js-vc-preview"), panes = q(".js-vc-panes");
      const cntEl = q(".js-vc-cnt"), ctxPill = q(".js-vc-ctx"), ctxText = q(".js-vc-ctx-text"), subBtn = q(".js-vc-submit"), msgEl = q(".js-vc-msg"), fsBtn = q(".js-vc-fs"), banModal = q(".js-vc-ban-modal");
      let curTab = "write", isFS = false, prevTimer = 0, prevCtrl = null;
      sBan = q(".js-vc-status");

      const syncLock = () => win.__voloSyncScrollLock?.();
      const gRowEl = q(".js-vc-guest-row"), idSumLbl = q(".js-vc-id-sum-label");
      const isNarrow = () => (win.innerWidth || 1024) < 768;
      const finePointer = () => { try { return !!win.matchMedia?.("(pointer: fine)").matches; } catch { return false; } };
      const focusNoScroll = (n) => { if (!n) return; try { n.focus({ preventScroll: true }); } catch { n.focus(); } };
      /* Keyboard state is committed only when the measured value is stable for two consecutive frames,
         which avoids is-kb-open flicker while iOS animates the keyboard / IME candidate bar. */
      let vvRAF = 0, kbState = false, kbPending = null, vvTop = -1, vvH = -1, vvKb = -1;
      // On narrow screens with the keyboard open, fold the guest identity row into a one-line summary
      // (unless one of its inputs is focused) so the textarea keeps the remaining space.
      const syncIdentity = () => {
        if (!drawer || !gRowEl) return;
        const ae = doc.activeElement, idFocus = !!(ae && gRowEl.contains(ae));
        const collapse = kbState && !idFocus && !gRowEl.hidden && isNarrow();
        if (collapse && idSumLbl) {
          const nm = String(gRowEl.querySelector("input[name='author_name']")?.value || "").trim();
          idSumLbl.textContent = nm ? dL("guest-as", "{name}").replace("{name}", nm) : dL("nick", "Name");
        }
        clsT(drawer, "is-identity-collapsed", collapse);
      };
      const focusIdentity = () => {
        if (!gRowEl) return;
        gRowEl.hidden = false; clsR(drawer, "is-identity-collapsed");
        gRowEl.querySelector("input[name='author_name']")?.focus();
      };
      const setKb = (v) => {
        if (kbState === v) return;
        kbState = v; clsT(drawer, "is-kb-open", v); syncIdentity();
        if (v && doc.activeElement === ta && bodyEl) bodyEl.scrollTop = bodyEl.scrollHeight;
      };
      const clrVV = () => {
        if (vvRAF) { cancelAnimationFrame(vvRAF); vvRAF = 0; }
        kbPending = null; kbState = false; vvTop = vvH = vvKb = -1;
        if (drawer) {
          ["--vc-vv-top", "--vc-vv-h", "--vc-kb"].forEach((p) => drawer.style.removeProperty(p));
          clsR(drawer, "is-kb-open", "has-vv", "is-identity-collapsed");
        }
      };
      /* Geometry is exposed only through CSS custom properties: the sheet follows the keyboard with
         translateY(-kb) and fullscreen is pinned with translateY(vv.offsetTop), so per-frame visualViewport
         scrolls never touch top/bottom; height only changes when the visual viewport is resized. */
      const syncVV = () => {
        if (vvRAF) { cancelAnimationFrame(vvRAF); vvRAF = 0; }
        const vv = win.visualViewport;
        if (!drawer || drawer.hidden || !vv) return;
        // Pinch-zoom also shrinks the visual viewport; don't mistake it for a keyboard — fall back to static CSS.
        if ((vv.scale || 1) > 1.05) { clsR(drawer, "has-vv"); return; }
        const ih = win.innerHeight || rootEl.clientHeight || 0;
        const top = Math.max(0, Math.round(vv.offsetTop)), h = Math.round(vv.height);
        const kb = Math.max(0, Math.round(ih - vv.height - vv.offsetTop));
        const kbOpen = drawer.contains(doc.activeElement) && (kb > 80 || (isNarrow() && vv.height < 480));
        const ds = drawer.style;
        if (top !== vvTop) ds.setProperty("--vc-vv-top", (vvTop = top) + "px");
        if (h !== vvH) ds.setProperty("--vc-vv-h", (vvH = h) + "px");
        if (kb !== vvKb) ds.setProperty("--vc-kb", (vvKb = kb) + "px");
        clsA(drawer, "has-vv");
        if (kbOpen === kbState) kbPending = null;
        else if (kbPending === kbOpen) { kbPending = null; setKb(kbOpen); }
        else { kbPending = kbOpen; vvRAF = requestAnimationFrame(syncVV); }
      };
      const scheduleVV = () => { if (!vvRAF) vvRAF = requestAnimationFrame(syncVV); };
      onEv(win.visualViewport, "resize", scheduleVV);
      onEv(win.visualViewport, "scroll", scheduleVV);

      const submitLbl = dL("submit", "Post"), saveLbl = dL("save", "Save");
      const syncCounter = () => {
        if (!ta || !cntEl) return;
        const [, len, lines] = countStats(ta.value), over = len > maxLen || lines > maxLines;
        cntEl.textContent = len + " / " + maxLen + (lines > 1 || lines > maxLines ? " · " + lines + "/" + maxLines + " " + linesLbl : "");
        clsT(cntEl, "is-over", over);
        if (subBtn && !isSubmitting) subBtn.disabled = over;
      };
      onBounds = syncCounter;

      const updatePreview = (imm) => {
        if (!ta || !pBox || curTab === "write") return;
        clearTimeout(prevTimer); prevCtrl?.abort(); prevCtrl = null;
        const raw = ta.value || "", emptyLbl = dL("preview-empty", ""), emptyHTML = '<div class="vc-empty">' + esc(emptyLbl) + "</div>";
        if (!compactMD(raw)) { pBox.innerHTML = emptyHTML; return; }
        const run = () => {
          prevCtrl = win.AbortController ? new AbortController() : null;
          jReq("/api/v1/comments/preview", "POST", { content_md: raw }, false, prevCtrl?.signal).then((res) => {
            pBox.innerHTML = res.ok && typeof res.d.html === "string" ? res.d.html || emptyHTML : mdPrev(raw, emptyLbl);
            enhanceDOM(pBox, copyLbl);
          }).catch((err) => {
            if (err?.name !== "AbortError") { pBox.innerHTML = mdPrev(raw, emptyLbl); enhanceDOM(pBox, copyLbl); }
          });
        };
        if (imm) run(); else prevTimer = setTimeout(run, 180);
      };

      const setTab = (which) => {
        if (!ta || !pBox || !panes) return;
        if (which === "split" && (!isFS || (win.innerWidth || 1024) < 768)) which = "preview";
        curTab = panes.dataset.paneMode = which;
        qa(el, "[data-vc-tab]").forEach((b) => clsT(b, "is-active", b.dataset.vcTab === which));
        ta.hidden = which === "preview"; pBox.hidden = which === "write";
        if (which !== "write") updatePreview(true);
      };

      const setFullscreen = (next) => {
        if (!drawer || isFS === next) return;
        // Snapshot caret so it survives the relayout (switching tabs / geometry can drop focus on mobile).
        const hadFocus = !!ta && doc.activeElement === ta, selS = ta?.selectionStart ?? 0, selE = ta?.selectionEnd ?? 0;
        isFS = next; clsT(drawer, "is-fullscreen", isFS);
        if (fsBtn) {
          const lbl = dL(isFS ? "exit-fullscreen" : "fullscreen", isFS ? "Exit Fullscreen" : "Fullscreen");
          sa(fsBtn, "aria-pressed", "" + isFS); fsBtn.title = lbl; sa(fsBtn, "aria-label", lbl);
          fsBtn.textContent = isFS ? "❐" : "⛶";
        }
        if (bdrop) { bdrop.hidden = false; clsR(bdrop, "is-closing"); clsA(bdrop, "is-open"); }
        if (isFS) {
          if ((win.innerWidth || 1024) >= 768 && curTab === "write") setTab("split");
        } else if (curTab === "split") {
          setTab("write");
        }
        syncVV(); syncLock();
        if (hadFocus && ta && !ta.hidden) {
          // Focus normally never leaves (toolbar presses are cancelled); if it did, put it and the caret back
          // without scrolling. Untouched focus is left alone — re-focusing makes iOS scroll/jump.
          if (doc.activeElement !== ta) {
            focusNoScroll(ta);
            try { ta.setSelectionRange(selS, selE); } catch {}
          }
          requestAnimationFrame(() => { if (doc.activeElement === ta && clsH(drawer, "is-kb-open") && bodyEl) bodyEl.scrollTop = bodyEl.scrollHeight; });
          return;
        }
        // Not focused: only auto-focus with a fine pointer — on touch devices focus() would pop the keyboard.
        if (ta && !ta.hidden && doc.activeElement !== ta && finePointer()) focusNoScroll(ta);
      };

      onEv(win, "resize", () => {
        if (isFS && curTab === "split" && (win.innerWidth || 1024) < 768) setTab("write");
        scheduleVV(); syncIdentity();
      });

      let lastTriggerEl = null, lastBanTriggerEl = null;
      const trapModalTab = (box, ev) => {
        if (!box || box.hidden || ev.key !== "Tab" || ev.defaultPrevented || ev.ctrlKey || ev.metaKey || ev.altKey) return;
        const nodes = qa(box, 'button:not([disabled]), [href], input:not([disabled]):not([tabindex="-1"]):not([type="hidden"]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])')
          .filter((n) => !n.hidden && !n.closest("[hidden]") && n.offsetParent !== null);
        if (!nodes.length) return;
        const first = nodes[0], last = nodes[nodes.length - 1], ae = doc.activeElement;
        if (ev.shiftKey) {
          if (ae === first || !box.contains(ae)) { ev.preventDefault(); focusNoScroll(last); }
        } else if (ae === last || !box.contains(ae)) {
          ev.preventDefault(); focusNoScroll(first);
        }
      };
      if (drawer) onEv(drawer, "keydown", (ev) => trapModalTab(drawer, ev));
      if (banModal) onEv(banModal, "keydown", (ev) => trapModalTab(banModal, ev));

      const closeBanModal = (restoreFocus = true) => {
        if (!banModal || banModal.hidden) return;
        banModal.hidden = true;
        syncLock();
        const prev = lastBanTriggerEl;
        lastBanTriggerEl = null;
        if (restoreFocus && prev && prev.isConnected) focusNoScroll(prev);
      };

      const openComposer = (triggerNode) => {
        if (!drawer || viewerMuted || !bdrop) return;
        if (drawer.hidden) {
          const cand = triggerNode || doc.activeElement;
          lastTriggerEl = cand && cand !== doc.body && cand !== rootEl ? cand : q(".js-vc-open");
        }
        if (bdrop.parentElement !== el) el.appendChild(bdrop);
        if (drawer.parentElement !== el) el.appendChild(drawer);
        drawer.hidden = false; clsR(drawer, "is-closing", "is-inline"); clsA(drawer, "is-open");
        sa(drawer, "aria-modal", "true");
        bdrop.hidden = false; clsR(bdrop, "is-closing"); clsA(bdrop, "is-open");
        syncVV(); syncLock();
        if (!isFS) setTab("write");
        setTimeout(() => { focusNoScroll(ta); syncVV(); }, 30);
      };

      const closeComposer = (restoreFocus = true) => {
        if (!drawer || drawer.hidden) return;
        if (isFS) setFullscreen(false);
        if (editingID && ta) { editingID = ta.value = ""; syncCounter(); syncCtx(); }
        clrVV();
        [bdrop, drawer].forEach((n) => n && (clsR(n, "is-open", "is-inline", "is-closing"), n.hidden = true));
        syncLock();
        const prev = lastTriggerEl;
        lastTriggerEl = null;
        if (restoreFocus && prev && prev.isConnected) {
          focusNoScroll(prev);
        } else if (restoreFocus) {
          const fb = q(".js-vc-open");
          if (fb && fb.isConnected) focusNoScroll(fb);
        }
      };

      refreshList(1, false);

      const cfTap = (btn) => {
        if (btn.dataset.confirm === "1") {
          clearTimeout(btn._cfT); delete btn.dataset.confirm;
          if (btn._orig) btn.textContent = btn._orig;
          return true;
        }
        btn._orig = btn.textContent; btn.dataset.confirm = "1"; btn.textContent = dL("confirm", "Confirm?");
        btn._cfT = setTimeout(() => { delete btn.dataset.confirm; btn.textContent = btn._orig; }, 3000);
        return false;
      };
      const actReq = (u, m, b, c, ok) => jReq(u, m, b, c).then((r) => (!r.ok ? showBanner(r.d.error || netErrLbl, true) : ok(r.d))).catch(() => showBanner(netErrLbl, true));
      const syncCtx = () => {
        if (!ctxPill || !ctxText || !subBtn) return;
        ctxPill.hidden = !(editingID || repID);
        ctxText.textContent = editingID ? dL("editing", "Editing") : repID ? dL("replying", "@{name}").replace("{name}", repName) : "";
        subBtn.textContent = editingID ? saveLbl : submitLbl;
      };
      const setErr = (t) => msgEl && (msgEl.textContent = t || "", clsT(msgEl, "is-err", !!t));

      onEv(form, "focusin", () => {
        syncIdentity();
        setTimeout(() => {
          syncVV();
          if (doc.activeElement === ta && clsH(drawer, "is-kb-open") && bodyEl) bodyEl.scrollTop = bodyEl.scrollHeight;
        }, 60);
      });
      onEv(form, "focusout", () => setTimeout(() => { scheduleVV(); syncIdentity(); }, 80));

      /* Tapping toolbar / tab / fullscreen buttons must not blur the textarea: a blur collapses the
         mobile keyboard (and IME composition) only for it to re-open right after. Cancelling the
         pointerdown (touch/pen) and mousedown defaults keeps focus where it is; click still fires. */
      const keepFocusSel = ".js-vc-fs, [data-vc-tab], .vc-tool-btn, .js-vc-id-expand, .js-vc-clear-ctx";
      const keepFocus = (ev) => {
        if (!ta || doc.activeElement !== ta || (ev.button !== undefined && ev.button !== 0)) return;
        if (ev.target?.closest?.(keepFocusSel)) ev.preventDefault();
      };
      onEv(drawer, "pointerdown", (ev) => { if (ev.pointerType && ev.pointerType !== "mouse") keepFocus(ev); });
      onEv(drawer, "mousedown", keepFocus);

      if (ta && cntEl) {
        onEv(ta, "input", () => { syncCounter(); setErr(""); if (curTab !== "write") updatePreview(false); });
        onEv(ta, "scroll", () => {
          const mS = ta.scrollHeight - ta.clientHeight;
          if (curTab === "split" && pBox && mS > 0) pBox.scrollTop = (ta.scrollTop / mS) * (pBox.scrollHeight - pBox.clientHeight);
        });
        onEv(ta, "keydown", (ev) => {
          const mod = ev.ctrlKey || ev.metaKey, k = ev.key.toLowerCase();
          if (mod && ev.key === "Enter" && form) { ev.preventDefault(); form.requestSubmit ? form.requestSubmit() : subBtn?.click(); return; }
          if (mod && ev.shiftKey && k === "f") { ev.preventDefault(); setFullscreen(!isFS); return; }
          if (mod && !ev.shiftKey && !ev.altKey && (k === "b" || k === "i" || k === "e" || k === "k")) {
            ev.preventDefault(); mdTool(ta, { b: "bold", i: "italic", e: "code", k: "link" }[k]); return;
          }
          if (ev.key === "Tab" && !mod && !ev.altKey) {
            const s = ta.selectionStart || 0, e = ta.selectionEnd || 0;
            if (s !== e) { ev.preventDefault(); applyEdit(ta, ta.value.slice(0, s) + "  " + ta.value.slice(e), s + 2); }
          }
        });
      }

      el._handleEscape = () => {
        if (banModal && !banModal.hidden) { closeBanModal(true); return true; }
        if (isFS) { setFullscreen(false); return true; }
        if (drawer && !drawer.hidden) { closeComposer(true); return true; }
        return false;
      };
      el._closeOverlays = () => { closeBanModal(false); closeComposer(false); syncLock(); };

      onEv(form, "submit", (ev) => {
        ev.preventDefault();
        if (!subBtn || !ta || isSubmitting) return;
        const fd = new FormData(form), [content, len, lines] = countStats(ta.value);
        if (len < minLen || len > maxLen) { setErr(dL("err-len", "Comment must be {min}–{max} characters").replace("{min}", "" + minLen).replace("{max}", "" + maxLen)); ta.focus(); return; }
        if (lines > maxLines) { setErr(dL("err-lines", "Maximum {max} lines allowed").replace("{max}", "" + maxLines)); ta.focus(); return; }
        if (!isLogged && !editingID && !String(fd.get("author_name") || "").trim()) {
          focusIdentity(); return;
        }
        isSubmitting = subBtn.disabled = true; setErr("");
        const done = () => { isSubmitting = false; syncCounter(); syncCtx(); };
        if (editingID) {
          jReq("/api/v1/comments/" + encU(editingID), "PATCH", { content_md: content }, !!csrfToken).then((res) => {
            if (!res.ok) return setErr(res.d.error || netErrLbl);
            editingID = ta.value = ""; closeComposer(); refreshList(curPage, false);
          }).catch(() => setErr(netErrLbl)).finally(done);
          return;
        }
        const doPost = (chal, sol) => {
          const nameV = String(fd.get("author_name") || "").trim(), emailV = String(fd.get("notify_email") || "").trim(), urlV = String(fd.get("author_url") || "").trim(), wasRep = !!repID;
          jReq("/api/v1/comments", "POST", {
            target_type: tType, target_id: tID, parent_id: repID, author_name: nameV, notify_email: emailV, author_url: urlV,
            content_md: content, website_hp: fd.get("website_hp") || "", pow_challenge: chal, pow_solution: sol, env_fp: win.__voloEnvFP ? win.__voloEnvFP() : ""
          }, !!csrfToken).then((res) => {
            if (!res.ok) return setErr(res.d.error || netErrLbl);
            if (res.d.passport_token) { gpToken = res.d.passport_token; savePassport(gpToken); }
            if (!isLogged && nameV) {
              try { localStorage.setItem(PROF_KEY, JSON.stringify({ name: nameV, email: emailV, url: urlV })); } catch {}
              const chip = q(".js-vc-guest-chip"), chipLbl = q(".js-vc-guest-label"), gRow = q(".js-vc-guest-row");
              if (chip && chipLbl) { chipLbl.textContent = dL("guest-as", "{name}").replace("{name}", nameV); chip.hidden = false; }
              if (gRow) gRow.hidden = true;
            }
            ta.value = repID = repName = ""; closeComposer();
            showBanner(res.d.comment?.status === "pending" ? dL("pending", "") : "", false);
            refreshList(wasRep ? curPage : 1, false);
          }).catch(() => setErr(netErrLbl)).finally(done);
        };
        if (isLogged) doPost("", "");
        else {
          subBtn.textContent = dL("pow", "…");
          fetch("/api/v1/pow/challenge").then((r) => { if (!r.ok) throw 0; return r.json(); })
            .then((c) => solvePoW(c.challenge, c.difficulty_bits).then((sol) => doPost(c.challenge, sol)))
            .catch(() => { done(); setErr(netErrLbl); });
        }
      });

      onEv(el, "click", (ev) => {
        const cl = (s) => ev.target.closest(s), ref = () => refreshList(curPage, false);
        const expBtn = cl(".js-vc-expand-tall");
        if (expBtn) {
          const b = expBtn.previousElementSibling;
          if (clsH(b, "vc-content")) {
            const open = clsH(b, "is-collapsed");
            clsT(b, "is-collapsed", !open); b.dataset.vcExpanded = open ? "1" : "0";
            sa(expBtn, "aria-expanded", "" + open); expBtn.textContent = open ? collapseLbl : expandLbl;
          }
          return;
        }
        const cpBtn = cl(".js-vc-copy");
        if (cpBtn) {
          const cn = cpBtn.parentElement?.querySelector("pre code, pre");
          if (cn) copyStr(cn.textContent || "").then(() => {
            cpBtn.textContent = copiedLbl; clsA(cpBtn, "is-copied");
            clearTimeout(cpBtn._t); cpBtn._t = setTimeout(() => { cpBtn.textContent = copyLbl; clsR(cpBtn, "is-copied"); }, 1600);
          }).catch(() => {});
          return;
        }
        const openBtn = cl(".js-vc-open");
        if (openBtn) {
          if (editingID && ta) { ta.value = ""; syncCounter(); }
          repID = repName = editingID = ""; syncCtx(); openComposer(openBtn); return;
        }
        if (cl(".js-vc-fs")) { setFullscreen(!isFS); return; }
        if (cl(".js-vc-close")) { closeComposer(true); return; }
        if (cl(".js-vc-backdrop")) { if (isFS) setFullscreen(false); else closeComposer(true); return; }
        if (cl(".js-vc-clear-ctx")) {
          repID = repName = "";
          if (editingID && ta) ta.value = "";
          editingID = ""; ta?.dispatchEvent(new Event("input")); syncCtx();
          return;
        }
        if (cl(".js-vc-toggle-guest")) {
          const gRow = q(".js-vc-guest-row");
          if (gRow && !(gRow.hidden = !gRow.hidden)) focusIdentity();
          else syncIdentity();
          return;
        }
        if (cl(".js-vc-id-expand")) { focusIdentity(); return; }
        const pBtn = cl(".js-vc-page");
        if (pBtn) { if (!pBtn.disabled) { refreshList(+pBtn.dataset.page || 1, false); win.__voloScrollToEl?.(el, false); } return; }
        const smBtn = cl(".js-vc-show-more");
        if (smBtn) { const mb = smBtn.previousElementSibling; if (mb) { mb.hidden = false; applyTall(mb); } smBtn.remove(); return; }
        const mdBtn = cl("[data-md-act]");
        if (mdBtn && ta) { if (curTab === "preview") setTab("write"); mdTool(ta, mdBtn.dataset.mdAct); return; }
        const tabBtn = cl("[data-vc-tab]");
        if (tabBtn) { setTab(tabBtn.dataset.vcTab); return; }
        const rBtn = cl(".js-vc-reply");
        if (rBtn) {
          if (editingID && ta) { ta.value = ""; syncCounter(); }
          editingID = ""; repID = rBtn.dataset.id; repName = rBtn.dataset.name; syncCtx(); openComposer(rBtn); return;
        }
        const eBtn = cl(".js-vc-edit");
        if (eBtn && ta) { repID = repName = ""; editingID = eBtn.dataset.id; ta.value = eBtn.dataset.md || ""; syncCounter(); syncCtx(); openComposer(eBtn); return; }
        const dBtn = cl(".js-vc-del");
        if (dBtn) { if (cfTap(dBtn)) actReq("/api/v1/comments/" + encU(dBtn.dataset.id), "DELETE", null, !!csrfToken, ref); return; }
        const apBtn = cl(".js-vc-mod-approve");
        if (apBtn) { actReq("/api/admin/comments/" + encU(apBtn.dataset.id), "PATCH", { status: "approved" }, true, ref); return; }
        const mdDel = cl(".js-vc-mod-del");
        if (mdDel) { if (cfTap(mdDel)) actReq("/api/admin/comments/" + encU(mdDel.dataset.id), "DELETE", null, true, ref); return; }
        const banBtn = cl(".js-vc-mod-ban");
        if (banBtn && banModal) {
          lastBanTriggerEl = banBtn;
          banID = banBtn.dataset.id;
          const ip = banBtn.dataset.ip, bt = banModal.querySelector(".js-vc-ban-title"), bn = banModal.querySelector(".js-vc-ban-note");
          if (bt) bt.textContent = dL("ban-title", "Ban IP") + (ip ? " (" + ip + ")" : "");
          if (bn) bn.value = "";
          banModal.hidden = false; syncLock();
          setTimeout(() => focusNoScroll(bn || banModal.querySelector(".js-vc-ban-cancel")), 20);
          return;
        }
        const durBtn = cl(".vc-dur-btn");
        if (durBtn && banModal) {
          banHours = +durBtn.dataset.hours || 0;
          qa(banModal, ".vc-dur-btn").forEach((b) => clsT(b, "is-active", b === durBtn));
          return;
        }
        if (cl(".js-vc-ban-cancel") || (banModal && ev.target === banModal)) { closeBanModal(true); return; }
        if (cl(".js-vc-ban-ok") && banModal && banID) {
          const rej = banModal.querySelector(".js-vc-ban-reject"), noteIn = banModal.querySelector(".js-vc-ban-note");
          closeBanModal(false);
          actReq("/api/admin/comments/" + encU(banID) + "/ban-ip", "POST", { note: noteIn ? noteIn.value.trim() : "", expires_hours: banHours, reject_comment: rej ? !!rej.checked : true }, true, ref);
          return;
        }
        const cBtn = cl(".js-vc-claim");
        if (cBtn && !claimedDone) {
          actReq("/api/v1/reader/claim-guest-comments", "POST", null, !!csrfToken, (d) => {
            claimedDone = cBtn.disabled = true; gpToken = ""; clearPassport();
            cBtn.textContent = d.message || "OK";
            setTimeout(() => { cBtn.hidden = true; }, 1800); ref();
          });
        }
      });
    });
  }

  onEv(doc, "keydown", (ev) => {
    if (ev.key === "Escape") {
      const lb = byId("volo-lightbox");
      if (lb && !lb.hidden) return;
      if (byId("volo-comments")?._handleEscape?.()) ev.stopPropagation();
    }
  });

  function focusHash() {
    let h = rawHash();
    if (!h) return;
    if (h === "comments") h = "volo-comments";
    const isCmt = h.startsWith("comment-");
    if (h !== "volo-comments" && !isCmt) return;
    const node = byId(h);
    if (!node) { if (isCmt) byId("volo-comments")?._stepToHash?.(h.slice(8)); return; }
    const box = node.closest?.(".js-vc-more-box[hidden]");
    if (box) { box.hidden = false; const b = box.nextElementSibling; if (clsH(b, "js-vc-show-more")) b.remove(); }
    if (isCmt) {
      const b = node.querySelector?.(".vc-content.is-collapsed");
      if (b) { clsR(b, "is-collapsed"); b.dataset.vcExpanded = "1"; const eb = b.nextElementSibling; if (clsH(eb, "js-vc-expand-tall")) sa(eb, "aria-expanded", "true"); }
      clsA(node, "is-target"); clearTimeout(node._hlT); node._hlT = setTimeout(() => clsR(node, "is-target"), 3200);
    }
    if (win.__voloScrollToEl) win.__voloScrollToEl(node, isCmt); else node.scrollIntoView({ behavior: "smooth", block: "center" });
  }

  onEv(win, "hashchange", focusHash);
  win.VoloCommentWidget = { mount };
  if (doc.readyState === "loading") onEv(doc, "DOMContentLoaded", mount); else mount();
})();
