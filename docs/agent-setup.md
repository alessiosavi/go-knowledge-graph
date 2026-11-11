# Connecting an agent to gkg (MCP)

`gkg` is an MCP server with two transports:

- **stdio**: `gkg mcp` — the agent host starts the process and speaks JSON-RPC
  over stdio. Recommended for local agents.
- **streamable HTTP**: `POST /mcp` on the `serve` surface — for remote or
  shared deployments. Set `GKG_API_KEY` to require
  `Authorization: Bearer <key>` (the health probes stay exempt). Through the
  edge (`https://<host>/mcp`) clients send the edge password instead:
  `Authorization: Basic <base64 of login:password>`; the edge swaps it for the
  internal bearer. Keep the credential in an environment variable, not in a
  shared config file.

The tool set is the same on both transports:

| Tool             | Purpose                                                                 |
| ---------------- | ----------------------------------------------------------------------- |
| `ingest`         | submit a URL or inline text; returns the document id                    |
| `status`         | pipeline status of one document, or the most recent documents           |
| `search`         | text search over entity names and chunks (vector fusion when enabled)   |
| `get_entity`     | entity, aliases, `together` co-mentions (with shared chunk counts) and evidenced facts |
| `traverse`       | walk fact edges up to two hops with direction/relation filters          |
| `list_documents` | documents with metadata, filterable by status                           |
| `cypher`         | guarded read-only Cypher with row cap and timeout                       |

Low-confidence facts are excluded unless the tool call opts in with
`include_low_confidence`.

## Environment

`gkg mcp` reads the same configuration as the server: `GKG_MEMGRAPH_URI`,
`GKG_MEMGRAPH_USER`, `GKG_MEMGRAPH_PASSWORD`, the extractor settings and so
on (see `.env.example`). When connecting to the compose stack from the
host, the Memgraph port is `127.0.0.1:${MEMGRAPH_PORT:-7687}` and the
credentials are the ones in `.env`.

PDF/Office inputs convert through the stack's docling on `localhost:5001`
by default; set `GKG_CONVERTER_MODE=off` when running the binary without
the stack.

## OpenCode (v2 config)

Add to the project's `opencode.jsonc` (or the global config). Under V2,
servers live under `mcp.servers` and secrets stay in the environment:

```jsonc
{
  "$schema": "https://opencode.ai/config.json",
  "mcp": {
    "servers": {
      "gkg": {
        "type": "local",
        "command": ["gkg", "mcp"],
        "environment": {
          "GKG_MEMGRAPH_URI": "bolt://localhost:7687",
          "GKG_MEMGRAPH_USER": "gkg",
          "GKG_MEMGRAPH_PASSWORD": "{env:GKG_MEMGRAPH_PASSWORD}",
          "GKG_EXTRACTOR_MODE": "openai", // override the sidecar default
          "GKG_EXTRACTOR_OPENAI_API_KEY": "{env:OPENAI_API_KEY}",
        },
      },
    },
  },
}
```

Or from the CLI:

```sh
opencode mcp add gkg --env GKG_MEMGRAPH_URI=bolt://localhost:7687 \
  --env GKG_MEMGRAPH_USER=gkg --env GKG_MEMGRAPH_PASSWORD={env:GKG_MEMGRAPH_PASSWORD} \
  -- gkg mcp
opencode mcp list    # ✓ gkg connected
```

Notes:

- Code Mode is the default in OpenCode V2: the tools are reachable through
  `tools.gkg.*`. Set `"codemode": false` on the server to expose the seven
  tools directly.
- **Streamable HTTP instead of stdio** (e.g. the compose stack):

  ```jsonc
  {
    "gkg": {
      "type": "remote",
      "url": "http://localhost:8080/mcp",
      "oauth": false,
      "headers": { "Authorization": "Bearer {env:GKG_API_KEY}" },
    },
  }
  ```

## Claude Desktop

Edit `claude_desktop_config.json` (Settings → Developer → Edit Config).
The `GKG_EXTRACTOR_MODE=openai` setting below overrides the sidecar default:

```json
{
  "mcpServers": {
    "gkg": {
      "command": "gkg",
      "args": ["mcp"],
      "env": {
        "GKG_MEMGRAPH_URI": "bolt://localhost:7687",
        "GKG_MEMGRAPH_USER": "gkg",
        "GKG_MEMGRAPH_PASSWORD": "<from .env>",
        "GKG_EXTRACTOR_MODE": "openai",
        "GKG_EXTRACTOR_OPENAI_API_KEY": "<your key>"
      }
    }
  }
}
```

## Example prompts

- "Ingest `https://en.wikipedia.org/wiki/Knowledge_graph` and tell me when
  it is done."
- "Search for 'Memgraph' and summarise the top chunks with their sources."
- "Show everything the graph knows about Ada Lovelace, with citations."
- "Traverse from the entity `ada lovelace` two hops, relations
  `wrote` and `located_in`, and list the evidence excerpts."
- "Run read-only Cypher: `MATCH (d:Document) RETURN d.id, d.status`."

## Verifying the connection

The MCP smoke check used by `scripts/acceptance.sh` drives a real client
over stdio:

```sh
go run ./scripts/mcpsmoke -query "your marker" -- gkg mcp
```

The design's acceptance criterion also asks for one real desktop agent
client: OpenCode or Claude Desktop with the configuration above, running
`search` and `get_entity`. The script covers the automated slice (stdio
`tools/list` + `search`); the desktop-client run stays a manual step
outside the script.
