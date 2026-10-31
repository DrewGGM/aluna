//go:build integration

package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/DrewGGM/aluna/internal/httpapi/gen"
	"github.com/DrewGGM/aluna/internal/ids"
)

// depositWithHold creates a deposit of 1,000 COP (minor units), its clearing account and a hold of 500 on
// the deposit, all over HTTP, and returns the server, the deposit and the hold.
func depositWithHold(t *testing.T) (*httptest.Server, string, gen.Hold) {
	t.Helper()
	ts := apiServer(t, ids.New())
	cash, deposit, clearing := createAccount(t, ts, "asset"), createAccount(t, ts, "liability"), createAccount(t, ts, "liability")
	if r := postWithKey(t, ts, ids.New().String(), postingBody(t, cash, deposit, 1000, 1000, "")); r.status != http.StatusCreated {
		t.Fatalf("funding = %d %s", r.status, r.body)
	}
	return ts, deposit, placeHoldOver(t, ts, deposit, clearing, 500)
}

func captureBody(units int64) string {
	return fmt.Sprintf(`{"amount":{"amount":%d,"currency":"COP"},"business_date":%q}`, units, time.Now().UTC().Format(time.DateOnly))
}

func TestCaptureAHoldOverHTTP(t *testing.T) {
	ts, deposit, hold := depositWithHold(t)
	path, key := "/v1/holds/"+hold.Id.String()+"/capture", ids.New().String()

	r := postTo(t, ts, path, key, captureBody(200))

	var got gen.HoldCapture
	if r.status != http.StatusOK || json.Unmarshal(r.body, &got) != nil || got.Hold.Status != gen.HoldStatusCaptured ||
		got.Hold.Captured.Amount != 200 || len(got.Transaction.Entries) != 2 {
		t.Fatalf("capture = %d %s", r.status, r.body)
	}
	if replay := postTo(t, ts, path, key, captureBody(200)); replay.status != http.StatusOK || !bytes.Equal(replay.body, r.body) {
		t.Fatalf("capture replay = %d %s, want the same response", replay.status, replay.body)
	}
	if got := balanceOf(t, ts, deposit); got != -800 {
		t.Fatalf("deposit balance = %d, want -800 (200 of 1,000 captured)", got)
	}
}

// TestConcurrentCapturesOfAHold (AB-3): two simultaneous captures of the same hold, with different keys: one
// passes and the other gets 409 hold_not_pending; it is posted once.
func TestConcurrentCapturesOfAHold(t *testing.T) {
	ts, deposit, hold := depositWithHold(t)
	path := "/v1/holds/" + hold.Id.String() + "/capture"
	responses, errs := make([]response, 2), make([]error, 2)
	var wg sync.WaitGroup
	for i := range responses {
		key := ids.New().String()
		wg.Go(func() { responses[i], errs[i] = send(t.Context(), ts, path, &key, captureBody(500)) })
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		t.Fatal(err)
	}

	ok, conflict := 0, 0
	for _, r := range responses {
		switch {
		case r.status == http.StatusOK:
			ok++
		case r.status == http.StatusConflict && problemCode(t, r) == "hold_not_pending":
			conflict++
		default:
			t.Errorf("unexpected response: %d %s", r.status, r.body)
		}
	}
	if ok != 1 || conflict != 1 {
		t.Fatalf("%d captures passed and %d got 409; want one and one", ok, conflict)
	}
	if got := balanceOf(t, ts, deposit); got != -500 {
		t.Fatalf("deposit balance = %d, want -500: the hold was posted once", got)
	}
}

func TestHoldErrorsOverHTTP(t *testing.T) {
	ts, _, hold := depositWithHold(t)
	capture, release := "/v1/holds/"+hold.Id.String()+"/capture", "/v1/holds/"+hold.Id.String()+"/release"
	cases := []struct {
		name, path, body string
		status           int
		code             string
	}{
		{"too much", capture, captureBody(501), http.StatusUnprocessableEntity, "amount_exceeds_hold"},
		{"in another currency", capture, `{"amount":{"amount":100,"currency":"USD"},"business_date":"2026-10-27"}`, http.StatusUnprocessableEntity, "currency_mismatch"},
		{"nonexistent hold", "/v1/holds/" + ids.New().String() + "/release", "", http.StatusNotFound, "not_found"},
		{"release", release, "", http.StatusOK, ""},
		{"capture a released one", capture, captureBody(100), http.StatusConflict, "hold_not_pending"},
	}
	for _, tc := range cases {
		r := postTo(t, ts, tc.path, ids.New().String(), tc.body)
		if r.status != tc.status || (tc.code != "" && problemCode(t, r) != tc.code) {
			t.Errorf("%s = %d %s, want %d %s", tc.name, r.status, r.body, tc.status, tc.code)
		}
	}
	if r, err := send(t.Context(), ts, release, nil, ""); err != nil || r.status != http.StatusBadRequest {
		t.Errorf("release without Idempotency-Key = %d (%v), want 400", r.status, err)
	}
}

// TestTheSameKeyOnTwoHoldsCapturesBoth: the key is per hold; the same key and the same body on another hold
// are not taken for a retry.
func TestTheSameKeyOnTwoHoldsCapturesBoth(t *testing.T) {
	ts, _, first := depositWithHold(t)
	other := placeHoldOver(t, ts, first.AccountId.String(), first.CounterAccountId.String(), 100)
	key := ids.New().String()

	for _, hold := range []gen.Hold{first, other} {
		r := postTo(t, ts, "/v1/holds/"+hold.Id.String()+"/capture", key, captureBody(100))
		var got gen.HoldCapture
		if r.status != http.StatusOK || json.Unmarshal(r.body, &got) != nil || got.Hold.Id != hold.Id {
			t.Fatalf("capture of %s = %d %s", hold.Id, r.status, r.body)
		}
	}
}

// placeHoldOver creates over HTTP a hold of units on account, expiring in an hour.
func placeHoldOver(t *testing.T, ts *httptest.Server, account, counter string, units int64) gen.Hold {
	t.Helper()
	body := fmt.Sprintf(`{"account_id":%q,"counter_account_id":%q,"amount":{"amount":%d,"currency":"COP"},"expires_at":%q}`,
		account, counter, units, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
	r := postTo(t, ts, "/v1/holds", ids.New().String(), body)
	var hold gen.Hold
	if r.status != http.StatusCreated || json.Unmarshal(r.body, &hold) != nil || hold.Status != gen.HoldStatusPending {
		t.Fatalf("create hold = %d %s", r.status, r.body)
	}
	return hold
}

// TestACaptureNeedsItsBusinessDate: as in the posting, the business date 0001-01-01 is invalid and is not
// replaced by the entity's.
func TestACaptureNeedsItsBusinessDate(t *testing.T) {
	ts, _, hold := depositWithHold(t)

	r := postTo(t, ts, "/v1/holds/"+hold.Id.String()+"/capture", ids.New().String(), `{"amount":{"amount":100,"currency":"COP"},"business_date":"0001-01-01"}`)

	if r.status != http.StatusUnprocessableEntity || problemCode(t, r) != "invalid_posting" {
		t.Fatalf("capture with a zero business date = %d %s, want 422 invalid_posting", r.status, r.body)
	}
}
