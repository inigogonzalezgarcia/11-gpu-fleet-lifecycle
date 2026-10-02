package plan_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/inigogonzalezgarcia/11-gpu-fleet-lifecycle/internal/plan"
)

// The plans in examples/ are what the README and the e2e tests use: they must stay valid.
func TestExamplePlansParse(t *testing.T) {
	files, err := filepath.Glob("../../examples/*.json")
	if err != nil || len(files) == 0 {
		t.Fatalf("no example plans found (%v)", err)
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := plan.Parse(b); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
}
