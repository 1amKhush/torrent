package db

import "time"

// LocalFile represents a file shared by this peer
type LocalFile struct {
	ID        string    `db:"id"`
	CID       string    `db:"cid"`
	Filename  string    `db:"filename"`
	FileSize  int64     `db:"file_size"`
	FilePath  string    `db:"file_path"`
	FileHash  string    `db:"file_hash"`
	CreatedAt time.Time `db:"created_at"`
}

// Download represents a file downloaded by this peer
type Download struct {
	ID           string    `db:"id"`
	CID          string    `db:"cid"`
	Filename     string    `db:"filename"`
	FileSize     int64     `db:"file_size"`
	DownloadPath string    `db:"download_path"`
	DownloadedAt time.Time `db:"downloaded_at"`
	Status       string    `db:"status"`
}
