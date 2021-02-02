package litestream

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc64"
	"io"
	"io/ioutil"
	"log"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Default DB settings.
const (
	DefaultMonitorInterval    = 1 * time.Second
	DefaultCheckpointInterval = 1 * time.Minute
	DefaultMinCheckpointPageN = 1000
	DefaultMaxCheckpointPageN = 10000
)

// MaxIndex is the maximum possible WAL index.
// If this index is reached then a new generation will be started.
const MaxIndex = 0x7FFFFFFF

// BusyTimeout is the timeout to wait for EBUSY from SQLite.
const BusyTimeout = 1 * time.Second

// DB represents a managed instance of a SQLite database in the file system.
type DB struct {
	mu       sync.RWMutex
	path     string        // part to database
	db       *sql.DB       // target database
	rtx      *sql.Tx       // long running read transaction
	pageSize int           // page size, in bytes
	notify   chan struct{} // closes on WAL change

	uid, gid       int // db user/group obtained on init
	mode           os.FileMode
	diruid, dirgid int // db parent user/group obtained on init
	dirmode        os.FileMode

	ctx    context.Context
	cancel func()
	wg     sync.WaitGroup

	// Metrics
	dbSizeGauge                 prometheus.Gauge
	walSizeGauge                prometheus.Gauge
	totalWALBytesCounter        prometheus.Counter
	shadowWALIndexGauge         prometheus.Gauge
	shadowWALSizeGauge          prometheus.Gauge
	syncNCounter                prometheus.Counter
	syncErrorNCounter           prometheus.Counter
	syncSecondsCounter          prometheus.Counter
	checkpointNCounterVec       *prometheus.CounterVec
	checkpointErrorNCounterVec  *prometheus.CounterVec
	checkpointSecondsCounterVec *prometheus.CounterVec

	// Minimum threshold of WAL size, in pages, before a passive checkpoint.
	// A passive checkpoint will attempt a checkpoint but fail if there are
	// active transactions occurring at the same time.
	MinCheckpointPageN int

	// Maximum threshold of WAL size, in pages, before a forced checkpoint.
	// A forced checkpoint will block new transactions and wait for existing
	// transactions to finish before issuing a checkpoint and resetting the WAL.
	//
	// If zero, no checkpoints are forced. This can cause the WAL to grow
	// unbounded if there are always read transactions occurring.
	MaxCheckpointPageN int

	// Time between automatic checkpoints in the WAL. This is done to allow
	// more fine-grained WAL files so that restores can be performed with
	// better precision.
	CheckpointInterval time.Duration

	// Frequency at which to perform db sync.
	MonitorInterval time.Duration

	// List of replicas for the database.
	// Must be set before calling Open().
	Replicas []Replica
}

// NewDB returns a new instance of DB for a given path.
func NewDB(path string) *DB {
	db := &DB{
		path:   path,
		notify: make(chan struct{}),
		uid:    -1, gid: -1, mode: 0600,
		diruid: -1, dirgid: -1, dirmode: 0700,

		MinCheckpointPageN: DefaultMinCheckpointPageN,
		MaxCheckpointPageN: DefaultMaxCheckpointPageN,
		CheckpointInterval: DefaultCheckpointInterval,
		MonitorInterval:    DefaultMonitorInterval,
	}

	db.dbSizeGauge = dbSizeGaugeVec.WithLabelValues(db.path)
	db.walSizeGauge = walSizeGaugeVec.WithLabelValues(db.path)
	db.totalWALBytesCounter = totalWALBytesCounterVec.WithLabelValues(db.path)
	db.shadowWALIndexGauge = shadowWALIndexGaugeVec.WithLabelValues(db.path)
	db.shadowWALSizeGauge = shadowWALSizeGaugeVec.WithLabelValues(db.path)
	db.syncNCounter = syncNCounterVec.WithLabelValues(db.path)
	db.syncErrorNCounter = syncErrorNCounterVec.WithLabelValues(db.path)
	db.syncSecondsCounter = syncSecondsCounterVec.WithLabelValues(db.path)
	db.checkpointNCounterVec = checkpointNCounterVec.MustCurryWith(prometheus.Labels{"db": db.path})
	db.checkpointErrorNCounterVec = checkpointErrorNCounterVec.MustCurryWith(prometheus.Labels{"db": db.path})
	db.checkpointSecondsCounterVec = checkpointSecondsCounterVec.MustCurryWith(prometheus.Labels{"db": db.path})

	db.ctx, db.cancel = context.WithCancel(context.Background())

	return db
}

// SQLDB returns a reference to the underlying sql.DB connection.
func (db *DB) SQLDB() *sql.DB {
	return db.db
}

// Path returns the path to the database.
func (db *DB) Path() string {
	return db.path
}

// WALPath returns the path to the database's WAL file.
func (db *DB) WALPath() string {
	return db.path + "-wal"
}

// MetaPath returns the path to the database metadata.
func (db *DB) MetaPath() string {
	dir, file := filepath.Split(db.path)
	return filepath.Join(dir, "."+file+MetaDirSuffix)
}

// GenerationNamePath returns the path of the name of the current generation.
func (db *DB) GenerationNamePath() string {
	return filepath.Join(db.MetaPath(), "generation")
}

// GenerationPath returns the path of a single generation.
// Panics if generation is blank.
func (db *DB) GenerationPath(generation string) string {
	assert(generation != "", "generation name required")
	return filepath.Join(db.MetaPath(), "generations", generation)
}

// ShadowWALDir returns the path of the shadow wal directory.
// Panics if generation is blank.
func (db *DB) ShadowWALDir(generation string) string {
	return filepath.Join(db.GenerationPath(generation), "wal")
}

// ShadowWALPath returns the path of a single shadow WAL file.
// Panics if generation is blank or index is negative.
func (db *DB) ShadowWALPath(generation string, index int) string {
	assert(index >= 0, "shadow wal index cannot be negative")
	return filepath.Join(db.ShadowWALDir(generation), FormatWALPath(index))
}

// CurrentShadowWALPath returns the path to the last shadow WAL in a generation.
func (db *DB) CurrentShadowWALPath(generation string) (string, error) {
	index, _, err := db.CurrentShadowWALIndex(generation)
	if err != nil {
		return "", err
	}
	return db.ShadowWALPath(generation, index), nil
}

// CurrentShadowWALIndex returns the current WAL index & total size.
func (db *DB) CurrentShadowWALIndex(generation string) (index int, size int64, err error) {
	fis, err := ioutil.ReadDir(filepath.Join(db.GenerationPath(generation), "wal"))
	if os.IsNotExist(err) {
		return 0, 0, nil // no wal files written for generation
	} else if err != nil {
		return 0, 0, err
	}

	// Find highest wal index.
	for _, fi := range fis {
		if !strings.HasSuffix(fi.Name(), WALExt) {
			continue
		}
		if v, _, _, err := ParseWALPath(fi.Name()); err != nil {
			continue // invalid wal filename
		} else if v > index {
			index = v
		}

		size += fi.Size()
	}
	return index, size, nil
}

// Replica returns a replica by name.
func (db *DB) Replica(name string) Replica {
	for _, r := range db.Replicas {
		if r.Name() == name {
			return r
		}
	}
	return nil
}

