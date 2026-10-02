// Package rollout upgrades a component (a GPU driver, in the lab) across a fleet of Kubernetes
// nodes, one canary first and then in batches, with a health gate on every node:
//
//	Pending ─▶ Draining ─▶ Installing ─▶ Validating ─▶ Soaking ─▶ Done
//	                            │             │            │
//	                            └──── any failure ─────────┘
//	                                         ▼
//	                       RollingBack ─▶ RollbackValidating ─▶ RolledBack (back in service, old version)
//	                                                       └──▶ Quarantined (cordoned, needs a human)
//
// A canary failure halts the rollout; later failures halt it once they exceed the failure budget.
// Nodes already in progress always finish their current cycle, so nothing is left half-upgraded.
//
// The plan and the operator's controls live in the rollout-plan ConfigMap, progress in the
// rollout-status ConfigMap, and each node's step in its own annotations. The controller holds no
// state of its own, so it can restart at any point.
package rollout

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/inigogonzalezgarcia/11-gpu-fleet-lifecycle/internal/agent"
	"github.com/inigogonzalezgarcia/11-gpu-fleet-lifecycle/internal/k8s"
	"github.com/inigogonzalezgarcia/11-gpu-fleet-lifecycle/internal/plan"
)

// Node annotations and labels.
const (
	prefix       = "lifecycle.lab/"
	AnnRollout   = prefix + "rollout"
	AnnState     = prefix + "state"
	AnnFrom      = prefix + "from-version"
	AnnSince     = prefix + "since"
	AnnJob       = prefix + "validation-job"
	AnnReason    = prefix + "reason"
	LabelVersion = prefix + "version" // inventory: the version the node's agent reports

	// Shared with repo 09 (gpu-node-remediation): both tools agree which nodes are out.
	LabelQuarantine = "gpu-remediation.lab/quarantined"
	AnnRemediation  = "gpu-remediation.lab/state"
)

// Node states.
const (
	Draining           = "Draining"
	Installing         = "Installing"
	Validating         = "Validating"
	Soaking            = "Soaking"
	Done               = "Done"
	RollingBack        = "RollingBack"
	RollbackValidating = "RollbackValidating"
	RolledBack         = "RolledBack"
	Quarantined        = "Quarantined"
)

// Rollout phases.
const (
	PhaseInvalid          = "Invalid"
	PhaseCanary           = "Canary"
	PhaseAwaitingApproval = "AwaitingApproval"
	PhaseRolling          = "Rolling"
	PhasePaused           = "Paused"
	PhaseWaitingForWindow = "WaitingForWindow"
	PhaseHalted           = "Halted"
	PhaseAborted          = "Aborted"
	PhaseComplete         = "Complete"
)

var active = map[string]bool{Draining: true, Installing: true, Validating: true, Soaking: true, RollingBack: true, RollbackValidating: true}

// Hardware-class XIDs: seeing one while a node soaks fails the upgrade (same list as repos 09 and 10).
var hardwareXID = map[int]bool{48: true, 63: true, 64: true, 74: true, 79: true, 92: true, 95: true, 119: true, 120: true}

// Cluster is what the controller needs from Kubernetes. *k8s.Client implements it.
type Cluster interface {
	ListNodes(selector string) ([]k8s.Node, error)
	PatchNode(name string, patch map[string]any) error
	PodsOnNode(node string) ([]k8s.Pod, error)
	Evict(namespace, name string) error
	CreateJob(namespace string, job map[string]any) error
	JobState(namespace, name string) (succeeded, failed bool, err error)
	DeleteJob(namespace, name string) error
	NodeEvent(namespace, node, eventType, reason, message string) error
	GetConfigMapData(namespace, name string) (map[string]string, error)
	PutConfigMapData(namespace, name string, data map[string]string) error
}

// Agent installs versions on a node and reports on them. *agent.HTTP implements it.
type Agent interface {
	Version(n k8s.Node) (agent.Version, error)
	Health(n k8s.Node) (agent.Health, error)
	Install(n k8s.Node, version string) error
}

