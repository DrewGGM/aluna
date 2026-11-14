// Package anchors3 anchors the audit blocks in an S3 bucket with Object Lock (ADR-0012, ADR-0017): one object per
// block with its hash in hex, created only if it does not exist and retained in compliance mode, so that until its
// retention ends nobody, the account's root user included, rewrites or deletes that version. Object Lock protects
// versions, not keys: a new version or a delete marker can still be put over it, so an anchor is read only if its
// key has that one version and nothing else.
package anchors3

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // S3 requires Content-MD5 on an Object Lock write, as transport integrity, not security
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/DrewGGM/aluna/internal/audit/internal/app"
)

// Credentials are the keys that sign the requests to the bucket.
type Credentials struct{ AccessKeyID, SecretAccessKey, SessionToken string }

var (
	// errBucket means an anchor bucket that cannot be used: its URL, prefix, region, credentials or retention.
	errBucket = errors.New("invalid audit anchor bucket")
	// errResponse means a response of the bucket that is neither the object nor its absence.
	errResponse = errors.New("unexpected response of the audit anchor bucket")
	// errReplaced means an anchor whose key has more than its one version: someone put another version or a delete
	// marker over the locked one, and what reads as the anchor is not what was anchored.
	errReplaced = errors.New("the audit anchor was replaced or deleted")
)

var (
	// unreservedPath is a bucket URL path made only of characters that need no escaping in a signed request.
	unreservedPath = regexp.MustCompile(`^(/[A-Za-z0-9._~-]+)*$`)
	// unreservedPrefix is a key prefix of such characters, its parts separated by single slashes.
	unreservedPrefix = regexp.MustCompile(`^([A-Za-z0-9._~-]+(/[A-Za-z0-9._~-]+)*)?$`)
)

// maxResponse bounds what is read of a response: an anchor is 64 characters, a listing of its versions or an error
// a short document. A longer listing is one with too many versions: it does not parse, and the read fails.
const maxResponse = 16 << 10

// Bucket is the anchor in an S3 bucket.
type Bucket struct {
	bucket *url.URL
	prefix string
	signer signer
	retain time.Duration
	client *http.Client
}

// New opens the anchor in the bucket at bucketURL, virtual-hosted (https://bucket.s3.us-east-1.amazonaws.com) or
// path style (https://host/bucket), under key prefix (none if empty), in region, signed with creds through
// client: each anchor is retained for retain from when it is written.
func New(bucketURL, prefix, region string, creds Credentials, retain time.Duration, client *http.Client) (*Bucket, error) {
	bucket, err := url.Parse(strings.TrimSuffix(bucketURL, "/"))
	switch {
	case err != nil:
		return nil, fmt.Errorf("%w: its URL does not parse", errBucket)
	case bucket.Scheme != "https" && bucket.Scheme != "http", bucket.Host == "", bucket.User != nil, bucket.RawQuery != "",
		bucket.Fragment != "", !unreservedPath.MatchString(bucket.Path):
		return nil, fmt.Errorf("%w: its URL must be http(s), without credentials or query, its path of letters, digits and ._~-", errBucket)
	case !unreservedPrefix.MatchString(prefix):
		return nil, fmt.Errorf("%w: its prefix must be letters, digits and ._~- separated by single slashes", errBucket)
	case region == "" || creds.AccessKeyID == "" || creds.SecretAccessKey == "":
		return nil, fmt.Errorf("%w: it needs its region and access keys", errBucket)
	case retain <= 0:
		return nil, fmt.Errorf("%w: a retention of %s", errBucket, retain)
	}
	return &Bucket{bucket: bucket, prefix: prefix, signer: signer{region: region, creds: creds}, retain: retain, client: client}, nil
}

// key is blockID's object key.
func (b *Bucket) key(blockID int64) string {
	name := fmt.Sprintf("%020d.sha256", blockID)
	if b.prefix == "" {
		return name
	}
	return b.prefix + "/" + name
}

// object is the URL of blockID's object.
func (b *Bucket) object(blockID int64) *url.URL {
	object := *b.bucket
	object.Path += "/" + b.key(blockID)
	return &object
}

// Put anchors hash for blockID: the object is created only if it does not exist (If-None-Match), so an anchor is
// never rewritten, and it is retained in compliance mode. Repeating it with the same hash does nothing (a retried
// seal); with a different one it is app.ErrAnchored.
func (b *Bucket) Put(ctx context.Context, blockID int64, hash [32]byte) error {
	body := []byte(hex.EncodeToString(hash[:]))
	sum := md5.Sum(body) //nolint:gosec // transport integrity that S3 requires, not security
	status, _, err := b.do(ctx, http.MethodPut, b.object(blockID), body, map[string]string{
		"Content-Md5":                         base64.StdEncoding.EncodeToString(sum[:]),
		"If-None-Match":                       "*",
		"X-Amz-Object-Lock-Mode":              "COMPLIANCE",
		"X-Amz-Object-Lock-Retain-Until-Date": time.Now().Add(b.retain).UTC().Format(time.RFC3339),
	})
	switch {
	case err != nil:
		return fmt.Errorf("anchor block %d: %w", blockID, err)
	case status == http.StatusPreconditionFailed || status == http.StatusConflict:
		// 412: it is there, the same anchor or another one; 409: a concurrent request on the key won, perhaps a
		// deletion. Get tells which.
		return b.checkAnchored(ctx, blockID, hash)
	case status != http.StatusOK:
		return fmt.Errorf("anchor block %d: %w: %d", blockID, errResponse, status)
	}
	return nil
}

