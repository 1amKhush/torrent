package tracker

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	db "torrentium/internal/db"

	_ "github.com/mattn/go-sqlite3"
)

type Tracker struct {
	// peers    map[string]bool // (In-memory map )jo currently connected peers hai unke IDs ko store karta hai.
	repo *db.Repository
	// peersMux sync.RWMutex // peers map ko concurrency clashes se bachane ke liye reead and write Mutex.
}

type Repository struct {
	DB *sql.DB
}

// ek naya tracker instance initialize karte hai
func NewTracker(DB *sql.DB) *Tracker {
	return &Tracker{
		repo: &db.Repository{DB: DB},
	}
}

func (t *Tracker) AddPeer(ctx context.Context, peerID, name string, multiaddrs []string) error {
	// Map ko lock karte hain taaki race conditions na ho.

	_, err := t.repo.UpsertPeer(ctx, peerID, name)
	fmt.Println("debugging 1000")
	if err != nil {
		fmt.Println("debugging 22")
		log.Printf("Failed to upsert peer %s: %v", peerID, err)
		return err
	}
	return nil
}

// AddFileWithPeer ek file ko database mein add karta hai aur use ek peer ke saath link kar deta hai.
func (t *Tracker) AddFileWithPeer(ctx context.Context, fileHash, filename string, fileSize int64, peerID string) (string, error) {
	// Pehle file ko `files` table mein insert karte hain (ya agar exist karti hai to ID get karte hain).
	fileID, err := t.repo.InsertFile(ctx, fileHash, filename, fileSize, "")
	if err != nil {
		return "", err
	}
	// Fir `peer_files` table mein entry banakar file aur peer ko link karte hain.
	// TODO: Replace "peerID" with the actual peer ID variable as needed.
	 // Set this to the correct peer ID
	_, err = t.repo.InsertPeerFile(ctx, fileID, peerID)
	if err != nil {
		return "", err
	}

	return fileID, nil
}
