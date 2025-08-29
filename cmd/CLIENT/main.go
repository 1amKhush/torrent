package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	//"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	//"path/filepath"
	"sort"
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
	"github.com/multiformats/go-multiaddr"
	"github.com/multiformats/go-multihash"
	"github.com/pion/webrtc/v3"
)

const (
	DefaultPieceSize = 1 << 20 // 1 MiB pieces
	MaxProviders     = 10
	MaxChunk         = 64 * 1024 // 64KiB chunks
)

type Client struct {
	host            host.Host
	dht             *dht.IpfsDHT
	webRTCPeers     map[peer.ID]*webRTC.WebRTCPeer
	peersMux        sync.RWMutex
	sharingFiles    map[string]*FileInfo
	activeDownloads map[string]*os.File
	downloadsMux    sync.RWMutex
	db              *db.Repository
	rateLimitBps    int64
}

type FileInfo struct {
	FilePath string
	Hash     string
	Size     int64
	Name     string
	PieceSz  int64
}

type pieceRequest struct {
	Command string `json:"command"`
	CID     string `json:"cid"`
	Index   int64  `json:"index"`
	Offset  int64  `json:"offset"`
	Size    int64  `json:"size"`
}

type controlMessage struct {
	Command   string `json:"command"`
	CID       string `json:"cid,omitempty"`
	PieceSize int64  `json:"piece_size,omitempty"`
	TotalSize int64  `json:"total_size,omitempty"`
	HashHex   string `json:"hash_hex,omitempty"`
	NumPieces int64  `json:"num_pieces,omitempty"`
	PieceHash string `json:"piece_hash,omitempty"`
	Index     int64  `json:"index,omitempty"`
	Filename  string `json:"filename,omitempty"`
}

var (
	manifestWaiters = make(map[string]chan controlMessage)
	manifestChMu    sync.Mutex
)

func setupGracefulShutdown(h host.Host) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-ch
		log.Println("Shutting down gracefully...")
		_ = h.Close()
		os.Exit(0)
	}()
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

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := godotenv.Load(); err != nil {
		log.Printf("Warning: Could not load .env file: %v", err)
	}

	DB := db.InitDB()
	if DB == nil {
		log.Fatal("Database initialization failed")
	}

	h, d, err := p2p.NewHost(ctx, "/ip4/0.0.0.0/tcp/0")
	if err != nil {
		log.Fatal("Failed to create libp2p host:", err)
	}
	defer h.Close()

	go func() {
		if err := p2p.Bootstrap(ctx, h, d); err != nil {
			log.Printf("Error bootstrapping DHT: %v", err)
		}
	}()

	setupGracefulShutdown(h)

	repo := db.NewRepository(DB)
	client := NewClient(h, d, repo)
	client.startDHTMaintenance()
	p2p.RegisterSignalingProtocol(h, client.handleWebRTCOffer)

	client.commandLoop()
}

func (c *Client) commandLoop() {
	scanner := bufio.NewScanner(os.Stdin)
	c.printInstructions()
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
				fmt.Println("Usage: add <path>")
			} else {
				err = c.addFile(args[0])
			}
		case "list":
			c.listLocalFiles()
		case "search":
			if len(args) != 1 {
				fmt.Println("Usage: search <cid|text>")
			} else {
				c.checkConnectionHealth()
				if strings.HasPrefix(args[0], "bafy") || strings.HasPrefix(args[0], "Qm") {
					err = c.enhancedSearchByCID(args[0])
				} else {
					err = c.searchByText(args[0])
				}
			}
		case "download":
			if len(args) != 1 {
				fmt.Println("Usage: download <cid>")
			} else {
				err = c.downloadFile(args[0])
			}
		case "peers":
			c.listConnectedPeers()
		case "connect":
			if len(args) != 1 {
				fmt.Println("Usage: connect <multiaddr>")
				fmt.Println("Example: connect /ip4/127.0.0.1/tcp/54437/p2p/12D3KooWBLZFWsGZxoCFC8NsFgKvD6WJ6xV9UmYdR8t2C1kqYTcd")
			} else {
				err = c.connectToPeer(args[0])
			}
		case "announce":
			if len(args) != 1 {
				fmt.Println("Usage: announce <cid>")
			} else {
				err = c.announceFile(args[0])
			}
		case "health":
			c.checkConnectionHealth()
		case "debug":
			c.debugNetworkStatus()
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
	fmt.Println(" add <path>           - Share a file on the network")
	fmt.Println(" list                 - List your shared files")
	fmt.Println(" search <cid|text>    - Search by CID or filename text")
	fmt.Println(" download <cid>       - Download a file by CID")
	fmt.Println(" peers                - Show connected peers")
	fmt.Println(" connect <multiaddr>  - Manually connect to a peer")
	fmt.Println(" announce <cid>       - Re-announce a file to DHT")
	fmt.Println(" health               - Check connection health")
	fmt.Println(" debug                - Show detailed network debug info")
	fmt.Println(" help                 - Show this help")
	fmt.Println(" exit                 - Exit the application")
	fmt.Printf("\nYour Peer ID: %s\n", c.host.ID())
	fmt.Printf("Listening on: %v\n\n", c.host.Addrs())
}

