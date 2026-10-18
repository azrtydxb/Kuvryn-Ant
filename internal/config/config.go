// Package config validates startup settings without exposing secret values.
package config

import (
	"errors"
	"github.com/azrtydxb/Kuvryn-Ant/internal/secrets"
	"github.com/azrtydxb/Kuvryn-Ant/internal/storage"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Config struct {
	UpstreamCAs     map[string]string
	Storage         storage.Config
	InstanceID      string
	PublicURL       string
	Listen          string
	Database        *pgxpool.Config
	RootKey         []byte
	SecretKeys      *secrets.Keyring
	ShutdownTimeout time.Duration
	MaxRequests     int
	Retention       Retention
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
	c.Retention, err = loadRetention(getenv)
	if err != nil {
		return Config{}, err
	}
	c.UpstreamCAs, err = upstreamCAs(getenv("ANT_UPSTREAM_CA_MAP_FILE"))
	if err != nil {
		return Config{}, err
	}
	c.InstanceID = getenv("ANT_INSTANCE_ID")
	c.PublicURL = getenv("ANT_PUBLIC_URL")
	directory := getenv("ANT_DATA_DIR")
	if directory == "" {
		directory = ".local/data"
	}
	directory, err = filepath.Abs(directory)
	if err != nil {
		return Config{}, errors.New("ANT_DATA_DIR is invalid")
	}
	c.Storage = storage.Config{Backend: getenv("ANT_STORAGE_BACKEND"), Directory: directory, Endpoint: getenv("ANT_S3_ENDPOINT"), Region: getenv("ANT_S3_REGION"), Bucket: getenv("ANT_S3_BUCKET"), Prefix: getenv("ANT_S3_PREFIX"), CredentialsFile: getenv("ANT_S3_CREDENTIALS_FILE"), CAFile: getenv("ANT_S3_CA_FILE")}
	if value := getenv("ANT_S3_PATH_STYLE"); value != "" {
		c.Storage.PathStyle, err = strconv.ParseBool(value)
		if err != nil {
			return Config{}, errors.New("ANT_S3_PATH_STYLE is invalid")
		}
	}
	if value := getenv("ANT_S3_ALLOW_INSECURE_HTTP"); value != "" {
		c.Storage.AllowInsecureHTTP, err = strconv.ParseBool(value)
		if err != nil {
			return Config{}, errors.New("ANT_S3_ALLOW_INSECURE_HTTP is invalid")
		}
	}
	if c.Storage.Backend != "" && c.Storage.Backend != "disk" && c.Storage.Backend != "s3" {
		return Config{}, errors.New("ANT_STORAGE_BACKEND must be disk or s3")
	}
	keyPath := getenv("ANT_ROOT_KEY_FILE")
	if keyPath == "" {
		return Config{}, errors.New("ANT_ROOT_KEY_FILE is required")
	}
	// Reject special files before opening (a FIFO could block startup).
	info, err := os.Stat(keyPath)
	if err != nil || !info.Mode().IsRegular() || info.Size() != 32 {
		return Config{}, errors.New("ANT_ROOT_KEY_FILE must be a readable regular file containing exactly 32 raw bytes")
	}
	key, err := os.Open(keyPath)
	if err != nil {
		return Config{}, errors.New("ANT_ROOT_KEY_FILE must be a readable regular file containing exactly 32 raw bytes")
	}
	defer key.Close()
	// Inspect and read the same descriptor; projected Kubernetes secrets may use
	// symlinks that rotate between calls. Bound the read even if the file grows.
	info, err = key.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != 32 {
		return Config{}, errors.New("ANT_ROOT_KEY_FILE must be a readable regular file containing exactly 32 raw bytes")
	}
	c.RootKey, err = io.ReadAll(io.LimitReader(key, 33))
	if err != nil || len(c.RootKey) != 32 {
		clear(c.RootKey)
		return Config{}, errors.New("ANT_ROOT_KEY_FILE must contain exactly 32 raw bytes")
	}
	version := getenv("ANT_ROOT_KEY_VERSION")
	if version == "" {
		version = "v1"
	}
	c.SecretKeys, err = secrets.LoadFiles(version, c.RootKey, getenv("ANT_PREVIOUS_ROOT_KEYS_FILE"))
	if err != nil {
		clear(c.RootKey)
		return Config{}, errors.New("root keyring configuration is invalid")
	}
	return c, nil
}
