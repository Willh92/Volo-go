package plugin

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	defaultMaxCronConcurrency = 4
	maxCronJobsPerPlugin      = 16
	minCronIntervalSeconds    = 5
	maxCronIntervalSeconds    = 365 * 86400

	CronSourceManifest = "manifest"
	CronSourceDynamic  = "dynamic"
)

// CronJobSnapshot is the read-only observability view of a scheduled plugin job in CronRegistry.
type CronJobSnapshot struct {
	PluginID        string `json:"plugin_id"`
	Name            string `json:"name"`
	Handler         string `json:"handler"`
	Source          string `json:"source"` // manifest | dynamic
	IntervalSeconds int    `json:"interval_seconds"`
	NextRunAt       int64  `json:"next_run_at"`
	LastRunAt       int64  `json:"last_run_at,omitempty"`
	RunCount        int64  `json:"run_count"`
	Running         bool   `json:"running"`
	LastError       string `json:"last_error,omitempty"`
}

type cronRunFunc func(parentCtx context.Context, pluginID string, jobName string, handler string) error

type cronEntry struct {
	seq            uint64
	pluginID       string
	name           string
	handler        string
	source         string
	intervalSec    int
	interval       time.Duration
	nextRun        time.Time
	lastRun        time.Time
	runCount       int64
	lastError      string
	running        bool
	pendingTrigger bool
	removed        bool
	heapIdx        int
}

type cronMinHeap []*cronEntry

func (h cronMinHeap) less(i, j int) bool {
	if !h[i].nextRun.Equal(h[j].nextRun) {
		return h[i].nextRun.Before(h[j].nextRun)
	}
	return h[i].seq < h[j].seq
}

func (h cronMinHeap) swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].heapIdx = i
	h[j].heapIdx = j
}

func (h cronMinHeap) up(j int) {
	for j > 0 {
		i := (j - 1) / 2
		if i == j || !h.less(j, i) {
			break
		}
		h.swap(i, j)
		j = i
	}
}

func (h cronMinHeap) down(i0, n int) bool {
	i := i0
	for {
		j1 := 2*i + 1
		if j1 >= n || j1 < 0 {
			break
		}
		j := j1
		if j2 := j1 + 1; j2 < n && h.less(j2, j1) {
			j = j2
		}
		if !h.less(j, i) {
			break
		}
		h.swap(i, j)
		i = j
	}
	return i > i0
}

func (h *cronMinHeap) push(ent *cronEntry) {
	ent.heapIdx = len(*h)
	*h = append(*h, ent)
	h.up(len(*h) - 1)
}

func (h *cronMinHeap) pop() *cronEntry {
	n := len(*h) - 1
	h.swap(0, n)
	h.down(0, n)
	ent := (*h)[n]
	(*h)[n] = nil
	ent.heapIdx = -1
	*h = (*h)[:n]
	return ent
}

func (h *cronMinHeap) fix(i int) {
	if !h.down(i, len(*h)) {
		h.up(i)
	}
}

func (h *cronMinHeap) remove(i int) {
	n := len(*h) - 1
	if n != i {
		h.swap(i, n)
		if !h.down(i, n) {
			h.up(i)
		}
	}
	ent := (*h)[n]
	(*h)[n] = nil
	ent.heapIdx = -1
	*h = (*h)[:n]
}

type pluginCronGroup struct {
	pluginID string
	ctx      context.Context
	cancel   context.CancelFunc
	jobs     map[string]*cronEntry
	wg       sync.WaitGroup
}

// CronRegistry is a unified min-heap scheduled task center shared across all plugins.
// Instead of spawning one goroutine + ticker per plugin job, it multiplexes all scheduled
// tasks onto a single lazy timer coordinator and a bounded worker pool with non-reentrant execution.
type CronRegistry struct {
	mu       sync.Mutex
	pq       cronMinHeap
	groups   map[string]*pluginCronGroup
	seqGen   uint64
	started  bool
	closed   bool
	wakeCh   chan struct{}
	stopCh   chan struct{}
	sem      chan struct{}
	coordWG  sync.WaitGroup
	workerWG sync.WaitGroup

	unitFn func() time.Duration
	runFn  cronRunFunc
}

