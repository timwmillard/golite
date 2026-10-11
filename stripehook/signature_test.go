package stripehook

import (
	"errors"
	"testing"
	"time"
)

func TestVerify(t *testing.T) {
	payload := []byte(`{"id":"evt_1"}`)
	secret := "whsec_test"
	now := time.Unix(1700000000, 0)
	good := Sign(payload, secret, now)

	// Computed independently with
	// printf '1700000000.{"id":"evt_1"}' | openssl dgst -sha256 -hmac whsec_test
	// Sign was also checked against stripe-go's webhook.GenerateTestSignedPayload.
	const golden = "t=1700000000,v1=c89214b5b5da833daed6f0b8c5bb6bd58cea9022bd80ccc78230f3942d632925"

	tests := []struct {
		name      string
		payload   []byte
		header    string
		secret    string
		tolerance time.Duration
		at        time.Time
		want      error
	}{
		{"valid", payload, good, secret, DefaultTolerance, now, nil},
		{"golden", payload, golden, secret, DefaultTolerance, now, nil},
		{"one of several v1", payload, "t=1700000000,v1=00,v0=abc,v1=" + good[len("t=1700000000,v1="):], secret, DefaultTolerance, now, nil},
		{"tampered payload", []byte(`{"id":"evt_2"}`), good, secret, DefaultTolerance, now, ErrInvalidSignature},
		{"wrong secret", payload, good, "whsec_other", DefaultTolerance, now, ErrInvalidSignature},
		{"too old", payload, good, secret, DefaultTolerance, now.Add(DefaultTolerance + time.Second), ErrTooOld},
		{"no tolerance", payload, good, secret, 0, now.Add(24 * time.Hour), nil},
		{"empty header", payload, "", secret, DefaultTolerance, now, ErrNoSignature},
		{"no v1", payload, "t=1700000000,v0=abc", secret, DefaultTolerance, now, ErrNoSignature},
		{"bad timestamp", payload, "t=x,v1=" + good[len("t=1700000000,v1="):], secret, DefaultTolerance, now, ErrNoSignature},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := verify(tt.payload, tt.header, tt.secret, tt.tolerance, tt.at)
			if !errors.Is(err, tt.want) {
				t.Errorf("verify = %v, want %v", err, tt.want)
			}
		})
	}
}
