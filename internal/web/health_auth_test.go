package web

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHealthEndpointDoesNotRequireAdminAuthentication(t *testing.T) {
	s := &Server{adminPassword: "configured"}
	h := s.adminMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/health" {
			t.Fatalf("path = %q, want /api/health", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("GET /api/health status = %d, want 200", rr.Code)
	}
}

func TestProtectedAPIEndpointStillRequiresAdminAuthentication(t *testing.T) {
	s := &Server{adminPassword: "configured"}
	h := s.adminMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/accounts", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("GET /api/accounts status = %d, want 401", rr.Code)
	}
}