// Pos returns the current position of the database.
func (db *DB) Pos() (Pos, error) {
	generation, err := db.CurrentGeneration()
	if err != nil {
		return Pos{}, err
	} else if generation == "" {
		return Pos{}, nil
	}

	index, _, err := db.CurrentShadowWALIndex(generation)
	if err != nil {
		return Pos{}, err
	}

	fi, err := os.Stat(db.ShadowWALPath(generation, index))
	if os.IsNotExist(err) {
		return Pos{Generation: generation, Index: index}, nil
	} else if err != nil {
		return Pos{}, err
	}

	return Pos{Generation: generation, Index: index, Offset: frameAlign(fi.Size(), db.pageSize)}, nil
}

// Notify returns a channel that closes when the shadow WAL changes.
func (db *DB) Notify() <-chan struct{} {
	db.mu.RLock()
	defer db.mu.RUnlock()
	return db.notify
}

// PageSize returns the page size of the underlying database.
// Only valid after database exists & Init() has successfully run.
func (db *DB) PageSize() int {
	db.mu.RLock()
	defer db.mu.RUnlock()
	return db.pageSize
}

// Open initializes the background monitoring goroutine.
func (db *DB) Open() (err error) {
	// Validate that all replica names are unique.
	m := make(map[string]struct{})
	for _, r := range db.Replicas {
		if _, ok := m[r.Name()]; ok {
			return fmt.Errorf("duplicate replica name: %q", r.Name())
		}
		m[r.Name()] = struct{}{}
	}

	// Clear old temporary files that my have been left from a crash.
	if err := removeTmpFiles(db.MetaPath()); err != nil {
		return fmt.Errorf("cannot remove tmp files: %w", err)
	}

	// Start monitoring SQLite database in a separate goroutine.
	if db.MonitorInterval > 0 {
		db.wg.Add(1)
		go func() { defer db.wg.Done(); db.monitor() }()
	}

	return nil
}

// Close releases the read lock & closes the database. This method should only
// be called by tests as it causes the underlying database to be checkpointed.
func (db *DB) Close() (err error) {
	if e := db.SoftClose(); e != nil && err == nil {
		err = e
	}

	if db.db != nil {
		if e := db.db.Close(); e != nil && err == nil {
			err = e
		}
	}
	return err
}

// UpdatedAt returns the last modified time of the database or WAL file.
func (db *DB) UpdatedAt() (time.Time, error) {
	// Determine database modified time.
	fi, err := os.Stat(db.Path())
	if err != nil {
		return time.Time{}, err
	}
	t := fi.ModTime().UTC()

	// Use WAL modified time, if available & later.
	if fi, err := os.Stat(db.WALPath()); os.IsNotExist(err) {
		return t, nil
	} else if err != nil {
		return t, err
	} else if fi.ModTime().After(t) {
		t = fi.ModTime().UTC()
	}
	return t, nil
}

// Snapshots returns a list of all snapshots across all replicas.
func (db *DB) Snapshots(ctx context.Context) ([]*SnapshotInfo, error) {
	var infos []*SnapshotInfo
	for _, r := range db.Replicas {
		a, err := r.Snapshots(ctx)
		if err != nil {
			return nil, err
		}
		infos = append(infos, a...)
	}

	// Sort in order by time.
	sort.Slice(infos, func(i, j int) bool {
		return infos[i].CreatedAt.Before(infos[j].CreatedAt)
	})
	return infos, nil
}

// WALs returns a list of all WAL files across all replicas.
func (db *DB) WALs(ctx context.Context) ([]*WALInfo, error) {
	var infos []*WALInfo
	for _, r := range db.Replicas {
		a, err := r.WALs(ctx)
		if err != nil {
			return nil, err
		}
		infos = append(infos, a...)
	}

	// Sort in order by time.
	sort.Slice(infos, func(i, j int) bool {
		return infos[i].CreatedAt.Before(infos[j].CreatedAt)
	})
	return infos, nil
}

// init initializes the connection to the database.
// Skipped if already initialized or if the database file does not exist.
func (db *DB) init() (err error) {
	// Exit if already initialized.
	if db.db != nil {
		return nil
	}

	// Exit if no database file exists.
	fi, err := os.Stat(db.path)
	if os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	db.uid, db.gid = fileinfo(fi)
	db.mode = fi.Mode()

	// Obtain permissions for parent directory.
	if fi, err = os.Stat(filepath.Dir(db.path)); err != nil {
		return err
	}
	db.diruid, db.dirgid = fileinfo(fi)
	db.dirmode = fi.Mode()

	dsn := db.path
	dsn += fmt.Sprintf("?_busy_timeout=%d", BusyTimeout.Milliseconds())

	// Connect to SQLite database & enable WAL.
	if db.db, err = sql.Open("sqlite3", dsn); err != nil {
		return err
	} else if _, err := db.db.Exec(`PRAGMA journal_mode = wal;`); err != nil {
		return fmt.Errorf("enable wal: %w", err)
	}

	// Disable autocheckpoint for litestream's connection.
	if _, err := db.db.ExecContext(db.ctx, `PRAGMA wal_autocheckpoint = 0;`); err != nil {
		return fmt.Errorf("disable autocheckpoint: %w", err)
	}

	// Create a table to force writes to the WAL when empty.
	// There should only ever be one row with id=1.
	if _, err := db.db.Exec(`CREATE TABLE IF NOT EXISTS _litestream_seq (id INTEGER PRIMARY KEY, seq INTEGER);`); err != nil {
		return fmt.Errorf("create _litestream_seq table: %w", err)
	}

	// Create a lock table to force write locks during sync.
	// The sync write transaction always rolls back so no data should be in this table.
	if _, err := db.db.Exec(`CREATE TABLE IF NOT EXISTS _litestream_lock (id INTEGER);`); err != nil {
		return fmt.Errorf("create _litestream_lock table: %w", err)
	}

	// Start a long-running read transaction to prevent other transactions
	// from checkpointing.
	if err := db.acquireReadLock(); err != nil {
		return fmt.Errorf("acquire read lock: %w", err)
	}

	// Read page size.
	if err := db.db.QueryRow(`PRAGMA page_size;`).Scan(&db.pageSize); err != nil {
		return fmt.Errorf("read page size: %w", err)
	} else if db.pageSize <= 0 {
		return fmt.Errorf("invalid db page size: %d", db.pageSize)
	}

	// Ensure meta directory structure exists.
	if err := mkdirAll(db.MetaPath(), db.dirmode, db.diruid, db.dirgid); err != nil {
		return err
	}

	// If we have an existing shadow WAL, ensure the headers match.
	if err := db.verifyHeadersMatch(); err != nil {
		log.Printf("%s: init: cannot determine last wal position, clearing generation (%s)", db.path, err)
		if err := os.Remove(db.GenerationNamePath()); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove generation name: %w", err)
		}
	}

	// Clean up previous generations.
	if err := db.clean(); err != nil {
		return fmt.Errorf("clean: %w", err)
	}

	// Start replication.
	for _, r := range db.Replicas {
		r.Start(db.ctx)
	}

	return nil
}

