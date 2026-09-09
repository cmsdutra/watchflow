//go:build !windows

package main

import "fmt"

// spawnDetached não existe fora do Windows: no Linux o desacoplamento do
// terminal é responsabilidade do systemd --user, que já sabe reiniciar o daemon,
// coletar o log e respeitar o encerramento gracioso. Duplicar isso no binário
// daria um segundo mecanismo de ciclo de vida sem nada a mais.
func spawnDetached(_ []string) (int, error) {
	return 0, fmt.Errorf(
		"--detach é específico do Windows; neste sistema use o serviço do systemd: " +
			"systemctl --user enable --now watchflow")
}
