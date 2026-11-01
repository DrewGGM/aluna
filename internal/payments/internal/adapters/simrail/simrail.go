// Package simrail is a simulated Bre-B rail (AR-2) until the real one can be reached (D-1). It takes every
// valid message and decides by the whole part of its amount: ending in 13 it is rejected, ending in 99 the
// rail takes over 20 seconds to take it, and anything else is accepted. The outcome comes as a notification
// signed as Standard Webhooks says (HMAC-SHA256 of id.timestamp.body) to /v1/rails/simbreb/notifications.
// Notifications still waiting when the process stops are lost, as a real rail's would be until it retried.
package simrail

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/DrewGGM/aluna/internal/payments/internal/app"
)

const (
	name             = "simbreb"
	notificationPath = "/v1/rails/" + name + "/notifications"
	slowTake         = 21 * time.Second // over 20 s: longer than the core waits (app.Send)
	notifyTimeout    = 10 * time.Second
	closedAccount    = "AC04" // ISO 20022 reason of a rejection
)

// errNotTaken means the core answered a notification with an error.
var errNotTaken = errors.New("the core did not take the notification")

// notification is a RailOutcome as the rail sends it.
type notification struct {
	EndToEndID string         `json:"end_to_end_id"`
	TxID       string         `json:"tx_id"`
	RailRef    string         `json:"rail_ref"`
	Status     app.RailStatus `json:"status"`
	Reason     string         `json:"reason,omitempty"`
}

// Simulator is the simulated rail.
type Simulator struct {
	baseURL string
	secret  []byte
	client  *http.Client
	log     *slog.Logger
	now     func() time.Time // the time of a notification's signature
	delay   time.Duration    // how long it takes a slow message
}

// New is a simulated rail that notifies the core at baseURL with client, signing with secret; log records the
// notifications the core did not take.
func New(baseURL string, secret []byte, client *http.Client, log *slog.Logger) *Simulator {
	return &Simulator{
		baseURL: strings.TrimSuffix(baseURL, "/"), secret: secret, client: client, log: log, now: time.Now, delay: slowTake,
	}
}

// Name is the rail's name, the one in its notification path.
func (s *Simulator) Name() string { return name }

// Send takes msg and notifies its outcome; a sender that has already given up sends nothing. A slow message is
// taken after the rail's delay, and its outcome notified then; if ctx ends first, Send returns ctx's error,
// but the rail still notifies the message: its sender does not know the outcome until then. A notification
// the core does not take is logged and not sent again: reconciliation (FR-34) finds it.
func (s *Simulator) Send(ctx context.Context, msg app.OutboundMessage) (app.SendAck, error) {
	if err := ctx.Err(); err != nil {
		return app.SendAck{}, err
	}
	if err := check(msg); err != nil {
		return app.SendAck{}, err
	}
	outcome := app.RailOutcome{EndToEndID: msg.EndToEndID, TxID: msg.TxID, RailRef: "SIM" + msg.EndToEndID, Status: app.RailAccepted}
	var wait time.Duration
	switch whole := wholePart(msg); {
	case strings.HasSuffix(whole, "13"):
		outcome.Status, outcome.Reason = app.RailRejected, closedAccount
	case strings.HasSuffix(whole, "99"):
		wait = s.delay
	}
	ack := app.SendAck{RailRef: outcome.RailRef}
	go s.notifyAfter(context.WithoutCancel(ctx), wait, outcome) // the rail notifies even if the sender stops waiting
	if wait == 0 {
		return ack, nil
	}
	select {
	case <-time.After(wait):
		return ack, nil
	case <-ctx.Done():
		return app.SendAck{}, ctx.Err()
	}
}

// check reports whether the rail takes msg: an end-to-end id, a positive amount, and a debtor and a creditor
// each by account or by key. Otherwise it is app.ErrRailRefused.
func check(msg app.OutboundMessage) error {
	switch {
	case msg.EndToEndID == "":
		return fmt.Errorf("%w: missing EndToEndId", app.ErrRailRefused)
	case msg.Amount.Sign() <= 0:
		return fmt.Errorf("%w: the amount must be positive", app.ErrRailRefused)
	case !byAccountOrKey(msg.Debtor) || !byAccountOrKey(msg.Creditor):
		return fmt.Errorf("%w: the debtor and the creditor go by account or by key", app.ErrRailRefused)
	}
	return nil
}

func byAccountOrKey(a app.AccountRef) bool { return (a.Number == "") != (a.Key == "") }

// wholePart is the whole part of msg's amount in its currency's units: the pesos of 50013.00 COP are 50013.
// It reads the amount's decimal text, whose whole part comes before the point, if the currency has decimals.
func wholePart(msg app.OutboundMessage) string {
	whole, _, _ := strings.Cut(msg.Amount.Precise().String(), ".")
	return whole
}

// notifyAfter sends outcome to the core after wait, and logs it if the core does not take it.
func (s *Simulator) notifyAfter(ctx context.Context, wait time.Duration, outcome app.RailOutcome) {
	time.Sleep(wait)
	ctx, cancel := context.WithTimeout(ctx, notifyTimeout)
	defer cancel()
	if err := s.notify(ctx, outcome); err != nil {
		s.log.LogAttrs(ctx, slog.LevelWarn, "deliver rail notification",
			slog.String("rail", name), slog.String("rail_ref", outcome.RailRef), slog.String("error", err.Error()))
	}
}

// notify posts outcome, signed, to the core.
func (s *Simulator) notify(ctx context.Context, outcome app.RailOutcome) error {
	body, err := json.Marshal(notification(outcome))
	if err != nil {
		return fmt.Errorf("rail notification: %w", err)
	}
	id, timestamp := outcome.RailRef, strconv.FormatInt(s.now().Unix(), 10)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.baseURL+notificationPath, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("rail notification: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("webhook-id", id)
	req.Header.Set("webhook-timestamp", timestamp)
	req.Header.Set("webhook-signature", "v1,"+sign(s.secret, id, timestamp, body))
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("rail notification: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // closing it does not change the result
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%w: it answered %d", errNotTaken, resp.StatusCode)
	}
	return nil
}

// sign is the Standard Webhooks signature of a notification: HMAC-SHA256 with secret of id.timestamp.body, in
// base 64.
func sign(secret []byte, id, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(id + "." + timestamp + "."))
	mac.Write(body)
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}
