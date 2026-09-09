# WatchFlow

Sincronização automática de pastas via Git, sem plugins e sem intervenção manual.

O WatchFlow é um pequeno programa que roda em segundo plano no seu computador,
observa uma pasta e, sempre que algo muda, faz commit e sincroniza com um
repositório Git remoto. Você trabalha normalmente nos seus arquivos; ele cuida
do resto.

O caso de uso original é manter um cofre do Obsidian sincronizado entre várias
máquinas, mas o programa não sabe nem se importa com qual aplicativo alterou os
arquivos — funciona igualmente com um editor de texto, um script ou o próprio
gerenciador de arquivos.

> **Estado do projeto:** funcional e testado, mas ainda em desenvolvimento
> inicial. Roda em Linux e em Windows 10 1803+. Veja
> [Limitações conhecidas](#limitações-conhecidas).

---

## Sumário

- [Por que isso existe](#por-que-isso-existe)
- [Como funciona](#como-funciona)
- [O que ele garante](#o-que-ele-garante)
- [Requisitos](#requisitos)
- [Instalação](#instalação)
- [Configuração](#configuração)
- [Uso diário](#uso-diário)
- [Rodando como serviço](#rodando-como-serviço)
- [Quando dá problema](#quando-dá-problema)
- [Limitações conhecidas](#limitações-conhecidas)
- [Desenvolvimento](#desenvolvimento)
- [Licença](#licença)

---

## Por que isso existe

Manter anotações sincronizadas entre computadores usando Git funciona bem, até
que você precisa lembrar de fazer commit. Aí você esquece, edita no outro
computador, e quando lembra tem duas versões divergentes para resolver na mão.

As soluções existentes costumam ter um dos dois problemas:

- **Plugins de editor** só funcionam dentro daquele editor. Se você editar um
  arquivo por fora, nada acontece.
- **Sincronizadores genéricos** (Dropbox, Syncthing) resolvem o transporte, mas
  não te dão histórico de versões nem controle sobre conflitos.

O WatchFlow fica entre os dois: reage a mudanças no sistema de arquivos, em
qualquer aplicativo, e usa Git para versionar. E, principalmente, ele foi feito
com uma regra acima de todas as outras: **nunca perder o seu trabalho**.

---

## Como funciona

O daemon fica parado até algo acontecer. Três coisas podem acioná-lo:

| Gatilho | Quando |
|---|---|
| Você edita um arquivo | ~15s depois (agrupando edições em rajada) |
| Relógio interno | a cada 5 minutos |
| O daemon acabou de subir | ~3 segundos depois |

Nos três casos ele executa o mesmo ciclo:

```
Gatilho
   ↓
Filtros descartam ruído (.git/, arquivos temporários)
   ↓
Grava a intenção de sincronizar no banco local (SQLite)
   ↓
git add → git commit → traz do remoto → git push
```

Três detalhes que valem entender:

**A espera de 15 segundos** existe para não gerar um commit a cada arquivo
salvo. Se você salvar cinco arquivos em dez segundos, tudo vira um commit só. Há
um teto (60s por padrão) para que edições contínuas não adiem a sincronização
indefinidamente.

**A verificação periódica** existe porque tudo o mais parte de eventos locais.
Sem ela, o que outra máquina publicasse só chegaria quando você editasse algo —
e você poderia acabar editando em cima de uma versão desatualizada. Ela é
controlada por `pull_interval` e pode ser desligada.

**A intenção é gravada em disco antes de qualquer coisa acontecer.** Se faltar
energia no meio de um `git push`, o trabalho pendente não se perde: na próxima
vez que o programa subir, ele retoma de onde parou.

---

## O que ele garante

Estas não são promessas de marketing; são as regras que guiam o código.

### Ele funciona nos dois sentidos

Além de enviar o que você muda, ele traz o que as outras máquinas publicaram.

Isso acontece em três momentos: quando você edita qualquer arquivo local, a cada
`pull_interval` (5 minutos por padrão), e alguns segundos depois de o daemon
subir — porque o instante em que a máquina acaba de ligar é justamente aquele em
que ela está mais desatualizada.

Para forçar a qualquer momento: `watchflow sync`, ou a tecla `s` na TUI.

### Seus arquivos nunca são sobrescritos

O WatchFlow **não usa `git pull`**. Em vez disso, ele busca as mudanças do
servidor, compara com o que você tem localmente e decide:

| Situação | O que ele faz |
|---|---|
| Só você mudou coisas | Envia (`push`) |
| Só o servidor mudou | Traz as mudanças (`fast-forward`) |
| Ambos mudaram, em arquivos ou linhas diferentes | Junta automaticamente (`merge`) |
| Ambos mudaram **a mesma linha** | **Desfaz tudo e para**, avisando você |

No último caso, ele executa `git merge --abort` imediatamente. Sua pasta volta
exatamente ao estado anterior, sem aqueles marcadores `<<<<<<< HEAD` no meio do
texto. O programa então para de sincronizar aquela pasta e te notifica — porque
juntar duas versões conflitantes é uma decisão sua, não dele.

### Ele nunca força nada

Não existe `git push --force` em lugar nenhum do código. Se o envio for
rejeitado, ele tenta novamente depois de sincronizar.

### Ele respeita outros programas

Se o Obsidian, o VS Code ou você estiverem no meio de uma operação Git, o Git
cria um arquivo de trava. O WatchFlow **espera** essa trava sair (até 10s) e
nunca a remove. Se ela persistir, ele adia e tenta mais tarde.

### Funciona offline

Sem internet, ele continua fazendo commits locais normalmente. As tentativas de
envio são reagendadas com espera progressiva. Quando a rede voltar, tudo sobe.

### Suas credenciais não vazam nos logs

Se você usa uma URL com token embutido, ele é removido de tudo que for gravado
em log ou no banco de dados.

---

## Requisitos

- **Linux** com kernel que suporte `inotify` (qualquer distribuição moderna),
  ou **Windows 10 1803+** / Windows 11 — a versão mínima é a que introduziu o
  suporte a sockets Unix, que o daemon usa para falar com a CLI
- **Git** instalado (versão 2.30 ou mais recente é o recomendado). No Windows,
  o [Git for Windows](https://git-scm.com/download/win)
- Uma pasta que já seja um repositório Git com um remoto configurado

Para compilar a partir do código-fonte, você também precisa do **Go 1.24+**.

O programa é um binário único, sem dependências de runtime. Não precisa de
Docker, banco de dados externo nem nada rodando em segundo plano além dele
mesmo. Em repouso consome poucos megabytes de memória.

---

## Instalação

### Instalação assistida (recomendado)

```bash
git clone https://github.com/cmsdutra/watchflow.git
cd watchflow
./install.sh
```

O script compila o binário, instala em `~/.local/bin`, cria a configuração
inicial a partir do exemplo comentado e registra o serviço no systemd. Ele
avisa se `~/.local/bin` não estiver no seu `PATH`.

**Ele não instala dependências com `sudo`.** Se faltar o Git ou o Go, ele
mostra o comando exato para a sua distribuição e para de forma clara. No caso
do Go, ele também confere a *versão* — o Go dos repositórios costuma ser antigo
demais para compilar o projeto, então a orientação aponta para o tarball
oficial em vez do pacote da distro.

Rodar de novo mais tarde atualiza o binário sem tocar na sua configuração nem
no estado do daemon — é assim que você atualiza para uma versão nova.

Opções:

| Flag | Efeito |
|---|---|
| `--prefix DIR` | Instala o binário em outro diretório |
| `--no-service` | Não registra a unit do systemd |
| `--no-build` | Usa o binário já existente em `bin/` |
| `--yes` | Não faz perguntas |

(`./install.sh` na raiz é um atalho para `scripts/install.sh`; os dois são o
mesmo script.)

#### No Windows

```powershell
git clone https://github.com/cmsdutra/watchflow.git
cd watchflow
powershell -ExecutionPolicy Bypass -File scripts\install.ps1
```

O `install.ps1` faz o mesmo que o `install.sh`, com as mesmas garantias — é
idempotente, nunca sobrescreve configuração existente e não pede privilégio de
administrador em momento algum. As diferenças são as da plataforma: instala em
`%LOCALAPPDATA%\Programs\WatchFlow`, altera só o `Path` de `HKCU` e, no lugar da
unit do systemd, registra uma tarefa por-usuário no Agendador de Tarefas que
sobe o daemon no logon. As flags são `-Prefix`, `-NoService`, `-NoBuild` e
`-Yes`.

A configuração fica em `~/.config/watchflow/config.yaml` nas duas plataformas,
de propósito: o mesmo arquivo serve nas duas.

Para iniciar o daemon à mão no Windows, use `watchflow start --detach`. Sem essa
flag o processo fica preso ao terminal — e o `watchflow.exe` é um binário de
console, então fechar a janela o mata. Com ela, o daemon sobe sem console
nenhum e sobrevive ao fechamento do terminal. É o que a tarefa do Agendador usa.
No Linux esse papel é do systemd, e a flag não existe lá.

O `--prefix` que você usar fica registrado no desinstalador, então não precisa
repeti-lo na hora de remover.

### Instalação manual

Se preferir controlar cada passo:

```bash
make build
mkdir -p ~/.local/bin
cp bin/watchflow ~/.local/bin/
```

Confirme que funcionou:

```bash
watchflow version
```

### Desinstalação

O instalador copia o desinstalador para junto do binário, então ele funciona de
qualquer diretório mesmo que você já tenha apagado este repositório:

```bash
watchflow-uninstall
```

No Windows, do mesmo jeito:

```powershell
& "$env:LOCALAPPDATA\Programs\WatchFlow\watchflow-uninstall.ps1"
```

Se ainda tiver o código-fonte, o equivalente é:

```bash
./scripts/uninstall.sh                                    # Linux
powershell -ExecutionPolicy Bypass -File scripts\uninstall.ps1   # Windows
```

Para o serviço, remove a unit (ou a tarefa do Agendador, no Windows) e apaga o
binário. **Sua configuração e o histórico do daemon são preservados**, de modo
que reinstalar depois devolve tudo como estava. Use `--purge` — `-Purge` no
Windows — para removê-los também.

A versão Windows também remove do seu `PATH` a entrada que o instalador
adicionou, já que lá ela foi escrita no registro em vez de sugerida num arquivo
de shell.

As pastas que você sincronizava e os repositórios Git dentro delas nunca são
tocados, com ou sem `--purge`.

### Verificando o ambiente

Antes de configurar, rode o diagnóstico:

```bash
watchflow doctor
```

Ele verifica se o Git está instalado, se os limites do kernel para monitoramento
de arquivos são suficientes para o tamanho da sua pasta, se os diretórios de
trabalho estão acessíveis e se o serviço systemd está saudável. Se algo estiver
errado, ele diz o que fazer.

Duas checagens merecem atenção porque cobrem falhas que **não aparecem no log**:

- **Serviço.** Um daemon iniciado à mão no terminal morre junto com a janela, sem
  deixar registro. Se o `doctor` avisar que a unidade não está instalada, ou que
  está em loop de reinício, é esse o problema — e o log não vai te contar.
- **Merge drivers.** Se o `.gitattributes` do cofre nomeia um driver que este
  clone não define, o `doctor` avisa antes do primeiro conflito. Veja
  [Arquivos que conflitam sempre](#arquivos-que-conflitam-sempre).

O problema mais comum é o limite de arquivos monitorados. Se o `doctor`
reclamar disso:

```bash
echo "fs.inotify.max_user_watches=524288" | sudo tee -a /etc/sysctl.conf
sudo sysctl -p
```

---

## Configuração

A configuração fica em `~/.config/watchflow/config.yaml` e é feita **editando o
arquivo YAML** — não há assistente interativo nem comandos para adicionar
pastas pela linha de comando. O script de instalação cria o arquivo a partir de
[`configs/watchflow.example.yaml`](configs/watchflow.example.yaml), que é
inteiramente comentado; a partir daí é só ajustar.

O mínimo para sincronizar uma pasta são três linhas:

```yaml
watchers:
  - name: meu-cofre
    path: ~/Anotacoes
```

Todo o resto tem padrão: janela de agrupamento de 15s (teto de 60s), 5
tentativas, notificação de erro e conflito, e o pipeline de sincronização Git
(`check_locks` → `add` → `commit` → `safe_sync` → `push`). Só declare o que
quiser mudar.

O arquivo completo, com tudo explícito:

```yaml
version: 1

daemon:
  state_dir: "~/.local/state/watchflow"
  socket_path: "/run/user/${UID}/watchflow.sock"
  log_level: "info"
  max_concurrent_pipelines: 2

notifications:
  enabled: true
  on_success: false    # não avisa em sincronizações normais
  on_error: true
  on_conflict: true    # sempre avisa quando precisa da sua atenção
  backend: "desktop"   # desktop, log ou webhook

watchers:
  - name: "meu-cofre"
    path: "~/Anotacoes"
    debounce: "15s"        # espera de silêncio antes de agir
    max_wait: "60s"        # teto máximo de espera
    pull_interval: "5m"    # consulta o remoto mesmo sem alterações locais ("0" desativa)
    ignore:
      - ".git/**"
      - ".obsidian/cache/**"
      - "**/*.tmp"
    pipelines:
      - "sincronizar"

pipelines:
  sincronizar:
    timeout: "120s"
    max_retries: 5
    steps:
      - action: "git.check_locks"
      - action: "git.add"
      - action: "git.commit"
        params:
          message: "watchflow: auto-sync {timestamp}"
      - action: "git.safe_sync"
      - action: "git.push"
```

Dois campos merecem explicação, porque o nome sugere mais do que fazem:

**`ignore` filtra eventos, não o Git.** Ele decide o que **dispara** uma
sincronização — um arquivo listado ali não acorda o daemon. Mas se esse arquivo
já é rastreado pelo Git, ele continua entrando nos commits normalmente, porque
quem monta o commit é o `git add` do pipeline, não o watcher. Para excluir um
arquivo do versionamento, use `.gitignore` (e `git rm --cached`, se ele já
estiver rastreado).

**`pull_interval` domina a latência de espelhamento.** Uma edição na máquina A
chega na máquina B em dois saltos: publicar (≈ `debounce` + alguns segundos de
pipeline) e a outra máquina descobrir (em média *metade* do `pull_interval`). Com
os padrões, são ~17s no primeiro salto e ~2min30s no segundo — ou seja, baixar o
`debounce` quase não muda nada, e baixar o `pull_interval` muda quase tudo. O
mínimo aceito é 30s; cada consulta é uma ida à rede, então valores agressivos
custam tráfego em cofres grandes.

Depois de editar, valide antes de rodar:

```bash
watchflow config validate
```

A validação confere a sintaxe, se as pastas existem, se os nomes de ações são
válidos e se os parâmetros de cada passo fazem sentido. Um erro de digitação é
apontado ali, e não silenciosamente ignorado.

Ela também avisa (sem reprovar) quando uma pasta vigiada não é um repositório
Git, ou é mas não tem remoto configurado — as duas causas mais comuns de "o
daemon está rodando e nada acontece". São avisos, e não erros, para que uma
pasta mal configurada não impeça as outras de funcionar.

### Aplicando as mudanças

```bash
watchflow reload
```

Adiciona, remove e reinicia pastas individualmente, sem derrubar o daemon nem
interromper as que não mudaram. Se a configuração estiver inválida, nada é
alterado e o daemon segue com a anterior.

Mudanças na seção `daemon` (socket, diretório de estado, concorrência, nível de
log) exigem reiniciar de fato — o `reload` avisa quando for o caso:

```
systemctl --user restart watchflow
```

### Ações disponíveis nos pipelines

| Ação | O que faz |
|---|---|
| `git.check_locks` | Espera travas de outros programas saírem |
| `git.add` | Adiciona as mudanças ao próximo commit |
| `git.commit` | Cria o commit (nunca cria commits vazios) |
| `git.safe_sync` | Sincroniza com o servidor de forma segura |
| `git.push` | Envia os commits |

---

## Uso diário

Na prática, depois de configurado, você não interage com o WatchFlow. Mas os
comandos existem quando você precisa.

### Iniciar e parar

```bash
watchflow start     # inicia (fica ocupando o terminal)
watchflow stop      # encerra de forma limpa
```

O encerramento é ordenado: ele para de captar mudanças novas, deixa terminar o
que estava em andamento e devolve para a fila o que sobrou.

### Ver o que está acontecendo

```bash
watchflow status    # tabela de saúde das pastas monitoradas
watchflow jobs      # fila de trabalho pendente
watchflow runs      # histórico do que já rodou
watchflow logs -f   # acompanha os eventos em tempo real
```

Todos aceitam `--json`, caso você queira usar em scripts.

### Painel interativo

```bash
watchflow tui
```

Abre um painel no terminal com tudo junto: estado das pastas, fila e eventos
recentes, atualizando sozinho. Dentro dele:

| Tecla | Ação |
|---|---|
| `s` | Sincroniza a pasta selecionada agora |
| `p` | Pausa ou retoma a pasta selecionada |
| `r` | Atualiza a tela |
| `Tab` | Muda o painel em foco |
| `↑` `↓` | Navega entre as pastas |
| `q` | Sai |

Fechar o painel **não** para a sincronização — ele é só uma janela para o que
está acontecendo.

### Controles pontuais

```bash
watchflow reload                  # relê a configuração sem reiniciar
watchflow sync                    # sincroniza tudo agora, sem esperar
watchflow sync meu-cofre          # sincroniza só uma pasta
watchflow pause meu-cofre         # congela temporariamente
watchflow resume meu-cofre        # retoma
```

Pausar é seguro: as mudanças continuam sendo registradas na fila e são
processadas quando você retomar. Nada é descartado.

---

## Rodando como serviço

Se você usou `./install.sh`, isso já foi feito — o script instala a unit do
systemd. Para habilitar:

```bash
systemctl --user enable --now watchflow
```

Instalação manual da unit, se preferir:

```bash
mkdir -p ~/.config/systemd/user
cp deploy/systemd/watchflow.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now watchflow
```

Verificando:

```bash
systemctl --user status watchflow
journalctl --user -u watchflow -f
```

### Login ou boot da máquina?

Por padrão, **no seu login** — não no boot. Se a máquina liga mas ninguém entra
na sessão, o WatchFlow não sobe.

Para que ele rode desde o boot, independentemente de haver alguém logado:

```bash
sudo loginctl enable-linger $USER
```

Essa é a diferença entre "sincroniza quando eu uso o computador" e "sincroniza
sempre que ele estiver ligado". Para um computador pessoal, o padrão costuma
bastar; para uma máquina que fica ligada servindo de ponto central, use o
`enable-linger`.

---

## Quando dá problema

### "Conflito detectado, sincronização parada"

Aconteceu o que era esperado: você e outra máquina editaram a mesma linha do
mesmo arquivo. Sua pasta local está **intacta** — o WatchFlow desfez a tentativa
de junção antes de tocar em qualquer coisa.

Para resolver:

```bash
cd ~/Anotacoes
git fetch origin
git merge origin/main    # resolva os conflitos como preferir
git commit
watchflow resume meu-cofre
```

O `resume` é a sua confirmação de que está tudo certo. Até você rodar esse
comando, o WatchFlow não mexe nessa pasta.

### Arquivos que conflitam sempre

Alguns arquivos mudam sozinhos, de forma diferente em cada máquina, e não têm
como ser mesclados linha a linha. O caso clássico é o estado de interface do
Obsidian (`.obsidian/workspace.json`): ele guarda layout de painéis e último
arquivo aberto, muda a cada clique, e duas máquinas sempre divergem nele. Se ele
for versionado, o conflito é questão de tempo, não de azar.

Há dois caminhos. O primeiro é parar de versioná-lo, com `.gitignore` mais
`git rm --cached` — simples, mas você perde a sincronização daquele estado.

O segundo mantém a sincronização e resolve o empate por regra fixa, com um merge
driver que sempre adota a versão remota:

```bash
# no repositório, uma vez por máquina
git config --local merge.theirs.name "sempre usa a versao remota"
git config --local merge.theirs.driver "cp %B %A"
```

```gitattributes
# .gitattributes, versionado junto com o cofre
.obsidian/workspace.json merge=theirs
.obsidian/appearance.json merge=theirs
```

A definição é a mesma no Windows: o Git for Windows executa merge drivers pelo
`sh` do MSYS que acompanha a instalação, não pelo `cmd.exe`, então `cp %B %A`
funciona sem alteração. **Não troque por `copy /Y`** — ele falha com
`copy: command not found`, e a variante `cmd /c copy /Y` é pior: termina com
sucesso aparente, sem marcador de conflito, mas sem adotar a versão remota.

**Atenção ao detalhe que causa surpresa:** o `.gitattributes` é versionado e
chega junto com o clone, mas a definição do driver mora no `git config --local`
e **não viaja**. Num clone novo só a primeira metade existe, e o Git não avisa —
ele volta em silêncio ao merge de texto padrão e produz exatamente o conflito
que a configuração deveria evitar. Cada máquina precisa rodar o `git config` uma
vez. O `watchflow doctor` verifica isso.

### Não está sincronizando

Nesta ordem:

```bash
watchflow doctor        # o ambiente está sadio?
watchflow status        # a pasta está HEALTHY, PAUSED ou parada?
watchflow jobs          # tem trabalho preso na fila?
watchflow logs --level warn -n 50
```

Antes de concluir que algo está errado, confira a expectativa de tempo: uma
alteração **sua** leva ~15s para virar commit, e uma alteração vinda de **outra
máquina** pode levar até 5 minutos para aparecer (ou ~3s se o daemon acabou de
subir). Para não esperar:

```bash
watchflow sync
```

Os estados possíveis de uma pasta:

| Estado | Significado |
|---|---|
| `HEALTHY` | Tudo normal |
| `DEGRADED` | Falhas temporárias, tentando de novo (rede fora, por exemplo) |
| `PAUSED` | Você pausou |
| `CONFLICT_HALTED` | Parado esperando você resolver um conflito |

### Erros ficam guardados onde?

Em `~/.local/state/watchflow/watchflow.log`, no formato JSON, com rotação
automática. O comando `watchflow logs` lê esse arquivo — e funciona mesmo com o
programa parado, o que ajuda a investigar por que ele parou.

---

## Limitações conhecidas

Vale saber antes de adotar:

- **Linux e Windows; macOS não foi avaliado.** No Windows a suíte roda verde e o
  ciclo completo foi verificado numa máquina real (Windows 11, Git for Windows
  2.53), mas o uso tem menos quilometragem que no Linux. Duas coisas ficaram sem
  exercício: um push cuja credencial do Git Credential Manager esteja expirada, e
  o cofre sincronizado com as duas máquinas ligadas ao mesmo tempo.
- **No Windows, o caminho do socket precisa ser curto.** O endereço de um socket
  Unix cabe em 108 bytes, e os caminhos temporários do Windows estouram isso com
  facilidade. O `watchflow doctor` mede e avisa antes que o daemon falhe.
- **Arquivos que diferem só em maiúsculas não coexistem no Windows.** Um cofre
  vindo do Linux com `Nota.md` e `nota.md` traz só um dos dois para a árvore de
  trabalho, e o outro aparece como modificado para sempre. O histórico não corre
  risco — o Git se recusa a estagiar o caminho colidido —, e o `doctor` aponta os
  pares. Resolver exige renomear num sistema de arquivos sensível a maiúsculas.
- **No Windows, `make test` precisa de um compilador C.** O alvo usa `-race`, que
  exige cgo. Sem ele, use `go test ./...` — o projeto é Go puro e a suíte roda
  normalmente.
- **Num console legado do Windows, a saída decorada pode sair truncada.** Emoji,
  cores e acentos são escritos em UTF-8; um console em codepage 850 os exibe
  errado. O Windows Terminal mostra tudo corretamente, e `chcp 65001` resolve
  num console antigo. É só aparência — nenhum comando se comporta diferente.
- **Só Git.** A arquitetura prevê outros destinos (WebDAV, S3, rsync), mas
  apenas o Git está implementado.
- **Sem resolução automática de conflitos**, por decisão de projeto. Quando duas
  máquinas mudam a mesma linha, ele para e chama você.
- **Uma instância por máquina.** Iniciar uma segunda cópia falha
  intencionalmente, para evitar dois processos mexendo na mesma pasta.
- **Projeto novo.** Está testado, mas ainda não tem quilometragem de uso real em
  larga escala. Mantenha seus dados versionados em outro lugar também.

---

## Desenvolvimento

```bash
make build    # compila
make test     # testes com detector de condições de corrida
make lint     # análise estática
make all      # os três acima
```

### Organização do código

```
cmd/watchflow/       comandos de linha de comando
scripts/             instalação e desinstalação
internal/
  config/            leitura e validação do YAML
  core/              orquestrador e ciclo de vida
  watcher/           monitoramento de arquivos
  normalizer/        filtros e supressão de eco
  debouncer/         agrupamento de mudanças em rajada
  queue/             fila persistente em SQLite
  pipeline/          execução das etapas
  providers/git/     as ações do Git
  ipc/               comunicação entre CLI e serviço
  logger/            logs estruturados e ocultação de segredos
  notify/            alertas ao usuário
  tui/               painel interativo
  locking/           travas de concorrência
```

### Invariantes de engenharia

As regras abaixo não são preferências de estilo: cada uma existe porque violá-la
faz o usuário perder trabalho. Vários comentários no código apontam para esta
seção. **Uma mudança que quebre qualquer uma delas não entra.**

**Safe Git Sync, no lugar de `git pull --rebase`.** O rebase pode parar em
*detached HEAD* esperando entrada interativa, travando o repositório num daemon
desassistido. O fluxo obrigatório é: verificar `git status --porcelain` e pular
o commit se não houver alteração real; commitar o que houver **antes** de
qualquer contato com o remoto; `git fetch`; inspecionar a divergência com
`git rev-list --left-right`; então `push` direto, `merge --ff-only` ou
`merge --no-edit`, conforme o caso.

**Conflito interrompe, nunca resolve.** Havendo conflito de linhas, executar
`git merge --abort` imediatamente, deixar a árvore intacta, marcar o job como
`BLOCKED`, o watcher como `CONFLICT_HALTED` e notificar o usuário. **Marcadores
de conflito (`<<<<<<< HEAD`) jamais são escritos nos arquivos do cofre.**

**Supressão de eco.** As próprias operações do daemon (`merge`, `checkout`)
disparam eventos do sistema de arquivos. `internal/normalizer/echo_suppressor.go`
mantém uma janela em memória (caminho + expiração de ~2s) para descartá-los.
Sem isso o daemon reage às próprias escritas, indefinidamente.

**Travas.** Uma instância por máquina, garantida pelo socket exclusivo. O daemon
**nunca** remove um `.git/index.lock` de terceiros (Obsidian Git, VS Code): ele
aguarda com backoff até `max_wait_lock` e, persistindo, falha como transitório
para retentativa. Um mutex por caminho de repositório impede dois pipelines na
mesma árvore.

**Sem shell.** Nada de `sh -c`, `bash -c` ou interpolação de strings para chamar
programas externos. Todo comando é `exec.CommandContext(ctx, "git", args...)`
com os argumentos em `[]string`, e caminhos são resolvidos com
`filepath.EvalSymlinks`.

**Segredos nunca chegam ao log.** URLs de remote e saídas do Git passam por
sanitização antes de ir para o log ou o banco, para suprimir tokens embutidos
(`https://token@github.com/...`).

### Classificação de erros

O tratamento de falhas segue três categorias, e a diferença entre elas define se
o WatchFlow insiste, para ou desiste:

| Categoria | Causas | Ação | Estado do watcher |
|---|---|---|---|
| **Transitória** | Rede fora, servidor Git indisponível, `index.lock` de terceiro | Job vai para `PENDING_RETRY`, com backoff exponencial e jitter (`min(300s, 5s × 2^tentativa + rand(0,3s))`) | `HEALTHY`, log em `WARN` |
| **Conflito** | Edição divergente do mesmo arquivo em duas máquinas | `git merge --abort` imediato, job `BLOCKED`, alerta ao usuário | `CONFLICT_HALTED`, exige intervenção |
| **Fatal** | Caminho inexistente, permissão negada, Git ausente | Interrompe o watcher afetado | `DEGRADED`, log em `ERROR` |

### Contribuindo

Além das invariantes acima:

- Todo comportamento novo vem com teste.
- Se uma mudança pode fazer o usuário perder trabalho, ela não entra.

---

## Licença

Ainda não definida. Enquanto isso, todos os direitos reservados ao autor.
