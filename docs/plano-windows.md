# Plano de compatibilização com Windows

> **Status:** não iniciado. Levantamento feito em 2026-09-05 a partir de
> `eb884d7`, em uma máquina Linux, por inspeção de código e compilação cruzada.
> Nada foi testado em Windows ainda.

O WatchFlow hoje declara suporte apenas a Linux. Este documento planeja a
compatibilização com Windows, na ordem em que as etapas se destravam.

---

## Ponto de partida

Verificações executadas no levantamento, não suposições:

| Verificação | Resultado |
|---|---|
| `GOOS=windows GOARCH=amd64 go build ./...` | passa |
| `GOOS=windows go vet ./...` | passa |
| cgo | não usado — `modernc.org/sqlite` é Go puro |
| build tags de plataforma | nenhuma no projeto |
| `internal/locking` | `sync.Mutex` em memória, sem `flock` |
| `internal/normalizer` | já usa `filepath.ToSlash` em todos os caminhos |
| rotação de log (`internal/logger/rotate.go`) | fecha o arquivo antes do `os.Rename` — ordem correta para Windows |
| socket padrão (`internal/config/config.go`) | degrada para `state_dir` quando `/run/user/N` não existe |

**Não há barreira de compilação.** O trabalho é comportamental: código que
compila e roda, mas faz a coisa errada. Por isso o plano é uma sequência de
verificações com correções pontuais, e não uma porta para outra plataforma.

---

## Fase 0 — Medir antes de mexer

Na máquina Windows, antes de escrever qualquer código. O objetivo é substituir
previsão por fato.

```powershell
go build ./...
go test ./... 2>&1 | Tee-Object -FilePath windows-baseline.txt
```

Dez arquivos de teste abrem socket Unix e dez fazem suposições de Unix (`/tmp`,
`chmod`, `0755`). Espere falhas; o que importa é **quais** e por quê.

Em seguida, com um cofre pequeno — não o cofre real:

```powershell
.\watchflow.exe doctor
.\watchflow.exe start -f
# em outro terminal:
.\watchflow.exe status
```

O `status` é o teste decisivo da Fase 1.

**Critério de saída:** existe a lista real de falhas.

---

## Fase 1 — IPC

Maior risco, e decide o formato do resto do trabalho.

`internal/ipc` usa `net.Listen("unix", …)` e `net.Dial("unix", …)`. O Windows 10
1803+ tem `AF_UNIX`, mas o suporte do Go a sockets Unix no Windows tem histórico
irregular e **precisa ser verificado na máquina** — o `watchflow status` da Fase
0 responde em segundos.

**Se funcionar:**

- Atenção ao limite de ~108 bytes no caminho do socket; os `t.TempDir()` do
  Windows estouram com facilidade.
- `os.Chmod(sock, 0600)` (`internal/ipc/server.go:84`) é no-op: o socket fica sem
  restrição de acesso.

**Se não funcionar:** named pipes (`\\.\pipe\watchflow`) via
`github.com/Microsoft/go-winio`. O isolamento necessário já existe — bastam
`listener_unix.go` e `listener_windows.go` com build tags, sem tocar no
protocolo nem nas estruturas `Server`/`Client`. Named pipe exige descritor de
segurança explícito para restringir ao usuário; isso não vem de graça.

---

## Fase 2 — Ciclo de vida do processo

`syscall.SIGTERM` (`cmd/watchflow/start.go:85`) compila no Windows e **nunca é
entregue**. Só `os.Interrupt` chega, e apenas em console. É a mesma classe de
problema que derrubou o daemon no Linux quando o terminal foi fechado: o
encerramento gracioso depende de um sinal que a plataforma não manda.

Duas opções:

1. **Serviço Windows real** — `golang.org/x/sys/windows/svc`, com o SCM
   entregando `SERVICE_CONTROL_STOP`. Correto e integrado, mas exige caminho de
   código separado no `start`.
2. **Agendador de Tarefas no logon** — zero código, e já resolve o problema de
   sobreviver ao fechamento do terminal.

Recomendação: começar por (2) e migrar para (1) se houver necessidade real.

Em ambos os casos, o check `[Serviço]` do `doctor` precisa de equivalente — hoje
ele retorna INFO em não-Linux (`cmd/watchflow/doctor.go`).

---

## Fase 3 — Git

A fase mais específica do produto, e a única que pode corromper cofres em
silêncio. **Não sincronizar nenhum cofre real antes de fechá-la.**

