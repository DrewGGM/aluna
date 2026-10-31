// Package domain holds the definition of the products (ADR-0008): pure, without I/O.
package domain

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"regexp"
	"slices"
	"unicode/utf8"

	"github.com/DrewGGM/aluna/internal/calendar"
	"github.com/DrewGGM/aluna/internal/money"
)

// ErrInvalidProduct means a product definition that does not follow deposit-product.v1.json or the rules
// the schema cannot express.
var ErrInvalidProduct = errors.New("invalid product definition")

// DayCount is the day count convention of the interest.
type DayCount string

// Day count conventions. ACT/ACT is the ISDA one: each day counts over the length of its own year.
const (
	ACT365F DayCount = "ACT/365F"
	ACT360  DayCount = "ACT/360"
	E30360  DayCount = "30E/360"
	ACTACT  DayCount = "ACT/ACT"
)

// Frequency is how often the accrued interest is capitalized.
type Frequency string

// Capitalization frequencies.
const (
	Monthly    Frequency = "monthly"
	Quarterly  Frequency = "quarterly"
	Semiannual Frequency = "semiannual"
	Annual     Frequency = "annual"
)

// RateTier is the effective annual rate, as a fraction, of the balance slab that starts at FromUnits.
type RateTier struct {
	FromUnits int64
	RateEA    money.Precise
}

// FeeTrigger is the operation a fee charges.
type FeeTrigger string

// Operations that are charged.
const (
	OnWithdrawal FeeTrigger = "withdrawal"
	OnTransfer   FeeTrigger = "transfer"
)

// FeeRule is a fee of AmountUnits on each Trigger operation, unless the rule named ExemptRule is satisfied.
// An empty ExemptRule has no exemption.
type FeeRule struct {
	Code        string
	Trigger     FeeTrigger
	AmountUnits int64
	ExemptRule  string
}

// Limit is an amount in minor units or a number of SMMLV; only one of the two is set. The zero Limit does
// not apply.
type Limit struct {
	Units int64
	SMMLV int64
}

// Limits are the limits of the product: per operation, of the debits of the day and of the month, and of
// the balance.
type Limits struct {
	PerOperation  Limit
	DailyDebits   Limit
	MonthlyDebits Limit
	MaxBalance    Limit
}

// AccountMapping are the codes of the internal ledger accounts the product posts against.
type AccountMapping struct {
	Cash            string
	InterestExpense string
	FeeIncome       string
	GMFPayable      string
}

// DepositProduct is the definition of a version of a deposit product. Rules are named CEL predicates.
type DepositProduct struct {
	Currency                money.Currency
	DayCount                DayCount
	RateEA                  []RateTier
	CapitalizationFrequency Frequency
	DateConvention          calendar.Convention
	Fees                    []FeeRule
	Limits                  Limits
	DormancyDays            int
	AllowedKYC              []string
	GMFExemptEligible       bool
	Accounts                AccountMapping
	Rules                   map[string]string
}

// The values the parser accepts; deposit-product.v1.json has the same ones. kycStatuses are party's
// (products does not import party).
var (
	dayCounts          = []DayCount{ACT365F, ACT360, E30360, ACTACT}
	frequencies        = []Frequency{Monthly, Quarterly, Semiannual, Annual}
	conventions        = []calendar.Convention{calendar.Following, calendar.ModifiedFollowing, calendar.Preceding, calendar.Unadjusted}
	feeTriggers        = []FeeTrigger{OnWithdrawal, OnTransfer}
	kycStatuses        = []string{"pending", "approved", "rejected", "suspended"}
	ratePattern        = regexp.MustCompile(`^(0(\.[0-9]{1,8})?|1(\.0{1,8})?)$`)
	namePattern        = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	accountCodePattern = regexp.MustCompile(`^[A-Z][A-Z0-9]*(\.[A-Z0-9_]+)*$`)
	// keyPattern is the shape of every key of a definition: the schema's properties and the rule names.
	keyPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
)

// Bounds of a definition.
const (
	maxTiers        = 16
	maxFees         = 32
	maxRules        = 32
	maxRuleLength   = 4096
	maxDormancyDays = 3650
	maxSMMLV        = 1000
	maxAccountCode  = 64
)

// depositProductJSON is a definition as deposit-product.v1.json describes it. A pointer tells a missing
// field from its zero value.
type depositProductJSON struct {
	Currency                string              `json:"currency"`
	DayCount                DayCount            `json:"day_count"`
	RateTiers               []rateTierJSON      `json:"rate_tiers"`
	CapitalizationFrequency Frequency           `json:"capitalization_frequency"`
	DateConvention          calendar.Convention `json:"date_convention"`
	Fees                    []feeRuleJSON       `json:"fees"`
	Limits                  limitsJSON          `json:"limits"`
	DormancyDays            *int                `json:"dormancy_days"`
	AllowedKYC              []string            `json:"allowed_kyc"`
	GMFExemptEligible       *bool               `json:"gmf_exempt_eligible"`
	Accounts                *accountMappingJSON `json:"accounts"`
	Rules                   map[string]string   `json:"rules"`
}

