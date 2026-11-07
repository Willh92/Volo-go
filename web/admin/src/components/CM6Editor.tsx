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
}

let cachedEditorImpl: FunctionComponent<CM6EditorProps> | null = null;

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
