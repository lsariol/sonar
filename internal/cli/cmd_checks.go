package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/lsariol/sonar/internal/containers"
	"github.com/lsariol/sonar/internal/snapshot"
)

var checkHeader = []string{"PROJECT", "SERVICE", "URL", "STATE", "LAST CHECK", "RESULT", "LATENCY"}

func checkRow(now time.Time, ch snapshot.Check) []string {
	result, latency := "-", "-"
	if ch.LastAt != nil {
		result = ch.LastOutcome
		if ch.LastStatus != 0 {
			result += fmt.Sprintf(" (%d)", ch.LastStatus)
		}
		if ch.ConsecutiveFailures > 1 {
			result += fmt.Sprintf(", %d in a row", ch.ConsecutiveFailures)
		}
	}
	if ch.LastLatencyMs != nil {
		latency = fmt.Sprintf("%d ms", *ch.LastLatencyMs)
	}
	return []string{projectName(ch.Project), ch.Service, ch.URL, ch.State.String(), formatTimePtr(ch.LastAt), result, latency}
}

func (c *CLI) checks(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return usageError{form: "checks"}
	}
	snap, err := c.snapshot()
	if err != nil {
		return err
	}
	now := c.opts.Now()

	if len(snap.Checks) == 0 {
		info(fmt.Sprintf("No public addresses are checked. A service gets checked with the label %s=https://… in its compose file.", containers.PublicURLLabel))
	} else {
		rows := [][]string{checkHeader}
		for _, ch := range snap.Checks {
			rows = append(rows, checkRow(now, ch))
		}
		table(rows)
		info(fmt.Sprintf("%d %s", len(snap.Checks), plural(len(snap.Checks), "address", "addresses")))
	}

	for _, ch := range snap.Checks {
		if ch.LastError != "" {
			warn(fmt.Sprintf("%s/%s, %s: %s", projectName(ch.Project), ch.Service, ch.URL, ch.LastError))
		}
	}
	for _, a := range snap.Attention {
		if strings.Contains(a, containers.PublicURLLabel) {
			warn("Not checked: " + a)
		}
	}
	if !snap.Fresh(now) {
		warn(fmt.Sprintf("The snapshot is %s old: is sonar-agent running?", age(now, snap.GeneratedAt)))
	}
	return nil
}
