package net

import (
	"container/list"
	"context"
	"errors"
	"sync"

	"github.com/golang/glog"
	crypto "github.com/libp2p/go-libp2p-crypto"
	net "github.com/libp2p/go-libp2p-net"
	peer "github.com/libp2p/go-libp2p-peer"
	peerstore "github.com/libp2p/go-libp2p-peerstore"
	protocol "github.com/libp2p/go-libp2p-protocol"
	ma "github.com/multiformats/go-multiaddr"
)

/**
The basic network is a push-based streaming protocol.  It works as follow:
	- When a video is broadcasted, it's stored at a local broadcaster
	- When a viewer wants to view a video, it sends a subscribe request to the network
	- The network routes the request towards the broadcast node via kademlia routing

**/

var Protocol = protocol.ID("/livepeer_video/0.0.1")
var ErrNoClosePeers = errors.New("NoClosePeers")
var ErrUnknownMsg = errors.New("UnknownMsgType")
var ErrProtocol = errors.New("ProtocolError")

type VideoMuxer interface {
	WriteSegment(seqNo uint64, strmID string, data []byte) error
}

//BasicVideoNetwork creates a kademlia network using libp2p.  It does push-based video delivery, and handles the protocol in the background.
type BasicVideoNetwork struct {
	NetworkNode  *NetworkNode
	broadcasters map[string]*BasicBroadcaster
	subscribers  map[string]*BasicSubscriber

	// streams           map[string]*stream.VideoStream
	// streamSubscribers map[string]*stream.StreamSubscriber
	// cancellation      map[string]context.CancelFunc
}

//NewBasicNetwork creates a libp2p node, handle the basic (push-based) video protocol.
func NewBasicNetwork(port int, priv crypto.PrivKey, pub crypto.PubKey) (*BasicVideoNetwork, error) {
	n, err := NewNode(port, priv, pub)
	if err != nil {
		glog.Errorf("Error creating a new node: %v", err)
		return nil, err
	}

	nw := &BasicVideoNetwork{NetworkNode: n, broadcasters: make(map[string]*BasicBroadcaster), subscribers: make(map[string]*BasicSubscriber)}
	// if err = nw.setupProtocol(n); err != nil {
	// 	glog.Errorf("Error setting up video protocol: %v", err)
	// 	return nil, err
	// }

	return nw, nil
}

func (n *BasicVideoNetwork) NewBroadcaster(strmID string) Broadcaster {
	// b := &BasicBroadcaster{Network: n, StrmID: strmID, q: list.New(), host: n.NetworkNode.PeerHost, lock: &sync.Mutex{}, listeners: make(map[string]peerstore.PeerInfo)}
	b := &BasicBroadcaster{Network: n, StrmID: strmID, q: list.New(), lock: &sync.Mutex{}, listeners: make(map[string]VideoMuxer)}
	n.broadcasters[strmID] = b
	return b
}

func (n *BasicVideoNetwork) GetBroadcaster(strmID string) Broadcaster {
	b, ok := n.broadcasters[strmID]
	if !ok {
		return nil
	}
	return b
}

func (n *BasicVideoNetwork) NewSubscriber(strmID string) Subscriber {
	s := &BasicSubscriber{Network: n, StrmID: strmID, host: n.NetworkNode.PeerHost, msgChan: make(chan StreamDataMsg)}
	n.subscribers[strmID] = s
	return s
}

func (n *BasicVideoNetwork) GetSubscriber(strmID string) Subscriber {
	s, ok := n.subscribers[strmID]
	if !ok {
		return nil
	}
	return s
}

func (n *BasicVideoNetwork) Connect(nodeID, addr string) error {
	pid, err := peer.IDHexDecode(nodeID)
	if err != nil {
		glog.Errorf("Invalid node ID: %v", err)
		return err
	}

	var paddr ma.Multiaddr
	paddr, err = ma.NewMultiaddr(addr)
	if err != nil {
		glog.Errorf("Invalid addr: %v", err)
		return err
	}

	n.NetworkNode.PeerHost.Peerstore().AddAddr(pid, paddr, peerstore.PermanentAddrTTL)
	n.NetworkNode.PeerHost.Connect(context.Background(), peerstore.PeerInfo{ID: pid})

	// n.SendJoin(sid)
	return nil
}

func (nw *BasicVideoNetwork) SetupProtocol() error {
	glog.Infof("Setting up protocol: %v", Protocol)
	nw.NetworkNode.PeerHost.SetStreamHandler(Protocol, func(stream net.Stream) {
		streamHandler(nw, stream)
	})

	return nil
}

