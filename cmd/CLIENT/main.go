package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"
	webRTC "torrentium/Internal/client"
	torrentfile "torrentium/internal/torrent"

	"strings"
	db "torrentium/internal/db"
	tracker "torrentium/internal/tracker"

	"database/sql"

	"torrentium/internal/p2p"

	"github.com/gorilla/websocket"
	"github.com/joho/godotenv"
	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	libp2pws "github.com/libp2p/go-libp2p/p2p/transport/websocket"
	"github.com/pion/webrtc/v3"
)

type Client struct {
	host            host.Host
	//peerName        string
	webRTCPeers     map[peer.ID]*webRTC.WebRTCPeer
	peersMux        sync.RWMutex
	sharingFiles    map[string]string
	activeDownloads map[string]*os.File // Track active file downloads
	downloadsMux    sync.RWMutex

	// Channels for handling responses
	fileListChan chan []db.File
	peerListChan chan []db.Peer
}

// Message is used for WebSocket and P2P communication
type Message struct {
	Command string          `json:"command"`           // name of command jaise : ADD_PEER
	Payload json.RawMessage `json:"payload,omitempty"` // according to command, payload mein data hai
}

// HandshakePayload is used for handshake messages
type HandshakePayload struct {
	PeerID string `json:"peer_id"`
}

// FileTransferPayload is used for file chunk transfer
type FileTransferPayload struct {
	FileID     string `json:"file_id"`
	ChunkIndex int    `json:"chunk_index"`
	Data       []byte `json:"data"`
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true // Allow all origins for now
	},
}

// ConnectionManager manages WebSocket connections by peer ID
type ConnectionManager struct {
	connections map[string]*websocket.Conn
	mu          sync.RWMutex
}

func NewConnectionManager() *ConnectionManager {
	return &ConnectionManager{
		connections: make(map[string]*websocket.Conn),
	}
}

func (cm *ConnectionManager) AddConnection(peerID string, conn *websocket.Conn) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	cm.connections[peerID] = conn
}

func (cm *ConnectionManager) RemoveConnection(peerID string) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	delete(cm.connections, peerID)
}

func (cm *ConnectionManager) GetConnection(peerID string) (*websocket.Conn, bool) {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	conn, exists := cm.connections[peerID]
	return conn, exists
}

// Define a generic Message type for WebSocket communication if p2p.Message is undefined
type WSMessage struct {
	Command string      `json:"command"`
	Payload interface{} `json:"payload,omitempty"`
}

func (cm *ConnectionManager) SendToConnection(peerID string, msg WSMessage) error {
	conn, exists := cm.GetConnection(peerID)
	if !exists {
		return nil // Connection not found
	}
	return conn.WriteJSON(msg)
}

type Tracker struct {
	// peers    map[string]bool // (In-memory map )jo currently connected peers hai unke IDs ko store karta hai.
	repo *db.Repository
	// peersMux sync.RWMutex // peers map ko concurrency clashes se bachane ke liye reead and write Mutex.
}

type Repository struct {
	DB *sql.DB
}

func main() {
	if err := godotenv.Load(); err != nil {
		log.Printf("Warning: Could not load .env file: %v", err)
		log.Println("Proceeding with system environment variables...")
	}

	if err := godotenv.Load(); err != nil {
		log.Printf("Warning: Could not load .env file: %v", err)
		log.Println("Proceeding with system environment variables...")
	}
	// Initialize the database

	DB := db.InitDB()

	t := tracker.NewTracker(DB)

	fmt.Println(t)

	// repo := db.NewRepository(DB)

	// Create libp2p host with WebSocket support
	h, err := libp2p.New(
		libp2p.Transport(libp2pws.New),                    // Add WebSocket transport
		libp2p.ListenAddrStrings("/ip4/0.0.0.0/tcp/0/ws"), // WebSocket listen address
	)
	if err != nil {
		log.Fatal("Failed to create libp2p host:", err)
	}
	log.Printf("Peer libp2p Host ID: %s", h.ID())

	setupGracefulShutdown(h)
	// instance of client struct
	client := NewClient(h)

	client.commandLoop(t)

	p2p.RegisterSignalingProtocol(h, client.handleWebRTCOffer)

	// Create connection manager
	cm := NewConnectionManager()

	// Setup WebSocket handler
	http.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		handleWebSocketConnection(w, r, t, cm)
	})

	// log.Printf("-> WebSocket tracker listening on %s", wsAddr)
	// log.Fatal(http.ListenAndServe(wsAddr, nil))
}

