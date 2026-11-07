import { useEffect, useState } from "preact/hooks";
import { apiFetch, b64urlToBytes, bytesToB64url, csrfTokenSig, currentUserSig, refreshMe, showToast } from "../api";
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

function parseCSVList(raw: string): string[] {
  return raw
    .split(/[,，\n]/)
    .map((s) => s.trim())
    .filter(Boolean);
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

  // Account Security state
  const [meInfo, setMeInfo] = useState<any>(null);
  const [profileForm, setProfileForm] = useState({
    display_name: "",
    bio: "",
    website_url: "",
    current_password: "",
    new_password: "",
  });
  const [totpEnroll, setTotpEnroll] = useState<{ secret: string; otpauth_uri: string } | null>(null);
  const [totpCode, setTotpCode] = useState("");
  const [recoveryCodes, setRecoveryCodes] = useState<string[] | null>(null);
  const [passkeys, setPasskeys] = useState<any[]>([]);
  const [pkName, setPkName] = useState(() => t("sys.pk_default_name"));
  const [sessions, setSessions] = useState<any[]>([]);
  const [tokens, setTokens] = useState<any[]>([]);
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
  const [userForm, setUserForm] = useState({ username: "", email: "", display_name: "", role: "editor", password: "" });
  const [bans, setBans] = useState<any[]>([]);
  const [banForm, setBanForm] = useState({ cidr: "", note: "manual", expires_hours: 24 });
  const [geoRules, setGeoRules] = useState<any[]>([]);
  const [geoCircuitEnabled, setGeoCircuitEnabled] = useState<boolean>(true);
  const [geoForm, setGeoForm] = useState({ name: "Geo Rule", action: "deny", countries: "", regions: "", sort_order: 10 });
  const [includeMediaExport, setIncludeMediaExport] = useState<boolean>(true);
  const [dryRunRestore, setDryRunRestore] = useState<boolean>(false);
  const [doctorReport, setDoctorReport] = useState<any>(null);
  const [auditLogs, setAuditLogs] = useState<any[]>([]);

  useEffect(() => {
    if (activeTab) {
      setSub(activeTab);
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
        setProfileForm({
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
      } else if (sub === "backup" && isAdmin) {
        const [doc, st] = await Promise.all([
          apiFetch<any>("/api/admin/doctor"),
          apiFetch<any>("/api/admin/settings"),
        ]);
        setDoctorReport(doc);
        setDomains(st.domains || {});
        setVersions(st.versions || {});
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
      showToast(t("common.saved"));
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
                onNavigate?.(k);
              }}
            >
              {t(`sys.${k}`)}
            </button>
          ))}
        </div>
      )}

      {sub === "settings_site" && isAdmin && domains && (
        <div class="volo-grid-2">
          {/* Site Identity Domain */}
          <div class="volo-card">
            <div class="volo-sec-head">
              <div class="volo-sec-copy">
                <h3 class="volo-card-title">{t("sys.dom1_head")}</h3>
                <p class="volo-sec-desc">{t("sys.dom1_desc")}</p>
              </div>
            </div>
            <div class="volo-field">
              <label>{t("sys.site_title_sub")}</label>
              <div class="volo-grid-2">
                <input class="volo-input" placeholder={t("sys.site_title_ph")} value={domains.site?.title || ""} onInput={(e) => setDomField("site", "title", (e.currentTarget as HTMLInputElement).value)} />
                <input class="volo-input" placeholder={t("sys.site_sub_ph")} value={domains.site?.subtitle || ""} onInput={(e) => setDomField("site", "subtitle", (e.currentTarget as HTMLInputElement).value)} />
              </div>
            </div>
            <div class="volo-field">
              <label>{t("sys.site_desc")}</label>
              <textarea class="volo-textarea" rows={2} value={domains.site?.description || ""} onInput={(e) => setDomField("site", "description", (e.currentTarget as HTMLTextAreaElement).value)} />
            </div>
            <div class="volo-field">
              <label>{t("sys.site_logo_fav")}</label>
              <div class="volo-grid-2">
                <input class="volo-input" placeholder={t("sys.site_logo_ph")} value={domains.site?.logo_url || ""} onInput={(e) => setDomField("site", "logo_url", (e.currentTarget as HTMLInputElement).value)} />
                <input class="volo-input" placeholder={t("sys.site_fav_ph")} value={domains.site?.favicon_url || ""} onInput={(e) => setDomField("site", "favicon_url", (e.currentTarget as HTMLInputElement).value)} />
              </div>
            </div>
            <div class="volo-grid-2">
              <div class="volo-field">
                <label>{t("sys.default_locale")}</label>
                <select class="volo-select" value={domains.site?.default_locale || "zh-CN"} onChange={(e) => setDomField("site", "default_locale", (e.currentTarget as HTMLSelectElement).value)}>
                  {locales.map((loc) => (
                    <option key={loc.code} value={loc.code}>{loc.name} ({loc.code})</option>
                  ))}
                </select>
              </div>
              <div class="volo-field">
                <label>{t("setup.timezone")}</label>
                <input class="volo-input" value={domains.site?.timezone || "Asia/Shanghai"} onInput={(e) => setDomField("site", "timezone", (e.currentTarget as HTMLInputElement).value)} />
              </div>
            </div>
            <div class="volo-field">
              <label>{t("sys.notice_board")}</label>
              <textarea class="volo-textarea" rows={2} value={domains.site?.notice_board || ""} onInput={(e) => setDomField("site", "notice_board", (e.currentTarget as HTMLTextAreaElement).value)} />
            </div>
            <div class="volo-field">
              <label>{t("sys.license_tpl")}</label>
              <input class="volo-input" value={domains.site?.license_template || ""} onInput={(e) => setDomField("site", "license_template", (e.currentTarget as HTMLInputElement).value)} />
            </div>
            <div class="volo-field">
              <label>{t("sys.footer_text")}</label>
              <input class="volo-input" value={domains.site?.footer_text || ""} onInput={(e) => setDomField("site", "footer_text", (e.currentTarget as HTMLInputElement).value)} />
            </div>
            <div class="volo-field">
              <label>{t("sys.custom_head")}</label>
              <textarea class="volo-textarea" rows={2} style={{ fontFamily: "var(--volo-mono)", fontSize: "12px" }} value={domains.site?.custom_head_meta || ""} onInput={(e) => setDomField("site", "custom_head_meta", (e.currentTarget as HTMLTextAreaElement).value)} />
            </div>

            <div class="volo-check-grid">
              <label class="volo-check-item">
                <input type="checkbox" checked={domains.site?.allow_locale_switch !== false} onChange={(e) => setDomField("site", "allow_locale_switch", (e.currentTarget as HTMLInputElement).checked)} />
                <span>{t("sys.allow_locale_switch")}</span>
              </label>
              <label class="volo-check-item">
                <input type="checkbox" checked={!!domains.site?.show_admin_login_entry} onChange={(e) => setDomField("site", "show_admin_login_entry", (e.currentTarget as HTMLInputElement).checked)} />
                <span>{t("sys.show_admin_entry")}</span>
              </label>
            </div>

            <div class="volo-subcard">
              <div class="volo-subcard-title">{t("sys.social_title")}</div>
              <div style={{ display: "flex", gap: "6px", marginBottom: "8px" }}>
                <input class="volo-input" style={{ maxWidth: "150px" }} placeholder={t("sys.social_label_ph")} value={socialDraft.label} onInput={(e) => setSocialDraft({ ...socialDraft, label: (e.currentTarget as HTMLInputElement).value })} />
                <input class="volo-input" placeholder={t("sys.social_url_ph")} value={socialDraft.url} onInput={(e) => setSocialDraft({ ...socialDraft, url: (e.currentTarget as HTMLInputElement).value })} />
                <button
                  type="button"
                  class="volo-btn volo-btn-sm"
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
              {Array.isArray(domains.site?.social_links) && domains.site.social_links.length > 0 && (
                <div style={{ display: "flex", gap: "6px", flexWrap: "wrap" }}>
                  {domains.site.social_links.map((sl: any, idx: number) => (
                    <span key={sl.id || idx} class="volo-cat-chip" style={{ maxWidth: "none" }}>
                      <strong>{sl.label}</strong>
                      <span class="volo-help">{sl.url}</span>
                      <button
                        type="button"
                        style={{ border: 0, background: "transparent", cursor: "pointer", color: "inherit", fontWeight: 700 }}
                        onClick={() => {
                          const next = domains.site.social_links.filter((_: any, i: number) => i !== idx);
                          setDomField("site", "social_links", next);
                        }}
                      >
                        ×
                      </button>
                    </span>
                  ))}
                </div>
              )}
            </div>

            <button type="button" class="volo-btn volo-btn-primary" onClick={() => patchDomain("site", domains.site || {})}>
              {t("sys.save_site")}
            </button>
          </div>

          {/* Reading & Comment Protection Domain */}
          <div class="volo-card">
            <div class="volo-sec-head">
              <div class="volo-sec-copy">
                <h3 class="volo-card-title">{t("sys.dom2_head")}</h3>
                <p class="volo-sec-desc">{t("sys.dom2_desc")}</p>
              </div>
            </div>
            <div class="volo-grid-3">
              <div class="volo-field">
                <label>{t("sys.list_style")}</label>
                <select class="volo-select" value={domains.reading?.list_style || "summary"} onChange={(e) => setDomField("reading", "list_style", (e.currentTarget as HTMLSelectElement).value)}>
                  <option value="summary">{t("sys.list_summary")}</option>
                  <option value="title_only">{t("sys.list_title_only")}</option>
                  <option value="full">{t("sys.list_full")}</option>
                </select>
              </div>
              <div class="volo-field">
                <label>{t("sys.posts_per_page")}</label>
                <input type="number" class="volo-input" value={domains.reading?.posts_per_page ?? 15} onInput={(e) => setDomField("reading", "posts_per_page", Number((e.currentTarget as HTMLInputElement).value))} />
              </div>
              <div class="volo-field">
                <label>{t("sys.outdated_days")}</label>
                <input type="number" class="volo-input" value={domains.reading?.outdated_notice_days ?? 365} onInput={(e) => setDomField("reading", "outdated_notice_days", Number((e.currentTarget as HTMLInputElement).value))} />
              </div>
            </div>

            <div class="volo-field">
              <label>{t("sys.sidebar_counts")}</label>
              <div style={{ display: "grid", gridTemplateColumns: "repeat(4, minmax(0, 1fr))", gap: "8px" }}>
                <input type="number" class="volo-input" value={domains.reading?.recent_posts_count ?? 8} onInput={(e) => setDomField("reading", "recent_posts_count", Number((e.currentTarget as HTMLInputElement).value))} />
                <input type="number" class="volo-input" value={domains.reading?.recent_comments_count ?? 8} onInput={(e) => setDomField("reading", "recent_comments_count", Number((e.currentTarget as HTMLInputElement).value))} />
                <input type="number" class="volo-input" value={domains.reading?.popular_tags_count ?? 20} onInput={(e) => setDomField("reading", "popular_tags_count", Number((e.currentTarget as HTMLInputElement).value))} />
                <input type="number" class="volo-input" value={domains.reading?.related_posts_count ?? 5} onInput={(e) => setDomField("reading", "related_posts_count", Number((e.currentTarget as HTMLInputElement).value))} />
              </div>
            </div>

            <div class="volo-check-grid">
              <label class="volo-check-item">
                <input type="checkbox" checked={domains.reading?.enable_reading_stats !== false} onChange={(e) => setDomField("reading", "enable_reading_stats", (e.currentTarget as HTMLInputElement).checked)} />
                <span>{t("sys.enable_read_stats")}</span>
              </label>
              <label class="volo-check-item">
                <input type="checkbox" checked={domains.reading?.enable_instant_search !== false} onChange={(e) => setDomField("reading", "enable_instant_search", (e.currentTarget as HTMLInputElement).checked)} />
                <span>{t("sys.enable_search")}</span>
              </label>
              <label class="volo-check-item">
                <input type="checkbox" checked={!!domains.reading?.show_code_line_numbers} onChange={(e) => setDomField("reading", "show_code_line_numbers", (e.currentTarget as HTMLInputElement).checked)} />
                <span>{t("sys.show_line_num")}</span>
              </label>
              <label class="volo-check-item">
                <input type="checkbox" checked={!!domains.reading?.allow_reader_register} onChange={(e) => setDomField("reading", "allow_reader_register", (e.currentTarget as HTMLInputElement).checked)} />
                <span>{t("sys.allow_reader_reg")}</span>
              </label>
            </div>

            <div class="volo-subcard">
              <div class="volo-subcard-title">{t("sys.cmt_policy_title")}</div>
              <div class="volo-grid-3">
                <div class="volo-field">
                  <label>{t("sys.cmt_access")}</label>
                  <select class="volo-select" value={domains.reading?.comment_access_mode || "all"} onChange={(e) => setDomField("reading", "comment_access_mode", (e.currentTarget as HTMLSelectElement).value)}>
                    <option value="all">{t("sys.cmt_access_all")}</option>
                    <option value="registered">{t("sys.cmt_access_reg")}</option>
                    <option value="off">{t("sys.cmt_access_off")}</option>
                  </select>
                </div>
                <div class="volo-field">
                  <label>{t("sys.cmt_mod")}</label>
                  <select class="volo-select" value={domains.reading?.comment_moderation_mode || "smart"} onChange={(e) => setDomField("reading", "comment_moderation_mode", (e.currentTarget as HTMLSelectElement).value)}>
                    <option value="smart">{t("sys.cmt_mod_smart")}</option>
                    <option value="all">{t("sys.cmt_mod_all")}</option>
                    <option value="off">{t("sys.cmt_mod_off")}</option>
                  </select>
                </div>
                <div class="volo-field">
                  <label>{t("sys.pow_bits")}</label>
                  <input type="number" class="volo-input" value={domains.reading?.pow_difficulty_bits ?? 16} onInput={(e) => setDomField("reading", "pow_difficulty_bits", Number((e.currentTarget as HTMLInputElement).value))} />
                </div>
              </div>
              <label class="volo-check-item" style={{ marginBottom: "10px" }}>
                <input type="checkbox" checked={domains.reading?.builtin_spam_filter !== false} onChange={(e) => setDomField("reading", "builtin_spam_filter", (e.currentTarget as HTMLInputElement).checked)} />
                <span>{t("sys.spam_filter")}</span>
              </label>
              <div class="volo-grid-2">
                <div class="volo-field" style={{ marginBottom: 0 }}>
                  <label>{t("sys.blocked_kw")}</label>
                  <input
                    class="volo-input"
                    value={Array.isArray(domains.reading?.blocked_keywords) ? domains.reading.blocked_keywords.join(", ") : ""}
                    onInput={(e) => setDomField("reading", "blocked_keywords", parseCSVList((e.currentTarget as HTMLInputElement).value))}
                  />
                </div>
                <div class="volo-field" style={{ marginBottom: 0 }}>
                  <label>{t("sys.allowed_kw")}</label>
                  <input
                    class="volo-input"
                    value={Array.isArray(domains.reading?.allowed_keywords) ? domains.reading.allowed_keywords.join(", ") : ""}
                    onInput={(e) => setDomField("reading", "allowed_keywords", parseCSVList((e.currentTarget as HTMLInputElement).value))}
                  />
                </div>
              </div>
            </div>

            <button type="button" class="volo-btn volo-btn-primary" onClick={() => patchDomain("reading", domains.reading || {})}>
              {t("sys.save_reading")}
            </button>
          </div>
        </div>
      )}

      {sub === "settings_io" && isAdmin && domains && (
        <div class="volo-grid-2">
          {/* Media Storage Engine Domain */}
          <div class="volo-card">
            <div class="volo-sec-head">
              <div class="volo-sec-copy">
                <h3 class="volo-card-title">{t("sys.dom4_head")}</h3>
                <p class="volo-sec-desc">{t("sys.dom4_desc")}</p>
              </div>
            </div>
            <div class="volo-grid-3">
              <div class="volo-field">
                <label>{t("sys.storage_driver")}</label>
                <select class="volo-select" value={domains.storage?.active_driver || "local"} onChange={(e) => setDomField("storage", "active_driver", (e.currentTarget as HTMLSelectElement).value)}>
                  <option value="local">{t("sys.driver_local")}</option>
                  <option value="s3">{t("sys.driver_s3")}</option>
                </select>
              </div>
              <div class="volo-field">
                <label>{t("sys.max_upload_mb")}</label>
                <input type="number" class="volo-input" value={domains.storage?.max_upload_mb ?? 50} onInput={(e) => setDomField("storage", "max_upload_mb", Number((e.currentTarget as HTMLInputElement).value))} />
              </div>
              <div class="volo-field">
                <label>{t("sys.img_quality")}</label>
                <input type="number" class="volo-input" value={domains.storage?.image_quality ?? 85} onInput={(e) => setDomField("storage", "image_quality", Number((e.currentTarget as HTMLInputElement).value))} />
              </div>
            </div>

            <div class="volo-check-grid">
              <label class="volo-check-item">
                <input type="checkbox" checked={domains.storage?.strip_exif !== false} onChange={(e) => setDomField("storage", "strip_exif", (e.currentTarget as HTMLInputElement).checked)} />
                <span>{t("sys.strip_exif")}</span>
              </label>
              <label class="volo-check-item">
                <input type="checkbox" checked={domains.storage?.adaptive_webp !== false} onChange={(e) => setDomField("storage", "adaptive_webp", (e.currentTarget as HTMLInputElement).checked)} />
                <span>{t("sys.adaptive_webp")}</span>
              </label>
            </div>

            <div class="volo-subcard">
              <div class="volo-subcard-title">{t("sys.s3_sub_title")}</div>
              <div class="volo-grid-2" style={{ marginBottom: "8px" }}>
                <input class="volo-input" placeholder={t("sys.s3_endpoint")} value={domains.storage?.s3_endpoint || ""} onInput={(e) => setDomField("storage", "s3_endpoint", (e.currentTarget as HTMLInputElement).value)} />
                <input class="volo-input" placeholder={t("sys.s3_region_ph")} value={domains.storage?.s3_region || ""} onInput={(e) => setDomField("storage", "s3_region", (e.currentTarget as HTMLInputElement).value)} />
              </div>
              <div class="volo-grid-3" style={{ marginBottom: "8px" }}>
                <input class="volo-input" placeholder={t("sys.s3_bucket_ph")} value={domains.storage?.s3_bucket || ""} onInput={(e) => setDomField("storage", "s3_bucket", (e.currentTarget as HTMLInputElement).value)} />
                <input class="volo-input" placeholder={t("sys.s3_ak_ph")} value={domains.storage?.s3_access_key || ""} onInput={(e) => setDomField("storage", "s3_access_key", (e.currentTarget as HTMLInputElement).value)} />
                <input type="password" class="volo-input" placeholder={domains.storage?.s3_secret_key_set ? t("sys.s3_sk_sealed_ph") : t("sys.s3_sk_ph")} value={s3Secret} onInput={(e) => setS3Secret((e.currentTarget as HTMLInputElement).value)} />
              </div>
              <div class="volo-grid-2" style={{ marginBottom: "8px" }}>
                <input class="volo-input" placeholder={t("sys.s3_pub_url_ph")} value={domains.storage?.s3_public_base_url || ""} onInput={(e) => setDomField("storage", "s3_public_base_url", (e.currentTarget as HTMLInputElement).value)} />
                <input class="volo-input" placeholder={t("sys.s3_prefix_ph")} value={domains.storage?.s3_key_prefix || ""} onInput={(e) => setDomField("storage", "s3_key_prefix", (e.currentTarget as HTMLInputElement).value)} />
              </div>
              <label class="volo-check-item">
                <input type="checkbox" checked={!!domains.storage?.s3_path_style} onChange={(e) => setDomField("storage", "s3_path_style", (e.currentTarget as HTMLInputElement).checked)} />
                <span>{t("sys.s3_path_style")}</span>
              </label>
            </div>

            <div style={{ display: "flex", gap: "8px" }}>
              <button
                type="button"
                class="volo-btn volo-btn-primary"
                onClick={() => {
                  const body: any = { ...(domains.storage || {}) };
                  if (s3Secret) body.s3_secret_key = s3Secret;
                  void patchDomain("storage", body);
                  setS3Secret("");
                }}
              >
                {t("sys.save_storage")}
              </button>
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
            </div>
          </div>

          {/* SMTP Mail Notifications Domain */}
          <div class="volo-card">
            <div class="volo-sec-head">
              <div class="volo-sec-copy">
                <h3 class="volo-card-title">{t("sys.dom3_head")}</h3>
                <p class="volo-sec-desc">{t("sys.dom3_desc")}</p>
              </div>
            </div>
            <div class="volo-grid-3">
              <div class="volo-field">
                <label>{t("sys.smtp_host")}</label>
                <input class="volo-input" value={domains.notifications?.smtp_host || ""} onInput={(e) => setDomField("notifications", "smtp_host", (e.currentTarget as HTMLInputElement).value)} />
              </div>
              <div class="volo-field">
                <label>{t("sys.smtp_port")}</label>
                <input type="number" class="volo-input" value={domains.notifications?.smtp_port ?? 465} onInput={(e) => setDomField("notifications", "smtp_port", Number((e.currentTarget as HTMLInputElement).value))} />
              </div>
              <div class="volo-field">
                <label>{t("sys.smtp_tls")}</label>
                <select class="volo-select" value={domains.notifications?.smtp_security || "tls"} onChange={(e) => setDomField("notifications", "smtp_security", (e.currentTarget as HTMLSelectElement).value)}>
                  <option value="tls">{t("sys.tls_implicit")}</option>
                  <option value="starttls">{t("sys.tls_starttls")}</option>
                  <option value="plain_internal">{t("sys.tls_plain")}</option>
                </select>
              </div>
            </div>
            <div class="volo-grid-2">
              <div class="volo-field">
                <label>{t("sys.smtp_user_from")}</label>
                <input class="volo-input" placeholder={t("sys.smtp_user_ph")} value={domains.notifications?.smtp_username || ""} onInput={(e) => setDomField("notifications", "smtp_username", (e.currentTarget as HTMLInputElement).value)} />
                <input class="volo-input" style={{ marginTop: "6px" }} placeholder={t("sys.smtp_from_ph")} value={domains.notifications?.smtp_from_email || ""} onInput={(e) => setDomField("notifications", "smtp_from_email", (e.currentTarget as HTMLInputElement).value)} />
              </div>
              <div class="volo-field">
                <label>
                  {t("sys.smtp_pwd")} {domains.notifications?.smtp_password_set && <span class="volo-badge volo-badge-ok">{t("sys.sealed")}</span>}
                </label>
                <input type="password" class="volo-input" placeholder={t("sys.keep_blank_ph")} value={smtpPwd} onInput={(e) => setSmtpPwd((e.currentTarget as HTMLInputElement).value)} />
                <input class="volo-input" style={{ marginTop: "6px" }} placeholder={t("sys.smtp_from_name_ph")} value={domains.notifications?.smtp_from_name || ""} onInput={(e) => setDomField("notifications", "smtp_from_name", (e.currentTarget as HTMLInputElement).value)} />
              </div>
            </div>

            <div class="volo-check-grid">
              <label class="volo-check-item">
                <input type="checkbox" checked={!!domains.notifications?.smtp_enabled} onChange={(e) => setDomField("notifications", "smtp_enabled", (e.currentTarget as HTMLInputElement).checked)} />
                <span>{t("sys.enable_smtp")}</span>
              </label>
              <label class="volo-check-item">
                <input type="checkbox" checked={domains.notifications?.smtp_auth !== false} onChange={(e) => setDomField("notifications", "smtp_auth", (e.currentTarget as HTMLInputElement).checked)} />
                <span>{t("sys.smtp_auth")}</span>
              </label>
              <label class="volo-check-item">
                <input type="checkbox" checked={domains.notifications?.notify_admin_on_comment !== false} onChange={(e) => setDomField("notifications", "notify_admin_on_comment", (e.currentTarget as HTMLInputElement).checked)} />
                <span>{t("sys.notify_admin_cmt")}</span>
              </label>
              <label class="volo-check-item">
                <input type="checkbox" checked={domains.notifications?.notify_reply_by_email !== false} onChange={(e) => setDomField("notifications", "notify_reply_by_email", (e.currentTarget as HTMLInputElement).checked)} />
                <span>{t("sys.notify_reply_mail")}</span>
              </label>
            </div>

            <div class="volo-subcard">
              <div style={{ display: "flex", gap: "8px" }}>
                <input class="volo-input" placeholder={t("sys.test_mail_ph")} value={testMailTo} onInput={(e) => setTestMailTo((e.currentTarget as HTMLInputElement).value)} />
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
              </div>
            </div>

            <button
              type="button"
              class="volo-btn volo-btn-primary"
              onClick={() => {
                const body: any = { ...(domains.notifications || {}) };
                if (smtpPwd) body.smtp_password = smtpPwd;
                void patchDomain("notifications", body);
                setSmtpPwd("");
              }}
            >
              {t("sys.save_notify")}
            </button>
          </div>
        </div>
      )}

      {sub === "account" && (
        <div class="volo-grid-2">
          <div class="volo-card">
            <div class="volo-sec-head">
              <div class="volo-sec-copy">
                <h3 class="volo-card-title">{t("sys.profile_head")}</h3>
                <p class="volo-sec-desc">{t("sys.profile_desc")}</p>
              </div>
            </div>
            <form
              onSubmit={async (e) => {
                e.preventDefault();
                try {
                  await apiFetch("/api/admin/me", { method: "PATCH", body: JSON.stringify(profileForm) });
                  setProfileForm({ ...profileForm, current_password: "", new_password: "" });
                  await refreshMe();
                  showToast(t("common.saved"));
                } catch (err: any) {
                  showToast(err.message);
                }
              }}
            >
              <div class="volo-grid-2">
                <div class="volo-field">
                  <label>{t("setup.display_name")}</label>
                  <input class="volo-input" value={profileForm.display_name} onInput={(e) => setProfileForm({ ...profileForm, display_name: (e.currentTarget as HTMLInputElement).value })} />
                </div>
                <div class="volo-field">
                  <label>{t("sys.website_url")}</label>
                  <input class="volo-input" placeholder="https://" value={profileForm.website_url} onInput={(e) => setProfileForm({ ...profileForm, website_url: (e.currentTarget as HTMLInputElement).value })} />
                </div>
              </div>
              <div class="volo-field">
                <label>{t("sys.bio")}</label>
                <textarea class="volo-textarea" rows={2} value={profileForm.bio} onInput={(e) => setProfileForm({ ...profileForm, bio: (e.currentTarget as HTMLTextAreaElement).value })} />
              </div>
              <div class="volo-grid-2">
                <div class="volo-field">
                  <label>{t("sys.cur_pwd")}</label>
                  <input type="password" class="volo-input" value={profileForm.current_password} onInput={(e) => setProfileForm({ ...profileForm, current_password: (e.currentTarget as HTMLInputElement).value })} />
                </div>
                <div class="volo-field">
                  <label>{t("sys.new_pwd")}</label>
                  <input
                    type="password"
                    class="volo-input"
                    minLength={6}
                    value={profileForm.new_password}
                    onInput={(e) => setProfileForm({ ...profileForm, new_password: (e.currentTarget as HTMLInputElement).value })}
                  />
                </div>
              </div>
              <button type="submit" class="volo-btn volo-btn-primary">{t("common.save")}</button>
            </form>
          </div>

          <div class="volo-card">
            <div class="volo-sec-head">
              <div class="volo-sec-copy">
                <h3 class="volo-card-title">{t("sys.totp_head")}</h3>
                <p class="volo-sec-desc">{t("sys.totp_desc")}</p>
              </div>
            </div>
            <div class="volo-subcard" style={{ marginTop: 0 }}>
              {meInfo?.totp_enabled ? (
                <div style={{ display: "flex", alignItems: "center", justifyContent: "space-between", gap: "10px" }}>
                  <span class="volo-status-pill ok">
                    <span class="volo-status-dot" />
                    {t("sys.totp_enabled")}
                  </span>
                  <button
                    type="button"
                    class="volo-act-btn danger"
                    onClick={async () => {
                      if (!window.confirm(t("sys.confirm_disable_totp"))) return;
                      await apiFetch("/api/admin/security/totp", { method: "DELETE" });
                      void loadSub();
                    }}
                  >
                    {t("sys.totp_disable")}
                  </button>
                </div>
              ) : totpEnroll ? (
                <div>
                  <div class="volo-help" style={{ wordBreak: "break-all", marginBottom: "8px" }}>
                    {t("sys.totp_secret")} <code>{totpEnroll.secret}</code><br />
                    {t("sys.totp_uri")} <code>{totpEnroll.otpauth_uri}</code>
                  </div>
                  <div style={{ display: "flex", gap: "8px" }}>
                    <input class="volo-input" placeholder={t("sys.totp_code_ph")} value={totpCode} onInput={(e) => setTotpCode((e.currentTarget as HTMLInputElement).value)} />
                    <button
                      type="button"
                      class="volo-btn volo-btn-primary"
                      onClick={async () => {
                        const res = await apiFetch<{ recovery_codes: string[] }>("/api/admin/security/totp/enroll", {
                          method: "POST",
                          body: JSON.stringify({ secret: totpEnroll.secret, code: totpCode }),
                        });
                        setRecoveryCodes(res.recovery_codes || []);
                        setTotpEnroll(null);
                        void loadSub();
                      }}
                    >
                      {t("sys.totp_activate")}
                    </button>
                  </div>
                </div>
              ) : (
                <button
                  type="button"
                  class="volo-btn volo-btn-sm"
                  onClick={async () => {
                    const res = await apiFetch<{ secret: string; otpauth_uri: string }>("/api/admin/security/totp/setup", { method: "POST", body: "{}" });
                    setTotpEnroll(res);
                  }}
                >
                  {t("sys.totp_enroll")}
                </button>
              )}
              {recoveryCodes && (
                <pre style={{ background: "var(--volo-surface)", padding: "10px", borderRadius: "8px", fontSize: "12px", marginTop: "8px" }}>
                  {t("sys.recovery_codes_tip")}{"\n"}{recoveryCodes.join("   ")}
                </pre>
              )}
            </div>

            <div style={{ display: "flex", gap: "8px", marginBottom: "12px" }}>
              <input class="volo-input" placeholder={t("sys.pk_name_ph")} value={pkName} onInput={(e) => setPkName((e.currentTarget as HTMLInputElement).value)} />
              <button type="button" class="volo-btn volo-btn-sm volo-btn-primary" onClick={registerPasskey}>{t("sys.pk_add")}</button>
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
                      <td colSpan={3} class="volo-empty-state">{t("tbl.empty")}</td>
                    </tr>
                  ) : (
                    passkeys.map((pk) => (
                      <tr key={pk.id}>
                        <td><div class="volo-cell-title">{pk.name}</div></td>
                        <td class="col-date">{formatDateTime(pk.created_at)}</td>
                        <td class="col-act">
                          <div class="volo-row-actions">
                            <button type="button" class="volo-act-btn danger" onClick={async () => {
                              await apiFetch(`/api/admin/security/passkeys/${pk.id}`, { method: "DELETE" });
                              void loadSub();
                            }}>{t("common.delete")}</button>
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
            </div>
            <div class="volo-form-bar">
              <div style={{ display: "flex", gap: "8px" }}>
                <input class="volo-input" placeholder={t("sys.pat_label_ph")} value={patName} onInput={(e) => setPatName((e.currentTarget as HTMLInputElement).value)} />
                <button
                  type="button"
                  class="volo-btn volo-btn-primary volo-btn-sm"
                  onClick={async () => {
                    const res = await apiFetch<{ plaintext_token: string }>("/api/admin/security/tokens", {
                      method: "POST",
                      body: JSON.stringify({ name: patName, scopes: patScopes }),
                    });
                    setIssuedPAT(res.plaintext_token);
                    void loadSub();
                  }}
                >
                  {t("sys.pat_generate")}
                </button>
              </div>
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
            {issuedPAT && (
              <div class="volo-subcard" style={{ background: "var(--volo-accent-soft)", wordBreak: "break-all", fontSize: "12px" }}>
                <strong>{t("sys.pat_copy_once")}</strong> <code>{issuedPAT}</code>
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
                      <td colSpan={3} class="volo-empty-state">{t("tbl.empty")}</td>
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
                        <td><span class="volo-help">{tk.scopes_json}</span></td>
                        <td class="col-act">
                          <div class="volo-row-actions">
                            <button type="button" class="volo-act-btn danger" onClick={async () => {
                              await apiFetch(`/api/admin/security/tokens/${tk.id}`, { method: "DELETE" });
                              void loadSub();
                            }}>{t("sys.pat_revoke")}</button>
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
                  {sessions.map((s) => (
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
                            <span>{String(s.user_agent || "").slice(0, 64)}</span>
                            <span>{formatDateTime(s.last_seen_at)}</span>
                          </div>
                        </div>
                      </td>
                      <td class="col-act">
                        <div class="volo-row-actions">
                          {s.id !== meInfo?.session_id && (
                            <button type="button" class="volo-act-btn danger" onClick={async () => {
                              await apiFetch(`/api/admin/security/sessions/${s.id}`, { method: "DELETE" });
                              void loadSub();
                            }}>{t("sys.sess_kick")}</button>
                          )}
                        </div>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          </div>
        </div>
      )}

      {sub === "users" && isAdmin && (
        <div class="volo-card">
          <div class="volo-sec-head">
            <div class="volo-sec-copy">
              <h3 class="volo-card-title">{t("sys.users_head")}</h3>
              <p class="volo-sec-desc">{t("sys.users_desc")}</p>
            </div>
          </div>
          <form
            onSubmit={async (e) => {
              e.preventDefault();
              try {
                await apiFetch("/api/admin/users", { method: "POST", body: JSON.stringify(userForm) });
                setUserForm({ username: "", email: "", display_name: "", role: "editor", password: "" });
                showToast(t("common.saved"));
                void loadSub();
              } catch (err: any) {
                showToast(err.message);
              }
            }}
            class="volo-form-bar"
          >
            <div class="volo-grid-3">
              <input class="volo-input" required placeholder={t("sys.user_name_ph")} value={userForm.username} onInput={(e) => setUserForm({ ...userForm, username: (e.currentTarget as HTMLInputElement).value })} />
              <input class="volo-input" placeholder={t("sys.user_disp_ph")} value={userForm.display_name} onInput={(e) => setUserForm({ ...userForm, display_name: (e.currentTarget as HTMLInputElement).value })} />
              <input type="email" class="volo-input" required placeholder={t("sys.user_email_ph")} value={userForm.email} onInput={(e) => setUserForm({ ...userForm, email: (e.currentTarget as HTMLInputElement).value })} />
            </div>
            <div style={{ display: "flex", gap: "8px", marginTop: "8px" }}>
              <select class="volo-select" style={{ maxWidth: "240px" }} value={userForm.role} onChange={(e) => setUserForm({ ...userForm, role: (e.currentTarget as HTMLSelectElement).value })}>
                <option value="editor">{t("sys.role_editor_opt")}</option>
                <option value="reader">{t("sys.role_reader_opt")}</option>
                <option value="admin">{t("sys.role_admin_opt")}</option>
              </select>
              <input
                type="password"
                class="volo-input"
                required
                minLength={6}
                placeholder={t("sys.user_pwd_ph")}
                value={userForm.password}
                onInput={(e) => setUserForm({ ...userForm, password: (e.currentTarget as HTMLInputElement).value })}
              />
              <button type="submit" class="volo-btn volo-btn-primary">{t("common.create")}</button>
            </div>
          </form>
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
                {users.length === 0 ? (
                  <tr>
                    <td colSpan={5} class="volo-empty-state">{t("tbl.empty")}</td>
                  </tr>
                ) : (
                  users.map((u) => {
                    const isSelf = u.id === currentUserSig.value?.id;
                    return (
                      <tr key={u.id}>
                        <td class="col-fluid">
                          <div class="volo-cell-main">
                            <div class="volo-cell-title">
                              <span>{u.display_name || u.username}</span>
                              <span class="volo-code-chip">@{u.username}</span>
                            </div>
                            {u.email && <div class="volo-cell-meta"><span>{u.email}</span></div>}
                          </div>
                        </td>
                        <td class="col-cat">
                          {isSelf ? (
                            <span class="volo-cat-chip">{t(`role.${u.role}`)}</span>
                          ) : (
                            <select
                              class="volo-select"
                              style={{ height: "30px", padding: "0 8px", fontSize: "12px" }}
                              value={u.role}
                              onChange={async (e) => {
                                const nextRole = (e.currentTarget as HTMLSelectElement).value;
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
                            >
                              <option value="admin">{t("role.admin")}</option>
                              <option value="editor">{t("role.editor")}</option>
                              <option value="reader">{t("role.reader")}</option>
                            </select>
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
                            {!isSelf && (
                              <>
                                <button
                                  type="button"
                                  class="volo-act-btn"
                                  onClick={async () => {
                                    const nextSt = u.status === "active" ? "disabled" : "active";
                                    await apiFetch(`/api/admin/users/${u.id}`, {
                                      method: "PATCH",
                                      body: JSON.stringify({ status: nextSt }),
                                    });
                                    void loadSub();
                                  }}
                                >
                                  {u.status === "active" ? t("sys.user_disable") : t("sys.user_enable")}
                                </button>
                                <button type="button" class="volo-act-btn danger" onClick={async () => {
                                  await apiFetch(`/api/admin/users/${u.id}`, { method: "DELETE" });
                                  void loadSub();
                                }}>{t("common.delete")}</button>
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
        </div>
      )}

      {sub === "waf" && isAdmin && (
        <>
          {domains?.security && (
            <div class="volo-card">
              <div class="volo-sec-head">
                <div class="volo-sec-copy">
                  <h3 class="volo-card-title">{t("sys.sec_policy_head")}</h3>
                  <p class="volo-sec-desc">{t("sys.sec_policy_desc")}</p>
                </div>
                <button
                  type="button"
                  class="volo-btn volo-btn-sm"
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
              </div>
              <div class="volo-grid-3">
                <div class="volo-field">
                  <label>{t("sys.waf_window_sec")}</label>
                  <input type="number" class="volo-input" value={domains.security?.waf_window_seconds ?? 180} onInput={(e) => setDomField("security", "waf_window_seconds", Number((e.currentTarget as HTMLInputElement).value))} />
                </div>
                <div class="volo-field">
                  <label>{t("sys.waf_max_req")}</label>
                  <input type="number" class="volo-input" value={domains.security?.waf_max_requests ?? 180} onInput={(e) => setDomField("security", "waf_max_requests", Number((e.currentTarget as HTMLInputElement).value))} />
                </div>
                <div class="volo-field">
                  <label>{t("sys.totp_scope")}</label>
                  <select class="volo-select" value={domains.security?.totp_required_scope || "off"} onChange={(e) => setDomField("security", "totp_required_scope", (e.currentTarget as HTMLSelectElement).value)}>
                    <option value="off">{t("sys.totp_scope_off")}</option>
                    <option value="admin">{t("sys.totp_scope_admin")}</option>
                    <option value="staff">{t("sys.totp_scope_staff")}</option>
                  </select>
                </div>
              </div>
              <div class="volo-field">
                <label>{t("sys.dyn_login_path")}</label>
                <input class="volo-input" placeholder="/secret-entry" value={domains.security?.dynamic_login_path || ""} onInput={(e) => setDomField("security", "dynamic_login_path", (e.currentTarget as HTMLInputElement).value)} />
              </div>
              <div class="volo-check-grid">
                <label class="volo-check-item">
                  <input type="checkbox" checked={domains.security?.risk_control_enabled !== false} onChange={(e) => setDomField("security", "risk_control_enabled", (e.currentTarget as HTMLInputElement).checked)} />
                  <span>{t("sys.risk_on")}</span>
                </label>
                <label class="volo-check-item">
                  <input type="checkbox" checked={domains.security?.waf_enabled !== false} onChange={(e) => setDomField("security", "waf_enabled", (e.currentTarget as HTMLInputElement).checked)} />
                  <span>{t("sys.waf_on")}</span>
                </label>
                <label class="volo-check-item">
                  <input type="checkbox" checked={!!domains.security?.geo_policy_active} onChange={(e) => setDomField("security", "geo_policy_active", (e.currentTarget as HTMLInputElement).checked)} />
                  <span>{t("sys.geo_policy_on")}</span>
                </label>
              </div>
              <button type="button" class="volo-btn volo-btn-primary" onClick={() => patchDomain("security", domains.security || {})}>
                {t("sys.save_sec_policy")}
              </button>
            </div>
          )}

          <div class="volo-grid-2">
            <div class="volo-card">
              <div class="volo-sec-head">
                <div class="volo-sec-copy">
                  <h3 class="volo-card-title">{t("sys.waf_bans_head")}</h3>
                  <p class="volo-sec-desc">{t("sys.waf_bans_desc")}</p>
                </div>
              </div>
              <form
                onSubmit={async (e) => {
                  e.preventDefault();
                  await apiFetch("/api/admin/security/ip-bans", { method: "POST", body: JSON.stringify(banForm) });
                  setBanForm({ cidr: "", note: "manual", expires_hours: 24 });
                  void loadSub();
                }}
                class="volo-form-bar"
                style={{ display: "flex", gap: "8px", flexWrap: "wrap" }}
              >
                <input class="volo-input" style={{ flex: 1, minWidth: "170px" }} required placeholder={t("sys.waf_cidr_ph")} value={banForm.cidr} onInput={(e) => setBanForm({ ...banForm, cidr: (e.currentTarget as HTMLInputElement).value })} />
                <select class="volo-select" style={{ width: "136px" }} value={banForm.expires_hours} onChange={(e) => setBanForm({ ...banForm, expires_hours: Number((e.currentTarget as HTMLSelectElement).value) })}>
                  <option value={24}>{t("sys.waf_dur_24h")}</option>
                  <option value={168}>{t("sys.waf_dur_7d")}</option>
                  <option value={720}>{t("sys.waf_dur_30d")}</option>
                  <option value={0}>{t("sys.waf_dur_perm")}</option>
                </select>
                <button type="submit" class="volo-btn volo-btn-primary">{t("sys.waf_ban_btn")}</button>
              </form>
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
                    {bans.length === 0 ? (
                      <tr>
                        <td colSpan={3} class="volo-empty-state">{t("tbl.empty")}</td>
                      </tr>
                    ) : (
                      bans.map((b) => (
                        <tr key={b.id}>
                          <td><span class="volo-code-chip">{b.cidr}</span></td>
                          <td>
                            <div class="volo-cell-meta">
                              <span>{b.note || b.reason || "—"}</span>
                              <span>{b.expires_at > 0 ? formatDateTime(b.expires_at) : t("sys.waf_dur_perm")}</span>
                            </div>
                          </td>
                          <td class="col-act">
                            <div class="volo-row-actions">
                              <button type="button" class="volo-act-btn" onClick={async () => {
                                await apiFetch(`/api/admin/security/ip-bans/${b.id}`, { method: "DELETE" });
                                void loadSub();
                              }}>{t("sys.waf_unban_btn")}</button>
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
                  <h3 class="volo-card-title">{t("sys.geo_head")}</h3>
                  <p class="volo-sec-desc">{t("sys.geo_desc")}</p>
                </div>
              </div>
              {!geoCircuitEnabled && (
                <div class="volo-subcard" style={{ marginTop: 0, fontSize: "12.5px", color: "var(--volo-warn)" }}>
                  {t("sys.geo_circuit_off")}
                </div>
              )}
              <form
                onSubmit={async (e) => {
                  e.preventDefault();
                  await apiFetch("/api/admin/security/geo-rules", { method: "POST", body: JSON.stringify({ ...geoForm, is_enabled: true }) });
                  setGeoForm({ name: "Geo Rule", action: "deny", countries: "", regions: "", sort_order: 10 });
                  void loadSub();
                }}
                class="volo-form-bar"
              >
                <div class="volo-grid-3">
                  <select class="volo-select" value={geoForm.action} onChange={(e) => setGeoForm({ ...geoForm, action: (e.currentTarget as HTMLSelectElement).value })}>
                    <option value="deny">{t("sys.geo_deny")}</option>
                    <option value="allow">{t("sys.geo_allow")}</option>
                  </select>
                  <input class="volo-input" placeholder={t("sys.geo_iso_ph")} value={geoForm.countries} onInput={(e) => setGeoForm({ ...geoForm, countries: (e.currentTarget as HTMLInputElement).value })} />
                  <div style={{ display: "flex", gap: "6px" }}>
                    <input class="volo-input" placeholder={t("sys.geo_region_ph")} value={geoForm.regions} onInput={(e) => setGeoForm({ ...geoForm, regions: (e.currentTarget as HTMLInputElement).value })} />
                    <button type="submit" class="volo-btn volo-btn-primary">+</button>
                  </div>
                </div>
              </form>
              <div class="volo-table-wrap">
                <table class="volo-table" style={{ minWidth: "100%" }}>
                  <thead>
                    <tr>
                      <th>{t("art.status")}</th>
                      <th>{t("tbl.col_scope")}</th>
                      <th class="col-act col-act-th">{t("art.actions")}</th>
                    </tr>
                  </thead>
                  <tbody>
                    {geoRules.length === 0 ? (
                      <tr>
                        <td colSpan={3} class="volo-empty-state">{t("tbl.empty")}</td>
                      </tr>
                    ) : (
                      geoRules.map((g) => (
                        <tr key={g.id}>
                          <td>
                            <span class={`volo-status-pill ${g.action === "deny" ? "danger" : "ok"}`}>
                              <span class="volo-status-dot" />
                              {g.action === "deny" ? t("sys.geo_deny") : t("sys.geo_allow")}
                            </span>
                          </td>
                          <td>
                            <span class="volo-code-chip">
                              ISO={g.countries || "*"} · {g.regions || "*"}
                            </span>
                          </td>
                          <td class="col-act">
                            <div class="volo-row-actions">
                              <button type="button" class="volo-act-btn danger" onClick={async () => {
                                await apiFetch(`/api/admin/security/geo-rules/${g.id}`, { method: "DELETE" });
                                void loadSub();
                              }}>{t("common.delete")}</button>
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
        </>
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

              <div class="volo-subcard" style={{ marginTop: 0 }}>
                <div class="volo-check-grid" style={{ marginTop: 0 }}>
                  <label class="volo-check-item">
                    <input type="checkbox" checked={includeMediaExport} onChange={(e) => setIncludeMediaExport((e.currentTarget as HTMLInputElement).checked)} />
                    <span>{t("sys.backup_with_media")}</span>
                  </label>
                  <label class="volo-check-item">
                    <input type="checkbox" checked={dryRunRestore} onChange={(e) => setDryRunRestore((e.currentTarget as HTMLInputElement).checked)} />
                    <span>{t("sys.backup_dry_run")}</span>
                  </label>
                </div>

                <div style={{ display: "flex", gap: "10px", flexWrap: "wrap" }}>
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

                  <label class="volo-btn" style={{ cursor: "pointer" }}>
                    {t("sys.backup_restore")}
                    <input
                      type="file"
                      accept=".zip"
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
                        } catch (err: any) {
                          showToast(err.message);
                        }
                        (e.currentTarget as HTMLInputElement).value = "";
                      }}
                    />
                  </label>

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
                        } catch (err: any) {
                          showToast(err.message);
                        }
                        (e.currentTarget as HTMLInputElement).value = "";
                      }}
                    />
                  </label>
                </div>
              </div>
            </div>

            {domains?.backup && (
              <div class="volo-card">
                <div class="volo-sec-head">
                  <div class="volo-sec-copy">
                    <h3 class="volo-card-title">{t("sys.auto_bak_head")}</h3>
                    <p class="volo-sec-desc">{t("sys.auto_bak_desc")}</p>
                  </div>
                </div>
                <div class="volo-grid-3">
                  <div class="volo-field">
                    <label>{t("sys.auto_interval_h")}</label>
                    <input type="number" class="volo-input" value={domains.backup?.local_interval_hours ?? 24} onInput={(e) => setDomField("backup", "local_interval_hours", Number((e.currentTarget as HTMLInputElement).value))} />
                  </div>
                  <div class="volo-field">
                    <label>{t("sys.auto_keep_cnt")}</label>
                    <input type="number" class="volo-input" value={domains.backup?.local_keep_count ?? 7} onInput={(e) => setDomField("backup", "local_keep_count", Number((e.currentTarget as HTMLInputElement).value))} />
                  </div>
                  <div class="volo-field">
                    <label>{t("sys.remote_proto")}</label>
                    <select class="volo-select" value={domains.backup?.remote_protocol || "s3"} onChange={(e) => setDomField("backup", "remote_protocol", (e.currentTarget as HTMLSelectElement).value)}>
                      <option value="s3">S3 / R2 / MinIO</option>
                      <option value="webdav">WebDAV</option>
                    </select>
                  </div>
                </div>
                <div class="volo-check-grid">
                  <label class="volo-check-item">
                    <input type="checkbox" checked={!!domains.backup?.local_auto_enabled} onChange={(e) => setDomField("backup", "local_auto_enabled", (e.currentTarget as HTMLInputElement).checked)} />
                    <span>{t("sys.auto_local_on")}</span>
                  </label>
                  <label class="volo-check-item">
                    <input type="checkbox" checked={!!domains.backup?.include_media_in_auto} onChange={(e) => setDomField("backup", "include_media_in_auto", (e.currentTarget as HTMLInputElement).checked)} />
                    <span>{t("sys.auto_media_on")}</span>
                  </label>
                  <label class="volo-check-item">
                    <input type="checkbox" checked={!!domains.backup?.remote_enabled} onChange={(e) => setDomField("backup", "remote_enabled", (e.currentTarget as HTMLInputElement).checked)} />
                    <span>{t("sys.remote_bak_on")}</span>
                  </label>
                </div>

                {domains.backup?.remote_protocol === "webdav" ? (
                  <div class="volo-subcard">
                    <div class="volo-grid-2" style={{ marginBottom: "8px" }}>
                      <input class="volo-input" placeholder={t("sys.webdav_url_ph")} value={domains.backup?.webdav_url || ""} onInput={(e) => setDomField("backup", "webdav_url", (e.currentTarget as HTMLInputElement).value)} />
                      <input class="volo-input" placeholder={t("sys.webdav_dir_ph")} value={domains.backup?.webdav_dir || ""} onInput={(e) => setDomField("backup", "webdav_dir", (e.currentTarget as HTMLInputElement).value)} />
                    </div>
                    <div class="volo-grid-2">
                      <input class="volo-input" placeholder={t("sys.webdav_user_ph")} value={domains.backup?.webdav_user || ""} onInput={(e) => setDomField("backup", "webdav_user", (e.currentTarget as HTMLInputElement).value)} />
                      <input type="password" class="volo-input" placeholder={domains.backup?.webdav_password_set ? t("sys.s3_sk_sealed_ph") : t("sys.webdav_pwd_ph")} value={bakWebdavPwd} onInput={(e) => setBakWebdavPwd((e.currentTarget as HTMLInputElement).value)} />
                    </div>
                  </div>
                ) : (
                  <div class="volo-subcard">
                    <div class="volo-grid-2" style={{ marginBottom: "8px" }}>
                      <input class="volo-input" placeholder={t("sys.s3_endpoint")} value={domains.backup?.s3_endpoint || ""} onInput={(e) => setDomField("backup", "s3_endpoint", (e.currentTarget as HTMLInputElement).value)} />
                      <input class="volo-input" placeholder={t("sys.s3_bucket_ph")} value={domains.backup?.s3_bucket || ""} onInput={(e) => setDomField("backup", "s3_bucket", (e.currentTarget as HTMLInputElement).value)} />
                    </div>
                    <div class="volo-grid-2">
                      <input class="volo-input" placeholder={t("sys.s3_ak_ph")} value={domains.backup?.s3_access_key || ""} onInput={(e) => setDomField("backup", "s3_access_key", (e.currentTarget as HTMLInputElement).value)} />
                      <input type="password" class="volo-input" placeholder={domains.backup?.s3_secret_key_set ? t("sys.s3_sk_sealed_ph") : t("sys.s3_sk_ph")} value={bakS3Secret} onInput={(e) => setBakS3Secret((e.currentTarget as HTMLInputElement).value)} />
                    </div>
                  </div>
                )}

                <button
                  type="button"
                  class="volo-btn volo-btn-primary"
                  onClick={() => {
                    const body: any = { ...(domains.backup || {}) };
                    if (bakWebdavPwd) body.webdav_password = bakWebdavPwd;
                    if (bakS3Secret) body.s3_secret_key = bakS3Secret;
                    void patchDomain("backup", body);
                    setBakWebdavPwd("");
                    setBakS3Secret("");
                  }}
                >
                  {t("sys.save_backup_cfg")}
                </button>
              </div>
            )}
          </div>

          <div class="volo-card">
            <div class="volo-sec-head">
              <div class="volo-sec-copy">
                <h3 class="volo-card-title">{t("sys.doctor_head")}</h3>
                <p class="volo-sec-desc">{t("sys.doctor_desc")}</p>
              </div>
              <div style={{ display: "flex", alignItems: "center", gap: "8px" }}>
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
                      <td colSpan={3} class="volo-empty-state">{t("sys.doctor_loading")}</td>
                    </tr>
                  ) : (
                    (doctorReport.checks || []).map((chk: any) => (
                      <tr key={chk.name}>
                        <td class="col-cat"><span class="volo-code-chip">{chk.name}</span></td>
                        <td class="col-status">
                          <span class={`volo-status-pill ${chk.status === "ok" ? "ok" : chk.status === "warn" ? "warn" : "danger"}`}>
                            <span class="volo-status-dot" />
                            {String(chk.status || "ok").toUpperCase()}
                          </span>
                        </td>
                        <td class="col-fluid">
                          <div class="volo-cell-main">
                            <div class="volo-cell-title" style={{ fontWeight: 550 }}>{chk.summary}</div>
                            {chk.detail && <div class="volo-cell-meta"><span>{chk.detail}</span></div>}
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
                {auditLogs.length === 0 ? (
                  <tr>
                    <td colSpan={4} class="volo-empty-state">{t("tbl.empty")}</td>
                  </tr>
                ) : (
                  auditLogs.map((l) => (
                    <tr key={l.id}>
                      <td class="col-date">{formatDateTime(l.created_at)}</td>
                      <td class="col-cat"><span class="volo-cat-chip">{l.actor_name}</span></td>
                      <td class="col-cat"><span class="volo-mini-tag">{l.action}</span></td>
                      <td class="col-fluid">
                        <div class="volo-cell-meta" style={{ color: "var(--volo-text)" }}>
                          <span>{l.resource_type}:{l.resource_id}</span>
                          {l.client_ip && <span class="volo-code-chip">{l.client_ip}</span>}
                        </div>
                      </td>
                    </tr>
                  ))
                )}
              </tbody>
            </table>
          </div>
        </div>
      )}
    </div>
  );
}
