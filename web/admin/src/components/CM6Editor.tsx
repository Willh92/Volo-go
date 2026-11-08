import type { FunctionComponent } from "preact";
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

let cachedEditorImpl: FunctionComponent<CM6EditorProps> | null = null;
let cachedCssEditorImpl: FunctionComponent<CM6CssEditorProps> | null = null;

export function CM6Editor(props: CM6EditorProps) {
  const [Impl, setImpl] = useState<FunctionComponent<CM6EditorProps> | null>(() => cachedEditorImpl);

  useEffect(() => {
    if (cachedEditorImpl) return;
    let cancelled = false;
    void import("./CM6EditorCore").then((mod) => {
      cachedEditorImpl = mod.CM6EditorCore;
      if (!cancelled) {
        setImpl(() => mod.CM6EditorCore);
      }
    });
    return () => {
      cancelled = true;
    };
  }, []);

  if (!Impl) {
    return (
      <textarea
        class="volo-textarea"
        rows={14}
        value={props.value}
        onInput={(e) => props.onChange((e.currentTarget as HTMLTextAreaElement).value)}
        placeholder={t("ed.loading")}
      />
    );
  }

  return <Impl {...props} />;
}

export function CM6CssEditor(props: CM6CssEditorProps) {
  const [Impl, setImpl] = useState<FunctionComponent<CM6CssEditorProps> | null>(() => cachedCssEditorImpl);

  useEffect(() => {
    if (cachedCssEditorImpl) return;
    let cancelled = false;
    void import("./CM6EditorCore").then((mod) => {
      cachedCssEditorImpl = mod.CM6CssEditorCore;
      if (!cancelled) {
        setImpl(() => mod.CM6CssEditorCore);
      }
    });
    return () => {
      cancelled = true;
    };
  }, []);

  if (!Impl) {
    return (
      <textarea
        class="volo-textarea volo-mono"
        rows={10}
        value={props.value}
        onInput={(e) => props.onChange((e.currentTarget as HTMLTextAreaElement).value)}
        placeholder={props.placeholder || ""}
      />
    );
  }

  return <Impl {...props} />;
}

