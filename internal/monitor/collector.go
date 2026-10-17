package monitor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
	"github.com/asheshgoplani/agent-deck/internal/statedb"
	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

// matrix/hardened: each profile retains its owning DB and original Instances;
// data_version changes do not reset every pane's sampling/throttle state.
// ProfileRegistry retains one profile's owning storage and change observer.
type ProfileRegistry interface {
	Load() ([]*session.Instance, error)
	Changed() (bool, error)
	Close() error
}
type registry struct {
	storage  *session.Storage
	observer *statedb.RegistryObserver
	version  int64
	epoch    uint64
	touch    int64
}

func openRegistry(profile string) (ProfileRegistry, error) {
	storage, err := session.NewStorageWithProfile(profile)
	if err != nil {
		return nil, err
	}
	observer, err := storage.GetDB().NewRegistryObserver()
	if err != nil {
		_ = storage.Close()
		return nil, err
	}
	return &registry{storage: storage, observer: observer}, nil
}
func (r *registry) Changed() (bool, error) {
	v, e, err := r.observer.Probe()
	if err != nil {
		return false, err
	}
	return v != r.version || e != r.epoch, nil
}
func (r *registry) Load() ([]*session.Instance, error) {
	touch, err := r.storage.GetDB().LastModified()
	if err != nil {
		return nil, err
	}
	before, epoch, err := r.observer.Probe()
	if err != nil {
		return nil, err
	}
	instances, _, _, err := r.storage.LoadWithGroupsSnapshot()
	if err != nil {
		return nil, err
	}
	after, endEpoch, err := r.observer.Probe()
	if err != nil {
		return nil, err
	}
	afterTouch, err := r.storage.GetDB().LastModified()
	if err != nil {
		return nil, err
	}
	// A racing writer forces another conservative reload next pass.
	if before == after && epoch == endEpoch && touch == afterTouch {
		r.version, r.epoch = after, endEpoch
	} else {
		r.version, r.epoch = 0, 0
	}
	r.touch = touch
	return instances, nil
}
func (r *registry) Fence() int64 { return r.touch }
func (r *registry) PersistStatus(id, expected, status, tool string, touch int64) (bool, error) {
	return r.storage.GetDB().WriteObservedStatusIfCurrent(id, expected, status, tool, touch)
}
func (r *registry) Acknowledgments() (map[string]statedb.StatusRow, error) {
	return r.storage.GetDB().ReadAllStatuses()
}
func (r *registry) PersistAcknowledged(id, status string, ack bool, touch, ackAt int64) (bool, error) {
	return r.storage.GetDB().WriteObservedAcknowledged(id, status, ack, touch, ackAt)
}
func (r *registry) Close() error {
	a := r.observer.Close()
	b := r.storage.Close()
	if a != nil {
		return a
	}
	return b
}

type observedInstance struct {
	instance       *session.Instance
	identity       string
	row            session.RuntimeSessionSnapshot
	inflight       bool
	done           chan session.RuntimeSessionSnapshot
	expectedStatus string
	expectedTool   string
	expectedTouch  int64
	sharedAck      bool
	sharedAckKnown bool
	expectedAckAt  int64
	// matrix/hardened: immutable routing copied before any probe can mutate inst.
	queryTarget *tmux.Session
}
type profileState struct {
	mu           sync.Mutex
	name, epoch  string
	demandUntil  time.Time
	wakeFence    time.Time
	lastPass     time.Time
	refresh      bool
	eventRefresh bool
	revision     uint64
	latest       *publication
	history      []*publication
	historyBytes int
	changed      chan struct{}
	registry     ProfileRegistry
	records      map[string]*observedInstance
	retired      []*observedInstance
	order        []string
	rotation     int
	sse          *session.OpenCodeSSEWatcher
}

