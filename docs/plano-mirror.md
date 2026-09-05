# mobile-mirror — avaliação e briefing

> **Status: em espera — não executar.**
> Avaliado em 2026-09-05 contra o estado real do cofre Chaos. A análise de
> integridade deste briefing continua válida e reaproveitável; o que não se
> sustenta hoje é a **motivação**. Retomar apenas se uma das condições de
> reativação abaixo ocorrer.

## Por que está em espera

Medições do cofre Chaos em 2026-09-05, depois de parar de versionar os
`node_modules` dos plugins (commit `ca92f67e`, que sozinho cortou 73% do peso):

```
cofre rastreado          : 3539 arquivos, 36.1 MiB
projeção deste briefing  : 1586 arquivos, 12.2 MiB
economia                 : 1953 arquivos, 23.9 MiB
```

**Ganho de espaço: ~24 MiB.** Não paga um subsistema de sincronização
bidirecional — muito menos um cujo modo de falha é apagar notas.

O segundo argumento considerado — poluição do índice do Obsidian — **não
existe**:

```
.md rastreados                        : 2191
  em dot-dirs (.agents/, .claude/, .codex/) :  638   Obsidian já ignora
  visíveis ao Obsidian                      : 1553
  cobertos por userIgnoreFilters            :   11
  notas realmente indexadas                 : 1542
```

O Obsidian ignora diretórios iniciados por ponto por padrão, e é exatamente
onde vive toda a infraestrutura de agentes. No celular será igual: os 638
arquivos ocupam bytes no clone e ficam invisíveis no aplicativo.

Vale registrar por que "tirar a infra do cofre" também não é saída: ela está
deliberadamente entrelaçada com o conteúdo, com escopo por área PARA
(`300_Trabalho/.agents/` com 226 arquivos, `400_Biblioteca/.agents/` com 77,
e assim por diante). É assim que as skills são descobertas. Separá-las
quebraria o escopo.

## Decisão adotada

Um repositório só, com clone raso no celular via `obsidian-git`. Zero
engenharia nova.

## Condições de reativação

Basta uma:

1. O clone raso não dar conta na prática — `isomorphic-git` é lento, e só o
   uso real decide.
2. Surgir necessidade de **conteúdo diferente por dispositivo**, e não apenas
   de menos conteúdo (por exemplo um `Chaos-Work` sem `700_Financeiro`). Esse
   é o único caso em que projeção é a resposta certa, e nada mais resolve.
3. O `.obsidian/appearance.json` virar conflito recorrente no celular, onde
   não há suporte a merge driver.

## Se for retomado, três correções antes de executar

1. **Fixar a arquitetura antes de delegar.** O briefing manda o agente
   "avaliar SQLite, JSON, journal" para o estado de sincronização (§6). Os dois
   lados já são repositórios Git: derivar cada commit do espelho de um commit
   do principal, gravando a procedência num trailer (`Source-Commit: <sha>`),
   faz o BASE ser uma árvore Git em vez de um banco paralelo. Com isso vêm de
   graça rename detection (`git diff -M`), tombstones, merge de três vias com
   base real, recuperação de falhas e imunidade a loops — que os §§6, 7, 10,
   12, 13 e 18 pedem para construir à mão. Arte prévia: `josh`, que faz
   projeção filtrada bidirecional de repositórios Git.
2. **Cortar o §21 (múltiplas projeções) do escopo.** Uma fonte com N espelhos
   bidirecionais independentes é replicação multi-master, e manter isso como
   requisito arquitetural contamina o desenho inteiro por um caso hipotético.
3. **Fechar a issue #1 antes da Fase 1.** As escritas do próprio WatchFlow
   realimentam seu watcher; no mirror o daemon vigia e escreve nos dois lados,
   agravando exatamente o problema que o §10 chama de crítico.

## Lacunas do briefing, se for retomado

- **Mudança de filtro não tem política definida.** O §12 cita "alteração de
  filtro" como causa possível de ausência, mas nunca define o que acontece ao
  **adicionar** um `exclude`: os arquivos já espelhados somem do celular? E se
  houver edição local ainda não trazida? É o caminho mais provável para perda
  silenciosa, e passa despercebido.
- **Artefatos de conflito dentro do vault (§13).** Um
  `foo.conflict-mobile-….md` seria indexado pelo Obsidian como nota e
  espelhado de volta. Precisa sair da projeção e do Git.
