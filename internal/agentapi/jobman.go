package agentapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"cloud2api/internal/store"
)

// errJobCanceled marks job runs stopped by cancellation.
var errJobCanceled = errors.New("canceled")

// jobWaitCap bounds the ?wait=N long-poll (§4.10.4).
const jobWaitCap = 60 * time.Second

// jobGlobalConcurrency caps in-flight job goroutines instance-wide; per-key
// limits come from limits.concurrent_jobs.
const jobGlobalConcurrency = 8

// jobMaxDuration bounds one job runner; transfers of very large trees should
// fail rather than hang forever.
const jobMaxDuration = 6 * time.Hour

// jobPollFastSlow are the OpenList task poll intervals: fast for the first
// window (small files complete quickly), then relaxed.
const (
	jobPollFast     = 500 * time.Millisecond
	jobPollSlow     = 2 * time.Second
	jobPollFastFor  = 10 * time.Second
	jobLostTracking = 5 * time.Minute // task vanished from undone+done lists
)

// taskTypes are the OpenList task kinds the job engine tracks.
const (
	taskCopy     = "copy"
	taskMove     = "move"
	taskOffline  = "offline_download"
	taskUpload   = "upload"
	toolSimpleHT = "SimpleHttp"
)

// jobManager owns long-poll wakeup channels and the global runner semaphore.
type jobManager struct {
	mu      sync.Mutex
	waiters map[string][]chan struct{} // subject id (job id or request id) -> waiters

	sem chan struct{} // global runner slots
}

func newJobManager() *jobManager {
	return &jobManager{
		waiters: map[string][]chan struct{}{},
		sem:     make(chan struct{}, jobGlobalConcurrency),
	}
}

// watch registers a wakeup channel for id.
func (m *jobManager) watch(id string) chan struct{} {
	ch := make(chan struct{})
	m.mu.Lock()
	m.waiters[id] = append(m.waiters[id], ch)
	m.mu.Unlock()
	return ch
}

// unwatch removes a channel registered for id.
func (m *jobManager) unwatch(id string, ch chan struct{}) {
	m.mu.Lock()
	list := m.waiters[id]
	for i, c := range list {
		if c == ch {
			m.waiters[id] = append(list[:i], list[i+1:]...)
			break
		}
	}
	m.mu.Unlock()
}

// notify wakes every waiter of id (state change broadcast).
func (m *jobManager) notify(id string) {
	m.mu.Lock()
	list := m.waiters[id]
	delete(m.waiters, id)
	m.mu.Unlock()
	for _, ch := range list {
		close(ch)
	}
}

