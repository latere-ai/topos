package critic_test

import (
	"os/exec"
	"strings"
	"testing"
)

// TestOnlyCriticPackageImportsTopos enforces the backend seam: within the
// adversarial capability only latere.ai/x/topos/adversarial/critic may import
// the topos runtime (the root package latere.ai/x/topos or a runtime
// subpackage such as models or sandbox). The engine core and every other
// adversarial package must not, so the native critic stays an opt-in backend
// rather than a core dependency. Intra-adversarial imports
// (latere.ai/x/topos/adversarial/...) are not runtime imports and are excluded.
// Test imports are not counted (go list .Imports excludes them), so this file
// importing the runtime is fine.
func TestOnlyCriticPackageImportsTopos(t *testing.T) {
	out, err := exec.Command("go", "list", "-f",
		"{{.ImportPath}} {{range .Imports}}{{.}} {{end}}",
		"latere.ai/x/topos/adversarial/...").CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, out)
	}
	const allowed = "latere.ai/x/topos/adversarial/critic"
	isRuntime := func(imp string) bool {
		if imp == "latere.ai/x/topos" {
			return true
		}
		if imp == "latere.ai/x/topos/adversarial" {
			return false
		}
		return strings.HasPrefix(imp, "latere.ai/x/topos/") &&
			!strings.HasPrefix(imp, "latere.ai/x/topos/adversarial/")
	}
	var offenders []string
	for line := range strings.SplitSeq(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		pkg := fields[0]
		if pkg == allowed {
			continue
		}
		for _, imp := range fields[1:] {
			if isRuntime(imp) {
				offenders = append(offenders, pkg+" -> "+imp)
			}
		}
	}
	if len(offenders) > 0 {
		t.Errorf("only %s may import the topos runtime:\n%s", allowed, strings.Join(offenders, "\n"))
	}
}