// verifyHeadersMatch returns true if the primary WAL and last shadow WAL header match.
func (db *DB) verifyHeadersMatch() error {
	// Determine current generation.
	generation, err := db.CurrentGeneration()
	if err != nil {
		return err
	} else if generation == "" {
		return nil
	}

	// Find current generation & latest shadow WAL.
	shadowWALPath, err := db.CurrentShadowWALPath(generation)
	if err != nil {
		return fmt.Errorf("cannot determine current shadow wal path: %w", err)
	}

	hdr0, err := readWALHeader(db.WALPath())
	if os.IsNotExist(err) {
		return fmt.Errorf("no primary wal: %w", err)
	} else if err != nil {
		return fmt.Errorf("primary wal header: %w", err)
	}

	hdr1, err := readWALHeader(shadowWALPath)
	if os.IsNotExist(err) {
		return fmt.Errorf("no shadow wal")
	} else if err != nil {
		return fmt.Errorf("shadow wal header: %w", err)
	}

	if !bytes.Equal(hdr0, hdr1) {
		return fmt.Errorf("wal header mismatch")
	}
	return nil
}

// clean removes old generations & WAL files.
func (db *DB) clean() error {
	if err := db.cleanGenerations(); err != nil {
		return err
	}
	return db.cleanWAL()
}

// cleanGenerations removes old generations.
func (db *DB) cleanGenerations() error {
	generation, err := db.CurrentGeneration()
	if err != nil {
		return err
	}

	dir := filepath.Join(db.MetaPath(), "generations")
	fis, err := ioutil.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	for _, fi := range fis {
		// Skip the current generation.
		if filepath.Base(fi.Name()) == generation {
			continue
		}

		// Delete all other generations.
		if err := os.RemoveAll(filepath.Join(dir, fi.Name())); err != nil {
			return err
		}
	}
	return nil
}

// cleanWAL removes WAL files that have been replicated.
func (db *DB) cleanWAL() error {
	generation, err := db.CurrentGeneration()
	if err != nil {
		return err
	}

	// Determine lowest index that's been replicated to all replicas.
	min := -1
	for _, r := range db.Replicas {
		pos := r.LastPos()
		if pos.Generation != generation {
			pos = Pos{} // different generation, reset index to zero
		}
		if min == -1 || pos.Index < min {
			min = pos.Index
		}
	}

	// Skip if our lowest index is too small.
	if min <= 0 {
		return nil
	}
	min-- // Keep an extra WAL file.

	// Remove all WAL files for the generation before the lowest index.
	dir := db.ShadowWALDir(generation)
	fis, err := ioutil.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	for _, fi := range fis {
		if idx, _, _, err := ParseWALPath(fi.Name()); err != nil || idx >= min {
			continue
		}
		if err := os.Remove(filepath.Join(dir, fi.Name())); err != nil {
			return err
		}
	}
	return nil
}

// SoftClose closes everything but the underlying db connection. This method
// is available because the binary needs to avoid closing the database on exit
// to prevent autocheckpointing.
func (db *DB) SoftClose() (err error) {
	db.cancel()
	db.wg.Wait()

	// Ensure replicas all stop replicating.
	for _, r := range db.Replicas {
		r.Stop()
	}

	if db.rtx != nil {
		if e := db.releaseReadLock(); e != nil && err == nil {
			err = e
		}
	}
	return err
}

// acquireReadLock begins a read transaction on the database to prevent checkpointing.
func (db *DB) acquireReadLock() error {
	if db.rtx != nil {
		return nil
	}

	// Start long running read-transaction to prevent checkpoints.
	tx, err := db.db.Begin()
	if err != nil {
		return err
	}

	// Execute read query to obtain read lock.
	if _, err := tx.ExecContext(db.ctx, `SELECT COUNT(1) FROM _litestream_seq;`); err != nil {
		_ = tx.Rollback()
		return err
	}

	// Track transaction so we can release it later before checkpoint.
	db.rtx = tx
	return nil
}

// releaseReadLock rolls back the long-running read transaction.
func (db *DB) releaseReadLock() error {
	// Ignore if we do not have a read lock.
	if db.rtx == nil {
		return nil
	}

	// Rollback & clear read transaction.
	err := db.rtx.Rollback()
	db.rtx = nil
	return err
}

// CurrentGeneration returns the name of the generation saved to the "generation"
// file in the meta data directory. Returns empty string if none exists.
func (db *DB) CurrentGeneration() (string, error) {
	buf, err := ioutil.ReadFile(db.GenerationNamePath())
	if os.IsNotExist(err) {
		return "", nil
	} else if err != nil {
		return "", err
	}

	// TODO: Verify if generation directory exists. If not, delete name file.

	generation := strings.TrimSpace(string(buf))
	if len(generation) != GenerationNameLen {
		return "", nil
	}
	return generation, nil
}

// createGeneration starts a new generation by creating the generation
// directory, snapshotting to each replica, and updating the current
// generation name.
func (db *DB) createGeneration() (string, error) {
	// Generate random generation hex name.
	buf := make([]byte, GenerationNameLen/2)
	_, _ = rand.New(rand.NewSource(time.Now().UnixNano())).Read(buf)
	generation := hex.EncodeToString(buf)

	// Generate new directory.
	dir := filepath.Join(db.MetaPath(), "generations", generation)
	if err := mkdirAll(dir, db.dirmode, db.diruid, db.dirgid); err != nil {
		return "", err
	}

	// Initialize shadow WAL with copy of header.
	if _, err := db.initShadowWALFile(db.ShadowWALPath(generation, 0)); err != nil {
		return "", fmt.Errorf("initialize shadow wal: %w", err)
	}

	// Atomically write generation name as current generation.
	generationNamePath := db.GenerationNamePath()
	if err := ioutil.WriteFile(generationNamePath+".tmp", []byte(generation+"\n"), db.mode); err != nil {
		return "", fmt.Errorf("write generation temp file: %w", err)
	}
	_ = os.Chown(generationNamePath+".tmp", db.uid, db.gid)
	if err := os.Rename(generationNamePath+".tmp", generationNamePath); err != nil {
		return "", fmt.Errorf("rename generation file: %w", err)
	}

	// Remove old generations.
	if err := db.clean(); err != nil {
		return "", err
	}

	return generation, nil
}

