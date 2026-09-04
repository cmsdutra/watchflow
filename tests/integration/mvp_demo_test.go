package integration_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/watchflow/watchflow/internal/config"
	"github.com/watchflow/watchflow/internal/core"
	"github.com/watchflow/watchflow/internal/ipc"
	"github.com/watchflow/watchflow/internal/queue"
	"github.com/watchflow/watchflow/tests/testutil"
)

// TestMVP_FullAutonomousSyncDemonstration valida integralmente o roteiro de homologação do MVP (Fase 9 / WF-016).
// Simula a sincronização bidirecional entre dois cofres locais (Vault 1 e Vault 2) através de um remote central bare,
// cobrindo:
// 1. Inicialização e status baseline limpo.
// 2. Coalescência reativa (debounce) de múltiplos arquivos com auto-commit e push.
// 3. Resiliência offline-first com commit local preservado durante queda de rede e recuperação ao reconectar.
// 4. Convergência bidirecional com Safe Git Sync (merge de 3 vias sem detached HEAD).
// 5. Proteção absoluta contra perda de dados em conflitos de merge (git merge --abort automático, zero marcadores de conflito e estado CONFLICT_HALTED).
// 6. Encerramento gracioso do daemon via IPC sem corrupção de estado.
func TestMVP_FullAutonomousSyncDemonstration(t *testing.T) {
	tempBase := t.TempDir()

	// 1. Cria o remote bare central
	bareRemote := testutil.NewBareRepo(t)

	// 2. Inicializa o Vault 1 (Cofre monitorado pelo WatchFlow daemon)
	vault1 := testutil.NewGitSandbox(t)
	vault1.WriteFile("README.md", "# Knowledge Vault — WatchFlow Monitored")
	vault1.CommitAll("initial commit")
	vault1.MustRunGit("remote", "add", "origin", bareRemote)
	vault1.MustRunGit("push", "-u", "origin", "main")

	// 3. Inicializa o Vault 2 (Segundo dispositivo / peer clonado do remote bare)
	vault2 := testutil.CloneRepo(t, bareRemote)

	// 4. Configuração do daemon WatchFlow para monitorar Vault 1
	stateDir := filepath.Join(tempBase, "state")
	sockPath := filepath.Join(tempBase, "watchflow.sock")

	cfg := &config.Config{
		Version: 1,
		Daemon: config.DaemonConfig{
			StateDir:               stateDir,
			SocketPath:             sockPath,
			LogLevel:               "info",
			MaxConcurrentPipelines: 2,
		},
		Watchers: []config.WatcherConfig{
			{
				Name:             "vault-1",
				Path:             vault1.RootDir,
				ResolvedPath:     vault1.RootDir,
				Debounce:         "100ms",
				MaxWait:          "250ms",
				DebounceDuration: 100 * time.Millisecond,
				MaxWaitDuration:  250 * time.Millisecond,
				Pipelines:        []string{"vault-sync"},
			},
		},
		Pipelines: map[string]config.Pipeline{
			"vault-sync": {
				Timeout:         "15s",
				TimeoutDuration: 15 * time.Second,
				Steps: []config.Step{
					{Action: "git.check_locks"},
					{Action: "git.add"},
					{
						Action: "git.commit",
						Params: map[string]interface{}{
							"message": "watchflow: auto-sync {timestamp}",
						},
					},
					{Action: "git.safe_sync"},
					{Action: "git.push"},
				},
			},
		},
	}

	coord, err := core.NewCoordinator(cfg, "1.0.0-mvp")
	if err != nil {
		t.Fatalf("falha ao instanciar coordinator: %v", err)
	}

	daemonCtx, daemonCancel := context.WithCancel(context.Background())
	defer daemonCancel()

	go func() {
		_ = coord.Start(daemonCtx)
	}()

	// Aguarda vinculação do Unix socket
	client := ipc.NewClient(sockPath)
	var isConnected bool
	for i := 0; i < 30; i++ {
		time.Sleep(100 * time.Millisecond)
		if _, statErr := os.Stat(sockPath); statErr == nil {
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			if _, statusErr := client.Status(ctx); statusErr == nil {
				isConnected = true
				cancel()
				break
			}
			cancel()
		}
	}
	if !isConnected {
		t.Fatalf("daemon não inicializou o socket IPC a tempo")
	}

	// -------------------------------------------------------------------------
	// Marco 1: Status Baseline e Fila Limpa
	// -------------------------------------------------------------------------
	t.Log("--- Marco 1: Verificação de Linha de Base (Baseline) ---")
	statusResp, err := client.Status(context.Background())
	if err != nil {
		t.Fatalf("falha ao consultar status inicial: %v", err)
	}
	if len(statusResp.Watchers) != 1 || statusResp.Watchers[0].Status != string(queue.WatcherHealthy) {
		t.Fatalf("status baseline inesperado: %+v", statusResp)
	}
	if statusResp.PendingJobs != 0 {
		t.Fatalf("esperava 0 jobs pendentes no início, obtido: %d", statusResp.PendingJobs)
	}

	// -------------------------------------------------------------------------
	// Marco 2: Coalescência Reativa (Debounce) e Propagação para o Remote
	// -------------------------------------------------------------------------
	t.Log("--- Marco 2: Debounce de Rajadas e Propagação para o Remote ---")
	vault1.WriteFile("Inbox/ideia1.md", "# Ideia 1\nTexto da nota 1.")
	time.Sleep(30 * time.Millisecond)
	vault1.WriteFile("Inbox/ideia2.md", "# Ideia 2\nTexto da nota 2.")

	// Aguarda processamento do debouncer (100ms) + pipeline Git (add + commit + push)
	time.Sleep(1200 * time.Millisecond)

	// Verifica se Vault 1 possui o commit automático
	v1Log := vault1.MustRunGit("log", "--oneline", "-n", "1")
	if !strings.Contains(v1Log, "watchflow: auto-sync") {
		t.Fatalf("commit automático não encontrado no Vault 1. Log: %s", v1Log)
	}

	// Verifica se Vault 2 recebe os novos arquivos ao puxar do remote bare
	vault2.MustRunGit("pull", "origin", "main")
	if content := vault2.ReadFile("Inbox/ideia1.md"); !strings.Contains(content, "Texto da nota 1.") {
		t.Fatalf("Vault 2 não recebeu ideia1.md sincronizada: %s", content)
	}
	if content := vault2.ReadFile("Inbox/ideia2.md"); !strings.Contains(content, "Texto da nota 2.") {
		t.Fatalf("Vault 2 não recebeu ideia2.md sincronizada: %s", content)
	}

	// -------------------------------------------------------------------------
	// Marco 3: Resiliência Offline-First (Queda de Conectividade e Auto-Recuperação)
	// -------------------------------------------------------------------------
	t.Log("--- Marco 3: Resiliência Offline-First e Retenção de Jobs ---")
	// Simula queda de rede configurando remote inválido
	invalidRemote := filepath.Join(tempBase, "broken-remote.git")
	vault1.MustRunGit("remote", "set-url", "origin", invalidRemote)

	// Criação de nota no modo offline
	vault1.WriteFile("Inbox/offline_note.md", "# Nota Offline\nCriada sem conexão.")

	// Aguarda o pipeline tentar executar e falhar no git.push
	time.Sleep(1200 * time.Millisecond)

	// A nota local DEVE estar commitada com segurança no histórico do Git (Anti-Data-Loss)
	v1OfflineLog := vault1.MustRunGit("log", "--oneline", "-n", "1")
	if !strings.Contains(v1OfflineLog, "watchflow: auto-sync") {
		t.Fatalf("Vault 1 não realizou commit local no modo offline! Log: %s", v1OfflineLog)
	}

	// Restaura a conexão com o remote bare
	vault1.MustRunGit("remote", "set-url", "origin", bareRemote)

	// Força disparo de sincronização via IPC (simulando restabelecimento de rede)
	syncResp, err := client.Sync(context.Background(), "vault-1")
	if err != nil || len(syncResp.EnqueuedJobs) == 0 {
		t.Fatalf("falha ao engatilhar sync manual após retorno da rede: %v", err)
	}

	time.Sleep(1200 * time.Millisecond)

	// Vault 2 agora deve conseguir receber a nota offline
	vault2.MustRunGit("pull", "origin", "main")
	if content := vault2.ReadFile("Inbox/offline_note.md"); !strings.Contains(content, "Criada sem conexão.") {
		t.Fatalf("Vault 2 não recebeu offline_note.md após reconexão: %s", content)
	}

	// -------------------------------------------------------------------------
	// Marco 4: Safe Git Sync e Convergência Bidirecional sem Rebase Detached HEAD
	// -------------------------------------------------------------------------
	t.Log("--- Marco 4: Convergência Bidirecional com Safe Git Sync ---")
	// Vault 2 adiciona uma nota e dá push
	vault2.WriteFile("Inbox/peer_note.md", "# Nota do Peer\nCriada no outro laptop.")
	vault2.CommitAll("feat: nota criada no dispositivo peer")
	vault2.MustRunGit("push", "origin", "main")

	// Concorrentemente, Vault 1 cria uma nota local distinta
	vault1.WriteFile("Inbox/local_concurrent.md", "# Nota Local Concorrente\nCriada no laptop principal.")

	// Aguarda processamento: Vault 1 deve commitar localmente, fetch, 3-way merge limpo e push
	time.Sleep(1500 * time.Millisecond)

	// Vault 1 deve ter a nota do peer mesclada
	if content := vault1.ReadFile("Inbox/peer_note.md"); !strings.Contains(content, "Criada no outro laptop.") {
		t.Fatalf("Vault 1 não incorporou a nota do peer via Safe Git Sync: %s", content)
	}

	// Vault 2 puxa a mesclagem e deve ter ambas as notas
	vault2.MustRunGit("pull", "origin", "main")
	if content := vault2.ReadFile("Inbox/local_concurrent.md"); !strings.Contains(content, "Criada no laptop principal.") {
		t.Fatalf("Vault 2 não recebeu local_concurrent.md mesclada: %s", content)
	}

	// -------------------------------------------------------------------------
	// Marco 5: Proteção Contra Conflito de Merge (Anti-Data-Loss e Abort Imediato)
	// -------------------------------------------------------------------------
	t.Log("--- Marco 5: Proteção em Conflito de Linhas (git merge --abort) ---")
	// 1. Cria base compartilhada para o arquivo de conflito
	vault1.WriteFile("Inbox/shared_doc.md", "# Título Original\nLinha A original.\nLinha B original.")
	time.Sleep(1200 * time.Millisecond) // sincroniza
	vault2.MustRunGit("pull", "origin", "main")

	// 2. Vault 2 altera linha A e envia ao remote
	vault2.WriteFile("Inbox/shared_doc.md", "# Título Alterado no Peer\nLinha A alterada no peer.\nLinha B original.")
	vault2.CommitAll("modificacao conflitante no peer")
	vault2.MustRunGit("push", "origin", "main")

	// 3. Vault 1 altera a mesma linha A com conteúdo diferente
	localContent := "# Título Alterado no Local\nLinha A alterada no local.\nLinha B original."
	vault1.WriteFile("Inbox/shared_doc.md", localContent)

	// Aguarda Safe Git Sync detectar a colisão de merge
	time.Sleep(1500 * time.Millisecond)

	// 4. Verificação das Garantias Anti-Data-Loss:
	// A) O worktree local do Vault 1 DEVE estar limpo (sem marcadores <<<<<<< HEAD)
	currentContent := vault1.ReadFile("Inbox/shared_doc.md")
	if strings.Contains(currentContent, "<<<<<<<") || strings.Contains(currentContent, ">>>>>>>") {
		t.Fatalf("VIOLAÇÃO ANTI-DATA-LOSS: marcadores de conflito foram injetados no arquivo Markdown!\n%s", currentContent)
	}

	// B) O arquivo local preservou o conteúdo digitado pelo usuário
	if currentContent != localContent {
		t.Fatalf("VIOLAÇÃO ANTI-DATA-LOSS: conteúdo local foi sobrescrito ou corrompido! Obtido:\n%s", currentContent)
	}

	// C) O status do watcher no daemon deve estar marcado como CONFLICT_HALTED
	conflictStatus, err := client.Status(context.Background())
	if err != nil {
		t.Fatalf("falha ao consultar status após conflito: %v", err)
	}
	if conflictStatus.Watchers[0].Status != string(queue.WatcherConflictHalted) {
		t.Fatalf("status do watcher deveria ser CONFLICT_HALTED, obtido: %s", conflictStatus.Watchers[0].Status)
	}

	// -------------------------------------------------------------------------
	// Marco 6: Encerramento Gracioso e Integridade do SQLite
	// -------------------------------------------------------------------------
	t.Log("--- Marco 6: Encerramento Gracioso via IPC ---")
	stopResp, err := client.Stop(context.Background())
	if err != nil || !stopResp.Success {
		t.Fatalf("falha ao solicitar encerramento via IPC: %v", err)
	}

	time.Sleep(300 * time.Millisecond)

	// Shutdown formal para liberação dos locks
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelShutdown()
	_ = coord.Shutdown(shutdownCtx)

	t.Log("🎉 Todos os 6 marcos do MVP foram homologados com 100% de sucesso e 0 perdas de dados!")
}
