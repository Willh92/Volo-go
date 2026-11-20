/* Volo Comment Widget — Role Bounds, Rich Toolbar, Inline & Fullscreen Split Editor, Height Defense */
(() => {
  "use strict";
  const win = window, doc = document, encU = encodeURIComponent;
  const LS_KEY = "volo_gp_v1", PROF_KEY = "volo.comment.profile.v1", IDB_NAME = "volo_identity", IDB_STORE = "kv";
  const byId = (i) => doc.getElementById(i);
  const ce = (t, c) => { const n = doc.createElement(t); if (c) n.className = c; return n; };
  const ga = (n, k) => n?.getAttribute(k) || "";
  const sa = (n, k, v) => n?.setAttribute(k, v);
  const qa = (n, s) => (n ? [...n.querySelectorAll(s)] : []);
  const clsA = (n, ...c) => n?.classList.add(...c);
  const clsR = (n, ...c) => n?.classList.remove(...c);
  const clsT = (n, c, v) => n?.classList.toggle(c, v);
  const clsH = (n, c) => !!n?.classList.contains(c);
  const onEv = (n, e, f, c) => n?.addEventListener(e, f, c);
  const btnH = (c, b, x = "") => '<button type="button" class="' + c + '"' + x + ">" + b + "</button>";
  const dvH = (c, b = "", x = "") => '<div class="' + c + '"' + x + ">" + b + "</div>";
  const spH = (c, b) => '<span class="' + c + '">' + b + "</span>";
  const lsOp = (k, v) => { try { return v === undefined ? localStorage.getItem(k) : v === null ? localStorage.removeItem(k) : localStorage.setItem(k, v); } catch { return ""; } };

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
  const idbRW = (fn) => idbOp("readwrite", (st, res) => (fn(st), res("")));
  const savePassport = (tok) => { if (tok) { lsOp(LS_KEY, tok); idbRW((st) => st.put(tok, LS_KEY)); } };
  const clearPassport = () => { lsOp(LS_KEY, null); idbRW((st) => st.delete(LS_KEY)); };
  const loadPassport = (allowMint) => {
    const ls = lsOp(LS_KEY) || "";
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
      for (const b of h) if (!b) z += 8; else { z += Math.clz32(b) - 24; break; }
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
    const w = ce("div", cls);
    p.insertBefore(w, el); w.appendChild(el); return w;
  };
  const enhanceDOM = (root, copyLbl) => {
    qa(root, "table:not(.lntable)").forEach((t) => {
      if (!t.closest("pre, .lntable")) wrapNode(t, "volo-table-scroll");
    });
    qa(root, "pre, table.lntable").forEach((n) => {
      if (n.closest(".volo-code-block, .vc-code-wrap") || (n.tagName === "PRE" && n.closest(".lntable"))) return;
      const w = wrapNode(n, "vc-code-wrap");
      if (w) {
        const b = ce("button", "volo-copy-btn vc-code-copy js-vc-copy");
        b.type = "button"; b.textContent = copyLbl; w.insertBefore(b, n);
      }
    });
  };

  const mdPrev = (md, emptyLbl) => {
    const t = compactMD(md);
    return t ? "<p>" + esc(t).replace(/\n/g, "<br>") + "</p>" : dvH("vc-empty", esc(emptyLbl));
  };

  const applyEdit = (ta, nextVal, s0, s1) => {
    ta.value = nextVal; ta.focus(); ta.setSelectionRange(s0, s1 ?? s0); ta.dispatchEvent(new Event("input"));
  };

  const mdTool = (ta, act) => {
    const s = ta.selectionStart || 0, e = ta.selectionEnd || 0, v = ta.value || "", sel = v.slice(s, e);
    const set = (r, a, b) => applyEdit(ta, v.slice(0, s) + r + v.slice(e), a, b);
    const INL = { bold: ["**", "**", "bold"], italic: ["*", "*", "italic"], strike: ["~~", "~~", "text"], code: ["`", "`", "code"] }[act];
    if (INL) {
      const b = sel || INL[2], r = INL[0] + b + INL[1], st = s + INL[0].length;
      return set(r, sel ? s + r.length : st, sel ? s + r.length : st + b.length);
    }
    if (act === "link" || act === "image") {
      if (act === "link" && /^https?:\/\//i.test(sel.trim())) return set("[link](" + sel.trim() + ")", s + 1, s + 5);
      const pfx = act === "image" ? "![" : "[", lbl = sel || act, u = s + pfx.length + lbl.length + 2;
      return set(pfx + lbl + "](https://)", u, u + 8);
    }
    const PFX = { h3: ["### ", /^#{1,6}\s+/], quote: ["> ", /^>\s?/], ul: ["- ", /^[-*+]\s+/], ol: ["1. ", /^\d+\.\s+/] }[act];
    if (PFX) {
      const ls = v.lastIndexOf("\n", s - 1) + 1, leI = v.indexOf("\n", e), le = leI < 0 ? v.length : leI;
      const out = v.slice(ls, le).split("\n").map((ln, i) => PFX[1].test(ln) ? ln.replace(PFX[1], "") : (act === "ol" ? (i + 1) + ". " : PFX[0]) + ln).join("\n");
      return applyEdit(ta, v.slice(0, ls) + out + v.slice(le), ls + out.length);
    }
    const pre = s > 0 && v[s - 1] !== "\n" ? "\n" : "", post = e < v.length && v[e] !== "\n" ? "\n" : "";
    const BLK = { hr: [(s > 0 ? "\n\n" : "") + "---\n", 0], fence: [pre + "```\n" + sel + "\n```" + post, pre.length + 4 + sel.length], table: [pre + "| Col 1 | Col 2 |\n| --- | --- |\n|  |  |" + post, pre.length + 2] }[act];
    if (BLK) set(BLK[0], s + (BLK[1] || BLK[0].length), act === "table" ? s + BLK[1] + 5 : undefined);
  };

  const rawHash = () => {
    const h = (win.location.hash || "").slice(1);
    try { return decodeURIComponent(h); } catch { return h; }
  };
  const hashCID = () => { const h = rawHash(); return h.startsWith("comment-") ? h.slice(8) : ""; };
  const copyStr = (txt) => {
    const v = txt || "", leg = () => {
      try {
        const t = ce("textarea");
        t.value = v; t.readOnly = 1; t.style.cssText = "position:fixed;top:0;opacity:0";
        doc.body.appendChild(t); t.select();
        const ok = doc.execCommand("copy"); t.remove(); return ok;
      } catch { return 0; }
    };
    return navigator.clipboard?.writeText ? navigator.clipboard.writeText(v).catch(() => leg() ? 0 : Promise.reject()) : leg() ? Promise.resolve() : Promise.reject();
  };

  const TOOLS = "bold|Bold|<b>B</b>,italic|Italic|<i>I</i>,strike|Strike|<s>S</s>,code|Code|&lt;/&gt;,,h3|H3|H3,quote|Quote|❝,ul|List|•,ol|Ordered|1.,hr|Rule|—,,link|Link|🔗,image|Image|🖼,fence|Code Block|{ },table|Table|⊞"
    .split(",").map((x) => x && x.split("|"));

  function mount() {
    const el = byId("volo-comments");
    if (!el || ga(el, "data-mounted") === "1") return;
    sa(el, "data-mounted", "1");
    const rootEl = doc.documentElement, q = (s, r = el) => r.querySelector(s);
    const attr = (k, d) => ga(el, k) || d || "", dL = (k, d) => attr("data-l-" + k, d), dE = (k, d) => esc(dL(k, d));
    const tType = attr("data-target-type", "article"), tID = attr("data-target-id"), isLogged = attr("data-is-logged") === "true";
    const viewerID = attr("data-viewer-id"), viewerName = attr("data-viewer-name", "Reader"), viewerRole = attr("data-viewer-role");
    const isStaff = viewerRole === "admin" || viewerRole === "editor", rawVA = attr("data-viewer-avatar");
    const vAvatar = isSafeHref(rawVA) ? rawVA : "/avatars/" + encU(viewerID || viewerName) + ".svg";
    const csrfToken = attr("data-csrf-token") || ga(q('meta[name="csrf-token"]', doc), "content");
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
      if (!b || b.dataset.vcExpanded === "1" || q(".volo-comment-shielded", b)) return;
      clsR(b, "is-collapsed");
      const nx = b.nextElementSibling;
      if (b.scrollHeight > 260) {
        clsA(b, "is-tall", "is-collapsed");
        if (!clsH(nx, "js-vc-expand-tall")) {
          const btn = ce("button", "volo-comment-expand-btn js-vc-expand-tall");
          btn.type = "button"; sa(btn, "aria-expanded", "false"); btn.textContent = expandLbl;
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
      const isDel = !!c.is_deleted || c.status === "deleted", seed = c.avatar_seed || c.id || "guest", cid = esc(c.id), acts = [];
      let av = "/avatars/" + encU(seed) + ".svg", who = "", badges = "", bodyHTML = String(c.content_html || "");
      if (!isDel) {
        if (isSafeHref(c.avatar_url)) av = String(c.avatar_url).trim();
        const pName = c.reply_to_name || (depth >= 1 ? parentAuthor : ""), at = pName ? spH("vc-reply-at", "@" + esc(pName)) : "";
        if (at) bodyHTML = /^\s*<p\b[^>]*>/i.test(bodyHTML) ? bodyHTML.replace(/^(\s*<p\b[^>]*>)/i, "$1" + at + " ") : at + " " + bodyHTML;
        const rawU = c.author_url ? String(c.author_url).trim() : "";
        who = /^https?:\/\//i.test(rawU) ? '<a href="' + esc(rawU) + '" rel="nofollow ugc noopener" target="_blank">' + esc(c.author_name) + "</a>" : "<strong>" + esc(c.author_name) + "</strong>";
        const delLbl = dL("del", "Delete"), mkAct = (cls, ex, lbl) => btnH("vc-act-btn " + cls, esc(lbl), ' data-id="' + cid + '"' + ex);
        if (canCompose && !viewerMuted) acts.push(mkAct("js-vc-reply", ' data-name="' + esc(c.author_name) + '"', dL("reply", "Reply")));
        if (c.can_edit && !c.is_shielded && !viewerMuted) acts.push(mkAct("js-vc-edit", ' data-md="' + esc(c.content_md || "") + '"', dL("edit", "Edit")), mkAct("vc-act-btn--danger js-vc-del", "", delLbl));
        if (c.can_moderate && csrfToken) {
          if (c.status === "pending") acts.push(mkAct("js-vc-mod-approve", "", dL("approve", "Approve")));
          if (!c.can_edit) acts.push(mkAct("vc-act-btn--danger js-vc-mod-del", "", delLbl));
          if (c.can_ban_ip) acts.push(mkAct("vc-act-btn--danger js-vc-mod-ban", ' data-ip="' + esc(c.ip_masked || "") + '"', dL("ban", "Ban IP")));
        }
        badges = (c.is_author ? spH("vc-author-badge", dE("author", "Author")) : "") +
          (c.status === "pending" ? spH("vc-pending-badge vc-badge--pending", dE("pending-badge", "Pending")) : "");
      }
      const reps = c.replies || [];
      let kids = "";
      if (reps.length) {
        const dN = (depth || 0) + 1, dA = ' data-depth="' + Math.min(dN, 3) + '"', mapR = (a) => a.map((r) => renderItem(r, dN, c.author_name || "")).join("");
        kids = !depth && reps.length > 3 && !hasDesc(reps.slice(3), hashCID())
          ? dvH("vc-replies", mapR(reps.slice(0, 3)) + dvH("js-vc-more-box", mapR(reps.slice(3)), " hidden") + btnH("vc-more-replies js-vc-show-more", esc(dL("more-replies", "+{count}").replace("{count}", "" + (reps.length - 3)))), dA)
          : dvH("vc-replies", mapR(reps), dA);
      }
      return dvH("vc-item" + (isDel ? " is-deleted" : ""),
        '<img class="vc-avatar" src="' + esc(av) + '" data-avatar-seed="' + esc(seed) + '" alt="" width="38" height="38" loading="lazy">' +
        dvH("vc-body",
          dvH("vc-meta", who + badges + "<span>" + (isDel ? "" : "· ") + fmtTime(c.created_at) + "</span>" +
            (!isDel && c.geo_region ? spH("vc-geo", esc(c.geo_region)) : "") +
            (!isDel && c.ip_masked ? spH("vc-ip-badge", esc(c.ip_masked)) : "")) +
          dvH("vc-content vc-item-body volo-prose", bodyHTML) +
          (acts.length ? dvH("vc-actions", acts.join("")) : "")) +
        kids, ' id="comment-' + cid + '"');
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
        minLen = d.comment_min_length || minLen; maxLen = d.comment_max_length || maxLen; maxLines = d.comment_max_lines || maxLines;
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
            btnH("pagination__item js-vc-page", dE("prev", "←"), ' data-page="' + (curPage - 1) + '"' + (curPage <= 1 ? " disabled" : "")) +
            spH("vc-pager__info", esc(dL("page", "{page}/{total}").replace("{page}", "" + curPage).replace("{total}", "" + totPages))) +
            btnH("pagination__item js-vc-page", dE("next", "→"), ' data-page="' + (curPage + 1) + '"' + (curPage >= totPages ? " disabled" : "")) + "</nav>" : "";
        box.innerHTML = (arr.length ? arr.map((c) => renderItem(c, 0)).join("") : mode === "off" ? "" : dvH("vc-empty", dE("empty", ""))) + pager;
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
        if (box && !box.innerHTML) box.innerHTML = dvH("vc-list-err", esc(netErrLbl));
        else showBanner(netErrLbl, true);
      });
    };

    el._stepToHash = (cid) => { if (cid && totPages > 1) { seekID = cid; seekSteps = 0; initScroll = true; refreshList(1); } };

    (isLogged ? (viewerRole === "reader" ? loadPassport(false) : (clearPassport(), Promise.resolve(""))) : mode === "off" ? Promise.resolve("") : loadPassport(true)).then((tok) => {
      gpToken = tok;
      let prof = {};
      if (mode !== "off") { try { prof = JSON.parse(lsOp(PROF_KEY) || "{}") || {}; } catch {} }
      const claimHTML = isLogged && viewerRole === "reader" ? btnH("studio-icon-btn js-vc-claim", dE("claim", "Claim"), " hidden") : "";
      const cancelLbl = dE("cancel", "Cancel");
      let topPrompt = "", drwHTML = "";

      if (mode === "off") {
        topPrompt = dvH("volo-banner vc-closed-banner", dE("closed", "Comments are currently closed."));
      } else if (mode === "registered_only" && !isLogged) {
        const p = (win.location.pathname || "/") + (win.location.search || "") + "#volo-comments";
        const lHref = p[0] !== "/" || p.startsWith("//") || p.startsWith("/\\") ? "/reader/login" : "/reader/login?next=" + encU(p);
        topPrompt = dvH("volo-banner vc-regonly-banner", "<span>" + dE("regonly", "Sign in to comment") + '</span> <a class="vc-login-link" href="' + esc(lHref) + '">' + dE("login", "Sign In") + " →</a>");
      } else {
        topPrompt = btnH("vc-trigger-bar js-vc-open", "<span>" + dE("trigger", "Write…") + "</span>" + spH("vc-trigger-bar__kbd", dE("shortcut", "Ctrl+Enter")));
        const idHTML = isLogged
          ? dvH("vc-identity", '<img class="vc-identity__avatar" src="' + esc(vAvatar) + '" data-avatar-seed="' + esc(viewerID || viewerName) + '" alt="" width="28" height="28"><span>' + esc(viewerName) + "</span>" + (viewerRole === "admin" ? spH("vc-author-badge", dE("author", "Author")) : ""))
          : dvH("vc-guest-chip js-vc-guest-chip", spH("js-vc-guest-label", esc(dL("guest-as", "{name}").replace("{name}", prof.name || ""))) + "<span>·</span>" + btnH("js-vc-toggle-guest", dE("edit-profile", "Edit")), prof.name ? "" : " hidden");
        const gIn = (nm, tp, lk, fb, val, ex = "") => '<input class="volo-input vc-input" name="' + nm + '"' + (tp ? ' type="' + tp + '"' : "") + ex + ' placeholder="' + dE(lk, fb) + '" value="' + esc(val || "") + '">';
        const gRow = isLogged ? "" : dvH("vc-row js-vc-guest-row", gIn("author_name", "", "nick", "Name", prof.name, ' required maxlength="48"') + gIn("notify_email", "email", "email", "Email", prof.email) + gIn("author_url", "url", "site", "Website", prof.url), prof.name ? " hidden" : "") +
          btnH("vc-id-summary js-vc-id-expand", spH("vc-id-summary__label js-vc-id-sum-label", "") + spH("vc-id-summary__act", dE("edit-profile", "Edit")));
        const tools = TOOLS.map((t) => {
          if (!t) return '<span class="vc-tb-sep" aria-hidden="true"></span>';
          const tip = dE("tb-" + t[0], t[1]);
          return btnH("vc-tool-btn", t[2], ' data-md-act="' + t[0] + '" title="' + tip + '" aria-label="' + tip + '"');
        }).join("");
        const fsLbl = dE("fullscreen", "Fullscreen");
        drwHTML = dvH("vc-backdrop js-vc-backdrop", "", " hidden") + dvH("volo-comment-composer js-vc-drawer",
          '<form class="vc-form js-vc-form">' +
          dvH("vc-composer-top",
            dvH("vc-composer-top__left", idHTML + dvH("vc-context-pill js-vc-ctx", spH("js-vc-ctx-text", "") + btnH("js-vc-clear-ctx", "×", ' aria-label="Clear"'), " hidden")) +
            dvH("vc-composer-top__right",
              btnH("studio-icon-btn studio-icon-btn--square js-vc-fs", "⛶", ' aria-pressed="false" title="' + fsLbl + '" aria-label="' + fsLbl + '"') +
              btnH("studio-icon-btn studio-icon-btn--square js-vc-close", "✕", ' aria-label="' + cancelLbl + '"'))) +
          dvH("vc-composer-body js-vc-body", gRow +
            '<input class="vc-hp" name="website_hp" tabindex="-1" autocomplete="off">' +
            dvH("vc-editor-shell",
              dvH("vc-toolbar",
                dvH("vc-md-tools", tools, ' role="toolbar"') +
                dvH("vc-tabs", btnH("vc-tab is-active", dE("write", "Write"), ' data-vc-tab="write"') + btnH("vc-tab vc-tab--split", dE("split", "Split"), ' data-vc-tab="split"') + btnH("vc-tab", dE("preview", "Preview"), ' data-vc-tab="preview"'), ' role="tablist"')) +
              dvH("vc-editor-panes js-vc-panes", '<textarea class="vc-textarea js-vc-ta" name="content_md" required placeholder="' + dE("ph", "") + '"></textarea>' + dvH("vc-preview volo-cw-preview volo-prose js-vc-preview", "", " hidden"), ' data-pane-mode="write"'))) +
          dvH("vc-composer-foot",
            dvH("vc-composer-meta", spH("vc-char-cnt js-vc-cnt", "0 / " + maxLen) + spH("vc-msg js-vc-msg", "")) +
            dvH("vc-composer-btns", btnH("studio-icon-btn js-vc-close", cancelLbl) + '<button type="submit" class="volo-btn js-vc-submit">' + dE("submit", "Post") + "</button>")) +
          "</form>", ' role="dialog" aria-modal="true" hidden');
      }

      const banHTML = csrfToken
        ? dvH("vc-ban-modal js-vc-ban-modal", dvH("vc-ban-modal__panel",
            '<h4 class="vc-ban-modal__title js-vc-ban-title">' + dE("ban-title", "Ban IP") + "</h4>" +
            dvH("vc-ban-durations", btnH("vc-dur-btn", dE("ban-24h", "24h"), ' data-hours="24"') + btnH("vc-dur-btn", dE("ban-7d", "7d"), ' data-hours="168"') + btnH("vc-dur-btn is-active", dE("ban-forever", "Permanent"), ' data-hours="0"')) +
            '<label class="vc-ban-check"><input type="checkbox" class="js-vc-ban-reject" checked><span>' + dE("ban-reject", "") + '</span></label><input type="text" class="volo-input vc-input js-vc-ban-note" maxlength="160" placeholder="' + dE("ban-note", "") + '">' +
            dvH("vc-ban-foot", btnH("studio-icon-btn js-vc-ban-cancel", cancelLbl) + btnH("volo-btn vc-btn-danger js-vc-ban-ok", dE("ban-confirm", "Confirm"))),
            ' role="dialog" aria-modal="true"'), " hidden") : "";

      el.innerHTML = dvH("vc-head", '<h3 class="vc-head__title">' + dE("title", "Comments") + "</h3>" + claimHTML) + topPrompt + dvH("js-vc-status volo-banner", "", " hidden") + dvH("js-vc-list") + drwHTML + banHTML;

      const drawer = q(".js-vc-drawer"), bdrop = q(".js-vc-backdrop"), form = q(".js-vc-form"), bodyEl = q(".js-vc-body"), ta = q(".js-vc-ta"), pBox = q(".js-vc-preview"), panes = q(".js-vc-panes");
      const cntEl = q(".js-vc-cnt"), ctxPill = q(".js-vc-ctx"), ctxText = q(".js-vc-ctx-text"), subBtn = q(".js-vc-submit"), msgEl = q(".js-vc-msg"), fsBtn = q(".js-vc-fs"), banModal = q(".js-vc-ban-modal");
      let curTab = "write", isFS = false, prevTimer = 0, prevCtrl = null;
      sBan = q(".js-vc-status");

      const syncLock = () => win.__voloSyncScrollLock?.();
      const gRowEl = q(".js-vc-guest-row"), idSumLbl = q(".js-vc-id-sum-label");
      const isNarrow = () => (win.innerWidth || 1024) < 768;
      const finePointer = () => { try { return !!win.matchMedia?.("(pointer: fine)").matches; } catch { return false; } };
      const focusNoScroll = (n) => { if (!n) return; try { n.focus({ preventScroll: true }); } catch { n.focus(); } };
      let vvRAF = 0, kbState = false, kbPending = null, vvPrev = [];
      const syncIdentity = () => {
        if (!drawer || !gRowEl) return;
        const ae = doc.activeElement, collapse = kbState && !(ae && gRowEl.contains(ae)) && !gRowEl.hidden && isNarrow();
        if (collapse && idSumLbl) {
          const nm = String(q("input[name='author_name']")?.value || "").trim();
          idSumLbl.textContent = nm ? dL("guest-as", "{name}").replace("{name}", nm) : dL("nick", "Name");
        }
        clsT(drawer, "is-identity-collapsed", collapse);
      };
      const focusIdentity = () => {
        if (!gRowEl) return;
        gRowEl.hidden = false; clsR(drawer, "is-identity-collapsed");
        q("input[name='author_name']")?.focus();
      };
      const setKb = (v) => {
        if (kbState === v) return;
        kbState = v; clsT(drawer, "is-kb-open", v); syncIdentity();
        if (v && doc.activeElement === ta && bodyEl) bodyEl.scrollTop = bodyEl.scrollHeight;
      };
      const clrVV = () => {
        if (vvRAF) { cancelAnimationFrame(vvRAF); vvRAF = 0; }
        kbPending = null; kbState = false; vvPrev = [];
        if (drawer) {
          ["--vc-vv-top", "--vc-vv-h", "--vc-kb"].forEach((p) => drawer.style.removeProperty(p));
          clsR(drawer, "is-kb-open", "has-vv", "is-identity-collapsed");
        }
      };
      const syncVV = () => {
        if (vvRAF) { cancelAnimationFrame(vvRAF); vvRAF = 0; }
        const vv = win.visualViewport;
        if (!drawer || drawer.hidden || !vv) return;
        if ((vv.scale || 1) > 1.05) return clsR(drawer, "has-vv");
        const ih = win.innerHeight || rootEl.clientHeight || 0;
        const top = Math.max(0, Math.round(vv.offsetTop)), h = Math.round(vv.height), kb = Math.max(0, Math.round(ih - vv.height - vv.offsetTop));
        const kbOpen = drawer.contains(doc.activeElement) && (kb > 80 || (isNarrow() && vv.height < 480));
        const ds = drawer.style; ["--vc-vv-top", "--vc-vv-h", "--vc-kb"].forEach((k, i) => { const v = [top, h, kb][i]; if (v !== vvPrev[i]) ds.setProperty(k, (vvPrev[i] = v) + "px"); });
        clsA(drawer, "has-vv");
        if (kbOpen === kbState) kbPending = null;
        else if (kbPending === kbOpen) { kbPending = null; setKb(kbOpen); }
        else { kbPending = kbOpen; vvRAF = requestAnimationFrame(syncVV); }
      };
      const scheduleVV = () => { if (!vvRAF) vvRAF = requestAnimationFrame(syncVV); };
      ["resize", "scroll"].forEach((e) => onEv(win.visualViewport, e, scheduleVV));

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
        const raw = ta.value || "", emptyLbl = dL("preview-empty", ""), emptyHTML = dvH("vc-empty", esc(emptyLbl));
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
        if (which === "split" && (!isFS || isNarrow())) which = "preview";
        curTab = panes.dataset.paneMode = which;
        qa(el, "[data-vc-tab]").forEach((b) => clsT(b, "is-active", b.dataset.vcTab === which));
        ta.hidden = which === "preview"; pBox.hidden = which === "write";
        if (which !== "write") updatePreview(true);
      };

      const setFullscreen = (next) => {
        if (!drawer || isFS === next) return;
        const hadFocus = !!ta && doc.activeElement === ta, selS = ta?.selectionStart ?? 0, selE = ta?.selectionEnd ?? 0;
        isFS = next; clsT(drawer, "is-fullscreen", isFS);
        if (fsBtn) {
          const lbl = dL(isFS ? "exit-fullscreen" : "fullscreen", isFS ? "Exit Fullscreen" : "Fullscreen");
          sa(fsBtn, "aria-pressed", "" + isFS); fsBtn.title = lbl; sa(fsBtn, "aria-label", lbl);
          fsBtn.textContent = isFS ? "❐" : "⛶";
        }
        if (bdrop) { bdrop.hidden = false; clsR(bdrop, "is-closing"); clsA(bdrop, "is-open"); }
        if (isFS) { if (!isNarrow() && curTab === "write") setTab("split"); }
        else if (curTab === "split") setTab("write");
        syncVV(); syncLock();
        if (hadFocus && ta && !ta.hidden) {
          if (doc.activeElement !== ta) {
            focusNoScroll(ta);
            try { ta.setSelectionRange(selS, selE); } catch {}
          }
          requestAnimationFrame(() => { if (doc.activeElement === ta && clsH(drawer, "is-kb-open") && bodyEl) bodyEl.scrollTop = bodyEl.scrollHeight; });
          return;
        }
        if (ta && !ta.hidden && doc.activeElement !== ta && finePointer()) focusNoScroll(ta);
      };

      onEv(win, "resize", () => {
        if (isFS && curTab === "split" && isNarrow()) setTab("write");
        scheduleVV(); syncIdentity();
      });

      let lastTriggerEl = null, lastBanTriggerEl = null;
      const trapModalTab = (box, ev) => {
        if (!box || box.hidden || ev.key !== "Tab" || ev.defaultPrevented || ev.ctrlKey || ev.metaKey || ev.altKey) return;
        const nodes = qa(box, "button,[href],input:not([type=hidden]),select,textarea,[tabindex]").filter((n) => !n.disabled && n.tabIndex >= 0 && !n.hidden && !n.closest("[hidden]") && n.offsetParent !== null);
        if (!nodes.length) return;
        const first = nodes[0], last = nodes[nodes.length - 1], ae = doc.activeElement;
        if (ev.shiftKey) {
          if (ae === first || !box.contains(ae)) { ev.preventDefault(); focusNoScroll(last); }
        } else if (ae === last || !box.contains(ae)) {
          ev.preventDefault(); focusNoScroll(first);
        }
      };
      [drawer, banModal].forEach((b) => onEv(b, "keydown", (ev) => trapModalTab(b, ev)));

      const clrEdit = () => { if (editingID && ta) { editingID = ta.value = ""; syncCounter(); } };
      const closeBanModal = (restoreFocus = true) => {
        if (!banModal || banModal.hidden) return;
        banModal.hidden = true; syncLock();
        const prev = lastBanTriggerEl; lastBanTriggerEl = null;
        if (restoreFocus && prev?.isConnected) focusNoScroll(prev);
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
        clrEdit(); syncCtx(); clrVV();
        [bdrop, drawer].forEach((n) => n && (clsR(n, "is-open", "is-inline", "is-closing"), n.hidden = true));
        syncLock();
        const prev = lastTriggerEl; lastTriggerEl = null;
        const tgt = prev?.isConnected ? prev : q(".js-vc-open");
        if (restoreFocus && tgt?.isConnected) focusNoScroll(tgt);
      };

      refreshList(1);

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

      const keepFocus = (ev) => {
        if (!ta || doc.activeElement !== ta || (ev.button !== undefined && ev.button !== 0)) return;
        if (ev.target?.closest?.(".js-vc-fs, [data-vc-tab], .vc-tool-btn, .js-vc-id-expand, .js-vc-clear-ctx")) ev.preventDefault();
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
        if (!isLogged && !editingID && !String(fd.get("author_name") || "").trim()) return focusIdentity();
        isSubmitting = subBtn.disabled = true; setErr("");
        const done = () => { isSubmitting = false; syncCounter(); syncCtx(); };
        if (editingID) {
          jReq("/api/v1/comments/" + encU(editingID), "PATCH", { content_md: content }, !!csrfToken).then((res) => {
            if (!res.ok) return setErr(res.d.error || netErrLbl);
            editingID = ta.value = ""; closeComposer(); refreshList(curPage);
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
              lsOp(PROF_KEY, JSON.stringify({ name: nameV, email: emailV, url: urlV }));
              const chip = q(".js-vc-guest-chip"), chipLbl = q(".js-vc-guest-label"), gRow = q(".js-vc-guest-row");
              if (chip && chipLbl) { chipLbl.textContent = dL("guest-as", "{name}").replace("{name}", nameV); chip.hidden = false; }
              if (gRow) gRow.hidden = true;
            }
            ta.value = repID = repName = ""; closeComposer();
            showBanner(res.d.comment?.status === "pending" ? dL("pending", "") : "", false);
            refreshList(wasRep ? curPage : 1);
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
        const cl = (s) => ev.target.closest(s), ref = () => refreshList(curPage);
        let t;
        if ((t = cl(".js-vc-expand-tall"))) {
          const b = t.previousElementSibling;
          if (clsH(b, "vc-content")) {
            const open = clsH(b, "is-collapsed");
            clsT(b, "is-collapsed", !open); b.dataset.vcExpanded = open ? "1" : "0";
            sa(t, "aria-expanded", "" + open); t.textContent = open ? collapseLbl : expandLbl;
          }
        } else if ((t = cl(".js-vc-copy"))) {
          const p = t.parentElement, lt = q(".lntable", p), cn = (lt && qa(lt, "tr").length < 2 && q("td:last-child pre code, td:last-child pre", lt)) || lt || q("pre code, pre", p);
          if (cn) {
            copyStr(win.__voloExtractCode ? win.__voloExtractCode(cn) : (cn.textContent || "").replace(/\u00a0/g, " ")).then(() => {
              t.textContent = copiedLbl; sa(t, "data-copied", "true"); clsA(t, "is-copied");
              clearTimeout(t._t);
              t._t = setTimeout(() => { t.textContent = copyLbl; t.removeAttribute("data-copied"); clsR(t, "is-copied"); }, 1600);
            }).catch(() => {});
          }
        } else if ((t = cl(".js-vc-open"))) { clrEdit(); repID = repName = editingID = ""; syncCtx(); openComposer(t); }
        else if (cl(".js-vc-fs")) setFullscreen(!isFS);
        else if (cl(".js-vc-close")) closeComposer();
        else if (cl(".js-vc-backdrop")) isFS ? setFullscreen(false) : closeComposer();
        else if (cl(".js-vc-clear-ctx")) { repID = repName = ""; clrEdit(); ta?.dispatchEvent(new Event("input")); syncCtx(); }
        else if (cl(".js-vc-toggle-guest")) {
          const gRow = q(".js-vc-guest-row");
          if (gRow && !(gRow.hidden = !gRow.hidden)) focusIdentity(); else syncIdentity();
        }
        else if (cl(".js-vc-id-expand")) focusIdentity();
        else if ((t = cl(".js-vc-page"))) { if (!t.disabled) { refreshList(+t.dataset.page || 1); win.__voloScrollToEl?.(el, false); } }
        else if ((t = cl(".js-vc-show-more"))) { const mb = t.previousElementSibling; if (mb) { mb.hidden = false; applyTall(mb); } t.remove(); }
        else if ((t = cl("[data-md-act]")) && ta) { if (curTab === "preview") setTab("write"); mdTool(ta, t.dataset.mdAct); }
        else if ((t = cl("[data-vc-tab]"))) setTab(t.dataset.vcTab);
        else if ((t = cl(".js-vc-reply"))) { clrEdit(); editingID = ""; repID = t.dataset.id; repName = t.dataset.name; syncCtx(); openComposer(t); }
        else if ((t = cl(".js-vc-edit")) && ta) { repID = repName = ""; editingID = t.dataset.id; ta.value = t.dataset.md || ""; syncCounter(); syncCtx(); openComposer(t); }
        else if ((t = cl(".js-vc-del"))) { if (cfTap(t)) actReq("/api/v1/comments/" + encU(t.dataset.id), "DELETE", null, !!csrfToken, ref); }
        else if ((t = cl(".js-vc-mod-approve"))) actReq("/api/admin/comments/" + encU(t.dataset.id), "PATCH", { status: "approved" }, true, ref);
        else if ((t = cl(".js-vc-mod-del"))) { if (cfTap(t)) actReq("/api/admin/comments/" + encU(t.dataset.id), "DELETE", null, true, ref); }
        else if ((t = cl(".js-vc-mod-ban")) && banModal) {
          lastBanTriggerEl = t; banID = t.dataset.id;
          const ip = t.dataset.ip, bt = q(".js-vc-ban-title"), bn = q(".js-vc-ban-note");
          if (bt) bt.textContent = dL("ban-title", "Ban IP") + (ip ? " (" + ip + ")" : "");
          if (bn) bn.value = "";
          banModal.hidden = false; syncLock();
          setTimeout(() => focusNoScroll(bn || q(".js-vc-ban-cancel")), 20);
        }
        else if ((t = cl(".vc-dur-btn")) && banModal) {
          banHours = +t.dataset.hours || 0;
          qa(banModal, ".vc-dur-btn").forEach((b) => clsT(b, "is-active", b === t));
        }
        else if (cl(".js-vc-ban-cancel") || (banModal && ev.target === banModal)) closeBanModal();
        else if (cl(".js-vc-ban-ok") && banModal && banID) {
          const rej = q(".js-vc-ban-reject"), noteIn = q(".js-vc-ban-note");
          closeBanModal(false);
          actReq("/api/admin/comments/" + encU(banID) + "/ban-ip", "POST", { note: noteIn ? noteIn.value.trim() : "", expires_hours: banHours, reject_comment: rej ? !!rej.checked : true }, true, ref);
        }
        else if ((t = cl(".js-vc-claim")) && !claimedDone) {
          actReq("/api/v1/reader/claim-guest-comments", "POST", null, !!csrfToken, (d) => {
            claimedDone = t.disabled = true; gpToken = ""; clearPassport();
            t.textContent = d.message || "OK";
            setTimeout(() => { t.hidden = true; }, 1800); ref();
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
    win.__voloScrollToEl ? win.__voloScrollToEl(node, isCmt) : node.scrollIntoView({ behavior: "smooth", block: "center" });
  }

  onEv(win, "hashchange", focusHash);
  win.VoloCommentWidget = { mount };
  if (doc.readyState === "loading") onEv(doc, "DOMContentLoaded", mount); else mount();
})();
