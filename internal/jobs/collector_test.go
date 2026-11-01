package jobs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/dbtest"
	"go-monolith/metrics"
)

// gather runs the collector through a registry the way /metrics does and returns
// the samples of every family by label set ("a=b,c=d" in label-name order).
func gather(t *testing.T, c prometheus.Collector) map[string]map[string]float64 {
	t.Helper()
	reg := prometheus.NewRegistry()
	require.NoError(t, reg.Register(c), "the collector must describe exactly what it collects")
	families, err := reg.Gather()
	require.NoError(t, err)

	out := map[string]map[string]float64{}
	for _, family := range families {
		samples := map[string]float64{}
		for _, m := range family.GetMetric() {
			key := ""
			for _, label := range m.GetLabel() {
				if key != "" {
					key += ","
				}
				key += label.GetName() + "=" + label.GetValue()
			}
			samples[key] = m.GetGauge().GetValue()
		}
		out[family.GetName()] = samples
	}
	return out
}

func TestStateCollectorReportsQueuedJobs(t *testing.T) {
	e := newInsertOnlyRiver(t)
	collector := NewStateCollector(e.database.SqlDB)
	ctx := context.Background()

	before := gather(t, collector)
	require.EqualValues(t, 1, before["state_collector_up"][""])

	// One job that has been due for ten minutes, one due now. (The insert-only
	// client has no scheduler, so the age is set in the row.)
	old, err := e.client.Insert(ctx, DistillationWorkerArgs{FileID: uuid.New()}, nil)
	require.NoError(t, err)
	_, err = e.pool.Exec(ctx, `UPDATE river_job SET scheduled_at = now() - interval '10 minutes' WHERE id = $1`, old.Job.ID)
	require.NoError(t, err)
	_, err = e.client.Insert(ctx, DistillationWorkerArgs{FileID: uuid.New()}, nil)
	require.NoError(t, err)

	after := gather(t, collector)
	const available = "kind=distillation_worker,state=available"
	assert.Equal(t, before["river_jobs"][available]+2, after["river_jobs"][available], "both jobs wait")

	// Every state is reported for a kind that has jobs, so a line does not vanish.
	for _, state := range jobStates {
		_, ok := after["river_jobs"][fmt.Sprintf("kind=distillation_worker,state=%s", state)]
		assert.True(t, ok, "state %s is missing", state)
	}

	age := after["river_oldest_available_job_age_seconds"]["kind=distillation_worker"]
	assert.GreaterOrEqual(t, age, 600.0, "the oldest waiting job has been due for at least ten minutes")
	assert.Less(t, age, 3600.0)
}

func TestStateCollectorCountsFilesByStatus(t *testing.T) {
	e := newInsertOnlyRiver(t)
	collector := NewStateCollector(e.database.SqlDB)
	ctx := context.Background()

	user, err := e.database.Queries.CreateUser(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { dbtest.DeleteUser(t, e.database.Pool, user.ID) })

	before := gather(t, collector)["ingested_files"]
	for _, status := range fileStatuses {
		_, ok := before["status="+status]
		assert.True(t, ok, "status %s is always reported, 0 when there are no files", status)
	}

	for i, status := range []string{"pending", "processing", "processing", "failed"} {
		_, err := e.database.SqlDB.ExecContext(ctx,
			`INSERT INTO ingested_files (user_id, file_name, file_hash, mime_type, file_size, status, local_path)
			 VALUES ($1, $2, $3, 'text/plain', 1, $4, 'x')`, user.ID, fmt.Sprintf("f%d.txt", i), uuid.NewString(), status)
		require.NoError(t, err)
	}
	// A deleted file is not counted.
	_, err = e.database.SqlDB.ExecContext(ctx,
		`INSERT INTO ingested_files (user_id, file_name, file_hash, mime_type, file_size, status, local_path, deleted_at)
		 VALUES ($1, 'gone.txt', $2, 'text/plain', 1, 'completed', 'x', now())`, user.ID, uuid.NewString())
	require.NoError(t, err)

	after := gather(t, collector)["ingested_files"]
	assert.Equal(t, before["status=pending"]+1, after["status=pending"])
	assert.Equal(t, before["status=processing"]+2, after["status=processing"])
	assert.Equal(t, before["status=failed"]+1, after["status=failed"])
	assert.Equal(t, before["status=completed"], after["status=completed"], "a soft deleted file does not count")
}

func TestStateCollectorSaysWhenTheDatabaseIsGone(t *testing.T) {
	e := newInsertOnlyRiver(t)
	closed, err := sql.Open("pgx", dbtest.ConnString(t))
	require.NoError(t, err)
	require.NoError(t, closed.Close())

	got := gather(t, NewStateCollector(closed))

	assert.EqualValues(t, 0, got["state_collector_up"][""], "an unreadable database is a visible 0, not a hang or a gap")
	_ = e
}

func TestIngestResultNamesTheOutcome(t *testing.T) {
	live := context.Background()
	stopped, cancel := context.WithCancel(context.Background())
	cancel()

	assert.Equal(t, "completed", ingestResult(live, nil))
	assert.Equal(t, "cancelled", ingestResult(live, river.JobCancel(errors.New("empty file"))))
	assert.Equal(t, "cancelled", ingestResult(live, fmt.Errorf("wrapped: %w", river.JobCancel(errors.New("x")))))
	assert.Equal(t, "failed", ingestResult(live, errors.New("embedder down")))
	assert.Equal(t, "interrupted", ingestResult(stopped, errors.New("context canceled")))
	assert.Equal(t, "cancelled", ingestResult(stopped, river.JobCancel(errors.New("x"))), "a permanent failure is not hidden by a stop")
}

func counterValue(t *testing.T, c prometheus.Counter) float64 {
	t.Helper()
	var m dto.Metric
	require.NoError(t, c.Write(&m))
	return m.GetCounter().GetValue()
}

func histogramCount(t *testing.T, o prometheus.Observer) uint64 {
	t.Helper()
	var m dto.Metric
	require.NoError(t, o.(prometheus.Metric).Write(&m))
	return m.GetHistogram().GetSampleCount()
}

func TestIngestWorkerCountsItsAttempts(t *testing.T) {
	e := newIngestEnv(t)
	ctx := context.Background()

	completed := counterValue(t, metrics.IngestJobs.WithLabelValues("completed"))
	cancelled := counterValue(t, metrics.IngestJobs.WithLabelValues("cancelled"))
	durations := histogramCount(t, metrics.IngestDuration.WithLabelValues("completed"))

	file := e.addFile(t, "metrics.txt", []byte("Първи ред на документа.\nВтори ред на документа."))
	require.NoError(t, e.worker.Work(ctx, ingestJob(file.ID, 1, 25)))
	assert.Equal(t, completed+1, counterValue(t, metrics.IngestJobs.WithLabelValues("completed")))
	assert.Equal(t, durations+1, histogramCount(t, metrics.IngestDuration.WithLabelValues("completed")))

	// A file row that is gone can never be ingested: River is told to stop.
	err := e.worker.Work(ctx, ingestJob(uuid.New(), 1, 25))
	require.True(t, isCancelled(err))
	assert.Equal(t, cancelled+1, counterValue(t, metrics.IngestJobs.WithLabelValues("cancelled")))
	assert.Equal(t, completed+1, counterValue(t, metrics.IngestJobs.WithLabelValues("completed")), "a cancelled attempt is not a completed one")
}
