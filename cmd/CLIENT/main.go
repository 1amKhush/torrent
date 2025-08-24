package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	webRTC "torrentium/Internal/client"
	"torrentium/internal/db"
	"torrentium/internal/p2p"
	//torrentfile "torrentium/internal/torrent"
	"torrentium/internal/tracker"

	"github.com/ipfs/go-cid"
	"github.com/joho/godotenv"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/multiformats/go-multihash"
	"github.com/pion/webrtc/v3"
)

type Client struct {
	host            host.Host
	dht             *dht.IpfsDHT
	webRTCPeers     map[peer.ID]*webRTC.WebRTCPeer
	peersMux        sync.RWMutex
	sharingFiles    map[string]string // map[fileID]filePath
	activeDownloads map[string]*os.File // Track active file downloads
	downloadsMux    sync.RWMutex
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := godotenv.Load(); err != nil {
		log.Printf("Warning: Could not load .env file: %v", err)
		log.Println("Proceeding with system environment variables...")
	}

	DB := db.InitDB()
	if DB == nil {
		log.Fatal("Database initialization failed")
	}
	t := tracker.NewTracker(DB)

	// Create libp2p host with DHT
	h, d, err := p2p.NewHost(ctx, "/ip4/0.0.0.0/tcp/0")
	if err != nil {
		log.Fatal("Failed to create libp2p host:", err)
	}
	defer h.Close()

	// Bootstrap the DHT
	go func() {
		if err := p2p.Bootstrap(ctx, h, d); err != nil {
			log.Printf("Error bootstrapping DHT: %v", err)
		}
	}()


	setupGracefulShutdown(h)
	
	client := NewClient(h, d)

	// Register the signaling protocol handler
	p2p.RegisterSignalingProtocol(h, client.handleWebRTCOffer)

	// Start the command loop
	client.commandLoop(t)
}


func NewClient(h host.Host, d *dht.IpfsDHT) *Client {
	return &Client{
		host:            h,
		dht:             d,
		webRTCPeers:     make(map[peer.ID]*webRTC.WebRTCPeer),
		sharingFiles:    make(map[string]string),
		activeDownloads: make(map[string]*os.File),
	}
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
			err = c.listFiles(t)
		case "get":
			if len(args) != 1 {
				err = errors.New("usage: get <file_id>")
			} else {
				err = c.getFile(args[0])
			}
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

	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return err
	}
	fileHashBytes := hasher.Sum(nil)
	fileHashStr := fmt.Sprintf("%x", fileHashBytes)

	// Create a CID from the file hash
	mhash, err := multihash.Encode(fileHashBytes, multihash.SHA2_256)
	if err != nil {
		return err
	}
	cid := cid.NewCidV1(cid.Raw, mhash)

	// Announce to the DHT that we are providing this file
	log.Printf("Announcing file with CID %s to the DHT...", cid.String())
	provideCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if err := c.dht.Provide(provideCtx, cid, true); err != nil {
		return fmt.Errorf("failed to provide file on DHT: %w", err)
	}
	log.Println("Successfully announced file to the DHT.")


	// Still add to local DB for `list` command functionality
	fileID, err := t.AddFileWithPeer(ctx, fileHashStr, info.Name(), info.Size(), c.host.ID().String())
	if err != nil {
		return fmt.Errorf("could not add file with the associated peer: %w", err)
	}

	c.sharingFiles[fileID] = filePath

	fmt.Printf("File '%s' (CID: %s) announced successfully and is ready to be shared.\n", filepath.Base(filePath), cid.String())
	return nil
}

func (c *Client) listFiles(t *tracker.Tracker) error {
	ctx := context.Background()
	files, err := t.GetAllFiles(ctx)
	if err != nil {
		return err
	}

	if len(files) == 0 {
		fmt.Println("No files available in the local tracker DB.")
		return nil
	}

	fmt.Println("\nAvailable Files (from local DB):")
	for _, file := range files {
		fmt.Println("--------------------")
		fmt.Printf("  ID: %s\n  Name: %s\n  Size: %s\n", file.FileHash, file.Filename, webRTC.FormatFileSize(file.FileSize))
	}
	fmt.Println("--------------------")
	return nil
}

func (c *Client) getFile(fileID string) error {
	ctx := context.Background()
	
	// For DHT, we need the CID. The fileID from the list command is the file hash.
	fileHashBytes, err := hex.DecodeString(fileID)
	if err != nil {
		return fmt.Errorf("invalid file ID/hash provided. Make sure to use the hash from the 'list' command: %w", err)
	}
	mhash, err := multihash.Encode(fileHashBytes, multihash.SHA2_256)
	if err != nil {
		return err
	}
	cidToGet := cid.NewCidV1(cid.Raw, mhash)

	log.Printf("Searching for peers with CID: %s", cidToGet.String())
	
	findCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	peers, err := c.dht.FindProviders(findCtx, cidToGet)
	if err != nil {
		return err
	}

	var targetPeer peer.AddrInfo
	found := false
	for p := range peers {
		if p.ID != c.host.ID() {
			targetPeer = p
			found = true
			break
		}
	}

	if !found {
		return fmt.Errorf("no other peers found for CID: %s", cidToGet.String())
	}
	
	log.Printf("Found peer %s. Initiating WebRTC connection...", targetPeer.ID)

	webrtcPeer, err := c.initiateWebRTCConnection(targetPeer.ID)
	if err != nil {
		return fmt.Errorf("could not establish WebRTC connection with peer %s: %w", targetPeer.ID, err)
	}
	log.Printf("WebRTC connection established with %s!", targetPeer.ID)

	localFile, err := os.Create(fileID + ".download")
	if err != nil {
		webrtcPeer.Close()
		return fmt.Errorf("failed to create local file for download: %w", err)
	}
	webrtcPeer.SetFileWriter(localFile)
	log.Printf("Downloading to %s...", localFile.Name())


	// Send the file request over the data channel
	requestPayload := map[string]string{
		"command": "REQUEST_FILE",
		"file_id": fileID,
	}
	if err := webrtcPeer.Send(requestPayload); err != nil {
		webrtcPeer.Close()
		return fmt.Errorf("failed to send file request to peer: %w", err)
	}

	return nil
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
	c.addWebRTCPeer(targetPeerID, webRTCPeer)


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
			fileID, hasFileID := message["file_id"]
			if !hasFileID {
				log.Println("Received file request without a file_id.")
				return
			}
			go c.sendFile(p, fileID)
		} else if status, ok := message["status"]; ok && status == "TRANSFER_COMPLETE" {
			log.Println("File transfer complete!")
			if writer := p.GetFileWriter(); writer != nil {
				writer.Close()
				// TODO: Rename file from .download to final name
			}
			p.Close()
		}
	} else { // Binary data (file chunk)
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