- **Um acerto não declarado:** excluir `.obsidian/` da projeção elimina o
  problema do `appearance.json` sem merge driver no celular. Vale explicitar
  como benefício.

---

# Briefing original (preservado na íntegra)

*O texto abaixo é o briefing como foi escrito, sem alterações. As ressalvas
estão no cabeçalho acima.*

## Tarefa: projetar e implementar o backend `mobile-mirror` do WatchFlow

Você é um agente de engenharia de software responsável por projetar e implementar um novo backend para o **WatchFlow**.

O WatchFlow é um serviço de sincronização orientado a eventos que observa alterações em arquivos locais e desacopla:

1. detecção de mudanças;
2. fila/coordenação de eventos;
3. resolução de estado/conflitos;
4. destino ou estratégia de sincronização.

O objetivo desta tarefa é adicionar um backend denominado:

```text
mobile-mirror
```

Esse backend deverá criar e manter uma **projeção filtrada e bidirecional de um vault principal do Obsidian**, destinada ao uso em dispositivos móveis.

O dispositivo móvel NÃO acessará diretamente o vault principal.

A arquitetura pretendida é:

```text
                       COMPUTADOR

                ┌────────────────────┐
                │    Vault principal │
                │       Chaos        │
                │                    │
                │ notas              │
                │ scripts            │
                │ skills             │
                │ agentes            │
                │ estado             │
                │ workbench          │
                │ anexos             │
                └─────────┬──────────┘
                          │
                          │ WatchFlow
                          │ backend:
                          │ mobile-mirror
                          ▼
                ┌────────────────────┐
                │    Chaos-Mobile    │
                │                    │
                │ projeção filtrada  │
                │ do vault principal │
                └─────────┬──────────┘
                          │
                         Git
                          │
                          ▼
                       GitHub
                          │
                         Git
                          │
                          ▼

                         CELULAR

                ┌────────────────────┐
                │    Chaos-Mobile    │
                │      Obsidian      │
                │   + Obsidian Git   │
                └────────────────────┘
```

---

# 1. Objetivo funcional

O `mobile-mirror` deve permitir que o usuário tenha no celular apenas os arquivos relevantes para consulta e edição de notas, sem transportar toda a infraestrutura existente no vault principal.

Exemplos de conteúdos que normalmente DEVEM ser disponibilizados no mobile:

- notas Markdown;
- arquivos Canvas;
- imagens utilizadas pelas notas;
- determinados anexos;
- determinados diretórios definidos pelo usuário.

Exemplos de conteúdos que normalmente NÃO DEVEM ser enviados ao mobile:

- `.codex/`;
- `.agents/`;
- `.claude/`;
- `.gemini/`;
- `.common-box/`;
- `scripts/`;
- arquivos de estado;
- logs;
- caches;
- workbenches;
- arquivos de ingestão;
- arquivos temporários;
- artefatos intermediários de agentes;
- PDFs ou outros anexos pesados, caso assim configurado.

O backend deve ser configurável e NÃO deve codificar essas exclusões diretamente.

---

# 2. Princípio arquitetural

O `Chaos-Mobile` não deve ser considerado um segundo vault independente.

Ele deve ser tratado como uma:

> projeção materializada, filtrada e bidirecional do vault principal.

O vault principal permanece sendo a fonte canônica da estrutura e da infraestrutura.

Entretanto, determinadas alterações realizadas no mobile devem poder retornar ao vault principal.

Exemplos:

```text
Celular:
editar nota       → permitido
criar nota        → permitido
renomear nota     → permitido
mover nota        → permitido
apagar nota       → permitido
```

Essas operações devem ser propagadas de volta ao vault principal quando forem válidas segundo as regras de sincronização.

---

# 3. Integração com Git

O backend `mobile-mirror` NÃO precisa implementar o protocolo de sincronização com o celular.

A responsabilidade deve ficar separada:

```text
WatchFlow
    │
    │ sincronização local
    ▼
Chaos-Mobile/
    │
    │ Git normal
    ▼
GitHub
    │
    │ Obsidian Git
    ▼
Android
```

O `mobile-mirror` deve trabalhar entre:

```text
Vault principal ⇄ Vault mobile local
```

O Git deverá trabalhar entre:

