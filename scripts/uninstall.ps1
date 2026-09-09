<#
.SYNOPSIS
    Desinstalador do WatchFlow para Windows.

.DESCRIPTION
    Remove a tarefa do Agendador e o binário. Por padrão PRESERVA sua
    configuração e o estado do daemon — use -Purge para apagá-los também.

    Em nenhuma hipótese este script toca nas pastas que você monitorava nem nos
    repositórios Git dentro delas.

    Sem -Purge, sua configuração e o histórico do daemon são preservados, de
    modo que reinstalar depois devolve tudo como estava.

.PARAMETER Prefix
    Onde o binário foi instalado. O install.ps1 grava o prefixo real nesta
    linha ao copiar o desinstalador, então a cópia instalada já vem com o valor
    correto mesmo que a instalação tenha usado -Prefix.

.PARAMETER Purge
    Também apaga a configuração e o estado do daemon.

.PARAMETER Yes
    Não faz perguntas; assume "sim" para tudo.

.EXAMPLE
    watchflow-uninstall.ps1
    watchflow-uninstall.ps1 -Purge
#>
[CmdletBinding()]
param(
    [string]$Prefix = "$env:LOCALAPPDATA\Programs\WatchFlow",
    [switch]$Purge,
    [switch]$Yes
)

$ErrorActionPreference = 'Stop'

$ConfigDir  = Join-Path $HOME '.config\watchflow'
$StateDir   = Join-Path $HOME '.local\state\watchflow'
$BinaryName = 'watchflow.exe'
$TaskName   = 'WatchFlow'

function Write-Step { param([string]$m) Write-Host "==> $m" -ForegroundColor White }
function Write-Ok   { param([string]$m) Write-Host "  [ok] $m" -ForegroundColor Green }
function Write-Warn2{ param([string]$m) Write-Host "  [!] $m" -ForegroundColor Yellow }
function Write-Skip { param([string]$m) Write-Host "  [.] $m" -ForegroundColor DarkGray }

function Confirm-Step {
    param([string]$Question)
    if ($Yes) { return $true }
    # Sem console interativo, o padrão seguro é não apagar nada.
    if ([Console]::IsInputRedirected) { return $false }
    return (Read-Host "  $Question [s/N]") -match '^[sSyY]'
}

$InstalledBinary = Join-Path $Prefix $BinaryName

# ---------------------------------------------------------------------------
# 1. Tarefa do Agendador
# ---------------------------------------------------------------------------

Write-Step 'Parando o daemon'

# Encerramento ordenado antes de matar a tarefa: o daemon devolve à fila os
# jobs em voo. É também o que libera o .exe, que o Windows mantém bloqueado
# enquanto o processo vive — sem isso a remoção do binário falharia.
if (Test-Path $InstalledBinary) {
    try {
        & $InstalledBinary stop 2>&1 | Out-Null
        Start-Sleep -Milliseconds 500
        Write-Ok 'daemon encerrado graciosamente'
    } catch {
        Write-Skip 'daemon não estava em execução'
    }
} else {
    Write-Skip "nada em $InstalledBinary"
}

$task = Get-ScheduledTask -TaskName $TaskName -ErrorAction SilentlyContinue
if ($task) {
    try {
        Stop-ScheduledTask -TaskName $TaskName -ErrorAction SilentlyContinue
        Unregister-ScheduledTask -TaskName $TaskName -Confirm:$false
        Write-Ok "tarefa '$TaskName' removida do Agendador"
    } catch {
        Write-Warn2 "falha ao remover a tarefa: $($_.Exception.Message)"
    }
} else {
    Write-Skip 'nenhuma tarefa registrada no Agendador'
}

# Um daemon iniciado à mão não morre com a tarefa.
$avulso = Get-Process -Name 'watchflow' -ErrorAction SilentlyContinue
if ($avulso) {
    Write-Warn2 "ainda há $($avulso.Count) processo(s) 'watchflow' em execução (PID: $($avulso.Id -join ', '))"
    if (Confirm-Step 'Encerrar agora?') {
        $avulso | Stop-Process -Force
        Write-Ok 'processos encerrados'
    }
}

# ---------------------------------------------------------------------------
# 2. Binário
# ---------------------------------------------------------------------------

Write-Step 'Removendo o binário'

if (Test-Path $InstalledBinary) {
    try {
        Remove-Item $InstalledBinary -Force
        Write-Ok $InstalledBinary
    } catch {
        # Diferente do Linux, o Windows recusa remover um arquivo em uso. Se o
        # stop acima não bastou, dizer isso é mais útil do que falhar seco.
        Write-Warn2 "não foi possível remover '$InstalledBinary': $($_.Exception.Message)"
        Write-Warn2 'Feche qualquer processo watchflow e rode este script de novo.'
    }
} else {
    Write-Skip "nada em $InstalledBinary"
}