// Config is the controller's own configuration (the rollout itself is in the plan).
type Config struct {
	Namespace       string // ConfigMaps and validation Jobs
	PlanConfigMap   string
	StatusConfigMap string
	EventNamespace  string
	ValidationImage string
	ValidationGPUs  int
	AgentPort       int
}

// Status is written to the rollout-status ConfigMap as status.json.
type Status struct {
	Rollout    string   `json:"rollout"`
	Target     string   `json:"target"`
	Phase      string   `json:"phase"`
	Message    string   `json:"message,omitempty"`
	Canary     []string `json:"canary,omitempty"`
	InProgress []string `json:"inProgress,omitempty"`
	Done       []string `json:"done,omitempty"`
	Pending    []string `json:"pending,omitempty"`
	Failed     []string `json:"failed,omitempty"`
	Skipped    []string `json:"skipped,omitempty"`
	Started    string   `json:"started,omitempty"`
	Updated    string   `json:"updated,omitempty"`
}

// Controller runs rollouts.
type Controller struct {
	cfg   Config
	k     Cluster
	agent Agent
	log   *slog.Logger
	now   func() time.Time

	mu      sync.Mutex
	last    Status
	actions map[string]int
}

// New builds a controller.
func New(cfg Config, k Cluster, a Agent, log *slog.Logger) *Controller {
	return &Controller{cfg: cfg, k: k, agent: a, log: log, now: time.Now, actions: map[string]int{}}
}

// Snapshot returns the last status and action counters, for /metrics.
func (c *Controller) Snapshot() (Status, map[string]int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	a := make(map[string]int, len(c.actions))
	for k, v := range c.actions {
		a[k] = v
	}
	return c.last, a
}

func (c *Controller) count(action string) {
	c.mu.Lock()
	c.actions[action]++
	c.mu.Unlock()
}