// acquire takes a global runner slot.
func (m *jobManager) acquire(ctx context.Context) bool {
	select {
	case m.sem <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

func (m *jobManager) release() { <-m.sem }

// ---- job rendering ----

// jobView is the agent-visible job object (result internals stripped).
func jobView(j store.Job) map[string]any {
	result := map[string]any{}
	if j.Result != "" {
		_ = json.Unmarshal([]byte(j.Result), &result)
	}
	delete(result, "task_ids")
	delete(result, "task_type")
	delete(result, "partial")
	var req any = map[string]any{}
	if j.Request != "" {
		var r map[string]any
		if json.Unmarshal([]byte(j.Request), &r) == nil {
			req = r
		}
	}
	return map[string]any{
		"id":         j.ID,
		"kind":       j.Kind,
		"state":      j.State,
		"progress":   j.Progress,
		"request":    req,
		"result":     result,
		"error":      j.Error,
		"created_at": j.CreatedAt.UTC().Format(time.RFC3339),
		"updated_at": j.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

// jobResult unmarshals the stored result JSON.
func jobResult(j store.Job) map[string]any {
	out := map[string]any{}
	if j.Result != "" {
		_ = json.Unmarshal([]byte(j.Result), &out)
	}
	return out
}

// jobState updates a job and wakes long-pollers.
func (s *Server) jobState(id, state string, progress float64, result map[string]any, errMsg string) {
	raw := "{}"
	if result != nil {
		if b, err := json.Marshal(result); err == nil {
			raw = string(b)
		}
	}
	if err := s.st.UpdateJobState(id, state, progress, raw, errMsg); err != nil {
		s.log.Warn("job update failed", "id", id, "err", err)
	}
	s.jobman.notify(id)
}

// setJobIfActive writes a non-terminal runner state only while the job row is
// still active (queued/running); a concurrent cancel is never overwritten.
func (s *Server) setJobIfActive(id, state string, progress float64, result map[string]any, errMsg string) {
	if fresh, err := s.st.GetJob(id); err == nil && store.JobTerminal(fresh.State) {
		return
	}
	s.jobState(id, state, progress, result, errMsg)
}

// jobStateIfNotCanceled updates the job unless a concurrent cancel already
// moved it to canceled (the runner's completion must not overwrite that).
func (s *Server) jobStateIfNotCanceled(id, state string, progress float64, result map[string]any, errMsg string) {
	if j, err := s.st.GetJob(id); err == nil && j.State == store.JobCanceled && state != store.JobCanceled {
		return
	}
	s.jobState(id, state, progress, result, errMsg)
}

// jobCanceled reports whether the job has been canceled meanwhile.
func (s *Server) jobCanceled(id string) bool {
	j, err := s.st.GetJob(id)
	return err == nil && j.State == store.JobCanceled
}

// StartJobs resumes tracking of jobs that were queued or running when
// cloud2api stopped (jobs survive restarts, §4.10.4).
func (s *Server) StartJobs(ctx context.Context) {
	jobs, err := s.st.ActiveJobs()
	if err != nil {
		s.log.Warn("job resume query failed", "err", err)
		return
	}
	for _, j := range jobs {
		job := j
		go s.runJob(ctx, job, true)
	}
	if len(jobs) > 0 {
		s.log.Info("resumed jobs", "count", len(jobs))
	}
}

// runJob executes a queued job (fresh or resumed) under the global semaphore.
func (s *Server) runJob(ctx context.Context, job store.Job, resumed bool) {
	if !s.jobman.acquire(ctx) {
		return
	}
	defer s.jobman.release()

	runCtx, cancel := context.WithTimeout(ctx, jobMaxDuration)
	defer cancel()

	// Reload the job: a cancel may have landed while queued.
	if s.jobCanceled(job.ID) {
		return
	}
	s.setJobIfActive(job.ID, store.JobRunning, 0, nil, "")

	var err error
	switch job.Kind {
	case store.JobKindTransfer:
		err = s.runTransferJob(runCtx, job, resumed)
	case store.JobKindFetch:
		err = s.runFetchJob(runCtx, job, resumed)
	case store.JobKindBatch:
		err = s.runBatchJob(runCtx, job, resumed)
	default:
		err = fmt.Errorf("unknown job kind %q", job.Kind)
	}
	if err == nil {
		return // the runner set the terminal state itself
	}
	if runCtx.Err() != nil || s.jobCanceled(job.ID) {
		s.jobStateIfNotCanceled(job.ID, store.JobCanceled, 0, nil, "canceled")
		return
	}
	s.jobStateIfNotCanceled(job.ID, store.JobFailed, 0, nil, err.Error())
}

// ---- OpenList task tracking ----

// olTask is the slice of OpenList's task model the engine needs. States were
// probed on the live instance: 0 pending, 1 running, 2 succeeded, 4 canceled
// ("context canceled"), 7 failed (e.g. "http status code 401"); 3/5/6 are the
// canceling/errored/failing intermediates and map onto running/failed.
type olTask struct {
	ID       string  `json:"id"`
	State    int     `json:"state"`
	Status   string  `json:"status"`
	Progress float64 `json:"progress"`
	Error    string  `json:"error"`
}

const (
	olTaskPending   = 0
	olTaskRunning   = 1
	olTaskSucceeded = 2
	olTaskCanceled  = 4
)

func olTaskTerminalState(t olTask) string {
	switch {
	case t.State == olTaskSucceeded:
		return store.JobSucceeded
	case t.State == olTaskCanceled:
		return store.JobCanceled
	case t.State >= 5:
		return store.JobFailed
	default:
		return store.JobRunning
	}
}

// taskList fetches OpenList's undone or done task list of one type.
func (s *Server) taskList(ctx context.Context, taskType string, done bool) ([]olTask, error) {
	path := "/api/task/" + taskType + "/undone"
	if done {
		path = "/api/task/" + taskType + "/done"
	}
	data, err := s.fsCall(ctx, "GET", path, nil)
	if err != nil {
		return nil, err
	}
	var tasks []olTask
	if err := json.Unmarshal(data, &tasks); err != nil {
		return nil, err
	}
	return tasks, nil
}

// taskCancel asks OpenList to cancel one task.
func (s *Server) taskCancel(ctx context.Context, taskType, tid string) error {
	_, err := s.fsCall(ctx, "POST", "/api/task/"+taskType+"/cancel?tid="+tid, nil)
	return err
}

// trackOpenListTasks polls the submitted OpenList tasks of a job to
// completion and finalizes the job state. Progress is mirrored into the job
// row so GET /v1/jobs/{id} shows live numbers between long-polls. It returns
// nil on success, an error whose message is "canceled" for cancellations, or
// the upstream failure text.
func (s *Server) trackOpenListTasks(ctx context.Context, job store.Job, taskType string, tids []string) error {
	start := time.Now()
	fastUntil := start.Add(jobPollFastFor)
	lastProgress := -1
	for {
		if ctx.Err() != nil {
			return errJobCanceled
		}
		if s.jobCanceled(job.ID) {
			cctx, cc := context.WithTimeout(context.Background(), 10*time.Second)
			for _, tid := range tids {
				_ = s.taskCancel(cctx, taskType, tid)
			}
			cc()
			return errJobCanceled
		}

		found := map[string]olTask{}
		for _, done := range []bool{false, true} {
			tasks, err := s.taskList(ctx, taskType, done)
			if err != nil {
				s.log.Warn("task poll failed", "job", job.ID, "err", err)
			}
			for _, t := range tasks {
				found[t.ID] = t
			}
		}

		aggState, progress, missing := store.JobRunning, 0.0, 0
		var firstErr string
		canceledCount := 0
		for _, tid := range tids {
			t, ok := found[tid]
			if !ok {
				missing++
				continue
			}
			aggState = worstTaskState(aggState, olTaskTerminalState(t))
			progress += t.Progress
			if olTaskTerminalState(t) == store.JobFailed && t.Error != "" && firstErr == "" {
				firstErr = t.Error
			}
			if olTaskTerminalState(t) == store.JobCanceled {
				canceledCount++
			}
		}
		known := len(tids) - missing
		if known > 0 {
			progress /= float64(known)
		}

		// Refresh progress at most every percent change; setJobIfActive
		// keeps a concurrent cancel from being overwritten.
		if int(progress) != lastProgress {
			lastProgress = int(progress)
			if job.ID != "" {
				if fresh, err := s.st.GetJob(job.ID); err == nil && fresh.State == store.JobRunning {
					res := jobResult(fresh)
					s.setJobIfActive(job.ID, store.JobRunning, progress, res, fresh.Error)
				}
			}
		}

		if missing == len(tids) && time.Since(start) > jobLostTracking {
			return fmt.Errorf("task tracking lost: OpenList no longer reports this task (restart?)")
		}
		if missing == 0 || (known > 0 && aggState != store.JobRunning) {
			// All tasks reached a terminal state (or every known one did and
			// at least one is not running).
			if aggState == store.JobFailed {
				msg := firstErr
				if msg == "" {
					msg = "upstream task failed"
				}
				return fmt.Errorf("%s", msg)
			}
			if aggState == store.JobCanceled || canceledCount > 0 {
				return errJobCanceled
			}
			if aggState == store.JobSucceeded {
				return nil
			}
		}

		interval := jobPollSlow
		if time.Now().Before(fastUntil) {
			interval = jobPollFast
		}
		select {
		case <-ctx.Done():
			return errJobCanceled
		case <-time.After(interval):
		}
	}
}

// worstTaskState merges task terminal states: failed > canceled > succeeded >
// running.
func worstTaskState(a, b string) string {
	rank := map[string]int{store.JobRunning: 0, store.JobSucceeded: 1, store.JobCanceled: 2, store.JobFailed: 3}
	if rank[b] > rank[a] {
		return b
	}
	return a
}

func marshalOr(v map[string]any, def string) string {
	if v == nil {
		return def
	}
	b, err := json.Marshal(v)
	if err != nil {
		return def
	}
	return string(b)
}

// rememberTasks persists the submitted OpenList task ids into the job result
// so a cloud2api restart can resume polling them.
func (s *Server) rememberTasks(jobID, taskType string, tids []string) map[string]any {
	j, err := s.st.GetJob(jobID)
	if err != nil {
		return map[string]any{"task_ids": tids, "task_type": taskType}
	}
	res := jobResult(j)
	res["task_ids"] = tids
	res["task_type"] = taskType
	s.setJobIfActive(jobID, store.JobRunning, 0, res, j.Error)
	return res
}
