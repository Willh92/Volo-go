import { useEffect, useState } from "preact/hooks";
import {
  apiFetch,
  b64urlToBytes,
  bytesToB64url,
  csrfTokenSig,
  currentUserSig,
  refreshMe,
  resetConsoleScroll,
  showToast,
  storageDriverSig,
} from "../api";
import { MediaPickButton } from "../components/MediaLibraryModal";
import { AnimatedModal, ConsoleSelect, NavIconPreview, Pagination, paginateItems } from "../components/UIControls";
import { availableLocalesSig, formatDateTime, t } from "../i18n";

export type SysTab =
  | "settings_site"
  | "settings_io"
  | "users"
  | "backup"
  | "account"
  | "waf"
  | "audit";

const PAT_SCOPES = [
  "articles:read",
  "articles:write",
  "pages:write",
  "media:write",
  "backup:read",
] as const;

const SOCIAL_PRESETS: Array<{ label: string; urlHint: string }> = [
  { label: "GitHub", urlHint: "https://github.com/" },
  { label: "X / Twitter", urlHint: "https://x.com/" },
  { label: "Telegram", urlHint: "https://t.me/" },
  { label: "Email", urlHint: "mailto:" },
  { label: "Bilibili", urlHint: "https://space.bilibili.com/" },
  { label: "Mastodon", urlHint: "https://mastodon.social/@" },
];

function parseCSVList(raw: string): string[] {
  return raw
    .split(/[,，\n]/)
    .map((s) => s.trim())
    .filter(Boolean);
}