func newProfile(name, epoch string) *profileState {
	return &profileState{name: name, epoch: epoch, records: make(map[string]*observedInstance), changed: make(chan struct{})}
}
func (p *profileState) wait(ctx context.Context, after uint64) error {
	for {
		p.mu.Lock()
		ready := p.latest != nil && p.revision > after && !p.latest.snapshot.ObservedAt.Before(p.wakeFence)
		changed := p.changed
		p.mu.Unlock()
		if ready {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
}
func (p *profileState) waitFresh(ctx context.Context, at time.Time) error {
	for {
		p.mu.Lock()
		ready := p.latest != nil && !p.latest.snapshot.ObservedAt.Before(at)
		changed := p.changed
		p.mu.Unlock()
		if ready {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
}
func (p *profileState) observations(epoch string, after uint64) ([]byte, *GapError, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.history) == 0 {
		return nil, nil, fmt.Errorf("monitor has no observation")
	}
	oldest := p.history[0].snapshot.Revision
	if (epoch != "" && epoch != p.epoch) || (epoch != "" && after+1 < oldest) || after > p.revision {
		return nil, &GapError{Epoch: p.epoch, Oldest: oldest, Latest: p.revision}, nil
	}
	// Empty cursor establishes a baseline, never fabricates old completion edges.
	if epoch == "" && after == 0 {
		return append(append([]byte("["), p.latest.encoded...), ']'), nil, nil
	}
	var body bytes.Buffer
	body.WriteByte('[')
	first := true
	for _, pub := range p.history {
		if pub.snapshot.Revision <= after {
			continue
		}
		overhead := 1 // closing array bracket
		if !first {
			overhead++
		} // separator before all but the first publication
		if body.Len()+len(pub.encoded)+overhead > maxResponseBytes {
			break
		}
		if !first {
			body.WriteByte(',')
		}
		first = false
		body.Write(pub.encoded)
	}
	body.WriteByte(']')
	return body.Bytes(), nil, nil
}

func (s *Server) reconcile(p *profileState) error {
	if p.registry == nil {
		reg, err := s.opts.LoadProfile(p.name)
		if err != nil {
			return err
		}
		p.registry = reg
	}
	changed, err := p.registry.Changed()
	if err != nil {
		return err
	}
	if !changed && len(p.order) > 0 {
		return nil
	}
	instances, err := p.registry.Load()
	if err != nil {
		return err
	}
	next := make(map[string]*observedInstance, len(instances))
	order := make([]string, 0, len(instances))
	for _, inst := range instances {
		if inst == nil {
			continue
		}
		id := inst.ID
		identity := inst.RuntimeIdentity()
		old := p.records[id]
		if old != nil && ((old.inflight && old.identity == identity) || (!old.inflight && old.instance.ReconcileRuntimeRegistry(inst))) {
			old.identity = identity
			next[id] = old
		} else {
			inst.SetRuntimeProducer(true)
			next[id] = &observedInstance{instance: inst, identity: identity, expectedStatus: string(inst.GetStatusThreadSafe()), row: session.RuntimeSessionSnapshot{ID: id, Profile: p.name, Epoch: p.epoch, Identity: identity, Status: inst.GetStatusThreadSafe(), Error: "runtime observation pending"}}
			if old != nil && old.inflight {
				p.retired = append(p.retired, old)
			}
		}
		// matrix/hardened: registry rows are fresh and not probed concurrently.
		// Copy only immutable routing, never a tmux Session mutex/cache.
		next[id].queryTarget = nil
		if !inst.IsArchived() {
			if target := inst.GetTmuxSession(); target != nil {
				next[id].queryTarget = &tmux.Session{Name: target.Name, SocketName: target.SocketName}
			}
		}
		if !next[id].inflight {
			next[id].expectedStatus = string(inst.GetStatusThreadSafe())
			next[id].expectedTool = inst.GetToolThreadSafe()
		}
		order = append(order, id)
	}
	for id, old := range p.records {
		if next[id] == nil && old.inflight {
			p.retired = append(p.retired, old)
		}
	}
	p.records, p.order = next, order
	return nil
}

func (s *Server) hooksWatcher() *session.StatusFileWatcher {
	s.hookMu.Lock()
	defer s.hookMu.Unlock()
	if s.hooks != nil {
		return s.hooks
	}
	w, err := session.NewStatusFileWatcher(s.forceActive)
	if err != nil {
		return nil
	}
	s.hooks = w
	done := make(chan struct{})
	s.hookDone = done
	go func() { defer close(done); w.Start() }()
	return w
}

func liveStatus(status session.Status) bool {
	return status == session.StatusRunning || status == session.StatusWaiting || status == session.StatusIdle
}
func paneAlive(inst *session.Instance) bool {
	sess := inst.GetTmuxSession()
	if sess == nil {
		return false
	}
	if sess.SocketName == tmux.DefaultSocketName() {
		if _, at, ok := tmux.GetCachedPaneInfoSnapshot(sess.Name); ok && time.Since(at) <= 5*time.Second {
			return true
		}
	}
	alive, err := sess.ProbeExists()
	return err == nil && alive
}

func (s *Server) probe(p *profileState, record *observedInstance, pass *session.StatusUpdatePass, at time.Time, hooks *session.StatusFileWatcher) {
	defer func() { <-s.workers }()
	var row session.RuntimeSessionSnapshot
	func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				row = record.row
				row.Error = fmt.Sprintf("runtime probe panic: %v", recovered)
			}
		}()
		if s.opts.Probe != nil {
			row = s.opts.Probe(record.instance, pass, at)
			return
		}
		inst := record.instance
		var hook *session.HookStatus
		if hooks != nil {
			hook = hooks.GetHookStatus(inst.ID)
			if hook != nil && !inst.RuntimeHookCompatible(hook) {
				hook = nil
			}
			if hook != nil && liveStatus(inst.GetStatusThreadSafe()) && paneAlive(inst) {
				inst.UpdateHookStatus(hook)
			}
		}
		if p.sse != nil {
			if ss := p.sse.GetStatus(inst.ID); ss != nil {
				inst.UpdateOpenCodeSSEStatus(ss.Status, ss.UpdatedAt)
			}
		}
		if record.row.ObservedAt.IsZero() && inst.GetToolThreadSafe() == "claude" && hook == nil {
			inst.ForceNextStatusCheck()
		}
		var err error
		if !inst.IsArchived() && inst.GetStatusThreadSafe() != session.StatusStopped {
			err = pass.UpdateStatus(inst)
		}
		row = inst.RuntimeSnapshot(p.name, p.epoch, 0, at)
		if row.Status == session.StatusRunning {
			row.Acknowledged = false
		}
		if err != nil {
			row.Error = err.Error()
		}
		if hook != nil && ((!liveStatus(row.Status)) || (row.Evidence.HookAt.Equal(hook.UpdatedAt) && row.Evidence.HookStatus == hook.Status && row.Evidence.HookEvent == hook.Event)) {
			copy := *hook
			row.Hook = &copy
			row.HookKnown = true
		}
	}()
	row.ID = record.instance.ID
	row.Profile = p.name
	row.Epoch = p.epoch
	row.ObservedAt = at
	if row.Identity == "" {
		row.Identity = record.identity
	}
	record.done <- row
}

