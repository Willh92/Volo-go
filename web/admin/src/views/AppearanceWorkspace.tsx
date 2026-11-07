import { useEffect, useState } from "preact/hooks";
import {
  accentSig,
  apiFetch,
  applyThemeAttributes,
  openConfirm,
  resetConsoleScroll,
  showToast,
  themeModeSig,
} from "../api";
import {
  ensureLocaleLoaded,
  exportFullTemplateJSON,
  localeSig,
  syncExternalLocales,
  t,
} from "../i18n";
import {
  AnimatedModal,
  ConsoleEmpty,
  ConsoleSelect,
  paginateItems,
  Pagination,
  SwitchToggle,
  TableSkeleton,
  UploadButton,
} from "../components/UIControls";
import { MediaPickButton } from "../components/MediaLibraryModal";

export type AppearanceTab = "themes" | "locales" | "plugins";

const CLEAR_SECRET_SENTINEL = "__VOLO_CLEAR_SECRET__";

const ACCENTS: Array<{ id: string; swatch: string }> = [
  { id: "forest", swatch: "#246b51" },
  { id: "ocean", swatch: "#2d6583" },
  { id: "teal", swatch: "#286c70" },
  { id: "amber", swatch: "#855c27" },
  { id: "plum", swatch: "#70517d" },
  { id: "rose", swatch: "#87525f" },
];

function renderAccentLabel(id: string): string {
  switch (id) {
    case "ocean":
      return t("accent.ocean");
    case "teal":
      return t("accent.teal");
    case "amber":
      return t("accent.amber");
    case "plum":
      return t("accent.plum");
    case "rose":
      return t("accent.rose");
    default:
      return t("accent.forest");
  }
}

function buildSchemaDefaults(schema: any[]): Record<string, any> {
  const out: Record<string, any> = {};
  for (const f of schema || []) {
    if (!f?.key) continue;
    const isSecret = Boolean(f.secret || f.type === "secret" || f.type === "password");
    if (isSecret) {
      out[f.key] = "";
    } else if (f.default !== undefined) {
      out[f.key] = f.default;
    } else if (f.type === "boolean") {
      out[f.key] = false;
    } else if (f.type === "number") {
      out[f.key] = 0;
    } else {
      out[f.key] = "";
    }
  }
  return out;
}

