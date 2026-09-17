package core

import (
	"time"

	"github.com/watchflow/watchflow/internal/watcher"
)

func SetWatcherStartTimeout(d time.Duration) (restore func()) {
	old := watcherStartTimeout
	watcherStartTimeout = d
	return func() { watcherStartTimeout = old }
}

func SetWorkerExitGrace(d time.Duration) (restore func()) {
	old := workerExitGrace
	workerExitGrace = d
	return func() { workerExitGrace = old }
}

func SetWatcherFactory(f func(name, root string) (*watcher.Watcher, error)) (restore func()) {
	old := newWatcher
	newWatcher = f
	return func() { newWatcher = old }
}

func (c *Coordinator) IsCapturing(name string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	_, ok := c.watchers[name]
	return ok
}