type rateTierJSON struct {
	FromUnits *int64 `json:"from_units"`
	RateEA    string `json:"rate_ea"`
}

type feeRuleJSON struct {
	Code        string     `json:"code"`
	Trigger     FeeTrigger `json:"trigger"`
	AmountUnits *int64     `json:"amount_units"`
	ExemptRule  *string    `json:"exempt_rule"`
}

type limitsJSON struct {
	PerOperation  *limitJSON `json:"per_operation"`
	DailyDebits   *limitJSON `json:"daily_debits"`
	MonthlyDebits *limitJSON `json:"monthly_debits"`
	MaxBalance    *limitJSON `json:"max_balance"`
}

type limitJSON struct {
	Units *int64 `json:"units"`
	SMMLV *int64 `json:"smmlv"`
}

type accountMappingJSON struct {
	Cash            string `json:"cash"`
	InterestExpense string `json:"interest_expense"`
	FeeIncome       string `json:"fee_income"`
	GMFPayable      string `json:"gmf_payable"`
}

// ParseDepositProduct reads a definition that follows deposit-product.v1.json, or ErrInvalidProduct with
// the field that fails. A wrong type is reported with the path encoding/json gives (rate_tiers.from_units,
// without the index).
func ParseDepositProduct(raw []byte) (DepositProduct, error) {
	if err := checkKeys(raw); err != nil {
		return DepositProduct{}, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var in depositProductJSON
	if err := dec.Decode(&in); err != nil {
		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &typeErr) {
			return DepositProduct{}, invalid(cmp.Or(typeErr.Field, "definition"), "wrong type")
		}
		return DepositProduct{}, fmt.Errorf("%w: %w", ErrInvalidProduct, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return DepositProduct{}, fmt.Errorf("%w: data after the definition", ErrInvalidProduct)
	}
	return in.toDomain()
}

// jsonLevel is an object or an array open while checkKeys reads a document; keys is nil in an array.
type jsonLevel struct {
	keys    map[string]bool
	wantKey bool
}

// checkKeys rejects what encoding/json lets through and the schema does not: it matches keys without
// regard to case and keeps the last of a repeated key, so a definition could read differently from what
// its approver saw. It also rejects null, which no field of the schema admits.
func checkKeys(raw []byte) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	var levels []*jsonLevel
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("%w: %w", ErrInvalidProduct, err)
		}
		if levels, err = next(levels, tok); err != nil {
			return err
		}
	}
}

// next takes tok into the open levels and returns them.
func next(levels []*jsonLevel, tok json.Token) ([]*jsonLevel, error) {
	if n := len(levels); n > 0 && levels[n-1].wantKey && tok != json.Delim('}') {
		return levels, levels[n-1].add(tok)
	}
	switch tok {
	case nil:
		return nil, fmt.Errorf("%w: null is not a value of a definition", ErrInvalidProduct)
	case json.Delim('{'):
		return append(levels, &jsonLevel{keys: map[string]bool{}, wantKey: true}), nil
	case json.Delim('['):
		return append(levels, &jsonLevel{}), nil
	case json.Delim('}'), json.Delim(']'):
		levels = levels[:len(levels)-1]
	}
	if n := len(levels); n > 0 && levels[n-1].keys != nil {
		levels[n-1].wantKey = true // a value of the object ended: a key or its end comes next
	}
	return levels, nil
}

func (l *jsonLevel) add(tok json.Token) error {
	key, _ := tok.(string) // the decoder only gives a string where a key goes
	if !keyPattern.MatchString(key) {
		return fmt.Errorf("%w: key %+.64q is not lowercase letters, digits and _", ErrInvalidProduct, key)
	}
	if l.keys[key] {
		return fmt.Errorf("%w: key %.64q is repeated", ErrInvalidProduct, key)
	}
	l.keys[key], l.wantKey = true, false
	return nil
}

func invalid(field, detail string) error {
	return fmt.Errorf("%w: %s: %s", ErrInvalidProduct, field, detail)
}

// oneOf checks that v, required, is one of known.
func oneOf[T ~string](field string, v T, known []T) error {
	if v == "" {
		return invalid(field, "required")
	}
	if !slices.Contains(known, v) {
		return invalid(field, fmt.Sprintf("unknown %.64q", string(v)))
	}
	return nil
}

