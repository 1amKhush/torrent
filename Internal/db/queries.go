package db

import (
	"context"
	"database/sql"

	"encoding/json"
	"fmt" // FORMAT PACKAGE
	"log"
	"strings"
	"time"

	"github.com/google/uuid"

	_ "github.com/mattn/go-sqlite3"
)

// repository struct mein saare DB operations hai
type Repository struct {
	DB *sql.DB
}

// ek naya repo bna rha hai (say for a new user)
func NewRepository(db *sql.DB) *Repository {
	return &Repository{DB: db}
}

func (r *Repository) UpsertPeer(ctx context.Context, peerID, name string) (int64, error) {
	now := time.Now()
	var peerUUID int64 // store peer uuid AS INT
	fmt.Println("debugging 11")
	tx, err := r.DB.BeginTx(ctx, nil)
	fmt.Println("debugging 12")
	if err != nil {
		return 0, fmt.Errorf("could not begin transaction: %w", err)
	}
	defer tx.Rollback()
	fmt.Println("debugging 13")
	// Atomic upsert using ON CONFLICT
	upsertQuery := `
        INSERT INTO peers (peer_id, name, multiaddrs, is_online, last_seen, created_at)
        VALUES (?, ?, ?, 1, ?, ?)
        ON CONFLICT (peer_id) 
        DO UPDATE SET 
            name = excluded.name,
            multiaddrs = excluded.multiaddrs,
            is_online = 1,
            last_seen = excluded.last_seen
    `
	fmt.Println("debugging 14")

	selectQuery := "SELECT id FROM peers WHERE peer_id = ?"
	err = tx.QueryRowContext(ctx, selectQuery, peerID).Scan(&peerUUID)

	if err == sql.ErrNoRows {
		// Peer doesn't exist, insert new one and get the UUID
		_, err = tx.ExecContext(ctx, upsertQuery, peerID, name, "", now, now)
		if err != nil {
			fmt.Println("debugging 16")
			return 0, fmt.Errorf("failed to upsert peer: %w", err)
		}

		// ADDING THESE LINES OF CODE AS DEBUGING PROBLEM

		err = tx.QueryRowContext(ctx, selectQuery, peerID).Scan(&peerUUID)
		if err != nil {
			fmt.Println("debugging 100")
			return 0, fmt.Errorf("failed to get peer UUID after insert: %w", err)
		}
	} else if err != nil {
		// Handle other errors
		fmt.Println("debugging 101")
		return 0, fmt.Errorf("failed to query existing peer: %w", err)
	}

	//  This queryrowcontext expects return too so use execontent becoz no retunr clause in sqlite
	// err = tx.QueryRowContext(ctx, upsertQuery, peerID, name, multiaddrs, now, now).Scan(&peerUUID)

	fmt.Println("debugging 103")
	if err != nil {
		fmt.Println("debugging 102")
		return 0, fmt.Errorf("failed to upsert peer: %w", err)
	}
	fmt.Println("debugging 17")
	// Handle trust score insertion for new peers only
	trustQuery := `
        INSERT INTO trust_scores (peer_id, score, updated_at) 
        VALUES (?, 0.50, ?)
        ON CONFLICT (peer_id) DO NOTHING
    `
	fmt.Println("debugging 18")
	_, err = tx.ExecContext(ctx, trustQuery, peerID, now) // use peerID not peerUUID
	fmt.Println("debugging 19")
	if err != nil {
		fmt.Println("debugging 20")
		return 0, fmt.Errorf("failed to handle trust score: %w", err)
	}
	fmt.Println("debugging 10001")
	err = tx.Commit()
	if err != nil {
		fmt.Println("debugging commit error")
		return 0, fmt.Errorf("failed to commit transaction: %w", err)
	}
	fmt.Println("debugging transaction committed successfully")

	return peerUUID, nil
}