// Reconcile runs one pass.
func (c *Controller) Reconcile() error {
	data, err := c.k.GetConfigMapData(c.cfg.Namespace, c.cfg.PlanConfigMap)
	if errors.Is(err, k8s.ErrNotFound) {
		return nil // nothing to do until someone submits a plan
	}
	if err != nil {
		return fmt.Errorf("read plan: %w", err)
	}
	prev := c.loadStatus()
	p, err := plan.Parse([]byte(data["plan.json"]))
	if err != nil {
		st := Status{Rollout: prev.Rollout, Phase: PhaseInvalid, Message: err.Error()}
		return c.saveStatus(prev, st)
	}
	st := prev
	if st.Rollout != p.Name {
		st = Status{Rollout: p.Name, Target: p.Target, Phase: PhaseCanary, Started: c.now().UTC().Format(time.RFC3339)}
	}
	control := strings.TrimSpace(data["control"])
	approved := strings.TrimSpace(data["approve"]) == p.Name

	nodes, err := c.k.ListNodes(p.Selector)
	if err != nil {
		return fmt.Errorf("list nodes: %w", err)
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Name < nodes[j].Name })

	var errs []error
	var inProgress, done, failed, pending, skipped []string
	versions := map[string]string{}
	byName := map[string]k8s.Node{}
	othersOut := 0
	for _, n := range nodes {
		byName[n.Name] = n
		v, verr := c.agent.Version(n)
		if verr == nil {
			versions[n.Name] = v.Current
			if n.Labels[LabelVersion] != v.Current {
				if err := c.k.PatchNode(n.Name, labels(map[string]any{LabelVersion: v.Current})); err != nil {
					errs = append(errs, err)
				}
			}
		}
		mine := n.Annotations[AnnRollout] == p.Name
		state := ""
		if mine {
			state = n.Annotations[AnnState]
		}
		if active[state] {
			next, err := c.advance(n, p, state)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", n.Name, err))
			}
			state = next
		}
		switch {
		case active[state]:
			inProgress = append(inProgress, n.Name+" ("+state+")")
		case state == Done:
			done = append(done, n.Name)
		case state == RolledBack || state == Quarantined:
			failed = append(failed, fmt.Sprintf("%s: %s (%s)", n.Name, n.Annotations[AnnReason], state))
		case n.Labels[LabelQuarantine] == "true" || n.Annotations[AnnRemediation] != "":
			skipped = append(skipped, n.Name+": being remediated or quarantined")
			if n.Unschedulable {
				othersOut++
			}
		case n.Unschedulable:
			skipped = append(skipped, n.Name+": cordoned by someone else")
			othersOut++
		case verr != nil:
			skipped = append(skipped, n.Name+": agent unreachable")
		case versions[n.Name] == p.Target:
			done = append(done, n.Name)
		default:
			pending = append(pending, n.Name)
		}
	}
	st.InProgress, st.Done, st.Failed, st.Pending, st.Skipped = inProgress, done, failed, pending, skipped
	st.Target = p.Target

	switch st.Phase {
	case PhaseHalted, PhaseAborted, PhaseComplete:
		return errors.Join(append(errs, c.saveStatus(prev, st))...)
	}
	if control == "abort" {
		st.Phase, st.Message = PhaseAborted, "aborted by the operator; nodes in progress finish their current cycle"
		c.log.Warn("rollout aborted", "rollout", p.Name)
		return errors.Join(append(errs, c.saveStatus(prev, st))...)
	}

	failedNames := map[string]bool{}
	for _, f := range failed {
		failedNames[strings.SplitN(f, ":", 2)[0]] = true
	}
	for _, cn := range st.Canary {
		if failedNames[cn] {
			st.Phase, st.Message = PhaseHalted, "canary "+cn+" failed; no other node was touched"
			c.count("halt")
			return errors.Join(append(errs, c.saveStatus(prev, st))...)
		}
	}
	if len(failed) > p.FailureBudget {
		st.Phase, st.Message = PhaseHalted, fmt.Sprintf("%d node(s) failed, failure budget is %d", len(failed), p.FailureBudget)
		c.count("halt")
		return errors.Join(append(errs, c.saveStatus(prev, st))...)
	}
	if len(pending) == 0 && len(inProgress) == 0 {
		st.Phase = PhaseComplete
		st.Message = fmt.Sprintf("%d node(s) on %s", len(done), p.Target)
		if len(failed) > 0 {
			st.Message += fmt.Sprintf("; %d failed within the failure budget", len(failed))
		}
		return errors.Join(append(errs, c.saveStatus(prev, st))...)
	}

	// Which nodes may start now?
	var want []string
	stage := PhaseRolling
	if p.Canary > 0 && len(st.Canary) == 0 {
		n := p.Canary
		if n > len(pending) {
			n = len(pending)
		}
		st.Canary = append([]string(nil), pending[:n]...)
	}
	canaryDone := true
	for _, cn := range st.Canary {
		if !contains(done, cn) {
			canaryDone = false
		}
	}
	switch {
	case !canaryDone:
		stage = PhaseCanary
		for _, cn := range st.Canary {
			if contains(pending, cn) {
				want = append(want, cn)
			}
		}
		st.Message = "upgrading canary: " + strings.Join(st.Canary, ", ")
	case p.PauseAfterCanary && !approved:
		stage = PhaseAwaitingApproval
		st.Message = fmt.Sprintf("canary passed on %s; approve rollout %q to continue", strings.Join(st.Canary, ", "), p.Name)
	default:
		stage = PhaseRolling
		if len(inProgress) == 0 { // batches run one after the other
			size := p.BatchSize
			if size > len(pending) {
				size = len(pending)
			}
			want = pending[:size]
		}
		st.Message = fmt.Sprintf("%d done, %d pending", len(done), len(pending))
	}
	budget := p.MaxUnavailable - len(inProgress) - othersOut
	if len(want) > budget {
		if budget < 0 {
			budget = 0
		}
		want = want[:budget]
		if len(want) == 0 && len(inProgress) == 0 {
			st.Message = fmt.Sprintf("waiting: %d node(s) already out of service by other means (maxUnavailable %d)", othersOut, p.MaxUnavailable)
		}
	}
	st.Phase = stage
	if len(want) > 0 {
		switch {
		case control == "pause":
			st.Phase, st.Message = PhasePaused, "paused by the operator; nodes in progress finish their current cycle"
			want = nil
		case !p.Window.Open(c.now()):
			if len(inProgress) == 0 {
				st.Phase = PhaseWaitingForWindow
			}
			st.Message = fmt.Sprintf("outside the maintenance window (%s-%s UTC)", p.Window.Start, p.Window.End)
			want = nil
		}
	}
	for _, name := range want {
		if err := c.start(byName[name], p, versions[name]); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
			continue
		}
		st.InProgress = append(st.InProgress, name+" ("+Draining+")")
		st.Pending = remove(st.Pending, name)
	}
	return errors.Join(append(errs, c.saveStatus(prev, st))...)
}

