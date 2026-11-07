import { useEffect, useState } from "preact/hooks";
import { accentSig, apiFetch, applyThemeAttributes, showToast, themeModeSig } from "../api";
import {
  ensureLocaleLoaded,
  exportFullTemplateJSON,
  localeSig,
  syncExternalLocales,
  t,
} from "../i18n";
import { ConsoleSelect, paginateItems, Pagination } from "../components/UIControls";

export type AppearanceTab = "themes" | "locales" | "plugins";

const ACCENTS: Array<{ id: string; swatch: string }> = [
  { id: "forest", swatch: "#246b51" },
  { id: "ocean", swatch: "#2d6583" },
  { id: "teal", swatch: "#286c70" },
  { id: "amber", swatch: "#855c27" },
  { id: "plum", swatch: "#70517d" },
  { id: "rose", swatch: "#87525f" },
];

export function AppearanceWorkspace({
  activeTab = "themes",
  onNavigate,
}: {
  activeTab?: AppearanceTab;
  onNavigate?: (sec: string) => void;
}) {
  const curLocale = localeSig.value;
  const [sub, setSub] = useState<AppearanceTab>(activeTab);
  const [themes, setThemes] = useState<any[]>([]);
  const [appCfg, setAppCfg] = useState<any>(null);
  const [appVer, setAppVer] = useState<number>(0);

  const [themeModal, setThemeModal] = useState<any | null>(null);
  const [themeCfgVals, setThemeCfgVals] = useState<Record<string, any>>({});

  // Language Pack Extensions state
  const [locPacks, setLocPacks] = useState<any[]>([]);
  const [localesDir, setLocalesDir] = useState<string>("");
  const [defaultLocale, setDefaultLocale] = useState<string>("zh-CN");
  const [locPage, setLocPage] = useState<number>(1);

  // Plugins state
  const [plugins, setPlugins] = useState<any[]>([]);
  const [pluginPage, setPluginPage] = useState<number>(1);
  const [cfgModal, setCfgModal] = useState<any | null>(null);
  const [cfgValues, setCfgValues] = useState<Record<string, any>>({});
  const [logsModal, setLogsModal] = useState<{ id: string; logs: any[] } | null>(null);

  useEffect(() => {
    setSub(activeTab);
  }, [activeTab]);

  const loadData = async () => {
    try {
      if (sub === "themes") {
        const [th, st] = await Promise.all([
          apiFetch<{ themes?: any[]; items?: any[] }>(`/api/admin/themes?locale=${encodeURIComponent(localeSig.value)}`),
          apiFetch<any>("/api/admin/settings"),
        ]);
        setThemes(th.themes || th.items || []);
        setAppCfg(st.domains?.appearance || {});
        setAppVer(st.versions?.appearance || 0);
      } else if (sub === "locales") {
        const res = await apiFetch<{
          locales_dir?: string;
          default_locale?: string;
          items?: any[];
          locales?: any[];
        }>("/api/admin/locales");
        setLocPacks(res.items || []);
        setLocalesDir(res.locales_dir || "./locales");
        setDefaultLocale(res.default_locale || "zh-CN");
        if (res.locales) {
          syncExternalLocales(res.locales);
        }
      } else {
        const pl = await apiFetch<{ items: any[] }>(`/api/admin/plugins?locale=${encodeURIComponent(localeSig.value)}`);
        setPlugins(pl.items || []);
      }
    } catch (err: any) {
      showToast(err.message);
    }
  };

  useEffect(() => {
    void loadData();
  }, [sub, curLocale]);

  const activateTheme = async (themeID: string) => {
    if (!appCfg) return;
    try {
      const nextCfg = { ...appCfg, active_theme: themeID };
      const res = await apiFetch<any>("/api/admin/settings/appearance", {
        method: "PATCH",
        body: JSON.stringify({ expected_version: appVer, ...nextCfg }),
      });
      setAppCfg(res.value || nextCfg);
      setAppVer(res.version);
      showToast(t("common.saved"));
    } catch (err: any) {
      showToast(err.message);
    }
  };

  const downloadStarterTemplate = async (base: "zh-CN" | "en-US") => {
    const jsonText = await exportFullTemplateJSON(base);
    const blob = new Blob([jsonText], { type: "application/json;charset=utf-8" });
    const a = document.createElement("a");
    a.href = URL.createObjectURL(blob);
    a.download = `volo-locale-template-${base}.json`;
    a.click();
    URL.revokeObjectURL(a.href);
  };

  return (
    <div>
      <div class="volo-subtabs">
        {(["themes", "locales", "plugins"] as AppearanceTab[]).map((tab) => (
          <button
            key={tab}
            type="button"
            class={`volo-tab ${sub === tab ? "active" : ""}`}
            onClick={() => {
              setSub(tab);
              onNavigate?.(tab);
            }}
          >
            {t(`app.${tab}`)}
          </button>
        ))}
      </div>

      {sub === "themes" && appCfg && (
        <>
          <div class="volo-card">
            <div class="volo-sec-head">
              <div class="volo-sec-copy">
                <h3 class="volo-card-title">{t("app.gallery_head")}</h3>
                <p class="volo-sec-desc">{t("app.gallery_sub")}</p>
              </div>
              <label class="volo-btn volo-btn-primary volo-btn-sm" style={{ cursor: "pointer" }}>
                {t("app.install_theme_zip")}
                <input
                  type="file"
                  accept=".zip"
                  style={{ display: "none" }}
                  onChange={async (e) => {
                    const f = (e.currentTarget as HTMLInputElement).files?.[0];
                    if (!f) return;
                    try {
                      const fd = new FormData();
                      fd.append("file", f);
                      await apiFetch(`/api/admin/themes/install?locale=${encodeURIComponent(localeSig.value)}`, {
                        method: "POST",
                        body: fd,
                      });
                      showToast(t("app.theme_installed"));
                      void loadData();
                    } catch (err: any) {
                      showToast(err.message);
                    }
                    (e.currentTarget as HTMLInputElement).value = "";
                  }}
                />
              </label>
            </div>

            <div class="volo-theme-grid">
              {themes.map((th) => {
                const isActive = (appCfg.active_theme || "volo-studio") === th.id;
                const schema: any[] = th.localized_schema || th.config_schema || [];
                const previewSrc = th.preview_url || `/themes/${encodeURIComponent(th.id)}/preview`;
                const title = th.display_name || th.name || th.id;
                const desc = th.display_description || th.description || "";
                return (
                  <div key={th.id} class={`volo-theme-card ${isActive ? "active" : ""}`}>
                    <div class="volo-theme-thumb">
                      <img src={previewSrc} alt={title} loading="lazy" />
                      <div class="volo-theme-badges">
                        {isActive && <span class="volo-badge volo-badge-ok">{t("app.theme_active_badge")}</span>}
                        <span class="volo-badge">
                          {th.builtin ? t("app.theme_builtin") : t("app.theme_external")}
                        </span>
                      </div>
                    </div>
                    <div class="volo-theme-body">
                      <div style={{ display: "flex", justifyContent: "space-between", alignItems: "baseline", gap: "8px" }}>
                        <strong style={{ fontSize: "15px" }}>{title}</strong>
                        <span class="volo-help">v{th.version || "1.0.0"} · {th.author || "Volo"}</span>
                      </div>
                      <div class="volo-help" style={{ flex: 1 }}>{desc}</div>
                      <div style={{ display: "flex", gap: "6px", flexWrap: "wrap", marginTop: "6px" }}>
                        {!isActive && (
                          <button
                            type="button"
                            class="volo-btn volo-btn-sm volo-btn-primary"
                            onClick={() => activateTheme(th.id)}
                          >
                            {t("app.theme_activate")}
                          </button>
                        )}
                        <a
                          class="volo-btn volo-btn-sm"
                          href={`/?skin=${encodeURIComponent(th.id)}`}
                          target="_blank"
                          rel="noreferrer"
                        >
                          {t("common.preview")} ↗
                        </a>
                        {schema.length > 0 && (
                          <button
                            type="button"
                            class="volo-btn volo-btn-sm"
                            onClick={() => {
                              const defaults: Record<string, any> = {};
                              schema.forEach((f: any) => {
                                if (f.default !== undefined) defaults[f.key] = f.default;
                              });
                              setThemeCfgVals({ ...defaults, ...(th.config_values || {}) });
                              setThemeModal(th);
                            }}
                          >
                            {t("app.theme_settings")}
                          </button>
                        )}
                        {!th.builtin && !isActive && (
                          <button
                            type="button"
                            class="volo-btn volo-btn-sm volo-btn-danger"
                            onClick={async () => {
                              if (!window.confirm(t("app.theme_uninstall_confirm", { id: th.id }))) return;
                              try {
                                await apiFetch(`/api/admin/themes/${encodeURIComponent(th.id)}`, { method: "DELETE" });
                                showToast(t("common.deleted"));
                                void loadData();
                              } catch (err: any) {
                                showToast(err.message);
                              }
                            }}
                          >
                            {t("app.uninstall")}
                          </button>
                        )}
                      </div>
                    </div>
                  </div>
                );
              })}
            </div>
          </div>

          <div class="volo-card">
            <div class="volo-sec-head">
              <div class="volo-sec-copy">
                <h3 class="volo-card-title">{t("app.themes_head")}</h3>
                <p class="volo-sec-desc">{t("app.themes_desc")}</p>
              </div>
            </div>
            <div class="volo-grid-2">
              <div class="volo-field">
                <label>{t("app.default_mode")}</label>
                <select
                  class="volo-select"
                  value={appCfg.default_mode || "auto"}
                  onChange={(e) => setAppCfg({ ...appCfg, default_mode: (e.currentTarget as HTMLSelectElement).value })}
                >
                  <option value="auto">{t("app.mode_auto")}</option>
                  <option value="system">{t("app.mode_system")}</option>
                  <option value="light">{t("app.mode_light")}</option>
                  <option value="dark">{t("app.mode_dark")}</option>
                </select>
              </div>
              <div class="volo-field">
                <label>{t("app.shared_accent")}</label>
                <div style={{ display: "flex", gap: "6px", flexWrap: "wrap" }}>
                  {ACCENTS.map((ac) => (
                    <button
                      key={ac.id}
                      type="button"
                      class={`volo-btn volo-btn-sm ${(appCfg.accent || accentSig.value) === ac.id ? "volo-btn-primary" : ""}`}
                      onClick={() => {
                        setAppCfg({ ...appCfg, accent: ac.id });
                        applyThemeAttributes(themeModeSig.value, ac.id, true);
                      }}
                    >
                      <span class="volo-swatch-dot" style={{ background: ac.swatch, width: "11px", height: "11px" }} />
                      {t(`accent.${ac.id}`)}
                    </button>
                  ))}
                </div>
              </div>
            </div>

            <div class="volo-field">
              <label>{t("app.custom_css")}</label>
              <textarea
                class="volo-textarea"
                rows={4}
                style={{ fontFamily: "var(--volo-mono)", fontSize: "12.5px" }}
                value={appCfg.custom_css || ""}
                onInput={(e) => setAppCfg({ ...appCfg, custom_css: (e.currentTarget as HTMLTextAreaElement).value })}
              />
            </div>

            <div style={{ display: "flex", gap: "10px", alignItems: "center" }}>
              <button
                type="button"
                class="volo-btn volo-btn-primary"
                onClick={async () => {
                  const res = await apiFetch<any>("/api/admin/settings/appearance", {
                    method: "PATCH",
                    body: JSON.stringify({ expected_version: appVer, ...appCfg }),
                  });
                  setAppVer(res.version);
                  showToast(t("common.saved"));
                }}
              >
                {t("common.save")}
              </button>
              <a
                class="volo-btn"
                href={`/?skin=${encodeURIComponent(appCfg.active_theme || "volo-studio")}`}
                target="_blank"
                rel="noreferrer"
              >
                {t("app.preview_theme")} ↗
              </a>
            </div>
          </div>
        </>
      )}

      {sub === "locales" && (
        <div class="volo-card">
          <div class="volo-sec-head">
            <div class="volo-sec-copy">
              <h3 class="volo-card-title">{t("app.loc_head")}</h3>
              <p class="volo-sec-desc">
                {t("app.loc_desc")}{" "}
                <span class="volo-code-chip" style={{ verticalAlign: "middle" }}>
                  {t("app.loc_dir_label")} {localesDir || "./locales"}
                </span>
              </p>
            </div>
            <div style={{ display: "flex", gap: "8px", flexWrap: "wrap" }}>
              <label class="volo-btn volo-btn-primary volo-btn-sm" style={{ cursor: "pointer" }}>
                {t("app.loc_install_btn")}
                <input
                  type="file"
                  accept=".json,.zip"
                  style={{ display: "none" }}
                  onChange={async (e) => {
                    const f = (e.currentTarget as HTMLInputElement).files?.[0];
                    if (!f) return;
                    try {
                      const fd = new FormData();
                      fd.append("file", f);
                      const res = await apiFetch<{ items?: any[]; locales?: any[] }>("/api/admin/locales/install", {
                        method: "POST",
                        body: fd,
                      });
                      if (res.locales) syncExternalLocales(res.locales);
                      if (res.items) setLocPacks(res.items);
                      await ensureLocaleLoaded(localeSig.value, true);
                      showToast(t("app.loc_installed"));
                    } catch (err: any) {
                      showToast(err.message);
                    }
                    (e.currentTarget as HTMLInputElement).value = "";
                  }}
                />
              </label>
              <button
                type="button"
                class="volo-btn volo-btn-sm"
                onClick={async () => {
                  try {
                    const res = await apiFetch<{ items?: any[]; locales?: any[] }>("/api/admin/locales/reload", {
                      method: "POST",
                      body: "{}",
                    });
                    if (res.locales) syncExternalLocales(res.locales);
                    if (res.items) setLocPacks(res.items);
                    await ensureLocaleLoaded(localeSig.value, true);
                    showToast(t("app.loc_reloaded"));
                  } catch (err: any) {
                    showToast(err.message);
                  }
                }}
              >
                {t("app.loc_reload_btn")}
              </button>
              <button type="button" class="volo-btn volo-btn-sm" onClick={() => downloadStarterTemplate("zh-CN")}>
                {t("app.loc_tpl_zh")}
              </button>
              <button type="button" class="volo-btn volo-btn-sm" onClick={() => downloadStarterTemplate("en-US")}>
                {t("app.loc_tpl_en")}
              </button>
            </div>
          </div>

          <div class="volo-table-wrap">
            <table class="volo-table">
              <thead>
                <tr>
                  <th class="col-fluid">{t("app.loc_col_lang")}</th>
                  <th class="col-cat">{t("app.loc_col_code")}</th>
                  <th class="col-status">{t("app.loc_col_source")}</th>
                  <th class="col-num">{t("app.loc_col_keys")}</th>
                  <th class="col-act col-act-th">{t("art.actions")}</th>
                </tr>
              </thead>
              <tbody>
                {locPacks.length === 0 ? (
                  <tr>
                    <td colSpan={5} class="volo-empty-state">{t("tbl.empty")}</td>
                  </tr>
                ) : (
                  paginateItems(locPacks, locPage, 12).slice.map((p) => {
                    const isDef = p.code === defaultLocale;
                    const isUI = p.code === curLocale;
                    const srcLabel =
                      p.source === "external"
                        ? t("app.loc_src_external")
                        : p.source === "override"
                          ? t("app.loc_src_override")
                          : t("app.loc_src_builtin");
                    return (
                      <tr key={p.code}>
                        <td class="col-fluid">
                          <div class="volo-cell-main">
                            <div class="volo-cell-title">
                              <span>{p.name || p.code}</span>
                              {isDef && <span class="volo-mini-tag">{t("app.loc_badge_default")}</span>}
                              {isUI && <span class="volo-mini-tag warn">{t("app.loc_badge_ui")}</span>}
                            </div>
                            <div class="volo-cell-meta">
                              {p.author && <span>{p.author}</span>}
                              {p.version && <span>v{p.version}</span>}
                              {p.file && <span class="volo-code-chip">{p.file}</span>}
                            </div>
                          </div>
                        </td>
                        <td class="col-cat">
                          <div class="volo-cell-meta">
                            <span class="volo-code-chip">{p.code}</span>
                            <span class="volo-cat-chip">{String(p.dir || "ltr").toUpperCase()}</span>
                          </div>
                        </td>
                        <td class="col-status">
                          <span class={`volo-status-pill ${p.source === "external" ? "ok" : p.source === "override" ? "warn" : ""}`}>
                            <span class="volo-status-dot" />
                            {srcLabel}
                          </span>
                        </td>
                        <td class="col-num">
                          <div class="volo-cell-main">
                            <span>{t("app.loc_keys_stat", { total: p.total_keys ?? 0 })}</span>
                            {p.custom_keys > 0 && (
                              <span class="volo-help">{t("app.loc_custom_stat", { custom: p.custom_keys })}</span>
                            )}
                          </div>
                        </td>
                        <td class="col-act">
                          <div class="volo-row-actions">
                            {!isUI && (
                              <button
                                type="button"
                                class="volo-act-btn"
                                onClick={async () => {
                                  const next = await ensureLocaleLoaded(p.code, true);
                                  await apiFetch("/api/admin/me", {
                                    method: "PATCH",
                                    body: JSON.stringify({ preferred_locale: next }),
                                  }).catch(() => {});
                                }}
                              >
                                {t("app.loc_use_ui")}
                              </button>
                            )}
                            {!isDef && (
                              <button
                                type="button"
                                class="volo-act-btn"
                                onClick={async () => {
                                  try {
                                    await apiFetch("/api/admin/settings/site", {
                                      method: "PATCH",
                                      body: JSON.stringify({ default_locale: p.code }),
                                    });
                                    setDefaultLocale(p.code);
                                    showToast(t("common.saved"));
                                  } catch (err: any) {
                                    showToast(err.message);
                                  }
                                }}
                              >
                                {t("app.loc_set_default")}
                              </button>
                            )}
                            <a
                              class="volo-act-btn"
                              href={`/api/admin/locales/${encodeURIComponent(p.code)}/export`}
                            >
                              {t("app.loc_export")}
                            </a>
                            {p.has_external && (
                              <button
                                type="button"
                                class="volo-act-btn danger"
                                onClick={async () => {
                                  if (!window.confirm(t("app.loc_confirm_del", { code: p.code }))) return;
                                  try {
                                    const res = await apiFetch<{ items?: any[]; locales?: any[] }>(
                                      `/api/admin/locales/${encodeURIComponent(p.code)}`,
                                      { method: "DELETE" },
                                    );
                                    if (res.locales) syncExternalLocales(res.locales);
                                    if (res.items) setLocPacks(res.items);
                                    showToast(t("app.loc_deleted"));
                                    void loadData();
                                  } catch (err: any) {
                                    showToast(err.message);
                                  }
                                }}
                              >
                                {t("app.loc_remove")}
                              </button>
                            )}
                          </div>
                        </td>
                      </tr>
                    );
                  })
                )}
              </tbody>
            </table>
          </div>
          <Pagination total={locPacks.length} page={locPage} pageSize={12} onChange={setLocPage} />
        </div>
      )}

      {sub === "plugins" && (
        <div class="volo-card">
          <div class="volo-sec-head">
            <div class="volo-sec-copy">
              <h3 class="volo-card-title">{t("app.plugins_head")}</h3>
              <p class="volo-sec-desc">{t("app.plugins_desc")}</p>
            </div>
            <label class="volo-btn volo-btn-primary volo-btn-sm" style={{ cursor: "pointer" }}>
              {t("app.install_zip")}
              <input
                type="file"
                accept=".zip"
                style={{ display: "none" }}
                onChange={async (e) => {
                  const f = (e.currentTarget as HTMLInputElement).files?.[0];
                  if (!f) return;
                  try {
                    const fd = new FormData();
                    fd.append("file", f);
                    await apiFetch(`/api/admin/plugins/install?locale=${encodeURIComponent(localeSig.value)}`, {
                      method: "POST",
                      body: fd,
                    });
                    showToast(t("app.installed_toast"));
                    void loadData();
                  } catch (err: any) {
                    showToast(err.message);
                  }
                  (e.currentTarget as HTMLInputElement).value = "";
                }}
              />
            </label>
          </div>

          <div class="volo-table-wrap">
            <table class="volo-table">
              <thead>
                <tr>
                  <th class="col-fluid">{t("app.plugin_col_info")}</th>
                  <th class="col-cat">{t("app.plugin_col_runtime")}</th>
                  <th class="col-status">{t("art.status")}</th>
                  <th class="col-act col-act-th">{t("art.actions")}</th>
                </tr>
              </thead>
              <tbody>
                {plugins.length === 0 ? (
                  <tr>
                    <td colSpan={4} class="volo-empty-state">{t("app.plugins_empty")}</td>
                  </tr>
                ) : (
                  paginateItems(plugins, pluginPage, 12).slice.map((p) => (
                    <tr key={p.id}>
                      <td class="col-fluid">
                        <div class="volo-cell-main">
                          <div class="volo-cell-title">
                            <span>{p.name || p.id}</span>
                            <span class="volo-code-chip">{p.id}</span>
                          </div>
                          {p.description && <div class="volo-cell-meta"><span>{p.description}</span></div>}
                          {p.last_error && (
                            <div style={{ color: "var(--volo-danger)", fontSize: "12px" }}>
                              {t("app.last_error", { err: p.last_error })}
                            </div>
                          )}
                        </div>
                      </td>
                      <td class="col-cat">
                        <span class="volo-cat-chip">
                          {p.kind} · {p.runtime} · v{p.version}
                        </span>
                      </td>
                      <td class="col-status">
                        {p.circuit_state === "CIRCUIT_OPEN" ? (
                          <span class="volo-status-pill danger">
                            <span class="volo-status-dot" />
                            {t("app.circuit_open", { count: p.failure_count })}
                          </span>
                        ) : p.is_enabled ? (
                          <span class="volo-status-pill ok">
                            <span class="volo-status-dot" />
                            {t("app.enabled")}
                          </span>
                        ) : (
                          <span class="volo-status-pill">
                            <span class="volo-status-dot" />
                            {t("app.disabled")}
                          </span>
                        )}
                      </td>
                      <td class="col-act">
                        <div class="volo-row-actions">
                          {p.circuit_state === "CIRCUIT_OPEN" && (
                            <button
                              type="button"
                              class="volo-act-btn"
                              onClick={async () => {
                                await apiFetch(`/api/admin/plugins/${p.id}/reset-circuit`, { method: "POST", body: "{}" });
                                showToast(t("app.circuit_reset_toast"));
                                void loadData();
                              }}
                            >
                              {t("app.reset_circuit")}
                            </button>
                          )}
                          <button
                            type="button"
                            class="volo-act-btn"
                            onClick={async () => {
                              await apiFetch(`/api/admin/plugins/${p.id}/${p.is_enabled ? "disable" : "enable"}`, {
                                method: "POST",
                                body: "{}",
                              });
                              void loadData();
                            }}
                          >
                            {p.is_enabled ? t("app.disable") : t("app.enable")}
                          </button>
                          {p.config_schema?.length > 0 && (
                            <button
                              type="button"
                              class="volo-act-btn"
                              onClick={() => {
                                setCfgModal(p);
                                setCfgValues({ ...(p.public_config || {}) });
                              }}
                            >
                              {t("app.configure")}
                            </button>
                          )}
                          <button
                            type="button"
                            class="volo-act-btn"
                            onClick={async () => {
                              const r = await apiFetch<{ items: any[] }>(`/api/admin/plugins/${p.id}/logs`);
                              setLogsModal({ id: p.id, logs: r.items || [] });
                            }}
                          >
                            {t("app.logs")}
                          </button>
                          {p.kind === "external" && (
                            <button
                              type="button"
                              class="volo-act-btn danger"
                              onClick={async () => {
                                if (!window.confirm(t("app.confirm_uninstall", { id: p.id }))) return;
                                await apiFetch(`/api/admin/plugins/${p.id}`, { method: "DELETE" });
                                void loadData();
                              }}
                            >
                              {t("app.uninstall")}
                            </button>
                          )}
                        </div>
                      </td>
                    </tr>
                  ))
                )}
              </tbody>
            </table>
          </div>
          <Pagination total={plugins.length} page={pluginPage} pageSize={12} onChange={setPluginPage} />
        </div>
      )}

      {themeModal && (
        <div class="volo-modal-backdrop" onClick={() => setThemeModal(null)}>
          <form class="volo-modal" autoComplete="off" onSubmit={(e) => e.preventDefault()} onClick={(e) => e.stopPropagation()}>
            <div class="volo-card-head">
              <h3 class="volo-card-title">
                {t("app.theme_cfg_title", { name: themeModal.display_name || themeModal.name || themeModal.id })}
              </h3>
              <button type="button" class="volo-btn volo-btn-sm" onClick={() => setThemeModal(null)}>
                {t("common.close")}
              </button>
            </div>
            {(themeModal.localized_schema || themeModal.config_schema || []).map((f: any) => (
              <div key={f.key} class="volo-field">
                <label>{f.label || f.key}</label>
                {f.description && <div class="volo-help">{f.description}</div>}
                {f.type === "boolean" ? (
                  <label class="volo-check-item" style={{ marginTop: "4px" }}>
                    <input
                      type="checkbox"
                      checked={Boolean(themeCfgVals[f.key] ?? f.default)}
                      onChange={(e) =>
                        setThemeCfgVals({ ...themeCfgVals, [f.key]: (e.currentTarget as HTMLInputElement).checked })
                      }
                    />
                    <span>{f.label || f.key}</span>
                  </label>
                ) : f.type === "select" && f.options ? (
                  <ConsoleSelect
                    value={String(themeCfgVals[f.key] ?? f.default ?? "")}
                    onChange={(v) => setThemeCfgVals({ ...themeCfgVals, [f.key]: v })}
                    options={f.options.map((op: any) => ({
                      value: String(op.value),
                      label: String(op.label || op.value),
                    }))}
                  />
                ) : f.type === "textarea" ? (
                  <textarea
                    class="volo-textarea"
                    rows={3}
                    value={String(themeCfgVals[f.key] ?? f.default ?? "")}
                    onInput={(e) =>
                      setThemeCfgVals({ ...themeCfgVals, [f.key]: (e.currentTarget as HTMLTextAreaElement).value })
                    }
                  />
                ) : (
                  <input
                    type={f.type === "number" ? "number" : "text"}
                    class="volo-input"
                    name={`theme_opt_${f.key}`}
                    autoComplete="off"
                    value={themeCfgVals[f.key] ?? f.default ?? ""}
                    onInput={(e) => {
                      const raw = (e.currentTarget as HTMLInputElement).value;
                      setThemeCfgVals({ ...themeCfgVals, [f.key]: f.type === "number" ? Number(raw) : raw });
                    }}
                  />
                )}
              </div>
            ))}
            <button
              type="button"
              class="volo-btn volo-btn-primary"
              onClick={async () => {
                try {
                  await apiFetch(`/api/admin/themes/${encodeURIComponent(themeModal.id)}/config`, {
                    method: "PUT",
                    body: JSON.stringify(themeCfgVals),
                  });
                  setThemeModal(null);
                  showToast(t("common.saved"));
                  void loadData();
                } catch (err: any) {
                  showToast(err.message);
                }
              }}
            >
              {t("common.save")}
            </button>
          </form>
        </div>
      )}

      {cfgModal && (
        <div class="volo-modal-backdrop" onClick={() => setCfgModal(null)}>
          <form class="volo-modal" autoComplete="off" onSubmit={(e) => e.preventDefault()} onClick={(e) => e.stopPropagation()}>
            <div class="volo-card-head">
              <h3 class="volo-card-title">{t("app.cfg_title", { name: cfgModal.name || cfgModal.id })}</h3>
              <button type="button" class="volo-btn volo-btn-sm" onClick={() => setCfgModal(null)}>{t("common.close")}</button>
            </div>
            {(cfgModal.config_schema || []).map((f: any) => (
              <div key={f.key} class="volo-field">
                <label>
                  {f.label || f.key}{" "}
                  {f.secret && cfgModal.secret_keys_set?.[f.key] && (
                    <span class="volo-badge volo-badge-ok">{t("app.secret_sealed_hint")}</span>
                  )}
                </label>
                {f.type === "boolean" ? (
                  <label class="volo-check-item">
                    <input
                      type="checkbox"
                      checked={Boolean(cfgValues[f.key] ?? f.default)}
                      onChange={(e) => setCfgValues({ ...cfgValues, [f.key]: (e.currentTarget as HTMLInputElement).checked })}
                    />
                    <span>{f.label || f.key}</span>
                  </label>
                ) : f.type === "select" && f.options ? (
                  <ConsoleSelect
                    value={String(cfgValues[f.key] ?? f.default ?? "")}
                    onChange={(v) => setCfgValues({ ...cfgValues, [f.key]: v })}
                    options={f.options.map((op: any) => ({
                      value: String(op.value),
                      label: String(op.label || op.value),
                    }))}
                  />
                ) : (
                  <input
                    type={f.secret ? "password" : f.type === "number" ? "number" : "text"}
                    class="volo-input"
                    name={`plugin_cfg_${f.key}`}
                    autoComplete={f.secret ? "new-password" : "off"}
                    placeholder={f.secret ? "••••••••" : ""}
                    value={cfgValues[f.key] ?? ""}
                    onInput={(e) => {
                      const raw = (e.currentTarget as HTMLInputElement).value;
                      setCfgValues({ ...cfgValues, [f.key]: f.type === "number" ? Number(raw) : raw });
                    }}
                  />
                )}
              </div>
            ))}
            <button
              type="button"
              class="volo-btn volo-btn-primary"
              onClick={async () => {
                await apiFetch(`/api/admin/plugins/${cfgModal.id}/config`, {
                  method: "PATCH",
                  body: JSON.stringify(cfgValues),
                });
                setCfgModal(null);
                showToast(t("common.saved"));
                void loadData();
              }}
            >
              {t("common.save")}
            </button>
          </form>
        </div>
      )}

      {logsModal && (
        <div class="volo-modal-backdrop" onClick={() => setLogsModal(null)}>
          <div class="volo-modal" onClick={(e) => e.stopPropagation()}>
            <div class="volo-card-head">
              <h3 class="volo-card-title">{t("app.logs_title", { id: logsModal.id })}</h3>
              <button type="button" class="volo-btn volo-btn-sm" onClick={() => setLogsModal(null)}>{t("common.close")}</button>
            </div>
            <pre style={{ maxHeight: "360px", overflow: "auto", fontSize: "12px", background: "var(--volo-raised)", padding: "12px", borderRadius: "8px" }}>
              {logsModal.logs.length === 0
                ? t("app.logs_empty")
                : logsModal.logs.map((l) => `[${l.level}] ${l.event}: ${l.message}`).join("\n")}
            </pre>
          </div>
        </div>
      )}
    </div>
  );
}