```text
Vault mobile local ⇄ GitHub ⇄ celular
```

Essa separação é obrigatória.

Não acople a lógica do `mobile-mirror` à API do GitHub.

O backend poderá, entretanto, opcionalmente emitir eventos/hooks que permitam a outro componente executar:

```text
git add
git commit
git pull
git push
```

ou utilizar um backend Git já existente no WatchFlow, caso a arquitetura atual permita composição de backends.

---

# 4. Fluxo esperado

## Alteração originada no computador

Exemplo:

```text
Chaos/
100_Projetos/WatchFlow/arquitetura.md
```

é alterado.

Se o arquivo satisfizer as regras de inclusão:

```text
WatchFlow detecta alteração
    ↓
mobile-mirror recebe evento
    ↓
verifica filtro
    ↓
compara estado
    ↓
propaga alteração
    ↓
Chaos-Mobile/
100_Projetos/WatchFlow/arquitetura.md
```

Posteriormente:

```text
Git → commit/push → GitHub → celular
```

---

## Alteração originada no celular

O usuário edita:

```text
Chaos-Mobile/
100_Projetos/WatchFlow/ideias.md
```

No celular:

```text
Obsidian Git
    ↓
commit/push
    ↓
GitHub
```

No computador:

```text
git pull
    ↓
Chaos-Mobile muda localmente
    ↓
WatchFlow detecta alteração
    ↓
mobile-mirror identifica origem mobile
    ↓
compara estado
    ↓
propaga para Chaos/
```

Resultado:

```text
Chaos/
100_Projetos/WatchFlow/ideias.md
```

é atualizado.

---

# 5. Sincronização bidirecional

Não implemente o sistema como simples:

```text
cp
```

ou:

```text
rsync --delete
```

A sincronização precisa possuir conhecimento de estado anterior.

O backend deve conseguir distinguir pelo menos:

### Caso A — alteração apenas no principal

```text
BASE   = A
MAIN   = B
MOBILE = A
```

Resultado:

```text
MAIN → MOBILE
```

### Caso B — alteração apenas no mobile

```text
BASE   = A
MAIN   = A
MOBILE = B
```

Resultado:

```text
MOBILE → MAIN
```

### Caso C — nenhum mudou

```text
BASE   = A
MAIN   = A
MOBILE = A
```

Resultado:

```text
NOOP
```

### Caso D — ambos mudaram para o mesmo conteúdo

```text
BASE   = A
MAIN   = B
MOBILE = B
```

Resultado:

```text
aceitar B
atualizar estado
```

### Caso E — ambos mudaram de forma diferente

```text
BASE   = A
MAIN   = B
MOBILE = C
```

Resultado:

```text
CONFLICT
```

Nenhum dos lados pode simplesmente sobrescrever o outro.

---

# 6. Estado de sincronização

Projete um mecanismo persistente para registrar a última versão comum conhecida.

Exemplo conceitual:

```json
{
  "100_Projetos/WatchFlow/ideias.md": {
    "base_hash": "sha256:abc123",
    "main_hash": "sha256:def456",
    "mobile_hash": "sha256:ghi789",
    "last_sync": "2026-09-05T15:00:00Z"
  }
}
```

Isso é apenas um exemplo.

Analise e escolha a representação adequada.

Considere:

- SQLite;
- JSON;
- arquivos estruturados;
- journal append-only;
- banco/estado já existente no WatchFlow.

Dê preferência ao mecanismo já adotado pelo projeto, se existir.

Não introduza uma nova dependência apenas por conveniência sem justificar.

---

# 7. Identidade dos arquivos

Não assuma que:

```text
path == identidade permanente
```

Arquivos podem ser:

- renomeados;
- movidos;
- deletados;
- recriados.

Projete uma estratégia para distinguir quando possível:

```text
rename
```

de:

```text
delete + create
```

Avalie:

- hashes;
- inode quando aplicável;
- similaridade de conteúdo;
- eventos fornecidos pelo watcher;
- IDs internos já existentes no WatchFlow.

Não complique desnecessariamente a primeira versão.

Se uma estratégia robusta de rename detection exigir complexidade excessiva, proponha-a para uma fase posterior.

---

# 8. Filtros

O backend deve aceitar configuração declarativa.

Exemplo:

