// Copyright 2014 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.
//
// Author: Spencer Kimball (spencer.kimball@gmail.com)

package gossip

import (
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/pkg/errors"
	circuit "github.com/rubyist/circuitbreaker"
	"golang.org/x/net/context"

	"github.com/cockroachdb/cockroach/roachpb"
	"github.com/cockroachdb/cockroach/rpc"
	"github.com/cockroachdb/cockroach/util"
	"github.com/cockroachdb/cockroach/util/grpcutil"
	"github.com/cockroachdb/cockroach/util/log"
	"github.com/cockroachdb/cockroach/util/stop"
	"github.com/cockroachdb/cockroach/util/timeutil"
)

// client is a client-side RPC connection to a gossip peer node.
type client struct {
	ctx                   context.Context
	createdAt             time.Time
	peerID                roachpb.NodeID           // Peer node ID; 0 until first gossip response
	addr                  net.Addr                 // Peer node network address
	forwardAddr           *util.UnresolvedAddr     // Set if disconnected with an alternate addr
	remoteHighWaterStamps map[roachpb.NodeID]int64 // Remote server's high water timestamps
	closer                chan struct{}            // Client shutdown channel
	clientMetrics         Metrics
	nodeMetrics           Metrics
}

// extractKeys returns a string representation of a gossip delta's keys.
func extractKeys(delta map[string]*Info) string {
	keys := make([]string, 0, len(delta))
	for key := range delta {
		keys = append(keys, key)
	}
	return fmt.Sprintf("%s", keys)
}

// newClient creates and returns a client struct.
func newClient(ctx context.Context, addr net.Addr, nodeMetrics Metrics) *client {
	return &client{
		ctx:       ctx,
		createdAt: timeutil.Now(),
		addr:      addr,
		remoteHighWaterStamps: map[roachpb.NodeID]int64{},
		closer:                make(chan struct{}),
		clientMetrics:         makeMetrics(),
		nodeMetrics:           nodeMetrics,
	}
}

// start dials the remote addr and commences gossip once connected. Upon exit,
// the client is sent on the disconnected channel. This method starts client
// processing in a goroutine and returns immediately.
func (c *client) start(
	g *Gossip,
	disconnected chan *client,
	rpcCtx *rpc.Context,
	stopper *stop.Stopper,
	nodeID roachpb.NodeID,
	breaker *circuit.Breaker,
) {
	stopper.RunWorker(func() {
		ctx, cancel := context.WithCancel(c.ctx)
		var wg sync.WaitGroup
		defer func() {
			// This closes the outgoing stream, causing any attempt to send or
			// receive to return an error.
			//
			// Note: it is still possible for incoming gossip to be processed after
			// this point.
			cancel()

			// The stream is closed, but there may still be some incoming gossip
			// being processed. Wait until that is complete to avoid racing the
			// client's removal against the discovery of its remote's node ID.
			wg.Wait()
			disconnected <- c
		}()

		consecFailures := breaker.ConsecFailures()
		var stream Gossip_GossipClient
		if err := breaker.Call(func() error {
			// Note: avoid using `grpc.WithBlock` here. This code is already
			// asynchronous from the caller's perspective, so the only effect of
			// `WithBlock` here is blocking shutdown - at the time of this writing,
			// that ends ups up making `kv` tests take twice as long.
			conn, err := rpcCtx.GRPCDial(c.addr.String())
			if err != nil {
				return err
			}
			if stream, err = NewGossipClient(conn).Gossip(ctx); err != nil {
				return err
			}
			return c.requestGossip(g, stream)
		}, 0); err != nil {
			if consecFailures == 0 {
				log.Warningf(ctx, "node %d: failed to start gossip client: %s", nodeID, err)
			}
			return
		}

		// Start gossiping.
		log.Infof(ctx, "node %d: started gossip client to %s", nodeID, c.addr)
		if err := c.gossip(ctx, g, stream, stopper, &wg); err != nil {
			if !grpcutil.IsClosedConnection(err) {
				g.mu.Lock()
				if c.peerID != 0 {
					log.Infof(ctx, "node %d: closing client to node %d (%s): %s", nodeID, c.peerID, c.addr, err)
				} else {
					log.Infof(ctx, "node %d: closing client to %s: %s", nodeID, c.addr, err)
				}
				g.mu.Unlock()
			}
		}
	})
}

// close stops the client gossip loop and returns immediately.
func (c *client) close() {
	select {
	case <-c.closer:
	default:
		close(c.closer)
	}
}

// requestGossip requests the latest gossip from the remote server by
// supplying a map of this node's knowledge of other nodes' high water
// timestamps.
func (c *client) requestGossip(g *Gossip, stream Gossip_GossipClient) error {
	g.mu.Lock()
	args := &Request{
		NodeID:          g.mu.is.NodeID,
		Addr:            g.mu.is.NodeAddr,
		HighWaterStamps: g.mu.is.getHighWaterStamps(),
	}
	g.mu.Unlock()

	bytesSent := int64(args.Size())
	c.clientMetrics.BytesSent.Add(bytesSent)
	c.nodeMetrics.BytesSent.Add(bytesSent)

	return stream.Send(args)
}