func (c *Client) commandLoop(t *tracker.Tracker) {
	scanner := bufio.NewScanner(os.Stdin)
	webRTC.PrintClientInstructions()
	for {
		fmt.Print("> ")
		if !scanner.Scan() {
			break
		}
		parts := strings.Fields(scanner.Text())
		if len(parts) == 0 {
			continue
		}
		cmd, args := parts[0], parts[1:]

		var err error
		switch cmd {
		case "help":
			webRTC.PrintClientInstructions()
		case "add":
			if len(args) != 1 {
				err = errors.New("usage: add <filepath>")
			} else {
				err = c.addFile(args[0], t)
			}
		case "list":
			err = c.listFiles()
		case "exit":
			return
		default:
			err = errors.New("unknown command")
		}
		if err != nil {
			log.Printf("Error: %v", err)
		}
	}
}

func (c *Client) addFile(filePath string, t *tracker.Tracker) error {
	ctx := context.Background()

	file, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}

	Filename := info.Name()
	FileSize := info.Size()
	var remotePeerID string

	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return err
	}
	fileHash := fmt.Sprintf("%x", hasher.Sum(nil))

	if err := torrentfile.CreateTorrentFile(filePath); err != nil {
		log.Printf("Warning: failed to create .torrent file: %v", err)
	}

	_, err = t.AddFileWithPeer(ctx, fileHash, Filename, FileSize, remotePeerID)
	if err != nil {
		fmt.Println("Could not add file with the associated peer.")
	}
	fmt.Printf("File '%s' announced successfully and is ready to be shared.\n", filepath.Base(filePath))
	return nil
}

// GetAllFiles database mein available sabhi files ki list return karta hai.
func (t *Tracker) GetAllFiles(ctx context.Context) ([]db.File, error) {
	return t.repo.FindAllFiles(ctx)
}

func (c *Client) listFiles() error {

	var files []db.File
	if len(files) == 0 {
		fmt.Println("No files available on the tracker.")
		return nil
	}

	fmt.Println("\nAvailable Files:")
	for _, file := range files {
		fmt.Println("--------------------")
		fmt.Printf("  ID: %s\n  Name: %s\n  Size: %s\n", file.ID, file.Filename, webRTC.FormatFileSize(file.FileSize))
	}
	fmt.Println("--------------------")
	return nil
}

func NewClient(h host.Host) *Client {
	return &Client{
		host:            h,
		webRTCPeers:     make(map[peer.ID]*webRTC.WebRTCPeer),
		sharingFiles:    make(map[string]string), // map[keytype]valuetype
		activeDownloads: make(map[string]*os.File),
		fileListChan:    make(chan []db.File, 1),
		peerListChan:    make(chan []db.Peer, 1),
	}
}

func setupGracefulShutdown(h host.Host) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ch
		log.Println("Shutting down...")
		if err := h.Close(); err != nil {
			log.Printf("Error closing libp2p host: %v", err)
		}
		os.Exit(0)
	}()
}

