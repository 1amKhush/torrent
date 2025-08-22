package db

import (
	"time"
)

type Peer struct {
	ID         string    `db:"id"`
	PeerID     string    `db:"peer_id"`
	Name       string    `db:"name"`
	Multiaddrs []string  `db:"multiaddrs"`
	IsOnline   bool      `db:"is_online"`
	LastSeen   time.Time `db:"last_seen"`
	CreatedAt  time.Time `db:"created_at"`
}

type File struct {
	ID          string    `db:"id"`
	FileHash    string    `db:"file_hash"`
	Filename    string    `db:"filename"`
	FileSize    int64     `db:"file_size"`
	ContentType string    `db:"content_type"`
	CreatedAt   time.Time `db:"created_at"`
}

type PeerFile struct {
    ID         string    `json:"id"`
    FileID     string    `json:"file_id"`
    PeerID     string    `json:"peer_id"`
    AnnouncedAt time.Time `json:"announced_at"`
    Score      float64   `json:"score"`
}
// USELESS

// type TrustScore struct {
// 	ID                  string    `db:"id"`
// 	PeerID              string    `db:"peer_id"`
// 	Score               float64   `db:"score"`
// 	SuccessfulTransfers int       `db:"successful_transfers"`
// 	FailedTransfers     int       `db:"failed_transfers"`
// 	UpdatedAt           time.Time `db:"updated_at"`
// }
