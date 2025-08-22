package db

import (
	"database/sql"
	"fmt"
	"log"
	"os"

	// "github.com/jackc/pgx/v5/pgxpool"

	"github.com/joho/godotenv"
	_ "github.com/mattn/go-sqlite3"
)

var DB *sql.DB

func InitDB() *sql.DB {
	if err := godotenv.Load("C:\\Users\\myind\\Downloads\\FINAL_TORRENTBASE CODE\\Torrentium\\.env"); err != nil {
		log.Printf("Warning: Could not load .env file: %v. Proceeding with environment variables.", err)
	}

	dbpath := os.Getenv("SQLITE_DB_PATH")

	fmt.Println("dB path :")
	fmt.Println(dbpath)

	// if dbpath == "" {
	// 	dbpath = "./peers.db" // Default fallback
	// 	log.Printf("SQLITE_DB_PATH not set, using default: %s", dbpath)
	// }

	var err error
	DB, err = sql.Open("sqlite3", dbpath)
	if err != nil {
		log.Fatalf("Error creating DB pool: %v\n", err)
	}

	if err = DB.Ping(); err != nil {
		log.Fatalf("Error connecting to DB: %v\n", err)
	}

	log.Println("-> Successfully connected to DB")
	return DB
}