function SchemaFieldControl({
  field: f,
  values,
  onChange,
  secretKeysSet,
  namePrefix,
}: {
  field: any;
  values: Record<string, any>;
  onChange: (key: string, next: any) => void;
  secretKeysSet?: Record<string, boolean>;
  namePrefix: string;
}) {
  const label = f.label || f.key;
  const isSecret = Boolean(f.secret || f.type === "secret" || f.type === "password");

  if (f.type === "boolean") {
    return (
      <div class="volo-field">
        <SwitchToggle
          checked={Boolean(values[f.key] ?? f.default)}
          onChange={(next) => onChange(f.key, next)}
          label={label}
          hint={f.description}
        />
      </div>
    );
  }

  if (isSecret) {
    const curVal = values[f.key];
    const isClearing = curVal === CLEAR_SECRET_SENTINEL;
    const isSealed = Boolean(secretKeysSet?.[f.key]);
    return (
      <div class="volo-field">
        <label>
          {label}{" "}
          {isSealed && !isClearing && (
            <span class="volo-badge volo-badge-ok">{t("app.secret_sealed_hint")}</span>
          )}
        </label>
        {f.description && <div class="volo-help">{f.description}</div>}
        <input
          type="password"
          class="volo-input"
          name={`${namePrefix}_${f.key}`}
          autoComplete="new-password"
          disabled={isClearing}
          placeholder={isClearing ? t("app.clear_saved_secret") : "••••••••"}
          value={isClearing ? "" : String(curVal ?? "")}
          onInput={(e) => onChange(f.key, (e.currentTarget as HTMLInputElement).value)}
        />
        <div style={{ marginTop: "6px" }}>
          <SwitchToggle
            checked={isClearing}
            onChange={(clear) => onChange(f.key, clear ? CLEAR_SECRET_SENTINEL : "")}
            label={t("app.clear_saved_secret")}
          />
        </div>
      </div>
    );
  }

  return (
    <div class="volo-field">
      <label>{label}</label>
      {f.description && <div class="volo-help">{f.description}</div>}
      {f.type === "select" && Array.isArray(f.options) ? (
        <ConsoleSelect
          value={String(values[f.key] ?? f.default ?? "")}
          onChange={(v) => onChange(f.key, v)}
          options={f.options.map((op: any) =>
            typeof op === "object" && op !== null
              ? { value: String(op.value), label: String(op.label || op.value) }
              : { value: String(op), label: String(op) },
          )}
        />
      ) : f.type === "color" ? (
        (() => {
          const rawColor = String(values[f.key] ?? f.default ?? "");
          const swatchVal = /^#[0-9a-fA-F]{6}$/.test(rawColor.trim()) ? rawColor.trim() : "#246b51";
          return (
            <div class="volo-input-row">
              <input
                type="color"
                value={swatchVal}
                aria-label={label}
                style={{
                  width: "40px",
                  height: "36px",
                  padding: "2px",
                  border: "1px solid var(--volo-line)",
                  borderRadius: "8px",
                  background: "var(--volo-surface)",
                  cursor: "pointer",
                  flexShrink: 0,
                }}
                onInput={(e) => onChange(f.key, (e.currentTarget as HTMLInputElement).value)}
              />
              <input
                type="text"
                class="volo-input"
                name={`${namePrefix}_${f.key}`}
                autoComplete="off"
                placeholder="#246b51"
                value={rawColor}
                onInput={(e) => onChange(f.key, (e.currentTarget as HTMLInputElement).value)}
              />
            </div>
          );
        })()
      ) : f.type === "image" ? (
        <div class="volo-input-row">
          <input
            type="text"
            class="volo-input"
            name={`${namePrefix}_${f.key}`}
            autoComplete="off"
            value={String(values[f.key] ?? f.default ?? "")}
            onInput={(e) => onChange(f.key, (e.currentTarget as HTMLInputElement).value)}
          />
          <MediaPickButton filterKind="image" onPick={(url) => onChange(f.key, url)} />
        </div>
      ) : f.type === "textarea" ? (
        <textarea
          class="volo-textarea"
          rows={3}
          value={String(values[f.key] ?? f.default ?? "")}
          onInput={(e) => onChange(f.key, (e.currentTarget as HTMLTextAreaElement).value)}
        />
      ) : (
        <input
          type={f.type === "number" ? "number" : f.type === "url" ? "url" : "text"}
          class="volo-input"
          name={`${namePrefix}_${f.key}`}
          autoComplete="off"
          value={values[f.key] ?? f.default ?? ""}
          onInput={(e) => {
            const raw = (e.currentTarget as HTMLInputElement).value;
            onChange(f.key, f.type === "number" ? (raw === "" ? 0 : Number(raw)) : raw);
          }}
        />
      )}
    </div>
  );
}

