# Prompt para agente de IA — WatchFlow

## Missão

Projetar um serviço local de automação reativa orientado a eventos de arquivos, independente de aplicações específicas, capaz de observar diretórios, detectar alterações, aplicar políticas de debounce/agrupamento e disparar pipelines configuráveis.

O caso de uso inicial é a sincronização automática de um vault do Obsidian entre máquinas sem depender de ação manual do usuário ou do evento de fechamento do aplicativo.

Entretanto, o sistema NÃO deve ser concebido como um "plugin do Obsidian" nem como um "Git auto-push daemon".

A arquitetura deve ser genérica e extensível.

O conceito central é:

> File-system event → normalization → debounce/coalescing → rule matching → pipeline → provider/action

Exemplos futuros de ações:

- Git commit/pull/rebase/push
- sincronização com serviço de nuvem
- WebDAV
- S3-compatible storage
- rsync/SSH
- Google Drive
- OneDrive
- execução de scripts locais
- atualização de índices
- geração de embeddings
- disparo de workflows de agentes
- backup
- notificações

A sincronização Git deve ser apenas o primeiro provider/action implementado.

---

# 1. Visão do produto

Criar um daemon local, provisoriamente chamado `watchflow`, responsável por observar alterações no sistema de arquivos e executar pipelines declarativos.

Exemplo conceitual:

```text
Vault/
   ↓
filesystem watcher
   ↓
event normalization
   ↓
debounce/coalescing
   ↓
rule engine
   ↓
pipeline
   ├── git-sync
   ├── backup
   ├── update-index
   └── notify
```

O daemon deve ser independente do aplicativo que realizou a alteração.

Exemplo:

- Obsidian altera `nota.md`;
- Codex CLI cria outro arquivo;
- um script modifica YAML;
- o usuário move um PDF pelo gerenciador de arquivos.

Para o sistema, todos são apenas eventos no filesystem.

---

# 2. Princípios arquiteturais

Adotar os seguintes princípios.

## 2.1. Application agnostic

Não depender de Obsidian, VS Code, Codex, Claude Code ou qualquer outro aplicativo.

O daemon observa o filesystem.

## 2.2. Provider agnostic

A camada responsável por detectar mudanças não deve saber se o destino é:

- Git;
- Google Drive;
- S3;
- WebDAV;
- SSH;
- outro serviço.

Providers devem ser módulos substituíveis.

## 2.3. Pipelines declarativos

O comportamento deve ser definido por configuração, e não hardcoded.

Exemplo desejável:

```yaml
watchers:
  - name: personal-vault
    path: ~/Vault

    debounce: 15s

    ignore:
      - ".git/**"
      - ".obsidian/cache/**"
      - "*.tmp"

    pipelines:
      - git-sync
      - notify-on-error
```

Com pipelines definidos separadamente:

```yaml
pipelines:

  git-sync:
    steps:
      - action: git.add

      - action: git.commit
        message: "auto: {{timestamp}}"

      - action: git.pull
        strategy: rebase

      - action: git.push
```

## 2.4. Idempotência

A repetição de um evento ou pipeline não deve produzir efeitos incorretos.

Exemplo:

```text
git commit
```

não deve falhar como erro grave apenas porque não há mudanças.

## 2.5. Falhas explícitas

Nunca esconder conflitos.

Especialmente:

- conflito de merge;
- conflito de rebase;
- lock de repositório;
- autenticação;
- indisponibilidade de rede;
- provider indisponível.

O daemon deve registrar o erro e interromper apenas o pipeline afetado.

## 2.6. Concorrência controlada

Nunca permitir duas operações incompatíveis simultaneamente sobre o mesmo recurso.

Exemplo:

```text
git pull
```

e

```text
git push
```

não podem concorrer dentro do mesmo repositório.

Implementar locks por recurso/repositório.

## 2.7. Offline-first

Mudanças locais não podem ser perdidas porque a máquina está sem internet.

O sistema deve:

1. detectar mudanças;
2. registrar trabalho pendente;
3. tentar sincronizar;
4. manter o job pendente se falhar por indisponibilidade externa;
5. tentar novamente posteriormente.

## 2.8. Observabilidade

O sistema deve possuir logs estruturados e estado verificável.

