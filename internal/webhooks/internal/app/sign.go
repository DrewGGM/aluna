package app

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// secretPrefix marks a Standard Webhooks secret: whsec_ and the key in base64.
const secretPrefix = "whsec_"

// NewSecret is a new endpoint's secret: 32 random bytes, as Standard Webhooks writes them.
func NewSecret() (string, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return "", fmt.Errorf("webhook secret: %w", err)
	}
	return secretPrefix + base64.StdEncoding.EncodeToString(key), nil
}

// Signed are the headers of a webhook (Standard Webhooks): its id, the unix second it was sent and its signature, v1
// and the base64 HMAC-SHA256, with the secret's key, of id.timestamp.body.
type Signed struct {
	ID, Timestamp, Signature string
}

// Sign signs body as the webhook id sent at at, with secret.
func Sign(secret, id string, at time.Time, body []byte) (Signed, error) {
	key, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(secret, secretPrefix))
	if err != nil || !strings.HasPrefix(secret, secretPrefix) {
		return Signed{}, fmt.Errorf("webhook secret: not %s and base64", secretPrefix)
	}
	timestamp := strconv.FormatInt(at.Unix(), 10)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(id + "." + timestamp + "."))
	mac.Write(body)
	return Signed{ID: id, Timestamp: timestamp, Signature: "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil))}, nil
}