func (s *Server) finish(p *profileState, record *observedInstance) bool {
	if !record.inflight {
		return false
	}
	select {
	case row := <-record.done:
		record.inflight = false
		if p.records[record.instance.ID] == record {
			if writer, ok := p.registry.(interface {
				PersistStatus(string, string, string, string, int64) (bool, error)
			}); ok && row.Error == "" && (string(row.Status) != record.expectedStatus || record.instance.GetToolThreadSafe() != record.expectedTool) {
				applied, err := writer.PersistStatus(row.ID, record.expectedStatus, string(row.Status), record.instance.GetToolThreadSafe(), record.expectedTouch)
				if err != nil {
					row.Error = err.Error()
				} else if !applied {
					row.Error = "runtime registry changed during status observation"
				}
			}
			if writer, ok := p.registry.(interface {
				PersistAcknowledged(string, string, bool, int64, int64) (bool, error)
			}); ok && row.Error == "" && (!record.sharedAckKnown || row.Acknowledged != record.sharedAck) {
				applied, err := writer.PersistAcknowledged(row.ID, string(row.Status), row.Acknowledged, record.expectedTouch, record.expectedAckAt)
				if err != nil {
					row.Error = err.Error()
				} else if !applied {
					row.Error = "runtime registry changed during acknowledgement observation"
				} else {
					record.sharedAck = row.Acknowledged
					record.sharedAckKnown = true
				}
			}
			record.row = row
			record.identity = row.Identity
		}
		return true
	default:
		return false
	}
}

// matrix/hardened: only already-reserved workers enter a small capture group.
// Probes retain their original classification and per-reservation observation
// time; failed/expired prefetches fall through to their individual capture path.
type matrixProbeLaunch struct {
	record *observedInstance
	at     time.Time
}

