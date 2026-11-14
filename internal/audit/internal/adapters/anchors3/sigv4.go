package anchors3

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"slices"
	"strings"
	"time"
)

// signer signs S3 requests with AWS Signature Version 4 (ADR-0017): only as much of it as one object's PUT and GET
// and the listing of its versions need, so that the anchor does not bring the AWS SDK and its dependency tree.
type signer struct {
	region string
	creds  Credentials
}

const (
	algorithm = "AWS4-HMAC-SHA256"
	service   = "s3"
	amzDate   = "20060102T150405Z"
	dateOnly  = "20060102"
)

// sign adds to r, whose body is payload and whose query, if it has one, is already canonical (sorted and escaped as
// AWS does), its x-amz-date and x-amz-content-sha256 (and x-amz-security-token with a session token) as of now, and
// its Authorization, which signs every header r carries and its host.
func (s signer) sign(r *http.Request, payload []byte, now time.Time) {
	now = now.UTC()
	payloadHash := hexSHA256(payload)
	r.Header.Set("X-Amz-Date", now.Format(amzDate))
	r.Header.Set("X-Amz-Content-Sha256", payloadHash)
	if s.creds.SessionToken != "" {
		r.Header.Set("X-Amz-Security-Token", s.creds.SessionToken)
	}
	names, headers := canonicalHeaders(r)
	path := r.URL.EscapedPath()
	if path == "" {
		path = "/"
	}
	canonical := strings.Join([]string{r.Method, path, r.URL.RawQuery, headers, names, payloadHash}, "\n")
	scope := strings.Join([]string{now.Format(dateOnly), s.region, service, "aws4_request"}, "/")
	toSign := strings.Join([]string{algorithm, now.Format(amzDate), scope, hexSHA256([]byte(canonical))}, "\n")
	key := []byte("AWS4" + s.creds.SecretAccessKey)
	for _, part := range []string{now.Format(dateOnly), s.region, service, "aws4_request"} {
		key = hmacSHA256(key, part)
	}
	r.Header.Set("Authorization", algorithm+" Credential="+s.creds.AccessKeyID+"/"+scope+", SignedHeaders="+names+
		", Signature="+hex.EncodeToString(hmacSHA256(key, toSign)))
}

// canonicalHeaders are the names, lowercase and sorted, of r's headers and its host, separated by semicolons, and
// those headers as "name:value" lines, each one ending in a newline.
func canonicalHeaders(r *http.Request) (string, string) {
	values := map[string]string{"host": r.URL.Host}
	for name, v := range r.Header {
		values[strings.ToLower(name)] = strings.TrimSpace(strings.Join(v, ","))
	}
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	slices.Sort(names)
	var lines strings.Builder
	for _, name := range names {
		lines.WriteString(name + ":" + values[name] + "\n")
	}
	return strings.Join(names, ";"), lines.String()
}

func hexSHA256(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}
