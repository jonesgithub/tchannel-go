// Copyright (c) 2015 Uber Technologies, Inc.

// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package tchannel

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/uber/tchannel-go/atomic"
	"github.com/uber/tchannel-go/relay"
)

// _maxRelayTombs is the maximum number of tombs we'll accumulate in a single
// relayItems.
const _maxRelayTombs = 1e4

// _relayTombTTL is the length of time we'll keep a tomb before GC'ing it.
const _relayTombTTL = time.Second

type relayItem struct {
	*time.Timer

	remapID     uint32
	destination *Relayer
	tomb        bool
}

type relayItems struct {
	sync.RWMutex

	logger Logger
	tombs  uint64
	items  map[uint32]relayItem
}

func newRelayItems(logger Logger) *relayItems {
	return &relayItems{
		items:  make(map[uint32]relayItem),
		logger: logger,
	}
}

// Count returns the number of non-tombstone items in the relay.
func (r *relayItems) Count() int {
	r.RLock()
	n := len(r.items) - int(r.tombs)
	r.RUnlock()
	return n
}

// Get checks for a relay item by ID, returning the item and a bool indicating
// whether the item was found.
func (r *relayItems) Get(id uint32) (relayItem, bool) {
	r.RLock()
	item, ok := r.items[id]
	r.RUnlock()

	return item, ok
}

// Add adds a relay item.
func (r *relayItems) Add(id uint32, item relayItem) {
	r.Lock()
	r.items[id] = item
	r.Unlock()
}

// Delete removes a relayItem completely (without leaving a tombstone). It returns
// a bool indicating whether we completed a relayed call.
func (r *relayItems) Delete(id uint32) bool {
	r.Lock()
	item, ok := r.items[id]
	if !ok {
		r.Unlock()
		r.logger.WithFields(LogField{"id", id}).Warn("Attempted to delete non-existent relay item.")
		return false
	}
	delete(r.items, id)
	if item.tomb {
		r.tombs--
	}
	r.Unlock()

	item.Stop()
	return !item.tomb
}

// Entomb sets the tomb bit on a relayItem and schedules a garbage collection. It
// returns a bool indicating whether we completed a relayed call.
func (r *relayItems) Entomb(id uint32, deleteAfter time.Duration) bool {
	r.Lock()
	if r.tombs > _maxRelayTombs {
		r.Unlock()
		r.logger.WithFields(LogField{"id", id}).Warn("Too many tombstones, deleting relay item immediately.")
		return false
	}
	item, ok := r.items[id]
	if !ok {
		r.Unlock()
		r.logger.WithFields(LogField{"id", id}).Warn("Can't find relay item to entomb.")
		return false
	}
	if item.tomb {
		r.Unlock()
		r.logger.WithFields(LogField{"id", id}).Warn("Re-entombing a tombstone.")
		return false
	}
	r.tombs++
	item.tomb = true
	r.items[id] = item
	r.Unlock()

	// TODO: We should be clearing these out in batches, rather than creating
	// individual timers for each item.
	time.AfterFunc(deleteAfter, func() { r.Delete(id) })
	return true
}

type frameType int

const (
	requestFrame  frameType = 0
	responseFrame frameType = 1
)

// A Relayer forwards frames.
type Relayer struct {
	metrics StatsReporter
	hosts   relay.Hosts

	// outbound is the remapping for requests that originated on this
	// connection, and are outbound towards some other connection.
	// It stores remappings for all request frames read on this connection.
	outbound *relayItems

	// inbound is the remapping for requests that originated on some other
	// connection which was directed to this connection.
	// It stores remappings for all response frames read on this connection.
	inbound *relayItems

	peers   *PeerList
	conn    *Connection
	logger  Logger
	pending atomic.Uint32
}

// NewRelayer constructs a Relayer.
func NewRelayer(ch *Channel, conn *Connection) *Relayer {
	return &Relayer{
		metrics:  conn.statsReporter,
		hosts:    ch.RelayHosts(),
		outbound: newRelayItems(ch.Logger().WithFields(LogField{"relay", "outbound"})),
		inbound:  newRelayItems(ch.Logger().WithFields(LogField{"relay", "inbound"})),
		peers:    ch.Peers(),
		conn:     conn,
		logger:   conn.log,
	}
}

// Hosts returns the RelayHosts guiding peer selection.
func (r *Relayer) Hosts() relay.Hosts {
	return r.hosts
}

// Relay is called for each frame that is read on the connection.
func (r *Relayer) Relay(f *Frame) error {
	if f.messageType() != messageTypeCallReq {
		return r.handleNonCallReq(f)
	}
	return r.handleCallReq(newLazyCallReq(f))
}

// Receive receives frames intended for this connection.
func (r *Relayer) Receive(f *Frame, fType frameType) {
	{
		// TODO: Since this block is only checking for safety, we should not
		// enable this in production builds.

		// If we receive a response frame, we expect to find that ID in our outbound.
		// If we receive a request frame, we expect to find that ID in our inbound.
		items := r.receiverItems(fType)

		if _, ok := items.Get(f.Header.ID); !ok {
			r.logger.WithFields(
				LogField{"ID", f.Header.ID},
			).Warn("Received a frame without a RelayItem.")
		}
	}

	r.conn.sendCh <- f
	if finishesCall(f) {
		items := r.receiverItems(fType)
		r.finishRelayItem(items, f.Header.ID)
	}
}