func (s *Server) launchProbeGroup(p *profileState, group []matrixProbeLaunch, pass *session.StatusUpdatePass, hooks *session.StatusFileWatcher) {
	if len(group) == 0 {
		return
	}
	if len(group) == 1 || s.opts.Probe != nil {
		for _, launch := range group {
			// A singleton still represents a socket capture dispatch, even
			// though no native prefetch command can combine it with a neighbor.
			inst := launch.record.instance
			if s.opts.Probe == nil && !inst.IsArchived() && inst.GetStatusThreadSafe() != session.StatusStopped && inst.GetTmuxSession() != nil {
				matrixRecordSocketBatch(1)
			}
			go s.probe(p, launch.record, pass, launch.at, hooks)
		}
		return
	}
	// A stalled server must not hold already-reserved probes on another socket.
	// Non-capture probes also start immediately, preserving stopped/archive paths.
	var partitions [][]matrixProbeLaunch
	var sockets []string
	for _, launch := range group {
		inst := launch.record.instance
		pane := inst.GetTmuxSession()
		if inst.IsArchived() || inst.GetStatusThreadSafe() == session.StatusStopped || pane == nil {
			go s.probe(p, launch.record, pass, launch.at, hooks)
			continue
		}
		socket := strings.TrimSpace(pane.SocketName)
		index := -1
		for i, existing := range sockets {
			if existing == socket {
				index = i
				break
			}
		}
		if index < 0 {
			sockets = append(sockets, socket)
			partitions = append(partitions, []matrixProbeLaunch{launch})
		} else {
			partitions[index] = append(partitions[index], launch)
		}
	}
	for _, partition := range partitions {
		s.launchCaptureProbeGroup(p, partition, pass, hooks)
	}
}

func (s *Server) launchCaptureProbeGroup(p *profileState, group []matrixProbeLaunch, pass *session.StatusUpdatePass, hooks *session.StatusFileWatcher) {
	matrixRecordSocketBatch(len(group))
	if len(group) == 1 {
		launch := group[0]
		go s.probe(p, launch.record, pass, launch.at, hooks)
		return
	}
	go func() {
		func() {
			// The ordinary probe has its own panic-to-error boundary. An optional
			// prefetch cannot leak all of this group's reserved worker slots.
			defer func() { _ = recover() }()
			var panes []*tmux.Session
			for _, launch := range group {
				inst := launch.record.instance
				if !inst.IsArchived() && inst.GetStatusThreadSafe() != session.StatusStopped {
					panes = append(panes, inst.GetTmuxSession())
				}
			}
			tmux.PrefetchCapturePanes(s.ctx, panes)
		}()
		for _, launch := range group {
			go s.probe(p, launch.record, pass, launch.at, hooks)
		}
	}()
}

