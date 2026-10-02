package rollout

import (
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/inigogonzalezgarcia/11-gpu-fleet-lifecycle/internal/agent"
	"github.com/inigogonzalezgarcia/11-gpu-fleet-lifecycle/internal/k8s"
)

// --- fakes --------------------------------------------------------------------------------------

type fakeAgent struct {
	current   map[string]string
	upgrading map[string]string
	bad       map[string]bool // versions whose validation fails
	xid       map[string]bool // versions that throw XID 79 while soaking
	badBack   bool            // rollback validation fails too
	installs  []string
}

func (a *fakeAgent) Version(n k8s.Node) (agent.Version, error) {
	// An install completes on the first Version call after it was requested.
	if u := a.upgrading[n.Name]; u != "" {
		a.current[n.Name], a.upgrading[n.Name] = u, ""
	}
	return agent.Version{Current: a.current[n.Name]}, nil
}

func (a *fakeAgent) Health(n k8s.Node) (agent.Health, error) {
	if a.xid[a.current[n.Name]] {
		return agent.Health{XID: 79}, nil
	}
	return agent.Health{}, nil
}

func (a *fakeAgent) Install(n k8s.Node, v string) error {
	a.upgrading[n.Name] = v
	a.installs = append(a.installs, n.Name+"="+v)
	return nil
}

type fakeCluster struct {
	nodes   map[string]*k8s.Node
	pods    map[string][]k8s.Pod
	blocked map[string]bool
	jobs    map[string]string // job -> node
	cms     map[string]map[string]string
	agent   *fakeAgent
	// every time a node is cordoned/uncordoned we record how many are cordoned by the rollout
	maxOut int
}

func (f *fakeCluster) ListNodes(string) ([]k8s.Node, error) {
	var out []k8s.Node
	for _, n := range f.nodes {
		c := *n
		c.Annotations, c.Labels = map[string]string{}, map[string]string{}
		for k, v := range n.Annotations {
			c.Annotations[k] = v
		}
		for k, v := range n.Labels {
			c.Labels[k] = v
		}
		out = append(out, c)
	}
	return out, nil
}

func (f *fakeCluster) PatchNode(name string, patch map[string]any) error {
	n := f.nodes[name]
	if md, ok := patch["metadata"].(map[string]any); ok {
		for field, target := range map[string]map[string]string{"annotations": n.Annotations, "labels": n.Labels} {
			if m, ok := md[field].(map[string]any); ok {
				for k, v := range m {
					if v == nil {
						delete(target, k)
					} else {
						target[k] = v.(string)
					}
				}
			}
		}
	}
	if spec, ok := patch["spec"].(map[string]any); ok {
		n.Unschedulable = spec["unschedulable"].(bool)
	}
	out := 0
	for _, x := range f.nodes {
		if x.Unschedulable {
			out++
		}
	}
	if out > f.maxOut {
		f.maxOut = out
	}
	return nil
}

func (f *fakeCluster) PodsOnNode(node string) ([]k8s.Pod, error) { return f.pods[node], nil }

func (f *fakeCluster) Evict(ns, name string) error {
	if f.blocked[name] {
		return k8s.ErrBlocked
	}
	for node, pods := range f.pods {
		for i, p := range pods {
			if p.Name == name {
				f.pods[node] = append(pods[:i:i], pods[i+1:]...)
			}
		}
	}
	return nil
}

