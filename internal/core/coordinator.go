package core

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/watchflow/watchflow/internal/config"
	"github.com/watchflow/watchflow/internal/debouncer"
	"github.com/watchflow/watchflow/internal/ipc"
	"github.com/watchflow/watchflow/internal/logger"
	"github.com/watchflow/watchflow/internal/normalizer"
	"github.com/watchflow/watchflow/internal/notify"
	"github.com/watchflow/watchflow/internal/pipeline"
	"github.com/watchflow/watchflow/internal/providers"
	// Registra os provedores de ações Git no catálogo global providers.DefaultRegistry via init()
	_ "github.com/watchflow/watchflow/internal/providers/git"
	"github.com/watchflow/watchflow/internal/queue"
	"github.com/watchflow/watchflow/internal/watcher"
)

const (
	// pausedHoldDelay é o intervalo de reavaliação de um job cujo watcher está
	// pausado pelo usuário.
	pausedHoldDelay = 5 * time.Second
	// conflictHoldDelay espaça a reavaliação de jobs retidos por conflito, que
	// só destravam com ação humana.
	conflictHoldDelay = 30 * time.Second
	// ipcBindTimeout limita a espera pela vinculação do socket de controle.
	ipcBindTimeout = 5 * time.Second
	// initialPullDelay é a espera antes da primeira consulta ao remoto, curta o
	// bastante para ser imediata na prática e longa o bastante para o daemon
	// terminar de subir.
	initialPullDelay = 3 * time.Second
	// maintenanceInterval é a cadência da rotina de manutenção (retenção do
	// banco e coleta do supressor de eco).
	maintenanceInterval = 1 * time.Hour
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
	// watcherCancels permite parar um watcher isoladamente, sem derrubar os
	// demais — é o que torna a recarga a quente possível.
	watcherCancels map[string]context.CancelFunc
	captureCtx     context.Context
	cfgPath        string
	startTime      time.Time
	cancel         context.CancelFunc
	cancelExec     context.CancelFunc
	workerWg       sync.WaitGroup
	drainWg        sync.WaitGroup
	mu             sync.RWMutex
	paused         map[string]bool
	// lastEnqueue evita que a consulta periódica ao remoto gere trabalho
	// redundante logo depois de uma sincronização disparada por edição local.
	lastEnqueue map[string]time.Time
	version     string
	log         *slog.Logger
	notifier    notify.Notifier
}

// NewCoordinator instancia o coordenador central, configurando o banco SQLite e preparando o estado.
func NewCoordinator(cfg *config.Config, appVersion string) (*Coordinator, error) {
	if cfg == nil {
		return nil, fmt.Errorf("configuração não pode ser nula")
	}

	log := logger.For("coordinator")

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
		if err := store.RegisterWatcher(&queue.WatcherRecord{
			ID:     w.Name,
			Name:   w.Name,
			Path:   targetPath,
			Status: queue.WatcherHealthy,
		}); err != nil {
			log.Error("falha ao registrar watcher no banco de estado",
				slog.String("watcher", w.Name), slog.String("path", targetPath), slog.Any("error", err))
		}
	}

	// Executa recuperação pós-crash antes de iniciar captura
	report, err := RunStartupRecovery(context.Background(), store, cfg.Watchers)
	switch {
	case err != nil:
		log.Error("recuperação de startup falhou", slog.Any("error", err))
	case report != nil:
		log.Info("recuperação de startup concluída",
			slog.Int64("jobs_reenfileirados", report.JobsReset),
			slog.Int("repos_limpos", len(report.ReposCleaned)),
			slog.Int("avisos", len(report.Warnings)))
		for _, w := range report.Warnings {
			log.Warn("aviso da recuperação de startup", slog.String("detalhe", w))
		}
	}

	runner := pipeline.NewRunner(store, providers.DefaultRegistry, cfg)

	c := &Coordinator{
		cfg:            cfg,
		store:          store,
		runner:         runner,
		watchers:       make(map[string]*watcher.Watcher),
		debouncers:     make(map[string]*debouncer.Debouncer),
		filters:        make(map[string]*normalizer.Filter),
		watcherCancels: make(map[string]context.CancelFunc),
		paused:         make(map[string]bool),
		lastEnqueue:    make(map[string]time.Time),
		version:        appVersion,
		log:            log,
		notifier:       notify.New(cfg.Notifications.NotifyOptions()),
	}

	sockPath := config.ExpandPath(cfg.Daemon.SocketPath)
	c.ipcServer = ipc.NewServer(sockPath, c)

	return c, nil
}

// SetConfigPath registra de onde a configuração foi lida, para que 'reload'
// possa reler o mesmo arquivo.
func (c *Coordinator) SetConfigPath(path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cfgPath = path
}

// Store retorna a referência ao armazenamento SQLite gerenciado pelo coordenador.
func (c *Coordinator) Store() *queue.Store {
	return c.store
}

