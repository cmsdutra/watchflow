//go:build windows

package proc

import (
	"os/exec"
	"syscall"
)

// createNoWindow impede que o Windows aloque um console para o processo filho.
const createNoWindow = 0x08000000

// HideConsole configura o comando para não abrir janela de console.
//
// Importa porque o daemon roda destacado, sem console próprio (ver
// cmd/watchflow/detach_windows.go). Um processo de console iniciado por um pai
// sem console ganha um console NOVO — e, como o daemon executa git várias vezes
// por ciclo, o usuário vê janelas piscando na tela a cada sincronização.
//
// Herdar o console do pai não é opção aqui: é justamente o que prendia o daemon
// ao terminal. A saída dos comandos já é capturada em buffers e vai para o log
// estruturado, então nenhum console é necessário em momento algum.
func HideConsole(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
	cmd.SysProcAttr.CreationFlags |= createNoWindow
}
