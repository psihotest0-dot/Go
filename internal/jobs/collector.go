package jobs

import (
	"context"
	"database/sql"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// collectTimeout bounds the queries of one scrape, so a slow database cannot make
// /metrics hang.
const collectTimeout = 3 * time.Second

// jobStates are the states of a River job; every kind that has a job gets a
// sample for each of them (0 when there is none), so a dashboard line does not
// vanish and reappear.
var jobStates = []string{"available", "cancelled", "completed", "discarded", "pending", "retryable", "running", "scheduled"}

// fileStatuses are the ingest states of a file (ingested_files.status).
var fileStatuses = []string{"pending", "processing", "completed", "failed"}

// StateCollector reports what is waiting in the database: River jobs by kind and
// state, how long the oldest waiting job has been due, and the files by ingest
// status. It reads the tables on every scrape (two small aggregate queries), which
// is how the queue is seen without any change to the workers.
type StateCollector struct {
	db *sql.DB

	jobs   *prometheus.Desc
	oldest *prometheus.Desc
	files  *prometheus.Desc
	up     *prometheus.Desc
}

// NewStateCollector creates the collector; register it with prometheus.MustRegister.
func NewStateCollector(db *sql.DB) *StateCollector {
	return &StateCollector{
		db: db,
		jobs: prometheus.NewDesc("river_jobs",
			"River jobs by kind and state.", []string{"kind", "state"}, nil),
		oldest: prometheus.NewDesc("river_oldest_available_job_age_seconds",
			"How long the oldest job that is ready to run has been waiting, by kind (0 when none waits).", []string{"kind"}, nil),
		files: prometheus.NewDesc("ingested_files",
			"Uploaded files (not deleted) by ingest status.", []string{"status"}, nil),
		up: prometheus.NewDesc("state_collector_up",
			"1 when the last scrape could read the job and file tables, 0 when it could not.", nil, nil),
	}
}

func (c *StateCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.jobs
	ch <- c.oldest
	ch <- c.files
	ch <- c.up
}

func (c *StateCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), collectTimeout)
	defer cancel()

	ok := c.collectJobs(ctx, ch) && c.collectFiles(ctx, ch)
	up := 1.0
	if !ok {
		up = 0
	}
	ch <- prometheus.MustNewConstMetric(c.up, prometheus.GaugeValue, up)
}

func (c *StateCollector) collectJobs(ctx context.Context, ch chan<- prometheus.Metric) bool {
	counts := map[string]map[string]float64{} // kind -> state -> count
	rows, err := c.db.QueryContext(ctx, `SELECT kind, state::text, count(*) FROM river_job GROUP BY kind, state`)
	if err != nil {
		slog.Warn("metrics: failed to read the River jobs", "error", err)
		return false
	}
	for rows.Next() {
		var kind, state string
		var n float64
		if err := rows.Scan(&kind, &state, &n); err != nil {
			_ = rows.Close()
			slog.Warn("metrics: failed to read a River job row", "error", err)
			return false
		}
		if counts[kind] == nil {
			counts[kind] = map[string]float64{}
		}
		counts[kind][state] = n
	}
	if err := rows.Err(); err != nil {
		slog.Warn("metrics: failed to read the River jobs", "error", err)
		return false
	}
	_ = rows.Close()

	ages := map[string]float64{}
	ageRows, err := c.db.QueryContext(ctx, `
		SELECT kind, COALESCE(EXTRACT(EPOCH FROM now() - min(scheduled_at)), 0)
		FROM river_job WHERE state = 'available' GROUP BY kind`)
	if err != nil {
		slog.Warn("metrics: failed to read the age of the waiting River jobs", "error", err)
		return false
	}
	for ageRows.Next() {
		var kind string
		var age float64
		if err := ageRows.Scan(&kind, &age); err != nil {
			_ = ageRows.Close()
			slog.Warn("metrics: failed to read a River job age", "error", err)
			return false
		}
		ages[kind] = max(age, 0)
	}
	if err := ageRows.Err(); err != nil {
		slog.Warn("metrics: failed to read the age of the waiting River jobs", "error", err)
		return false
	}
	_ = ageRows.Close()

	for kind, byState := range counts {
		for _, state := range jobStates {
			ch <- prometheus.MustNewConstMetric(c.jobs, prometheus.GaugeValue, byState[state], kind, state)
		}
		ch <- prometheus.MustNewConstMetric(c.oldest, prometheus.GaugeValue, ages[kind], kind)
	}
	return true
}

func (c *StateCollector) collectFiles(ctx context.Context, ch chan<- prometheus.Metric) bool {
	rows, err := c.db.QueryContext(ctx, `SELECT status, count(*) FROM ingested_files WHERE deleted_at IS NULL GROUP BY status`)
	if err != nil {
		slog.Warn("metrics: failed to read the file statuses", "error", err)
		return false
	}
	defer rows.Close()

	counts := map[string]float64{}
	for rows.Next() {
		var status string
		var n float64
		if err := rows.Scan(&status, &n); err != nil {
			slog.Warn("metrics: failed to read a file status row", "error", err)
			return false
		}
		counts[status] = n
	}
	if err := rows.Err(); err != nil {
		slog.Warn("metrics: failed to read the file statuses", "error", err)
		return false
	}

	for _, status := range fileStatuses {
		ch <- prometheus.MustNewConstMetric(c.files, prometheus.GaugeValue, counts[status], status)
	}
	return true
}