func (c *Client) debugNetworkStatus() {
	fmt.Println("\n=== Network Debug Info ===")
	fmt.Printf("Our Peer ID: %s\n", c.host.ID())
	fmt.Printf("Our Addresses:\n")
	for _, addr := range c.host.Addrs() {
		fmt.Printf(" %s/p2p/%s\n", addr, c.host.ID())
	}

	peers := c.host.Network().Peers()
	fmt.Printf("\nConnected Peers (%d):\n", len(peers))
	for i, peerID := range peers {
		conn := c.host.Network().ConnsToPeer(peerID)
		if len(conn) > 0 {
			fmt.Printf(" %d. %s\n", i+1, peerID)
			fmt.Printf("    Address: %s\n", conn[0].RemoteMultiaddr())
		}
	}

	routingTableSize := c.dht.RoutingTable().Size()
	fmt.Printf("\nDHT Routing Table Size: %d\n", routingTableSize)
	
	fmt.Printf("\nShared Files (%d):\n", len(c.sharingFiles))
	for cid, fileInfo := range c.sharingFiles {
		fmt.Printf(" CID: %s\n", cid)
		fmt.Printf(" File: %s\n", fileInfo.Name)
		fmt.Printf(" ---\n")
	}
}

func (c *Client) announceFile(cidStr string) error {
	fileCID, err := cid.Decode(cidStr)
	if err != nil {
		return fmt.Errorf("invalid CID: %w", err)
	}
	fmt.Printf("Re-announcing CID %s to DHT...\n", cidStr)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := c.dht.Provide(ctx, fileCID, true); err != nil {
		return fmt.Errorf("failed to announce: %w", err)
	}
	fmt.Println(" - Successfully announced to DHT")
	return nil
}

func (c *Client) startDHTMaintenance() {
	go func() {
		ticker := time.NewTicker(10 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			log.Println("Performing DHT maintenance...")
			c.dht.RefreshRoutingTable()
			peers := c.host.Network().Peers()
			log.Printf("Connected to %d peers", len(peers))
			if len(peers) < 5 {
				log.Println("Low peer count; re-bootstrapping...")
				ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
				_ = p2p.Bootstrap(ctx, c.host, c.dht)
				cancel()
			}
		}
	}()
}

func (c *Client) checkConnectionHealth() {
	peers := c.host.Network().Peers()
	fmt.Printf("\n=== Connection Health ===\n")
	fmt.Printf("Connected peers: %d\n", len(peers))
	if len(peers) < 3 {
		fmt.Println(" - Warning: Low peer count. Consider restarting or checking network connectivity.")
	} else {
		fmt.Println(" - Good peer connectivity")
	}
	
	routingTableSize := c.dht.RoutingTable().Size()
	fmt.Printf("DHT routing table size: %d\n", routingTableSize)
	if routingTableSize < 10 {
		fmt.Println(" - Warning: Small DHT routing table. File discovery may be limited.")
	} else {
		fmt.Println(" - Good DHT connectivity")
	}
}