Deve ser possível responder:

```text
O daemon está rodando?
Qual foi o último sync?
Existe algum job pendente?
Algum provider está com erro?
Existe conflito?
```

---

# 3. Arquitetura conceitual

Avaliar a seguinte arquitetura.

```text
┌──────────────────────────┐
│ Filesystem Watcher       │
│ inotify / fsnotify etc.  │
└────────────┬─────────────┘
             │
             ▼
┌──────────────────────────┐
│ Event Normalizer         │
└────────────┬─────────────┘
             │
             ▼
┌──────────────────────────┐
│ Debounce / Coalescing    │
└────────────┬─────────────┘
             │
             ▼
┌──────────────────────────┐
│ Rule Engine              │
└────────────┬─────────────┘
             │
             ▼
┌──────────────────────────┐
│ Job Queue                │
└────────────┬─────────────┘
             │
             ▼
┌──────────────────────────┐
│ Pipeline Engine          │
└────────────┬─────────────┘
             │
             ▼
┌──────────────────────────┐
│ Actions / Providers      │
├──────────────────────────┤
│ Git                      │
│ WebDAV                   │
│ S3                       │
│ SSH                      │
│ Cloud                    │
│ Script                   │
└──────────────────────────┘
```

Não assumir que esta arquitetura é obrigatoriamente a melhor.

Criticá-la e propor ajustes quando necessário.

---

# 4. Componentes mínimos

Avaliar e especificar pelo menos:

## Watcher

Responsável por receber eventos do filesystem.

Deve lidar com:

- create;
- modify;
- delete;
- rename;
- move.

Evitar polling se houver API nativa adequada no sistema operacional.

## Event normalizer

Eventos de filesystem podem ser ruidosos.

Uma única gravação pode gerar múltiplos eventos.

Normalizar isso antes de chegar ao pipeline.

## Debounce/coalescing

Exemplo:

```text
nota.md modificada
+1s nota.md modificada
+2s config alterada
+4s nota.md modificada
```

Com:

```yaml
debounce: 10s
```

deve resultar, idealmente, em apenas um job.

Avaliar:

- trailing debounce;
- max wait;
- batch window.

Exemplo:

```yaml
debounce: 10s
max_wait: 60s
```

Assim, alterações contínuas não impedem sincronização indefinidamente.

## Rule engine

Permitir filtros por:

- diretório;
- glob;
- extensão;
- tipo de evento.

Exemplo futuro:

```yaml
rules:

  - match:
      glob: "**/*.md"

    pipeline:
      - git-sync
      - update-index

  - match:
      glob: "**/*.pdf"

    pipeline:
      - cloud-backup
```

## Queue

Jobs precisam sobreviver a falhas temporárias.

Avaliar se o MVP deve usar:

- memória;
- arquivo JSON;
- SQLite;
- BoltDB;
- outra solução embutida.

Priorizar simplicidade e robustez.

## Pipeline engine

Executar steps sequencialmente.

Exemplo:

```text
git.add
    ↓
git.commit
    ↓
git.pull --rebase
    ↓
git.push
```

Cada step deve retornar estado estruturado.

## Providers/actions

Separar:

```text
pipeline engine
```

de:

```text
implementação concreta das ações
```

---

# 5. Provider Git inicial

Implementar como primeiro provider.

Pipeline conceitual:

```text
detect change
     ↓
git status
     ↓
git add
     ↓
git commit
     ↓
git pull --rebase
     ↓
git push
```

Entretanto, considerar cuidadosamente problemas de concorrência e sincronização.

Requisitos:

- detectar se o diretório é repositório;
- não criar commit vazio;
- não executar operações concorrentes;
- lidar com ausência de remote;
- permitir configuração de branch;
- detectar conflito;
- nunca resolver conflito automaticamente sem política explícita;
- suportar offline;
- retry de falhas transitórias;
- distinguir erro transitório de erro que exige intervenção.

Possível mensagem automática:

```text
auto: 2026-09-04T18:10:32-03:00
```

ou:

```text
watchflow: sync 2026-09-04 18:10
```

Avaliar se commits automáticos tão frequentes são desejáveis.

Considerar também estratégia alternativa:

```text
pull/rebase
commit
push
```

ou outras sequências.

Explicar qual é mais segura e por quê.

---

# 6. Sincronização entre múltiplas máquinas

Caso principal:

```text
Notebook A
    ↕
remote
    ↕
Notebook B
```

A solução deve minimizar o risco de:

- divergência;
- overwrite;
- rebase inesperado;
- conflito silencioso;
- push rejeitado.

Considerar situação:

```text
A offline → alterações
B online  → alterações + push
A online novamente
```

Especificar comportamento correto.

---

# 7. Interação com outros processos Git

O mesmo repositório pode ser usado simultaneamente por:

- usuário no terminal;
- Obsidian Git;
- Codex CLI;
- IDE;
- watchflow.

Portanto:

- respeitar `.git/index.lock`;
- implementar lock próprio do daemon;
- nunca remover `index.lock` de terceiros automaticamente;
- utilizar timeout;
- retry posterior.

Avaliar se é recomendável desabilitar automações Git concorrentes do Obsidian quando o daemon estiver ativo.

---

# 8. Estado persistente

O daemon deve manter estado suficiente para responder:

```text
last_event
last_successful_sync
last_failed_sync
pending_jobs
retry_count
current_status
```

Preferir armazenamento simples.

Exemplo:

```text
~/.local/state/watchflow/
```

Possível estrutura:

```text
watchflow/
├── state.db
├── logs/
└── locks/
```

---

# 9. Logs

Utilizar logs estruturados.

Exemplo:

```json
{
  "timestamp": "2026-09-04T18:10:22-03:00",
  "watcher": "personal-vault",
  "pipeline": "git-sync",
  "step": "git.push",
  "status": "success",
  "duration_ms": 481
}
```

Níveis:

```text
DEBUG
INFO
WARN
ERROR
```

Nunca registrar:

- tokens;
- senhas;
- credenciais;
- secrets.

---

# 10. Notificações

Prever interface abstrata para notificações.

Eventos relevantes:

```text
sync failed
conflict detected
authentication failed
retry exhausted
```

Evitar notificações de sucesso constantes por padrão.

---

# 11. Serviço do sistema

No Linux, avaliar:

```text
systemd --user
```

Exemplo conceitual:

```text
systemctl --user enable --now watchflow
```

O daemon deve iniciar automaticamente na sessão do usuário.

Avaliar posteriormente:

- macOS LaunchAgent;
- Windows Service/Task Scheduler.

---

# 12. Linguagem

Avaliar principalmente:

- Go
- Rust
- Python

Critérios:

- daemon de longa duração;
- baixo consumo;
- concorrência;
- suporte nativo a filesystem watcher;
- facilidade de distribuição;
- binário único;
- portabilidade;
- qualidade do ecossistema;
- manutenção.

Há preferência inicial por Go, mas NÃO assumir isso sem análise.

Escolher a linguagem e justificar.

---

# 13. Interface de linha de comando

Planejar CLI semelhante a:

```bash
watchflow start
watchflow stop
watchflow status
watchflow sync
watchflow logs
watchflow doctor
watchflow config validate
```

Possivelmente:

```bash
watchflow status personal-vault
```

Resultado esperado:

```text
personal-vault

status: healthy
last event: 18:04:12
last sync: 18:04:28
pending jobs: 0
provider: git
remote: origin
branch: main
```

---

# 14. Configuração

Preferir um único arquivo legível.

Avaliar:

```text
YAML
TOML
```

Exemplo:

```yaml
version: 1

watchers:

  - name: vault

    path: ~/Documents/Vault

    debounce: 15s
    max_wait: 60s

    ignore:
      - ".git/**"
      - ".obsidian/cache/**"
      - "*.tmp"

    pipelines:
      - git-sync

pipelines:

  git-sync:
    steps:

      - action: git.add

      - action: git.commit
        message: "auto: {{timestamp}}"

      - action: git.pull
        strategy: rebase

      - action: git.push
```

---

# 15. Segurança

Nunca executar comandos arbitrários vindos de arquivos observados.

Se existir futuramente:

```yaml
action: shell
```

deve ser explicitamente habilitado.

Considerar:

