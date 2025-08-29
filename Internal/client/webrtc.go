package client

import (
	"encoding/json"
	"io"
	"log"
	"os"
	"sync"
	"time"

	"github.com/pion/webrtc/v3"
	"github.com/libp2p/go-libp2p/core/network"
)

type WebRTCPeer struct {
	peerConnection *webrtc.PeerConnection
	dataChannel    *webrtc.DataChannel
	closeChan      chan struct{}
	onMessage      func(msg webrtc.DataChannelMessage, peer *WebRTCPeer)
	fileWriter     io.WriteCloser
	writerMutex    sync.Mutex
	signalingStream network.Stream
}

func NewWebRTCPeer(onMessage func(msg webrtc.DataChannelMessage, peer *WebRTCPeer)) (*WebRTCPeer, error) {
	config := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{
				URLs: []string{"stun:stun.l.google.com:19302"},
			},
		},
	}
	pc, err := webrtc.NewPeerConnection(config)
	if err != nil {
		return nil, err
	}

	peer := &WebRTCPeer{
		peerConnection: pc,
		closeChan:      make(chan struct{}),
		onMessage:      onMessage,
	}

	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		log.Printf("Peer Connection State changed: %s\n", state.String())
		if state == webrtc.PeerConnectionStateFailed || state == webrtc.PeerConnectionStateClosed || state == webrtc.PeerConnectionStateDisconnected {
			if peer.fileWriter != nil {
				peer.fileWriter.Close()
			}
			close(peer.closeChan)
		}
	})

	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		log.Printf("New DataChannel \"%s\" (ID: %d) received!\n", dc.Label(), *dc.ID())
		peer.dataChannel = dc
		peer.setupDataChannel()
	})

	return peer, nil
}

func (p *WebRTCPeer) CreateOffer() (string, error) {
	dc, err := p.peerConnection.CreateDataChannel("data", nil)
	if err != nil {
		return "", err
	}
	p.dataChannel = dc
	p.setupDataChannel()

	offer, err := p.peerConnection.CreateOffer(nil)
	if err != nil {
		return "", err
	}
	if err := p.peerConnection.SetLocalDescription(offer); err != nil {
		return "", err
	}
	return p.encode(offer)
}

func (p *WebRTCPeer) CreateAnswer(offerStr string) (string, error) {
	var offer webrtc.SessionDescription
	if err := p.decode(offerStr, &offer); err != nil {
		return "", err
	}
	if err := p.peerConnection.SetRemoteDescription(offer); err != nil {
		return "", err
	}
	answer, err := p.peerConnection.CreateAnswer(nil)
	if err != nil {
		return "", err
	}
	if err := p.peerConnection.SetLocalDescription(answer); err != nil {
		return "", err
	}
	return p.encode(answer)
}

func (p *WebRTCPeer) SetAnswer(answerStr string) error {
	var answer webrtc.SessionDescription
	if err := p.decode(answerStr, &answer); err != nil {
		return err
	}
	return p.peerConnection.SetRemoteDescription(answer)
}

func (p *WebRTCPeer) setupDataChannel() {
	p.dataChannel.OnOpen(func() {
		log.Printf("Data channel '%s' (ID: %d) opened!\n", p.dataChannel.Label(), *p.dataChannel.ID())
	})

	p.dataChannel.OnMessage(func(msg webrtc.DataChannelMessage) {
		p.onMessage(msg, p)
	})
}

func (p *WebRTCPeer) SendJSON(v interface{}) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return p.dataChannel.SendText(string(b))
}

func (p *WebRTCPeer) SendRaw(b []byte) error {
	return p.dataChannel.Send(b)
}

func (p *WebRTCPeer) WaitForConnection(timeout time.Duration) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.Now().Add(timeout)
	for {
		if p.peerConnection.ConnectionState() == webrtc.PeerConnectionStateConnected {
			return nil
		}
		if time.Now().After(deadline) {
			return os.ErrDeadlineExceeded
		}
		<-ticker.C
	}
}

func (p *WebRTCPeer) Close() {
	if p.peerConnection != nil {
		p.peerConnection.Close()
	}
}

func (p *WebRTCPeer) SetSignalingStream(s network.Stream) {
	p.signalingStream = s
}

func (p *WebRTCPeer) SetFileWriter(w io.WriteCloser) {
	p.writerMutex.Lock()
	defer p.writerMutex.Unlock()
	p.fileWriter = w
}

func (p *WebRTCPeer) GetFileWriter() io.WriteCloser {
	p.writerMutex.Lock()
	defer p.writerMutex.Unlock()
	return p.fileWriter
}

func (p *WebRTCPeer) encode(obj interface{}) (string, error) {
	b, err := json.Marshal(obj)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func (p *WebRTCPeer) decode(s string, obj interface{}) error {
	return json.Unmarshal([]byte(s), obj)
}

func (p *WebRTCPeer) WaitForClose() {
	<-p.closeChan
}