func (f *fakeCluster) CreateJob(ns string, job map[string]any) error {
	name := job["metadata"].(map[string]any)["name"].(string)
	node := job["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["nodeName"].(string)
	f.jobs[name] = node
	return nil
}

// JobState: validation passes unless the node runs a bad version (or any version, for badBack rollbacks).
func (f *fakeCluster) JobState(ns, name string) (bool, bool, error) {
	node, ok := f.jobs[name]
	if !ok {
		return false, false, k8s.ErrNotFound
	}
	v := f.agent.current[node]
	fail := f.agent.bad[v] || (f.agent.badBack && f.nodes[node].Annotations[AnnState] == RollbackValidating)
	return !fail, fail, nil
}

func (f *fakeCluster) DeleteJob(ns, name string) error                   { delete(f.jobs, name); return nil }
func (f *fakeCluster) NodeEvent(ns, node, typ, reason, msg string) error { return nil }

func (f *fakeCluster) GetConfigMapData(ns, name string) (map[string]string, error) {
	d, ok := f.cms[name]
	if !ok {
		return nil, k8s.ErrNotFound
	}
	out := map[string]string{}
	for k, v := range d {
		out[k] = v
	}
	return out, nil
}

func (f *fakeCluster) PutConfigMapData(ns, name string, data map[string]string) error {
	if f.cms[name] == nil {
		f.cms[name] = map[string]string{}
	}
	for k, v := range data {
		f.cms[name][k] = v
	}
	return nil
}

// --- harness ------------------------------------------------------------------------------------

type harness struct {
	f   *fakeCluster
	a   *fakeAgent
	c   *Controller
	now time.Time
}

func setup(t *testing.T, nodes ...string) *harness {
	t.Helper()
	a := &fakeAgent{current: map[string]string{}, upgrading: map[string]string{}, bad: map[string]bool{"r580-lab-bad": true}, xid: map[string]bool{"r580-lab-xid": true}}
	f := &fakeCluster{nodes: map[string]*k8s.Node{}, pods: map[string][]k8s.Pod{}, blocked: map[string]bool{}, jobs: map[string]string{},
		cms: map[string]map[string]string{}, agent: a}
	for _, n := range nodes {
		f.nodes[n] = &k8s.Node{Name: n, Labels: map[string]string{}, Annotations: map[string]string{}, InternalIP: "10.0.0.1"}
		a.current[n] = "r570-lab"
	}
	h := &harness{f: f, a: a, now: time.Date(2026, 10, 2, 23, 0, 0, 0, time.UTC)}
	h.c = New(Config{Namespace: "gpu-lifecycle", PlanConfigMap: "rollout-plan", StatusConfigMap: "rollout-status",
		EventNamespace: "default", ValidationImage: "busybox", ValidationGPUs: 8, AgentPort: 9500},
		f, a, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.c.now = func() time.Time { return h.now }
	return h
}

func (h *harness) plan(t *testing.T, extra string, data ...string) {
	t.Helper()
	p := `{"name":"r580","selector":"gpu=true","target":"r580-lab","canary":1,"batchSize":2,"maxUnavailable":2,"soak":"30s"` + extra + `}`
	h.f.cms["rollout-plan"] = map[string]string{"plan.json": p}
	for i := 0; i+1 < len(data); i += 2 {
		h.f.cms["rollout-plan"][data[i]] = data[i+1]
	}
}

func (h *harness) pass(t *testing.T) Status {
	t.Helper()
	if err := h.c.Reconcile(); err != nil {
		t.Fatal(err)
	}
	h.now = h.now.Add(10 * time.Second)
	var st Status
	_ = json.Unmarshal([]byte(h.f.cms["rollout-status"]["status.json"]), &st)
	return st
}

func (h *harness) run(t *testing.T, passes int) Status {
	var st Status
	for i := 0; i < passes; i++ {
		st = h.pass(t)
	}
	return st
}

func (h *harness) state(n string) string { return h.f.nodes[n].Annotations[AnnState] }

// --- tests --------------------------------------------------------------------------------------

func TestCanaryThenBatches(t *testing.T) {
	h := setup(t, "n1", "n2", "n3", "n4", "n5")
	h.plan(t, "")
	st := h.pass(t)
	if st.Phase != PhaseCanary || len(st.Canary) != 1 || st.Canary[0] != "n1" || h.state("n1") != Draining || h.state("n2") != "" {
		t.Fatalf("first pass should start only the canary: %+v", st)
	}
	for i := 0; i < 30 && h.state("n1") != Done; i++ {
		if h.state("n2") != "" {
			t.Fatal("no other node may start before the canary is done")
		}
		h.pass(t)
	}
	st = h.run(t, 40)
	if st.Phase != PhaseComplete {
		t.Fatalf("want Complete, got %+v", st)
	}
	for _, n := range []string{"n1", "n2", "n3", "n4", "n5"} {
		if h.a.current[n] != "r580-lab" || h.f.nodes[n].Unschedulable || h.f.nodes[n].Labels[LabelVersion] != "r580-lab" {
			t.Fatalf("%s not upgraded and back in service", n)
		}
	}
	if h.f.maxOut > 2 {
		t.Fatalf("maxUnavailable is 2, saw %d nodes out", h.f.maxOut)
	}
}

func TestSoakTakesTheConfiguredTime(t *testing.T) {
	h := setup(t, "n1")
	h.plan(t, `,"soak":"60s"`)
	for i := 0; i < 20 && h.state("n1") != Soaking; i++ {
		h.pass(t)
	}
	soakStart := h.now
	for h.state("n1") == Soaking {
		h.pass(t)
	}
	if got := h.now.Sub(soakStart); got < 60*time.Second {
		t.Fatalf("soaked only %s", got)
	}
}

func TestBadVersionHaltsAtTheCanary(t *testing.T) {
	h := setup(t, "n1", "n2", "n3")
	h.plan(t, `,"name":"bad","target":"r580-lab-bad"`)
	st := h.run(t, 30)
	if st.Phase != PhaseHalted || !strings.Contains(st.Message, "canary n1 failed") {
		t.Fatalf("want Halted on the canary, got %+v", st)
	}
	if h.state("n1") != RolledBack || h.a.current["n1"] != "r570-lab" || h.f.nodes["n1"].Unschedulable {
		t.Fatalf("canary should be rolled back and in service: %s %s", h.state("n1"), h.a.current["n1"])
	}
	for _, n := range []string{"n2", "n3"} {
		if h.state(n) != "" || h.a.current[n] != "r570-lab" {
			t.Fatalf("%s must not be touched after a canary failure", n)
		}
	}
	if !strings.Contains(h.f.nodes["n1"].Annotations[AnnReason], "validation failed") {
		t.Fatalf("reason: %q", h.f.nodes["n1"].Annotations[AnnReason])
	}
}

func TestXIDWhileSoakingRollsBack(t *testing.T) {
	h := setup(t, "n1")
	h.plan(t, `,"target":"r580-lab-xid"`)
	h.run(t, 20)
	if h.state("n1") != RolledBack || !strings.Contains(h.f.nodes["n1"].Annotations[AnnReason], "XID 79") {
		t.Fatalf("want RolledBack for XID while soaking, got %s %q", h.state("n1"), h.f.nodes["n1"].Annotations[AnnReason])
	}
}

func TestFailedRollbackQuarantines(t *testing.T) {
	h := setup(t, "n1")
	h.a.badBack = true
	h.plan(t, `,"target":"r580-lab-bad"`)
	h.run(t, 20)
	n := h.f.nodes["n1"]
	if h.state("n1") != Quarantined || n.Labels[LabelQuarantine] != "true" || !n.Unschedulable {
		t.Fatalf("want quarantined and cordoned, got %s %v", h.state("n1"), n.Labels)
	}
}

func TestPauseAfterCanaryNeedsApproval(t *testing.T) {
	h := setup(t, "n1", "n2", "n3")
	h.plan(t, `,"pauseAfterCanary":true`)
	st := h.run(t, 20)
	if st.Phase != PhaseAwaitingApproval || h.state("n2") != "" {
		t.Fatalf("want AwaitingApproval with nothing else started, got %+v", st)
	}
	h.f.cms["rollout-plan"]["approve"] = "some-other-rollout"
	if st = h.run(t, 3); st.Phase != PhaseAwaitingApproval {
		t.Fatal("an approval for another rollout must not count")
	}
	h.f.cms["rollout-plan"]["approve"] = "r580"
	if st = h.run(t, 40); st.Phase != PhaseComplete {
		t.Fatalf("want Complete after approval, got %+v", st)
	}
}

func TestOutsideWindowNothingStarts(t *testing.T) {
	h := setup(t, "n1", "n2")
	h.plan(t, `,"window":{"start":"01:00","end":"05:00"}`) // harness clock is 23:00 UTC
	st := h.run(t, 3)
	if st.Phase != PhaseWaitingForWindow || h.state("n1") != "" {
		t.Fatalf("want WaitingForWindow, got %+v", st)
	}
	h.now = time.Date(2026, 10, 3, 1, 30, 0, 0, time.UTC)
	if st = h.pass(t); st.Phase != PhaseCanary || h.state("n1") != Draining {
		t.Fatalf("window open: canary should start, got %+v", st)
	}
}

func TestPauseAndAbort(t *testing.T) {
	h := setup(t, "n1", "n2", "n3")
	h.plan(t, `,"canary":0,"batchSize":1,"maxUnavailable":1`, "control", "pause")
	if st := h.run(t, 3); st.Phase != PhasePaused || h.state("n1") != "" {
		t.Fatalf("paused rollout must not start nodes: %+v", st)
	}
	h.f.cms["rollout-plan"]["control"] = "run"
	h.pass(t)
	h.f.cms["rollout-plan"]["control"] = "abort"
	st := h.run(t, 20)
	if st.Phase != PhaseAborted || h.state("n1") != Done || h.state("n2") != "" {
		t.Fatalf("abort: the node in progress finishes, nothing new starts: %+v n1=%s", st, h.state("n1"))
	}
}

func TestDrainWaitsForPDB(t *testing.T) {
	h := setup(t, "n1")
	h.f.pods["n1"] = []k8s.Pod{
		{Namespace: "ml", Name: "trainer-0", Phase: "Running", OwnerKinds: []string{"ReplicaSet"}},
		{Namespace: "kube-system", Name: "agent-x", Phase: "Running", OwnerKinds: []string{"DaemonSet"}},
	}
	h.f.blocked["trainer-0"] = true
	h.plan(t, "")
	h.run(t, 5)
	if h.state("n1") != Draining || len(h.a.installs) != 0 {
		t.Fatalf("install must wait for the drain: %s %v", h.state("n1"), h.a.installs)
	}
	h.f.blocked["trainer-0"] = false
	h.run(t, 20)
	if h.state("n1") != Done {
		t.Fatalf("want Done once the PDB allowed the eviction, got %s", h.state("n1"))
	}
}

func TestSkipsNodesOthersOwnAndCountsThemAgainstTheBudget(t *testing.T) {
	h := setup(t, "n1", "n2", "n3", "n4")
	h.f.nodes["n1"].Labels[LabelQuarantine] = "true"
	h.f.nodes["n1"].Unschedulable = true
	h.f.nodes["n2"].Annotations[AnnRemediation] = "Draining"
	h.f.nodes["n2"].Unschedulable = true
	h.plan(t, `,"canary":0,"batchSize":2,"maxUnavailable":2`)
	st := h.pass(t)
	if len(st.Skipped) != 2 || h.state("n3") != "" || h.state("n4") != "" {
		t.Fatalf("2 nodes already out with maxUnavailable 2: nothing may start, got %+v", st)
	}
	delete(h.f.nodes["n2"].Annotations, AnnRemediation)
	h.f.nodes["n2"].Unschedulable = false
	st = h.run(t, 40)
	if st.Phase != PhaseComplete || h.state("n1") != "" || h.a.current["n1"] != "r570-lab" {
		t.Fatalf("quarantined node must be left alone: %+v", st)
	}
}

func TestUpToDateNodesAreDoneAndInvalidPlansReported(t *testing.T) {
	h := setup(t, "n1")
	h.a.current["n1"] = "r580-lab"
	h.plan(t, "")
	if st := h.pass(t); st.Phase != PhaseComplete || h.state("n1") != "" {
		t.Fatalf("nothing to do: %+v", st)
	}
	h.f.cms["rollout-plan"]["plan.json"] = `{"name":"x"}`
	if st := h.pass(t); st.Phase != PhaseInvalid || !strings.Contains(st.Message, "selector is required") {
		t.Fatalf("invalid plan should be reported: %+v", st)
	}
}

func TestFailureBudget(t *testing.T) {
	h := setup(t, "n1", "n2", "n3", "n4")
	h.plan(t, `,"canary":0,"batchSize":1,"maxUnavailable":1,"failureBudget":1,"target":"r580-lab-bad"`)
	st := h.run(t, 60)
	if st.Phase != PhaseHalted || len(st.Failed) != 2 || h.state("n3") != "" {
		t.Fatalf("second failure exceeds a budget of 1 and halts: %+v", st)
	}
}
