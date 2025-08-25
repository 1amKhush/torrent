package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	webRTC "torrentium/Internal/client"
	db "torrentium/Internal/db"
	p2p "torrentium/Internal/p2p"

	"github.com/ipfs/go-cid"
	"github.com/joho/godotenv"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/multiformats/go-multihash"
	"github.com/pion/webrtc/v3"
	// dht1 "torrentium/Internal/dht/keycache"
)

// RWMutex => But when one goroutine writes (adds/removes a peer), it blocks all readers and writers until it’s done (ensures consistency).
type Client struct {
	host            host.Host
	dht             *dht.IpfsDHT
	webRTCPeers     map[peer.ID]*webRTC.WebRTCPeer
	peersMux        sync.RWMutex
	sharingFiles    map[string]*FileInfo // map[CID]fileInfo
	activeDownloads map[string]*os.File
	downloadsMux    sync.RWMutex
	db              *db.Repository
}

type FileInfo struct {
	FilePath string
	Hash     string
	Size     int64
	Name     string
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := godotenv.Load(); err != nil {
		log.Printf("Warning: Could not load .env file: %v", err)
	}

	DB := db.InitDB()
	// fmt.Println("debugging 10")
	if DB == nil {
		// fmt.Println("debugging 8")
		log.Fatal("Database initialization failed")
	}
	// fmt.Println("debugging 7")

	h, d, err := p2p.NewHost(ctx, "/ip4/0.0.0.0/tcp/0")
	// fmt.Println("debugging 3")
	if err != nil {
		// fmt.Println("debugging 4")
		log.Fatal("Failed to create libp2p host:", err)
	}
	defer h.Close()

	go func() {
		// fmt.Println("debugging 1")
		if err := p2p.Bootstrap(ctx, h, d); err != nil {
			// fmt.Println("debugging 2")
			log.Printf("Error bootstrapping DHT: %v", err)
		}
	}()

	setupGracefulShutdown(h)
	repo := db.NewRepository(DB)
	client := NewClient(h, d, repo)

	p2p.RegisterSignalingProtocol(h, client.handleWebRTCOffer)

	client.commandLoop()
}

func NewClient(h host.Host, d *dht.IpfsDHT, repo *db.Repository) *Client {
	return &Client{
		host:            h,
		dht:             d,
		webRTCPeers:     make(map[peer.ID]*webRTC.WebRTCPeer),
		sharingFiles:    make(map[string]*FileInfo),
		activeDownloads: make(map[string]*os.File),
		db:              repo,
	}
}

func (c *Client) commandLoop() {
	scanner := bufio.NewScanner(os.Stdin)
	c.printInstructions()
	//fmt.Println("ggggh")
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
			c.printInstructions()
		case "add":
			if len(args) != 1 {
				fmt.Println("Usage: add <filepath>")
			} else {
				err = c.addFile(args[0])
			}
		case "list":
			c.listLocalFiles()
		case "search":
			if len(args) != 1 {
				fmt.Println("Usage: search <query>")
			} else {
				err = c.searchFiles(args[0])
			}
		case "download":
			if len(args) != 1 {
				fmt.Println("Usage: download <CID>")
			} else {
				err = c.downloadFile(args[0])
			}
		case "peers":
			c.listConnectedPeers()
		case "exit":
			return
		default:
			fmt.Println("Unknown command. Type 'help' for available commands.")
		}

		if err != nil {
			log.Printf("Error: %v", err)
		}
	}
}

func (c *Client) printInstructions() {
	fmt.Println("\n=== Decentralized P2P File Sharing ===")
	fmt.Println("Commands:")
	fmt.Println("  add <filepath>     - Share a file on the network")
	fmt.Println("  list              - List your shared files")
	fmt.Println("  search <query>    - Search for files on the network")
	fmt.Println("  download <CID>    - Download a file by CID")
	fmt.Println("  peers            - Show connected peers")
	fmt.Println("  help             - Show this help")
	fmt.Println("  exit             - Exit the application")
	fmt.Printf("\nYour Peer ID: %s\n", c.host.ID())
	fmt.Printf("Listening on: %v\n\n", c.host.Addrs())
}

