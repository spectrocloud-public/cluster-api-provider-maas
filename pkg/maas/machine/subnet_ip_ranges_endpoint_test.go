package machine

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/spectrocloud/maas-client-go/maasclient"
)

// TestSubnetIPRangeEndpoints pins the HTTP shape of the two subnet IP-range calls.
//
// The pre-compose static IP check calls GetUnreservedIPRanges. maas-client-go before
// v0.1.8-beta1 requested /subnets/{id}/unreserved_ip_ranges as a path, which MAAS answers
// with 404, and the check fails closed, so every static-IP compose was refused. The client
// now uses the subnet operation form, /subnets/{id}/?op=..., and this test keeps the
// replace directive in go.mod honest across dependency bumps.
func TestSubnetIPRangeEndpoints(t *testing.T) {
	tests := []struct {
		name string
		call func(context.Context, maasclient.ClientSetInterface) error
		op   string
	}{
		{
			name: "unreserved ip ranges",
			call: func(ctx context.Context, clientSet maasclient.ClientSetInterface) error {
				_, err := clientSet.Subnets().GetUnreservedIPRanges(ctx, 1)
				return err
			},
			op: "unreserved_ip_ranges",
		},
		{
			name: "reserved ip ranges",
			call: func(ctx context.Context, clientSet maasclient.ClientSetInterface) error {
				_, err := clientSet.Subnets().GetReservedIPRanges(ctx, 1)
				return err
			},
			op: "reserved_ip_ranges",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var (
				mu       sync.Mutex
				gotPath  string
				gotQuery string
			)

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				gotPath = r.URL.Path
				gotQuery = r.URL.Query().Get("op")
				mu.Unlock()

				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte("[]"))
			}))
			defer server.Close()

			clientSet := maasclient.NewAuthenticatedClientSet(server.URL, "consumer:token:secret")
			if err := tt.call(context.Background(), clientSet); err != nil {
				t.Fatalf("%s: call failed: %v", tt.name, err)
			}

			mu.Lock()
			defer mu.Unlock()

			if gotPath != "/api/2.0/subnets/1/" {
				t.Errorf("%s: request path = %q, want %q", tt.name, gotPath, "/api/2.0/subnets/1/")
			}
			if gotQuery != tt.op {
				t.Errorf("%s: op query = %q, want %q", tt.name, gotQuery, tt.op)
			}
		})
	}
}