```yaml
mobile_mirror:
  source: "/home/user/Vaults/Chaos"
  mirror: "/home/user/Vaults/Chaos-Mobile"

  include:
    - "**/*.md"
    - "**/*.canvas"
    - "**/*.png"
    - "**/*.jpg"

  exclude:
    - "**/.codex/**"
    - "**/.agents/**"
    - "**/.claude/**"
    - "**/.gemini/**"
    - "**/.common-box/**"
    - "**/scripts/**"
    - "**/state/**"
    - "**/workbench/**"
    - "**/.ingest/**"
    - "**/tmp/**"
    - "**/logs/**"
    - "**/*.pdf"
    - "**/*.zip"
```

A sintaxe real deve seguir o padrão de configuração existente no WatchFlow.

Não crie outro formato se o projeto já possui um.

Defina claramente:

- precedência entre include/exclude;
- comportamento quando nenhuma regra é encontrada;
- tratamento de arquivos ocultos;
- tratamento de symlinks;
- tratamento de arquivos fora do source por symlink;
- case sensitivity;
- normalização de paths.

---

# 9. Segurança

O backend deve impedir path traversal e escapes do diretório configurado.

Entradas como:

```text
../../etc/passwd
```

jamais podem resultar em leitura ou escrita fora dos roots autorizados.

Considere também:

- symlink traversal;
- paths absolutos inesperados;
- links quebrados;
- loops de symlink;
- nomes incompatíveis entre filesystems;
- diferenças de case sensitivity.

---

# 10. Loops de sincronização

Este requisito é crítico.

Considere:

```text
MAIN muda
↓
mobile-mirror copia para MOBILE
↓
watcher detecta MOBILE mudando
↓
mobile-mirror interpreta como nova edição
↓
copia novamente para MAIN
↓
watcher detecta MAIN
↓
...
```

O sistema deve possuir estratégia explícita de prevenção de loops.

Avalie mecanismos como:

- event IDs;
- operation IDs;
- origin metadata;
- suppression windows;
- hashes;
- estado de aplicação;
- journal interno.

Não use apenas `sleep()` ou debounce temporal como mecanismo de correção.

Debounce pode ser utilizado como otimização, mas não como garantia de consistência.

---

# 11. Operações suportadas

Projete comportamento explícito para:

```text
CREATE
MODIFY
DELETE
RENAME
MOVE
```

Para cada operação, determine comportamento em:

```text
MAIN → MOBILE
MOBILE → MAIN
```

Inclua na especificação uma matriz semelhante a:

```text
| Operação | Main → Mobile | Mobile → Main | Conflito possível |
|----------|---------------|---------------|-------------------|
| create   | ...           | ...           | ...               |
| modify   | ...           | ...           | ...               |
| delete   | ...           | ...           | ...               |
| rename   | ...           | ...           | ...               |
| move     | ...           | ...           | ...               |
```

---

# 12. Exclusões

Delete é uma operação destrutiva.

Não trate:

```text
arquivo ausente
```

automaticamente como:

```text
usuário deletou o arquivo
```

A ausência pode decorrer de:

- alteração de filtro;
- clone incompleto;
- falha de filesystem;
- operação Git intermediária;
- sparse checkout;
- diretório temporariamente inacessível.

Defina critérios explícitos para propagação de exclusões.

Considere tombstones.

Exemplo conceitual:

```json
{
  "path": "Notas/foo.md",
  "deleted_at": "...",
  "origin": "mobile",
  "base_hash": "..."
}
```

Avalie se são necessários já no MVP.

---

# 13. Conflitos

Para arquivos Markdown e outros arquivos textuais, considere merge de três vias:

```text
base
main
mobile
```

Preferencialmente utilizando mecanismo robusto já disponível no ambiente.

Um conflito NÃO pode resultar silenciosamente em perda de dados.

Defina uma estratégia padrão.

Exemplo aceitável:

```text
Notas/foo.md
Notas/foo.conflict-mobile-20260905T150000.md
```

ou uma área:

```text
.watchflow/conflicts/
```

Entretanto, antes de escolher, avalie a arquitetura existente.

Todo conflito deve produzir:

- evento estruturado;
- log;
- informação de paths envolvidos;
- hashes;
- timestamp;
- origem;
- razão do conflito.

---

# 14. Arquivos binários

Não tente merge textual de arquivos binários.

Defina estratégia como:

```text
um lado mudou → propagar

ambos mudaram →
    conflito
```

Nunca sobrescreva silenciosamente arquivos binários divergentes.

---

# 15. Inicialização do mirror

O primeiro uso precisa ser tratado separadamente.

Considere:

```text
MAIN contém arquivos
MOBILE está vazio
```

O comportamento esperado normalmente será:

```text
materializar projeção MAIN → MOBILE
```

Mas também considere:

```text
MAIN contém arquivos
MOBILE já contém arquivos
```

Neste caso NÃO assuma que o mirror pode ser sobrescrito.

Deve existir processo de:

```text
bootstrap
```

ou:

```text
reconcile
```

que compare os dois estados.

Projete comandos ou APIs como, por exemplo:

```text
watchflow mobile-mirror init
watchflow mobile-mirror status
watchflow mobile-mirror sync
watchflow mobile-mirror reconcile
```

Use os padrões de CLI já existentes no projeto, se houver.

---

# 16. Status e observabilidade

O usuário deve conseguir descobrir:

```text
quantos arquivos estão sincronizados
quantos estão divergentes
quantos estão em conflito
último sync
última alteração originada do main
última alteração originada do mobile
erros
```

Exemplo conceitual:

```text
$ watchflow mobile-mirror status

Mirror: Chaos-Mobile
Source: Chaos

Tracked:    4,821
Synced:     4,819
Pending:    1
Conflicts:  1

Last sync:
2026-09-05 15:41:02

Conflict:
100_Projetos/WatchFlow/ideias.md
```

Não implemente necessariamente essa UX exata; adapte ao projeto.

---

# 17. Atomicidade

Ao copiar/escrever arquivos, evite deixar arquivos parcialmente escritos.

Prefira estratégia:

```text
write temporary
fsync quando necessário
atomic rename
```

Avalie as garantias reais do filesystem utilizado.

---

# 18. Crash recovery

Considere interrupção do processo em momentos como:

```text
arquivo copiado
estado ainda não persistido
```

ou:

```text
estado persistido
arquivo ainda não copiado
```

O sistema deve conseguir se recuperar de forma determinística na próxima execução.

Avalie journal/transações quando necessário.

A prioridade deve ser:

```text
consistência > velocidade
```

---

# 19. Git e arquivos intermediários

O backend deve ser consciente de que o diretório mobile é também um repositório Git.

Não sincronize nem interprete como notas:

```text
.git/**
```

Considere eventos provocados por:

```text
git checkout
git pull
git merge
git reset
```

Um `git pull` pode alterar dezenas ou centenas de arquivos em sequência.

O backend deve lidar adequadamente com bursts de eventos.

Se apropriado, implemente:

```text
debounce
batching
transaction/coalescing
```

sem sacrificar consistência.

---

# 20. Arquitetura plugável

O backend deve respeitar a arquitetura de adapters/backends/providers já existente.

A intenção conceitual é:

```text
WatchFlow
│
├── watcher
├── event bus / queue
├── state
├── conflict resolution
│
└── backends
    ├── git
    ├── google-drive
    ├── onedrive
    └── mobile-mirror
```

NÃO refatore todo o WatchFlow apenas para acomodar este backend, salvo quando houver defeito arquitetural concreto que impeça a implementação.

Se alterações no core forem necessárias:

1. identifique-as;
2. justifique-as;
3. mantenha-as mínimas;
4. preserve compatibilidade.

---

# 21. Compatibilidade futura

A arquitetura deve permitir futuramente múltiplas projeções:

```text
Chaos
├── Chaos-Mobile
├── Chaos-Tablet
├── Chaos-Work
└── Chaos-ReadOnly
```

Cada projeção poderá possuir:

```text
filtros diferentes
políticas diferentes
roots diferentes
direções diferentes
```

Portanto, evite singleton global como:

```text
THE_MOBILE_DIR
```

Modele o conceito genérico de:

```text
projection / mirror
```

mesmo que inicialmente exista apenas `mobile-mirror`.

---

# 22. Políticas de direção

Preveja no desenho a possibilidade futura de configuração:

```yaml
direction: bidirectional
```

ou:

```yaml
direction: source-to-mirror
```

ou:

```yaml
direction: mirror-to-source
```

O MVP pode implementar prioritariamente:

```text
bidirectional
```