func (c *Controller) loadStatus() Status {
	var st Status
	data, err := c.k.GetConfigMapData(c.cfg.Namespace, c.cfg.StatusConfigMap)
	if err == nil {
		_ = json.Unmarshal([]byte(data["status.json"]), &st)
	}
	return st
}

func (c *Controller) saveStatus(prev, st Status) error {
	cmp := func(s Status) string { s.Updated = ""; b, _ := json.Marshal(s); return string(b) }
	c.mu.Lock()
	c.last = st
	c.mu.Unlock()
	if cmp(prev) == cmp(st) && prev.Updated != "" {
		return nil
	}
	if prev.Phase != st.Phase || prev.Rollout != st.Rollout {
		c.log.Info("rollout phase", "rollout", st.Rollout, "phase", st.Phase, "message", st.Message)
	}
	st.Updated = c.now().UTC().Format(time.RFC3339)
	c.mu.Lock()
	c.last = st
	c.mu.Unlock()
	b, _ := json.MarshalIndent(st, "", "  ")
	return c.k.PutConfigMapData(c.cfg.Namespace, c.cfg.StatusConfigMap, map[string]string{"status.json": string(b)})
}

// start takes a node out of service for the upgrade.
func (c *Controller) start(n k8s.Node, p *plan.Plan, from string) error {
	c.count("start")
	return c.set(n, Draining, map[string]any{
		AnnRollout: p.Name, AnnFrom: from, AnnReason: nil, AnnJob: nil,
	}, true, "Normal", "UpgradeStarted", fmt.Sprintf("%s %s -> %s", p.Component, from, p.Target))
}

// set patches the node state (and extra annotations) in one call and records an event.
func (c *Controller) set(n k8s.Node, state string, extra map[string]any, unschedulable bool, evType, reason, msg string) error {
	ann := map[string]any{AnnState: state, AnnSince: c.now().UTC().Format(time.RFC3339)}
	for k, v := range extra {
		ann[k] = v
	}
	if err := c.k.PatchNode(n.Name, map[string]any{
		"metadata": map[string]any{"annotations": ann},
		"spec":     map[string]any{"unschedulable": unschedulable},
	}); err != nil {
		return err
	}
	for k, v := range ann { // keep the local copy in step, so this pass reports the new state
		if v == nil {
			delete(n.Annotations, k)
		} else {
			n.Annotations[k] = v.(string)
		}
	}
	c.log.Info("node", "node", n.Name, "state", state, "message", msg)
	if err := c.k.NodeEvent(c.cfg.EventNamespace, n.Name, evType, reason, msg); err != nil {
		c.log.Warn("could not record event", "node", n.Name, "error", err)
	}
	return nil
}

func (c *Controller) since(n k8s.Node) time.Duration {
	t, err := time.Parse(time.RFC3339, n.Annotations[AnnSince])
	if err != nil {
		return 0
	}
	return c.now().Sub(t)
}

