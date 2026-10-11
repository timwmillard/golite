package stripehook

import "testing"

func TestCLIOutput(t *testing.T) {
	banner := "\x1b[1mReady!\x1b[0m You are using Stripe API Version [2099-01-01]. Your webhook signing secret is whsec_abc123 (^C to quit)"
	line := stripANSI(banner)
	if want := "Ready! You are using Stripe API Version [2099-01-01]. Your webhook signing secret is whsec_abc123 (^C to quit)"; line != want {
		t.Errorf("stripANSI = %q, want %q", line, want)
	}
	if got := webhookSecret(line); got != "whsec_abc123" {
		t.Errorf("webhookSecret = %q", got)
	}
	if got := webhookSecret("no secret here"); got != "" {
		t.Errorf("webhookSecret = %q, want empty", got)
	}
	if got := webhookSecret("secret whsec_end"); got != "whsec_end" {
		t.Errorf("webhookSecret at end = %q", got)
	}

	fwd := "2026-10-11 09:30:00   --> account.updated [evt_1Abc]"
	id := eventID(fwd)
	if id != "evt_1Abc" {
		t.Errorf("eventID = %q", id)
	}
	if got := cleanCLILine(fwd, id); got != "--> account.updated" {
		t.Errorf("cleanCLILine = %q", got)
	}
	if got := eventID("[evt_unterminated"); got != "" {
		t.Errorf("eventID = %q, want empty", got)
	}
}
