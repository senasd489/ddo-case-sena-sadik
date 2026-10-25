package auth

import (
	"crypto/subtle"
	"net/http"

	"github.com/senasd489/ddo-case-sena-sadik/backend/internal/httpx"
)

// IngestOrRoles accepts either the shared machine-to-machine X-Ingest-Token
// (simulator, meter head-end, call-centre integration) — the request then
// runs as the System principal — or a user JWT with one of roles.
func IngestOrRoles(s *Signer, token string, roles ...Role) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if tok := r.Header.Get("X-Ingest-Token"); tok != "" {
				if token == "" || subtle.ConstantTimeCompare([]byte(tok), []byte(token)) != 1 {
					httpx.WriteError(w, r, httpx.NewError(http.StatusUnauthorized, "invalid_ingest_token", "geçersiz ingest token"))
					return
				}
				next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), System)))
				return
			}
			Authenticate(s)(Require(roles...)(next)).ServeHTTP(w, r)
		})
	}
}
