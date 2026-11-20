import test from "node:test";
import assert from "node:assert/strict";
import { EditorState } from "@codemirror/state";
import {
  createPropSyncTracker,
  isPropSyncUpdate,
  propSyncAnnotation,
  recordEditorEmit,
  shouldApplyPropToView,
} from "../src/components/cm6PropSync.ts";

test("propSyncAnnotation suppresses updateListener re-emission on programmatic prop sync", () => {
  const state = EditorState.create({ doc: "initial" });

  // Programmatic sync transaction carries propSyncAnnotation
  const syncTr = state.update({
    changes: { from: 0, to: state.doc.length, insert: "external" },
    annotations: propSyncAnnotation.of(true),
  });
  assert.equal(isPropSyncUpdate({ transactions: [syncTr] }), true);

  // Normal user transaction does NOT carry propSyncAnnotation
  const userTr = state.update({
    changes: { from: 0, to: state.doc.length, insert: "user typed" },
  });
  assert.equal(isPropSyncUpdate({ transactions: [userTr] }), false);
});

test("shouldApplyPropToView prevents rapid Backspace + insertText stale-prop ping-pong", () => {
  const tracker = createPropSyncTracker();
  const emitted: string[] = [];

  // Simulate user Control+A + Backspace -> emits ""
  let cmDoc = "";
  recordEditorEmit(tracker, cmDoc);
  emitted.push(cmDoc);

  // Immediately before Preact commits "", user pastes/inserts richBody -> emits richBody
  const richBody = "# S3 Heading\n\n> [!NOTE]\n> Callout";
  cmDoc = richBody;
  recordEditorEmit(tracker, cmDoc);
  emitted.push(cmDoc);

  // Render 1 commits stale prop "" while CM6 already holds richBody
  assert.equal(shouldApplyPropToView(tracker, "", cmDoc), false);
  // Render 2 commits richBody which matches cmDoc -> no overwrite needed
  assert.equal(shouldApplyPropToView(tracker, richBody, cmDoc), false);
  assert.deepEqual(emitted, ["", richBody]);
});

test("shouldApplyPropToView applies genuine external prop resets and article switches", () => {
  const tracker = createPropSyncTracker();
  recordEditorEmit(tracker, "draft text");

  // External startNewArticle resets value to "" while doc is still "draft text"
  assert.equal(shouldApplyPropToView(tracker, "", "draft text"), true);

  // External article load changes value to "# Loaded"
  assert.equal(shouldApplyPropToView(tracker, "# Loaded", ""), true);

  // Matching value does not dispatch
  assert.equal(shouldApplyPropToView(tracker, "# Loaded", "# Loaded"), false);
});

test("shouldApplyPropToView handles >16 rapid burst emits without evicting older pending values", () => {
  const tracker = createPropSyncTracker();
  const emits: string[] = [];
  for (let i = 1; i <= 40; i++) {
    const val = `step-${i}`;
    recordEditorEmit(tracker, val);
    emits.push(val);
  }
  const latest = emits[emits.length - 1];
  // Stale Preact prop flush for step-1 must NOT revert CM6 from step-40
  assert.equal(shouldApplyPropToView(tracker, emits[0], latest), false);
  // Subsequent intermediate prop flushes must also NOT revert CM6
  assert.equal(shouldApplyPropToView(tracker, emits[20], latest), false);
  // Final flush matches latest -> no dispatch needed
  assert.equal(shouldApplyPropToView(tracker, latest, latest), false);
});
