package storage

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestEnsureBucketRequiresExistingBucket(t *testing.T) {
	for _, test := range []struct {
		name    string
		status  int
		wantErr bool
	}{
		{"已预建", http.StatusOK, false},
		{"不存在", http.StatusNotFound, true},
		{"无权限", http.StatusForbidden, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet && r.URL.Query().Has("location") {
					w.Header().Set("Content-Type", "application/xml")
					_, _ = w.Write([]byte(`<LocationConstraint xmlns="http://s3.amazonaws.com/doc/2006-03-01/"/>`))
					return
				}
				if r.Method != http.MethodHead || r.URL.Path != "/squyrrl/" {
					t.Errorf("只应检查桶是否存在，收到 %s %s", r.Method, r.URL)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				w.WriteHeader(test.status)
			}))
			defer origin.Close()

			client, err := New(strings.TrimPrefix(origin.URL, "http://"), "test-access", "test-secret", "squyrrl", false)
			if err != nil {
				t.Fatal(err)
			}
			if err := client.EnsureBucket(context.Background()); (err != nil) != test.wantErr {
				t.Fatalf("EnsureBucket() err=%v, wantErr=%v", err, test.wantErr)
			}
		})
	}
}
