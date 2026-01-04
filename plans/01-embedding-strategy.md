# Improvement Area 1: Embedding Model Strategy

## Problem Statement

The original architecture document assumes embeddings will be generated but does not specify:
1. Which embedding model to use
2. How to handle embedding model upgrades/versioning
3. Latency and cost implications
4. Offline/local fallback strategies

This is a **HIGH SEVERITY** gap that must be addressed before implementation.

---

## Questions to Address

### 1. Model Selection

| Model | Dimensions | Cost (per 1M tokens) | Latency | Quality |
|-------|-----------|---------------------|---------|---------|
| OpenAI `text-embedding-3-small` | 1536 | $0.02 | 100-200ms | Good |
| OpenAI `text-embedding-3-large` | 3072 | $0.13 | 150-300ms | Better |
| Cohere `embed-english-v3.0` | 1024 | $0.10 | 100-200ms | Good |
| Voyage AI `voyage-large-2` | 1536 | $0.12 | 100-200ms | Excellent |
| Local: `nomic-embed-text` | 768 | Free | 20-50ms | Good |
| Local: `bge-large-en-v1.5` | 1024 | Free | 30-60ms | Very Good |

**Decision needed:** Which model(s) should be the default? Should we support multiple?

### 2. Embedding Versioning Problem

When upgrading embedding models:
- Old vectors become incompatible with new vectors
- Cosine similarity across different model versions is meaningless
- Full re-embedding of all memories is expensive

**Proposed solutions:**
1. Store `embedding_model_version` on each Memory node
2. Implement lazy re-embedding on retrieval
3. Batch re-embedding migration script
4. Separate vector indexes per model version

### 3. Cost Projection

For a "chatty" agent creating 100 memories/day:
- Average memory size: 500 tokens
- Daily embedding cost: 50K tokens × $0.02/1M = $0.001/day
- Monthly: ~$0.03

For enterprise scale (1000 agents, 1000 memories/day each):
- Daily: 500M tokens × $0.02/1M = $10/day
- Monthly: ~$300

**Decision needed:** Is API cost acceptable, or should local models be prioritized?

### 4. Latency Budget

Current budget (from review):
| Operation | Target | With OpenAI API |
|-----------|--------|-----------------|
| Total recall_memory | <100ms | 150-350ms ❌ |

The embedding API call dominates latency.

---

## Proposed Implementation

### Option A: API-First with Local Fallback

```go
type EmbeddingProvider interface {
    Embed(ctx context.Context, text string) ([]float32, error)
    ModelName() string
    Dimensions() int
}

type EmbeddingService struct {
    primary   EmbeddingProvider  // OpenAI
    fallback  EmbeddingProvider  // Ollama/local
    cache     *lru.Cache         // LRU cache for repeated queries
}

func (s *EmbeddingService) Embed(ctx context.Context, text string) ([]float32, error) {
    // Check cache first
    if cached, ok := s.cache.Get(hashText(text)); ok {
        return cached.([]float32), nil
    }

    // Try primary
    vec, err := s.primary.Embed(ctx, text)
    if err != nil {
        // Fallback to local
        vec, err = s.fallback.Embed(ctx, text)
    }

    if err == nil {
        s.cache.Add(hashText(text), vec)
    }
    return vec, err
}
```

### Option B: Local-First for Privacy

Use Ollama with `nomic-embed-text` by default:
- Zero API costs
- <50ms latency
- Full privacy (no data leaves machine)
- Trade-off: Slightly lower quality embeddings

### Option C: Hybrid with Query Classification

```go
func (s *EmbeddingService) Embed(ctx context.Context, text string, importance int) ([]float32, error) {
    if importance >= 8 {
        // High importance: use best model
        return s.openAI.Embed(ctx, text)
    }
    // Normal: use fast local model
    return s.ollama.Embed(ctx, text)
}
```

---

## Migration Strategy for Model Upgrades

### Schema Addition

```cypher
// Add model version tracking
MATCH (m:Memory)
WHERE m.embeddingModel IS NULL
SET m.embeddingModel = 'text-embedding-3-small-v1'
```

### Separate Index per Model

```cypher
// Index for v1 embeddings
CREATE VECTOR INDEX `memory_embeddings_v1`
FOR (n:Memory) ON (n.embedding)
OPTIONS {indexConfig: {
  `vector.dimensions`: 1536,
  `vector.similarity_function`: 'cosine'
}}

// When migrating to v2 (different dimensions)
CREATE VECTOR INDEX `memory_embeddings_v2`
FOR (n:Memory) ON (n.embedding_v2)
OPTIONS {indexConfig: {
  `vector.dimensions`: 3072,
  `vector.similarity_function`: 'cosine'
}}
```

### Lazy Re-embedding

```go
func (s *MemoryService) Recall(ctx context.Context, query string) ([]Memory, error) {
    queryVec, _ := s.embedder.Embed(ctx, query)
    currentModel := s.embedder.ModelName()

    memories, _ := s.neo4j.VectorSearch(queryVec)

    // Check if any memories need re-embedding
    for i, mem := range memories {
        if mem.EmbeddingModel != currentModel {
            // Re-embed in background, don't block retrieval
            go s.reembedMemory(mem.ID)
        }
    }

    return memories, nil
}
```

---

## Recommended Decision

**For Phase 1:** Start with Option B (Local-First)
- Use Ollama + `nomic-embed-text` (768 dimensions)
- Zero cost, low latency, full privacy
- Adjust Neo4j index to 768 dimensions

**For Phase 2:** Add Option A capabilities
- Support OpenAI as optional upgrade
- Implement caching layer
- Add model versioning to schema

**For Phase 3:** Implement Option C
- Importance-based model routing
- Full migration tooling

---

## Action Items

- [x] Decide on default embedding model → **Ollama/nomic-embed-text (768d)**
- [x] Update Neo4j vector index dimensions to match chosen model → **768d in schema**
- [x] Implement `EmbeddingProvider` interface → **`internal/embedding/provider.go`**
- [x] Add Ollama integration for local embeddings → **`internal/embedding/ollama.go`**
- [x] Design caching strategy for repeated queries → **LRU cache in Service**
- [x] Add `embeddingModel` field to Memory node schema → **`internal/storage/neo4j.go`**
- [ ] Create migration script for model upgrades
- [ ] Benchmark latency with chosen model(s)
- [ ] Document cost projections for different scale scenarios

---

## References

- [OpenAI Embeddings Documentation](https://platform.openai.com/docs/guides/embeddings)
- [Ollama Embedding Models](https://ollama.ai/library)
- [MTEB Leaderboard](https://huggingface.co/spaces/mteb/leaderboard) - Embedding quality benchmarks
- [Nomic Embed Text](https://huggingface.co/nomic-ai/nomic-embed-text-v1.5)