// Sync copies pending data from the WAL to the shadow WAL.
func (db *DB) Sync() (err error) {
	db.mu.Lock()
	defer db.mu.Unlock()

	// Initialize database, if necessary. Exit if no DB exists.
	if err := db.init(); err != nil {
		return err
	} else if db.db == nil {
		Tracef("%s: sync: no database found", db.path)
		return nil
	}

	// Track total sync metrics.
	t := time.Now()
	defer func() {
		db.syncNCounter.Inc()
		if err != nil {
			db.syncErrorNCounter.Inc()
		}
		db.syncSecondsCounter.Add(float64(time.Since(t).Seconds()))
	}()

	// Ensure WAL has at least one frame in it.
	if err := db.ensureWALExists(); err != nil {
		return fmt.Errorf("ensure wal exists: %w", err)
	}

	// Start a transaction. This will be promoted immediately after.
	tx, err := db.db.Begin()
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}

	// Ensure write transaction rolls back before returning.
	defer func() {
		if e := rollback(tx); e != nil && err == nil {
			err = e
		}
	}()

	// Insert into the lock table to promote to a write tx. The lock table
	// insert will never actually occur because our tx will be rolled back,
	// however, it will ensure our tx grabs the write lock. Unfortunately,
	// we can't call "BEGIN IMMEDIATE" as we are already in a transaction.
	if _, err := tx.ExecContext(db.ctx, `INSERT INTO _litestream_lock (id) VALUES (1);`); err != nil {
		return fmt.Errorf("_litestream_lock: %w", err)
	}

	// Verify our last sync matches the current state of the WAL.
	// This ensures that we have an existing generation & that the last sync
	// position of the real WAL hasn't been overwritten by another process.
	info, err := db.verify()
	if err != nil {
		return fmt.Errorf("cannot verify wal state: %w", err)
	}
	Tracef("%s: sync: info=%#v", db.path, info)

	// Track if anything in the shadow WAL changes and then notify at the end.
	changed := info.walSize != info.shadowWALSize || info.restart || info.reason != ""

	// If we are unable to verify the WAL state then we start a new generation.
	if info.reason != "" {
		// Start new generation & notify user via log message.
		if info.generation, err = db.createGeneration(); err != nil {
			return fmt.Errorf("create generation: %w", err)
		}
		log.Printf("%s: sync: new generation %q, %s", db.path, info.generation, info.reason)

		// Clear shadow wal info.
		info.shadowWALPath = db.ShadowWALPath(info.generation, 0)
		info.shadowWALSize = WALHeaderSize
		info.restart = false
		info.reason = ""

	}

	// Synchronize real WAL with current shadow WAL.
	newWALSize, err := db.syncWAL(info)
	if err != nil {
		return fmt.Errorf("sync wal: %w", err)
	}

	// If WAL size is great than max threshold, force checkpoint.
	// If WAL size is greater than min threshold, attempt checkpoint.
	var checkpoint bool
	checkpointMode := CheckpointModePassive
	if db.MaxCheckpointPageN > 0 && newWALSize >= calcWALSize(db.pageSize, db.MaxCheckpointPageN) {
		checkpoint, checkpointMode = true, CheckpointModeRestart
	} else if newWALSize >= calcWALSize(db.pageSize, db.MinCheckpointPageN) {
		checkpoint = true
	} else if db.CheckpointInterval > 0 && !info.dbModTime.IsZero() && time.Since(info.dbModTime) > db.CheckpointInterval && newWALSize > calcWALSize(db.pageSize, 1) {
		checkpoint = true
	}

	// Release write lock before checkpointing & exiting.
	if err := tx.Rollback(); err != nil {
		return fmt.Errorf("rollback write tx: %w", err)
	}

	// Issue the checkpoint.
	if checkpoint {
		changed = true

		if err := db.checkpointAndInit(info.generation, checkpointMode); err != nil {
			return fmt.Errorf("checkpoint: mode=%v err=%w", checkpointMode, err)
		}
	}

	// Clean up any old files.
	if err := db.clean(); err != nil {
		return fmt.Errorf("cannot clean: %w", err)
	}

	// Compute current index and total shadow WAL size.
	// This is only for metrics so we ignore any errors that occur.
	index, size, _ := db.CurrentShadowWALIndex(info.generation)
	db.shadowWALIndexGauge.Set(float64(index))
	db.shadowWALSizeGauge.Set(float64(size))

	// Notify replicas of WAL changes.
	if changed {
		close(db.notify)
		db.notify = make(chan struct{})
	}

	Tracef("%s: sync: ok", db.path)

	return nil
}

// ensureWALExists checks that the real WAL exists and has a header.
func (db *DB) ensureWALExists() (err error) {
	// Exit early if WAL header exists.
	if fi, err := os.Stat(db.WALPath()); err == nil && fi.Size() >= WALHeaderSize {
		return nil
	}

	// Otherwise create transaction that updates the internal litestream table.
	_, err = db.db.Exec(`INSERT INTO _litestream_seq (id, seq) VALUES (1, 1) ON CONFLICT (id) DO UPDATE SET seq = seq + 1`)
	return err
}

// verify ensures the current shadow WAL state matches where it left off from
// the real WAL. Returns generation & WAL sync information. If info.reason is
// not blank, verification failed and a new generation should be started.
func (db *DB) verify() (info syncInfo, err error) {
	// Look up existing generation.
	generation, err := db.CurrentGeneration()
	if err != nil {
		return info, fmt.Errorf("cannot find current generation: %w", err)
	} else if generation == "" {
		info.reason = "no generation exists"
		return info, nil
	}
	info.generation = generation

	// Determine total bytes of real DB for metrics.
	fi, err := os.Stat(db.Path())
	if err != nil {
		return info, err
	}
	info.dbModTime = fi.ModTime()
	db.dbSizeGauge.Set(float64(fi.Size()))

	// Determine total bytes of real WAL.
	fi, err = os.Stat(db.WALPath())
	if err != nil {
		return info, err
	}
	info.walSize = fi.Size()
	info.walModTime = fi.ModTime()
	db.walSizeGauge.Set(float64(fi.Size()))

	// Open shadow WAL to copy append to.
	index, _, err := db.CurrentShadowWALIndex(info.generation)
	if err != nil {
		return info, fmt.Errorf("cannot determine shadow WAL index: %w", err)
	} else if index >= MaxIndex {
		info.reason = "max index exceeded"
		return info, nil
	}
	info.shadowWALPath = db.ShadowWALPath(generation, index)

	// Determine shadow WAL current size.
	fi, err = os.Stat(info.shadowWALPath)
	if os.IsNotExist(err) {
		info.reason = "no shadow wal"
		return info, nil
	} else if err != nil {
		return info, err
	}
	info.shadowWALSize = frameAlign(fi.Size(), db.pageSize)

	// Truncate shadow WAL if there is a partial page.
	// Exit if shadow WAL does not contain a full header.
	if info.shadowWALSize < WALHeaderSize {
		info.reason = "short shadow wal"
		return info, nil
	}

	// If shadow WAL is larger than real WAL then the WAL has been truncated
	// so we cannot determine our last state.
	if info.shadowWALSize > info.walSize {
		info.reason = "wal truncated by another process"
		return info, nil
	}

	// Compare WAL headers. Start a new shadow WAL if they are mismatched.
	if hdr0, err := readWALHeader(db.WALPath()); err != nil {
		return info, fmt.Errorf("cannot read wal header: %w", err)
	} else if hdr1, err := readWALHeader(info.shadowWALPath); err != nil {
		return info, fmt.Errorf("cannot read shadow wal header: %w", err)
	} else if !bytes.Equal(hdr0, hdr1) {
		info.restart = !bytes.Equal(hdr0, hdr1)
	}

	// If we only have a header then ensure header matches.
	// Otherwise we need to start a new generation.
	if info.shadowWALSize == WALHeaderSize && info.restart {
		info.reason = "wal header only, mismatched"
		return info, nil
	}

	// Verify last page synced still matches.
	if info.shadowWALSize > WALHeaderSize {
		offset := info.shadowWALSize - int64(db.pageSize+WALFrameHeaderSize)
		if buf0, err := readFileAt(db.WALPath(), offset, int64(db.pageSize+WALFrameHeaderSize)); err != nil {
			return info, fmt.Errorf("cannot read last synced wal page: %w", err)
		} else if buf1, err := readFileAt(info.shadowWALPath, offset, int64(db.pageSize+WALFrameHeaderSize)); err != nil {
			return info, fmt.Errorf("cannot read last synced shadow wal page: %w", err)
		} else if !bytes.Equal(buf0, buf1) {
			info.reason = "wal overwritten by another process"
			return info, nil
		}
	}

	return info, nil
}

