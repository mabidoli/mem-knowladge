# Universal Memory System (UMS)

A persistent, semantic memory layer for LLM agents using Neo4j graph-vector storage and the Model Context Protocol (MCP).

## Features

- **Hybrid Storage**: Neo4j for both graph relationships and vector embeddings
- **Local-First Embeddings**: Uses Ollama with `nomic-embed-text` by default (zero API costs)
- **Optional OpenAI**: Supports OpenAI embeddings with automatic fallback
- **Time-Decay Retrieval**: Balances semantic relevance with recency
- **MCP Integration**: Standard protocol for LLM tool integration
- **Embedding Cache**: LRU cache reduces redundant API calls

## Quick Start

### Prerequisites

- Docker & Docker Compose
- Go 1.23+
- (Optional) Ollama installed locally

### 1. Start Infrastructure

```bash
cd ums-server
make docker-up
```

This starts Neo4j and Ollama, and pulls the `nomic-embed-text` model.

### 2. Build and Run

```bash
make build
./bin/ums
```

### 3. Configure Claude Desktop

Add to `~/Library/Application Support/Claude/claude_desktop_config.json`:

```json
{
  "mcpServers": {
    "memory": {
      "command": "/path/to/ums-server/bin/ums",
      "env": {
        "NEO4J_URI": "bolt://localhost:7687",
        "NEO4J_USER": "neo4j",
        "NEO4J_PASSWORD": "password"
      }
    }
  }
}
```

## Configuration

| Environment Variable | Default | Description |
|---------------------|---------|-------------|
| `NEO4J_URI` | `bolt://localhost:7687` | Neo4j connection URI |
| `NEO4J_USER` | `neo4j` | Neo4j username |
| `NEO4J_PASSWORD` | `password` | Neo4j password |
| `EMBEDDING_PROVIDER` | `ollama` | `ollama` or `openai` |
| `OLLAMA_URL` | `http://localhost:11434` | Ollama API endpoint |
| `OLLAMA_MODEL` | `nomic-embed-text` | Ollama embedding model |
| `OPENAI_API_KEY` | - | Required if using OpenAI |

## MCP Tools

### save_memory

Store a new memory with semantic embedding.

```json
{
  "content": "The user prefers dark mode interfaces",
  "importance": 7,
  "tags": ["preferences", "ui"]
}
```

### recall_memory

Retrieve relevant memories based on semantic similarity and recency.

```json
{
  "query": "What does the user prefer for UI?",
  "limit": 5
}
```

### forget_memory

Soft-delete a memory by ID.

```json
{
  "memory_id": "mem_1234567890"
}
```

## Architecture

```
┌─────────────────┐     ┌─────────────────┐
│   LLM Agent     │────▶│   MCP Server    │
│  (Claude, etc)  │     │   (Go/stdio)    │
└─────────────────┘     └────────┬────────┘
                                 │
                    ┌────────────┴────────────┐
                    │                         │
              ┌─────▼─────┐            ┌──────▼──────┐
              │  Ollama   │            │   Neo4j     │
              │ Embeddings│            │ Graph+Vector│
              └───────────┘            └─────────────┘
```

## Development

```bash
# Run tests
make test

# Lint code
make lint

# Start with monitoring (Prometheus + Grafana)
make docker-up-full
```

## License

MIT
