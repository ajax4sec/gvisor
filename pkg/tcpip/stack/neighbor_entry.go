// Copyright 2020 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package stack

import (
	"fmt"
	"sync"
	"time"

	"gvisor.dev/gvisor/pkg/sleep"
	"gvisor.dev/gvisor/pkg/tcpip"
)

// NeighborEntry describes a neighboring device in the local network.
type NeighborEntry struct {
	Addr      tcpip.Address
	LocalAddr tcpip.Address
	LinkAddr  tcpip.LinkAddress
	State     NeighborState
	UpdatedAt time.Time
}

// NeighborState defines the state of a NeighborEntry within the Neighbor
// Unreachability Detection state machine, as per RFC 4861 section 7.3.2.
type NeighborState uint8

const (
	// Unknown means reachability has not been verified yet. This is the initial
	// state of entries that have been created automatically by the Neighbor
	// Unreachability Detection state machine.
	Unknown NeighborState = iota
	// Incomplete means that there is an outstanding request to resolve the address.
	// This is the initial state.
	Incomplete
	// Reachable means the path to the neighbor is functioning properly for both
	// receive and transmit paths.
	Reachable
	// Stale means reachability to the neighbor is unknown, but packets are still
	// able to be transmitted to the possibly stale link address.
	Stale
	// Delay means reachability to the neighbor is unknown and pending
	// confirmation from an upper-level protocol like TCP, but packets are still
	// able to be transmitted to the possibly stale link address.
	Delay
	// Probe means a reachability confirmation is actively being sought by
	// periodically retransmitting reachability probes until a reachability
	// confirmation is received, or until the max amount of probes has been sent.
	Probe
	// Static describes entries that have been explicitly added by the user.
	// They do not expire and are not deleted until explicitly removed.
	Static
	// Failed means traffic should not be sent to this neighbor since attempts of
	// reachability have returned inconclusive.
	Failed
)

// neighborEntry implements a neighbor entry's individual node behavior, as per
// RFC 4861 section 7.3.3. Neighbor Unreachability Detection operates in
// parallel with the sending of packets to a neighbor, necessitating the
// entry's lock to be acquired for all operations.
type neighborEntry struct {
	neighborEntryEntry

	nic      *NIC
	protocol tcpip.NetworkProtocolNumber

	mu struct {
		sync.RWMutex
		neigh NeighborEntry

		// linkRes provides the functionality to send reachability probes, used in
		// Neighbor Unreachability Detection.
		linkRes LinkAddressResolver

		// wakers is a set of waiters for address resolution result. Anytime state
		// transitions out of incomplete these waiters are notified. It is nil iff
		// address resolution is ongoing and no clients are waiting for the result.
		wakers map[*sleep.Waker]struct{}

		// done is used to allow callers to wait on address resolution. It is nil
		// iff nudState is not Reachable and address resolution is not yet in progress.
		done chan struct{}

		isRouter     bool
		timer        tcpip.CancellableTimer
		retryCounter uint32
	}

	// nudState points to the Neighbor Unreachability Detection configuration.
	nudState *NUDState
}

// newNeighborEntry creates a neighbor cache entry starting at the default
// state, Unknown. Transition out of Unknown by calling either
// `handlePacketQueuedLocked` or `handleProbeLocked` on the newly created
// neighborEntry.
func newNeighborEntry(nic *NIC, remoteAddr tcpip.Address, localAddr tcpip.Address, nudState *NUDState, linkRes LinkAddressResolver) *neighborEntry {
	e := &neighborEntry{
		nic:      nic,
		nudState: nudState,
	}
	e.mu.neigh = NeighborEntry{
		Addr:      remoteAddr,
		LocalAddr: localAddr,
		State:     Unknown,
	}
	e.mu.linkRes = linkRes
	return e
}

func newStaticNeighborEntry(nic *NIC, addr tcpip.Address, linkAddr tcpip.LinkAddress, state *NUDState) *neighborEntry {
	if nic.stack.nudDisp != nil {
		nic.stack.nudDisp.OnNeighborAdded(nic.id, addr, linkAddr, Static)
	}
	e := &neighborEntry{
		nic:      nic,
		nudState: state,
	}
	e.mu.neigh = NeighborEntry{
		Addr:      addr,
		LinkAddr:  linkAddr,
		State:     Static,
		UpdatedAt: time.Now(),
	}
	return e
}