// Start inicia o pipeline reativo, os workers e o servidor IPC.
func (c *Coordinator) Start(ctx context.Context) error {
	// Dois context.Context distintos e independentes:
	//   coordCtx  — captura de eventos (watchers, filtros, debouncers, IPC).
	//   execCtx   — execução de jobs já em voo (comandos git).
	// No shutdown a captura é cortada imediatamente, mas os jobs em andamento
	// recebem o prazo do shutdown para concluir. Cancelar um único contexto
	// enviaria SIGKILL a um 'git push' em curso, deixando o job em RUNNING.
	coordCtx, cancel := context.WithCancel(ctx)
	c.cancel = cancel

	execCtx, cancelExec := context.WithCancel(context.WithoutCancel(ctx))
	c.cancelExec = cancelExec

	c.captureCtx = coordCtx
	c.startTime = time.Now()

	// 1. Inicia o servidor IPC em segundo plano com detecção rápida de erro
	ipcErrCh := make(chan error, 1)
	go func() {
		if err := c.ipcServer.Start(coordCtx); err != nil {
			c.log.Error("servidor IPC encerrou com erro", slog.Any("error", err))
			ipcErrCh <- err
		}
	}()

	// Confirma a trava de processo único aguardando o socket ficar de fato
	// vinculado. A espera fixa anterior podia expirar antes do bind sob carga de
	// I/O e o daemon seguia adiante achando que havia vencido a trava.
	select {
	case err := <-ipcErrCh:
		cancel()
		return fmt.Errorf("falha ao iniciar servidor IPC: %w", err)
	case <-c.ipcServer.Ready():
	case <-time.After(ipcBindTimeout):
		cancel()
		return fmt.Errorf("tempo esgotado (%s) aguardando o servidor IPC vincular o socket", ipcBindTimeout)
	case <-coordCtx.Done():
		cancel()
		return coordCtx.Err()
	}

	// 2. Inicia os workers que consom jobs da fila persistente
	workersCount := c.cfg.Daemon.MaxConcurrentPipelines
	if workersCount <= 0 {
		workersCount = 2
	}

	c.log.Info("iniciando workers de pipeline", slog.Int("quantidade", workersCount))

	for i := 1; i <= workersCount; i++ {
		workerID := fmt.Sprintf("worker-%d", i)
		c.workerWg.Add(1)
		go c.workerLoop(coordCtx, execCtx, workerID)
	}

	// 3. Inicializa e conecta a cadeia reativa para cada watcher ativo
	for _, wCfg := range c.cfg.Watchers {
		if !wCfg.IsEnabled() {
			continue
		}
		if err := c.startWatcher(coordCtx, wCfg); err != nil {
			cancel()
			return err
		}
	}

	// 4. Rotina periódica de manutenção
	c.workerWg.Add(1)
	go c.maintenanceLoop(coordCtx)

	// O mapa passou a ser mutável em tempo de execução (reload), então a
	// contagem precisa ser lida sob a trava como qualquer outro acesso.
	c.mu.RLock()
	activeWatchers := len(c.watchers)
	c.mu.RUnlock()

	c.log.Info("daemon pronto",
		slog.String("versao", c.version),
		slog.Int("watchers", activeWatchers),
		slog.Int("pid", os.Getpid()))

	select {
	case <-coordCtx.Done():
		return nil
	case err := <-ipcErrCh:
		return err
	}
}

// startWatcher monta a cadeia reativa de um watcher e a inicia sob um contexto
// próprio, derivado do de captura. Isso permite parar um watcher isoladamente
// durante uma recarga sem afetar os demais.
func (c *Coordinator) startWatcher(parent context.Context, wCfg config.WatcherConfig) error {
	targetPath := wCfg.ResolvedPath
	if targetPath == "" {
		targetPath = config.ExpandPath(wCfg.Path)
	}

	w, err := watcher.New(wCfg.Name, targetPath)
	if err != nil {
		c.log.Error("falha ao criar watcher",
			slog.String("watcher", wCfg.Name), slog.String("path", targetPath), slog.Any("error", err))
		return fmt.Errorf("falha ao criar watcher para '%s' (%s): %w", wCfg.Name, targetPath, err)
	}

	filter, err := normalizer.NewFilter(targetPath, wCfg.Ignore)
	if err != nil {
		_ = w.Close()
		return fmt.Errorf("falha ao criar normalizador para '%s': %w", wCfg.Name, err)
	}

	deb, err := debouncer.New(wCfg.Name, wCfg.DebounceDuration, wCfg.MaxWaitDuration)
	if err != nil {
		_ = w.Close()
		return fmt.Errorf("falha ao criar debouncer para '%s': %w", wCfg.Name, err)
	}

	watcherCtx, cancelWatcher := context.WithCancel(parent)

	c.mu.Lock()
	c.watchers[wCfg.Name] = w
	c.filters[wCfg.Name] = filter
	c.debouncers[wCfg.Name] = deb
	c.watcherCancels[wCfg.Name] = cancelWatcher
	c.mu.Unlock()

	c.bindWatcherPipeline(watcherCtx, wCfg, w, filter, deb)

	w.Start(watcherCtx)
	deb.Start(watcherCtx)

	if wCfg.PullIntervalDuration > 0 {
		go c.pullLoop(watcherCtx, wCfg)
	}

	c.log.Info("watcher ativo",
		slog.String("watcher", wCfg.Name),
		slog.String("path", targetPath),
		slog.Int("diretorios_vigiados", len(w.WatchedDirs())),
		slog.Duration("debounce", wCfg.DebounceDuration),
		slog.Duration("max_wait", wCfg.MaxWaitDuration),
		slog.Duration("pull_interval", wCfg.PullIntervalDuration))

	return nil
}

