import { createLazyView } from "../views/lazyWorkspaces";

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
  hasAudio?: boolean;
  onSave?: () => void;
}

export interface CM6CssEditorProps {
  value: string;
  onChange: (next: string) => void;
  placeholder?: string;
  onSave?: () => void;
}

const loadCM6Core = () => import("./CM6EditorCore");

export const CM6Editor = createLazyView<CM6EditorProps>(
  () => loadCM6Core().then((m) => m.CM6EditorCore),
  "320px"
);

export const CM6CssEditor = createLazyView<CM6CssEditorProps>(
  () => loadCM6Core().then((m) => m.CM6CssEditorCore),
  "180px"
);
