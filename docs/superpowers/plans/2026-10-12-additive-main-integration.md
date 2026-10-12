# Additive main integration implementation plan

Date: 2026-10-12. Status: ready for implementation; no validation claimed.

Contract: [integration design](../specs/2026-10-12-additive-main-integration.md). Existing main is authoritative. Apply reviewed feature deltas with existing module imports; never replace overlapping files wholesale.

## Ownership and dependency order

Four bounded production tasks can run concurrently with disjoint file ownership. Each worker is sharing the checkout, preserves others' changes and commits only its explicit paths. A single coordinator owns final serialized validation and publication. Do not run module edits or heavy tests in parallel.

| Task | Exclusive file ownership | Deliverable and dependencies |
| --- | --- | --- |
| 1. Provider and private storage | `internal/remoteaccess/**`, new `internal/localauth/{limits,password,store}*.go`, `internal/appdata/**`, `go.mod`, `go.sum`, `THIRD_PARTY_NOTICES` | Add providers/auth/storage and tests; normalize module imports; preserve main dependencies. Publish exact DTO/API completion to tasks 2/3 before their compile checks. |
| 2. Application and CLI | `internal/app/**`, `cmd/wa-indexer/**`, `internal/clientsetup/**`, narrowly necessary `internal/mcpserver/**` | Compose existing lifecycle with managed routes, Docker bridge, health and owner-link; preserve sends/attachments/transcription. Uses task 1 APIs and task 3 NewManagedRemote. |
| 3. Owner interface | `internal/localui/**` | Add container listener policy, managed auth and wizard without replacing main UI or legacy admin; publish ManagedRemoteConfig/NewManagedRemote and DTOs first. Uses task 1 authenticator and provider DTOs. |
| 4. Packaging and docs | `Dockerfile`, `compose.yaml`, `.dockerignore`, `.env.example` if present, `.gitignore`, `.github/workflows/**`, `scripts/**`, `packaging/**`, `docs/**` excluding the two integration documents, `README.md`, `CHANGELOG.md` | Add runtime packaging and new feature guides/site; preserve existing docs/assets/workflows and release behavior. Update harness imports and tool expectations for main; wait for completed combined runtime before qualification. |

Task 1 owns the outer provider router route allowlist; task 3 owns owner realm route allowlists and assets. Task 2 supplies the actual scoped handler. Agree exact route additions once and test across both boundaries. Packaging/helper notice trees belong to task 4; top-level notices and module graph belong to task 1. Neither overwrites the other's notice inventory.

## Task 1: foundation

Port new remoteaccess package and localauth password/store/limits files with existing module import normalization. Port new appdata remote_access, remote_provider and tailscale_cache files plus their tests. Compose donor changes into existing paths/native_socket files; retain current ownership, legacy path and socket behavior. Additive new files are reusable mechanically only after checking name collisions and main API compatibility.

Merge donor dependency additions into main go.mod/go.sum; preserve main's module line and unrelated versions. Reconcile shared minimum versions deliberately and record actual resolved graph. Keep required Tailscale privacy build tags and all corresponding notices. No other worker runs go mod tidy concurrently.

Before handoff, extend provider router exact-path delegation for main send APIs and attachment downloads, preserving canonical-path/Host/Origin checks and browser-off behavior. Test bearer-path forwarding, wrong authority, encoded aliases, query rejection, missing owner auth and native-only route denial. Provider status must preserve saved authority, use bounded retries, report Cloudflare route_unverified honestly, and never substitute random hostnames.

## Task 2: application and CLI

Add donor container_options, health, owner_link, managed_mcp, owner_password, remote_access and Docker clientsetup files/tests. Add DockerRunner to copied configuration. Compose modifications to main options/main/cleanup, app/config/connections/remote_connections/mcp_listeners and templates/validation rather than replacing them. Keep existing remote_admin_config and transcription_config files and all CLI routes.

Retain main listener root/cancel, attachment service, remoteHandler publication and mcpRequestHandler. Add Managed flag to RemoteMCPConfig. Managed handler construction includes all current read/send/attachment features. Resolve attachment authority changes without stranding native handler references. Preserve cfg.Transcription and main resource reset paths.

Create default provider manager only from successful serve composition and only with the app-owned lease; offline utility commands create none. Managed factory calls task 3's NewManagedRemote, leaving legacy NewRemote and LinkOwnerRevocations unchanged. Manual remote MCP remains an explicit conflict with managed selection. Managed Claude header capability stays false unless existing manual declaration applies.

Tests include all current tools in tools/list, allowed/denied sends, permission expiry/revocation during upload, durable replay/conflict/outcome_unknown, attachments over native and managed routes, current authority after native-first startup, stale ticket denial, managed stop joining active reads/sends, archive reset and failed-shutdown lease retention, existing manual admin and transcription configuration.

## Task 3: owner UI and realm

