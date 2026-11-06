import { useEffect, useState } from "preact/hooks";
import { apiFetch, currentUserSig, showToast } from "../api";
import { formatDateTime, t } from "../i18n";

type DistTab = "nav" | "links" | "follows" | "redirects" | "webhooks" | "feed_seo";

export function DistributeWorkspace() {
  const isAdmin = currentUserSig.value?.role === "admin";
  const [sub, setSub] = useState<DistTab>("nav");

  const [navItems, setNavItems] = useState<any[]>([]);
  const [navForm, setNavForm] = useState({ title: "", url: "/", zh: "", en: "", sort_order: 10, open_in_new_tab: false });

  const [links, setLinks] = useState<any[]>([]);
  const [linkForm, setLinkForm] = useState({ name: "", url: "", avatar_url: "", description: "", category: "Friends", sort_order: 1 });

  const [follows, setFollows] = useState<any[]>([]);
  const [followForm, setFollowForm] = useState({ title: "", site_url: "", feed_url: "", description: "" });

  const [redirects, setRedirects] = useState<any[]>([]);
  const [redirForm, setRedirForm] = useState({ source_path: "", target_path: "", status_code: 301 });

  const [webhooks, setWebhooks] = useState<any[]>([]);
  const [whForm, setWhForm] = useState({
    name: "",
    platform: "custom",
    target_url: "",
    secret: "",
    events: ["article.published", "comment.created"],
  });

  const [feedCfg, setFeedCfg] = useState<any>(null);
  const [feedVer, setFeedVer] = useState<number>(0);
  const [seoCfg, setSeoCfg] = useState<any>(null);
  const [seoVer, setSeoVer] = useState<number>(0);

  const loadSub = async () => {
    try {
      if (sub === "nav") {
        const r = await apiFetch<{ items: any[] }>("/api/admin/nav");
        setNavItems(r.items || []);
      } else if (sub === "links") {
        const r = await apiFetch<{ items: any[] }>("/api/admin/links");
        setLinks(r.items || []);
      } else if (sub === "follows") {
        const r = await apiFetch<{ items: any[] }>("/api/admin/follows");
        setFollows(r.items || []);
      } else if (sub === "redirects") {
        const r = await apiFetch<{ items: any[] }>("/api/admin/redirects");
        setRedirects(r.items || []);
      } else if (sub === "webhooks" && isAdmin) {
        const r = await apiFetch<{ items: any[] }>("/api/admin/webhooks");
        setWebhooks(r.items || []);
      } else if (sub === "feed_seo" && isAdmin) {
        const s = await apiFetch<any>("/api/admin/settings");
        setFeedCfg(s.domains?.feed || {});
        setFeedVer(s.versions?.feed || 0);
        setSeoCfg(s.domains?.seo || {});
        setSeoVer(s.versions?.seo || 0);
      }
    } catch (err: any) {
      showToast(err.message);
    }
  };

  useEffect(() => {
    void loadSub();
  }, [sub]);

  return (
    <div>
      <div class="volo-subtabs">
        {(["nav", "links", "follows", "redirects", ...(isAdmin ? ["webhooks", "feed_seo"] : [])] as DistTab[]).map((k) => (
          <button key={k} type="button" class={`volo-tab ${sub === k ? "active" : ""}`} onClick={() => setSub(k)}>
            {t(`dist.${k}`)}
          </button>
        ))}
      </div>

      {sub === "nav" && (
        <div class="volo-card">
          <h3 class="volo-card-title" style={{ marginBottom: "12px" }}>{t("dist.nav")} (Decoupled from Standalone Pages)</h3>
          <form
            onSubmit={async (e) => {
              e.preventDefault();
              const i18nTitles: Record<string, string> = {};
              if (navForm.zh) i18nTitles["zh-CN"] = navForm.zh;
              if (navForm.en) i18nTitles["en-US"] = navForm.en;
              await apiFetch("/api/admin/nav", {
                method: "POST",
                body: JSON.stringify({
                  title: navForm.title,
                  url: navForm.url,
                  i18n_titles_json: JSON.stringify(i18nTitles),
                  sort_order: Number(navForm.sort_order) || 10,
                  open_in_new_tab: navForm.open_in_new_tab,
                  is_enabled: true,
                }),
              });
              setNavForm({ title: "", url: "/", zh: "", en: "", sort_order: 10, open_in_new_tab: false });
              void loadSub();
            }}
            class="volo-grid-3"
            style={{ marginBottom: "16px" }}
          >
            <input class="volo-input" required placeholder="Default Label" value={navForm.title} onInput={(e) => setNavForm({ ...navForm, title: (e.currentTarget as HTMLInputElement).value })} />
            <input class="volo-input" required placeholder="URL (/about, /books, https://...)" value={navForm.url} onInput={(e) => setNavForm({ ...navForm, url: (e.currentTarget as HTMLInputElement).value })} />
            <div style={{ display: "flex", gap: "6px" }}>
              <input class="volo-input" placeholder="中文标题" value={navForm.zh} onInput={(e) => setNavForm({ ...navForm, zh: (e.currentTarget as HTMLInputElement).value })} />
              <input class="volo-input" placeholder="English Title" value={navForm.en} onInput={(e) => setNavForm({ ...navForm, en: (e.currentTarget as HTMLInputElement).value })} />
              <button type="submit" class="volo-btn volo-btn-primary">{t("common.create")}</button>
            </div>
          </form>
          {navItems.map((it) => (
            <div key={it.id} style={{ display: "flex", justifyContent: "space-between", padding: "9px 0", borderBottom: "1px solid var(--volo-line)" }}>
              <div><strong>{it.title}</strong> → <code>{it.url}</code> <span class="volo-help">{it.i18n_titles_json}</span></div>
              <button type="button" class="volo-btn volo-btn-sm volo-btn-danger" onClick={async () => {
                await apiFetch(`/api/admin/nav/${it.id}`, { method: "DELETE" });
                void loadSub();
              }}>×</button>
            </div>
          ))}
        </div>
      )}

      {sub === "links" && (
        <div class="volo-card">
          <div class="volo-card-head">
            <h3 class="volo-card-title">{t("dist.links")}</h3>
            <button
              type="button"
              class="volo-btn volo-btn-sm"
              onClick={async () => {
                const r = await apiFetch<{ checked: number; healthy: number }>("/api/admin/links/patrol", { method: "POST", body: "{}" });
                showToast(`Patrolled ${r.checked} links (${r.healthy} healthy)`);
                void loadSub();
              }}
            >
              Run SSRF-Safe Health Patrol
            </button>
          </div>
          <form
            onSubmit={async (e) => {
              e.preventDefault();
              await apiFetch("/api/admin/links", { method: "POST", body: JSON.stringify(linkForm) });
              setLinkForm({ name: "", url: "", avatar_url: "", description: "", category: "Friends", sort_order: 1 });
              void loadSub();
            }}
            class="volo-grid-3"
            style={{ marginBottom: "16px" }}
          >
            <input class="volo-input" required placeholder="Site Name" value={linkForm.name} onInput={(e) => setLinkForm({ ...linkForm, name: (e.currentTarget as HTMLInputElement).value })} />
            <input class="volo-input" required placeholder="https://example.com" value={linkForm.url} onInput={(e) => setLinkForm({ ...linkForm, url: (e.currentTarget as HTMLInputElement).value })} />
            <div style={{ display: "flex", gap: "8px" }}>
              <input class="volo-input" placeholder="Description" value={linkForm.description} onInput={(e) => setLinkForm({ ...linkForm, description: (e.currentTarget as HTMLInputElement).value })} />
              <button type="submit" class="volo-btn volo-btn-primary">{t("common.create")}</button>
            </div>
          </form>
          {links.map((l) => (
            <div key={l.id} style={{ display: "flex", justifyContent: "space-between", padding: "10px 0", borderBottom: "1px solid var(--volo-line)" }}>
              <div>
                <strong>{l.name}</strong> <a href={l.url} target="_blank" rel="noreferrer">{l.url}</a>{" "}
                <span class={`volo-badge ${l.health_status === "unreachable" ? "volo-badge-danger" : "volo-badge-ok"}`}>{l.health_status || "ok"}</span>
              </div>
              <button type="button" class="volo-btn volo-btn-sm volo-btn-danger" onClick={async () => {
                await apiFetch(`/api/admin/links/${l.id}`, { method: "DELETE" });
                void loadSub();
              }}>×</button>
            </div>
          ))}
        </div>
      )}

      {sub === "follows" && (
        <div class="volo-card">
          <div class="volo-card-head">
            <h3 class="volo-card-title">{t("dist.follows")}</h3>
            <button
              type="button"
              class="volo-btn volo-btn-sm"
              onClick={async () => {
                const r = await apiFetch<{ synced_items: number }>("/api/admin/follows/sync", { method: "POST", body: "{}" });
                showToast(`Synced ${r.synced_items} RSS/Atom item(s)`);
                void loadSub();
              }}
            >
              Sync All Feeds Now
            </button>
          </div>
          <form
            onSubmit={async (e) => {
              e.preventDefault();
              await apiFetch("/api/admin/follows", { method: "POST", body: JSON.stringify(followForm) });
              setFollowForm({ title: "", site_url: "", feed_url: "", description: "" });
              void loadSub();
            }}
            class="volo-grid-3"
            style={{ marginBottom: "16px" }}
          >
            <input class="volo-input" required placeholder="Blog Name" value={followForm.title} onInput={(e) => setFollowForm({ ...followForm, title: (e.currentTarget as HTMLInputElement).value })} />
            <input class="volo-input" required placeholder="RSS/Atom Feed URL" value={followForm.feed_url} onInput={(e) => setFollowForm({ ...followForm, feed_url: (e.currentTarget as HTMLInputElement).value })} />
            <div style={{ display: "flex", gap: "8px" }}>
              <input class="volo-input" placeholder="Site URL" value={followForm.site_url} onInput={(e) => setFollowForm({ ...followForm, site_url: (e.currentTarget as HTMLInputElement).value })} />
              <button type="submit" class="volo-btn volo-btn-primary">{t("common.create")}</button>
            </div>
          </form>
          {follows.map((f) => (
            <div key={f.id} style={{ display: "flex", justifyContent: "space-between", padding: "10px 0", borderBottom: "1px solid var(--volo-line)" }}>
              <div>
                <strong>{f.title}</strong> · <code>{f.feed_url}</code>{" "}
                <span class="volo-help">Last sync: {formatDateTime(f.last_synced_at)} {f.sync_error ? `(Err: ${f.sync_error})` : ""}</span>
              </div>
              <button type="button" class="volo-btn volo-btn-sm volo-btn-danger" onClick={async () => {
                await apiFetch(`/api/admin/follows/${f.id}`, { method: "DELETE" });
                void loadSub();
              }}>×</button>
            </div>
          ))}
        </div>
      )}

      {sub === "redirects" && (
        <div class="volo-card">
          <h3 class="volo-card-title" style={{ marginBottom: "12px" }}>{t("dist.redirects")} (Zero-404 Permalink Guardian)</h3>
          <form
            onSubmit={async (e) => {
              e.preventDefault();
              await apiFetch("/api/admin/redirects", { method: "POST", body: JSON.stringify(redirForm) });
              setRedirForm({ source_path: "", target_path: "", status_code: 301 });
              void loadSub();
            }}
            class="volo-grid-3"
            style={{ marginBottom: "16px" }}
          >
            <input class="volo-input" required placeholder="/old/path.html" value={redirForm.source_path} onInput={(e) => setRedirForm({ ...redirForm, source_path: (e.currentTarget as HTMLInputElement).value })} />
            <input class="volo-input" required placeholder="/posts/new-slug" value={redirForm.target_path} onInput={(e) => setRedirForm({ ...redirForm, target_path: (e.currentTarget as HTMLInputElement).value })} />
            <div style={{ display: "flex", gap: "8px" }}>
              <select class="volo-select" value={redirForm.status_code} onChange={(e) => setRedirForm({ ...redirForm, status_code: Number((e.currentTarget as HTMLSelectElement).value) })}>
                <option value={301}>301 Permanent</option>
                <option value={302}>302 Temporary</option>
              </select>
              <button type="submit" class="volo-btn volo-btn-primary">{t("common.create")}</button>
            </div>
          </form>
          {redirects.map((r) => (
            <div key={r.id} style={{ display: "flex", justifyContent: "space-between", padding: "9px 0", borderBottom: "1px solid var(--volo-line)" }}>
              <div>
                <span class="volo-badge">{r.status_code}</span> <code>{r.source_path}</code> → <code>{r.target_path}</code>{" "}
                <span class="volo-help">({r.hit_count ?? 0} hits)</span>
              </div>
              <button type="button" class="volo-btn volo-btn-sm volo-btn-danger" onClick={async () => {
                await apiFetch(`/api/admin/redirects/${r.id}`, { method: "DELETE" });
                void loadSub();
              }}>×</button>
            </div>
          ))}
        </div>
      )}

      {sub === "webhooks" && isAdmin && (
        <div class="volo-card">
          <h3 class="volo-card-title" style={{ marginBottom: "12px" }}>{t("dist.webhooks")} (Feishu · DingTalk · WeCom · Bark · Telegram · Custom HMAC)</h3>
          <form
            onSubmit={async (e) => {
              e.preventDefault();
              await apiFetch("/api/admin/webhooks", {
                method: "POST",
                body: JSON.stringify({ ...whForm, is_enabled: true }),
              });
              setWhForm({ name: "", platform: "custom", target_url: "", secret: "", events: ["article.published", "comment.created"] });
              void loadSub();
            }}
            style={{ marginBottom: "16px" }}
          >
            <div class="volo-grid-3">
              <input class="volo-input" required placeholder="Channel Name" value={whForm.name} onInput={(e) => setWhForm({ ...whForm, name: (e.currentTarget as HTMLInputElement).value })} />
              <select class="volo-select" value={whForm.platform} onChange={(e) => setWhForm({ ...whForm, platform: (e.currentTarget as HTMLSelectElement).value })}>
                <option value="custom">Custom (X-Volo-Signature-256)</option>
                <option value="feishu">Feishu / Lark Bot</option>
                <option value="dingtalk">DingTalk Signed Bot</option>
                <option value="wecom">WeCom Group Bot</option>
                <option value="bark">Bark iOS Push</option>
                <option value="telegram">Telegram Bot</option>
              </select>
              <input class="volo-input" placeholder="Signing Secret / Chat ID (AES-GCM encrypted)" value={whForm.secret} onInput={(e) => setWhForm({ ...whForm, secret: (e.currentTarget as HTMLInputElement).value })} />
            </div>
            <div style={{ display: "flex", gap: "8px", marginTop: "8px" }}>
              <input class="volo-input" required placeholder="https://open.feishu.cn/open-apis/bot/v2/hook/..." value={whForm.target_url} onInput={(e) => setWhForm({ ...whForm, target_url: (e.currentTarget as HTMLInputElement).value })} />
              <button type="submit" class="volo-btn volo-btn-primary">{t("common.create")}</button>
            </div>
          </form>
          {webhooks.map((item) => {
            const w = item.webhook || item;
            return (
              <div key={w.id} style={{ display: "flex", justifyContent: "space-between", alignItems: "center", padding: "10px 0", borderBottom: "1px solid var(--volo-line)" }}>
                <div>
                  <strong>{w.name}</strong> <span class="volo-badge">{w.platform}</span> <code>{w.target_url}</code>{" "}
                  {item.secret_set && <span class="volo-badge volo-badge-ok">Secret Sealed</span>}{" "}
                  {w.last_status_code > 0 && <span class="volo-help">HTTP {w.last_status_code}</span>}
                </div>
                <div style={{ display: "flex", gap: "6px" }}>
                  <button type="button" class="volo-btn volo-btn-sm" onClick={async () => {
                    try {
                      const r = await apiFetch<{ status_code: number }>(`/api/admin/webhooks/${w.id}/test`, { method: "POST", body: "{}" });
                      showToast(`Webhook delivered (HTTP ${r.status_code})`);
                      void loadSub();
                    } catch (err: any) {
                      showToast(err.message);
                    }
                  }}>Test</button>
                  <button type="button" class="volo-btn volo-btn-sm volo-btn-danger" onClick={async () => {
                    await apiFetch(`/api/admin/webhooks/${w.id}`, { method: "DELETE" });
                    void loadSub();
                  }}>×</button>
                </div>
              </div>
            );
          })}
        </div>
      )}

      {sub === "feed_seo" && isAdmin && feedCfg && seoCfg && (
        <div class="volo-grid-2">
          <div class="volo-card">
            <h3 class="volo-card-title" style={{ marginBottom: "12px" }}>Feed, `/llms.txt` & iTunes Podcast (`PATCH /settings/feed`)</h3>
            <div class="volo-field">
              <label>Feed Item Count</label>
              <input type="number" class="volo-input" value={feedCfg.item_count ?? 20} onInput={(e) => setFeedCfg({ ...feedCfg, item_count: Number((e.currentTarget as HTMLInputElement).value) })} />
            </div>
            <div style={{ display: "flex", flexDirection: "column", gap: "8px", marginBottom: "12px" }}>
              <label><input type="checkbox" checked={!!feedCfg.full_text_output} onChange={(e) => setFeedCfg({ ...feedCfg, full_text_output: (e.currentTarget as HTMLInputElement).checked })} /> Full-Text RSS/Atom Output</label>
              <label><input type="checkbox" checked={!!feedCfg.enable_llms_txt} onChange={(e) => setFeedCfg({ ...feedCfg, enable_llms_txt: (e.currentTarget as HTMLInputElement).checked })} /> Enable `/llms.txt` & `/llms-full.txt`</label>
              <label><input type="checkbox" checked={!!feedCfg.enable_raw_markdown} onChange={(e) => setFeedCfg({ ...feedCfg, enable_raw_markdown: (e.currentTarget as HTMLInputElement).checked })} /> Enable `/posts/&#123;slug&#125;.md`</label>
              <label><input type="checkbox" checked={!!feedCfg.podcast?.enabled} onChange={(e) => setFeedCfg({ ...feedCfg, podcast: { ...(feedCfg.podcast || {}), enabled: (e.currentTarget as HTMLInputElement).checked } })} /> Enable `/podcast.xml` (iTunes 2.0)</label>
            </div>
            <div class="volo-field">
              <label>Podcast Show Title & Cover URL</label>
              <input class="volo-input" placeholder="Podcast Title" value={feedCfg.podcast?.title || ""} onInput={(e) => setFeedCfg({ ...feedCfg, podcast: { ...(feedCfg.podcast || {}), title: (e.currentTarget as HTMLInputElement).value } })} />
              <input class="volo-input" style={{ marginTop: "6px" }} placeholder="Cover Image URL" value={feedCfg.podcast?.cover_image_url || ""} onInput={(e) => setFeedCfg({ ...feedCfg, podcast: { ...(feedCfg.podcast || {}), cover_image_url: (e.currentTarget as HTMLInputElement).value } })} />
            </div>
            <button
              type="button"
              class="volo-btn volo-btn-primary"
              onClick={async () => {
                const res = await apiFetch<any>("/api/admin/settings/feed", {
                  method: "PATCH",
                  body: JSON.stringify({ expected_version: feedVer, ...feedCfg }),
                });
                setFeedVer(res.version);
                showToast(t("common.saved"));
              }}
            >
              Save Feed & Podcast Domain
            </button>
          </div>

          <div class="volo-card">
            <h3 class="volo-card-title" style={{ marginBottom: "12px" }}>SEO, Sitemap, IndexNow & GitHub Showcase (`PATCH /settings/seo`)</h3>
            <div class="volo-field">
              <label>Default Meta Description</label>
              <textarea class="volo-textarea" rows={2} value={seoCfg.meta_description || ""} onInput={(e) => setSeoCfg({ ...seoCfg, meta_description: (e.currentTarget as HTMLTextAreaElement).value })} />
            </div>
            <div class="volo-field">
              <label>Default OG Image URL</label>
              <input class="volo-input" value={seoCfg.default_og_image_url || ""} onInput={(e) => setSeoCfg({ ...seoCfg, default_og_image_url: (e.currentTarget as HTMLInputElement).value })} />
            </div>
            <div class="volo-grid-2">
              <div class="volo-field">
                <label>IndexNow Key</label>
                <input class="volo-input" value={seoCfg.indexnow_key || ""} onInput={(e) => setSeoCfg({ ...seoCfg, indexnow_key: (e.currentTarget as HTMLInputElement).value })} />
              </div>
              <div class="volo-field">
                <label>GitHub Username (for `/github-stats/*.svg`)</label>
                <input class="volo-input" value={seoCfg.github_username || ""} onInput={(e) => setSeoCfg({ ...seoCfg, github_username: (e.currentTarget as HTMLInputElement).value })} />
              </div>
            </div>
            <label style={{ display: "inline-flex", alignItems: "center", gap: "6px", marginBottom: "12px" }}>
              <input type="checkbox" checked={!!seoCfg.indexnow_enabled} onChange={(e) => setSeoCfg({ ...seoCfg, indexnow_enabled: (e.currentTarget as HTMLInputElement).checked })} />
              Enable IndexNow Instant Push
            </label>
            <div style={{ display: "flex", gap: "8px", flexWrap: "wrap" }}>
              <button
                type="button"
                class="volo-btn volo-btn-primary"
                onClick={async () => {
                  const res = await apiFetch<any>("/api/admin/settings/seo", {
                    method: "PATCH",
                    body: JSON.stringify({ expected_version: seoVer, ...seoCfg }),
                  });
                  setSeoVer(res.version);
                  showToast(t("common.saved"));
                }}
              >
                Save SEO Domain
              </button>
              <button
                type="button"
                class="volo-btn"
                onClick={async () => {
                  const r = await apiFetch<{ repos: any[] }>("/api/admin/seo/github-sync", {
                    method: "POST",
                    body: JSON.stringify({ username: seoCfg.github_username || "" }),
                  });
                  showToast(`Synced ${r.repos?.length || 0} GitHub repositories`);
                }}
              >
                Sync GitHub Repos
              </button>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}
