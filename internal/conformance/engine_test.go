package conformance

import (
	"strings"
	"testing"
)

// conformantFacts is a service that satisfies every create-phase rule (db requested
// and wired). Deploy-only facts are omitted, so those rules are skipped (when-guarded).
func conformantFacts() Facts {
	return Facts{
		"chart.base":                  true,
		"image.pinned":                true,
		"image.approvedRegistry":      true,
		"pipeline.present":            true,
		"template.hasReadme":          true,
		"env.hasSecretLike":           false,
		"replicas":                    2,
		"resources.totalCpuMillis":    400,
		"application.finalizer":       true,
		"application.createNamespace": false,
		"database.requested":          true,
		"database.wired":              true,
	}
}

func TestDefaultPolicyLoads(t *testing.T) {
	p, err := Default()
	if err != nil {
		t.Fatalf("load default policy: %v", err)
	}
	if len(p.Rules) == 0 {
		t.Fatal("default policy has no rules")
	}
}

func TestEvaluate_FullyConformant(t *testing.T) {
	p, _ := Default()
	rep := Evaluate(p, conformantFacts())
	if rep.Percent != 100 {
		t.Fatalf("expected 100%%, got %d%% (%d/%d, failed=%d)", rep.Percent, rep.Score, rep.Max, rep.Failed)
	}
	if rep.Failed != 0 {
		t.Fatalf("expected 0 failures, got %d", rep.Failed)
	}
}

func TestEvaluate_FailingRuleLowersScore(t *testing.T) {
	p, _ := Default()
	f := conformantFacts()
	f["image.pinned"] = false // high-severity rule now fails
	rep := Evaluate(p, f)
	if rep.Percent == 100 {
		t.Fatal("expected score below 100 when a high-severity rule fails")
	}
	if rep.Failed != 1 {
		t.Fatalf("expected exactly 1 failure, got %d", rep.Failed)
	}
}

func TestEvaluate_WhenGateSkipsInapplicableRule(t *testing.T) {
	p, _ := Default()
	f := conformantFacts()
	// No database requested: the database-wiring rule must not apply, even if unwired.
	f["database.requested"] = false
	f["database.wired"] = false
	rep := Evaluate(p, f)
	for _, r := range rep.Results {
		if r.ID == "database-wired-when-requested" {
			t.Fatal("database rule should have been skipped when database not requested")
		}
	}
	if rep.Percent != 100 {
		t.Fatalf("expected 100%% (rule skipped), got %d%%", rep.Percent)
	}
}

func TestGate_PassesWhenFullyConformant(t *testing.T) {
	p, _ := Default()
	ok, reasons := p.Gate(Evaluate(p, conformantFacts()))
	if !ok {
		t.Fatalf("expected gate to pass, got reasons: %v", reasons)
	}
}

func TestGate_FailsOnRequiredRule(t *testing.T) {
	p, _ := Default()
	f := conformantFacts()
	f["image.pinned"] = false // required rule
	ok, reasons := p.Gate(Evaluate(p, f))
	if ok {
		t.Fatal("expected gate to fail when a required rule fails")
	}
	joined := strings.Join(reasons, " ")
	if !strings.Contains(joined, "image-pinned") {
		t.Fatalf("expected reason to mention image-pinned, got: %v", reasons)
	}
}

func TestGate_FailsOnLowScore(t *testing.T) {
	p, _ := Default()
	f := conformantFacts()
	// Fail only non-required rules until the score drops below minScore (80%).
	f["template.hasReadme"] = false         // medium (2)
	f["application.finalizer"] = false      // low (1)
	f["application.createNamespace"] = true // low (1)
	f["resources.totalCpuMillis"] = 3000    // medium (2), over the 2000m budget
	rep := Evaluate(p, f)
	ok, reasons := p.Gate(rep)
	if ok {
		t.Fatalf("expected gate to fail below minScore, got %d%%", rep.Percent)
	}
	if !strings.Contains(strings.Join(reasons, " "), "mínimo") {
		t.Fatalf("expected a score-threshold reason, got: %v", reasons)
	}
}

func TestForPhase(t *testing.T) {
	p, _ := Default()
	create := p.ForPhase(PhaseCreate)
	deploy := p.ForPhase(PhaseDeploy)

	if len(deploy.Rules) == 0 || len(deploy.Rules) >= len(create.Rules) {
		t.Fatalf("deploy phase should be a non-empty subset of create; create=%d deploy=%d", len(create.Rules), len(deploy.Rules))
	}
	has := func(rules []Rule, id string) bool {
		for _, r := range rules {
			if r.ID == id {
				return true
			}
		}
		return false
	}
	if !has(deploy.Rules, "image-pinned") {
		t.Error("deploy phase must include image-pinned")
	}
	if has(deploy.Rules, "base-chart") {
		t.Error("deploy phase must NOT include the create-only base-chart rule")
	}
}

func TestEvaluate_GteAndReplicas(t *testing.T) {
	p, _ := Default()
	f := conformantFacts()
	f["replicas"] = 0 // below the gte:1 bound
	rep := Evaluate(p, f)
	var found bool
	for _, r := range rep.Results {
		if r.ID == "replicas-in-bounds" {
			found = true
			if r.Pass {
				t.Fatal("replicas=0 should fail replicas-in-bounds")
			}
		}
	}
	if !found {
		t.Fatal("replicas-in-bounds rule missing")
	}
}
