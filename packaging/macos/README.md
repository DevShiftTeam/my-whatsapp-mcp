# macOS bundle tooling

Normal text search uses the executable from `scripts/app/build.sh`; an `.app` bundle is optional.

The package scripts assemble and inspect already-built Darwin ARM64 inputs. They do not build native dependencies, download models, sign, notarize, or publish a release. Developer tooling requires Python and Apple's binary inspection tools; the delivered app does not require Python or Node.js.

```sh
./scripts/package/build.sh AUDITED_MANIFEST INPUT_ROOT NEW_OUTPUT.app
./scripts/package/verify.sh ABSOLUTE_APP_BUNDLE
```

Use absolute paths for `INPUT_ROOT`, `NEW_OUTPUT.app`, and `ABSOLUTE_APP_BUNDLE`. The output must not already exist. Each declared artifact needs an input path, bundle-relative destination, role, size, SHA-256, executable flag, and Mach-O flag. The assembler checks required roles and bundle-relative native dependencies. Ambient `@rpath` resolution is rejected.

`packaging/release-manifest.json` is deliberately incomplete, so assembly with it fails. It does not describe a downloadable app release. Optional native bundles need complete runtime inputs and their license notices; an encoder bundle also requires its worker and profile artifacts.

Static verification confirms declared files and binary dependencies. It does not establish correct inference, clean-machine startup, media quality, redistribution permission, or release readiness. Run those checks for the exact artifacts before distribution.

See [optional models](../../docs/native-models.md) and [THIRD_PARTY_NOTICES](../../THIRD_PARTY_NOTICES).
