package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestInstallValidateHealth(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	n := &node{initial: "r570-lab", current: "r570-lab", delay: 4 * time.Second,
		bad: set("r580-lab-bad"), xid: set("r580-lab-xid"), now: func() time.Time { return now }}
	h := n.handler()
	do := func(method, target string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(method, target, nil))
		return w
	}
	if w := do("POST", "/install?version=r580-lab"); w.Code != http.StatusAccepted {
		t.Fatalf("install: %d", w.Code)
	}
	if w := do("GET", "/version"); !strings.Contains(w.Body.String(), `"upgrading":"r580-lab"`) {
		t.Fatalf("install in progress: %s", w.Body.String())
	}
	now = now.Add(5 * time.Second)
	if w := do("GET", "/version"); !strings.Contains(w.Body.String(), `"current":"r580-lab"`) {
		t.Fatalf("install done: %s", w.Body.String())
	}
	if w := do("GET", "/validate"); w.Code != 200 {
		t.Fatalf("good version should validate, got %d", w.Code)
	}
	do("POST", "/install?version=r580-lab-bad")
	now = now.Add(5 * time.Second)
	if w := do("GET", "/validate"); w.Code != 500 {
		t.Fatalf("bad version should fail validation, got %d", w.Code)
	}
	do("POST", "/install?version=r580-lab-xid")
	now = now.Add(5 * time.Second)
	if w := do("GET", "/health"); !strings.Contains(w.Body.String(), `"xid":0`) {
		t.Fatalf("no XID right after install: %s", w.Body.String())
	}
	now = now.Add(10 * time.Second)
	if w := do("GET", "/health"); !strings.Contains(w.Body.String(), `"xid":79`) {
		t.Fatalf("XID after a while: %s", w.Body.String())
	}
	do("POST", "/reset")
	if w := do("GET", "/version"); !strings.Contains(w.Body.String(), `"current":"r570-lab"`) {
		t.Fatalf("reset: %s", w.Body.String())
	}
	if w := do("GET", "/install?version=x"); w.Code != 405 {
		t.Fatalf("GET install should be rejected: %d", w.Code)
	}
}
