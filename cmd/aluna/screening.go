package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/DrewGGM/aluna/internal/compliance"
	"github.com/DrewGGM/aluna/internal/postgres"
)

// errScreening means a screening configuration that is not one of the two, or lacks what it needs.
var errScreening = errors.New("invalid screening configuration")

// screensLists reports whether ALUNA_SCREENING_VENDOR says to screen against the lists (lists) rather than with the
// fake vendor of development (fake, the default).
func screensLists() (bool, error) {
	switch v := envOr("ALUNA_SCREENING_VENDOR", "fake"); v {
	case "fake", "lists":
		return v == "lists", nil
	default:
		return false, fmt.Errorf("%w: ALUNA_SCREENING_VENDOR=%q is neither fake nor lists", errScreening, v)
	}
}

// listScreeningFromEnv is how the api screens against the lists (ADR-0018), nil with the fake vendor: the identity
// service of ALUNA_SCREENING_IDENTITY_URL and ALUNA_SCREENING_IDENTITY_TOKEN, ALUNA_SCREENING_THRESHOLD (0.92) and
// ALUNA_SCREENING_MAX_LIST_AGE_HOURS (48). compliance.NewWithLists checks what they say.
func listScreeningFromEnv() (*compliance.ListScreeningConfig, error) {
	if lists, err := screensLists(); err != nil || !lists {
		return nil, err
	}
	threshold, err := strconv.ParseFloat(envOr("ALUNA_SCREENING_THRESHOLD", "0.92"), 64)
	if err != nil {
		return nil, fmt.Errorf("%w: ALUNA_SCREENING_THRESHOLD is not a number", errScreening)
	}
	hours, err := strconv.ParseUint(envOr("ALUNA_SCREENING_MAX_LIST_AGE_HOURS", "48"), 10, 16)
	if err != nil {
		return nil, fmt.Errorf("%w: ALUNA_SCREENING_MAX_LIST_AGE_HOURS is not a whole number of hours", errScreening)
	}
	return &compliance.ListScreeningConfig{
		IdentityURL: os.Getenv("ALUNA_SCREENING_IDENTITY_URL"), IdentityToken: os.Getenv("ALUNA_SCREENING_IDENTITY_TOKEN"),
		Threshold: threshold, MaxAge: time.Duration(hours) * time.Hour, Client: &http.Client{},
	}, nil
}

// listDownloadTimeout bounds the download of a list: the PEPs are some 15 MB a page.
const listDownloadTimeout = 2 * time.Minute

// listSourcesFromEnv are where the worker refreshes the lists from, nil with the fake vendor: their owners, or the
// mirrors of ALUNA_SCREENING_OFAC_SDN_URL, ALUNA_SCREENING_OFAC_ALT_URL, ALUNA_SCREENING_UN_URL and
// ALUNA_SCREENING_PEP_URL.
func listSourcesFromEnv() (*compliance.ListSources, error) {
	if lists, err := screensLists(); err != nil || !lists {
		return nil, err
	}
	s := compliance.OfficialListSources(&http.Client{Timeout: listDownloadTimeout})
	s.OFACSDN, s.OFACAlt = envOr("ALUNA_SCREENING_OFAC_SDN_URL", s.OFACSDN), envOr("ALUNA_SCREENING_OFAC_ALT_URL", s.OFACAlt)
	s.UN, s.PEP = envOr("ALUNA_SCREENING_UN_URL", s.UN), envOr("ALUNA_SCREENING_PEP_URL", s.PEP)
	return &s, nil
}

// complianceFor is the compliance service that screens as l says, against the lists in q while ctx lasts, or with
// the fake vendor if l is nil, which it warns of; it publishes to events and logs to log.
func complianceFor(ctx context.Context, l *compliance.ListScreeningConfig, events compliance.Events, q postgres.Querier, log *slog.Logger,
) (*compliance.Service, error) {
	if l == nil {
		log.WarnContext(ctx, "screening with the fake vendor, which clears nearly every subject: production sets ALUNA_SCREENING_VENDOR=lists")
		return compliance.New(events), nil
	}
	lists := *l
	lists.Log = log
	return compliance.NewWithLists(ctx, events, q, lists)
}
