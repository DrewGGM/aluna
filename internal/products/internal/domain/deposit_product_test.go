package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/DrewGGM/aluna/internal/calendar"
	"github.com/DrewGGM/aluna/internal/money"
)

var schemaPath = filepath.Join("..", "..", "schema", "deposit-product.v1.json")

// invalidFiles are the invalid testdata files and the error each one gives.
var invalidFiles = map[string]string{
	"invalid-missing-day-count.json":  "day_count: required",
	"invalid-dormancy-days-type.json": "dormancy_days: wrong type",
	"invalid-rate-above-100.json":     `rate_tiers[0].rate_ea: "1.05" is not a fraction from 0 to 1`,
	"invalid-tiers-overlap.json":      "rate_tiers[2].from_units: not above the previous tier",
	"invalid-tiers-unordered.json":    "rate_tiers[2].from_units: not above the previous tier",
	"invalid-unknown-day-count.json":  `day_count: unknown "30/360"`,
}

// validFiles are the valid testdata files and what each one reads as.
func validFiles(t *testing.T) map[string]DepositProduct {
	t.Helper()
	cop, err := money.CurrencyOf("COP")
	if err != nil {
		t.Fatal(err)
	}
	accounts := func(product string) AccountMapping {
		return AccountMapping{Cash: "A.CASH.VAULT", InterestExpense: "E.INTEREST." + product, FeeIncome: "I.FEES." + product, GMFPayable: "L.TAX.GMF"}
	}
	return map[string]DepositProduct{
		"valid-savings.json": {
			Currency: cop, DayCount: ACT365F, RateEA: []RateTier{{0, precise(t, "0.015")}}, CapitalizationFrequency: Monthly,
			DateConvention: calendar.Following, DormancyDays: 365, AllowedKYC: []string{"approved"}, GMFExemptEligible: true,
			Accounts: accounts("SAVINGS"),
		},
		"valid-tiered-savings.json": {
			Currency: cop, DayCount: ACT360, CapitalizationFrequency: Quarterly, DateConvention: calendar.ModifiedFollowing,
			RateEA: []RateTier{{0, precise(t, "0.01")}, {1000000000, precise(t, "0.045")}, {5000000000, precise(t, "0.0625")}},
			Fees: []FeeRule{
				{Code: "withdrawal_fee", Trigger: OnWithdrawal, AmountUnits: 450000, ExemptRule: "first_withdrawals_free"},
				{Code: "transfer_fee", Trigger: OnTransfer, AmountUnits: 300000},
			},
			Limits:       Limits{PerOperation: Limit{Units: 2000000000}, DailyDebits: Limit{Units: 5000000000}},
			DormancyDays: 180, AllowedKYC: []string{"approved"}, Accounts: accounts("TIERED"),
			Rules: map[string]string{"first_withdrawals_free": "month_tx_count < 3"},
		},
		"valid-simplified-deposit.json": {
			Currency: cop, DayCount: ACT365F, RateEA: []RateTier{{0, precise(t, "0")}}, CapitalizationFrequency: Monthly,
			DateConvention: calendar.Following, Limits: Limits{MonthlyDebits: Limit{SMMLV: 3}, MaxBalance: Limit{SMMLV: 3}},
			DormancyDays: 365, AllowedKYC: []string{"pending", "approved"}, GMFExemptEligible: true, Accounts: accounts("SIMPLIFIED"),
		},
	}
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path) //nolint:gosec // the test's own fixtures
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func precise(t *testing.T, s string) money.Precise {
	t.Helper()
	p, err := money.ParsePrecise(s)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// TestTheValidProductsAreRead: each valid testdata file is read with all its fields.
func TestTheValidProductsAreRead(t *testing.T) {
	for name, want := range validFiles(t) {
		t.Run(name, func(t *testing.T) {
			got, err := ParseDepositProduct(readFile(t, filepath.Join("testdata", name)))
			// %+v prints the rates with Precise.String, so it compares them by value.
			if err != nil || fmt.Sprintf("%+v", got) != fmt.Sprintf("%+v", want) {
				t.Errorf("got %+v, %v\nwant %+v", got, err, want)
			}
		})
	}
}

// TestTheInvalidProductsAreRejected: each invalid testdata file is ErrInvalidProduct with its error.
func TestTheInvalidProductsAreRejected(t *testing.T) {
	for name, want := range invalidFiles {
		t.Run(name, func(t *testing.T) {
			_, err := ParseDepositProduct(readFile(t, filepath.Join("testdata", name)))
			if !errors.Is(err, ErrInvalidProduct) || !strings.Contains(err.Error(), want) {
				t.Errorf("got %v, want ErrInvalidProduct with %q", err, want)
			}
		})
	}
}

// TestEveryTestdataFileIsChecked: testdata has 3 valid and 6 invalid files, and no file that no test reads.
func TestEveryTestdataFileIsChecked(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("testdata", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	for _, p := range paths {
		files = append(files, filepath.Base(p))
	}
	valid := validFiles(t)
	checked := slices.Sorted(maps.Keys(valid))
	checked = append(checked, slices.Collect(maps.Keys(invalidFiles))...)
	slices.Sort(files)
	slices.Sort(checked)
	if !slices.Equal(files, checked) || len(valid) != 3 || len(invalidFiles) != 6 {
		t.Errorf("testdata = %v, the tests check %v (%d valid, %d invalid)", files, checked, len(valid), len(invalidFiles))
	}
}

// jsonTree reads the JSON file at path as a tree.
func jsonTree(t *testing.T, path string) map[string]any {
	t.Helper()
	var tree map[string]any
	if err := json.Unmarshal(readFile(t, path), &tree); err != nil {
		t.Fatal(err)
	}
	return tree
}

// pointer is the node of tree at a JSON pointer such as "/properties/day_count/enum" or "/rate_tiers/0";
// the empty pointer is tree.
func pointer(t *testing.T, tree map[string]any, ptr string) any {
	t.Helper()
	var node any = tree
	for step := range strings.SplitSeq(ptr, "/") {
		if step == "" {
			continue
		}
		if i, err := strconv.Atoi(step); err == nil {
			node = array(t, node)[i]
			continue
		}
		var ok bool
		if node, ok = object(t, node)[step]; !ok {
			t.Fatalf("%s: there is no %s", ptr, step)
		}
	}
	return node
}

// object is v as a JSON object, or the test fails.
func object(t *testing.T, v any) map[string]any {
	t.Helper()
	o, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("%v is not an object", v)
	}
	return o
}

// array is v as a JSON array, or the test fails.
func array(t *testing.T, v any) []any {
	t.Helper()
	a, ok := v.([]any)
	if !ok {
		t.Fatalf("%v is not an array", v)
	}
	return a
}

// document is a valid testdata file as a tree, for a test to change it.
func document(t *testing.T, name string) map[string]any {
	t.Helper()
	return jsonTree(t, filepath.Join("testdata", name))
}

func parseDocument(t *testing.T, doc map[string]any) error {
	t.Helper()
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	_, err = ParseDepositProduct(raw)
	return err
}

// fieldCases set key of the object at path of valid-tiered-savings.json to value, and the definition is
// rejected with want.
var fieldCases = []struct {
	name, path, key string
	value           any
	want            string
}{
	{"unsupported currency", "", "currency", "XYZ", `currency: unknown currency: "XYZ"`},
	{"first tier above 0", "/rate_tiers/0", "from_units", 100, "rate_tiers[0].from_units: the first tier starts at 0"},
	{"negative tier", "/rate_tiers/1", "from_units", -1, "rate_tiers[1].from_units: not above the previous tier"},
	{"rate with exponent", "/rate_tiers/0", "rate_ea", "1e-2", `rate_tiers[0].rate_ea: "1e-2" is not a fraction from 0 to 1`},
	{"rate with 9 decimals", "/rate_tiers/0", "rate_ea", "0.123456789", "rate_tiers[0].rate_ea: \"0.123456789\" is not a fraction"},
	{"no tiers", "", "rate_tiers", []any{}, "rate_tiers: required"},
	{"unknown frequency", "", "capitalization_frequency", "weekly", `capitalization_frequency: unknown "weekly"`},
	{"unknown convention", "", "date_convention", "nearest", `date_convention: unknown "nearest"`},
	{"repeated fee code", "/fees/1", "code", "withdrawal_fee", `fees[1].code: "withdrawal_fee" is repeated`},
	{"fee code with capitals", "/fees/0", "code", "Fee", `fees[0].code: "Fee" is not a name`},
	{"unknown trigger", "/fees/0", "trigger", "deposit", `fees[0].trigger: unknown "deposit"`},
	{"free fee", "/fees/0", "amount_units", 0, "fees[0].amount_units: must be at least 1"},
	{"exempt by a missing rule", "/fees/0", "exempt_rule", "vip", `fees[0].exempt_rule: no rule "vip"`},
	{"empty exemption", "/fees/0", "exempt_rule", "", "fees[0].exempt_rule: required"},
	{"limit in units and SMMLV", "/limits/per_operation", "smmlv", 3, "limits.per_operation: units or smmlv, not both"},
	{"limit of 0 units", "/limits/per_operation", "units", 0, "limits.per_operation.units: must be at least 1"},
	{"too many SMMLV", "/limits", "max_balance", map[string]any{"smmlv": 1001}, "limits.max_balance.smmlv: must be from 1 to 1000"},
	{"empty limit", "/limits", "max_balance", map[string]any{}, "limits.max_balance: units or smmlv is required"},
	{"no dormancy", "", "dormancy_days", 0, "dormancy_days: must be from 1 to 3650"},
	{"dormancy above ten years", "", "dormancy_days", 3651, "dormancy_days: must be from 1 to 3650"},
	{"unknown KYC status", "", "allowed_kyc", []any{"verified"}, `allowed_kyc[0]: unknown "verified"`},
	{"repeated KYC status", "", "allowed_kyc", []any{"approved", "approved"}, `allowed_kyc[1]: "approved" is repeated`},
	{"no KYC status", "", "allowed_kyc", []any{}, "allowed_kyc: required"},
	{"account code in lowercase", "/accounts", "cash", "a.cash", `accounts.cash: "a.cash" is not an account code`},
	{"account code too long", "/accounts", "cash", "A." + strings.Repeat("X", 63), "accounts.cash: longer than 64 characters"},
	{"rule name too long", "/rules", strings.Repeat("r", 65), "true", `rules: "rrrr`},
	{"empty rule", "/rules", "first_withdrawals_free", "", "rules.first_withdrawals_free: must have from 1 to 4096 characters"},
	{"unknown field", "", "interest_rate", "0.05", `unknown field "interest_rate"`},
	{"unknown tier field", "/rate_tiers/0", "to_units", 10, `unknown field "to_units"`},
	{"rule name in capitals", "/rules", "Free", "true", `key "Free" is not lowercase`},
	{"field in capitals", "", "CURRENCY", "USD", `key "CURRENCY" is not lowercase`},
	{"field with a Kelvin sign", "", "allowed_" + string(rune(0x212A)) + "yc", []any{"approved"}, `key "allowed_\u212ayc" is not lowercase`},
	{"null field", "", "fees", nil, "null is not a value of a definition"},
	{"null limit", "/limits", "max_balance", nil, "null is not a value of a definition"},
	{"null exemption", "/fees/0", "exempt_rule", nil, "null is not a value of a definition"},
}

// TestTheParserChecksEachField: the checks the schema expresses and the ones it cannot (supported currency,
// tiers from 0, unique fee codes, rules that exist).
func TestTheParserChecksEachField(t *testing.T) {
	for _, c := range fieldCases {
		t.Run(c.name, func(t *testing.T) {
			doc := document(t, "valid-tiered-savings.json")
			object(t, pointer(t, doc, c.path))[c.key] = c.value

			err := parseDocument(t, doc)

			if !errors.Is(err, ErrInvalidProduct) || !strings.Contains(err.Error(), c.want) {
				t.Errorf("got %v, want ErrInvalidProduct with %q", err, c.want)
			}
		})
	}
}

// TestAKeyIsReadOnce: encoding/json keeps the last of a repeated key; the definition is rejected, so it
// cannot read differently from what its approver saw.
func TestAKeyIsReadOnce(t *testing.T) {
	raw := string(readFile(t, filepath.Join("testdata", "valid-tiered-savings.json")))
	cases := map[string]string{
		"field":        strings.Replace(raw, `"currency": "COP",`, `"currency": "COP", "currency": "USD",`, 1),
		"tier field":   strings.Replace(raw, `"rate_ea": "0.01"`, `"rate_ea": "0.01", "rate_ea": "0.99"`, 1),
		"rule":         strings.Replace(raw, `"first_withdrawals_free": "month_tx_count < 3"`, `"first_withdrawals_free": "true", "first_withdrawals_free": "false"`, 1),
		"limit amount": strings.Replace(raw, `"units": 2000000000`, `"units": 2000000000, "units": 1`, 1),
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			if doc == raw {
				t.Fatal("the case did not change the document")
			}
			if _, err := ParseDepositProduct([]byte(doc)); !errors.Is(err, ErrInvalidProduct) || !strings.Contains(err.Error(), "is repeated") {
				t.Errorf("got %v, want ErrInvalidProduct: repeated key", err)
			}
		})
	}
}

