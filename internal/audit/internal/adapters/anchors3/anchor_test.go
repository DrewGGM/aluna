package anchors3

import (
	"crypto/md5" //nolint:gosec // the fake checks the Content-MD5 S3 checks
	"crypto/sha256"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DrewGGM/aluna/internal/audit/internal/app"
)

// fakeBucket is an S3 bucket in memory, at /anchors: it keeps every version put on a key, its id its index, takes a
// conditional PUT only if the key has none and only with its object lock in compliance mode until a later time, its
// Content-MD5 and a signature, lists a key's versions and reads one by its id. conflict answers 409 to the next
// PUT.
type fakeBucket struct {
	mu       sync.Mutex
	versions map[string][]string // by key: the bodies put on it, "" for a delete marker
	conflict bool
}

func (f *fakeBucket) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.Header.Get("Authorization"), algorithm+" Credential=AKID/") {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	key := strings.TrimPrefix(r.URL.Path, "/anchors/")
	switch {
	case r.Method == http.MethodGet && r.URL.Query().Has("versions"):
		f.list(w, r.URL.Query().Get("prefix"))
	case r.Method == http.MethodGet:
		f.get(w, key, r.URL.Query().Get("versionId"))
	default:
		w.WriteHeader(f.put(r, key))
	}
}

func (f *fakeBucket) list(w http.ResponseWriter, key string) {
	type entry struct {
		Key       string
		VersionID string `xml:"VersionId"`
	}
	var result struct {
		XMLName  xml.Name `xml:"ListVersionsResult"`
		Versions []entry  `xml:"Version"`
		Markers  []entry  `xml:"DeleteMarker"`
	}
	for i, body := range f.versions[key] {
		if body == "" {
			result.Markers = append(result.Markers, entry{key, strconv.Itoa(i)})
		} else {
			result.Versions = append(result.Versions, entry{key, strconv.Itoa(i)})
		}
	}
	_ = xml.NewEncoder(w).Encode(result)
}

