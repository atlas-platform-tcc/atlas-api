// Package conformance evaluates a service against a declarative platform policy. The
// rule set is YAML and the engine is pure Go, so the score feeds the provisioning Pull
// Request and the catalog without an external policy engine or cluster access
// (ADR atlas-api/0013).
package conformance

import (
	_ "embed"
	"fmt"
	"os"
	"slices"

	"gopkg.in/yaml.v3"
)

// defaultPolicy is the org policy shipped with the API; ATLAS_POLICY_FILE overrides it.
//
//go:embed policy.yaml
var defaultPolicy []byte

// Check asserts an operator over a named fact. Ops: nonempty, truthy, equals, gte, lte, contains.
type Check struct {
	Fact  string `yaml:"fact"`
	Op    string `yaml:"op"`
	Value any    `yaml:"value,omitempty"`
}

// Phases a rule can apply to. A rule with no phases defaults to creation only
// (ADR atlas-api/0019 extends the same policy to day-2 deploy).
const (
	PhaseCreate = "create"
	PhaseDeploy = "deploy"
)

// Rule is one policy rule: `When` gates whether it applies, Required makes it blocking
// at the gate (ADR atlas-api/0016) and Phases restricts it to given lifecycle phases.
type Rule struct {
	ID          string   `yaml:"id"`
	Description string   `yaml:"description"`
	Severity    string   `yaml:"severity"`
	Required    bool     `yaml:"required,omitempty"`
	Phases      []string `yaml:"phases,omitempty"`
	When        *Check   `yaml:"when,omitempty"`
	Assert      Check    `yaml:"assert"`
}

func (r Rule) appliesTo(phase string) bool {
	if len(r.Phases) == 0 {
		return phase == PhaseCreate
	}
	return slices.Contains(r.Phases, phase)
}

// Enforcement modes: "audit" (default) skips a fact it cannot gather, "enforce" fails
// closed on it (ADR atlas-api/0016).
const (
	ModeAudit   = "audit"
	ModeEnforce = "enforce"
)

// Policy is the ordered set of rules plus the gate threshold and enforcement mode.
// MinScore is the minimum percentage the report must reach; 0 disables it.
type Policy struct {
	MinScore int    `yaml:"minScore,omitempty"`
	Mode     string `yaml:"mode,omitempty"`
	Rules    []Rule `yaml:"rules"`
}

func (p Policy) enforcing() bool { return p.Mode == ModeEnforce }

// ForPhase returns the sub-policy applying to the given phase (ADR atlas-api/0019),
// keeping the same threshold and mode.
func (p Policy) ForPhase(phase string) Policy {
	out := Policy{MinScore: p.MinScore, Mode: p.Mode}
	for _, r := range p.Rules {
		if r.appliesTo(phase) {
			out.Rules = append(out.Rules, r)
		}
	}
	return out
}

// Default returns the embedded org policy.
func Default() (Policy, error) { return parse(defaultPolicy) }

// Load reads a policy from path, falling back to the embedded default when path is empty.
func Load(path string) (Policy, error) {
	if path == "" {
		return Default()
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Policy{}, fmt.Errorf("read policy %q: %w", path, err)
	}
	return parse(data)
}

func parse(data []byte) (Policy, error) {
	var p Policy
	if err := yaml.Unmarshal(data, &p); err != nil {
		return Policy{}, fmt.Errorf("parse policy: %w", err)
	}
	return p, nil
}
