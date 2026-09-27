//go:build linux

package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestManagerRoutesRegisterWithoutConflict(t *testing.T) {
	m := &manager{}
	h := m.routes()

	root := httptest.NewRecorder()
	h.ServeHTTP(root, httptest.NewRequest(http.MethodGet, "/", nil))
	if root.Code != http.StatusOK {
		t.Fatalf("GET / status=%d", root.Code)
	}

	api := httptest.NewRecorder()
	h.ServeHTTP(api, httptest.NewRequest(http.MethodGet, "/api/v1/status", nil))
	if api.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated API status=%d", api.Code)
	}
}
