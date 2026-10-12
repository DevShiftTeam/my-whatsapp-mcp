# Documentation dependency security disposition

Controller ruling: 2026-10-12, public-documentation Task 1 review round 1.
This developer-only file is outside the public page/download inventory. It must not enter HTML, local search, copied Markdown, individual downloads, or the combined documentation bundle.

## Accepted scope

Retain the selected stable VitePress 1.6.4 and exact locked dependencies for documentation preparation, later static builds, and checked static public output. This is narrow acceptance of the documented development-server and unused-formatter advisories, subject to the gates below. It is **not a clean dependency audit**, a general dependency-security approval, or permission to run affected server features.

Task 3's `build` sequences allowlisted preparation, static VitePress rendering, and Markdown export. It reads reviewed canonical Markdown, uses gray-matter YAML `safeLoad`/`safeDump`, validates qualification records, and writes generated staging. The JavaScript frontmatter engine is disabled. The build launches no documentation server. Publication remains unqualified.

The controller found that current stable VitePress is 1.6.4, while its next release is alpha; VitePress 1.6.4 declares Vite `^5.4.14`. Advisory patches in newer Vite majors are outside that supported range. Keep the selected stable pins; do not apply forced upgrades, gray-matter downgrades, or unsupported transitive overrides to obtain an apparently clean audit.

## Affected installed versions and primary advisories

The Task 1 lockfile resolves these seven affected packages. npm audit reports six moderate and one high finding, including inherited dependency-chain findings.

