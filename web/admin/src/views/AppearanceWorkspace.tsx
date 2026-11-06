import { useEffect, useState } from "preact/hooks";
import { accentSig, apiFetch, applyThemeAttributes, showToast, themeModeSig } from "../api";
import { localeSig, t } from "../i18n";

const ACCENTS = ["forest", "ocean", "teal", "amber", "plum", "rose"] as const;

export function AppearanceWorkspace() {
  const [sub, setSub] = useState<"themes" | "plugins">("themes");
  const [themes, setThemes] = useState<any[]>([]);
  const [appCfg, setAppCfg] = useState<any>(null);
  const [appVer, setAppVer] = useState<number>(0);

  const [plugins, setPlugins] = useState<any[]>([]);
  const [cfgModal, setCfgModal] = useState<any | null>(null);
  const [cfgValues, setCfgValues] = useState<Record<string, any>>({});
  const [logsModal, setLogsModal] = useState<{ id: string; logs: any[] } | null>(null);

  const loadData = async () => {
    try {
      if (sub === "themes") {
        const [th, st] = await Promise.all([
          apiFetch<{ themes?: any[]; items?: any[] }>("/api/admin/themes"),
          apiFetch<any>("/api/admin/settings"),
        ]);
        setThemes(th.themes || th.items || []);
        setAppCfg(st.domains?.appearance || {});
        setAppVer(st.versions?.appearance || 0);
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
  }, [sub]);

  return (
    <div>
      <div class="volo-subtabs">
        <button type="button" class={`volo-tab ${sub === "themes" ? "active" : ""}`} onClick={() => setSub("themes")}>
          {t("app.themes")}
        </button>
        <button type="button" class={`volo-tab ${sub === "plugins" ? "active" : ""}`} onClick={() => setSub("plugins")}>
          {t("app.plugins")}
        </button>
      </div>

      {sub === "themes" && appCfg && (
        <div class="volo-card">
          <h3 class="volo-card-title" style={{ marginBottom: "14px" }}>{t("app.themes")} (`PATCH /api/admin/settings/appearance`)</h3>
          <div class="volo-grid-3">
            <div class="volo-field">
              <label>Active Public Theme</label>
              <select
                class="volo-select"
                value={appCfg.active_theme || "volo-studio"}
                onChange={(e) => setAppCfg({ ...appCfg, active_theme: (e.currentTarget as HTMLSelectElement).value })}
              >
                {themes.map((th) => (
                  <option key={th.id} value={th.id}>{th.name || th.id} ({th.version || "1.0"})</option>
                ))}
              </select>
            </div>
            <div class="volo-field">
              <label>Default Color Mode</label>
              <select
                class="volo-select"
                value={appCfg.default_mode || "auto"}
                onChange={(e) => setAppCfg({ ...appCfg, default_mode: (e.currentTarget as HTMLSelectElement).value })}
              >
                <option value="auto">Auto (Day/Night + OS)</option>
                <option value="system">Follow OS</option>
                <option value="light">Always Light</option>
                <option value="dark">Always Dark</option>
              </select>
            </div>
            <div class="volo-field">
              <label>Shared Studio Accent</label>
              <div style={{ display: "flex", gap: "6px", flexWrap: "wrap" }}>
                {ACCENTS.map((ac) => (
                  <button
                    key={ac}
                    type="button"
                    class={`volo-btn volo-btn-sm ${(appCfg.accent || accentSig.value) === ac ? "volo-btn-primary" : ""}`}
                    onClick={() => {
                      setAppCfg({ ...appCfg, accent: ac });
                      applyThemeAttributes(themeModeSig.value, ac);
                    }}
                  >
                    {ac}
                  </button>
                ))}
              </div>
            </div>
          </div>

          <div class="volo-field">
            <label>Custom Public CSS Override (scoped safely)</label>
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
              href={`/?theme_preview=${encodeURIComponent(appCfg.active_theme || "volo-studio")}`}
              target="_blank"
              rel="noreferrer"
            >
              Preview Selected Theme
            </a>
          </div>
        </div>
      )}

      {sub === "plugins" && (
        <div class="volo-card">
          <div class="volo-card-head">
            <h3 class="volo-card-title">Dual-Mode Plugins (Built-in Go + Sandboxed Goja ES2020)</h3>
            <label class="volo-btn volo-btn-primary volo-btn-sm" style={{ cursor: "pointer" }}>
              + Hot-Install Plugin (.zip)
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
                    await apiFetch("/api/admin/plugins/install", { method: "POST", body: fd });
                    showToast("Plugin verified & installed!");
                    void loadData();
                  } catch (err: any) {
                    showToast(err.message);
                  }
                }}
              />
            </label>
          </div>

          {plugins.length === 0 ? (
            <p class="volo-help">No plugins discovered yet. Upload a `.zip` package or drop a folder in `./plugins/`.</p>
          ) : (
            plugins.map((p) => (
              <div key={p.id} class="volo-card" style={{ padding: "14px", marginBottom: "10px" }}>
                <div style={{ display: "flex", justifyContent: "space-between", alignItems: "center", gap: "12px", flexWrap: "wrap" }}>
                  <div>
                    <strong>{p.name || p.id}</strong>{" "}
                    <span class="volo-badge">{p.kind} · {p.runtime} · v{p.version}</span>{" "}
                    {p.circuit_state === "CIRCUIT_OPEN" ? (
                      <span class="volo-badge volo-badge-danger">CIRCUIT_OPEN ({p.failure_count} fails)</span>
                    ) : p.is_enabled ? (
                      <span class="volo-badge volo-badge-ok">ENABLED</span>
                    ) : (
                      <span class="volo-badge">DISABLED</span>
                    )}
                    <div class="volo-help" style={{ marginTop: "4px" }}>{p.description}</div>
                    {p.last_error && <div style={{ color: "var(--volo-danger)", fontSize: "12px" }}>Last error: {p.last_error}</div>}
                  </div>
                  <div style={{ display: "flex", gap: "6px", flexWrap: "wrap" }}>
                    {p.circuit_state === "CIRCUIT_OPEN" && (
                      <button
                        type="button"
                        class="volo-btn volo-btn-sm"
                        onClick={async () => {
                          await apiFetch(`/api/admin/plugins/${p.id}/reset-circuit`, { method: "POST", body: "{}" });
                          showToast("Circuit breaker reset");
                          void loadData();
                        }}
                      >
                        Reset Circuit
                      </button>
                    )}
                    <button
                      type="button"
                      class="volo-btn volo-btn-sm"
                      onClick={async () => {
                        await apiFetch(`/api/admin/plugins/${p.id}/${p.is_enabled ? "disable" : "enable"}`, { method: "POST", body: "{}" });
                        void loadData();
                      }}
                    >
                      {p.is_enabled ? "Disable" : "Enable"}
                    </button>
                    {p.config_schema?.length > 0 && (
                      <button
                        type="button"
                        class="volo-btn volo-btn-sm"
                        onClick={() => {
                          setCfgModal(p);
                          setCfgValues({ ...(p.public_config || {}) });
                        }}
                      >
                        Configure
                      </button>
                    )}
                    <button
                      type="button"
                      class="volo-btn volo-btn-sm"
                      onClick={async () => {
                        const r = await apiFetch<{ items: any[] }>(`/api/admin/plugins/${p.id}/logs`);
                        setLogsModal({ id: p.id, logs: r.items || [] });
                      }}
                    >
                      Logs
                    </button>
                    {p.kind === "external" && (
                      <button
                        type="button"
                        class="volo-btn volo-btn-sm volo-btn-danger"
                        onClick={async () => {
                          if (!window.confirm(`Uninstall external plugin ${p.id}?`)) return;
                          await apiFetch(`/api/admin/plugins/${p.id}`, { method: "DELETE" });
                          void loadData();
                        }}
                      >
                        Uninstall
                      </button>
                    )}
                  </div>
                </div>
              </div>
            ))
          )}
        </div>
      )}

      {cfgModal && (
        <div class="volo-modal-backdrop" onClick={() => setCfgModal(null)}>
          <div class="volo-modal" onClick={(e) => e.stopPropagation()}>
            <div class="volo-card-head">
              <h3 class="volo-card-title">Configure {cfgModal.name || cfgModal.id}</h3>
              <button type="button" class="volo-btn volo-btn-sm" onClick={() => setCfgModal(null)}>Close</button>
            </div>
            {(cfgModal.config_schema || []).map((f: any) => (
              <div key={f.key} class="volo-field">
                <label>
                  {f.label || f.key}{" "}
                  {f.secret && cfgModal.secret_keys_set?.[f.key] && (
                    <span class="volo-badge volo-badge-ok">AES-GCM Sealed (leave blank to keep)</span>
                  )}
                </label>
                {f.type === "boolean" ? (
                  <input
                    type="checkbox"
                    checked={Boolean(cfgValues[f.key] ?? f.default)}
                    onChange={(e) => setCfgValues({ ...cfgValues, [f.key]: (e.currentTarget as HTMLInputElement).checked })}
                  />
                ) : f.type === "select" && f.options ? (
                  <select
                    class="volo-select"
                    value={String(cfgValues[f.key] ?? f.default ?? "")}
                    onChange={(e) => setCfgValues({ ...cfgValues, [f.key]: (e.currentTarget as HTMLSelectElement).value })}
                  >
                    {f.options.map((op: any) => (
                      <option key={op.value} value={op.value}>{op.label || op.value}</option>
                    ))}
                  </select>
                ) : (
                  <input
                    type={f.secret ? "password" : f.type === "number" ? "number" : "text"}
                    class="volo-input"
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
          </div>
        </div>
      )}

      {logsModal && (
        <div class="volo-modal-backdrop" onClick={() => setLogsModal(null)}>
          <div class="volo-modal" onClick={(e) => e.stopPropagation()}>
            <div class="volo-card-head">
              <h3 class="volo-card-title">Plugin Logs: {logsModal.id}</h3>
              <button type="button" class="volo-btn volo-btn-sm" onClick={() => setLogsModal(null)}>Close</button>
            </div>
            <pre style={{ maxHeight: "360px", overflow: "auto", fontSize: "12px", background: "var(--volo-raised)", padding: "12px", borderRadius: "8px" }}>
              {logsModal.logs.length === 0
                ? "No log entries."
                : logsModal.logs.map((l) => `[${l.level}] ${l.event}: ${l.message}`).join("\n")}
            </pre>
          </div>
        </div>
      )}
    </div>
  );
}
