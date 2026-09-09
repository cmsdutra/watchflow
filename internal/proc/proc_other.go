//go:build !windows

package proc

import "os/exec"

// HideConsole não faz nada fora do Windows: em sistemas Unix um processo filho
// não abre janela alguma. A função existe para que os pontos de criação de
// processo fiquem idênticos nas duas plataformas, sem build tag espalhada pelo
// código de chamada.
func HideConsole(_ *exec.Cmd) {}