// TestOnlyOneDocumentIsRead: data after the definition is not ignored.
func TestOnlyOneDocumentIsRead(t *testing.T) {
	raw := readFile(t, filepath.Join("testdata", "valid-savings.json"))
	for _, extra := range []string{"{}", "x"} {
		_, err := ParseDepositProduct(append(slices.Clone(raw), extra...))
		if !errors.Is(err, ErrInvalidProduct) {
			t.Errorf("definition followed by %q = %v, want ErrInvalidProduct", extra, err)
		}
	}
}

// TestTooManyItemsAreRejected: the bounds of the lists and of the rules.
func TestTooManyItemsAreRejected(t *testing.T) {
	tiers := make([]any, maxTiers+1)
	for i := range tiers {
		tiers[i] = map[string]any{"from_units": i, "rate_ea": "0.01"}
	}
	fees := make([]any, maxFees+1)
	for i := range fees {
		fees[i] = map[string]any{"code": fmt.Sprintf("fee_%d", i), "trigger": "transfer", "amount_units": 1}
	}
	rules := map[string]any{}
	for i := range maxRules + 1 {
		rules[fmt.Sprintf("rule_%d", i)] = "true"
	}
	cases := map[string]any{"rate_tiers": tiers, "fees": fees, "rules": rules}
	for key, value := range cases {
		t.Run(key, func(t *testing.T) {
			doc := document(t, "valid-savings.json")
			doc[key] = value

			err := parseDocument(t, doc)

			if !errors.Is(err, ErrInvalidProduct) || !strings.Contains(err.Error(), key+": at most") {
				t.Errorf("got %v, want ErrInvalidProduct: at most", err)
			}
		})
	}
}

