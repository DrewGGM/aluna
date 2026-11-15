// Package sources reads the restrictive lists from where their owners publish them (ADR-0018): the SDN list of
// OFAC (CSV), the consolidated list of the UN Security Council (XML) and the PEPs that Función Pública publishes
// in datos.gov.co (Socrata JSON). Each list is its entries and a version, a hash of what was read, that changes
// when they do.
package sources

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/url"
)

// maxDownload bounds what is read of a list: the SDN list is some 6 MB, the PEPs of a page some 15 MB.
const maxDownload = 64 << 20

// errDownload means a list that could not be read: its owner did not answer it, or it is larger than any list.
var errDownload = errors.New("the list could not be downloaded")

// download is the body of a GET of target through client, which must be a 200 of up to maxDownload bytes. Its
// errors name the source by its host and path, never by its query, which may carry a token.
func download(ctx context.Context, client *http.Client, target string) ([]byte, error) {
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, target, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("%w: its URL does not parse", errDownload)
	}
	where := r.URL.Host + r.URL.Path
	resp, err := client.Do(r)
	if urlErr := (*url.Error)(nil); errors.As(err, &urlErr) {
		err = urlErr.Err // without the URL, query and all
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", errDownload, where, err)
	}
	defer resp.Body.Close() //nolint:errcheck // the body is read whole; closing it cannot lose anything
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: %s answered %d", errDownload, where, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxDownload+1))
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", errDownload, where, err)
	}
	if len(body) > maxDownload {
		return nil, fmt.Errorf("%w: %s is over %d bytes", errDownload, where, maxDownload)
	}
	return body, nil
}

// versionOf is the version of a list read as parts: a hash of them, which changes when they do.
func versionOf(parts ...[]byte) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write(p)
	}
	return version(h)
}

// version is the version of a list whose parts h hashed.
func version(h hash.Hash) string { return hex.EncodeToString(h.Sum(nil))[:16] }
