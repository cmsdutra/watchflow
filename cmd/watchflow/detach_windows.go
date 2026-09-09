//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
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

// spawnDetached relança este mesmo binário em segundo plano, sem console e sem
// vínculo com o terminal que o chamou.
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
