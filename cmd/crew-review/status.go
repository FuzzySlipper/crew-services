package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"crew-services/internal/review"
)

// status prints the review queue from a running crew-review. It only reads.
func status(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("crew-review status", flag.ContinueOnError)
	flags.SetOutput(stderr)
	url := flags.String("url", envOr(getenv, "CREW_REVIEW_URL", "http://127.0.0.1:8413"), "crew-review base URL")
	limit := flags.Int("limit", 5, "finished reviews to show (at most 100)")
	raw := flags.Bool("json", false, "print the pool as crew-review reports it")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(*url, "/")+"/v1/review-pool?limit="+strconv.Itoa(*limit), nil)
	if err != nil {
		fmt.Fprintln(stderr, "crew-review:", err)
		return 1
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintln(stderr, "crew-review is not answering:", err)
		return 1
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil || resp.StatusCode != http.StatusOK {
		fmt.Fprintf(stderr, "crew-review: review pool: %s %s %v\n", resp.Status, strings.TrimSpace(string(body)), err)
		return 1
	}
	if *raw {
		_, _ = stdout.Write(body)
		return 0
	}
	var pool review.Snapshot
	if err := json.Unmarshal(body, &pool); err != nil {
		fmt.Fprintln(stderr, "crew-review: review pool:", err)
		return 1
	}
	printStatus(stdout, pool, *limit, time.Now())
	return 0
}

func printStatus(w io.Writer, pool review.Snapshot, limit int, now time.Time) {
	fmt.Fprintf(w, "%s backend, capacity %d: %d running, %d queued, %d finalizing, %d waiting\n", pool.Backend, pool.Capacity, pool.Running, pool.Queued, pool.Finalizing, len(pool.Waiting))
	if loop := pool.SubmissionLoop; loop != nil && !loop.LastPassFinishedAt.IsZero() {
		fmt.Fprintf(w, "last submission pass %s ago", age(now, loop.LastPassFinishedAt))
		if loop.LastPassError != "" {
			fmt.Fprintf(w, ", failed: %s", loop.LastPassError)
		}
		fmt.Fprintln(w)
	}
	if len(pool.Active) > 0 {
		fmt.Fprintln(w, "\nActive")
		t := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(t, "STATE\tPROJECT\tTASK\tROUND\tAGE")
		for _, j := range pool.Active {
			fmt.Fprintf(t, "%s\t%s\t%d\t%d\t%s\n", j.State, j.Key.ProjectID, j.Key.TaskID, j.Key.ReviewRoundID, age(now, j.CreatedAt))
		}
		t.Flush()
	}
	if len(pool.Waiting) > 0 {
		fmt.Fprintln(w, "\nWaiting")
		t := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(t, "PHASE\tPROJECT\tTASK\tROUND\tAGE\tNOTE")
		for _, s := range pool.Waiting {
			fmt.Fprintf(t, "%s\t%s\t%d\t%s\t%s\t%s\n", s.Phase, s.ProjectID, s.TaskID, round(s.ReviewRoundID), age(now, s.CreatedAt), s.Failure)
		}
		t.Flush()
	}
	if len(pool.Recent) > 0 && limit > 0 {
		fmt.Fprintln(w, "\nRecent")
		t := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(t, "STATE\tVERDICT\tPROJECT\tTASK\tROUND\tTOOK\tENDED")
		for _, j := range pool.Recent {
			verdict := "-"
			if j.Receipt != nil && j.Receipt.Verdict != "" {
				verdict = string(j.Receipt.Verdict)
			}
			fmt.Fprintf(t, "%s\t%s\t%s\t%d\t%d\t%s\t%s ago\n", j.State, verdict, j.Key.ProjectID, j.Key.TaskID, j.Key.ReviewRoundID, age(j.UpdatedAt, j.CreatedAt), age(now, j.UpdatedAt))
		}
		t.Flush()
	}
}

func round(id int64) string {
	if id == 0 {
		return "-"
	}
	return strconv.FormatInt(id, 10)
}

func age(now, then time.Time) string {
	d := now.Sub(then)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}