func (r *Relayer) canHandleNewCall() bool {
	var canHandle bool
	r.conn.withStateRLock(func() error {
		canHandle = r.conn.state == connectionActive
		if canHandle {
			r.pending.Inc()
		}
		return nil
	})
	return canHandle
}

func (r *Relayer) getDestination(f lazyCallReq) (*Connection, bool, error) {
	if _, ok := r.outbound.Get(f.Header.ID); ok {
		r.logger.WithFields(LogField{"id", f.Header.ID}).Warn("received duplicate callReq")
		// TODO: this is a protocol error, kill the connection.
		return nil, false, errors.New("callReq with already active ID")
	}

	// Get the destination
	hostPort := r.hosts.Get(f)
	if hostPort == "" {
		// TODO: What is the span in the error frame actually used for, and do we need it?
		r.conn.SendSystemError(f.Header.ID, nil, errUnknownGroup(f.Service()))
		return nil, false, nil
	}
	peer := r.peers.GetOrAdd(hostPort)

	// TODO: Should connections use the call timeout? Or a separate timeout?
	remoteConn, err := peer.getConnectionTimeout(f.TTL())
	if err != nil {
		r.logger.WithFields(
			ErrField(err),
			LogField{"hostPort", hostPort},
		).Warn("Failed to connect to relay host.")
		// TODO: Same as above, do we need span here?
		r.conn.SendSystemError(f.Header.ID, nil, NewWrappedSystemError(ErrCodeNetwork, err))
		return nil, false, nil
	}

	return remoteConn, true, nil
}

func (r *Relayer) handleCallReq(f lazyCallReq) error {
	if !r.canHandleNewCall() {
		return ErrChannelClosed
	}

	// Get a remote connection and check whether it can handle this call.
	remoteConn, ok, err := r.getDestination(f)
	if err == nil && ok {
		if !remoteConn.relay.canHandleNewCall() {
			err = NewSystemError(ErrCodeNetwork, "selected closed connection, retry")
		}
	}
	if err != nil || !ok {
		// Failed to get a remote connection, or the connection is not in the right
		// state to handle this call. Since we already incremented pending on
		// the current relay, we need to decrement it.
		r.decrementPending()
		return err
	}

	destinationID := remoteConn.NextMessageID()
	ttl := f.TTL()
	remoteConn.relay.addRelayItem(false /* isOriginator */, destinationID, f.Header.ID, r, ttl)
	r.metrics.IncCounter("relay", nil, 1)
	relayToDest := r.addRelayItem(true /* isOriginator */, f.Header.ID, destinationID, remoteConn.relay, ttl)

	f.Header.ID = destinationID
	relayToDest.destination.Receive(f.Frame, requestFrame)
	return nil
}

// Handle all frames except messageTypeCallReq.
func (r *Relayer) handleNonCallReq(f *Frame) error {
	frameType := frameTypeFor(f)

	// If we read a request frame, we need to use the outbound map to decide
	// the destination. Otherwise, we use the inbound map.
	items := r.outbound
	if frameType == responseFrame {
		items = r.inbound
	}

	item, ok := items.Get(f.Header.ID)
	if !ok {
		return errors.New("non-callReq for inactive ID")
	}
	if item.tomb {
		// Call timed out, ignore this frame.
		// TODO: Add metrics for this case.
		return nil
	}
	originalID := f.Header.ID
	f.Header.ID = item.remapID
	item.destination.Receive(f, frameType)

	if finishesCall(f) {
		r.finishRelayItem(items, originalID)
	}
	return nil
}

// addRelayItem adds a relay item to either outbound or inbound.
func (r *Relayer) addRelayItem(isOriginator bool, id, remapID uint32, destination *Relayer, ttl time.Duration) relayItem {
	item := relayItem{
		remapID:     remapID,
		destination: destination,
	}

	items := r.inbound
	if isOriginator {
		items = r.outbound
	}
	item.Timer = time.AfterFunc(ttl, func() { r.timeoutRelayItem(items, id, isOriginator) })
	items.Add(id, item)
	return item
}

func (r *Relayer) timeoutRelayItem(items *relayItems, id uint32, isOriginator bool) {
	if ok := items.Entomb(id, _relayTombTTL); !ok {
		return
	}
	if isOriginator {
		// TODO: As above. What's the span in the error frame for?
		r.conn.SendSystemError(id, nil, ErrTimeout)
	}

	r.decrementPending()
}

func (r *Relayer) finishRelayItem(items *relayItems, id uint32) {
	if ok := items.Delete(id); !ok {
		return
	}

	r.decrementPending()
}

func (r *Relayer) decrementPending() {
	r.pending.Dec()
	r.conn.checkExchanges()
}

func (r *Relayer) canClose() bool {
	if r == nil {
		return true
	}
	return r.countPending() == 0
}

func (r *Relayer) countPending() uint32 {
	return r.pending.Load()
}

func (r *Relayer) receiverItems(fType frameType) *relayItems {
	if fType == requestFrame {
		return r.inbound
	}
	return r.outbound
}

func frameTypeFor(f *Frame) frameType {
	switch t := f.Header.messageType; t {
	case messageTypeCallRes, messageTypeCallResContinue, messageTypeError, messageTypePingRes:
		return responseFrame
	case messageTypeCallReq, messageTypeCallReqContinue, messageTypePingReq:
		return requestFrame
	default:
		panic(fmt.Sprintf("unsupported frame type: %v", t))
	}
}

func errUnknownGroup(group string) error {
	return NewSystemError(ErrCodeDeclined, "no peers for %q", group)
}
