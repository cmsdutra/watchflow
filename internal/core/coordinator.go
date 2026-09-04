package core

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/watchflow/watchflow/internal/config"
	"github.com/watchflow/watchflow/internal/debouncer"
	"github.com/watchflow/watchflow/internal/ipc"
	"github.com/watchflow/watchflow/internal/normalizer"
	"github.com/watchflow/watchflow/internal/pipeline"
	"github.com/watchflow/watchflow/internal/providers"
	// Registra os provedores de ações Git no catálogo global providers.DefaultRegistry via init()
	_ "github.com/watchflow/watchflow/internal/providers/git"
	"github.com/watchflow/watchflow/internal/queue"
	"github.com/watchflow/watchflow/internal/watcher"
)

// Coordinator é o orquestrador central do daemon, unificando watchers, normalizers,
// debouncers, fila SQLite, workers de pipeline e o servidor IPC.
type Coordinator struct {
	cfg        *config.Config
	store      *queue.Store
	runner     *pipeline.Runner
	ipcServer  *ipc.Server
	watchers   map[string]*watcher.Watcher
	debouncers map[string]*debouncer.Debouncer
	filters    map[string]*normalizer.Filter
	startTime  time.Time
	cancel     context.CancelFunc
	workerWg   sync.WaitGroup
	mu         sync.RWMutex
	paused     map[string]bool
	version    string
}

// NewCoordinator instancia o coordenador central, configurando o banco SQLite e preparando o estado.
func NewCoordinator(cfg *config.Config, appVersion string) (*Coordinator, error) {
	if cfg == nil {
		return nil, fmt.Errorf("configuração não pode ser nula")
	}

	stateDir := config.ExpandPath(cfg.Daemon.StateDir)
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		return nil, fmt.Errorf("falha ao criar diretório de estado '%s': %w", stateDir, err)
	}

	dbPath := filepath.Join(stateDir, "state.db")
	store, err := queue.NewSQLiteStore(dbPath)
	if err != nil {
		return nil, fmt.Errorf("falha ao abrir banco SQLite '%s': %w", dbPath, err)
	}

	// Registra todos os watchers configurados no SQLite
	for _, w := range cfg.Watchers {
		targetPath := w.ResolvedPath
		if targetPath == "" {
			targetPath = config.ExpandPath(w.Path)
		}
		_ = store.RegisterWatcher(&queue.WatcherRecord{
			ID:     w.Name,
			Name:   w.Name,
			Path:   targetPath,
			Status: queue.WatcherHealthy,
		})
	}

	// Executa recuperação pós-crash antes de iniciar captura
	_, _ = RunStartupRecovery(context.Background(), store, cfg.Watchers)

	runner := pipeline.NewRunner(store, providers.DefaultRegistry, cfg)

	c := &Coordinator{
		cfg:        cfg,
		store:      store,
		runner:     runner,
		watchers:   make(map[string]*watcher.Watcher),
		debouncers: make(map[string]*debouncer.Debouncer),
		filters:    make(map[string]*normalizer.Filter),
		paused:     make(map[string]bool),
		version:    appVersion,
	}

	sockPath := config.ExpandPath(cfg.Daemon.SocketPath)
	c.ipcServer = ipc.NewServer(sockPath, c)

	return c, nil
}

// Store retorna a referência ao armazenamento SQLite gerenciado pelo coordenador.
func (c *Coordinator) Store() *queue.Store {
	return c.store
}

// Start inicia o pipeline reativo, os workers e o servidor IPC.
func (c *Coordinator) Start(ctx context.Context) error {
	coordCtx, cancel := context.WithCancel(ctx)
	c.cancel = cancel
	c.startTime = time.Now()

	// 1. Inicia o servidor IPC em segundo plano com detecção rápida de erro
	ipcErrCh := make(chan error, 1)
	go func() {
		if err := c.ipcServer.Start(coordCtx); err != nil {
			ipcErrCh <- err
		}
	}()

	// Verifica se o IPC conseguiu vincular o socket (ex.: trava de processo único)
	select {
	case err := <-ipcErrCh:
		cancel()
		return fmt.Errorf("falha ao iniciar servidor IPC: %w", err)
	case <-time.After(50 * time.Millisecond):
	}

	// 2. Inicia os workers que consom jobs da fila persistente
	workersCount := c.cfg.Daemon.MaxConcurrentPipelines
	if workersCount <= 0 {
		workersCount = 2
	}

	for i := 1; i <= workersCount; i++ {
		workerID := fmt.Sprintf("worker-%d", i)
		c.workerWg.Add(1)
		go c.workerLoop(coordCtx, workerID)
	}

	// 3. Inicializa e conecta a cadeia reativa para cada watcher ativo
	for _, wCfg := range c.cfg.Watchers {
		if !wCfg.IsEnabled() {
			continue
		}

		targetPath := wCfg.ResolvedPath
		if targetPath == "" {
			targetPath = config.ExpandPath(wCfg.Path)
		}

		w, err := watcher.New(wCfg.Name, targetPath)
		if err != nil {
			cancel()
			return fmt.Errorf("falha ao criar watcher para '%s' (%s): %w", wCfg.Name, targetPath, err)
		}

		filter, err := normalizer.NewFilter(targetPath, wCfg.Ignore)
		if err != nil {
			cancel()
			return fmt.Errorf("falha ao criar normalizador para '%s': %w", wCfg.Name, err)
		}

		deb, err := debouncer.New(wCfg.Name, wCfg.DebounceDuration, wCfg.MaxWaitDuration)
		if err != nil {
			cancel()
			return fmt.Errorf("falha ao criar debouncer para '%s': %w", wCfg.Name, err)
		}

		c.mu.Lock()
		c.watchers[wCfg.Name] = w
		c.filters[wCfg.Name] = filter
		c.debouncers[wCfg.Name] = deb
		c.mu.Unlock()

		// Conecta os canais de fluxo
		c.bindWatcherPipeline(coordCtx, wCfg, w, filter, deb)

		w.Start(coordCtx)
		deb.Start(coordCtx)
	}

	select {
	case <-coordCtx.Done():
		return nil
	case err := <-ipcErrCh:
		return err
	}
}

