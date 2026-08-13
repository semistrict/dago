import * as esbuild from "esbuild";
import vuePlugin from "esbuild-plugin-vue3";
import * as fs from "fs";
import * as path from "path";
import * as zlib from "zlib";
import * as crypto from "crypto";
import { execFileSync, execSync } from "child_process";
import { justBashBrowserShims } from "@semistrict/dawasm-browser/esbuild";

// Esbuild plugin: rewrite any "monaco-editor*" import (including deep paths
// like monaco-editor/esm/vs/editor/editor.api) to the deployed runtime URL,
// marked external. Our custom bundle entry re-exports
// everything monaco-vim needs from that single file.
//
// We also bypass monaco-vim's package.json exports map: it routes the
// "browser" condition to a UMD bundle that esbuild wraps with a CJS
// require() shim, which then tries to require the external Monaco bundle at
// runtime and fails. Resolve directly to the ESM index.mjs instead.
function monacoExternalPlugin(monacoRuntimePath) {
  return {
    name: "monaco-external",
    setup(build) {
      build.onResolve({ filter: /^monaco-editor(\/|$)/ }, () => ({
        path: monacoRuntimePath,
        external: true,
      }));
      const monacoVimEsm = path.resolve(process.cwd(), "node_modules/monaco-vim/dist/index.mjs");
      build.onResolve({ filter: /^monaco-vim$/ }, () => ({ path: monacoVimEsm }));
    },
  };
}

const isWatch = process.argv.includes("--watch");
const isProd = !isWatch;
const verbose = process.env.VERBOSE === "1" || process.env.VERBOSE === "true";
// Release builds (NO_SOURCEMAPS=1) ship no JS source maps
// to keep the embedded binary small. Other builds emit them (gzip-compressed)
// so devtools work in development.
const dropSourceMaps = process.env.NO_SOURCEMAPS === "1";
// FAST_BUILD=1 is for builds whose output is only ever executed by tests, never
// shipped or debugged: CI's test steps. Source maps go away (nothing reads them
// headlessly) and the remaining assets compress at level 1 instead of 9. The
// output is byte-for-byte equivalent after decompression, so the server, the
// embed check and every test behave identically — only the artifact is bigger,
// which nothing downstream of a test run cares about. Worth ~5s per build, and
// CI builds the UI once per shelley step (8 of them).
const fastBuild = process.env.FAST_BUILD === "1";
const wasmBuild = process.env.WASM_BUILD === "1";
const publicBasePath = normalizeBasePath(process.env.PUBLIC_BASE_PATH || "/");
const monacoRuntimePath = `${publicBasePath}monaco-editor.js`;
const buildDefines = {
  __SHELLEY_BASE_PATH__: JSON.stringify(publicBasePath),
  __SHELLEY_WASM_BUILD__: JSON.stringify(wasmBuild),
};
const gzipLevel = fastBuild ? 1 : 9;
const noSourceMaps = dropSourceMaps || fastBuild;
const bundleMetafiles = [];

async function buildBundle(options) {
  const result = await esbuild.build({ ...options, metafile: true });
  bundleMetafiles.push(result.metafile);
  return result;
}

function normalizeBasePath(value) {
  const withLeadingSlash = value.startsWith("/") ? value : `/${value}`;
  return withLeadingSlash.endsWith("/") ? withLeadingSlash : `${withLeadingSlash}/`;
}

function withPublicBasePath(value) {
  return value.replaceAll("__SHELLEY_BASE_PATH__", publicBasePath);
}

function log(...args) {
  if (verbose) console.log(...args);
}

