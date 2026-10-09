package ingest

import (
	"fmt"
	"sync"

	"route/internal/api/proto"
)

type StreamWrapper struct {
	mu     sync.Mutex
	stream proto.Courier_SessionServer
}

func (sw *StreamWrapper) Send(msg *proto.ServerMessage) error {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	return sw.stream.Send(msg)
}

type StreamRegistry struct {
	mu      sync.RWMutex
	streams map[string]*StreamWrapper
}

func NewStreamRegistry() *StreamRegistry {
	return &StreamRegistry{
		streams: make(map[string]*StreamWrapper),
	}
}

func (r *StreamRegistry) Register(courierID string, stream proto.Courier_SessionServer) *StreamWrapper {
	r.mu.Lock()
	defer r.mu.Unlock()
	sw := &StreamWrapper{stream: stream}
	r.streams[courierID] = sw
	return sw
}

func (r *StreamRegistry) Unregister(courierID string, sw *StreamWrapper) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if current, ok := r.streams[courierID]; ok && current == sw {
		delete(r.streams, courierID)
	}
}

// HasStream reports whether courierID currently has a live stream, so a
// caller can avoid creating an offer for a courier that has no way to
// receive it.
func (r *StreamRegistry) HasStream(courierID string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.streams[courierID]
	return ok
}

func (r *StreamRegistry) SendOffer(courierID string, offer *proto.Offer) error {
	r.mu.RLock()
	sw, ok := r.streams[courierID]
	r.mu.RUnlock()

	if !ok {
		return fmt.Errorf("courier disconnected") // Client disconnected
	}

	return sw.Send(&proto.ServerMessage{
		Payload: &proto.ServerMessage_Offer{
			Offer: offer,
		},
	})
}