// toDomain checks the fields in the order of the schema and reports the first that fails.
func (in depositProductJSON) toDomain() (DepositProduct, error) {
	var p DepositProduct
	var errs [12]error
	p.Currency, errs[0] = currency(in.Currency)
	p.DayCount, errs[1] = in.DayCount, oneOf("day_count", in.DayCount, dayCounts)
	p.RateEA, errs[2] = rateTiers(in.RateTiers)
	p.CapitalizationFrequency, errs[3] = in.CapitalizationFrequency, oneOf("capitalization_frequency", in.CapitalizationFrequency, frequencies)
	p.DateConvention, errs[4] = in.DateConvention, oneOf("date_convention", in.DateConvention, conventions)
	p.Fees, errs[5] = feeRules(in.Fees, in.Rules)
	p.Limits, errs[6] = in.Limits.toDomain()
	p.DormancyDays, errs[7] = dormancyDays(in.DormancyDays)
	p.AllowedKYC, errs[8] = in.AllowedKYC, allowedKYC(in.AllowedKYC)
	p.GMFExemptEligible, errs[9] = gmfExemptEligible(in.GMFExemptEligible)
	p.Accounts, errs[10] = in.Accounts.toDomain()
	p.Rules, errs[11] = in.Rules, rules(in.Rules)
	if err := cmp.Or(errs[:]...); err != nil {
		return DepositProduct{}, err
	}
	return p, nil
}

func currency(code string) (money.Currency, error) {
	if code == "" {
		return money.Currency{}, invalid("currency", "required")
	}
	c, err := money.CurrencyOf(code)
	if err != nil {
		return money.Currency{}, fmt.Errorf("%w: currency: %w", ErrInvalidProduct, err)
	}
	return c, nil
}

// rateTiers reads the tiers: the first starts at 0 and each one above the previous one, so they do not
// overlap.
func rateTiers(in []rateTierJSON) ([]RateTier, error) {
	if len(in) == 0 {
		return nil, invalid("rate_tiers", "required")
	}
	if len(in) > maxTiers {
		return nil, invalid("rate_tiers", fmt.Sprintf("at most %d tiers", maxTiers))
	}
	tiers := make([]RateTier, len(in))
	for i, t := range in {
		field := fmt.Sprintf("rate_tiers[%d]", i)
		tier, err := t.toDomain(field)
		switch {
		case err != nil:
			return nil, err
		case i == 0 && tier.FromUnits != 0:
			return nil, invalid(field+".from_units", "the first tier starts at 0")
		case i > 0 && tier.FromUnits <= tiers[i-1].FromUnits:
			return nil, invalid(field+".from_units", "not above the previous tier")
		}
		tiers[i] = tier
	}
	return tiers, nil
}

func (t rateTierJSON) toDomain(field string) (RateTier, error) {
	if t.FromUnits == nil {
		return RateTier{}, invalid(field+".from_units", "required")
	}
	if t.RateEA == "" {
		return RateTier{}, invalid(field+".rate_ea", "required")
	}
	if !ratePattern.MatchString(t.RateEA) {
		return RateTier{}, invalid(field+".rate_ea", fmt.Sprintf("%.64q is not a fraction from 0 to 1", t.RateEA))
	}
	rate, err := money.ParsePrecise(t.RateEA)
	if err != nil {
		return RateTier{}, fmt.Errorf("%w: %s.rate_ea: %w", ErrInvalidProduct, field, err)
	}
	return RateTier{FromUnits: *t.FromUnits, RateEA: rate}, nil
}

// feeRules reads the fees, whose codes are unique and whose exemptions name one of rules.
func feeRules(in []feeRuleJSON, rules map[string]string) ([]FeeRule, error) {
	if len(in) > maxFees {
		return nil, invalid("fees", fmt.Sprintf("at most %d fees", maxFees))
	}
	var fees []FeeRule
	for i, f := range in {
		field := fmt.Sprintf("fees[%d]", i)
		fee, err := f.toDomain(field, rules)
		if err != nil {
			return nil, err
		}
		if slices.ContainsFunc(fees, func(other FeeRule) bool { return other.Code == fee.Code }) {
			return nil, invalid(field+".code", fmt.Sprintf("%.64q is repeated", fee.Code))
		}
		fees = append(fees, fee)
	}
	return fees, nil
}

func (f feeRuleJSON) toDomain(field string, rules map[string]string) (FeeRule, error) {
	if err := name(field+".code", f.Code); err != nil {
		return FeeRule{}, err
	}
	if err := oneOf(field+".trigger", f.Trigger, feeTriggers); err != nil {
		return FeeRule{}, err
	}
	if f.AmountUnits == nil {
		return FeeRule{}, invalid(field+".amount_units", "required")
	}
	if *f.AmountUnits < 1 {
		return FeeRule{}, invalid(field+".amount_units", "must be at least 1")
	}
	fee := FeeRule{Code: f.Code, Trigger: f.Trigger, AmountUnits: *f.AmountUnits}
	if f.ExemptRule == nil {
		return fee, nil
	}
	if err := name(field+".exempt_rule", *f.ExemptRule); err != nil {
		return FeeRule{}, err
	}
	if _, ok := rules[*f.ExemptRule]; !ok {
		return FeeRule{}, invalid(field+".exempt_rule", fmt.Sprintf("no rule %.64q", *f.ExemptRule))
	}
	fee.ExemptRule = *f.ExemptRule
	return fee, nil
}

