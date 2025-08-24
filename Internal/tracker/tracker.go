package tracker

import (
	"context"
	"database/sql"
	"log"
	db "torrentium/internal/db"

	_ "github.com/mattn/go-sqlite3"
)

type Tracker struct {
	repo *db.Repository
}

// ek naya tracker instance initialize karte hai
func NewTracker(DB *sql.DB) *Tracker {
	return &Tracker{
		repo: &db.Repository{DB: DB},
	}
}

func (t *Tracker) AddPeer(ctx context.Context, peerID, name string, multiaddrs []string) error {
	_, err := t.repo.UpsertPeer(ctx, peerID, name)
	if err != nil {
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
	_, err = t.repo.InsertPeerFile(ctx, peerID, fileID)
	if err != nil {
		return "", err
	}

	return fileID, nil
}

// GetAllFiles database mein available sabhi files ki list return karta hai.
func (t *Tracker) GetAllFiles(ctx context.Context) ([]db.File, error) {
	return t.repo.FindAllFiles(ctx)
}

// FindPeersForFile finds all online peers that have announced a given file.
func (t *Tracker) FindPeersForFile(ctx context.Context, fileID string) ([]db.Peer, error) {
	peerFiles, err := t.repo.FindOnlineFilePeersByID(ctx, fileID)
	if err != nil {
		return nil, err
	}

	var peers []db.Peer
	for _, pf := range peerFiles {
		peerInfo, err := t.repo.GetPeerInfoByDBID(ctx, pf.PeerID)
		if err != nil {
			log.Printf("Could not get peer info for DB ID %s: %v", pf.PeerID, err)
			continue
		}
		peers = append(peers, *peerInfo)
	}
	return peers, nil
}