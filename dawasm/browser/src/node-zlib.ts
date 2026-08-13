// just-bash 3.2's browser bundle imports node:zlib. These browser-native
// implementations keep Node-only code out of browser workers.
export { gzipSync, gunzipSync } from "fflate";

export const constants = {
  Z_BEST_COMPRESSION: 9,
  Z_BEST_SPEED: 1,
  Z_DEFAULT_COMPRESSION: 6,
} as const;
