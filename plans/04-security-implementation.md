# Improvement Area 4: Security Implementation

## Problem Statement

The original architecture mentions security but provides no concrete implementation:
1. No Neo4j role/user creation for RBAC
2. No input validation implementation
3. No rate limiting
4. Secrets stored in plain config files
5. No audit logging

This is a **HIGH SEVERITY** gap that could lead to data breaches or system abuse.

---

## Threat Model

### Actors

| Actor | Trust Level | Potential Actions |
|-------|-------------|-------------------|
| LLM Agent | Medium | May hallucinate malicious inputs |
| Local User | High | Configures system, provides secrets |
| External Attacker | None | May attempt injection if exposed |
| Compromised Dependency | Low | Supply chain attacks |

### Attack Vectors

| Vector | Risk | Mitigation |
|--------|------|------------|
| Cypher Injection | High | Parameterized queries (already done) |
| Memory Poisoning | Medium | Input validation, size limits |
| Resource Exhaustion | Medium | Rate limiting, buffer caps |
| Credential Theft | High | Secrets management |
| Data Exfiltration | Medium | RBAC, audit logging |
| Unauthorized Deletion | High | Restricted Neo4j permissions |

---

## Implementation Plan

### 1. Neo4j RBAC Configuration

#### Create Restricted User

```cypher
// Run as Neo4j admin
// Create role with minimal permissions
CREATE ROLE memory_writer;

// Grant read on all Memory nodes
GRANT MATCH {*} ON GRAPH neo4j NODES Memory TO memory_writer;
GRANT MATCH {*} ON GRAPH neo4j NODES Tag TO memory_writer;
GRANT MATCH {*} ON GRAPH neo4j NODES Entity TO memory_writer;

// Grant create (but not delete) on Memory
GRANT CREATE ON GRAPH neo4j NODES Memory TO memory_writer;
GRANT CREATE ON GRAPH neo4j NODES Tag TO memory_writer;
GRANT CREATE ON GRAPH neo4j RELATIONSHIPS TAGGED TO memory_writer;

// Grant set property (for soft delete)
GRANT SET PROPERTY {deleted, deletedAt} ON GRAPH neo4j NODES Memory TO memory_writer;

// Explicitly deny destructive operations
DENY DELETE ON GRAPH neo4j TO memory_writer;
DENY REMOVE LABEL ON GRAPH neo4j TO memory_writer;
DENY DROP INDEX ON DATABASE neo4j TO memory_writer;

// Create user with role
CREATE USER ums_service SET PASSWORD 'generated-secure-password' CHANGE NOT REQUIRED;
GRANT ROLE memory_writer TO ums_service;
```

#### Verify Permissions

```cypher
// Test as ums_service user
// Should succeed:
CREATE (m:Memory {content: "test", createdAt: datetime()})

// Should fail:
MATCH (m:Memory) DELETE m
// Error: DELETE not allowed for role memory_writer
```

### 2. Input Validation Layer