// TestTheSchemaHasTheParsersValues: the schema's enums, patterns and bounds are the parser's. The literal
// ones are the parser's minimums, which TestTheParserChecksEachField tries at their bound.
func TestTheSchemaHasTheParsersValues(t *testing.T) {
	s := jsonTree(t, schemaPath)
	cases := map[string]any{
		"/properties/day_count/enum":                       dayCounts,
		"/properties/capitalization_frequency/enum":        frequencies,
		"/properties/date_convention/enum":                 conventions,
		"/properties/allowed_kyc/items/enum":               kycStatuses,
		"/$defs/fee/properties/trigger/enum":               feeTriggers,
		"/$defs/rateTier/properties/rate_ea/pattern":       ratePattern.String(),
		"/$defs/fee/properties/code/pattern":               namePattern.String(),
		"/$defs/fee/properties/exempt_rule/pattern":        namePattern.String(),
		"/properties/rules/propertyNames/pattern":          namePattern.String(),
		"/$defs/accountCode/pattern":                       accountCodePattern.String(),
		"/$defs/accountCode/maxLength":                     maxAccountCode,
		"/properties/rate_tiers/maxItems":                  maxTiers,
		"/properties/fees/maxItems":                        maxFees,
		"/properties/rules/maxProperties":                  maxRules,
		"/properties/rules/additionalProperties/maxLength": maxRuleLength,
		"/properties/dormancy_days/maximum":                maxDormancyDays,
		"/$defs/limit/properties/smmlv/maximum":            maxSMMLV,
		"/properties/rate_tiers/minItems":                  1,
		"/properties/allowed_kyc/minItems":                 1,
		"/properties/allowed_kyc/uniqueItems":              true,
		"/properties/rules/additionalProperties/minLength": 1,
		"/properties/dormancy_days/minimum":                1,
		"/$defs/rateTier/properties/from_units/minimum":    0,
		"/$defs/fee/properties/amount_units/minimum":       1,
		"/$defs/limit/properties/units/minimum":            1,
		"/$defs/limit/properties/smmlv/minimum":            1,
		"/$defs/limit/minProperties":                       1,
		"/$defs/limit/maxProperties":                       1,
	}
	for ptr, want := range cases {
		got, err := json.Marshal(pointer(t, s, ptr))
		if err != nil {
			t.Fatal(err)
		}
		if w, _ := json.Marshal(want); string(got) != string(w) {
			t.Errorf("%s = %s, the parser has %s", ptr, got, w)
		}
	}
}

