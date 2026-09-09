# 001 — Notificador desktop para Windows

**Tipo:** feature · **Prioridade:** alta · **Estado:** aberta
**Aberta em:** 2026-09-09 · **Área:** `internal/notify`

---

## Problema

`internal/notify/desktop.go` implementa o backend `desktop` sobre `notify-send`,
que é do libnotify e **não existe no Windows**. O `newDesktopNotifier()` faz
`exec.LookPath("notify-send")`, falha, e a fábrica em `notify.go` recai para o
notificador de log:

```
WARN backend 'desktop' indisponível; alertas seguirão apenas para o log
     error="'notify-send' não encontrado no PATH"
```

Nada quebra — a degradação é deliberada e correta. O problema é o que ela custa:

**Um conflito de merge não alcança o usuário.** O watcher entra em
`CONFLICT_HALTED`, para de sincronizar, e o único rastro é uma linha no
`watchflow.log`. É exatamente o cenário que a notificação existe para cobrir: o
README ("Invariantes de engenharia") define que conflito **interrompe e chama o
usuário**, e a metade "chama o usuário" não acontece no Windows.

Quem configura `on_conflict: true` e vê `enabled: true` acredita razoavelmente
que será avisado. Hoje não é — e descobre quando reparar que o cofre parou de
sincronizar há dias.

---

## O que foi medido

Nesta máquina (Windows 11 build 26200, PowerShell 5.1.26100), não presumido:

| Verificação | Resultado |
|---|---|
| Toast WinRT via PowerShell, com AUMID emprestado do próprio PowerShell | **apareceu na tela**, confirmado por observação humana |
| Latência do envio | 109 ms a frio, **51 ms** com os assemblies WinRT já carregados |
| Custo de subir um `powershell -NoProfile` vazio | **141 ms** |
| Módulo `BurntToast` instalado | **não** |
| Pasta `Programs` do Menu Iniciar do usuário gravável sem admin | **sim** |
| `user32.dll` (fallback `MessageBox`) | presente |

Custo total estimado por alerta na abordagem com PowerShell: **~250 ms**,
confortável dentro do `sendTimeout` de 5 s definido em `notify.go`.

**O toast aparece sem registro prévio de AUMID.** Isso rebaixa a Fase 3 de
pré-requisito para polimento de identidade, e é o que permite planejar a Fase 2
como entregável independente.

### O acento saiu quebrado — e isso vira requisito

A primeira sonda exibiu o texto com caracteres corrompidos. A causa não é do
mecanismo de toast: o arquivo `.ps1` da sonda foi gravado em UTF-8 **sem BOM**,
e o PowerShell 5.1 lê um script sem BOM como ANSI (CP1252). Regravado com BOM,
o mesmo toast saiu correto.

É a mesma raiz do defeito já corrigido em `scripts/install.ps1`, e por isso o CI
tem o job `powershell` exigindo BOM em todo `.ps1`. Vale registrar aqui porque a
implementação **não** deve depender de um `.ps1` em disco justamente por causa
disso — e o desenho escolhido não depende: `-EncodedCommand` transporta o script
como base64 de UTF-16LE, e as variáveis de ambiente do processo filho são
convertidas para UTF-16 pelo `os/exec` do Go. Nenhuma das duas rotas passa por
uma heurística de codepage.

Se em algum momento a implementação escorregar para "escrever um `.ps1`
temporário e executá-lo", este é o defeito que volta — em texto de conflito com
acento, que é o caso mais provável num cofre em português.

---

## Restrições que o desenho precisa respeitar

Vêm do código e da plataforma; ignorar qualquer uma delas invalida a solução.

1. **Best-effort, sempre.** `notify.go` documenta que uma falha de notificação
   nunca interrompe nem altera o resultado de um pipeline. O backend novo não
   pode retornar erro que escape para o pipeline nem bloquear além do
   `sendTimeout` de 5 s.

2. **O daemon roda sem console.** Desde `cmd/watchflow/detach_windows.go`, um
   processo de console iniciado por ele ganharia uma janela nova. Qualquer
   `powershell` que este backend dispare **precisa** passar por
   `internal/proc.HideConsole` — do contrário volta o problema das janelas
   piscando.

3. **A tarefa do Agendador roda em sessão interativa, não S4U.** Foi escolha
   explícita em `scripts/install.ps1` justamente para que a notificação alcance a
   área de trabalho. Uma mudança futura para S4U mataria este recurso — o
   comentário lá precisa continuar dizendo isso.