func newCronRegistry(maxConcurrency int, unitFn func() time.Duration, runFn cronRunFunc) *CronRegistry {
	if maxConcurrency <= 0 {
		maxConcurrency = defaultMaxCronConcurrency
	}
	return &CronRegistry{
		groups: make(map[string]*pluginCronGroup),
		wakeCh: make(chan struct{}, 1),
		stopCh: make(chan struct{}),
		sem:    make(chan struct{}, maxConcurrency),
		unitFn: unitFn,
		runFn:  runFn,
	}
}

func (r *CronRegistry) baseUnit() time.Duration {
	if r.unitFn != nil {
		if u := r.unitFn(); u > 0 {
			return u
		}
	}
	return time.Second
}

// BeginPlugin opens (or resets) the active scheduling lifecycle group for pluginID.
func (r *CronRegistry) BeginPlugin(pluginID string) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	if prev, exists := r.groups[pluginID]; exists {
		r.detachGroupLocked(prev)
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.groups[pluginID] = &pluginCronGroup{
		pluginID: pluginID,
		ctx:      ctx,
		cancel:   cancel,
		jobs:     make(map[string]*cronEntry),
	}
	r.mu.Unlock()
}

// HasJob reports whether pluginID currently has a registered job with name.
func (r *CronRegistry) HasJob(pluginID string, name string) bool {
	cleanName := strings.TrimSpace(name)
	r.mu.Lock()
	defer r.mu.Unlock()
	grp, ok := r.groups[pluginID]
	if !ok {
		return false
	}
	_, exists := grp.jobs[cleanName]
	return exists
}

