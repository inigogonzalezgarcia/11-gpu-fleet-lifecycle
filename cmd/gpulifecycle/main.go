// Command gpulifecycle runs the fleet rollout controller inside a Kubernetes cluster.
// The rollout to run is the plan in the rollout-plan ConfigMap; see scripts/rollout.sh.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/inigogonzalezgarcia/11-gpu-fleet-lifecycle/internal/agent"
	"github.com/inigogonzalezgarcia/11-gpu-fleet-lifecycle/internal/k8s"
	"github.com/inigogonzalezgarcia/11-gpu-fleet-lifecycle/internal/rollout"
)

var version = "dev"

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	var cfg rollout.Config
	flag.StringVar(&cfg.Namespace, "namespace", env("POD_NAMESPACE", "gpu-lifecycle"), "namespace for ConfigMaps and validation Jobs")
	flag.StringVar(&cfg.PlanConfigMap, "plan-configmap", "rollout-plan", "ConfigMap with plan.json, control and approve")
	flag.StringVar(&cfg.StatusConfigMap, "status-configmap", "rollout-status", "ConfigMap the controller writes status.json to")
	flag.StringVar(&cfg.EventNamespace, "event-namespace", "default", "namespace for Node events")
	flag.StringVar(&cfg.ValidationImage, "validation-image", "busybox:1.37", "image for validation Jobs")
	flag.IntVar(&cfg.ValidationGPUs, "validation-gpus", 0, "nvidia.com/gpu each validation Job requests")
	flag.IntVar(&cfg.AgentPort, "agent-port", 9500, "port of the node agent")
	interval := flag.Duration("interval", 10*time.Second, "time between passes")
	listen := flag.String("listen", ":8080", "address for /metrics and /healthz")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	client, err := k8s.InCluster()
	if err != nil {
		log.Error("kubernetes client", "error", err)
		os.Exit(1)
	}
	c := rollout.New(cfg, client, agent.NewHTTP(cfg.AgentPort), log)

	var passes, failures, last atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		if t := last.Load(); t > 0 && time.Since(time.Unix(t, 0)) > 4**interval+time.Minute {
			http.Error(w, "reconcile loop is stuck", http.StatusServiceUnavailable)
			return
		}
		io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		st, actions := c.Snapshot()
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		fmt.Fprintln(w, "# HELP gpu_lifecycle_rollout_phase Current phase of the rollout (1 = current).")
		fmt.Fprintln(w, "# TYPE gpu_lifecycle_rollout_phase gauge")
		for _, ph := range []string{rollout.PhaseInvalid, rollout.PhaseCanary, rollout.PhaseAwaitingApproval, rollout.PhaseRolling,
			rollout.PhasePaused, rollout.PhaseWaitingForWindow, rollout.PhaseHalted, rollout.PhaseAborted, rollout.PhaseComplete} {
			v := 0
			if st.Phase == ph {
				v = 1
			}
			fmt.Fprintf(w, "gpu_lifecycle_rollout_phase{rollout=%q,target=%q,phase=%q} %d\n", st.Rollout, st.Target, ph, v)
		}
		fmt.Fprintln(w, "# HELP gpu_lifecycle_nodes Nodes in the current rollout by status.")
		fmt.Fprintln(w, "# TYPE gpu_lifecycle_nodes gauge")
		for _, kv := range []struct {
			k string
			n int
		}{{"in_progress", len(st.InProgress)}, {"done", len(st.Done)}, {"pending", len(st.Pending)}, {"failed", len(st.Failed)}, {"skipped", len(st.Skipped)}} {
			fmt.Fprintf(w, "gpu_lifecycle_nodes{status=%q} %d\n", kv.k, kv.n)
		}
		fmt.Fprintln(w, "# HELP gpu_lifecycle_actions_total Actions taken by the controller.")
		fmt.Fprintln(w, "# TYPE gpu_lifecycle_actions_total counter")
		for _, a := range []string{"done", "evict", "halt", "install", "quarantine", "rollback", "rolled_back", "start"} {
			fmt.Fprintf(w, "gpu_lifecycle_actions_total{action=%q} %d\n", a, actions[a])
		}
		fmt.Fprintf(w, "# TYPE gpu_lifecycle_reconcile_total counter\ngpu_lifecycle_reconcile_total %d\n", passes.Load())
		fmt.Fprintf(w, "# TYPE gpu_lifecycle_reconcile_errors_total counter\ngpu_lifecycle_reconcile_errors_total %d\n", failures.Load())
	})
	srv := &http.Server{Addr: *listen, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("metrics server", "error", err)
			os.Exit(1)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	log.Info("starting", "version", version, "namespace", cfg.Namespace, "plan", cfg.PlanConfigMap)
	t := time.NewTicker(*interval)
	defer t.Stop()
	for {
		passes.Add(1)
		if err := c.Reconcile(); err != nil {
			failures.Add(1)
			log.Error("reconcile", "error", strings.ReplaceAll(err.Error(), "\n", "; "))
		}
		last.Store(time.Now().Unix())
		select {
		case <-ctx.Done():
			sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = srv.Shutdown(sctx)
			return
		case <-t.C:
		}
	}
}
