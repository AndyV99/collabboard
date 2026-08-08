package realtime

// The transport between instances, and the in-process implementation of it.
//
// The interface is four methods because that is the whole of what a hub needs:
// tell me when this room has traffic, stop telling me, send some, and give me
// the stream. Anything wider would start to look like a message queue, and this
// is deliberately not one — see the package comment on why nothing here is
// durable.

import (
	"context"
	"errors"
	"slices"
	"sync"
)

// ErrBrokerClosed is returned by every [Broker] method after Close.
var ErrBrokerClosed = errors.New("realtime: broker is closed")

// Message is one delivery from the transport: the room it was addressed to,
// and the bytes to hand to that room's connections.
type Message struct {
	// Room is taken from the channel the message arrived on, and checked
	// against the room inside the envelope before it gets this far.
	Room Room

	// Payload is an already-encoded client [Frame]. The hub does not decode it.
	Payload []byte
}

// Broker carries events between instances.
//
// Subscribe and Unsubscribe are reference-counted by the caller, not here: the
// hub knows when a room gains its first local member and loses its last, and
// an implementation that also counted would be counting the same thing twice.
type Broker interface {
	// Subscribe starts delivering messages for room to [Broker.Messages]. It
	// returns only once the subscription is live, so a caller may publish
	// immediately afterwards and expect to receive its own message.
	Subscribe(ctx context.Context, room Room) error

	// Unsubscribe stops delivery for room. Unsubscribing a room that was never
	// subscribed is not an error.
	Unsubscribe(ctx context.Context, room Room) error

	// Publish sends payload to every instance subscribed to room, including
	// this one.
	Publish(ctx context.Context, room Room, payload []byte) error

	// Messages is the inbound stream. It is closed when the broker is closed,
	// which is how a hub's dispatch loop learns to stop.
	Messages() <-chan Message

	// Close releases the transport. Idempotent.
	Close() error
}

// MemoryBus is an in-process [Broker] transport: several brokers attached to
// one bus behave like several instances attached to one Redis.
//
// It exists so the concurrency in [Hub] — registration, fan-out, backpressure,
// shutdown — can be tested under `-race` in milliseconds, and so the
// two-instance property has a fast test as well as the container-backed one in
// realtime_integration_test.go. It is not a Redis simulator and makes no claim
// to be: the integration suite is what proves the real transport works.
type MemoryBus struct {
	mu      sync.Mutex
	brokers []*MemoryBroker
}

// NewMemoryBus returns an empty bus.
func NewMemoryBus() *MemoryBus {
	return &MemoryBus{}
}

// Broker attaches a new broker to the bus. Each one stands for one instance.
func (b *MemoryBus) Broker() *MemoryBroker {
	broker := &MemoryBroker{
		bus:      b,
		subs:     map[Room]struct{}{},
		messages: make(chan Message, DefaultBrokerBuffer),
		done:     make(chan struct{}),
	}

	b.mu.Lock()
	b.brokers = append(b.brokers, broker)
	b.mu.Unlock()

	return broker
}

func (b *MemoryBus) snapshot() []*MemoryBroker {
	b.mu.Lock()
	defer b.mu.Unlock()

	return slices.Clone(b.brokers)
}

func (b *MemoryBus) detach(broker *MemoryBroker) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.brokers = slices.DeleteFunc(b.brokers, func(candidate *MemoryBroker) bool {
		return candidate == broker
	})
}

// MemoryBroker is one instance's attachment to a [MemoryBus].
type MemoryBroker struct {
	bus *MemoryBus

	mu     sync.Mutex
	subs   map[Room]struct{}
	closed bool

	messages chan Message

	// inflight counts deliveries that have passed the closed check and are
	// about to send on messages. Close waits for it to drain before closing
	// the channel, which is what makes "send on a closed channel" impossible
	// rather than unlikely — the failure a -race run would only sometimes see.
	inflight sync.WaitGroup

	closeOnce sync.Once
	done      chan struct{}
}

// Subscribe implements [Broker].
func (b *MemoryBroker) Subscribe(_ context.Context, room Room) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		return ErrBrokerClosed
	}

	b.subs[room] = struct{}{}

	return nil
}

// Unsubscribe implements [Broker].
func (b *MemoryBroker) Unsubscribe(_ context.Context, room Room) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		return ErrBrokerClosed
	}

	delete(b.subs, room)

	return nil
}

// Publish implements [Broker]. Like Redis, it delivers to subscribers and
// drops the message on the floor if nobody is subscribed anywhere.
func (b *MemoryBroker) Publish(ctx context.Context, room Room, payload []byte) error {
	b.mu.Lock()
	closed := b.closed
	b.mu.Unlock()

	if closed {
		return ErrBrokerClosed
	}

	// Copied because the caller owns payload and every recipient shares this
	// one slice. Redis gives each instance its own bytes; a transport that did
	// not would make an aliasing bug show up only in production.
	shared := slices.Clone(payload)

	for _, broker := range b.bus.snapshot() {
		broker.deliver(ctx, room, shared)
	}

	return nil
}

func (b *MemoryBroker) deliver(ctx context.Context, room Room, payload []byte) {
	b.mu.Lock()

	_, subscribed := b.subs[room]
	if b.closed || !subscribed {
		b.mu.Unlock()

		return
	}

	b.inflight.Add(1)
	b.mu.Unlock()

	defer b.inflight.Done()

	select {
	case b.messages <- Message{Room: room, Payload: payload}:
	case <-b.done:
	case <-ctx.Done():
	}
}

// Messages implements [Broker].
func (b *MemoryBroker) Messages() <-chan Message { return b.messages }

// Close implements [Broker].
func (b *MemoryBroker) Close() error {
	b.closeOnce.Do(func() {
		b.mu.Lock()
		b.closed = true
		b.mu.Unlock()

		b.bus.detach(b)

		// Ordered: no new delivery can start once closed is set, close(done)
		// releases any that are blocked, and only then is the channel closed.
		close(b.done)
		b.inflight.Wait()
		close(b.messages)
	})

	return nil
}