func (c *Client) addFile(filePath string) error {
	ctx := context.Background()

	file, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("failed to open file: %w", err)
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("failed to get file info: %w", err)
	}

	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return fmt.Errorf("failed to calculate hash: %w", err)
	}

	fileHashBytes := hasher.Sum(nil)
	fileHashStr := hex.EncodeToString(fileHashBytes)

	mhash, err := multihash.Encode(fileHashBytes, multihash.SHA2_256)
	if err != nil {
		return fmt.Errorf("failed to create multihash: %w", err)
	}

	// Gives CID(content identifier) a hash to the added file
	fileCID := cid.NewCidV1(cid.Raw, mhash)

	// add file to the local DB
	if err := c.db.AddLocalFile(ctx, fileCID.String(), info.Name(), info.Size(), filePath, fileHashStr); err != nil {
		return fmt.Errorf("failed to store file metadata: %w", err)
	}

	c.sharingFiles[fileCID.String()] = &FileInfo{
		FilePath: filePath,
		Hash:     fileHashStr,
		Size:     info.Size(),
		Name:     info.Name(),
	}

	log.Printf("Announcing file %s with CID %s to DHT...", info.Name(), fileCID.String())
	provideCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	// here we are announcing file in DHT
	if err := c.dht.Provide(provideCtx, fileCID, true); err != nil {
		log.Printf("Warning: Failed to announce to DHT: %v", err)
	} else {
		log.Println("Successfully announced file to DHT")
	}

	fmt.Printf("✓ File '%s' is now being shared\n", info.Name())
	fmt.Printf("  CID: %s\n", fileCID.String())
	fmt.Printf("  Hash: %s\n", fileHashStr)
	fmt.Printf("  Size: %s\n", formatFileSize(info.Size()))

	return nil
}

func (c *Client) listLocalFiles() {
	ctx := context.Background()
	files, err := c.db.GetLocalFiles(ctx)
	if err != nil {
		log.Printf("Error retrieving files: %v", err)
		return
	}

	if len(files) == 0 {
		fmt.Println("No files being shared.")
		return
	}

	fmt.Println("\n=== Your Shared Files ===")
	for _, file := range files {
		fmt.Printf("Name: %s\n", file.Filename)
		fmt.Printf("  CID: %s\n", file.CID)
		fmt.Printf("  Size: %s\n", formatFileSize(file.FileSize))
		fmt.Printf("  Path: %s\n", file.FilePath)
		fmt.Println("  ---")
	}
}

func (c *Client) searchFiles(query string) error {
	if strings.HasPrefix(query, "bafy") || strings.HasPrefix(query, "Qm") {
		return c.searchByCID(query)
	}

	fmt.Printf("Searching for files containing '%s'...\n", query)
	fmt.Println("Note: Direct filename search requires content indexing.")
	fmt.Println("Try using the CID if you have it, or check with known peers.")

	return nil
}

func (c *Client) searchByCID(cidStr string) error {
	ctx := context.Background()

	fileCID, err := cid.Decode(cidStr)
	if err != nil {
		return fmt.Errorf("invalid CID: %w", err)
	}

	fmt.Printf("Searching for CID: %s\n", fileCID.String())

	findCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	// this function actually find the file with particular CID!
	providersChan := c.dht.FindProvidersAsync(findCtx, fileCID, 0)

	// List of peers having that file this returns!
	var foundPeers []peer.AddrInfo
	for provider := range providersChan {
		if provider.ID != c.host.ID() {
			foundPeers = append(foundPeers, provider)
			fmt.Printf("Found provider: %s\n", provider.ID)
		}
	}

	if len(foundPeers) == 0 {
		fmt.Println("No providers found for this CID")
		return nil
	}

	fmt.Printf("Found %d provider(s)\n", len(foundPeers))
	return nil
}