type syncInfo struct {
	generation    string    // generation name
	dbModTime     time.Time // last modified date of real DB file
	walSize       int64     // size of real WAL file
	walModTime    time.Time // last modified date of real WAL file
	shadowWALPath string    // name of last shadow WAL file
	shadowWALSize int64     // size of last shadow WAL file
	restart       bool      // if true, real WAL header does not match shadow WAL
	reason        string    // if non-blank, reason for sync failure
}

// syncWAL copies pending bytes from the real WAL to the shadow WAL.
func (db *DB) syncWAL(info syncInfo) (newSize int64, err error) {
	// Copy WAL starting from end of shadow WAL. Exit if no new shadow WAL needed.
	newSize, err = db.copyToShadowWAL(info.shadowWALPath)
	if err != nil {
		return newSize, fmt.Errorf("cannot copy to shadow wal: %w", err)
	} else if !info.restart {
		return newSize, nil // If no restart required, exit.
	}

	// Parse index of current shadow WAL file.
	dir, base := filepath.Split(info.shadowWALPath)
	index, _, _, err := ParseWALPath(base)
	if err != nil {
		return 0, fmt.Errorf("cannot parse shadow wal filename: %s", base)
	}

	// Start a new shadow WAL file with next index.
	newShadowWALPath := filepath.Join(dir, FormatWALPath(index+1))
	newSize, err = db.initShadowWALFile(newShadowWALPath)
	if err != nil {
		return 0, fmt.Errorf("cannot init shadow wal file: name=%s err=%w", newShadowWALPath, err)
	}
	return newSize, nil
}

func (db *DB) initShadowWALFile(filename string) (int64, error) {
	hdr, err := readWALHeader(db.WALPath())
	if err != nil {
		return 0, fmt.Errorf("read header: %w", err)
	}

	// Determine byte order for checksumming from header magic.
	bo, err := headerByteOrder(hdr)
	if err != nil {
		return 0, err
	}

	// Verify checksum.
	s0 := binary.BigEndian.Uint32(hdr[24:])
	s1 := binary.BigEndian.Uint32(hdr[28:])
	if v0, v1 := Checksum(bo, 0, 0, hdr[:24]); v0 != s0 || v1 != s1 {
		return 0, fmt.Errorf("invalid header checksum: (%x,%x) != (%x,%x)", v0, v1, s0, s1)
	}

	// Write header to new WAL shadow file.
	if err := mkdirAll(filepath.Dir(filename), db.dirmode, db.diruid, db.dirgid); err != nil {
		return 0, err
	} else if err := ioutil.WriteFile(filename, hdr, db.mode); err != nil {
		return 0, err
	}
	_ = os.Chown(filename, db.uid, db.gid)

	// Copy as much shadow WAL as available.
	newSize, err := db.copyToShadowWAL(filename)
	if err != nil {
		return 0, fmt.Errorf("cannot copy to new shadow wal: %w", err)
	}
	return newSize, nil
}

func (db *DB) copyToShadowWAL(filename string) (newSize int64, err error) {
	Tracef("%s: copy-shadow: %s", db.path, filename)

	r, err := os.Open(db.WALPath())
	if err != nil {
		return 0, err
	}
	defer r.Close()

	w, err := os.OpenFile(filename, os.O_RDWR, 0666)
	if err != nil {
		return 0, err
	}
	defer w.Close()

	fi, err := w.Stat()
	if err != nil {
		return 0, err
	}
	origSize := frameAlign(fi.Size(), db.pageSize)

	// Read shadow WAL header to determine byte order for checksum & salt.
	hdr := make([]byte, WALHeaderSize)
	if _, err := io.ReadFull(w, hdr); err != nil {
		return 0, fmt.Errorf("read header: %w", err)
	}
	hsalt0 := binary.BigEndian.Uint32(hdr[16:])
	hsalt1 := binary.BigEndian.Uint32(hdr[20:])

	bo, err := headerByteOrder(hdr)
	if err != nil {
		return 0, err
	}

	// Read previous checksum.
	chksum0, chksum1, err := readLastChecksumFrom(w, db.pageSize)
	if err != nil {
		return 0, fmt.Errorf("last checksum: %w", err)
	}

	// Seek to correct position on both files.
	if _, err := r.Seek(origSize, io.SeekStart); err != nil {
		return 0, fmt.Errorf("wal seek: %w", err)
	}

	// Read through WAL from last position to find the page of the last
	// committed transaction.
	tmpSz := origSize
	lastCommitSize := origSize
	buf := make([]byte, db.pageSize+WALFrameHeaderSize)
	for {
		Tracef("%s: copy-shadow: %s @ %d", db.path, filename, tmpSz)

		// Read next page from WAL file.
		if _, err := io.ReadFull(r, buf); err == io.EOF || err == io.ErrUnexpectedEOF {
			Tracef("%s: copy-shadow: break %s", db.path, err)
			break // end of file or partial page
		} else if err != nil {
			return 0, fmt.Errorf("read wal: %w", err)
		}

		// Read frame salt & compare to header salt. Stop reading on mismatch.
		salt0 := binary.BigEndian.Uint32(buf[8:])
		salt1 := binary.BigEndian.Uint32(buf[12:])
		if salt0 != hsalt0 || salt1 != hsalt1 {
			Tracef("%s: copy-shadow: break: salt mismatch", db.path)
			break
		}

		// Verify checksum of page is valid.
		fchksum0 := binary.BigEndian.Uint32(buf[16:])
		fchksum1 := binary.BigEndian.Uint32(buf[20:])
		chksum0, chksum1 = Checksum(bo, chksum0, chksum1, buf[:8])  // frame header
		chksum0, chksum1 = Checksum(bo, chksum0, chksum1, buf[24:]) // frame data
		if chksum0 != fchksum0 || chksum1 != fchksum1 {
			log.Printf("copy shadow: checksum mismatch, skipping: offset=%d (%x,%x) != (%x,%x)", tmpSz, chksum0, chksum1, fchksum0, fchksum1)
			break
		}

		// Add page to the new size of the shadow WAL.
		tmpSz += int64(len(buf))

		// Mark commit record.
		newDBSize := binary.BigEndian.Uint32(buf[4:])
		if newDBSize != 0 {
			lastCommitSize = tmpSz
		}
	}

	// Seek to correct position on both files.
	if _, err := r.Seek(origSize, io.SeekStart); err != nil {
		return 0, fmt.Errorf("wal seek: %w", err)
	} else if _, err := w.Seek(origSize, io.SeekStart); err != nil {
		return 0, fmt.Errorf("shadow wal seek: %w", err)
	}

	// Copy bytes, sync & close.
	if _, err := io.CopyN(w, r, lastCommitSize-origSize); err != nil {
		return 0, err
	} else if err := w.Sync(); err != nil {
		return 0, err
	} else if err := w.Close(); err != nil {
		return 0, err
	}

	// Track total number of bytes written to WAL.
	db.totalWALBytesCounter.Add(float64(lastCommitSize - origSize))

	return lastCommitSize, nil
}

