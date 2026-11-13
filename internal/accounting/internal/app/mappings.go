// Package app holds accounting's use cases: the mapping of the internal accounts to CUIF codes (ADR-0011).
package app

import (
	"context"

	"github.com/DrewGGM/aluna/internal/accounting/internal/domain"
	"github.com/DrewGGM/aluna/internal/calendar"
	"github.com/DrewGGM/aluna/internal/ids"
)

// Mappings is where an entity's mappings are kept.
type Mappings interface {
	// InsertMapping stores m: domain.ErrMappingOverlap if another of its pattern holds in its period.
	InsertMapping(ctx context.Context, entity ids.ID, m domain.Mapping) error
	// Mappings are all the entity's mappings.
	Mappings(ctx context.Context, entity ids.ID) ([]domain.Mapping, error)
	// BusinessDate is the entity's business date.
	BusinessDate(ctx context.Context, entity ids.ID) (calendar.Date, error)
}

// CheckMappings reports whether the entity can load mappings: domain.ErrInvalidMapping if one starts before its
// business date, domain.ErrMappingOverlap if one overlaps a stored one.
func CheckMappings(ctx context.Context, s Mappings, entity ids.ID, mappings []domain.Mapping) error {
	if err := checkStart(ctx, s, entity, mappings); err != nil {
		return err
	}
	stored, err := s.Mappings(ctx, entity)
	if err != nil {
		return err
	}
	return domain.CheckAgainst(stored, mappings)
}

// LoadMappings stores mappings: domain.ErrInvalidMapping if one starts before the business date, which may have
// moved since they were checked, and domain.ErrMappingOverlap if one overlaps a stored one, and the caller's
// transaction undoes the rest.
func LoadMappings(ctx context.Context, s Mappings, entity ids.ID, mappings []domain.Mapping) error {
	if err := checkStart(ctx, s, entity, mappings); err != nil {
		return err
	}
	for _, m := range mappings {
		if err := s.InsertMapping(ctx, entity, m); err != nil {
			return err
		}
	}
	return nil
}

// MapToCUIF is the CUIF code of the entity's internal account with code account on day: domain.ErrNoMapping if none
// is in force.
func MapToCUIF(ctx context.Context, s Mappings, entity ids.ID, account string, day calendar.Date) (string, error) {
	mappings, err := s.Mappings(ctx, entity)
	if err != nil {
		return "", err
	}
	return domain.Resolve(mappings, account, day)
}

func checkStart(ctx context.Context, s Mappings, entity ids.ID, mappings []domain.Mapping) error {
	today, err := s.BusinessDate(ctx, entity)
	if err != nil {
		return err
	}
	return domain.CheckStart(mappings, today)
}
