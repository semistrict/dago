// just-bash 3.2's advertised browser bundle still imports node:zlib. Keep that
// Node-only module out of Shelley and provide its browser-native equivalents.
export { gzipSync, gunzipSync } from "fflate";

export const constants = {
  Z_BEST_COMPRESSION: 9,
  Z_BEST_SPEED: 1,
  Z_DEFAULT_COMPRESSION: 6,
} as const;
