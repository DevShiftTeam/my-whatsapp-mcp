---
title: First run
description: Select the archive policy, pair privately, and inspect observed history.
---
# First run

After launching an existing [desktop executable](install/desktop.md), use the private browser owner session that the app opens. For the [Docker preview](install/docker-compose.md), keep the service running and request your private link:

```sh
docker compose exec wa-indexer wa-indexer owner-link --data-dir /var/lib/wa-indexer/data
```

Open it on the Docker host within **30 seconds**. Repeat the command if it expires or after restart.
Keep the owner link and any pairing QR code private.

## Log in as the archive owner

The local app creates its owner login link; this is separate from WhatsApp pairing and provider login. Keep the app running and open the current link on the archive computer. A stale browser tab after shutdown may need a fresh link. Do not publish the owner port or send the link to an AI client.

## Choose chats and dates before pairing

1. Open **Archive settings**. Choose **Last 30 days**, **Last 90 days**, **A date I choose**, or **All available history**. The start date is inclusive.
2. Select **All chats** or **Only chats I select**, then **Save choices**. Selective mode with no selected chats saves no message bodies. Discovery can retain chat metadata before selection.
3. Open **Manage connection → Show linking code**.
4. On your phone open **WhatsApp → Linked devices → Link a device**, then scan the QR code. Use an account you own or have permission to archive. Request a new code if it expires.
5. Watch **WhatsApp**, **History received**, and **Keyword search**. After allowed messages arrive, [search](everyday/search.md) for a known received phrase and open its context.

## Check observed coverage

A connected session and a sent history request do not establish stored messages or complete history. Check the received count and visible date range, then confirm an actual search result. **All available history** means what WhatsApp supplies to this linked device. The oldest observed message is not proof that earlier history is complete.

Bodies discarded before selecting a chat may be unavailable later. Backfill and retry behavior remains under development; expanding a filter cannot guarantee recovery. A zero count means no import has been observed, even if the connection says connected.

## Understand changes that delete data

Narrowing saved chats or moving the import date later removes stored messages outside that policy. Optional ongoing retention is separate: `0` keeps imported messages; a positive value keeps only that many days. Take a [backup](maintenance/backups.md) before deliberate policy deletion. Search filters only narrow a view and do not change retention.

[Pause import and disconnect](user-guide.md#pause-disconnect-or-delete) keep saved messages. Use [troubleshooting](troubleshooting.md) for missing coverage or reconciliation warnings. Real-account completeness and restart acceptance remain unverified.
