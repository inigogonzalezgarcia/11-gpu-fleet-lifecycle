package plan

import (
	"strings"
	"testing"
	"time"
)

func TestParseDefaultsAndValidation(t *testing.T) {
	p, err := Parse([]byte(`{"name":"r580","selector":"gpu=true","target":"r580-lab","soak":"30s"}`))
	if err != nil {
		t.Fatal(err)
	}
	if p.Canary != 1 || p.BatchSize != 1 || p.MaxUnavailable != 1 || p.Soak.Duration != 30*time.Second || p.Component != "driver" {
		t.Fatalf("defaults: %+v", p)
	}
	bad := map[string]string{
		`{"selector":"a","target":"b"}`:                                                                       "name is required",
		`{"name":"x","selector":"a","target":"b","canary":3,"maxUnavailable":2}`:                              "canary cannot be larger",
		`{"name":"x","selector":"a","target":"b","soak":30}`:                                                  "duration must be a string",
		`{"name":"x","selector":"a","target":"b","typo":1}`:                                                   "unknown field",
		`{"name":"x","selector":"a","target":"b","window":{"start":"25:00","end":"06:00"}}`:                   "HH:MM",
		`{"name":"x","selector":"a","target":"b","window":{"start":"22:00","end":"06:00","days":["Funday"]}}`: "Mon..Sun",
	}
	for in, want := range bad {
		if _, err := Parse([]byte(in)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: want error containing %q, got %v", in, want, err)
		}
	}
}

func TestWindow(t *testing.T) {
	at := func(s string) time.Time { v, _ := time.Parse(time.RFC3339, s); return v }
	night := &Window{Start: "22:00", End: "06:00", Days: []string{"Fri"}} // Friday night into Saturday
	cases := []struct {
		w    *Window
		t    string
		open bool
	}{
		{nil, "2026-10-02T12:00:00Z", true},
		{&Window{Start: "09:00", End: "17:00"}, "2026-10-02T12:00:00Z", true},
		{&Window{Start: "09:00", End: "17:00"}, "2026-10-02T17:00:00Z", false},
		{night, "2026-10-02T23:30:00Z", true},  // Friday 23:30
		{night, "2026-10-03T05:59:00Z", true},  // Saturday early morning, window opened Friday
		{night, "2026-10-03T23:30:00Z", false}, // Saturday night: not a Friday window
		{night, "2026-10-02T12:00:00Z", false},
	}
	for _, c := range cases {
		if got := c.w.Open(at(c.t)); got != c.open {
			t.Errorf("%+v at %s: want %v", c.w, c.t, c.open)
		}
	}
}
