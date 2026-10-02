package agent

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/inigogonzalezgarcia/11-gpu-fleet-lifecycle/internal/k8s"
)

func TestAgentCalls(t *testing.T) {
	installed := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/version":
			io.WriteString(w, `{"current":"r570-lab","upgrading":"r580-lab"}`)
		case "/health":
			io.WriteString(w, `{"xid":79,"dbe":0}`)
		case "/install":
			if r.Method != http.MethodPost {
				w.WriteHeader(405)
				return
			}
			installed = r.URL.Query().Get("version")
			w.WriteHeader(http.StatusAccepted)
		}
	}))
	defer srv.Close()
	host, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	p, _ := strconv.Atoi(port)
	a := NewHTTP(p)
	n := k8s.Node{Name: "n", InternalIP: host}
	v, err := a.Version(n)
	if err != nil || v.Current != "r570-lab" || v.Upgrading != "r580-lab" {
		t.Fatalf("version: %+v %v", v, err)
	}
	h, err := a.Health(n)
	if err != nil || h.XID != 79 {
		t.Fatalf("health: %+v %v", h, err)
	}
	if err := a.Install(n, "r580-lab"); err != nil || installed != "r580-lab" {
		t.Fatalf("install: %v %q", err, installed)
	}
	if _, err := a.Version(k8s.Node{Name: "x"}); err == nil {
		t.Fatal("node without IP must fail")
	}
}
