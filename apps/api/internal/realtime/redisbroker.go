package realtime

// The Redis pub/sub transport.
//
// # One connection, dynamic subscriptions
//
// go-redis's PubSub multiplexes any number of channels over one connection and
// re-subscribes to all of them after a reconnect, so the broker holds exactly
// one and issues SUBSCRIBE / UNSUBSCRIBE as rooms gain and lose their first and
// last local member. The alternative — a connection per room — would put the
// connection count of the whole fleet at (instances × boards being watched),
// which is a number nobody wants to size an ElastiCache node against.
//
// # Why Subscribe waits for the acknowledgement
//
// go-redis's Subscribe returns once the command has been *written*, not once
// Redis has processed it. The SUBSCRIBE travels on the pub/sub connection and
// the PUBLISH that follows travels on a pooled one, and Redis makes no ordering
// promise between two connections. So a hub that acknowledged a subscription as
// soon as Subscribe returned would have a window — small, load-dependent, and
// therefore exactly the kind that shows up once a quarter in production — where
// a client is told it is watching a board and the next event goes past it.
//
// Redis confirms a subscription with a message on the same stream, so this
// broker reads the stream with ChannelWithSubscriptions and blocks Subscribe
// until the confirmation for that channel arrives. Subscribe returning means
// the subscription is live, and the tests can be written as
// "subscribe, publish, assert" rather than "subscribe, publish, retry".

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/redis/go-redis/v9"
)

// RedisBrokerConfig are the dependencies of [NewRedisBroker].
type RedisBrokerConfig struct {
	// Client is the go-redis client. Required.
	Client redis.UniversalClient

	// Logger receives transport-level problems. Required.
	Logger *slog.Logger

	// Buffer is how many inbound messages may queue. Defaults to
	// [DefaultBrokerBuffer].
	Buffer int
}

// RedisBroker is the [Broker] used in every environment that runs more than one
// process, which is every environment except a developer's laptop.
type RedisBroker struct {
	client redis.UniversalClient
	pubsub *redis.PubSub
	logger *slog.Logger

	messages chan Message

	// mu guards waiters. Held only for map operations — never across a Redis
	// call and never across a channel send.
	mu      sync.Mutex
	waiters map[string][]chan struct{}

	closeOnce sync.Once
	done      chan struct{}
	wg        sync.WaitGroup
}

// NewRedisBroker opens the pub/sub connection and starts reading it.
//
// The connection is opened with no channels: rooms are subscribed as
// connections arrive. The caller owns the returned broker and must Close it.
func NewRedisBroker(cfg RedisBrokerConfig) (*RedisBroker, error) {
	if cfg.Client == nil {
		return nil, errors.New("realtime: redis broker needs a client")
	}

	if cfg.Logger == nil {
		return nil, errors.New("realtime: redis broker needs a logger")
	}

	buffer := cfg.Buffer
	if buffer <= 0 {
		buffer = DefaultBrokerBuffer
	}

	broker := &RedisBroker{
		client:   cfg.Client,
		pubsub:   cfg.Client.Subscribe(context.Background()),
		logger:   cfg.Logger,
		messages: make(chan Message, buffer),
		waiters:  map[string][]chan struct{}{},
		done:     make(chan struct{}),
	}

	broker.wg.Add(1)

	go broker.receive()

	return broker, nil
}

// Subscribe implements [Broker]. It returns once Redis has confirmed the
// subscription — see the file comment.
func (b *RedisBroker) Subscribe(ctx context.Context, room Room) error {
	if !room.valid() {
		return fmt.Errorf("realtime: refusing to subscribe to an incomplete room %s", room)
	}

	channel := room.Channel()

	// Registered before the command is sent. The other order has a race with
	// itself: the confirmation can arrive before the waiter exists, and then
	// nothing ever signals it.
	ack := b.addWaiter(channel)
	defer b.removeWaiter(channel, ack)

	if err := b.pubsub.Subscribe(ctx, channel); err != nil {
		return fmt.Errorf("subscribing to %s: %w", room, err)
	}

	select {
	case <-ack:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("waiting for the subscription to %s: %w", room, ctx.Err())
	case <-b.done:
		return ErrBrokerClosed
	}
}

// Unsubscribe implements [Broker].
//
// It does not wait for a confirmation. The room has already been removed from
// the hub's map by the time this runs, so a message that arrives in the gap is
// delivered to nobody — which is the behaviour we want anyway, and it means an
// unsubscribe never blocks a disconnecting client's cleanup on a Redis round
// trip.
func (b *RedisBroker) Unsubscribe(ctx context.Context, room Room) error {
	if err := b.pubsub.Unsubscribe(ctx, room.Channel()); err != nil {
		return fmt.Errorf("unsubscribing from %s: %w", room, err)
	}

	return nil
}

