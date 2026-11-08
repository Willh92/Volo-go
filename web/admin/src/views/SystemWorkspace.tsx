import { useEffect, useRef, useState } from "preact/hooks";
import {
  apiFetch,
  b64urlToBytes,
  bootConfig,
  bytesToB64url,
  currentUserSig,
  openConfirm,
  refreshMe,
  resetConsoleScroll,
  sanitizeAdminPath,
  showToast,
  storageDriverSig,
  uploadWithProgress,
} from "../api";
import { MediaPickButton } from "../components/MediaLibraryModal";
import {
  ActionMenu,
  AnimatedModal,
  BatchBar,
  ConsoleEmpty,
  ConsoleSelect,
  MobileSelectAllBar,
  NavIconPreview,
  Pagination,
  paginateItems,
  SwitchToggle,
  TableSkeleton,
  TokenListInput,
  UploadButton,
  UploadProgressBar,
  useDebouncedValue,
  useFlipList,
  PlusIcon,
  CloseIcon,
} from "../components/UIControls";
import {
  availableLocalesSig,
  formatDateTime,
  localizeAuditAction,
  localizeAuditResource,
  localizeBackupTable,
  localizeWafReason,
  t,
} from "../i18n";

export type SysTab =
  | "settings_site"
  | "settings_io"
  | "users"
  | "backup"
  | "account"
  | "waf"
  | "logs"
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

function parseScopes(raw: unknown): string[] {
  if (Array.isArray(raw)) return raw.map(String).filter(Boolean);
  const str = String(raw || "").trim();
  if (!str) return [];
  try {
    const parsed = JSON.parse(str);
    if (Array.isArray(parsed)) return parsed.map(String).filter(Boolean);
  } catch {
    // Fallback to CSV split
  }
  return parseCSVList(str);
}