// ShadowWALReader opens a reader for a shadow WAL file at a given position.
// If the reader is at the end of the file, it attempts to return the next file.
//
// The caller should check Pos() & Size() on the returned reader to check offset.
func (db *DB) ShadowWALReader(pos Pos) (r *ShadowWALReader, err error) {
	// Fetch reader for the requested position. Return if it has data.
	r, err = db.shadowWALReader(pos)
	if err != nil {
		return nil, err
	} else if r.N() > 0 {
		return r, nil
	}

	// Otherwise attempt to read the start of the next WAL file.
	pos.Index, pos.Offset = pos.Index+1, 0

	r, err = db.shadowWALReader(pos)
	if os.IsNotExist(err) {
		return nil, io.EOF
	}
	return r, err
}

// shadowWALReader opens a file reader for a shadow WAL file at a given position.
func (db *DB) shadowWALReader(pos Pos) (r *ShadowWALReader, err error) {
	filename := db.ShadowWALPath(pos.Generation, pos.Index)

	f, err := os.Open(filename)
	if err != nil {
		return nil, err
	}

	// Ensure file is closed if any error occurs.
	defer func() {
		if err != nil {
			r.Close()
		}
	}()

	// Fetch frame-aligned file size and ensure requested offset is not past EOF.
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}

	fileSize := frameAlign(fi.Size(), db.pageSize)
	if pos.Offset > fileSize {
		return nil, fmt.Errorf("wal reader offset too high: %d > %d", pos.Offset, fi.Size())
	}

	// Move file handle to offset position.
	if _, err := f.Seek(pos.Offset, io.SeekStart); err != nil {
		return nil, err
	}

	return &ShadowWALReader{
		f:   f,
		n:   fileSize - pos.Offset,
		pos: pos,
	}, nil
}

// frameAlign returns a frame-aligned offset.
// Returns zero if offset is less than the WAL header size.
func frameAlign(offset int64, pageSize int) int64 {
	assert(offset >= 0, "frameAlign(): offset must be non-negative")
	assert(pageSize >= 0, "frameAlign(): page size must be non-negative")

	if offset < WALHeaderSize {
		return 0
	}

	frameSize := WALFrameHeaderSize + int64(pageSize)
	frameN := (offset - WALHeaderSize) / frameSize
	return (frameN * frameSize) + WALHeaderSize
}

// ShadowWALReader represents a reader for a shadow WAL file that tracks WAL position.
type ShadowWALReader struct {
	f   *os.File
	n   int64
	pos Pos
}

// Close closes the underlying WAL file handle.
func (r *ShadowWALReader) Close() error { return r.f.Close() }

// N returns the remaining bytes in the reader.
func (r *ShadowWALReader) N() int64 { return r.n }

// Pos returns the current WAL position.
func (r *ShadowWALReader) Pos() Pos { return r.pos }

// Read reads bytes into p, updates the position, and returns the bytes read.
// Returns io.EOF at the end of the available section of the WAL.
func (r *ShadowWALReader) Read(p []byte) (n int, err error) {
	if r.n <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > r.n {
		p = p[0:r.n]
	}
	n, err = r.f.Read(p)
	r.n -= int64(n)
	r.pos.Offset += int64(n)
	return n, err
}

// SQLite WAL constants
const (
	WALHeaderChecksumOffset      = 24
	WALFrameHeaderChecksumOffset = 16
)

func readLastChecksumFrom(f *os.File, pageSize int) (uint32, uint32, error) {
	// Determine the byte offset of the checksum for the header (if no pages
	// exist) or for the last page (if at least one page exists).
	offset := int64(WALHeaderChecksumOffset)
	if fi, err := f.Stat(); err != nil {
		return 0, 0, err
	} else if sz := frameAlign(fi.Size(), pageSize); fi.Size() > WALHeaderSize {
		offset = sz - int64(pageSize) - WALFrameHeaderSize + WALFrameHeaderChecksumOffset
	}

	// Read big endian checksum.
	b := make([]byte, 8)
	if n, err := f.ReadAt(b, offset); err != nil {
		return 0, 0, err
	} else if n != len(b) {
		return 0, 0, io.ErrUnexpectedEOF
	}
	return binary.BigEndian.Uint32(b[0:]), binary.BigEndian.Uint32(b[4:]), nil
}

// Checkpoint performs a checkpoint on the WAL file.
func (db *DB) Checkpoint(mode string) (err error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	return db.checkpoint(mode)
}

func (db *DB) checkpoint(mode string) (err error) {
	// Ignore if there is no underlying database.
	if db.db == nil {
		return nil
	}

	// Track checkpoint metrics.
	t := time.Now()
	defer func() {
		labels := prometheus.Labels{"mode": mode}
		db.checkpointNCounterVec.With(labels).Inc()
		if err != nil {
			db.checkpointErrorNCounterVec.With(labels).Inc()
		}
		db.checkpointSecondsCounterVec.With(labels).Add(float64(time.Since(t).Seconds()))
	}()

	// Ensure the read lock has been removed before issuing a checkpoint.
	// We defer the re-acquire to ensure it occurs even on an early return.
	if err := db.releaseReadLock(); err != nil {
		return fmt.Errorf("release read lock: %w", err)
	}
	defer func() { _ = db.acquireReadLock() }()

	// A non-forced checkpoint is issued as "PASSIVE". This will only checkpoint
	// if there are not pending transactions. A forced checkpoint ("RESTART")
	// will wait for pending transactions to end & block new transactions before
	// forcing the checkpoint and restarting the WAL.
	//
	// See: https://www.sqlite.org/pragma.html#pragma_wal_checkpoint
	rawsql := `PRAGMA wal_checkpoint(` + mode + `);`

	var row [3]int
	if err := db.db.QueryRow(rawsql).Scan(&row[0], &row[1], &row[2]); err != nil {
		return err
	}
	Tracef("%s: checkpoint: mode=%v (%d,%d,%d)", db.path, mode, row[0], row[1], row[2])

	// Reacquire the read lock immediately after the checkpoint.
	if err := db.acquireReadLock(); err != nil {
		return fmt.Errorf("release read lock: %w", err)
	}

	return nil
}

// checkpointAndInit performs a checkpoint on the WAL file and initializes a
// new shadow WAL file.
func (db *DB) checkpointAndInit(generation, mode string) error {
	shadowWALPath, err := db.CurrentShadowWALPath(generation)
	if err != nil {
		return err
	}

	// Read WAL header before checkpoint to check if it has been restarted.
	hdr, err := readWALHeader(db.WALPath())
	if err != nil {
		return err
	}

	// Execute checkpoint and immediately issue a write to the WAL to ensure
	// a new page is written.
	if err := db.checkpoint(mode); err != nil {
		return err
	} else if _, err = db.db.Exec(`INSERT INTO _litestream_seq (id, seq) VALUES (1, 1) ON CONFLICT (id) DO UPDATE SET seq = seq + 1`); err != nil {
		return err
	}

	// If WAL hasn't been restarted, exit.
	if other, err := readWALHeader(db.WALPath()); err != nil {
		return err
	} else if bytes.Equal(hdr, other) {
		return nil
	}

	// Copy the end of the previous WAL before starting a new shadow WAL.
	if _, err := db.copyToShadowWAL(shadowWALPath); err != nil {
		return fmt.Errorf("cannot copy to end of shadow wal: %w", err)
	}

	// Parse index of current shadow WAL file.
	index, _, _, err := ParseWALPath(shadowWALPath)
	if err != nil {
		return fmt.Errorf("cannot parse shadow wal filename: %s", shadowWALPath)
	}

	// Start a new shadow WAL file with next index.
	newShadowWALPath := filepath.Join(filepath.Dir(shadowWALPath), FormatWALPath(index+1))
	if _, err := db.initShadowWALFile(newShadowWALPath); err != nil {
		return fmt.Errorf("cannot init shadow wal file: name=%s err=%w", newShadowWALPath, err)
	}

	return nil
}

