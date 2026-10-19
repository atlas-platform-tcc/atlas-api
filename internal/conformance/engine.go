package conformance

import (
	"fmt"
	"strings"

	"github.com/atlas-platform-tcc/atlas-api/internal/domain"
)

// Facts are the observations extracted from a service's artifacts, keyed by the
// names the policy rules reference (e.g. "image.pinned").
type Facts map[string]any

// severity weights: a high-severity rule is worth more of the score than a low one.
func weight(severity string) int {
	switch severity {
	case "high":
		return 3
	case "medium":
		return 2
	default:
		return 1
	}
}

// Evaluate scores the facts against the policy. Rules whose `When` is false are
// skipped (not applicable) and do not affect the score.
func Evaluate(p Policy, facts Facts) domain.ConformanceReport {
	var rep domain.ConformanceReport
	for _, r := range p.Rules {
		if r.When != nil && !eval(*r.When, facts) {
			continue // rule not applicable (e.g. database not requested)
		}
		w := weight(r.Severity)
		// An applicable rule whose fact is missing is always recorded (transparency) but
		// only counts against the score in enforce mode; audit is best-effort.
		if _, present := facts[r.Assert.Fact]; !present {
			if p.enforcing() {
				rep.Max += w
				rep.Failed++
			}
			rep.Results = append(rep.Results, domain.ConformanceResult{
				ID: r.ID, Description: r.Description, Severity: r.Severity, Pass: false, Unverified: true,
			})
			continue
		}
		rep.Max += w
		pass := eval(r.Assert, facts)
		if pass {
			rep.Score += w
			rep.Passed++
		} else {
			rep.Failed++
		}
		rep.Results = append(rep.Results, domain.ConformanceResult{
			ID: r.ID, Description: r.Description, Severity: r.Severity, Pass: pass,
		})
	}
	if rep.Max > 0 {
		rep.Percent = rep.Score * 100 / rep.Max
	}
	return rep
}

// Gate reports whether a report satisfies the policy's gate — the score reaches
// MinScore and every evaluated Required rule passes — plus the reasons for a failure
// (ADR atlas-api/0016).
func (p Policy) Gate(rep domain.ConformanceReport) (bool, []string) {
	var reasons []string
	if p.MinScore > 0 && rep.Percent < p.MinScore {
		reasons = append(reasons, fmt.Sprintf("score %d%% abaixo do mínimo exigido (%d%%)", rep.Percent, p.MinScore))
	}
	required := make(map[string]bool, len(p.Rules))
	for _, r := range p.Rules {
		if r.Required {
			required[r.ID] = true
		}
	}
	for _, res := range rep.Results {
		if !required[res.ID] || res.Pass {
			continue
		}
		// An unverified required rule blocks only in enforce mode.
		if res.Unverified && !p.enforcing() {
			continue
		}
		reasons = append(reasons, fmt.Sprintf("regra obrigatória não atendida: %s (%s)", res.ID, res.Description))
	}
	return len(reasons) == 0, reasons
}

func eval(c Check, facts Facts) bool {
	v, ok := facts[c.Fact]
	if !ok {
		return false
	}
	switch c.Op {
	case "nonempty":
		return !isEmpty(v)
	case "truthy":
		return isTruthy(v)
	case "equals":
		return equalsVal(v, c.Value)
	case "gte":
		a, okA := toFloat(v)
		b, okB := toFloat(c.Value)
		return okA && okB && a >= b
	case "lte":
		a, okA := toFloat(v)
		b, okB := toFloat(c.Value)
		return okA && okB && a <= b
	case "contains":
		return contains(v, c.Value)
	default:
		return false
	}
}

func isEmpty(v any) bool {
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t) == ""
	case nil:
		return true
	default:
		return false
	}
}

func isTruthy(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		return t != ""
	default:
		f, ok := toFloat(v)
		return ok && f != 0
	}
}

func equalsVal(a, b any) bool {
	if ab, aok := a.(bool); aok {
		if bb, bok := b.(bool); bok {
			return ab == bb
		}
	}
	if af, aok := toFloat(a); aok {
		if bf, bok := toFloat(b); bok {
			return af == bf
		}
	}
	return toString(a) == toString(b)
}

func contains(v, want any) bool {
	switch t := v.(type) {
	case string:
		return strings.Contains(t, toString(want))
	case []string:
		for _, e := range t {
			if e == toString(want) {
				return true
			}
		}
	case []any:
		for _, e := range t {
			if equalsVal(e, want) {
				return true
			}
		}
	}
	return false
}

func toFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	case float64:
		return t, true
	default:
		return 0, false
	}
}

func toString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case bool:
		if t {
			return "true"
		}
		return "false"
	default:
		return ""
	}
}
