// Command node-agent is the lab's stand-in for whatever installs drivers and firmware on a GPU node
// (the GPU Operator's driver DaemonSet, a firmware tool, a provisioning system). It runs on every
// GPU node with hostNetwork and never touches real hardware.
//
//	GET  /version                {"current":"r570-lab","upgrading":"r580-lab"}
//	POST /install?version=X      start installing X (takes -install-delay); 202 Accepted
//	GET  /validate               200 if the installed version works, 500 if it is one of -bad-versions
//	GET  /health                 {"xid":79,"dbe":0} once a version from -xid-versions has run for a while
//	POST /reset                  back to the initial version
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

type node struct {
	mu        sync.Mutex
	initial   string
	current   string
	upgrading string
	readyAt   time.Time
	installed time.Time
	delay     time.Duration
	bad, xid  map[string]bool
	now       func() time.Time
}

func set(csv string) map[string]bool {
	m := map[string]bool{}
	for _, v := range strings.Split(csv, ",") {
		if v = strings.TrimSpace(v); v != "" {
			m[v] = true
		}
	}
	return m
}

// tick finishes an install whose time has come.
func (n *node) tick() {
	if n.upgrading != "" && !n.now().Before(n.readyAt) {
		slog.Info("installed", "version", n.upgrading, "previous", n.current)
		n.current, n.upgrading, n.installed = n.upgrading, "", n.now()
	}
}

func (n *node) handler() http.Handler {
	mux := http.NewServeMux()
	writeJSON := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprintln(w, "ok") })
	mux.HandleFunc("/version", func(w http.ResponseWriter, _ *http.Request) {
		n.mu.Lock()
		defer n.mu.Unlock()
		n.tick()
		writeJSON(w, map[string]string{"current": n.current, "upgrading": n.upgrading})
	})
	mux.HandleFunc("/install", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		v := r.URL.Query().Get("version")
		if v == "" {
			http.Error(w, "version is required", http.StatusBadRequest)
			return
		}
		n.mu.Lock()
		defer n.mu.Unlock()
		n.tick()
		if v != n.upgrading && v != n.current {
			n.upgrading, n.readyAt = v, n.now().Add(n.delay)
			slog.Info("install requested", "version", v, "current", n.current)
		}
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprintf(w, "installing %s\n", v)
	})
	mux.HandleFunc("/validate", func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(2 * time.Second) // stands in for a burn-in
		n.mu.Lock()
		defer n.mu.Unlock()
		n.tick()
		if n.upgrading != "" || n.bad[n.current] {
			slog.Warn("validation failed", "version", n.current, "upgrading", n.upgrading)
			http.Error(w, "FAIL "+n.current, http.StatusInternalServerError)
			return
		}
		fmt.Fprintln(w, "PASS", n.current)
	})
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		n.mu.Lock()
		defer n.mu.Unlock()
		n.tick()
		h := map[string]any{"xid": 0, "dbe": 0}
		// A version that "passes validation but crashes under load": XID 79 a few seconds in.
		if n.xid[n.current] && n.now().Sub(n.installed) > 5*time.Second {
			h["xid"] = 79
		}
		writeJSON(w, h)
	})
	mux.HandleFunc("/reset", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		n.mu.Lock()
		defer n.mu.Unlock()
		n.current, n.upgrading = n.initial, ""
		fmt.Fprintln(w, "reset to", n.initial)
	})
	return mux
}

func main() {
	listen := flag.String("listen", ":9500", "address to listen on")
	initial := flag.String("version", "r570-lab", "version installed at start")
	delay := flag.Duration("install-delay", 4*time.Second, "how long an install takes")
	bad := flag.String("bad-versions", "", "comma-separated versions that fail validation")
	xid := flag.String("xid-versions", "", "comma-separated versions that report XID 79 after a few seconds")
	flag.Parse()
	n := &node{initial: *initial, current: *initial, delay: *delay, bad: set(*bad), xid: set(*xid), now: time.Now}
	slog.Info("node-agent listening", "addr", *listen, "version", *initial, "node", os.Getenv("NODE_NAME"))
	srv := &http.Server{Addr: *listen, Handler: n.handler(), ReadHeaderTimeout: 5 * time.Second}
	if err := srv.ListenAndServe(); err != nil {
		slog.Error("listen", "error", err)
		os.Exit(1)
	}
}
