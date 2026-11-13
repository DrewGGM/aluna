package app_test

import (
	"strings"
	"testing"
	"time"

	"github.com/DrewGGM/aluna/internal/webhooks/internal/app"
)

// TestASignatureIsStandardWebhooks: the example of the Standard Webhooks specification signs as it says.
func TestASignatureIsStandardWebhooks(t *testing.T) {
	// the specification's example key: public
	example := "whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw"                                        // gitleaks:allow
	signed, err := app.Sign(example, "msg_p5jXN8AQM9LWM0D4loKWxJek", time.Unix(1614265330, 0), // gitleaks:allow
		[]byte(`{"test": 2432232314}`))
	if err != nil {
		t.Fatal(err)
	}
	want := app.Signed{ID: "msg_p5jXN8AQM9LWM0D4loKWxJek", Timestamp: "1614265330", Signature: "v1,g0hM9SsE+OTPJTGt/tmIKtSyZlE3uFJELVlNIOLJ1OE="}
	if signed != want {
		t.Errorf("signed %+v, want %+v", signed, want)
	}
}

func TestASecretIsWhsec(t *testing.T) {
	a, err := app.NewSecret()
	if err != nil {
		t.Fatal(err)
	}
	b, err := app.NewSecret()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(a, "whsec_") || len(a) != len("whsec_")+44 || a == b {
		t.Errorf("secrets %s and %s", a, b)
	}
	if _, err := app.Sign("not-a-secret", "id", time.Unix(0, 0), nil); err == nil {
		t.Error("signed with a secret that is not one")
	}
}
