package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"
)

// matrix/hardened: one owned, immutable publication feeds REST, SSE and push.
// Encoded menu data/fingerprints are computed once, before per-client fanout.
type menuPublication struct {
	Snapshot    *MenuSnapshot
	Err         error
	Fingerprint string
	encoded     []byte
	// matrix/hardened: immutable transport/derived payloads are shared by all
	// consumers of this generation, independent of menu fingerprint equality.
	dashboardMenu []byte
	// matrix/hardened: opt-in dashboard clients share one versioned full value
	// and at most one smaller delta from the previous different content version.
	versionedMenu    []byte
	menuDelta        []byte
	deltaBaseVersion string
	commandCenter    *commandCenterDerived
}

type menuPublisherSubscriber struct {
	interval time.Duration
	observe  func(*MenuSnapshot)
	// matrix/hardened: DTO consumers keep private deep copies; transports use
	// only immutable encoding/derived state and never receive mutable menu DTOs.
	privateDTO bool
}

// matrix/hardened: construction uses a fixed production default; stream
// subscriptions supply their cadence independently (including test overrides).
const defaultMenuPublisherInterval = 2 * time.Second

// matrix/hardened: a single demand-driven collector replaces each browser's
// independent menu polling loop. No subscriber means no running poll loop.
type menuSnapshotPublisher struct {
	ctx    context.Context
	cancel context.CancelFunc
	loader func() (*MenuSnapshot, error)
	ttl    time.Duration
	wake   chan struct{}

	mu      sync.Mutex
	current menuPublication
	// matrix/hardened: successful content survives equal polls and source errors;
	// only one prior content DTO is retained, without old CC/artifact owners.
	content         menuDeltaContent
	previousContent menuDeltaContent
	at              time.Time
	generation      uint64
	revision        uint64
	refreshes       uint64
	errors          uint64
	dirty           bool
	closed          bool
	running         bool
	loading         chan struct{}
	subscribers     map[chan menuPublication]menuPublisherSubscriber
}

// matrix/hardened: construction is lazy and creates no background collector.
func newMenuSnapshotPublisher(ctx context.Context, loader func() (*MenuSnapshot, error), interval time.Duration) *menuSnapshotPublisher {
	if interval <= 0 {
		interval = defaultMenuPublisherInterval
	}
	ctx, cancel := context.WithCancel(ctx)
	return &menuSnapshotPublisher{
		ctx: ctx, cancel: cancel, loader: loader, ttl: interval,
		wake: make(chan struct{}, 1), dirty: true,
		subscribers: make(map[chan menuPublication]menuPublisherSubscriber),
	}
}

// matrix/hardened: decode the owned wire value rather than sharing nested maps,
// slices, sandbox settings or analytics pointers with readers or source DTOs.
func (v menuPublication) clone() menuPublication {
	if v.Snapshot != nil {
		var snapshot MenuSnapshot
		if err := json.Unmarshal(v.encoded, &snapshot); err != nil {
			return menuPublication{Err: err}
		}
		v.Snapshot = &snapshot
	}
	return v
}

// matrix/hardened: transport publications share owned immutable wire/derived
// data without decoding a menu DTO for each connected browser or push worker.
func (v menuPublication) wire() menuPublication {
	v.Snapshot = nil
	return v
}

func (p *menuSnapshotPublisher) intervalLocked() time.Duration {
	interval := p.ttl
	for _, sub := range p.subscribers {
		if sub.interval > 0 && sub.interval < interval {
			interval = sub.interval
		}
	}
	return interval
}

// matrix/hardened: all callers coalesce both successful and failed loads. A
// failed refresh is visible as an error, never a successful stale publication.
func (p *menuSnapshotPublisher) LoadMenuSnapshot() (*MenuSnapshot, error) {
	v, err := p.load(p.ctx)
	if err != nil {
		return nil, err
	}
	return v.Snapshot, v.Err
}

func (p *menuSnapshotPublisher) load(ctx context.Context) (menuPublication, error) {
	v, err := p.loadOwned(ctx)
	if err != nil {
		return menuPublication{}, err
	}
	return v.clone(), nil
}

// matrix/hardened: raw HTTP menu output can reuse encoding without exposing a
// mutable DTO. Default load/LoadMenuSnapshot still return deep private copies.
func (p *menuSnapshotPublisher) loadWire(ctx context.Context) (menuPublication, error) {
	v, err := p.loadOwned(ctx)
	return v.wire(), err
}

