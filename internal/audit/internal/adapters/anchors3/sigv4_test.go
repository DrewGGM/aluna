package anchors3

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestTheSignatureMatchesAWSsExample: signing the GET Object example of the AWS Signature Version 4 documentation (bucket
// examplebucket, us-east-1, 2013-05-24) gives the Authorization header the documentation gives.
func TestTheSignatureMatchesAWSsExample(t *testing.T) {
	r, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://examplebucket.s3.amazonaws.com/test.txt", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Range", "bytes=0-9")
	s := signer{region: "us-east-1", creds: Credentials{ //nolint:gosec // the keys of AWS's published example, not credentials
		AccessKeyID: "AKIAIOSFODNN7EXAMPLE", SecretAccessKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", // gitleaks:allow (AWS's published example)
	}}

	s.sign(r, nil, time.Date(2013, time.May, 24, 0, 0, 0, 0, time.UTC))

	want := "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request, " +
		"SignedHeaders=host;range;x-amz-content-sha256;x-amz-date, " +
		"Signature=f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41"
	if got := r.Header.Get("Authorization"); got != want {
		t.Fatalf("Authorization = %s\nwant %s", got, want)
	}
}

// TestAQuerySignatureMatchesAWSsExample: signing the GET Bucket (List Objects) example of the same documentation,
// whose query is already canonical and whose path is empty, gives the signature it gives.
func TestAQuerySignatureMatchesAWSsExample(t *testing.T) {
	r, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://examplebucket.s3.amazonaws.com?max-keys=2&prefix=J", nil)
	if err != nil {
		t.Fatal(err)
	}
	s := signer{region: "us-east-1", creds: Credentials{ //nolint:gosec // the keys of AWS's published example, not credentials
		AccessKeyID: "AKIAIOSFODNN7EXAMPLE", SecretAccessKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", // gitleaks:allow (AWS's published example)
	}}

	s.sign(r, nil, time.Date(2013, time.May, 24, 0, 0, 0, 0, time.UTC))

	if got := r.Header.Get("Authorization"); !strings.HasSuffix(got, "Signature=34b48302e7b5fa45bde8084f4b7868a86f0a534bc59db6670ed5711ef69dc6f7") {
		t.Fatalf("Authorization = %s, want the example's signature", got)
	}
}