- command injection;
- symlink traversal;
- path traversal;
- secrets;
- permissões;
- execução sobre repositórios não confiáveis.

---

# 16. Critérios de qualidade

O projeto deve priorizar:

1. confiabilidade;
2. previsibilidade;
3. preservação de dados;
4. simplicidade operacional;
5. observabilidade;
6. baixo consumo de recursos;
7. extensibilidade.

Nunca priorizar conveniência em detrimento da integridade dos arquivos.

---

# 17. Escopo do MVP

O MVP deve ser deliberadamente pequeno.

Sugestão:

```text
watch directory
      ↓
debounce
      ↓
enqueue
      ↓
git pipeline
      ↓
log result
```

Suportar inicialmente apenas:

- Linux;
- um ou vários diretórios;
- provider Git;
- configuração local;
- systemd user;
- logs;
- retries;
- locks;
- status CLI.

NÃO implementar inicialmente:

- GUI;
- mobile;
- servidor central;
- sync peer-to-peer;
- Google Drive;
- S3;
- embeddings;
- agentes;
- dashboard web.

Esses itens devem ficar apenas previstos arquiteturalmente.

---

# 18. Testes obrigatórios

O planejamento deve prever testes progressivos.

## Testes unitários

Exemplos:

- parsing da configuração;
- debounce;
- rule matching;
- queue;
- retry policy;
- classificação de erros;
- geração de mensagens de commit.

## Testes de integração

Criar repositórios Git temporários durante o teste.

Testar:

```text
arquivo alterado
→ commit
→ push
```

Testar também:

```text
arquivo alterado várias vezes
→ apenas um job
```

## Teste offline

```text
alteração
→ network unavailable
→ job pending
→ network returns
→ retry
→ push success
```

## Teste de conflito

Máquina simulada A e B:

```text
A altera arquivo X
B altera arquivo X
B push
A tenta sync
```

Resultado:

```text
conflict detected
pipeline halted
no data loss
user notified
```

## Teste de concorrência

Duas alterações simultâneas não devem provocar duas operações Git concorrentes.

## Teste de restart

```text
job pending
→ daemon killed
→ daemon restarted
```

O job deve continuar pendente caso seja adotada fila persistente.

## Teste de stress

Gerar centenas ou milhares de eventos de filesystem.

Verificar:

- memória;
- CPU;
- número de jobs;
- coalescing.

---

# 19. Critérios de aceitação do MVP

O MVP só pode ser considerado concluído quando:

- detectar alterações automaticamente;
- agrupar eventos redundantes;
- realizar sync Git sem intervenção;
- não gerar commits vazios;
- sobreviver a perda temporária de internet;
- impedir operações Git concorrentes;
- detectar conflitos;
- nunca resolver conflitos silenciosamente;
- reiniciar sem perder estado crítico;
- possuir logs úteis;
- possuir `status`;
- funcionar como serviço `systemd --user`;
- passar por suíte automatizada de testes.

---

# 20. Roadmap arquitetural futuro

Planejar extensibilidade para:

## Providers

```text
git
webdav
s3
ssh
google-drive
onedrive
```

## Actions

```text
backup
shell
notify
index
embedding
agent-trigger
```

## Triggers

Inicialmente:

```text
filesystem change
```

Futuramente:

```text
timer
application close
system suspend
system resume
network available
manual
webhook
```

Assim, futuramente seria possível:

```yaml
triggers:

  - filesystem.change

  - system.suspend

  - network.online
```

---

# 21. Não assumir complexidade desnecessária

Aplicar YAGNI.

Não introduzir prematuramente:

- Kafka;
- Redis;
- microservices;
- Kubernetes;
- banco externo;
- message broker;
- daemon distribuído;
- servidor remoto próprio.

O sistema deve funcionar inicialmente como um único processo local.

---

# 22. Decisões que devem ser explicitamente analisadas

Antes de implementar, produzir ADRs ou decisões arquiteturais para:

1. linguagem;
2. biblioteca de filesystem watching;
3. armazenamento de estado;
4. formato de configuração;
5. política de debounce;
6. política de retry;
7. estratégia Git;
8. lock/concurrency model;
9. lifecycle do daemon;
10. abstração de providers/actions.

Para cada decisão, registrar:

```text
Context
Options
Decision
Rationale
Consequences
```

---

# 23. Estrutura inicial do projeto

Propor uma estrutura de diretórios adequada.

Exemplo meramente indicativo:

```text
watchflow/
├── cmd/
│   └── watchflow/
├── internal/
│   ├── watcher/
│   ├── events/
│   ├── debounce/
│   ├── queue/
│   ├── pipeline/
│   ├── providers/
│   │   └── git/
│   ├── config/
│   ├── state/
│   ├── locking/
│   └── logging/
├── configs/
├── docs/
│   └── adr/
├── tests/
└── README.md
```

Não adotar esta estrutura automaticamente: avaliar e melhorar.

---

# 24. Forma de trabalho

NÃO começar a implementação diretamente.

Primeiro executar a fase de planejamento.

O plano deve ser suficientemente detalhado para que outro agente de IA consiga implementar tickets individuais sem precisar reinterpretar a arquitetura inteira.

Tickets devem ser pequenos, testáveis e com dependências explícitas.

Cada ticket deve conter:

```text
ID
Título
Objetivo
Contexto
Escopo
Fora de escopo
Dependências
Arquivos/componentes esperados
Implementação esperada
Critérios de aceitação
Testes
Riscos
Definition of Done
```

Preferir tickets implementáveis em uma única sessão de trabalho.

---

# COMANDO

Analise integralmente esta especificação e gere um PLANO DE IMPLEMENTAÇÃO, sem escrever ainda o código de produção.

O plano deve conter, nesta ordem:

1. resumo arquitetural;
2. dúvidas ou inconsistências encontradas na especificação;
3. decisões arquiteturais necessárias;
4. ADRs preliminares para as decisões mais importantes;
5. arquitetura proposta;
6. modelo de dados e estado;
7. estrutura de diretórios do projeto;
8. estratégia de configuração;
9. lifecycle completo de um evento;
10. modelo de concorrência e locking;
11. estratégia de tratamento de erros;
12. estratégia de retry;
13. estratégia específica do provider Git;
14. modelo de extensibilidade para futuros providers/actions;
15. estratégia de observabilidade;
16. estratégia de segurança;
17. estratégia de testes;
18. roadmap por fases;
19. tickets de implementação para cada fase;
20. testes intermediários obrigatórios ao final de cada fase;
21. critérios de aprovação/reprovação de cada fase;
22. riscos técnicos e respectivas mitigações;
23. definição objetiva do MVP;
24. backlog pós-MVP.

Organize as fases de modo que cada uma produza um sistema verificável.

Exemplo conceitual:

Fase 0 — arquitetura e scaffolding
Fase 1 — watcher e eventos
Fase 2 — debounce e job queue
Fase 3 — pipeline engine
Fase 4 — provider Git local
Fase 5 — remote sync e retries
Fase 6 — persistência e recuperação
Fase 7 — CLI e observabilidade
Fase 8 — systemd e hardening
Fase 9 — release do MVP

Não seguir essa divisão cegamente. Melhore-a se houver uma decomposição tecnicamente superior.

Para CADA fase, especificar:

- objetivo;
- entregáveis;
- tickets;
- dependências;
- testes unitários;
- testes de integração;
- teste manual de smoke;
- cenário de falha que deve ser validado;
- critérios de aceite;
- condição que impede avanço à próxima fase.

Ao final, gerar também:

## Dependency graph

Representar as dependências entre os tickets.

## Critical path

Identificar os tickets que formam o caminho crítico até o MVP.

## Test matrix

Criar uma matriz:

| Cenário | Unit | Integration | E2E | Manual |
|---|---|---|---|---|

## MVP demo scenario

Descrever um roteiro reproduzível demonstrando:

```text
1. daemon iniciado
2. arquivo do vault alterado
3. eventos coalescidos
4. job criado
5. commit realizado
6. remote temporariamente indisponível
7. retry agendado
8. remote volta
9. pull/rebase
10. push concluído
11. status mostra sistema saudável
```

## Anti-data-loss checklist

Criar um checklist específico de mecanismos que garantam que nenhuma automação destrua, sobrescreva ou silenciosamente descarte alterações do usuário.

Não implementar código até que esse plano seja revisado.