# Um binário fora do prefixo esperado passaria despercebido.
$outro = Get-Command watchflow -ErrorAction SilentlyContinue
if ($outro -and $outro.Source -ne $InstalledBinary) {
    Write-Warn2 "ainda existe um 'watchflow' em $($outro.Source) — remova-o à mão se quiser."
}

# ---------------------------------------------------------------------------
# 3. PATH do usuário
# ---------------------------------------------------------------------------
#
# Isto não tem equivalente no uninstall.sh: no Linux o instalador apenas sugere
# uma linha para o shell rc, enquanto aqui ele de fato escreveu em HKCU. Deixar
# a entrada para trás sujaria o PATH do usuário permanentemente.

Write-Step 'Restaurando o PATH do usuário'

$userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
if ($userPath -and $userPath -like "*$Prefix*") {
    $limpo = ($userPath -split ';' | Where-Object { $_ -and $_.TrimEnd('\') -ne $Prefix.TrimEnd('\') }) -join ';'
    [Environment]::SetEnvironmentVariable('Path', $limpo, 'User')
    Write-Ok "'$Prefix' removido do PATH do usuário"
} else {
    Write-Skip 'PATH do usuário não mencionava o prefixo'
}

# O diretório da instalação, se ficou vazio.
if ((Test-Path $Prefix) -and -not (Get-ChildItem $Prefix -Force | Where-Object { $_.Name -notlike '*uninstall*' })) {
    Write-Skip "$Prefix ficará vazio após a autoexclusão do desinstalador"
}

# ---------------------------------------------------------------------------
# 4. Dados do usuário
# ---------------------------------------------------------------------------

if ($Purge) {
    Write-Step 'Removendo configuração e estado (-Purge)'
    Write-Host ''
    Write-Host '  Isto apaga:' -ForegroundColor White
    if (Test-Path $ConfigDir) { Write-Host "    $ConfigDir (sua configuração)" }
    if (Test-Path $StateDir)  { Write-Host "    $StateDir (fila, histórico e logs)" }
    Write-Host '  Suas pastas monitoradas e os repositórios Git nelas NÃO são tocados.' -ForegroundColor DarkGray
    Write-Host ''

    if (Confirm-Step 'Confirma a remoção?') {
        if (Test-Path $ConfigDir) { Remove-Item -Recurse -Force $ConfigDir; Write-Ok 'configuração removida' }
        if (Test-Path $StateDir)  { Remove-Item -Recurse -Force $StateDir;  Write-Ok 'estado removido' }
    } else {
        Write-Skip 'remoção cancelada; configuração e estado preservados'
    }
} else {
    Write-Step 'Preservando seus dados'
    if (Test-Path $ConfigDir) { Write-Skip "configuração mantida: $ConfigDir" }
    if (Test-Path $StateDir)  { Write-Skip "estado mantido: $StateDir" }
    Write-Host '    Use -Purge para removê-los também.'
}

# ---------------------------------------------------------------------------
# 5. Encerramento
# ---------------------------------------------------------------------------

Write-Host ''
Write-Step 'Desinstalação concluída'
Write-Host '  As pastas que você sincronizava seguem intactas, com todo o histórico'
Write-Host '  Git preservado. Nada do seu conteúdo foi removido.'
Write-Host ''

# ---------------------------------------------------------------------------
# 6. Autoexclusão
# ---------------------------------------------------------------------------
#
# O uninstall.sh se apaga direto: no Linux o inode sobrevive ao 'rm' enquanto o
# processo o mantém aberto. No Windows o arquivo fica bloqueado durante a
# execução, então a remoção é delegada a um processo separado que espera este
# terminar. Só a cópia instalada se apaga — a do repositório é código-fonte.
$self = $PSCommandPath
if ($self -and $self.StartsWith($Prefix, [StringComparison]::OrdinalIgnoreCase)) {
    Start-Process -WindowStyle Hidden -FilePath 'powershell' -ArgumentList @(
        '-NoProfile', '-Command',
        "Start-Sleep -Seconds 2; Remove-Item -LiteralPath '$self' -Force -ErrorAction SilentlyContinue; " +
        "if (Test-Path '$Prefix') { Get-Item '$Prefix' | Where-Object { -not (Get-ChildItem `$_ -Force) } | Remove-Item -Force -ErrorAction SilentlyContinue }"
    )
    Write-Host "  (o desinstalador se remove em instantes: $self)" -ForegroundColor DarkGray
    Write-Host ''
}
