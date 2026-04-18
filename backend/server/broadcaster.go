package server

import (
	"sync"

	pb "github.com/antong314/whatscli-rs/backend/gen/pb"
)

// Broadcaster fans out ServerEvents to all connected gRPC clients.
type Broadcaster struct {
	mu      sync.RWMutex
	clients map[uint64]chan *pb.ServerEvent
	nextID  uint64
}

func NewBroadcaster() *Broadcaster {
	return &Broadcaster{
		clients: make(map[uint64]chan *pb.ServerEvent),
	}
}

// Subscribe registers a new client and returns its ID and event channel.
// The channel is buffered to prevent a slow client from blocking others.
func (b *Broadcaster) Subscribe() (uint64, chan *pb.ServerEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()
	id := b.nextID
	b.nextID++
	ch := make(chan *pb.ServerEvent, 256)
	b.clients[id] = ch
	return id, ch
}

// Unsubscribe removes a client and closes its channel.
func (b *Broadcaster) Unsubscribe(id uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if ch, ok := b.clients[id]; ok {
		close(ch)
		delete(b.clients, id)
	}
}

// Send delivers an event to all connected clients. If a client's buffer is
// full the event is dropped for that client (non-blocking).
func (b *Broadcaster) Send(event *pb.ServerEvent) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, ch := range b.clients {
		select {
		case ch <- event:
		default:
		}
	}
}

// ClientCount returns the number of connected clients.
func (b *Broadcaster) ClientCount() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.clients)
}
