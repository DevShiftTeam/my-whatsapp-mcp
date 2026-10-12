# Historical snapshot implementation plan

Spec: historical-snapshot-spec.md

Ownership: one source implementation owner in an isolated checkout at the deployed base. Runtime preparation, private authorization comparison, activation and rollback belong to the runtime owner. Packaging integration belongs to the parent task. No real user data is inspected by implementation.

## Task 1: storage and marker boundary
Write synthetic storage/marker rejection and unchanged-file tests, then implement an explicit query-only archive constructor and unconditional write admission denial. Preserve safety fences, current expiry and exact schema checks.

## Task 2: application composition and client publication
Write constructor escape/mutation tests. Construct keyword-only resources without runtime owners. Verify previously ready registries without journal writes. Deny send/fetch before durable/provider work. Add trusted capture configuration and CLI validation.

## Task 3: HTTP and presentation
Add deny-default route gate after existing authentication/origin/CSRF checks. Render persistent safe capture metadata in native and legacy HTML, status and MCP instructions. Preserve all normal paths.

## Task 4: qualification
Run focused synthetic tests, twice self-review security and lifecycle boundaries, report exact evidence and residual limitations, then request parent review and commit/index slot. Heavy test/build execution is serialized by the parent.
