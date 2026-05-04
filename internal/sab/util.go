package sab

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/pburkhalter/arrarr/internal/job"
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func contextWithTimeoutSafe(ctx context.Context, seconds int) (context.Context, context.CancelFunc) {
	if ctx.Err() != nil {
		return context.WithTimeout(context.Background(), time.Duration(seconds)*time.Second)
	}
	return context.WithTimeout(ctx, time.Duration(seconds)*time.Second)
}

func formatBytes(n int64) string {
	const (
		KB = 1024
		MB = 1024 * KB
		GB = 1024 * MB
		TB = 1024 * GB
	)
	switch {
	case n >= TB:
		return fmt.Sprintf("%.2f TB", float64(n)/TB)
	case n >= GB:
		return fmt.Sprintf("%.2f GB", float64(n)/GB)
	case n >= MB:
		return fmt.Sprintf("%.2f MB", float64(n)/MB)
	case n >= KB:
		return fmt.Sprintf("%.2f KB", float64(n)/KB)
	default:
		return fmt.Sprintf("%d B", n)
	}
}

func formatMB(n int64) string {
	return fmt.Sprintf("%.2f", float64(n)/(1024.0*1024.0))
}

func parseLimit(q string, def, max int) int {
	if q == "" {
		return def
	}
	n, err := strconv.Atoi(q)
	if err != nil || n <= 0 {
		return def
	}
	if n > max {
		return max
	}
	return n
}

func stateToSABStatus(s job.State) string {
	switch s {
	case job.StateNew:
		return "Queued"
	case job.StateSubmitted:
		return "Queued"
	case job.StateDownloading:
		return "Downloading"
	case job.StateCompletedTorbox:
		return "Verifying"
	}
	return "Queued"
}

func percentageFor(s job.State) string {
	switch s {
	case job.StateDownloading:
		return "50"
	case job.StateCompletedTorbox:
		return "99"
	}
	return "0"
}

func drainAndClose(rc io.ReadCloser) {
	_, _ = io.Copy(io.Discard, rc)
	_ = rc.Close()
}
