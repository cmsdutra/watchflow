//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// Flags de criação de processo do Windows. Não estão expostas por constante em
// syscall, então vão explícitas aqui.
const (
	// detachedProcess desliga o filho do console do pai. Sem isso, fechar a
	// janela que iniciou o daemon envia CTRL_CLOSE_EVENT e o mata junto.
	detachedProcess = 0x00000008
	// createNewProcessGroup isola o filho dos sinais Ctrl+C/Ctrl+Break do grupo
	// de quem o iniciou.
	createNewProcessGroup = 0x00000200
	// createNoWindow evita que o Windows aloque um console novo para o filho,
	// que é um binário do subsistema de console.
	createNoWindow = 0x08000000
)

// daemonBinaryFor traduz o caminho do lançador no caminho do daemon.
//
// A distribuição no Windows tem dois executáveis a partir do mesmo código:
// 'watchflow.exe', de subsistema de console, que é a CLI; e 'watchfloww.exe',
// de subsistema GUI, cujo único papel é ser o alvo da tarefa do Agendador. O
// sufixo 'w' segue a convenção antiga do Windows — 'pythonw.exe', 'javaw.exe'.
//
// A razão é que o Agendador, ao executar um binário de console numa sessão
// interativa, aloca um console para ele. Como a ação da tarefa repete a cada
// poucos minutos para supervisionar o daemon, isso vira uma janela piscando na
// tela do usuário sem parar. Um binário GUI nunca recebe console.
//
// O daemon em si continua sendo o de console: ele é iniciado com CREATE_NO_WINDOW
// e portanto também não abre janela, e mantém stdout/stderr válidos — o que o
// GUI não tem, e de que o subsistema de log precisa.
func daemonBinaryFor(launcher string) string {
	dir := filepath.Dir(launcher)
	name := filepath.Base(launcher)

	if strings.HasSuffix(strings.ToLower(name), "w.exe") {
		name = name[:len(name)-len("w.exe")] + ".exe"
		candidate := filepath.Join(dir, name)
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}

	// Sem par de console instalado ao lado, relançar a si mesmo continua sendo
	// melhor do que falhar: o daemon roda, apenas sem stdout/stderr.
	return launcher
}

// spawnDetached relança o daemon em segundo plano, sem console e sem vínculo
// com o terminal que o chamou.
//
// O Windows não tem equivalente ao fork/setsid do Unix, e um app de console
// iniciado pelo Agendador de Tarefas em sessão interativa ganha uma janela de
// console de verdade — que o usuário fecha sem saber que está derrubando o
// daemon. As três flags acima, juntas, são o que resolve: sem console herdado,
// sem console novo e fora do grupo de sinais do pai.
func spawnDetached(args []string) (int, error) {
	exePath, err := os.Executable()
	if err != nil {
		return 0, fmt.Errorf("não foi possível determinar o caminho do próprio binário: %w", err)
	}
	exePath = daemonBinaryFor(exePath)

	cmd := exec.Command(exePath, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: detachedProcess | createNewProcessGroup | createNoWindow,
	}

	// Nada de herdar stdio: o filho registra tudo no log estruturado do
	// state_dir, e herdar os handles do pai o prenderia ao terminal de novo.
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil

	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("falha ao iniciar o processo em segundo plano: %w", err)
	}

	pid := cmd.Process.Pid
	// Release desvincula o filho: sem isso o Go manteria um handle esperando
	// por um Wait que nunca vem.
	if err := cmd.Process.Release(); err != nil {
		return pid, fmt.Errorf("processo iniciado (PID %d), mas houve falha ao desvinculá-lo: %w", pid, err)
	}

	return pid, nil
}