// pullLoop consulta o remoto periodicamente, mesmo sem alterações locais.
//
// Toda a captura parte de eventos do sistema de arquivos local; sem este laço,
// o que outra máquina publica só chega quando o usuário edita algo aqui — e ele
// pode acabar editando em cima de uma versão desatualizada.
//
// O job enfileirado é o mesmo pipeline de sempre: sem alterações locais, 'add' e
// 'commit' se pulam sozinhos, 'safe_sync' traz o que houver e 'push' reporta que
// já está em dia. Não é preciso um pipeline separado só para trazer.
func (c *Coordinator) pullLoop(ctx context.Context, wCfg config.WatcherConfig) {
	log := c.log.With(
		slog.String("watcher", wCfg.Name),
		slog.String("rotina", "pull_periodico"))

	ticker := time.NewTicker(wCfg.PullIntervalDuration)
	defer ticker.Stop()

	// A primeira consulta acontece logo no início, e não após um intervalo
	// inteiro. O momento em que a máquina acabou de ligar é exatamente aquele em
	// que ela está mais desatualizada: esperar 5 minutos para descobrir o que as
	// outras máquinas publicaram enquanto ela estava desligada é o pior caso
	// possível — e é quando o usuário mais provavelmente vai abrir as notas e
	// editar em cima de uma versão velha.
	initial := time.NewTimer(initialPullDelay)
	defer initial.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-initial.C:
		case <-ticker.C:
		}

		if skip, reason := c.shouldSkipPull(wCfg); skip {
			log.Debug("consulta periódica ao remoto pulada", slog.String("motivo", reason))
			continue
		}

		for _, pipeName := range wCfg.Pipelines {
			jobID := fmt.Sprintf("pull_%d_%s_%s", time.Now().UnixNano(), wCfg.Name, pipeName)
			job := &queue.Job{
				ID:           jobID,
				WatcherID:    wCfg.Name,
				PipelineName: pipeName,
				PayloadFiles: []string{},
				MaxRetries:   c.retryLimitFor(pipeName),
			}

			if err := c.store.EnqueueJob(job); err != nil {
				log.Error("falha ao enfileirar consulta periódica ao remoto",
					slog.String("job_id", jobID), slog.Any("error", err))
				continue
			}
			log.Debug("consulta periódica ao remoto enfileirada", slog.String("job_id", jobID))
		}
	}
}

// shouldSkipPull evita trabalho inútil: um watcher pausado ou interrompido por
// conflito não deve acumular jobs, e uma sincronização recente já trouxe o que
// havia no remoto.
//
// A janela olha apenas para sincronizações disparadas por ALTERAÇÃO LOCAL. O
// laço de pull não carimba lastEnqueue: se carimbasse, cada consulta suprimiria
// a seguinte — o ticker tem o mesmo período da janela, então o tick seguinte
// encontraria time.Since() alguns microssegundos abaixo do limite e se pularia,
// fazendo a cadência real virar o dobro do pull_interval configurado.
func (c *Coordinator) shouldSkipPull(wCfg config.WatcherConfig) (bool, string) {
	c.mu.RLock()
	isPaused := c.paused[wCfg.Name]
	last := c.lastEnqueue[wCfg.Name]
	c.mu.RUnlock()

	switch {
	case isPaused:
		return true, "watcher pausado"
	case c.isConflictHalted(wCfg.Name):
		return true, "watcher aguardando resolução de conflito"
	case !last.IsZero() && time.Since(last) < wCfg.PullIntervalDuration:
		return true, "sincronização recente disparada por alteração local"
	}

	return false, ""
}

// markEnqueued registra que o watcher acabou de gerar trabalho.
func (c *Coordinator) markEnqueued(name string) {
	c.mu.Lock()
	c.lastEnqueue[name] = time.Now()
	c.mu.Unlock()
}

// stopWatcher encerra um watcher específico. O debouncer descarrega o lote
// pendente e fecha o canal de saída, de modo que a goroutine de drenagem ainda
// persiste esse lote na fila antes de terminar — nada é perdido.
func (c *Coordinator) stopWatcher(name string) {
	c.mu.Lock()
	cancelWatcher, hasCancel := c.watcherCancels[name]
	w, hasWatcher := c.watchers[name]
	deb, hasDeb := c.debouncers[name]

	delete(c.watcherCancels, name)
	delete(c.watchers, name)
	delete(c.debouncers, name)
	delete(c.filters, name)
	c.mu.Unlock()

	if hasWatcher {
		if err := w.Close(); err != nil {
			c.log.Warn("falha ao encerrar watcher", slog.String("watcher", name), slog.Any("error", err))
		}
	}
	if hasDeb {
		deb.Stop()
	}
	if hasCancel {
		cancelWatcher()
	}

	c.log.Info("watcher encerrado", slog.String("watcher", name))
}