func (s *Server) collect(p *profileState) {
	started := time.Now().UTC()
	s.loads.Add(1)
	if err := s.reconcile(p); err != nil {
		s.publish(p, started, err)
		return
	}
	for _, record := range p.records {
		s.finish(p, record)
	}
	retired := p.retired[:0]
	for _, record := range p.retired {
		s.finish(p, record)
		if record.inflight {
			retired = append(retired, record)
		}
	}
	p.retired = retired
	if source, ok := p.registry.(interface {
		Acknowledgments() (map[string]statedb.StatusRow, error)
	}); ok {
		acks, err := source.Acknowledgments()
		if err != nil {
			s.publish(p, started, err)
			return
		}
		for id, record := range p.records {
			if record.inflight {
				continue
			}
			if ack, known := acks[id]; known && (!record.sharedAckKnown || ack.Acknowledged != record.sharedAck || ack.AcknowledgedAt != record.expectedAckAt) {
				record.sharedAck, record.sharedAckKnown, record.expectedAckAt = ack.Acknowledged, true, ack.AcknowledgedAt
				var at time.Time
				if record.expectedAckAt != 0 {
					at = time.Unix(0, record.expectedAckAt)
				}
				record.instance.SetRuntimeProducerAcknowledgment(record.sharedAck, at)
			}
		}
	}
	var hooks *session.StatusFileWatcher
	if s.opts.Probe == nil {
		// matrix/hardened: stable registry order prevents anchor churn between
		// passes. Detached routing remains safe while older probes finish.
		if s.opts.QueryTransport != nil {
			targets := make([]*tmux.Session, 0, len(p.order))
			for _, id := range p.order {
				record := p.records[id]
				if record.queryTarget != nil && record.instance.GetStatusThreadSafe() != session.StatusStopped {
					targets = append(targets, record.queryTarget)
				}
			}
			s.opts.QueryTransport.RegisterTargets(p.name, targets)
		}
		hooks = s.hooksWatcher()
		tmux.RefreshExistingSessions()
		tmux.RefreshPaneInfoCache()
		if p.sse == nil {
			p.sse = session.NewOpenCodeSSEWatcher(s.forceActive)
		}
		var targets []session.SSETarget
		for _, record := range p.records {
			if record.inflight {
				continue
			}
			inst := record.instance
			if inst.GetToolThreadSafe() == "opencode" && inst.GetStatusThreadSafe() != session.StatusStopped {
				if port := inst.GetOpenCodePort(); port > 0 {
					targets = append(targets, session.SSETarget{InstanceID: inst.ID, Port: port})
				}
			}
		}
		p.sse.Sync(targets)
	}
	pass := new(session.StatusUpdatePass)
	budget, cancel := context.WithTimeout(s.ctx, s.opts.PassBudget)
	defer cancel()
	// Retired generations for an ID continue occupying its probe slot. Even a
	// restart cannot multiply blocked legacy calls until the old call returns.
	blocked := make(map[string]bool)
	for _, record := range p.retired {
		if record.inflight {
			blocked[record.instance.ID] = true
		}
	}
	remaining := len(p.order)
	index := 0
	if remaining > 0 {
		index = p.rotation % remaining
	}
	// matrix/hardened: adjacent worker completions may join one pending group
	// within 3ms of its first reservation. Never wait behind its own full pool;
	// original probes remain the sole releasers of every reserved worker slot.
	var group []matrixProbeLaunch
	scheduler := matrixBatchScheduler{ctx: s.ctx, budget: budget, workers: s.workers, window: matrixCaptureCoalesceWindow}
	flush := func(reason matrixBatchFlushReason) {
		matrixRecordLogicalBatch(len(group), reason)
		s.launchProbeGroup(p, group, pass, hooks)
		group = nil
		scheduler.groupFlushed()
	}
	for remaining > 0 && s.ctx.Err() == nil && budget.Err() == nil {
		record := p.records[p.order[index]]
		index = (index + 1) % len(p.order)
		remaining--
		if record == nil || record.inflight || blocked[record.instance.ID] || (record.instance.IsArchived() && !record.row.ObservedAt.IsZero()) {
			continue
		}
		inst := record.instance
		nonCapture := s.opts.Probe != nil || inst.IsArchived() || inst.GetStatusThreadSafe() == session.StatusStopped || inst.GetTmuxSession() == nil
		if nonCapture && len(group) != 0 {
			// Non-capture demand cannot benefit from holding a capture group.
			// Start its pending neighbors before the ordinary worker wait.
			flush(matrixBatchNonCapture)
		}
		reserved, reason := scheduler.reserve(len(group))
		if reason != matrixBatchNoFlush {
			flush(reason)
			if reason == matrixBatchCancel || reason == matrixBatchBudget {
				remaining = 0
			} else {
				reserved, _ = scheduler.reserve(0)
				if !reserved {
					remaining = 0
				}
			}
		}
		if reserved {
			if source, ok := p.registry.(interface{ Fence() int64 }); ok {
				record.expectedTouch = source.Fence()
				record.instance.SetRuntimeProducerFence(record.expectedTouch)
			}
			record.inflight = true
			record.done = make(chan session.RuntimeSessionSnapshot, 1)
			observedAt := time.Now().UTC()
			record.instance.SetRuntimeProducerObservedAt(observedAt)
			// Non-capture paths have no batch work to combine and must not spend
			// their slots on a capture admission window (including stopped/nil).
			if nonCapture {
				reason := matrixBatchNonCapture
				if s.opts.Probe != nil {
					reason = matrixBatchOverride
				}
				matrixRecordLogicalBatch(1, reason)
				go s.probe(p, record, pass, observedAt, hooks)
				continue
			}
			if len(group) == 0 {
				scheduler.groupStarted(time.Now())
			}
			group = append(group, matrixProbeLaunch{record, observedAt})
			if len(group) == tmux.CapturePaneBatchLimit {
				flush(matrixBatchFull)
			}
		}
	}
	reason := scheduler.interrupted()
	if reason == matrixBatchNoFlush {
		reason = matrixBatchEnd
	}
	flush(reason)
	p.rotation = index
	// Collect completed calls within the same bounded pass, without blocking
	// the sampler behind one pane. Fast panes remain publishable beside a hang.
	for {
		pending := false
		for _, record := range p.records {
			s.finish(p, record)
			if record.inflight {
				pending = true
			}
		}
		if !pending || s.ctx.Err() != nil {
			break
		}
		select {
		case <-budget.Done():
			goto finished
		case <-s.ctx.Done():
			goto finished
		case <-time.After(5 * time.Millisecond):
		}
	}
finished:
	// A registry edit during any probe retires its generation before publication.
	// Native-ID/activity writes by this sampler reconcile against its own object.
	err := s.reconcile(p)
	s.publish(p, started, err)
}

