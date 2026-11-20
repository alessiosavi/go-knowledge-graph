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
| `ask`            | answer a natural-language question in one call when the server-side agent is enabled |
| `search`         | text search over entity names and chunks (vector fusion when enabled)   |
| `get_entity`     | entity, aliases, `together` co-mentions (with shared chunk counts) and evidenced facts |
| `get_chunks`     | chunk text, up to 4 KiB per chunk; `truncated` marks a cut; optionally up to two neighbours |
| `traverse`       | walk fact edges up to two hops with direction/relation filters          |
| `list_documents` | documents with metadata, filterable by status and domain                           |
| `cypher`         | guarded read-only Cypher with row cap and timeout                       |

Prefer `ask` for natural-language questions. It runs the server-side agent's
retrieval loop and spends the server's LLM budget per call. Use `search`,
`get_entity`, `get_chunks`, and `traverse` for targeted lookups. To expose
`ask`, set `GKG_AGENT_ENABLED=true` and configure the agent's provider with
`GKG_AGENT_BASE_URL`, `GKG_AGENT_API_KEY`, and `GKG_AGENT_MODEL`.

Send a required `question` and optionally a `domain` slug. The response
contains Markdown `answer` and `steps` (tool-requesting model turns). Set
`include_evidence: true` to add evidence within a 32 KiB budget for one JSON
copy of the result. The SDK sends that copy as both structured and text
content. A `truncated` flag marks a shortened evidence list or item.
Shortened results can be JSON string previews. An answer that exhausts the
budget is returned in full with `truncated: true` and no evidence. A client
that sends a progress token receives one tool-name update per step. Progress
notifications are written inline, so a stalled client slows the ask. The call
uses `GKG_AGENT_TIMEOUT_S` (default 60 seconds).

By default, `get_entity` and `traverse` return typed facts that meet the assert
floor or that the judge confirmed. Set `include_low_confidence` to return all
typed facts. Entity and traversal results include `hidden` counts when any are
nonzero. The counts cover unjudged and unclear `FactCandidate` records and
`RejectedFact` audits touching the viewed entity or traversal start.
Each `(head_key, rel, tail_key)` counts once per state across chunks.
The counts are independent of read filters and exclude typed edges suppressed
by `confirmed_only` or the default low-confidence filter.
With confirmation enabled, candidates are excluded
unless `include_unverified:true` is requested and `facts.allow_unverified_reads`
is enabled (default false). Included candidates carry `unverified:true` and their
`judge` state (empty for unjudged); a disabled setting yields
`unverified_ignored:true`. Rejected audits are never returned as facts or edges.
The built-in agent's tools keep candidates hidden even when this setting is enabled.
Traversal can include candidate facts at the depth where they are found, but
does not enqueue their far endpoints for further expansion. An endpoint can
still expand when reached through a typed fact edge.

Pass `"confirmed_only": true` to `get_entity` or `traverse` to return only
judge-confirmed typed facts at any confidence. The option applies at every
traversal hop and takes precedence over `include_low_confidence` and
`include_unverified`, so candidates are excluded.

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
  `tools.gkg.*`. Set `"codemode": false` on the server to expose the
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

## A remote agent over HTTPS (your own code)

An agent you build (for example a Python service with its own model loop)
reaches gkg through the edge with its own login
([USAGE.md §5](../USAGE.md) shows how to create one). There are two ways to
plug it in:

- **One MCP `ask` call.** Connect to `https://<host>/mcp` and call `ask` with
  `{"question": "What does the graph know about Ada Lovelace?"}`. gkg's
  agent does the retrieval and returns the answer. HTTP clients can also
  use `POST /v1/agent/ask`.
- **Targeted tools over MCP.** Connect to `https://<host>/mcp` and hand gkg's
  read tools (`search`, `get_entity`, `get_chunks`, `traverse`, `list_documents`,
  `status`, `cypher`) to your model, which then does the retrieval itself.
  Every login can also `ingest`; leave that tool out if the agent should
  only read.

Python, with the official MCP SDK (`mcp` 2.x; on 1.x use
`streamablehttp_client(url, headers=...)` instead of the httpx2 client):

```python
import base64
import os

import anyio
import httpx2
from mcp import Client
from mcp.client.streamable_http import streamable_http_client
from mcp.types import TextContent

GKG_MCP = "https://<host>/mcp"
basic = base64.b64encode(f"{os.environ['GKG_LOGIN']}:{os.environ['GKG_PASSWORD']}".encode()).decode()


async def main() -> None:
    async with httpx2.AsyncClient(
        headers={"Authorization": f"Basic {basic}"},
        timeout=httpx2.Timeout(30.0, read=300.0),
    ) as http:
        async with Client(streamable_http_client(GKG_MCP, http_client=http)) as gkg:
            tools = await gkg.list_tools()  # pass these to your model, minus `ingest`
            result = await gkg.call_tool("search", {"query": "Ada Lovelace", "limit": 5})
            for block in result.content:
                if isinstance(block, TextContent):
                    print(block.text)


anyio.run(main)
```

Clients that accept a header connect the same way, e.g. Claude Code:

```sh
claude mcp add --transport http gkg https://<host>/mcp \
  --header "Authorization: Basic $(printf '%s:%s' "$GKG_LOGIN" "$GKG_PASSWORD" | base64)"
```

Hosted connectors that only support OAuth (no fixed header) cannot log in
to the edge today.

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

### Domain scope

MCP tools `ask`, `search`, `get_entity`, `get_chunks`, `traverse`,
`list_documents` and `status` accept an optional `domain` slug. Empty selects
all. Search hits, entity views, and document views include `domain`. `GET /v1/domains`
provides domain names, done-document and product counts, and whether a
product store is bound, for the web UI's search and ask pickers.

MCP ask and REST ask (JSON and stream) accept `domain` with the question.
That request scope applies to all answer tools, regardless of model arguments. Out-of-scope
entities/traversal starts are not found; chunks are `missing`; lists filter
before their limit. Cypher is not domain-enforced; the system prompt states
the scope. At v4, `get_entity` names use `EntityKey` folding (possessives,
edge punctuation), including document-scoped entities; older graphs keep
case folding. Unscoped ambiguous names return `candidates`, one per domain.
Scoped vector search over-fetches 5× then filters, which can yield fewer hits.