### Fim de linha (CRLF/LF) — risco número um

O Git for Windows usa `core.autocrlf=true` por padrão. Num cofre compartilhado
com Linux, o checkout reescreve todo arquivo de texto com CRLF; o watcher vê o
cofre inteiro modificado, commita, e a máquina Linux recebe o inverso. O
resultado é um pingue-pongue de commits gigantes e conflito garantido — e, por
ser automático, sem ninguém perceber a tempo.

Mitigação, no `.gitattributes` versionado de cada cofre:

```gitattributes
* text=auto eol=lf
```

mais `git config --local core.autocrlf false` na máquina Windows. Candidato a
check do `doctor`.

### O merge driver `theirs` quebra

A definição usada no Linux é `cp %B %A`, e **`cp` não existe no `cmd.exe`**. Na
máquina Windows precisa ser `copy /Y "%B" "%A"`, ou apontar para o `cp` do
ambiente MSYS que acompanha o Git for Windows.

Como a definição do driver mora em `git config --local` e não viaja no clone,
essa diferença por máquina cabe no modelo já adotado (ver
[README, "Arquivos que conflitam sempre"](../README.md)). Note que o check de
merge drivers do `doctor` verifica apenas que o driver **existe**, não que o
comando dele funciona naquela plataforma.

### Outros dois

- Confirmar que o Git for Windows respeita `LANG=C` / `LC_ALL=C`
  (`internal/providers/git/git.go`), de que depende o parsing das mensagens.
- Validar push não-interativo com o Git Credential Manager, no lugar do keyring.

---

## Fase 4 — Caminhos e convenções

- **`os.Getuid()` retorna -1 no Windows.** `${UID}` vira `"-1"` no `ExpandPath`
  (`internal/config/config.go`). O fallback salva na prática, mas é um valor sem
  sentido esperando para confundir.
- **`~/.config` e `~/.local/state` funcionam, mas não são idiomáticos.** Decidir
  entre manter (o mesmo `config.yaml` serve nas duas plataformas) ou adotar
  `%APPDATA%` / `%LOCALAPPDATA%`. Manter e documentar é o caminho mais simples.
- **`0700` / `0600` são no-op.** O `state_dir` guarda log e banco; no Windows
  ficam com a ACL herdada. Decidir entre aplicar ACL ou documentar a diferença.
- **`MAX_PATH` de 260 caracteres.** Cofres com hierarquia profunda estouram.
  Verificar `LongPathsEnabled` no registro — bom candidato a check do `doctor`,
  no lugar do de inotify.
- **Sistema de arquivos case-insensitive.** Arquivos vindos do Linux que diferem
  só em maiúsculas colidem no checkout.

---

## Fase 5 — Watcher e escala

`WalkAndAdd` (`internal/watcher/tree.go`) registra diretório a diretório — um
cofre real pode ter mais de mil. No Windows cada `Add` do fsnotify vira um handle
de `ReadDirectoryChangesW`, com risco de estouro de buffer
(`ERROR_NOTIFY_ENUM_DIR`) sob rajada de escrita.

**Não otimizar antes de medir.** Começar por um cofre pequeno; só se doer,
avaliar watch recursivo nativo — o `ReadDirectoryChangesW` suporta, mas o
fsnotify não expõe.

Somar a isso o Windows Defender: varredura em tempo real sobre pasta com escrita
intensa custa caro e gera eventos. Uma exclusão para o cofre é candidata a
recomendação no `doctor`.

---

## Fase 6 — Distribuição e CI

- `scripts/install.sh` → `scripts/install.ps1`, mantendo a idempotência (não
  sobrescrever configuração existente).
- CI: adicionar `GOOS=windows go build` como gate barato desde já; um job
  `windows-latest` rodando a suíte depois que a Fase 0 disser o que esperar.

---

## Ordem e critério de parada

**0 → 1 → 2** entregam um daemon que sobe, sobrevive ao logout e responde a
comandos.

**3** é o que impede corromper cofres.

**4, 5, 6** são polimento e podem ser incrementais.

Marco intermediário recomendado:

> WatchFlow roda no Windows sincronizando um cofre de teste, **com a máquina
> Linux desligada**.

Isso valida as fases 0 a 3 sem arriscar cofres reais. Só ligar as duas máquinas
ao mesmo tempo depois que a Fase 3 estiver fechada — caso contrário o primeiro
checkout com CRLF vira um commit de todos os arquivos do cofre.
