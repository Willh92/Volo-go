import { useEffect, useState } from "preact/hooks";
import { apiFetch, resetConsoleScroll, showToast } from "../api";
import { t } from "../i18n";

export type AboutHelpTab = "about" | "help";

interface ProjectInfoPayload {
  version: string;
  go_version: string;
  os_arch: string;
  db_dialect: string;
  admin_gzip_kb: number;
  online: boolean;
  has_update: boolean;
  checked_at: number;
  repo: {
    full_name: string;
    html_url: string;
    description: string;
    stars: number;
    forks: number;
    open_issues: number;
    language: string;
    license: string;
  };
  latest_release: {
    tag_name: string;
    name: string;
    html_url: string;
    published_at: string;
  };
}

export function AboutHelpWorkspace({
  activeTab = "about",
  onNavigate,
}: {
  activeTab?: AboutHelpTab;
  onNavigate?: (sec: string) => void;
}) {
  const [sub, setSub] = useState<AboutHelpTab>(activeTab);
  const [info, setInfo] = useState<ProjectInfoPayload | null>(null);
  const [loading, setLoading] = useState(false);
  const [helpQuery, setHelpQuery] = useState("");

  useEffect(() => {
    setSub(activeTab);
  }, [activeTab]);

  const loadInfo = async (forceRefresh = false) => {
    setLoading(true);
    try {
      const url = forceRefresh
        ? "/api/admin/about/project-info?refresh=1"
        : "/api/admin/about/project-info";
      const res = await apiFetch<ProjectInfoPayload>(url);
      setInfo(res);
      if (forceRefresh) {
        showToast(t("about.refreshed_toast"));
      }
    } catch (err: any) {
      showToast(err.message);
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    if (sub === "about" && !info) {
      void loadInfo(false);
    }
  }, [sub]);

  const switchSub = (next: AboutHelpTab) => {
    setSub(next);
    resetConsoleScroll();
    onNavigate?.(next);
  };

  const helpCards = [
    {
      id: "md",
      title: t("help.md_head"),
      desc: t("help.md_desc"),
      items: [
        { code: ':::lock pwd="1234" hint="..."', text: t("help.md_item_lock_pwd") },
        { code: ':::lock type="user"', text: t("help.md_item_lock_user") },
        { code: ':::book title="..." author="..."', text: t("help.md_item_book") },
        { code: ":::bookshelf", text: t("help.md_item_shelf") },
        { code: "[02:15] / [01:05:30]", text: t("help.md_item_podcast") },
      ],
    },
    {
      id: "shortcuts",
      title: t("help.shortcut_head"),
      desc: t("help.shortcut_desc"),
      items: [
        { code: "Ctrl / Cmd + K", text: t("help.sc_item_search") },
        { code: "Ctrl / Cmd + B / I", text: t("help.sc_item_format") },
        { code: "IR / WYSIWYG / SV", text: t("help.sc_item_modes") },
        { code: "Drag & Paste", text: t("help.sc_item_upload") },
        { code: "Media Picker", text: t("help.sc_item_library") },
      ],
    },
    {
      id: "ops",
      title: t("help.ops_head"),
      desc: t("help.ops_desc"),
      items: [
        { code: "volo serve", text: t("help.ops_item_serve") },
        { code: "volo doctor", text: t("help.ops_item_doctor") },
        { code: "NDJSON .zip", text: t("help.ops_item_backup") },
        { code: "Legacy .zip ETL", text: t("help.ops_item_etl") },
        { code: "301 Guardian", text: t("help.ops_item_301") },
      ],
    },
    {
      id: "sec",
      title: t("help.sec_head"),
      desc: t("help.sec_desc"),
      items: [
        { code: "WebAuthn Passkey", text: t("help.sec_item_passkey") },
        { code: "TOTP + Recovery", text: t("help.sec_item_totp") },
        { code: "WAF & GeoIP", text: t("help.sec_item_waf") },
        { code: "Scoped PAT", text: t("help.sec_item_pat") },
        { code: "Zero-PII Analytics", text: t("help.sec_item_privacy") },
      ],
    },
  ];

  const q = helpQuery.trim().toLowerCase();
  const visibleHelp = helpCards.filter((card) => {
    if (!q) return true;
    if (card.title.toLowerCase().includes(q) || card.desc.toLowerCase().includes(q)) return true;
    return card.items.some(
      (it) => it.code.toLowerCase().includes(q) || it.text.toLowerCase().includes(q)
    );
  });

  return (
    <div>
      <div class="volo-subtabs">
        <button
          type="button"
          class={`volo-tab ${sub === "about" ? "active" : ""}`}
          onClick={() => switchSub("about")}
        >
          {t("nav.about")}
        </button>
        <button
          type="button"
          class={`volo-tab ${sub === "help" ? "active" : ""}`}
          onClick={() => switchSub("help")}
        >
          {t("nav.help")}
        </button>
      </div>

      {sub === "about" ? (
        <div>
          <div class="volo-about-hero">
            <div class="volo-about-hero-left">
              <div class="volo-about-mark">V.</div>
              <div>
                <div style={{ fontSize: "11px", fontWeight: 700, letterSpacing: "0.15em", opacity: 0.8 }}>
                  A SPACE FOR YOUR WORDS
                </div>
                <h2 style={{ margin: "4px 0", fontSize: "26px", fontWeight: 760 }}>Volo</h2>
                <div style={{ fontSize: "14px", opacity: 0.86 }}>{t("about.hero_sub")}</div>
              </div>
            </div>
            <div style={{ display: "flex", alignItems: "center", gap: "10px", flexWrap: "wrap" }}>
              <span
                class="volo-badge"
                style={{
                  background: "rgba(255,255,255,0.16)",
                  color: "#ffffff",
                  padding: "6px 13px",
                  fontSize: "12.5px",
                }}
              >
                {t("about.cur_version")} {info?.version || "v1.0.0"}
              </span>
            </div>
          </div>

          <div class="volo-grid-2">
            <div class="volo-card">
              <div class="volo-sec-head">
                <div class="volo-sec-copy">
                  <h3 class="volo-card-title">{t("about.repo_head")}</h3>
                  <p class="volo-sec-desc">
                    <a
                      href={info?.repo?.html_url || "https://github.com/Willh92/Volo-go"}
                      target="_blank"
                      rel="noreferrer"
                      style={{ color: "var(--volo-accent)", fontWeight: 600, textDecoration: "none" }}
                    >
                      {info?.repo?.full_name || "Willh92/Volo-go"} ↗
                    </a>
                  </p>
                </div>
                <span class={`volo-status-pill ${info?.online ? "ok" : ""}`}>
                  <span class="volo-status-dot" />
                  {info?.online ? t("about.online") : t("about.offline")}
                </span>
              </div>

              <p class="volo-sec-desc" style={{ marginBottom: "14px" }}>
                {info?.repo?.description || t("about.repo_default_desc")}
              </p>

              <div class="volo-repo-stats">
                <div class="volo-repo-stat">
                  <strong>{info?.repo?.stars ?? 0}</strong>
                  <span>Stars</span>
                </div>
                <div class="volo-repo-stat">
                  <strong>{info?.repo?.forks ?? 0}</strong>
                  <span>Forks</span>
                </div>
                <div class="volo-repo-stat">
                  <strong>{info?.repo?.open_issues ?? 0}</strong>
                  <span>Issues</span>
                </div>
              </div>

              <div class="volo-about-kv-grid">
                <div class="volo-about-kv">
                  <small>{t("about.kv_lang")}</small>
                  <span>{info?.go_version || "Go"}</span>
                </div>
                <div class="volo-about-kv">
                  <small>{t("about.kv_arch")}</small>
                  <span>{info?.os_arch || "linux/amd64"}</span>
                </div>
                <div class="volo-about-kv">
                  <small>{t("about.kv_db")}</small>
                  <span>{(info?.db_dialect || "sqlite").toUpperCase()}</span>
                </div>
                <div class="volo-about-kv">
                  <small>{t("about.kv_bundle")}</small>
                  <span>{info?.admin_gzip_kb ? `${info.admin_gzip_kb} KB (gzip)` : "< 300 KB"}</span>
                </div>
              </div>

              <div class="volo-form-actions">
                <button
                  type="button"
                  class="volo-btn volo-btn-sm"
                  disabled={loading}
                  onClick={() => void loadInfo(true)}
                >
                  {loading ? t("shell.loading") : t("about.refresh_btn")}
                </button>
                <a
                  class="volo-btn volo-btn-primary volo-btn-sm"
                  href={info?.repo?.html_url || "https://github.com/Willh92/Volo-go"}
                  target="_blank"
                  rel="noreferrer"
                >
                  GitHub ↗
                </a>
              </div>
            </div>

            <div style={{ display: "flex", flexDirection: "column", gap: "18px" }}>
              <div class="volo-card" style={{ marginBottom: 0 }}>
                <div class="volo-sec-head">
                  <div class="volo-sec-copy">
                    <h3 class="volo-card-title">{t("about.release_head")}</h3>
                    <p class="volo-sec-desc">{t("about.release_desc")}</p>
                  </div>
                  <span class={`volo-badge ${info?.has_update ? "volo-badge-warn" : "volo-badge-ok"}`}>
                    {info?.has_update ? t("about.update_avail") : t("about.up_to_date")}
                  </span>
                </div>

                <div class="volo-subcard" style={{ margin: "6px 0 12px" }}>
                  <div style={{ display: "flex", justifyContent: "space-between", alignItems: "center", gap: "8px" }}>
                    <span class="volo-help">{t("about.latest_tag")}</span>
                    <span class="volo-code-chip">
                      {info?.latest_release?.tag_name || info?.version || "v1.0.0"}
                    </span>
                  </div>
                  {info?.latest_release?.published_at && (
                    <div class="volo-help" style={{ marginTop: "6px" }}>
                      {info.latest_release.published_at}
                    </div>
                  )}
                </div>

                <div class="volo-form-actions">
                  <a
                    class="volo-btn volo-btn-sm"
                    href={info?.latest_release?.html_url || "https://github.com/Willh92/Volo-go/releases"}
                    target="_blank"
                    rel="noreferrer"
                  >
                    {t("about.view_releases")} ↗
                  </a>
                </div>
              </div>

              <div class="volo-card" style={{ marginBottom: 0 }}>
                <h3 class="volo-card-title" style={{ marginBottom: "8px" }}>
                  {t("about.credits_head")}
                </h3>
                <p class="volo-sec-desc" style={{ lineHeight: 1.7 }}>
                  {t("about.credits_body")}
                </p>
                <div class="volo-form-actions">
                  <button
                    type="button"
                    class="volo-btn volo-btn-sm"
                    onClick={() => switchSub("help")}
                  >
                    {t("about.open_help_btn")} →
                  </button>
                </div>
              </div>
            </div>
          </div>
        </div>
      ) : (
        <div>
          <div class="volo-card">
            <div class="volo-filter-bar" style={{ marginBottom: 0 }}>
              <div class="volo-sec-copy">
                <h3 class="volo-card-title">{t("help.head")}</h3>
                <p class="volo-sec-desc">{t("help.sub")}</p>
              </div>
              <div class="volo-filter-controls">
                <input
                  class="volo-input"
                  style={{ width: "260px" }}
                  placeholder={t("help.search_ph")}
                  value={helpQuery}
                  onInput={(e) => setHelpQuery((e.target as HTMLInputElement).value)}
                />
              </div>
            </div>
          </div>

          <div class="volo-grid-2">
            {visibleHelp.map((card) => (
              <div key={card.id} class="volo-card" style={{ marginBottom: 0 }}>
                <h3 class="volo-card-title" style={{ marginBottom: "4px" }}>{card.title}</h3>
                <p class="volo-sec-desc" style={{ marginBottom: "14px" }}>{card.desc}</p>
                <div style={{ display: "flex", flexDirection: "column", gap: "10px" }}>
                  {card.items.map((it, idx) => (
                    <div
                      key={idx}
                      style={{
                        display: "flex",
                        alignItems: "flex-start",
                        justifyContent: "space-between",
                        gap: "12px",
                        paddingBottom: idx < card.items.length - 1 ? "9px" : 0,
                        borderBottom: idx < card.items.length - 1 ? "1px dashed var(--volo-line)" : "0",
                      }}
                    >
                      <span style={{ fontSize: "13px", lineHeight: 1.5 }}>{it.text}</span>
                      <code class="volo-kbd" style={{ flexShrink: 0 }}>{it.code}</code>
                    </div>
                  ))}
                </div>
              </div>
            ))}
          </div>
        </div>
      )}
    </div>
  );
}
