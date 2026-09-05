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
> inicial. Só roda em Linux. Veja [Limitações conhecidas](#limitações-conhecidas).

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

- **Linux** com kernel que suporte `inotify` (qualquer distribuição moderna)
- **Git** instalado (versão 2.30 ou mais recente é o recomendado)
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

Se ainda tiver o código-fonte, o equivalente é:

```bash
./scripts/uninstall.sh
```

Para o serviço, remove a unit e apaga o binário. **Sua configuração e o
histórico do daemon são preservados**, de modo que reinstalar depois devolve
tudo como estava. Use `--purge` para removê-los também.

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

- **Só Linux.** O código depende de `inotify`, sockets Unix e `systemd --user`.
  Windows e macOS exigiriam trabalho de portabilidade, documentado em
  [`Plano de Implementação - TUI.md`](Plano%20de%20Implementação%20-%20TUI.md).
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

### Contribuindo

O arquivo [`AGENTS.md`](AGENTS.md) descreve as regras de engenharia do projeto —
em especial as de segurança de dados, que não são negociáveis. Vale ler antes de
propor mudanças.

Pedidos práticos:

- Todo comportamento novo vem com teste.
- Nada de `sh -c` ou interpolação de strings para chamar comandos externos.
- Se uma mudança pode fazer o usuário perder trabalho, ela não entra.

---

## Licença

Ainda não definida. Enquanto isso, todos os direitos reservados ao autor.
