// Package auth handles identity: password verification, token issue and verification,
// and the authenticated principal that flows through a request.
//
// Scope is deliberately modest. NEXUS issues its own short-lived JWTs against a local
// user table, which is enough to demonstrate authentication and authorisation without
// the project becoming an identity provider. The Principal type and the Authenticator
// interface are the seam an OIDC provider would be introduced behind.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/argon2"

	"github.com/Keathan2609/nexus/internal/core/domain"
	"github.com/Keathan2609/nexus/internal/platform/database"
	"github.com/Keathan2609/nexus/internal/rbac"
)

var (
	ErrInvalidCredentials = errors.New("invalid email or password")
	ErrInvalidToken       = errors.New("invalid or expired token")
	ErrInactiveUser       = errors.New("user account is not active")
)

// Principal is the authenticated identity of a request.
type Principal struct {
	UserID         domain.ID
	OrganisationID domain.ID
	Email          string
	Name           string
	Role           rbac.Role
	// TokenID distinguishes an API token from an interactive session, which the audit
	// trail records.
	TokenID domain.ID
}

func (p Principal) IsSystem() bool { return p.UserID == "" }

func (p Principal) ActorType() string {
	if p.TokenID != "" {
		return "TOKEN"
	}
	if p.IsSystem() {
		return "SYSTEM"
	}
	return "USER"
}

// --- password hashing --------------------------------------------------------
//
// Argon2id, the current recommendation for password storage. The parameters below are a
// deliberate trade: 64 MiB and one pass over four threads is comfortably above the
// minimum recommended cost while staying fast enough that a login does not feel slow on
// a laptop or hold a database connection open.

const (
	argonTime    = 1
	argonMemory  = 64 * 1024
	argonThreads = 4
	argonKeyLen  = 32
	saltLen      = 16
)

// HashPassword returns an encoded hash carrying its own parameters, so the cost can be
// raised later without invalidating existing hashes.
func HashPassword(password string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate salt: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key)), nil
}

// VerifyPassword compares a candidate against an encoded hash in constant time.
func VerifyPassword(encoded, password string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var memory uint32
	var timeCost uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &timeCost, &threads); err != nil {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false
	}
	got := argon2.IDKey([]byte(password), salt, timeCost, memory, threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// --- tokens ------------------------------------------------------------------

type Service struct {
	db     *database.DB
	secret []byte
	ttl    time.Duration
	issuer string
}

func NewService(db *database.DB, secret []byte, ttl time.Duration) *Service {
	return &Service{db: db, secret: secret, ttl: ttl, issuer: "nexus"}
}

type Claims struct {
	jwt.RegisteredClaims
	OrganisationID string `json:"org"`
	Email          string `json:"email"`
	Name           string `json:"name"`
	Role           string `json:"role"`
}

// Login verifies credentials and issues a token.
//
// A missing user and a wrong password return the same error, and the password hash is
// still verified against a dummy value when the user does not exist, so response timing
// does not reveal which addresses are registered.
func (s *Service) Login(ctx context.Context, email, password string) (string, *Principal, error) {
	var (
		userID   domain.ID
		orgID    domain.ID
		hash     string
		name     string
		isActive bool
		role     string
	)
	err := s.db.QueryRow(ctx,
		`SELECT u.id, u.organisation_id, u.password_hash, u.name, u.is_active,
		        COALESCE((SELECT m.role FROM memberships m
		                   WHERE m.user_id = u.id AND m.team_id IS NULL
		                   LIMIT 1), 'VIEWER')
		   FROM users u
		  WHERE lower(u.email) = lower($1)`, email).
		Scan(&userID, &orgID, &hash, &name, &isActive, &role)
	if err != nil {
		// Spend comparable time so a missing account is not distinguishable by latency.
		_ = VerifyPassword("$argon2id$v=19$m=65536,t=1,p=4$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", password)
		return "", nil, ErrInvalidCredentials
	}
	if !VerifyPassword(hash, password) {
		return "", nil, ErrInvalidCredentials
	}
	if !isActive {
		return "", nil, ErrInactiveUser
	}

	p := &Principal{
		UserID: userID, OrganisationID: orgID, Email: email,
		Name: name, Role: rbac.Role(role),
	}
	token, err := s.Issue(p)
	if err != nil {
		return "", nil, err
	}
	_, _ = s.db.Exec(ctx, `UPDATE users SET last_login_at = now() WHERE id = $1`, userID)
	return token, p, nil
}

func (s *Service) Issue(p *Principal) (string, error) {
	now := time.Now()
	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   p.UserID.String(),
			Issuer:    s.issuer,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(s.ttl)),
			NotBefore: jwt.NewNumericDate(now),
			ID:        domain.NewID(domain.PrefixEvent).String(),
		},
		OrganisationID: p.OrganisationID.String(),
		Email:          p.Email,
		Name:           p.Name,
		Role:           string(p.Role),
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString(s.secret)
	if err != nil {
		return "", fmt.Errorf("sign token: %w", err)
	}
	return signed, nil
}

