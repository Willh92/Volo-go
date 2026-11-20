import test from "node:test";
import assert from "node:assert/strict";
import { resolveTaxonomyInitAction } from "../src/views/articleInitGuard.ts";

const cleanBase = {
  section: "editor",
  editingID: null as string | null,
  dirty: false,
  title: "",
  slug: "",
  contentMD: "",
  summaryMD: "",
  categoryID: "",
  editingCategoryName: "",
  hasCategories: true,
};

test("resolveTaxonomyInitAction allows clean reset only when uninitialized and untouched", () => {
  assert.equal(resolveTaxonomyInitAction(cleanBase), "reset");
  assert.equal(resolveTaxonomyInitAction({ ...cleanBase, editingID: "" }), "apply_default_category");
  assert.equal(resolveTaxonomyInitAction({ ...cleanBase, editingID: "", hasCategories: false }), "noop");
});

test("resolveTaxonomyInitAction preserves mid-typing input when taxonomies settle late", () => {
  assert.equal(
    resolveTaxonomyInitAction({ ...cleanBase, dirty: true }),
    "apply_default_category"
  );
  assert.equal(
    resolveTaxonomyInitAction({ ...cleanBase, title: "Draft Title" }),
    "apply_default_category"
  );
  assert.equal(
    resolveTaxonomyInitAction({ ...cleanBase, editingID: "", slug: "draft-slug", contentMD: "Hello" }),
    "apply_default_category"
  );
  assert.equal(
    resolveTaxonomyInitAction({
      ...cleanBase,
      editingID: "",
      dirty: true,
      title: "Typed",
      categoryID: "cat_1",
    }),
    "noop"
  );
});

test("resolveTaxonomyInitAction returns noop outside editor or when editing existing article", () => {
  assert.equal(resolveTaxonomyInitAction({ ...cleanBase, section: "articles" }), "noop");
  assert.equal(resolveTaxonomyInitAction({ ...cleanBase, editingID: "art_42" }), "noop");
});
