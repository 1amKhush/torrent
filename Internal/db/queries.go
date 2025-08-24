package db

import (
	"context"
	"database/sql"
	
	"time"

	"github.com/google/uuid"
	_ "github.com/mattn/go-sqlite3"
)

type Repository struct {
	DB *sql.DB
}

func NewRepository(db *sql.DB) *Repository {
	return &Repository{DB: db}
}

// AddLocalFile adds a file that this peer is sharing
func (r *Repository) AddLocalFile(ctx context.Context, cid, filename string, fileSize int64, filePath, fileHash string) error {
	query := `
	INSERT INTO local_files (id, cid, filename, file_size, file_path, file_hash, created_at)
	VALUES (?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(cid) DO UPDATE SET
		filename = excluded.filename,
		file_path = excluded.file_path`

	_, err := r.DB.ExecContext(ctx, query,
		uuid.New().String(),
		cid,
		filename,
		fileSize,
		filePath,
		fileHash,
		time.Now(),
	)

	return err
}

// GetLocalFiles returns all files shared by this peer
func (r *Repository) GetLocalFiles(ctx context.Context) ([]LocalFile, error) {
	query := `SELECT id, cid, filename, file_size, file_path, file_hash, created_at 
			  FROM local_files ORDER BY created_at DESC`

	rows, err := r.DB.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var files []LocalFile
	for rows.Next() {
		var file LocalFile
		if err := rows.Scan(
			&file.ID,
			&file.CID,
			&file.Filename,
			&file.FileSize,
			&file.FilePath,
			&file.FileHash,
			&file.CreatedAt,
		); err != nil {
			return nil, err
		}
		files = append(files, file)
	}

	return files, rows.Err()
}

// AddDownload records a completed download
func (r *Repository) AddDownload(ctx context.Context, cid, filename string, fileSize int64, downloadPath string) error {
	query := `
	INSERT INTO downloads (id, cid, filename, file_size, download_path, downloaded_at, status)
	VALUES (?, ?, ?, ?, ?, ?, 'completed')
	ON CONFLICT(cid) DO NOTHING`

	_, err := r.DB.ExecContext(ctx, query,
		uuid.New().String(),
		cid,
		filename,
		fileSize,
		downloadPath,
		time.Now(),
	)

	return err
}

// GetDownloads returns all downloaded files
func (r *Repository) GetDownloads(ctx context.Context) ([]Download, error) {
	query := `SELECT id, cid, filename, file_size, download_path, downloaded_at, status 
			  FROM downloads ORDER BY downloaded_at DESC`

	rows, err := r.DB.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var downloads []Download
	for rows.Next() {
		var download Download
		if err := rows.Scan(
			&download.ID,
			&download.CID,
			&download.Filename,
			&download.FileSize,
			&download.DownloadPath,
			&download.DownloadedAt,
			&download.Status,
		); err != nil {
			return nil, err
		}
		downloads = append(downloads, download)
	}

	return downloads, rows.Err()
}

// GetLocalFileByCID returns a specific local file by CID
func (r *Repository) GetLocalFileByCID(ctx context.Context, cid string) (*LocalFile, error) {
	query := `SELECT id, cid, filename, file_size, file_path, file_hash, created_at 
			  FROM local_files WHERE cid = ?`

	var file LocalFile
	err := r.DB.QueryRowContext(ctx, query, cid).Scan(
		&file.ID,
		&file.CID,
		&file.Filename,
		&file.FileSize,
		&file.FilePath,
		&file.FileHash,
		&file.CreatedAt,
	)

	if err != nil {
		return nil, err
	}

	return &file, nil
}

// DeleteLocalFile removes a file from sharing
func (r *Repository) DeleteLocalFile(ctx context.Context, cid string) error {
	query := `DELETE FROM local_files WHERE cid = ?`
	_, err := r.DB.ExecContext(ctx, query, cid)
	return err
}
