package nudgeparity

import (
	"container/list"
	"fmt"
	"sync"
	"time"
)

// Config defines strict memory and time bounds for a Comparator.
type Config struct {
	Retention  time.Duration
	MaxPending int
	MaxRecent  int
	Now        func() time.Time
}

// Comparator performs a reorder-safe, bounded join of expected and actual
// observations. It returns records to its caller and performs no external I/O.
type Comparator struct {
	mu           sync.Mutex
	retention    time.Duration
	maxPending   int
	maxRecent    int
	now          func() time.Time
	lastNow      time.Time
	pending      map[string]*pendingEntry
	pendingOrder list.List
	recent       map[string]*recentEntry
	recentOrder  list.List
	snapshot     Snapshot
}

type pendingEntry struct {
	operationID string
	expiresAt   time.Time
	order       *list.Element
	expected    Observation
	hasExpected bool
	actual      Observation
	hasActual   bool
}

type recentEntry struct {
	operationID string
	order       *list.Element
	expected    Observation
	hasExpected bool
	actual      Observation
	hasActual   bool
}

// New creates an effect-free comparator with explicit retention bounds.
func New(config Config) (*Comparator, error) {
	if config.Retention <= 0 {
		return nil, fmt.Errorf("creating nudge parity comparator: retention must be positive")
	}
	if config.MaxPending <= 0 {
		return nil, fmt.Errorf("creating nudge parity comparator: max pending must be positive")
	}
	if config.MaxRecent <= 0 {
		return nil, fmt.Errorf("creating nudge parity comparator: max recent must be positive")
	}
	now := config.Now
	if now == nil {
		now = time.Now
	}
	return &Comparator{
		retention:  config.Retention,
		maxPending: config.MaxPending,
		maxRecent:  config.MaxRecent,
		now:        now,
		pending:    make(map[string]*pendingEntry, config.MaxPending),
		recent:     make(map[string]*recentEntry, config.MaxRecent),
	}, nil
}