// advance moves one node forward and returns its new state.
func (c *Controller) advance(n k8s.Node, p *plan.Plan, state string) (string, error) {
	from := n.Annotations[AnnFrom]
	switch state {
	case Draining:
		left, err := c.drain(n)
		if err != nil || left > 0 {
			return state, err
		}
		if err := c.agent.Install(n, p.Target); err != nil {
			return state, fmt.Errorf("install: %w", err)
		}
		c.count("install")
		return Installing, c.set(n, Installing, nil, true, "Normal", "UpgradeInstalling", "installing "+p.Target)

	case Installing:
		v, err := c.agent.Version(n)
		if err != nil {
			return state, err
		}
		if v.Current == p.Target && v.Upgrading == "" {
			return c.startValidation(n, p, Validating, "validating "+p.Target)
		}
		if c.since(n) > p.UpgradeTimeout.Duration {
			return c.rollback(n, p, fmt.Sprintf("install of %s did not finish within %s", p.Target, p.UpgradeTimeout))
		}
		if v.Upgrading != p.Target {
			return state, c.agent.Install(n, p.Target) // the agent lost the request (restart): ask again
		}
		return state, nil

	case Validating:
		ok, failed, err := c.jobState(n)
		switch {
		case err != nil:
			return state, err
		case ok:
			return Soaking, c.set(n, Soaking, map[string]any{AnnJob: nil}, true, "Normal", "UpgradeSoaking", fmt.Sprintf("validated, soaking for %s", p.Soak))
		case failed:
			return c.rollback(n, p, "validation failed on "+p.Target)
		case c.since(n) > p.ValidationTimeout.Duration:
			return c.rollback(n, p, fmt.Sprintf("validation did not finish within %s", p.ValidationTimeout))
		}
		return state, nil

	case Soaking:
		h, err := c.agent.Health(n)
		if err != nil {
			return state, err
		}
		if hardwareXID[h.XID] || h.DBE > 0 {
			return c.rollback(n, p, fmt.Sprintf("XID %d / %.0f ECC error(s) while soaking on %s", h.XID, h.DBE, p.Target))
		}
		if c.since(n) < p.Soak.Duration {
			return state, nil
		}
		c.count("done")
		return Done, c.set(n, Done, map[string]any{AnnReason: nil}, false, "Normal", "UpgradeDone", "running "+p.Target)

	case RollingBack:
		if from == "" {
			return c.quarantine(n, "upgrade failed and the previous version is unknown")
		}
		v, err := c.agent.Version(n)
		if err != nil {
			return state, err
		}
		if v.Current == from && v.Upgrading == "" {
			return c.startValidation(n, p, RollbackValidating, "validating rollback to "+from)
		}
		if c.since(n) > p.UpgradeTimeout.Duration {
			return c.quarantine(n, n.Annotations[AnnReason]+"; rollback did not finish")
		}
		if v.Upgrading != from {
			return state, c.agent.Install(n, from)
		}
		return state, nil

	case RollbackValidating:
		ok, failed, err := c.jobState(n)
		switch {
		case err != nil:
			return state, err
		case ok:
			c.count("rolled_back")
			return RolledBack, c.set(n, RolledBack, map[string]any{AnnJob: nil}, false, "Warning", "UpgradeRolledBack",
				n.Annotations[AnnReason]+"; back in service on "+from)
		case failed || c.since(n) > p.ValidationTimeout.Duration:
			return c.quarantine(n, n.Annotations[AnnReason]+"; rollback to "+from+" also failed validation")
		}
		return state, nil
	}
	return state, fmt.Errorf("unknown state %q", state)
}

func (c *Controller) rollback(n k8s.Node, p *plan.Plan, reason string) (string, error) {
	c.count("rollback")
	from := n.Annotations[AnnFrom]
	if err := c.set(n, RollingBack, map[string]any{AnnReason: reason, AnnJob: nil}, true, "Warning", "UpgradeFailed",
		reason+"; rolling back to "+from); err != nil {
		return RollingBack, err
	}
	if from != "" {
		if err := c.agent.Install(n, from); err != nil {
			return RollingBack, fmt.Errorf("rollback install: %w", err)
		}
	}
	return RollingBack, nil
}

func (c *Controller) quarantine(n k8s.Node, reason string) (string, error) {
	c.count("quarantine")
	if err := c.k.PatchNode(n.Name, labels(map[string]any{LabelQuarantine: "true"})); err != nil {
		return n.Annotations[AnnState], err
	}
	return Quarantined, c.set(n, Quarantined, map[string]any{AnnReason: reason, AnnJob: nil}, true, "Warning", "UpgradeQuarantined", reason)
}

func (c *Controller) jobState(n k8s.Node) (ok, failed bool, err error) {
	job := n.Annotations[AnnJob]
	ok, failed, err = c.k.JobState(c.cfg.Namespace, job)
	if errors.Is(err, k8s.ErrNotFound) {
		return false, true, nil // a validation Job that vanished did not pass
	}
	if ok {
		_ = c.k.DeleteJob(c.cfg.Namespace, job)
	}
	return ok, failed, err
}

