import { ComponentType, h } from "preact";
import { useEffect, useState } from "preact/hooks";
import { t } from "../i18n";

export function createLazyView<P extends object>(
  loader: () => Promise<ComponentType<P>>,
  minHeight = "280px"
): ComponentType<P> {
  let cached: ComponentType<P> | null = null;
  let pending: Promise<ComponentType<P>> | null = null;

  const load = (): Promise<ComponentType<P>> => {
    if (cached) return Promise.resolve(cached);
    if (!pending) {
      pending = loader()
        .then((comp) => {
          cached = comp;
          return comp;
        })
        .catch((err) => {
          pending = null;
          throw err;
        });
    }
    return pending;
  };

  return function LazyWorkspaceView(props: P) {
    const [state, setState] = useState<{ Comp: ComponentType<P> | null }>(() => ({ Comp: cached }));
    const [loadErr, setLoadErr] = useState(false);
    const [retryTick, setRetryTick] = useState(0);

    useEffect(() => {
      if (cached) {
        if (state.Comp !== cached) setState({ Comp: cached });
        return;
      }
      let active = true;
      setLoadErr(false);
      load()
        .then((Comp) => {
          if (active) setState({ Comp });
        })
        .catch(() => {
          if (active) setLoadErr(true);
        });
      return () => {
        active = false;
      };
    }, [retryTick]);

    if (state.Comp) {
      return h(state.Comp, props);
    }
    if (loadErr) {
      return h(
        "div",
        {
          class: "volo-card",
          role: "alert",
          style: {
            minHeight,
            display: "flex",
            flexDirection: "column",
            alignItems: "center",
            justifyContent: "center",
            gap: "12px",
            textAlign: "center",
          },
        },
        h("span", { class: "volo-help" }, t("api.err_network")),
        h(
          "button",
          {
            type: "button",
            class: "volo-btn volo-btn-sm",
            onClick: () => setRetryTick((n) => n + 1),
          },
          t("common.refresh")
        )
      );
    }
    return h(
      "div",
      {
        class: "volo-card",
        style: {
          minHeight,
          display: "flex",
          alignItems: "center",
          justifyContent: "center",
        },
      },
      h("span", { class: "volo-help" }, t("shell.loading"))
    );
  };
}

export const OverviewWorkspace = createLazyView(() =>
  import("./OverviewWorkspace").then((m) => m.OverviewWorkspace)
);
const loadExtraWorkspaces = () => import("./extraWorkspaces");
export const ContentWorkspace = createLazyView(() =>
  loadExtraWorkspaces().then((m) => m.ContentWorkspace)
);
export const DistributeWorkspace = createLazyView(() =>
  loadExtraWorkspaces().then((m) => m.DistributeWorkspace)
);
export const AppearanceWorkspace = createLazyView(() =>
  loadExtraWorkspaces().then((m) => m.AppearanceWorkspace)
);
export const SystemWorkspace = createLazyView(() =>
  loadExtraWorkspaces().then((m) => m.SystemWorkspace)
);
export const AboutHelpWorkspace = createLazyView(() =>
  loadExtraWorkspaces().then((m) => m.AboutHelpWorkspace)
);