func (f *fakeBucket) get(w http.ResponseWriter, key, versionID string) {
	versions := f.versions[key]
	i, err := strconv.Atoi(versionID)
	if err != nil || i >= len(versions) || versions[i] == "" {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	_, _ = io.WriteString(w, versions[i])
}

func (f *fakeBucket) put(r *http.Request, key string) int {
	body, _ := io.ReadAll(r.Body)
	sum := md5.Sum(body) //nolint:gosec // what S3 checks
	until, err := time.Parse(time.RFC3339, r.Header.Get("X-Amz-Object-Lock-Retain-Until-Date"))
	switch {
	case f.conflict:
		f.conflict = false
		return http.StatusConflict
	case r.Header.Get("If-None-Match") != "*" || r.Header.Get("X-Amz-Object-Lock-Mode") != "COMPLIANCE" || err != nil ||
		!until.After(time.Now()) || r.Header.Get("Content-Md5") != base64.StdEncoding.EncodeToString(sum[:]):
		return http.StatusBadRequest
	case len(f.versions[key]) > 0 && f.versions[key][len(f.versions[key])-1] != "":
		return http.StatusPreconditionFailed
	}
	f.versions[key] = append(f.versions[key], string(body))
	return http.StatusOK
}

func bucketAt(t *testing.T, f *fakeBucket) *Bucket {
	t.Helper()
	ts := httptest.NewServer(f)
	t.Cleanup(ts.Close)
	b, err := New(ts.URL+"/anchors", "audit", "us-east-1", Credentials{AccessKeyID: "AKID", SecretAccessKey: "secret"}, time.Hour, ts.Client())
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestAnAnchorIsCreatedOnce (ADR-0017): the anchored hash reads back the same, in its block's object, retained in
// compliance mode until later; anchoring it again with the same hash does nothing, also after a conflict, and with
// a different one is ErrAnchored, without changing it; a block with no object is ErrNotAnchored.
func TestAnAnchorIsCreatedOnce(t *testing.T) {
	f := &fakeBucket{versions: map[string][]string{}}
	b := bucketAt(t, f)
	first, second := sha256.Sum256([]byte("one")), sha256.Sum256([]byte("two"))

	if err := b.Put(t.Context(), 7, first); err != nil {
		t.Fatal(err)
	}
	if err := b.Put(t.Context(), 7, first); err != nil {
		t.Fatalf("repeating the same anchor = %v", err)
	}
	f.conflict = true
	if err := b.Put(t.Context(), 7, first); err != nil {
		t.Fatalf("repeating the same anchor after a conflict = %v", err)
	}
	if err := b.Put(t.Context(), 7, second); !errors.Is(err, app.ErrAnchored) {
		t.Fatalf("rewriting the anchor = %v, want ErrAnchored", err)
	}

	if got, err := b.Get(t.Context(), 7); err != nil || got != first {
		t.Fatalf("anchor = %x (%v), want %x", got, err, first)
	}
	if versions := f.versions["audit/00000000000000000007.sha256"]; len(versions) != 1 {
		t.Fatalf("versions of the block's key under the prefix = %v, want one", versions)
	}
	if _, err := b.Get(t.Context(), 8); !errors.Is(err, app.ErrNotAnchored) {
		t.Fatalf("a block with no anchor = %v, want ErrNotAnchored", err)
	}
}

// TestAReplacedAnchorDoesNotRead (ADR-0017): Object Lock keeps the anchored version, not the key. Another version put
// over it, or a delete marker, would read as the anchor: either is errReplaced, not a hash nor ErrNotAnchored. A
// key with only a delete marker is errReplaced too.
func TestAReplacedAnchorDoesNotRead(t *testing.T) {
	f := &fakeBucket{versions: map[string][]string{"audit/00000000000000000007.sha256": {""}}}
	if _, err := bucketAt(t, f).Get(t.Context(), 7); !errors.Is(err, errReplaced) {
		t.Errorf("a key with only a delete marker = %v, want errReplaced", err)
	}

	for name, over := range map[string]string{"another version": "forged", "a deletion": ""} {
		f := &fakeBucket{versions: map[string][]string{}}
		b := bucketAt(t, f)
		if err := b.Put(t.Context(), 7, sha256.Sum256([]byte("one"))); err != nil {
			t.Fatal(err)
		}
		f.versions["audit/00000000000000000007.sha256"] = append(f.versions["audit/00000000000000000007.sha256"], over)

		if _, err := b.Get(t.Context(), 7); !errors.Is(err, errReplaced) {
			t.Errorf("an anchor with %s = %v, want errReplaced", name, err)
		}
	}
}

// TestABucketNeedsAllItTakes: a bucket URL that is not http(s), has credentials, a query or characters to escape, a
// prefix with such characters or empty parts, a missing region or key, or no retention is refused.
func TestABucketNeedsAllItTakes(t *testing.T) {
	keys := Credentials{AccessKeyID: "AKID", SecretAccessKey: "secret"}
	for name, tc := range map[string]struct {
		url, prefix, region string
		keys                Credentials
		retain              time.Duration
	}{
		"another scheme":    {"ftp://host/bucket", "", "us-east-1", keys, time.Hour},
		"user information":  {"https://user:pass@host/bucket", "", "us-east-1", keys, time.Hour},
		"a query":           {"https://host/bucket?x=1", "", "us-east-1", keys, time.Hour},
		"a space":           {"https://host/my bucket", "", "us-east-1", keys, time.Hour},
		"an empty part":     {"https://host/bucket", "audit//2026", "us-east-1", keys, time.Hour},
		"a prefix to quote": {"https://host/bucket", "audit log", "us-east-1", keys, time.Hour},
		"no region":         {"https://host/bucket", "", "", keys, time.Hour},
		"no secret key":     {"https://host/bucket", "", "us-east-1", Credentials{AccessKeyID: "AKID"}, time.Hour},
		"no retention":      {"https://host/bucket", "", "us-east-1", keys, 0},
	} {
		if _, err := New(tc.url, tc.prefix, tc.region, tc.keys, tc.retain, nil); !errors.Is(err, errBucket) {
			t.Errorf("%s = %v, want errBucket", name, err)
		}
	}
}

// TestAKeyIsEscapedAsAWSSigns: a query value keeps letters, digits and -._~ and escapes the rest as %XX.
func TestAKeyIsEscapedAsAWSSigns(t *testing.T) {
	if got := escape("audit/0001.sha256 x+~"); got != "audit%2F0001.sha256%20x%2B~" {
		t.Fatalf("escaped = %s", got)
	}
}