func (c *Client) addFile(filePath string) error {
	ctx := context.Background()
	f, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("failed to open file: %w", err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("failed to get file info: %w", err)
	}

	hasher := sha256.New()
	if _, err := io.Copy(hasher, f); err != nil {
		return fmt.Errorf("failed to calculate hash: %w", err)
	}

	fileHashBytes := hasher.Sum(nil)
	fileHashStr := hex.EncodeToString(fileHashBytes)
	mhash, err := multihash.Encode(fileHashBytes, multihash.SHA2_256)
	if err != nil {
		return fmt.Errorf("failed to create multihash: %w", err)
	}

	fileCID := cid.NewCidV1(cid.Raw, mhash)

	// Create pieces manifest
	pieceSz := int64(DefaultPieceSize)
	numPieces := (info.Size() + pieceSz - 1) / pieceSz
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}

	for idx := int64(0); idx < numPieces; idx++ {
		offset := idx * pieceSz
		size := min64(pieceSz, info.Size()-offset)
		h := sha256.New()
		if _, err := io.CopyN(h, f, size); err != nil {
			return err
		}
		ph := hex.EncodeToString(h.Sum(nil))
		if err := c.db.UpsertPiece(ctx, fileCID.String(), idx, offset, size, ph, true); err != nil {
			return err
		}
	}

	if err := c.db.AddLocalFile(ctx, fileCID.String(), info.Name(), info.Size(), filePath, fileHashStr); err != nil {
		return fmt.Errorf("failed to store file metadata: %w", err)
	}

	c.sharingFiles[fileCID.String()] = &FileInfo{
		FilePath: filePath,
		Hash:     fileHashStr,
		Size:     info.Size(),
		Name:     info.Name(),
		PieceSz:  pieceSz,
	}

	log.Printf("Announcing file %s with CID %s to DHT...", info.Name(), fileCID.String())
	provideCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if err := c.dht.Provide(provideCtx, fileCID, true); err != nil {
		log.Printf(" - Warning: Failed to announce to DHT: %v", err)
	} else {
		log.Println(" - Successfully announced file to DHT")
	}

	fmt.Printf("✓ File '%s' is now being shared\n", info.Name())
	fmt.Printf(" CID: %s\n", fileCID.String())
	fmt.Printf(" Hash: %s\n", fileHashStr)
	fmt.Printf(" Size: %s\n", formatFileSize(info.Size()))
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
		fmt.Println(" - No files being shared.")
		return
	}
	fmt.Println("\n=== Your Shared Files ===")
	for _, file := range files {
		fmt.Printf("Name: %s\n", file.Filename)
		fmt.Printf(" CID: %s\n", file.CID)
		fmt.Printf(" Size: %s\n", formatFileSize(file.FileSize))
		fmt.Printf(" Path: %s\n", file.FilePath)
		fmt.Println(" ---")
	}
}

func (c *Client) searchByText(q string) error {
	ctx := context.Background()
	matches, err := c.db.SearchByFilename(ctx, q)
	if err != nil {
		return err
	}
	if len(matches) == 0 {
		fmt.Printf("Searching for files containing '%s'...\n", q)
		fmt.Println("Note: Direct filename search requires content indexing.")
		fmt.Println("Try using the CID if you have it, or check with known peers.")
		return nil
	}
	fmt.Printf("Local index matches for '%s':\n", q)
	for _, m := range matches {
		fmt.Printf("- %s  CID:%s\n", m.Filename, m.CID)
	}
	return nil
}

func (c *Client) enhancedSearchByCID(cidStr string) error {
	fileCID, err := cid.Decode(cidStr)
	if err != nil {
		return fmt.Errorf("invalid CID: %w", err)
	}
	fmt.Printf("Searching for CID: %s\n", fileCID.String())
	providers, err := c.findProvidersWithTimeout(fileCID, 60*time.Second, MaxProviders)
	if err != nil {
		return fmt.Errorf("provider search failed: %w", err)
	}

	if len(providers) == 0 {
		fmt.Println("No providers found for this CID")
		fmt.Println("This could mean:")
		fmt.Println(" - The file is not being shared")
		fmt.Println(" - The provider is offline")
		fmt.Println(" - Network connectivity issues")
		fmt.Println(" - DHT routing problems")
		return nil
	}

	fmt.Printf("Found %d provider(s):\n", len(providers))
	for i, provider := range providers {
		fmt.Printf(" %d. %s\n", i+1, provider.ID)
		if c.host.Network().Connectedness(provider.ID) == network.Connected {
			fmt.Printf(" - Already connected\n")
		} else {
			fmt.Printf(" - Not connected\n")
		}
	}
	return nil
}

