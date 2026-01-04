// Package main provides the entry point for the Universal Memory System MCP server.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"go.uber.org/zap"

	"github.com/mabidoli/ums/internal/embedding"
	"github.com/mabidoli/ums/internal/storage"
)

const (
	Version = "1.0.0"
)

// Config holds all configuration for the UMS server.
type Config struct {
	// Neo4j settings
	Neo4jURI      string
	Neo4jUser     string
	Neo4jPassword string
	Neo4jDatabase string

	// Embedding settings
	EmbeddingProvider string // "ollama" or "openai"
	OllamaURL         string
	OllamaModel       string
	OpenAIKey         string
	OpenAIModel       string

	// Retrieval settings
	RetrievalAlpha  float64 // Weight for vector score (default: 0.7)
	RetrievalLambda float64 // Time decay constant (default: 0.1)
	DefaultLimit    int     // Default number of results (default: 5)
}

func main() {
	// Initialize logger
	logger, _ := zap.NewProduction()
	defer logger.Sync()

	// Load configuration from environment
	cfg := loadConfig()

	// Create context with cancellation
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle shutdown signals
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		logger.Info("Shutdown signal received")
		cancel()
	}()

	// Initialize embedding provider
	embeddingService, err := initEmbedding(cfg)
	if err != nil {
		logger.Fatal("Failed to initialize embedding service", zap.Error(err))
	}

	// Initialize Neo4j storage
	store, err := storage.NewNeo4jStore(ctx, storage.Neo4jConfig{
		URI:        cfg.Neo4jURI,
		Username:   cfg.Neo4jUser,
		Password:   cfg.Neo4jPassword,
		Database:   cfg.Neo4jDatabase,
		Dimensions: embeddingService.Dimensions(),
	})
	if err != nil {
		logger.Fatal("Failed to initialize Neo4j", zap.Error(err))
	}
	defer store.Close(ctx)

	// Create MCP server
	s := server.NewMCPServer(
		"Universal Memory System",
		Version,
		server.WithToolCapabilities(true),
		server.WithResourceCapabilities(true, false),
	)

	// Register tools
	registerTools(s, store, embeddingService, cfg, logger)

	// Register resources
	registerResources(s, store, embeddingService)

	logger.Info("Starting Universal Memory System",
		zap.String("version", Version),
		zap.String("embedding_provider", cfg.EmbeddingProvider),
		zap.Int("dimensions", embeddingService.Dimensions()),
	)

	// Start stdio server
	if err := server.ServeStdio(s); err != nil {
		logger.Fatal("Server error", zap.Error(err))
	}
}

func loadConfig() Config {
	return Config{
		// Neo4j
		Neo4jURI:      getEnv("NEO4J_URI", "bolt://localhost:7687"),
		Neo4jUser:     getEnv("NEO4J_USER", "neo4j"),
		Neo4jPassword: getEnv("NEO4J_PASSWORD", "password"),
		Neo4jDatabase: getEnv("NEO4J_DATABASE", "neo4j"),

		// Embedding
		EmbeddingProvider: getEnv("EMBEDDING_PROVIDER", "ollama"),
		OllamaURL:         getEnv("OLLAMA_URL", "http://localhost:11434"),
		OllamaModel:       getEnv("OLLAMA_MODEL", "nomic-embed-text"),
		OpenAIKey:         os.Getenv("OPENAI_API_KEY"),
		OpenAIModel:       getEnv("OPENAI_MODEL", "text-embedding-3-small"),

		// Retrieval
		RetrievalAlpha:  0.7,
		RetrievalLambda: 0.1,
		DefaultLimit:    5,
	}
}

func initEmbedding(cfg Config) (*embedding.Service, error) {
	var primary, fallback embedding.Provider

	switch cfg.EmbeddingProvider {
	case "openai":
		if cfg.OpenAIKey == "" {
			return nil, fmt.Errorf("OPENAI_API_KEY required for openai provider")
		}
		var err error
		primary, err = embedding.NewOpenAIProvider(embedding.OpenAIConfig{
			APIKey: cfg.OpenAIKey,
			Model:  cfg.OpenAIModel,
		})
		if err != nil {
			return nil, err
		}
		// Use Ollama as fallback
		fallback = embedding.NewOllamaProvider(embedding.OllamaConfig{
			BaseURL: cfg.OllamaURL,
			Model:   cfg.OllamaModel,
		})

	case "ollama":
		fallthrough
	default:
		primary = embedding.NewOllamaProvider(embedding.OllamaConfig{
			BaseURL: cfg.OllamaURL,
			Model:   cfg.OllamaModel,
		})
		// No fallback for local-first mode
	}

	return embedding.NewService(primary, fallback, embedding.ServiceConfig{
		CacheSize: 1000,
	})
}

