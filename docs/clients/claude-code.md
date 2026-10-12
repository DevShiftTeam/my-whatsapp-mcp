---
title: Claude Code
description: Connect Claude Code to your local or remote archive.
---
# Claude Code

In My WhatsApp **AI connections**, create a Claude Code connection and choose its chats, date range, and expiry. Copy the generated configuration. New connections are read-only.

## Local stdio

Keep My WhatsApp running on the same computer. Save the generated JSON in a file such as `wa-indexer.mcp.json`:

```json
{
  "mcpServers": {
    "wa_indexer": {
      "type": "stdio",
      "command": "/absolute/path/to/wa-indexer",
      "args": ["mcp", "stdio", "--config", "/absolute/private/path/native-client.json"]
    }
  }
}
```

Use the exact generated absolute paths. The executable and each argument are separate values, so paths with spaces or Hebrew characters work without shell quoting. Paths containing `${...}` are not supported because Claude Code expands that syntax.

My WhatsApp manages the private credential file. Copy its path, not its contents. No port or token environment variable is needed.

## Docker stdio

For a [Docker installation](../install/docker-compose.md), create a local Claude Code connection and copy its generated configuration:

```json
{
  "mcpServers": {
    "wa_indexer": {
      "type": "stdio",
      "command": "docker",
      "args": ["exec", "-i", "wa-indexer", "/usr/local/bin/wa-indexer", "mcp", "stdio", "--config", "/var/lib/wa-indexer/data/clients/native-client.json"]
    }
  }
}
```

Use the generated file path and your container name. Keep `-i`; do not add `-t`. The container must already be running, and Claude Code must have access to Docker. The credentials stay inside the private volume.

## Remote bearer

Set up [remote access](../remote/index.md), then create a remote Claude Code credential. Use the saved HTTPS endpoint ending in `/mcp`:

```json
{
  "mcpServers": {
    "wa_indexer": {
      "type": "http",
      "url": "https://bearer.example.com/mcp",
      "headers": {"Authorization": "Bearer ${WA_INDEXER_CLIENT_TOKEN}"}
    }
  }
}
```

Set `WA_INDEXER_CLIENT_TOKEN` to the raw My WhatsApp token in the environment of the Claude Code process. The JSON supplies the `Bearer ` prefix. Use a dedicated variable, not an Anthropic account key or other service credential. Keep `type: "http"` in the entry.

Load your selected JSON file with:

```sh
claude --mcp-config './wa-indexer.mcp.json' --strict-mcp-config
```

The managed setup does not provide OAuth. Use bearer authentication for this route.

## Check access and revoke it

Ask Claude Code to find a harmless phrase in a permitted chat and open its context. Check that excluded chats stay hidden. After changing permissions, repeat the query to confirm the change.

Revoke the connection in My WhatsApp before removing its client configuration. See [MCP tools](../mcp.md) and [sending permissions](outbound-messaging.md).

Successful calls in the actual Claude Code application have not yet been confirmed. See [compatibility](../compatibility.md) and the [official Claude Code MCP guide](https://code.claude.com/docs/en/mcp).