func streamHandler(nw *BasicVideoNetwork, stream net.Stream) error {
	glog.Infof("%v Received a stream from %v", stream.Conn().LocalPeer().Pretty(), stream.Conn().RemotePeer().Pretty())
	var msg Msg

	ws := WrapStream(stream)
	err := ws.Dec.Decode(&msg)

	if err != nil {
		glog.Errorf("Got error decoding msg: %v", err)
		return err
	}

	//Video Protocol:
	//	- StreamData
	//	- FinishStream
	//	- SubReq
	//	- CancelSub

	//Livepeer Protocol:
	//	- TranscodeInfo
	//	- TranscodeInfoAck (TranscodeInfo will re-send until getting an Ack)
	switch msg.Op {
	case SubReqID:
		sr, ok := msg.Data.(SubReqMsg)
		if !ok {
			glog.Errorf("Cannot convert SubReqMsg: %v", msg.Data)
			return ErrProtocol
		}
		// glog.Infof("Got Sub Req: %v", sr)
		return handleSubReq(nw, sr, ws)
	case CancelSubID:
		cr, ok := msg.Data.(CancelSubMsg)
		if !ok {
			glog.Errorf("Cannot convert CancelSubMsg: %v", msg.Data)
			return ErrProtocol
		}
		return nw.handleCancelSubReq(cr, ws.Stream.Conn().RemotePeer())
	case StreamDataID:
		// glog.Infof("Got Stream Data: %v", msg.Data)
		//Enque it into the subscriber
		sd, ok := msg.Data.(StreamDataMsg)
		if !ok {
			glog.Errorf("Cannot convert SubReqMsg: %v", msg.Data)
		}
		return nw.handleStreamData(sd)
	case FinishStreamID:
		fs, ok := msg.Data.(FinishStreamMsg)
		if !ok {
			glog.Errorf("Cannot convert FinishStreamMsg: %v", msg.Data)
		}
		return nw.handleFinishStream(fs)
	default:
		glog.Infof("Unknown Data: %v -- closing stream", msg)
		stream.Close()
		return ErrUnknownMsg
	}

	return nil
}

func handleSubReq(nw *BasicVideoNetwork, subReq SubReqMsg, ws *BasicStream) error {
	b := nw.broadcasters[subReq.StrmID]
	if b == nil {
		//This is when you are a relay node
		glog.Infof("Cannot find local broadcaster for stream: %v.  Forwarding along to the network", subReq.StrmID)

		//Create a relayer, hook up the relaying

		//Subscribe from the network
	}

	//TODO: Add verification code for the SubNodeID (Make sure the message is not spoofed)
	remotePid := peer.IDHexEncode(ws.Stream.Conn().RemotePeer())
	b.listeners[remotePid] = ws
	return nil
}

func (nw *BasicVideoNetwork) handleCancelSubReq(cr CancelSubMsg, rpeer peer.ID) error {
	if b := nw.broadcasters[cr.StrmID]; b != nil {
		//Remove from listener
		delete(b.listeners, peer.IDHexEncode(rpeer))
		return nil
	} else {
		//TODO: Add relay case
		glog.Errorf("Cannot find broadcaster or relayer.  Error!")
		return ErrProtocol
	}
}

func (nw *BasicVideoNetwork) handleStreamData(sd StreamDataMsg) error {
	// if b := nw.broadcasters[sd.StrmID]; b != nil {
	// 	//TODO: This is questionable.  Do we every have this case?
	// 	glog.Infof("Calling broadcast")
	// 	b.Broadcast(sd.SeqNo, sd.Data)
	// 	return nil
	// } else
	if s := nw.subscribers[sd.StrmID]; s != nil {
		// glog.Infof("Inserting into subscriber msg queue: %v", sd)
		s.msgChan <- sd
		return nil
	} else {
		//TODO: Add relay case
		glog.Errorf("Something is wrong.  Expect broadcaster or subscriber to exist at this point (should have been setup when SubReq came in)")
		return ErrProtocol
	}
}

func (nw *BasicVideoNetwork) handleFinishStream(fs FinishStreamMsg) error {
	if s := nw.subscribers[fs.StrmID]; s != nil {
		//Cancel subscriber worker, delete subscriber
		s.cancelWorker()
		delete(nw.subscribers, fs.StrmID)
		return nil
	} else {
		//TODO: Add relay case
		glog.Errorf("Error: cannot find subscriber or relayer")
		return ErrProtocol
	}
}