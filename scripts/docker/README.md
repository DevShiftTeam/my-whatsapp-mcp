# Docker build and test tools

This is developer tooling; normal source-preview installation only uses Docker and Compose. Runtime images contain no Node, SDK harness, compiler or shell. The slim build target is a comparison image, not the recommended runtime.

Use Node 24, the exact committed docs/.vitepress lockfile (Playwright 1.64.0), Go 1.27.1 and the existing Go MCP SDK v1.8.0. Install the locked docs development dependencies and Playwright Chromium before running. Do not use the docs Playwright configuration; this directory disables screenshots, videos, traces, request artifacts and failure body output.

Build `Dockerfile` targets `runtime`, `slim` and `harness`. Supply `--build-arg SOURCE_REVISION=<40-character-commit>` for runtime/slim. Build `./scripts/docker/helper/sdk` into a private ignored path with `GOTOOLCHAIN=go1.27.1 go build`, and set `WA_DOCKER_SDK_HELPER` to its absolute path. Set `WA_DOCKER_HELPER_IMAGE` to the separately built harness image (prefer its resolved image ID).

Run `node scripts/docker/acceptance.mjs --image <prebuilt-image-reference-or-ID>`. It resolves the image once, checks its source label, and tests that exact ID without pulling or rebuilding. Fixed JSON summaries identify image ID, repository digests (empty for an unpublished local image), source revision, native image platform and harness revision. These tests do not exercise the actual Codex or Claude applications.

Run `node scripts/docker/benchmark.mjs --image <minimal-ID> --comparison <slim-ID> --samples 5`. It measures HTTP readiness and Docker health separately on five fresh and five populated synthetic volumes per image, health-command overhead, idle RSS/CPU, restart and clean shutdown. Pull/build timing is measured separately. Use a fresh `BUILD_CACHE_SCOPE` to measure an initially empty private Go cache; do not prune shared caches.

Native socket regression tests use `scripts/docker/native-test.Dockerfile` and its narrow context override. The produced race-test image runs under default seccomp, numeric UID10001, cap_drop ALL, no-new-privileges, read-only root and bounded /tmp.

Synthetic fixtures are assigned random task names and run labels; cleanup only removes that run's containers, networks and volumes. Wrong-owner/nonempty/symlink tests bind a disposable daemon-native volume directory so Docker Desktop host-share UID synthesis does not invalidate the test. No real account, user archive, global Docker credentials, daemon settings or published image is touched.

Engine 28+ is required for localhost-published-port isolation; Compose 2.20.2+ supports start_interval. The 30s health start period is a failure grace window, not a delay. All persistent writes use the single private volume; /tmp is bounded transient scratch. Check the 10-second stop limit with the version and archive size you intend to run.

## Native CI validation

`docker.yml` calls `docker-native.yml` separately on native `ubuntu-24.04` and `ubuntu-24.04-arm`. Both use a clean immutable checkout, exact Node/Go/Buildx versions and digest-pinned BuildKit, runtime bases, SBOM generator and Skopeo from `packaging/docker-images.json`. The host kernel, Docker daemon and requested platform must agree. PR jobs have only `contents: read`; they do not log into a registry or receive provider credentials. Both acceptance and five-sample fresh/reused-volume benchmarks must pass before artifact upload. No comparable hosted-runner performance baseline exists, so the evidence explicitly leaves that comparison unqualified.

BuildKit exports **one attested OCI archive**. `oci.mjs` checks descriptor sizes/hashes, all layers and uncompressed diff IDs, config/source/platform labels, provenance build revision, SPDX content and attestation subjects. `ci.mjs load` uses pinned Skopeo to select just the runnable platform into a Docker archive for local loading. That conversion intentionally omits attestations from the Docker-only copy; the original attested archive is preserved unchanged. The loaded/tested Docker config ID must equal the original OCI runnable config digest. Acceptance, benchmark, original archive checksum, full graph, source/harness revision, run/attempt and actual runner/tool versions are bound in `evidence.json`.

The workflows test images without publishing them. They have no registry login or package-write permission. Confirm both architecture jobs pass for the intended commit before describing an image as tested.

Publisher-helper pulls report fixed failure codes for unavailable manifests, denied access, rate limiting, network/TLS failures or an unclassified pull failure. Classification examines only a bounded stderr tail and never prints registry output, credential values or private paths. These diagnostic hints do not establish registry availability or authorize replacing a digest: retain the pinned helper and version check until any replacement has been independently reviewed. Pull failures stop loading before acceptance; they do not qualify the built image.

Run `node --test scripts/docker/tests/*.test.mjs` for the focused policy/graph tests. Run `node scripts/docker/tests/helper-cleanup.integration.mjs` explicitly with a separate Docker configuration without registry credentials to prove a timed-out helper is actually removed. `node scripts/docker/local-copy-proof.mjs PATH_TO_ATTESTED_OCI_ARCHIVE` performs a local-only native import/registry/copy-back proof. It creates exact randomly named containers, one internal network and one staging volume and removes them in `finally`; it never contacts an external registry for publication. Supply a private canonical `TMPDIR` for retained proof files. The local staging volume avoids Docker Desktop bind-share metadata/write behavior; native Linux CI uses private directories. These local checks do not test hosted runners, actual AI clients, remote providers, models, or real WhatsApp history.

## Read/send contract

The combined app exposes exactly twelve tools: eight archive reads, three send tools and read-only send status. The browser fixture selects Archive because the app opens Stats. Demo remains disconnected, so send calls must be denied even if an owner grants synthetic permissions. Existing Go fake-connector tests separately cover success, journal replay/conflict, large media and delayed-send deadlines. These are not real delivery proof. The SDK driver uses a bounded 12 MiB line envelope and 65-second send deadline; product limits remain 8 MiB decoded and 60-second sends.
