package p2p

import (
	"context"
	"crypto/rand"
	"fmt"
	"log"
	"os"
	"sync"

	"github.com/libp2p/go-libp2p"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/routing"
	ma "github.com/multiformats/go-multiaddr"
)

const privKeyFile = "private_key"

// NewHost creates a new libp2p host with a Kademlia DHT.
func NewHost(ctx context.Context, listenAddr string) (host.Host, *dht.IpfsDHT, error) {
	priv, err := loadOrGeneratePrivateKey()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to load/generate private key: %w", err)
	}

	maddr, err := ma.NewMultiaddr(listenAddr)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to parse listen address '%s': %w", listenAddr, err)
	}

	var idht *dht.IpfsDHT

	h, err := libp2p.New(
		libp2p.Identity(priv),
		libp2p.ListenAddrs(maddr),
		libp2p.Routing(func(h host.Host) (routing.PeerRouting, error) {
			idht, err = dht.New(ctx, h)
			return idht, err
		}),
		libp2p.EnableAutoRelay(),
	)

	if err != nil {
		return nil, nil, err
	}

	log.Printf("Host created with ID: %s", h.ID())
	log.Printf("Host listening on: %s/p2p/%s", h.Addrs()[0], h.ID())
	return h, idht, nil
}

// Bootstrap connects the host to the public libp2p bootstrap peers.
func Bootstrap(ctx context.Context, h host.Host, d *dht.IpfsDHT) error {
	if err := d.Bootstrap(ctx); err != nil {
		return fmt.Errorf("failed to bootstrap DHT: %w", err)
	}

	var wg sync.WaitGroup
	for _, peerAddr := range dht.DefaultBootstrapPeers {
		peerinfo, _ := peer.AddrInfoFromP2pAddr(peerAddr)
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := h.Connect(ctx, *peerinfo); err != nil {
				log.Printf("Warning: could not connect to bootstrap peer %s: %s", peerinfo.ID, err)
			} else {
				log.Printf("Connection established with bootstrap peer %s", peerinfo.ID)
			}
		}()
	}

	wg.Wait()
	return nil
}

func loadOrGeneratePrivateKey() (crypto.PrivKey, error) {
	privBytes, err := os.ReadFile(privKeyFile)
	if os.IsNotExist(err) {
		priv, _, err := crypto.GenerateEd25519Key(rand.Reader)
		if err != nil {
			return nil, err
		}

		privBytes, err := crypto.MarshalPrivateKey(priv)
		if err != nil {
			return nil, err
		}

		if err := os.WriteFile(privKeyFile, privBytes, 0600); err != nil {
			return nil, fmt.Errorf("failed to write private key to file: %w", err)
		}

		log.Println("Generated new libp2p private key.")
		return priv, nil
	} else if err != nil {
		return nil, err
	}

	log.Println("Loaded existing libp2p private key.")
	return crypto.UnmarshalPrivateKey(privBytes)
}