// notifyWakersLocked notifies those waiting for address resolution to resolve,
// whether it succeeded or failed. Assumes the entry has already been
// appropriately locked.
func (e *neighborEntry) notifyWakersLocked() {
	for w := range e.mu.wakers {
		w.Assert()
	}
	e.mu.wakers = nil
	if ch := e.mu.done; ch != nil {
		close(ch)
		e.mu.done = nil
	}
}

func (e *neighborEntry) dispatchEventAdded(s NeighborState) {
	if e.nic.stack.nudDisp == nil {
		return
	}
	e.nic.stack.nudDisp.OnNeighborAdded(e.nic.id, e.mu.neigh.Addr, e.mu.neigh.LinkAddr, s)
}

func (e *neighborEntry) dispatchEventStateChange(s NeighborState) {
	if e.nic.stack.nudDisp == nil {
		return
	}
	e.nic.stack.nudDisp.OnNeighborStateChange(e.nic.id, e.mu.neigh.Addr, e.mu.neigh.LinkAddr, s)
}

func (e *neighborEntry) dispatchEventRemoved(s NeighborState) {
	if e.nic.stack.nudDisp == nil {
		return
	}
	e.nic.stack.nudDisp.OnNeighborRemoved(e.nic.id, e.mu.neigh.Addr, e.mu.neigh.LinkAddr, s)
}

// setStateLocked transitions the entry into state s immediately.
//
// Follows the logic defined in RFC 4861 section 7.3.3.
//
// e.mu MUST be locked.
func (e *neighborEntry) setStateLocked(s NeighborState) {
	e.mu.timer.StopLocked()

	config := e.nudState.Config()

	switch s {
	case Incomplete:
		// For this state, the counter is retry used to count how many broadcast
		// probes have been sent since the initial transition to Incomplete.
		if e.mu.retryCounter >= config.MaxMulticastProbes {
			// "If no Neighbor Advertisement is received after MAX_MULTICAST_SOLICIT
			// solicitations, address resolution has failed. The sender MUST return
			// ICMP destination unreachable indications with code 3 (Address
			// Unreachable) for each packet queued awaiting address resolution."
			//    - RFC 4861 section 7.2.2
			//
			// There is no need to send an ICMP destination unreachable indiciation
			// since the failure to resolve the address is expected to only occur on
			// this node. Thus, redirecting traffic is currently not supported.
			//
			// "If the error occurs on a node other than the node originating the
			// packet, an ICMP error message is generated. If the error occurs on the
			// originating node, an implementation is not required to actually create
			// and send an ICMP error packet to the source, as long as the
			// upper-layer sender is notified through an appropriate mechanism (e.g.,
			// return value from a procedure call). Note, however, that an
			// implementation may find it convenient in some cases to return errors
			// to the sender by taking the offending packet, generating an ICMP error
			// message, and then delivering it (locally) through the generic error-
			// handling routines.'
			//    - RFC 4861 section 2.1
			e.dispatchEventRemoved(Incomplete)
			e.setStateLocked(Failed)
			return
		}

		if err := e.mu.linkRes.LinkAddressRequest(e.mu.neigh.Addr, e.mu.neigh.LocalAddr, "", e.nic.linkEP); err != nil {
			e.dispatchEventRemoved(Incomplete)
			e.setStateLocked(Failed)
			return
		}

		e.mu.retryCounter++
		e.mu.timer = tcpip.MakeCancellableTimer(&e.mu, func() {
			e.setStateLocked(Incomplete)
		})
		e.mu.timer.Reset(config.RetransmitTimer)

	case Reachable:
		e.mu.timer = tcpip.MakeCancellableTimer(&e.mu, func() {
			e.dispatchEventStateChange(Stale)
			e.setStateLocked(Stale)
		})
		e.mu.timer.Reset(e.nudState.ReachableTime())

	case Delay:
		// The retry counter is reused to count how many unicast probes have been
		// sent during the Probe state.
		e.mu.retryCounter = 0
		e.mu.timer = tcpip.MakeCancellableTimer(&e.mu, func() {
			e.dispatchEventStateChange(Probe)
			e.setStateLocked(Probe)
		})
		e.mu.timer.Reset(config.DelayFirstProbeTime)

	case Probe:
		if e.mu.retryCounter >= config.MaxUnicastProbes {
			e.dispatchEventRemoved(Probe)
			e.setStateLocked(Failed)
			return
		}

		if err := e.mu.linkRes.LinkAddressRequest(e.mu.neigh.Addr, e.mu.neigh.LocalAddr, e.mu.neigh.LinkAddr, e.nic.linkEP); err != nil {
			e.dispatchEventRemoved(Probe)
			e.setStateLocked(Failed)
			return
		}

		e.mu.retryCounter++
		e.mu.timer = tcpip.MakeCancellableTimer(&e.mu, func() {
			e.setStateLocked(Probe)
		})
		e.mu.timer.Reset(config.RetransmitTimer)

	case Failed:
		e.notifyWakersLocked()
		e.mu.retryCounter = 0

		// When the neighbor cache is torn down, the timer running setState does
		// not stop. An additional check is necessary to avoid a nil pointer
		// reference for when the NUDConfigurations is garbage collected.
		if e.nudState != nil {
			e.mu.timer = tcpip.MakeCancellableTimer(&e.mu, func() {
				e.setStateLocked(Unknown)
			})
			e.mu.timer.Reset(config.UnreachableTime)
		}

	case Unknown, Stale, Static:
		// Do nothing

	default:
		panic(fmt.Sprintf("Invalid state transition from %q to %q", e.mu.neigh.State, s))
	}

	if e.mu.neigh.State != s {
		e.mu.neigh.State = s
		e.mu.neigh.UpdatedAt = time.Now()
	}
}