function formatBytes(bytes: number): string {
  const n = Number(bytes || 0);
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`;
  return `${(n / (1024 * 1024)).toFixed(2)} MB`;
}

function renderSysTabLabel(tab: SysTab): string {
  switch (tab) {
    case "settings_site":
      return t("sys.settings_site");
    case "settings_io":
      return t("sys.settings_io");
    case "users":
      return t("sys.users");
    case "backup":
      return t("sys.backup");
    case "account":
      return t("sys.account");
    case "waf":
      return t("sys.waf");
    case "audit":
      return t("sys.audit");
  }
}

function renderRoleLabel(role: string): string {
  if (role === "admin") return t("role.admin");
  if (role === "editor") return t("role.editor");
  return t("role.reader");
}

export function SystemWorkspace({
  activeTab,
  onNavigate,
}: {
  activeTab?: SysTab;
  onNavigate?: (sec: string) => void;
}) {
  const isAdmin = currentUserSig.value?.role === "admin";
  const locales = availableLocalesSig.value;
  const defaultTab: SysTab = isAdmin ? "settings_site" : "account";
  const [sub, setSub] = useState<SysTab>(activeTab || defaultTab);

  // Secondary sub-tabs for dense security workspaces
  const [accSub, setAccSub] = useState<"profile" | "sessions">("profile");
  const [wafSub, setWafSub] = useState<"policy" | "bans" | "geo">("policy");

  // Pagination state
  const [usersPage, setUsersPage] = useState(1);
  const [bansPage, setBansPage] = useState(1);
  const [geoPage, setGeoPage] = useState(1);
  const [localBakPage, setLocalBakPage] = useState(1);
  const [auditPage, setAuditPage] = useState(1);
  const [sessPage, setSessPage] = useState(1);

  // Account Security state
  const [meInfo, setMeInfo] = useState<any>(null);
  const [profileForm, setProfileForm] = useState({
    avatar_url: currentUserSig.value?.avatar_url || "",
    display_name: "",
    bio: "",
    website_url: "",
    current_password: "",
    new_password: "",
  });
  const [avatarImgErr, setAvatarImgErr] = useState(false);
  const [totpEnroll, setTotpEnroll] = useState<{ secret: string; otpauth_uri: string } | null>(null);
  const [totpCode, setTotpCode] = useState("");
  const [recoveryCodes, setRecoveryCodes] = useState<string[] | null>(null);
  const [passkeys, setPasskeys] = useState<any[]>([]);
  const [pkModalOpen, setPkModalOpen] = useState(false);
  const [pkName, setPkName] = useState(() => t("sys.pk_default_name"));
  const [sessions, setSessions] = useState<any[]>([]);
  const [tokens, setTokens] = useState<any[]>([]);
  const [patModalOpen, setPatModalOpen] = useState(false);
  const [patName, setPatName] = useState("Obsidian CLI");
  const [patScopes, setPatScopes] = useState<string[]>(["articles:read", "articles:write", "media:write"]);
  const [issuedPAT, setIssuedPAT] = useState<string>("");

  // Atomic Domain Settings state (`domains` + `versions`)
  const [domains, setDomains] = useState<Record<string, any> | null>(null);
  const [versions, setVersions] = useState<Record<string, number>>({});
  const [smtpPwd, setSmtpPwd] = useState("");
  const [testMailTo, setTestMailTo] = useState("");
  const [s3Secret, setS3Secret] = useState("");
  const [bakWebdavPwd, setBakWebdavPwd] = useState("");
  const [bakS3Secret, setBakS3Secret] = useState("");
  const [socialDraft, setSocialDraft] = useState({ label: "", url: "" });

  // Users / WAF / Backup / Audit
  const [users, setUsers] = useState<any[]>([]);
  const [userRoleFilter, setUserRoleFilter] = useState<"all" | "admin" | "editor" | "reader">("all");
  const [userQuery, setUserQuery] = useState("");
  const [userModalOpen, setUserModalOpen] = useState(false);
  const [editingUserId, setEditingUserId] = useState<string | null>(null);
  const [userForm, setUserForm] = useState({
    username: "",
    email: "",
    display_name: "",
    role: "editor",
    status: "active",
    password: "",
  });

  const [bans, setBans] = useState<any[]>([]);
  const [banModalOpen, setBanModalOpen] = useState(false);
  const [banForm, setBanForm] = useState({ cidr: "", note: "manual", expires_hours: 24 });

  const [geoRules, setGeoRules] = useState<any[]>([]);
  const [geoCircuitEnabled, setGeoCircuitEnabled] = useState<boolean>(true);
  const [geoModalOpen, setGeoModalOpen] = useState(false);
  const [editingGeoId, setEditingGeoId] = useState<string | null>(null);
  const [geoForm, setGeoForm] = useState({
    name: "Geo Rule",
    action: "deny",
    countries: "",
    regions: "",
    sort_order: 10,
    is_enabled: true,
    on_miss: "next",
  });

  const [includeMediaExport, setIncludeMediaExport] = useState<boolean>(true);
  const [dryRunRestore, setDryRunRestore] = useState<boolean>(false);
  const [localBackups, setLocalBackups] = useState<Array<{ name: string; size_bytes: number; created_at: number }>>([]);
  const [creatingSnapshot, setCreatingSnapshot] = useState<boolean>(false);
  const [doctorReport, setDoctorReport] = useState<any>(null);
  const [auditLogs, setAuditLogs] = useState<any[]>([]);

  useEffect(() => {
    setAvatarImgErr(false);
  }, [profileForm.avatar_url]);

  useEffect(() => {
    if (activeTab) {
      setSub(activeTab);
      resetConsoleScroll();
    }
  }, [activeTab]);

  const loadSub = async () => {
    try {
      if (sub === "account") {
        const [me, pks, sess, tks] = await Promise.all([
          apiFetch<any>("/api/admin/me"),
          apiFetch<{ items: any[] }>("/api/admin/security/passkeys"),
          apiFetch<{ items: any[] }>("/api/admin/security/sessions"),
          apiFetch<{ items: any[] }>("/api/admin/security/tokens"),
        ]);
        setMeInfo(me);
        if (me.storage_driver) {
          storageDriverSig.value = me.storage_driver;
        }
        setProfileForm({
          avatar_url: me.user?.avatar_url || "",
          display_name: me.user?.display_name || "",
          bio: me.user?.bio || "",
          website_url: me.user?.website_url || "",
          current_password: "",
          new_password: "",
        });
        setPasskeys(pks.items || []);
        setSessions(sess.items || []);
        setTokens(tks.items || []);
      } else if ((sub === "settings_site" || sub === "settings_io") && isAdmin) {
        const res = await apiFetch<any>("/api/admin/settings");
        setDomains(res.domains || {});
        setVersions(res.versions || {});
        if (res.domains?.storage) {
          storageDriverSig.value = res.domains.storage.active_driver || "local";
        }
      } else if (sub === "users" && isAdmin) {
        const res = await apiFetch<{ items: any[] }>("/api/admin/users");
        setUsers(res.items || []);
      } else if (sub === "waf" && isAdmin) {
        const [b, g, st] = await Promise.all([
          apiFetch<{ items: any[] }>("/api/admin/security/ip-bans"),
          apiFetch<{ items: any[]; geo_access_circuit_enabled?: boolean }>("/api/admin/security/geo-rules"),
          apiFetch<any>("/api/admin/settings"),
        ]);
        setBans(b.items || []);
        setGeoRules(g.items || []);
        setGeoCircuitEnabled(g.geo_access_circuit_enabled !== false);
        setDomains(st.domains || {});
        setVersions(st.versions || {});
        if (st.domains?.storage) {
          storageDriverSig.value = st.domains.storage.active_driver || "local";
        }
      } else if (sub === "backup" && isAdmin) {
        const [doc, st, lb] = await Promise.all([
          apiFetch<any>("/api/admin/doctor"),
          apiFetch<any>("/api/admin/settings"),
          apiFetch<{ items: Array<{ name: string; size_bytes: number; created_at: number }> }>("/api/admin/backup/local"),
        ]);
        setDoctorReport(doc);
        setDomains(st.domains || {});
        setVersions(st.versions || {});
        setLocalBackups(lb.items || []);
        if (st.domains?.storage) {
          storageDriverSig.value = st.domains.storage.active_driver || "local";
        }
      } else if (sub === "audit" && isAdmin) {
        const logs = await apiFetch<{ items: any[] }>("/api/admin/audit-logs");
        setAuditLogs(logs.items || []);
      }
    } catch (err: any) {
      showToast(err.message);
    }
  };

  useEffect(() => {
    void loadSub();
  }, [sub]);

  const patchDomain = async (domainName: string, bodyObj: Record<string, any>) => {
    try {
      const ver = versions[domainName] ?? 0;
      const res = await apiFetch<any>(`/api/admin/settings/${domainName}`, {
        method: "PATCH",
        body: JSON.stringify({ expected_version: ver, ...bodyObj }),
      });
      setDomains((prev) => ({ ...(prev || {}), [domainName]: res.value }));
      setVersions((prev) => ({ ...prev, [domainName]: res.version }));
      if (domainName === "storage") {
        storageDriverSig.value = res.value?.active_driver || bodyObj.active_driver || "local";
      }
      showToast(t("common.saved"));
    } catch (err: any) {
      showToast(err.message);
    }
  };

  const uploadImageAsset = async (file: File, onUploaded: (url: string) => void) => {
    try {
      const fd = new FormData();
      fd.append("file", file);
      const res = await apiFetch<{
        url?: string;
        asset?: { public_url?: string };
        item?: { public_url?: string };
      }>("/api/admin/media/upload", {
        method: "POST",
        body: fd,
      });
      const nextUrl = res.url || res.asset?.public_url || res.item?.public_url || "";
      if (nextUrl) {
        onUploaded(nextUrl);
        showToast(t("ed.uploaded", { name: file.name }));
      }
    } catch (err: any) {
      showToast(err.message);
    }
  };

  const registerPasskey = async () => {
    if (!window.PublicKeyCredential || !navigator.credentials) {
      showToast(t("auth.passkey_https_only"));
      return;
    }
    try {
      const begin = await apiFetch<any>("/api/admin/security/passkeys/begin", { method: "POST", body: "{}" });
      const cred = (await navigator.credentials.create({
        publicKey: {
          challenge: b64urlToBytes(begin.challenge),
          rp: { name: "Volo-go", id: begin.rp_id },
          user: {
            id: new TextEncoder().encode(currentUserSig.value?.id || "staff"),
            name: currentUserSig.value?.username || "admin",
            displayName: currentUserSig.value?.display_name || "Admin",
          },
          pubKeyCredParams: [
            { type: "public-key", alg: -7 },
            { type: "public-key", alg: -257 },
          ],
          authenticatorSelection: { userVerification: "preferred" },
          timeout: 60000,
        },
      })) as PublicKeyCredential | null;
      if (!cred) return;
      const resp = cred.response as AuthenticatorAttestationResponse;
      await apiFetch("/api/admin/security/passkeys/finish", {
        method: "POST",
        body: JSON.stringify({
          state_token: begin.state_token,
          name: pkName || "Passkey",
          client_data_json: bytesToB64url(resp.clientDataJSON),
          attestation_object: bytesToB64url(resp.attestationObject),
        }),
      });
      setPkModalOpen(false);
      showToast(t("sys.pk_registered"));
      void loadSub();
    } catch (err: any) {
      showToast(err?.message || t("auth.passkey_cancelled"));
    }
  };

  const isSecurityGroup = sub === "account" || sub === "waf" || sub === "audit";
  const groupTabs: SysTab[] = isSecurityGroup
    ? isAdmin
      ? ["account", "waf", "audit"]
      : ["account"]
    : ["settings_site", "settings_io", "users", "backup"];

  const setDomField = (dom: string, field: string, val: any) => {
    setDomains((prev) => ({
      ...(prev || {}),
      [dom]: { ...(prev?.[dom] || {}), [field]: val },
    }));
  };

  const filteredUsers = users.filter((u) => {
    if (userRoleFilter !== "all" && u.role !== userRoleFilter) return false;
    if (userQuery.trim()) {
      const q = userQuery.trim().toLowerCase();
      return (
        String(u.username || "").toLowerCase().includes(q) ||
        String(u.display_name || "").toLowerCase().includes(q) ||
        String(u.email || "").toLowerCase().includes(q)
      );
    }
    return true;
  });

  const usersPaged = paginateItems(filteredUsers, usersPage, 15);
  const bansPaged = paginateItems(bans, bansPage, 15);
  const geoPaged = paginateItems(geoRules, geoPage, 15);
  const localBakPaged = paginateItems(localBackups, localBakPage, 10);
  const auditPaged = paginateItems(auditLogs, auditPage, 20);
  const sessPaged = paginateItems(sessions, sessPage, 10);

  return (
    <div>
      {groupTabs.length > 1 && (
        <div class="volo-subtabs">
          {groupTabs.map((k) => (
            <button
              key={k}
              type="button"
              class={`volo-tab ${sub === k ? "active" : ""}`}
              onClick={() => {
                setSub(k);
                resetConsoleScroll();
                onNavigate?.(k);
              }}
            >
              {renderSysTabLabel(k)}
            </button>
          ))}
        </div>
      )}

      {sub === "settings_site" && isAdmin && domains && (
        <div class="volo-grid-2">
          {/* Site Identity Domain */}
          <form
            class="volo-card"
            autoComplete="off"
            onSubmit={(e) => {
              e.preventDefault();
              void patchDomain("site", domains.site || {});
            }}
          >
            <div class="volo-sec-head">
              <div class="volo-sec-copy">
                <h3 class="volo-card-title">{t("sys.dom1_head")}</h3>
                <p class="volo-sec-desc">{t("sys.dom1_desc")}</p>
              </div>
            </div>
            <div class="volo-grid-2">
              <div class="volo-field">
                <label>{t("sys.lbl_site_title")}</label>
                <input
                  class="volo-input"
                  name="site_main_title"
                  placeholder={t("sys.site_title_ph")}
                  value={domains.site?.title || ""}
                  onInput={(e) => setDomField("site", "title", (e.currentTarget as HTMLInputElement).value)}
                />
              </div>
              <div class="volo-field">
                <label>{t("sys.lbl_site_sub")}</label>
                <input
                  class="volo-input"
                  name="site_sub_tagline"
                  placeholder={t("sys.site_sub_ph")}
                  value={domains.site?.subtitle || ""}
                  onInput={(e) => setDomField("site", "subtitle", (e.currentTarget as HTMLInputElement).value)}
                />
              </div>
            </div>
            <div class="volo-field">
              <label>{t("sys.site_desc")}</label>
              <textarea
                class="volo-textarea"
                rows={2}
                value={domains.site?.description || ""}
                onInput={(e) => setDomField("site", "description", (e.currentTarget as HTMLTextAreaElement).value)}
              />
            </div>
            <div class="volo-field">
              <label>{t("sys.lbl_logo_url")}</label>
              <div class="volo-input-row">
                <NavIconPreview icon={domains.site?.logo_url} fallbackLabel={domains.site?.title || "V"} />
                <input
                  class="volo-input"
                  name="site_brand_logo"
                  placeholder={t("sys.site_logo_ph")}
                  value={domains.site?.logo_url || ""}
                  onInput={(e) => setDomField("site", "logo_url", (e.currentTarget as HTMLInputElement).value)}
                />
                <label class="volo-btn volo-btn-sm" style={{ cursor: "pointer", flexShrink: 0 }}>
                  {t("sys.upload_img_btn")}
                  <input
                    type="file"
                    accept="image/*"
                    style={{ display: "none" }}
                    onChange={(e) => {
                      const f = (e.currentTarget as HTMLInputElement).files?.[0];
                      if (f) {
                        void uploadImageAsset(f, (url) => setDomField("site", "logo_url", url));
                      }
                      (e.currentTarget as HTMLInputElement).value = "";
                    }}
                  />
                </label>
                <MediaPickButton filterKind="image" onPick={(url) => setDomField("site", "logo_url", url)} />
              </div>
            </div>
            <div class="volo-field">
              <label>{t("sys.lbl_fav_url")}</label>
              <div class="volo-input-row">
                <NavIconPreview icon={domains.site?.favicon_url} fallbackLabel="F" />
                <input
                  class="volo-input"
                  name="site_favicon_path"
                  placeholder={t("sys.site_fav_ph")}
                  value={domains.site?.favicon_url || ""}
                  onInput={(e) => setDomField("site", "favicon_url", (e.currentTarget as HTMLInputElement).value)}
                />
                <label class="volo-btn volo-btn-sm" style={{ cursor: "pointer", flexShrink: 0 }}>
                  {t("sys.upload_img_btn")}
                  <input
                    type="file"
                    accept="image/*,.ico,.svg"
                    style={{ display: "none" }}
                    onChange={(e) => {
                      const f = (e.currentTarget as HTMLInputElement).files?.[0];
                      if (f) {
                        void uploadImageAsset(f, (url) => setDomField("site", "favicon_url", url));
                      }
                      (e.currentTarget as HTMLInputElement).value = "";
                    }}
                  />
                </label>
                <MediaPickButton filterKind="image" onPick={(url) => setDomField("site", "favicon_url", url)} />
              </div>
            </div>
            <div class="volo-grid-2">
              <div class="volo-field">
                <label>{t("sys.default_locale")}</label>
                <ConsoleSelect
                  value={domains.site?.default_locale || "zh-CN"}
                  options={locales.map((loc) => ({
                    value: loc.code,
                    label: loc.name,
                    badge: loc.code,
                  }))}
                  onChange={(v) => setDomField("site", "default_locale", v)}
                />
              </div>
              <div class="volo-field">
                <label>{t("setup.timezone")}</label>
                <input
                  class="volo-input"
                  name="site_iana_tz"
                  value={domains.site?.timezone || "Asia/Shanghai"}
                  onInput={(e) => setDomField("site", "timezone", (e.currentTarget as HTMLInputElement).value)}
                />
              </div>
            </div>
            <div class="volo-field">
              <label>{t("sys.notice_board")}</label>
              <textarea
                class="volo-textarea"
                rows={2}
                value={domains.site?.notice_board || ""}
                onInput={(e) => setDomField("site", "notice_board", (e.currentTarget as HTMLTextAreaElement).value)}
              />
            </div>
            <div class="volo-field">
              <label>{t("sys.license_tpl")}</label>
              <input
                class="volo-input"
                name="site_license_tpl"
                value={domains.site?.license_template || ""}
                onInput={(e) => setDomField("site", "license_template", (e.currentTarget as HTMLInputElement).value)}
              />
            </div>
            <div class="volo-field">
              <label>{t("sys.footer_text")}</label>
              <input
                class="volo-input"
                name="site_footer_note"
                value={domains.site?.footer_text || ""}
                onInput={(e) => setDomField("site", "footer_text", (e.currentTarget as HTMLInputElement).value)}
              />
            </div>
            <div class="volo-field">
              <label>{t("sys.custom_head")}</label>
              <textarea
                class="volo-textarea"
                rows={2}
                style={{ fontFamily: "var(--volo-mono)", fontSize: "12px" }}
                value={domains.site?.custom_head_meta || ""}
                onInput={(e) => setDomField("site", "custom_head_meta", (e.currentTarget as HTMLTextAreaElement).value)}
              />
            </div>

            <div class="volo-check-grid">
              <label class="volo-check-item">
                <input
                  type="checkbox"
                  checked={domains.site?.allow_locale_switch !== false}
                  onChange={(e) => setDomField("site", "allow_locale_switch", (e.currentTarget as HTMLInputElement).checked)}
                />
                <span>{t("sys.allow_locale_switch")}</span>
              </label>
              <label class="volo-check-item">
                <input
                  type="checkbox"
                  checked={!!domains.site?.show_admin_login_entry}
                  onChange={(e) => setDomField("site", "show_admin_login_entry", (e.currentTarget as HTMLInputElement).checked)}
                />
                <span>{t("sys.show_admin_entry")}</span>
              </label>
            </div>

            <div class="volo-subcard">
              <div class="volo-subcard-title">{t("sys.social_title")}</div>
              <div style={{ display: "flex", alignItems: "center", gap: "6px", flexWrap: "wrap", marginBottom: "10px" }}>
                <span class="volo-help">{t("sys.social_presets")}</span>
                {SOCIAL_PRESETS.map((p) => (
                  <button
                    key={p.label}
                    type="button"
                    class="volo-preset-chip"
                    onClick={() =>
                      setSocialDraft((prev) => ({
                        label: p.label,
                        url: prev.url || p.urlHint,
                      }))
                    }
                  >
                    {p.label}
                  </button>
                ))}
              </div>
              <div class="volo-grid-2">
                <div class="volo-field" style={{ marginBottom: "8px" }}>
                  <label>{t("sys.lbl_social_name")}</label>
                  <input
                    class="volo-input"
                    name="social_platform_label"
                    placeholder={t("sys.social_label_ph")}
                    value={socialDraft.label}
                    onInput={(e) => setSocialDraft({ ...socialDraft, label: (e.currentTarget as HTMLInputElement).value })}
                  />
                </div>
                <div class="volo-field" style={{ marginBottom: "8px" }}>
                  <label>{t("sys.lbl_social_url")}</label>
                  <div style={{ display: "flex", gap: "6px" }}>
                    <input
                      class="volo-input"
                      name="social_profile_link"
                      placeholder={t("sys.social_url_ph")}
                      value={socialDraft.url}
                      onInput={(e) => setSocialDraft({ ...socialDraft, url: (e.currentTarget as HTMLInputElement).value })}
                    />
                    <button
                      type="button"
                      class="volo-btn volo-btn-sm"
                      style={{ flexShrink: 0 }}
                      onClick={() => {
                        if (!socialDraft.label.trim() || !socialDraft.url.trim()) return;
                        const list = Array.isArray(domains.site?.social_links) ? [...domains.site.social_links] : [];
                        list.push({
                          id: `soc-${Date.now()}`,
                          label: socialDraft.label.trim(),
                          icon: socialDraft.label.trim().toLowerCase(),
                          url: socialDraft.url.trim(),
                        });
                        setDomField("site", "social_links", list);
                        setSocialDraft({ label: "", url: "" });
                      }}
                    >
                      {t("sys.social_add")}
                    </button>
                  </div>
                </div>
              </div>
              {Array.isArray(domains.site?.social_links) && domains.site.social_links.length > 0 && (
                <div style={{ display: "flex", flexDirection: "column", gap: "6px", marginTop: "8px" }}>
                  {domains.site.social_links.map((s: any, idx: number) => (
                    <div
                      key={s.id || idx}
                      style={{
                        display: "flex",
                        alignItems: "center",
                        gap: "8px",
                        padding: "6px 10px",
                        borderRadius: "8px",
                        border: "1px solid var(--volo-line)",
                        background: "var(--volo-surface)",
                      }}
                    >
                      <NavIconPreview
                        icon={s.icon || String(s.label || "").toLowerCase()}
                        fallbackLabel={s.label}
                        size={15}
                      />
                      <strong style={{ fontSize: "13px" }}>{s.label}</strong>
                      <span
                        class="volo-help"
                        style={{ flex: 1, minWidth: 0, overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }}
                      >
                        {s.url}
                      </span>
                      <button
                        type="button"
                        class="volo-act-btn danger"
                        onClick={() => {
                          const next = domains.site.social_links.filter((_: any, i: number) => i !== idx);
                          setDomField("site", "social_links", next);
                        }}
                      >
                        {t("common.delete")}
                      </button>
                    </div>
                  ))}
                </div>
              )}
            </div>

            <div class="volo-form-actions">
              <button type="submit" class="volo-btn volo-btn-primary">
                {t("sys.save_site")}
              </button>
            </div>
          </form>

          {/* Reading & Comment Protection Domain */}
          <form
            class="volo-card"
            autoComplete="off"
            onSubmit={(e) => {
              e.preventDefault();
              void patchDomain("reading", domains.reading || {});
            }}
          >
            <div class="volo-sec-head">
              <div class="volo-sec-copy">
                <h3 class="volo-card-title">{t("sys.dom2_head")}</h3>
                <p class="volo-sec-desc">{t("sys.dom2_desc")}</p>
              </div>
            </div>
            <div class="volo-grid-3">
              <div class="volo-field">
                <label>{t("sys.list_style")}</label>
                <ConsoleSelect
                  value={domains.reading?.list_style || "summary"}
                  options={[
                    { value: "summary", label: t("sys.list_summary") },
                    { value: "title_only", label: t("sys.list_title_only") },
                    { value: "full", label: t("sys.list_full") },
                  ]}
                  onChange={(v) => setDomField("reading", "list_style", v)}
                />
              </div>
              <div class="volo-field">
                <label>{t("sys.posts_per_page")}</label>
                <input
                  type="number"
                  class="volo-input"
                  value={domains.reading?.posts_per_page ?? 15}
                  onInput={(e) => setDomField("reading", "posts_per_page", Number((e.currentTarget as HTMLInputElement).value))}
                />
              </div>
              <div class="volo-field">
                <label>{t("sys.outdated_days")}</label>
                <input
                  type="number"
                  class="volo-input"
                  value={domains.reading?.outdated_notice_days ?? 365}
                  onInput={(e) => setDomField("reading", "outdated_notice_days", Number((e.currentTarget as HTMLInputElement).value))}
                />
              </div>
            </div>

            <div class="volo-grid-2">
              <div class="volo-field">
                <label>{t("sys.sb_hot_posts")}</label>
                <input
                  type="number"
                  class="volo-input"
                  value={domains.reading?.recent_posts_count ?? 8}
                  onInput={(e) => setDomField("reading", "recent_posts_count", Number((e.currentTarget as HTMLInputElement).value))}
                />
              </div>
              <div class="volo-field">
                <label>{t("sys.sb_recent_cmts")}</label>
                <input
                  type="number"
                  class="volo-input"
                  value={domains.reading?.recent_comments_count ?? 8}
                  onInput={(e) => setDomField("reading", "recent_comments_count", Number((e.currentTarget as HTMLInputElement).value))}
                />
              </div>
              <div class="volo-field">
                <label>{t("sys.sb_popular_tags")}</label>
                <input
                  type="number"
                  class="volo-input"
                  value={domains.reading?.popular_tags_count ?? 20}
                  onInput={(e) => setDomField("reading", "popular_tags_count", Number((e.currentTarget as HTMLInputElement).value))}
                />
              </div>
              <div class="volo-field">
                <label>{t("sys.sb_related_posts")}</label>
                <input
                  type="number"
                  class="volo-input"
                  value={domains.reading?.related_posts_count ?? 5}
                  onInput={(e) => setDomField("reading", "related_posts_count", Number((e.currentTarget as HTMLInputElement).value))}
                />
              </div>
            </div>

            <div class="volo-check-grid">
              <label class="volo-check-item">
                <input
                  type="checkbox"
                  checked={domains.reading?.enable_reading_stats !== false}
                  onChange={(e) => setDomField("reading", "enable_reading_stats", (e.currentTarget as HTMLInputElement).checked)}
                />
                <span>{t("sys.enable_read_stats")}</span>
              </label>
              <label class="volo-check-item">
                <input
                  type="checkbox"
                  checked={domains.reading?.enable_instant_search !== false}
                  onChange={(e) => setDomField("reading", "enable_instant_search", (e.currentTarget as HTMLInputElement).checked)}
                />
                <span>{t("sys.enable_search")}</span>
              </label>
              <label class="volo-check-item">
                <input
                  type="checkbox"
                  checked={!!domains.reading?.show_code_line_numbers}
                  onChange={(e) => setDomField("reading", "show_code_line_numbers", (e.currentTarget as HTMLInputElement).checked)}
                />
                <span>{t("sys.show_line_num")}</span>
              </label>
              <label class="volo-check-item">
                <input
                  type="checkbox"
                  checked={!!domains.reading?.allow_reader_register}
                  onChange={(e) => setDomField("reading", "allow_reader_register", (e.currentTarget as HTMLInputElement).checked)}
                />
                <span>{t("sys.allow_reader_reg")}</span>
              </label>
            </div>

            <div class="volo-subcard">
              <div class="volo-subcard-title">{t("sys.cmt_policy_title")}</div>
              <div class="volo-grid-3">
                <div class="volo-field">
                  <label>{t("sys.cmt_access")}</label>
                  <ConsoleSelect
                    value={domains.reading?.comment_access_mode || "all"}
                    options={[
                      { value: "all", label: t("sys.cmt_access_all") },
                      { value: "registered", label: t("sys.cmt_access_reg") },
                      { value: "off", label: t("sys.cmt_access_off") },
                    ]}
                    onChange={(v) => setDomField("reading", "comment_access_mode", v)}
                  />
                </div>
                <div class="volo-field">
                  <label>{t("sys.cmt_mod")}</label>
                  <ConsoleSelect
                    value={domains.reading?.comment_moderation_mode || "smart"}
                    options={[
                      { value: "smart", label: t("sys.cmt_mod_smart") },
                      { value: "all", label: t("sys.cmt_mod_all") },
                      { value: "off", label: t("sys.cmt_mod_off") },
                    ]}
                    onChange={(v) => setDomField("reading", "comment_moderation_mode", v)}
                  />
                </div>
                <div class="volo-field">
                  <label>{t("sys.pow_bits")}</label>
                  <input
                    type="number"
                    class="volo-input"
                    value={domains.reading?.pow_difficulty_bits ?? 16}
                    onInput={(e) => setDomField("reading", "pow_difficulty_bits", Number((e.currentTarget as HTMLInputElement).value))}
                  />
                </div>
              </div>
              <label class="volo-check-item" style={{ marginBottom: "10px" }}>
                <input
                  type="checkbox"
                  checked={domains.reading?.builtin_spam_filter !== false}
                  onChange={(e) => setDomField("reading", "builtin_spam_filter", (e.currentTarget as HTMLInputElement).checked)}
                />
                <span>{t("sys.spam_filter")}</span>
              </label>
              <div class="volo-grid-2">
                <div class="volo-field" style={{ marginBottom: 0 }}>
                  <label>{t("sys.blocked_kw")}</label>
                  <input
                    class="volo-input"
                    name="cmt_blocked_words"
                    value={Array.isArray(domains.reading?.blocked_keywords) ? domains.reading.blocked_keywords.join(", ") : ""}
                    onInput={(e) => setDomField("reading", "blocked_keywords", parseCSVList((e.currentTarget as HTMLInputElement).value))}
                  />
                </div>
                <div class="volo-field" style={{ marginBottom: 0 }}>
                  <label>{t("sys.allowed_kw")}</label>
                  <input
                    class="volo-input"
                    name="cmt_allowed_words"
                    value={Array.isArray(domains.reading?.allowed_keywords) ? domains.reading.allowed_keywords.join(", ") : ""}
                    onInput={(e) => setDomField("reading", "allowed_keywords", parseCSVList((e.currentTarget as HTMLInputElement).value))}
                  />
                </div>
              </div>
            </div>

            <div class="volo-form-actions">
              <button type="submit" class="volo-btn volo-btn-primary">
                {t("sys.save_reading")}
              </button>
            </div>
          </form>
        </div>
      )}

      {sub === "settings_io" && isAdmin && domains && (
        <div class="volo-grid-2">
          {/* Media Storage Engine Domain */}
          <form
            class="volo-card"
            autoComplete="off"
            onSubmit={(e) => {
              e.preventDefault();
              const body: any = { ...(domains.storage || {}) };
              if (s3Secret) body.s3_secret_key = s3Secret;
              void patchDomain("storage", body);
              setS3Secret("");
            }}
          >
            <div class="volo-sec-head">
              <div class="volo-sec-copy">
                <h3 class="volo-card-title">{t("sys.dom4_head")}</h3>
                <p class="volo-sec-desc">{t("sys.dom4_desc")}</p>
              </div>
            </div>
            <div class="volo-grid-3">
              <div class="volo-field">
                <label>{t("sys.storage_driver")}</label>
                <ConsoleSelect
                  value={domains.storage?.active_driver || "local"}
                  options={[
                    { value: "local", label: t("sys.driver_local") },
                    { value: "s3", label: t("sys.driver_s3") },
                  ]}
                  onChange={(v) => setDomField("storage", "active_driver", v)}
                />
              </div>
              <div class="volo-field">
                <label>{t("sys.max_upload_mb")}</label>
                <input
                  type="number"
                  class="volo-input"
                  value={domains.storage?.max_upload_mb ?? 50}
                  onInput={(e) => setDomField("storage", "max_upload_mb", Number((e.currentTarget as HTMLInputElement).value))}
                />
              </div>
              <div class="volo-field">
                <label>{t("sys.img_quality")}</label>
                <input
                  type="number"
                  class="volo-input"
                  value={domains.storage?.image_quality ?? 85}
                  onInput={(e) => setDomField("storage", "image_quality", Number((e.currentTarget as HTMLInputElement).value))}
                />
              </div>
            </div>

            <div class="volo-check-grid">
              <label class="volo-check-item">
                <input
                  type="checkbox"
                  checked={domains.storage?.strip_exif !== false}
                  onChange={(e) => setDomField("storage", "strip_exif", (e.currentTarget as HTMLInputElement).checked)}
                />
                <span>{t("sys.strip_exif")}</span>
              </label>
              <label class="volo-check-item">
                <input
                  type="checkbox"
                  checked={domains.storage?.adaptive_webp !== false}
                  onChange={(e) => setDomField("storage", "adaptive_webp", (e.currentTarget as HTMLInputElement).checked)}
                />
                <span>{t("sys.adaptive_webp")}</span>
              </label>
            </div>

            <div class="volo-subcard">
              <div class="volo-subcard-title">{t("sys.s3_sub_title")}</div>
              <div class="volo-grid-2">
                <div class="volo-field">
                  <label>{t("sys.s3_endpoint")}</label>
                  <input
                    class="volo-input"
                    name="storage_s3_endpoint"
                    placeholder="https://s3.amazonaws.com"
                    value={domains.storage?.s3_endpoint || ""}
                    onInput={(e) => setDomField("storage", "s3_endpoint", (e.currentTarget as HTMLInputElement).value)}
                  />
                </div>
                <div class="volo-field">
                  <label>{t("sys.lbl_s3_region")}</label>
                  <input
                    class="volo-input"
                    name="storage_s3_region"
                    placeholder={t("sys.s3_region_ph")}
                    value={domains.storage?.s3_region || ""}
                    onInput={(e) => setDomField("storage", "s3_region", (e.currentTarget as HTMLInputElement).value)}
                  />
                </div>
              </div>
              <div class="volo-grid-3">
                <div class="volo-field">
                  <label>{t("sys.lbl_s3_bucket")}</label>
                  <input
                    class="volo-input"
                    name="storage_s3_bucket"
                    placeholder={t("sys.s3_bucket_ph")}
                    value={domains.storage?.s3_bucket || ""}
                    onInput={(e) => setDomField("storage", "s3_bucket", (e.currentTarget as HTMLInputElement).value)}
                  />
                </div>
                <div class="volo-field">
                  <label>{t("sys.lbl_s3_ak")}</label>
                  <input
                    class="volo-input"
                    name="storage_s3_ak_token"
                    autoComplete="off"
                    placeholder={t("sys.s3_ak_ph")}
                    value={domains.storage?.s3_access_key || ""}
                    onInput={(e) => setDomField("storage", "s3_access_key", (e.currentTarget as HTMLInputElement).value)}
                  />
                </div>
                <div class="volo-field">
                  <label>
                    {t("sys.lbl_s3_sk")}{" "}
                    {domains.storage?.s3_secret_key_set && <span class="volo-badge volo-badge-ok">{t("sys.sealed")}</span>}
                  </label>
                  <input
                    type="password"
                    class="volo-input"
                    name="s3_sealed_secret"
                    autoComplete="new-password"
                    placeholder={domains.storage?.s3_secret_key_set ? t("sys.s3_sk_sealed_ph") : t("sys.s3_sk_ph")}
                    value={s3Secret}
                    onInput={(e) => setS3Secret((e.currentTarget as HTMLInputElement).value)}
                  />
                </div>
              </div>
              <div class="volo-grid-2">
                <div class="volo-field">
                  <label>{t("sys.lbl_s3_pub_url")}</label>
                  <input
                    class="volo-input"
                    name="storage_s3_cdn_domain"
                    placeholder={t("sys.s3_pub_url_ph")}
                    value={domains.storage?.s3_public_base_url || ""}
                    onInput={(e) => setDomField("storage", "s3_public_base_url", (e.currentTarget as HTMLInputElement).value)}
                  />
                </div>
                <div class="volo-field">
                  <label>{t("sys.lbl_s3_prefix")}</label>
                  <input
                    class="volo-input"
                    name="storage_s3_key_prefix"
                    placeholder={t("sys.s3_prefix_ph")}
                    value={domains.storage?.s3_key_prefix || ""}
                    onInput={(e) => setDomField("storage", "s3_key_prefix", (e.currentTarget as HTMLInputElement).value)}
                  />
                </div>
              </div>
              <label class="volo-check-item">
                <input
                  type="checkbox"
                  checked={!!domains.storage?.s3_path_style}
                  onChange={(e) => setDomField("storage", "s3_path_style", (e.currentTarget as HTMLInputElement).checked)}
                />
                <span>{t("sys.s3_path_style")}</span>
              </label>
            </div>

            <div class="volo-form-actions">
              <button
                type="button"
                class="volo-btn"
                onClick={async () => {
                  try {
                    await apiFetch("/api/admin/settings/storage/test", {
                      method: "POST",
                      body: JSON.stringify({ settings: domains.storage, s3_secret_key: s3Secret }),
                    });
                    showToast(t("sys.storage_ok_toast"));
                  } catch (err: any) {
                    showToast(err.message);
                  }
                }}
              >
                {t("sys.test_storage")}
              </button>
              <button type="submit" class="volo-btn volo-btn-primary">
                {t("sys.save_storage")}
              </button>
            </div>
          </form>

          {/* SMTP Mail Notifications Domain */}
          <form
            class="volo-card"
            autoComplete="off"
            onSubmit={(e) => {
              e.preventDefault();
              const body: any = { ...(domains.notifications || {}) };
              if (smtpPwd) body.smtp_password = smtpPwd;
              void patchDomain("notifications", body);
              setSmtpPwd("");
            }}
          >
            <div class="volo-sec-head">
              <div class="volo-sec-copy">
                <h3 class="volo-card-title">{t("sys.dom3_head")}</h3>
                <p class="volo-sec-desc">{t("sys.dom3_desc")}</p>
              </div>
            </div>
            <div class="volo-grid-3">
              <div class="volo-field">
                <label>{t("sys.smtp_host")}</label>
                <input
                  class="volo-input"
                  name="smtp_relay_host"
                  placeholder="smtp.example.com"
                  value={domains.notifications?.smtp_host || ""}
                  onInput={(e) => setDomField("notifications", "smtp_host", (e.currentTarget as HTMLInputElement).value)}
                />
              </div>
              <div class="volo-field">
                <label>{t("sys.smtp_port")}</label>
                <input
                  type="number"
                  class="volo-input"
                  value={domains.notifications?.smtp_port ?? 465}
                  onInput={(e) => setDomField("notifications", "smtp_port", Number((e.currentTarget as HTMLInputElement).value))}
                />
              </div>
              <div class="volo-field">
                <label>{t("sys.smtp_tls")}</label>
                <ConsoleSelect
                  value={domains.notifications?.smtp_security || "tls"}
                  options={[
                    { value: "tls", label: t("sys.tls_implicit") },
                    { value: "starttls", label: t("sys.tls_starttls") },
                    { value: "plain_internal", label: t("sys.tls_plain") },
                  ]}
                  onChange={(v) => setDomField("notifications", "smtp_security", v)}
                />
              </div>
            </div>
            <div class="volo-grid-2">
              <div class="volo-field">
                <label>{t("sys.lbl_smtp_user")}</label>
                <input
                  class="volo-input"
                  name="smtp_relay_account"
                  autoComplete="off"
                  placeholder={t("sys.smtp_user_ph")}
                  value={domains.notifications?.smtp_username || ""}
                  onInput={(e) => setDomField("notifications", "smtp_username", (e.currentTarget as HTMLInputElement).value)}
                />
              </div>
              <div class="volo-field">
                <label>
                  {t("sys.smtp_pwd")}{" "}
                  {domains.notifications?.smtp_password_set && <span class="volo-badge volo-badge-ok">{t("sys.sealed")}</span>}
                </label>
                <input
                  type="password"
                  class="volo-input"
                  name="smtp_sealed_secret"
                  autoComplete="new-password"
                  placeholder={t("sys.keep_blank_ph")}
                  value={smtpPwd}
                  onInput={(e) => setSmtpPwd((e.currentTarget as HTMLInputElement).value)}
                />
              </div>
            </div>
            <div class="volo-grid-2">
              <div class="volo-field">
                <label>{t("sys.lbl_smtp_from")}</label>
                <input
                  class="volo-input"
                  name="smtp_sender_address"
                  autoComplete="off"
                  placeholder={t("sys.smtp_from_ph")}
                  value={domains.notifications?.smtp_from_email || ""}
                  onInput={(e) => setDomField("notifications", "smtp_from_email", (e.currentTarget as HTMLInputElement).value)}
                />
              </div>
              <div class="volo-field">
                <label>{t("sys.smtp_from_name")}</label>
                <input
                  class="volo-input"
                  name="smtp_sender_title"
                  placeholder={t("sys.smtp_from_name_ph")}
                  value={domains.notifications?.smtp_from_name || ""}
                  onInput={(e) => setDomField("notifications", "smtp_from_name", (e.currentTarget as HTMLInputElement).value)}
                />
              </div>
            </div>

            <div class="volo-check-grid">
              <label class="volo-check-item">
                <input
                  type="checkbox"
                  checked={!!domains.notifications?.smtp_enabled}
                  onChange={(e) => setDomField("notifications", "smtp_enabled", (e.currentTarget as HTMLInputElement).checked)}
                />
                <span>{t("sys.enable_smtp")}</span>
              </label>
              <label class="volo-check-item">
                <input
                  type="checkbox"
                  checked={domains.notifications?.smtp_auth !== false}
                  onChange={(e) => setDomField("notifications", "smtp_auth", (e.currentTarget as HTMLInputElement).checked)}
                />
                <span>{t("sys.smtp_auth")}</span>
              </label>
              <label class="volo-check-item">
                <input
                  type="checkbox"
                  checked={domains.notifications?.notify_admin_on_comment !== false}
                  onChange={(e) => setDomField("notifications", "notify_admin_on_comment", (e.currentTarget as HTMLInputElement).checked)}
                />
                <span>{t("sys.notify_admin_cmt")}</span>
              </label>
              <label class="volo-check-item">
                <input
                  type="checkbox"
                  checked={domains.notifications?.notify_reply_by_email !== false}
                  onChange={(e) => setDomField("notifications", "notify_reply_by_email", (e.currentTarget as HTMLInputElement).checked)}
                />
                <span>{t("sys.notify_reply_mail")}</span>
              </label>
            </div>

            <div class="volo-subcard">
              <div class="volo-field" style={{ marginBottom: 0 }}>
                <label>{t("sys.lbl_test_mail")}</label>
                <input
                  class="volo-input"
                  name="smtp_probe_recipient"
                  autoComplete="off"
                  placeholder={t("sys.test_mail_ph")}
                  value={testMailTo}
                  onInput={(e) => setTestMailTo((e.currentTarget as HTMLInputElement).value)}
                />
              </div>
            </div>

            <div class="volo-form-actions">
              <button
                type="button"
                class="volo-btn"
                onClick={async () => {
                  try {
                    await apiFetch("/api/admin/settings/notifications/test-smtp", {
                      method: "POST",
                      body: JSON.stringify({ to_email: testMailTo, settings: domains.notifications, smtp_password: smtpPwd }),
                    });
                    showToast(t("sys.smtp_sent_toast"));
                  } catch (err: any) {
                    showToast(err.message);
                  }
                }}
              >
                {t("sys.test_smtp")}
              </button>
              <button type="submit" class="volo-btn volo-btn-primary">
                {t("sys.save_notify")}
              </button>
            </div>
          </form>
        </div>
      )}

      {sub === "account" && (
        <div>
          <div class="volo-seg-bar" style={{ marginBottom: "16px" }}>
            <button
              type="button"
              class={`volo-seg-btn ${accSub === "profile" ? "active" : ""}`}
              onClick={() => {
                setAccSub("profile");
                resetConsoleScroll();
              }}
            >
              {t("sys.sub_acc_profile")}
            </button>
            <button
              type="button"
              class={`volo-seg-btn ${accSub === "sessions" ? "active" : ""}`}
              onClick={() => {
                setAccSub("sessions");
                resetConsoleScroll();
              }}
            >
              {t("sys.sub_acc_sessions")}
            </button>
          </div>

          {accSub === "profile" && (
            <div class="volo-grid-2">
              <div class="volo-card">
                <div class="volo-sec-head">
                  <div class="volo-sec-copy">
                    <h3 class="volo-card-title">{t("sys.profile_head")}</h3>
                    <p class="volo-sec-desc">{t("sys.profile_desc")}</p>
                  </div>
                </div>
                <form
                  autoComplete="off"
                  onSubmit={async (e) => {
                    e.preventDefault();
                    try {
                      await apiFetch("/api/admin/me", {
                        method: "PATCH",
                        body: JSON.stringify({
                          avatar_url: profileForm.avatar_url,
                          display_name: profileForm.display_name,
                          bio: profileForm.bio,
                          website_url: profileForm.website_url,
                          current_password: profileForm.current_password,
                          new_password: profileForm.new_password,
                        }),
                      });
                      setProfileForm((prev) => ({ ...prev, current_password: "", new_password: "" }));
                      await refreshMe();
                      showToast(t("common.saved"));
                    } catch (err: any) {
                      showToast(err.message);
                    }
                  }}
                >
                  <div class="volo-field">
                    <label>{t("sys.lbl_avatar_url")}</label>
                    <div class="volo-avatar-edit-row">
                      <div class="volo-avatar-preview">
                        {profileForm.avatar_url.trim() && !avatarImgErr ? (
                          <img
                            src={profileForm.avatar_url}
                            alt={profileForm.display_name || currentUserSig.value?.username || "avatar"}
                            onError={() => setAvatarImgErr(true)}
                          />
                        ) : (
                          <span>
                            {(profileForm.display_name || currentUserSig.value?.username || "U")
                              .trim()
                              .charAt(0)
                              .toUpperCase()}
                          </span>
                        )}
                      </div>
                      <div class="volo-input-row" style={{ flex: 1, minWidth: "220px" }}>
                        <input
                          class="volo-input"
                          name="author_avatar_url"
                          autoComplete="off"
                          placeholder={t("sys.avatar_url_ph")}
                          value={profileForm.avatar_url}
                          onInput={(e) =>
                            setProfileForm((prev) => ({
                              ...prev,
                              avatar_url: (e.currentTarget as HTMLInputElement).value,
                            }))
                          }
                        />
                        <label class="volo-btn volo-btn-sm" style={{ cursor: "pointer", flexShrink: 0 }}>
                          {t("sys.upload_img_btn")}
                          <input
                            type="file"
                            accept="image/*"
                            style={{ display: "none" }}
                            onChange={(e) => {
                              const f = (e.currentTarget as HTMLInputElement).files?.[0];
                              if (f) {
                                void uploadImageAsset(f, (url) =>
                                  setProfileForm((prev) => ({ ...prev, avatar_url: url })),
                                );
                              }
                              (e.currentTarget as HTMLInputElement).value = "";
                            }}
                          />
                        </label>
                        <MediaPickButton
                          filterKind="image"
                          onPick={(url) => setProfileForm((prev) => ({ ...prev, avatar_url: url }))}
                        />
                      </div>
                    </div>
                  </div>

                  <div class="volo-grid-2">
                    <div class="volo-field">
                      <label>{t("setup.display_name")}</label>
                      <input
                        class="volo-input"
                        name="author_public_alias"
                        autoComplete="off"
                        value={profileForm.display_name}
                        onInput={(e) => setProfileForm({ ...profileForm, display_name: (e.currentTarget as HTMLInputElement).value })}
                      />
                    </div>
                    <div class="volo-field">
                      <label>{t("sys.website_url")}</label>
                      <input
                        class="volo-input"
                        type="url"
                        name="author_public_blog_url"
                        autoComplete="url"
                        placeholder="https://"
                        value={profileForm.website_url}
                        onInput={(e) => setProfileForm({ ...profileForm, website_url: (e.currentTarget as HTMLInputElement).value })}
                      />
                    </div>
                  </div>
                  <div class="volo-field">
                    <label>{t("sys.bio")}</label>
                    <textarea
                      class="volo-textarea"
                      rows={2}
                      name="author_public_bio"
                      value={profileForm.bio}
                      onInput={(e) => setProfileForm({ ...profileForm, bio: (e.currentTarget as HTMLTextAreaElement).value })}
                    />
                  </div>
                  <div class="volo-grid-2">
                    <div class="volo-field">
                      <label>{t("sys.cur_pwd")}</label>
                      <input
                        type="password"
                        class="volo-input"
                        name="staff_old_passphrase"
                        autoComplete="new-password"
                        value={profileForm.current_password}
                        onInput={(e) => setProfileForm({ ...profileForm, current_password: (e.currentTarget as HTMLInputElement).value })}
                      />
                    </div>
                    <div class="volo-field">
                      <label>{t("sys.new_pwd")}</label>
                      <input
                        type="password"
                        class="volo-input"
                        name="staff_next_passphrase"
                        autoComplete="new-password"
                        minLength={6}
                        value={profileForm.new_password}
                        onInput={(e) => setProfileForm({ ...profileForm, new_password: (e.currentTarget as HTMLInputElement).value })}
                      />
                    </div>
                  </div>
                  <div class="volo-form-actions">
                    <button type="submit" class="volo-btn volo-btn-primary">
                      {t("common.save")}
                    </button>
                  </div>
                </form>
              </div>

              <div class="volo-card">
                <div class="volo-sec-head">
                  <div class="volo-sec-copy">
                    <h3 class="volo-card-title">{t("sys.totp_head")}</h3>
                    <p class="volo-sec-desc">{t("sys.totp_desc")}</p>
                  </div>
                </div>
                <div class="volo-subcard" style={{ marginTop: 0, marginBottom: 0 }}>
                  {meInfo?.totp_enabled ? (
                    <div style={{ display: "flex", alignItems: "center", justifyContent: "space-between", gap: "10px", flexWrap: "wrap" }}>
                      <span class="volo-status-pill ok">
                        <span class="volo-status-dot" />
                        {t("sys.totp_enabled")}
                      </span>
                      <button
                        type="button"
                        class="volo-act-btn danger"
                        onClick={async () => {
                          if (!window.confirm(t("sys.confirm_disable_totp"))) return;
                          try {
                            await apiFetch("/api/admin/security/totp", { method: "DELETE" });
                            void loadSub();
                          } catch (err: any) {
                            showToast(err.message);
                          }
                        }}
                      >
                        {t("sys.totp_disable")}
                      </button>
                    </div>
                  ) : totpEnroll ? (
                    <div>
                      <div class="volo-help" style={{ wordBreak: "break-all", marginBottom: "10px" }}>
                        {t("sys.totp_secret")} <code>{totpEnroll.secret}</code>
                        <br />
                        {t("sys.totp_uri")} <code>{totpEnroll.otpauth_uri}</code>
                      </div>
                      <div class="volo-field">
                        <label>{t("sys.totp_code_ph")}</label>
                        <input
                          class="volo-input"
                          name="totp_verify_digits"
                          autoComplete="one-time-code"
                          placeholder={t("sys.totp_code_ph")}
                          value={totpCode}
                          onInput={(e) => setTotpCode((e.currentTarget as HTMLInputElement).value)}
                        />
                      </div>
                      <div class="volo-form-actions">
                        <button
                          type="button"
                          class="volo-btn"
                          onClick={() => {
                            setTotpEnroll(null);
                            setTotpCode("");
                          }}
                        >
                          {t("common.cancel")}
                        </button>
                        <button
                          type="button"
                          class="volo-btn volo-btn-primary"
                          onClick={async () => {
                            try {
                              const res = await apiFetch<{ recovery_codes: string[] }>("/api/admin/security/totp/enroll", {
                                method: "POST",
                                body: JSON.stringify({ secret: totpEnroll.secret, code: totpCode }),
                              });
                              setRecoveryCodes(res.recovery_codes || []);
                              setTotpEnroll(null);
                              setTotpCode("");
                              void loadSub();
                            } catch (err: any) {
                              showToast(err.message);
                            }
                          }}
                        >
                          {t("sys.totp_activate")}
                        </button>
                      </div>
                    </div>
                  ) : (
                    <div class="volo-form-actions no-border" style={{ marginTop: 0, paddingTop: 0 }}>
                      <button
                        type="button"
                        class="volo-btn volo-btn-sm volo-btn-primary"
                        onClick={async () => {
                          try {
                            const res = await apiFetch<{ secret: string; otpauth_uri: string }>("/api/admin/security/totp/setup", {
                              method: "POST",
                              body: "{}",
                            });
                            setTotpEnroll(res);
                          } catch (err: any) {
                            showToast(err.message);
                          }
                        }}
                      >
                        {t("sys.totp_enroll")}
                      </button>
                    </div>
                  )}
                  {recoveryCodes && (
                    <pre
                      style={{
                        background: "var(--volo-surface)",
                        padding: "10px",
                        borderRadius: "8px",
                        fontSize: "12px",
                        marginTop: "10px",
                        whiteSpace: "pre-wrap",
                      }}
                    >
                      {t("sys.recovery_codes_tip")}
                      {"\n"}
                      {recoveryCodes.join("   ")}
                    </pre>
                  )}
                </div>
              </div>
            </div>
          )}

          {accSub === "sessions" && (
            <>
              <div class="volo-grid-2">
                <div class="volo-card">
                  <div class="volo-sec-head">
                    <div class="volo-sec-copy">
                      <h3 class="volo-card-title">{t("sys.pk_col_device")}</h3>
                      <p class="volo-sec-desc">{t("sys.totp_desc")}</p>
                    </div>
                    <button
                      type="button"
                      class="volo-btn volo-btn-primary volo-btn-sm"
                      onClick={() => {
                        setPkName(t("sys.pk_default_name"));
                        setPkModalOpen(true);
                      }}
                    >
                      {t("sys.pk_add")}
                    </button>
                  </div>

                  <div class="volo-table-wrap">
                    <table class="volo-table" style={{ minWidth: "100%" }}>
                      <thead>
                        <tr>
                          <th>{t("sys.pk_col_device")}</th>
                          <th class="col-date">{t("sys.col_created")}</th>
                          <th class="col-act col-act-th">{t("art.actions")}</th>
                        </tr>
                      </thead>
                      <tbody>
                        {passkeys.length === 0 ? (
                          <tr>
                            <td colSpan={3} class="volo-empty-state">
                              {t("tbl.empty")}
                            </td>
                          </tr>
                        ) : (
                          passkeys.map((pk) => (
                            <tr key={pk.id}>
                              <td>
                                <div class="volo-cell-title">{pk.name}</div>
                              </td>
                              <td class="col-date">{formatDateTime(pk.created_at)}</td>
                              <td class="col-act">
                                <div class="volo-row-actions">
                                  <button
                                    type="button"
                                    class="volo-act-btn danger"
                                    onClick={async () => {
                                      try {
                                        await apiFetch(`/api/admin/security/passkeys/${pk.id}`, { method: "DELETE" });
                                        void loadSub();
                                      } catch (err: any) {
                                        showToast(err.message);
                                      }
                                    }}
                                  >
                                    {t("common.delete")}
                                  </button>
                                </div>
                              </td>
                            </tr>
                          ))
                        )}
                      </tbody>
                    </table>
                  </div>
                </div>

                <div class="volo-card">
                  <div class="volo-sec-head">
                    <div class="volo-sec-copy">
                      <h3 class="volo-card-title">{t("sys.pat_head")}</h3>
                      <p class="volo-sec-desc">{t("sys.pat_desc")}</p>
                    </div>
                    <button
                      type="button"
                      class="volo-btn volo-btn-primary volo-btn-sm"
                      onClick={() => {
                        setIssuedPAT("");
                        setPatName("Obsidian CLI");
                        setPatScopes(["articles:read", "articles:write", "media:write"]);
                        setPatModalOpen(true);
                      }}
                    >
                      + {t("sys.pat_generate")}
                    </button>
                  </div>

                  {issuedPAT && !patModalOpen && (
                    <div
                      class="volo-subcard"
                      style={{
                        marginTop: 0,
                        background: "var(--volo-accent-soft)",
                        wordBreak: "break-all",
                        fontSize: "12px",
                        display: "flex",
                        alignItems: "center",
                        justifyContent: "space-between",
                        gap: "10px",
                        flexWrap: "wrap",
                      }}
                    >
                      <div style={{ flex: 1, minWidth: "200px" }}>
                        <strong>{t("sys.pat_copy_once")}</strong> <code>{issuedPAT}</code>
                      </div>
                      <button
                        type="button"
                        class="volo-btn volo-btn-sm"
                        onClick={() => {
                          void navigator.clipboard.writeText(issuedPAT);
                          showToast(t("cnt.media_copied"));
                        }}
                      >
                        {t("cnt.media_copy")}
                      </button>
                    </div>
                  )}

                  <div class="volo-table-wrap">
                    <table class="volo-table" style={{ minWidth: "100%" }}>
                      <thead>
                        <tr>
                          <th>{t("sys.pat_col_token")}</th>
                          <th>{t("sys.pat_col_scopes")}</th>
                          <th class="col-act col-act-th">{t("art.actions")}</th>
                        </tr>
                      </thead>
                      <tbody>
                        {tokens.length === 0 ? (
                          <tr>
                            <td colSpan={3} class="volo-empty-state">
                              {t("tbl.empty")}
                            </td>
                          </tr>
                        ) : (
                          tokens.map((tk) => (
                            <tr key={tk.id}>
                              <td>
                                <div class="volo-cell-main">
                                  <div class="volo-cell-title">{tk.name}</div>
                                  <span class="volo-code-chip">{tk.token_prefix}…</span>
                                </div>
                              </td>
                              <td>
                                <span class="volo-help">{tk.scopes_json}</span>
                              </td>
                              <td class="col-act">
                                <div class="volo-row-actions">
                                  <button
                                    type="button"
                                    class="volo-act-btn danger"
                                    onClick={async () => {
                                      try {
                                        await apiFetch(`/api/admin/security/tokens/${tk.id}`, { method: "DELETE" });
                                        void loadSub();
                                      } catch (err: any) {
                                        showToast(err.message);
                                      }
                                    }}
                                  >
                                    {t("sys.pat_revoke")}
                                  </button>
                                </div>
                              </td>
                            </tr>
                          ))
                        )}
                      </tbody>
                    </table>
                  </div>
                </div>
              </div>

              <div class="volo-card">
                <div class="volo-sec-head">
                  <div class="volo-sec-copy">
                    <h3 class="volo-card-title">{t("sys.sess_head")}</h3>
                    <p class="volo-sec-desc">{t("sys.sess_desc")}</p>
                  </div>
                </div>
                <div class="volo-table-wrap">
                  <table class="volo-table" style={{ minWidth: "100%" }}>
                    <thead>
                      <tr>
                        <th>{t("sys.sess_col_device")}</th>
                        <th class="col-act col-act-th">{t("art.actions")}</th>
                      </tr>
                    </thead>
                    <tbody>
                      {sessPaged.slice.length === 0 ? (
                        <tr>
                          <td colSpan={2} class="volo-empty-state">
                            {t("tbl.empty")}
                          </td>
                        </tr>
                      ) : (
                        sessPaged.slice.map((s) => (
                          <tr key={s.id}>
                            <td>
                              <div class="volo-cell-main">
                                <div class="volo-cell-title">
                                  <span class="volo-code-chip">{s.client_ip}</span>
                                  {s.geo_region && <span class="volo-mini-tag">{s.geo_region}</span>}
                                  {s.id === meInfo?.session_id && (
                                    <span class="volo-status-pill ok">
                                      <span class="volo-status-dot" />
                                      {t("sys.sess_current")}
                                    </span>
                                  )}
                                </div>
                                <div class="volo-cell-meta">
                                  <span>{String(s.user_agent || "").slice(0, 72)}</span>
                                  <span>{formatDateTime(s.last_seen_at)}</span>
                                </div>
                              </div>
                            </td>
                            <td class="col-act">
                              <div class="volo-row-actions">
                                {s.id !== meInfo?.session_id && (
                                  <button
                                    type="button"
                                    class="volo-act-btn danger"
                                    onClick={async () => {
                                      try {
                                        await apiFetch(`/api/admin/security/sessions/${s.id}`, { method: "DELETE" });
                                        void loadSub();
                                      } catch (err: any) {
                                        showToast(err.message);
                                      }
                                    }}
                                  >
                                    {t("sys.sess_kick")}
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
                <Pagination
                  total={sessions.length}
                  page={sessPaged.safePage}
                  pageSize={10}
                  onChange={(p) => {
                    setSessPage(p);
                    resetConsoleScroll();
                  }}
                />
              </div>

              <AnimatedModal
                open={pkModalOpen}
                onClose={() => setPkModalOpen(false)}
                title={t("sys.pk_modal_title")}
                maxWidth="480px"
                footer={
                  <>
                    <button type="button" class="volo-btn" onClick={() => setPkModalOpen(false)}>
                      {t("common.cancel")}
                    </button>
                    <button type="button" class="volo-btn volo-btn-primary" onClick={() => void registerPasskey()}>
                      {t("sys.pk_register_btn")}
                    </button>
                  </>
                }
              >
                <form
                  autoComplete="off"
                  onSubmit={(e) => {
                    e.preventDefault();
                    void registerPasskey();
                  }}
                >
                  <div class="volo-field" style={{ marginBottom: 0 }}>
                    <label>{t("sys.lbl_pk_name")}</label>
                    <input
                      class="volo-input"
                      name="passkey_device_label"
                      autoComplete="off"
                      placeholder={t("sys.pk_name_ph")}
                      value={pkName}
                      onInput={(e) => setPkName((e.currentTarget as HTMLInputElement).value)}
                    />
                  </div>
                </form>
              </AnimatedModal>

              <AnimatedModal
                open={patModalOpen}
                onClose={() => setPatModalOpen(false)}
                title={t("sys.pat_modal_title")}
                maxWidth="540px"
                footer={
                  issuedPAT ? (
                    <>
                      <button type="button" class="volo-btn" onClick={() => setPatModalOpen(false)}>
                        {t("common.close")}
                      </button>
                      <button
                        type="button"
                        class="volo-btn volo-btn-primary"
                        onClick={() => {
                          void navigator.clipboard.writeText(issuedPAT);
                          showToast(t("cnt.media_copied"));
                        }}
                      >
                        {t("cnt.media_copy")}
                      </button>
                    </>
                  ) : (
                    <>
                      <button type="button" class="volo-btn" onClick={() => setPatModalOpen(false)}>
                        {t("common.cancel")}
                      </button>
                      <button
                        type="button"
                        class="volo-btn volo-btn-primary"
                        onClick={async () => {
                          try {
                            const res = await apiFetch<{ plaintext_token: string }>("/api/admin/security/tokens", {
                              method: "POST",
                              body: JSON.stringify({ name: patName, scopes: patScopes }),
                            });
                            setIssuedPAT(res.plaintext_token);
                            void loadSub();
                          } catch (err: any) {
                            showToast(err.message);
                          }
                        }}
                      >
                        {t("sys.pat_generate")}
                      </button>
                    </>
                  )
                }
              >
                {issuedPAT ? (
                  <div
                    class="volo-subcard"
                    style={{
                      marginTop: 0,
                      marginBottom: 0,
                      background: "var(--volo-accent-soft)",
                      wordBreak: "break-all",
                    }}
                  >
                    <div style={{ fontWeight: 650, fontSize: "13px", marginBottom: "8px" }}>
                      {t("sys.pat_copy_once")}
                    </div>
                    <code
                      style={{
                        display: "block",
                        padding: "10px 12px",
                        borderRadius: "8px",
                        background: "var(--volo-surface)",
                        border: "1px solid var(--volo-line)",
                        fontSize: "12.5px",
                        userSelect: "all",
                      }}
                    >
                      {issuedPAT}
                    </code>
                  </div>
                ) : (
                  <form
                    autoComplete="off"
                    onSubmit={async (e) => {
                      e.preventDefault();
                      try {
                        const res = await apiFetch<{ plaintext_token: string }>("/api/admin/security/tokens", {
                          method: "POST",
                          body: JSON.stringify({ name: patName, scopes: patScopes }),
                        });
                        setIssuedPAT(res.plaintext_token);
                        void loadSub();
                      } catch (err: any) {
                        showToast(err.message);
                      }
                    }}
                  >
                    <div class="volo-field">
                      <label>{t("sys.lbl_pat_name")}</label>
                      <input
                        class="volo-input"
                        required
                        name="pat_purpose_label"
                        autoComplete="off"
                        placeholder={t("sys.pat_label_ph")}
                        value={patName}
                        onInput={(e) => setPatName((e.currentTarget as HTMLInputElement).value)}
                      />
                    </div>
                    <div class="volo-field" style={{ marginBottom: 0 }}>
                      <label>{t("sys.lbl_pat_scopes")}</label>
                      <div class="volo-check-grid" style={{ marginBottom: 0 }}>
                        {PAT_SCOPES.map((sc) => (
                          <label key={sc} class="volo-check-item">
                            <input
                              type="checkbox"
                              checked={patScopes.includes(sc)}
                              onChange={(e) => {
                                const on = (e.currentTarget as HTMLInputElement).checked;
                                setPatScopes(on ? [...patScopes, sc] : patScopes.filter((x) => x !== sc));
                              }}
                            />
                            <span>{sc}</span>
                          </label>
                        ))}
                      </div>
                    </div>
                  </form>
                )}
              </AnimatedModal>
            </>
          )}
        </div>
      )}

      {sub === "users" && isAdmin && (
        <div class="volo-card">
          <div class="volo-sec-head">
            <div class="volo-sec-copy">
              <h3 class="volo-card-title">{t("sys.users_head")}</h3>
              <p class="volo-sec-desc">{t("sys.users_desc")}</p>
            </div>
            <button
              type="button"
              class="volo-btn volo-btn-primary volo-btn-sm"
              onClick={() => {
                setEditingUserId(null);
                setUserForm({
                  username: "",
                  email: "",
                  display_name: "",
                  role: "editor",
                  status: "active",
                  password: "",
                });
                setUserModalOpen(true);
              }}
            >
              + {t("sys.new_user")}
            </button>
          </div>

          <div class="volo-filter-bar">
            <div class="volo-filter-pills">
              {(["all", "admin", "editor", "reader"] as const).map((rf) => (
                <button
                  key={rf}
                  type="button"
                  class={`volo-filter-pill ${userRoleFilter === rf ? "active" : ""}`}
                  onClick={() => {
                    setUserRoleFilter(rf);
                    setUsersPage(1);
                  }}
                >
                  {rf === "all" ? t("art.all") : renderRoleLabel(rf)}
                </button>
              ))}
            </div>
            <div class="volo-filter-controls">
              <input
                class="volo-input"
                style={{ width: "220px" }}
                name="users_search_filter"
                autoComplete="off"
                placeholder={t("sys.user_search_ph")}
                value={userQuery}
                onInput={(e) => {
                  setUserQuery((e.currentTarget as HTMLInputElement).value);
                  setUsersPage(1);
                }}
              />
            </div>
          </div>

          <div class="volo-table-wrap">
            <table class="volo-table">
              <thead>
                <tr>
                  <th class="col-fluid">{t("sys.col_user")}</th>
                  <th class="col-cat">{t("sys.col_role")}</th>
                  <th class="col-status">{t("art.status")}</th>
                  <th class="col-date">{t("sys.col_created")}</th>
                  <th class="col-act col-act-th">{t("art.actions")}</th>
                </tr>
              </thead>
              <tbody>
                {usersPaged.slice.length === 0 ? (
                  <tr>
                    <td colSpan={5} class="volo-empty-state">
                      {t("tbl.empty")}
                    </td>
                  </tr>
                ) : (
                  usersPaged.slice.map((u) => {
                    const isSelf = u.id === currentUserSig.value?.id;
                    return (
                      <tr key={u.id}>
                        <td class="col-fluid">
                          <div class="volo-cell-main">
                            <div class="volo-cell-title">
                              <span>{u.display_name || u.username}</span>
                              <span class="volo-code-chip">@{u.username}</span>
                            </div>
                            {u.email && (
                              <div class="volo-cell-meta">
                                <span>{u.email}</span>
                              </div>
                            )}
                          </div>
                        </td>
                        <td class="col-cat" style={{ minWidth: "150px" }}>
                          {isSelf ? (
                            <span class="volo-cat-chip">{renderRoleLabel(u.role)}</span>
                          ) : (
                            <ConsoleSelect
                              value={u.role}
                              options={[
                                { value: "admin", label: t("role.admin") },
                                { value: "editor", label: t("role.editor") },
                                { value: "reader", label: t("role.reader") },
                              ]}
                              onChange={async (nextRole) => {
                                try {
                                  await apiFetch(`/api/admin/users/${u.id}`, {
                                    method: "PATCH",
                                    body: JSON.stringify({ role: nextRole }),
                                  });
                                  showToast(t("common.saved"));
                                  void loadSub();
                                } catch (err: any) {
                                  showToast(err.message);
                                }
                              }}
                            />
                          )}
                        </td>
                        <td class="col-status">
                          <span class={`volo-status-pill ${u.status === "active" ? "ok" : "warn"}`}>
                            <span class="volo-status-dot" />
                            {u.status === "active" ? t("tbl.status_active") : t("tbl.status_disabled")}
                          </span>
                        </td>
                        <td class="col-date">{formatDateTime(u.created_at)}</td>
                        <td class="col-act">
                          <div class="volo-row-actions">
                            <button
                              type="button"
                              class="volo-act-btn"
                              onClick={() => {
                                setEditingUserId(u.id);
                                setUserForm({
                                  username: u.username || "",
                                  email: u.email || "",
                                  display_name: u.display_name || "",
                                  role: u.role || "editor",
                                  status: u.status || "active",
                                  password: "",
                                });
                                setUserModalOpen(true);
                              }}
                            >
                              {t("common.edit")}
                            </button>
                            {!isSelf && (
                              <>
                                <button
                                  type="button"
                                  class="volo-act-btn"
                                  onClick={async () => {
                                    try {
                                      const nextSt = u.status === "active" ? "disabled" : "active";
                                      await apiFetch(`/api/admin/users/${u.id}`, {
                                        method: "PATCH",
                                        body: JSON.stringify({ status: nextSt }),
                                      });
                                      void loadSub();
                                    } catch (err: any) {
                                      showToast(err.message);
                                    }
                                  }}
                                >
                                  {u.status === "active" ? t("sys.user_disable") : t("sys.user_enable")}
                                </button>
                                <button
                                  type="button"
                                  class="volo-act-btn danger"
                                  onClick={async () => {
                                    if (!window.confirm(t("sys.confirm_del_user", { name: u.username }))) return;
                                    try {
                                      await apiFetch(`/api/admin/users/${u.id}`, { method: "DELETE" });
                                      void loadSub();
                                    } catch (err: any) {
                                      showToast(err.message);
                                    }
                                  }}
                                >
                                  {t("common.delete")}
                                </button>
                              </>
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
            total={filteredUsers.length}
            page={usersPaged.safePage}
            pageSize={15}
            onChange={(p) => {
              setUsersPage(p);
              resetConsoleScroll();
            }}
          />

          <AnimatedModal
            open={userModalOpen}
            onClose={() => setUserModalOpen(false)}
            title={editingUserId ? t("sys.edit_user") : t("sys.new_user")}
            maxWidth="580px"
            footer={
              <>
                <button type="button" class="volo-btn" onClick={() => setUserModalOpen(false)}>
                  {t("common.cancel")}
                </button>
                <button type="submit" form="volo-sys-user-form" class="volo-btn volo-btn-primary">
                  {editingUserId ? t("common.save") : t("common.create")}
                </button>
              </>
            }
          >
            <form
              id="volo-sys-user-form"
              autoComplete="off"
              onSubmit={async (e) => {
                e.preventDefault();
                try {
                  if (editingUserId) {
                    const isSelfEdit = editingUserId === currentUserSig.value?.id;
                    const body: Record<string, any> = {
                      display_name: userForm.display_name,
                      email: userForm.email,
                      role: userForm.role,
                      status: userForm.status,
                    };
                    if (userForm.password.trim()) {
                      body.password = userForm.password;
                    }
                    await apiFetch(`/api/admin/users/${editingUserId}`, {
                      method: "PATCH",
                      body: JSON.stringify(body),
                    });
                    if (isSelfEdit) {
                      await refreshMe();
                    }
                  } else {
                    await apiFetch("/api/admin/users", {
                      method: "POST",
                      body: JSON.stringify({
                        username: userForm.username,
                        display_name: userForm.display_name,
                        email: userForm.email,
                        role: userForm.role,
                        password: userForm.password,
                      }),
                    });
                  }
                  setUserModalOpen(false);
                  showToast(t("common.saved"));
                  void loadSub();
                } catch (err: any) {
                  showToast(err.message);
                }
              }}
            >
              <div class="volo-grid-2">
                <div class="volo-field">
                  <label>{t("sys.lbl_user_name")}</label>
                  <input
                    class="volo-input"
                    required
                    disabled={!!editingUserId}
                    name="member_handle_field"
                    autoComplete="off"
                    placeholder={t("sys.user_name_ph")}
                    value={userForm.username}
                    onInput={(e) => setUserForm({ ...userForm, username: (e.currentTarget as HTMLInputElement).value })}
                  />
                </div>
                <div class="volo-field">
                  <label>{t("sys.lbl_user_disp")}</label>
                  <input
                    class="volo-input"
                    name="member_alias_field"
                    autoComplete="off"
                    placeholder={t("sys.user_disp_ph")}
                    value={userForm.display_name}
                    onInput={(e) => setUserForm({ ...userForm, display_name: (e.currentTarget as HTMLInputElement).value })}
                  />
                </div>
              </div>

              <div class="volo-field">
                <label>{t("sys.lbl_user_email")}</label>
                <input
                  type="email"
                  class="volo-input"
                  required
                  name="member_mailbox_field"
                  autoComplete="off"
                  placeholder={t("sys.user_email_ph")}
                  value={userForm.email}
                  onInput={(e) => setUserForm({ ...userForm, email: (e.currentTarget as HTMLInputElement).value })}
                />
              </div>

              {editingUserId ? (
                <div class="volo-grid-2">
                  <div class="volo-field">
                    <label>{t("sys.lbl_user_role")}</label>
                    <ConsoleSelect
                      value={userForm.role}
                      disabled={editingUserId === currentUserSig.value?.id}
                      options={[
                        { value: "editor", label: t("sys.role_editor_opt") },
                        { value: "reader", label: t("sys.role_reader_opt") },
                        { value: "admin", label: t("sys.role_admin_opt") },
                      ]}
                      onChange={(v) => setUserForm({ ...userForm, role: v })}
                    />
                  </div>
                  <div class="volo-field">
                    <label>{t("sys.lbl_user_status")}</label>
                    <ConsoleSelect
                      value={userForm.status}
                      disabled={editingUserId === currentUserSig.value?.id}
                      options={[
                        { value: "active", label: t("tbl.status_active") },
                        { value: "disabled", label: t("tbl.status_disabled") },
                      ]}
                      onChange={(v) => setUserForm({ ...userForm, status: v })}
                    />
                  </div>
                </div>
              ) : (
                <div class="volo-field">
                  <label>{t("sys.lbl_user_role")}</label>
                  <ConsoleSelect
                    value={userForm.role}
                    options={[
                      { value: "editor", label: t("sys.role_editor_opt") },
                      { value: "reader", label: t("sys.role_reader_opt") },
                      { value: "admin", label: t("sys.role_admin_opt") },
                    ]}
                    onChange={(v) => setUserForm({ ...userForm, role: v })}
                  />
                </div>
              )}

              <div class="volo-field" style={{ marginBottom: 0 }}>
                <label>{editingUserId ? t("sys.lbl_user_pwd_reset") : t("sys.lbl_user_pwd")}</label>
                <input
                  type="password"
                  class="volo-input"
                  required={!editingUserId}
                  minLength={6}
                  name="member_passphrase_field"
                  autoComplete="new-password"
                  placeholder={editingUserId ? t("sys.user_pwd_reset_ph") : t("sys.user_pwd_ph")}
                  value={userForm.password}
                  onInput={(e) => setUserForm({ ...userForm, password: (e.currentTarget as HTMLInputElement).value })}
                />
              </div>
            </form>
          </AnimatedModal>
        </div>
      )}

      {sub === "waf" && isAdmin && (
        <div>
          <div class="volo-seg-bar" style={{ marginBottom: "16px" }}>
            <button
              type="button"
              class={`volo-seg-btn ${wafSub === "policy" ? "active" : ""}`}
              onClick={() => {
                setWafSub("policy");
                resetConsoleScroll();
              }}
            >
              {t("sys.sub_waf_policy")}
            </button>
            <button
              type="button"
              class={`volo-seg-btn ${wafSub === "bans" ? "active" : ""}`}
              onClick={() => {
                setWafSub("bans");
                resetConsoleScroll();
              }}
            >
              {t("sys.sub_waf_bans")} ({bans.length})
            </button>
            <button
              type="button"
              class={`volo-seg-btn ${wafSub === "geo" ? "active" : ""}`}
              onClick={() => {
                setWafSub("geo");
                resetConsoleScroll();
              }}
            >
              {t("sys.sub_waf_geo")} ({geoRules.length})
            </button>
          </div>

          {wafSub === "policy" && domains?.security && (
            <form
              class="volo-card"
              autoComplete="off"
              onSubmit={(e) => {
                e.preventDefault();
                void patchDomain("security", domains.security || {});
              }}
            >
              <div class="volo-sec-head">
                <div class="volo-sec-copy">
                  <h3 class="volo-card-title">{t("sys.sec_policy_head")}</h3>
                  <p class="volo-sec-desc">{t("sys.sec_policy_desc")}</p>
                </div>
              </div>
              <div class="volo-grid-3">
                <div class="volo-field">
                  <label>{t("sys.waf_window_sec")}</label>
                  <input
                    type="number"
                    class="volo-input"
                    value={domains.security?.waf_window_seconds ?? 180}
                    onInput={(e) => setDomField("security", "waf_window_seconds", Number((e.currentTarget as HTMLInputElement).value))}
                  />
                </div>
                <div class="volo-field">
                  <label>{t("sys.waf_max_req")}</label>
                  <input
                    type="number"
                    class="volo-input"
                    value={domains.security?.waf_max_requests ?? 180}
                    onInput={(e) => setDomField("security", "waf_max_requests", Number((e.currentTarget as HTMLInputElement).value))}
                  />
                </div>
                <div class="volo-field">
                  <label>{t("sys.totp_scope")}</label>
                  <ConsoleSelect
                    value={domains.security?.totp_required_scope || "off"}
                    options={[
                      { value: "off", label: t("sys.totp_scope_off") },
                      { value: "admin", label: t("sys.totp_scope_admin") },
                      { value: "staff", label: t("sys.totp_scope_staff") },
                    ]}
                    onChange={(v) => setDomField("security", "totp_required_scope", v)}
                  />
                </div>
              </div>
              <div class="volo-field">
                <label>{t("sys.dyn_login_path")}</label>
                <input
                  class="volo-input"
                  name="sec_custom_entry_route"
                  autoComplete="off"
                  placeholder="/secret-entry"
                  value={domains.security?.dynamic_login_path || ""}
                  onInput={(e) => setDomField("security", "dynamic_login_path", (e.currentTarget as HTMLInputElement).value)}
                />
              </div>
              <div class="volo-check-grid">
                <label class="volo-check-item">
                  <input
                    type="checkbox"
                    checked={domains.security?.risk_control_enabled !== false}
                    onChange={(e) => setDomField("security", "risk_control_enabled", (e.currentTarget as HTMLInputElement).checked)}
                  />
                  <span>{t("sys.risk_on")}</span>
                </label>
                <label class="volo-check-item">
                  <input
                    type="checkbox"
                    checked={domains.security?.waf_enabled !== false}
                    onChange={(e) => setDomField("security", "waf_enabled", (e.currentTarget as HTMLInputElement).checked)}
                  />
                  <span>{t("sys.waf_on")}</span>
                </label>
                <label class="volo-check-item">
                  <input
                    type="checkbox"
                    checked={!!domains.security?.geo_policy_active}
                    onChange={(e) => setDomField("security", "geo_policy_active", (e.currentTarget as HTMLInputElement).checked)}
                  />
                  <span>{t("sys.geo_policy_on")}</span>
                </label>
              </div>
              <div class="volo-form-actions">
                <button
                  type="button"
                  class="volo-btn"
                  onClick={async () => {
                    try {
                      await apiFetch("/api/admin/security/rotate-share-secret", { method: "POST", body: "{}" });
                      showToast(t("sys.share_rotated"));
                    } catch (err: any) {
                      showToast(err.message);
                    }
                  }}
                >
                  {t("sys.rotate_share")}
                </button>
                <button type="submit" class="volo-btn volo-btn-primary">
                  {t("sys.save_sec_policy")}
                </button>
              </div>
            </form>
          )}

          {wafSub === "bans" && (
            <div class="volo-card">
              <div class="volo-sec-head">
                <div class="volo-sec-copy">
                  <h3 class="volo-card-title">{t("sys.waf_bans_head")}</h3>
                  <p class="volo-sec-desc">{t("sys.waf_bans_desc")}</p>
                </div>
                <button
                  type="button"
                  class="volo-btn volo-btn-primary volo-btn-sm"
                  onClick={() => {
                    setBanForm({ cidr: "", note: "manual", expires_hours: 24 });
                    setBanModalOpen(true);
                  }}
                >
                  + {t("sys.new_ban")}
                </button>
              </div>

              <div class="volo-table-wrap">
                <table class="volo-table" style={{ minWidth: "100%" }}>
                  <thead>
                    <tr>
                      <th>{t("tbl.col_cidr")}</th>
                      <th>{t("tbl.col_note")}</th>
                      <th class="col-act col-act-th">{t("art.actions")}</th>
                    </tr>
                  </thead>
                  <tbody>
                    {bansPaged.slice.length === 0 ? (
                      <tr>
                        <td colSpan={3} class="volo-empty-state">
                          {t("tbl.empty")}
                        </td>
                      </tr>
                    ) : (
                      bansPaged.slice.map((b) => (
                        <tr key={b.id}>
                          <td>
                            <span class="volo-code-chip">{b.cidr}</span>
                          </td>
                          <td>
                            <div class="volo-cell-meta">
                              <span>{b.note || b.reason || "—"}</span>
                              <span>{b.expires_at > 0 ? formatDateTime(b.expires_at) : t("sys.waf_dur_perm")}</span>
                            </div>
                          </td>
                          <td class="col-act">
                            <div class="volo-row-actions">
                              <button
                                type="button"
                                class="volo-act-btn"
                                onClick={async () => {
                                  try {
                                    await apiFetch(`/api/admin/security/ip-bans/${b.id}`, { method: "DELETE" });
                                    void loadSub();
                                  } catch (err: any) {
                                    showToast(err.message);
                                  }
                                }}
                              >
                                {t("sys.waf_unban_btn")}
                              </button>
                            </div>
                          </td>
                        </tr>
                      ))
                    )}
                  </tbody>
                </table>
              </div>
              <Pagination
                total={bans.length}
                page={bansPaged.safePage}
                pageSize={15}
                onChange={(p) => {
                  setBansPage(p);
                  resetConsoleScroll();
                }}
              />

              <AnimatedModal
                open={banModalOpen}
                onClose={() => setBanModalOpen(false)}
                title={t("sys.new_ban")}
                maxWidth="520px"
                footer={
                  <>
                    <button type="button" class="volo-btn" onClick={() => setBanModalOpen(false)}>
                      {t("common.cancel")}
                    </button>
                    <button type="submit" form="volo-sys-ban-form" class="volo-btn volo-btn-primary">
                      {t("sys.waf_ban_btn")}
                    </button>
                  </>
                }
              >
                <form
                  id="volo-sys-ban-form"
                  autoComplete="off"
                  onSubmit={async (e) => {
                    e.preventDefault();
                    try {
                      await apiFetch("/api/admin/security/ip-bans", {
                        method: "POST",
                        body: JSON.stringify({
                          cidr: banForm.cidr,
                          note: banForm.note,
                          expires_hours: banForm.expires_hours,
                        }),
                      });
                      setBanModalOpen(false);
                      showToast(t("common.saved"));
                      void loadSub();
                    } catch (err: any) {
                      showToast(err.message);
                    }
                  }}
                >
                  <div class="volo-field">
                    <label>{t("sys.lbl_ban_cidr")}</label>
                    <input
                      class="volo-input"
                      required
                      name="waf_ban_cidr_target"
                      autoComplete="off"
                      placeholder={t("sys.waf_cidr_ph")}
                      value={banForm.cidr}
                      onInput={(e) => setBanForm({ ...banForm, cidr: (e.currentTarget as HTMLInputElement).value })}
                    />
                  </div>
                  <div class="volo-field">
                    <label>{t("sys.lbl_ban_dur")}</label>
                    <ConsoleSelect
                      value={String(banForm.expires_hours)}
                      options={[
                        { value: "24", label: t("sys.waf_dur_24h") },
                        { value: "168", label: t("sys.waf_dur_7d") },
                        { value: "720", label: t("sys.waf_dur_30d") },
                        { value: "0", label: t("sys.waf_dur_perm") },
                      ]}
                      onChange={(v) => setBanForm({ ...banForm, expires_hours: Number(v) })}
                    />
                  </div>
                  <div class="volo-field" style={{ marginBottom: 0 }}>
                    <label>{t("sys.lbl_ban_reason")}</label>
                    <input
                      class="volo-input"
                      name="waf_ban_reason_note"
                      autoComplete="off"
                      value={banForm.note}
                      onInput={(e) => setBanForm({ ...banForm, note: (e.currentTarget as HTMLInputElement).value })}
                    />
                  </div>
                </form>
              </AnimatedModal>
            </div>
          )}

          {wafSub === "geo" && (
            <div class="volo-card">
              <div class="volo-sec-head">
                <div class="volo-sec-copy">
                  <h3 class="volo-card-title">{t("sys.geo_head")}</h3>
                  <p class="volo-sec-desc">{t("sys.geo_desc")}</p>
                </div>
                <button
                  type="button"
                  class="volo-btn volo-btn-primary volo-btn-sm"
                  onClick={() => {
                    setEditingGeoId(null);
                    setGeoForm({
                      name: "Geo Rule",
                      action: "deny",
                      countries: "",
                      regions: "",
                      sort_order: 10,
                      is_enabled: true,
                      on_miss: "next",
                    });
                    setGeoModalOpen(true);
                  }}
                >
                  + {t("sys.new_geo_rule")}
                </button>
              </div>
              {!geoCircuitEnabled && (
                <div class="volo-subcard" style={{ marginTop: 0, fontSize: "12.5px", color: "var(--volo-warn)" }}>
                  {t("sys.geo_circuit_off")}
                </div>
              )}
              <div class="volo-table-wrap">
                <table class="volo-table" style={{ minWidth: "100%" }}>
                  <thead>
                    <tr>
                      <th>{t("art.status")}</th>
                      <th>{t("tbl.col_scope")}</th>
                      <th>{t("tbl.col_note")}</th>
                      <th class="col-act col-act-th">{t("art.actions")}</th>
                    </tr>
                  </thead>
                  <tbody>
                    {geoPaged.slice.length === 0 ? (
                      <tr>
                        <td colSpan={4} class="volo-empty-state">
                          {t("tbl.empty")}
                        </td>
                      </tr>
                    ) : (
                      geoPaged.slice.map((g) => (
                        <tr key={g.id}>
                          <td>
                            <div style={{ display: "flex", alignItems: "center", gap: "6px", flexWrap: "wrap" }}>
                              <span class={`volo-status-pill ${g.action === "deny" ? "danger" : "ok"}`}>
                                <span class="volo-status-dot" />
                                {g.action === "deny" ? t("sys.geo_deny") : t("sys.geo_allow")}
                              </span>
                              {g.is_enabled === false && (
                                <span class="volo-mini-tag warn">{t("tbl.status_disabled")}</span>
                              )}
                            </div>
                          </td>
                          <td>
                            <span class="volo-code-chip">
                              ISO={g.countries || "*"} · {g.regions || "*"}
                            </span>
                          </td>
                          <td>
                            <div class="volo-cell-meta">
                              <span>{g.name || "—"}</span>
                              {typeof g.sort_order === "number" && <span class="volo-code-chip">#{g.sort_order}</span>}
                            </div>
                          </td>
                          <td class="col-act">
                            <div class="volo-row-actions">
                              <button
                                type="button"
                                class="volo-act-btn"
                                onClick={() => {
                                  setEditingGeoId(g.id);
                                  setGeoForm({
                                    name: g.name || "Geo Rule",
                                    action: g.action || "deny",
                                    countries: g.countries || "",
                                    regions: g.regions || "",
                                    sort_order: typeof g.sort_order === "number" ? g.sort_order : 10,
                                    is_enabled: g.is_enabled !== false,
                                    on_miss: g.on_miss || "next",
                                  });
                                  setGeoModalOpen(true);
                                }}
                              >
                                {t("common.edit")}
                              </button>
                              <button
                                type="button"
                                class="volo-act-btn danger"
                                onClick={async () => {
                                  try {
                                    await apiFetch(`/api/admin/security/geo-rules/${g.id}`, { method: "DELETE" });
                                    void loadSub();
                                  } catch (err: any) {
                                    showToast(err.message);
                                  }
                                }}
                              >
                                {t("common.delete")}
                              </button>
                            </div>
                          </td>
                        </tr>
                      ))
                    )}
                  </tbody>
                </table>
              </div>
              <Pagination
                total={geoRules.length}
                page={geoPaged.safePage}
                pageSize={15}
                onChange={(p) => {
                  setGeoPage(p);
                  resetConsoleScroll();
                }}
              />

              <AnimatedModal
                open={geoModalOpen}
                onClose={() => setGeoModalOpen(false)}
                title={editingGeoId ? t("sys.edit_geo_rule") : t("sys.new_geo_rule")}
                maxWidth="540px"
                footer={
                  <>
                    <button type="button" class="volo-btn" onClick={() => setGeoModalOpen(false)}>
                      {t("common.cancel")}
                    </button>
                    <button type="submit" form="volo-sys-geo-form" class="volo-btn volo-btn-primary">
                      {editingGeoId ? t("common.save") : t("common.create")}
                    </button>
                  </>
                }
              >
                <form
                  id="volo-sys-geo-form"
                  autoComplete="off"
                  onSubmit={async (e) => {
                    e.preventDefault();
                    try {
                      if (editingGeoId) {
                        await apiFetch(`/api/admin/security/geo-rules/${editingGeoId}`, {
                          method: "PUT",
                          body: JSON.stringify(geoForm),
                        });
                      } else {
                        await apiFetch("/api/admin/security/geo-rules", {
                          method: "POST",
                          body: JSON.stringify(geoForm),
                        });
                      }
                      setGeoModalOpen(false);
                      showToast(t("common.saved"));
                      void loadSub();
                    } catch (err: any) {
                      showToast(err.message);
                    }
                  }}
                >
                  <div class="volo-grid-2">
                    <div class="volo-field">
                      <label>{t("sys.lbl_geo_mode")}</label>
                      <ConsoleSelect
                        value={geoForm.action}
                        options={[
                          { value: "deny", label: t("sys.geo_deny") },
                          { value: "allow", label: t("sys.geo_allow") },
                        ]}
                        onChange={(v) => setGeoForm({ ...geoForm, action: v })}
                      />
                    </div>
                    <div class="volo-field">
                      <label>{t("sys.lbl_geo_sort")}</label>
                      <input
                        type="number"
                        class="volo-input"
                        value={geoForm.sort_order}
                        onInput={(e) => setGeoForm({ ...geoForm, sort_order: Number((e.currentTarget as HTMLInputElement).value) })}
                      />
                    </div>
                  </div>
                  <div class="volo-field">
                    <label>{t("sys.lbl_geo_note")}</label>
                    <input
                      class="volo-input"
                      name="geo_rule_title_note"
                      autoComplete="off"
                      value={geoForm.name}
                      onInput={(e) => setGeoForm({ ...geoForm, name: (e.currentTarget as HTMLInputElement).value })}
                    />
                  </div>
                  <div class="volo-grid-2">
                    <div class="volo-field">
                      <label>{t("sys.lbl_geo_country")}</label>
                      <input
                        class="volo-input"
                        name="geo_iso_codes"
                        autoComplete="off"
                        placeholder={t("sys.geo_iso_ph")}
                        value={geoForm.countries}
                        onInput={(e) => setGeoForm({ ...geoForm, countries: (e.currentTarget as HTMLInputElement).value })}
                      />
                    </div>
                    <div class="volo-field">
                      <label>{t("sys.lbl_geo_region")}</label>
                      <input
                        class="volo-input"
                        name="geo_subdivision_codes"
                        autoComplete="off"
                        placeholder={t("sys.geo_region_ph")}
                        value={geoForm.regions}
                        onInput={(e) => setGeoForm({ ...geoForm, regions: (e.currentTarget as HTMLInputElement).value })}
                      />
                    </div>
                  </div>
                  <label class="volo-check-item" style={{ marginBottom: 0 }}>
                    <input
                      type="checkbox"
                      checked={geoForm.is_enabled}
                      onChange={(e) => setGeoForm({ ...geoForm, is_enabled: (e.currentTarget as HTMLInputElement).checked })}
                    />
                    <span>{t("sys.geo_rule_enabled")}</span>
                  </label>
                </form>
              </AnimatedModal>
            </div>
          )}
        </div>
      )}

      {sub === "backup" && isAdmin && (
        <>
          <div class="volo-grid-2">
            <div class="volo-card">
              <div class="volo-sec-head">
                <div class="volo-sec-copy">
                  <h3 class="volo-card-title">{t("sys.backup_head")}</h3>
                  <p class="volo-sec-desc">{t("sys.backup_desc")}</p>
                </div>
              </div>

              <div class="volo-subcard" style={{ marginTop: 0, marginBottom: 0 }}>
                <div class="volo-check-grid" style={{ marginTop: 0, marginBottom: 0 }}>
                  <label class="volo-check-item">
                    <input
                      type="checkbox"
                      checked={includeMediaExport}
                      onChange={(e) => setIncludeMediaExport((e.currentTarget as HTMLInputElement).checked)}
                    />
                    <span>{t("sys.backup_with_media")}</span>
                  </label>
                  <label class="volo-check-item">
                    <input
                      type="checkbox"
                      checked={dryRunRestore}
                      onChange={(e) => setDryRunRestore((e.currentTarget as HTMLInputElement).checked)}
                    />
                    <span>{t("sys.backup_dry_run")}</span>
                  </label>
                </div>
              </div>

              <div class="volo-form-actions">
                <label class="volo-btn" style={{ cursor: "pointer" }}>
                  {t("sys.backup_legacy")}
                  <input
                    type="file"
                    accept=".zip,.json"
                    style={{ display: "none" }}
                    onChange={async (e) => {
                      const f = (e.currentTarget as HTMLInputElement).files?.[0];
                      if (!f) return;
                      try {
                        const fd = new FormData();
                        fd.append("file", f);
                        await apiFetch("/api/admin/backup/import-legacy", { method: "POST", body: fd });
                        showToast(t("setup.legacy_done"));
                        void loadSub();
                      } catch (err: any) {
                        showToast(err.message);
                      }
                      (e.currentTarget as HTMLInputElement).value = "";
                    }}
                  />
                </label>

                <label class="volo-btn" style={{ cursor: "pointer" }}>
                  {t("sys.backup_restore")}
                  <input
                    type="file"
                    accept=".zip,.json"
                    style={{ display: "none" }}
                    onChange={async (e) => {
                      const f = (e.currentTarget as HTMLInputElement).files?.[0];
                      if (!f) return;
                      if (!dryRunRestore && !window.confirm(t("sys.confirm_restore"))) {
                        (e.currentTarget as HTMLInputElement).value = "";
                        return;
                      }
                      try {
                        const fd = new FormData();
                        fd.append("file", f);
                        fd.append("dry_run", dryRunRestore ? "true" : "false");
                        await apiFetch("/api/admin/backup/restore", { method: "POST", body: fd });
                        showToast(dryRunRestore ? t("sys.backup_dry_ok") : t("sys.backup_restored"));
                        void loadSub();
                      } catch (err: any) {
                        showToast(err.message);
                      }
                      (e.currentTarget as HTMLInputElement).value = "";
                    }}
                  />
                </label>

                <button
                  type="button"
                  class="volo-btn volo-btn-primary"
                  onClick={async () => {
                    const csrf = csrfTokenSig.value || (await apiFetch<any>("/api/admin/me")).csrf_token;
                    const res = await fetch(`/api/admin/backup/export?media=${includeMediaExport ? 1 : 0}`, {
                      method: "POST",
                      headers: { "X-CSRF-Token": csrf },
                      credentials: "same-origin",
                    });
                    if (!res.ok) {
                      showToast(t("sys.backup_export_err"));
                      return;
                    }
                    const blob = await res.blob();
                    const a = document.createElement("a");
                    a.href = URL.createObjectURL(blob);
                    a.download = `volo-backup-${Date.now()}.zip`;
                    a.click();
                    URL.revokeObjectURL(a.href);
                  }}
                >
                  {t("sys.backup_export")}
                </button>
              </div>
            </div>

            {domains?.backup && (
              <form
                class="volo-card"
                autoComplete="off"
                onSubmit={(e) => {
                  e.preventDefault();
                  const body: any = { ...(domains.backup || {}) };
                  if (bakWebdavPwd) body.webdav_password = bakWebdavPwd;
                  if (bakS3Secret) body.s3_secret_key = bakS3Secret;
                  void patchDomain("backup", body);
                  setBakWebdavPwd("");
                  setBakS3Secret("");
                }}
              >
                <div class="volo-sec-head">
                  <div class="volo-sec-copy">
                    <h3 class="volo-card-title">{t("sys.auto_bak_head")}</h3>
                    <p class="volo-sec-desc">{t("sys.auto_bak_desc")}</p>
                  </div>
                </div>
                <div class="volo-grid-3">
                  <div class="volo-field">
                    <label>{t("sys.auto_interval_h")}</label>
                    <input
                      type="number"
                      class="volo-input"
                      value={domains.backup?.local_interval_hours ?? 24}
                      onInput={(e) => setDomField("backup", "local_interval_hours", Number((e.currentTarget as HTMLInputElement).value))}
                    />
                  </div>
                  <div class="volo-field">
                    <label>{t("sys.auto_keep_cnt")}</label>
                    <input
                      type="number"
                      class="volo-input"
                      value={domains.backup?.local_keep_count ?? 7}
                      onInput={(e) => setDomField("backup", "local_keep_count", Number((e.currentTarget as HTMLInputElement).value))}
                    />
                  </div>
                  <div class="volo-field">
                    <label>{t("sys.remote_proto")}</label>
                    <ConsoleSelect
                      value={domains.backup?.remote_protocol || "s3"}
                      options={[
                        { value: "s3", label: "S3 / R2 / MinIO" },
                        { value: "webdav", label: "WebDAV" },
                      ]}
                      onChange={(v) => setDomField("backup", "remote_protocol", v)}
                    />
                  </div>
                </div>
                <div class="volo-check-grid">
                  <label class="volo-check-item">
                    <input
                      type="checkbox"
                      checked={!!domains.backup?.local_auto_enabled}
                      onChange={(e) => setDomField("backup", "local_auto_enabled", (e.currentTarget as HTMLInputElement).checked)}
                    />
                    <span>{t("sys.auto_local_on")}</span>
                  </label>
                  <label class="volo-check-item">
                    <input
                      type="checkbox"
                      checked={!!domains.backup?.include_media_in_auto}
                      onChange={(e) => setDomField("backup", "include_media_in_auto", (e.currentTarget as HTMLInputElement).checked)}
                    />
                    <span>{t("sys.auto_media_on")}</span>
                  </label>
                  <label class="volo-check-item">
                    <input
                      type="checkbox"
                      checked={!!domains.backup?.remote_enabled}
                      onChange={(e) => setDomField("backup", "remote_enabled", (e.currentTarget as HTMLInputElement).checked)}
                    />
                    <span>{t("sys.remote_bak_on")}</span>
                  </label>
                </div>

                {domains.backup?.remote_protocol === "webdav" ? (
                  <div class="volo-subcard">
                    <div class="volo-grid-2">
                      <div class="volo-field">
                        <label>{t("sys.lbl_webdav_url")}</label>
                        <input
                          class="volo-input"
                          name="bak_webdav_endpoint"
                          autoComplete="off"
                          placeholder={t("sys.webdav_url_ph")}
                          value={domains.backup?.webdav_url || ""}
                          onInput={(e) => setDomField("backup", "webdav_url", (e.currentTarget as HTMLInputElement).value)}
                        />
                      </div>
                      <div class="volo-field">
                        <label>{t("sys.lbl_webdav_dir")}</label>
                        <input
                          class="volo-input"
                          name="bak_webdav_folder"
                          autoComplete="off"
                          placeholder={t("sys.webdav_dir_ph")}
                          value={domains.backup?.webdav_dir || ""}
                          onInput={(e) => setDomField("backup", "webdav_dir", (e.currentTarget as HTMLInputElement).value)}
                        />
                      </div>
                    </div>
                    <div class="volo-grid-2">
                      <div class="volo-field" style={{ marginBottom: 0 }}>
                        <label>{t("sys.lbl_webdav_user")}</label>
                        <input
                          class="volo-input"
                          name="bak_webdav_account"
                          autoComplete="off"
                          placeholder={t("sys.webdav_user_ph")}
                          value={domains.backup?.webdav_user || ""}
                          onInput={(e) => setDomField("backup", "webdav_user", (e.currentTarget as HTMLInputElement).value)}
                        />
                      </div>
                      <div class="volo-field" style={{ marginBottom: 0 }}>
                        <label>
                          {t("sys.lbl_webdav_pwd")}{" "}
                          {domains.backup?.webdav_password_set && <span class="volo-badge volo-badge-ok">{t("sys.sealed")}</span>}
                        </label>
                        <input
                          type="password"
                          class="volo-input"
                          name="bak_webdav_sealed_secret"
                          autoComplete="new-password"
                          placeholder={domains.backup?.webdav_password_set ? t("sys.s3_sk_sealed_ph") : t("sys.webdav_pwd_ph")}
                          value={bakWebdavPwd}
                          onInput={(e) => setBakWebdavPwd((e.currentTarget as HTMLInputElement).value)}
                        />
                      </div>
                    </div>
                  </div>
                ) : (
                  <div class="volo-subcard">
                    <div class="volo-grid-2">
                      <div class="volo-field">
                        <label>{t("sys.s3_endpoint")}</label>
                        <input
                          class="volo-input"
                          name="bak_s3_endpoint"
                          autoComplete="off"
                          placeholder="https://s3.amazonaws.com"
                          value={domains.backup?.s3_endpoint || ""}
                          onInput={(e) => setDomField("backup", "s3_endpoint", (e.currentTarget as HTMLInputElement).value)}
                        />
                      </div>
                      <div class="volo-field">
                        <label>{t("sys.lbl_s3_bucket")}</label>
                        <input
                          class="volo-input"
                          name="bak_s3_bucket"
                          autoComplete="off"
                          placeholder={t("sys.s3_bucket_ph")}
                          value={domains.backup?.s3_bucket || ""}
                          onInput={(e) => setDomField("backup", "s3_bucket", (e.currentTarget as HTMLInputElement).value)}
                        />
                      </div>
                    </div>
                    <div class="volo-grid-2">
                      <div class="volo-field" style={{ marginBottom: 0 }}>
                        <label>{t("sys.lbl_s3_ak")}</label>
                        <input
                          class="volo-input"
                          name="bak_s3_ak_token"
                          autoComplete="off"
                          placeholder={t("sys.s3_ak_ph")}
                          value={domains.backup?.s3_access_key || ""}
                          onInput={(e) => setDomField("backup", "s3_access_key", (e.currentTarget as HTMLInputElement).value)}
                        />
                      </div>
                      <div class="volo-field" style={{ marginBottom: 0 }}>
                        <label>
                          {t("sys.lbl_s3_sk")}{" "}
                          {domains.backup?.s3_secret_key_set && <span class="volo-badge volo-badge-ok">{t("sys.sealed")}</span>}
                        </label>
                        <input
                          type="password"
                          class="volo-input"
                          name="bak_s3_sealed_secret"
                          autoComplete="new-password"
                          placeholder={domains.backup?.s3_secret_key_set ? t("sys.s3_sk_sealed_ph") : t("sys.s3_sk_ph")}
                          value={bakS3Secret}
                          onInput={(e) => setBakS3Secret((e.currentTarget as HTMLInputElement).value)}
                        />
                      </div>
                    </div>
                  </div>
                )}

                <div class="volo-form-actions">
                  <button type="submit" class="volo-btn volo-btn-primary">
                    {t("sys.save_backup_cfg")}
                  </button>
                </div>
              </form>
            )}
          </div>

          {/* Local Rolling Backup Snapshots */}
          <div class="volo-card">
            <div class="volo-sec-head">
              <div class="volo-sec-copy">
                <h3 class="volo-card-title">{t("sys.bak_local_head")}</h3>
                <p class="volo-sec-desc">{t("sys.auto_bak_desc")}</p>
              </div>
              <button
                type="button"
                class="volo-btn volo-btn-primary volo-btn-sm"
                disabled={creatingSnapshot}
                onClick={async () => {
                  setCreatingSnapshot(true);
                  try {
                    const res = await apiFetch<{
                      file: string;
                      remote_warning?: string;
                      items?: Array<{ name: string; size_bytes: number; created_at: number }>;
                    }>("/api/admin/backup/local", { method: "POST", body: "{}" });
                    if (Array.isArray(res.items)) {
                      setLocalBackups(res.items);
                    } else {
                      void loadSub();
                    }
                    showToast(t("sys.bak_local_created", { name: res.file || "snapshot.zip" }));
                  } catch (err: any) {
                    showToast(err.message);
                  } finally {
                    setCreatingSnapshot(false);
                  }
                }}
              >
                {t("sys.bak_local_now")}
              </button>
            </div>
            <div class="volo-table-wrap">
              <table class="volo-table">
                <thead>
                  <tr>
                    <th class="col-fluid">{t("sys.bak_col_file")}</th>
                    <th class="col-cat">{t("cnt.media_col_size")}</th>
                    <th class="col-date">{t("sys.col_created")}</th>
                    <th class="col-act col-act-th">{t("art.actions")}</th>
                  </tr>
                </thead>
                <tbody>
                  {localBakPaged.slice.length === 0 ? (
                    <tr>
                      <td colSpan={4} class="volo-empty-state">
                        {t("tbl.empty")}
                      </td>
                    </tr>
                  ) : (
                    localBakPaged.slice.map((b) => (
                      <tr key={b.name}>
                        <td class="col-fluid">
                          <span class="volo-code-chip">{b.name}</span>
                        </td>
                        <td class="col-cat">
                          <span class="volo-help">{formatBytes(b.size_bytes)}</span>
                        </td>
                        <td class="col-date">{formatDateTime(b.created_at)}</td>
                        <td class="col-act">
                          <div class="volo-row-actions">
                            <a
                              class="volo-act-btn"
                              href={`/api/admin/backup/local/${encodeURIComponent(b.name)}`}
                              download={b.name}
                            >
                              {t("sys.bak_download")}
                            </a>
                            <button
                              type="button"
                              class="volo-act-btn danger"
                              onClick={async () => {
                                try {
                                  const res = await apiFetch<{
                                    items?: Array<{ name: string; size_bytes: number; created_at: number }>;
                                  }>(`/api/admin/backup/local/${encodeURIComponent(b.name)}`, { method: "DELETE" });
                                  if (Array.isArray(res.items)) {
                                    setLocalBackups(res.items);
                                  } else {
                                    void loadSub();
                                  }
                                  showToast(t("common.deleted"));
                                } catch (err: any) {
                                  showToast(err.message);
                                }
                              }}
                            >
                              {t("common.delete")}
                            </button>
                          </div>
                        </td>
                      </tr>
                    ))
                  )}
                </tbody>
              </table>
            </div>
            <Pagination
              total={localBackups.length}
              page={localBakPaged.safePage}
              pageSize={10}
              onChange={(p) => {
                setLocalBakPage(p);
                resetConsoleScroll();
              }}
            />
          </div>

          <div class="volo-card">
            <div class="volo-sec-head">
              <div class="volo-sec-copy">
                <h3 class="volo-card-title">{t("sys.doctor_head")}</h3>
                <p class="volo-sec-desc">{t("sys.doctor_desc")}</p>
              </div>
              <div style={{ display: "flex", alignItems: "center", gap: "8px", flexWrap: "wrap" }}>
                {doctorReport && (
                  <span class={`volo-status-pill ${doctorReport.healthy ? "ok" : "danger"}`}>
                    <span class="volo-status-dot" />
                    {doctorReport.healthy ? t("sys.doctor_healthy") : t("sys.doctor_issues")}
                  </span>
                )}
                <button type="button" class="volo-btn volo-btn-sm" onClick={() => loadSub()}>
                  {t("common.refresh")}
                </button>
              </div>
            </div>
            <div class="volo-table-wrap">
              <table class="volo-table">
                <thead>
                  <tr>
                    <th class="col-cat">{t("sys.doctor_col_check")}</th>
                    <th class="col-status">{t("art.status")}</th>
                    <th class="col-fluid">{t("sys.doctor_col_summary")}</th>
                  </tr>
                </thead>
                <tbody>
                  {!doctorReport ? (
                    <tr>
                      <td colSpan={3} class="volo-empty-state">
                        {t("sys.doctor_loading")}
                      </td>
                    </tr>
                  ) : (
                    (doctorReport.checks || []).map((chk: any) => (
                      <tr key={chk.name}>
                        <td class="col-cat">
                          <span class="volo-code-chip">{chk.name}</span>
                        </td>
                        <td class="col-status">
                          <span class={`volo-status-pill ${chk.status === "ok" ? "ok" : chk.status === "warn" ? "warn" : "danger"}`}>
                            <span class="volo-status-dot" />
                            {String(chk.status || "ok").toUpperCase()}
                          </span>
                        </td>
                        <td class="col-fluid">
                          <div class="volo-cell-main">
                            <div class="volo-cell-title" style={{ fontWeight: 550 }}>
                              {chk.summary}
                            </div>
                            {chk.detail && (
                              <div class="volo-cell-meta">
                                <span>{chk.detail}</span>
                              </div>
                            )}
                          </div>
                        </td>
                      </tr>
                    ))
                  )}
                </tbody>
              </table>
            </div>
          </div>
        </>
      )}

      {sub === "audit" && isAdmin && (
        <div class="volo-card">
          <div class="volo-sec-head">
            <div class="volo-sec-copy">
              <h3 class="volo-card-title">{t("sys.audit")}</h3>
              <p class="volo-sec-desc">{t("sys.audit_desc")}</p>
            </div>
            <button type="button" class="volo-btn volo-btn-sm" onClick={() => loadSub()}>
              {t("common.refresh")}
            </button>
          </div>
          <div class="volo-table-wrap">
            <table class="volo-table">
              <thead>
                <tr>
                  <th class="col-date">{t("sys.audit_col_time")}</th>
                  <th class="col-cat">{t("sys.audit_col_actor")}</th>
                  <th class="col-cat">{t("sys.audit_col_action")}</th>
                  <th class="col-fluid">{t("sys.audit_col_res")}</th>
                </tr>
              </thead>
              <tbody>
                {auditPaged.slice.length === 0 ? (
                  <tr>
                    <td colSpan={4} class="volo-empty-state">
                      {t("tbl.empty")}
                    </td>
                  </tr>
                ) : (
                  auditPaged.slice.map((l) => (
                    <tr key={l.id}>
                      <td class="col-date">{formatDateTime(l.created_at)}</td>
                      <td class="col-cat">
                        <span class="volo-cat-chip">{l.actor_name}</span>
                      </td>
                      <td class="col-cat">
                        <span class="volo-mini-tag">{l.action}</span>
                      </td>
                      <td class="col-fluid">
                        <div class="volo-cell-meta" style={{ color: "var(--volo-text)" }}>
                          <span>
                            {l.resource_type}:{l.resource_id}
                          </span>
                          {l.client_ip && <span class="volo-code-chip">{l.client_ip}</span>}
                        </div>
                      </td>
                    </tr>
                  ))
                )}
              </tbody>
            </table>
          </div>
          <Pagination
            total={auditLogs.length}
            page={auditPaged.safePage}
            pageSize={20}
            onChange={(p) => {
              setAuditPage(p);
              resetConsoleScroll();
            }}
          />
        </div>
      )}
    </div>
  );
}
