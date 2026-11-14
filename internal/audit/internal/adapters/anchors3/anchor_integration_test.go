//go:build integration

package anchors3

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/DrewGGM/aluna/internal/audit/internal/app"
	"github.com/DrewGGM/aluna/internal/ids"
)

// gatewayImage is the Versity S3 Gateway the tests run against, pinned by digest: it checks signatures and keeps
// Object Lock.
const gatewayImage = "versity/versitygw:v1.10.0@sha256:89cad13cc981986ee284cbafea4613f9533ce6fd5f37a2c96c4c4fb8adb3a316"

// gateway is an S3 service with Object Lock that checks signatures: the one at ALUNA_TEST_S3_URL, with the keys of
// ALUNA_TEST_S3_ACCESS_KEY and ALUNA_TEST_S3_SECRET_KEY, or else a Versity S3 Gateway started for the test.
func gateway(t *testing.T) (string, Credentials) {
	t.Helper()
	if endpoint := os.Getenv("ALUNA_TEST_S3_URL"); endpoint != "" {
		return endpoint, Credentials{AccessKeyID: os.Getenv("ALUNA_TEST_S3_ACCESS_KEY"), SecretAccessKey: os.Getenv("ALUNA_TEST_S3_SECRET_KEY")}
	}
	keys := Credentials{AccessKeyID: "anchoruser", SecretAccessKey: "anchorsecret123"} // gitleaks:allow (a test gateway's)
	c, err := testcontainers.GenericContainer(t.Context(), testcontainers.GenericContainerRequest{
		Started: true,
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        gatewayImage,
			Entrypoint:   []string{"sh", "-c", "mkdir -p /data/gw /data/versions && exec versitygw posix --versioning-dir /data/versions /data/gw"},
			Env:          map[string]string{"ROOT_ACCESS_KEY": keys.AccessKeyID, "ROOT_SECRET_KEY": keys.SecretAccessKey},
			ExposedPorts: []string{"7070/tcp"},
			WaitingFor:   wait.ForListeningPort("7070/tcp"),
		},
	})
	testcontainers.CleanupContainer(t, c)
	if err != nil {
		t.Fatal(err)
	}
	endpoint, err := c.PortEndpoint(t.Context(), "7070/tcp", "http")
	if err != nil {
		t.Fatal(err)
	}
	return endpoint, keys
}

// lockedBucket creates on the gateway a bucket with Object Lock and is the anchor in it, under the prefix audit,
// retained for an hour.
func lockedBucket(t *testing.T) *Bucket {
	t.Helper()
	endpoint, keys := gateway(t)
	name := "anchors-" + strings.ReplaceAll(ids.New().String(), "-", "")
	status := signed(t, endpoint+"/"+name, keys, map[string]string{"X-Amz-Bucket-Object-Lock-Enabled": "true"}, nil)
	if status != http.StatusOK {
		t.Fatalf("create bucket %s = %d", name, status)
	}
	b, err := New(endpoint+"/"+name, "audit", "us-east-1", keys, time.Hour, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// signed sends a signed PUT of body, with headers, to target and is its status.
func signed(t *testing.T, target string, keys Credentials, headers map[string]string, body []byte) int {
	t.Helper()
	r, err := http.NewRequestWithContext(t.Context(), http.MethodPut, target, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range headers {
		r.Header.Set(name, value)
	}
	signer{region: "us-east-1", creds: keys}.sign(r, body, time.Now())
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode
}

// TestAnAnchorIsLockedInTheBucket (ADR-0017): on an S3 service that checks signatures and keeps Object Lock, the
// anchor reads back the same and is retained in compliance mode; anchoring it again with the same hash does
// nothing and with a different one is ErrAnchored, without changing it; a block with no object is ErrNotAnchored.
func TestAnAnchorIsLockedInTheBucket(t *testing.T) {
	b := lockedBucket(t)
	first, second := sha256.Sum256([]byte("one")), sha256.Sum256([]byte("two"))

	if err := b.Put(t.Context(), 7, first); err != nil {
		t.Fatal(err)
	}
	if err := b.Put(t.Context(), 7, first); err != nil {
		t.Fatalf("repeating the same anchor = %v", err)
	}
	if err := b.Put(t.Context(), 7, second); !errors.Is(err, app.ErrAnchored) {
		t.Fatalf("rewriting the anchor = %v, want ErrAnchored", err)
	}

	if got, err := b.Get(t.Context(), 7); err != nil || got != first {
		t.Fatalf("anchor = %x (%v), want %x", got, err, first)
	}
	if mode := lockMode(t, b, 7); mode != "COMPLIANCE" {
		t.Fatalf("the anchor's object lock = %q, want COMPLIANCE", mode)
	}
	if _, err := b.Get(t.Context(), 8); !errors.Is(err, app.ErrNotAnchored) {
		t.Fatalf("a block with no anchor = %v, want ErrNotAnchored", err)
	}
}

// TestAnAnchorPutOverDoesNotRead (ADR-0017): whoever holds the keys can put another version over the locked one
// without If-None-Match; the anchor then has two versions and does not read, instead of reading as the forged one.
func TestAnAnchorPutOverDoesNotRead(t *testing.T) {
	b := lockedBucket(t)
	if err := b.Put(t.Context(), 7, sha256.Sum256([]byte("one"))); err != nil {
		t.Fatal(err)
	}
	forged := sha256.Sum256([]byte("forged"))

	if status := signed(t, b.object(7).String(), b.signer.creds, nil, []byte(hex.EncodeToString(forged[:]))); status != http.StatusOK {
		t.Fatalf("putting another version = %d, want it taken (Object Lock keeps versions, not keys)", status)
	}

	if got, err := b.Get(t.Context(), 7); !errors.Is(err, errReplaced) {
		t.Fatalf("an anchor put over = %x (%v), want errReplaced, not %x", got, err, forged)
	}
}

// lockMode is the object lock mode the bucket reports for blockID's object.
func lockMode(t *testing.T, b *Bucket, blockID int64) string {
	t.Helper()
	r, err := http.NewRequestWithContext(t.Context(), http.MethodHead, b.object(blockID).String(), http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	b.signer.sign(r, nil, time.Now())
	resp, err := b.client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	return resp.Header.Get("X-Amz-Object-Lock-Mode")
}
