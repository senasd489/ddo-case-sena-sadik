package auth

import (
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/senasd489/ddo-case-sena-sadik/backend/internal/httpx"
)

type Handlers struct{ svc *Service }

func NewHandlers(svc *Service) *Handlers { return &Handlers{svc: svc} }

// Public routes: login / refresh / logout.
func (h *Handlers) Mount(r chi.Router) {
	r.Post("/auth/login", httpx.H(h.login))
	r.Post("/auth/refresh", httpx.H(h.refresh))
	r.Post("/auth/logout", httpx.H(h.logout))
}

type loginReq struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

func (h *Handlers) login(w http.ResponseWriter, r *http.Request) error {
	var req loginReq
	if err := httpx.Decode(r, &req); err != nil {
		return err
	}
	details := map[string]any{}
	if !strings.Contains(req.Email, "@") || len(req.Email) > 254 {
		details["email"] = "geçerli bir e-posta girin"
	}
	if req.Password == "" || len(req.Password) > 256 {
		details["password"] = "parola gerekli"
	}
	if len(details) > 0 {
		return httpx.Validation(details)
	}
	t, err := h.svc.Login(r.Context(), req.Email, req.Password)
	if errors.Is(err, ErrBadCredentials) {
		return httpx.NewError(http.StatusUnauthorized, "invalid_credentials", "e-posta veya parola hatalı")
	}
	if err != nil {
		return err
	}
	httpx.LoggerFrom(r.Context()).Info("login", "user_id", t.User.UserID, "role", t.User.Role)
	httpx.JSON(w, http.StatusOK, t)
	return nil
}

type refreshReq struct {
	RefreshToken string `json:"refresh_token"`
}

func (h *Handlers) refresh(w http.ResponseWriter, r *http.Request) error {
	var req refreshReq
	if err := httpx.Decode(r, &req); err != nil {
		return err
	}
	if req.RefreshToken == "" {
		return httpx.Validation(map[string]any{"refresh_token": "gerekli"})
	}
	t, err := h.svc.Refresh(r.Context(), req.RefreshToken)
	switch {
	case errors.Is(err, ErrRefreshReused):
		return httpx.NewError(http.StatusUnauthorized, "refresh_reused", "yenileme belirteci daha önce kullanılmış; tüm oturumlar kapatıldı")
	case errors.Is(err, ErrRefreshInvalid):
		return httpx.NewError(http.StatusUnauthorized, "refresh_invalid", "yenileme belirteci geçersiz")
	case err != nil:
		return err
	}
	httpx.JSON(w, http.StatusOK, t)
	return nil
}

func (h *Handlers) logout(w http.ResponseWriter, r *http.Request) error {
	var req refreshReq
	if err := httpx.Decode(r, &req); err != nil {
		return err
	}
	if err := h.svc.Logout(r.Context(), req.RefreshToken); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// Me returns the caller (mounted behind Authenticate).
func Me(w http.ResponseWriter, r *http.Request) {
	httpx.JSON(w, http.StatusOK, FromContext(r.Context()))
}
