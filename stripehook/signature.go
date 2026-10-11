package stripehook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// DefaultTolerance is how old a delivery's signature may be before it's
// rejected, as a defence against replays. It's Stripe's own default.
const DefaultTolerance = 5 * time.Minute

var (
	// ErrNoSignature means the Stripe-Signature header has no timestamp or
	// no v1 signature.
	ErrNoSignature = errors.New("stripehook: no signature in header")

	// ErrInvalidSignature means none of the header's v1 signatures match
	// the payload signed with the secret.
	ErrInvalidSignature = errors.New("stripehook: signature doesn't match payload")

	// ErrTooOld means the signature's timestamp is older than the tolerance.
	ErrTooOld = errors.New("stripehook: signature timestamp too old")
)

// Verify checks a webhook payload against its Stripe-Signature header
// ("t=<unix>,v1=<hex>[,v1=<hex>...]"): one of the v1 signatures must be
// the HMAC-SHA256 of "<t>.<payload>" keyed by secret, and t must be no
// older than tolerance. A tolerance of 0 skips the age check.
func Verify(payload []byte, header, secret string, tolerance time.Duration) error {
	return verify(payload, header, secret, tolerance, time.Now())
}

func verify(payload []byte, header, secret string, tolerance time.Duration, now time.Time) error {
	var ts string
	var sigs [][]byte
	for part := range strings.SplitSeq(header, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		switch k {
		case "t":
			ts = v
		case "v1":
			// Not hex, or not a SHA-256: can't match, so skip it.
			if sig, err := hex.DecodeString(v); err == nil && len(sig) == sha256.Size {
				sigs = append(sigs, sig)
			}
		}
	}
	if ts == "" || len(sigs) == 0 {
		return ErrNoSignature
	}
	unix, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return fmt.Errorf("%w: bad timestamp %q", ErrNoSignature, ts)
	}

	want := sign(payload, secret, ts)
	matched := false
	for _, sig := range sigs {
		if hmac.Equal(sig, want) {
			matched = true
		}
	}
	if !matched {
		return ErrInvalidSignature
	}
	if tolerance > 0 && now.Sub(time.Unix(unix, 0)) > tolerance {
		return ErrTooOld
	}
	return nil
}

// Sign returns the Stripe-Signature header Stripe would send with payload
// at time t, for tests that post deliveries to a Hook.
func Sign(payload []byte, secret string, t time.Time) string {
	ts := strconv.FormatInt(t.Unix(), 10)
	return "t=" + ts + ",v1=" + hex.EncodeToString(sign(payload, secret, ts))
}

func sign(payload []byte, secret, ts string) []byte {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts))
	mac.Write([]byte("."))
	mac.Write(payload)
	return mac.Sum(nil)
}