func (c *Client) findProvidersWithTimeout(id cid.Cid, timeout time.Duration, maxProviders int) ([]peer.AddrInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	providersChan := c.dht.FindProvidersAsync(ctx, id, maxProviders)
	var providers []peer.AddrInfo
	var totalFound int

	done := make(chan struct{})
	go func() {
		defer close(done)
		for provider := range providersChan {
			totalFound++
			if provider.ID != c.host.ID() {
				providers = append(providers, provider)
				fmt.Printf(" - Found provider %d: %s\n", len(providers), provider.ID)
				if len(providers) >= maxProviders {
					break
				}
			}
		}
	}()

	select {
	case <-done:
		fmt.Printf("Provider search completed. Found %d total providers, %d unique external providers\n",
			totalFound, len(providers))
	case <-time.After(timeout):
		fmt.Printf("Provider search timed out. Found %d providers so far\n", len(providers))
	}

	return providers, nil
}

func (c *Client) connectToPeer(multiaddrStr string) error {
	addr, err := multiaddr.NewMultiaddr(multiaddrStr)
	if err != nil {
		return fmt.Errorf("invalid multiaddr: %w", err)
	}

	peerInfo, err := peer.AddrInfoFromP2pAddr(addr)
	if err != nil {
		return fmt.Errorf("failed to parse peer info: %w", err)
	}

	fmt.Printf("Attempting to connect to peer %s...\n", peerInfo.ID)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := c.host.Connect(ctx, *peerInfo); err != nil {
		return fmt.Errorf("failed to connect: %w", err)
	}

	fmt.Printf(" - Successfully connected to peer %s\n", peerInfo.ID)
	return nil
}

func (c *Client) listConnectedPeers() {
	peers := c.host.Network().Peers()
	fmt.Printf("\n=== Connected Peers (%d) ===\n", len(peers))
	for _, peerID := range peers {
		conn := c.host.Network().ConnsToPeer(peerID)
		if len(conn) > 0 {
			fmt.Printf("Peer: %s\n", peerID)
			fmt.Printf(" Address: %s\n", conn[0].RemoteMultiaddr())
		}
	}
}

