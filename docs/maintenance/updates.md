---
title: Updates
description: Back up first and replace the application while keeping its private volume.
---
# Updates

A versioned downloadable app/container release remains planned. The [source-built Docker preview](../install/docker-compose.md) can be rebuilt while retaining the same volume. Record the current image/version, source revision, port, container name and launch settings, then make a [full stopped-writer backup](backups.md) before changing the app.

## Docker source preview

Review the selected source and migration notes, then use the same checkout, Compose project, `.env` and storage settings:

```sh
docker compose -f compose.yaml -f compose.build.yaml up -d --build
docker compose ps
docker compose exec wa-indexer wa-indexer owner-link --data-dir /var/lib/wa-indexer/data
```

The build replaces the app image; the named volume persists. Combined-source rebuild/recreation and persistence qualification is pending. Verify synthetic policy, archive, session files, client grants/private configurations and model storage before relying on a new image. Such tests do not prove compatibility of a future migration or a real WhatsApp session. Never delete storage, prune volumes or narrow archive policy to perform an update.

When a qualified versioned release becomes available, back up first, select its exact image in the release Compose file, run `docker compose pull`, then `docker compose up -d`. There is no public release image to pull today. Retain the same project/volume and do not use `latest`.

## Native development executable

Stop the app and any service/helper using its root, back up the full root, replace the executable with the selected build and relaunch with the same data root/settings. Never run two writers or app versions on one root.

## Verify and recover

Repeat owner-link for a new browser session; links expire in 30 seconds. Confirm saved choices, known search results, retained media/models/configuration and client grants. Test the exact AI client if you rely on it. Connection is separate from observed history coverage.

Treat migrations as potentially irreversible. If an update fails, stop all writers, preserve the changed volume separately and [restore the full backup to a new empty volume](backups.md#docker-compose-restore) using its recorded image/version. Do not downgrade a migrated database in place or copy only one SQLite file. Keep original and restored WhatsApp sessions stopped until you deliberately choose which one to run.
