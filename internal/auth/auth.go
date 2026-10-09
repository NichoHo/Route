// Package auth holds Route's two credentials: one shared bearer token for the
// mutating HTTP endpoints, and per-courier tokens for gRPC sessions.
//
// Courier tokens are HMACs of the courier id, keyed from the bearer token.
// The simulator asks routed for each token at start (POST /couriers/{id}/token,
// bearer-protected), so the signing key never leaves the server and a token
// only works for the one id it was issued for.
package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"strings"

	"google.golang.org/grpc/metadata"
)

const (
	// MetaCourierID and MetaAuthorization are the gRPC metadata keys a
	// courier session presents.
	MetaCourierID     = "x-courier-id"
	MetaAuthorization = "authorization"

	// MaxCourierIDLen bounds courier ids, which become Redis key segments.
	MaxCourierIDLen = 64

	// MinTokenLen is the shortest API token routed will start with.
	MinTokenLen = 16
)

type Auth struct {
	apiToken   string
	courierKey []byte
}

func New(apiToken string) *Auth {
	mac := hmac.New(sha256.New, []byte(apiToken))
	mac.Write([]byte("route-courier-token-v1"))
	return &Auth{apiToken: apiToken, courierKey: mac.Sum(nil)}
}

// Bearer returns the request's bearer token and whether it is the valid API
// token. Callers that need a per-token key (rate limiting) use the returned
// token only when ok is true.
func (a *Auth) Bearer(r *http.Request) (token string, ok bool) {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", false
	}
	token = h[len(prefix):]
	return token, subtle.ConstantTimeCompare([]byte(token), []byte(a.apiToken)) == 1
}

// CourierToken issues the token for one courier id.
func (a *Auth) CourierToken(courierID string) string {
	mac := hmac.New(sha256.New, a.courierKey)
	mac.Write([]byte(courierID))
	return hex.EncodeToString(mac.Sum(nil))
}

// VerifyCourier reports whether token is the valid token for courierID.
func (a *Auth) VerifyCourier(courierID, token string) bool {
	return ValidCourierID(courierID) && hmac.Equal([]byte(token), []byte(a.CourierToken(courierID)))
}

// ValidCourierID restricts ids to a safe charset: they become Redis key
// segments and appear in logs.
func ValidCourierID(id string) bool {
	if id == "" || len(id) > MaxCourierIDLen {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_', c == '.':
		default:
			return false
		}
	}
	return true
}

// CourierMetadata builds the gRPC metadata a courier session presents.
func CourierMetadata(courierID, token string) metadata.MD {
	return metadata.Pairs(MetaCourierID, courierID, MetaAuthorization, "Bearer "+token)
}

// CourierCredentials reads the courier id and token from an incoming gRPC
// context.
func CourierCredentials(ctx context.Context) (courierID, token string, ok bool) {
	md, found := metadata.FromIncomingContext(ctx)
	if !found {
		return "", "", false
	}
	ids, authz := md.Get(MetaCourierID), md.Get(MetaAuthorization)
	if len(ids) != 1 || len(authz) != 1 || !strings.HasPrefix(authz[0], "Bearer ") {
		return "", "", false
	}
	return ids[0], strings.TrimPrefix(authz[0], "Bearer "), true
}