// monitor runs in a separate goroutine and monitors the database & WAL.
func (db *DB) monitor() {
	ticker := time.NewTicker(db.MonitorInterval)
	defer ticker.Stop()

	for {
		// Wait for ticker or context close.
		select {
		case <-db.ctx.Done():
			return
		case <-ticker.C:
		}

		// Sync the database to the shadow WAL.
		if err := db.Sync(); err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("%s: sync error: %s", db.path, err)
		}
	}
}

// RestoreReplica restores the database from a replica based on the options given.
// This method will restore into opt.OutputPath, if specified, or into the
// DB's original database path. It can optionally restore from a specific
// replica or generation or it will automatically choose the best one. Finally,
// a timestamp can be specified to restore the database to a specific
// point-in-time.
func RestoreReplica(ctx context.Context, r Replica, opt RestoreOptions) error {
	// Validate options.
	if opt.OutputPath == "" {
		return fmt.Errorf("output path required")
	} else if opt.Generation == "" && opt.Index != math.MaxInt64 {
		return fmt.Errorf("must specify generation when restoring to index")
	} else if opt.Index != math.MaxInt64 && !opt.Timestamp.IsZero() {
		return fmt.Errorf("cannot specify index & timestamp to restore")
	}

	// Ensure logger exists.
	logger := opt.Logger
	if logger == nil {
		logger = log.New(ioutil.Discard, "", 0)
	}

	logPrefix := r.Name()
	if db := r.DB(); db != nil {
		logPrefix = fmt.Sprintf("%s(%s)", db.Path(), r.Name())
	}

	// Ensure output path does not already exist (unless this is a dry run).
	if !opt.DryRun {
		if _, err := os.Stat(opt.OutputPath); err == nil {
			return fmt.Errorf("cannot restore, output path already exists: %s", opt.OutputPath)
		} else if err != nil && !os.IsNotExist(err) {
			return err
		}
	}

	// Find lastest snapshot that occurs before timestamp.
	minWALIndex, err := SnapshotIndexAt(ctx, r, opt.Generation, opt.Timestamp)
	if err != nil {
		return fmt.Errorf("cannot find snapshot index for restore: %w", err)
	}

	// Find the maximum WAL index that occurs before timestamp.
	maxWALIndex, err := WALIndexAt(ctx, r, opt.Generation, opt.Index, opt.Timestamp)
	if err != nil {
		return fmt.Errorf("cannot find max wal index for restore: %w", err)
	}
	logger.Printf("%s: starting restore: generation %s, index %08x-%08x", logPrefix, opt.Generation, minWALIndex, maxWALIndex)

	// Initialize starting position.
	pos := Pos{Generation: opt.Generation, Index: minWALIndex}
	tmpPath := opt.OutputPath + ".tmp"

	// Copy snapshot to output path.
	logger.Printf("%s: restoring snapshot %s/%08x to %s", logPrefix, opt.Generation, minWALIndex, tmpPath)
	if !opt.DryRun {
		if err := restoreSnapshot(ctx, r, pos.Generation, pos.Index, tmpPath); err != nil {
			return fmt.Errorf("cannot restore snapshot: %w", err)
		}
	}

	// Restore each WAL file until we reach our maximum index.
	for index := minWALIndex; index <= maxWALIndex; index++ {
		if !opt.DryRun {
			if err = restoreWAL(ctx, r, opt.Generation, index, tmpPath); os.IsNotExist(err) && index == minWALIndex && index == maxWALIndex {
				logger.Printf("%s: no wal available, snapshot only", logPrefix)
				break // snapshot file only, ignore error
			} else if err != nil {
				return fmt.Errorf("cannot restore wal: %w", err)
			}
		}

		if opt.Verbose {
			logger.Printf("%s: restored wal %s/%08x", logPrefix, opt.Generation, index)
		}
	}

	// Copy file to final location.
	logger.Printf("%s: renaming database from temporary location", logPrefix)
	if !opt.DryRun {
		if err := os.Rename(tmpPath, opt.OutputPath); err != nil {
			return err
		}
	}

	return nil
}

func checksumFile(filename string) (uint64, error) {
	f, err := os.Open(filename)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	h := crc64.New(crc64.MakeTable(crc64.ISO))
	if _, err := io.Copy(h, f); err != nil {
		return 0, err
	}
	return h.Sum64(), nil
}

// CalcRestoreTarget returns a replica & generation to restore from based on opt criteria.
func (db *DB) CalcRestoreTarget(ctx context.Context, opt RestoreOptions) (Replica, string, error) {
	var target struct {
		replica    Replica
		generation string
		stats      GenerationStats
	}

	for _, r := range db.Replicas {
		// Skip replica if it does not match filter.
		if opt.ReplicaName != "" && r.Name() != opt.ReplicaName {
			continue
		}

		generation, stats, err := CalcReplicaRestoreTarget(ctx, r, opt)
		if err != nil {
			return nil, "", err
		}

		// Use the latest replica if we have multiple candidates.
		if !stats.UpdatedAt.After(target.stats.UpdatedAt) {
			continue
		}

		target.replica, target.generation, target.stats = r, generation, stats
	}
	return target.replica, target.generation, nil
}

// CalcReplicaRestoreTarget returns a generation to restore from.
func CalcReplicaRestoreTarget(ctx context.Context, r Replica, opt RestoreOptions) (generation string, stats GenerationStats, err error) {
	var target struct {
		generation string
		stats      GenerationStats
	}

	generations, err := r.Generations(ctx)
	if err != nil {
		return "", stats, fmt.Errorf("cannot fetch generations: %w", err)
	}

	// Search generations for one that contains the requested timestamp.
	for _, generation := range generations {
		// Skip generation if it does not match filter.
		if opt.Generation != "" && generation != opt.Generation {
			continue
		}

		// Fetch stats for generation.
		stats, err := r.GenerationStats(ctx, generation)
		if err != nil {
			return "", stats, fmt.Errorf("cannot determine stats for generation (%s/%s): %s", r.Name(), generation, err)
		}

		// Skip if it does not contain timestamp.
		if !opt.Timestamp.IsZero() {
			if opt.Timestamp.Before(stats.CreatedAt) || opt.Timestamp.After(stats.UpdatedAt) {
				continue
			}
		}

		// Use the latest replica if we have multiple candidates.
		if !stats.UpdatedAt.After(target.stats.UpdatedAt) {
			continue
		}

		target.generation = generation
		target.stats = stats
	}

	return target.generation, target.stats, nil
}

