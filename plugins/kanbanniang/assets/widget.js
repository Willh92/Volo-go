(function () {
  var SVGS = {
    neko: '<svg viewBox="0 0 64 64" width="38" height="38" fill="none" stroke="currentColor" stroke-width="2.6" stroke-linecap="round" stroke-linejoin="round"><path d="M14 26 L18 11 L28 20 H36 L46 11 L50 26 C53 33 52 46 32 49 C12 46 11 33 14 26 Z"/><circle cx="24" cy="33" r="2.2" fill="currentColor"/><circle cx="40" cy="33" r="2.2" fill="currentColor"/><path d="M29 39 Q32 42 35 39"/></svg>',
    owl: '<svg viewBox="0 0 64 64" width="38" height="38" fill="none" stroke="currentColor" stroke-width="2.6" stroke-linecap="round" stroke-linejoin="round"><rect x="15" y="14" width="34" height="36" rx="16"/><circle cx="25" cy="29" r="5"/><circle cx="39" cy="29" r="5"/><path d="M30 35 L32 39 L34 35 Z" fill="currentColor"/></svg>',
    bot: '<svg viewBox="0 0 64 64" width="38" height="38" fill="none" stroke="currentColor" stroke-width="2.6" stroke-linecap="round" stroke-linejoin="round"><line x1="32" y1="9" x2="32" y2="17"/><circle cx="32" cy="7" r="2"/><rect x="14" y="17" width="36" height="30" rx="8"/><rect x="22" y="27" width="6" height="6" rx="1" fill="currentColor"/><rect x="36" y="27" width="6" height="6" rx="1" fill="currentColor"/><line x1="25" y1="39" x2="39" y2="39"/></svg>'
  };

  function mount() {
    var root = document.getElementById("volo-kanbanniang-root");
    if (!root || root.getAttribute("data-mounted") === "1") return;
    root.setAttribute("data-mounted", "1");

    var ch = root.getAttribute("data-character") || "neko";
    var greeting = root.getAttribute("data-greeting") || "Hello!";
    var pats = parseInt(root.getAttribute("data-pats") || "0", 10) || 0;

    var bubble = document.createElement("div");
    bubble.className = "volo-kbn-bubble";
    bubble.textContent = greeting;

    var btn = document.createElement("button");
    btn.type = "button";
    btn.className = "volo-kbn-btn";
    btn.setAttribute("aria-label", "Interactive Mascot");
    btn.innerHTML = SVGS[ch] || SVGS.neko;

    btn.addEventListener("click", function () {
      fetch("/plugins/kanbanniang/api/pat", {
        method: "POST",
        headers: { Accept: "application/json" }
      })
        .then(function (r) { return r.json(); })
        .then(function (d) {
          if (d && typeof d.total_pats === "number") {
            pats = d.total_pats;
          } else {
            pats += 1;
          }
          root.setAttribute("data-pats", String(pats));
          bubble.textContent = "✨ +" + pats;
        })
        .catch(function () {
          pats += 1;
          bubble.textContent = "✨ +" + pats;
        });
    });

    root.appendChild(bubble);
    root.appendChild(btn);
  }

  function resetBubble() {
    var bubble = document.querySelector("#volo-kanbanniang-root .volo-kbn-bubble");
    var root = document.getElementById("volo-kanbanniang-root");
    if (bubble && root) {
      bubble.textContent = root.getAttribute("data-greeting") || bubble.textContent;
    }
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", mount);
  } else {
    mount();
  }
  window.addEventListener("volo:after-navigate", resetBubble);
  document.addEventListener("volo:page-load", resetBubble);
})();
