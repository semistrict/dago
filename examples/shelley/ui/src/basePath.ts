declare const __SHELLEY_BASE_PATH__: string;
declare const __SHELLEY_WASM_BUILD__: boolean;

function normalizeBasePath(value: string): string {
  const withLeadingSlash = value.startsWith("/") ? value : `/${value}`;
  return withLeadingSlash.endsWith("/") ? withLeadingSlash : `${withLeadingSlash}/`;
}

export const appBasePath = normalizeBasePath(
  typeof __SHELLEY_BASE_PATH__ === "string" ? __SHELLEY_BASE_PATH__ : "/",
);
export const browserWasmBuild =
  typeof __SHELLEY_WASM_BUILD__ === "boolean" && __SHELLEY_WASM_BUILD__;

export function appPath(path = ""): string {
  return `${appBasePath}${path.replace(/^\/+/, "")}`;
}

export function appPathname(pathname = window.location.pathname): string {
  const baseWithoutSlash = appBasePath.slice(0, -1);
  if (!baseWithoutSlash) return pathname;
  if (pathname === baseWithoutSlash) return "/";
  if (!pathname.startsWith(appBasePath)) return pathname;
  return `/${pathname.slice(appBasePath.length)}`;
}