func (s *Server) publish(p *profileState, at time.Time, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	next := p.revision + 1
	snapshot := session.RuntimeProfileSnapshot{Protocol: Protocol, Epoch: p.epoch, Revision: next, Profile: p.name, ObservedAt: at}
	if err != nil {
		snapshot.Error = err.Error()
		s.errors.Add(1)
	}
	for _, id := range p.order {
		record := p.records[id]
		row := record.row.Clone()
		row.Revision = next
		row.Profile = p.name
		row.Epoch = p.epoch
		if row.ObservedAt.IsZero() {
			row.ObservedAt = at
			row.Error = "runtime observation has not completed"
		}
		if record.inflight {
			row.Error = "runtime probe still in flight; last observation retained"
		}
		if row.Identity != record.identity {
			row.Error = "runtime launch identity changed; observation pending"
		}
		if at.Sub(row.ObservedAt) > 10*time.Second && row.Error == "" {
			row.Error = "runtime observation is stale"
		}
		snapshot.Sessions = append(snapshot.Sessions, row)
	}
	encoded, encodeErr := json.Marshal(snapshot)
	if encodeErr == nil && len(encoded) > maxResponseBytes-2 {
		encodeErr = fmt.Errorf("monitor snapshot exceeds response limit")
	}
	if encodeErr != nil {
		snapshot.Error = encodeErr.Error()
		snapshot.Sessions = nil
		encoded, _ = json.Marshal(snapshot)
		s.errors.Add(1)
	}
	pub := &publication{snapshot: snapshot, encoded: encoded}
	p.latest = pub
	p.revision = next
	p.lastPass = time.Now()
	p.history = append(p.history, pub)
	p.historyBytes += len(encoded)
	for len(p.history) > s.opts.HistoryLimit || (p.historyBytes > maxHistoryBytes && len(p.history) > 1) {
		p.historyBytes -= len(p.history[0].encoded)
		p.history[0] = nil
		p.history = p.history[1:]
	}
	close(p.changed)
	p.changed = make(chan struct{})
	s.publications.Add(1)
}

func (s *Server) retireProfiles() {
	s.mu.Lock()
	profiles := make([]*profileState, 0, len(s.profiles))
	for _, p := range s.profiles {
		profiles = append(profiles, p)
	}
	s.mu.Unlock()
	for _, p := range profiles {
		if p.sse != nil {
			p.sse.Stop()
		}
		var pending []*observedInstance
		for _, r := range p.records {
			if r.inflight {
				pending = append(pending, r)
			}
		}
		for _, r := range p.retired {
			if r.inflight {
				pending = append(pending, r)
			}
		}
		reg := p.registry
		if reg == nil {
			continue
		}
		if len(pending) == 0 {
			_ = reg.Close()
			continue
		}
		// No DB close under a still-running legacy probe. This goroutine is bounded
		// by the number of occupied worker slots and exits naturally with them.
		go func() {
			for _, r := range pending {
				<-r.done
			}
			_ = reg.Close()
		}()
	}
}

// Stable order makes tests and operational readings independent of map order.
func sortedProfiles(m map[string]*profileState) []*profileState {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]*profileState, 0, len(keys))
	for _, k := range keys {
		out = append(out, m[k])
	}
	return out
}
