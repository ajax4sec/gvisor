// Copyright 2019 The gVisor Authors.
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

package ipv6

import (
	"strings"
	"testing"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/buffer"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/icmp"
)

// setupStackAndEndpoint creates a stack with a single NIC with a link-local
// address llladdr and an IPv6 endpoint to a remote with link-local address
// rlladdr
func setupStackAndEndpoint(t *testing.T, llladdr, rlladdr tcpip.Address) (*stack.Stack, stack.NetworkEndpoint) {
	t.Helper()

	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocol{NewProtocol()},
		TransportProtocols: []stack.TransportProtocol{icmp.NewProtocol6()},
	})

	if err := s.CreateNIC(1, &stubLinkEndpoint{}); err != nil {
		t.Fatalf("CreateNIC(_) = %s", err)
	}
	if err := s.AddAddress(1, ProtocolNumber, llladdr); err != nil {
		t.Fatalf("AddAddress(_, %d, %s) = %s", ProtocolNumber, llladdr, err)
	}

	{
		subnet, err := tcpip.NewSubnet(rlladdr, tcpip.AddressMask(strings.Repeat("\xff", len(rlladdr))))
		if err != nil {
			t.Fatal(err)
		}
		s.SetRouteTable(
			[]tcpip.Route{{
				Destination: subnet,
				NIC:         1,
			}},
		)
	}

	netProto := s.NetworkProtocolInstance(ProtocolNumber)
	if netProto == nil {
		t.Fatalf("cannot find protocol instance for network protocol %d", ProtocolNumber)
	}

	ep, err := netProto.NewEndpoint(0, tcpip.AddressWithPrefix{rlladdr, netProto.DefaultPrefixLen()}, &stubNUDHandler{}, &stubDispatcher{}, nil, s)
	if err != nil {
		t.Fatalf("NewEndpoint(_) = _, %s, want = _, nil", err)
	}

	return s, ep
}

// TestNeighorSolicitationWithSourceLinkLayerOption tests that receiving a
// valid NDP NS message with the Source Link Layer Address option results in a
// new entry in the link address cache for the sender of the message.
func TestNeighorSolicitationWithSourceLinkLayerOption(t *testing.T) {
	const nicID = 1

	tests := []struct {
		name             string
		optsBuf          []byte
		expectedLinkAddr tcpip.LinkAddress
	}{
		{
			name:             "Valid",
			optsBuf:          []byte{1, 1, 2, 3, 4, 5, 6, 7},
			expectedLinkAddr: "\x02\x03\x04\x05\x06\x07",
		},
		{
			name:    "Too Small",
			optsBuf: []byte{1, 1, 2, 3, 4, 5, 6},
		},
		{
			name:    "Invalid Length",
			optsBuf: []byte{1, 2, 2, 3, 4, 5, 6, 7},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s := stack.New(stack.Options{
				NetworkProtocols: []stack.NetworkProtocol{NewProtocol()},
			})
			e := channel.New(0, 1280, linkAddr0)
			if err := s.CreateNIC(nicID, e); err != nil {
				t.Fatalf("CreateNIC(%d, _) = %s", nicID, err)
			}
			if err := s.AddAddress(nicID, ProtocolNumber, lladdr0); err != nil {
				t.Fatalf("AddAddress(%d, %d, %s) = %s", nicID, ProtocolNumber, lladdr0, err)
			}

			ndpNSSize := header.ICMPv6NeighborSolicitMinimumSize + len(test.optsBuf)
			hdr := buffer.NewPrependable(header.IPv6MinimumSize + ndpNSSize)
			pkt := header.ICMPv6(hdr.Prepend(ndpNSSize))
			pkt.SetType(header.ICMPv6NeighborSolicit)
			ns := header.NDPNeighborSolicit(pkt.NDPPayload())
			ns.SetTargetAddress(lladdr0)
			opts := ns.Options()
			copy(opts, test.optsBuf)
			pkt.SetChecksum(header.ICMPv6Checksum(pkt, lladdr1, lladdr0, buffer.VectorisedView{}))
			payloadLength := hdr.UsedLength()
			ip := header.IPv6(hdr.Prepend(header.IPv6MinimumSize))
			ip.Encode(&header.IPv6Fields{
				PayloadLength: uint16(payloadLength),
				NextHeader:    uint8(header.ICMPv6ProtocolNumber),
				HopLimit:      255,
				SrcAddr:       lladdr1,
				DstAddr:       lladdr0,
			})

			invalid := s.Stats().ICMP.V6PacketsReceived.Invalid

			// Invalid count should initially be 0.
			if got := invalid.Value(); got != 0 {
				t.Fatalf("got invalid = %d, want = 0", got)
			}

			e.InjectInbound(ProtocolNumber, stack.PacketBuffer{
				Data: hdr.View().ToVectorisedView(),
			})

			var neigh stack.NeighborEntry
			neighbors, err := s.Neighbors(nicID)
			if err != nil {
				t.Errorf("s.Neighbors(%d): %s", nicID, err)
			}
			for _, n := range neighbors {
				if n.Addr == lladdr1 {
					neigh = n
					break
				}
			}

			if neigh.LinkAddr != test.expectedLinkAddr {
				t.Errorf("got link address = %s, want = %s", neigh.LinkAddr, test.expectedLinkAddr)
			}

			if test.expectedLinkAddr != "" {
				if neigh.State != stack.Stale {
					t.Errorf("got NUD state = %s, want = %s", neigh.State, stack.Stale)
				}

				// Invalid count should not have increased.
				if got := invalid.Value(); got != 0 {
					t.Errorf("got invalid = %d, want = 0", got)
				}
			} else {
				// Invalid count should have increased.
				if got := invalid.Value(); got != 1 {
					t.Errorf("got invalid = %d, want = 1", got)
				}
			}
		})
	}
}