// Verify parses and validates a bearer token.
func (s *Service) Verify(tokenString string) (*Principal, error) {
	claims := &Claims{}
	// The algorithm is pinned. Without this, a token presenting alg=none or an
	// asymmetric algorithm could be accepted with the wrong verification path.
	token, err := jwt.ParseWithClaims(tokenString, claims, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method %v", t.Header["alg"])
		}
		return s.secret, nil
	}, jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}), jwt.WithIssuer(s.issuer))
	if err != nil || !token.Valid {
		return nil, ErrInvalidToken
	}

	role := rbac.Role(claims.Role)
	if !role.Valid() {
		return nil, ErrInvalidToken
	}
	return &Principal{
		UserID:         domain.ID(claims.Subject),
		OrganisationID: domain.ID(claims.OrganisationID),
		Email:          claims.Email,
		Name:           claims.Name,
		Role:           role,
	}, nil
}

// --- API tokens --------------------------------------------------------------

// IssueAPIToken mints a long-lived token for the CLI and returns the plaintext exactly
// once. Only its SHA-256 is stored, so a database disclosure yields nothing usable.
func (s *Service) IssueAPIToken(ctx context.Context, userID domain.ID, name string, ttl time.Duration) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	plaintext := "nxs_" + base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(plaintext))

	var expires any
	if ttl > 0 {
		expires = time.Now().Add(ttl)
	}
	_, err := s.db.Exec(ctx,
		`INSERT INTO api_tokens (id, user_id, name, token_hash, prefix, expires_at)
		 VALUES ($1,$2,$3,$4,$5,$6)`,
		domain.NewID(domain.PrefixSecretRef), userID, name,
		hex.EncodeToString(sum[:]), plaintext[:12], expires)
	if err != nil {
		return "", fmt.Errorf("store token: %w", err)
	}
	return plaintext, nil
}

// VerifyAPIToken resolves a plaintext API token to its principal.
func (s *Service) VerifyAPIToken(ctx context.Context, plaintext string) (*Principal, error) {
	if !strings.HasPrefix(plaintext, "nxs_") {
		return nil, ErrInvalidToken
	}
	sum := sha256.Sum256([]byte(plaintext))

	var (
		tokenID domain.ID
		userID  domain.ID
		orgID   domain.ID
		email   string
		name    string
		role    string
	)
	err := s.db.QueryRow(ctx,
		`SELECT t.id, u.id, u.organisation_id, u.email, u.name,
		        COALESCE((SELECT m.role FROM memberships m
		                   WHERE m.user_id = u.id AND m.team_id IS NULL LIMIT 1), 'VIEWER')
		   FROM api_tokens t JOIN users u ON u.id = t.user_id
		  WHERE t.token_hash = $1
		    AND t.revoked_at IS NULL
		    AND (t.expires_at IS NULL OR t.expires_at > now())
		    AND u.is_active`, hex.EncodeToString(sum[:])).
		Scan(&tokenID, &userID, &orgID, &email, &name, &role)
	if err != nil {
		return nil, ErrInvalidToken
	}

	_, _ = s.db.Exec(ctx, `UPDATE api_tokens SET last_used_at = now() WHERE id = $1`, tokenID)
	return &Principal{
		UserID: userID, OrganisationID: orgID, Email: email,
		Name: name, Role: rbac.Role(role), TokenID: tokenID,
	}, nil
}

// --- context ------------------------------------------------------------------

type ctxKey int

const principalKey ctxKey = iota

func WithPrincipal(ctx context.Context, p *Principal) context.Context {
	return context.WithValue(ctx, principalKey, p)
}

func FromContext(ctx context.Context) (*Principal, bool) {
	p, ok := ctx.Value(principalKey).(*Principal)
	return p, ok && p != nil
}
