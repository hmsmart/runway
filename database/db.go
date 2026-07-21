package database

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/hmsmart/runway/database/sqlcgen"
	"github.com/hmsmart/runway/domains"
	"github.com/jellydator/ttlcache/v3"
	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// SessionTTL is how long a web session minted from a Telegram magic link
// stays valid. Sessions live only in memory, so a restart logs everyone out.
const SessionTTL = 24 * time.Hour

type Store struct {
	*sqlcgen.Queries
	db           *sql.DB
	TGTokens     *ttlcache.Cache[string, domains.User]
	LinkTokens   *ttlcache.Cache[string, domains.User]
	RelinkItems  *ttlcache.Cache[string, string]
	UpdateItems  *ttlcache.Cache[string, string]
	TGPhotos     *ttlcache.Cache[string, domains.Photo]
	Sessions     *ttlcache.Cache[string, domains.User]
}

func newDatabase(ctx context.Context, dbPath string) (*sql.DB,
	error) {

	if err := os.MkdirAll(filepath.Dir(dbPath), 0755); err != nil {
		return nil, fmt.Errorf("create db directory: %w", err)
	}

	dsn := fmt.Sprintf("file:%s?_txlock=immediate&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)", dbPath)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	db.SetMaxOpenConns(1) // SQLite has one writer at a time
	db.SetMaxIdleConns(1)

	//Check DB alive
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}

	if err := runMigrations(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("run migrations: %w", err)
	}

	return db, nil
}

func GetStore(ctx context.Context, dbPath string, tokenTTL time.Duration) (*Store, error) {
	db, err := newDatabase(ctx, dbPath)
	if err != nil {
		return nil, fmt.Errorf("failed to create database connection: %w", err)
	}
	TGTokens := ttlcache.New(ttlcache.WithTTL[string, domains.User](tokenTTL))
	LinkTokens := ttlcache.New(ttlcache.WithTTL[string, domains.User](tokenTTL))
	RelinkItems := ttlcache.New(ttlcache.WithTTL[string, string](tokenTTL))
	UpdateItems := ttlcache.New(ttlcache.WithTTL[string, string](tokenTTL))
	TGPhotos := ttlcache.New(ttlcache.WithTTL[string, domains.Photo](24 * time.Hour))
	Sessions := ttlcache.New(ttlcache.WithTTL[string, domains.User](SessionTTL))
	go TGTokens.Start()
	go LinkTokens.Start()
	go RelinkItems.Start()
	go UpdateItems.Start()
	go TGPhotos.Start()
	go Sessions.Start()
	return &Store{
		Queries:     sqlcgen.New(db),
		db:          db,
		TGTokens:    TGTokens,
		TGPhotos:    TGPhotos,
		LinkTokens:  LinkTokens,
		RelinkItems: RelinkItems,
		UpdateItems: UpdateItems,
		Sessions:    Sessions,
	}, nil
}

func (s *Store) Ping(ctx context.Context) error {
	return s.db.PingContext(ctx)
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) ExecTx(ctx context.Context, fn func(*sqlcgen.Queries) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	q := sqlcgen.New(tx)
	if err := fn(q); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

func runMigrations(db *sql.DB) error {
	goose.SetBaseFS(migrationsFS)

	if err := goose.SetDialect("sqlite3"); err != nil {
		return fmt.Errorf("set goose dialect: %w", err)
	}

	if err := goose.Up(db, "migrations"); err != nil {
		return fmt.Errorf("goose up: %w", err)
	}

	return nil
}