async function build() {
  const startTime = Date.now();
  try {
    // Ensure dist directory exists
    if (!fs.existsSync("dist")) {
      fs.mkdirSync("dist");
    }

    // Build Monaco editor worker separately (IIFE format for web worker)
    log("Building Monaco editor worker...");
    await buildBundle({
      entryPoints: ["node_modules/monaco-editor/esm/vs/editor/editor.worker.js"],
      bundle: true,
      outfile: "dist/editor.worker.js",
      format: "iife",
      minify: isProd,
      sourcemap: !noSourceMaps,
    });

    // Build @pierre/diffs worker for syntax highlighting (IIFE format for web worker)
    log("Building diffs worker...");
    await buildBundle({
      entryPoints: ["src/diffs-worker.ts"],
      bundle: true,
      outfile: "dist/diffs-worker.js",
      format: "iife",
      minify: isProd,
      sourcemap: !noSourceMaps,
    });

    // Browser-native Shelley runs the portable Go application in this worker.
    log("Building WASM application worker...");
    await buildBundle({
      entryPoints: ["src/wasm-worker.ts"],
      bundle: true,
      outfile: "dist/wasm-worker.js",
      format: "iife",
      minify: isProd,
      sourcemap: !noSourceMaps,
      plugins: [justBashBrowserShims()],
    });

    // Build Monaco editor as a separate chunk (JS + CSS).
    // We bundle through src/monaco-bundle-entry.js so we can also surface
    // the internal modules monaco-vim depends on (ShiftCommand) as named
    // exports of /monaco-editor.js — that way monaco-vim runs against the
    // *same* Monaco instance the rest of the app loads.
    log("Building Monaco editor bundle...");
    await buildBundle({
      entryPoints: ["src/monaco-bundle-entry.js"],
      bundle: true,
      outfile: "dist/monaco-editor.js",
      format: "esm",
      minify: isProd,
      sourcemap: !noSourceMaps,
      loader: {
        ".ttf": "file",
      },
    });

    // Build the Vue 3 + PrimeVue app (src/vue/main.ts). It emits dist/main.js +
    // dist/main.css, which index.html links statically (see src/index.html).
    log("Building main application (src/vue/main.ts)...");
    await buildBundle({
      entryPoints: ["src/vue/main.ts"],
      bundle: true,
      outfile: "dist/main.js",
      format: "esm",
      minify: isProd,
      sourcemap: !noSourceMaps,
      external: ["monaco-editor", monacoRuntimePath],
      define: buildDefines,
      loader: {
        ".png": "dataurl",
        ".svg": "text",
        ".woff": "dataurl",
        ".woff2": "dataurl",
        ".ttf": "dataurl",
        ".eot": "dataurl",
      },
      // Prefer ESM entry points so dynamic imports (e.g. monaco-vim) end
      // up using `import` rather than CJS `require` (which esbuild can't
      // emit at runtime in the browser).
      // monaco-vim's package.json exports a UMD bundle under the "browser"
      // condition; esbuild picks that by default and wraps it in a CJS
      // shim that requires() the external /monaco-editor.js at runtime,
      // which fails in the browser. Force resolution to its ESM build.

      // monaco-vim imports specific submodules of monaco-editor. Rewrite
      // those to the same runtime URL the rest of the app uses, so we end
      // up with a single Monaco instance instead of two. The rewritten
      // imports are marked external (above) so esbuild emits them as-is.
      plugins: [monacoExternalPlugin(monacoRuntimePath), vuePlugin()],
    });

    // /static/excalidraw/skill.js: self-contained Excalidraw + React +
    // skill helper bundle. The host React app fetches it same-origin and
    // streams it into the sandboxed `output_iframe` iframe via
    // postMessage; the iframe wraps it in a Blob and import()s it from
    // its own opaque origin, sidestepping CORS.
    log("Building /static/excalidraw bundle...");
    fs.mkdirSync("dist/static/excalidraw", { recursive: true });
    await buildBundle({
      entryPoints: ["src/excalidraw-skill.js"],
      bundle: true,
      outfile: "dist/static/excalidraw/skill.js",
      format: "esm",
      minify: isProd,
      sourcemap: false,
      define: { "process.env.NODE_ENV": '"production"' },
      // Inline the stylesheet and any referenced font/icon assets as data
      // URLs so the resulting module is fully self-contained.
      loader: {
        ".css": "text",
        ".woff": "dataurl",
        ".woff2": "dataurl",
        ".ttf": "dataurl",
        ".png": "dataurl",
        ".svg": "dataurl",
      },
    });

    // Copy static files
    const indexHTML = withPublicBasePath(fs.readFileSync("src/index.html", "utf8"));
    fs.writeFileSync("dist/index.html", indexHTML);
    fs.copyFileSync("src/styles.css", "dist/styles.css");

    for (const generatedPage of ["404.html", ".nojekyll"]) {
      const generatedPath = `dist/${generatedPage}`;
      if (fs.existsSync(generatedPath)) fs.unlinkSync(generatedPath);
    }
    if (wasmBuild) {
      // Pages serves 404.html for client-side conversation routes while
      // preserving the requested URL, allowing the app router to restore it.
      fs.writeFileSync("dist/404.html", indexHTML);
      fs.writeFileSync("dist/.nojekyll", "");
    }

    // Copy assets (icons, manifest, etc.)
    const assetsDir = "src/assets";
    if (fs.existsSync(assetsDir)) {
      for (const file of fs.readdirSync(assetsDir)) {
        if (file === "manifest.json") {
          const manifest = withPublicBasePath(fs.readFileSync(`${assetsDir}/${file}`, "utf8"));
          fs.writeFileSync(`dist/${file}`, manifest);
        } else {
          fs.copyFileSync(`${assetsDir}/${file}`, `dist/${file}`);
        }
      }
    }

    writeThirdPartyArtifacts(bundleMetafiles);

    // Write build info without embedding local filesystem paths.
    // Get git commit info
    let commit = "";
    let commitTime = "";
    let modified = false;
    try {
      commit = execSync("git rev-parse HEAD", { encoding: "utf8" }).trim();
      commitTime = execSync("git log -1 --format=%cI", { encoding: "utf8" }).trim();
      // Check for modifications, excluding the dist/ directory (which we're currently building)
      const status = execSync("git status --porcelain --ignore-submodules", { encoding: "utf8" });
      // Filter out dist/ changes since those are expected during build
      const significantChanges = status
        .split("\n")
        .filter((line) => line.trim() && !line.includes("dist/"));
      modified = significantChanges.length > 0;
    } catch (e) {
      // Git not available or not a git repo
    }

    const buildInfo = {
      timestamp: Date.now(),
      date: new Date().toISOString(),
      commit: commit,
      commitTime: commitTime,
      modified: modified,
    };
    fs.writeFileSync("dist/build-info.json", JSON.stringify(buildInfo, null, 2));

    // Generate gzip versions of large files and remove originals to reduce binary size
    // The server will decompress on-the-fly for the rare clients that don't support gzip
    log("\nGenerating gzip compressed files...");
    const filesToCompress = [
      "monaco-editor.js",
      "editor.worker.js",
      "diffs-worker.js",
      "wasm-worker.js",
      "monaco-editor.css",
      "styles.css",
      "main.js",
      "main.css",
      "static/excalidraw/skill.js",
    ];
    const checksums = {};
    let totalOrigSize = 0;
    let totalGzSize = 0;

    for (const file of filesToCompress) {
      const inputPath = `dist/${file}`;
      const outputPath = `dist/${file}.gz`;
      if (fs.existsSync(inputPath)) {
        const input = fs.readFileSync(inputPath);
        const compressed = zlib.gzipSync(input, { level: gzipLevel });
        fs.writeFileSync(outputPath, compressed);

        // Compute SHA256 of the compressed content for ETag
        const hash = crypto.createHash("sha256").update(compressed).digest("hex").slice(0, 16);
        checksums[file] = hash;

        totalOrigSize += input.length;
        totalGzSize += compressed.length;

        if (verbose) {
          const origKb = (input.length / 1024).toFixed(1);
          const gzKb = (compressed.length / 1024).toFixed(1);
          const ratio = ((compressed.length / input.length) * 100).toFixed(0);
          console.log(`  ${file}: ${origKb} KB -> ${gzKb} KB gzip (${ratio}%) [${hash}]`);
        }

        // Native builds embed only compressed assets. A standalone WASM build
        // is served by an ordinary static server, so it keeps originals too.
        if (!wasmBuild) fs.unlinkSync(inputPath);
      }
    }

    // Source maps are large (tens of MB uncompressed) and only fetched by
    // browsers with devtools open. Release builds (NO_SOURCEMAPS=1, set by
    // release.yml) drop them entirely; other builds gzip them so the embedded
    // binary stays small while devtools still work. The server serves
    // <name>.map from the embedded <name>.map.gz, exactly as for .js/.css.
    log(noSourceMaps ? "\nRemoving source maps..." : "\nGzipping source maps...");
    for (const file of fs.readdirSync("dist")) {
      if (noSourceMaps) {
        // dist/ isn't cleaned between builds, so also drop .map.gz left over
        // from a previous dev build.
        if (file.endsWith(".map") || file.endsWith(".map.gz")) {
          fs.unlinkSync(`dist/${file}`);
        }
        continue;
      }
      if (!file.endsWith(".map")) continue;
      const inputPath = `dist/${file}`;
      const input = fs.readFileSync(inputPath);
      const compressed = zlib.gzipSync(input, { level: gzipLevel });
      fs.writeFileSync(`${inputPath}.gz`, compressed);
      // Record a content checksum so the server can emit ETags and answer 304s
      // for source maps, matching the other compressed assets.
      checksums[file] = crypto.createHash("sha256").update(compressed).digest("hex").slice(0, 16);
      fs.unlinkSync(inputPath);
      if (verbose) {
        const origKb = (input.length / 1024).toFixed(1);
        const gzKb = (compressed.length / 1024).toFixed(1);
        console.log(`  ${file}: ${origKb} KB -> ${gzKb} KB gzip`);
      }
    }

    // Write checksums for ETag support
    fs.writeFileSync("dist/checksums.json", JSON.stringify(checksums, null, 2));
    log("\nChecksums written to dist/checksums.json");

    if (verbose) {
      console.log("\nOther files:");
      const otherFiles = fs
        .readdirSync("dist")
        .filter((f) => (f.endsWith(".ttf") || f.endsWith(".map")) && !f.endsWith(".gz"));
      for (const file of otherFiles.sort()) {
        const stats = fs.statSync(`dist/${file}`);
        const sizeKb = (stats.size / 1024).toFixed(1);
        console.log(`  ${file}: ${sizeKb} KB`);
      }
    }

    const elapsed = ((Date.now() - startTime) / 1000).toFixed(1);
    const totalGzKb = (totalGzSize / 1024).toFixed(0);
    console.log(`UI built in ${elapsed}s (${totalGzKb} KB gzipped)`);
  } catch (error) {
    console.error("Build failed:", error);
    process.exit(1);
  }
}

