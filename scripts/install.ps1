<#
.SYNOPSIS
    Instalador do WatchFlow para Windows.

.DESCRIPTION
    Equivalente de scripts/install.sh: compila o binário, instala no PATH do
    usuário, prepara a configuração inicial e registra a tarefa do Agendador
    que sobe o daemon no logon.

    O script é idempotente: rodá-lo de novo atualiza o binário sem tocar na sua
    configuração nem no estado do daemon.

    Nada aqui exige privilégio de administrador. A tarefa do Agendador é
    registrada por-usuário (RunLevel Limited) e todos os caminhos ficam dentro
    do perfil do usuário — mesma garantia que o instalador do Linux dá ao se
    confinar ao $HOME.

.PARAMETER Prefix
    Onde instalar o binário. Padrão: %LOCALAPPDATA%\Programs\WatchFlow

.PARAMETER NoService
    Não registra a tarefa no Agendador de Tarefas.

.PARAMETER NoBuild
    Usa o binário já existente em bin\ em vez de recompilar.

.PARAMETER Yes
    Não faz perguntas; assume "sim" para tudo.

.EXAMPLE
    powershell -ExecutionPolicy Bypass -File scripts\install.ps1
#>
[CmdletBinding()]
param(
    [string]$Prefix = "$env:LOCALAPPDATA\Programs\WatchFlow",
    [switch]$NoService,
    [switch]$NoBuild,
    [switch]$Yes
)

$ErrorActionPreference = 'Stop'

# ---------------------------------------------------------------------------
# Caminhos
# ---------------------------------------------------------------------------

# A configuração fica em ~/.config/watchflow, e não em %APPDATA%, de propósito:
# o mesmo config.yaml serve nas duas plataformas, que é o modelo do projeto.
# Ver README, "Limitações conhecidas".
$RepoRoot   = Split-Path -Parent $PSScriptRoot
$ConfigDir  = Join-Path $HOME '.config\watchflow'
$ConfigFile = Join-Path $ConfigDir 'config.yaml'
$StateDir   = Join-Path $HOME '.local\state\watchflow'
$BinaryName = 'watchflow.exe'
$TaskName   = 'WatchFlow'   # precisa casar com scheduledTaskName em doctor_windows_checks.go

# ---------------------------------------------------------------------------
# Saída
# ---------------------------------------------------------------------------

function Write-Step { param([string]$m) Write-Host "==> $m" -ForegroundColor White }
function Write-Ok   { param([string]$m) Write-Host "  [ok] $m" -ForegroundColor Green }
function Write-Warn2{ param([string]$m) Write-Host "  [!] $m" -ForegroundColor Yellow }
function Write-Skip { param([string]$m) Write-Host "  [.] $m" -ForegroundColor DarkGray }
function Die        { param([string]$m) Write-Host "erro: $m" -ForegroundColor Red; exit 1 }

function Confirm-Step {
    param([string]$Question)
    if ($Yes) { return $true }
    $answer = Read-Host "$Question [s/N]"
    return $answer -match '^[sSyY]'
}

# ---------------------------------------------------------------------------
# 1. Pré-requisitos
# ---------------------------------------------------------------------------

Write-Step 'Verificando pré-requisitos'

$git = Get-Command git -ErrorAction SilentlyContinue
if (-not $git) {
    Die @'
git não encontrado no PATH. Instale o Git for Windows:

    winget install --id Git.Git -e

Ou baixe em https://git-scm.com/download/win
'@
}
Write-Ok "git $((git --version) -replace '^git version ')"

# A versão exigida vem do go.mod para que esta mensagem não fique defasada.
$RequiredGo = (Select-String -Path (Join-Path $RepoRoot 'go.mod') -Pattern '^go (\d+\.\d+(\.\d+)?)').Matches[0].Groups[1].Value

function Test-VersionLess {
    param([string]$Have, [string]$Want)
    # Normaliza para três campos: "1.24" não pode ser considerado menor que "1.24.0".
    while (($Have -split '\.').Count -lt 3) { $Have = "$Have.0" }
    while (($Want -split '\.').Count -lt 3) { $Want = "$Want.0" }
    return ([version]$Have) -lt ([version]$Want)
}