// checkAnchored is nil if blockID already has hash anchored and app.ErrAnchored if it has a different one.
func (b *Bucket) checkAnchored(ctx context.Context, blockID int64, hash [32]byte) error {
	anchored, err := b.Get(ctx, blockID)
	if err != nil {
		return err
	}
	if anchored != hash {
		return fmt.Errorf("%w: %d", app.ErrAnchored, blockID)
	}
	return nil
}

// Get is the hash anchored for blockID, read from the one version its key has, by its id: with none it is
// app.ErrNotAnchored, and with more than one, or a delete marker, errReplaced.
func (b *Bucket) Get(ctx context.Context, blockID int64) ([32]byte, error) {
	version, err := b.onlyVersion(ctx, blockID)
	if err != nil {
		return [32]byte{}, err
	}
	object := b.object(blockID)
	object.RawQuery = "versionId=" + escape(version)
	status, response, err := b.do(ctx, http.MethodGet, object, nil, nil)
	switch {
	case err != nil:
		return [32]byte{}, fmt.Errorf("read anchor of block %d: %w", blockID, err)
	case status != http.StatusOK:
		return [32]byte{}, fmt.Errorf("read anchor of block %d: %w: %d", blockID, errResponse, status)
	}
	var hash [32]byte
	text := strings.TrimSpace(string(response))
	if len(text) != hex.EncodedLen(len(hash)) {
		return [32]byte{}, fmt.Errorf("unreadable anchor of block %d: %d characters", blockID, len(text))
	}
	if _, err := hex.Decode(hash[:], []byte(text)); err != nil {
		return [32]byte{}, fmt.Errorf("unreadable anchor of block %d: %w", blockID, err)
	}
	return hash, nil
}

// listed is a version or a delete marker of a key, as ListObjectVersions lists it.
type listed struct {
	Key       string
	VersionID string `xml:"VersionId"`
}

// onlyVersion is the id of the one version blockID's key has (ListObjectVersions of the key as prefix, counting
// only the key itself): with none it is app.ErrNotAnchored; with more than one, a delete marker, or more than a
// listing holds, errReplaced.
func (b *Bucket) onlyVersion(ctx context.Context, blockID int64) (string, error) {
	key := b.key(blockID)
	listing := *b.bucket
	listing.RawQuery = "prefix=" + escape(key) + "&versions=" // in canonical form: sorted, and escaped as AWS does
	status, response, err := b.do(ctx, http.MethodGet, &listing, nil, nil)
	switch {
	case err != nil:
		return "", fmt.Errorf("list the versions of the anchor of block %d: %w", blockID, err)
	case status != http.StatusOK:
		return "", fmt.Errorf("list the versions of the anchor of block %d: %w: %d", blockID, errResponse, status)
	case len(response) >= maxResponse:
		return "", fmt.Errorf("%w: block %d has more versions than a listing holds", errReplaced, blockID)
	}
	var result struct {
		Truncated bool     `xml:"IsTruncated"`
		Versions  []listed `xml:"Version"`
		Markers   []listed `xml:"DeleteMarker"`
	}
	if err := xml.Unmarshal(response, &result); err != nil {
		return "", fmt.Errorf("list the versions of the anchor of block %d: %w: %w", blockID, errResponse, err)
	}
	versions, markers := ofKey(result.Versions, key), ofKey(result.Markers, key)
	switch {
	case result.Truncated || len(versions) > 1 || len(markers) > 0:
		return "", fmt.Errorf("%w: block %d has %d versions and %d deletions", errReplaced, blockID, len(versions), len(markers))
	case len(versions) == 0:
		return "", fmt.Errorf("%w: %d", app.ErrNotAnchored, blockID)
	}
	return versions[0].VersionID, nil
}

// ofKey are the entries of all that are of key.
func ofKey(all []listed, key string) []listed {
	var of []listed
	for _, e := range all {
		if e.Key == key {
			of = append(of, e)
		}
	}
	return of
}

// do sends method on target, with body and headers, signed, and returns the response's status and up to
// maxResponse bytes of it.
func (b *Bucket) do(ctx context.Context, method string, target *url.URL, body []byte, headers map[string]string) (int, []byte, error) {
	r, err := http.NewRequestWithContext(ctx, method, target.String(), bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	for name, value := range headers {
		r.Header.Set(name, value)
	}
	b.signer.sign(r, body, time.Now())
	resp, err := b.client.Do(r)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close() //nolint:errcheck // the response is read whole; closing it cannot lose anything
	response, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	return resp.StatusCode, response, err
}

// escape is s escaped as AWS signs a query value: every byte but letters, digits and -._~ as %XX.
func escape(s string) string {
	var e strings.Builder
	for _, c := range []byte(s) {
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || strings.IndexByte("-._~", c) >= 0 {
			e.WriteByte(c)
			continue
		}
		fmt.Fprintf(&e, "%%%02X", c)
	}
	return e.String()
}
