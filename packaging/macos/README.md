# macOS bundle tooling

Normal text search uses the executable from `scripts/app/build.sh`; an `.app` bundle is optional.

The package scripts assemble and inspect already-built Darwin ARM64 inputs. They do not build native dependencies, download models, sign, notarize, or publish a release. Developer tooling requires Python and Apple's binary inspection tools; the delivered app does not require Python or Node.js.

## Basic unsigned candidate

Use the explicit `basic` profile for a text-first candidate without bundled inference runtimes, workers or models. It includes the already-built Go application and embedded browser UI, the pinned Cloudflared helper for optional remote access, the existing logo, LICENSE, third-party notices and helper notices. This does not remove optional voice setup or change the native-AI profiles.

Prepare an existing owner-only directory (mode `0700`) outside the checkout. Supply an **already audited** application, its exact SHA-256, the full source commit recorded for that application, and an already extracted Cloudflared executable matching `packaging/cloudflared-manifest.json`:

```sh
./scripts/package/build-basic-macos.sh \
  --application /absolute/path/to/audited/wa-indexer \
  --application-sha256 APPLICATION_SHA256 \
  --source-revision FULL_40_CHARACTER_SOURCE_COMMIT \
  --cloudflared /absolute/path/to/pinned/cloudflared \
  --output "/absolute/private/packages/My WhatsApp.new.app"
./scripts/package/verify.sh "/absolute/private/packages/My WhatsApp.new.app"
```

Alternatively supply `--cloudflared-asset /absolute/path/to/cloudflared-darwin-arm64.tgz` instead of `--cloudflared`. Exactly one is required. Both modes check the pinned executable size and SHA-256; archive mode also verifies the archive pin and safely extracts its sole executable. An arbitrary system-installed helper is not a substitute.

The helper never builds Go, downloads an asset, probes a native executable or opens an archive. It records the supplied source commit; the operator must independently establish that the audited executable came from it. Source inputs are not modified. Symlinks, shared writable paths, a wrong application hash, unpinned helper bytes, unsafe output permissions and existing outputs are refused. Temporary staging is private and removed after packaging. Set `WA_INDEXER_PACKAGE_PYTHON` to an absolute Python path if the system Python is unsuitable.

The candidate manifest is `Contents/candidate-manifest.json`, **not** the native-AI `Contents/release-manifest.json`. Every included file has a declared role, size and SHA-256. The command prints the candidate manifest's SHA-256 for an external audit record. `bundle_ready: true` only permits static assembly; `release_ready` remains `false` and `runtime_qualification` remains `NOT_RUN`. No signing, notarization, clean-machine or distribution-license qualification is implied.

The unchanged Info.plist launches `Contents/MacOS/wa-indexer` directly. Its no-argument behavior starts the local app and opens the authenticated owner window in the default browser; it does not launch Terminal. A second launch uses the existing same-profile owner handshake rather than starting another WhatsApp session. Archive, SDK, models and client settings stay outside the bundle in the existing default `WA Indexer` data directory. This is not a tray app and does not add a desktop framework. The logo is preserved as a resource; no new Dock icon is generated.

### Candidate checklist

- Record the application/source audit and printed candidate-manifest SHA-256 outside the checkout.
- Run static verification for the exact newly created bundle. Both assembly and verification require `bundle_profile: "basic"`, reject AI selections and unknown roles, and enforce hashes, private ownership, ARM64 Mach-O deployment metadata and declared native dependencies. Verification also rejects undeclared candidate files.
- Separately qualify Finder launch, default-browser onboarding, phone QR linking, chat/date choices, fresh text search, scoped MCP reads and reopening/restart persistence. Static tests do not perform or certify those journeys.
- Keep unsigned/Gatekeeper limitations and macOS 14+ ARM64-only scope explicit. Do not advertise a downloadable release, Windows/Linux qualification, universal support or optional AI quality from this candidate.

## Existing native-AI assembly

```sh
./scripts/package/build.sh AUDITED_MANIFEST INPUT_ROOT NEW_OUTPUT.app
./scripts/package/verify.sh ABSOLUTE_APP_BUNDLE
```

Use absolute paths for `INPUT_ROOT`, `NEW_OUTPUT.app`, and `ABSOLUTE_APP_BUNDLE`. The output must not already exist. Each declared artifact needs an input path, bundle-relative destination, role, size, SHA-256, executable flag, and Mach-O flag. The assembler checks required roles and bundle-relative native dependencies. Ambient `@rpath` resolution is rejected.

Omitting `bundle_profile`, or explicitly choosing `"native-ai"`, preserves the existing native-runtime requirement and encoder-worker rules. Unknown profile names are refused; there is no automatic fallback to basic. A basic manifest cannot select `ai_profiles`, `selected_encoder_profile`, native AI roles or an AI-required role. Its staged input root and output parent must be private, and its input manifest must be an owner-owned regular `0600` file. Info.plist and both manifest destinations are reserved and cannot be supplied as artifacts.

`packaging/release-manifest.json` is deliberately incomplete, so assembly with it fails. It does not describe a downloadable app release. Optional native bundles need complete runtime inputs and their license notices; an encoder bundle also requires its worker and profile artifacts.

Static verification confirms declared files and binary dependencies. It does not establish correct inference, clean-machine startup, media quality, redistribution permission, or release readiness. Run those checks for the exact artifacts before distribution.

See [optional models](../../docs/native-models.md) and [THIRD_PARTY_NOTICES](../../THIRD_PARTY_NOTICES).