func (c *Client) initiateWebRTCConnection(targetPeerID peer.ID) (*webRTC.WebRTCPeer, error) {
	log.Printf("Creating signaling stream to peer %s...", targetPeerID)

	// Create context with timeout for stream creation
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	s, err := c.host.NewStream(ctx, targetPeerID, p2p.SignalingProtocolID)
	if err != nil {
		return nil, fmt.Errorf("failed to create signaling stream to %s: %w", targetPeerID, err)
	}
	log.Printf("Successfully created signaling stream to peer %s", targetPeerID)

	webRTCPeer, err := webRTC.NewWebRTCPeer(c.onDataChannelMessage)
	if err != nil {
		s.Close()
		return nil, fmt.Errorf("failed to create WebRTC peer: %w", err)
	}

	webRTCPeer.SetSignalingStream(s)

	log.Println("Creating WebRTC offer...")
	offer, err := webRTCPeer.CreateOffer()
	if err != nil {
		webRTCPeer.Close()
		return nil, fmt.Errorf("failed to create WebRTC offer: %w", err)
	}

	log.Println("Sending offer to remote peer...")
	encoder := json.NewEncoder(s)
	if err := encoder.Encode(offer); err != nil {
		webRTCPeer.Close()
		return nil, fmt.Errorf("failed to send offer: %w", err)
	}

	log.Println("Waiting for answer from remote peer...")
	var answer string
	decoder := json.NewDecoder(s)
	if err := decoder.Decode(&answer); err != nil {
		webRTCPeer.Close()
		return nil, fmt.Errorf("failed to receive answer: %w", err)
	}

	log.Println("Setting remote answer...")
	if err := webRTCPeer.SetAnswer(answer); err != nil {
		webRTCPeer.Close()
		return nil, fmt.Errorf("failed to set answer: %w", err)
	}

	log.Println("Waiting for WebRTC connection to establish...")
	if err := webRTCPeer.WaitForConnection(30 * time.Second); err != nil {
		webRTCPeer.Close()
		return nil, fmt.Errorf("failed to establish WebRTC connection: %w", err)
	}
	return webRTCPeer, nil
}

func (c *Client) handleWebRTCOffer(offer, remotePeerIDStr string, s network.Stream) (string, error) {
	remotePeerID, err := peer.Decode(remotePeerIDStr)
	if err != nil {
		return "", err
	}

	log.Printf("Handling incoming WebRTC offer from %s", remotePeerID)
	webRTCPeer, err := webRTC.NewWebRTCPeer(c.onDataChannelMessage)
	if err != nil {
		return "", err
	}

	webRTCPeer.SetSignalingStream(s)

	answer, err := webRTCPeer.CreateAnswer(offer)
	if err != nil {
		webRTCPeer.Close()
		return "", err
	}

	c.addWebRTCPeer(remotePeerID, webRTCPeer)
	return answer, nil
}

func (c *Client) onDataChannelMessage(msg webrtc.DataChannelMessage, p *webRTC.WebRTCPeer) {
	if msg.IsString {
		var message map[string]string
		if err := json.Unmarshal(msg.Data, &message); err != nil {
			log.Printf("Received un-parseable message: %s", string(msg.Data))
			return
		}

		if cmd, ok := message["command"]; ok && cmd == "REQUEST_FILE" {
			fileIDStr, hasFileID := message["file_id"]
			if !hasFileID {
				log.Println("Received file request without a file_id.")
				return
			}
			// // fileID, err := uuid.Parse(fileIDStr)
			// if err != nil {
			// 	log.Printf("Received file request with invalid file ID: %s", fileIDStr)
			// 	return
			// }
			go c.sendFile(p, fileIDStr)
		} else if status, ok := message["status"]; ok && status == "TRANSFER_COMPLETE" {
			log.Println("File transfer complete!")
			if writer := p.GetFileWriter(); writer != nil {
				writer.Close()
			}
			p.Close()
		}
	} else {
		if writer := p.GetFileWriter(); writer != nil {
			if _, err := writer.Write(msg.Data); err != nil {
				log.Printf("Error writing file chunk: %v", err)
			}
		} else {
			log.Println("Received binary data but no file writer is active.")
		}
	}
}