func (c *Client) downloadFile(cidStr string) error {
	ctx := context.Background()
	fileCID, err := cid.Decode(cidStr)
	if err != nil {
		return fmt.Errorf("invalid CID: %w", err)
	}

	fmt.Printf("Looking for providers of CID: %s\n", fileCID.String())
	providers, err := c.findProvidersWithTimeout(fileCID, 120*time.Second, 5)
	if err != nil {
		return fmt.Errorf("provider search failed: %w", err)
	}

	if len(providers) == 0 {
		return fmt.Errorf("no providers found for CID: %s", fileCID.String())
	}

	fmt.Printf("Found %d provider(s). Attempting connections...\n", len(providers))
	
	// Sort by peer score
	sort.Slice(providers, func(i, j int) bool {
		si, _ := c.db.GetPeerScore(ctx, providers[i].ID.String())
		sj, _ := c.db.GetPeerScore(ctx, providers[j].ID.String())
		return si > sj
	})

	var lastErr error
	for i, provider := range providers {
		fmt.Printf("Trying provider %d/%d: %s\n", i+1, len(providers), provider.ID)
		webrtcPeer, err := c.initiateWebRTCConnectionWithRetry(provider.ID, 2)
		if err != nil {
			fmt.Printf("Failed to connect to provider %s: %v\n", provider.ID, err)
			_ = c.db.SetPeerScore(ctx, provider.ID.String(), -2)
			lastErr = err
			continue
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

		if err := webrtcPeer.SendJSON(request); err != nil {
			webrtcPeer.Close()
			localFile.Close()
			os.Remove(downloadPath)
			fmt.Printf("Failed to send file request to provider %s: %v\n", provider.ID, err)
			lastErr = err
			continue
		}

		fmt.Println("Download initiated successfully!")
		_ = c.db.SetPeerScore(ctx, provider.ID.String(), +3)
		return nil
	}

	return fmt.Errorf("failed to connect to any compatible provider. Last error: %w", lastErr)
}

func (c *Client) initiateWebRTCConnectionWithRetry(targetPeerID peer.ID, maxRetries int) (*webRTC.WebRTCPeer, error) {
	var lastErr error
	for attempt := 1; attempt <= maxRetries; attempt++ {
		if attempt > 1 {
			backoff := time.Duration(1<<uint(attempt-1)) * time.Second
			fmt.Printf("Retrying in %v (attempt %d/%d)...\n", backoff, attempt, maxRetries)
			time.Sleep(backoff)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		
		// Try to find peer address info
		var info peer.AddrInfo
		info.ID = targetPeerID
		if pinfo, err := c.dht.FindPeer(ctx, targetPeerID); err == nil && len(pinfo.Addrs) > 0 {
			info = pinfo
			fmt.Printf("Found %d addresses via DHT\n", len(info.Addrs))
		} else {
			fmt.Printf("DHT lookup failed: %v\n", err)
			cancel()
			continue
		}
		cancel()

		if len(info.Addrs) == 0 {
			lastErr = fmt.Errorf("peer %s has no known multiaddrs", targetPeerID)
			continue
		}

		c.host.Peerstore().AddAddrs(info.ID, info.Addrs, time.Hour)

		// Connect if not already connected
		if c.host.Network().Connectedness(info.ID) != network.Connected {
			fmt.Printf("Connecting to peer %s...\n", info.ID)
			connectCtx, connectCancel := context.WithTimeout(context.Background(), 45*time.Second)
			err := c.host.Connect(connectCtx, info)
			connectCancel()
			if err != nil {
				lastErr = fmt.Errorf("failed to connect to peer %s: %w", info.ID, err)
				continue
			}
			fmt.Printf("Successfully connected to peer %s\n", info.ID)
			time.Sleep(2 * time.Second) // Let connection stabilize
		}

		// Create WebRTC peer
		webrtcPeer, err := webRTC.NewWebRTCPeer(c.onDataChannelMessage)
		if err != nil {
			lastErr = err
			continue
		}

		// Create offer
		offer, err := webrtcPeer.CreateOffer()
		if err != nil {
			webrtcPeer.Close()
			lastErr = err
			continue
		}

		// Create signaling stream
		streamCtx, streamCancel := context.WithTimeout(context.Background(), 30*time.Second)
		s, err := c.host.NewStream(streamCtx, targetPeerID, p2p.SignalingProtocolID)
		streamCancel()
		if err != nil {
			webrtcPeer.Close()
			lastErr = err
			continue
		}

		webrtcPeer.SetSignalingStream(s)

		// Send offer and receive answer
		encoder := json.NewEncoder(s)
		decoder := json.NewDecoder(s)

		if err := encoder.Encode(offer); err != nil {
			webrtcPeer.Close()
			lastErr = err
			continue
		}

		var answer string
		if err := decoder.Decode(&answer); err != nil {
			webrtcPeer.Close()
			lastErr = err
			continue
		}

		if strings.HasPrefix(answer, "ERROR:") {
			webrtcPeer.Close()
			lastErr = fmt.Errorf(answer)
			continue
		}

		if err := webrtcPeer.SetAnswer(answer); err != nil {
			webrtcPeer.Close()
			lastErr = err
			continue
		}

		// Wait for connection
		if err := webrtcPeer.WaitForConnection(30 * time.Second); err != nil {
			webrtcPeer.Close()
			lastErr = err
			continue
		}

		fmt.Printf("WebRTC connection established with %s\n", targetPeerID)
		c.peersMux.Lock()
		c.webRTCPeers[targetPeerID] = webrtcPeer
		c.peersMux.Unlock()
		return webrtcPeer, nil
	}
	return nil, lastErr
}

// WebRTC offer handler for incoming connections
func (c *Client) handleWebRTCOffer(offer, remotePeerID string, s network.Stream) (string, error) {
	peerID, err := peer.Decode(remotePeerID)
	if err != nil {
		return "", fmt.Errorf("invalid peer ID: %w", err)
	}

	webrtcPeer, err := webRTC.NewWebRTCPeer(c.onDataChannelMessage)
	if err != nil {
		return "", err
	}

	webrtcPeer.SetSignalingStream(s)
	answer, err := webrtcPeer.CreateAnswer(offer)
	if err != nil {
		webrtcPeer.Close()
		return "", err
	}

	c.peersMux.Lock()
	c.webRTCPeers[peerID] = webrtcPeer
	c.peersMux.Unlock()

	return answer, nil
}

// DataChannel message handler
func (c *Client) onDataChannelMessage(msg webrtc.DataChannelMessage, peer *webRTC.WebRTCPeer) {
	if msg.IsString {
		var ctrl controlMessage
		if err := json.Unmarshal(msg.Data, &ctrl); err == nil {
			c.handleControlMessage(ctrl, peer)
		}
	} else {
		// Binary data - write to file
		if w := peer.GetFileWriter(); w != nil {
			_, _ = w.Write(msg.Data)
		}
	}
}

func (c *Client) handleControlMessage(ctrl controlMessage, peer *webRTC.WebRTCPeer) {
	ctx := context.Background()
	switch ctrl.Command {
	case "REQUEST_FILE":
		c.handleFileRequest(ctx, ctrl, peer)
	case "REQUEST_MANIFEST":
		c.handleManifestRequest(ctx, ctrl, peer)
	case "MANIFEST":
		manifestChMu.Lock()
		ch := manifestWaiters[ctrl.CID]
		if ch != nil {
			ch <- ctrl
		}
		manifestChMu.Unlock()
	case "PIECE_OK":
		// Acknowledgment - no action needed
	default:
		log.Printf("Unknown control command: %s", ctrl.Command)
	}
}

func (c *Client) handleFileRequest(ctx context.Context, ctrl controlMessage, peer *webRTC.WebRTCPeer) {
	fileInfo, exists := c.sharingFiles[ctrl.CID]
	if !exists {
		localFile, err := c.db.GetLocalFileByCID(ctx, ctrl.CID)
		if err != nil {
			log.Printf("File not found: %s", ctrl.CID)
			return
		}
		fileInfo = &FileInfo{
			FilePath: localFile.FilePath,
			Hash:     localFile.FileHash,
			Size:     localFile.FileSize,
			Name:     localFile.Filename,
		}
	}

	file, err := os.Open(fileInfo.FilePath)
	if err != nil {
		log.Printf("Error opening file %s: %v", fileInfo.FilePath, err)
		return
	}
	defer file.Close()

	// Send file in chunks
	buffer := make([]byte, MaxChunk)
	for {
		n, err := file.Read(buffer)
		if n > 0 {
			if sendErr := peer.SendRaw(buffer[:n]); sendErr != nil {
				log.Printf("Error sending data: %v", sendErr)
				break
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			log.Printf("Error reading file: %v", err)
			break
		}
	}
}

func (c *Client) handleManifestRequest(ctx context.Context, ctrl controlMessage, peer *webRTC.WebRTCPeer) {
	localFile, err := c.db.GetLocalFileByCID(ctx, ctrl.CID)
	if err != nil {
		log.Printf("File not found for manifest: %s", ctrl.CID)
		return
	}

	pieces, err := c.db.GetPieces(ctx, ctrl.CID)
	if err != nil {
		log.Printf("Error getting pieces: %v", err)
		return
	}

	manifest := controlMessage{
		Command:   "MANIFEST",
		CID:       ctrl.CID,
		TotalSize: localFile.FileSize,
		HashHex:   localFile.FileHash,
		NumPieces: int64(len(pieces)),
		Filename:  localFile.Filename,
	}

	if err := peer.SendJSON(manifest); err != nil {
		log.Printf("Error sending manifest: %v", err)
	}
}

// Helper functions
func formatFileSize(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(bytes)/float64(div), "KMGTPE"[exp])
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
