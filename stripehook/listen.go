package stripehook

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// Listen runs `stripe listen` from the Stripe CLI in the background,
// forwarding the account's events (and its connected accounts') to
// forwardTo, e.g. "http://localhost:7880/webhooks/stripe", and returns a
// func that stops it. It's for local development, where Stripe can't reach
// the app: in production, register the endpoint with Stripe instead.
//
// The CLI makes up a new signing secret each time it starts, so Listen
// reads it from the CLI's output and passes it to SetSecret; leave
// Config.Secret empty. apiKey may be empty to use the CLI's own login
// (`stripe login`). The CLI's output is logged at debug level.
//
// It fails if the stripe CLI isn't on the PATH. The CLI stops when ctx is
// done, or when stop is called.
func (h *Hook) Listen(ctx context.Context, apiKey, forwardTo string) (stop func(), err error) {
	if _, err := exec.LookPath("stripe"); err != nil {
		return nil, errors.New("stripehook: stripe CLI not found on PATH")
	}

	args := []string{"listen", "--forward-to", forwardTo}
	if apiKey != "" {
		args = append([]string{"--api-key", apiKey}, args...)
	}
	cmd := exec.CommandContext(ctx, "stripe", args...)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("stripehook: start stripe listen: %w", err)
	}
	h.log.Info("stripehook: stripe listen started", "forward_to", forwardTo, "pid", cmd.Process.Pid)

	go h.logCLI(stdout)
	go h.logCLI(stderr)

	done := make(chan struct{})
	go func() {
		defer close(done)
		err := cmd.Wait()
		switch {
		case ctx.Err() != nil:
		case err != nil:
			h.log.Error("stripehook: stripe listen exited", "err", err)
		default:
			h.log.Info("stripehook: stripe listen stopped")
		}
	}()

	return func() {
		cmd.Process.Kill()
		<-done
	}, nil
}

// logCLI logs the CLI's output, passing on the signing secret when it
// appears.
func (h *Hook) logCLI(r io.Reader) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := stripANSI(scanner.Text())
		if strings.TrimSpace(line) == "" {
			continue
		}
		if secret := webhookSecret(line); secret != "" {
			h.SetSecret(secret)
		}
		if id := eventID(line); id != "" {
			h.log.Debug("stripe cli: "+cleanCLILine(line, id), "event", id)
		} else {
			h.log.Debug("stripe cli: " + cleanCLILine(line, ""))
		}
	}
	if err := scanner.Err(); err != nil {
		h.log.Debug("stripehook: reading stripe listen output", "err", err)
	}
}

// webhookSecret returns the "whsec_..." token in the CLI's startup banner
// ("Ready! ... Your webhook signing secret is whsec_xxx (^C to quit)"), or
// "" if line has none.
func webhookSecret(line string) string {
	start := strings.Index(line, "whsec_")
	if start == -1 {
		return ""
	}
	rest := line[start:]
	if end := strings.IndexAny(rest, " \t("); end != -1 {
		return rest[:end]
	}
	return rest
}

// eventID returns the evt_ id in "[evt_...]", or "" if line has none.
func eventID(line string) string {
	start := strings.Index(line, "[evt_")
	if start == -1 {
		return ""
	}
	end := strings.Index(line[start:], "]")
	if end == -1 {
		return ""
	}
	return line[start+1 : start+end]
}

// cleanCLILine drops the "YYYY-MM-DD HH:MM:SS" prefix and the event id
// (logged as an attribute instead) from a CLI line.
func cleanCLILine(line, id string) string {
	if len(line) > 19 && line[4] == '-' && line[7] == '-' && line[10] == ' ' && line[13] == ':' && line[16] == ':' {
		line = line[19:]
	}
	if id != "" {
		line = strings.Replace(line, "["+id+"]", "", 1)
	}
	return strings.TrimSpace(line)
}

// stripANSI removes ANSI escape sequences, which the CLI colors its output
// with.
func stripANSI(s string) string {
	var b strings.Builder
	inEscape := false
	for i, r := range s {
		if r == '\x1b' && i+1 < len(s) && s[i+1] == '[' {
			inEscape = true
			continue
		}
		if inEscape {
			if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') {
				inEscape = false
			}
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
