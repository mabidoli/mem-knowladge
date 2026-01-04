// Package embedding provides interfaces and implementations for text embedding generation.
package embedding

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"

	lru "github.com/hashicorp/golang-lru/v2"
)

// Provider defines the interface for embedding generation.
type Provider interface {
	// Embed generates a vector embedding for the given text.
	Embed(ctx context.Context, text string) ([]float32, error)

	// ModelName returns the identifier for this embedding model.
	ModelName() string

	// Dimensions returns the vector dimensions produced by this model.
	Dimensions() int
}

// Service wraps embedding providers with caching and fallback support.
type Service struct {
	primary  Provider
	fallback Provider
	cache    *lru.Cache[string, []float32]
	mu       sync.RWMutex

	// Metrics
	cacheHits   int64
	cacheMisses int64
	fallbacks   int64
}

// ServiceConfig configures the embedding service.
type ServiceConfig struct {
	CacheSize int // Number of embeddings to cache (default: 1000)
}

// NewService creates a new embedding service with the given providers.
func NewService(primary Provider, fallback Provider, cfg ServiceConfig) (*Service, error) {
	if cfg.CacheSize <= 0 {
		cfg.CacheSize = 1000
	}

	cache, err := lru.New[string, []float32](cfg.CacheSize)
	if err != nil {
		return nil, fmt.Errorf("failed to create cache: %w", err)
	}

	return &Service{
		primary:  primary,
		fallback: fallback,
		cache:    cache,
	}, nil
}

// Embed generates an embedding, using cache and fallback as needed.
func (s *Service) Embed(ctx context.Context, text string) ([]float32, error) {
	cacheKey := hashText(text)

	// Check cache first
	s.mu.RLock()
	if vec, ok := s.cache.Get(cacheKey); ok {
		s.mu.RUnlock()
		s.mu.Lock()
		s.cacheHits++
		s.mu.Unlock()
		return vec, nil
	}
	s.mu.RUnlock()

	s.mu.Lock()
	s.cacheMisses++
	s.mu.Unlock()

	// Try primary provider
	vec, err := s.primary.Embed(ctx, text)
	if err != nil {
		// Fallback if available
		if s.fallback != nil {
			s.mu.Lock()
			s.fallbacks++
			s.mu.Unlock()

			vec, err = s.fallback.Embed(ctx, text)
			if err != nil {
				return nil, fmt.Errorf("both providers failed: %w", err)
			}
		} else {
			return nil, err
		}
	}

	// Cache successful result
	s.mu.Lock()
	s.cache.Add(cacheKey, vec)
	s.mu.Unlock()

	return vec, nil
}

// ModelName returns the primary model name.
func (s *Service) ModelName() string {
	return s.primary.ModelName()
}

// Dimensions returns the primary model dimensions.
func (s *Service) Dimensions() int {
	return s.primary.Dimensions()
}

// Stats returns cache statistics.
func (s *Service) Stats() (hits, misses, fallbacks int64) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cacheHits, s.cacheMisses, s.fallbacks
}

// hashText creates a cache key from text content.
func hashText(text string) string {
	h := sha256.Sum256([]byte(text))
	return hex.EncodeToString(h[:16]) // Use first 16 bytes for shorter key
}
