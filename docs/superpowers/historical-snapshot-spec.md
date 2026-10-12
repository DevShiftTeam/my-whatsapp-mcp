# Historical read-only snapshot

An explicitly selected, separately prepared historical profile serves saved keyword reads, Stats, scoped MCP reads, contacts, group and attachment metadata. The capture time is verified by its operator; newer data is excluded. Current wall-clock source and credential expiry applies. Current authorization equivalence must be privately verified before sealing; a historical grant never restores revoked or narrowed access.

The private marker is required and prevents ordinary archive opening and mutation utilities. Snapshot construction creates no connector, source observer, writer, migration, recovery, model worker, download or transcription job. Startup validates exact schema, safety and ready client journals without repairing them. All application mutations and unlisted HTTP routes fail closed. Owner authentication and ephemeral transport controls retain existing origin and CSRF rules.

Both owner interfaces and MCP identify historical capture, partial coverage, unknown current phone state, and disabled sending/syncing. Original delivery is unavailable because ordinary delivery writes durable pins; metadata and existing transcript retrieval remain available. Optional transcription readiness may use service metadata only.

Normal-mode capabilities and contracts are unchanged. This is a temporary serving mode, not reconciliation or a fresh WhatsApp backup.

## Trusted preparation and launch contract

After recovery and private current-authorization equivalence checks, the operator supplies `archive/historical-snapshot.json` within the separate profile: a private owned regular single-link file (0600, at most 2048 bytes). The exact JSON schema is:

```json
{"version":1,"captured_at":"2026-01-02T03:04:05Z","authorization_verified_at":"2026-01-02T04:04:05Z","current_authorization_verified":true}
```

These are synthetic examples, not a capture claim. `captured_at` must be the verified historical capture time; verification must be no earlier than capture and no later than startup. Unknown keys, trailing JSON, missing/false attestations and mismatched capture times fail closed. The attestation is an operator trust boundary, not an automated comparison against another profile. Repeat equivalence validation before each activation; stop the snapshot if current authorization changes. No tool in this mode silently creates or updates the marker.

Launch the existing `serve` command with explicit absolute `--data-dir` and `--historical-snapshot-at RFC3339`. Existing MCP/admin/voice-service configuration flags remain usable; demo, open-existing, and original-budget changes are incompatible. Recover and rebind any copied native transport configuration before sealing. Snapshot startup neither requires nor opens the session database. MCP authentication stays bearer/native scoped; owner cookies remain separate.

Sending tools remain discoverable for protocol compatibility and fail with `historical_snapshot_read_only` before send backend work. Attachment search and metadata remain available; original delivery, including retained files and fetch-if-needed, reports unavailable with that code. Existing transcripts remain searchable. Model catalog/state remains inspectable; installation state is unknown unless independently inspected. An optional service readiness result means metadata identity was verified, not that any recording was processed.

The Stats connection control is disabled in historical mode and states that syncing is disabled; it cannot offer or invoke Resume import. Normal paused archives retain their Resume import action.

Managed provider restoration and setup are disabled in historical mode, including provider changes, owner password configuration, and managed MCP publication. Existing explicit manual MCP/admin transport remains available with current authorization checks; normal serving retains managed setup.