// currently online peers ko return karta hai
func (r *Repository) FindOnlinePeers(ctx context.Context) ([]Peer, error) {
	var multiaddrsStr string
	query := `SELECT id, peer_id, name, multiaddrs, is_online, last_seen, created_at FROM peers WHERE is_online = 1`
	fmt.Println("GG 1")
	rows, err := r.DB.QueryContext(ctx, query)
	fmt.Println("GG 2")
	if err != nil {
		fmt.Println("GG 3")
		return nil, err
	}
	defer rows.Close()
	fmt.Println("GG 4")

	var peers []Peer
	for rows.Next() {
		var peer Peer
		fmt.Println("GG 5")
		if err := rows.Scan(&peer.ID, &peer.PeerID, &peer.Name, &multiaddrsStr, &peer.IsOnline, &peer.LastSeen, &peer.CreatedAt); err != nil {
			return nil, err
		}
		peer.Multiaddrs = parseMultiaddrs(multiaddrsStr)
		fmt.Println("GG 6")
		peers = append(peers, peer)
	}
	return peers, rows.Err()
}

func parseMultiaddrs(s string) []string {
	if s == "" {
		return []string{}
	}

	// If it's JSON format like ["addr1", "addr2"]
	if strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]") {
		var result []string
		if err := json.Unmarshal([]byte(s), &result); err == nil {
			return result
		}
	}

	// If it's comma-separated like "addr1,addr2,addr3"
	parts := strings.Split(s, ",")
	// Trim whitespace from each part
	for i, part := range parts {
		parts[i] = strings.TrimSpace(part)
	}
	return parts
}

// Jab koi peer disconnect kare, use offline mark karne ke liye
func (r *Repository) SetPeerOffline(ctx context.Context, peerID string) error {
	now := time.Now()
	_, err := r.DB.ExecContext(ctx,
		`UPDATE peers SET is_online=0, last_seen=? WHERE peer_id=?`,
		now, peerID)
	return err
}

// Server start hone par sab peers ko offline mark karta hai
// tracker ko jab host karenge aur restart karna pada toh saare peeer disconnect ho jaenge aur offline mark ho jaenge
func (r *Repository) MarkAllPeersOffline(ctx context.Context) error {
	fmt.Println("nde5")

	// Add these debug checks:
	if r == nil {
		fmt.Println("ERROR: Repository r is nil!")
		return fmt.Errorf("repository is nil")
	}
	fmt.Println(r)

	if r.DB == nil {
		fmt.Println("ERROR: r.DB is nil!")
		return fmt.Errorf("database connection is nil")
	}

	fmt.Println("DEBUG: Repository and DB are not nil, attempting query...")

	_, err := r.DB.ExecContext(ctx, `UPDATE peers SET is_online=0 WHERE is_online=1`)

	fmt.Println("nde6")
	return err
}

// Peer ki full info return karta hai DB ID ke basis par
func (r *Repository) GetPeerInfoByDBID(ctx context.Context, peerDBID string) (*Peer, error) {
	var peer Peer
	var multiaddrsStr sql.NullString
	err := r.DB.QueryRowContext(ctx, `SELECT id, peer_id, name, multiaddrs, is_online, last_seen, created_at FROM peers WHERE id = ?`, peerDBID).Scan(&peer.ID, &peer.PeerID, &peer.Name, &multiaddrsStr, &peer.IsOnline, &peer.LastSeen, &peer.CreatedAt)
	if err != nil {
		return nil, err
	}
	if multiaddrsStr.Valid {
		peer.Multiaddrs = parseMultiaddrs(multiaddrsStr.String)
	}
	return &peer, nil
}