// restoreSnapshot copies a snapshot from the replica to a file.
func restoreSnapshot(ctx context.Context, r Replica, generation string, index int, filename string) error {
	// Determine the user/group & mode based on the DB, if available.
	uid, gid, mode := -1, -1, os.FileMode(0600)
	diruid, dirgid, dirmode := -1, -1, os.FileMode(0700)
	if db := r.DB(); db != nil {
		uid, gid, mode = db.uid, db.gid, db.mode
		diruid, dirgid, dirmode = db.diruid, db.dirgid, db.dirmode
	}

	if err := mkdirAll(filepath.Dir(filename), dirmode, diruid, dirgid); err != nil {
		return err
	}

	f, err := createFile(filename, mode, uid, gid)
	if err != nil {
		return err
	}
	defer f.Close()

	rd, err := r.SnapshotReader(ctx, generation, index)
	if err != nil {
		return err
	}
	defer rd.Close()

	if _, err := io.Copy(f, rd); err != nil {
		return err
	}

	if err := f.Sync(); err != nil {
		return err
	}
	return f.Close()
}

// restoreWAL copies a WAL file from the replica to the local WAL and forces checkpoint.
func restoreWAL(ctx context.Context, r Replica, generation string, index int, dbPath string) error {
	// Determine the user/group & mode based on the DB, if available.
	uid, gid, mode := -1, -1, os.FileMode(0600)
	if db := r.DB(); db != nil {
		uid, gid, mode = db.uid, db.gid, db.mode
	}

	// Open WAL file from replica.
	rd, err := r.WALReader(ctx, generation, index)
	if err != nil {
		return err
	}
	defer rd.Close()

	// Open handle to destination WAL path.
	f, err := createFile(dbPath+"-wal", mode, uid, gid)
	if err != nil {
		return err
	}
	defer f.Close()

	// Copy WAL to target path.
	if _, err := io.Copy(f, rd); err != nil {
		return err
	} else if err := f.Close(); err != nil {
		return err
	}

	// Open SQLite database and force a truncating checkpoint.
	d, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return err
	}
	defer d.Close()

	var row [3]int
	if err := d.QueryRow(`PRAGMA wal_checkpoint(TRUNCATE);`).Scan(&row[0], &row[1], &row[2]); err != nil {
		return err
	} else if row[0] != 0 {
		return fmt.Errorf("truncation checkpoint failed during restore (%d,%d,%d)", row[0], row[1], row[2])
	}

	return d.Close()
}

// CRC64 returns a CRC-64 ISO checksum of the database and its current position.
//
// This function obtains a read lock so it prevents syncs from occurring until
// the operation is complete. The database will still be usable but it will be
// unable to checkpoint during this time.
//
// If dst is set, the database file is copied to that location before checksum.
func (db *DB) CRC64(dst string) (uint64, Pos, error) {
	db.mu.Lock()
	defer db.mu.Unlock()

	if err := db.init(); err != nil {
		return 0, Pos{}, err
	} else if db.db == nil {
		return 0, Pos{}, os.ErrNotExist
	}

	generation, err := db.CurrentGeneration()
	if err != nil {
		return 0, Pos{}, fmt.Errorf("cannot find current generation: %w", err)
	} else if generation == "" {
		return 0, Pos{}, fmt.Errorf("no current generation")
	}

	// Force a RESTART checkpoint to ensure the database is at the start of the WAL.
	if err := db.checkpointAndInit(generation, CheckpointModeRestart); err != nil {
		return 0, Pos{}, err
	}

	// Obtain current position. Clear the offset since we are only reading the
	// DB and not applying the current WAL.
	pos, err := db.Pos()
	if err != nil {
		return 0, pos, err
	}
	pos.Offset = 0

	// Copy file, if dst specified.
	checksumPath := db.Path()
	if dst != "" {
		if err := copyFile(dst, db.Path()); err != nil {
			return 0, Pos{}, err
		}
		checksumPath = dst
	}

	chksum, err := checksumFile(checksumPath)
	if err != nil {
		return 0, pos, err
	}
	return chksum, pos, nil
}

// RestoreOptions represents options for DB.Restore().
type RestoreOptions struct {
	// Target path to restore into.
	// If blank, the original DB path is used.
	OutputPath string

	// Specific replica to restore from.
	// If blank, all replicas are considered.
	ReplicaName string

	// Specific generation to restore from.
	// If blank, all generations considered.
	Generation string

	// Specific index to restore from.
	// Set to math.MaxInt64 to ignore index.
	Index int

	// Point-in-time to restore database.
	// If zero, database restore to most recent state available.
	Timestamp time.Time

	// If true, no actual restore is performed.
	// Only equivalent log output for a regular restore.
	DryRun bool

	// Logging settings.
	Logger  *log.Logger
	Verbose bool
}

// NewRestoreOptions returns a new instance of RestoreOptions with defaults.
func NewRestoreOptions() RestoreOptions {
	return RestoreOptions{
		Index: math.MaxInt64,
	}
}

// Database metrics.
var (
	dbSizeGaugeVec = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "litestream",
		Subsystem: "db",
		Name:      "db_size",
		Help:      "The current size of the real DB",
	}, []string{"db"})

	walSizeGaugeVec = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "litestream",
		Subsystem: "db",
		Name:      "wal_size",
		Help:      "The current size of the real WAL",
	}, []string{"db"})

	totalWALBytesCounterVec = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "litestream",
		Subsystem: "db",
		Name:      "total_wal_bytes",
		Help:      "Total number of bytes written to shadow WAL",
	}, []string{"db"})

	shadowWALIndexGaugeVec = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "litestream",
		Subsystem: "db",
		Name:      "shadow_wal_index",
		Help:      "The current index of the shadow WAL",
	}, []string{"db"})

	shadowWALSizeGaugeVec = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "litestream",
		Subsystem: "db",
		Name:      "shadow_wal_size",
		Help:      "Current size of shadow WAL, in bytes",
	}, []string{"db"})

	syncNCounterVec = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "litestream",
		Subsystem: "db",
		Name:      "sync_count",
		Help:      "Number of sync operations performed",
	}, []string{"db"})

	syncErrorNCounterVec = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "litestream",
		Subsystem: "db",
		Name:      "sync_error_count",
		Help:      "Number of sync errors that have occurred",
	}, []string{"db"})

	syncSecondsCounterVec = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "litestream",
		Subsystem: "db",
		Name:      "sync_seconds",
		Help:      "Time spent syncing shadow WAL, in seconds",
	}, []string{"db"})

	checkpointNCounterVec = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "litestream",
		Subsystem: "db",
		Name:      "checkpoint_count",
		Help:      "Number of checkpoint operations performed",
	}, []string{"db", "mode"})

	checkpointErrorNCounterVec = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "litestream",
		Subsystem: "db",
		Name:      "checkpoint_error_count",
		Help:      "Number of checkpoint errors that have occurred",
	}, []string{"db", "mode"})

	checkpointSecondsCounterVec = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "litestream",
		Subsystem: "db",
		Name:      "checkpoint_seconds",
		Help:      "Time spent checkpointing WAL, in seconds",
	}, []string{"db", "mode"})
)

func headerByteOrder(hdr []byte) (binary.ByteOrder, error) {
	magic := binary.BigEndian.Uint32(hdr[0:])
	switch magic {
	case 0x377f0682:
		return binary.LittleEndian, nil
	case 0x377f0683:
		return binary.BigEndian, nil
	default:
		return nil, fmt.Errorf("invalid wal header magic: %x", magic)
	}
}

func copyFile(dst, src string) error {
	r, err := os.Open(src)
	if err != nil {
		return err
	}
	defer r.Close()

	w, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer w.Close()

	if _, err := io.Copy(w, r); err != nil {
		return err
	} else if err := w.Sync(); err != nil {
		return err
	}
	return nil
}
