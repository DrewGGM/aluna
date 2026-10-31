package app

import (
	"errors"
	"fmt"
	"sync"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/common/env"
	"cel.dev/cel-go/common/operators"
)

// ErrInvalidRule means a rule that does not compile, reads a variable the environment does not have or is not
// a predicate.
var ErrInvalidRule = errors.New("invalid rule")

// Bounds of a rule. ruleCostLimit bounds what evaluating it may cost (FR-20): a product rule compares a few
// facts, and one that loops over large literals stops early instead of taking the core's CPU. The cost
// limit does not cover compiling, and type checking nested literals grows with the cube of their depth (a
// rule of 4 KB took seconds): ruleNesting bounds that depth.
const (
	ruleCostLimit = 1000
	ruleNesting   = 32
)

// ruleFunctions are the only functions of the standard library a rule may call: logic, comparisons,
// arithmetic, membership, size and text. The rest is left out on purpose: the timestamp functions read the
// zone database on every call (I/O, ADR-0008), and conversions to double bring floating point into rules
// about money.
var ruleFunctions = []string{
	operators.Conditional, operators.LogicalAnd, operators.LogicalOr, operators.LogicalNot, operators.NotStrictlyFalse,
	operators.Equals, operators.NotEquals, operators.Less, operators.LessEquals, operators.Greater, operators.GreaterEquals,
	operators.Add, operators.Subtract, operators.Multiply, operators.Divide, operators.Modulo, operators.Negate,
	operators.Index, operators.In, "size", "contains", "startsWith", "endsWith",
}

// RuleVars are the facts a rule reads, as the variables party_kind, risk_level, age, balance_units and
// month_tx_count.
type RuleVars struct {
	PartyKind, RiskLevel string
	Age                  int
	BalanceUnits         int64
	MonthTxCount         int
}

// Rule is a compiled CEL predicate of a product (ADR-0008), such as a fee exemption.
type Rule struct {
	program cel.Program
}

// ruleEnv is the CEL environment of the rules: only the variables of RuleVars, ruleFunctions and the all and
// exists macros.
var ruleEnv = sync.OnceValues(func() (*cel.Env, error) {
	subset := env.NewLibrarySubset().AddIncludedMacros(operators.All, operators.Exists)
	for _, name := range ruleFunctions {
		subset.AddIncludedFunctions(env.NewFunction(name))
	}
	return cel.NewCustomEnv(
		cel.StdLib(cel.StdLibSubset(subset)),
		cel.ParserRecursionLimit(ruleNesting),
		cel.Variable("party_kind", cel.StringType),
		cel.Variable("risk_level", cel.StringType),
		cel.Variable("age", cel.IntType),
		cel.Variable("balance_units", cel.IntType),
		cel.Variable("month_tx_count", cel.IntType),
	)
})

// CompileRule compiles expr, a boolean CEL expression over the variables of RuleVars, or ErrInvalidRule.
func CompileRule(expr string) (Rule, error) {
	rules, err := ruleEnv()
	if err != nil {
		return Rule{}, fmt.Errorf("rule environment: %w", err)
	}
	ast, issues := rules.Compile(expr)
	if err := issues.Err(); err != nil {
		return Rule{}, fmt.Errorf("%w: %w", ErrInvalidRule, err)
	}
	if !ast.OutputType().IsExactType(cel.BoolType) {
		return Rule{}, fmt.Errorf("%w: it gives %s, not a bool", ErrInvalidRule, ast.OutputType())
	}
	program, err := rules.Program(ast, cel.CostLimit(ruleCostLimit))
	if err != nil {
		return Rule{}, fmt.Errorf("%w: %w", ErrInvalidRule, err)
	}
	return Rule{program: program}, nil
}

// Eval reports whether vars satisfy the rule. A rule that fails or exceeds its cost limit is not
// satisfied (FR-20), and neither is the zero Rule, which was never compiled: an exemption that cannot be
// decided is not granted.
func (r Rule) Eval(vars RuleVars) bool {
	if r.program == nil {
		return false
	}
	out, _, err := r.program.Eval(map[string]any{
		"party_kind":     vars.PartyKind,
		"risk_level":     vars.RiskLevel,
		"age":            int64(vars.Age),
		"balance_units":  vars.BalanceUnits,
		"month_tx_count": int64(vars.MonthTxCount),
	})
	if err != nil {
		return false
	}
	satisfied, ok := out.Value().(bool)
	return ok && satisfied
}
