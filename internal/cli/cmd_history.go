package cli

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/lsariol/sonar/internal/api"
)

// maxDays is the longest strip `days` shows: what the database keeps.
const maxDays = 380

func (c *CLI) days(ctx context.Context, args []string) error {
	form := "days <project> [--days N]"
	value, rest, err := takeValue(args[1:], "--days")
	if err != nil {
		return usageError{reason: err.Error() + ".", form: form}
	}
	if len(rest) != 1 {
		return usageError{form: form}
	}
	n := 30
	if value != "" {
		n, err = strconv.Atoi(value)
		if err != nil || n < 1 || n > maxDays {
			return usageError{reason: fmt.Sprintf("--days must be a number from 1 to %d.", maxDays), form: form}
		}
	}
	project := rest[0]

	now := c.opts.Now()
	byProject, _, err := c.history(ctx, now, max(n, 90))
	if err != nil {
		return err
	}
	rows, ok := byProject[project]
	if !ok && !slices.Contains(c.projectNames(), project) {
		return fmt.Errorf("No project %q is running, and it has no history in the last %d days. \"projects\" lists what's running.", project, max(n, 90))
	}

	strip, up30, up90 := api.ProjectDays(now, n, rows)
	lines := [][]string{{"DATE (UTC)", "STATE", "UPTIME", "COVERAGE"}}
	for _, d := range strip {
		lines = append(lines, []string{d.Date, d.State, percentPtr(d.UptimePct), percent(d.CoveragePct)})
	}
	table(lines)
	info(fmt.Sprintf("%d %s. Uptime: %s over 30 days, %s over 90 days.", n, plural(n, "day", "days"), uptimeText(up30), uptimeText(up90)))
	return nil
}

// takeValue removes a flag and its value ("--days 7" or "--days=7") from
// args.
func takeValue(args []string, flag string) (string, []string, error) {
	value := ""
	rest := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == flag:
			if i+1 >= len(args) || args[i+1] == "" {
				return "", nil, fmt.Errorf("%s needs a value", flag)
			}
			value = args[i+1]
			i++
		case strings.HasPrefix(a, flag+"="):
			value = strings.TrimPrefix(a, flag+"=")
			if value == "" {
				return "", nil, fmt.Errorf("%s needs a value", flag)
			}
		default:
			rest = append(rest, a)
		}
	}
	return value, rest, nil
}