// RegisterJob registers or updates a periodic task in the unified min-heap scheduler.
func (r *CronRegistry) RegisterJob(pluginID string, spec CronSpec, source string, immediate bool) error {
	name := strings.TrimSpace(spec.Name)
	handler := strings.TrimSpace(spec.Handler)
	if name == "" || len(name) > 64 {
		return errors.New("cron: job name must be 1..64 characters")
	}
	if handler == "" || len(handler) > 128 {
		return errors.New("cron: handler function name must be 1..128 characters")
	}
	if spec.IntervalSeconds < minCronIntervalSeconds {
		return fmt.Errorf("cron: interval_seconds must be >= %d", minCronIntervalSeconds)
	}
	if spec.IntervalSeconds > maxCronIntervalSeconds {
		spec.IntervalSeconds = maxCronIntervalSeconds
	}
	if source == "" {
		source = CronSourceDynamic
	}

	interval := time.Duration(spec.IntervalSeconds) * r.baseUnit()
	if interval <= 0 {
		interval = time.Millisecond
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed {
		return errors.New("cron: scheduler is closed")
	}
	grp, ok := r.groups[pluginID]
	if !ok {
		return fmt.Errorf("cron: plugin %q runtime is not active", pluginID)
	}

	now := time.Now()
	nextRun := now.Add(interval)
	if immediate {
		nextRun = now
	}

	if existing, exists := grp.jobs[name]; exists {
		existing.handler = handler
		existing.source = source
		existing.intervalSec = spec.IntervalSeconds
		existing.interval = interval
		if existing.running {
			if immediate {
				existing.pendingTrigger = true
			}
		} else {
			existing.nextRun = nextRun
			if existing.heapIdx >= 0 {
				r.pq.fix(existing.heapIdx)
			} else {
				r.pq.push(existing)
			}
		}
		r.ensureStartedLocked()
		r.wakeLocked()
		return nil
	}

	if len(grp.jobs) >= maxCronJobsPerPlugin {
		return fmt.Errorf("cron: plugin %q exceeded maximum of %d scheduled jobs", pluginID, maxCronJobsPerPlugin)
	}

	r.seqGen++
	ent := &cronEntry{
		seq:         r.seqGen,
		pluginID:    pluginID,
		name:        name,
		handler:     handler,
		source:      source,
		intervalSec: spec.IntervalSeconds,
		interval:    interval,
		nextRun:     nextRun,
		heapIdx:     -1,
	}
	grp.jobs[name] = ent
	r.pq.push(ent)
	r.ensureStartedLocked()
	r.wakeLocked()
	return nil
}

// UnregisterJob removes a single scheduled task for pluginID.
func (r *CronRegistry) UnregisterJob(pluginID string, name string) bool {
	cleanName := strings.TrimSpace(name)
	if cleanName == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	grp, ok := r.groups[pluginID]
	if !ok {
		return false
	}
	ent, exists := grp.jobs[cleanName]
	if !exists {
		return false
	}
	ent.removed = true
	ent.pendingTrigger = false
	delete(grp.jobs, cleanName)
	if ent.heapIdx >= 0 {
		r.pq.remove(ent.heapIdx)
	}
	r.wakeLocked()
	return true
}

// Trigger wakes one job (or all jobs of pluginID when name == "") for immediate execution.
func (r *CronRegistry) Trigger(pluginID string, name string) int {
	cleanName := strings.TrimSpace(name)
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed {
		return 0
	}
	grp, ok := r.groups[pluginID]
	if !ok || len(grp.jobs) == 0 {
		return 0
	}

	now := time.Now()
	triggered := 0
	triggerOne := func(ent *cronEntry) {
		if ent == nil || ent.removed {
			return
		}
		triggered++
		if ent.running {
			ent.pendingTrigger = true
			return
		}
		ent.nextRun = now
		if ent.heapIdx >= 0 {
			r.pq.fix(ent.heapIdx)
		} else {
			r.pq.push(ent)
		}
	}

	if cleanName != "" {
		triggerOne(grp.jobs[cleanName])
	} else {
		for _, ent := range grp.jobs {
			triggerOne(ent)
		}
	}
	if triggered > 0 {
		r.ensureStartedLocked()
		r.wakeLocked()
	}
	return triggered
}

// UnregisterPlugin cancels and removes all scheduled tasks belonging to pluginID.
// Never pass wait=true from inside a cron worker goroutine of the same plugin!
func (r *CronRegistry) UnregisterPlugin(pluginID string, wait bool) {
	r.mu.Lock()
	grp, exists := r.groups[pluginID]
	if exists {
		delete(r.groups, pluginID)
		r.detachGroupLocked(grp)
		r.wakeLocked()
	}
	r.mu.Unlock()

	if exists && wait {
		grp.wg.Wait()
	}
}

func (r *CronRegistry) detachGroupLocked(grp *pluginCronGroup) {
	if grp == nil {
		return
	}
	grp.cancel()
	for name, ent := range grp.jobs {
		ent.removed = true
		ent.pendingTrigger = false
		if ent.heapIdx >= 0 {
			r.pq.remove(ent.heapIdx)
		}
		delete(grp.jobs, name)
	}
}

// ListJobs returns deterministic snapshots of active scheduled jobs (filtered by pluginID if non-empty).
func (r *CronRegistry) ListJobs(pluginID string) []CronJobSnapshot {
	filterID := strings.TrimSpace(pluginID)
	r.mu.Lock()
	var out []CronJobSnapshot
	for id, grp := range r.groups {
		if filterID != "" && id != filterID {
			continue
		}
		for _, ent := range grp.jobs {
			out = append(out, snapshotEntry(ent))
		}
	}
	r.mu.Unlock()

	sort.Slice(out, func(i, j int) bool {
		if out[i].PluginID != out[j].PluginID {
			return out[i].PluginID < out[j].PluginID
		}
		return out[i].Name < out[j].Name
	})
	if out == nil {
		return []CronJobSnapshot{}
	}
	return out
}

func snapshotEntry(ent *cronEntry) CronJobSnapshot {
	var lastRunMS, nextRunMS int64
	if !ent.lastRun.IsZero() {
		lastRunMS = ent.lastRun.UnixMilli()
	}
	if !ent.nextRun.IsZero() {
		nextRunMS = ent.nextRun.UnixMilli()
	}
	return CronJobSnapshot{
		PluginID:        ent.pluginID,
		Name:            ent.name,
		Handler:         ent.handler,
		Source:          ent.source,
		IntervalSeconds: ent.intervalSec,
		NextRunAt:       nextRunMS,
		LastRunAt:       lastRunMS,
		RunCount:        ent.runCount,
		Running:         ent.running,
		LastError:       ent.lastError,
	}
}

// Close shuts down the unified coordinator goroutine and waits for all in-flight jobs to finish.
func (r *CronRegistry) Close() {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	for id, grp := range r.groups {
		delete(r.groups, id)
		r.detachGroupLocked(grp)
	}
	wasStarted := r.started
	close(r.stopCh)
	r.mu.Unlock()

	if wasStarted {
		r.coordWG.Wait()
	}
	r.workerWG.Wait()
}

func (r *CronRegistry) ensureStartedLocked() {
	if r.started || r.closed {
		return
	}
	r.started = true
	r.coordWG.Add(1)
	go r.loop()
}

func (r *CronRegistry) wakeLocked() {
	select {
	case r.wakeCh <- struct{}{}:
	default:
	}
}

func (r *CronRegistry) loop() {
	defer r.coordWG.Done()

	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timerActive := false

	stopTimer := func() {
		if timerActive {
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timerActive = false
		}
	}

	for {
		r.mu.Lock()
		if r.closed {
			r.mu.Unlock()
			stopTimer()
			return
		}

		now := time.Now()
		for len(r.pq) > 0 && !r.pq[0].nextRun.After(now) {
			ent := r.pq.pop()
			grp, ok := r.groups[ent.pluginID]
			if !ok || ent.removed || grp.ctx.Err() != nil {
				continue
			}

			// Acquire worker slot without blocking the coordinator loop.
			select {
			case r.sem <- struct{}{}:
				ent.running = true
				ent.pendingTrigger = false
				grp.wg.Add(1)
				r.workerWG.Add(1)
				go r.executeEntry(grp, ent)
			default:
				// All worker slots busy; retry shortly without spinning.
				ent.nextRun = now.Add(5 * time.Millisecond)
				r.pq.push(ent)
				goto scheduleWait
			}
		}

	scheduleWait:
		hasNext := len(r.pq) > 0
		var waitDur time.Duration
		if hasNext {
			waitDur = max(0, time.Until(r.pq[0].nextRun))
		}
		r.mu.Unlock()

		stopTimer()
		var timerCh <-chan time.Time
		if hasNext {
			timer.Reset(waitDur)
			timerActive = true
			timerCh = timer.C
		}

		select {
		case <-r.stopCh:
			stopTimer()
			return
		case <-r.wakeCh:
		case <-timerCh:
			timerActive = false
		}
	}
}

func (r *CronRegistry) executeEntry(grp *pluginCronGroup, ent *cronEntry) {
	defer func() {
		<-r.sem
		grp.wg.Done()
		r.workerWG.Done()
	}()

	var runErr error
	if r.runFn != nil && grp.ctx.Err() == nil {
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					runErr = fmt.Errorf("cron panic: %v", rec)
				}
			}()
			runErr = r.runFn(grp.ctx, ent.pluginID, ent.name, ent.handler)
		}()
	}

	r.mu.Lock()
	finNow := time.Now()
	ent.running = false
	ent.lastRun = finNow
	ent.runCount++
	if runErr != nil && !errors.Is(runErr, context.Canceled) {
		ent.lastError = runErr.Error()
	} else if runErr == nil {
		ent.lastError = ""
	}

	if !r.closed && !ent.removed && r.groups[ent.pluginID] == grp && grp.ctx.Err() == nil {
		if ent.pendingTrigger {
			ent.pendingTrigger = false
			ent.nextRun = finNow
		} else {
			ent.nextRun = finNow.Add(ent.interval)
		}
		r.pq.push(ent)
		r.wakeLocked()
	}
	r.mu.Unlock()
}
