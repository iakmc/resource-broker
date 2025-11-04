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

package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"
)

var (
	fDBHostFile     = flag.String("db-host-file", "localhost", "Database host")
	fDBPortFile     = flag.String("db-port-file", "5432", "Database port")
	fDBUserFile     = flag.String("db-user-file", "user", "Database user")
	fDBPasswordFile = flag.String("db-password-file", "password", "Database password")
	fDBNameFile     = flag.String("db-name-file", "dbname", "Database name")
)

type dbWrapper struct {
	mu             sync.RWMutex
	db             *sql.DB
	lastConnString string
}

func (m *dbWrapper) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.db != nil {
		if err := m.db.Close(); err != nil {
			log.Printf("failed to close db: %v", err)
		}
		m.db = nil
	}
}

func openDB(ctx context.Context, conn string) (*sql.DB, error) {
	db, err := sql.Open("postgres", conn)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close() //nolint:errcheck,gosec
		return nil, err
	}
	return db, nil
}

func (m *dbWrapper) swap(ctx context.Context, connString string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.db != nil {
		// best-effort close
		if err := m.db.Close(); err != nil {
			return err
		}
	}
	newDB, err := openDB(ctx, connString)
	if err != nil {
		return err
	}
	log.Printf("Connected to DB successfully with new connection string: %q", connString)
	m.db = newDB
	m.lastConnString = connString
	return nil
}

func readFile(path string) (string, error) {
	b, err := os.ReadFile(path) //nolint:gosec
	if err != nil {
		return "", err
	}
	s := strings.TrimSpace(string(b))
	if s == "" {
		return "", errors.New("empty file content")
	}
	return s, nil
}

func buildConnString() (string, error) {
	dbHost, err := readFile(*fDBHostFile)
	if err != nil {
		return "", err
	}
	dbPort, err := readFile(*fDBPortFile)
	if err != nil {
		return "", err
	}
	dbUser, err := readFile(*fDBUserFile)
	if err != nil {
		return "", err
	}
	dbPassword, err := readFile(*fDBPasswordFile)
	if err != nil {
		return "", err
	}
	dbName, err := readFile(*fDBNameFile)
	if err != nil {
		return "", err
	}

	return fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=require", dbHost, dbPort, dbUser, dbPassword, dbName), nil
}

func (m *dbWrapper) reloadLoop(ctx context.Context, errCh chan error) {
	log.Println("Starting DB config reload loop...")

	// watcher would be better but seems to fail due to mount things in
	// kube
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			connString, err := buildConnString()
			if err != nil {
				errCh <- fmt.Errorf("failed to build db conn string: %w", err)
				continue
			}
			if m.lastConnString == "" {
				// first time setup
				if err := m.swap(ctx, connString); err != nil {
					errCh <- fmt.Errorf("failed to set db connection initially: %w", err)
					continue
				}
				continue
			}
			if connString == m.lastConnString {
				continue // no change
			}
			log.Println("DB connection details change detected, reloading...")
			if err := m.swap(ctx, connString); err != nil {
				errCh <- fmt.Errorf("failed to swap db connection: %w", err)
			}
		}
	}
}