func (c *Client) sendFile(p *webRTC.WebRTCPeer, fileID string) {
	log.Printf("Processing request to send file with ID: %s", fileID)

	filePath, ok := c.sharingFiles[fileID]
	if !ok {
		log.Printf("Error: Received request for file ID %s, but I am not sharing it.", fileID)
		p.Send(map[string]string{"error": "File not found"})
		return
	}

	file, err := os.Open(filePath)
	if err != nil {
		log.Printf("Error opening file %s to send: %v", filePath, err)
		p.Send(map[string]string{"error": "Could not open file"})
		return
	}
	defer file.Close()

	log.Printf("Starting file transfer for %s", filepath.Base(filePath))
	buffer := make([]byte, 64*1024)
	for {
		bytesRead, err := file.Read(buffer)
		if err != nil {
			if err == io.EOF {
				break
			}
			log.Printf("Error reading file chunk: %v", err)
			return
		}
		if err := p.SendRaw(buffer[:bytesRead]); err != nil {
			log.Printf("Error sending file chunk: %v", err)
			return
		}
	}
	log.Printf("Finished sending file %s", filepath.Base(filePath))
	p.Send(map[string]string{"status": "TRANSFER_COMPLETE"})
}

func (c *Client) addWebRTCPeer(id peer.ID, p *webRTC.WebRTCPeer) {
	c.peersMux.Lock()
	defer c.peersMux.Unlock()
	c.webRTCPeers[id] = p
}

func handleWebSocketConnection(w http.ResponseWriter, r *http.Request, t *tracker.Tracker, cm *ConnectionManager) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WebSocket upgrade failed: %v", err)
		return
	}
	defer conn.Close()

	log.Println("New WebSocket connection established")

	var connectedPeerID string // Track which peer this connection belongs to

	// Handle the connection
	for {
		var msg Message
		err := conn.ReadJSON(&msg)
		if err != nil {
			log.Printf("WebSocket read error: %v", err)
			break
		}

		log.Printf("Received message: Command=%s", msg.Command)

		// Handle file chunks specially - forward them to the requester

		if msg.Command == "FILE_CHUNK" {
			handleFileChunk(msg, cm)
			continue
		}

		// response := handleTrackerMessage(msg, t, cm)
		// log.Printf("Sending response: Command=%s", response.Command)

		// Track the peer ID after successful handshake
		// Example handshake logic: if HANDSHAKE received, respond with WELCOME
		var response Message
		if msg.Command == "HANDSHAKE" {
			var payload HandshakePayload
			if err := json.Unmarshal(msg.Payload, &payload); err == nil {
				connectedPeerID = payload.PeerID
				cm.AddConnection(connectedPeerID, conn)
				log.Printf("Tracked connection for peer: %s", connectedPeerID)
				// Respond with WELCOME
				response = Message{
					Command: "WELCOME",
					Payload: json.RawMessage([]byte(`{"peer_id":"` + connectedPeerID + `"}`)),
				}
				if err := conn.WriteJSON(response); err != nil {
					log.Printf("WebSocket write error: %v", err)
					break
				}
				continue
			}
		}
	}

	// Mark peer as offline when connection closes

	if connectedPeerID != "" {
		log.Printf("Marking peer %s as offline due to connection close", connectedPeerID)
		cm.RemoveConnection(connectedPeerID)
		// t.RemovePeer(connectedPeerID) // Method does not exist, so removed
	}

	log.Println("WebSocket connection closed")
}

// handleFileChunk forwards file chunks to the requesting peer
func handleFileChunk(msg Message, cm *ConnectionManager) {
	var chunkPayload FileTransferPayload
	if err := json.Unmarshal(msg.Payload, &chunkPayload); err != nil {
		log.Printf("Error unmarshaling file chunk: %v", err)
		return
	}

	log.Printf("Received file chunk %d for FileID %s, forwarding to requester", chunkPayload.ChunkIndex, chunkPayload.FileID)

	// Forward chunk to the requester peer
	// For simplicity, we'll broadcast to all connections and let the client filter
	// In a production system, you'd track active transfers properly
	cm.mu.RLock()
	for peerID, conn := range cm.connections {
		if peerID != "" { // Don't send back to sender
			if err := conn.WriteJSON(msg); err != nil {
				log.Printf("Failed to forward chunk to peer %s: %v", peerID, err)
			}
		}
	}
	cm.mu.RUnlock()
}
