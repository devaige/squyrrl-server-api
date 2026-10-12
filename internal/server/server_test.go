package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/squyrrl/api/internal/config"
)

func TestEmbeddedEdgeRoutesAreUnavailable(t *testing.T) {
	srv := New(&config.Config{
		WebAuthnRPID:    "squyrrl.com",
		WebAuthnRPName:  "Squyrrl",
		WebAuthnOrigins: []string{"https://squyrrl.com"},
	}, nil, nil)

	for _, route := range []struct {
		method string
		path   string
	}{
		{http.MethodPut, "/edge/v1/single"},
		{http.MethodPut, "/edge/v1/part/1"},
		{http.MethodGet, "/edge/v1/blob"},
	} {
		t.Run(route.path, func(t *testing.T) {
			response := httptest.NewRecorder()
			srv.Handler().ServeHTTP(response, httptest.NewRequest(route.method, route.path, nil))
			if response.Code != http.StatusNotFound {
				t.Fatalf("开发专用边缘应返回 404，得到 %d", response.Code)
			}
		})
	}
}