func (c *Coordinator) bindWatcherPipeline(
	ctx context.Context,
	wCfg config.WatcherConfig,
	w *watcher.Watcher,
	filter *normalizer.Filter,
	deb *debouncer.Debouncer,
) {
	normEventsCh := make(chan normalizer.NormalizedEvent, 100)
	log := c.log.With(slog.String("watcher", wCfg.Name))

	// Etapa 0: Erros do kernel/fsnotify. Sem este consumidor o canal satura e
	// perdas de evento (IN_Q_OVERFLOW, estouro de max_user_watches) passariam
	// despercebidas, deixando o daemon cego sem qualquer sinal externo.
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case err, ok := <-w.Errors():
				if !ok {
					return
				}
				log.Error("erro reportado pelo watcher do kernel", slog.Any("error", err))

				// Um estouro de max_user_watches ou de fila do inotify significa
				// que eventos foram perdidos: o watcher não está mais íntegro e
				// o usuário precisa saber disso em 'watchflow status'.
				if updErr := c.store.UpdateWatcherStatus(wCfg.Name, queue.WatcherDegraded, err.Error()); updErr != nil {
					log.Warn("falha ao marcar watcher como DEGRADED", slog.Any("error", updErr))
				}
				_ = c.notifier.Notify(ctx, notify.Failure(wCfg.Name, "watcher",
					"o monitoramento do sistema de arquivos reportou erro; eventos podem ter sido perdidos: "+err.Error()))
			}
		}
	}()

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
				if err := c.store.UpdateWatcherEventTime(wCfg.Name); err != nil {
					log.Warn("falha ao registrar horário do último evento", slog.Any("error", err))
				}
				// O envio também observa o cancelamento: sem isso a goroutine
				// vazaria bloqueada caso o debouncer pare com o buffer cheio.
				select {
				case deb.In() <- ev:
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	// Etapa 3: Debouncer Batch -> SQLite Queue
	//
	// Encerra apenas quando o debouncer fecha o canal de saída, e não em
	// ctx.Done(): é isso que garante que o flush final do encerramento seja
	// persistido na fila em vez de descartado junto com a goroutine.
	c.drainWg.Add(1)
	go func() {
		defer c.drainWg.Done()

		for batch := range deb.Out() {
			if len(batch.Files) == 0 {
				continue
			}

			// Um watcher pausado NÃO descarta o lote: o job é persistido e
			// aguarda na fila até o resume. Descartar aqui perderia alterações
			// reais do usuário em silêncio.
			c.markEnqueued(wCfg.Name)

			log.Info("lote de alterações consolidado",
				slog.Int("arquivos", len(batch.Files)),
				slog.Int("eventos", batch.EventsCount),
				slog.String("gatilho", string(batch.Trigger)))

			for _, pipeName := range wCfg.Pipelines {
				jobID := fmt.Sprintf("job_%d_%s_%s", time.Now().UnixNano(), wCfg.Name, pipeName)
				job := &queue.Job{
					ID:           jobID,
					WatcherID:    wCfg.Name,
					PipelineName: pipeName,
					PayloadFiles: batch.Files,
					MaxRetries:   c.retryLimitFor(pipeName),
				}

				if err := c.store.EnqueueJob(job); err != nil {
					// Ponto mais sensível do daemon: falhar aqui significa
					// perder a intenção de sincronizar as alterações do lote.
					log.Error("FALHA AO ENFILEIRAR JOB — alterações não serão sincronizadas",
						slog.String("job_id", jobID),
						slog.String("pipeline", pipeName),
						slog.Int("arquivos", len(batch.Files)),
						slog.Any("error", err))
					continue
				}

				log.Debug("job enfileirado",
					slog.String("job_id", jobID), slog.String("pipeline", pipeName))
			}
		}
	}()
}

// workerLoop consome jobs da fila persistente. stopCtx interrompe a captura de
// novos jobs; execCtx governa a execução do job corrente e só é cancelado ao
// final do prazo de shutdown.
func (c *Coordinator) workerLoop(stopCtx, execCtx context.Context, workerID string) {
	defer c.workerWg.Done()

	log := c.log.With(slog.String("worker", workerID))
	defer log.Debug("worker encerrado")

	for {
		select {
		case <-stopCtx.Done():
			return
		default:
		}

		job, err := c.store.DequeueNextPending(workerID)
		if err != nil {
			log.Warn("falha ao alocar próximo job da fila", slog.Any("error", err))
		}
		if err != nil || job == nil {
			// Nenhum job pendente no momento: dorme brevemente com backoff leve
			select {
			case <-stopCtx.Done():
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
			// Adiamento por decisão do usuário não é falha: reenfileira sem
			// consumir uma tentativa, caso contrário uma pausa de poucos
			// segundos esgotaria max_retries e mataria os jobs pendentes.
			if err := c.store.RequeueJob(job.ID, pausedHoldDelay, "watcher pausado pelo usuário; execução adiada"); err != nil {
				log.Error("falha ao reenfileirar job de watcher pausado",
					slog.String("job_id", job.ID), slog.Any("error", err))
			}
			continue
		}

		// CONFLICT_HALTED significa parada até intervenção humana. Sem esta
		// checagem o daemon reexecuta o pipeline indefinidamente: cada
		// 'git merge --abort' reescreve a árvore, o inotify dispara de novo e
		// o ciclo se realimenta, gerando um alerta por volta.
		if c.isConflictHalted(job.WatcherID) {
			if err := c.store.RequeueJob(job.ID, conflictHoldDelay,
				"watcher interrompido por conflito; aguardando 'watchflow resume'"); err != nil {
				log.Error("falha ao segurar job de watcher em conflito",
					slog.String("job_id", job.ID), slog.Any("error", err))
			}
			log.Debug("job retido: watcher aguardando resolução manual de conflito",
				slog.String("job_id", job.ID), slog.String("watcher", job.WatcherID))
			continue
		}

		// Executa o job através do runner
		jobLog := log.With(
			slog.String("job_id", job.ID),
			slog.String("watcher", job.WatcherID),
			slog.String("pipeline", job.PipelineName))

		jobLog.Info("executando job",
			slog.Int("arquivos", len(job.PayloadFiles)),
			slog.Int("tentativa", job.RetryCount+1))

		res, execErr := c.runner.ExecuteJob(execCtx, job)
		switch {
		case execErr != nil:
			attrs := []any{slog.Any("error", execErr)}
			if res != nil {
				attrs = append(attrs,
					slog.String("step", res.ErrorStep),
					slog.String("categoria", res.Category.String()),
					slog.Duration("duracao", res.Duration))
			}
			jobLog.Error("job falhou", attrs...)
		case res != nil:
			jobLog.Info("job concluído com sucesso", slog.Duration("duracao", res.Duration))
		}

		c.notifyJobOutcome(execCtx, job, res, execErr)
	}
}

// maintenanceLoop executa periodicamente a poda do banco e a coleta de entradas
// vencidas do supressor de eco, mantendo o consumo estável em execução contínua.
func (c *Coordinator) maintenanceLoop(ctx context.Context) {
	defer c.workerWg.Done()

	log := c.log.With(slog.String("rotina", "manutencao"))
	ticker := time.NewTicker(maintenanceInterval)
	defer ticker.Stop()

	run := func() {
		if res, err := c.store.PurgeOldRecords(queue.DefaultRetention); err != nil {
			log.Warn("falha ao purgar registros antigos", slog.Any("error", err))
		} else if res.Jobs > 0 || res.Runs > 0 {
			log.Info("histórico antigo removido",
				slog.Int64("jobs", res.Jobs), slog.Int64("execucoes", res.Runs))
		}

		if n := normalizer.DefaultEchoSuppressor.PurgeExpired(); n > 0 {
			log.Debug("entradas de supressão de eco expiradas removidas", slog.Int("quantidade", n))
		}
	}

	run() // Primeira passagem no boot

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			run()
		}
	}
}

