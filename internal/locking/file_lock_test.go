package locking_test

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/watchflow/watchflow/internal/locking"
)

func TestRepoLocker_MutualExclusion(t *testing.T) {
	locker := locking.NewRepoLocker()
	repoPath := "/tmp/test-repo-lock"

	var counter int32
	var wg sync.WaitGroup

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			unlock := locker.Lock(repoPath)
			defer unlock()

			current := atomic.AddInt32(&counter, 1)
			if current != 1 {
				t.Errorf("violação de exclusão mútua: counter = %d", current)
			}
			time.Sleep(5 * time.Millisecond)
			atomic.AddInt32(&counter, -1)
		}()
	}

	wg.Wait()
}

func TestRepoLocker_TryLock(t *testing.T) {
	locker := locking.NewRepoLocker()
	repoPath := "/tmp/test-repo-trylock"

	unlock, ok := locker.TryLock(repoPath)
	if !ok || unlock == nil {
		t.Fatalf("esperava adquirir o lock via TryLock")
	}

	// Segunda tentativa deve falhar
	unlock2, ok2 := locker.TryLock(repoPath)
	if ok2 || unlock2 != nil {
		t.Errorf("esperava falha no segundo TryLock")
	}

	unlock()

	// Após liberar, deve conseguir adquirir
	unlock3, ok3 := locker.TryLock(repoPath)
	if !ok3 || unlock3 == nil {
		t.Errorf("esperava conseguir adquirir o lock após liberação")
	}
	unlock3()
}
