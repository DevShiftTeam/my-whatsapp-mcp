# Documentation build-tool security

The documentation uses pinned VitePress 1.6.4 and locked dependencies. The Node toolchain is used to build static files; it is not part of the archive runtime or the published website.

## Known advisories

The locked installation was reviewed on October 29, 2026 and reports three findings: two moderate and one high. This is not a clean dependency audit. The findings are limited to these build-tool chains:

| Package | Installed version | Affected area |
| --- | --- | --- |
| VitePress | 1.6.4 | Inherits Vite development-server advisories |
| Vite | 5.4.21 | Development-server path handling and Windows editor middleware |
| esbuild | 0.21.5 | Development-server CORS |

Primary advisories:

- [Vite optimized-dependency path traversal](https://github.com/advisories/GHSA-4w7w-66w2-5vf9)
- [Vite Windows editor credential disclosure](https://github.com/advisories/GHSA-v6wh-96g9-6wx3)
- [Vite Windows filesystem-deny bypass](https://github.com/advisories/GHSA-fx2h-pf6j-xcff)
- [esbuild development-server CORS](https://github.com/advisories/GHSA-67mh-4wv8-2f99)
- [Windows editor argument escaping](https://github.com/vitejs/launch-editor/security/advisories/GHSA-698v-4r3x-3vcv)
- [Editor command-line flag handling](https://github.com/vitejs/launch-editor/security/advisories/GHSA-r825-w8mm-5pxw)
- [Windows editor path checks](https://github.com/vitejs/launch-editor/security/advisories/GHSA-587h-7wmj-h5pm)
- [Vite custom development middleware](https://github.com/vitejs/vite/security/advisories/GHSA-9jrq-w75r-8gcw)
- [Vite development-server file scope](https://github.com/vitejs/vite/security/advisories/GHSA-rq7h-c2jc-7f22)

The direct Markdown-it dependency used by exports and output validation is pinned to patched 14.3.3 for the [inline HTML parsing](https://github.com/markdown-it/markdown-it/security/advisories/GHSA-vfgh-jcmw-g37q) and [multiline reference parsing](https://github.com/markdown-it/markdown-it/security/advisories/GHSA-gfr2-rv44-r23c) denial-of-service advisories. The dependency check continues to reject these findings for the direct package.

VitePress 1.6.4 also bundles its own Markdown-it renderer with HTML enabled. Updating the direct dependency does not patch that bundled copy, and npm audit does not report it separately. Both parser issues remain a build-time availability risk until a supported patched VitePress baseline is qualified. Its inputs are the staged repository guides, never HTTP submissions. Crafted pull-request content can still consume CI build time; documentation jobs have a 15-minute timeout, pull-request builds receive read-only repository permissions, and deployment runs only from `main`. This residual bundled-parser risk is outside the three npm findings above.

## Build and preview restrictions

Use `npm run build` for static output. Use `npm run preview` for the separate Node static-file server, which binds to `127.0.0.1` and serves only approved built files.

Do not replace it with a Vite or VitePress development or preview server. Loopback binding alone does not remove the development-server advisory risks. The current build uses Vite and esbuild build operations, not their HTTP serving features. gray-matter uses the safe YAML loader and dumper; executable frontmatter and the js-yaml CLI formatter are not used.

The editor advisories require invoking editor-launch code, and the Vite request-handling advisories require a development server or its custom middleware. The supported build and separate static preview start none of these paths. This disposition does not authorize those development features or claim that the installed Vite package is patched.

The output checker validates pages, links, search entries, Markdown exports, and allowed assets. It rejects private paths, credential patterns, extra files, and mismatched exports. Static output contains neither the Node build tools nor the archive database.

## Dependency changes

Run `npm run check:dependencies` before publishing and after changing the lockfile. It reports the known findings and rejects new affected packages, advisories, severities, or unexpected stable VitePress versions. Keep exact dependency pins and review advisory reachability when commands or workflows change.

Prefer a supported stable patched baseline when available, then test preparation, exports, static build, and browser behavior. Do not use forced upgrades or unsupported overrides merely to suppress an audit finding.

The GitHub Pages workflow builds with read-only repository permissions. Only the deployment job receives Pages write permission. Docker images and archive execution do not use the documentation dependencies.