// sendGossip sends the latest gossip to the remote server, based on
// the remote server's notion of other nodes' high water timestamps.
func (c *client) sendGossip(g *Gossip, stream Gossip_GossipClient) error {
	g.mu.Lock()
	if delta := g.mu.is.delta(c.remoteHighWaterStamps); len(delta) > 0 {
		args := Request{
			NodeID:          g.mu.is.NodeID,
			Addr:            g.mu.is.NodeAddr,
			Delta:           delta,
			HighWaterStamps: g.mu.is.getHighWaterStamps(),
		}

		bytesSent := int64(args.Size())
		infosSent := int64(len(delta))
		c.clientMetrics.BytesSent.Add(bytesSent)
		c.clientMetrics.InfosSent.Add(infosSent)
		c.nodeMetrics.BytesSent.Add(bytesSent)
		c.nodeMetrics.InfosSent.Add(infosSent)

		if log.V(1) {
			if c.peerID != 0 {
				log.Infof(c.ctx, "node %d: sending %s to node %d (%s)", g.mu.is.NodeID, extractKeys(args.Delta), c.peerID, c.addr)
			} else {
				log.Infof(c.ctx, "node %d: sending %s to %s", g.mu.is.NodeID, extractKeys(args.Delta), c.addr)
			}
		}

		g.mu.Unlock()
		return stream.Send(&args)
	}
	g.mu.Unlock()
	return nil
}

// handleResponse handles errors, remote forwarding, and combines delta
// gossip infos from the remote server with this node's infostore.
func (c *client) handleResponse(g *Gossip, reply *Response) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	bytesReceived := int64(reply.Size())
	infosReceived := int64(len(reply.Delta))
	c.clientMetrics.BytesReceived.Add(bytesReceived)
	c.clientMetrics.InfosReceived.Add(infosReceived)
	c.nodeMetrics.BytesReceived.Add(bytesReceived)
	c.nodeMetrics.InfosReceived.Add(infosReceived)

	// Combine remote node's infostore delta with ours.
	if reply.Delta != nil {
		freshCount, err := g.mu.is.combine(reply.Delta, reply.NodeID)
		if err != nil {
			log.Warningf(c.ctx, "node %d: failed to fully combine delta from node %d: %s", g.mu.is.NodeID, reply.NodeID, err)
		}
		if infoCount := len(reply.Delta); infoCount > 0 {
			if log.V(1) {
				log.Infof(c.ctx, "node %d: received %s from node %d (%d fresh)", g.mu.is.NodeID, extractKeys(reply.Delta), reply.NodeID, freshCount)
			}
		}
		g.maybeTightenLocked()
	}
	c.peerID = reply.NodeID
	g.outgoing.addNode(c.peerID)
	c.remoteHighWaterStamps = reply.HighWaterStamps

	// Handle remote forwarding.
	if reply.AlternateAddr != nil {
		if g.hasIncomingLocked(reply.AlternateNodeID) || g.hasOutgoingLocked(reply.AlternateNodeID) {
			return errors.Errorf("received forward from node %d to %d (%s); already have active connection, skipping",
				reply.NodeID, reply.AlternateNodeID, reply.AlternateAddr)
		}
		// We try to resolve the address, but don't actually use the result.
		// The certificates (if any) may only be valid for the unresolved
		// address.
		if _, err := reply.AlternateAddr.Resolve(); err != nil {
			return errors.Errorf("unable to resolve alternate address %s for node %d: %s", reply.AlternateAddr, reply.AlternateNodeID, err)
		}
		c.forwardAddr = reply.AlternateAddr
		return errors.Errorf("received forward from node %d to %d (%s)", reply.NodeID, reply.AlternateNodeID, reply.AlternateAddr)
	}

	// Check whether we're connected at this point.
	g.signalConnectedLocked()

	// Check whether this outgoing client is duplicating work already
	// being done by an incoming client, either because an outgoing
	// matches an incoming or the client is connecting to itself.
	if g.mu.is.NodeID == c.peerID {
		return errors.Errorf("stopping outgoing client to node %d (%s); loopback connection", c.peerID, c.addr)
	} else if g.hasIncomingLocked(c.peerID) && g.mu.is.NodeID > c.peerID {
		// To avoid mutual shutdown, we only shutdown our client if our
		// node ID is higher than the peer's.
		return errors.Errorf("stopping outgoing client to node %d (%s); already have incoming", c.peerID, c.addr)
	}

	return nil
}

// gossip loops, sending deltas of the infostore and receiving deltas
// in turn. If an alternate is proposed on response, the client addr
// is modified and method returns for forwarding by caller.
func (c *client) gossip(
	ctx context.Context,
	g *Gossip,
	stream Gossip_GossipClient,
	stopper *stop.Stopper,
	wg *sync.WaitGroup,
) error {
	sendGossipChan := make(chan struct{}, 1)

	// Register a callback for gossip updates.
	updateCallback := func(_ string, _ roachpb.Value) {
		select {
		case sendGossipChan <- struct{}{}:
		default:
		}
	}
	// Defer calling "undoer" callback returned from registration.
	defer g.RegisterCallback(".*", updateCallback)()

	errCh := make(chan error, 1)
	// This wait group is used to allow the caller to wait until gossip
	// processing is terminated.
	wg.Add(1)
	stopper.RunWorker(func() {
		defer wg.Done()

		errCh <- func() error {
			for {
				reply, err := stream.Recv()
				if err != nil {
					return err
				}
				if err := c.handleResponse(g, reply); err != nil {
					return err
				}
			}
		}()
	})

	for {
		select {
		case <-c.closer:
			return nil
		case <-stopper.ShouldStop():
			return nil
		case err := <-errCh:
			return err
		case <-sendGossipChan:
			if err := c.sendGossip(g, stream); err != nil {
				return err
			}
		}
	}
}
