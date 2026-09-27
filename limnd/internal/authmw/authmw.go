// Package authmw provides a minimal shared-secret auth check for LIMN's
// internal HTTP services. These services are meant to run on localhost
// only, but "meant to" isn't enforcement — anything else on the box (or
// a misconfigured bind address) can otherwise reach them. This closes
// that gap cheaply: one shared secret, checked in constant time.
package authmw

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// Require wraps a handler so it only runs when the request carries
// "Authorization: Bearer <secret>" matching the configured value.
func Require(secret string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		const prefix = "Bearer "
		if !strings.HasPrefix(auth, prefix) {
			http.Error(w, "missing bearer token", http.StatusUnauthorized)
			return
		}
		token := strings.TrimPrefix(auth, prefix)
		if subtle.ConstantTimeCompare([]byte(token), []byte(secret)) != 1 {
			http.Error(w, "invalid bearer token", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

// RequireEnv reads the given environment variable name via the provided
// getter and fails loudly (returns "", false) if it's unset or empty,
// rather than letting a caller silently fall back to running open.
func RequireEnv(getenv func(string) string, key string) (string, bool) {
	v := getenv(key)
	if v == "" {
		return "", false
	}
	return v, true
}