// handlePacketQueuedLocked advances the state machine according to a packet being
// queued for outgoing transmission.
//
// Follows the logic defined in RFC 4861 section 7.3.3.
func (e *neighborEntry) handlePacketQueuedLocked(linkRes LinkAddressResolver) {
	e.mu.linkRes = linkRes
	switch e.mu.neigh.State {
	case Unknown:
		e.dispatchEventAdded(Incomplete)
		e.setStateLocked(Incomplete)

	case Stale:
		e.dispatchEventStateChange(Delay)
		e.setStateLocked(Delay)

	case Incomplete, Reachable, Delay, Probe, Static, Failed:
		// Do nothing

	default:
		panic(fmt.Sprintf("Invalid cache entry state: %s", e.mu.neigh.State))
	}
}

// handleProbeLocked processes an incoming neighbor probe (e.g. ARP request or
// Neighbor Solicitation for ARP or NDP, respectively).
//
// Follows the logic defined in RFC 4861 section 7.2.3.
func (e *neighborEntry) handleProbeLocked(remoteLinkAddr tcpip.LinkAddress) {
	// Probes MUST be silently discarded if the target address is tentative, does
	// not exist, or not bound to the NIC as per RFC 4861 section 7.2.3. These
	// checks MUST be done by the NetworkEndpoint.

	switch e.mu.neigh.State {
	case Unknown, Incomplete, Failed:
		e.mu.neigh.LinkAddr = remoteLinkAddr
		e.dispatchEventAdded(Stale)
		e.setStateLocked(Stale)
		e.notifyWakersLocked()

	case Reachable, Delay, Probe:
		if e.mu.neigh.LinkAddr != remoteLinkAddr {
			e.mu.neigh.LinkAddr = remoteLinkAddr
			e.dispatchEventStateChange(Stale)
			e.setStateLocked(Stale)
		}

	case Stale:
		if e.mu.neigh.LinkAddr != remoteLinkAddr {
			e.mu.neigh.LinkAddr = remoteLinkAddr
			e.dispatchEventStateChange(Stale)
		}

	case Static:
		// Do nothing

	default:
		panic(fmt.Sprintf("Invalid cache entry state: %s", e.mu.neigh.State))
	}
}