func (c *Coordinator) bindWatcherPipeline(
	ctx context.Context,
	wCfg config.WatcherConfig,
	w *watcher.Watcher,
	filter *normalizer.Filter,
	deb *debouncer.Debouncer,
) {
	normEventsCh := make(chan normalizer.NormalizedEvent, 100)

	// Etapa 1: Watcher -> Normalizer
	go filter.ProcessStream(ctx, w.Events(), normEventsCh)

	// Etapa 2: Normalizer -> Debouncer
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-normEventsCh:
				if !ok {
					return
				}
				_ = c.store.UpdateWatcherEventTime(wCfg.Name)
				deb.In() <- ev
			}
		}
	}()

	// Etapa 3: Debouncer Batch -> SQLite Queue
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case batch, ok := <-deb.Out():
				if !ok {
					return
				}
				if len(batch.Files) == 0 {
					continue
				}

				c.mu.RLock()
				isPaused := c.paused[wCfg.Name]
				c.mu.RUnlock()
				if isPaused {
					continue
				}

				// Para cada pipeline associado a este watcher, cria um job persistente
				for _, pipeName := range wCfg.Pipelines {
					jobID := fmt.Sprintf("job_%d_%s", time.Now().UnixNano(), wCfg.Name)
					job := &queue.Job{
						ID:           jobID,
						WatcherID:    wCfg.Name,
						PipelineName: pipeName,
						PayloadFiles: batch.Files,
						MaxRetries:   5,
					}
					_ = c.store.EnqueueJob(job)
				}
			}
		}
	}()
}

func (c *Coordinator) workerLoop(ctx context.Context, workerID string) {
	defer c.workerWg.Done()

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		job, err := c.store.DequeueNextPending(workerID)
		if err != nil || job == nil {
			// Nenhum job pendente no momento: dorme brevemente com backoff leve
			select {
			case <-ctx.Done():
				return
			case <-time.After(300 * time.Millisecond):
				continue
			}
		}

		// Checa se o watcher do job está pausado
		c.mu.RLock()
		isPaused := c.paused[job.WatcherID]
		c.mu.RUnlock()
		if isPaused {
			// Devolve para pending para processar quando for despausado
			_ = c.store.MarkJobFailed(job.ID, "watcher pausado temporariamente", true, 5*time.Second)
			continue
		}

		// Executa o job através do runner
		_, _ = c.runner.ExecuteJob(ctx, job)
	}
}

// Shutdown finaliza ordenadamente o daemon liberando todos os recursos.
func (c *Coordinator) Shutdown(ctx context.Context) error {
	if c.cancel != nil {
		c.cancel()
	}

	// 1. Para todos os watchers
	c.mu.Lock()
	for _, w := range c.watchers {
		_ = w.Close()
	}
	// 2. Para todos os debouncers
	for _, deb := range c.debouncers {
		deb.Stop()
	}
	c.mu.Unlock()

	// 3. Aguarda workers em voo com timeout
	workerDone := make(chan struct{})
	go func() {
		c.workerWg.Wait()
		close(workerDone)
	}()

	select {
	case <-workerDone:
	case <-ctx.Done():
	}

	// 4. Encerra servidor IPC
	if c.ipcServer != nil {
		_ = c.ipcServer.Close()
	}

	// 5. Encerra o SQLite store
	if c.store != nil {
		_ = c.store.Close()
	}

	return nil
}

