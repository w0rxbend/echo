package matrix

import (
	"container/heap"
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/worxbend/echo/internal/animations"
)

var ErrPlayQueueFull = errors.New("matrix play queue is full")

type ScheduledItem struct {
	PlayItem
	AnimationID           string                   `json:"animation_id,omitempty"`
	RestorePolicy         animations.RestorePolicy `json:"restore_policy,omitempty"`
	CreatedAt             time.Time                `json:"created_at,omitempty"`
	QueueDepthAtAdmission int                      `json:"-"`
	Control               *ControlItem             `json:"control,omitempty"`
	animationCompletion   *animationCompletion
}

type playQueue struct {
	mu       sync.Mutex
	notify   chan struct{}
	capacity int
	seq      uint64
	items    priorityItems
}

type queueHandle struct {
	seq uint64
}

func newPlayQueue(capacity int) *playQueue {
	return &playQueue{
		notify:   make(chan struct{}, 1),
		capacity: capacity,
	}
}

func (q *playQueue) enqueueScheduled(ctx context.Context, item ScheduledItem) (queueHandle, int, error) {
	if err := ctx.Err(); err != nil {
		return queueHandle{}, 0, err
	}
	if item.RestorePolicy == "" {
		item.RestorePolicy = animations.RestoreLeave
	}
	item.ensureAnimationCompletion()
	if item.CreatedAt.IsZero() {
		item.CreatedAt = time.Now().UTC()
	}

	q.mu.Lock()
	defer q.mu.Unlock()

	if q.capacity > 0 && len(q.items) >= q.capacity {
		return queueHandle{}, len(q.items), ErrPlayQueueFull
	}

	q.seq++
	handle := queueHandle{seq: q.seq}
	depth := len(q.items) + 1
	item.QueueDepthAtAdmission = depth
	heap.Push(&q.items, priorityItem{item: item, seq: handle.seq})
	q.signalLocked()

	return handle, depth, nil
}

func (q *playQueue) next(ctx context.Context) (ScheduledItem, error) {
	for {
		q.mu.Lock()
		if len(q.items) > 0 {
			entry := heap.Pop(&q.items).(priorityItem)
			q.mu.Unlock()
			return entry.item, nil
		}
		notify := q.notify
		q.mu.Unlock()

		select {
		case <-ctx.Done():
			return ScheduledItem{}, ctx.Err()
		case <-notify:
		}
	}
}

func (q *playQueue) len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items)
}

func (q *playQueue) snapshot() []QueueItemStatus {
	q.mu.Lock()
	defer q.mu.Unlock()

	entries := make([]priorityItem, len(q.items))
	copy(entries, q.items)
	sort.SliceStable(entries, func(i, j int) bool {
		return entries[i].less(entries[j])
	})

	snapshot := make([]QueueItemStatus, len(entries))
	for i, entry := range entries {
		snapshot[i] = queueItemStatus(entry.item)
	}
	return snapshot
}

func queueItemStatus(item ScheduledItem) QueueItemStatus {
	status := QueueItemStatus{
		Kind:          QueueItemAnimation,
		ID:            item.ID,
		EventID:       item.EventID,
		AnimationID:   item.AnimationID,
		Priority:      item.Priority,
		RestorePolicy: item.RestorePolicy,
		CreatedAt:     item.CreatedAt,
		Deadline:      item.Deadline,
	}
	if item.Control == nil {
		return status
	}

	control := item.Control
	status.Kind = QueueItemControl
	status.Control = &QueueControlStatus{
		ID:         control.ID,
		Kind:       control.Kind,
		Priority:   control.Priority,
		Brightness: control.Brightness,
		EffectID:   control.EffectID,
		Interval:   control.Interval,
		Color:      control.Color,
		CreatedAt:  control.CreatedAt,
		Deadline:   control.Deadline,
	}
	return status
}

func (q *playQueue) remove(handle queueHandle) (ScheduledItem, int, bool) {
	if handle.seq == 0 {
		return ScheduledItem{}, 0, false
	}

	q.mu.Lock()
	defer q.mu.Unlock()

	for _, entry := range q.items {
		if entry.seq == handle.seq {
			depth := len(q.items)
			removed := heap.Remove(&q.items, entry.index).(priorityItem)
			return removed.item, depth, true
		}
	}
	return ScheduledItem{}, 0, false
}

func (q *playQueue) signalLocked() {
	select {
	case q.notify <- struct{}{}:
	default:
	}
}

func (q *playQueue) clear() []ScheduledItem {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.clearLocked()
}

func (q *playQueue) clearLocked() []ScheduledItem {
	entries := q.items
	q.items = nil

	items := make([]ScheduledItem, len(entries))
	for i, entry := range entries {
		items[i] = entry.item
	}
	return items
}

type priorityItem struct {
	item  ScheduledItem
	seq   uint64
	index int
}

type priorityItems []priorityItem

func (p priorityItems) Len() int {
	return len(p)
}

func (p priorityItems) Less(i, j int) bool {
	return p[i].less(p[j])
}

func (item priorityItem) less(other priorityItem) bool {
	// Controls run in a reserved admin lane: once the current item finishes,
	// any queued control is selected before normal animations, regardless of
	// animation priority. Control priority still orders controls against other
	// controls, and the heap sequence preserves FIFO for equal priorities.
	if (item.item.Control != nil) != (other.item.Control != nil) {
		return item.item.Control != nil
	}
	if item.item.Priority != other.item.Priority {
		return item.item.Priority > other.item.Priority
	}
	return item.seq < other.seq
}

func (p priorityItems) Swap(i, j int) {
	p[i], p[j] = p[j], p[i]
	p[i].index = i
	p[j].index = j
}

func (p *priorityItems) Push(x any) {
	item := x.(priorityItem)
	item.index = len(*p)
	*p = append(*p, item)
}

func (p *priorityItems) Pop() any {
	old := *p
	n := len(old)
	item := old[n-1]
	old[n-1] = priorityItem{}
	*p = old[:n-1]
	return item
}
