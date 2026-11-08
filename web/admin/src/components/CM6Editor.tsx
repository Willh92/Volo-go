import { CM6CssEditorCore, CM6EditorCore } from "./CM6EditorCore";

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

export const CM6Editor = CM6EditorCore;
export const CM6CssEditor = CM6CssEditorCore;

