package money

import (
	"errors"
	"fmt"
	"math/big"
	"slices"
)

// ErrInvalidWeights indica pesos de reparto vacíos, negativos o que suman cero.
var ErrInvalidWeights = errors.New("pesos de reparto inválidos")

// Allocate reparte a en partes proporcionales a weights por el método del mayor residuo: cada parte
// recibe el piso de su cuota exacta y las unidades que sobran van de a una a las de mayor residuo; en
// empate, a la primera. Es determinista, las partes suman exactamente a y ninguna se aleja una unidad
// o más de su cuota exacta. Un monto negativo se reparte como su valor absoluto, con signo. Como Add,
// rechaza el monto sin moneda.
func (a Amount) Allocate(weights []int64) ([]Amount, error) {
	if a.cur.code == "" {
		return nil, fmt.Errorf("%w: reparto de un monto sin moneda", ErrUnknownCurrency)
	}
	total, err := totalWeight(weights)
	if err != nil {
		return nil, err
	}
	whole := new(big.Int).Abs(big.NewInt(a.units))
	shares := make([]*big.Int, len(weights))
	remainders := make([]*big.Int, len(weights))
	left := new(big.Int).Set(whole)
	for i, w := range weights {
		quota := new(big.Int).Mul(whole, big.NewInt(w))
		shares[i], remainders[i] = quota.QuoRem(quota, total, new(big.Int))
		left.Sub(left, shares[i])
	}
	// left < len(weights): es la suma de las partes fraccionarias.
	order := make([]int, len(weights))
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(i, j int) int { return remainders[j].Cmp(remainders[i]) })
	for _, i := range order[:left.Int64()] {
		shares[i].Add(shares[i], big.NewInt(1))
	}
	negative := a.units < 0
	parts := make([]Amount, len(weights))
	for i, s := range shares {
		if negative {
			s.Neg(s)
		}
		parts[i] = New(s.Int64(), a.cur)
	}
	return parts, nil
}

func totalWeight(weights []int64) (*big.Int, error) {
	if len(weights) == 0 {
		return nil, fmt.Errorf("%w: sin pesos", ErrInvalidWeights)
	}
	total := new(big.Int)
	for _, w := range weights {
		if w < 0 {
			return nil, fmt.Errorf("%w: peso negativo %d", ErrInvalidWeights, w)
		}
		total.Add(total, big.NewInt(w))
	}
	if total.Sign() == 0 {
		return nil, fmt.Errorf("%w: los pesos suman cero", ErrInvalidWeights)
	}
	return total, nil
}
