package sortedindex

import "sync"

// PayloadPump turns the data of notifies into the event channel of a
// model.PayloadWatch, for the RADOS store and the in-memory store
// alike. It keeps a bounded queue: a holder that falls behind loses the
// data that did not fit, and is handed one empty event afterwards,
// which means "check everything" (the push worker then reads its whole
// partition). Push never blocks, so the thread that delivers notifies
// (librados' callback thread) is never held up by a slow reader.
type PayloadPump struct {
	mu       sync.Mutex
	q        [][]byte
	overflow bool
	wake     chan struct{}
	out      chan []byte
	done     chan struct{}
	once     sync.Once
}

// PayloadBuffer is how many notifies a PayloadPump queues before it
// drops them and signals an overflow instead.
const PayloadBuffer = 256

// NewPayloadPump starts a pump; Close ends it.
func NewPayloadPump() *PayloadPump {
	p := &PayloadPump{wake: make(chan struct{}, 1), out: make(chan []byte), done: make(chan struct{})}
	go p.run()

	return p
}

// Push queues the data of one notify (a copy of it).
func (p *PayloadPump) Push(data []byte) {
	p.mu.Lock()

	if len(p.q) < PayloadBuffer {
		p.q = append(p.q, append([]byte(nil), data...))
	} else {
		p.overflow = true
	}

	p.mu.Unlock()

	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// Events is the channel the data arrives on; an empty event follows an
// overflow.
func (p *PayloadPump) Events() <-chan []byte { return p.out }

// Close ends the pump; what is queued is dropped.
func (p *PayloadPump) Close() { p.once.Do(func() { close(p.done) }) }

func (p *PayloadPump) run() {
	for {
		select {
		case <-p.done:
			return
		case <-p.wake:
		}

		for {
			p.mu.Lock()
			var item []byte

			more := len(p.q) > 0
			if more {
				item, p.q = p.q[0], p.q[1:]
			} else if p.overflow {
				item, p.overflow, more = nil, false, true
			}
			p.mu.Unlock()

			if !more {
				break
			}

			select {
			case <-p.done:
				return
			case p.out <- item:
			}
		}
	}
}