// matrix/hardened: internal collectors/derived builders only read the owned
// immutable generation. Never return this DTO from a transport or public load.
func (p *menuSnapshotPublisher) loadOwned(ctx context.Context) (menuPublication, error) {
	for {
		p.mu.Lock()
		if p.closed || p.ctx.Err() != nil {
			p.mu.Unlock()
			return menuPublication{}, context.Canceled
		}
		if !p.dirty && !p.at.IsZero() && time.Since(p.at) < p.intervalLocked() {
			v := p.current
			p.mu.Unlock()
			return v, nil
		}
		if p.loading == nil {
			p.loading = make(chan struct{})
			p.refreshes++
			go p.collect(p.loading, p.generation)
		}
		ready := p.loading
		p.mu.Unlock()
		select {
		case <-ctx.Done():
			return menuPublication{}, ctx.Err()
		case <-p.ctx.Done():
			return menuPublication{}, p.ctx.Err()
		case <-ready:
		}
	}
}

// matrix/hardened: source collection never holds the publisher mutex. Its
// generation must still match after mutations before any result can publish.
func (p *menuSnapshotPublisher) collect(ready chan struct{}, generation uint64) {
	// matrix/hardened: capture immutable content owners briefly; diff/encoding
	// stay outside the lock and the original generation fence still admits them.
	p.mu.Lock()
	content, previousContent := p.content, p.previousContent
	p.mu.Unlock()
	snapshot, err := p.loader()
	v := menuPublication{Err: err}
	if err == nil {
		if snapshot == nil {
			v.Err = errors.New("menu snapshot is unavailable")
		} else if v.encoded, err = json.Marshal(snapshot); err != nil {
			v.Err = err
		} else {
			v.Snapshot = snapshot
			v = v.clone()
			v.Fingerprint = menuSnapshotFingerprint(v.Snapshot)
			// matrix/hardened: encode the typed WS menu envelope once. Connection
			// writers use these owned bytes directly without rescanning RawMessage.
			v.dashboardMenu = append([]byte(`{"type":"menu","data":`), v.encoded...)
			v.dashboardMenu = append(v.dashboardMenu, '}')
			base := content
			if v.Fingerprint == content.version {
				base = previousContent
			}
			v = withVersionedMenu(base.snapshot, base.version, v)
			v.commandCenter = &commandCenterDerived{}
		}
	}
	p.mu.Lock()
	if !p.closed && p.ctx.Err() == nil && generation == p.generation {
		if v.Err == nil {
			if v.Fingerprint != p.content.version {
				p.previousContent = p.content
			}
			p.content = menuDeltaContent{snapshot: v.Snapshot, version: v.Fingerprint}
		}
		p.current, p.at, p.dirty = v, time.Now(), false
		p.revision++
		if v.Err != nil {
			p.errors++
		}
		for ch, sub := range p.subscribers {
			if v.Err == nil && sub.observe != nil {
				// Observers read the owned immutable DTO and only reduce bounded
				// memory state; no snapshot mutation, retention, I/O or publisher calls.
				sub.observe(v.Snapshot)
			}
			p.offerLocked(ch, v, sub.privateDTO)
		}
	}
	p.loading = nil
	close(ready)
	p.mu.Unlock()
	p.signal()
}

// matrix/hardened: DTO readers get private copies while transport readers get
// no mutable snapshot. Either mode has one replaceable pending publication.
func (p *menuSnapshotPublisher) offerLocked(ch chan menuPublication, v menuPublication, privateDTO bool) {
	if privateDTO {
		v = v.clone()
	} else {
		v = v.wire()
	}
	select {
	case ch <- v:
	default:
		select {
		case <-ch:
		default:
		}
		ch <- v
	}
}

// matrix/hardened: Subscribe gives channel readers private DTOs and returns an
// idempotent release. As before, observe reads the owned immutable snapshot;
// it must be nonblocking, memory-only, and must not mutate or retain the DTO.
func (p *menuSnapshotPublisher) Subscribe(interval time.Duration, observe func(*MenuSnapshot)) (<-chan menuPublication, func()) {
	return p.subscribe(interval, observe, true)
}

// matrix/hardened: production stream/push consumers only need immutable wire
// data; their observers have the same read-only contract as default Subscribe.
func (p *menuSnapshotPublisher) subscribeWire(interval time.Duration, observe func(*MenuSnapshot)) (<-chan menuPublication, func()) {
	return p.subscribe(interval, observe, false)
}

