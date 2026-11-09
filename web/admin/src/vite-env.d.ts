/// <reference types="vite/client" />

declare module "virtual:volo-locales" {
  export interface BuiltinLocaleMeta {
    code: string;
    name: string;
    dir: string;
  }
  export const builtinLocales: BuiltinLocaleMeta[];
  export const keys: string[];
}
