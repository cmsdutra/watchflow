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

Quando você salva um arquivo, acontece o seguinte:

```
Você salva um arquivo
        ↓
O kernel avisa o WatchFlow (inotify)
        ↓
Filtros descartam ruído (.git/, arquivos temporários)
        ↓
Espera 15s de silêncio para agrupar edições em rajada
        ↓
Grava a intenção de sincronizar no banco local (SQLite)
        ↓
Executa: git add → git commit → sincroniza → git push
```

Dois detalhes importantes desse fluxo:

**A espera de 15 segundos** existe para não gerar um commit a cada tecla salva.
Se você salvar cinco arquivos em dez segundos, tudo vira um commit só. Há também
um teto (60s por padrão) para que edições contínuas não adiem a sincronização
indefinidamente.

**A intenção é gravada em disco antes de qualquer coisa acontecer.** Se faltar
energia no meio de um `git push`, o trabalho pendente não se perde: na próxima
vez que o programa subir, ele retoma de onde parou.

---

## O que ele garante

Estas não são promessas de marketing; são as regras que guiam o código.

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
./scripts/install.sh
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
de arquivos são suficientes para o tamanho da sua pasta, e se os diretórios de
trabalho estão acessíveis. Se algo estiver errado, ele diz o que fazer.

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

Um arquivo mínimo para sincronizar uma pasta:

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
    debounce: "15s"      # espera de silêncio antes de agir
    max_wait: "60s"      # teto máximo de espera
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

Depois de editar, valide antes de rodar:

```bash
watchflow config validate
```

A validação confere a sintaxe, se as pastas existem, se os nomes de ações são
válidos e se os parâmetros de cada passo fazem sentido. Um erro de digitação é
apontado ali, e não silenciosamente ignorado.

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
watchflow sync                    # sincroniza tudo agora, sem esperar
watchflow sync meu-cofre          # sincroniza só uma pasta
watchflow pause meu-cofre         # congela temporariamente
watchflow resume meu-cofre        # retoma
```

Pausar é seguro: as mudanças continuam sendo registradas na fila e são
processadas quando você retomar. Nada é descartado.

---

## Rodando como serviço

Para que ele suba junto com sua sessão, use o systemd do usuário:

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

Se quiser que continue rodando mesmo com você deslogado do ambiente gráfico:

```bash
sudo loginctl enable-linger $USER
```

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

### Não está sincronizando

Nesta ordem:

```bash
watchflow doctor        # o ambiente está sadio?
watchflow status        # a pasta está HEALTHY, PAUSED ou parada?
watchflow jobs          # tem trabalho preso na fila?
watchflow logs --level warn -n 50
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