| Installed package | Audit affected range | Dependency path / advisory |
| --- | --- | --- |
| VitePress 1.6.4 | `<=1.6.4` | Direct dependency → Vite; inherits Vite advisories |
| Vite 5.4.21 | `<=6.4.2` (aggregate) | VitePress → Vite; advisories below |
| esbuild 0.21.5 | `<=0.24.2` | VitePress → Vite → esbuild; [development-server CORS](https://github.com/advisories/GHSA-67mh-4wv8-2f99) |
| gray-matter 4.0.3 | `>=2.0.2` (inherited) | Direct dependency → js-yaml; inherits formatter advisory |
| js-yaml 3.15.2 | `3.2.7 - 3.15.2` (inherited) | gray-matter → js-yaml → argparse |
| argparse 1.0.10 | `1.0.0 - 1.0.10` (inherited) | gray-matter → js-yaml → argparse → sprintf-js |
| sprintf-js 1.0.3 | `<=1.1.3` (audit aggregate `*`) | [unbounded-precision denial of service](https://github.com/advisories/GHSA-hp3w-g68c-fv3c) |

Vite's primary advisories:

- [Optimized dependency `.map` path traversal](https://github.com/advisories/GHSA-4w7w-66w2-5vf9): installed Vite is within `<=6.4.1`; published patched lines are 6.4.2, 7.3.2, and 8.0.5. The affected route is development-server optimized-dependency middleware.
- [Windows launch-editor UNC disclosure](https://github.com/advisories/GHSA-v6wh-96g9-6wx3): installed Vite is within `<=6.4.2`; published patched lines are 6.4.3, 7.3.5, and 8.0.16; launch-editor's own patch is 2.14.1. The affected operation is editor middleware on Windows with NTLM authentication.
- [Windows alternate-path `server.fs.deny` bypass](https://github.com/advisories/GHSA-fx2h-pf6j-xcff): installed Vite is within `<=6.4.2`; published patched lines are 6.4.3, 7.3.5, and 8.0.16. It concerns a running dev server and sensitive allowed files on NTFS or Windows volumes using short names.

esbuild's CORS advisory affects its **serve** feature; its published patch is 0.25.0. Local Vite code uses esbuild transform/build helpers rather than `esbuild.serve`. The formatter advisory has no published patch; npm suggests downgrading gray-matter to 2.0.1, which this disposition does not authorize.

## Reachability and server restrictions

The following assessment is based on inspected installed code and primary advisory conditions. It does not claim exploit reproduction or prove every future dependency call path safe.

- Current preparation does not import Vite or esbuild. gray-matter's YAML operations import js-yaml's loader/dumper; they do not run js-yaml's command-line entrypoint or argparse's sprintf formatting path. The vulnerable formatter remains installed.
- Later static builds may invoke Vite and esbuild transform/build operations. A build process and its resulting static HTML do not run the listed development-server HTTP handlers. Task 3 and Task 5 must reassess the actual implementation rather than inherit this statement automatically.
- The archive runtime has no Node documentation package dependency. Public Pages output serves checked static files; it does not execute the Node build toolchain or host an archive/MCP process.

**No Vite dev server is approved. No VitePress preview workflow is approved, including wildcard or other non-loopback binding.** Loopback binding alone does not authorize affected development/editor middleware: a malicious browser page can reach local services in some advisory scenarios.

Task 3 replaces the earlier preview refusal with `scripts/preview.mjs`, a separate Node static-file server. It imports only Node built-ins, the reviewed public inventory, and pure URL/location helpers; it does not import Vite, VitePress, preparation, or Markdown parsing. It binds the literal `127.0.0.1` address and accepts only an optional numeric `--port`. Every host override is rejected. The fixed CLI root is generated `dist/`; requests require the exact validated repository prefix, and only reviewed HTML/Markdown/download paths and generated asset extensions are served. Traversal, encoded paths, dotfiles, unlisted pages, symlink roots/ancestors/files, directory listings, SPA fallback, and methods other than GET/HEAD are refused. Do not replace this server with `vitepress dev`, `vite dev`, `vite preview`, or `vitepress preview`.

## Required remediation and review gates

**Task 3:** replace the refusal with a separate static-output-only preview using a literal loopback address. Serve only the checked generated `dist/` tree. Do not import or invoke Vite/VitePress server middleware, traverse canonical/private source directories, or accept wildcard/non-loopback binding. Test the actual command, listener address, static-root containment, direct nested routes, and refusal of unsafe paths. Reassess the pinned advisories against both the static build and preview implementation before approving server use.

**Task 5 / CI / publication:** rerun dependency audit against the exact committed lockfile and review each advisory's reachability in actual build, preview, tests, and deployed static output. New findings, newly reachable vulnerable paths, or a supported stable patched release require a fresh controller disposition and remediation plan before enabling the workflow. Do not describe the audit as clean while findings remain. CI must not expose a Vite development/editor server or publish Node/private build material.

**Future dependency update:** prefer a supported stable patched dependency baseline once available, then validate preparation, Markdown exports, static build/output boundaries, and browser behavior. A changed lockfile or workflow does not inherit this narrow acceptance automatically.

Detailed original npm audit JSON, exact runtime/checksum evidence, and observed tests remain in the excluded controller Task 1 report under `.superpowers/sdd/`. This file records the committed operating restriction; it is not an end-user verification claim.

## Task 3 implementation reassessment

Re-audited the unchanged lockfile on 2026-10-12: **seven findings remain (six moderate, one high)**, matching the accepted advisory inventory above. No dependency was upgraded or overridden. The static build calls only `vitepress build ..`; the isolated preview uses `node:http` and filesystem reads. Browser tests use that exact preview against built output. Neither path starts the affected optimized-dependency, editor, Vite filesystem-serving, or esbuild serve handlers. Pure URL helpers are separated into `scripts/urls.mjs`; the browser theme does not bundle Node filesystem access, gray-matter, or the export parser. gray-matter remains on the reviewed safe YAML loader/dumper path; its CLI formatter dependency remains installed but unused.

Focused regression coverage checks the actual npm preview command and rejected host overrides, literal listener address, full-base direct nested HTML, assets and Markdown, unsupported methods, missing/private paths, encoded traversal, and symlink boundaries. This evidence supports the narrow static-only implementation; it does not make the dependency audit clean or grant CI/publication approval. The Task 5 fresh-audit and controller-review gate remains required.

## Task 5 artifact and CI reassessment (2026-10-12)

Fresh `npm audit --json` against the Task 5 lockfile still exits 1 with the same **seven findings: six moderate, one high**. The five primary advisory URLs above were reopened and their scope reviewed. The npm registry still identifies VitePress **1.6.4** as `latest` and **2.0.0-alpha.20** as `next`. The selected stable baseline and its supported Vite range remain unchanged. This is not a clean dependency audit.

Task 5 adds exact **parse5 8.0.1** (HTML DOM parsing), **acorn 8.19.0** (JavaScript syntax trees, without evaluation), and **postcss-value-parser 4.2.0** (CSS URL/import tokens); parse5's `entities` dependency is locked to **8.1.0**. The existing exact **markdown-it 14.3.2** parses Markdown link/reference tokens. These packages are build/test tools only and introduce no current audit finding. No unsupported override or downgrade was applied.

Reachability was reassessed against installed source and the actual commands:

- `scripts/build.mjs` invokes only `vitepress build ..`. Vite's installed `dist/node/chunks/dep-BK3b2jBa.js` contains the affected editor/filesystem middleware in its `createServer` path; the build uses its `transformWithEsbuild`/build path. No Vite or esbuild development server is started by CI, tests or preview.
- `gray-matter/lib/engines.js` binds YAML to `safeLoad`/`safeDump`. `js-yaml/index.js` exports loader/dumper APIs; `js-yaml/bin/js-yaml.js` imports argparse, and `argparse/lib/argument_parser.js` imports sprintf-js. The reviewed build never invokes that CLI/formatter path. Executable frontmatter remains disabled.
- `scripts/check-output.mjs` parses HTML, Markdown and JavaScript without executing emitted scripts. It checks exact inventory, staged/export/bundle parity, page hash map, search document routes/anchors, imported chunk existence, known private/credential patterns and unknown files. Arbitrary copied assets and extra page chunks are rejected. Only named VitePress runtime families and `404.html` are accepted as generated files.
- `scripts/preview.mjs` still imports Node built-ins and pure configuration only. Browser qualification revealed two missing exact generated runtime paths: `vp-icons.css` and `assets/chunks/@localSearchIndexroot.<hash>.js`. These are now served through narrow rules. Other `@` paths, encodings, traversal, private pages, symlinks and host overrides remain denied. Without the icon CSS, VitePress had attempted its remote Iconify fallback; the corrected built-site journey now observes zero external requests.
- `.github/workflows/docs.yml` uses `contents: read` for PR builds and no provider secrets. Only a successful `main` push or main-only manual dispatch in a public repository can upload/deploy. Only the deployment job receives `pages: write` and `id-token: write`; it checks the exact intended repository, uses `github-pages`, and does not cancel in-progress deployments. No `pull_request_target` event or build in the privileged job is used.

`npm run check:dependencies` now performs the live audit and rejects any newly affected package, new advisory chain, changed severity, malformed/unavailable audit, or changed/unavailable stable VitePress tag. It reports accepted remaining findings explicitly. It does not replace source reachability review when dependencies, commands or the workflow change.

The official action refs were read from GitHub on 2026-10-12; all four refs were commit objects (no annotated tag remained to dereference). Workflow pins: checkout v5 `fbc6f3992d24b796d5a048ff273f7fcc4a7b6c09`, setup-node v5 `a0853c24544627f65ddf259abe73b1d18a591444`, upload-pages-artifact v4 `7b1f4a764d45c48632c6b24a0339c27f5614fb0b`, deploy-pages v4 `d6db90164ac5ed86f2b6aed7e0febac5b3c0c03e`. The inspected upload action also pins its nested upload-artifact v4.6.2 to `ea165f8d65b6e75b540449e92b4886f43607fa02`. The documentation process is pinned to Node **24.21.0**; deploy-pages' own action runtime is upstream Node 20 and is separate from that build runtime.

Local artifact and Chromium qualification is recorded in the excluded Task 5 report; it is not provider/client acceptance or a hosted CI run. All guide qualification records remain unchanged. Live read-only repository inspection still reports `DevShiftTeam/my-whatsapp-mcp` **private**, `main`, `has_pages: false`. Public repository setup, Pages source set to GitHub Actions, reviewed merge/publication authority, and a successful HTTPS deployment remain external gates. Independent controller review remains required before enabling or publishing the prepared workflow.