// Publish implements [Broker].
func (b *RedisBroker) Publish(ctx context.Context, room Room, payload []byte) error {
	if !room.valid() {
		return fmt.Errorf("realtime: refusing to publish to an incomplete room %s", room)
	}

	envelope, err := json.Marshal(transportEnvelope{
		TenantID: room.TenantID,
		BoardID:  room.BoardID,
		Payload:  payload,
	})
	if err != nil {
		return fmt.Errorf("encoding the transport envelope for %s: %w", room, err)
	}

	if err := b.client.Publish(ctx, room.Channel(), envelope).Err(); err != nil {
		return fmt.Errorf("publishing to %s: %w", room, err)
	}

	return nil
}

// Messages implements [Broker].
func (b *RedisBroker) Messages() <-chan Message { return b.messages }

// Close implements [Broker].
func (b *RedisBroker) Close() error {
	var err error

	b.closeOnce.Do(func() {
		close(b.done)

		// Closing the PubSub ends the receive goroutine's range, which is what
		// makes closing b.messages safe: that goroutine is its only producer.
		err = b.pubsub.Close()
		b.wg.Wait()
		close(b.messages)
	})

	if err != nil {
		return fmt.Errorf("closing the realtime pub/sub connection: %w", err)
	}

	return nil
}

// receive is the single reader of the pub/sub stream and the single producer
// of b.messages.
func (b *RedisBroker) receive() {
	defer b.wg.Done()

	for received := range b.pubsub.ChannelWithSubscriptions() {
		switch value := received.(type) {
		case *redis.Subscription:
			b.handleSubscription(value)
		case *redis.Message:
			b.handleMessage(value)
		case *redis.Pong:
			// Keepalive on the pub/sub connection, issued by go-redis's own
			// health check. Nothing to do.
		}
	}
}

func (b *RedisBroker) handleSubscription(sub *redis.Subscription) {
	if sub.Kind != "subscribe" {
		return
	}

	b.mu.Lock()
	waiters := b.waiters[sub.Channel]
	delete(b.waiters, sub.Channel)
	b.mu.Unlock()

	for _, waiter := range waiters {
		close(waiter)
	}
}

// handleMessage decodes one published envelope and forwards it.
//
// The room inside the envelope is checked against the channel it arrived on. A
// mismatch is not a client error and not something a well-behaved fleet
// produces: it means either a bug in the addressing here or something else
// publishing into this namespace. Either way the message is dropped and logged
// at error, because a delivered mismatch is a cross-tenant leak.
func (b *RedisBroker) handleMessage(msg *redis.Message) {
	var envelope transportEnvelope
	if err := json.Unmarshal([]byte(msg.Payload), &envelope); err != nil {
		b.logger.Error("discarding an undecodable realtime message",
			slog.String("event", "realtime.transport.undecodable"),
			slog.String("channel", msg.Channel),
			slog.Any("error", err),
		)

		return
	}

	room := envelope.room()

	if !room.valid() || room.Channel() != msg.Channel {
		b.logger.Error("discarding a misrouted realtime message",
			slog.String("event", "realtime.transport.misrouted"),
			slog.String("channel", msg.Channel),
			slog.String("envelope_room", room.String()),
		)

		return
	}

	select {
	case b.messages <- Message{Room: room, Payload: envelope.Payload}:
	case <-b.done:
	}
}

func (b *RedisBroker) addWaiter(channel string) chan struct{} {
	waiter := make(chan struct{})

	b.mu.Lock()
	b.waiters[channel] = append(b.waiters[channel], waiter)
	b.mu.Unlock()

	return waiter
}

// removeWaiter drops a waiter that is no longer being waited on, so a
// cancelled Subscribe does not leave an entry behind. Closing the channel is
// the confirmation path's job and must not happen twice, so this only removes.
func (b *RedisBroker) removeWaiter(channel string, waiter chan struct{}) {
	b.mu.Lock()
	defer b.mu.Unlock()

	remaining := b.waiters[channel][:0]

	for _, candidate := range b.waiters[channel] {
		if candidate != waiter {
			remaining = append(remaining, candidate)
		}
	}

	if len(remaining) == 0 {
		delete(b.waiters, channel)

		return
	}

	b.waiters[channel] = remaining
}