// TestNeighorAdvertisementWithTargetLinkLayerOption tests that receiving a
// valid NDP NA message with the Target Link Layer Address option does not
// result in a new entry in the link address cache for the target of the message.
func TestNeighorAdvertisementWithTargetLinkLayerOption(t *testing.T) {
	const nicID = 1

	tests := []struct {
		name    string
		optsBuf []byte
		isValid bool
	}{
		{
			name:    "Valid",
			optsBuf: []byte{2, 1, 2, 3, 4, 5, 6, 7},
			isValid: true,
		},
		{
			name:    "Too Small",
			optsBuf: []byte{2, 1, 2, 3, 4, 5, 6},
		},
		{
			name:    "Invalid Length",
			optsBuf: []byte{2, 2, 2, 3, 4, 5, 6, 7},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s := stack.New(stack.Options{
				NetworkProtocols: []stack.NetworkProtocol{NewProtocol()},
			})
			e := channel.New(0, 1280, linkAddr0)
			if err := s.CreateNIC(nicID, e); err != nil {
				t.Fatalf("CreateNIC(%d, _) = %s", nicID, err)
			}
			if err := s.AddAddress(nicID, ProtocolNumber, lladdr0); err != nil {
				t.Fatalf("AddAddress(%d, %d, %s) = %s", nicID, ProtocolNumber, lladdr0, err)
			}

			ndpNASize := header.ICMPv6NeighborAdvertMinimumSize + len(test.optsBuf)
			hdr := buffer.NewPrependable(header.IPv6MinimumSize + ndpNASize)
			pkt := header.ICMPv6(hdr.Prepend(ndpNASize))
			pkt.SetType(header.ICMPv6NeighborAdvert)
			ns := header.NDPNeighborAdvert(pkt.NDPPayload())
			ns.SetTargetAddress(lladdr1)
			opts := ns.Options()
			copy(opts, test.optsBuf)
			pkt.SetChecksum(header.ICMPv6Checksum(pkt, lladdr1, lladdr0, buffer.VectorisedView{}))
			payloadLength := hdr.UsedLength()
			ip := header.IPv6(hdr.Prepend(header.IPv6MinimumSize))
			ip.Encode(&header.IPv6Fields{
				PayloadLength: uint16(payloadLength),
				NextHeader:    uint8(header.ICMPv6ProtocolNumber),
				HopLimit:      255,
				SrcAddr:       lladdr1,
				DstAddr:       lladdr0,
			})

			invalid := s.Stats().ICMP.V6PacketsReceived.Invalid

			// Invalid count should initially be 0.
			if got := invalid.Value(); got != 0 {
				t.Fatalf("got invalid = %d, want = 0", got)
			}

			e.InjectInbound(ProtocolNumber, stack.PacketBuffer{
				Data: hdr.View().ToVectorisedView(),
			})

			neighbors, err := s.Neighbors(nicID)
			if err != nil {
				t.Errorf("s.Neighbors(%d): %s", nicID, err)
			}

			neighborByAddr := make(map[tcpip.Address]stack.NeighborEntry)
			for _, n := range neighbors {
				neighborByAddr[n.Addr] = n
			}

			_, ok := neighborByAddr[lladdr1]
			if ok {
				t.Errorf("unexpectedly got neighbor entry for %q", lladdr1)
			}

			if test.isValid {
				// Invalid count should not have increased.
				if got := invalid.Value(); got != 0 {
					t.Errorf("got invalid = %d, want = 0", got)
				}
			} else {
				// Invalid count should have increased.
				if got := invalid.Value(); got != 1 {
					t.Errorf("got invalid = %d, want = 1", got)
				}
			}
		})
	}
}

