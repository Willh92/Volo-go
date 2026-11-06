import { render } from "preact";
import { useEffect, useState } from "preact/hooks";
import "./styles/console.css";
import {
  accentSig,
  apiFetch,
  applyThemeAttributes,
  bootConfig,
  csrfTokenSig,
  currentUserSig,
  refreshMe,
  themeModeSig,
  toastSig,
} from "./api";
import { localeSig, normalizeLocale, t } from "./i18n";
import { SetupWizard } from "./views/SetupWizard";
import { LoginView } from "./views/LoginView";
import { OverviewWorkspace } from "./views/OverviewWorkspace";
import { ContentWorkspace } from "./views/ContentWorkspace";
import { DistributeWorkspace } from "./views/DistributeWorkspace";
import { AppearanceWorkspace } from "./views/AppearanceWorkspace";
import { SystemWorkspace } from "./views/SystemWorkspace";

type WorkspaceID = "overview" | "content" | "distribute" | "appearance" | "system";

function App() {
  const [ready, setReady] = useState(bootConfig.mode === "setup");
  const [ws, setWs] = useState<WorkspaceID>("overview");

  useEffect(() => {
    localeSig.value = normalizeLocale(bootConfig.default_locale);
    applyThemeAttributes(themeModeSig.value, accentSig.value);
    if (bootConfig.mode !== "setup") {
      refreshMe().finally(() => setReady(true));
    }
  }, []);

  if (bootConfig.mode === "setup") {
    return (
      <>
        <SetupWizard />
        {toastSig.value && <div class="volo-toast">{toastSig.value}</div>}
      </>
    );
  }

  if (!ready) {
    return <div class="volo-center-stage"><span class="volo-help">Loading Studio Console...</span></div>;
  }

  const user = currentUserSig.value;
  if (!user) {
    return (
      <>
        <LoginView />
        {toastSig.value && <div class="volo-toast">{toastSig.value}</div>}
      </>
    );
  }

  const isAdmin = user.role === "admin";
  const workspaces: WorkspaceID[] = isAdmin
    ? ["overview", "content", "distribute", "appearance", "system"]
    : ["overview", "content", "distribute", "system"];

  return (
    <div class="volo-shell">
      <aside class="volo-rail">
        <div class="volo-brand">
          <div class="volo-brand-mark">V</div>
          <div>
            <div class="volo-brand-title">{bootConfig.site_title || "Volo Studio"}</div>
            <div class="volo-brand-sub">{user.role.toUpperCase()} · @{user.username}</div>
          </div>
        </div>

        <nav class="volo-nav-list">
          {workspaces.map((id) => (
            <button
              key={id}
              type="button"
              class={`volo-nav-btn ${ws === id ? "active" : ""}`}
              onClick={() => setWs(id)}
            >
              <span>{t(`ws.${id}`)}</span>
            </button>
          ))}
        </nav>

        <div class="volo-rail-footer">
          <a class="volo-rail-link" href="/" target="_blank" rel="noreferrer">
            <span>{t("nav.view_site")}</span>
            <span>↗</span>
          </a>
          <button
            type="button"
            class="volo-nav-btn"
            onClick={async () => {
              await apiFetch("/api/admin/auth/logout", { method: "POST", body: "{}" }).catch(() => {});
              currentUserSig.value = null;
              csrfTokenSig.value = "";
            }}
          >
            {t("nav.logout")}
          </button>
        </div>
      </aside>

      <div class="volo-main">
        <header class="volo-topbar">
          <h1 class="volo-topbar-title">{t(`ws.${ws}`)}</h1>
          <div class="volo-topbar-actions">
            <select
              class="volo-select"
              style={{ width: "105px" }}
              value={themeModeSig.value}
              onChange={(e) => applyThemeAttributes((e.currentTarget as HTMLSelectElement).value, accentSig.value)}
            >
              <option value="auto">Auto</option>
              <option value="system">System</option>
              <option value="light">Light</option>
              <option value="dark">Dark</option>
            </select>
            <select
              class="volo-select"
              style={{ width: "115px" }}
              value={localeSig.value}
              onChange={async (e) => {
                const next = normalizeLocale((e.currentTarget as HTMLSelectElement).value);
                localeSig.value = next;
                await apiFetch("/api/admin/me", {
                  method: "PATCH",
                  body: JSON.stringify({ preferred_locale: next }),
                }).catch(() => {});
              }}
            >
              <option value="zh-CN">简体中文</option>
              <option value="en-US">English</option>
            </select>
          </div>
        </header>

        <main class="volo-content">
          {ws === "overview" && <OverviewWorkspace />}
          {ws === "content" && <ContentWorkspace />}
          {ws === "distribute" && <DistributeWorkspace />}
          {ws === "appearance" && isAdmin && <AppearanceWorkspace />}
          {ws === "system" && <SystemWorkspace />}
        </main>
      </div>

      {toastSig.value && <div class="volo-toast">{toastSig.value}</div>}
    </div>
  );
}

const root = document.getElementById("app");
if (root) {
  render(<App />, root);
}
