package gateway

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// UserPrincipal is the verified identity attached to an authenticated request.
type UserPrincipal struct {
	UserID string
	Email  string
}

// userJWTClaims is the payload schema. Same shape as the Python issuer:
//
//	{"sub": user_id, "email": email, "iat": ..., "exp": ...}
type userJWTClaims struct {
	Email string `json:"email"`
	jwt.RegisteredClaims
}

// IssueUserToken mints a HS256-signed JWT for (userID, email) valid for ttl
// seconds. Returns an error if secret is empty.
func IssueUserToken(userID, email, secret string, ttl time.Duration) (string, error) {
	if secret == "" {
		return "", errors.New("USER_JWT_SECRET is empty — cannot issue tokens")
	}
	now := time.Now().UTC()
	claims := userJWTClaims{
		Email: email,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:  userID,
			IssuedAt: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
		},
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return tok.SignedString([]byte(secret))
}

// IssueUserTokenAt is IssueUserToken with an injectable clock for tests.
func IssueUserTokenAt(userID, email, secret string, ttl time.Duration, now time.Time) (string, error) {
	if secret == "" {
		return "", errors.New("USER_JWT_SECRET is empty — cannot issue tokens")
	}
	claims := userJWTClaims{
		Email: email,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   userID,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
		},
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return tok.SignedString([]byte(secret))
}

// VerifyUserToken parses + verifies the bearer token from an Authorization
// header. Returns (nil, status code, message) on failure. On success returns
// the principal; the caller writes nothing.
func VerifyUserToken(authzHeader, secret string) (*UserPrincipal, int, string) {
	if secret == "" {
		return nil, http.StatusServiceUnavailable, "user auth not configured"
	}
	tokenStr, ok := extractBearer(authzHeader)
	if !ok {
		return nil, http.StatusUnauthorized, "missing bearer token"
	}
	claims := &userJWTClaims{}
	parsed, err := jwt.ParseWithClaims(tokenStr, claims, func(t *jwt.Token) (any, error) {
		// Reject any non-HS256 algorithm (alg=none, RSA confused-deputy, …).
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, errors.New("unsupported signing method")
		}
		if t.Method.Alg() != "HS256" {
			return nil, errors.New("unsupported alg")
		}
		return []byte(secret), nil
	}, jwt.WithValidMethods([]string{"HS256"}))
	if err != nil {
		switch {
		case errors.Is(err, jwt.ErrTokenExpired):
			return nil, http.StatusUnauthorized, "token expired"
		case errors.Is(err, jwt.ErrTokenSignatureInvalid):
			return nil, http.StatusUnauthorized, "bad signature"
		default:
			return nil, http.StatusUnauthorized, "malformed token"
		}
	}
	if !parsed.Valid {
		return nil, http.StatusUnauthorized, "invalid token"
	}
	if claims.Subject == "" || claims.Email == "" {
		return nil, http.StatusUnauthorized, "malformed claims"
	}
	return &UserPrincipal{UserID: claims.Subject, Email: claims.Email}, 0, ""
}

// userPrincipalCtxKey is the typed context key for the request-scoped principal.
type userPrincipalCtxKey struct{}

// PrincipalFromContext returns the principal previously stored on ctx, or nil.
func PrincipalFromContext(ctx context.Context) *UserPrincipal {
	v, ok := ctx.Value(userPrincipalCtxKey{}).(*UserPrincipal)
	if !ok {
		return nil
	}
	return v
}

// UserAuthMiddleware verifies the request's bearer token against secret. On
// success it stores the principal on the request context.
func UserAuthMiddleware(secret string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			principal, code, msg := VerifyUserToken(r.Header.Get("Authorization"), secret)
			if principal == nil {
				if code == http.StatusUnauthorized {
					w.Header().Set("WWW-Authenticate", "Bearer")
				}
				writeJSONError(w, code, msg)
				return
			}
			ctx := context.WithValue(r.Context(), userPrincipalCtxKey{}, principal)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