// retryLimitFor resolve o número de tentativas declarado pelo pipeline.
func (c *Coordinator) retryLimitFor(pipelineName string) int {
	if p, ok := c.cfg.Pipelines[pipelineName]; ok {
		return p.RetryLimit()
	}
	return config.DefaultMaxRetries
}

// hasWatcher informa se o nome corresponde a um watcher declarado na configuração.
func (c *Coordinator) hasWatcher(name string) bool {
	for _, w := range c.cfg.Watchers {
		if w.Name == name {
			return true
		}
	}
	return false
}

// isConflictHalted informa se o watcher está interrompido por conflito de merge
// e, portanto, não deve ter pipelines executados até que o usuário intervenha.
func (c *Coordinator) isConflictHalted(watcherID string) bool {
	rec, err := c.store.GetWatcher(watcherID)
	if err != nil || rec == nil {
		return false
	}
	return rec.Status == queue.WatcherConflictHalted
}

// notifyJobOutcome traduz o resultado de um job em alerta ao usuário.
// Falhas transitórias que ainda serão retentadas não geram notificação: só
// interessa avisar quando há algo que exija ação humana.
func (c *Coordinator) notifyJobOutcome(ctx context.Context, job *queue.Job, res *pipeline.RunResult, execErr error) {
	if res == nil {
		return
	}

	switch {
	case execErr == nil && res.Success:
		_ = c.notifier.Notify(ctx, notify.Success(job.WatcherID, len(job.PayloadFiles), res.Duration))

	case res.Category == pipeline.CategoryConflict:
		detail := ""
		if res.Error != nil {
			detail = res.Error.Error()
		}
		_ = c.notifier.Notify(ctx, notify.Conflict(job.WatcherID, detail))

	case !res.WillRetry:
		detail := ""
		if res.Error != nil {
			detail = res.Error.Error()
		}
		_ = c.notifier.Notify(ctx, notify.Failure(job.WatcherID, job.PipelineName, detail))
	}
}

// Shutdown finaliza ordenadamente o daemon liberando todos os recursos.
func (c *Coordinator) Shutdown(ctx context.Context) error {
	c.log.Info("iniciando encerramento gracioso")

	if c.cancel != nil {
		c.cancel()
	}

	// 1. Para todos os watchers
	c.mu.Lock()
	for name, w := range c.watchers {
		if err := w.Close(); err != nil {
			c.log.Warn("falha ao encerrar watcher", slog.String("watcher", name), slog.Any("error", err))
		}
	}
	// 2. Para todos os debouncers
	for _, deb := range c.debouncers {
		deb.Stop()
	}
	c.mu.Unlock()

	// 3. Aguarda a persistência do lote final descarregado pelos debouncers
	if !waitGroup(ctx, &c.drainWg) {
		c.log.Warn("prazo esgotado antes de persistir o lote final dos debouncers")
	}

	// 4. Aguarda os jobs em voo concluírem dentro do prazo de shutdown
	workersFinished := waitGroup(ctx, &c.workerWg)
	if !workersFinished {
		c.log.Warn("prazo de encerramento esgotado; interrompendo jobs ainda em execução")
	}

	// 5. Prazo esgotado: só agora os comandos git em curso são interrompidos
	if c.cancelExec != nil {
		c.cancelExec()
	}

	if !workersFinished {
		c.workerWg.Wait()
	}

	// 6. Jobs interrompidos por ordem de parada não são falha: voltam para
	// PENDING sem consumir retry, para serem retomados no próximo boot.
	if c.store != nil {
		requeued, err := c.store.RequeueRunningJobs("daemon encerrado graciosamente; job reenfileirado sem penalidade")
		if err != nil {
			c.log.Error("falha ao reenfileirar jobs interrompidos", slog.Any("error", err))
		} else if requeued > 0 {
			c.log.Info("jobs interrompidos devolvidos à fila sem penalidade", slog.Int64("quantidade", requeued))
		}
	}

	// 7. Encerra servidor IPC
	if c.ipcServer != nil {
		if err := c.ipcServer.Close(); err != nil {
			c.log.Warn("falha ao encerrar servidor IPC", slog.Any("error", err))
		}
	}

	// 8. Encerra o SQLite store
	if c.store != nil {
		if err := c.store.Close(); err != nil {
			c.log.Warn("falha ao fechar banco de estado", slog.Any("error", err))
		}
	}

	c.log.Info("encerramento concluído")
	return nil
}

