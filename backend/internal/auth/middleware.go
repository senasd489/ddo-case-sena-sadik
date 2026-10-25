package auth

import (
	"net/http"
	"strings"

	"github.com/senasd489/ddo-case-sena-sadik/backend/internal/httpx"
)

// Authenticate requires a valid Bearer access token; otherwise 401.
func Authenticate(s *Signer) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := r.Header.Get("Authorization")
			token, ok := strings.CutPrefix(h, "Bearer ")
			if !ok || token == "" {
				// EventSource başlık gönderemediği için SSE uçlarında ?access_token kabul edilir.
				token = r.URL.Query().Get("access_token")
			}
			if token == "" {
				httpx.WriteError(w, r, httpx.ErrUnauthorized)
				return
			}
			p, err := s.Verify(token)
			if err != nil {
				httpx.WriteError(w, r, httpx.NewError(http.StatusUnauthorized, "invalid_token", "erişim belirteci geçersiz veya süresi dolmuş"))
				return
			}
			next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
		})
	}
}

// Require allows only the given roles; everyone else gets 403. Admin is not
// implicitly allowed: each route lists exactly who may call it.
func Require(roles ...Role) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p := FromContext(r.Context())
			if p == nil {
				httpx.WriteError(w, r, httpx.ErrUnauthorized)
				return
			}
			if !p.Is(roles...) {
				httpx.WriteError(w, r, httpx.ErrForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