function writeThirdPartyArtifacts(metafiles) {
  const components = new Map();
  for (const metafile of metafiles) {
    for (const input of Object.keys(metafile.inputs)) {
      const packageRoot = findNodePackageRoot(path.resolve(input));
      if (!packageRoot) continue;
      const manifest = JSON.parse(fs.readFileSync(path.join(packageRoot, "package.json"), "utf8"));
      if (!manifest.name || !manifest.version) continue;
      const key = `npm:${manifest.name}@${manifest.version}`;
      const packageName = manifest.name.startsWith("@")
        ? `%40${manifest.name.slice(1)}`
        : manifest.name;
      const licenseFiles = readLicenseFiles(packageRoot);
      const declaredLicense = normalizeLicense(manifest.license);
      components.set(key, {
        type: "library",
        group: manifest.name.startsWith("@") ? manifest.name.split("/")[0].slice(1) : undefined,
        name: manifest.name,
        version: manifest.version,
        license: declaredLicense === "NOASSERTION" ? detectLicense(licenseFiles) : declaredLicense,
        licenseFiles,
        purl: `pkg:npm/${packageName}@${manifest.version}`,
      });
    }
  }

  if (wasmBuild) {
    const goTemplate = "{{with .Module}}{{.Path}}|{{.Version}}|{{.Dir}}{{end}}";
    const goModules = execFileSync(
      "go",
      ["list", "-deps", "-f", goTemplate, "./cmd/shelley-wasm"],
      {
        cwd: "..",
        encoding: "utf8",
        env: { ...process.env, GOOS: "js", GOARCH: "wasm" },
      },
    );
    for (const line of goModules.split("\n")) {
      if (!line.trim()) continue;
      const [modulePath, version, moduleDir] = line.split("|");
      if (!modulePath || !moduleDir) continue;
      const resolvedVersion = version || "local";
      const key = `go:${modulePath}@${resolvedVersion}`;
      const licenseFiles = readLicenseFiles(moduleDir);
      components.set(key, {
        type: "library",
        name: modulePath,
        version: resolvedVersion,
        license: detectLicense(licenseFiles),
        licenseFiles,
        purl: version ? `pkg:golang/${modulePath}@${version}` : undefined,
      });
    }
  }

  const sorted = [...components.values()].sort((left, right) =>
    `${left.name}@${left.version}`.localeCompare(`${right.name}@${right.version}`),
  );
  const bom = {
    bomFormat: "CycloneDX",
    specVersion: "1.5",
    version: 1,
    metadata: {
      component: {
        type: "application",
        name: "Shelley browser app",
        licenses: [{ expression: "Apache-2.0" }],
      },
    },
    components: sorted.map(({ licenseFiles, license, ...component }) => ({
      ...component,
      licenses: [{ expression: license }],
    })),
  };
  fs.writeFileSync("dist/sbom.cdx.json", `${JSON.stringify(bom, null, 2)}\n`);

  const notice = [
    "THIRD-PARTY SOFTWARE NOTICES",
    "",
    "This browser application was copied from and modified from Shelley, Copyright 2026 Bold Software, Inc., under the Apache License, Version 2.0.",
    "",
    "--- Shelley application LICENSE ---",
    fs.readFileSync("../LICENSE", "utf8").trim(),
    "",
    "This artifact contains the following bundled components. License texts are reproduced below when supplied by the component.",
    "",
  ];
  for (const component of sorted) {
    notice.push(
      `${component.name} ${component.version}`,
      `Declared license: ${component.license}`,
      "",
    );
    for (const file of component.licenseFiles) {
      notice.push(`--- ${file.name} ---`, file.text.trim(), "");
    }
  }
  fs.writeFileSync("dist/THIRD_PARTY_NOTICES.txt", `${notice.join("\n").trim()}\n`);
}

