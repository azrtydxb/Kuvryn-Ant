// Package config validates startup settings without exposing secret values.
package config

import (
	"errors"
	"net"
	"os"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Config struct {
	Listen          string
	Database        *pgxpool.Config
	RootKey         []byte
	ShutdownTimeout time.Duration
	MaxRequests     int
}

func Load(getenv func(string) string) (Config, error) {
	c := Config{Listen: "127.0.0.1:8080", ShutdownTimeout: 15 * time.Second, MaxRequests: 128}
	if v := getenv("ANT_LISTEN"); v != "" {
		c.Listen = v
	}
	_, port, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return Config{}, errors.New("ANT_LISTEN must be host:port")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 0 || n > 65535 {
		return Config{}, errors.New("ANT_LISTEN port is invalid")
	}
	dsn := getenv("ANT_DATABASE_URL")
	if dsn == "" {
		return Config{}, errors.New("ANT_DATABASE_URL is required (PostgreSQL only)")
	}
	c.Database, err = pgxpool.ParseConfig(dsn)
	if err != nil {
		return Config{}, errors.New("ANT_DATABASE_URL is invalid")
	}
	c.Database.MaxConns = 16
	c.Database.MinConns = 0
	c.Database.ConnConfig.ConnectTimeout = 5 * time.Second
	keyPath := getenv("ANT_ROOT_KEY_FILE")
	if keyPath == "" {
		return Config{}, errors.New("ANT_ROOT_KEY_FILE is required")
	}
	info, err := os.Stat(keyPath)
	if err != nil || !info.Mode().IsRegular() || info.Size() != 32 {
		return Config{}, errors.New("ANT_ROOT_KEY_FILE must be a readable regular file containing exactly 32 raw bytes")
	}
	c.RootKey, err = os.ReadFile(keyPath)
	if err != nil || len(c.RootKey) != 32 {
		return Config{}, errors.New("ANT_ROOT_KEY_FILE must contain exactly 32 raw bytes")
	}
	if v := getenv("ANT_SHUTDOWN_TIMEOUT"); v != "" {
		c.ShutdownTimeout, err = time.ParseDuration(v)
		if err != nil || c.ShutdownTimeout < time.Second || c.ShutdownTimeout > time.Minute {
			return Config{}, errors.New("ANT_SHUTDOWN_TIMEOUT must be between 1s and 1m")
		}
	}
	if v := getenv("ANT_MAX_REQUESTS"); v != "" {
		c.MaxRequests, err = strconv.Atoi(v)
		if err != nil || c.MaxRequests < 1 || c.MaxRequests > 4096 {
			return Config{}, errors.New("ANT_MAX_REQUESTS must be between 1 and 4096")
		}
	}
	return c, nil
}