mas a arquitetura não deve tornar impossível adicionar as demais.

---

# 23. Requisitos de qualidade

O código deve ser:

- simples;
- modular;
- testável;
- determinístico;
- observável;
- seguro contra perda silenciosa de dados;
- compatível com os padrões já adotados no WatchFlow.

Evite:

- abstrações prematuras;
- frameworks novos sem necessidade;
- dependências grandes;
- serviços externos;
- polling quando eventos de filesystem já estiverem disponíveis;
- lógica escondida em scripts shell quando ela pertença ao domínio da aplicação.

---

# 24. Investigação obrigatória do repositório

ANTES de propor a implementação:

1. leia `README`;
2. leia `AGENTS.md` e equivalentes aplicáveis;
3. identifique linguagem e runtime;
4. analise estrutura do projeto;
5. encontre interfaces atuais de backend;
6. encontre watcher/file events;
7. identifique mecanismo de logging;
8. identifique configuração;
9. identifique testes;
10. identifique CLI;
11. identifique state management;
12. identifique mecanismo atual de conflitos, caso exista.

Não presuma arquitetura que não existe.

Sempre prefira integrar-se aos padrões existentes.

---

# 25. Entregável inicial obrigatório

NÃO comece implementando imediatamente.

Primeiro gere:

```text
docs/plans/mobile-mirror.md
```

ou o local equivalente adotado pelo repositório.

O documento deve conter obrigatoriamente:

## 1. Estado atual do WatchFlow

Explique apenas o necessário para contextualizar a implementação.

## 2. Problema

Descreva por que o mobile não deve simplesmente clonar todo o vault.

## 3. Objetivos

## 4. Não objetivos

## 5. Arquitetura proposta

Inclua diagrama ASCII.

## 6. Modelo de dados

Inclua estado de sincronização, hashes, tombstones e conflitos, quando aplicáveis.

## 7. Fluxos

Inclua:

```text
main → mobile
mobile → main
bootstrap
delete
rename
conflict
git pull burst
crash recovery
```

## 8. Máquina de estados ou algoritmo de reconciliação

Defina precisamente como BASE/MAIN/MOBILE são comparados.

## 9. Estratégia de prevenção de loops

## 10. Estratégia de filtros

## 11. Estratégia de conflito

## 12. Estratégia de recuperação de falhas

## 13. Alterações necessárias no core

Se nenhuma:

```text
Nenhuma.
```

## 14. Riscos

## 15. Decisões arquiteturais

Liste decisões relevantes e alternativas descartadas.

---

# 26. Plano de implementação em fases

Depois da especificação, produza um plano incremental.

Sugestão conceitual:

```text
Phase 0 — investigação e interfaces
Phase 1 — projection engine unidirecional
Phase 2 — estado persistente
Phase 3 — sincronização bidirecional
Phase 4 — deletes/renames
Phase 5 — conflitos
Phase 6 — observabilidade/CLI
Phase 7 — hardening e crash recovery
Phase 8 — integração Git/workflow mobile
```

Não siga essas fases cegamente.

Adapte após examinar o repositório.

Cada fase deve resultar em software funcional e testável.

---

# 27. Tickets

Transforme o plano em tickets pequenos.

Formato:

```markdown
### MM-001 — <título>

**Objetivo**

...

**Arquivos/componentes prováveis**

...

**Implementação**

...

**Critérios de aceitação**

- [ ] ...
- [ ] ...

**Testes**

- [ ] unitário:
- [ ] integração:
- [ ] regressão:

**Dependências**

...

**Riscos**

...
```

Um ticket deve, em regra, ser executável por um agente em uma única sessão de trabalho.

Evite tickets gigantes como:

```text
"Implementar sincronização"
```

Prefira unidades verificáveis.

---

# 28. Testes obrigatórios

Crie uma matriz de testes que inclua pelo menos:

### Sincronização básica

```text
main create → mobile
main modify → mobile
mobile create → main
mobile modify → main
```

### Delete

```text
main delete
mobile delete
delete vs modify
```

### Rename/move

```text
main rename
mobile rename
rename vs edit
```

### Conflitos

```text
main edit + mobile edit
main delete + mobile edit
mobile delete + main edit
binary conflict
```

### Filtros

```text
included
excluded
nested excluded
hidden
symlink
```

### Resiliência