func TestNDPValidation(t *testing.T) {
	setup := func(t *testing.T) (*stack.Stack, stack.NetworkEndpoint, stack.Route) {
		t.Helper()

		// Create a stack with the assigned link-local address lladdr0
		// and an endpoint to lladdr1.
		s, ep := setupStackAndEndpoint(t, lladdr0, lladdr1)

		r, err := s.FindRoute(1, lladdr0, lladdr1, ProtocolNumber, false /* multicastLoop */)
		if err != nil {
			t.Fatalf("FindRoute(_) = _, %s, want = _, nil", err)
		}

		return s, ep, r
	}

	handleIPv6Payload := func(hdr buffer.Prependable, hopLimit uint8, atomicFragment bool, ep stack.NetworkEndpoint, r *stack.Route) {
		nextHdr := uint8(header.ICMPv6ProtocolNumber)
		if atomicFragment {
			bytes := hdr.Prepend(header.IPv6FragmentExtHdrLength)
			bytes[0] = nextHdr
			nextHdr = uint8(header.IPv6FragmentExtHdrIdentifier)
		}

		payloadLength := hdr.UsedLength()
		ip := header.IPv6(hdr.Prepend(header.IPv6MinimumSize))
		ip.Encode(&header.IPv6Fields{
			PayloadLength: uint16(payloadLength),
			NextHeader:    nextHdr,
			HopLimit:      hopLimit,
			SrcAddr:       r.LocalAddress,
			DstAddr:       r.RemoteAddress,
		})
		ep.HandlePacket(r, stack.PacketBuffer{
			Data: hdr.View().ToVectorisedView(),
		})
	}

	var tllData [header.NDPLinkLayerAddressSize]byte
	header.NDPOptions(tllData[:]).Serialize(header.NDPOptionsSerializer{
		header.NDPTargetLinkLayerAddressOption(linkAddr1),
	})

	types := []struct {
		name        string
		typ         header.ICMPv6Type
		size        int
		extraData   []byte
		statCounter func(tcpip.ICMPv6ReceivedPacketStats) *tcpip.StatCounter
		routerOnly  bool
	}{
		{
			name: "RouterSolicit",
			typ:  header.ICMPv6RouterSolicit,
			size: header.ICMPv6MinimumSize,
			statCounter: func(stats tcpip.ICMPv6ReceivedPacketStats) *tcpip.StatCounter {
				return stats.RouterSolicit
			},
			routerOnly: true,
		},
		{
			name: "RouterAdvert",
			typ:  header.ICMPv6RouterAdvert,
			size: header.ICMPv6HeaderSize + header.NDPRAMinimumSize,
			statCounter: func(stats tcpip.ICMPv6ReceivedPacketStats) *tcpip.StatCounter {
				return stats.RouterAdvert
			},
		},
		{
			name: "NeighborSolicit",
			typ:  header.ICMPv6NeighborSolicit,
			size: header.ICMPv6NeighborSolicitMinimumSize,
			statCounter: func(stats tcpip.ICMPv6ReceivedPacketStats) *tcpip.StatCounter {
				return stats.NeighborSolicit
			},
		},
		{
			name:      "NeighborAdvert",
			typ:       header.ICMPv6NeighborAdvert,
			size:      header.ICMPv6NeighborAdvertMinimumSize,
			extraData: tllData[:],
			statCounter: func(stats tcpip.ICMPv6ReceivedPacketStats) *tcpip.StatCounter {
				return stats.NeighborAdvert
			},
		},
		{
			name: "RedirectMsg",
			typ:  header.ICMPv6RedirectMsg,
			size: header.ICMPv6MinimumSize,
			statCounter: func(stats tcpip.ICMPv6ReceivedPacketStats) *tcpip.StatCounter {
				return stats.RedirectMsg
			},
		},
	}

	subTests := []struct {
		name           string
		atomicFragment bool
		hopLimit       uint8
		code           uint8
		valid          bool
	}{
		{
			name:           "Valid",
			atomicFragment: false,
			hopLimit:       header.NDPHopLimit,
			code:           0,
			valid:          true,
		},
		{
			name:           "Fragmented",
			atomicFragment: true,
			hopLimit:       header.NDPHopLimit,
			code:           0,
			valid:          false,
		},
		{
			name:           "Invalid hop limit",
			atomicFragment: false,
			hopLimit:       header.NDPHopLimit - 1,
			code:           0,
			valid:          false,
		},
		{
			name:           "Invalid ICMPv6 code",
			atomicFragment: false,
			hopLimit:       header.NDPHopLimit,
			code:           1,
			valid:          false,
		},
	}

	for _, typ := range types {
		runTest := func(isRouter bool) {
			name := typ.name
			if isRouter {
				name += " (Router)"
			}
			t.Run(name, func(t *testing.T) {
				for _, test := range subTests {
					t.Run(test.name, func(t *testing.T) {
						s, ep, r := setup(t)
						defer r.Release()

						if isRouter {
							s.SetForwarding(true) // act as a router
						}

						stats := s.Stats().ICMP.V6PacketsReceived
						invalid := stats.Invalid
						typStat := typ.statCounter(stats)

						extraDataLen := len(typ.extraData)
						hdr := buffer.NewPrependable(header.IPv6MinimumSize + typ.size + extraDataLen + header.IPv6FragmentExtHdrLength)
						extraData := buffer.View(hdr.Prepend(extraDataLen))
						copy(extraData, typ.extraData)
						pkt := header.ICMPv6(hdr.Prepend(typ.size))
						pkt.SetType(typ.typ)
						pkt.SetCode(test.code)
						pkt.SetChecksum(header.ICMPv6Checksum(pkt, r.LocalAddress, r.RemoteAddress, extraData.ToVectorisedView()))

						// Rx count of the NDP message should initially be 0.
						if got := typStat.Value(); got != 0 {
							t.Errorf("got %s = %d, want = 0", typ.name, got)
						}

						// Invalid count should initially be 0.
						if got := invalid.Value(); got != 0 {
							t.Errorf("got invalid = %d, want = 0", got)
						}

						if t.Failed() {
							t.FailNow()
						}

						handleIPv6Payload(hdr, test.hopLimit, test.atomicFragment, ep, &r)

						// Rx count of the NDP packet should have increased.
						if got := typStat.Value(); got != 1 {
							t.Errorf("got %s = %d, want = 1", typ.name, got)
						}

						want := uint64(0)
						if !test.valid || (!isRouter && typ.routerOnly) {
							// Invalid count should have increased.
							want = 1
						}
						if got := invalid.Value(); got != want {
							t.Errorf("got invalid = %d, want = %d", got, want)
						}
					})
				}
			})
		}

		runTest(false /* isRouter */)
		runTest(true /* isRouter */)
	}
}

