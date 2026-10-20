// pruner.go
//
// PRI-3457: llm-proxy's request logs under LogDir
// (<upstream>/<YYYY-MM-DD>/<session>.jsonl) grow forever with nothing to
// remove them. On one coworker this filled a 47 GB data disk and stopped the
// box for 6 days. Pruner deletes by age and enforces a total-size cap,
// deleting the oldest files first, on every box with no operator step: it
// runs once at startup and then on a timer from inside the same process that
// writes the logs.
package main

import (
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// PruneStats summarizes a single prune pass. Counts and byte totals only -
// never file paths or contents, which may carry session data.
type PruneStats struct {
	AgeFilesDeleted  int
	AgeBytesDeleted  int64
	SizeFilesDeleted int
	SizeBytesDeleted int64
	DirsRemoved      int
	Errors           int
}

// Pruner periodically deletes old or excess llm-proxy request logs so a
// coworker's data disk cannot fill up from unbounded log growth (PRI-3457).
// It never touches a file the Logger currently has open for append.
type Pruner struct {
	baseDir       string
	retentionDays int
	maxBytes      int64
	interval      time.Duration
	activeFiles   func() map[string]struct{}
	// removeFile deletes a single log file. Defaults to os.Remove; tests in
	// this package override it to inject latency or failures without
	// touching the real filesystem semantics.
	removeFile func(string) error

	mu      sync.Mutex
	started bool
	stopCh  chan struct{}
	doneCh  chan struct{}
}

// NewPruner builds a Pruner over baseDir (the same directory Logger writes
// into). retentionDays <= 0 disables age-based pruning; maxBytes <= 0
// disables the total-size cap. interval <= 0 defaults to one hour.
// activeFiles must return the set of log file paths currently open for
// append (see Logger.ActiveLogFiles) - PruneOnce consults it before every
// delete; a nil callback is treated as "nothing is active".
func NewPruner(baseDir string, retentionDays int, maxBytes int64, interval time.Duration, activeFiles func() map[string]struct{}) *Pruner {
	if interval <= 0 {
		interval = time.Hour
	}
	if activeFiles == nil {
		activeFiles = func() map[string]struct{} { return nil }
	}
	return &Pruner{
		baseDir:       baseDir,
		retentionDays: retentionDays,
		maxBytes:      maxBytes,
		interval:      interval,
		activeFiles:   activeFiles,
		removeFile:    os.Remove,
	}
}

// Start returns immediately; it never runs a prune pass on the calling
// goroutine. The first pass (and every one after it, every interval) runs
// on a background goroutine. This matters on a box sitting on months of
// unpruned logs: a first pass there can be tens of thousands of deletes,
// and NewServer calls Start while constructing the Server that answers
// /health - a synchronous first pass there would hold up startup health
// checks for however long that pass takes (PRI-3457 review).
//
// If both retentionDays and maxBytes are <= 0, pruning is fully disabled
// (Jesse's call: off by default in the bare binary; sen-core's
// docker-compose turns it on - 14 days / 5 GiB - for colleague boxes).
// Disabled means disabled: no goroutine is started, no pass ever runs, no
// file is ever walked or deleted. Start logs that pruning is disabled once
// and returns.
//
// A second call to Start is a no-op (idempotent), whether pruning ended up
// enabled or disabled.
func (p *Pruner) Start() {
	p.mu.Lock()
	if p.started {
		p.mu.Unlock()
		return
	}
	p.started = true

	if p.retentionDays <= 0 && p.maxBytes <= 0 {
		p.mu.Unlock()
		log.Printf("llm-proxy log prune: disabled (log_retention_days and log_max_bytes are both 0)")
		return
	}

	p.stopCh = make(chan struct{})
	p.doneCh = make(chan struct{})
	stopCh, doneCh := p.stopCh, p.doneCh
	p.mu.Unlock()

	go func() {
		defer close(doneCh)
		// First pass runs here too, on this goroutine, not on the caller's.
		// It's cancellation-aware (see pruneOnce): if Stop is called while
		// it's still running, it checks stopCh between file deletes and
		// returns early, so Stop doesn't wait out the whole pass.
		p.runAndLog(stopCh)
		ticker := time.NewTicker(p.interval)
		defer ticker.Stop()
		for {
			select {
			case <-stopCh:
				return
			case <-ticker.C:
				p.runAndLog(stopCh)
			}
		}
	}()
}

// Stop signals the background loop to exit and waits for it to finish.
// Idempotent; safe to call even if Start was never called, or if Start ran
// but pruning was disabled (no goroutine to stop, so nothing to wait for).
func (p *Pruner) Stop() {
	p.mu.Lock()
	if !p.started {
		p.mu.Unlock()
		return
	}
	stopCh, doneCh := p.stopCh, p.doneCh
	p.started = false
	p.mu.Unlock()

	if stopCh == nil {
		// Start found pruning disabled and never launched a goroutine.
		return
	}
	close(stopCh)
	<-doneCh
}

func (p *Pruner) runAndLog(stop <-chan struct{}) {
	stats := p.pruneOnce(stop)
	// Counts and bytes only - never a path or a line of log content. A pass
	// that stopped early (shutdown mid-pass) still logs here: its partial
	// counts are accurate for the work it actually did, and a dedicated
	// "stopped early" branch would add a second log shape for no real
	// benefit - operators already see fewer files/bytes than a full pass as
	// the signal that something cut it short.
	log.Printf(
		"llm-proxy log prune: age removed %d files (%d bytes), size-cap removed %d files (%d bytes), removed %d empty date dirs, %d errors",
		stats.AgeFilesDeleted, stats.AgeBytesDeleted,
		stats.SizeFilesDeleted, stats.SizeBytesDeleted,
		stats.DirsRemoved, stats.Errors,
	)
}

// stopRequested reports whether stop has been closed, without blocking.
// A nil stop channel (PruneOnce's uncancellable entry point) never reports
// true - a nil channel's receive case in a select is simply never ready.
func stopRequested(stop <-chan struct{}) bool {
	select {
	case <-stop:
		return true
	default:
		return false
	}
}

// prunableFile is a candidate log file discovered under baseDir, kept around
// after the age pass so the size-cap pass can consider it too.
type prunableFile struct {
	path    string
	size    int64
	modTime time.Time
}

// PruneOnce runs a single, uncancellable pass to completion. It's the
// direct entry point for tests and any caller that wants one deterministic
// full pass; the background loop in Start uses pruneOnce with a
// cancellation channel instead, so Stop doesn't have to wait one out.
func (p *Pruner) PruneOnce() PruneStats {
	return p.pruneOnce(nil)
}

// pruneOnce runs a single pass over baseDir: age-based deletion, then the
// total-size cap (oldest first), then empty-date-dir cleanup. It never
// returns an error and never panics - a missing log dir, an unreadable
// directory, or a permission error on an individual file is counted in
// stats.Errors and skipped; the rest of the pass continues regardless, and
// the caller (the running proxy) is never affected either way.
//
// Only files inside the <baseDir>/<upstream>/<date>/ structure are ever
// considered. Anything else directly under baseDir - sessions.db today,
// whatever else tomorrow - is never touched, no matter its age.
//
// stop, if non-nil, is checked between file deletes and between phases
// (age, size-cap, empty-dir sweep). Once closed, the pass returns
// immediately with whatever partial stats it has accumulated so far -
// never mid-delete, never leaving a file half-removed or double-counted.
func (p *Pruner) pruneOnce(stop <-chan struct{}) PruneStats {
	var stats PruneStats
	active := p.activeFiles()

	upstreamDirs, err := os.ReadDir(p.baseDir)
	if err != nil {
		if !os.IsNotExist(err) {
			// baseDir exists but can't be listed (permissions, etc.) - a
			// real problem to surface in stats, but not one that should
			// stop the proxy.
			stats.Errors++
		}
		return stats
	}

	var cutoff time.Time
	if p.retentionDays > 0 {
		cutoff = time.Now().Add(-time.Duration(p.retentionDays) * 24 * time.Hour)
	}

	var remaining []prunableFile

	for _, upstreamEnt := range upstreamDirs {
		if !upstreamEnt.IsDir() {
			continue // e.g. sessions.db lives directly under baseDir
		}
		upstreamPath := filepath.Join(p.baseDir, upstreamEnt.Name())

		dateDirs, err := os.ReadDir(upstreamPath)
		if err != nil {
			stats.Errors++
			continue
		}

		for _, dateEnt := range dateDirs {
			if !dateEnt.IsDir() {
				continue
			}
			datePath := filepath.Join(upstreamPath, dateEnt.Name())

			entries, err := os.ReadDir(datePath)
			if err != nil {
				stats.Errors++
				continue
			}

			for _, ent := range entries {
				if stopRequested(stop) {
					return stats
				}
				if ent.IsDir() {
					continue // unexpected; don't recurse or delete it
				}
				filePath := filepath.Join(datePath, ent.Name())

				info, err := ent.Info()
				if err != nil {
					// Can't stat it (race, permissions): skip, don't guess.
					stats.Errors++
					continue
				}

				if _, isActive := active[filepath.Clean(filePath)]; isActive {
					remaining = append(remaining, prunableFile{filePath, info.Size(), info.ModTime()})
					continue
				}

				if p.retentionDays > 0 && info.ModTime().Before(cutoff) {
					size := info.Size()
					if err := p.removeFile(filePath); err != nil {
						if !os.IsNotExist(err) {
							stats.Errors++
						}
						continue
					}
					stats.AgeFilesDeleted++
					stats.AgeBytesDeleted += size
					continue
				}

				remaining = append(remaining, prunableFile{filePath, info.Size(), info.ModTime()})
			}
		}
	}

	// Phase boundary: a big age phase can run out the clock on its own: check
	// once here, before starting the (potentially also large) size-cap scan,
	// rather than only between individual deletes within each phase.
	if stopRequested(stop) {
		return stats
	}

	if p.maxBytes > 0 {
		var total int64
		for _, f := range remaining {
			total += f.size
		}
		if total > p.maxBytes {
			sort.Slice(remaining, func(i, j int) bool {
				return remaining[i].modTime.Before(remaining[j].modTime)
			})
			for _, f := range remaining {
				if stopRequested(stop) {
					return stats
				}
				if total <= p.maxBytes {
					break
				}
				if _, isActive := active[filepath.Clean(f.path)]; isActive {
					continue
				}
				if err := p.removeFile(f.path); err != nil {
					if !os.IsNotExist(err) {
						stats.Errors++
					}
					continue
				}
				stats.SizeFilesDeleted++
				stats.SizeBytesDeleted += f.size
				total -= f.size
			}
		}
	}

	// Phase boundary: don't start the directory-cleanup sweep either if
	// shutdown is already in progress - it's cheap, but there's no reason to
	// do even cheap work after cancellation.
	if stopRequested(stop) {
		return stats
	}

	// Final sweep: any date dir left with nothing in it (by either pass
	// above) is removed. Re-reads fresh rather than tracking counts through
	// both passes, which is cheap (directories here are small) and avoids
	// bookkeeping mismatches between the age and size-cap passes.
	for _, upstreamEnt := range upstreamDirs {
		if !upstreamEnt.IsDir() {
			continue
		}
		upstreamPath := filepath.Join(p.baseDir, upstreamEnt.Name())
		dateDirs, err := os.ReadDir(upstreamPath)
		if err != nil {
			continue
		}
		for _, dateEnt := range dateDirs {
			if !dateEnt.IsDir() {
				continue
			}
			datePath := filepath.Join(upstreamPath, dateEnt.Name())
			entries, err := os.ReadDir(datePath)
			if err != nil {
				continue
			}
			if len(entries) == 0 {
				if err := os.Remove(datePath); err == nil {
					stats.DirsRemoved++
				}
			}
		}
	}

	return stats
}
