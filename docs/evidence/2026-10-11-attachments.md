# Attachment qualification — 2026-10-11

Source: `704b74a996b4b3d854e1253d564f7a8026dedf16`. Archive schema: **14**. This record covers synthetic qualification and one owner-authorized live attachment case.

## Synthetic qualification

CI run `38167207756` passed on macOS and Linux, including Go build, vet and tests, plus UI, Python and script checks. Focused document, attachment, lifecycle and race checks also passed. Code and security reviews approved the 69-file change.

Fixtures cover scoped metadata and neighboring-message search, pagination, credential and source withdrawal, expiry, retained-byte checks, bounded download tickets, owner-only historical document recovery, and MCP/stdio forwarding. These results are separate from the live provider and client checks below.

## Live transport and recovery

Both direct remote MCP and the existing active Composio account found and retrieved the same approved archived PDF. Each transfer returned **1,385,386 bytes**, matched the retained original's SHA-256 evidence, and had a valid PDF header. Search also returned four authorized neighboring messages.

At verification, the archive was connected, unpaused and safe. The existing Composio credential was retained. During transfer verification, the operator sent no messages, created no clients and invoked no owner document-retry requests.

Before that verification, the primary phone returned three document-recovery responses. One eligible historical placeholder became a document with a retained original; the other two responses had no payload. Request acknowledgements alone did not establish recovery.

## Local PDF text check and limits

A separate local Poppler check extracted nonempty text from the verified downloaded PDF. No PDF body text was indexed by the app, and no AI host's interpretation of the file was verified.

This proves attachment lookup and byte transport for the tested source and clients. It does not establish complete attachment history, availability of other files, or accuracy of an AI summary. Names, source IDs, filenames, private paths, credentials and download URLs are omitted from this record.
