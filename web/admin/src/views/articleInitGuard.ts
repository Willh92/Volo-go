export interface ArticleInitSnapshot {
  section: string;
  editingID: string | null;
  dirty: boolean;
  title: string;
  slug: string;
  contentMD: string;
  summaryMD: string;
  categoryID: string;
  editingCategoryName: string;
  hasCategories: boolean;
}

export type TaxonomyInitAction = "reset" | "apply_default_category" | "noop";

export function resolveTaxonomyInitAction(s: ArticleInitSnapshot): TaxonomyInitAction {
  if (s.section !== "editor") return "noop";
  if (s.editingID !== null && s.editingID !== "") return "noop";
  const hasUserInput = Boolean(
    s.dirty ||
      s.title.trim() ||
      s.slug.trim() ||
      s.contentMD.trim() ||
      s.summaryMD.trim()
  );
  if (s.editingID === null && !hasUserInput) {
    return "reset";
  }
  if (!s.categoryID && !s.editingCategoryName && s.hasCategories) {
    return "apply_default_category";
  }
  return "noop";
}