function summarizeUA(raw: string): string {
  const ua = String(raw || "").trim();
  if (!ua) return "—";
  let browser = "";
  if (/Edg\//i.test(ua)) browser = "Edge";
  else if (/OPR\/|Opera/i.test(ua)) browser = "Opera";
  else if (/Firefox\//i.test(ua)) browser = "Firefox";
  else if (/Obsidian/i.test(ua)) browser = "Obsidian";
  else if (/curl\//i.test(ua)) browser = "curl";
  else if (/CriOS|Chrome\//i.test(ua)) browser = "Chrome";
  else if (/Safari\//i.test(ua)) browser = "Safari";

  let os = "";
  if (/iPhone|iPad|iPod/i.test(ua)) os = "iOS";
  else if (/Android/i.test(ua)) os = "Android";
  else if (/Mac OS X|Macintosh/i.test(ua)) os = "macOS";
  else if (/Windows/i.test(ua)) os = "Windows";
  else if (/Linux/i.test(ua)) os = "Linux";

  if (browser && os) return `${browser} · ${os}`;
  if (browser || os) return browser || os;
  return ua.length > 42 ? `${ua.slice(0, 42)}…` : ua;
}

function formatDetailJSON(raw: unknown): string {
  const str = typeof raw === "string" ? raw.trim() : JSON.stringify(raw ?? {});
  if (!str || str === "{}" || str === "null") return "";
  try {
    const obj = JSON.parse(str);
    if (!obj || (typeof obj === "object" && Object.keys(obj).length === 0)) return "";
    return JSON.stringify(obj, null, 2);
  } catch {
    return str;
  }
}

function formatBytes(bytes: number): string {
  const n = Number(bytes || 0);
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`;
  return `${(n / (1024 * 1024)).toFixed(2)} MB`;
}

function formatLogTs(ms: number): string {
  if (!ms) return "";
  const d = new Date(ms);
  const p = (n: number) => String(n).padStart(2, "0");
  return `${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`;
}

function extractLogAttrs(entry: any): { msg: string; attrs: Array<[string, string]> } {
  const attrs: Array<[string, string]> = [];
  const rawAttrs = entry?.attrs ?? entry?.fields;
  if (rawAttrs) {
    try {
      const obj = typeof rawAttrs === "string" ? JSON.parse(rawAttrs) : rawAttrs;
      if (obj && typeof obj === "object" && !Array.isArray(obj)) {
        for (const [k, v] of Object.entries(obj)) {
          attrs.push([k, typeof v === "object" ? JSON.stringify(v) : String(v)]);
        }
      }
    } catch {
      // ignore malformed attrs JSON
    }
  }
  let msg = String(entry?.message || "");
  if (attrs.length === 0 && msg.includes("=")) {
    const tokens = msg.split(/\s+/);
    const tailAttrs: Array<[string, string]> = [];
    while (tokens.length > 1) {
      const last = tokens[tokens.length - 1];
      const m = /^([a-zA-Z_][a-zA-Z0-9_.-]*)=([^\s]+)$/.exec(last);
      if (!m) break;
      tailAttrs.unshift([m[1], m[2]]);
      tokens.pop();
    }
    if (tailAttrs.length > 0) {
      msg = tokens.join(" ");
      attrs.push(...tailAttrs);
    }
  }
  return { msg, attrs };
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
    case "logs":
      return t("nav.logs");
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
  const [loading, setLoading] = useState(false);

  const [accSub, setAccSub] = useState<"profile" | "sessions">("profile");
  const [wafSub, setWafSub] = useState<"policy" | "bans" | "geo">("policy");

  const [usersPage, setUsersPage] = useState(1);
  const [usersPageSize, setUsersPageSize] = useState(20);
  const [bansPage, setBansPage] = useState(1);
  const [bansPageSize, setBansPageSize] = useState(20);
  const [geoPage, setGeoPage] = useState(1);
  const [geoPageSize, setGeoPageSize] = useState(20);
  const [localBakPage, setLocalBakPage] = useState(1);
  const [auditPage, setAuditPage] = useState(1);
  const [auditPageSize, setAuditPageSize] = useState(20);
  const [pkPage, setPkPage] = useState(1);
  const [patPage, setPatPage] = useState(1);
  const [sessPage, setSessPage] = useState(1);

  // Account Security state
  const [meInfo, setMeInfo] = useState<any>(null);
  const [profileForm, setProfileForm] = useState({
    avatar_url: currentUserSig.value?.avatar_url || "",
    display_name: "",
    email: currentUserSig.value?.email || "",
    bio: "",
    website_url: "",
    current_password: "",
    new_password: "",
  });
  const [avatarImgErr, setAvatarImgErr] = useState(false);
  const [totpEnroll, setTotpEnroll] = useState<{
    secret: string;
    otpauth_url?: string;
    otpauth_uri?: string;
    qr_svg?: string;
  } | null>(null);
  const [totpCode, setTotpCode] = useState("");
  const [totpCurrPwd, setTotpCurrPwd] = useState("");
  const [totpDisableOpen, setTotpDisableOpen] = useState(false);
  const [totpDisablePwd, setTotpDisablePwd] = useState("");
  const [recoveryCodes, setRecoveryCodes] = useState<string[] | null>(null);
  const [passkeys, setPasskeys] = useState<any[]>([]);
  const [pkModalOpen, setPkModalOpen] = useState(false);
  const [pkName, setPkName] = useState(() => t("sys.pk_default_name"));
  const [pkCurrPwd, setPkCurrPwd] = useState("");
  const [delPkTarget, setDelPkTarget] = useState<{ id: string; name: string } | null>(null);
  const [delPkPwd, setDelPkPwd] = useState("");
  const [oidcLinks, setOidcLinks] = useState<Array<{ id: string; name: string; credential_id: string; created_at: number }>>([]);
  const [oidcProviders, setOidcProviders] = useState<Array<{ name: string; display_name?: string }>>([]);
  const [unlinkOidcTarget, setUnlinkOidcTarget] = useState<{ id: string; name: string } | null>(null);
  const [unlinkOidcPwd, setUnlinkOidcPwd] = useState("");
  const [sessions, setSessions] = useState<any[]>([]);
  const [revokingOthers, setRevokingOthers] = useState(false);
  const [tokens, setTokens] = useState<any[]>([]);
  const [patModalOpen, setPatModalOpen] = useState(false);
  const [patName, setPatName] = useState("Obsidian CLI");
  const [patScopes, setPatScopes] = useState<string[]>(["articles:read", "articles:write", "media:write"]);
  const [patExpiresDays, setPatExpiresDays] = useState<number>(90);
  const [patCurrPwd, setPatCurrPwd] = useState("");
  const [issuedPAT, setIssuedPAT] = useState<string>("");

  // Atomic Domain Settings state
  const [domains, setDomains] = useState<Record<string, any> | null>(null);
  const [versions, setVersions] = useState<Record<string, number>>({});
  const [savingDomain, setSavingDomain] = useState<string>("");
  const [storageDrivers, setStorageDrivers] = useState<any[]>([]);
  const [defaultUploadDir, setDefaultUploadDir] = useState<string>("./uploads");
  const [smtpPwd, setSmtpPwd] = useState("");
  const [testMailTo, setTestMailTo] = useState("");
  const [s3Secret, setS3Secret] = useState("");
  const [bakWebdavPwd, setBakWebdavPwd] = useState("");
  const [bakS3Secret, setBakS3Secret] = useState("");
  const [socialDraft, setSocialDraft] = useState({ label: "", url: "" });
  const [editingSocialIdx, setEditingSocialIdx] = useState<number | null>(null);
  const [socialDirty, setSocialDirty] = useState(false);

  // Users (server-paginated)
  const [users, setUsers] = useState<any[]>([]);
  const [usersTotal, setUsersTotal] = useState(0);
  const [userRoleFilter, setUserRoleFilter] = useState<"all" | "admin" | "editor" | "reader">("all");
  const [userQuery, setUserQuery] = useState("");
  const debouncedUserQ = useDebouncedValue(userQuery, 250);
  const [userModalOpen, setUserModalOpen] = useState(false);
  const [editingUserId, setEditingUserId] = useState<string | null>(null);
  const [origUserEmail, setOrigUserEmail] = useState<string>("");
  const [userForm, setUserForm] = useState({
    username: "",
    email: "",
    display_name: "",
    role: "editor",
    status: "active",
    password: "",
    current_password: "",
  });

  // WAF IP Bans (server-paginated) + GeoIP
  const [bans, setBans] = useState<any[]>([]);
  const [bansTotal, setBansTotal] = useState(0);
  const [banStateFilter, setBanStateFilter] = useState<"active" | "expired" | "all">("active");
  const [banQuery, setBanQuery] = useState("");
  const debouncedBanQ = useDebouncedValue(banQuery, 250);
  const [selBanIds, setSelBanIds] = useState<string[]>([]);
  const [banModalOpen, setBanModalOpen] = useState(false);
  const [banForm, setBanForm] = useState({ cidr: "", note: "manual", expires_hours: 24 });

  const [geoRules, setGeoRules] = useState<any[]>([]);
  const [geoQuery, setGeoQuery] = useState("");
  const [geoCircuitEnabled, setGeoCircuitEnabled] = useState<boolean>(true);
  const [defaultGeoIPDir, setDefaultGeoIPDir] = useState<string>("./data/geoip");
  const [effectiveGeoIPDir, setEffectiveGeoIPDir] = useState<string>("./data/geoip");
  const [geoDBStatus, setGeoDBStatus] = useState<{
    path?: string;
    active_files?: string[];
    skipped_files?: string[];
  } | null>(null);
  const [reloadingGeo, setReloadingGeo] = useState<boolean>(false);
  const [geoModalOpen, setGeoModalOpen] = useState(false);
  const [editingGeoId, setEditingGeoId] = useState<string | null>(null);
  const [movingGeo, setMovingGeo] = useState(false);
  const geoFlip = useFlipList<HTMLTableSectionElement>();
  const socialFlip = useFlipList<HTMLDivElement>();
  const [geoForm, setGeoForm] = useState({
    name: "Geo Rule",
    action: "deny",
    countries: "",
    regions: "",
    sort_order: 10,
    is_enabled: true,
    on_miss: "next",
  });

  // Backup & Doctor
  const [includeMediaExport, setIncludeMediaExport] = useState<boolean>(true);
  const [exportingBackup, setExportingBackup] = useState<boolean>(false);
  const [exportBakOpen, setExportBakOpen] = useState<boolean>(false);
  const [exportBakPwd, setExportBakPwd] = useState<string>("");
  const [dryRunRestore, setDryRunRestore] = useState<boolean>(false);
  const [restoreCurrPwd, setRestoreCurrPwd] = useState<string>("");
  const [legacyDryRun, setLegacyDryRun] = useState<boolean>(true);
  const [legacyBusy, setLegacyBusy] = useState<boolean>(false);
  const [legacyProgress, setLegacyProgress] = useState<number | null>(null);
  const [restoreBusy, setRestoreBusy] = useState<boolean>(false);
  const [restoreProgress, setRestoreProgress] = useState<number | null>(null);
  const [legacyReport, setLegacyReport] = useState<any>(null);
  const [localBakEndpoint, setLocalBakEndpoint] = useState<string>("/api/admin/backups/local");
  const [localBackups, setLocalBackups] = useState<
    Array<{ name: string; size_bytes: number; mod_time?: number | string; created_at?: number | string }>
  >([]);
  const [creatingSnapshot, setCreatingSnapshot] = useState<boolean>(false);
  const [doctorReport, setDoctorReport] = useState<any>(null);

  // Audit Logs (server-paginated + persistent distinct actions)
  const [auditLogs, setAuditLogs] = useState<any[]>([]);
  const [auditTotal, setAuditTotal] = useState(0);
  const [knownAuditActions, setKnownAuditActions] = useState<string[]>([]);
  const [auditQuery, setAuditQuery] = useState("");
  const debouncedAuditQ = useDebouncedValue(auditQuery, 250);
  const [auditAction, setAuditAction] = useState("all");
  const [expandedAuditId, setExpandedAuditId] = useState<string | null>(null);

  // Runtime Logs
  const [runtimeLogs, setRuntimeLogs] = useState<any[]>([]);
  const [logStats, setLogStats] = useState<any>(null);
  const [logLevel, setLogLevel] = useState("ALL");
  const [logQuery, setLogQuery] = useState("");
  const debouncedLogQ = useDebouncedValue(logQuery, 250);
  const [autoScrollLogs, setAutoScrollLogs] = useState<boolean>(true);
  const logViewerRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    setAvatarImgErr(false);
  }, [profileForm.avatar_url]);

  useEffect(() => {
    if (activeTab) {
      setSub(activeTab);
      resetConsoleScroll();
    }
  }, [activeTab]);

  const loadRuntimeLogs = async (lvl = logLevel, q = debouncedLogQ) => {
    try {
      const params = new URLSearchParams({ limit: "400" });
      if (lvl && lvl !== "ALL") params.set("level", lvl);
      if (q.trim()) params.set("q", q.trim());
      const res = await apiFetch<any>(`/api/admin/runtime-logs?${params.toString()}`);
      setRuntimeLogs(res.items || []);
      setLogStats(res);
    } catch (err: any) {
      showToast(err.message);
    }
  };

  const loadUsersList = async () => {
    if (!isAdmin) return;
    setLoading(true);
    try {
      const params = new URLSearchParams();
      params.set("page", String(usersPage));
      params.set("per_page", String(usersPageSize));
      if (userRoleFilter !== "all") params.set("role", userRoleFilter);
      if (debouncedUserQ.trim()) params.set("q", debouncedUserQ.trim());
      const res = await apiFetch<{ items: any[]; total?: number }>(`/api/admin/users?${params.toString()}`);
      const list = res.items || [];
      setUsers(list);
      setUsersTotal(res.total ?? list.length);
    } catch (err: any) {
      showToast(err.message);
    } finally {
      setLoading(false);
    }
  };

  const loadBansList = async () => {
    if (!isAdmin) return;
    setLoading(true);
    try {
      const params = new URLSearchParams();
      params.set("page", String(bansPage));
      params.set("per_page", String(bansPageSize));
      params.set("state", banStateFilter);
      if (debouncedBanQ.trim()) params.set("q", debouncedBanQ.trim());
      const b = await apiFetch<{ items: any[]; total?: number }>(`/api/admin/security/ip-bans?${params.toString()}`);
      const list = b.items || [];
      setBans(list);
      setBansTotal(b.total ?? list.length);
    } catch (err: any) {
      showToast(err.message);
    } finally {
      setLoading(false);
    }
  };

  const loadAuditLogs = async (q = debouncedAuditQ, act = auditAction, pg = auditPage, sz = auditPageSize) => {
    if (!isAdmin) return;
    setLoading(true);
    try {
      const params = new URLSearchParams();
      params.set("page", String(pg));
      params.set("per_page", String(sz));
      if (q.trim()) params.set("q", q.trim());
      if (act && act !== "all") params.set("action", act);
      const res = await apiFetch<{ items: any[]; total?: number; actions?: string[] }>(
        `/api/admin/audit-logs?${params.toString()}`
      );
      const list = res.items || [];
      setAuditLogs(list);
      setAuditTotal(res.total ?? list.length);
      const seen = list.map((l) => String(l.action || "")).filter(Boolean);
      const serverActions = Array.isArray(res.actions) ? res.actions.map(String).filter(Boolean) : [];
      if (seen.length > 0 || serverActions.length > 0) {
        setKnownAuditActions((prev) => Array.from(new Set([...prev, ...serverActions, ...seen])).sort());
      }
    } catch (err: any) {
      showToast(err.message);
    } finally {
      setLoading(false);
    }
  };

  const loadSub = async () => {
    if (sub === "users" && isAdmin) {
      await loadUsersList();
      return;
    }
    if (sub === "audit" && isAdmin) {
      await loadAuditLogs();
      return;
    }
    if (sub === "logs" && isAdmin) {
      setLoading(true);
      await loadRuntimeLogs(logLevel, debouncedLogQ);
      setLoading(false);
      return;
    }
    setLoading(true);
    try {
      if (sub === "account") {
        const [me, pks, sess, tks, oidc] = await Promise.all([
          apiFetch<any>("/api/admin/me"),
          apiFetch<{ items: any[] }>("/api/admin/security/passkeys?per_page=100"),
          apiFetch<{ items: any[] }>("/api/admin/security/sessions?per_page=100"),
          apiFetch<{ items: any[] }>("/api/admin/security/tokens?per_page=100"),
          apiFetch<{ items?: any[]; providers?: any[] }>("/api/admin/security/oidc-links").catch(() => ({
            items: [],
            providers: [],
          })),
        ]);
        setMeInfo(me);
        if (me.storage_driver) storageDriverSig.value = me.storage_driver;
        setProfileForm({
          avatar_url: me.user?.avatar_url || "",
          display_name: me.user?.display_name || "",
          email: me.user?.email || "",
          bio: me.user?.bio || "",
          website_url: me.user?.website_url || "",
          current_password: "",
          new_password: "",
        });
        setPasskeys(pks.items || []);
        setSessions(sess.items || []);
        setTokens(tks.items || []);
        setOidcLinks(Array.isArray(oidc.items) ? oidc.items : []);
        setOidcProviders(Array.isArray(oidc.providers) ? oidc.providers : []);
      } else if ((sub === "settings_site" || sub === "settings_io") && isAdmin) {
        const res = await apiFetch<any>("/api/admin/settings");
        setDomains(res.domains || {});
        setVersions(res.versions || {});
        setSocialDirty(false);
        setEditingSocialIdx(null);
        if (res.default_upload_dir) setDefaultUploadDir(res.default_upload_dir);
        if (res.default_geoip_dir) setDefaultGeoIPDir(res.default_geoip_dir);
        if (res.effective_geoip_dir) setEffectiveGeoIPDir(res.effective_geoip_dir);
        if (res.geoip_status) setGeoDBStatus(res.geoip_status);
        if (Array.isArray(res.storage_drivers)) setStorageDrivers(res.storage_drivers);
        if (res.domains?.storage) storageDriverSig.value = res.domains.storage.active_driver || "local";
      } else if (sub === "waf" && isAdmin) {
        const banSp = new URLSearchParams({
          page: String(bansPage),
          per_page: String(bansPageSize),
          state: banStateFilter,
        });
        if (debouncedBanQ.trim()) banSp.set("q", debouncedBanQ.trim());
        const [b, g, st] = await Promise.all([
          apiFetch<{ items: any[]; total?: number }>(`/api/admin/security/ip-bans?${banSp.toString()}`),
          apiFetch<{
            items: any[];
            geo_access_circuit_enabled?: boolean;
            default_geoip_dir?: string;
            effective_geoip_dir?: string;
            geoip_status?: { path?: string; active_files?: string[]; skipped_files?: string[] };
          }>("/api/admin/security/geo-rules?per_page=200"),
          apiFetch<any>("/api/admin/settings"),
        ]);
        const bList = b.items || [];
        setBans(bList);
        setBansTotal(b.total ?? bList.length);
        setGeoRules(g.items || []);
        setGeoCircuitEnabled(g.geo_access_circuit_enabled !== false);
        setDomains(st.domains || {});
        setVersions(st.versions || {});
        if (st.default_upload_dir) setDefaultUploadDir(st.default_upload_dir);
        if (g.default_geoip_dir || st.default_geoip_dir) {
          setDefaultGeoIPDir(g.default_geoip_dir || st.default_geoip_dir);
        }
        if (g.effective_geoip_dir || st.effective_geoip_dir) {
          setEffectiveGeoIPDir(g.effective_geoip_dir || st.effective_geoip_dir);
        }
        if (g.geoip_status || st.geoip_status) {
          setGeoDBStatus(g.geoip_status || st.geoip_status);
        }
        if (st.domains?.storage) storageDriverSig.value = st.domains.storage.active_driver || "local";
      } else if (sub === "backup" && isAdmin) {
        const fetchLocalSnaps = async () => {
          try {
            const res = await apiFetch<{
              items: Array<{ name: string; size_bytes: number; mod_time?: number | string; created_at?: number | string }>;
            }>("/api/admin/backups/local");
            setLocalBakEndpoint("/api/admin/backups/local");
            return res;
          } catch {
            const res = await apiFetch<{
              items: Array<{ name: string; size_bytes: number; mod_time?: number | string; created_at?: number | string }>;
            }>("/api/admin/backup/local");
            setLocalBakEndpoint("/api/admin/backup/local");
            return res;
          }
        };
        const [doc, st, lb] = await Promise.all([
          apiFetch<any>("/api/admin/doctor"),
          apiFetch<any>("/api/admin/settings"),
          fetchLocalSnaps(),
        ]);
        setDoctorReport(doc);
        setDomains(st.domains || {});
        setVersions(st.versions || {});
        setLocalBackups(lb.items || []);
        if (st.default_upload_dir) setDefaultUploadDir(st.default_upload_dir);
        if (st.default_geoip_dir) setDefaultGeoIPDir(st.default_geoip_dir);
        if (st.effective_geoip_dir) setEffectiveGeoIPDir(st.effective_geoip_dir);
        if (st.geoip_status) setGeoDBStatus(st.geoip_status);
        if (Array.isArray(st.storage_drivers)) setStorageDrivers(st.storage_drivers);
        if (st.domains?.storage) storageDriverSig.value = st.domains.storage.active_driver || "local";
      }
    } catch (err: any) {
      showToast(err.message);
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    if (sub === "users" || sub === "audit" || sub === "logs") return;
    void loadSub();
  }, [sub]);

  // Users server-paginated effect
  useEffect(() => {
    if (sub !== "users" || !isAdmin) return;
    if (userQuery !== debouncedUserQ) return;
    void loadUsersList();
  }, [sub, usersPage, usersPageSize, userRoleFilter, debouncedUserQ]);

  // WAF IP Bans server-paginated effect
  useEffect(() => {
    if (sub !== "waf" || wafSub !== "bans" || !isAdmin) return;
    if (banQuery !== debouncedBanQ) return;
    void loadBansList();
  }, [sub, wafSub, bansPage, bansPageSize, banStateFilter, debouncedBanQ]);

  // Audit Logs server-paginated effect
  useEffect(() => {
    if (sub !== "audit" || !isAdmin) return;
    if (auditQuery !== debouncedAuditQ) return;
    void loadAuditLogs(debouncedAuditQ, auditAction, auditPage, auditPageSize);
  }, [sub, auditPage, auditPageSize, auditAction, debouncedAuditQ]);

  // Runtime Logs debounced search effect
  useEffect(() => {
    if (sub !== "logs" || !isAdmin) return;
    if (logQuery !== debouncedLogQ) return;
    void loadRuntimeLogs(logLevel, debouncedLogQ);
  }, [sub, logLevel, debouncedLogQ]);

  useEffect(() => {
    if (sub !== "logs" || !autoScrollLogs || !logViewerRef.current) return;
    logViewerRef.current.scrollTop = logViewerRef.current.scrollHeight;
  }, [sub, runtimeLogs, autoScrollLogs]);

  const patchDomain = async (domainName: string, bodyObj: Record<string, any>): Promise<boolean> => {
    if (savingDomain) return false;
    setSavingDomain(domainName);
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
      if (domainName === "site") {
        setSocialDirty(false);
        setEditingSocialIdx(null);
      }
      if (domainName === "security") {
        if (res.effective_geoip_dir) setEffectiveGeoIPDir(res.effective_geoip_dir);
        if (res.geoip_status) setGeoDBStatus(res.geoip_status);
        const rawDyn = String(res.value?.dynamic_login_path || "").trim();
        const fallbackMount = sanitizeAdminPath(bootConfig.base_admin_path || "/console");
        const nextMount = rawDyn ? sanitizeAdminPath(rawDyn) : fallbackMount;
        const curMount = sanitizeAdminPath(window.location.pathname.replace(/\/+$/, "") || "/console");
        bootConfig.admin_path = nextMount;
        if (nextMount !== curMount) {
          showToast(t("common.saved"));
          window.setTimeout(() => {
            window.location.assign(`${nextMount}/${window.location.hash || "#waf"}`);
          }, 350);
          return true;
        }
      }
      showToast(t("common.saved"));
      return true;
    } catch (err: any) {
      showToast(err.message);
      return false;
    } finally {
      setSavingDomain("");
    }
  };

  const reloadGeoDatabases = async (savePathFirst = false) => {
    if (reloadingGeo || savingDomain) return;
    setReloadingGeo(true);
    try {
      const body: Record<string, any> = {};
      if (savePathFirst) {
        body.geoip_dir = String(domains?.security?.geoip_dir || "").trim();
      }
      const res = await apiFetch<{
        ok: boolean;
        geoip_dir?: string;
        default_geoip_dir?: string;
        effective_geoip_dir?: string;
        geoip_status?: { path?: string; active_files?: string[]; skipped_files?: string[] };
        version?: number;
      }>("/api/admin/security/geo-rules/reload", {
        method: "POST",
        body: JSON.stringify(body),
      });
      if (res.default_geoip_dir) setDefaultGeoIPDir(res.default_geoip_dir);
      if (res.effective_geoip_dir) setEffectiveGeoIPDir(res.effective_geoip_dir);
      if (res.geoip_status) setGeoDBStatus(res.geoip_status);
      if (savePathFirst && typeof res.geoip_dir === "string") {
        setDomains((prev) => ({
          ...(prev || {}),
          security: { ...(prev?.security || {}), geoip_dir: res.geoip_dir },
        }));
        if (typeof res.version === "number" && res.version > 0) {
          setVersions((prev) => ({ ...prev, security: res.version! }));
        }
      }
      const count = Array.isArray(res.geoip_status?.active_files) ? res.geoip_status!.active_files!.length : 0;
      showToast(t("sys.geo_reloaded_toast", { count }));
    } catch (err: any) {
      showToast(err.message);
    } finally {
      setReloadingGeo(false);
    }
  };

  const clearDomainSecret = async (domainName: string, secretKey: string) => {
    if (savingDomain) return;
    if (!(await openConfirm({ message: t("sys.clear_secret_confirm"), danger: true }))) return;
    await patchDomain(domainName, { clear_secrets: [secretKey] });
  };

  const uploadImageAsset = async (file: File, onUploaded: (url: string) => void) => {
    try {
      const fd = new FormData();
      fd.append("file", file);
      const res = await apiFetch<{
        url?: string;
        asset?: { public_url?: string };
        item?: { public_url?: string };
      }>("/api/admin/media/upload", { method: "POST", body: fd });
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
    if (!pkCurrPwd.trim()) {
      showToast(t("sys.reauth_pwd_required"), "err");
      return;
    }
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
          current_password: pkCurrPwd,
        }),
      });
      setPkModalOpen(false);
      setPkCurrPwd("");
      showToast(t("sys.pk_registered"));
      void loadSub();
    } catch (err: any) {
      showToast(err?.message || t("auth.passkey_cancelled"));
    }
  };

  const toggleGeoRuleEnabled = async (g: any, nextEnabled: boolean) => {
    try {
      await apiFetch(`/api/admin/security/geo-rules/${encodeURIComponent(g.id)}`, {
        method: "PUT",
        body: JSON.stringify({
          name: g.name || "Geo Rule",
          action: g.action || "deny",
          countries: g.countries || "",
          regions: g.regions || "",
          sort_order: typeof g.sort_order === "number" ? g.sort_order : 10,
          is_enabled: nextEnabled,
          on_miss: g.on_miss || "next",
        }),
      });
      void loadSub();
    } catch (err: any) {
      showToast(err.message);
    }
  };

  const moveGeoRule = async (ruleId: string, delta: -1 | 1) => {
    if (movingGeo) return;
    const idx = geoRules.findIndex((r) => r.id === ruleId);
    const targetIdx = idx + delta;
    if (idx < 0 || targetIdx < 0 || targetIdx >= geoRules.length) return;

    const prevRules = geoRules;
    const orders = geoRules.map((r, i) => (typeof r.sort_order === "number" ? r.sort_order : (i + 1) * 10));
    const hasTies = new Set(orders).size < geoRules.length;
    const reordered = [...geoRules];
    const [moved] = reordered.splice(idx, 1);
    reordered.splice(targetIdx, 0, moved);

    const updates: Array<{ id: string; body: any }> = [];
    let nextRules: any[];
    if (hasTies) {
      nextRules = reordered.map((r, i) => {
        const desired = (i + 1) * 10;
        if (r.sort_order !== desired) {
          updates.push({
            id: r.id,
            body: {
              name: r.name || "Geo Rule",
              action: r.action || "deny",
              countries: r.countries || "",
              regions: r.regions || "",
              sort_order: desired,
              is_enabled: r.is_enabled !== false,
              on_miss: r.on_miss || "next",
            },
          });
        }
        return { ...r, sort_order: desired };
      });
    } else {
      const cur = geoRules[idx];
      const adj = geoRules[targetIdx];
      const curSort = orders[idx];
      const adjSort = orders[targetIdx];
      nextRules = [...reordered];
      nextRules[idx] = { ...adj, sort_order: curSort };
      nextRules[targetIdx] = { ...cur, sort_order: adjSort };
      updates.push(
        {
          id: cur.id,
          body: {
            name: cur.name || "Geo Rule",
            action: cur.action || "deny",
            countries: cur.countries || "",
            regions: cur.regions || "",
            sort_order: adjSort,
            is_enabled: cur.is_enabled !== false,
            on_miss: cur.on_miss || "next",
          },
        },
        {
          id: adj.id,
          body: {
            name: adj.name || "Geo Rule",
            action: adj.action || "deny",
            countries: adj.countries || "",
            regions: adj.regions || "",
            sort_order: curSort,
            is_enabled: adj.is_enabled !== false,
            on_miss: adj.on_miss || "next",
          },
        }
      );
    }

    setMovingGeo(true);
    geoFlip.captureFlip();
    setGeoRules(nextRules);

    try {
      await Promise.all(
        updates.map((u) =>
          apiFetch(`/api/admin/security/geo-rules/${encodeURIComponent(u.id)}`, {
            method: "PUT",
            body: JSON.stringify(u.body),
          })
        )
      );
    } catch (err: any) {
      setGeoRules(prevRules);
      showToast(err.message);
      void loadSub();
    } finally {
      setMovingGeo(false);
    }
  };

  const isSecurityGroup = sub === "account" || sub === "waf" || sub === "logs" || sub === "audit";
  const groupTabs: SysTab[] = isSecurityGroup
    ? isAdmin
      ? ["account", "waf", "logs", "audit"]
      : ["account"]
    : ["settings_site", "settings_io", "users", "backup"];

  const setDomField = (dom: string, field: string, val: any) => {
    setDomains((prev) => ({
      ...(prev || {}),
      [dom]: { ...(prev?.[dom] || {}), [field]: val },
    }));
  };

  const storageDriverOpts = (() => {
    const opts: Array<{ value: string; label: string; badge?: string }> = [
      { value: "local", label: t("sys.driver_local") },
      { value: "s3", label: t("sys.driver_s3") },
    ];
    for (const d of storageDrivers) {
      const id = typeof d === "string" ? d : d?.id || d?.value;
      const label = typeof d === "string" ? d : d?.label || d?.name || id;
      if (id && !opts.some((o) => o.value === id)) {
        opts.push({ value: id, label, badge: t("sys.plugin_driver_badge") });
      }
    }
    const cur = domains?.storage?.active_driver || "";
    if (cur && !opts.some((o) => o.value === cur)) {
      opts.push({ value: cur, label: cur, badge: t("sys.plugin_driver_badge") });
    }
    return opts;
  })();

  const origEmail = (meInfo?.user?.email || currentUserSig.value?.email || "").trim().toLowerCase();
  const emailChanged = profileForm.email.trim().toLowerCase() !== origEmail;
  const needsProfileReauth = Boolean(profileForm.new_password.trim()) || emailChanged;

  const auditActionOptions = (() => {
    const set = new Set<string>(knownAuditActions);
    for (const l of auditLogs) {
      if (l.action) set.add(String(l.action));
    }
    if (auditAction !== "all") set.add(auditAction);
    return [
      { value: "all", label: t("art.all") },
      ...Array.from(set)
        .sort()
        .map((a) => ({ value: a, label: localizeAuditAction(a) })),
    ];
  })();

  const filteredGeoRules = geoQuery.trim()
    ? geoRules.filter((g) => {
        const q = geoQuery.trim().toLowerCase();
        return (
          String(g.name || "").toLowerCase().includes(q) ||
          String(g.countries || "").toLowerCase().includes(q) ||
          String(g.regions || "").toLowerCase().includes(q) ||
          String(g.action || "").toLowerCase().includes(q)
        );
      })
    : geoRules;

  const usersPageSlice =
    users.length > usersPageSize ? paginateItems(users, usersPage, usersPageSize).slice : users;
  const bansPageSlice =
    bans.length > bansPageSize ? paginateItems(bans, bansPage, bansPageSize).slice : bans;
  const auditPageSlice =
    auditLogs.length > auditPageSize ? paginateItems(auditLogs, auditPage, auditPageSize).slice : auditLogs;

  const geoPaged = paginateItems(filteredGeoRules, geoPage, geoPageSize);
  const localBakPaged = paginateItems(localBackups, localBakPage, 10);
  const pkPaged = paginateItems(passkeys, pkPage, 10);
  const patPaged = paginateItems(tokens, patPage, 10);
  const sessPaged = paginateItems(sessions, sessPage, 10);
  const otherSessionsCount = sessions.filter((s) => s.id !== meInfo?.session_id).length;

  const geoMissOptions = [
    { value: "next", label: t("sys.geo_miss_next") },
    { value: "allow", label: t("sys.geo_allow") },
    { value: "block", label: t("sys.geo_miss_block") },
    { value: "challenge", label: t("sys.geo_miss_challenge") },
    { value: "deny", label: t("sys.geo_deny") },
  ];
  if (geoForm.on_miss && !geoMissOptions.some((o) => o.value === geoForm.on_miss)) {
    geoMissOptions.push({ value: geoForm.on_miss, label: geoForm.on_miss });
  }

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
              <div class="volo-input-row" style={{ flexWrap: "wrap" }}>
                <NavIconPreview icon={domains.site?.logo_url} fallbackLabel={domains.site?.title || "V"} />
                <input
                  class="volo-input"
                  style={{ flex: "1 1 180px", minWidth: 0 }}
                  name="site_brand_logo"
                  placeholder={t("sys.site_logo_ph")}
                  value={domains.site?.logo_url || ""}
                  onInput={(e) => setDomField("site", "logo_url", (e.currentTarget as HTMLInputElement).value)}
                />
                <UploadButton
                  accept="image/*"
                  label={t("sys.upload_img_btn")}
                  onFile={(f) => void uploadImageAsset(f, (url) => setDomField("site", "logo_url", url))}
                />
                <MediaPickButton filterKind="image" onPick={(url) => setDomField("site", "logo_url", url)} />
              </div>
            </div>
            <div class="volo-field">
              <label>{t("sys.lbl_fav_url")}</label>
              <div class="volo-input-row" style={{ flexWrap: "wrap" }}>
                <NavIconPreview icon={domains.site?.favicon_url} fallbackLabel="F" />
                <input
                  class="volo-input"
                  style={{ flex: "1 1 180px", minWidth: 0 }}
                  name="site_favicon_path"
                  placeholder={t("sys.site_fav_ph")}
                  value={domains.site?.favicon_url || ""}
                  onInput={(e) => setDomField("site", "favicon_url", (e.currentTarget as HTMLInputElement).value)}
                />
                <UploadButton
                  accept="image/*,.ico,.svg"
                  label={t("sys.upload_img_btn")}
                  onFile={(f) => void uploadImageAsset(f, (url) => setDomField("site", "favicon_url", url))}
                />
                <MediaPickButton filterKind="image" onPick={(url) => setDomField("site", "favicon_url", url)} />
              </div>
            </div>
            <div class="volo-grid-2">
              <div class="volo-field">
                <label>{t("sys.default_locale")}</label>
                <ConsoleSelect
                  value={domains.site?.default_locale || "zh-CN"}
                  options={locales.map((loc) => ({ value: loc.code, label: loc.name, badge: loc.code }))}
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
                rows={3}
                value={domains.site?.notice_board || ""}
                onInput={(e) => setDomField("site", "notice_board", (e.currentTarget as HTMLTextAreaElement).value)}
              />
              <span class="volo-help">{t("sys.notice_board_hint")}</span>
            </div>

            <div class="volo-check-grid">
              <SwitchToggle
                checked={domains.site?.allow_locale_switch !== false}
                onChange={(v) => setDomField("site", "allow_locale_switch", v)}
                label={t("sys.allow_locale_switch")}
              />
              <SwitchToggle
                checked={!!domains.site?.show_admin_login_entry}
                onChange={(v) => setDomField("site", "show_admin_login_entry", v)}
                label={t("sys.show_admin_entry")}
              />
              <SwitchToggle
                checked={domains.site?.enable_license_notice !== false}
                onChange={(v) => setDomField("site", "enable_license_notice", v)}
                label={t("sys.enable_license_notice")}
              />
              <SwitchToggle
                checked={domains.site?.enable_footer_html !== false}
                onChange={(v) => setDomField("site", "enable_footer_html", v)}
                label={t("sys.enable_footer_html")}
              />
            </div>

            {domains.site?.enable_license_notice !== false && (
              <div class="volo-field">
                <label>{t("sys.license_tpl")}</label>
                <textarea
                  class="volo-textarea"
                  rows={2}
                  value={domains.site?.license_template || ""}
                  onInput={(e) => setDomField("site", "license_template", (e.currentTarget as HTMLTextAreaElement).value)}
                />
                <small class="volo-help">{t("sys.license_vars_hint")}</small>
              </div>
            )}

            <div class="volo-field">
              <label>{t("sys.custom_head")}</label>
              <textarea
                class="volo-textarea volo-mono"
                rows={4}
                spellcheck={false}
                value={domains.site?.custom_head_meta || ""}
                onInput={(e) => setDomField("site", "custom_head_meta", (e.currentTarget as HTMLTextAreaElement).value)}
              />
              <small class="volo-help">{t("sys.custom_head_hint")}</small>
            </div>

            {domains.site?.enable_footer_html !== false && (
              <div class="volo-field">
                <label>{t("sys.footer_text")}</label>
                <textarea
                  class="volo-textarea volo-mono"
                  rows={4}
                  spellcheck={false}
                  value={domains.site?.footer_text || ""}
                  onInput={(e) => setDomField("site", "footer_text", (e.currentTarget as HTMLTextAreaElement).value)}
                />
                <small class="volo-help">{t("sys.footer_html_hint")}</small>
              </div>
            )}

            <div class="volo-subcard">
              <div class="volo-subcard-title">{t("sys.social_title")}</div>
              <div class="volo-input-row" style={{ marginBottom: "10px", flexWrap: "wrap" }}>
                <span class="volo-help">{t("sys.social_presets")}</span>
                {SOCIAL_PRESETS.map((p) => (
                  <button
                    key={p.label}
                    type="button"
                    class="volo-chip-btn"
                    onClick={() => setSocialDraft((prev) => ({ label: p.label, url: prev.url || p.urlHint }))}
                  >
                    {p.label}
                  </button>
                ))}
              </div>
              <div class="volo-grid-2">
                <div class="volo-field">
                  <label>{t("sys.lbl_social_name")}</label>
                  <input
                    class="volo-input"
                    name="social_platform_label"
                    placeholder={t("sys.social_label_ph")}
                    value={socialDraft.label}
                    onInput={(e) => setSocialDraft({ ...socialDraft, label: (e.currentTarget as HTMLInputElement).value })}
                  />
                </div>
                <div class="volo-field">
                  <label>{t("sys.lbl_social_url")}</label>
                  <div class="volo-input-row" style={{ flexWrap: "wrap" }}>
                    <input
                      class="volo-input"
                      style={{ flex: "1 1 160px", minWidth: 0 }}
                      name="social_profile_link"
                      placeholder={t("sys.social_url_ph")}
                      value={socialDraft.url}
                      onInput={(e) => setSocialDraft({ ...socialDraft, url: (e.currentTarget as HTMLInputElement).value })}
                    />
                    <button
                      type="button"
                      class="volo-btn volo-btn-sm"
                      onClick={() => {
                        if (!socialDraft.label.trim() || !socialDraft.url.trim()) return;
                        const list = Array.isArray(domains.site?.social_links) ? [...domains.site.social_links] : [];
                        if (editingSocialIdx !== null && editingSocialIdx >= 0 && editingSocialIdx < list.length) {
                          list[editingSocialIdx] = {
                            ...list[editingSocialIdx],
                            label: socialDraft.label.trim(),
                            icon: socialDraft.label.trim().toLowerCase(),
                            url: socialDraft.url.trim(),
                          };
                          setEditingSocialIdx(null);
                        } else {
                          list.push({
                            id: `soc-${Date.now()}`,
                            label: socialDraft.label.trim(),
                            icon: socialDraft.label.trim().toLowerCase(),
                            url: socialDraft.url.trim(),
                          });
                        }
                        setDomField("site", "social_links", list);
                        setSocialDraft({ label: "", url: "" });
                        setSocialDirty(true);
                      }}
                    >
                      {editingSocialIdx !== null ? t("common.save") : (<><PlusIcon size={14} /> {t("sys.social_add")}</>)}
                    </button>
                    {editingSocialIdx !== null && (
                      <button
                        type="button"
                        class="volo-btn volo-btn-sm"
                        onClick={() => {
                          setEditingSocialIdx(null);
                          setSocialDraft({ label: "", url: "" });
                        }}
                      >
                        {t("common.cancel")}
                      </button>
                    )}
                  </div>
                </div>
              </div>
              {Array.isArray(domains.site?.social_links) && domains.site.social_links.length > 0 && (() => {
                const seenKeys = new Map<string, number>();
                return (
                  <div ref={socialFlip.containerRef}>
                    {domains.site.social_links.map((s: any, idx: number, arr: any[]) => {
                      const rawBase = String(s.id || `${s.label || ""}::${s.url || ""}`);
                      const dup = (seenKeys.get(rawBase) || 0) + 1;
                      seenKeys.set(rawBase, dup);
                      const flipKey = dup > 1 ? `${rawBase}#${dup}` : rawBase;
                      return (
                        <div
                          key={flipKey}
                          data-flip-key={flipKey}
                          class="volo-input-row"
                          style={{ marginTop: "6px", flexWrap: "wrap" }}
                        >
                          <NavIconPreview icon={s.icon || String(s.label || "").toLowerCase()} fallbackLabel={s.label} size={15} />
                          <strong>{s.label}</strong>
                          <span
                            class="volo-help"
                            style={{ flex: "1 1 140px", minWidth: 0, overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }}
                          >
                            {s.url}
                          </span>
                          <div class="volo-row-actions">
                            <button
                              type="button"
                              class="volo-act-btn"
                              disabled={idx <= 0}
                              onClick={() => {
                                socialFlip.captureFlip();
                                const next = [...arr];
                                const [moved] = next.splice(idx, 1);
                                next.splice(idx - 1, 0, moved);
                                setDomField("site", "social_links", next);
                                setSocialDirty(true);
                              }}
                            >
                              ↑
                            </button>
                            <button
                              type="button"
                              class="volo-act-btn"
                              disabled={idx >= arr.length - 1}
                              onClick={() => {
                                socialFlip.captureFlip();
                                const next = [...arr];
                                const [moved] = next.splice(idx, 1);
                                next.splice(idx + 1, 0, moved);
                                setDomField("site", "social_links", next);
                                setSocialDirty(true);
                              }}
                            >
                              ↓
                            </button>
                            <button
                              type="button"
                              class="volo-act-btn"
                              onClick={() => {
                                setEditingSocialIdx(idx);
                                setSocialDraft({ label: s.label || "", url: s.url || "" });
                              }}
                            >
                              {t("common.edit")}
                            </button>
                            <button
                              type="button"
                              class="volo-act-btn danger"
                              onClick={async () => {
                                if (!(await openConfirm({ message: t("sys.confirm_del_social", { name: s.label }), danger: true }))) return;
                                const next = domains.site.social_links.filter((_: any, i: number) => i !== idx);
                                setDomField("site", "social_links", next);
                                if (editingSocialIdx === idx) {
                                  setEditingSocialIdx(null);
                                  setSocialDraft({ label: "", url: "" });
                                }
                                setSocialDirty(true);
                              }}
                            >
                              {t("common.delete")}
                            </button>
                          </div>
                        </div>
                      );
                    })}
                  </div>
                );
              })()}
              {socialDirty && (
                <div class="volo-help" style={{ marginTop: "8px", color: "var(--volo-warn)" }}>
                  {t("sys.social_unsaved_hint")}
                </div>
              )}
            </div>

            <div class="volo-form-actions">
              <button type="submit" class="volo-btn volo-btn-primary" disabled={savingDomain === "site"}>
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
            <div class="volo-auto-grid">
              <div class="volo-field">
                <label>{t("sys.list_style")}</label>
                <ConsoleSelect
                  searchable={false}
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
              {domains.reading?.enable_outdated_notice !== false && (
                <div class="volo-field">
                  <label>{t("sys.outdated_days")}</label>
                  <input
                    type="number"
                    class="volo-input"
                    value={domains.reading?.outdated_notice_days ?? 365}
                    onInput={(e) => setDomField("reading", "outdated_notice_days", Number((e.currentTarget as HTMLInputElement).value))}
                  />
                </div>
              )}
            </div>

            <div class="volo-auto-grid">
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

            <div class="volo-grid-2">
              <div class="volo-field">
                <label>{t("sys.asset_cdn_prefix")}</label>
                <input
                  class="volo-input"
                  name="reading_asset_cdn_prefix"
                  placeholder="https://static.example.com"
                  value={domains.reading?.asset_cdn_prefix || ""}
                  onInput={(e) => setDomField("reading", "asset_cdn_prefix", (e.currentTarget as HTMLInputElement).value)}
                />
                <small class="volo-help">{t("sys.asset_cdn_hint")}</small>
              </div>

              <div class="volo-field">
                <label>{t("sys.vendor_cdn_prefix")}</label>
                <input
                  class="volo-input"
                  name="reading_vendor_cdn_prefix"
                  placeholder="https://cdn.jsdelivr.net/npm"
                  value={domains.reading?.vendor_cdn_prefix || ""}
                  onInput={(e) => setDomField("reading", "vendor_cdn_prefix", (e.currentTarget as HTMLInputElement).value)}
                />
                <div class="volo-filter-pills" style={{ marginTop: "6px" }}>
                  {[
                    { label: "jsDelivr", val: "https://cdn.jsdelivr.net/npm" },
                    { label: "Fastly", val: "https://fastly.jsdelivr.net/npm" },
                    { label: "UNPKG", val: "https://unpkg.com" },
                    { label: "ZStatic", val: "https://s4.zstatic.net/npm" },
                  ].map((p) => {
                    const cur = (domains.reading?.vendor_cdn_prefix || "https://cdn.jsdelivr.net/npm").replace(/\/+$/, "");
                    return (
                      <button
                        key={p.val}
                        type="button"
                        class={`volo-filter-pill ${cur === p.val ? "active" : ""}`}
                        onClick={() => setDomField("reading", "vendor_cdn_prefix", p.val)}
                      >
                        {p.label}
                      </button>
                    );
                  })}
                </div>
                <small class="volo-help">{t("sys.vendor_cdn_hint")}</small>
              </div>
            </div>

            <div class="volo-check-grid">
              <SwitchToggle
                checked={domains.reading?.enable_reading_stats !== false}
                onChange={(v) => setDomField("reading", "enable_reading_stats", v)}
                label={t("sys.enable_read_stats")}
              />
              <SwitchToggle
                checked={domains.reading?.enable_outdated_notice !== false}
                onChange={(v) => setDomField("reading", "enable_outdated_notice", v)}
                label={t("sys.enable_outdated_notice")}
              />
              <SwitchToggle
                checked={domains.reading?.enable_instant_search !== false}
                onChange={(v) => setDomField("reading", "enable_instant_search", v)}
                label={t("sys.enable_search")}
              />
              <SwitchToggle
                checked={domains.reading?.enable_pjax !== false}
                onChange={(v) => setDomField("reading", "enable_pjax", v)}
                label={t("sys.enable_pjax")}
              />
              <SwitchToggle
                checked={!!domains.reading?.show_code_line_numbers}
                onChange={(v) => setDomField("reading", "show_code_line_numbers", v)}
                label={t("sys.show_line_num")}
              />
              <SwitchToggle
                checked={!!domains.reading?.allow_reader_register}
                onChange={(v) => setDomField("reading", "allow_reader_register", v)}
                label={t("sys.allow_reader_reg")}
              />
            </div>

            <div class="volo-subcard">
              <div class="volo-subcard-title">{t("sys.cmt_policy_title")}</div>
              <div class="volo-auto-grid">
                <div class="volo-field">
                  <label>{t("sys.cmt_access")}</label>
                  <ConsoleSelect
                    searchable={false}
                    value={domains.reading?.comment_access_mode || "all"}
                    options={[
                      { value: "all", label: t("sys.cmt_access_all") },
                      { value: "registered", label: t("sys.cmt_access_reg") },
                      { value: "off", label: t("sys.cmt_access_off") },
                    ]}
                    onChange={(v) => setDomField("reading", "comment_access_mode", v)}
                  />
                </div>
                {domains.reading?.comment_access_mode !== "off" && (
                  <>
                    <div class="volo-field">
                      <label>{t("sys.cmt_mod")}</label>
                      <ConsoleSelect
                        searchable={false}
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
                  </>
                )}
              </div>
              {domains.reading?.comment_access_mode !== "off" && (
                <>
                  <div class="volo-check-grid">
                    <SwitchToggle
                      checked={domains.reading?.builtin_spam_filter !== false}
                      onChange={(v) => setDomField("reading", "builtin_spam_filter", v)}
                      label={t("sys.spam_filter")}
                    />
                  </div>
                  {domains.reading?.builtin_spam_filter !== false && (
                    <>
                      <div class="volo-field">
                        <label>{t("sys.blocked_kw")}</label>
                        <TokenListInput
                          values={Array.isArray(domains.reading?.blocked_keywords) ? domains.reading.blocked_keywords : []}
                          onChange={(next) => setDomField("reading", "blocked_keywords", next)}
                        />
                      </div>
                      <div class="volo-field">
                        <label>{t("sys.allowed_kw")}</label>
                        <TokenListInput
                          values={Array.isArray(domains.reading?.allowed_keywords) ? domains.reading.allowed_keywords : []}
                          onChange={(next) => setDomField("reading", "allowed_keywords", next)}
                        />
                      </div>
                    </>
                  )}
                </>
              )}
            </div>

            <div class="volo-form-actions">
              <button type="submit" class="volo-btn volo-btn-primary" disabled={savingDomain === "reading"}>
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
            onSubmit={async (e) => {
              e.preventDefault();
              const body: any = { ...(domains.storage || {}) };
              if (s3Secret) body.s3_secret_key = s3Secret;
              const ok = await patchDomain("storage", body);
              if (ok) setS3Secret("");
            }}
          >
            <div class="volo-sec-head">
              <div class="volo-sec-copy">
                <h3 class="volo-card-title">{t("sys.dom4_head")}</h3>
                <p class="volo-sec-desc">{t("sys.dom4_desc")}</p>
              </div>
            </div>
            <div class="volo-auto-grid">
              <div class="volo-field">
                <label>{t("sys.storage_driver")}</label>
                <ConsoleSelect
                  value={domains.storage?.active_driver || "local"}
                  options={storageDriverOpts}
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
              <SwitchToggle
                checked={domains.storage?.strip_exif !== false}
                onChange={(v) => setDomField("storage", "strip_exif", v)}
                label={t("sys.strip_exif")}
              />
              <SwitchToggle
                checked={domains.storage?.adaptive_webp !== false}
                onChange={(v) => setDomField("storage", "adaptive_webp", v)}
                label={t("sys.adaptive_webp")}
              />
            </div>

            {(!domains.storage?.active_driver || domains.storage?.active_driver === "local") && (
              <div class="volo-subcard">
                <div class="volo-subcard-title">{t("sys.local_sub_title")}</div>
                <div class="volo-grid-2">
                  <div class="volo-field">
                    <label>{t("sys.local_dir")}</label>
                    <input
                      class="volo-input"
                      name="storage_local_dir"
                      autoComplete="off"
                      placeholder={defaultUploadDir || "./uploads"}
                      value={domains.storage?.local_dir || ""}
                      onInput={(e) => setDomField("storage", "local_dir", (e.currentTarget as HTMLInputElement).value)}
                    />
                    <span class="volo-help">{t("sys.local_dir_hint", { dir: defaultUploadDir || "./uploads" })}</span>
                  </div>
                  <div class="volo-field">
                    <label>{t("sys.local_key_prefix")}</label>
                    <input
                      class="volo-input"
                      name="storage_local_key_prefix"
                      autoComplete="off"
                      placeholder="media / blog"
                      value={domains.storage?.local_key_prefix || ""}
                      onInput={(e) => setDomField("storage", "local_key_prefix", (e.currentTarget as HTMLInputElement).value)}
                    />
                    <span class="volo-help">{t("sys.local_key_prefix_hint")}</span>
                  </div>
                </div>
              </div>
            )}

            {domains.storage?.active_driver === "s3" && (
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
                <div class="volo-auto-grid">
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
                    <label style={{ display: "flex", alignItems: "center", justifyContent: "space-between", gap: "8px", flexWrap: "wrap" }}>
                      <span>
                        {t("sys.lbl_s3_sk")}{" "}
                        {Boolean(domains.storage?.s3_secret_key_set || domains.storage?.has_s3_secret_key) && (
                          <span class="volo-badge volo-badge-ok">{t("sys.sealed")}</span>
                        )}
                      </span>
                      {Boolean(domains.storage?.s3_secret_key_set || domains.storage?.has_s3_secret_key) && (
                        <button
                          type="button"
                          class="volo-act-btn danger"
                          disabled={!!savingDomain}
                          onClick={() => void clearDomainSecret("storage", "s3_secret_key")}
                        >
                          {t("sys.clear_saved_secret")}
                        </button>
                      )}
                    </label>
                    <input
                      type="password"
                      class="volo-input"
                      name="s3_sealed_secret"
                      autoComplete="new-password"
                      placeholder={Boolean(domains.storage?.s3_secret_key_set || domains.storage?.has_s3_secret_key) ? t("sys.s3_sk_sealed_ph") : t("sys.s3_sk_ph")}
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
                <SwitchToggle
                  checked={!!domains.storage?.s3_path_style}
                  onChange={(v) => setDomField("storage", "s3_path_style", v)}
                  label={t("sys.s3_path_style")}
                />
              </div>
            )}

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
            onSubmit={async (e) => {
              e.preventDefault();
              const body: any = { ...(domains.notifications || {}) };
              if (smtpPwd) body.smtp_password = smtpPwd;
              const ok = await patchDomain("notifications", body);
              if (ok) setSmtpPwd("");
            }}
          >
            <div class="volo-sec-head">
              <div class="volo-sec-copy">
                <h3 class="volo-card-title">{t("sys.dom3_head")}</h3>
                <p class="volo-sec-desc">{t("sys.dom3_desc")}</p>
              </div>
            </div>

            <div class="volo-check-grid">
              <SwitchToggle
                checked={!!domains.notifications?.smtp_enabled}
                onChange={(v) => setDomField("notifications", "smtp_enabled", v)}
                label={t("sys.enable_smtp")}
              />
            </div>

            {domains.notifications?.smtp_enabled && (
              <>
                <div class="volo-auto-grid">
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

                <div class="volo-check-grid">
                  <SwitchToggle
                    checked={domains.notifications?.smtp_auth !== false}
                    onChange={(v) => setDomField("notifications", "smtp_auth", v)}
                    label={t("sys.smtp_auth")}
                  />
                  <SwitchToggle
                    checked={domains.notifications?.notify_admin_on_comment !== false}
                    onChange={(v) => setDomField("notifications", "notify_admin_on_comment", v)}
                    label={t("sys.notify_admin_cmt")}
                  />
                  <SwitchToggle
                    checked={domains.notifications?.notify_reply_by_email !== false}
                    onChange={(v) => setDomField("notifications", "notify_reply_by_email", v)}
                    label={t("sys.notify_reply_mail")}
                  />
                </div>

                {domains.notifications?.smtp_auth !== false && (
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
                      <label style={{ display: "flex", alignItems: "center", justifyContent: "space-between", gap: "8px", flexWrap: "wrap" }}>
                        <span>
                          {t("sys.smtp_pwd")}{" "}
                          {Boolean(domains.notifications?.smtp_password_set || domains.notifications?.has_smtp_password) && (
                            <span class="volo-badge volo-badge-ok">{t("sys.sealed")}</span>
                          )}
                        </span>
                        {Boolean(domains.notifications?.smtp_password_set || domains.notifications?.has_smtp_password) && (
                          <button
                            type="button"
                            class="volo-act-btn danger"
                            disabled={!!savingDomain}
                            onClick={() => void clearDomainSecret("notifications", "smtp_password")}
                          >
                            {t("sys.clear_saved_secret")}
                          </button>
                        )}
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
                )}

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

                <div class="volo-subcard">
                  <div class="volo-field">
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
              </>
            )}

            <div class="volo-form-actions">
              {domains.notifications?.smtp_enabled && (
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
              )}
              <button type="submit" class="volo-btn volo-btn-primary" disabled={!!savingDomain}>
                {t("sys.save_notify")}
              </button>
            </div>
          </form>
        </div>
      )}

      {sub === "account" && (
        <div>
          <div class="volo-seg" style={{ marginBottom: "16px" }}>
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
                    if (needsProfileReauth && !profileForm.current_password.trim()) {
                      showToast(t("sys.reauth_pwd_required"), "err");
                      return;
                    }
                    try {
                      await apiFetch("/api/admin/me", {
                        method: "PATCH",
                        body: JSON.stringify({
                          avatar_url: profileForm.avatar_url,
                          display_name: profileForm.display_name,
                          email: profileForm.email.trim(),
                          bio: profileForm.bio,
                          website_url: profileForm.website_url,
                          current_password: profileForm.current_password,
                          new_password: profileForm.new_password,
                        }),
                      });
                      setProfileForm((prev) => ({ ...prev, current_password: "", new_password: "" }));
                      await refreshMe();
                      void loadSub();
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
                            {(profileForm.display_name || currentUserSig.value?.username || "U").trim().charAt(0).toUpperCase()}
                          </span>
                        )}
                      </div>
                      <div class="volo-input-row" style={{ flex: 1, flexWrap: "wrap" }}>
                        <input
                          class="volo-input"
                          style={{ flex: "1 1 160px", minWidth: 0 }}
                          name="author_avatar_url"
                          autoComplete="off"
                          placeholder={t("sys.avatar_url_ph")}
                          value={profileForm.avatar_url}
                          onInput={(e) => setProfileForm((prev) => ({ ...prev, avatar_url: (e.currentTarget as HTMLInputElement).value }))}
                        />
                        <UploadButton
                          accept="image/*"
                          label={t("sys.upload_img_btn")}
                          onFile={(f) => void uploadImageAsset(f, (url) => setProfileForm((prev) => ({ ...prev, avatar_url: url })))}
                        />
                        <MediaPickButton filterKind="image" onPick={(url) => setProfileForm((prev) => ({ ...prev, avatar_url: url }))} />
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
                      <label>{t("sys.lbl_user_email")}</label>
                      <input
                        class="volo-input"
                        type="email"
                        name="author_account_email"
                        autoComplete="off"
                        value={profileForm.email}
                        onInput={(e) => setProfileForm({ ...profileForm, email: (e.currentTarget as HTMLInputElement).value })}
                      />
                    </div>
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
                      <label>
                        {t("sys.cur_pwd")}
                        {needsProfileReauth ? " *" : ""}
                      </label>
                      <input
                        type="password"
                        class="volo-input"
                        required={needsProfileReauth}
                        name="staff_old_passphrase"
                        autoComplete="new-password"
                        placeholder={needsProfileReauth ? t("sys.reauth_pwd_ph") : ""}
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

              <div style={{ display: "flex", flexDirection: "column", gap: "16px" }}>
                <div class="volo-card">
                  <div class="volo-sec-head">
                    <div class="volo-sec-copy">
                      <h3 class="volo-card-title">{t("sys.totp_head")}</h3>
                      <p class="volo-sec-desc">{t("sys.totp_desc")}</p>
                    </div>
                  </div>
                  <div class="volo-subcard">
                    {meInfo?.totp_enabled ? (
                      <div class="volo-input-row" style={{ justifyContent: "space-between", flexWrap: "wrap" }}>
                        <span class="volo-status-pill ok">
                          <span class="volo-status-dot" />
                          {t("sys.totp_enabled")}
                        </span>
                        <button
                          type="button"
                          class="volo-act-btn danger"
                          onClick={() => {
                            setTotpDisablePwd("");
                            setTotpDisableOpen(true);
                          }}
                        >
                          {t("sys.totp_disable")}
                        </button>
                      </div>
                    ) : totpEnroll ? (
                      <div>
                        {totpEnroll.qr_svg && (
                          <div
                            style={{
                              display: "inline-flex",
                              padding: "10px",
                              background: "#fff",
                              borderRadius: "10px",
                              border: "1px solid var(--volo-line)",
                              marginBottom: "10px",
                            }}
                          >
                            <img
                              src={`data:image/svg+xml;utf8,${encodeURIComponent(totpEnroll.qr_svg)}`}
                              alt="TOTP QR"
                              style={{ width: "168px", height: "168px", display: "block" }}
                            />
                          </div>
                        )}
                        <div class="volo-input-row" style={{ flexWrap: "wrap", marginBottom: "8px" }}>
                          <span class="volo-help" style={{ wordBreak: "break-all" }}>
                            {t("sys.totp_secret")} <code>{totpEnroll.secret}</code>
                          </span>
                          <button
                            type="button"
                            class="volo-act-btn"
                            onClick={() => {
                              void navigator.clipboard.writeText(totpEnroll.secret);
                              showToast(t("cnt.media_copied"));
                            }}
                          >
                            {t("sys.totp_copy_secret")}
                          </button>
                        </div>
                        {(totpEnroll.otpauth_url || totpEnroll.otpauth_uri) && (
                          <div class="volo-help" style={{ wordBreak: "break-all", marginBottom: "10px" }}>
                            {t("sys.totp_uri")} <code>{totpEnroll.otpauth_url || totpEnroll.otpauth_uri}</code>
                          </div>
                        )}
                        <div class="volo-grid-2">
                          <div class="volo-field">
                            <label>{t("sys.totp_code_ph")} *</label>
                            <input
                              class="volo-input"
                              name="totp_verify_digits"
                              autoComplete="one-time-code"
                              placeholder={t("sys.totp_code_ph")}
                              value={totpCode}
                              onInput={(e) => setTotpCode((e.currentTarget as HTMLInputElement).value)}
                            />
                          </div>
                          <div class="volo-field">
                            <label>{t("sys.cur_pwd")} *</label>
                            <input
                              type="password"
                              class="volo-input"
                              required
                              name="totp_stepup_password"
                              autoComplete="current-password"
                              placeholder={t("sys.reauth_pwd_ph")}
                              value={totpCurrPwd}
                              onInput={(e) => setTotpCurrPwd((e.currentTarget as HTMLInputElement).value)}
                            />
                          </div>
                        </div>
                        <div class="volo-form-actions">
                          <button
                            type="button"
                            class="volo-btn"
                            onClick={() => {
                              setTotpEnroll(null);
                              setTotpCode("");
                              setTotpCurrPwd("");
                            }}
                          >
                            {t("common.cancel")}
                          </button>
                          <button
                            type="button"
                            class="volo-btn volo-btn-primary"
                            onClick={async () => {
                              if (!totpCurrPwd.trim()) {
                                showToast(t("sys.reauth_pwd_required"), "err");
                                return;
                              }
                              try {
                                const res = await apiFetch<{ recovery_codes: string[] }>("/api/admin/security/totp/enroll", {
                                  method: "POST",
                                  body: JSON.stringify({
                                    secret: totpEnroll.secret,
                                    code: totpCode,
                                    current_password: totpCurrPwd,
                                  }),
                                });
                                setRecoveryCodes(res.recovery_codes || []);
                                setTotpEnroll(null);
                                setTotpCode("");
                                setTotpCurrPwd("");
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
                      <div class="volo-form-actions no-border">
                        <button
                          type="button"
                          class="volo-btn volo-btn-sm volo-btn-primary"
                          onClick={async () => {
                            try {
                              const res = await apiFetch<{
                                secret: string;
                                otpauth_url?: string;
                                otpauth_uri?: string;
                                qr_svg?: string;
                              }>("/api/admin/security/totp/setup", {
                                method: "POST",
                                body: "{}",
                              });
                              setTotpCode("");
                              setTotpCurrPwd("");
                              setTotpEnroll(res);
                            } catch (err: any) {
                              showToast(err.message);
                            }
                          }}
                        >
                          <PlusIcon size={14} /> {t("sys.totp_enroll")}
                        </button>
                      </div>
                    )}
                    {recoveryCodes && (
                      <pre class="volo-subcard" style={{ fontSize: "12px", whiteSpace: "pre-wrap" }}>
                        {t("sys.recovery_codes_tip")}
                        {"\n"}
                        {recoveryCodes.join("   ")}
                      </pre>
                    )}
                  </div>

                  <AnimatedModal
                    open={totpDisableOpen}
                    onClose={() => setTotpDisableOpen(false)}
                    title={t("sys.totp_disable")}
                    subtitle={t("sys.confirm_disable_totp")}
                    maxWidth="440px"
                    footer={
                      <>
                        <button type="button" class="volo-btn" onClick={() => setTotpDisableOpen(false)}>
                          {t("common.cancel")}
                        </button>
                        <button type="submit" form="volo-totp-disable-form" class="volo-btn volo-btn-danger">
                          {t("sys.totp_disable")}
                        </button>
                      </>
                    }
                  >
                    <form
                      id="volo-totp-disable-form"
                      autoComplete="off"
                      onSubmit={async (e) => {
                        e.preventDefault();
                        if (!totpDisablePwd.trim()) {
                          showToast(t("sys.reauth_pwd_required"), "err");
                          return;
                        }
                        try {
                          await apiFetch("/api/admin/security/totp", {
                            method: "DELETE",
                            body: JSON.stringify({ current_password: totpDisablePwd }),
                          });
                          setTotpDisableOpen(false);
                          setTotpDisablePwd("");
                          showToast(t("common.saved"));
                          void loadSub();
                        } catch (err: any) {
                          showToast(err.message);
                        }
                      }}
                    >
                      <div class="volo-field">
                        <label>{t("sys.cur_pwd")} *</label>
                        <input
                          type="password"
                          class="volo-input"
                          required
                          autoComplete="current-password"
                          placeholder={t("sys.reauth_pwd_ph")}
                          value={totpDisablePwd}
                          onInput={(e) => setTotpDisablePwd((e.currentTarget as HTMLInputElement).value)}
                        />
                      </div>
                    </form>
                  </AnimatedModal>
                </div>

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
                        setPkCurrPwd("");
                        setPkModalOpen(true);
                      }}
                    >
                      <PlusIcon size={14} /> {t("sys.pk_add")}
                    </button>
                  </div>

                  <div class="volo-table-wrap">
                    <table class="volo-table volo-table-compact">
                      <thead>
                        <tr>
                          <th class="col-fluid">{t("sys.pk_col_device")}</th>
                          <th class="col-date">{t("sys.col_created")}</th>
                          <th class="col-act col-act-th">{t("art.actions")}</th>
                        </tr>
                      </thead>
                      <tbody>
                        {loading ? (
                          <TableSkeleton cols={3} rows={3} />
                        ) : pkPaged.slice.length === 0 ? (
                          <ConsoleEmpty colSpan={3} />
                        ) : (
                          pkPaged.slice.map((pk) => (
                            <tr key={pk.id}>
                              <td class="col-fluid" data-label={t("sys.pk_col_device")}>
                                <div class="volo-cell-title">{pk.name}</div>
                              </td>
                              <td class="col-date" data-label={t("sys.col_created")}>
                                {formatDateTime(pk.created_at)}
                              </td>
                              <td class="col-act" data-label={t("art.actions")}>
                                <div class="volo-row-actions">
                                  <button
                                    type="button"
                                    class="volo-act-btn danger"
                                    onClick={() => {
                                      setDelPkPwd("");
                                      setDelPkTarget({ id: pk.id, name: pk.name || pk.id });
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
                    total={passkeys.length}
                    page={pkPaged.safePage}
                    pageSize={10}
                    onChange={setPkPage}
                  />
                </div>

                <div class="volo-card">
                  <div class="volo-sec-head">
                    <div class="volo-sec-copy">
                      <h3 class="volo-card-title">{t("sys.oidc_links_title")}</h3>
                      <p class="volo-sec-desc">{t("sys.oidc_links_desc")}</p>
                    </div>
                    {oidcProviders.length > 0 && (
                      <div class="volo-input-row" style={{ flexWrap: "wrap", gap: "6px" }}>
                        {oidcProviders.map((p) => (
                          <a
                            key={p.name}
                            class="volo-btn volo-btn-sm"
                            href={`/api/v1/auth/oidc/${encodeURIComponent(p.name)}/start?target=bind`}
                          >
                            <PlusIcon size={14} /> {t("sys.oidc_link_btn", { provider: p.display_name || p.name })}
                          </a>
                        ))}
                      </div>
                    )}
                  </div>

                  {oidcLinks.length === 0 ? (
                    <div class="volo-help" style={{ padding: "8px 0" }}>
                      {t("sys.oidc_empty")}
                    </div>
                  ) : (
                    <div class="volo-table-wrap">
                      <table class="volo-table volo-table-compact">
                        <thead>
                          <tr>
                            <th class="col-fluid">{t("sys.oidc_col_account")}</th>
                            <th class="col-date">{t("sys.col_created")}</th>
                            <th class="col-act col-act-th">{t("art.actions")}</th>
                          </tr>
                        </thead>
                        <tbody>
                          {oidcLinks.map((lk) => (
                            <tr key={lk.id}>
                              <td class="col-fluid" data-label={t("sys.oidc_col_account")} style={{ minWidth: 0 }}>
                                <div class="volo-cell-main" style={{ minWidth: 0 }}>
                                  <div class="volo-cell-title" style={{ minWidth: 0 }}>
                                    <span class="volo-wrap-anywhere">{lk.name}</span>
                                  </div>
                                  {lk.credential_id && (
                                    <div class="volo-cell-meta" style={{ minWidth: 0 }}>
                                      <span class="volo-code-chip is-wrap">{lk.credential_id}</span>
                                    </div>
                                  )}
                                </div>
                              </td>
                              <td class="col-date" data-label={t("sys.col_created")}>
                                {formatDateTime(lk.created_at)}
                              </td>
                              <td class="col-act" data-label={t("art.actions")}>
                                <div class="volo-row-actions">
                                  <button
                                    type="button"
                                    class="volo-act-btn danger"
                                    onClick={() => {
                                      setUnlinkOidcPwd("");
                                      setUnlinkOidcTarget({ id: lk.id, name: lk.name || lk.credential_id || lk.id });
                                    }}
                                  >
                                    {t("sys.oidc_unlink_btn")}
                                  </button>
                                </div>
                              </td>
                            </tr>
                          ))}
                        </tbody>
                      </table>
                    </div>
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
                        setPatExpiresDays(90);
                        setPatCurrPwd("");
                        setPatModalOpen(true);
                      }}
                    >
                      <PlusIcon size={14} /> {t("sys.pat_generate")}
                    </button>
                  </div>

                  {issuedPAT && !patModalOpen && (
                    <div class="volo-subcard volo-input-row" style={{ justifyContent: "space-between", flexWrap: "wrap", wordBreak: "break-all" }}>
                      <div>
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
                    <table class="volo-table volo-table-compact">
                      <thead>
                        <tr>
                          <th class="col-fluid">{t("sys.pat_col_token")}</th>
                          <th class="col-fluid">{t("sys.pat_col_scopes")}</th>
                          <th class="col-act col-act-th">{t("art.actions")}</th>
                        </tr>
                      </thead>
                      <tbody>
                        {loading ? (
                          <TableSkeleton cols={3} rows={3} />
                        ) : patPaged.slice.length === 0 ? (
                          <ConsoleEmpty colSpan={3} />
                        ) : (
                          patPaged.slice.map((tk) => {
                            const scList = parseScopes(tk.scopes_json);
                            return (
                              <tr key={tk.id}>
                                <td class="col-fluid" data-label={t("sys.pat_col_token")}>
                                  <div class="volo-cell-main">
                                    <div class="volo-cell-title">
                                      <span>{tk.name}</span>
                                      <span class="volo-code-chip">{tk.token_prefix}…</span>
                                    </div>
                                    <div class="volo-cell-meta" style={{ flexWrap: "wrap" }}>
                                      <span>
                                        {tk.last_used_at > 0
                                          ? t("sys.pat_last_used", { time: formatDateTime(tk.last_used_at) })
                                          : t("sys.pat_never_used")}
                                      </span>
                                      <span>
                                        {tk.expires_at > 0
                                          ? t("sys.pat_expires", { time: formatDateTime(tk.expires_at) })
                                          : t("sys.waf_dur_perm")}
                                      </span>
                                    </div>
                                  </div>
                                </td>
                                <td class="col-fluid" data-label={t("sys.pat_col_scopes")}>
                                  <div class="volo-cell-meta" style={{ flexWrap: "wrap", gap: "4px" }}>
                                    {scList.length > 0 ? scList.map((sc) => (
                                      <span key={sc} class="volo-mini-tag">{sc}</span>
                                    )) : <span class="volo-help">—</span>}
                                  </div>
                                </td>
                                <td class="col-act" data-label={t("art.actions")}>
                                  <div class="volo-row-actions">
                                    <button
                                      type="button"
                                      class="volo-act-btn danger"
                                      onClick={async () => {
                                        if (!(await openConfirm({ message: t("sys.confirm_revoke_pat", { name: tk.name }), danger: true }))) return;
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
                            );
                          })
                        )}
                      </tbody>
                    </table>
                  </div>
                  <Pagination
                    total={tokens.length}
                    page={patPaged.safePage}
                    pageSize={10}
                    onChange={setPatPage}
                  />
                </div>

                <div class="volo-card">
                  <div class="volo-sec-head">
                    <div class="volo-sec-copy">
                      <h3 class="volo-card-title">{t("sys.sess_head")}</h3>
                      <p class="volo-sec-desc">{t("sys.sess_desc")}</p>
                    </div>
                    {otherSessionsCount > 0 && (
                      <button
                        type="button"
                        class="volo-btn volo-btn-sm volo-btn-danger"
                        disabled={revokingOthers}
                        onClick={async () => {
                          if (
                            !(await openConfirm({
                              message: t("sys.confirm_kick_other_sess", { n: otherSessionsCount }),
                              danger: true,
                            }))
                          ) {
                            return;
                          }
                          setRevokingOthers(true);
                          try {
                            await apiFetch("/api/admin/security/sessions", { method: "DELETE" });
                            showToast(t("sys.sess_kicked_others"));
                            void loadSub();
                          } catch (err: any) {
                            showToast(err.message);
                          } finally {
                            setRevokingOthers(false);
                          }
                        }}
                      >
                        {t("sys.sess_kick_others")} ({otherSessionsCount})
                      </button>
                    )}
                  </div>
                  <div class="volo-table-wrap">
                    <table class="volo-table volo-table-compact">
                      <thead>
                        <tr>
                          <th class="col-fluid">{t("sys.sess_col_device")}</th>
                          <th class="col-act col-act-th">{t("art.actions")}</th>
                        </tr>
                      </thead>
                      <tbody>
                        {loading ? (
                          <TableSkeleton cols={2} rows={4} />
                        ) : sessPaged.slice.length === 0 ? (
                          <ConsoleEmpty colSpan={2} />
                        ) : (
                          sessPaged.slice.map((s) => {
                            const isCurrentSess = s.id === meInfo?.session_id;
                            return (
                              <tr key={s.id}>
                                <td class="col-fluid" data-label={t("sys.sess_col_device")}>
                                  <div class="volo-cell-main">
                                    <div class="volo-cell-title">
                                      <span class="volo-code-chip">{s.client_ip || s.ip_masked}</span>
                                      {s.geo_region && <span class="volo-mini-tag">{s.geo_region}</span>}
                                      <span class="volo-mini-tag" title={s.user_agent || ""}>
                                        {summarizeUA(s.user_agent)}
                                      </span>
                                      {isCurrentSess && (
                                        <span class="volo-status-pill ok">
                                          <span class="volo-status-dot" />
                                          {t("sys.sess_current")}
                                        </span>
                                      )}
                                    </div>
                                    <div class="volo-cell-meta">
                                      <span title={s.user_agent || ""}>{formatDateTime(s.last_seen_at)}</span>
                                    </div>
                                  </div>
                                </td>
                                <td class={`col-act ${isCurrentSess ? "is-empty-act" : ""}`.trim()} data-label={t("art.actions")}>
                                  <div class="volo-row-actions">
                                    {!isCurrentSess && (
                                      <button
                                        type="button"
                                        class="volo-act-btn danger"
                                        onClick={async () => {
                                          if (!(await openConfirm({ message: t("sys.confirm_kick_sess"), danger: true }))) return;
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
                            );
                          })
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
              </div>

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
                        type="submit"
                        form="volo-pat-create-form"
                        class="volo-btn volo-btn-primary"
                      >
                        {t("sys.pat_generate")}
                      </button>
                    </>
                  )
                }
              >
                {issuedPAT ? (
                  <div class="volo-subcard" style={{ wordBreak: "break-all" }}>
                    <div class="volo-subcard-title">{t("sys.pat_copy_once")}</div>
                    <code style={{ display: "block", userSelect: "all" }}>{issuedPAT}</code>
                  </div>
                ) : (
                  <form
                    id="volo-pat-create-form"
                    autoComplete="off"
                    onSubmit={async (e) => {
                      e.preventDefault();
                      if (!patCurrPwd.trim()) {
                        showToast(t("sys.stepup_pwd_required"), "err");
                        return;
                      }
                      try {
                        const res = await apiFetch<{ plaintext_token: string }>("/api/admin/security/tokens", {
                          method: "POST",
                          body: JSON.stringify({
                            name: patName,
                            scopes: patScopes,
                            expires_in_days: patExpiresDays,
                            current_password: patCurrPwd,
                          }),
                        });
                        setPatCurrPwd("");
                        setIssuedPAT(res.plaintext_token);
                        void loadSub();
                      } catch (err: any) {
                        showToast(err.message);
                      }
                    }}
                  >
                    <div class="volo-grid-2">
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
                      <div class="volo-field">
                        <label>{t("sys.lbl_pat_exp")}</label>
                        <ConsoleSelect
                          value={String(patExpiresDays)}
                          options={[
                            { value: "30", label: t("sys.pat_exp_30d") },
                            { value: "90", label: t("sys.pat_exp_90d") },
                            { value: "365", label: t("sys.pat_exp_365d") },
                            { value: "0", label: t("sys.pat_exp_never") },
                          ]}
                          onChange={(v) => setPatExpiresDays(Number(v))}
                        />
                      </div>
                    </div>
                    <div class="volo-field">
                      <label>{t("sys.lbl_pat_scopes")}</label>
                      <div class="volo-check-grid">
                        {PAT_SCOPES.map((sc) => (
                          <SwitchToggle
                            key={sc}
                            checked={patScopes.includes(sc)}
                            onChange={(on) => setPatScopes(on ? [...patScopes, sc] : patScopes.filter((x) => x !== sc))}
                            label={sc}
                          />
                        ))}
                      </div>
                    </div>
                    <div class="volo-field">
                      <label>{t("sys.cur_pwd")} *</label>
                      <input
                        type="password"
                        class="volo-input"
                        required
                        name="pat_stepup_password"
                        autoComplete="current-password"
                        placeholder={t("sys.reauth_pwd_ph")}
                        value={patCurrPwd}
                        onInput={(e) => setPatCurrPwd((e.currentTarget as HTMLInputElement).value)}
                      />
                    </div>
                  </form>
                )}
              </AnimatedModal>
            </>
          )}

          <AnimatedModal
            open={Boolean(delPkTarget)}
            onClose={() => {
              setDelPkTarget(null);
              setDelPkPwd("");
            }}
            title={t("common.delete")}
            subtitle={delPkTarget ? t("sys.confirm_del_pk", { name: delPkTarget.name }) : ""}
            maxWidth="440px"
            footer={
              <>
                <button
                  type="button"
                  class="volo-btn"
                  onClick={() => {
                    setDelPkTarget(null);
                    setDelPkPwd("");
                  }}
                >
                  {t("common.cancel")}
                </button>
                <button type="submit" form="volo-del-pk-form" class="volo-btn volo-btn-danger">
                  {t("common.delete")}
                </button>
              </>
            }
          >
            <form
              id="volo-del-pk-form"
              autoComplete="off"
              onSubmit={async (e) => {
                e.preventDefault();
                if (!delPkTarget) return;
                if (!delPkPwd.trim()) {
                  showToast(t("sys.stepup_pwd_required"), "err");
                  return;
                }
                try {
                  await apiFetch(`/api/admin/security/passkeys/${encodeURIComponent(delPkTarget.id)}`, {
                    method: "DELETE",
                    body: JSON.stringify({ current_password: delPkPwd }),
                  });
                  setDelPkTarget(null);
                  setDelPkPwd("");
                  showToast(t("common.deleted"));
                  void loadSub();
                } catch (err: any) {
                  showToast(err.message);
                }
              }}
            >
              <div class="volo-field">
                <label>{t("sys.cur_pwd")} *</label>
                <input
                  type="password"
                  class="volo-input"
                  required
                  name="del_pk_stepup_password"
                  autoComplete="current-password"
                  placeholder={t("sys.reauth_pwd_ph")}
                  value={delPkPwd}
                  onInput={(e) => setDelPkPwd((e.currentTarget as HTMLInputElement).value)}
                />
              </div>
            </form>
          </AnimatedModal>

          <AnimatedModal
            open={Boolean(unlinkOidcTarget)}
            onClose={() => {
              setUnlinkOidcTarget(null);
              setUnlinkOidcPwd("");
            }}
            title={t("sys.oidc_unlink_btn")}
            subtitle={unlinkOidcTarget ? t("sys.oidc_confirm_unlink", { name: unlinkOidcTarget.name }) : ""}
            maxWidth="440px"
            footer={
              <>
                <button
                  type="button"
                  class="volo-btn"
                  onClick={() => {
                    setUnlinkOidcTarget(null);
                    setUnlinkOidcPwd("");
                  }}
                >
                  {t("common.cancel")}
                </button>
                <button type="submit" form="volo-unlink-oidc-form" class="volo-btn volo-btn-danger">
                  {t("sys.oidc_unlink_btn")}
                </button>
              </>
            }
          >
            <form
              id="volo-unlink-oidc-form"
              autoComplete="off"
              onSubmit={async (e) => {
                e.preventDefault();
                if (!unlinkOidcTarget) return;
                if (!unlinkOidcPwd.trim()) {
                  showToast(t("sys.stepup_pwd_required"), "err");
                  return;
                }
                try {
                  await apiFetch(`/api/admin/security/oidc-links/${encodeURIComponent(unlinkOidcTarget.id)}`, {
                    method: "DELETE",
                    body: JSON.stringify({ current_password: unlinkOidcPwd }),
                  });
                  setUnlinkOidcTarget(null);
                  setUnlinkOidcPwd("");
                  showToast(t("common.deleted"));
                  void loadSub();
                } catch (err: any) {
                  showToast(err.message);
                }
              }}
            >
              <div class="volo-field">
                <label>{t("sys.cur_pwd")} *</label>
                <input
                  type="password"
                  class="volo-input"
                  required
                  name="unlink_oidc_stepup_password"
                  autoComplete="current-password"
                  placeholder={t("sys.reauth_pwd_ph")}
                  value={unlinkOidcPwd}
                  onInput={(e) => setUnlinkOidcPwd((e.currentTarget as HTMLInputElement).value)}
                />
              </div>
            </form>
          </AnimatedModal>

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
              <div class="volo-field">
                <label>{t("sys.lbl_pk_name")}</label>
                <input
                  class="volo-input"
                  autoComplete="off"
                  placeholder={t("sys.pk_name_ph")}
                  value={pkName}
                  onInput={(e) => setPkName((e.currentTarget as HTMLInputElement).value)}
                />
              </div>
              <div class="volo-field">
                <label>{t("sys.cur_pwd")} *</label>
                <input
                  type="password"
                  class="volo-input"
                  required
                  name="pk_stepup_password"
                  autoComplete="current-password"
                  placeholder={t("sys.reauth_pwd_ph")}
                  value={pkCurrPwd}
                  onInput={(e) => setPkCurrPwd((e.currentTarget as HTMLInputElement).value)}
                />
              </div>
            </form>
          </AnimatedModal>
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
                setOrigUserEmail("");
                setUserForm({
                  username: "",
                  email: "",
                  display_name: "",
                  role: "editor",
                  status: "active",
                  password: "",
                  current_password: "",
                });
                setUserModalOpen(true);
              }}
            >
              <PlusIcon size={14} /> {t("sys.new_user")}
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
                class="volo-input volo-search-input"
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
                {loading ? (
                  <TableSkeleton cols={5} rows={4} />
                ) : usersPageSlice.length === 0 ? (
                  <ConsoleEmpty colSpan={5} />
                ) : (
                  usersPageSlice.map((u) => {
                    const isSelf = u.id === currentUserSig.value?.id;
                    return (
                      <tr key={u.id}>
                        <td class="col-fluid" data-label={t("sys.col_user")}>
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
                        <td class="col-cat" data-label={t("sys.col_role")}>
                          {isSelf ? (
                            <span class="volo-cat-chip">{renderRoleLabel(u.role)}</span>
                          ) : (
                            <div class="volo-role-select-wrap">
                              <ConsoleSelect
                                searchable={false}
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
                                    void loadUsersList();
                                  } catch (err: any) {
                                    showToast(err.message);
                                  }
                                }}
                              />
                            </div>
                          )}
                        </td>
                        <td class="col-status" data-label={t("art.status")}>
                          <span class={`volo-status-pill ${u.status === "active" ? "ok" : "warn"}`}>
                            <span class="volo-status-dot" />
                            {u.status === "active" ? t("tbl.status_active") : t("tbl.status_disabled")}
                          </span>
                        </td>
                        <td class="col-date" data-label={t("sys.col_created")}>
                          {formatDateTime(u.created_at)}
                        </td>
                        <td class="col-act" data-label={t("art.actions")}>
                          <div class="volo-row-actions">
                            <button
                              type="button"
                              class="volo-act-btn"
                              onClick={() => {
                                setEditingUserId(u.id);
                                setOrigUserEmail(u.email || "");
                                setUserForm({
                                  username: u.username || "",
                                  email: u.email || "",
                                  display_name: u.display_name || "",
                                  role: u.role || "editor",
                                  status: u.status || "active",
                                  password: "",
                                  current_password: "",
                                });
                                setUserModalOpen(true);
                              }}
                            >
                              {t("common.edit")}
                            </button>
                            {!isSelf && (
                              <ActionMenu
                                items={[
                                  {
                                    label: u.status === "active" ? t("sys.user_disable") : t("sys.user_enable"),
                                    onClick: async () => {
                                      const nextSt = u.status === "active" ? "disabled" : "active";
                                      if (
                                        nextSt === "disabled" &&
                                        !(await openConfirm({ message: t("sys.confirm_disable_user", { name: u.username }), danger: true }))
                                      ) {
                                        return;
                                      }
                                      try {
                                        await apiFetch(`/api/admin/users/${u.id}`, {
                                          method: "PATCH",
                                          body: JSON.stringify({ status: nextSt }),
                                        });
                                        void loadUsersList();
                                      } catch (err: any) {
                                        showToast(err.message);
                                      }
                                    },
                                  },
                                  {
                                    label: t("common.delete"),
                                    danger: true,
                                    onClick: async () => {
                                      if (!(await openConfirm({ message: t("sys.confirm_del_user", { name: u.username }), danger: true }))) return;
                                      try {
                                        await apiFetch(`/api/admin/users/${u.id}`, { method: "DELETE" });
                                        void loadUsersList();
                                      } catch (err: any) {
                                        showToast(err.message);
                                      }
                                    },
                                  },
                                ]}
                              />
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
            total={usersTotal}
            page={usersPage}
            pageSize={usersPageSize}
            onPageSizeChange={(sz) => {
              setUsersPageSize(sz);
              setUsersPage(1);
            }}
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
                    const selfNeedsStepUp =
                      isSelfEdit &&
                      (userForm.email.trim().toLowerCase() !== origUserEmail.trim().toLowerCase() || Boolean(userForm.password.trim()));
                    if (selfNeedsStepUp && !userForm.current_password.trim()) {
                      showToast(t("sys.reauth_pwd_required"), "err");
                      return;
                    }
                    const body: Record<string, any> = {
                      display_name: userForm.display_name,
                      email: userForm.email,
                      role: userForm.role,
                      status: userForm.status,
                    };
                    if (userForm.password.trim()) body.password = userForm.password;
                    if (selfNeedsStepUp) body.current_password = userForm.current_password;
                    await apiFetch(`/api/admin/users/${editingUserId}`, {
                      method: "PATCH",
                      body: JSON.stringify(body),
                    });
                    if (isSelfEdit) await refreshMe();
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
                  void loadUsersList();
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

              <div class="volo-field">
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

              {Boolean(
                editingUserId &&
                  editingUserId === currentUserSig.value?.id &&
                  (userForm.email.trim().toLowerCase() !== origUserEmail.trim().toLowerCase() || Boolean(userForm.password.trim()))
              ) && (
                <div class="volo-field">
                  <label>{t("sys.cur_pwd")} *</label>
                  <input
                    type="password"
                    class="volo-input"
                    required
                    name="member_self_stepup_password"
                    autoComplete="current-password"
                    placeholder={t("sys.reauth_pwd_ph")}
                    value={userForm.current_password}
                    onInput={(e) => setUserForm({ ...userForm, current_password: (e.currentTarget as HTMLInputElement).value })}
                  />
                </div>
              )}
            </form>
          </AnimatedModal>
        </div>
      )}

      {sub === "waf" && isAdmin && (
        <div>
          <div class="volo-seg" style={{ marginBottom: "16px" }}>
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
              {t("sys.sub_waf_bans")} ({bansTotal})
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
              <div class="volo-check-grid">
                <SwitchToggle
                  checked={domains.security?.risk_control_enabled !== false}
                  onChange={(v) => setDomField("security", "risk_control_enabled", v)}
                  label={t("sys.risk_on")}
                />
                <SwitchToggle
                  checked={domains.security?.waf_enabled !== false}
                  onChange={(v) => setDomField("security", "waf_enabled", v)}
                  label={t("sys.waf_on")}
                />
                <SwitchToggle
                  checked={!!domains.security?.geo_policy_active}
                  onChange={(v) => setDomField("security", "geo_policy_active", v)}
                  label={t("sys.geo_policy_on")}
                />
              </div>
              <div class="volo-auto-grid">
                {domains.security?.waf_enabled !== false && (
                  <>
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
                  </>
                )}
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
              <div class="volo-grid-2">
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
                <div class="volo-field">
                  <label>{t("sys.geo_dir_label")}</label>
                  <input
                    class="volo-input"
                    name="sec_geoip_dir_path"
                    autoComplete="off"
                    placeholder={defaultGeoIPDir || "./data/geoip"}
                    value={domains.security?.geoip_dir || ""}
                    onInput={(e) => setDomField("security", "geoip_dir", (e.currentTarget as HTMLInputElement).value)}
                  />
                </div>
              </div>
              <div class="volo-form-actions">
                <button
                  type="button"
                  class="volo-btn"
                  onClick={async () => {
                    if (!(await openConfirm({ message: t("sys.confirm_rotate_share"), danger: true }))) return;
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
                <button type="submit" class="volo-btn volo-btn-primary" disabled={!!savingDomain}>
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
                  <PlusIcon size={14} /> {t("sys.new_ban")}
                </button>
              </div>

              <div class="volo-filter-bar">
                <div class="volo-filter-pills">
                  {(
                    [
                      ["active", t("sys.waf_state_active")],
                      ["expired", t("sys.waf_state_expired")],
                      ["all", t("art.all")],
                    ] as const
                  ).map(([st, lbl]) => (
                    <button
                      key={st}
                      type="button"
                      class={`volo-filter-pill ${banStateFilter === st ? "active" : ""}`}
                      onClick={() => {
                        setBanStateFilter(st);
                        setBansPage(1);
                        setSelBanIds([]);
                      }}
                    >
                      {lbl}
                    </button>
                  ))}
                </div>
                <div class="volo-filter-controls">
                  <input
                    class="volo-input volo-search-input"
                    name="waf_bans_search_filter"
                    autoComplete="off"
                    placeholder={t("sys.waf_ban_search_ph")}
                    value={banQuery}
                    onInput={(e) => {
                      setBanQuery((e.currentTarget as HTMLInputElement).value);
                      setBansPage(1);
                    }}
                  />
                </div>
              </div>

              <MobileSelectAllBar
                checked={bansPageSlice.length > 0 && bansPageSlice.every((b) => selBanIds.includes(b.id))}
                count={selBanIds.length}
                total={bansPageSlice.length}
                onToggle={(all) => {
                  const pageIds = bansPageSlice.map((b) => b.id);
                  setSelBanIds((prev) =>
                    all ? Array.from(new Set([...prev, ...pageIds])) : prev.filter((id) => !pageIds.includes(id))
                  );
                }}
              />

              <div class="volo-table-wrap">
                <table class="volo-table volo-table-compact">
                  <thead>
                    <tr>
                      <th class="col-check">
                        <input
                          type="checkbox"
                          checked={bansPageSlice.length > 0 && bansPageSlice.every((b) => selBanIds.includes(b.id))}
                          onChange={(e) => {
                            const all = (e.currentTarget as HTMLInputElement).checked;
                            const pageIds = bansPageSlice.map((b) => b.id);
                            setSelBanIds((prev) =>
                              all ? Array.from(new Set([...prev, ...pageIds])) : prev.filter((id) => !pageIds.includes(id))
                            );
                          }}
                        />
                      </th>
                      <th>{t("tbl.col_cidr")}</th>
                      <th class="col-fluid">{t("tbl.col_note")}</th>
                      <th class="col-act col-act-th">{t("art.actions")}</th>
                    </tr>
                  </thead>
                  <tbody>
                    {loading ? (
                      <TableSkeleton cols={4} rows={4} />
                    ) : bansPageSlice.length === 0 ? (
                      <ConsoleEmpty colSpan={4} />
                    ) : (
                      bansPageSlice.map((b) => (
                        <tr key={b.id}>
                          <td class="col-check">
                            <input
                              type="checkbox"
                              checked={selBanIds.includes(b.id)}
                              onChange={(e) => {
                                const on = (e.currentTarget as HTMLInputElement).checked;
                                setSelBanIds((prev) => (on ? [...prev, b.id] : prev.filter((x) => x !== b.id)));
                              }}
                            />
                          </td>
                          <td data-label={t("tbl.col_cidr")}>
                            <span class="volo-code-chip">{b.cidr}</span>
                          </td>
                          <td class="col-fluid" data-label={t("tbl.col_note")}>
                            <div class="volo-cell-meta">
                              <span>
                                {(() => {
                                  const code = String(b.reason_code || "").trim();
                                  const reason = String(b.reason || "").trim();
                                  const note = String(b.note || "").trim();
                                  const primaryRaw = code || reason || note;
                                  if (!primaryRaw) return "—";
                                  const primary = localizeWafReason(primaryRaw);
                                  const extra =
                                    note &&
                                    note.toLowerCase() !== primaryRaw.toLowerCase() &&
                                    !/^auto[-_ ]?ban\s*:/i.test(note)
                                      ? localizeWafReason(note)
                                      : "";
                                  return extra && extra !== primary ? `${primary} · ${extra}` : primary;
                                })()}
                              </span>
                              <span>{b.expires_at > 0 ? formatDateTime(b.expires_at) : t("sys.waf_dur_perm")}</span>
                            </div>
                          </td>
                          <td class="col-act" data-label={t("art.actions")}>
                            <div class="volo-row-actions">
                              <button
                                type="button"
                                class="volo-act-btn danger"
                                onClick={async () => {
                                  if (!(await openConfirm({ message: t("sys.confirm_unban_ip", { cidr: b.cidr }), danger: true }))) return;
                                  try {
                                    await apiFetch(`/api/admin/security/ip-bans/${b.id}`, { method: "DELETE" });
                                    setSelBanIds((prev) => prev.filter((x) => x !== b.id));
                                    void loadBansList();
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

              <BatchBar count={selBanIds.length} onClear={() => setSelBanIds([])}>
                <button
                  type="button"
                  class="volo-act-btn danger"
                  onClick={async () => {
                    if (
                      !(await openConfirm({
                        message: t("sys.confirm_batch_unban", { n: selBanIds.length }),
                        danger: true,
                      }))
                    ) {
                      return;
                    }
                    try {
                      await apiFetch("/api/admin/security/ip-bans/batch", {
                        method: "POST",
                        body: JSON.stringify({ action: "delete", ids: selBanIds }),
                      });
                      setSelBanIds([]);
                      void loadBansList();
                    } catch (err: any) {
                      showToast(err.message);
                    }
                  }}
                >
                  {t("sys.waf_unban_btn")}
                </button>
              </BatchBar>

              <Pagination
                total={bansTotal}
                page={bansPage}
                pageSize={bansPageSize}
                onPageSizeChange={(sz) => {
                  setBansPageSize(sz);
                  setBansPage(1);
                }}
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
                      void loadBansList();
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
                  <div class="volo-field">
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
                    const maxSort = geoRules.reduce(
                      (m, r) => Math.max(m, typeof r.sort_order === "number" ? r.sort_order : 0),
                      0
                    );
                    setEditingGeoId(null);
                    setGeoForm({
                      name: "Geo Rule",
                      action: "deny",
                      countries: "",
                      regions: "",
                      sort_order: maxSort + 10,
                      is_enabled: true,
                      on_miss: "next",
                    });
                    setGeoModalOpen(true);
                  }}
                >
                  <PlusIcon size={14} /> {t("sys.new_geo_rule")}
                </button>
              </div>
              {!geoCircuitEnabled && (
                <div class="volo-subcard" style={{ color: "var(--volo-warn)" }}>
                  {t("sys.geo_circuit_off")}
                </div>
              )}
              <div class="volo-subcard" style={{ marginBottom: "14px" }}>
                <div
                  class="volo-input-row"
                  style={{ justifyContent: "space-between", flexWrap: "wrap", gap: "8px", marginBottom: "8px" }}
                >
                  <div class="volo-subcard-title" style={{ marginBottom: 0 }}>
                    {t("sys.geo_db_sub_title")}
                  </div>
                  <div class="volo-input-row" style={{ flexWrap: "wrap", gap: "6px" }}>
                    {Array.isArray(geoDBStatus?.active_files) && geoDBStatus!.active_files!.length > 0 ? (
                      <>
                        <span class="volo-status-pill ok">
                          <span class="volo-status-dot" />
                          {t("sys.geo_status_active")}
                        </span>
                        {geoDBStatus!.active_files!.map((f) => (
                          <span key={f} class="volo-code-chip">
                            {f}
                          </span>
                        ))}
                      </>
                    ) : (
                      <span class="volo-help">
                        {t("sys.geo_status_empty", {
                          path: effectiveGeoIPDir || defaultGeoIPDir || "./data/geoip",
                        })}
                      </span>
                    )}
                  </div>
                </div>
                <div class="volo-field" style={{ marginBottom: 0 }}>
                  <label>{t("sys.geo_dir_label")}</label>
                  <div class="volo-input-row" style={{ flexWrap: "wrap", gap: "8px" }}>
                    <input
                      class="volo-input"
                      style={{ flex: "1 1 260px" }}
                      name="geo_rules_db_path_cfg"
                      autoComplete="off"
                      placeholder={defaultGeoIPDir || "./data/geoip"}
                      value={domains?.security?.geoip_dir || ""}
                      onInput={(e) =>
                        setDomField("security", "geoip_dir", (e.currentTarget as HTMLInputElement).value)
                      }
                      onKeyDown={(e) => {
                        if (e.key === "Enter") {
                          e.preventDefault();
                          void reloadGeoDatabases(true);
                        }
                      }}
                    />
                    <button
                      type="button"
                      class="volo-btn volo-btn-primary volo-btn-sm"
                      disabled={reloadingGeo || !!savingDomain}
                      onClick={() => void reloadGeoDatabases(true)}
                    >
                      {t("sys.geo_save_path_btn")}
                    </button>
                    <button
                      type="button"
                      class="volo-btn volo-btn-sm"
                      disabled={reloadingGeo || !!savingDomain}
                      onClick={() => void reloadGeoDatabases(false)}
                    >
                      {t("sys.geo_reload_btn")}
                    </button>
                  </div>
                  <span class="volo-help">
                    {t("sys.geo_dir_hint", { dir: defaultGeoIPDir || "./data/geoip" })}
                  </span>
                  {Array.isArray(geoDBStatus?.skipped_files) && geoDBStatus!.skipped_files!.length > 0 && (
                    <span class="volo-help" style={{ color: "var(--volo-warn)" }}>
                      {t("sys.geo_status_skipped", { files: geoDBStatus!.skipped_files!.join(", ") })}
                    </span>
                  )}
                </div>
              </div>
              <div class="volo-filter-bar">
                <div class="volo-filter-controls">
                  <input
                    class="volo-input volo-search-input"
                    name="geo_rules_search_filter"
                    autoComplete="off"
                    placeholder={t("sys.geo_search_ph")}
                    value={geoQuery}
                    onInput={(e) => {
                      setGeoQuery((e.currentTarget as HTMLInputElement).value);
                      setGeoPage(1);
                    }}
                  />
                </div>
              </div>
              <div class="volo-table-wrap">
                <table class="volo-table volo-table-compact">
                  <thead>
                    <tr>
                      <th>{t("art.status")}</th>
                      <th class="col-fluid">{t("tbl.col_scope")}</th>
                      <th class="col-fluid">{t("tbl.col_note")}</th>
                      <th class="col-act col-act-th">{t("art.actions")}</th>
                    </tr>
                  </thead>
                  <tbody ref={geoFlip.containerRef}>
                    {loading && geoRules.length === 0 ? (
                      <TableSkeleton cols={4} rows={4} />
                    ) : geoPaged.slice.length === 0 ? (
                      <ConsoleEmpty colSpan={4} />
                    ) : (
                      geoPaged.slice.map((g) => {
                        const fullIdx = geoRules.findIndex((r) => r.id === g.id);
                        const canMove = !geoQuery.trim();
                        const actTone =
                          g.action === "allow" ? "ok" : g.action === "challenge" ? "warn" : "danger";
                        const actLabel =
                          g.action === "allow"
                            ? t("sys.geo_allow")
                            : g.action === "block"
                              ? t("sys.geo_miss_block")
                              : g.action === "challenge"
                                ? t("sys.geo_miss_challenge")
                                : t("sys.geo_deny");
                        return (
                          <tr key={g.id} data-flip-key={g.id}>
                            <td data-label={t("art.status")}>
                              <div class="volo-input-row" style={{ flexWrap: "wrap", gap: "8px" }}>
                                <SwitchToggle
                                  checked={g.is_enabled !== false}
                                  onChange={(next) => void toggleGeoRuleEnabled(g, next)}
                                />
                                <span class={`volo-status-pill ${actTone}`}>
                                  <span class="volo-status-dot" />
                                  {actLabel}
                                </span>
                                {g.on_miss && g.on_miss !== "next" && (
                                  <span class="volo-mini-tag">
                                    {geoMissOptions.find((o) => o.value === g.on_miss)?.label || g.on_miss}
                                  </span>
                                )}
                              </div>
                            </td>
                            <td class="col-fluid" data-label={t("tbl.col_scope")}>
                              <span class="volo-code-chip">
                                ISO={g.countries || "*"} · {g.regions || "*"}
                              </span>
                            </td>
                            <td class="col-fluid" data-label={t("tbl.col_note")}>
                              <div class="volo-cell-meta">
                                <span>{g.name || "—"}</span>
                                {typeof g.sort_order === "number" && <span class="volo-code-chip">#{g.sort_order}</span>}
                              </div>
                            </td>
                            <td class="col-act" data-label={t("art.actions")}>
                              <div class="volo-row-actions">
                                <button
                                  type="button"
                                  class="volo-act-btn"
                                  disabled={!canMove || movingGeo || fullIdx <= 0}
                                  title={!canMove ? t("ui.sort_disabled_filtered") : t("cnt.move_up")}
                                  aria-label={t("cnt.move_up")}
                                  onClick={() => void moveGeoRule(g.id, -1)}
                                >
                                  ↑
                                </button>
                                <button
                                  type="button"
                                  class="volo-act-btn"
                                  disabled={!canMove || movingGeo || fullIdx < 0 || fullIdx >= geoRules.length - 1}
                                  title={!canMove ? t("ui.sort_disabled_filtered") : t("cnt.move_down")}
                                  aria-label={t("cnt.move_down")}
                                  onClick={() => void moveGeoRule(g.id, 1)}
                                >
                                  ↓
                                </button>
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
                                    if (
                                      !(await openConfirm({
                                        message: t("sys.confirm_del_geo", { name: g.name || g.countries || "*" }),
                                        danger: true,
                                      }))
                                    ) {
                                      return;
                                    }
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
                        );
                      })
                    )}
                  </tbody>
                </table>
              </div>
              <Pagination
                total={filteredGeoRules.length}
                page={geoPaged.safePage}
                pageSize={geoPageSize}
                onPageSizeChange={(sz) => {
                  setGeoPageSize(sz);
                  setGeoPage(1);
                }}
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
                  <div class="volo-auto-grid">
                    <div class="volo-field">
                      <label>{t("sys.lbl_geo_mode")}</label>
                      <ConsoleSelect
                        value={geoForm.action}
                        options={[
                          { value: "deny", label: t("sys.geo_deny") },
                          { value: "allow", label: t("sys.geo_allow") },
                          { value: "block", label: t("sys.geo_miss_block") },
                          { value: "challenge", label: t("sys.geo_miss_challenge") },
                        ]}
                        onChange={(v) => setGeoForm({ ...geoForm, action: v })}
                      />
                    </div>
                    <div class="volo-field">
                      <label>{t("sys.lbl_geo_on_miss")}</label>
                      <ConsoleSelect
                        value={geoForm.on_miss}
                        options={geoMissOptions}
                        onChange={(v) => setGeoForm({ ...geoForm, on_miss: v })}
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
                  <div class="volo-field">
                    <label>{t("sys.lbl_geo_country")}</label>
                    <TokenListInput
                      uppercase
                      placeholder={t("sys.geo_iso_ph")}
                      values={parseCSVList(geoForm.countries)}
                      onChange={(next) => setGeoForm({ ...geoForm, countries: next.join(",") })}
                    />
                  </div>
                  <div class="volo-field">
                    <label>{t("sys.lbl_geo_region")}</label>
                    <TokenListInput
                      placeholder={t("sys.geo_region_ph")}
                      values={parseCSVList(geoForm.regions)}
                      onChange={(next) => setGeoForm({ ...geoForm, regions: next.join(",") })}
                    />
                  </div>
                  <SwitchToggle
                    checked={geoForm.is_enabled}
                    onChange={(v) => setGeoForm({ ...geoForm, is_enabled: v })}
                    label={t("sys.geo_rule_enabled")}
                  />
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

              <div class="volo-subcard">
                <div class="volo-check-grid">
                  <SwitchToggle
                    checked={includeMediaExport}
                    onChange={setIncludeMediaExport}
                    label={t("sys.backup_with_media")}
                  />
                  <SwitchToggle
                    checked={dryRunRestore}
                    onChange={setDryRunRestore}
                    label={t("sys.backup_dry_run")}
                  />
                </div>
                {!dryRunRestore && (
                  <div class="volo-field" style={{ marginTop: "10px", marginBottom: "8px" }}>
                    <label>{t("sys.cur_pwd")} *</label>
                    <input
                      type="password"
                      class="volo-input"
                      name="backup_restore_stepup_password"
                      autoComplete="current-password"
                      placeholder={t("sys.reauth_pwd_ph")}
                      value={restoreCurrPwd}
                      onInput={(e) => setRestoreCurrPwd((e.currentTarget as HTMLInputElement).value)}
                    />
                  </div>
                )}
                <UploadProgressBar progress={restoreProgress} label={t("sys.backup_restore")} />
                <div class="volo-form-actions">
                  <UploadButton
                    accept=".zip,.json"
                    small={false}
                    busy={restoreBusy}
                    label={t("sys.backup_restore")}
                    onFile={async (f) => {
                      if (!dryRunRestore) {
                        if (!restoreCurrPwd.trim()) {
                          showToast(t("sys.stepup_pwd_required"), "err");
                          return;
                        }
                        if (!(await openConfirm({ message: t("sys.confirm_restore"), danger: true }))) return;
                      }
                      setRestoreBusy(true);
                      setRestoreProgress(0);
                      try {
                        const fd = new FormData();
                        fd.append("file", f);
                        fd.append("dry_run", dryRunRestore ? "true" : "false");
                        if (!dryRunRestore) {
                          fd.append("current_password", restoreCurrPwd);
                        }
                        await uploadWithProgress("/api/admin/backups/restore", fd, setRestoreProgress);
                        if (!dryRunRestore) setRestoreCurrPwd("");
                        showToast(dryRunRestore ? t("sys.backup_dry_ok") : t("sys.backup_restored"));
                        void loadSub();
                      } catch (err: any) {
                        showToast(err.message);
                      } finally {
                        setRestoreBusy(false);
                        setRestoreProgress(null);
                      }
                    }}
                  />
                  <button
                    type="button"
                    class="volo-btn volo-btn-primary"
                    disabled={exportingBackup}
                    onClick={() => {
                      if (exportingBackup) return;
                      setExportBakPwd("");
                      setExportBakOpen(true);
                    }}
                  >
                    {exportingBackup ? t("sys.backup_exporting") : t("sys.backup_export")}
                  </button>
                </div>
              </div>

              <AnimatedModal
                open={exportBakOpen}
                onClose={() => {
                  if (exportingBackup) return;
                  setExportBakOpen(false);
                  setExportBakPwd("");
                }}
                title={t("sys.backup_export")}
                subtitle={t("sys.stepup_backup_desc")}
                maxWidth="440px"
                footer={
                  <>
                    <button
                      type="button"
                      class="volo-btn"
                      disabled={exportingBackup}
                      onClick={() => {
                        setExportBakOpen(false);
                        setExportBakPwd("");
                      }}
                    >
                      {t("common.cancel")}
                    </button>
                    <button
                      type="submit"
                      form="volo-backup-export-form"
                      class="volo-btn volo-btn-primary"
                      disabled={exportingBackup}
                    >
                      {exportingBackup ? t("sys.backup_exporting") : t("sys.backup_export")}
                    </button>
                  </>
                }
              >
                <form
                  id="volo-backup-export-form"
                  autoComplete="off"
                  onSubmit={async (e) => {
                    e.preventDefault();
                    if (exportingBackup) return;
                    if (!exportBakPwd.trim()) {
                      showToast(t("sys.stepup_pwd_required"), "err");
                      return;
                    }
                    setExportingBackup(true);
                    try {
                      const res = await apiFetch<{
                        ticket: string;
                        export_url?: string;
                        download_url?: string;
                      }>("/api/admin/backups/ticket", {
                        method: "POST",
                        body: JSON.stringify({
                          include_media: includeMediaExport,
                          current_password: exportBakPwd,
                        }),
                      });
                      const baseUrl =
                        res.download_url ||
                        res.export_url ||
                        `/api/admin/backups/export?ticket=${encodeURIComponent(res.ticket)}`;
                      const sep = baseUrl.includes("?") ? "&" : "?";
                      const dlUrl = baseUrl.includes("media=")
                        ? baseUrl
                        : `${baseUrl}${sep}media=${includeMediaExport ? 1 : 0}`;
                      const a = document.createElement("a");
                      a.href = dlUrl;
                      a.download = "";
                      a.style.display = "none";
                      document.body.appendChild(a);
                      a.click();
                      a.remove();
                      setExportBakOpen(false);
                      setExportBakPwd("");
                    } catch (err: any) {
                      showToast(err?.message || t("sys.backup_export_err"), "err");
                    } finally {
                      setExportingBackup(false);
                    }
                  }}
                >
                  <div class="volo-field">
                    <label>{t("sys.cur_pwd")} *</label>
                    <input
                      type="password"
                      class="volo-input"
                      required
                      name="backup_export_stepup_password"
                      autoComplete="current-password"
                      placeholder={t("sys.reauth_pwd_ph")}
                      value={exportBakPwd}
                      onInput={(e) => setExportBakPwd((e.currentTarget as HTMLInputElement).value)}
                    />
                  </div>
                </form>
              </AnimatedModal>

              <div class="volo-subcard">
                <div class="volo-subcard-title">{t("sys.backup_legacy")}</div>
                <div class="volo-check-grid">
                  <SwitchToggle
                    checked={legacyDryRun}
                    onChange={setLegacyDryRun}
                    label={t("sys.import_dryrun_lbl")}
                  />
                </div>
                <UploadProgressBar progress={legacyProgress} label={t("setup.importing")} />
                <div class="volo-form-actions no-border">
                  <UploadButton
                    accept=".zip,.json"
                    small={false}
                    busy={legacyBusy}
                    busyLabel={t("setup.importing")}
                    label={t("sys.backup_legacy")}
                    onFile={async (f) => {
                      if (!legacyDryRun && !(await openConfirm({ message: t("sys.confirm_import_legacy"), danger: true }))) return;
                      setLegacyBusy(true);
                      setLegacyProgress(0);
                      try {
                        const fd = new FormData();
                        fd.append("file", f);
                        fd.append("dry_run", legacyDryRun ? "1" : "0");
                        const res = await uploadWithProgress<{ report?: any; dry_run?: boolean }>(
                          `/api/admin/backups/import-legacy${legacyDryRun ? "?dry_run=1" : ""}`,
                          fd,
                          setLegacyProgress,
                        );
                        setLegacyReport(res.report || res);
                        showToast(legacyDryRun ? t("sys.import_dryrun_ok") : t("setup.legacy_done"), "ok");
                        if (!legacyDryRun) void loadSub();
                      } catch (err: any) {
                        showToast(err.message);
                      } finally {
                        setLegacyBusy(false);
                        setLegacyProgress(null);
                      }
                    }}
                  />
                </div>

                {legacyReport && (
                  <div class="volo-subcard">
                    <div class="volo-input-row" style={{ justifyContent: "space-between", marginBottom: "8px", flexWrap: "wrap" }}>
                      <div class="volo-input-row" style={{ flexWrap: "wrap" }}>
                        <span class={`volo-status-pill ${legacyReport.dry_run ? "warn" : "ok"}`}>
                          <span class="volo-status-dot" />
                          {legacyReport.dry_run ? t("sys.legacy_preview_badge") : t("setup.legacy_done")}
                        </span>
                        {legacyReport.source_version && <span class="volo-code-chip">v:{legacyReport.source_version}</span>}
                      </div>
                      <button type="button" class="volo-act-btn" onClick={() => setLegacyReport(null)}>
                        {t("common.close")}
                      </button>
                    </div>
                    <div class="volo-cell-meta" style={{ flexWrap: "wrap", gap: "6px" }}>
                      {(
                        [
                          ["articles", legacyReport.articles],
                          ["article_revisions", legacyReport.revisions],
                          ["categories", legacyReport.categories],
                          ["tags", legacyReport.tags],
                          ["comments", legacyReport.comments],
                          ["links", legacyReport.links],
                          ["follows", legacyReport.follows],
                          ["series", legacyReport.series],
                          ["books", legacyReport.books],
                          ["nav_items", legacyReport.nav_items],
                          ["redirects", legacyReport.redirects_created],
                          ["users", legacyReport.users],
                        ] as Array<[string, number | undefined]>
                      )
                        .filter(([, n]) => typeof n === "number")
                        .map(([k, n]) => (
                          <span key={k} class="volo-mini-tag">
                            {localizeBackupTable(k)}: {n}
                          </span>
                        ))}
                    </div>
                    {Array.isArray(legacyReport.category_conflicts) && legacyReport.category_conflicts.length > 0 && (
                      <div class="volo-help" style={{ marginTop: "6px" }}>
                        <strong>{t("sys.legacy_conflicts", { count: legacyReport.category_conflicts.length })}:</strong>{" "}
                        {legacyReport.category_conflicts.slice(0, 10).join("; ")}
                      </div>
                    )}
                    {Array.isArray(legacyReport.warnings) && legacyReport.warnings.length > 0 && (
                      <div class="volo-help" style={{ marginTop: "6px", color: "var(--volo-warn)" }}>
                        <strong>{t("sys.legacy_warnings", { count: legacyReport.warnings.length })}:</strong>{" "}
                        {legacyReport.warnings.slice(0, 10).join("; ")}
                      </div>
                    )}
                  </div>
                )}
              </div>
            </div>

            {domains?.backup && (
              <form
                class="volo-card"
                autoComplete="off"
                onSubmit={async (e) => {
                  e.preventDefault();
                  const body: any = { ...(domains.backup || {}) };
                  if (bakWebdavPwd) body.webdav_password = bakWebdavPwd;
                  if (bakS3Secret) body.s3_secret_key = bakS3Secret;
                  const ok = await patchDomain("backup", body);
                  if (ok) {
                    setBakWebdavPwd("");
                    setBakS3Secret("");
                  }
                }}
              >
                <div class="volo-sec-head">
                  <div class="volo-sec-copy">
                    <h3 class="volo-card-title">{t("sys.auto_bak_head")}</h3>
                    <p class="volo-sec-desc">{t("sys.auto_bak_desc")}</p>
                  </div>
                </div>
                <div class="volo-check-grid">
                  <SwitchToggle
                    checked={!!domains.backup?.local_auto_enabled}
                    onChange={(v) => setDomField("backup", "local_auto_enabled", v)}
                    label={t("sys.auto_local_on")}
                  />
                  {domains.backup?.local_auto_enabled && (
                    <SwitchToggle
                      checked={!!domains.backup?.include_media_in_auto}
                      onChange={(v) => setDomField("backup", "include_media_in_auto", v)}
                      label={t("sys.auto_media_on")}
                    />
                  )}
                  <SwitchToggle
                    checked={!!domains.backup?.remote_enabled}
                    onChange={(v) => setDomField("backup", "remote_enabled", v)}
                    label={t("sys.remote_bak_on")}
                  />
                </div>

                {domains.backup?.local_auto_enabled && (
                  <div class="volo-grid-2">
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
                  </div>
                )}

                {domains.backup?.remote_enabled && (
                  <>
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
                          <div class="volo-field">
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
                          <div class="volo-field">
                            <label style={{ display: "flex", alignItems: "center", justifyContent: "space-between", gap: "8px", flexWrap: "wrap" }}>
                              <span>
                                {t("sys.lbl_webdav_pwd")}{" "}
                                {Boolean(domains.backup?.webdav_password_set || domains.backup?.has_webdav_password) && (
                                  <span class="volo-badge volo-badge-ok">{t("sys.sealed")}</span>
                                )}
                              </span>
                              {Boolean(domains.backup?.webdav_password_set || domains.backup?.has_webdav_password) && (
                                <button
                                  type="button"
                                  class="volo-act-btn danger"
                                  disabled={!!savingDomain}
                                  onClick={() => void clearDomainSecret("backup", "webdav_password")}
                                >
                                  {t("sys.clear_saved_secret")}
                                </button>
                              )}
                            </label>
                            <input
                              type="password"
                              class="volo-input"
                              name="bak_webdav_sealed_secret"
                              autoComplete="new-password"
                              placeholder={Boolean(domains.backup?.webdav_password_set || domains.backup?.has_webdav_password) ? t("sys.s3_sk_sealed_ph") : t("sys.webdav_pwd_ph")}
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
                          <div class="volo-field">
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
                          <div class="volo-field">
                            <label style={{ display: "flex", alignItems: "center", justifyContent: "space-between", gap: "8px", flexWrap: "wrap" }}>
                              <span>
                                {t("sys.lbl_s3_sk")}{" "}
                                {Boolean(domains.backup?.s3_secret_key_set || domains.backup?.has_s3_secret_key) && (
                                  <span class="volo-badge volo-badge-ok">{t("sys.sealed")}</span>
                                )}
                              </span>
                              {Boolean(domains.backup?.s3_secret_key_set || domains.backup?.has_s3_secret_key) && (
                                <button
                                  type="button"
                                  class="volo-act-btn danger"
                                  disabled={!!savingDomain}
                                  onClick={() => void clearDomainSecret("backup", "s3_secret_key")}
                                >
                                  {t("sys.clear_saved_secret")}
                                </button>
                              )}
                            </label>
                            <input
                              type="password"
                              class="volo-input"
                              name="bak_s3_sealed_secret"
                              autoComplete="new-password"
                              placeholder={Boolean(domains.backup?.s3_secret_key_set || domains.backup?.has_s3_secret_key) ? t("sys.s3_sk_sealed_ph") : t("sys.s3_sk_ph")}
                              value={bakS3Secret}
                              onInput={(e) => setBakS3Secret((e.currentTarget as HTMLInputElement).value)}
                            />
                          </div>
                        </div>
                      </div>
                    )}
                  </>
                )}

                <div class="volo-form-actions">
                  <button type="submit" class="volo-btn volo-btn-primary" disabled={!!savingDomain}>
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
                      file?: string;
                      name?: string;
                      remote_warning?: string;
                      items?: Array<{ name: string; size_bytes: number; mod_time?: number | string; created_at?: number | string }>;
                    }>(localBakEndpoint, { method: "POST", body: "{}" });
                    if (Array.isArray(res.items)) setLocalBackups(res.items);
                    else void loadSub();
                    if (res.remote_warning) {
                      showToast(res.remote_warning, "warn");
                    } else {
                      showToast(t("sys.bak_local_created", { name: res.file || res.name || "snapshot.zip" }));
                    }
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
              <table class="volo-table volo-table-compact">
                <thead>
                  <tr>
                    <th class="col-fluid">{t("sys.bak_col_file")}</th>
                    <th class="col-cat">{t("cnt.media_col_size")}</th>
                    <th class="col-date">{t("sys.col_created")}</th>
                    <th class="col-act col-act-th">{t("art.actions")}</th>
                  </tr>
                </thead>
                <tbody>
                  {loading ? (
                    <TableSkeleton cols={4} rows={3} />
                  ) : localBakPaged.slice.length === 0 ? (
                    <ConsoleEmpty colSpan={4} />
                  ) : (
                    localBakPaged.slice.map((b) => (
                      <tr key={b.name}>
                        <td class="col-fluid" data-label={t("sys.bak_col_file")}>
                          <span class="volo-code-chip">{b.name}</span>
                        </td>
                        <td class="col-cat" data-label={t("cnt.media_col_size")}>
                          <span class="volo-help">{formatBytes(b.size_bytes)}</span>
                        </td>
                        <td class="col-date" data-label={t("sys.col_created")}>
                          {formatDateTime(b.mod_time ?? b.created_at)}
                        </td>
                        <td class="col-act" data-label={t("art.actions")}>
                          <div class="volo-row-actions">
                            <a
                              class="volo-act-btn"
                              href={`${localBakEndpoint}/${encodeURIComponent(b.name)}`}
                              download={b.name}
                            >
                              {t("sys.bak_download")}
                            </a>
                            <ActionMenu
                              items={[
                                {
                                  label: t("sys.bak_restore_btn"),
                                  onClick: async () => {
                                    if (
                                      !dryRunRestore &&
                                      !(await openConfirm({ message: t("sys.confirm_restore_snap", { name: b.name }), danger: true }))
                                    ) {
                                      return;
                                    }
                                    try {
                                      await apiFetch(
                                        `${localBakEndpoint}/${encodeURIComponent(b.name)}/restore${dryRunRestore ? "?dry_run=1" : ""}`,
                                        {
                                          method: "POST",
                                          body: JSON.stringify({ dry_run: dryRunRestore }),
                                        }
                                      );
                                      showToast(dryRunRestore ? t("sys.backup_dry_ok") : t("sys.backup_restored"));
                                      void loadSub();
                                    } catch (err: any) {
                                      showToast(err.message);
                                    }
                                  },
                                },
                                {
                                  label: t("common.delete"),
                                  danger: true,
                                  onClick: async () => {
                                    if (!(await openConfirm({ message: t("sys.confirm_del_snap", { name: b.name }), danger: true }))) return;
                                    try {
                                      const res = await apiFetch<{
                                        items?: Array<{ name: string; size_bytes: number; mod_time?: number | string; created_at?: number | string }>;
                                      }>(`${localBakEndpoint}/${encodeURIComponent(b.name)}`, { method: "DELETE" });
                                      if (Array.isArray(res.items)) setLocalBackups(res.items);
                                      else void loadSub();
                                      showToast(t("common.deleted"));
                                    } catch (err: any) {
                                      showToast(err.message);
                                    }
                                  },
                                },
                              ]}
                            />
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
              <div class="volo-input-row" style={{ flexWrap: "wrap" }}>
                {doctorReport && (
                  <span class={`volo-status-pill ${doctorReport.healthy ? "ok" : "danger"}`}>
                    <span class="volo-status-dot" />
                    {doctorReport.healthy ? t("sys.doctor_healthy") : t("sys.doctor_issues")}
                  </span>
                )}
                <button type="button" class="volo-btn volo-btn-sm" onClick={() => void loadSub()}>
                  {t("common.refresh")}
                </button>
              </div>
            </div>
            <div class="volo-table-wrap">
              <table class="volo-table volo-table-compact">
                <thead>
                  <tr>
                    <th class="col-cat">{t("sys.doctor_col_check")}</th>
                    <th class="col-status">{t("art.status")}</th>
                    <th class="col-fluid">{t("sys.doctor_col_summary")}</th>
                  </tr>
                </thead>
                <tbody>
                  {!doctorReport ? (
                    <TableSkeleton cols={3} rows={4} />
                  ) : (doctorReport.checks || []).length === 0 ? (
                    <ConsoleEmpty colSpan={3} />
                  ) : (
                    (doctorReport.checks || []).map((chk: any) => {
                      const nameKey = `sys.doc_name_${chk.name}`;
                      const localizedName = t(nameKey) !== nameKey ? t(nameKey) : chk.name;
                      const rawStatus = String(chk.status || "ok").toLowerCase();
                      const statusKey = `sys.doc_status_${rawStatus}`;
                      const statusLabel = t(statusKey) !== statusKey ? t(statusKey) : rawStatus.toUpperCase();
                      const codeKey = chk.code ? `sys.doc_${chk.code}` : "";
                      const safeParams: Record<string, string> = { extra: "", ...(chk.params || {}) };
                      const translatedSummary = codeKey ? t(codeKey, safeParams) : "";
                      const localizedSummary =
                        codeKey && translatedSummary !== codeKey ? translatedSummary : chk.summary;
                      return (
                        <tr key={chk.name}>
                          <td class="col-cat" data-label={t("sys.doctor_col_check")}>
                            <strong style={{ fontWeight: 600 }}>{localizedName}</strong>
                            {localizedName !== chk.name && (
                              <span class="volo-code-chip" style={{ marginLeft: "6px" }}>
                                {chk.name}
                              </span>
                            )}
                          </td>
                          <td class="col-status" data-label={t("art.status")}>
                            <span class={`volo-status-pill ${rawStatus === "ok" ? "ok" : rawStatus === "warn" ? "warn" : "danger"}`}>
                              <span class="volo-status-dot" />
                              {statusLabel}
                            </span>
                          </td>
                          <td class="col-fluid" data-label={t("sys.doctor_col_summary")}>
                            <div class="volo-cell-main">
                              <div class="volo-cell-title">{localizedSummary}</div>
                              {chk.detail && (
                                <div class="volo-cell-meta">
                                  <span>{chk.detail}</span>
                                </div>
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
        </>
      )}

      {sub === "logs" && isAdmin && (
        <div class="volo-card">
          <div class="volo-sec-head">
            <div class="volo-sec-copy">
              <h3 class="volo-card-title">{t("sys.logs_head")}</h3>
              <p class="volo-sec-desc">{t("sys.logs_sub")}</p>
            </div>
            <div class="volo-input-row" style={{ flexWrap: "wrap" }}>
              {logStats && (
                <span class="volo-code-chip is-wrap">
                  {t("sys.logs_stats", {
                    count: logStats.total ?? runtimeLogs.length,
                    cap: logStats.capacity ?? 400,
                    goroutines: logStats.goroutines ?? 0,
                    mem: formatBytes(logStats.heap_alloc_bytes || 0),
                  })}
                </span>
              )}
              <button type="button" class="volo-btn volo-btn-sm" onClick={() => void loadRuntimeLogs()}>
                {t("common.refresh")}
              </button>
              <a
                class="volo-btn volo-btn-sm"
                href={`/api/admin/runtime-logs?format=text${logLevel !== "ALL" ? `&level=${encodeURIComponent(logLevel)}` : ""}${logQuery.trim() ? `&q=${encodeURIComponent(logQuery.trim())}` : ""}`}
                download="volo-runtime.log"
              >
                {t("sys.logs_download")}
              </a>
              <button
                type="button"
                class="volo-btn volo-btn-sm volo-btn-danger"
                onClick={async () => {
                  if (!(await openConfirm({ message: t("sys.logs_clear_confirm"), danger: true }))) return;
                  try {
                    await apiFetch("/api/admin/runtime-logs", { method: "DELETE" });
                    await loadRuntimeLogs();
                  } catch (err: any) {
                    showToast(err.message);
                  }
                }}
              >
                {t("sys.logs_clear")}
              </button>
            </div>
          </div>

          <div class="volo-filter-bar">
            <div class="volo-filter-pills">
              {(["ALL", "INFO", "WARN", "ERROR"] as const).map((lv) => (
                <button
                  key={lv}
                  type="button"
                  class={`volo-filter-pill ${logLevel === lv ? "active" : ""}`}
                  onClick={() => setLogLevel(lv)}
                >
                  {lv === "ALL" ? t("art.all") : lv}
                </button>
              ))}
            </div>
            <div class="volo-filter-controls">
              <SwitchToggle
                checked={autoScrollLogs}
                onChange={setAutoScrollLogs}
                label={t("sys.logs_autoscroll")}
              />
              <input
                class="volo-input volo-search-input"
                name="runtime_logs_query"
                autoComplete="off"
                placeholder={t("sys.logs_search_ph")}
                value={logQuery}
                onInput={(e) => setLogQuery((e.currentTarget as HTMLInputElement).value)}
                onKeyDown={(e) => {
                  if (e.key === "Enter") void loadRuntimeLogs(logLevel, logQuery);
                }}
              />
              {logQuery && (
                <button type="button" class="volo-act-btn" onClick={() => setLogQuery("")}>
                  <CloseIcon size={13} />
                </button>
              )}
            </div>
          </div>

          <div ref={logViewerRef} class="volo-log-viewer">
            {runtimeLogs.length === 0 ? (
              <div class="volo-log-empty">{t("sys.logs_empty")}</div>
            ) : (
              runtimeLogs.map((l) => {
                const lv = String(l.level || "INFO");
                const { msg, attrs } = extractLogAttrs(l);
                return (
                  <div key={l.seq || `${l.timestamp}-${l.message}`} class="volo-log-row">
                    <span class="volo-log-ts">{formatLogTs(l.timestamp)}</span>
                    <span class={`volo-log-lvl ${lv.toLowerCase()}`}>{lv}</span>
                    {l.source && <span class="volo-log-attrs">({l.source})</span>}
                    <span>{msg}</span>
                    {attrs.map(([ak, av], idx) => (
                      <span key={`${ak}-${idx}`} class="volo-log-chip">
                        {ak}={av}
                      </span>
                    ))}
                  </div>
                );
              })
            )}
          </div>
        </div>
      )}

      {sub === "audit" && isAdmin && (
        <div class="volo-card">
          <div class="volo-sec-head">
            <div class="volo-sec-copy">
              <h3 class="volo-card-title">{t("sys.audit")}</h3>
              <p class="volo-sec-desc">{t("sys.audit_desc")}</p>
            </div>
            <button type="button" class="volo-btn volo-btn-sm" onClick={() => void loadAuditLogs()}>
              {t("common.refresh")}
            </button>
          </div>

          <div class="volo-filter-bar">
            <div class="volo-filter-controls">
              <ConsoleSelect
                value={auditAction}
                options={auditActionOptions}
                onChange={(v) => {
                  setAuditAction(v);
                  setAuditPage(1);
                }}
              />
              <input
                class="volo-input volo-search-input"
                name="audit_search_filter"
                autoComplete="off"
                placeholder={t("sys.audit_search_ph")}
                value={auditQuery}
                onInput={(e) => {
                  const nextQ = (e.currentTarget as HTMLInputElement).value;
                  setAuditQuery(nextQ);
                  setAuditPage(1);
                }}
                onKeyDown={(e) => {
                  if (e.key === "Enter") void loadAuditLogs(auditQuery, auditAction, auditPage, auditPageSize);
                }}
              />
            </div>
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
                {loading ? (
                  <TableSkeleton cols={4} rows={5} />
                ) : auditPageSlice.length === 0 ? (
                  <ConsoleEmpty colSpan={4} />
                ) : (
                  auditPageSlice.map((l) => {
                    const prettyDetail = formatDetailJSON(l.detail_json);
                    const isExpanded = expandedAuditId === l.id;
                    return (
                      <tr key={l.id}>
                        <td class="col-date" data-label={t("sys.audit_col_time")}>
                          {formatDateTime(l.created_at)}
                        </td>
                        <td class="col-cat" data-label={t("sys.audit_col_actor")}>
                          <span class="volo-cat-chip">{l.actor_name}</span>
                        </td>
                        <td class="col-cat" data-label={t("sys.audit_col_action")}>
                          <span class="volo-mini-tag" title={l.action}>
                            {localizeAuditAction(l.action)}
                          </span>
                        </td>
                        <td class="col-fluid" data-label={t("sys.audit_col_res")} style={{ minWidth: 0 }}>
                          <div class="volo-input-row" style={{ flexWrap: "wrap", minWidth: 0 }}>
                            <span class="volo-code-chip is-wrap" title={`${l.resource_type}:${l.resource_id}`}>
                              {localizeAuditResource(l.resource_type)}
                              {l.resource_id ? ` · ${l.resource_id}` : ""}
                            </span>
                            {l.client_ip && <span class="volo-code-chip">{l.client_ip}</span>}
                            {prettyDetail && (
                              <button
                                type="button"
                                class="volo-act-btn"
                                onClick={() => setExpandedAuditId(isExpanded ? null : l.id)}
                              >
                                {isExpanded ? t("common.close") : t("sys.audit_detail_btn")}
                              </button>
                            )}
                          </div>
                          {isExpanded && prettyDetail && (
                            <pre
                              class="volo-subcard"
                              style={{
                                marginTop: "8px",
                                fontSize: "12px",
                                whiteSpace: "pre-wrap",
                                overflowWrap: "anywhere",
                                overflowX: "auto",
                              }}
                            >
                              {prettyDetail}
                            </pre>
                          )}
                        </td>
                      </tr>
                    );
                  })
                )}
              </tbody>
            </table>
          </div>
          <Pagination
            total={auditTotal}
            page={auditPage}
            pageSize={auditPageSize}
            onPageSizeChange={(sz) => {
              setAuditPageSize(sz);
              setAuditPage(1);
            }}
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