func (p *menuSnapshotPublisher) subscribe(interval time.Duration, observe func(*MenuSnapshot), privateDTO bool) (<-chan menuPublication, func()) {
	ch := make(chan menuPublication, 1)
	p.mu.Lock()
	if p.closed || p.ctx.Err() != nil {
		close(ch)
		p.mu.Unlock()
		return ch, func() {}
	}
	p.subscribers[ch] = menuPublisherSubscriber{interval: interval, observe: observe, privateDTO: privateDTO}
	if !p.dirty && !p.at.IsZero() && time.Since(p.at) < p.intervalLocked() {
		if p.current.Err == nil && observe != nil {
			observe(p.current.Snapshot)
		}
		p.offerLocked(ch, p.current, privateDTO)
	}
	if !p.running {
		p.running = true
		go p.run()
	}
	p.mu.Unlock()
	p.signal()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			p.mu.Lock()
			if _, present := p.subscribers[ch]; present {
				delete(p.subscribers, ch)
				close(ch)
			}
			p.mu.Unlock()
			p.signal()
		})
	}
}

func (p *menuSnapshotPublisher) signal() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// matrix/hardened: invalidation wakes existing demand immediately. With no
// demand it stays lazy until REST or the next subscriber requests a snapshot.
func (p *menuSnapshotPublisher) Invalidate() {
	p.invalidate(nil)
}

// matrix/hardened: mutation consumers can publish a new baseline epoch in the
// same invalidation boundary. An accepted old callback cannot see the new epoch,
// and a fresh callback cannot publish before the epoch reset is committed.
// afterInvalidation must be a short in-memory update and cannot call publisher.
func (p *menuSnapshotPublisher) invalidate(afterInvalidation func()) {
	p.mu.Lock()
	p.generation++
	p.dirty = true
	if afterInvalidation != nil {
		afterInvalidation()
	}
	p.mu.Unlock()
	p.signal()
}

// matrix/hardened: the loop exits at zero demand. Every timer is stopped on
// cancellation; an uninterruptible source call can only return into a discarded
// generation after Close, never revive the loop or publish after shutdown.
func (p *menuSnapshotPublisher) run() {
	defer func() {
		if p.ctx.Err() != nil {
			p.Close()
		}
	}()
	for {
		p.mu.Lock()
		if p.closed || len(p.subscribers) == 0 || p.ctx.Err() != nil {
			p.running = false
			p.mu.Unlock()
			return
		}
		delay := time.Until(p.at.Add(p.intervalLocked()))
		if p.dirty || p.at.IsZero() {
			delay = 0
		}
		p.mu.Unlock()
		if delay <= 0 {
			// matrix/hardened: the loop only ensures a generation exists; avoid
			// decoding an otherwise-unused private DTO on every collector tick.
			_, _ = p.loadOwned(p.ctx)
			continue
		}
		timer := time.NewTimer(delay)
		select {
		case <-p.ctx.Done():
		case <-p.wake:
		case <-timer.C:
		}
		timer.Stop()
	}
}

// matrix/hardened: closing releases subscribers and load waiters immediately.
func (p *menuSnapshotPublisher) Close() {
	p.cancel()
	p.mu.Lock()
	if !p.closed {
		p.closed = true
		for ch := range p.subscribers {
			delete(p.subscribers, ch)
			close(ch)
		}
	}
	p.mu.Unlock()
	p.signal()
}

// matrix/hardened: authenticated, read-only counters prove fanout collection
// behavior without displaying internal publisher details in the product UI.
func (s *Server) handleMenuPublisherDiagnostics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAPIError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method not allowed")
		return
	}
	if !s.authorizeRequest(r) {
		writeAPIError(w, http.StatusUnauthorized, "UNAUTHORIZED", "unauthorized")
		return
	}
	p := s.menuPublisher
	p.mu.Lock()
	stats := struct {
		Revision                uint64 `json:"revision"`
		Subscribers             int    `json:"subscribers"`
		RefreshCount            uint64 `json:"refreshCount"`
		ErrorCount              uint64 `json:"errorCount"`
		Running                 bool   `json:"running"`
		Loading                 bool   `json:"loading"`
		Dirty                   bool   `json:"dirty"`
		HasError                bool   `json:"hasError"`
		PushOverflowCount       uint64 `json:"pushOverflowCount"`
		CommandCenterBuildCount uint64 `json:"commandCenterBuildCount"`
	}{Revision: p.revision, Subscribers: len(p.subscribers), RefreshCount: p.refreshes, ErrorCount: p.errors, Running: p.running, Loading: p.loading != nil, Dirty: p.dirty, HasError: p.current.Err != nil}
	p.mu.Unlock()
	stats.CommandCenterBuildCount = s.commandCenterBuilds.Load()
	if push, ok := s.push.(*pushService); ok {
		stats.PushOverflowCount = push.publishedOverflows.Load()
	}
	writeJSON(w, http.StatusOK, stats)
}