func (c *Client) downloadFile(cidStr string) error {
	ctx := context.Background()

	fileCID, err := cid.Decode(cidStr)
	if err != nil {
		return fmt.Errorf("invalid CID: %w", err)
	}

	fmt.Printf("Looking for providers of CID: %s\n", fileCID.String())

	findCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	providersChan := c.dht.FindProvidersAsync(findCtx, fileCID, 0)

	var targetPeer peer.AddrInfo
	found := false

	for provider := range providersChan {
		if provider.ID != c.host.ID() {
			targetPeer = provider
			found = true
			break
		}
	}

	if !found {
		return fmt.Errorf("no providers found for CID: %s", fileCID.String())
	}

	fmt.Printf("Found provider %s. Establishing connection...\n", targetPeer.ID)

	webrtcPeer, err := c.initiateWebRTCConnection(targetPeer.ID)
	if err != nil {
		return fmt.Errorf("failed to establish WebRTC connection: %w", err)
	}

	downloadPath := fmt.Sprintf("%s.download", cidStr)
	localFile, err := os.Create(downloadPath)
	if err != nil {
		webrtcPeer.Close()
		return fmt.Errorf("failed to create download file: %w", err)
	}

	webrtcPeer.SetFileWriter(localFile)
	fmt.Printf("Downloading to %s...\n", downloadPath)

	request := map[string]string{
		"command": "REQUEST_FILE",
		"cid":     cidStr,
	}

	if err := webrtcPeer.Send(request); err != nil {
		webrtcPeer.Close()
		return fmt.Errorf("failed to send file request: %w", err)
	}

	return nil
}

func (c *Client) listConnectedPeers() {
	peers := c.host.Network().Peers()
	fmt.Printf("\n=== Connected Peers (%d) ===\n", len(peers))

	for _, peerID := range peers {
		conn := c.host.Network().ConnsToPeer(peerID)
		if len(conn) > 0 {
			fmt.Printf("Peer: %s\n", peerID)
			fmt.Printf("  Address: %s\n", conn[0].RemoteMultiaddr())
		}
	}
}

func (c *Client) initiateWebRTCConnection(targetPeerID peer.ID) (*webRTC.WebRTCPeer, error) {
	log.Printf("Creating signaling stream to peer %s...", targetPeerID)

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Second)
	defer cancel()
	// fmt.Println("debug 1")

	info := peer.AddrInfo{ID: targetPeerID, Addrs: c.host.Peerstore().Addrs(targetPeerID)}

	// If no addrs yet, ask the DHT (libp2p kad) for them
	if len(info.Addrs) == 0 {
		pinfo, err := c.dht.FindPeer(ctx, targetPeerID)
		if err != nil {
			return nil, fmt.Errorf("could not resolve peer addresses via DHT: %w", err)
		}
		info = pinfo
	}

	if len(info.Addrs) == 0 {
		return nil, fmt.Errorf("peer %s has no known multiaddrs", targetPeerID)
	}

	// 3) Put addrs in Peerstore and connect if not already connected
	c.host.Peerstore().AddAddrs(info.ID, info.Addrs, 30*time.Minute)

	if c.host.Network().Connectedness(info.ID) != network.Connected {
		if err := c.host.Connect(ctx, info); err != nil {
			return nil, fmt.Errorf("failed to connect to peer %s: %w", info.ID, err)
		}
	}

	// 4) Now open the signaling stream (use the SAME protocol ID as your handler)
	sctx, scancel := context.WithTimeout(ctx, 20*time.Second)
	defer scancel()

	s, err := c.host.NewStream(sctx, targetPeerID, p2p.SignalingProtocolID)
	if err != nil {
		return nil, fmt.Errorf("failed to create signaling stream: %w", err)
	}

	webRTCPeer, err := webRTC.NewWebRTCPeer(c.onDataChannelMessage)
	if err != nil {
		s.Close()
		return nil, fmt.Errorf("failed to create WebRTC peer: %w", err)
	}

	webRTCPeer.SetSignalingStream(s)
	c.addWebRTCPeer(targetPeerID, webRTCPeer)

	offer, err := webRTCPeer.CreateOffer()
	if err != nil {
		webRTCPeer.Close()
		return nil, fmt.Errorf("failed to create offer: %w", err)
	}

	encoder := json.NewEncoder(s)
	if err := encoder.Encode(offer); err != nil {
		webRTCPeer.Close()
		return nil, fmt.Errorf("failed to send offer: %w", err)
	}

	var answer string
	decoder := json.NewDecoder(s)
	if err := decoder.Decode(&answer); err != nil {
		webRTCPeer.Close()
		return nil, fmt.Errorf("failed to receive answer: %w", err)
	}

	if err := webRTCPeer.SetAnswer(answer); err != nil {
		webRTCPeer.Close()
		return nil, fmt.Errorf("failed to set answer: %w", err)
	}

	if err := webRTCPeer.WaitForConnection(30 * time.Second); err != nil {
		webRTCPeer.Close()
		return nil, fmt.Errorf("failed to establish connection: %w", err)
	}

	log.Printf("WebRTC connection established with %s", targetPeerID)
	return webRTCPeer, nil
}

