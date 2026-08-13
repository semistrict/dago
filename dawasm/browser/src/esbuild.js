import { fileURLToPath } from "node:url";

const zlibShim = fileURLToPath(new URL("./node-zlib.ts", import.meta.url));

// justBashBrowserShims returns the esbuild resolver needed for just-bash's
// browser worker bundle.
export function justBashBrowserShims() {
  return {
    name: "dawasm-just-bash-browser-shims",
    setup(build) {
      build.onResolve({ filter: /^node:zlib$/ }, () => ({ path: zlibShim }));
    },
  };
}