// Observe accepts one immutable observation and returns any records made
// terminal by expiry, capacity, duplication, or pairing.
func (c *Comparator) Observe(observation Observation) ([]Result, error) {
	if c == nil {
		return nil, fmt.Errorf("observing nudge parity: comparator is nil")
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	now, err := c.nowLocked()
	if err != nil {
		return nil, err
	}
	if err := observation.validate(now); err != nil {
		c.snapshot.InvalidObservations++
		return nil, err
	}

	results := c.expireLocked(now)
	if recent := c.recent[observation.OperationID]; recent != nil {
		result := c.classifyRecentLocked(recent, observation)
		c.recordLocked(result)
		return append(results, result), nil
	}

	entry := c.pending[observation.OperationID]
	if entry != nil {
		if existing, ok := entry.observation(observation.Side); ok {
			result := duplicateResult(entry, existing, observation)
			c.recordLocked(result)
			return append(results, result), nil
		}
		entry.set(observation)
		result := compareEntry(entry)
		c.removePendingLocked(entry)
		c.addRecentLocked(entry)
		c.recordLocked(result)
		return append(results, result), nil
	}

	if len(c.pending) >= c.maxPending {
		oldest := c.pendingOrder.Front()
		if oldest == nil {
			return nil, fmt.Errorf("observing nudge parity: pending index and order disagree")
		}
		result := c.terminalizePendingLocked(oldest.Value.(*pendingEntry), ReasonCapacity, true)
		results = append(results, result)
	}
	entry = &pendingEntry{
		operationID: observation.OperationID,
		expiresAt:   now.Add(c.retention),
	}
	entry.set(observation)
	entry.order = c.pendingOrder.PushBack(entry)
	c.pending[entry.operationID] = entry
	c.updateStateCountsLocked()
	return results, nil
}

// Sweep expires all pending observations whose retention deadline has passed.
func (c *Comparator) Sweep() ([]Result, error) {
	if c == nil {
		return nil, fmt.Errorf("sweeping nudge parity: comparator is nil")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now, err := c.nowLocked()
	if err != nil {
		return nil, err
	}
	return c.expireLocked(now), nil
}

// Flush reports every pending counterpart as missing, then clears pending and
// recent state. Counters remain monotonic across the reset.
func (c *Comparator) Flush() []Result {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	results := make([]Result, 0, len(c.pending))
	for c.pendingOrder.Len() != 0 {
		entry := c.pendingOrder.Front().Value.(*pendingEntry)
		results = append(results, c.terminalizePendingLocked(entry, ReasonFlush, false))
	}
	clear(c.recent)
	c.recentOrder.Init()
	c.updateStateCountsLocked()
	return results
}

// Snapshot returns fixed-cardinality counters and current retained-state sizes.
func (c *Comparator) Snapshot() Snapshot {
	if c == nil {
		return Snapshot{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	snapshot := c.snapshot
	snapshot.Pending = len(c.pending)
	snapshot.Recent = len(c.recent)
	return snapshot
}

func (c *Comparator) nowLocked() (time.Time, error) {
	now := c.now()
	if now.IsZero() {
		return time.Time{}, ErrInvalidClock
	}
	if !c.lastNow.IsZero() && now.Before(c.lastNow) {
		c.snapshot.ClockRegressions++
		return time.Time{}, fmt.Errorf("%w: previous=%s current=%s", ErrClockRegression, c.lastNow, now)
	}
	c.lastNow = now
	return now, nil
}

func (c *Comparator) expireLocked(now time.Time) []Result {
	var results []Result
	for element := c.pendingOrder.Front(); element != nil; element = c.pendingOrder.Front() {
		entry := element.Value.(*pendingEntry)
		if entry.expiresAt.After(now) {
			break
		}
		results = append(results, c.terminalizePendingLocked(entry, ReasonExpired, true))
	}
	return results
}

func (c *Comparator) terminalizePendingLocked(entry *pendingEntry, reason Reason, retainRecent bool) Result {
	classification := ClassificationMissingExpected
	if entry.hasExpected {
		classification = ClassificationMissingActual
	}
	result := resultFromEntry(entry, classification, reason)
	c.removePendingLocked(entry)
	if retainRecent {
		c.addRecentLocked(entry)
	}
	c.recordLocked(result)
	return result
}

func (c *Comparator) removePendingLocked(entry *pendingEntry) {
	delete(c.pending, entry.operationID)
	if entry.order != nil {
		c.pendingOrder.Remove(entry.order)
		entry.order = nil
	}
	c.updateStateCountsLocked()
}

func (c *Comparator) addRecentLocked(entry *pendingEntry) {
	for len(c.recent) >= c.maxRecent {
		oldest := c.recentOrder.Front()
		if oldest == nil {
			break
		}
		oldEntry := oldest.Value.(*recentEntry)
		delete(c.recent, oldEntry.operationID)
		c.recentOrder.Remove(oldest)
	}
	recent := &recentEntry{
		operationID: entry.operationID,
		expected:    entry.expected,
		hasExpected: entry.hasExpected,
		actual:      entry.actual,
		hasActual:   entry.hasActual,
	}
	recent.order = c.recentOrder.PushBack(recent)
	c.recent[recent.operationID] = recent
	c.updateStateCountsLocked()
}

func (c *Comparator) classifyRecentLocked(recent *recentEntry, incoming Observation) Result {
	entry := &pendingEntry{
		operationID: recent.operationID,
		expected:    recent.expected,
		hasExpected: recent.hasExpected,
		actual:      recent.actual,
		hasActual:   recent.hasActual,
	}
	if existing, ok := entry.observation(incoming.Side); ok {
		return duplicateResult(entry, existing, incoming)
	}
	result := resultFromEntry(entry, ClassificationLate, ReasonCounterpartAfterTerminal)
	result.Incoming = incoming
	result.HasIncoming = true
	return result
}

func (c *Comparator) recordLocked(result Result) {
	if result.Classification > ClassificationUnknown && result.Classification < classificationLimit {
		c.snapshot.classifications[result.Classification]++
	}
	if result.Reason > ReasonUnknown && result.Reason < reasonLimit {
		c.snapshot.reasons[result.Reason]++
	}
}

func (c *Comparator) updateStateCountsLocked() {
	c.snapshot.Pending = len(c.pending)
	c.snapshot.Recent = len(c.recent)
}

func (e *pendingEntry) set(observation Observation) {
	if observation.Side == SideExpected {
		e.expected = observation
		e.hasExpected = true
		return
	}
	e.actual = observation
	e.hasActual = true
}

func (e *pendingEntry) observation(side Side) (Observation, bool) {
	if side == SideExpected {
		return e.expected, e.hasExpected
	}
	return e.actual, e.hasActual
}

func compareEntry(entry *pendingEntry) Result {
	classification, reason := ClassificationSame, ReasonEquivalent
	switch {
	case !entry.expected.Input.complete() || !entry.actual.Input.complete():
		classification, reason = ClassificationIncomparable, ReasonInputIncomplete
	case entry.expected.Input != entry.actual.Input:
		classification, reason = ClassificationIncomparable, ReasonInputMismatch
	case !entry.expected.Watermarks.complete() || !entry.actual.Watermarks.complete():
		classification, reason = ClassificationIncomparable, ReasonWatermarkIncomplete
	case entry.expected.Watermarks != entry.actual.Watermarks:
		classification, reason = ClassificationIncomparable, ReasonWatermarkMismatch
	case entry.expected.Plan != entry.actual.Plan:
		classification, reason = ClassificationDivergent, ReasonPlanMismatch
	}
	return resultFromEntry(entry, classification, reason)
}

func duplicateResult(entry *pendingEntry, existing, incoming Observation) Result {
	reason := ReasonDuplicateConflicting
	if existing == incoming {
		reason = ReasonDuplicateIdentical
	}
	result := resultFromEntry(entry, ClassificationDuplicate, reason)
	result.Incoming = incoming
	result.HasIncoming = true
	return result
}

func resultFromEntry(entry *pendingEntry, classification Classification, reason Reason) Result {
	result := Result{
		OperationID:    entry.operationID,
		Classification: classification,
		Reason:         reason,
		Expected:       entry.expected,
		HasExpected:    entry.hasExpected,
		Actual:         entry.actual,
		HasActual:      entry.hasActual,
	}
	if entry.hasExpected {
		result.ExpectedLatency = latencyFor(entry.expected)
	}
	if entry.hasActual {
		result.ActualLatency = latencyFor(entry.actual)
	}
	return result
}