```text
process crash
partial write
state corruption
filesystem unavailable
permission denied
```

### Git

Simule:

```text
git pull
```

alterando vários arquivos rapidamente.

Garanta que o sistema termine em estado correto independentemente da ordem dos eventos emitidos pelo watcher.

---

# 29. Testes baseados em propriedades

Avalie introduzir property-based tests para invariantes importantes.

Exemplos:

### Invariante 1

Após convergência sem conflito:

```text
hash(main) == hash(mobile) == base_hash
```

para arquivos incluídos.

### Invariante 2

Arquivos excluídos nunca são criados pelo backend no mirror.

### Invariante 3

Nenhuma operação pode escrever fora de:

```text
source_root
mirror_root
```

### Invariante 4

Processar o mesmo evento duas vezes deve ser idempotente.

### Invariante 5

Um restart em qualquer ponto deve eventualmente convergir para o mesmo estado que uma execução sem interrupção.

---

# 30. Critérios gerais de aceitação

O projeto estará funcional quando for possível demonstrar:

```text
1. usuário edita nota no PC
2. alteração aparece no Chaos-Mobile
3. alteração pode ser commitada/pushed normalmente

4. usuário edita nota simulando o celular em Chaos-Mobile
5. alteração retorna ao Chaos

6. scripts/skills/state definidos como excluídos nunca entram no mobile

7. conflito simultâneo não causa perda silenciosa

8. delete não sobrescreve alteração concorrente

9. restart durante operação não corrompe estado

10. alterações produzidas pelo próprio backend não geram loop infinito
```

---

# 31. Cenário real de referência

Considere como cenário principal:

```text
Vault principal:
~/Vaults/Chaos

Mirror:
~/Vaults/Chaos-Mobile
```

Exemplo no principal:

```text
Chaos/
├── 000_Inbox/
├── 100_Projetos/
│   └── WatchFlow/
│       ├── README.md
│       ├── arquitetura.md
│       ├── ideias.md
│       ├── scripts/
│       ├── workbench/
│       └── .codex/
├── 700_Financeiro/
├── 999_Sistema/
├── .agents/
├── .codex/
└── ...
```

Projeção desejada:

```text
Chaos-Mobile/
├── 000_Inbox/
├── 100_Projetos/
│   └── WatchFlow/
│       ├── README.md
│       ├── arquitetura.md
│       └── ideias.md
└── 700_Financeiro/
```

Por exemplo:

```text
README.md       → sincronizado
arquitetura.md → sincronizado
ideias.md       → sincronizado

scripts/        → excluído
workbench/      → excluído
.codex/         → excluído
.agents/        → excluído
```

---

# 32. Importante: não confundir filtro com `.gitignore`

O filtro do `mobile-mirror` pertence ao domínio do WatchFlow.

Não use `.gitignore` como mecanismo principal de definição da projeção.

O `.gitignore` responde:

> o que este repositório Git versiona?

O filtro do `mobile-mirror` responde:

> quais recursos do vault principal fazem parte desta projeção?

São conceitos distintos.

---

# 33. Não implemente sincronização por timestamp apenas

Não considere:

```text
mtime mais novo vence
```

como mecanismo de resolução.

Timestamps podem divergir entre:

- arquivosystems;
- ferramentas Git;
- cópias;
- dispositivos.

Use identidade de conteúdo/estado.

Timestamps podem servir apenas como metadado auxiliar.

---

# 34. Não permitir "last write wins" silencioso

A política:

```text
arquivo mais recente sobrescreve o outro
```

NÃO é aceitável como padrão para conflito.

A prioridade é evitar perda silenciosa de informação.

---

# 35. Compatibilidade com Obsidian

Considere peculiaridades do Obsidian:

```text
[[wikilinks]]
embeds
![[arquivo]]
renames
attachments
.canvas
.obsidian/
```

O backend não precisa interpretar semanticamente Markdown no MVP.

Entretanto, evite decisões arquiteturais que inviabilizem futuramente:

- detecção de dependências de anexos;
- inclusão automática de imagens referenciadas;
- atualização de links;
- projeções baseadas em graph traversal.

Documente isso como possível extensão.

---

# 36. Extensão futura: inclusão por dependência

Considere futuramente suporte a:

```yaml
attachments:
  strategy: referenced
```

