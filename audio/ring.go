package audio

import (
	"errors"
	"sync"
)

var (
	ErrIsFull  = errors.New("ring is full")
	ErrIsEmpty = errors.New("ring is empty")
)

type Ring struct {
	buf    []interface{}
	size   int
	r      int // next position to read
	w      int // next position to write
	isFull bool
	mu     sync.Mutex
	wcd    *sync.Cond
	rcd    *sync.Cond
}

// NewRing New returns a new Ring whose buffer has the given size.
func NewRing(size int) *Ring {
	r := &Ring{
		buf:  make([]interface{}, size),
		size: size,
	}
	r.wcd = sync.NewCond(&r.mu)
	r.rcd = sync.NewCond(&r.mu)
	return r
}

func (r *Ring) TryPop() (b interface{}, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.w == r.r && !r.isFull {
		return nil, ErrIsEmpty
	}
	b = r.buf[r.r]
	r.buf[r.r] = nil
	r.r++
	if r.r == r.size {
		r.r = 0
	}
	r.isFull = false
	r.wcd.Signal()
	return b, err
}

func (r *Ring) TryPush(c interface{}) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.w == r.r && r.isFull {
		return ErrIsFull
	}
	r.buf[r.w] = c
	r.w++

	if r.w == r.size {
		r.w = 0
	}
	if r.w == r.r {
		r.isFull = true
	} else {
		r.rcd.Signal()
	}
	return nil
}

func (r *Ring) TryPeek() (b interface{}, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.w == r.r && !r.isFull {
		return nil, ErrIsEmpty
	}
	b = r.buf[r.r]
	return b, nil
}

func (r *Ring) Flush(predicate func(interface{}) bool) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.w == r.r && !r.isFull {
		return 0
	}
	writerPos := 0
	result := make([]interface{}, len(r.buf))
	count := r.w - r.r
	if count < 0 {
		count += r.size
	}
	if r.isFull {
		count = r.size
	}
	pos := r.r
	for i := 0; i < count; i++ {
		v := r.buf[pos]
		if v != nil && predicate(v) {
			result[writerPos] = v
			writerPos++
		}
		pos++
		if pos == r.size {
			pos = 0
		}
	}
	r.buf = result
	r.r = 0
	r.w = writerPos % r.size
	r.isFull = writerPos == r.size
	r.wcd.Broadcast()
	return writerPos
}

func (r *Ring) Push(c interface{}) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for r.isFull {
		r.wcd.Wait()
	}
	r.buf[r.w] = c
	r.w = (r.w + 1) % r.size
	r.isFull = r.w == r.r
	r.rcd.Signal()
}

// PushUntil waits for space, but abandons the item when its stream ends.
// The caller must close done before Reset, which wakes blocked producers.
func (r *Ring) PushUntil(c interface{}, done <-chan struct{}, accept ...func(interface{}) bool) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for {
		select {
		case <-done:
			return false
		default:
		}
		if len(accept) > 0 && !accept[0](c) {
			return true // intentionally discarded, not cancelled
		}
		if !r.isFull {
			break
		}
		r.wcd.Wait()
	}
	r.buf[r.w] = c
	r.w = (r.w + 1) % r.size
	r.isFull = r.w == r.r
	r.rcd.Signal()
	return true
}

func (r *Ring) Pop() interface{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	for r.w == r.r && !r.isFull {
		r.rcd.Wait()
	}
	value := r.buf[r.r]
	r.buf[r.r] = nil
	r.r = (r.r + 1) % r.size
	r.isFull = false
	r.wcd.Signal()
	return value
}

func (r *Ring) Peek() interface{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	for r.w == r.r && !r.isFull {
		r.rcd.Wait()
	}
	return r.buf[r.r]
}

// Length return the length of available read bytes.
func (r *Ring) Length() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.w == r.r {
		if r.isFull {
			return r.size
		}
		return 0
	}

	if r.w > r.r {
		return r.w - r.r
	}

	return r.size - r.r + r.w
}

// Capacity returns the size of the underlying buffer.
func (r *Ring) Capacity() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.size
}

// Free returns the length of available bytes to write.
func (r *Ring) Free() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.w == r.r {
		if r.isFull {
			return 0
		}
		return r.size
	}

	if r.w < r.r {
		return r.r - r.w
	}

	return r.size - r.w + r.r
}

// IsFull returns this Ring is full.
func (r *Ring) IsFull() bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.isFull
}

// IsEmpty returns this Ring is empty.
func (r *Ring) IsEmpty() bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	return !r.isFull && r.w == r.r
}

// Reset the read pointer and writer pointer to zero.
func (r *Ring) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.r = 0
	r.w = 0
	r.isFull = false
	clear(r.buf)
	r.wcd.Broadcast()
}

// ResetCapacity atomically discards queued items and replaces their storage.
// Blocked producers wake and recheck their cancellation and filtering rules.
func (r *Ring) ResetCapacity(capacity int) {
	if capacity <= 0 {
		panic("ring capacity must be positive")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = make([]interface{}, capacity)
	r.size, r.r, r.w, r.isFull = capacity, 0, 0, false
	r.wcd.Broadcast()
}
