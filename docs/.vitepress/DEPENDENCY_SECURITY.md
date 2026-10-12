# Documentation build-tool security

The documentation uses pinned VitePress 1.6.4 and locked dependencies. The Node toolchain is used to build static files; it is not part of the archive runtime or the published website.

## Known advisories

The locked installation reports seven findings: six moderate and one high. This is not a clean dependency audit. The findings include these dependency chains:

| Package | Installed version | Affected area |
| --- | --- | --- |
| VitePress | 1.6.4 | Inherits Vite development-server advisories |
| Vite | 5.4.21 | Development-server path handling and Windows editor middleware |
| esbuild | 0.21.5 | Development-server CORS |
| gray-matter | 4.0.3 | Inherits a formatter advisory through js-yaml and argparse |
| js-yaml | 3.15.2 | Dependency chain to argparse |
| argparse | 1.0.10 | Dependency chain to sprintf-js |
| sprintf-js | 1.0.3 | Unbounded formatting precision |

Primary advisories:

- [Vite optimized-dependency path traversal](https://github.com/advisories/GHSA-4w7w-66w2-5vf9)
- [Vite Windows editor credential disclosure](https://github.com/advisories/GHSA-v6wh-96g9-6wx3)
- [Vite Windows filesystem-deny bypass](https://github.com/advisories/GHSA-fx2h-pf6j-xcff)
- [esbuild development-server CORS](https://github.com/advisories/GHSA-67mh-4wv8-2f99)
- [sprintf-js formatting denial of service](https://github.com/advisories/GHSA-hp3w-g68c-fv3c)

## Build and preview restrictions

Use `npm run build` for static output. Use `npm run preview` for the separate Node static-file server, which binds to `127.0.0.1` and serves only approved built files.

Do not replace it with a Vite or VitePress development or preview server. Loopback binding alone does not remove the development-server advisory risks. The current build uses Vite and esbuild build operations, not their HTTP serving features. gray-matter uses the safe YAML loader and dumper; executable frontmatter and the js-yaml CLI formatter are not used.

The output checker validates pages, links, search entries, Markdown exports, and allowed assets. It rejects private paths, credential patterns, extra files, and mismatched exports. Static output contains neither the Node build tools nor the archive database.

## Dependency changes

Run `npm run check:dependencies` before publishing and after changing the lockfile. It reports the known findings and rejects new affected packages, advisories, severities, or unexpected stable VitePress versions. Keep exact dependency pins and review advisory reachability when commands or workflows change.

Prefer a supported stable patched baseline when available, then test preparation, exports, static build, and browser behavior. Do not use forced upgrades or unsupported overrides merely to suppress an audit finding.

The GitHub Pages workflow builds with read-only repository permissions. Only the deployment job receives Pages write permission. Docker images and archive execution do not use the documentation dependencies.
