package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/DrewGGM/aluna/internal/calendar"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/postgres"
)

// entityFlags are the flags of entity create, as typed.
type entityFlags struct {
	id, name, businessDate, currency string
}

// entityCommand runs entity create [--id ID] --name N --business-date yyyy-mm-dd --currency C.
func entityCommand(args []string, stdout io.Writer) int {
	if len(args) == 0 || args[0] != "create" {
		return printUsage()
	}
	var f entityFlags
	flags := flag.NewFlagSet("entity create", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&f.id, "id", "", "")
	flags.StringVar(&f.name, "name", "", "")
	flags.StringVar(&f.businessDate, "business-date", "", "")
	flags.StringVar(&f.currency, "currency", "", "")
	if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 || f.name == "" || f.businessDate == "" || f.currency == "" {
		return printUsage()
	}
	return untilSignal(func(ctx context.Context) int {
		return exitCode(runCreateEntity(ctx, os.Getenv("ALUNA_DATABASE_URL"), f, stdout))
	})
}

// runCreateEntity registers, as the tables' owner of the dsn database, a legal entity on its first business date, which
// has started in America/Bogota, with its functional currency, and prints its ID, the legal_entity_id its users' tokens carry: f.id, or a new one. An ID
// given lets the entity match the tokens an IAM already issues, such as the development realm's.
func runCreateEntity(ctx context.Context, dsn string, f entityFlags, out io.Writer) error {
	day, err := calendar.ParseDate(f.businessDate)
	if err != nil {
		return fmt.Errorf("--business-date: %w", err)
	}
	entity := ids.New()
	if f.id != "" {
		if entity, err = ids.Parse(f.id); err != nil {
			return fmt.Errorf("--id: %w", err)
		}
	}
	if dsn == "" {
		return errNoDatabaseURL
	}
	openCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	pool, err := postgres.Open(openCtx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := calendar.CreateLegalEntity(ctx, pool, calendar.LegalEntity{ID: entity, Name: f.name, BusinessDate: day, Currency: f.currency}, time.Now()); err != nil {
		return err
	}
	_, err = fmt.Fprintln(out, entity)
	return err
}
