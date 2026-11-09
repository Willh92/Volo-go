import { useEffect, useState } from "preact/hooks";
import { t } from "../i18n";

export interface QuickInsertItem {
  plugin_id?: string;
  id: string;
  label: string;
  template: string;
}

export interface CM6EditorProps {
  value: string;
  onChange: (next: string) => void;
  quickInserts?: QuickInsertItem[];
  onSave?: () => void;
}

export interface CM6CssEditorProps {
  value: string;
  onChange: (next: string) => void;
  placeholder?: string;
  onSave?: () => void;
}

type CM6CoreModule = typeof import("./CM6EditorCore");

let cachedCore: CM6CoreModule | null = null;
let loadingCore: Promise<CM6CoreModule> | null = null;

function loadCM6Core(): Promise<CM6CoreModule> {
  if (cachedCore) return Promise.resolve(cachedCore);
  if (!loadingCore) {
    loadingCore = import("./CM6EditorCore")
      .then((mod) => {
        cachedCore = mod;
        return mod;
      })
      .catch((err) => {
        loadingCore = null;
        throw err;
      });
  }
  return loadingCore;
}

export function CM6Editor(props: CM6EditorProps) {
  const [mod, setMod] = useState<CM6CoreModule | null>(() => cachedCore);

  useEffect(() => {
    if (cachedCore) {
      if (!mod) setMod(cachedCore);
      return;
    }
    let active = true;
    loadCM6Core()
      .then((m) => {
        if (active) setMod(m);
      })
      .catch(() => {});
    return () => {
      active = false;
    };
  }, []);

  if (!mod) {
    return (
      <div
        class="volo-card"
        style={{ minHeight: "320px", display: "flex", alignItems: "center", justifyContent: "center" }}
      >
        <span class="volo-help">{t("shell.loading")}</span>
      </div>
    );
  }
  const Core = mod.CM6EditorCore;
  return <Core {...props} />;
}

export function CM6CssEditor(props: CM6CssEditorProps) {
  const [mod, setMod] = useState<CM6CoreModule | null>(() => cachedCore);

  useEffect(() => {
    if (cachedCore) {
      if (!mod) setMod(cachedCore);
      return;
    }
    let active = true;
    loadCM6Core()
      .then((m) => {
        if (active) setMod(m);
      })
      .catch(() => {});
    return () => {
      active = false;
    };
  }, []);

  if (!mod) {
    return (
      <div
        class="volo-card"
        style={{ minHeight: "180px", display: "flex", alignItems: "center", justifyContent: "center" }}
      >
        <span class="volo-help">{t("shell.loading")}</span>
      </div>
    );
  }
  const CssCore = mod.CM6CssEditorCore;
  return <CssCore {...props} />;
}