// TestTheSchemaHasTheParsersFields: each object of the schema has the fields of the type the parser reads
// it into.
func TestTheSchemaHasTheParsersFields(t *testing.T) {
	s := jsonTree(t, schemaPath)
	cases := map[string]any{
		"/properties":                depositProductJSON{},
		"/$defs/rateTier/properties": rateTierJSON{},
		"/$defs/fee/properties":      feeRuleJSON{},
		"/$defs/limits/properties":   limitsJSON{},
		"/$defs/limit/properties":    limitJSON{},
		"/$defs/accounts/properties": accountMappingJSON{},
	}
	for ptr, v := range cases {
		var fields []string
		for f := range reflect.TypeOf(v).Fields() {
			fields = append(fields, strings.Split(f.Tag.Get("json"), ",")[0])
		}
		properties := slices.Sorted(maps.Keys(object(t, pointer(t, s, ptr))))
		slices.Sort(fields)
		if !slices.Equal(fields, properties) {
			t.Errorf("%s = %v, the parser reads %v", ptr, properties, fields)
		}
	}
}

// TestTheSchemasRequiredFieldsAreRequired: without any field the schema requires, the parser rejects the
// definition. The optional ones are absent in some valid testdata file.
func TestTheSchemasRequiredFieldsAreRequired(t *testing.T) {
	s := jsonTree(t, schemaPath)
	where := map[string]string{"/required": "", "/$defs/rateTier/required": "/rate_tiers/1", "/$defs/fee/required": "/fees/0", "/$defs/accounts/required": "/accounts"}
	for ptr, path := range where {
		for _, k := range array(t, pointer(t, s, ptr)) {
			key, ok := k.(string)
			if !ok {
				t.Fatalf("%s has %v, not a field name", ptr, k)
			}
			t.Run(path+"/"+key, func(t *testing.T) {
				doc := document(t, "valid-tiered-savings.json")
				delete(object(t, pointer(t, doc, path)), key)

				err := parseDocument(t, doc)

				if !errors.Is(err, ErrInvalidProduct) || !strings.Contains(err.Error(), key+": required") {
					t.Errorf("got %v, want ErrInvalidProduct: required", err)
				}
			})
		}
	}
}
