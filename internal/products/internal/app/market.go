package app

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/DrewGGM/aluna/internal/calendar"
	"github.com/DrewGGM/aluna/internal/money"
)

// MarketKind is the kind of a market parameter (FR-21).
type MarketKind string

// Kinds of market parameter. The usury rate goes by credit modality and the IBR by tenor; the others have
// no modality.
const (
	UsuryRate MarketKind = "usury_rate"
	UVT       MarketKind = "uvt"
	SMMLV     MarketKind = "smmlv"
	UVB       MarketKind = "uvb"
	IBR       MarketKind = "ibr"
)

var (
	// ErrInvalidMarketParameter means a market parameter that is not valid.
	ErrInvalidMarketParameter = errors.New("invalid market parameter")
	// ErrMarketParameterOverlap means another value of the same kind and modality holds on some day of the
	// period.
	ErrMarketParameterOverlap = errors.New("another value of the market parameter holds in that period")
	// ErrNoMarketParameter means no value of the parameter holds on that day: whoever needs it does not go on
	// without it (fail closed).
	ErrNoMarketParameter = errors.New("no value of the market parameter holds on that day")
)

// maxMarketDecimals is the precision a value is loaded with: the usury rate and the IBR are published with
// a few decimals as fractions, the UVT and the SMMLV in pesos.
const maxMarketDecimals = 8

var (
	marketKinds = []MarketKind{UsuryRate, UVT, SMMLV, UVB, IBR}
	// rates are the kinds that are fractions; they are also the ones that go by modality (the same shape as
	// a product code).
	rates = []MarketKind{UsuryRate, IBR}
	// valuePattern is the shape of a loaded value: digits, without sign or exponent; validValue bounds its
	// decimals.
	valuePattern = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?$`)
)

// ParseMarketValue reads the value of a parameter as it is loaded: a decimal without sign or exponent; or
// ErrInvalidMarketParameter.
func ParseMarketValue(text string) (money.Precise, error) {
	if !valuePattern.MatchString(text) {
		return money.Precise{}, fmt.Errorf("%w: the value %.64q is not a decimal without sign or exponent", ErrInvalidMarketParameter, text)
	}
	v, err := money.ParsePrecise(text)
	if err != nil {
		return money.Precise{}, fmt.Errorf("%w: %w", ErrInvalidMarketParameter, err)
	}
	return v, nil
}

// MarketParameter is the value of a parameter from ValidFrom (included) to ValidTo (excluded). A rate is
// an effective annual fraction (0.2531 is 25.31 % EA); the UVT, the SMMLV and the UVB are in pesos.
type MarketParameter struct {
	Kind               MarketKind
	Modality           string
	ValidFrom, ValidTo calendar.Date
	Value              money.Precise
}

// MarketParameters stores the series of market parameters.
type MarketParameters interface {
	InsertParameter(ctx context.Context, p MarketParameter) error
	Overlaps(ctx context.Context, p MarketParameter) (bool, error)
	ValueOn(ctx context.Context, kind MarketKind, modality string, on calendar.Date) (money.Precise, error)
}

// CheckMarketParameter reports whether p can be loaded: valid on its own and without overlapping another
// value, or ErrInvalidMarketParameter or ErrMarketParameterOverlap. A load request is checked before it waits
// for approval.
func CheckMarketParameter(ctx context.Context, s MarketParameters, p MarketParameter) error {
	if err := validMarketParameter(p); err != nil {
		return err
	}
	overlaps, err := s.Overlaps(ctx, p)
	if err != nil {
		return err
	}
	if overlaps {
		return fmt.Errorf("%w: %s %s", ErrMarketParameterOverlap, p.Kind, p.Modality)
	}
	return nil
}

// LoadMarketParameter stores p, or ErrInvalidMarketParameter or ErrMarketParameterOverlap (the database
// checks the overlap again, so two loads at once cannot both hold).
func LoadMarketParameter(ctx context.Context, s MarketParameters, p MarketParameter) error {
	if err := validMarketParameter(p); err != nil {
		return err
	}
	return s.InsertParameter(ctx, p)
}

// MarketValue is the value of parameter kind with its modality on day on, or ErrNoMarketParameter. An
// unknown kind or a modality the kind does not take is ErrInvalidMarketParameter, so a caller's mistake
// does not look like a missing value.
func MarketValue(ctx context.Context, s MarketParameters, kind MarketKind, modality string, on calendar.Date) (money.Precise, error) {
	if err := validKey(kind, modality); err != nil {
		return money.Precise{}, err
	}
	return s.ValueOn(ctx, kind, modality, on)
}

func validMarketParameter(p MarketParameter) error {
	if err := validKey(p.Kind, p.Modality); err != nil {
		return err
	}
	if !before(p.ValidFrom, p.ValidTo) {
		return fmt.Errorf("%w: valid_from is not before valid_to", ErrInvalidMarketParameter)
	}
	return validValue(p)
}

// validKey checks the kind and that it has a modality if, and only if, it is a rate.
func validKey(kind MarketKind, modality string) error {
	switch {
	case !slices.Contains(marketKinds, kind):
		return fmt.Errorf("%w: unknown kind %.64q", ErrInvalidMarketParameter, kind)
	case slices.Contains(rates, kind) && !codePattern.MatchString(modality):
		return fmt.Errorf("%w: %s needs a modality of lowercase letters, digits and _", ErrInvalidMarketParameter, kind)
	case !slices.Contains(rates, kind) && modality != "":
		return fmt.Errorf("%w: %s has no modality", ErrInvalidMarketParameter, kind)
	}
	return nil
}

// validValue checks that the value is positive, has at most maxMarketDecimals and, for a rate, is a
// fraction up to 1.
func validValue(p MarketParameter) error {
	var zero money.Precise
	if p.Value.Cmp(zero) <= 0 {
		return fmt.Errorf("%w: the value is not positive", ErrInvalidMarketParameter)
	}
	whole, decimals, _ := strings.Cut(p.Value.String(), ".")
	if len(decimals) > maxMarketDecimals {
		return fmt.Errorf("%w: the value has more than %d decimals", ErrInvalidMarketParameter, maxMarketDecimals)
	}
	if slices.Contains(rates, p.Kind) && !atMostOne(whole, decimals) {
		return fmt.Errorf("%w: %s is a fraction up to 1", ErrInvalidMarketParameter, p.Kind)
	}
	return nil
}

// atMostOne reports whether a positive decimal, split at its point, is at most 1: 0.x, 1 or 1.000...
func atMostOne(whole, decimals string) bool {
	return whole == "0" || whole == "1" && strings.Trim(decimals, "0") == ""
}

func before(a, b calendar.Date) bool {
	return cmp.Or(cmp.Compare(a.Year, b.Year), cmp.Compare(a.Month, b.Month), cmp.Compare(a.Day, b.Day)) < 0
}