// handleConfirmationLocked processes an incoming neighbor confirmation
// (e.g. ARP reply or Neighbor Advertisement for ARP or NDP, respectively).
//
// Follows the state machine defined by RFC 4861 section 7.2.5.
//
// TODO(gvisor.dev/issue/2277): To protect against ARP poisoning and other
// attacks against NDP functions, Secure Neighbor Discovery (SEND) Protocol
// should be deployed where preventing access to the broadcast segment might
// not be possible. SEND uses RSA key pairs to produce Cryptographically
// Generated Addresses (CGA), as defined in RFC 3972. This ensures that the
// claimed source of an NDP message is the owner of the claimed address.
func (e *neighborEntry) handleConfirmationLocked(linkAddr tcpip.LinkAddress, solicited, override, isRouter bool) {
	switch e.mu.neigh.State {
	case Incomplete:
		if linkAddr == "" {
			// "If the link layer has addresses and no Target Link-Layer Address
			// option is included, the receiving node SHOULD silently discard the
			// received advertisement." - RFC 4861 section 7.2.5
			break
		}

		e.mu.neigh.LinkAddr = linkAddr
		if solicited {
			e.dispatchEventStateChange(Reachable)
			e.setStateLocked(Reachable)
		} else {
			e.dispatchEventStateChange(Stale)
			e.setStateLocked(Stale)
		}
		e.mu.isRouter = isRouter
		e.notifyWakersLocked()

		// "Note that the Override flag is ignored if the entry is in the INCOMPLETE state."
		//   - RFC 4861 section 7.2.5

	case Reachable, Stale, Delay, Probe:
		sameLinkAddr := e.mu.neigh.LinkAddr == linkAddr
		if !override && !sameLinkAddr {
			if e.mu.neigh.State == Reachable {
				e.dispatchEventStateChange(Stale)
				e.setStateLocked(Stale)
			}
			break
		}

		if override && !sameLinkAddr {
			e.mu.neigh.LinkAddr = linkAddr
		}

		if !solicited && override && !sameLinkAddr {
			if e.mu.neigh.State != Stale {
				e.dispatchEventStateChange(Stale)
				e.setStateLocked(Stale)
			}
			break
		}

		if solicited && (override || sameLinkAddr) {
			if e.mu.neigh.State != Reachable {
				e.dispatchEventStateChange(Reachable)
				// Set state to Reachable again to refresh timers.
			}
			e.setStateLocked(Reachable)
			e.notifyWakersLocked()
		}

		if e.mu.isRouter && !isRouter {
			// "In those cases where the IsRouter flag changes from TRUE to FALSE as a
			// result of this update, the node MUST remove that router from the Default
			// Router List and update the Destination Cache entries for all
			// destinations using that neighbor as a router as specified in Section
			// 7.3.3.  This is needed to detect when a node that is used as a router
			// stops forwarding packets due to being configured as a host."
			//   - RFC 4861 section 7.2.5
			e.nic.mu.Lock()
			e.nic.mu.ndp.invalidateDefaultRouter(e.mu.neigh.Addr)
			e.nic.mu.Unlock()
		}
		e.mu.isRouter = isRouter

	case Unknown, Failed, Static:
		// Do nothing

	default:
		panic(fmt.Sprintf("Invalid cache entry state: %s", e.mu.neigh.State))
	}
}

// handleUpperLevelConfirmationLocked processes an incoming upper-level protocol
// (e.g. TCP acknowledgements) reachability confirmation.
func (e *neighborEntry) handleUpperLevelConfirmationLocked() {
	switch e.mu.neigh.State {
	case Reachable, Stale, Delay, Probe:
		if e.mu.neigh.State != Reachable {
			e.dispatchEventStateChange(Reachable)
			// Set state to Reachable again to refresh timers.
		}
		e.setStateLocked(Reachable)

	case Incomplete, Static:
		// Do nothing

	case Unknown, Failed:
		// There shouldn't be any upper-level protocols using a neighbor entry in
		// an invalid state.
		fallthrough
	default:
		panic(fmt.Sprintf("Invalid cache entry state: %s", e.mu.neigh.State))
	}
}