// TestRouterAdvertValidation tests that when the NIC is configured to handle
// NDP Router Advertisement packets, it validates the Router Advertisement
// properly before handling them.
func TestRouterAdvertValidation(t *testing.T) {
	tests := []struct {
		name            string
		src             tcpip.Address
		hopLimit        uint8
		code            uint8
		ndpPayload      []byte
		expectedSuccess bool
	}{
		{
			"OK",
			lladdr0,
			255,
			0,
			[]byte{
				0, 0, 0, 0,
				0, 0, 0, 0,
				0, 0, 0, 0,
			},
			true,
		},
		{
			"NonLinkLocalSourceAddr",
			addr1,
			255,
			0,
			[]byte{
				0, 0, 0, 0,
				0, 0, 0, 0,
				0, 0, 0, 0,
			},
			false,
		},
		{
			"HopLimitNot255",
			lladdr0,
			254,
			0,
			[]byte{
				0, 0, 0, 0,
				0, 0, 0, 0,
				0, 0, 0, 0,
			},
			false,
		},
		{
			"NonZeroCode",
			lladdr0,
			255,
			1,
			[]byte{
				0, 0, 0, 0,
				0, 0, 0, 0,
				0, 0, 0, 0,
			},
			false,
		},
		{
			"NDPPayloadTooSmall",
			lladdr0,
			255,
			0,
			[]byte{
				0, 0, 0, 0,
				0, 0, 0, 0,
				0, 0, 0,
			},
			false,
		},
		{
			"OKWithOptions",
			lladdr0,
			255,
			0,
			[]byte{
				// RA payload
				0, 0, 0, 0,
				0, 0, 0, 0,
				0, 0, 0, 0,

				// Option #1 (TargetLinkLayerAddress)
				2, 1, 0, 0, 0, 0, 0, 0,

				// Option #2 (unrecognized)
				255, 1, 0, 0, 0, 0, 0, 0,

				// Option #3 (PrefixInformation)
				3, 4, 0, 0, 0, 0, 0, 0,
				0, 0, 0, 0, 0, 0, 0, 0,
				0, 0, 0, 0, 0, 0, 0, 0,
				0, 0, 0, 0, 0, 0, 0, 0,
			},
			true,
		},
		{
			"OptionWithZeroLength",
			lladdr0,
			255,
			0,
			[]byte{
				// RA payload
				0, 0, 0, 0,
				0, 0, 0, 0,
				0, 0, 0, 0,

				// Option #1 (TargetLinkLayerAddress)
				// Invalid as it has 0 length.
				2, 0, 0, 0, 0, 0, 0, 0,

				// Option #2 (unrecognized)
				255, 1, 0, 0, 0, 0, 0, 0,

				// Option #3 (PrefixInformation)
				3, 4, 0, 0, 0, 0, 0, 0,
				0, 0, 0, 0, 0, 0, 0, 0,
				0, 0, 0, 0, 0, 0, 0, 0,
				0, 0, 0, 0, 0, 0, 0, 0,
			},
			false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			e := channel.New(10, 1280, linkAddr1)
			s := stack.New(stack.Options{
				NetworkProtocols: []stack.NetworkProtocol{NewProtocol()},
			})

			if err := s.CreateNIC(1, e); err != nil {
				t.Fatalf("CreateNIC(_) = %s", err)
			}

			icmpSize := header.ICMPv6HeaderSize + len(test.ndpPayload)
			hdr := buffer.NewPrependable(header.IPv6MinimumSize + icmpSize)
			pkt := header.ICMPv6(hdr.Prepend(icmpSize))
			pkt.SetType(header.ICMPv6RouterAdvert)
			pkt.SetCode(test.code)
			copy(pkt.NDPPayload(), test.ndpPayload)
			payloadLength := hdr.UsedLength()
			pkt.SetChecksum(header.ICMPv6Checksum(pkt, test.src, header.IPv6AllNodesMulticastAddress, buffer.VectorisedView{}))
			ip := header.IPv6(hdr.Prepend(header.IPv6MinimumSize))
			ip.Encode(&header.IPv6Fields{
				PayloadLength: uint16(payloadLength),
				NextHeader:    uint8(icmp.ProtocolNumber6),
				HopLimit:      test.hopLimit,
				SrcAddr:       test.src,
				DstAddr:       header.IPv6AllNodesMulticastAddress,
			})

			stats := s.Stats().ICMP.V6PacketsReceived
			invalid := stats.Invalid
			rxRA := stats.RouterAdvert

			if got := invalid.Value(); got != 0 {
				t.Fatalf("got invalid = %d, want = 0", got)
			}
			if got := rxRA.Value(); got != 0 {
				t.Fatalf("got rxRA = %d, want = 0", got)
			}

			e.InjectInbound(header.IPv6ProtocolNumber, stack.PacketBuffer{
				Data: hdr.View().ToVectorisedView(),
			})

			if got := rxRA.Value(); got != 1 {
				t.Fatalf("got rxRA = %d, want = 1", got)
			}

			if test.expectedSuccess {
				if got := invalid.Value(); got != 0 {
					t.Fatalf("got invalid = %d, want = 0", got)
				}
			} else {
				if got := invalid.Value(); got != 1 {
					t.Fatalf("got invalid = %d, want = 1", got)
				}
			}
		})
	}
}
