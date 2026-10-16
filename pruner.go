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
	}
}

// Start runs one prune pass immediately and synchronously - so its effects
// (including the summary log line) are complete before Start returns, with
// no observable race against whatever the caller does right after - then one
// more every interval in the background, until Stop is called. A second call
// to Start is a no-op (idempotent).
func (p *Pruner) Start() {
	p.mu.Lock()
	if p.started {
		p.mu.Unlock()
		return
	}
	p.started = true
	p.stopCh = make(chan struct{})
	p.doneCh = make(chan struct{})
	stopCh, doneCh := p.stopCh, p.doneCh
	p.mu.Unlock()

	p.runAndLog()

	go func() {
		defer close(doneCh)
		ticker := time.NewTicker(p.interval)
		defer ticker.Stop()
		for {
			select {
			case <-stopCh:
				return
			case <-ticker.C:
				p.runAndLog()
			}
		}
	}()
}

// Stop signals the background loop to exit and waits for it to finish.
// Idempotent; safe to call even if Start was never called.
func (p *Pruner) Stop() {
	p.mu.Lock()
	if !p.started {
		p.mu.Unlock()
		return
	}
	stopCh, doneCh := p.stopCh, p.doneCh
	p.started = false
	p.mu.Unlock()

	close(stopCh)
	<-doneCh
}

func (p *Pruner) runAndLog() {
	stats := p.PruneOnce()
	// Counts and bytes only - never a path or a line of log content.
	log.Printf(
		"llm-proxy log prune: age removed %d files (%d bytes), size-cap removed %d files (%d bytes), removed %d empty date dirs, %d errors",
		stats.AgeFilesDeleted, stats.AgeBytesDeleted,
		stats.SizeFilesDeleted, stats.SizeBytesDeleted,
		stats.DirsRemoved, stats.Errors,
	)
}

// prunableFile is a candidate log file discovered under baseDir, kept around
// after the age pass so the size-cap pass can consider it too.
type prunableFile struct {
	path    string
	size    int64
	modTime time.Time
}

// PruneOnce runs a single pass over baseDir: age-based deletion, then the
// total-size cap (oldest first), then empty-date-dir cleanup. It never
// returns an error and never panics - a missing log dir, an unreadable
// directory, or a permission error on an individual file is counted in
// stats.Errors and skipped; the rest of the pass continues regardless, and
// the caller (the running proxy) is never affected either way.
//
// Only files inside the <baseDir>/<upstream>/<date>/ structure are ever
// considered. Anything else directly under baseDir - sessions.db today,
// whatever else tomorrow - is never touched, no matter its age.
func (p *Pruner) PruneOnce() PruneStats {
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
					if err := os.Remove(filePath); err != nil {
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
				if total <= p.maxBytes {
					break
				}
				if _, isActive := active[filepath.Clean(f.path)]; isActive {
					continue
				}
				if err := os.Remove(f.path); err != nil {
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
