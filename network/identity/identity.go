package identity

import (
	"context"
	"errors"
	"fmt"

	"github.com/0xPolygon/polygon-edge/network/event"
	"github.com/hashicorp/go-hclog"

	"github.com/0xPolygon/polygon-edge/network/proto"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
)

const PeerID = "peerID"

var (
	ErrInvalidChainID   = errors.New("invalid chain ID")
	ErrNoAvailableSlots = errors.New("no available Slots")
)

// networkingServer defines the base communication interface between
// any networking server implementation and the IdentityService
type networkingServer interface {
	// PROTOCOL MANIPULATION //

	// NewIdentityClient returns an identity gRPC client connection
	NewIdentityClient(peerID peer.ID) (proto.IdentityClient, error)

	// PEER MANIPULATION //

	// DisconnectFromPeer attempts to disconnect from the specified peer
	DisconnectFromPeer(peerID peer.ID, reason string)

	// AddPeer adds a peer to the networking server's peer store
	AddPeer(id peer.ID, direction network.Direction)

	// UpdatePendingConnCount updates the pendingPeerConnections connection count for the direction [Thread safe]
	UpdatePendingConnCount(delta int64, direction network.Direction)

	// EmitEvent emits the specified peer event on the base networking server
	EmitEvent(event *event.PeerEvent)

	// TEMPORARY DIALING //

	// IsTemporaryDial checks if the peer connection is a temporary dial [Thread safe]
	IsTemporaryDial(peerID peer.ID) bool

	// CONNECTION INFORMATION //

	// HasFreeConnectionSlot checks if there are available outbound connection slots [Thread safe]
	HasFreeConnectionSlot(direction network.Direction) bool

	AddPendingPeer(peerID peer.ID, direction network.Direction)

	RemovePendingPeer(peerID peer.ID)

	HasPendingStatus(peerID peer.ID) bool

	HasIdentityPendingStatus(peerID peer.ID) bool
}

// IdentityService is a networking service used to handle peer handshaking.
// It acts as a gatekeeper to peer connectivity
type IdentityService struct {
	proto.UnimplementedIdentityServer

	logger     hclog.Logger     // The IdentityService logger
	baseServer networkingServer // The interface towards the base networking server

	chainID int64   // The chain ID of the network
	hostID  peer.ID // The base networking server's host peer ID
}

// NewIdentityService returns a new instance of the IdentityService
func NewIdentityService(
	server networkingServer,
	logger hclog.Logger,
	chainID int64,
	hostID peer.ID,
) *IdentityService {
	return &IdentityService{
		logger:     logger.Named("identity"),
		baseServer: server,
		chainID:    chainID,
		hostID:     hostID,
	}
}

func (i *IdentityService) GetNotifyBundle() *network.NotifyBundle {
	return &network.NotifyBundle{
		ConnectedF: func(net network.Network, conn network.Conn) {
			peerID := conn.RemotePeer()
			i.logger.Debug("Conn", "peer", peerID, "direction", conn.Stat().Direction)

			if i.baseServer.HasIdentityPendingStatus(peerID) {
				// handshake has already started
				return
			}

			if !i.baseServer.HasFreeConnectionSlot(conn.Stat().Direction) && !i.baseServer.HasPendingStatus(peerID) {
				i.disconnectFromPeer(peerID, ErrNoAvailableSlots.Error())
				return
			}

			// Mark the peer as pending (pending handshake)
			i.baseServer.AddPendingPeer(peerID, conn.Stat().Direction)

			go func() {
				eventType := event.PeerDialCompleted

				if err := i.handleConnected(peerID, conn.Stat().Direction); err != nil {
					i.baseServer.RemovePendingPeer(peerID)
					// Close the connection to the peer
					if !i.baseServer.HasPendingStatus(peerID) {
						i.disconnectFromPeer(peerID, err.Error())
					}

					eventType = event.PeerFailedToConnect
				} else {
					// Mark the peer as no longer pending
					i.baseServer.RemovePendingPeer(peerID)
				}

				// Emit an adequate event
				i.baseServer.EmitEvent(&event.PeerEvent{
					PeerID: peerID,
					Type:   eventType,
				})
			}()
		},
	}
}

// disconnectFromPeer disconnects from the specified peer
func (i *IdentityService) disconnectFromPeer(peerID peer.ID, reason string) {
	i.baseServer.DisconnectFromPeer(peerID, reason)
}

// handleConnected handles new network connections (handshakes)
func (i *IdentityService) handleConnected(peerID peer.ID, direction network.Direction) error {
	clt, clientErr := i.baseServer.NewIdentityClient(peerID)
	if clientErr != nil {
		return fmt.Errorf(
			"unable to create new identity client connection, %w",
			clientErr,
		)
	}

	// Construct the response status
	status := i.constructStatus(peerID)

	// Initiate the handshake
	resp, err := clt.Hello(context.Background(), status)
	if err != nil {
		return err
	}

	// Validate that the peers are working on the same chain
	if status.Chain != resp.Chain {
		return ErrInvalidChainID
	}

	// If this is a NOT temporary connection, save it
	if !resp.TemporaryDial && !status.TemporaryDial {
		i.baseServer.AddPeer(peerID, direction)
	}

	return nil
}

// Hello is the initial message that bundles peer information
// on first contact
func (i *IdentityService) Hello(_ context.Context, req *proto.Status) (*proto.Status, error) {
	// The peerID is the other node's peerID
	// as this method is invoking a call such as "Hello, <peerID>!"
	peerID, err := peer.Decode(req.Metadata[PeerID])
	if err != nil {
		return nil, err
	}

	return i.constructStatus(peerID), nil
}

// constructStatus constructs a status response of the current node
func (i *IdentityService) constructStatus(peerID peer.ID) *proto.Status {
	return &proto.Status{
		Metadata: map[string]string{
			PeerID: i.hostID.Pretty(),
		},
		Chain:         i.chainID,
		TemporaryDial: i.baseServer.IsTemporaryDial(peerID),
	}
}
