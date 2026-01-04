# Improvement Area 2: Memory Chunking & Summarization Strategy

## Problem Statement

The original architecture stores raw `content` without addressing:
1. What constitutes an optimal "memory unit"?
2. How to handle long conversations without context pollution?
3. No hierarchical memory structure (episodic vs semantic)
4. No summarization or consolidation pipeline

This is a **HIGH SEVERITY** gap that affects retrieval quality and storage efficiency.

---

## The Problem with Flat Memory

### Current Design (Flawed)

```
User says: "Let's discuss the quarterly report"
Agent says: "Sure, I'll help with that"
User says: "Revenue was $1.2M"
User says: "Expenses were $800K"
User says: "That gives us $400K profit"
→ Stored as 5 separate memories with no relationship
```

### Issues

1. **Fragmentation**: Related information scattered across nodes
2. **Context Loss**: "That gives us $400K profit" meaningless without prior context
3. **Retrieval Noise**: Searching for "profit" returns the fragment, not the full discussion
4. **Storage Bloat**: Every utterance stored, including filler ("Sure, I'll help")

---

## Proposed: Three-Tier Memory Architecture

```
┌─────────────────────────────────────────────────────────┐
│                    WORKING MEMORY                        │
│         (Current conversation context window)            │
│                   Not persisted                          │
└─────────────────────┬───────────────────────────────────┘
                      │ Session End / Summarization
                      ▼
┌─────────────────────────────────────────────────────────┐
│                   EPISODIC MEMORY                        │
│    Specific interactions with timestamps & context       │
│    "On Jan 4, user discussed Q4 report: revenue $1.2M,  │
│     expenses $800K, profit $400K"                        │
└─────────────────────┬───────────────────────────────────┘
                      │ Consolidation (weekly/monthly)
                      ▼
┌─────────────────────────────────────────────────────────┐
│                   SEMANTIC MEMORY                        │
│    Extracted facts, entities, and relationships          │
│    User → WORKS_AT → Company                            │
│    Company → HAS_METRIC → Revenue($1.2M, Q4-2024)       │
└─────────────────────────────────────────────────────────┘
```

---

## Chunking Strategies

### Strategy 1: Conversation-Turn Chunking

Group by conversation turn boundaries:

```go
type ConversationChunk struct {
    UserMessage   string
    AgentResponse string
    Timestamp     time.Time
    SessionID     string
}
```

**Pros:** Preserves Q&A context
**Cons:** Chunks can be very long or very short

### Strategy 2: Semantic Chunking

Use LLM to identify topic boundaries:

```go
func chunkByTopic(conversation []Message) []Chunk {
    // Use Claude to identify topic shifts
    prompt := `Identify topic boundaries in this conversation.
               Return line numbers where topics change.`
    // ... implementation
}
```

**Pros:** Coherent semantic units
**Cons:** Requires additional LLM call, adds latency

### Strategy 3: Fixed-Window with Overlap

```go
const (
    ChunkSize    = 500  // tokens
    OverlapSize  = 50   // tokens
)

func chunkFixedWindow(text string) []string {
    tokens := tokenize(text)
    var chunks []string
    for i := 0; i < len(tokens); i += ChunkSize - OverlapSize {
        end := min(i+ChunkSize, len(tokens))
        chunks = append(chunks, detokenize(tokens[i:end]))
    }
    return chunks
}
```

**Pros:** Predictable, fast
**Cons:** May split semantic units

### Recommended: Hybrid Approach

```go
func smartChunk(conversation []Message) []Chunk {
    // 1. First pass: group by conversation turns
    turns := groupByTurns(conversation)

    // 2. Second pass: merge small turns, split large ones
    var chunks []Chunk
    var buffer []Turn
    bufferTokens := 0

    for _, turn := range turns {
        turnTokens := countTokens(turn)

        if bufferTokens + turnTokens > MaxChunkTokens {
            // Flush buffer as chunk
            chunks = append(chunks, mergeToChunk(buffer))
            buffer = []Turn{turn}
            bufferTokens = turnTokens
        } else {
            buffer = append(buffer, turn)
            bufferTokens += turnTokens
        }
    }

    // 3. Third pass: summarize if needed
    for i, chunk := range chunks {
        if chunk.TokenCount > SummarizationThreshold {
            chunks[i].Summary = summarize(chunk.Content)
            chunks[i].IsSummarized = true
        }
    }

    return chunks
}
```

---

## Summarization Pipeline

### When to Summarize

| Trigger | Action |
|---------|--------|
| Session end | Summarize conversation into episodic memory |
| Chunk > 1000 tokens | Create condensed version |
| Memory age > 7 days | Consider consolidation |
| Similar memories detected | Merge and summarize |

### Summarization Prompt Template

```go
const SummarizationPrompt = `
Summarize the following conversation/memory for long-term storage.

Requirements:
1. Preserve key facts, decisions, and outcomes
2. Include relevant entities (people, projects, dates)
3. Maintain temporal context (when did this happen?)
4. Remove filler, pleasantries, and redundancy
5. Output should be 20-30% of original length

Original:
{{.Content}}

Summary:
`
```

### Summarization Service