func name(field, s string) error {
	if s == "" {
		return invalid(field, "required")
	}
	if !namePattern.MatchString(s) {
		return invalid(field, fmt.Sprintf("%.64q is not a name", s))
	}
	return nil
}

func (in limitsJSON) toDomain() (Limits, error) {
	var l Limits
	var errs [4]error
	l.PerOperation, errs[0] = limit("limits.per_operation", in.PerOperation)
	l.DailyDebits, errs[1] = limit("limits.daily_debits", in.DailyDebits)
	l.MonthlyDebits, errs[2] = limit("limits.monthly_debits", in.MonthlyDebits)
	l.MaxBalance, errs[3] = limit("limits.max_balance", in.MaxBalance)
	if err := cmp.Or(errs[:]...); err != nil {
		return Limits{}, err
	}
	return l, nil
}

// limit reads an optional limit: nil is the zero Limit, which does not apply.
func limit(field string, in *limitJSON) (Limit, error) {
	switch {
	case in == nil:
		return Limit{}, nil
	case in.Units != nil && in.SMMLV != nil:
		return Limit{}, invalid(field, "units or smmlv, not both")
	case in.Units != nil && *in.Units < 1:
		return Limit{}, invalid(field+".units", "must be at least 1")
	case in.Units != nil:
		return Limit{Units: *in.Units}, nil
	case in.SMMLV == nil:
		return Limit{}, invalid(field, "units or smmlv is required")
	case *in.SMMLV < 1 || *in.SMMLV > maxSMMLV:
		return Limit{}, invalid(field+".smmlv", fmt.Sprintf("must be from 1 to %d", maxSMMLV))
	}
	return Limit{SMMLV: *in.SMMLV}, nil
}

func (in *accountMappingJSON) toDomain() (AccountMapping, error) {
	if in == nil {
		return AccountMapping{}, invalid("accounts", "required")
	}
	if err := cmp.Or(
		accountCode("accounts.cash", in.Cash),
		accountCode("accounts.interest_expense", in.InterestExpense),
		accountCode("accounts.fee_income", in.FeeIncome),
		accountCode("accounts.gmf_payable", in.GMFPayable),
	); err != nil {
		return AccountMapping{}, err
	}
	return AccountMapping{Cash: in.Cash, InterestExpense: in.InterestExpense, FeeIncome: in.FeeIncome, GMFPayable: in.GMFPayable}, nil
}

func accountCode(field, code string) error {
	switch {
	case code == "":
		return invalid(field, "required")
	case len(code) > maxAccountCode:
		return invalid(field, fmt.Sprintf("longer than %d characters", maxAccountCode))
	case !accountCodePattern.MatchString(code):
		return invalid(field, fmt.Sprintf("%.64q is not an account code", code))
	}
	return nil
}

func dormancyDays(days *int) (int, error) {
	if days == nil {
		return 0, invalid("dormancy_days", "required")
	}
	if *days < 1 || *days > maxDormancyDays {
		return 0, invalid("dormancy_days", fmt.Sprintf("must be from 1 to %d", maxDormancyDays))
	}
	return *days, nil
}

func allowedKYC(statuses []string) error {
	if len(statuses) == 0 {
		return invalid("allowed_kyc", "required")
	}
	for i, s := range statuses {
		field := fmt.Sprintf("allowed_kyc[%d]", i)
		if err := oneOf(field, s, kycStatuses); err != nil {
			return err
		}
		if slices.Contains(statuses[:i], s) {
			return invalid(field, fmt.Sprintf("%.64q is repeated", s))
		}
	}
	return nil
}

func gmfExemptEligible(v *bool) (bool, error) {
	if v == nil {
		return false, invalid("gmf_exempt_eligible", "required")
	}
	return *v, nil
}

// rules checks the names and the length of the rules; compiling them is up to the rules engine (T-043).
func rules(in map[string]string) error {
	if len(in) > maxRules {
		return invalid("rules", fmt.Sprintf("at most %d rules", maxRules))
	}
	for _, n := range slices.Sorted(maps.Keys(in)) {
		if !namePattern.MatchString(n) {
			return invalid("rules", fmt.Sprintf("%.64q is not a name", n))
		}
		if length := utf8.RuneCountInString(in[n]); length < 1 || length > maxRuleLength {
			return invalid("rules."+n, fmt.Sprintf("must have from 1 to %d characters", maxRuleLength))
		}
	}
	return nil
}