onde somente anexos efetivamente referenciados por notas incluídas seriam enviados ao mobile.

Exemplo:

```markdown
![[arquitetura.png]]
```

faria:

```text
arquitetura.png
```

entrar automaticamente na projeção.

Isso NÃO precisa fazer parte do MVP, salvo se a arquitetura atual tornar isso trivial.

Mas a arquitetura escolhida não deve impedir essa evolução.

---

# 37. Extensão futura: políticas por tipo

A estrutura poderá futuramente aceitar:

```yaml
rules:
  markdown:
    direction: bidirectional

  images:
    direction: source-to-mirror

  pdf:
    direction: source-to-mirror
    max_size: 10MB

  scripts:
    include: false
```

Não implemente tudo agora.

Garanta apenas que o modelo arquitetural possa evoluir nessa direção.

---

# 38. Processo de execução solicitado

Execute nesta ordem:

```text
1. inspecionar repositório;
2. compreender arquitetura atual;
3. produzir especificação técnica;
4. produzir ADRs necessários;
5. produzir plano em fases;
6. produzir tickets;
7. produzir matriz de testes;
8. apontar riscos e dúvidas;
9. somente então iniciar implementação.
```

Após gerar o plano:

- revise-o criticamente;
- procure riscos de perda de dados;
- procure condições de corrida;
- procure ciclos;
- procure dependências desnecessárias;
- procure situações em que um `git pull` possa ser confundido com alterações produzidas pelo próprio mirror.

Corrija o plano antes de começar a implementação.

---

# 39. Implementação

Após finalizar e revisar o plano, execute os tickets em ordem.

Para cada ticket:

```text
1. implementar;
2. executar testes;
3. corrigir falhas;
4. executar suíte relevante;
5. registrar resultado;
6. somente então avançar.
```

Não acumule vários tickets sem testes intermediários.

---

# 40. Restrições

NÃO:

```text
- reescrever o projeto inteiro;
- substituir tecnologias existentes sem necessidade;
- introduzir banco de dados apenas por preferência pessoal;
- adicionar dependências sem justificar;
- usar last-write-wins silencioso;
- sobrescrever conflito;
- depender da API do GitHub;
- depender do Obsidian;
- depender do plugin Obsidian Git;
- colocar lógica específica do Android no backend;
- usar timestamps como fonte de verdade;
- tratar ausência como delete sem evidência;
- usar polling se a infraestrutura atual já fornece eventos adequados;
- misturar `.gitignore` com filtro de projeção;
- implementar primeiro e documentar depois.
```

---

# 41. Resultado esperado

Ao final, o WatchFlow deverá possuir uma abstração aproximadamente equivalente a:

```text
Projection
├── source
├── mirror
├── filters
├── state
├── reconcile()
├── propagate()
├── resolve_conflict()
└── status()
```

Os nomes concretos devem seguir as convenções do projeto.

O backend `mobile-mirror` será uma implementação/configuração dessa abstração.

O resultado final deve permitir:

```text
Chaos
   ⇅
WatchFlow mobile-mirror
   ⇅
Chaos-Mobile
   ⇅
Git
   ⇅
GitHub
   ⇅
Obsidian Git
   ⇅
Android
```

com:

```text
filtragem
sincronização bidirecional
detecção de conflitos
prevenção de loops
estado persistente
recuperação após falhas
nenhuma perda silenciosa de dados
```

---

# 42. Comando final

Agora:

1. inspecione integralmente a arquitetura relevante do repositório WatchFlow;
2. identifique os componentes reutilizáveis;
3. produza a especificação técnica do `mobile-mirror`;
4. produza o plano incremental de implementação;
5. decomponha o plano em tickets pequenos, testáveis e ordenados;
6. crie a matriz de testes;
7. identifique decisões que precisam de ADR;
8. revise o plano procurando riscos de integridade e perda de dados;
9. apresente um resumo da arquitetura final proposta;
10. em seguida, implemente o projeto fase por fase, executando e validando os testes após cada ticket.

Sempre privilegie a arquitetura e convenções já existentes no WatchFlow sobre as sugestões de nomenclatura deste documento.

Se alguma premissa deste prompt conflitar com a implementação real do projeto, não force a premissa: documente o conflito, explique a alternativa tecnicamente mais adequada e prossiga com a solução que melhor preserve a arquitetura do WatchFlow.