// Status responde à solicitação do comando 'watchflow status'.
func (c *Coordinator) Status(ctx context.Context) (*ipc.StatusResponse, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	watchers, err := c.store.ListWatchers()
	if err != nil {
		return nil, err
	}

	pendingJobs, _ := c.store.ListPendingJobs("")
	runningCount := 0
	blockedCount := 0

	var dtos []ipc.WatcherStatusDTO
	for _, w := range watchers {
		lastEv := ""
		if w.LastEventAt != nil {
			lastEv = w.LastEventAt.Format("2006-01-02 15:04:05")
		}
		lastSuccess := ""
		if w.LastSuccessSyncAt != nil {
			lastSuccess = w.LastSuccessSyncAt.Format("2006-01-02 15:04:05")
		}
		lastFailed := ""
		if w.LastFailedSyncAt != nil {
			lastFailed = w.LastFailedSyncAt.Format("2006-01-02 15:04:05")
		}

		statusStr := string(w.Status)
		if c.paused[w.Name] {
			statusStr = string(queue.WatcherPaused)
		}

		dtos = append(dtos, ipc.WatcherStatusDTO{
			ID:                w.ID,
			Name:              w.Name,
			Path:              w.Path,
			Status:            statusStr,
			LastEventAt:       lastEv,
			LastSuccessSyncAt: lastSuccess,
			LastFailedSyncAt:  lastFailed,
			LastError:         w.LastErrorMessage,
		})
	}

	uptimeStr := "-"
	if !c.startTime.IsZero() {
		uptimeStr = time.Since(c.startTime).Truncate(time.Second).String()
	}

	versionStr := c.version
	if versionStr == "" {
		versionStr = "0.1.0"
	}

	return &ipc.StatusResponse{
		DaemonPID:   os.Getpid(),
		Uptime:      uptimeStr,
		Version:     versionStr,
		Watchers:    dtos,
		PendingJobs: len(pendingJobs),
		RunningJobs: runningCount,
		BlockedJobs: blockedCount,
	}, nil
}

// Sync responde à solicitação do comando 'watchflow sync'.
func (c *Coordinator) Sync(ctx context.Context, watcherName string) (*ipc.SyncResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	var enqueued []string
	for _, wCfg := range c.cfg.Watchers {
		if watcherName != "" && wCfg.Name != watcherName {
			continue
		}

		for _, pipeName := range wCfg.Pipelines {
			jobID := fmt.Sprintf("manual_%d_%s", time.Now().UnixNano(), wCfg.Name)
			job := &queue.Job{
				ID:           jobID,
				WatcherID:    wCfg.Name,
				PipelineName: pipeName,
				PayloadFiles: []string{},
				MaxRetries:   5,
			}
			if err := c.store.EnqueueJob(job); err == nil {
				enqueued = append(enqueued, jobID)
			}
		}
	}

	return &ipc.SyncResponse{
		EnqueuedJobs: enqueued,
		Message:      fmt.Sprintf("sincronização forçada solicitada (%d jobs gerados)", len(enqueued)),
	}, nil
}

// Pause responde à solicitação do comando 'watchflow pause'.
func (c *Coordinator) Pause(ctx context.Context, watcherName string) (*ipc.ActionResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if watcherName != "" {
		c.paused[watcherName] = true
		_ = c.store.UpdateWatcherStatus(watcherName, queue.WatcherPaused, "")
		return &ipc.ActionResponse{
			Success: true,
			Message: fmt.Sprintf("watcher '%s' pausado com sucesso", watcherName),
		}, nil
	}

	for _, w := range c.cfg.Watchers {
		c.paused[w.Name] = true
		_ = c.store.UpdateWatcherStatus(w.Name, queue.WatcherPaused, "")
	}

	return &ipc.ActionResponse{
		Success: true,
		Message: "todos os watchers foram pausados",
	}, nil
}

// Resume responde à solicitação do comando 'watchflow resume'.
func (c *Coordinator) Resume(ctx context.Context, watcherName string) (*ipc.ActionResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if watcherName != "" {
		delete(c.paused, watcherName)
		_ = c.store.UpdateWatcherStatus(watcherName, queue.WatcherHealthy, "")
		return &ipc.ActionResponse{
			Success: true,
			Message: fmt.Sprintf("watcher '%s' retomado com sucesso", watcherName),
		}, nil
	}

	for _, w := range c.cfg.Watchers {
		delete(c.paused, w.Name)
		_ = c.store.UpdateWatcherStatus(w.Name, queue.WatcherHealthy, "")
	}

	return &ipc.ActionResponse{
		Success: true,
		Message: "todos os watchers foram retomados",
	}, nil
}

// Stop responde à solicitação do comando 'watchflow stop'.
func (c *Coordinator) Stop(ctx context.Context) (*ipc.ActionResponse, error) {
	go func() {
		time.Sleep(100 * time.Millisecond)
		if c.cancel != nil {
			c.cancel()
		}
	}()

	return &ipc.ActionResponse{
		Success: true,
		Message: "solicitação de encerramento do daemon aceita com sucesso",
	}, nil
}