// waitGroup aguarda o WaitGroup respeitando o prazo do contexto.
// Retorna true se o grupo concluiu dentro do prazo.
func waitGroup(ctx context.Context, wg *sync.WaitGroup) bool {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
	}
}

// Status responde à solicitação do comando 'watchflow status'.
func (c *Coordinator) Status(ctx context.Context) (*ipc.StatusResponse, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	watchers, err := c.store.ListWatchers()
	if err != nil {
		return nil, err
	}

	// Antes estes contadores eram constantes zero: 'watchflow status' sempre
	// reportava nenhum job em execução ou bloqueado, mesmo com um conflito ativo.
	counts, err := c.store.CountJobsByStatus()
	if err != nil {
		c.log.Warn("falha ao contar jobs por status", slog.Any("error", err))
		counts = map[queue.JobStatus]int{}
	}

	pendingCount := counts[queue.StatusPending]
	runningCount := counts[queue.StatusRunning]
	blockedCount := counts[queue.StatusBlocked]

	var dtos []ipc.WatcherStatusDTO
	for _, w := range watchers {
		lastEv := formatTimePtr(w.LastEventAt)
		lastSuccess := formatTimePtr(w.LastSuccessSyncAt)
		lastFailed := formatTimePtr(w.LastFailedSyncAt)

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
		PendingJobs: pendingCount,
		RunningJobs: runningCount,
		BlockedJobs: blockedCount,
	}, nil
}

// Reload relê o arquivo de configuração e aplica a quente o que for possível.
//
// Watchers são adicionados, removidos e reiniciados individualmente; pipelines e
// notificações são substituídos em memória. A seção 'daemon' (socket, diretório
// de estado, concorrência) não pode mudar com o processo no ar — essas
// diferenças são relatadas ao usuário em vez de aplicadas pela metade.
func (c *Coordinator) Reload(_ context.Context) (*ipc.ReloadResponse, error) {
	c.mu.RLock()
	cfgPath := c.cfgPath
	captureCtx := c.captureCtx
	oldCfg := c.cfg
	c.mu.RUnlock()

	if cfgPath == "" {
		return nil, fmt.Errorf("caminho da configuração desconhecido; reinicie o daemon para recarregar")
	}
	if captureCtx == nil {
		return nil, fmt.Errorf("daemon ainda não iniciou a captura de eventos")
	}

	newCfg, err := config.Load(cfgPath)
	if err != nil {
		// Configuração inválida não substitui a que está no ar: o daemon segue
		// funcionando com a anterior em vez de parar de sincronizar.
		c.log.Warn("recarga recusada: configuração inválida", slog.Any("error", err))
		return nil, fmt.Errorf("configuração inválida; nada foi alterado: %w", err)
	}

	res := &ipc.ReloadResponse{
		Success:        true,
		PipelinesTotal: len(newCfg.Pipelines),
		Warnings:       config.RepoWarnings(newCfg),
		NeedsRestart:   daemonDiff(&oldCfg.Daemon, &newCfg.Daemon),
	}

	oldWatchers := watchersByName(oldCfg)
	newWatchers := watchersByName(newCfg)

	// 1. Removidos e desabilitados
	for name := range oldWatchers {
		if _, kept := newWatchers[name]; !kept {
			c.stopWatcher(name)
			res.WatchersRemoved = append(res.WatchersRemoved, name)
		}
	}

	// 2. Alterados: reiniciados para que caminho, ignore e janelas valham
	for name, newW := range newWatchers {
		oldW, existed := oldWatchers[name]
		if !existed {
			continue
		}
		if reflect.DeepEqual(oldW, newW) {
			continue
		}

		c.stopWatcher(name)
		if err := c.startWatcher(captureCtx, newW); err != nil {
			c.log.Error("falha ao reiniciar watcher durante recarga",
				slog.String("watcher", name), slog.Any("error", err))
			res.Success = false
			continue
		}
		res.WatchersUpdated = append(res.WatchersUpdated, name)
	}

	// 3. Adicionados
	for name, newW := range newWatchers {
		if _, existed := oldWatchers[name]; existed {
			continue
		}
		if err := c.store.RegisterWatcher(&queue.WatcherRecord{
			ID: name, Name: name, Path: newW.ResolvedPath, Status: queue.WatcherHealthy,
		}); err != nil {
			c.log.Error("falha ao registrar watcher novo", slog.String("watcher", name), slog.Any("error", err))
		}
		if err := c.startWatcher(captureCtx, newW); err != nil {
			c.log.Error("falha ao iniciar watcher novo",
				slog.String("watcher", name), slog.Any("error", err))
			res.Success = false
			continue
		}
		res.WatchersAdded = append(res.WatchersAdded, name)
	}

	// 4. Pipelines e notificações valem para todos os jobs seguintes
	c.runner.UpdateConfig(newCfg)

	c.mu.Lock()
	c.cfg = newCfg
	c.notifier = notify.New(newCfg.Notifications.NotifyOptions())
	// Watchers que sumiram da configuração não devem deixar resíduo de pausa
	for name := range c.paused {
		if _, exists := newWatchers[name]; !exists {
			delete(c.paused, name)
		}
	}
	c.mu.Unlock()

	sort.Strings(res.WatchersAdded)
	sort.Strings(res.WatchersRemoved)
	sort.Strings(res.WatchersUpdated)

	res.Message = fmt.Sprintf("configuração recarregada (%d adicionado(s), %d removido(s), %d atualizado(s))",
		len(res.WatchersAdded), len(res.WatchersRemoved), len(res.WatchersUpdated))

	c.log.Info("configuração recarregada",
		slog.Int("adicionados", len(res.WatchersAdded)),
		slog.Int("removidos", len(res.WatchersRemoved)),
		slog.Int("atualizados", len(res.WatchersUpdated)),
		slog.Int("requer_reinicio", len(res.NeedsRestart)))

	return res, nil
}

