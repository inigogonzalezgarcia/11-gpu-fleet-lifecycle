// Package plan describes a fleet rollout: what version to reach, in what order, how fast, and
// which checks every node must pass before the next one starts.
package plan

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Plan is the rollout definition, stored as JSON in the rollout-plan ConfigMap.
type Plan struct {
	Name              string   `json:"name"`              // a new name starts a new rollout
	Selector          string   `json:"selector"`          // which nodes take part
	Component         string   `json:"component"`         // what is upgraded (lab: "driver")
	Target            string   `json:"target"`            // version to reach
	Canary            int      `json:"canary"`            // nodes upgraded first, on their own
	PauseAfterCanary  bool     `json:"pauseAfterCanary"`  // wait for a human after the canary
	BatchSize         int      `json:"batchSize"`         // nodes per batch after the canary
	MaxUnavailable    int      `json:"maxUnavailable"`    // never more nodes out of service than this
	FailureBudget     int      `json:"failureBudget"`     // failed nodes tolerated before halting (canary failures always halt)
	Soak              Duration `json:"soak"`              // watch a node this long after validation
	UpgradeTimeout    Duration `json:"upgradeTimeout"`    // max time for the agent to report the new version
	ValidationTimeout Duration `json:"validationTimeout"` // max time for the validation Job
	Window            *Window  `json:"window,omitempty"`  // only start nodes inside this window
}

// Duration is a time.Duration that reads "30s" style strings from JSON.
type Duration struct{ time.Duration }

// UnmarshalJSON parses a Go duration string.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be a string like \"30s\": %w", err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

// MarshalJSON writes the duration as a string.
func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }

// Window is a daily maintenance window in UTC, e.g. 22:00-06:00. Days limits it to some weekdays.
type Window struct {
	Start string   `json:"start"`          // "HH:MM"
	End   string   `json:"end"`            // "HH:MM"; earlier than Start means it ends the next day
	Days  []string `json:"days,omitempty"` // "Mon".."Sun"; empty means every day
}

func minutes(hhmm string) (int, error) {
	t, err := time.Parse("15:04", hhmm)
	if err != nil {
		return 0, fmt.Errorf("time %q must be HH:MM", hhmm)
	}
	return t.Hour()*60 + t.Minute(), nil
}

// Open reports whether t (converted to UTC) is inside the window. Overnight windows belong to the
// day they start on.
func (w *Window) Open(t time.Time) bool {
	if w == nil {
		return true
	}
	t = t.UTC()
	start, _ := minutes(w.Start)
	end, _ := minutes(w.End)
	now := t.Hour()*60 + t.Minute()
	day := t
	var in bool
	if start <= end {
		in = now >= start && now < end
	} else { // crosses midnight
		in = now >= start || now < end
		if now < end {
			day = t.AddDate(0, 0, -1)
		}
	}
	if !in || len(w.Days) == 0 {
		return in
	}
	for _, d := range w.Days {
		if strings.EqualFold(d, day.Weekday().String()[:3]) {
			return true
		}
	}
	return false
}

// Parse reads and validates a plan, filling defaults.
func Parse(data []byte) (*Plan, error) {
	p := &Plan{Component: "driver", Canary: 1, BatchSize: 1, MaxUnavailable: 1,
		Soak: Duration{2 * time.Minute}, UpgradeTimeout: Duration{15 * time.Minute}, ValidationTimeout: Duration{15 * time.Minute}}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(p); err != nil {
		return nil, fmt.Errorf("plan: %w", err)
	}
	var errs []string
	if p.Name == "" {
		errs = append(errs, "name is required")
	}
	if p.Selector == "" {
		errs = append(errs, "selector is required")
	}
	if p.Target == "" {
		errs = append(errs, "target is required")
	}
	if p.Canary < 0 || p.BatchSize < 1 || p.MaxUnavailable < 1 || p.FailureBudget < 0 {
		errs = append(errs, "canary >= 0, batchSize >= 1, maxUnavailable >= 1 and failureBudget >= 0")
	}
	if p.Canary > p.MaxUnavailable {
		errs = append(errs, "canary cannot be larger than maxUnavailable")
	}
	if p.Window != nil {
		for _, s := range []string{p.Window.Start, p.Window.End} {
			if _, err := minutes(s); err != nil {
				errs = append(errs, err.Error())
			}
		}
		for _, d := range p.Window.Days {
			if !strings.Contains("mon tue wed thu fri sat sun", strings.ToLower(d)) || len(d) != 3 {
				errs = append(errs, fmt.Sprintf("day %q must be Mon..Sun", d))
			}
		}
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("plan %q: %s", p.Name, strings.Join(errs, "; "))
	}
	return p, nil
}