Add donor listener_policy, remote_access, managed login and browser fixture files with symbol collision checks. Rename donor RemoteConfig/NewRemote to ManagedRemoteConfig/NewManagedRemote. Preserve main remote.go/remote_auth.go/remote_password.go APIs and tests. Explicitly deny new provider/password routes in legacy dispatch as well as managed remote dispatch; native permission cannot be inferred from a nil managed-realm pointer.

Merge Config.ContainerListen, health/serve shutdown and interface discovery into existing Server. Maintain exact native listener policy and owner bootstrap. Merge donor remote DTOs as optional interfaces. Keep main PairingState diagnostics and all existing routeMethods. Add managed everyday routes deliberately while withholding native-only operations. Coordinate outer asset/path additions with task 1.

Merge wizard HTML/JS/CSS into the current interface and adapt controls to existing theme/dropdown conventions. Distinguish native bootstrap, legacy password-admin login and managed challenge login explicitly. Test desktop/mobile logo/theme/dropdowns/Stats/sending controls and both old/new login flows, CSRF, remote secret-reveal denial and password-generation revocation.

## Task 4: packaging, harness and documentation

Copy additive donor Docker/Compose/packaging helper and site engine files; normalize Go imports in helper sources. Merge script/app build privacy tags and helper packaging into existing script behavior, preserving transcription and native model workflows. Keep main README, feature guides, assets and existing CI; add focused setup sections and Unreleased entry. Do not port unrelated donor baseline source or older roadmap documents.

Extend Docker SDK harness from its donor read-only tool expectation to the exact 12-tool full app: search_messages, get_context, list_chats, get_index_status, search_contacts, search_groups, search_attachments, get_attachment, send_message, send_media, send_voice_note and get_send_status. Preserve complete optional catalogs of 4/6/8/10/12 in the stdio validator. Three send mutations are readOnly=false/destructive=true/openWorld=true; send status is readOnly=true/destructive=false/openWorld=false. Keep annotations and schema bounds exact. Select Archive explicitly in the Docker browser fixture because main opens Stats by default. The SDK helper 64 KiB scanner and 15-second deadline do not qualify main larger-media/slow-send behavior: extend only the bounded test envelope or retain those contracts in existing Go tests, never reduce application limits. Use synthetic authorized fake-send tests for state-machine success and denial; a disconnected demo may prove tool exposure and send denial but cannot prove delivery. Cover attachment retained-original download, revocation, expiry and scope on combined image. Keep real-account WhatsApp/provider acceptance separate.

Port allowlisted website and new setup/remote/maintenance guides, then explicitly include main outbound/attachment/stats/transcription/legacy setup/client guides. Merge overlapping content with main feature truth. Adjust exact inventory/export tests to the expanded reviewed list; never relax private-output rules. Rewrite all inherited build/qualification records for combined source truth: old image numbers are historical or removed from current claims.

Use native architecture CI for image/runtime checks. Build workflows may build/validate; no tag-triggered registry push or release publication is enabled as a side effect of this integration. Pages deployment uses the authorized main/public repository flow. Preserve action pinning, least job permissions and source-bound artifacts.

## Serialized integration and acceptance

1. Coordinator snapshots task-owned source status, checks imports, formats changed Go and runs focused package checks after APIs converge. Workers do not broaden into other owners' paths to solve a transient missing-symbol state.
2. Run current main Go checks with SQLite FTS5 and required privacy tags, serial package execution where needed. Run required Python script/transcription tests and affected existing UI regression coverage. Record exact failures; do not label inherited donor results as combined-main PASS.
3. Build new minimal runtime and same-binary slim comparison if comparative claims remain. Record immutable source revision, image IDs and extracted binary hashes; mutable tags are not evidence. Run corrected browser and real SDK harness against combined image, including full main tool inventory and attachment/send consequences.
4. Measure normal unpaired serve with fresh and reused synthetic volumes, offline/network-disconnected readiness, persistence/recreation, filesystem modes, no startup acquisition, health and total SIGTERM deadline. Serialize Docker/build/browser/Go heavy work. Preserve exact owned fixture cleanup; no global prune or runtime restart.
5. Build strict site under Pages subpath; run exports/link checks and relevant desktop/mobile guide journeys. Ensure source/runtime status parity across HTML/search/Markdown and no private evidence. Native AMD64 proof follows actual hosted CI execution; local ARM64 cannot substitute.

After code/security review and required checks, the coordinator commits explicit task paths, verifies remote main has not advanced, reconciles concurrent main changes additively, and pushes the full feature integration to main. Public visibility and Pages are authorized; source publication is not a production archive restart or provider-account action. Observe hosted CI and deployed Pages nested routes/search/exports before claiming those outcomes. No separate feature preview branch is the final delivery.

## Completion receipt

Record immutable integrated main SHA, tested architectures and exact suites, image identities, main feature-regression results, hosted workflow conclusion and Pages URL/routes. Separate real provider/client/phone and native AI/history acceptance from synthetic verification. State unresolved gates precisely; do not mark all features qualified merely because documentation or HTTP health passes. Raw logs, local paths and private acceptance evidence remain outside tracked source and public site.
