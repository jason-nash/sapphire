package guest

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestK3sWorkloadReadiness(t *testing.T) {
	const readyNode = `{"metadata":{"name":"sapphire"},"status":{"conditions":[{"type":"NetworkUnavailable","status":"False"},{"type":"Ready","status":"True"}]}}`
	const defaultAccount = `{"metadata":{"name":"default","namespace":"default"}}`
	for _, test := range []struct {
		name          string
		readyStatus   int
		nodeStatus    int
		node          string
		accountStatus int
		account       string
		want          bool
	}{
		{name: "fully initialized", want: true},
		{name: "API unavailable", readyStatus: http.StatusServiceUnavailable},
		{name: "node not registered", nodeStatus: http.StatusNotFound},
		{name: "node not ready", node: `{"metadata":{"name":"sapphire"},"status":{"conditions":[{"type":"Ready","status":"False"}]}}`},
		{name: "node readiness unknown", node: `{"metadata":{"name":"sapphire"},"status":{"conditions":[{"type":"Ready","status":"Unknown"}]}}`},
		{name: "node has no ready condition", node: `{"metadata":{"name":"sapphire"},"status":{"conditions":[{"type":"NetworkUnavailable","status":"False"}]}}`},
		{name: "wrong node", node: `{"metadata":{"name":"other"},"status":{"conditions":[{"type":"Ready","status":"True"}]}}`},
		{name: "malformed node", node: `{`},
		{name: "service account not created", accountStatus: http.StatusNotFound},
		{name: "wrong service account namespace", account: `{"metadata":{"name":"default","namespace":"other"}}`},
		{name: "malformed service account", account: `{`},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				status, body := http.StatusOK, ""
				switch r.URL.Path {
				case "/readyz":
					status, body = test.readyStatus, "ok"
				case "/api/v1/nodes/sapphire":
					status, body = test.nodeStatus, test.node
					if body == "" {
						body = readyNode
					}
				case "/api/v1/namespaces/default/serviceaccounts/default":
					status, body = test.accountStatus, test.account
					if body == "" {
						body = defaultAccount
					}
				default:
					http.NotFound(w, r)
					return
				}
				if status == 0 {
					status = http.StatusOK
				}
				w.WriteHeader(status)
				fmt.Fprint(w, body)
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if got := k3sWorkloadReady(ctx, server.Client(), server.URL); got != test.want {
				t.Fatalf("ready = %v, want %v", got, test.want)
			}
		})
	}
}

func TestK3sWorkloadReadinessCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/readyz" {
			fmt.Fprint(w, "ok")
			return
		}
		cancel()
		<-r.Context().Done()
	}))
	defer server.Close()
	if k3sWorkloadReady(ctx, server.Client(), server.URL) {
		t.Fatal("cancelled readiness probe reported ready")
	}
}
