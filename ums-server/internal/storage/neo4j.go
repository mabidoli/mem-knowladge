// Package storage provides Neo4j-based persistence for the memory system.
package storage

import (
	"context"
	"fmt"
	"time"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
)

// Memory represents a stored memory node.
type Memory struct {
	ID             string    `json:"id"`
	Content        string    `json:"content"`
	Summary        string    `json:"summary,omitempty"`
	Embedding      []float32 `json:"-"`
	EmbeddingModel string    `json:"embedding_model"`
	Tags           []string  `json:"tags,omitempty"`
	Importance     int       `json:"importance"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
	Deleted        bool      `json:"deleted"`
}

// RecallResult represents a memory retrieval result with scoring.
type RecallResult struct {
	Memory      Memory  `json:"memory"`
	VectorScore float64 `json:"vector_score"`
	TimeScore   float64 `json:"time_score"`
	FinalScore  float64 `json:"final_score"`
}

// Neo4jStore implements memory storage using Neo4j.
type Neo4jStore struct {
	driver     neo4j.DriverWithContext
	database   string
	dimensions int
}

// Neo4jConfig configures the Neo4j connection.
type Neo4jConfig struct {
	URI        string // e.g., bolt://localhost:7687
	Username   string
	Password   string
	Database   string // Default: neo4j
	Dimensions int    // Vector dimensions (default: 768 for nomic-embed-text)
}

// NewNeo4jStore creates a new Neo4j storage instance.
func NewNeo4jStore(ctx context.Context, cfg Neo4jConfig) (*Neo4jStore, error) {
	if cfg.Database == "" {
		cfg.Database = "neo4j"
	}
	if cfg.Dimensions == 0 {
		cfg.Dimensions = 768
	}

	driver, err := neo4j.NewDriverWithContext(
		cfg.URI,
		neo4j.BasicAuth(cfg.Username, cfg.Password, ""),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create driver: %w", err)
	}

	// Verify connectivity
	if err := driver.VerifyConnectivity(ctx); err != nil {
		driver.Close(ctx)
		return nil, fmt.Errorf("failed to connect to Neo4j: %w", err)
	}

	store := &Neo4jStore{
		driver:     driver,
		database:   cfg.Database,
		dimensions: cfg.Dimensions,
	}

	// Initialize schema
	if err := store.initSchema(ctx); err != nil {
		driver.Close(ctx)
		return nil, fmt.Errorf("failed to initialize schema: %w", err)
	}

	return store, nil
}

// initSchema creates necessary indexes and constraints.
func (s *Neo4jStore) initSchema(ctx context.Context) error {
	session := s.driver.NewSession(ctx, neo4j.SessionConfig{
		DatabaseName: s.database,
		AccessMode:   neo4j.AccessModeWrite,
	})
	defer session.Close(ctx)

	queries := []string{
		// Unique constraint on Memory ID
		`CREATE CONSTRAINT memory_id IF NOT EXISTS FOR (m:Memory) REQUIRE m.id IS UNIQUE`,

		// Index on embedding model for version-aware queries
		`CREATE INDEX memory_model IF NOT EXISTS FOR (m:Memory) ON (m.embeddingModel)`,

		// Index on creation time for time-based queries
		`CREATE INDEX memory_created IF NOT EXISTS FOR (m:Memory) ON (m.createdAt)`,

		// Index on deleted flag for filtering
		`CREATE INDEX memory_deleted IF NOT EXISTS FOR (m:Memory) ON (m.deleted)`,

		// Vector index for semantic search
		fmt.Sprintf(`
			CREATE VECTOR INDEX memory_embeddings IF NOT EXISTS
			FOR (m:Memory) ON (m.embedding)
			OPTIONS {indexConfig: {
				`+"`vector.dimensions`"+`: %d,
				`+"`vector.similarity_function`"+`: 'cosine'
			}}
		`, s.dimensions),
	}

	for _, query := range queries {
		_, err := session.ExecuteWrite(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
			_, err := tx.Run(ctx, query, nil)
			return nil, err
		})
		if err != nil {
			// Ignore "already exists" errors
			continue
		}
	}

	return nil
}

// SaveMemory persists a memory to Neo4j.
func (s *Neo4jStore) SaveMemory(ctx context.Context, mem Memory) error {
	session := s.driver.NewSession(ctx, neo4j.SessionConfig{
		DatabaseName: s.database,
		AccessMode:   neo4j.AccessModeWrite,
	})
	defer session.Close(ctx)

	query := `
		CREATE (m:Memory {
			id: $id,
			content: $content,
			summary: $summary,
			embedding: $embedding,
			embeddingModel: $embeddingModel,
			importance: $importance,
			createdAt: datetime(),
			updatedAt: datetime(),
			deleted: false
		})
		WITH m
		UNWIND $tags AS tagName
		MERGE (t:Tag {name: tagName})
		MERGE (m)-[:TAGGED]->(t)
		RETURN m.id
	`

	params := map[string]any{
		"id":             mem.ID,
		"content":        mem.Content,
		"summary":        mem.Summary,
		"embedding":      mem.Embedding,
		"embeddingModel": mem.EmbeddingModel,
		"importance":     mem.Importance,
		"tags":           mem.Tags,
	}

	_, err := session.ExecuteWrite(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		_, err := tx.Run(ctx, query, params)
		return nil, err
	})

	return err
}

// RecallMemories retrieves memories using vector similarity and time decay.
func (s *Neo4jStore) RecallMemories(ctx context.Context, embedding []float32, limit int, alpha, lambda float64) ([]RecallResult, error) {
	session := s.driver.NewSession(ctx, neo4j.SessionConfig{
		DatabaseName: s.database,
		AccessMode:   neo4j.AccessModeRead,
	})
	defer session.Close(ctx)

	// Hybrid scoring: alpha * vectorScore + (1-alpha) * timeScore
	// timeScore = 1 / exp(lambda * daysElapsed)
	query := `
		CALL db.index.vector.queryNodes('memory_embeddings', $candidates, $embedding)
		YIELD node, score AS vectorScore
		WHERE NOT node.deleted
		WITH node, vectorScore,
			 duration.inDays(node.createdAt, datetime()).days AS daysElapsed
		WITH node, vectorScore,
			 1.0 / exp($lambda * daysElapsed) AS timeScore
		WITH node,
			 vectorScore,
			 timeScore,
			 ($alpha * vectorScore) + ((1.0 - $alpha) * timeScore) AS finalScore
		RETURN node.id AS id,
			   node.content AS content,
			   node.summary AS summary,
			   node.embeddingModel AS embeddingModel,
			   node.importance AS importance,
			   node.createdAt AS createdAt,
			   vectorScore,
			   timeScore,
			   finalScore
		ORDER BY finalScore DESC
		LIMIT $limit
	`

	params := map[string]any{
		"embedding":  embedding,
		"candidates": limit * 10, // Fetch more candidates for re-ranking
		"limit":      limit,
		"alpha":      alpha,
		"lambda":     lambda,
	}

	result, err := session.ExecuteRead(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		records, err := tx.Run(ctx, query, params)
		if err != nil {
			return nil, err
		}

		var results []RecallResult
		for records.Next(ctx) {
			record := records.Record()

			createdAt, _ := record.Get("createdAt")
			createdTime, _ := createdAt.(time.Time)

			results = append(results, RecallResult{
				Memory: Memory{
					ID:             record.Values[0].(string),
					Content:        record.Values[1].(string),
					Summary:        safeString(record.Values[2]),
					EmbeddingModel: safeString(record.Values[3]),
					Importance:     int(safeInt64(record.Values[4])),
					CreatedAt:      createdTime,
				},
				VectorScore: record.Values[6].(float64),
				TimeScore:   record.Values[7].(float64),
				FinalScore:  record.Values[8].(float64),
			})
		}

		return results, records.Err()
	})

	if err != nil {
		return nil, err
	}

	return result.([]RecallResult), nil
}

// ForgetMemory soft-deletes a memory by ID.
func (s *Neo4jStore) ForgetMemory(ctx context.Context, id string) error {
	session := s.driver.NewSession(ctx, neo4j.SessionConfig{
		DatabaseName: s.database,
		AccessMode:   neo4j.AccessModeWrite,
	})
	defer session.Close(ctx)

	query := `
		MATCH (m:Memory {id: $id})
		SET m.deleted = true, m.deletedAt = datetime()
		RETURN m.id
	`

	_, err := session.ExecuteWrite(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		result, err := tx.Run(ctx, query, map[string]any{"id": id})
		if err != nil {
			return nil, err
		}
		if !result.Next(ctx) {
			return nil, fmt.Errorf("memory not found: %s", id)
		}
		return nil, nil
	})

	return err
}

// Close closes the Neo4j driver.
func (s *Neo4jStore) Close(ctx context.Context) error {
	return s.driver.Close(ctx)
}

// HealthCheck verifies Neo4j connectivity.
func (s *Neo4jStore) HealthCheck(ctx context.Context) error {
	return s.driver.VerifyConnectivity(ctx)
}

// Helper functions
func safeString(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func safeInt64(v any) int64 {
	if v == nil {
		return 0
	}
	if i, ok := v.(int64); ok {
		return i
	}
	return 0
}