export function AppearanceWorkspace({
  activeTab = "themes",
  onNavigate,
}: {
  activeTab?: AppearanceTab;
  onNavigate?: (sec: string) => void;
}) {
  const curLocale = localeSig.value;
  const [sub, setSub] = useState<AppearanceTab>(activeTab);
  const [loading, setLoading] = useState<boolean>(true);
  const [uploading, setUploading] = useState<boolean>(false);

  const [themes, setThemes] = useState<any[]>([]);
  const [appCfg, setAppCfg] = useState<any>(null);
  const [appVer, setAppVer] = useState<number>(0);

  const [themeModal, setThemeModal] = useState<any | null>(null);
  const [themeModalOpen, setThemeModalOpen] = useState<boolean>(false);
  const [themeCfgVals, setThemeCfgVals] = useState<Record<string, any>>({});

  const [locPacks, setLocPacks] = useState<any[]>([]);
  const [localesDir, setLocalesDir] = useState<string>("");
  const [defaultLocale, setDefaultLocale] = useState<string>("zh-CN");
  const [locPage, setLocPage] = useState<number>(1);

  const [plugins, setPlugins] = useState<any[]>([]);
  const [pluginPage, setPluginPage] = useState<number>(1);
  const [cfgModal, setCfgModal] = useState<any | null>(null);
  const [cfgModalOpen, setCfgModalOpen] = useState<boolean>(false);
  const [cfgValues, setCfgValues] = useState<Record<string, any>>({});
  const [logsModal, setLogsModal] = useState<{ id: string; logs: any[] } | null>(null);
  const [logsModalOpen, setLogsModalOpen] = useState<boolean>(false);

  useEffect(() => {
    setSub(activeTab);
    resetConsoleScroll();
  }, [activeTab]);

  const loadData = async (silent = false) => {
    if (!silent) setLoading(true);
    try {
      if (sub === "themes") {
        const [th, st] = await Promise.all([
          apiFetch<{ themes?: any[]; items?: any[]; appearance?: any }>(
            `/api/admin/themes?locale=${encodeURIComponent(localeSig.value)}`,
          ),
          apiFetch<any>("/api/admin/settings"),
        ]);
        setThemes(th.themes || th.items || []);
        setAppCfg({ ...(th.appearance || {}), ...(st.domains?.appearance || {}) });
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
        const pl = await apiFetch<{ items: any[] }>(
          `/api/admin/plugins?locale=${encodeURIComponent(localeSig.value)}`,
        );
        setPlugins(pl.items || []);
      }
    } catch (err: any) {
      showToast(err.message, "err");
    } finally {
      setLoading(false);
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
      showToast(t("common.saved"), "ok");
    } catch (err: any) {
      showToast(err.message, "err");
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

  const themeSchema: any[] = themeModal?.localized_schema || themeModal?.config_schema || [];
  const pluginSchema: any[] = cfgModal?.localized_schema || cfgModal?.config_schema || [];

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
              resetConsoleScroll();
              onNavigate?.(tab);
            }}
          >
            {tab === "themes" ? t("app.themes") : tab === "locales" ? t("app.locales") : t("app.plugins")}
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
              <UploadButton
                accept=".zip"
                primary
                small
                busy={uploading}
                label={t("app.install_theme_zip")}
                onFile={async (f) => {
                  setUploading(true);
                  try {
                    const fd = new FormData();
                    fd.append("file", f);
                    await apiFetch(`/api/admin/themes/install?locale=${encodeURIComponent(localeSig.value)}`, {
                      method: "POST",
                      body: fd,
                    });
                    showToast(t("app.theme_installed"), "ok");
                    void loadData(true);
                  } catch (err: any) {
                    showToast(err.message, "err");
                  } finally {
                    setUploading(false);
                  }
                }}
              />
            </div>

            <div class="volo-theme-grid">
              {themes.map((th) => {
                const isBuiltin = Boolean(th.is_builtin || th.builtin);
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
                          {isBuiltin ? t("app.theme_builtin") : t("app.theme_external")}
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
                            onClick={() => void activateTheme(th.id)}
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
                              const defaults = buildSchemaDefaults(schema);
                              const saved = appCfg?.theme_configs?.[th.id] || th.config_values || {};
                              setThemeCfgVals({
                                ...defaults,
                                ...(th.config_values || {}),
                                ...saved,
                              });
                              setThemeModal(th);
                              setThemeModalOpen(true);
                            }}
                          >
                            {t("app.theme_settings")}
                          </button>
                        )}
                        {!isBuiltin && !isActive && (
                          <button
                            type="button"
                            class="volo-btn volo-btn-sm volo-btn-danger"
                            onClick={async () => {
                              if (
                                !(await openConfirm({
                                  message: t("app.theme_uninstall_confirm", { id: th.id }),
                                  danger: true,
                                }))
                              ) {
                                return;
                              }
                              try {
                                await apiFetch(`/api/admin/themes/${encodeURIComponent(th.id)}`, {
                                  method: "DELETE",
                                });
                                showToast(t("common.deleted"), "ok");
                                void loadData(true);
                              } catch (err: any) {
                                showToast(err.message, "err");
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
                <ConsoleSelect
                  value={appCfg.default_mode || "auto"}
                  options={[
                    { value: "auto", label: t("app.mode_auto") },
                    { value: "system", label: t("app.mode_system") },
                    { value: "light", label: t("app.mode_light") },
                    { value: "dark", label: t("app.mode_dark") },
                  ]}
                  onChange={(v) => setAppCfg({ ...appCfg, default_mode: v })}
                />
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
                        applyThemeAttributes(themeModeSig.value, ac.id, false);
                      }}
                    >
                      <span class="volo-swatch-dot" style={{ background: ac.swatch, width: "11px", height: "11px" }} />
                      {renderAccentLabel(ac.id)}
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

            <div class="volo-form-actions">
              <a
                class="volo-btn"
                href={`/?skin=${encodeURIComponent(appCfg.active_theme || "volo-studio")}`}
                target="_blank"
                rel="noreferrer"
              >
                {t("app.preview_theme")} ↗
              </a>
              <button
                type="button"
                class="volo-btn volo-btn-primary"
                onClick={async () => {
                  try {
                    const nextAccent = appCfg.accent || accentSig.value || "forest";
                    const res = await apiFetch<any>("/api/admin/settings/appearance", {
                      method: "PATCH",
                      body: JSON.stringify({ expected_version: appVer, ...appCfg, accent: nextAccent }),
                    });
                    setAppCfg(res.value || { ...appCfg, accent: nextAccent });
                    setAppVer(res.version);
                    applyThemeAttributes(themeModeSig.value, nextAccent, false);
                    showToast(t("common.saved"), "ok");
                  } catch (err: any) {
                    showToast(err.message, "err");
                  }
                }}
              >
                {t("common.save")}
              </button>
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
              <UploadButton
                accept=".json,.zip"
                primary
                small
                busy={uploading}
                label={t("app.loc_install_btn")}
                onFile={async (f) => {
                  setUploading(true);
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
                    showToast(t("app.loc_installed"), "ok");
                  } catch (err: any) {
                    showToast(err.message, "err");
                  } finally {
                    setUploading(false);
                  }
                }}
              />
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
                    showToast(t("app.loc_reloaded"), "ok");
                  } catch (err: any) {
                    showToast(err.message, "err");
                  }
                }}
              >
                {t("app.loc_reload_btn")}
              </button>
              <button type="button" class="volo-btn volo-btn-sm" onClick={() => void downloadStarterTemplate("zh-CN")}>
                {t("app.loc_tpl_zh")}
              </button>
              <button type="button" class="volo-btn volo-btn-sm" onClick={() => void downloadStarterTemplate("en-US")}>
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
                {loading ? (
                  <TableSkeleton cols={5} />
                ) : locPacks.length === 0 ? (
                  <ConsoleEmpty colSpan={5} />
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
                        <td class="col-cat" data-label={t("app.loc_col_code")}>
                          <div class="volo-cell-meta">
                            <span class="volo-code-chip">{p.code}</span>
                            <span class="volo-cat-chip">{String(p.dir || "ltr").toUpperCase()}</span>
                          </div>
                        </td>
                        <td class="col-status" data-label={t("app.loc_col_source")}>
                          <span class={`volo-status-pill ${p.source === "external" ? "ok" : p.source === "override" ? "warn" : ""}`}>
                            <span class="volo-status-dot" />
                            {srcLabel}
                          </span>
                        </td>
                        <td class="col-num" data-label={t("app.loc_col_keys")}>
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
                                    showToast(t("common.saved"), "ok");
                                  } catch (err: any) {
                                    showToast(err.message, "err");
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
                                  if (
                                    !(await openConfirm({
                                      message: t("app.loc_confirm_del", { code: p.code }),
                                      danger: true,
                                    }))
                                  ) {
                                    return;
                                  }
                                  try {
                                    const res = await apiFetch<{ items?: any[]; locales?: any[] }>(
                                      `/api/admin/locales/${encodeURIComponent(p.code)}`,
                                      { method: "DELETE" },
                                    );
                                    if (res.locales) syncExternalLocales(res.locales);
                                    if (res.items) setLocPacks(res.items);
                                    showToast(t("app.loc_deleted"), "ok");
                                    void loadData(true);
                                  } catch (err: any) {
                                    showToast(err.message, "err");
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
          <Pagination
            total={locPacks.length}
            page={locPage}
            pageSize={12}
            onChange={(p) => {
              setLocPage(p);
              resetConsoleScroll();
            }}
          />
        </div>
      )}

      {sub === "plugins" && (
        <div class="volo-card">
          <div class="volo-sec-head">
            <div class="volo-sec-copy">
              <h3 class="volo-card-title">{t("app.plugins_head")}</h3>
              <p class="volo-sec-desc">{t("app.plugins_desc")}</p>
            </div>
            <UploadButton
              accept=".zip"
              primary
              small
              busy={uploading}
              label={t("app.install_zip")}
              onFile={async (f) => {
                setUploading(true);
                try {
                  const fd = new FormData();
                  fd.append("file", f);
                  await apiFetch(`/api/admin/plugins/install?locale=${encodeURIComponent(localeSig.value)}`, {
                    method: "POST",
                    body: fd,
                  });
                  showToast(t("app.installed_toast"), "ok");
                  void loadData(true);
                } catch (err: any) {
                  showToast(err.message, "err");
                } finally {
                  setUploading(false);
                }
              }}
            />
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
                {loading ? (
                  <TableSkeleton cols={4} />
                ) : plugins.length === 0 ? (
                  <ConsoleEmpty colSpan={4} text={t("app.plugins_empty")} />
                ) : (
                  paginateItems(plugins, pluginPage, 12).slice.map((p) => {
                    const pSchema: any[] = p.localized_schema || p.config_schema || [];
                    return (
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
                        <td class="col-cat" data-label={t("app.plugin_col_runtime")}>
                          <span class="volo-cat-chip">
                            {p.kind} · {p.runtime} · v{p.version}
                          </span>
                        </td>
                        <td class="col-status" data-label={t("art.status")}>
                          <div style={{ display: "flex", flexDirection: "column", gap: "6px", alignItems: "flex-start" }}>
                            <SwitchToggle
                              checked={Boolean(p.is_enabled)}
                              label={p.is_enabled ? t("app.enabled") : t("app.disabled")}
                              onChange={async () => {
                                try {
                                  await apiFetch(
                                    `/api/admin/plugins/${encodeURIComponent(p.id)}/${p.is_enabled ? "disable" : "enable"}`,
                                    { method: "POST", body: "{}" },
                                  );
                                  void loadData(true);
                                } catch (err: any) {
                                  showToast(err.message, "err");
                                }
                              }}
                            />
                            {p.circuit_state === "CIRCUIT_OPEN" && (
                              <span class="volo-status-pill danger">
                                <span class="volo-status-dot" />
                                {t("app.circuit_open", { count: p.failure_count })}
                              </span>
                            )}
                          </div>
                        </td>
                        <td class="col-act">
                          <div class="volo-row-actions">
                            {p.circuit_state === "CIRCUIT_OPEN" && (
                              <button
                                type="button"
                                class="volo-act-btn"
                                onClick={async () => {
                                  try {
                                    await apiFetch(`/api/admin/plugins/${encodeURIComponent(p.id)}/reset-circuit`, {
                                      method: "POST",
                                      body: "{}",
                                    });
                                    showToast(t("app.circuit_reset_toast"), "ok");
                                    void loadData(true);
                                  } catch (err: any) {
                                    showToast(err.message, "err");
                                  }
                                }}
                              >
                                {t("app.reset_circuit")}
                              </button>
                            )}
                            <button
                              type="button"
                              class="volo-act-btn"
                              onClick={async () => {
                                try {
                                  await apiFetch(`/api/admin/plugins/${encodeURIComponent(p.id)}/reload`, {
                                    method: "POST",
                                    body: "{}",
                                  });
                                  showToast(t("app.plugin_reloaded"), "ok");
                                  void loadData(true);
                                } catch (err: any) {
                                  showToast(err.message, "err");
                                }
                              }}
                            >
                              {t("app.plugin_reload")}
                            </button>
                            {pSchema.length > 0 && (
                              <button
                                type="button"
                                class="volo-act-btn"
                                onClick={() => {
                                  const defaults = buildSchemaDefaults(pSchema);
                                  setCfgModal(p);
                                  setCfgValues({ ...defaults, ...(p.public_config || {}) });
                                  setCfgModalOpen(true);
                                }}
                              >
                                {t("app.configure")}
                              </button>
                            )}
                            <button
                              type="button"
                              class="volo-act-btn"
                              onClick={async () => {
                                try {
                                  const r = await apiFetch<{ items: any[] }>(
                                    `/api/admin/plugins/${encodeURIComponent(p.id)}/logs`,
                                  );
                                  setLogsModal({ id: p.id, logs: r.items || [] });
                                  setLogsModalOpen(true);
                                } catch (err: any) {
                                  showToast(err.message, "err");
                                }
                              }}
                            >
                              {t("app.logs")}
                            </button>
                            {p.kind === "external" && !p.is_builtin && !p.builtin && (
                              <button
                                type="button"
                                class="volo-act-btn danger"
                                onClick={async () => {
                                  if (
                                    !(await openConfirm({
                                      message: t("app.confirm_uninstall", { id: p.id }),
                                      danger: true,
                                    }))
                                  ) {
                                    return;
                                  }
                                  try {
                                    await apiFetch(`/api/admin/plugins/${encodeURIComponent(p.id)}`, {
                                      method: "DELETE",
                                    });
                                    showToast(t("common.deleted"), "ok");
                                    void loadData(true);
                                  } catch (err: any) {
                                    showToast(err.message, "err");
                                  }
                                }}
                              >
                                {t("app.uninstall")}
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
          <Pagination
            total={plugins.length}
            page={pluginPage}
            pageSize={12}
            onChange={(p) => {
              setPluginPage(p);
              resetConsoleScroll();
            }}
          />
        </div>
      )}

      <AnimatedModal
        open={themeModalOpen}
        onClose={() => setThemeModalOpen(false)}
        title={t("app.theme_cfg_title", {
          name: themeModal?.display_name || themeModal?.name || themeModal?.id || "",
        })}
        maxWidth="560px"
        footer={
          <>
            <button
              type="button"
              class="volo-btn"
              style={{ marginRight: "auto" }}
              onClick={async () => {
                if (!(await openConfirm({ message: t("app.confirm_reset_cfg"), danger: false }))) return;
                setThemeCfgVals(buildSchemaDefaults(themeSchema));
              }}
            >
              {t("app.reset_defaults")}
            </button>
            <button type="button" class="volo-btn" onClick={() => setThemeModalOpen(false)}>
              {t("common.cancel")}
            </button>
            <button
              type="button"
              class="volo-btn volo-btn-primary"
              onClick={async () => {
                if (!themeModal) return;
                try {
                  const res = await apiFetch<{ appearance?: any; version?: number }>(
                    `/api/admin/themes/${encodeURIComponent(themeModal.id)}/config`,
                    {
                      method: "PUT",
                      body: JSON.stringify(themeCfgVals),
                    },
                  );
                  if (res?.appearance) setAppCfg(res.appearance);
                  if (typeof res?.version === "number") setAppVer(res.version);
                  setThemeModalOpen(false);
                  showToast(t("common.saved"), "ok");
                  void loadData(true);
                } catch (err: any) {
                  showToast(err.message, "err");
                }
              }}
            >
              {t("common.save")}
            </button>
          </>
        }
      >
        <form autoComplete="off" onSubmit={(e) => e.preventDefault()}>
          {themeSchema.map((f: any) => (
            <SchemaFieldControl
              key={f.key}
              field={f}
              values={themeCfgVals}
              onChange={(key, next) => setThemeCfgVals((prev) => ({ ...prev, [key]: next }))}
              secretKeysSet={themeModal?.secret_keys_set}
              namePrefix="theme_opt"
            />
          ))}
        </form>
      </AnimatedModal>

      <AnimatedModal
        open={cfgModalOpen}
        onClose={() => setCfgModalOpen(false)}
        title={t("app.cfg_title", { name: cfgModal?.name || cfgModal?.id || "" })}
        maxWidth="560px"
        footer={
          <>
            <button
              type="button"
              class="volo-btn"
              style={{ marginRight: "auto" }}
              onClick={async () => {
                if (!(await openConfirm({ message: t("app.confirm_reset_cfg"), danger: false }))) return;
                setCfgValues(buildSchemaDefaults(pluginSchema));
              }}
            >
              {t("app.reset_defaults")}
            </button>
            <button type="button" class="volo-btn" onClick={() => setCfgModalOpen(false)}>
              {t("common.cancel")}
            </button>
            <button
              type="button"
              class="volo-btn volo-btn-primary"
              onClick={async () => {
                if (!cfgModal) return;
                try {
                  await apiFetch(`/api/admin/plugins/${encodeURIComponent(cfgModal.id)}/config`, {
                    method: "PATCH",
                    body: JSON.stringify(cfgValues),
                  });
                  setCfgModalOpen(false);
                  showToast(t("common.saved"), "ok");
                  void loadData(true);
                } catch (err: any) {
                  showToast(err.message, "err");
                }
              }}
            >
              {t("common.save")}
            </button>
          </>
        }
      >
        <form autoComplete="off" onSubmit={(e) => e.preventDefault()}>
          {pluginSchema.map((f: any) => (
            <SchemaFieldControl
              key={f.key}
              field={f}
              values={cfgValues}
              onChange={(key, next) => setCfgValues((prev) => ({ ...prev, [key]: next }))}
              secretKeysSet={cfgModal?.secret_keys_set}
              namePrefix="plugin_cfg"
            />
          ))}
        </form>
      </AnimatedModal>

      <AnimatedModal
        open={logsModalOpen}
        onClose={() => setLogsModalOpen(false)}
        title={t("app.logs_title", { id: logsModal?.id || "" })}
        maxWidth="680px"
        footer={
          <button type="button" class="volo-btn" onClick={() => setLogsModalOpen(false)}>
            {t("common.close")}
          </button>
        }
      >
        <pre
          style={{
            maxHeight: "360px",
            overflow: "auto",
            fontSize: "12px",
            background: "var(--volo-raised)",
            padding: "12px",
            borderRadius: "8px",
            margin: 0,
          }}
        >
          {!logsModal || logsModal.logs.length === 0
            ? t("app.logs_empty")
            : logsModal.logs.map((l) => `[${l.level}] ${l.event}: ${l.message}`).join("\n")}
        </pre>
      </AnimatedModal>
    </div>
  );
}