if (-not $NoBuild) {
    $go = Get-Command go -ErrorAction SilentlyContinue
    if (-not $go) {
        Die @"
go não encontrado no PATH.

  Este projeto exige Go $RequiredGo ou superior. Instale sem privilégio de
  administrador extraindo o ZIP oficial no perfil do usuário:

    Invoke-WebRequest https://go.dev/dl/go$RequiredGo.windows-amd64.zip -OutFile `$env:TEMP\go.zip
    Expand-Archive `$env:TEMP\go.zip -DestinationPath `$env:LOCALAPPDATA\Programs
    `$env:PATH = "`$env:LOCALAPPDATA\Programs\Go\bin;`$env:PATH"

  Versões mais recentes estão em https://go.dev/dl/

  Alternativa: se você já tem o binário compilado em bin\, rode este script com
  -NoBuild e o Go não será necessário.
"@
    }

    # Presença não basta: compilar com um Go antigo falha com um erro do
    # compilador bem menos claro do que este aviso.
    $GoVersion = ((go version) -split ' ')[2] -replace '^go'
    if (Test-VersionLess -Have $GoVersion -Want $RequiredGo) {
        Die "go $GoVersion instalado, mas este projeto exige $RequiredGo ou superior. Veja https://go.dev/dl/"
    }
    Write-Ok "go $GoVersion (exigido: $RequiredGo+)"
}

# ---------------------------------------------------------------------------
# 2. Compilação
# ---------------------------------------------------------------------------

$BuiltBinary = Join-Path $RepoRoot "bin\$BinaryName"

if (-not $NoBuild) {
    Write-Step 'Compilando'

    # Não usa 'make': o Makefile depende de utilitários POSIX (mkdir -p, date,
    # sed) que não existem no PowerShell. As flags são as mesmas.
    $version = (git -C $RepoRoot describe --tags --always --dirty 2>$null)
    if (-not $version) { $version = '0.1.0-dev' }
    $commit = (git -C $RepoRoot rev-parse --short HEAD 2>$null)
    if (-not $commit) { $commit = 'none' }
    $date = (Get-Date).ToUniversalTime().ToString('yyyy-MM-ddTHH:mm:ssZ')

    New-Item -ItemType Directory -Force (Join-Path $RepoRoot 'bin') | Out-Null
    $ldflags = "-X main.version=$version -X main.commit=$commit -X main.date=$date -s -w"

    Push-Location $RepoRoot
    try {
        $env:CGO_ENABLED = '0'
        & go build -ldflags $ldflags -o $BuiltBinary ./cmd/watchflow
        if ($LASTEXITCODE -ne 0) { Die "a compilação falhou. Rode 'go build ./...' para ver o erro." }
    } finally {
        Pop-Location
    }
    Write-Ok "binário gerado em bin\$BinaryName (v$version)"
} else {
    if (-not (Test-Path $BuiltBinary)) { Die "-NoBuild informado, mas não há binário em bin\$BinaryName" }
    Write-Skip 'compilação pulada (-NoBuild)'
}

# ---------------------------------------------------------------------------
# 3. Instalação do binário
# ---------------------------------------------------------------------------

Write-Step 'Instalando o binário'

New-Item -ItemType Directory -Force $Prefix | Out-Null
$InstalledBinary = Join-Path $Prefix $BinaryName

# Um daemon rodando mantém o .exe aberto, e no Windows isso impede a
# sobrescrita — diferente do Linux, onde o install sobre um binário em uso
# funciona. Parar o daemon antes é o que torna o script realmente idempotente.
if (Test-Path $InstalledBinary) {
    try {
        & $InstalledBinary stop 2>&1 | Out-Null
        Start-Sleep -Milliseconds 500
    } catch {
        # Daemon não estava rodando: seguir em frente.
    }
}

Copy-Item $BuiltBinary $InstalledBinary -Force
Write-Ok $InstalledBinary

# O desinstalador é autocontido: não referencia o repositório em ponto algum.
# Instalá-lo junto do binário resolve o caso de o usuário apagar o clone depois.
# O $Prefix real é gravado no default do parâmetro — equivalente ao sed que o
# install.sh faz —, para que a cópia instalada funcione mesmo com -Prefix.
$UninstallerSrc = Join-Path $PSScriptRoot 'uninstall.ps1'
$UninstallerDst = Join-Path $Prefix 'watchflow-uninstall.ps1'
if (Test-Path $UninstallerSrc) {
    (Get-Content $UninstallerSrc -Raw) -replace `
        '(?m)^(\s*\[string\]\$Prefix\s*=\s*).*$', "`$1`"$Prefix`"," `
        | Set-Content $UninstallerDst -Encoding UTF8
    Write-Ok $UninstallerDst
} else {
    Write-Warn2 "scripts\uninstall.ps1 não encontrado; desinstalador não instalado"
}

# PATH do usuário (HKCU), nunca o do sistema: alterar o do sistema exigiria
# administrador.
$UserPath = [Environment]::GetEnvironmentVariable('Path', 'User')
if ($UserPath -notlike "*$Prefix*") {
    [Environment]::SetEnvironmentVariable('Path', "$Prefix;$UserPath", 'User')
    $env:PATH = "$Prefix;$env:PATH"
    Write-Ok "$Prefix adicionado ao PATH do usuário"
    Write-Warn2 'Abra um novo terminal para que o PATH atualizado valha nele.'
} else {
    Write-Skip "$Prefix já está no PATH do usuário"
}

# ---------------------------------------------------------------------------
# 4. Configuração
# ---------------------------------------------------------------------------

Write-Step 'Preparando a configuração'

New-Item -ItemType Directory -Force $ConfigDir | Out-Null
New-Item -ItemType Directory -Force $StateDir | Out-Null

$ConfigIsNew = $false
if (Test-Path $ConfigFile) {
    # Sobrescrever apagaria os watchers que o usuário já configurou.
    Write-Skip "configuração já existe, mantida intacta: $ConfigFile"
} else {
    Copy-Item (Join-Path $RepoRoot 'configs\watchflow.example.yaml') $ConfigFile
    $ConfigIsNew = $true
    Write-Ok "configuração inicial criada: $ConfigFile"
    # O socket_path do exemplo usa a convenção /run/user/${UID} do Linux. No
    # Windows o WatchFlow já a ignora e cai no state_dir, mas deixar o valor
    # correto no arquivo evita a dúvida de quem for lê-lo.
    $sock = (Join-Path $StateDir 'watchflow.sock') -replace '\\', '/'
    (Get-Content $ConfigFile -Raw) `
        -replace '(?m)^(\s*socket_path:).*$', "`$1 `"$sock`"" `
        | Set-Content $ConfigFile -Encoding UTF8 -NoNewline
    Write-Ok "socket_path ajustado para o padrão do Windows"
}

# No Windows os bits 0700/0600 são no-op: o diretório de estado herda a ACL do
# perfil do usuário, que já é privada. Ver README.

# ---------------------------------------------------------------------------
# 5. Tarefa do Agendador
# ---------------------------------------------------------------------------

if (-not $NoService) {
    Write-Step 'Registrando a tarefa do Agendador'

    $existing = Get-ScheduledTask -TaskName $TaskName -ErrorAction SilentlyContinue

    if ($ConfigIsNew -and -not $existing) {
        # Subir o daemon com o exemplo apontando para uma pasta inexistente o
        # faria falhar em laço de reinício — mesma cautela do install.sh.
        Write-Skip 'tarefa não registrada ainda — edite a configuração primeiro'
    } else {
        $register = $true
        if (-not $existing -and -not (Confirm-Step 'Registrar e iniciar a tarefa agora?')) {
            $register = $false
        }

        if ($register) {
            # '--detach', e não '-f': o watchflow.exe é um binário do subsistema
            # de console, e o Agendador em sessão interativa lhe daria uma janela
            # de console de verdade. O usuário fecha essa janela sem imaginar que
            # ela é o daemon, e derruba a sincronização. Com --detach o processo
            # que a tarefa inicia apenas relança o daemon sem console e sai.
            #
            # A sessão continua sendo a interativa (e não S4U) de propósito: é o
            # que permite a notificação de conflito chegar à área de trabalho, e
            # conflito é exatamente o caso que exige a atenção do usuário.
            $action = New-ScheduledTaskAction -Execute $InstalledBinary -Argument 'start --detach'
            $trigger = New-ScheduledTaskTrigger -AtLogOn -User "$env:USERDOMAIN\$env:USERNAME"
            # ExecutionTimeLimit zero: mesmo com o lançador saindo rápido, nada
            # aqui pode ser morto por tempo.
            #
            # Sem RestartCount: como a tarefa termina assim que o lançador sai,
            # o Agendador não fica supervisionando o daemon, e uma política de
            # reinício só dispararia se o próprio lançador falhasse. Quem protege
            # contra instância duplicada é a trava de socket do daemon.
            $settings = New-ScheduledTaskSettingsSet `
                -AllowStartIfOnBatteries `
                -DontStopIfGoingOnBatteries `
                -ExecutionTimeLimit ([TimeSpan]::Zero) `
                -StartWhenAvailable

            try {
                Register-ScheduledTask -TaskName $TaskName `
                    -Action $action -Trigger $trigger -Settings $settings `
                    -Description 'WatchFlow — sincronização autônoma de cofres via Git' `
                    -Force | Out-Null

                if ($existing) {
                    Write-Ok 'tarefa já existia; atualizada para o binário novo'
                    Start-ScheduledTask -TaskName $TaskName
                    Write-Ok 'daemon reiniciado'
                } else {
                    Write-Ok "tarefa '$TaskName' registrada (logon do usuário, sem elevação)"
                    Start-ScheduledTask -TaskName $TaskName
                    Write-Ok 'daemon iniciado'
                }
            } catch {
                # A partir daqui o binário já está instalado: uma falha no
                # Agendador não pode deixar a instalação pela metade.
                Write-Warn2 "falha ao registrar a tarefa: $($_.Exception.Message)"
                Write-Warn2 "Você ainda pode rodar manualmente: $BinaryName start"
            }
        } else {
            Write-Skip 'tarefa não registrada'
        }
    }
} else {
    Write-Skip 'registro da tarefa pulado (-NoService)'
}

