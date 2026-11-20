import { Annotation } from "@codemirror/state";
import type { EditorView, ViewUpdate } from "@codemirror/view";

export const propSyncAnnotation = Annotation.define<boolean>();

export interface PropSyncTracker {
  pending: string[];
}

export function createPropSyncTracker(): PropSyncTracker {
  return { pending: [] };
}

export function recordEditorEmit(tracker: PropSyncTracker, docStr: string): void {
  tracker.pending.push(docStr);
  if (tracker.pending.length > 128) {
    tracker.pending.shift();
  }
}

export function isPropSyncUpdate(update: Pick<ViewUpdate, "transactions">): boolean {
  return (
    update.transactions.length > 0 &&
    update.transactions.every((tr) => Boolean(tr.annotation(propSyncAnnotation)))
  );
}

export function shouldApplyPropToView(tracker: PropSyncTracker, nextVal: string, curDoc: string): boolean {
  const idx = tracker.pending.lastIndexOf(nextVal);
  if (idx >= 0) {
    const hasNewerEmit = idx < tracker.pending.length - 1;
    tracker.pending.splice(0, idx + 1);
    if (hasNewerEmit) {
      return false;
    }
  } else {
    tracker.pending.length = 0;
  }
  return curDoc !== nextVal;
}

export function syncCMViewValue(
  view: EditorView | null,
  composing: boolean,
  tracker: PropSyncTracker,
  nextVal: string
): void {
  if (!view || view.composing || composing) return;
  const cur = view.state.doc.toString();
  if (!shouldApplyPropToView(tracker, nextVal, cur)) return;
  const sel = view.state.selection.main;
  view.dispatch({
    changes: { from: 0, to: cur.length, insert: nextVal },
    selection: {
      anchor: Math.min(sel.anchor, nextVal.length),
      head: Math.min(sel.head, nextVal.length),
    },
    annotations: propSyncAnnotation.of(true),
  });
}

export function buildCMImeHandlers(
  composingRef: { current: boolean },
  valueRef: { current: string },
  onChangeRef: { current: (next: string) => void },
  tracker: PropSyncTracker
) {
  return {
    compositionstart() {
      composingRef.current = true;
      return false;
    },
    compositionend(_event: CompositionEvent, view: EditorView) {
      composingRef.current = false;
      queueMicrotask(() => {
        if (view.composing || composingRef.current) return;
        const docStr = view.state.doc.toString();
        if (docStr !== (valueRef.current || "")) {
          recordEditorEmit(tracker, docStr);
          onChangeRef.current(docStr);
        }
      });
      return false;
    },
  };
}