func (c *Client) handleWebRTCOffer(offer, remotePeerIDStr string, s network.Stream) (string, error) {
	remotePeerID, err := peer.Decode(remotePeerIDStr)
	if err != nil {
		return "", err
	}

	log.Printf("Handling WebRTC offer from %s", remotePeerID)

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
			log.Printf("Received invalid message: %s", string(msg.Data))
			return
		}

		if cmd, ok := message["command"]; ok && cmd == "REQUEST_FILE" {
			if cid, hasCID := message["cid"]; hasCID {
				go c.sendFile(p, cid)
			}
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
		}
	}
}

func (c *Client) sendFile(p *webRTC.WebRTCPeer, cidStr string) {
	log.Printf("Processing file request for CID: %s", cidStr)

	fileInfo, ok := c.sharingFiles[cidStr]
	if !ok {
		log.Printf("File not found: %s", cidStr)
		p.Send(map[string]string{"error": "File not found"})
		return
	}

	file, err := os.Open(fileInfo.FilePath)
	if err != nil {
		log.Printf("Error opening file: %v", err)
		p.Send(map[string]string{"error": "Could not open file"})
		return
	}
	defer file.Close()

	log.Printf("Starting file transfer: %s", fileInfo.Name)

	buffer := make([]byte, 64*1024)
	for {
		bytesRead, err := file.Read(buffer)
		if err != nil {
			if err == io.EOF {
				break
			}
			log.Printf("Error reading file: %v", err)
			return
		}

		if err := p.SendRaw(buffer[:bytesRead]); err != nil {
			log.Printf("Error sending chunk: %v", err)
			return
		}
	}

	log.Printf("File transfer complete: %s", fileInfo.Name)
	p.Send(map[string]string{"status": "TRANSFER_COMPLETE"})
}

func (c *Client) addWebRTCPeer(id peer.ID, p *webRTC.WebRTCPeer) {
	c.peersMux.Lock()
	defer c.peersMux.Unlock()
	c.webRTCPeers[id] = p
}

func setupGracefulShutdown(h host.Host) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ch
		log.Println("Shutting down...")
		if err := h.Close(); err != nil {
			log.Printf("Error closing host: %v", err)
		}
		os.Exit(0)
	}()
}

func formatFileSize(size int64) string {
	const unit = 1024
	if size < unit {
		return fmt.Sprintf("%d B", size)
	}
	div, exp := int64(unit), 0
	for n := size / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(size)/float64(div), "KMGTPE"[exp])
}
