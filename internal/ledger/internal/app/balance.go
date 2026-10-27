package app

import "time"

// BalanceAsOf es el corte del saldo bitemporal (FR-8, ADR-0006): los entries con fecha valor hasta
// ValueDate (su día calendario, en su zona) y asentados (posted_at) hasta KnownAt. Un corte nil no
// filtra ese eje.
type BalanceAsOf struct {
	ValueDate, KnownAt *time.Time
}
