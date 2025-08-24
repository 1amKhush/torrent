package db

import (
	"database/sql"
	"fmt"
	"log"
	"os"

	"github.com/joho/godotenv"
	_ "github.com/mattn/go-sqlite3"
)

var DB *sql.DB

func InitDB() *sql.DB {
	if err := godotenv.Load(); err != nil {
		log.Printf("Warning: Could not load .env file: %v", err)
	}

	dbpath := os.Getenv("SQLITE_DB_PATH")
	if dbpath == "" {
		dbpath = "./peer.db" // Default path for peer's own database
	}

	var err error
	DB, err = sql.Open("sqlite3", dbpath)
	if err != nil {
		log.Fatalf("Error creating DB connection: %v", err)
	}

	if err = DB.Ping(); err != nil {
		log.Fatalf("Error connecting to DB: %v", err)
	}

	// Create tables for this peer's own data
	if err := createTables(DB); err != nil {
		log.Fatalf("Error creating tables: %v", err)
	}

	log.Println("Successfully connected to peer database")
	return DB
}

func createTables(db *sql.DB) error {
	// Table for this peer's shared files
	createLocalFilesTable := `
	CREATE TABLE IF NOT EXISTS local_files (
		id TEXT PRIMARY KEY,
		cid TEXT UNIQUE NOT NULL,
		filename TEXT NOT NULL,
		file_size INTEGER NOT NULL,
		file_path TEXT NOT NULL,
		file_hash TEXT NOT NULL,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);`

	// Table for downloaded files metadata
	createDownloadsTable := `
	CREATE TABLE IF NOT EXISTS downloads (
		id TEXT PRIMARY KEY,
		cid TEXT UNIQUE NOT NULL,
		filename TEXT NOT NULL,
		file_size INTEGER NOT NULL,
		download_path TEXT NOT NULL,
		downloaded_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		status TEXT DEFAULT 'completed'
	);`

	if _, err := db.Exec(createLocalFilesTable); err != nil {
		return fmt.Errorf("error creating local_files table: %w", err)
	}

	if _, err := db.Exec(createDownloadsTable); err != nil {
		return fmt.Errorf("error creating downloads table: %w", err)
	}

	return nil
}