# ---------------------------------------------------------------------------
# 6. Próximos passos
# ---------------------------------------------------------------------------

Write-Host ''
Write-Step 'Instalação concluída'
Write-Host ''

if ($ConfigIsNew) {
    Write-Host @"
A configuração é feita editando o arquivo YAML — não há assistente interativo.
O arquivo criado é o exemplo comentado; ajuste-o antes de usar.

  1. Edite a configuração (troque 'path' pela pasta que quer sincronizar):

       notepad $ConfigFile

  2. Confira se ficou válida:

       watchflow config validate

  3. Verifique o ambiente (git, permissões, caminhos longos, fim de linha):

       watchflow doctor

  4. Registre a tarefa que sobe o daemon no logon:

       powershell -ExecutionPolicy Bypass -File scripts\install.ps1

A pasta que você indicar em 'path' precisa já ser um repositório Git com um
remoto configurado.

Se o cofre for compartilhado com uma máquina Linux, versione um '.gitattributes'
com '* text=auto eol=lf' — o 'watchflow doctor' avisa quando falta.
"@
} else {
    Write-Host @"
Binário atualizado. Sua configuração e seu estado não foram tocados.

  watchflow status            estado das pastas monitoradas
  watchflow tui               painel interativo
  watchflow logs -f           acompanhar os eventos
  watchflow doctor            diagnóstico do ambiente

Para remover, de qualquer diretório:

  & '$Prefix\watchflow-uninstall.ps1'

Ele preserva sua configuração e seu estado; use -Purge para apagá-los também.
Não é preciso guardar este repositório: o desinstalador foi copiado junto do
binário e funciona sozinho.
"@
}

Write-Host ''
