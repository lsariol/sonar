package cli

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/lsariol/sonar/internal/store"
)

// updateRows is a table of updates, oldest first, with their versions.
func updateRows(updates []store.Update) [][]string {
	rows := [][]string{{"STARTED", "PROJECT", "SERVICE", "FROM", "TO", "TOOK", "OUTCOME"}}
	for _, u := range updates {
		rows = append(rows, []string{formatTime(u.Started), projectName(u.Project), u.Service, orDash(u.From), orDash(u.To),
			duration(u.Ended.Sub(u.Started).Round(time.Second)), u.Outcome})
	}
	return rows
}

func (c *CLI) updates(ctx context.Context, args []string) error {
	form := "updates [project] [--days N]"
	value, rest, err := takeValue(args[1:], "--days")
	if err != nil {
		return usageError{reason: err.Error() + ".", form: form}
	}
	if len(rest) > 1 {
		return usageError{form: form}
	}
	n := 7
	if value != "" {
		n, err = strconv.Atoi(value)
		if err != nil || n < 1 || n > maxDays {
			return usageError{reason: fmt.Sprintf("--days must be a number from 1 to %d.", maxDays), form: form}
		}
	}

	now := c.opts.Now()
	h, err := c.history(ctx, now, n)
	if err != nil {
		return err
	}
	var list []store.Update
	for project, ups := range h.updates {
		if len(rest) == 0 || rest[0] == projectName(project) {
			list = append(list, ups...)
		}
	}
	slices.SortFunc(list, func(a, b store.Update) int { return a.Started.Compare(b.Started) })

	scope := "any project"
	if len(rest) == 1 {
		scope = fmt.Sprintf("%q", rest[0])
	}
	if len(list) == 0 {
		info(fmt.Sprintf("No updates of %s in the last %d %s.", scope, n, plural(n, "day", "days")))
		return nil
	}
	table(updateRows(list))
	failed := 0
	for _, u := range list {
		if u.Outcome != "ok" {
			failed++
		}
	}
	info(fmt.Sprintf("%d %s of %s in the last %d %s, %d that didn't come up healthy.",
		len(list), plural(len(list), "update", "updates"), scope, n, plural(n, "day", "days"), failed))
	return nil
}
