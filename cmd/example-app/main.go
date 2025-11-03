/*
Copyright 2025.
SPDX-License-Identifier: Apache-2.0

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package main is an example app connecting to a PostgreSQL database,
package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"time"

	_ "github.com/lib/pq"
)

func main() {
	if err := doMain(); err != nil {
		log.Fatalf("error: %v", err)
	}
}

var fTickerInterval = flag.Duration("ticker-interval", 1*time.Second, "Interval between operations")

func doMain() error {
	flag.Parse()

	log.Println("Starting example app...")

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	db := &dbWrapper{}
	defer db.Close()

	errCh := make(chan error, 1)
	go db.reloadLoop(ctx, errCh)

	ticker := time.NewTicker(*fTickerInterval)
	defer ticker.Stop()

	log.Printf("Starting main loop with interval %v...", *fTickerInterval)
	for {
		select {
		case <-ctx.Done():
			log.Println("Shutting down...")
			return nil
		case err := <-errCh:
			log.Printf("error in database: %v", err)
		case <-ticker.C:
			log.Println("Performing database operations...")
			if err := func() error {
				db.mu.RLock()
				defer db.mu.RUnlock()
				if db.db == nil {
					return fmt.Errorf("database not connected")
				}

				ok, err := tableExists(ctx, db.db)
				if err != nil {
					return fmt.Errorf("table exists check error: %w", err)
				}
				if !ok {
					log.Printf("table data does not exist, creating...")
					if err := createTable(ctx, db.db); err != nil {
						return fmt.Errorf("create table error: %w", err)
					}
					log.Printf("table data created")
				}

				n, err := count(ctx, db.db)
				if err != nil {
					return fmt.Errorf("count error: %w", err)
				}
				log.Printf("row count: %d", n)

				newValue := rand.Text()
				if err := addData(ctx, db.db, newValue); err != nil {
					return fmt.Errorf("add data error: %w", err)
				}
				log.Printf("added new row with value: %s", newValue)

				return nil
			}(); err != nil {
				log.Printf("operation error: %v", err)
			}
		}
	}
}

func tableExists(ctx context.Context, db *sql.DB) (bool, error) {
	query := `
	SELECT EXISTS (
		SELECT FROM information_schema.tables
		WHERE table_schema = 'public'
		AND table_name = $1
	);`
	row := db.QueryRowContext(ctx, query, "data")

	var exists bool
	if err := row.Scan(&exists); err != nil {
		return false, err
	}
	return exists, nil
}

func createTable(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
	CREATE TABLE data (
		id SERIAL PRIMARY KEY,
		value TEXT NOT NULL
	);`)
	return err
}

func count(ctx context.Context, db *sql.DB) (int, error) {
	count, err := db.QueryContext(ctx, "SELECT COUNT(*) FROM data")
	if err != nil {
		return 0, err
	}
	defer count.Close() //nolint:errcheck

	var n int
	if count.Next() {
		if err := count.Scan(&n); err != nil {
			return 0, err
		}
	}

	return n, nil
}

func addData(ctx context.Context, db *sql.DB, value string) error {
	_, err := db.ExecContext(ctx, "INSERT INTO data (value) VALUES ($1)", value)
	return err
}