```go
package validation

import (
    "errors"
    "regexp"
    "unicode"
)

const (
    MaxContentLength    = 10000  // characters
    MaxTagLength        = 100
    MaxTagsPerMemory    = 20
    MaxQueryLength      = 500
)

var (
    ErrContentTooLong  = errors.New("content exceeds maximum length")
    ErrContentEmpty    = errors.New("content cannot be empty")
    ErrInvalidChars    = errors.New("content contains invalid characters")
    ErrTooManyTags     = errors.New("too many tags")
    ErrTagTooLong      = errors.New("tag exceeds maximum length")
)

// Disallow control characters except newline/tab
var controlCharRegex = regexp.MustCompile(`[\x00-\x08\x0B\x0C\x0E-\x1F\x7F]`)

// Disallow potential injection patterns (defense in depth)
var suspiciousPatterns = regexp.MustCompile(`(?i)(MATCH|CREATE|DELETE|DETACH|MERGE|CALL)\s*[\(\[]`)

type Validator struct {
    maxContentLen int
    maxQueryLen   int
}

func NewValidator() *Validator {
    return &Validator{
        maxContentLen: MaxContentLength,
        maxQueryLen:   MaxQueryLength,
    }
}

func (v *Validator) ValidateMemoryContent(content string) error {
    if len(content) == 0 {
        return ErrContentEmpty
    }

    if len(content) > v.maxContentLen {
        return ErrContentTooLong
    }

    if controlCharRegex.MatchString(content) {
        return ErrInvalidChars
    }

    // Note: This is defense-in-depth. Parameterized queries
    // already prevent injection, but we log suspicious inputs.
    if suspiciousPatterns.MatchString(content) {
        log.Warn("Suspicious pattern in memory content",
            "content_preview", content[:min(100, len(content))])
    }

    return nil
}

func (v *Validator) ValidateTags(tags []string) error {
    if len(tags) > MaxTagsPerMemory {
        return ErrTooManyTags
    }

    for _, tag := range tags {
        if len(tag) > MaxTagLength {
            return ErrTagTooLong
        }
        // Tags should be alphanumeric with hyphens/underscores
        if !isValidTagFormat(tag) {
            return fmt.Errorf("invalid tag format: %s", tag)
        }
    }

    return nil
}

func (v *Validator) ValidateQuery(query string) error {
    if len(query) == 0 {
        return errors.New("query cannot be empty")
    }
    if len(query) > v.maxQueryLen {
        return errors.New("query too long")
    }
    return nil
}

func isValidTagFormat(tag string) bool {
    for _, r := range tag {
        if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '-' && r != '_' {
            return false
        }
    }
    return true
}
```

### 3. Rate Limiting

```go
package ratelimit

import (
    "sync"
    "time"

    "golang.org/x/time/rate"
)

type RateLimiter struct {
    // Per-operation limits
    saveLimiter   *rate.Limiter
    recallLimiter *rate.Limiter

    // Global limits
    globalLimiter *rate.Limiter

    mu sync.Mutex
}

func NewRateLimiter(config RateLimitConfig) *RateLimiter {
    return &RateLimiter{
        // 10 saves per second, burst of 20
        saveLimiter: rate.NewLimiter(rate.Limit(config.SavePerSecond), config.SaveBurst),

        // 20 recalls per second, burst of 50
        recallLimiter: rate.NewLimiter(rate.Limit(config.RecallPerSecond), config.RecallBurst),

        // Global: 100 ops per second
        globalLimiter: rate.NewLimiter(rate.Limit(config.GlobalPerSecond), config.GlobalBurst),
    }
}

type RateLimitConfig struct {
    SavePerSecond    float64 `env:"UMS_RATE_SAVE_PER_SEC" default:"10"`
    SaveBurst        int     `env:"UMS_RATE_SAVE_BURST" default:"20"`
    RecallPerSecond  float64 `env:"UMS_RATE_RECALL_PER_SEC" default:"20"`
    RecallBurst      int     `env:"UMS_RATE_RECALL_BURST" default:"50"`
    GlobalPerSecond  float64 `env:"UMS_RATE_GLOBAL_PER_SEC" default:"100"`
    GlobalBurst      int     `env:"UMS_RATE_GLOBAL_BURST" default:"200"`
}

func (rl *RateLimiter) AllowSave() bool {
    return rl.globalLimiter.Allow() && rl.saveLimiter.Allow()
}

func (rl *RateLimiter) AllowRecall() bool {
    return rl.globalLimiter.Allow() && rl.recallLimiter.Allow()
}

// MCP integration
func (ms *MemorySystem) SaveMemoryWithRateLimit(ctx context.Context, req SaveRequest) (*mcp.CallToolResult, error) {
    if !ms.rateLimiter.AllowSave() {
        metrics.RateLimitHits.WithLabelValues("save").Inc()
        return mcp.NewToolResultError(
            "Rate limit exceeded. Please slow down memory creation.",
        ), nil
    }

    return ms.SaveMemory(ctx, req)
}
```

### 4. Secrets Management

#### Option A: Environment Variables (Basic)

```go
type Secrets struct {
    Neo4jPassword  string `env:"NEO4J_PASSWORD,required"`
    OpenAIKey      string `env:"OPENAI_API_KEY"`
    EncryptionKey  string `env:"UMS_ENCRYPTION_KEY"` // For at-rest encryption
}

func LoadSecrets() (*Secrets, error) {
    s := &Secrets{}

    // Never log secrets
    s.Neo4jPassword = os.Getenv("NEO4J_PASSWORD")
    if s.Neo4jPassword == "" {
        return nil, errors.New("NEO4J_PASSWORD required")
    }

    s.OpenAIKey = os.Getenv("OPENAI_API_KEY")
    s.EncryptionKey = os.Getenv("UMS_ENCRYPTION_KEY")

    return s, nil
}
```

#### Option B: Keyring Integration (Recommended for Desktop)

```go
import "github.com/zalando/go-keyring"

const (
    ServiceName = "universal-memory-system"
)

func GetNeo4jPassword() (string, error) {
    password, err := keyring.Get(ServiceName, "neo4j_password")
    if err == keyring.ErrNotFound {
        return "", errors.New("Neo4j password not configured. Run: ums configure")
    }
    return password, err
}

func SetNeo4jPassword(password string) error {
    return keyring.Set(ServiceName, "neo4j_password", password)
}
```

#### Option C: HashiCorp Vault (Enterprise)

```go
import vault "github.com/hashicorp/vault/api"

func LoadSecretsFromVault() (*Secrets, error) {
    client, err := vault.NewClient(vault.DefaultConfig())
    if err != nil {
        return nil, err
    }

    secret, err := client.Logical().Read("secret/data/ums")
    if err != nil {
        return nil, err
    }

    data := secret.Data["data"].(map[string]interface{})

    return &Secrets{
        Neo4jPassword: data["neo4j_password"].(string),
        OpenAIKey:     data["openai_key"].(string),
    }, nil
}
```

### 5. Audit Logging

```go
package audit

import (
    "encoding/json"
    "os"
    "time"
)

type AuditEvent struct {
    Timestamp   time.Time         `json:"timestamp"`
    Operation   string            `json:"operation"`
    Success     bool              `json:"success"`
    ContentHash string            `json:"content_hash,omitempty"` // SHA256 of content, not content itself
    Tags        []string          `json:"tags,omitempty"`
    Error       string            `json:"error,omitempty"`
    Metadata    map[string]string `json:"metadata,omitempty"`
}

type AuditLogger struct {
    file    *os.File
    encoder *json.Encoder
}

func NewAuditLogger(path string) (*AuditLogger, error) {
    f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
    if err != nil {
        return nil, err
    }
    return &AuditLogger{
        file:    f,
        encoder: json.NewEncoder(f),
    }, nil
}

func (al *AuditLogger) LogSave(content string, tags []string, success bool, err error) {
    event := AuditEvent{
        Timestamp:   time.Now().UTC(),
        Operation:   "save_memory",
        Success:     success,
        ContentHash: sha256Hash(content),
        Tags:        tags,
    }
    if err != nil {
        event.Error = err.Error()
    }
    al.encoder.Encode(event)
}

func (al *AuditLogger) LogRecall(query string, resultCount int, success bool) {
    event := AuditEvent{
        Timestamp: time.Now().UTC(),
        Operation: "recall_memory",
        Success:   success,
        Metadata: map[string]string{
            "query_hash":   sha256Hash(query),
            "result_count": strconv.Itoa(resultCount),
        },
    }
    al.encoder.Encode(event)
}

func (al *AuditLogger) LogForget(memoryID string, success bool) {
    event := AuditEvent{
        Timestamp: time.Now().UTC(),
        Operation: "forget_memory",
        Success:   success,
        Metadata: map[string]string{
            "memory_id": memoryID,
        },
    }
    al.encoder.Encode(event)
}

func sha256Hash(s string) string {
    h := sha256.Sum256([]byte(s))
    return hex.EncodeToString(h[:])
}
```

### 6. Content Encryption at Rest (Optional)

```go
package encryption

import (
    "crypto/aes"
    "crypto/cipher"
    "crypto/rand"
    "encoding/base64"
)

type Encryptor struct {
    gcm cipher.AEAD
}

func NewEncryptor(key []byte) (*Encryptor, error) {
    block, err := aes.NewCipher(key)
    if err != nil {
        return nil, err
    }

    gcm, err := cipher.NewGCM(block)
    if err != nil {
        return nil, err
    }

    return &Encryptor{gcm: gcm}, nil
}

func (e *Encryptor) Encrypt(plaintext string) (string, error) {
    nonce := make([]byte, e.gcm.NonceSize())
    if _, err := rand.Read(nonce); err != nil {
        return "", err
    }

    ciphertext := e.gcm.Seal(nonce, nonce, []byte(plaintext), nil)
    return base64.StdEncoding.EncodeToString(ciphertext), nil
}

func (e *Encryptor) Decrypt(ciphertext string) (string, error) {
    data, err := base64.StdEncoding.DecodeString(ciphertext)
    if err != nil {
        return "", err
    }

    nonceSize := e.gcm.NonceSize()
    if len(data) < nonceSize {
        return "", errors.New("ciphertext too short")
    }

    nonce, ciphertext := data[:nonceSize], data[nonceSize:]
    plaintext, err := e.gcm.Open(nil, nonce, ciphertext, nil)
    if err != nil {
        return "", err
    }

    return string(plaintext), nil
}
```

---

## Security Checklist

### Before Production

- [ ] Create restricted Neo4j user with RBAC
- [ ] Remove default `neo4j/password` credentials
- [ ] Implement input validation on all MCP handlers
- [ ] Add rate limiting
- [ ] Move secrets to keyring or secrets manager
- [ ] Enable audit logging
- [ ] Review and test DENY permissions in Neo4j

### Ongoing

- [ ] Rotate Neo4j password quarterly
- [ ] Review audit logs weekly
- [ ] Monitor rate limit metrics
- [ ] Update dependencies for security patches
- [ ] Penetration test annually

---

## MCP Handler Integration

```go
func (ms *MemorySystem) SaveMemorySecure(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
    // 1. Rate limit
    if !ms.rateLimiter.AllowSave() {
        ms.audit.LogSave("", nil, false, errors.New("rate limited"))
        return mcp.NewToolResultError("Rate limit exceeded"), nil
    }

    // 2. Extract and validate input
    content := req.Params.Arguments["content"].(string)
    tags := extractTags(req.Params.Arguments["tags"])

    if err := ms.validator.ValidateMemoryContent(content); err != nil {
        ms.audit.LogSave(content, tags, false, err)
        return mcp.NewToolResultError(err.Error()), nil
    }

    if err := ms.validator.ValidateTags(tags); err != nil {
        ms.audit.LogSave(content, tags, false, err)
        return mcp.NewToolResultError(err.Error()), nil
    }

    // 3. Optional: encrypt content
    storedContent := content
    if ms.encryptor != nil {
        var err error
        storedContent, err = ms.encryptor.Encrypt(content)
        if err != nil {
            return mcp.NewToolResultError("Encryption failed"), nil
        }
    }

    // 4. Queue for persistence
    payload := MemoryPayload{
        Content: storedContent,
        Tags:    tags,
    }

    select {
    case ms.queue <- payload:
        ms.audit.LogSave(content, tags, true, nil)
        return mcp.NewToolResultText("Memory saved"), nil
    case <-time.After(100 * time.Millisecond):
        ms.audit.LogSave(content, tags, false, errors.New("buffer full"))
        return mcp.NewToolResultError("System busy, retry later"), nil
    }
}
```

---

## Action Items

- [ ] Write Neo4j RBAC setup script
- [ ] Implement `Validator` package
- [ ] Implement `RateLimiter` with configurable limits
- [ ] Add keyring integration for secrets
- [ ] Implement audit logger with rotation
- [ ] Optional: Add AES-GCM encryption for sensitive memories
- [ ] Integration tests for permission denials
- [ ] Document security configuration in README
- [ ] Create `ums configure` CLI command for initial setup

---

## References

- [Neo4j RBAC Documentation](https://neo4j.com/docs/operations-manual/current/authentication-authorization/)
- [OWASP Input Validation Cheat Sheet](https://cheatsheetseries.owasp.org/cheatsheets/Input_Validation_Cheat_Sheet.html)
- [Go Keyring Library](https://github.com/zalando/go-keyring)
- [HashiCorp Vault Go Client](https://github.com/hashicorp/vault/tree/main/api)