func registerTools(s *server.MCPServer, store *storage.Neo4jStore, emb *embedding.Service, cfg Config, logger *zap.Logger) {
	// save_memory tool
	s.AddTool(
		mcp.NewTool("save_memory",
			mcp.WithDescription("Store a new memory with semantic embedding for later retrieval"),
			mcp.WithString("content",
				mcp.Required(),
				mcp.Description("The content to remember"),
			),
			mcp.WithNumber("importance",
				mcp.Description("Importance level 1-10 (default: 5)"),
			),
			mcp.WithArray("tags",
				mcp.Description("Tags for categorization"),
			),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			content := req.Params.Arguments["content"].(string)
			importance := 5
			if imp, ok := req.Params.Arguments["importance"].(float64); ok {
				importance = int(imp)
			}
			var tags []string
			if t, ok := req.Params.Arguments["tags"].([]interface{}); ok {
				for _, tag := range t {
					if s, ok := tag.(string); ok {
						tags = append(tags, s)
					}
				}
			}

			// Generate embedding
			vec, err := emb.Embed(ctx, content)
			if err != nil {
				logger.Error("Embedding failed", zap.Error(err))
				return mcp.NewToolResultError(fmt.Sprintf("Failed to generate embedding: %v", err)), nil
			}

			// Create memory
			mem := storage.Memory{
				ID:             generateID(),
				Content:        content,
				Embedding:      vec,
				EmbeddingModel: emb.ModelName(),
				Importance:     importance,
				Tags:           tags,
			}

			if err := store.SaveMemory(ctx, mem); err != nil {
				logger.Error("Save failed", zap.Error(err))
				return mcp.NewToolResultError(fmt.Sprintf("Failed to save memory: %v", err)), nil
			}

			logger.Info("Memory saved",
				zap.String("id", mem.ID),
				zap.Int("content_length", len(content)),
				zap.Strings("tags", tags),
			)

			return mcp.NewToolResultText(fmt.Sprintf("Memory saved with ID: %s", mem.ID)), nil
		},
	)

	// recall_memory tool
	s.AddTool(
		mcp.NewTool("recall_memory",
			mcp.WithDescription("Retrieve relevant memories based on semantic similarity and recency"),
			mcp.WithString("query",
				mcp.Required(),
				mcp.Description("What to search for in memories"),
			),
			mcp.WithNumber("limit",
				mcp.Description("Maximum number of results (default: 5)"),
			),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			query := req.Params.Arguments["query"].(string)
			limit := cfg.DefaultLimit
			if l, ok := req.Params.Arguments["limit"].(float64); ok {
				limit = int(l)
			}

			// Generate query embedding
			vec, err := emb.Embed(ctx, query)
			if err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("Failed to generate query embedding: %v", err)), nil
			}

			// Retrieve memories
			results, err := store.RecallMemories(ctx, vec, limit, cfg.RetrievalAlpha, cfg.RetrievalLambda)
			if err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("Failed to recall memories: %v", err)), nil
			}

			if len(results) == 0 {
				return mcp.NewToolResultText("No relevant memories found."), nil
			}

			// Format results
			output := fmt.Sprintf("Found %d relevant memories:\n\n", len(results))
			for i, r := range results {
				output += fmt.Sprintf("%d. [Score: %.2f] %s\n   Created: %s\n\n",
					i+1,
					r.FinalScore,
					r.Memory.Content,
					r.Memory.CreatedAt.Format("2006-01-02 15:04"),
				)
			}

			logger.Info("Memory recalled",
				zap.String("query", query),
				zap.Int("results", len(results)),
			)

			return mcp.NewToolResultText(output), nil
		},
	)

	// forget_memory tool
	s.AddTool(
		mcp.NewTool("forget_memory",
			mcp.WithDescription("Remove a memory by its ID (soft delete)"),
			mcp.WithString("memory_id",
				mcp.Required(),
				mcp.Description("The ID of the memory to forget"),
			),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			memoryID := req.Params.Arguments["memory_id"].(string)

			if err := store.ForgetMemory(ctx, memoryID); err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("Failed to forget memory: %v", err)), nil
			}

			logger.Info("Memory forgotten", zap.String("id", memoryID))
			return mcp.NewToolResultText(fmt.Sprintf("Memory %s has been forgotten.", memoryID)), nil
		},
	)
}

func registerResources(s *server.MCPServer, store *storage.Neo4jStore, emb *embedding.Service) {
	// Health status resource
	s.AddResource(
		mcp.NewResource("memory://health",
			mcp.WithResourceDescription("Memory system health status"),
			mcp.WithMIMEType("application/json"),
		),
		func(ctx context.Context, req mcp.ReadResourceRequest) (string, error) {
			status := "healthy"
			if err := store.HealthCheck(ctx); err != nil {
				status = "unhealthy: " + err.Error()
			}

			hits, misses, fallbacks := emb.Stats()
			return fmt.Sprintf(`{
  "status": "%s",
  "version": "%s",
  "embedding_model": "%s",
  "cache_hits": %d,
  "cache_misses": %d,
  "fallbacks": %d
}`, status, Version, emb.ModelName(), hits, misses, fallbacks), nil
		},
	)
}

func getEnv(key, defaultValue string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultValue
}

func generateID() string {
	return fmt.Sprintf("mem_%d", time.Now().UnixNano())
}