4. **Sem interpolação em shell.** O README é categórico, e aqui a tentação é
   real: o conteúdo do alerta inclui caminhos de arquivo e saída do Git, texto
   que o usuário não controla. Montar XML dentro de um `-Command` seria
   interpolar dado não confiável em código. Ver [Injeção](#injeção-o-detalhe-que-decide-a-implementação).

5. **A deduplicação já existe.** O `dispatcher` suprime alerta idêntico por 30
   min (`repeatCooldown`). O backend não precisa se preocupar com enxurrada.

6. **A costura de teste precisa sobreviver.** `desktopNotifier.run` é injetável
   para permitir teste sem sessão gráfica (`desktop.go:20`), e
   `notify_test.go` já exercita urgência por tipo de evento
   (`TestDesktopUrgencyForConflictIsCritical`). O equivalente Windows tem de ser
   testável do mesmo jeito.

7. **Um `config.yaml` só para as duas plataformas.** Decisão já adotada no
   projeto. O backend continua se chamando `desktop`; **não** se cria um
   `backend: "toast"`. A escolha da implementação é por `GOOS`, não por
   configuração.

---

## Opções consideradas

| # | Abordagem | Dependências | Latência | Contras |
|---|---|---|---|---|
| A | PowerShell + WinRT, AUMID emprestado do PowerShell | nenhuma | ~250 ms | Toast aparece como "Windows PowerShell"; identidade errada |
| **B** | **PowerShell + WinRT, AUMID próprio + atalho no Menu Iniciar** | **nenhuma** | **~250 ms** | Exige criar atalho na instalação |
| C | WinRT nativo via COM do Go (`x/sys/windows`) | promove `x/sys` a direta | ~0 ms | Volume grande de `unsafe`/COM, difícil de testar, difícil de manter |
| D | Biblioteca Go (`go-toast` e similares) | +1 dependência direta | ~250 ms | Faz internamente o mesmo que B, com menos controle |
| E | `MessageBox` via `user32.dll` | nenhuma | ~0 ms | Modal e intrusivo vindo de um daemon; bloqueia thread |

**Recomendação: B.**

A diferença entre A e B é só o registro de um atalho, e ela decide se o alerta
se identifica como WatchFlow ou como PowerShell — num alerta crítico, saber quem
está falando é parte da mensagem. C é a solução tecnicamente mais limpa e a
mais cara: troca ~250 ms, que já cabem no orçamento, por um bloco de COM que
ninguém vai querer manter. D não compra nada sobre B. E fica descartada como
backend principal: um modal disparado por um processo de segundo plano rouba o
foco do usuário no meio do trabalho, que é o oposto do que se quer.

---

## Injeção: o detalhe que decide a implementação

O jeito ingênuo — montar a string XML e passá-la em
`powershell -Command "... $xml ..."` — **viola a invariante "sem shell"** do
README. O título e o detalhe do alerta carregam caminho de arquivo e saída de
comando Git; um nome de arquivo com aspas ou `$(...)` vira execução.

A solução não é escapar melhor, é **não interpolar**:

1. O script PowerShell é **constante**, embutido no binário, e vai por
   `-EncodedCommand` (base64 UTF-16LE), o que também elimina o problema de
   aspas na linha de comando.
2. O conteúdo variável trafega em **variáveis de ambiente** do processo filho
   (`WF_TOAST_TITLE`, `WF_TOAST_BODY`, `WF_TOAST_SCENARIO`), que o script lê com
   `$env:WF_TOAST_TITLE`.
3. O script faz a escapada XML dele mesmo, via `XmlDocument`/`CreateTextNode`,
   em vez de concatenar texto no template.

Assim nenhum dado do usuário chega a ser código, em nenhuma camada.

---

## Plano de implementação

### Fase 1 — Separar o backend por plataforma

Sem mudança de comportamento; só abre espaço.

- `internal/notify/desktop.go` → mantém o que é comum: `Notification`, a
  tabela de urgência (`urgencyFor`, `expireForKind`), `truncate`, e a interface
  do backend.
- `internal/notify/desktop_unix.go` (`//go:build !windows`) → recebe o código
  atual de `notify-send`, intacto.
- `internal/notify/desktop_windows.go` (`//go:build windows`) → passa a ser o
  destino de `newDesktopNotifier()`.

**Critério de saída:** suíte verde nas duas plataformas, `go vet` limpo para
`GOOS=linux` e `GOOS=windows`, nenhum comportamento alterado no Linux.

### Fase 2 — O toast

Em `desktop_windows.go`:

- `newDesktopNotifier()` faz `exec.LookPath("powershell")` e falha com mensagem
  clara se ausente, para que a fábrica recaia no log como já faz hoje.
- Mantém o campo `run` injetável, com a mesma assinatura, para preservar a
  costura de teste.
- Monta o payload como descrito em [Injeção](#injeção-o-detalhe-que-decide-a-implementação).
- Aplica `proc.HideConsole` — obrigatório, ver restrição 2.
- Mapeia a urgência para o toast: `KindConflict` → `scenario="urgent"`, que não
  expira sozinho, coerente com o `expire-time=0` já usado no Linux.
- Respeita o `ctx`: `exec.CommandContext`, sem espera própria.

**Critério de saída:** um conflito real produz toast visível; um erro produz
toast comum; um sucesso não produz toast (já filtrado pelo `dispatcher`).

### Fase 3 — Identidade (AUMID)

- Definir a constante `watchflowAUMID = "WatchFlow.Daemon"` compartilhada entre
  o Go e o `install.ps1`.
- `scripts/install.ps1` cria um atalho `WatchFlow.lnk` em
  `[Environment]::GetFolderPath('Programs')` apontando para o binário instalado,
  com a propriedade `System.AppUserModel.ID` definida. É gravável **sem admin**,
  já verificado — e portanto não fere a restrição rígida do projeto.
- `scripts/uninstall.ps1` remove o atalho.
- Se o atalho não existir, o backend ainda funciona emprestando o AUMID do
  PowerShell (opção A): degradação de identidade, não de função.

**Critério de saída:** o toast se identifica como "WatchFlow"; desinstalar não
deixa atalho órfão.

### Fase 4 — Tornar o diagnóstico visível

Independente das fases acima e útil mesmo sem elas.

- Novo check em `cmd/watchflow/doctor.go`: `checkNotificationBackend(cfg)`,
  que avisa quando o backend configurado está indisponível — hoje esse fato só
  existe como uma linha `WARN` no log, que ninguém lê antes de precisar.
- A mensagem precisa dizer a consequência, não só o fato: "alertas de conflito
  não chegarão à área de trabalho".

**Critério de saída:** `watchflow doctor` reporta WARN quando
`backend: desktop` está configurado e indisponível.

### Fase 5 — Testes

- Reaproveitar o padrão de `notify_test.go`: injetar `run` e afirmar sobre os
  argumentos, sem sessão gráfica.
- Testes específicos, com `//go:build windows`:
  - o comando é `powershell` com `-EncodedCommand`, e **nenhum** dado do alerta
    aparece na linha de comando;
  - título e corpo chegam por variável de ambiente;
  - um título contendo `"`, `<`, `&` e `$(...)` não altera a estrutura do
    comando;
  - `KindConflict` produz cenário que não expira.
- Teste multiplataforma: `newDesktopNotifier()` devolve erro utilizável quando o
  executável não existe (garante a degradação para log).

### Fase 6 — Documentação

- README, seção de notificações: registrar que no Windows o backend `desktop`
  usa toast nativo, que a identidade depende do atalho criado pelo instalador, e
  que o Assistente de Foco pode suprimir o alerta — caso em que `watchflow
  status` continua mostrando `CONFLICT_HALTED`.
- Remover das "Limitações conhecidas" a ressalva de notificação, quando fechar.

---

## Critérios de aceitação

1. Com `enabled: true`, `on_conflict: true` e `backend: "desktop"` no Windows, um
   conflito de merge real produz um toast visível na área de trabalho.
2. O toast se identifica como WatchFlow, não como PowerShell.
3. Nenhuma janela de console pisca ao notificar (regressão do `--detach`).
4. Nenhum dado do alerta é interpolado em linha de comando ou em código
   PowerShell, comprovado por teste com metacaracteres.
5. Sem PowerShell disponível, o daemon degrada para log e avisa, como hoje.
6. O Linux não muda em nada: mesma suíte, mesmo `notify-send`, mesmo
   `config.yaml`.
7. `watchflow doctor` avisa quando o backend configurado está indisponível.

---

## Não faz parte desta issue

- Notificações no macOS (`osascript`/`terminal-notifier`): a plataforma não é
  suportada nem avaliada.
- Ações dentro do toast (botões "Abrir cofre", "Ver log"). Exigiriam um
  protocolo COM ativado e um handler registrado — vale como issue própria, se a
  notificação simples se mostrar insuficiente.
- Substituir o backend `webhook`, que é ortogonal e já funciona.

---

## Perguntas em aberto

1. ~~A sonda apareceu na tela?~~ **Respondida:** sim, com o AUMID emprestado do
   PowerShell e sem registro prévio. O texto saiu corrompido, mas por
   codificação do script da sonda, não pelo mecanismo — ver
   [O acento saiu quebrado](#o-acento-saiu-quebrado--e-isso-vira-requisito).
2. **Vale um fallback para conflito?** Se o toast puder ser suprimido pelo
   Assistente de Foco, um alerta que exige ação humana talvez mereça um segundo
   canal — `MessageBox` (opção E) apenas para `KindConflict`, ou o backend
   `webhook` em paralelo. Decisão de produto, não técnica.
3. **PowerShell 7 (`pwsh`)?** O plano assume `powershell.exe` 5.1, presente em
   toda instalação do Windows. Procurar `pwsh` primeiro custa pouco, mas só faz
   diferença se houver máquina sem o PowerShell clássico — o que não é o caso
   hoje.