// watchersByName indexa apenas os watchers habilitados: desabilitar um watcher
// na configuração equivale a removê-lo do ponto de vista da captura.
func watchersByName(cfg *config.Config) map[string]config.WatcherConfig {
	out := make(map[string]config.WatcherConfig, len(cfg.Watchers))
	for _, w := range cfg.Watchers {
		if w.IsEnabled() {
			out[w.Name] = w
		}
	}
	return out
}

// daemonDiff lista os campos da seção 'daemon' que mudaram e que só têm efeito
// após reiniciar o processo.
func daemonDiff(oldD, newD *config.DaemonConfig) []string {
	var changed []string

	if oldD.SocketPath != newD.SocketPath {
		changed = append(changed, fmt.Sprintf("socket_path (%s → %s)", oldD.SocketPath, newD.SocketPath))
	}
	if oldD.StateDir != newD.StateDir {
		changed = append(changed, fmt.Sprintf("state_dir (%s → %s)", oldD.StateDir, newD.StateDir))
	}
	if oldD.MaxConcurrentPipelines != newD.MaxConcurrentPipelines {
		changed = append(changed, fmt.Sprintf("max_concurrent_pipelines (%d → %d)",
			oldD.MaxConcurrentPipelines, newD.MaxConcurrentPipelines))
	}
	if oldD.LogLevel != newD.LogLevel {
		changed = append(changed, fmt.Sprintf("log_level (%s → %s)", oldD.LogLevel, newD.LogLevel))
	}

	return changed
}

// Jobs responde à consulta do estado da fila persistente (CLI 'jobs' e TUI).
func (c *Coordinator) Jobs(_ context.Context, watcherName string, limit int) (*ipc.JobsResponse, error) {
	if watcherName != "" && !c.hasWatcher(watcherName) {
		return nil, fmt.Errorf("watcher '%s' não existe na configuração", watcherName)
	}

	jobs, err := c.store.ListRecentJobs(watcherName, limit)
	if err != nil {
		c.log.Warn("falha ao listar jobs", slog.Any("error", err))
		return nil, err
	}

	dtos := make([]ipc.JobDTO, 0, len(jobs))
	for _, j := range jobs {
		dto := ipc.JobDTO{
			ID:           j.ID,
			WatcherID:    j.WatcherID,
			PipelineName: j.PipelineName,
			Status:       string(j.Status),
			Files:        len(j.PayloadFiles),
			RetryCount:   j.RetryCount,
			MaxRetries:   j.MaxRetries,
			LastError:    j.LastError,
			UpdatedAt:    formatTime(j.UpdatedAt),
		}
		// Só faz sentido exibir o agendamento de um job que ainda vai rodar.
		if j.Status == queue.StatusPending {
			dto.ScheduledFor = formatTime(j.ScheduledFor)
		}
		dtos = append(dtos, dto)
	}

	return &ipc.JobsResponse{Jobs: dtos}, nil
}

// Runs responde à consulta do histórico de auditoria de execuções.
func (c *Coordinator) Runs(_ context.Context, watcherName string, limit int) (*ipc.RunsResponse, error) {
	if watcherName != "" && !c.hasWatcher(watcherName) {
		return nil, fmt.Errorf("watcher '%s' não existe na configuração", watcherName)
	}

	runs, err := c.store.ListRecentRuns(watcherName, limit)
	if err != nil {
		c.log.Warn("falha ao listar execuções", slog.Any("error", err))
		return nil, err
	}

	dtos := make([]ipc.RunDTO, 0, len(runs))
	for _, r := range runs {
		dtos = append(dtos, ipc.RunDTO{
			ID:           r.ID,
			JobID:        r.JobID,
			WatcherID:    r.WatcherID,
			PipelineName: r.PipelineName,
			Status:       r.Status,
			DurationMs:   r.DurationMs,
			ErrorStep:    r.ErrorStep,
			ErrorDetails: r.ErrorDetails,
			CreatedAt:    formatTime(r.CreatedAt),
		})
	}

	return &ipc.RunsResponse{Runs: dtos}, nil
}

