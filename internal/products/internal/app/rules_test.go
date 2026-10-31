package app

import (
	"errors"
	"strings"
	"testing"
)

var ruleCustomer = RuleVars{PartyKind: "natural", RiskLevel: "low", Age: 30, BalanceUnits: 150000000, MonthTxCount: 2}

func compileRule(t *testing.T, expr string) Rule {
	t.Helper()
	r, err := CompileRule(expr)
	if err != nil {
		t.Fatalf("compile %q: %v", expr, err)
	}
	return r
}

// TestARuleReadsTheFacts: each variable reaches the rule with its value.
func TestARuleReadsTheFacts(t *testing.T) {
	cases := map[string]bool{
		`party_kind == "natural" && risk_level == "low"`: true,
		`age >= 18 && age < 65`:                          true,
		`balance_units >= 100000000`:                     true,
		`month_tx_count < 3`:                             true,
		`month_tx_count < 2`:                             false,
		`party_kind == "legal" || age > 70`:              false,
	}
	for expr, want := range cases {
		if got := compileRule(t, expr).Eval(ruleCustomer); got != want {
			t.Errorf("%s = %v, want %v", expr, got, want)
		}
	}
}

// TestAnInvalidRuleDoesNotCompile: an unknown variable, a syntax error, an expression that is not a
// predicate, a function outside the rules' subset (time zones read files; double is floating point) and a
// literal nested deeper than the limit are ErrInvalidRule.
func TestAnInvalidRuleDoesNotCompile(t *testing.T) {
	deep := strings.Repeat("[", ruleNesting+1) + strings.Repeat("]", ruleNesting+1) + " == []"
	for _, expr := range []string{
		`salary > 1000`, `age >`, `age + 1`, `""`,
		`timestamp("2026-01-01T00:00:00Z").getHours("America/Bogota") > 0`, `double(balance_units) > 1.0`, deep,
	} {
		if _, err := CompileRule(expr); !errors.Is(err, ErrInvalidRule) {
			t.Errorf("%s = %v, want ErrInvalidRule", expr, err)
		}
	}
}

// TestARuleOverItsCostIsNotSatisfied (FR-20): a rule whose evaluation exceeds the cost limit stops and is
// false, even if it would end up true; the same rule over a short list is true.
func TestARuleOverItsCostIsNotSatisfied(t *testing.T) {
	list := "[" + strings.Repeat("0,", 49) + "0]"
	expensive := list + ".all(a, " + list + ".all(b, a + b >= 0))" // 2,500 iterations
	cheap := "[0, 0].all(a, [0, 0].all(b, a + b >= 0))"

	gotExpensive, gotCheap := compileRule(t, expensive).Eval(ruleCustomer), compileRule(t, cheap).Eval(ruleCustomer)

	if gotExpensive || !gotCheap {
		t.Fatalf("expensive = %v, cheap = %v; want false and true", gotExpensive, gotCheap)
	}
}

// TestTheSubsetIsEnough: what a product rule needs still compiles: logic, comparisons, arithmetic,
// membership, size, text and the all and exists macros.
func TestTheSubsetIsEnough(t *testing.T) {
	expr := `(risk_level in ["low", "medium"] || age >= 65) && balance_units % 2 == 0 && -age < 0 && age <= 30 && party_kind != "legal" &&
		size(party_kind) > 0 && party_kind.startsWith("nat") && party_kind.endsWith("ral") && party_kind.contains("tu") &&
		[1, 2].exists(x, x == 2) && (month_tx_count > 0 ? true : false) && [1][0] == 1 && balance_units / 100 * 100 - 0 + 0 == balance_units`
	if !compileRule(t, expr).Eval(ruleCustomer) {
		t.Fatalf("%s = false, want true", expr)
	}
}

// TestTheZeroRuleIsNotSatisfied: a Rule that was never compiled, such as a missing entry of a map of rules,
// is false and not a panic.
func TestTheZeroRuleIsNotSatisfied(t *testing.T) {
	if (Rule{}).Eval(ruleCustomer) {
		t.Fatal("the zero Rule = true, want false")
	}
}

// TestARuleThatFailsIsNotSatisfied (FR-20): an error while evaluating, such as a division by zero, is false,
// not a satisfied rule.
func TestARuleThatFailsIsNotSatisfied(t *testing.T) {
	for _, expr := range []string{`100 / (age - 30) > 0`, `!(100 / (age - 30) > 0)`} {
		if compileRule(t, expr).Eval(ruleCustomer) {
			t.Errorf("%s = true, want false", expr)
		}
	}
}