func (c *Controller) startValidation(n k8s.Node, p *plan.Plan, next, msg string) (string, error) {
	name := fmt.Sprintf("gpu-validate-%s-%d", n.Name, c.now().Unix())
	if len(name) > 63 {
		name = strings.TrimLeft(name[len(name)-63:], "-")
	}
	if err := c.k.CreateJob(c.cfg.Namespace, c.validationJob(name, n.Name, p)); err != nil {
		return n.Annotations[AnnState], fmt.Errorf("create validation job: %w", err)
	}
	return next, c.set(n, next, map[string]any{AnnJob: name}, true, "Normal", "UpgradeValidating", msg)
}

// drain evicts what can be evicted and returns how many pods still have to go.
func (c *Controller) drain(n k8s.Node) (int, error) {
	pods, err := c.k.PodsOnNode(n.Name)
	if err != nil {
		return 0, err
	}
	left := 0
	for _, pod := range pods {
		if !evictable(pod, c.cfg.Namespace) {
			continue
		}
		left++
		if err := c.k.Evict(pod.Namespace, pod.Name); err != nil && !errors.Is(err, k8s.ErrBlocked) {
			return left, fmt.Errorf("evict %s/%s: %w", pod.Namespace, pod.Name, err)
		} else if err == nil {
			c.count("evict")
		}
	}
	return left, nil
}

func evictable(p k8s.Pod, ownNamespace string) bool {
	if p.Phase == "Succeeded" || p.Phase == "Failed" {
		return false
	}
	if _, mirror := p.Annotations["kubernetes.io/config.mirror"]; mirror {
		return false
	}
	for _, k := range p.OwnerKinds {
		if k == "DaemonSet" || (k == "Job" && p.Namespace == ownNamespace && strings.HasPrefix(p.Name, "gpu-validate-")) {
			return false
		}
	}
	return true
}

// validationJob runs on the node itself (it is cordoned, so nodeName bypasses the scheduler),
// takes every GPU and asks the node agent to check the installed version.
func (c *Controller) validationJob(name, node string, p *plan.Plan) map[string]any {
	container := map[string]any{
		"name":    "validate",
		"image":   c.cfg.ValidationImage,
		"command": []string{"sh", "-c", fmt.Sprintf(`wget -q -O- "http://${HOST_IP}:%d/validate"`, c.cfg.AgentPort)},
		"env": []map[string]any{{"name": "HOST_IP", "valueFrom": map[string]any{
			"fieldRef": map[string]string{"fieldPath": "status.hostIP"}}}},
		"securityContext": map[string]any{
			"allowPrivilegeEscalation": false, "runAsNonRoot": true, "runAsUser": 65534,
			"capabilities": map[string]any{"drop": []string{"ALL"}},
		},
	}
	if c.cfg.ValidationGPUs > 0 {
		container["resources"] = map[string]any{"limits": map[string]any{"nvidia.com/gpu": strconv.Itoa(c.cfg.ValidationGPUs)}}
	}
	return map[string]any{
		"apiVersion": "batch/v1",
		"kind":       "Job",
		"metadata": map[string]any{"name": name, "namespace": c.cfg.Namespace,
			"labels": map[string]string{"app.kubernetes.io/name": "gpu-validate", prefix + "node": node}},
		"spec": map[string]any{
			"backoffLimit":            0,
			"activeDeadlineSeconds":   int(p.ValidationTimeout.Seconds()),
			"ttlSecondsAfterFinished": 600,
			"template": map[string]any{
				"metadata": map[string]any{"labels": map[string]string{"app.kubernetes.io/name": "gpu-validate"}},
				"spec": map[string]any{
					"nodeName":                     node,
					"restartPolicy":                "Never",
					"automountServiceAccountToken": false,
					"tolerations":                  []map[string]string{{"operator": "Exists"}},
					"containers":                   []map[string]any{container},
				},
			},
		},
	}
}

func labels(l map[string]any) map[string]any {
	return map[string]any{"metadata": map[string]any{"labels": l}}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func remove(list []string, s string) []string {
	out := list[:0:0]
	for _, x := range list {
		if x != s {
			out = append(out, x)
		}
	}
	return out
}