// formatTime normaliza timestamps para exibição, devolvendo vazio quando nulos.
//
// O SQLite grava CURRENT_TIMESTAMP em UTC e o driver devolve o valor marcado
// como UTC. Formatar sem converter mostraria ao usuário um horário deslocado
// pelo fuso — em UTC-3, uma sincronização das 23h aparecia como 02h do dia
// seguinte. O instante sempre esteve correto; apenas a exibição não.
func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Local().Format("2006-01-02 15:04:05")
}

// formatTimePtr é a variante para colunas anuláveis.
func formatTimePtr(t *time.Time) string {
	if t == nil {
		return ""
	}
	return formatTime(*t)
}

// Sync responde à solicitação do comando 'watchflow sync'.
func (c *Coordinator) Sync(ctx context.Context, watcherName string) (*ipc.SyncResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if watcherName != "" && !c.hasWatcher(watcherName) {
		return nil, fmt.Errorf("watcher '%s' não existe na configuração", watcherName)
	}

	var (
		enqueued []string
		skipped  []string
	)

	for _, wCfg := range c.cfg.Watchers {
		if watcherName != "" && wCfg.Name != watcherName {
			continue
		}

		// Enfileirar para um watcher desabilitado produziria jobs que nenhum
		// watcher está monitorando e que ninguém pediu.
		if !wCfg.IsEnabled() {
			skipped = append(skipped, wCfg.Name)
			continue
		}

		for _, pipeName := range wCfg.Pipelines {
			jobID := fmt.Sprintf("manual_%d_%s", time.Now().UnixNano(), wCfg.Name)
			job := &queue.Job{
				ID:           jobID,
				WatcherID:    wCfg.Name,
				PipelineName: pipeName,
				PayloadFiles: []string{},
				MaxRetries:   c.retryLimitFor(pipeName),
			}
			if err := c.store.EnqueueJob(job); err != nil {
				c.log.Error("falha ao enfileirar job de sincronização manual",
					slog.String("job_id", jobID), slog.String("watcher", wCfg.Name), slog.Any("error", err))
				continue
			}
			enqueued = append(enqueued, jobID)
		}
	}

	c.log.Info("sincronização manual solicitada",
		slog.String("watcher_filtro", watcherName),
		slog.Int("jobs", len(enqueued)),
		slog.Int("ignorados", len(skipped)))

	message := fmt.Sprintf("sincronização forçada solicitada (%d jobs gerados)", len(enqueued))
	if len(skipped) > 0 {
		message += fmt.Sprintf("; %d watcher(s) desabilitado(s) ignorado(s): %s",
			len(skipped), strings.Join(skipped, ", "))
	}

	return &ipc.SyncResponse{
		EnqueuedJobs: enqueued,
		Message:      message,
	}, nil
}

// Pause responde à solicitação do comando 'watchflow pause'.
func (c *Coordinator) Pause(ctx context.Context, watcherName string) (*ipc.ActionResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if watcherName != "" {
		c.paused[watcherName] = true
		if err := c.store.UpdateWatcherStatus(watcherName, queue.WatcherPaused, ""); err != nil {
			c.log.Warn("falha ao persistir status PAUSED", slog.String("watcher", watcherName), slog.Any("error", err))
		}
		c.log.Info("watcher pausado", slog.String("watcher", watcherName))
		return &ipc.ActionResponse{
			Success: true,
			Message: fmt.Sprintf("watcher '%s' pausado com sucesso", watcherName),
		}, nil
	}

	for _, w := range c.cfg.Watchers {
		c.paused[w.Name] = true
		if err := c.store.UpdateWatcherStatus(w.Name, queue.WatcherPaused, ""); err != nil {
			c.log.Warn("falha ao persistir status PAUSED", slog.String("watcher", w.Name), slog.Any("error", err))
		}
	}
	c.log.Info("todos os watchers pausados", slog.Int("quantidade", len(c.cfg.Watchers)))

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
		// Também limpa CONFLICT_HALTED: 'resume' é a confirmação do usuário de
		// que o conflito foi resolvido manualmente.
		if err := c.store.UpdateWatcherStatus(watcherName, queue.WatcherHealthy, ""); err != nil {
			c.log.Warn("falha ao persistir status HEALTHY", slog.String("watcher", watcherName), slog.Any("error", err))
		}
		if r, ok := c.notifier.(notify.Resetter); ok {
			r.Reset(watcherName)
		}
		c.log.Info("watcher retomado", slog.String("watcher", watcherName))
		return &ipc.ActionResponse{
			Success: true,
			Message: fmt.Sprintf("watcher '%s' retomado com sucesso", watcherName),
		}, nil
	}

	for _, w := range c.cfg.Watchers {
		delete(c.paused, w.Name)
		if err := c.store.UpdateWatcherStatus(w.Name, queue.WatcherHealthy, ""); err != nil {
			c.log.Warn("falha ao persistir status HEALTHY", slog.String("watcher", w.Name), slog.Any("error", err))
		}
	}
	if r, ok := c.notifier.(notify.Resetter); ok {
		for _, w := range c.cfg.Watchers {
			r.Reset(w.Name)
		}
	}
	c.log.Info("todos os watchers retomados", slog.Int("quantidade", len(c.cfg.Watchers)))

	return &ipc.ActionResponse{
		Success: true,
		Message: "todos os watchers foram retomados",
	}, nil
}

// Stop responde à solicitação do comando 'watchflow stop'.
func (c *Coordinator) Stop(ctx context.Context) (*ipc.ActionResponse, error) {
	c.log.Info("encerramento solicitado via IPC")

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