// File ko DB mein insert karta hai (hash, size, type etc. ke saath)
// Aur agar file peehle se exit kar rhi hai toh name update kar dega (hash compare karne ke baad)
func (r *Repository) InsertFile(ctx context.Context, fileHash, filename string, fileSize int64, contentType string) (string, error) {
	var existingID string
	err := r.DB.QueryRowContext(ctx, `SELECT id FROM files WHERE file_hash = ?`, fileHash).Scan(&existingID)

	if err == nil {
		// File with this hash already exists, return its ID
		return existingID, nil
	} else if err != sql.ErrNoRows {
		// An actual error occurred
		return "", err
	}


	// File does not exist, insert it and return the new ID.
	fileID := uuid.New().String()
	_, err = r.DB.ExecContext(ctx, `
        INSERT INTO files (
            id,
            file_hash,
            filename,
            file_size,
            content_type,
            created_at
        ) VALUES (?, ?, ?, ?, ?, ?)
    `,
		fileID, // ← your generated UUID
		fileHash,
		filename,
		fileSize,
		contentType,
		time.Now(),
	)
	if err != nil {
		return "", err
	}
	return fileID, nil
}

// Tracker par available saari files ka list deta hai
func (r *Repository) FindAllFiles(ctx context.Context) ([]File, error) {
	query := `SELECT id, file_hash, filename, file_size, content_type, created_at FROM files ORDER BY created_at DESC`
	rows, err := r.DB.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var files []File
	for rows.Next() {
		var file File
		if err := rows.Scan(&file.ID, &file.FileHash, &file.Filename, &file.FileSize, &file.ContentType, &file.CreatedAt); err != nil {
			return nil, err
		}
		files = append(files, file)
	}
	return files, rows.Err()
}

// peer ki chosen file tracker pe register karta hai
// peer_id + file_id ka combination unique relation store hota hai
func (r *Repository) InsertPeerFile(ctx context.Context, peerLibp2pID string, fileID string) (string, error) {
	var peerDBID string
	err := r.DB.QueryRowContext(ctx, `SELECT id FROM peers WHERE peer_id = ?`, peerLibp2pID).Scan(&peerDBID)
	if err != nil {
		return "", fmt.Errorf("failed to find peer with peer_id=%s: %w", peerLibp2pID, err)
	}

	var peerFileID string
	// insert a new file or pehle se exist kar rhi hai toh woh fetch karta hai.
	query := `
        INSERT INTO peer_files (id, peer_id, file_id, announced_at)
        VALUES (?, ?, ?, ?)
		ON CONFLICT(peer_id, file_id) DO NOTHING
    `
	newID := uuid.New().String()
	_, err = r.DB.ExecContext(ctx, query, newID, peerDBID, fileID, time.Now())
	if err != nil {
		return "", err
	}

	// Get the ID of the (possibly existing) row
	selectQuery := `SELECT id FROM peer_files WHERE peer_id = ? AND file_id = ?`
	err = r.DB.QueryRowContext(ctx, selectQuery, peerDBID, fileID).Scan(&peerFileID)
	if err != nil {
		log.Printf("[Repository] InsertPeerFile error for peerDBID %s and fileID %s: %v", peerDBID, fileID, err)
		return "", err
	}

	return peerFileID, nil
}

// Kisi file ke liye saare online peers dikhata hai (abhi ke liye basic trust score dikhata hai)
func (r *Repository) FindOnlineFilePeersByID(ctx context.Context, fileID string) ([]PeerFile, error) {
	query := `
        SELECT pf.id, pf.file_id, pf.peer_id, pf.announced_at, COALESCE(ts.score, 0.5) as score
        FROM peer_files pf
        JOIN peers p ON pf.peer_id = p.id
        LEFT JOIN trust_scores ts ON p.peer_id = ts.peer_id
        WHERE pf.file_id = ? AND p.is_online = 1
    `
	rows, err := r.DB.QueryContext(ctx, query, fileID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var peerFiles []PeerFile
	for rows.Next() {
		var pfile PeerFile
		if err := rows.Scan(&pfile.ID, &pfile.FileID, &pfile.PeerID, &pfile.AnnouncedAt, &pfile.Score); err != nil {
			return nil, err
		}
		peerFiles = append(peerFiles, pfile)
	}
	return peerFiles, rows.Err()
}