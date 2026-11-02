// Package simrail is a simulated Bre-B rail (AR-2) until the real one can be reached (D-1). It takes every
// valid message and decides by the whole part of its amount: ending in 13 it is rejected, ending in 99 the
// rail takes over 20 seconds to take it, and anything else is accepted. The outcome comes as a notification
// signed as Standard Webhooks says (HMAC-SHA256 of id.timestamp.body) to /v1/rails/simbreb/notifications,
// where the core reads it back with Outcome. The transfers other banks send to the entity's customers come
// signed the same way to /v1/rails/simbreb/transfers, and the core reads them with Transfer. Notifications
// still waiting when the process stops are lost, as a real rail's would be until it retried.
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

	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/money"
	"github.com/DrewGGM/aluna/internal/payments/internal/app"
)

const (
	name             = app.RailSimBreB
	notificationPath = "/v1/rails/" + name + "/notifications"
	slowTake         = 21 * time.Second // over 20 s: longer than the core waits (app.Send)
	notifyTimeout    = 10 * time.Second
	closedAccount    = "AC04"          // ISO 20022 reason of a rejection
	replayWindow     = 5 * time.Minute // how far from now a notification may be signed (ADR-0010)
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

// Outcome is the outcome n carries if the rail signed it within 5 minutes of now, before or after: any of its
// v1 signatures is the HMAC-SHA256 with the rail's secret of its id, its timestamp and its body. Otherwise it
// is app.ErrUnverifiedNotification, which says nothing of what the body carries; a signed body that is not a
// notification is app.ErrInvalidNotification.
func (s *Simulator) Outcome(n app.Notification) (app.RailOutcome, error) {
	if err := s.verify(n); err != nil {
		return app.RailOutcome{}, err
	}
	var body notification
	if err := json.Unmarshal(n.Body, &body); err != nil {
		return app.RailOutcome{}, fmt.Errorf("%w: %w", app.ErrInvalidNotification, err)
	}
	return app.RailOutcome(body), nil
}

// transfer is an app.InboundTransfer as the rail sends it: its creditor agent is the entity's id, its amount in
// minor units of its currency, and its settlement date a calendar day.
type transfer struct {
	CreditorAgent  string `json:"creditor_agent"`
	EndToEndID     string `json:"end_to_end_id"`
	InstrID        string `json:"instr_id"`
	TxID           string `json:"tx_id"`
	RailRef        string `json:"rail_ref"`
	DebtorAgent    string `json:"debtor_agent"`
	Amount         int64  `json:"amount"`
	Currency       string `json:"currency"`
	SettlementDate string `json:"settlement_date"`
	Creditor       struct {
		AccountNumber string `json:"account_number"`
		Key           string `json:"key"`
	} `json:"creditor"`
}

// Transfer is the transfer received that n carries if the rail signed it, as Outcome checks it. A signed body
// that is not a transfer, or whose entity, currency or settlement date the core cannot read, is
// app.ErrInvalidNotification.
func (s *Simulator) Transfer(n app.Notification) (app.InboundTransfer, error) {
	if err := s.verify(n); err != nil {
		return app.InboundTransfer{}, err
	}
	var t transfer
	if err := json.Unmarshal(n.Body, &t); err != nil {
		return app.InboundTransfer{}, fmt.Errorf("%w: %w", app.ErrInvalidNotification, err)
	}
	entity, err := ids.Parse(t.CreditorAgent)
	if err != nil {
		return app.InboundTransfer{}, fmt.Errorf("%w: creditor agent: %w", app.ErrInvalidNotification, err)
	}
	c, err := money.CurrencyOf(t.Currency)
	if err != nil {
		return app.InboundTransfer{}, fmt.Errorf("%w: %w", app.ErrInvalidNotification, err)
	}
	settled, err := time.Parse(time.DateOnly, t.SettlementDate)
	if err != nil {
		return app.InboundTransfer{}, fmt.Errorf("%w: settlement date: %w", app.ErrInvalidNotification, err)
	}
	return app.InboundTransfer{
		LegalEntityID: entity, EndToEndID: t.EndToEndID, InstrID: t.InstrID, TxID: t.TxID, RailRef: t.RailRef, DebtorAgent: t.DebtorAgent,
		Amount: money.New(t.Amount, c), SettlementDate: settled, Creditor: app.AccountRef{Number: t.Creditor.AccountNumber, Key: t.Creditor.Key},
	}, nil
}

// verify is nil if the rail signed n within the replay window, comparing signatures in constant time.
func (s *Simulator) verify(n app.Notification) error {
	id, timestamp := n.Header.Get("webhook-id"), n.Header.Get("webhook-timestamp")
	unix, err := strconv.ParseInt(timestamp, 10, 64)
	switch {
	case id == "" || err != nil:
		return fmt.Errorf("%w: without its id or its timestamp", app.ErrUnverifiedNotification)
	case s.now().Sub(time.Unix(unix, 0)).Abs() > replayWindow:
		return fmt.Errorf("%w: signed at %d, outside the replay window", app.ErrUnverifiedNotification, unix)
	}
	want := []byte(sign(s.secret, id, timestamp, n.Body))
	for _, signature := range strings.Fields(n.Header.Get("webhook-signature")) {
		if version, value, _ := strings.Cut(signature, ","); version == "v1" && hmac.Equal([]byte(value), want) {
			return nil
		}
	}
	return fmt.Errorf("%w: no signature of the rail", app.ErrUnverifiedNotification)
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