```go
type SummarizationService struct {
    llm       LLMClient
    threshold int // tokens
}

func (s *SummarizationService) MaybeSummarize(memory Memory) Memory {
    if countTokens(memory.Content) < s.threshold {
        return memory
    }

    summary, err := s.llm.Complete(SummarizationPrompt, memory.Content)
    if err != nil {
        // Fallback: store original
        return memory
    }

    return Memory{
        Content:         summary,
        OriginalContent: memory.Content, // Keep original for deep retrieval
        IsSummarized:    true,
        SummarizedAt:    time.Now(),
    }
}
```

---

## Memory Consolidation (Episodic → Semantic)

### Daily Consolidation Job

```go
func consolidateMemories(ctx context.Context, neo4j Driver) error {
    // Find memories older than 7 days that haven't been consolidated
    query := `
        MATCH (m:Memory:Episodic)
        WHERE m.createdAt < datetime() - duration('P7D')
        AND NOT m.consolidated
        RETURN m
        ORDER BY m.createdAt
        LIMIT 100
    `

    memories := executeQuery(query)

    // Cluster similar memories
    clusters := clusterBySimilarity(memories, threshold: 0.85)

    for _, cluster := range clusters {
        // Extract facts and create semantic nodes
        facts := extractFacts(cluster)
        for _, fact := range facts {
            createSemanticNode(fact)
        }

        // Mark episodic memories as consolidated
        markConsolidated(cluster)
    }

    return nil
}
```

### Fact Extraction Prompt

```go
const FactExtractionPrompt = `
Extract factual information from these memories as structured data.

Memories:
{{range .Memories}}
- [{{.Timestamp}}] {{.Content}}
{{end}}

Extract:
1. Entities (people, organizations, projects)
2. Relationships (who works with whom, what belongs to what)
3. Facts (metrics, dates, decisions)
4. Preferences (user likes/dislikes)

Output as JSON:
{
  "entities": [{"name": "...", "type": "..."}],
  "relationships": [{"from": "...", "to": "...", "type": "..."}],
  "facts": [{"subject": "...", "predicate": "...", "object": "..."}],
  "preferences": [{"topic": "...", "sentiment": "positive|negative"}]
}
`
```

---

## Neo4j Schema Updates

### New Node Labels

```cypher
// Episodic Memory: Specific interactions
CREATE (m:Memory:Episodic {
    content: $content,
    summary: $summary,
    sessionId: $sessionId,
    createdAt: datetime(),
    consolidated: false,
    embedding: $embedding
})

// Semantic Memory: Extracted facts
CREATE (f:Memory:Semantic:Fact {
    subject: "Company",
    predicate: "has_revenue",
    object: "$1.2M",
    confidence: 0.95,
    extractedFrom: [$episodicMemoryIds],
    validFrom: date("2024-10-01"),
    validTo: null
})

// Entity nodes
CREATE (e:Entity:Person {name: "John", role: "CEO"})
CREATE (e:Entity:Organization {name: "Acme Corp"})
CREATE (e:Entity:Project {name: "Project Apollo"})
```

### Relationships

```cypher
// Link episodic to semantic
MATCH (e:Episodic), (s:Semantic)
WHERE e.id IN s.extractedFrom
CREATE (s)-[:DERIVED_FROM]->(e)

// Link entities
MATCH (p:Person {name: "John"}), (o:Organization {name: "Acme Corp"})
CREATE (p)-[:WORKS_AT {since: date("2020-01-01")}]->(o)
```

---

## Retrieval Updates

### Multi-Tier Retrieval

```go
func (s *MemoryService) Recall(ctx context.Context, query string) (*RecallResult, error) {
    queryVec := s.embed(query)

    // 1. Search semantic memory first (fast, precise)
    semanticResults := s.searchSemantic(queryVec, limit: 5)

    // 2. Search episodic memory (detailed context)
    episodicResults := s.searchEpisodic(queryVec, limit: 10)

    // 3. If episodic hit is summarized, optionally fetch original
    for i, r := range episodicResults {
        if r.IsSummarized && r.Score > 0.9 {
            episodicResults[i].OriginalContent = s.fetchOriginal(r.ID)
        }
    }

    return &RecallResult{
        Facts:    semanticResults,
        Episodes: episodicResults,
    }, nil
}
```

---

## Action Items

- [ ] Define Memory node subtypes (Episodic, Semantic, Fact, Entity)
- [ ] Implement chunking service with hybrid strategy
- [ ] Create summarization service with configurable LLM
- [ ] Design consolidation job (cron/scheduled)
- [ ] Update retrieval to search multiple memory tiers
- [ ] Add `sessionId` tracking for conversation grouping
- [ ] Implement duplicate detection before storage
- [ ] Create fact extraction prompts and parser
- [ ] Benchmark retrieval quality with multi-tier vs flat

---

## References

- [Cognitive Architecture for Memory](https://en.wikipedia.org/wiki/Atkinson%E2%80%93Shiffrin_memory_model)
- [MemGPT Paper](https://arxiv.org/abs/2310.08560) - Hierarchical memory for LLMs
- [Chunking Strategies for RAG](https://www.pinecone.io/learn/chunking-strategies/)