function findNodePackageRoot(input) {
  let directory = path.dirname(input);
  while (directory.startsWith(process.cwd())) {
    const manifest = path.join(directory, "package.json");
    if (directory.includes(`${path.sep}node_modules${path.sep}`) && fs.existsSync(manifest)) {
      return directory;
    }
    const parent = path.dirname(directory);
    if (parent === directory) break;
    directory = parent;
  }
  return null;
}

function readLicenseFiles(directory) {
  if (!fs.existsSync(directory)) return [];
  return fs
    .readdirSync(directory, { withFileTypes: true })
    .filter((entry) => entry.isFile() && /^(license|copying|notice)([._-]|$)/i.test(entry.name))
    .map((entry) => ({
      name: entry.name,
      text: fs.readFileSync(path.join(directory, entry.name), "utf8"),
    }));
}

function normalizeLicense(license) {
  if (typeof license === "string" && license.trim()) return license.trim();
  if (license && typeof license.type === "string" && license.type.trim())
    return license.type.trim();
  return "NOASSERTION";
}

function detectLicense(files) {
  const text = files
    .map((file) => file.text)
    .join("\n")
    .toLowerCase();
  if (text.includes("apache license") && text.includes("version 2.0")) return "Apache-2.0";
  if (text.includes("permission is hereby granted, free of charge")) return "MIT";
  if (text.includes("redistribution and use in source and binary forms")) return "BSD-3-Clause";
  return "NOASSERTION";
}

build();
