#!/usr/bin/env bash
#
# Instalador do WatchFlow.
#
# Compila o binário, instala no PATH do usuário, prepara a configuração inicial
# e, opcionalmente, registra o serviço no systemd --user.
#
# O script é idempotente: rodá-lo de novo atualiza o binário sem tocar na sua
# configuração nem no estado do daemon.

set -euo pipefail

# ---------------------------------------------------------------------------
# Padrões (sobrescritos pelas flags)
# ---------------------------------------------------------------------------

PREFIX="${PREFIX:-$HOME/.local/bin}"
CONFIG_DIR="${XDG_CONFIG_HOME:-$HOME/.config}/watchflow"
CONFIG_FILE="$CONFIG_DIR/config.yaml"
STATE_DIR="${XDG_STATE_HOME:-$HOME/.local/state}/watchflow"
SYSTEMD_DIR="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user"

INSTALL_SERVICE=1
DO_BUILD=1
ASSUME_YES=0

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BINARY_NAME="watchflow"

# ---------------------------------------------------------------------------
# Saída
# ---------------------------------------------------------------------------

if [[ -t 1 ]]; then
    C_RESET=$'\033[0m'; C_BOLD=$'\033[1m'; C_DIM=$'\033[2m'
    C_RED=$'\033[31m'; C_GREEN=$'\033[32m'; C_YELLOW=$'\033[33m'
else
    C_RESET=""; C_BOLD=""; C_DIM=""; C_RED=""; C_GREEN=""; C_YELLOW=""
fi

info()  { printf '%s==>%s %s\n' "$C_BOLD" "$C_RESET" "$*"; }
ok()    { printf '  %s✓%s %s\n' "$C_GREEN" "$C_RESET" "$*"; }
warn()  { printf '  %s!%s %s\n' "$C_YELLOW" "$C_RESET" "$*"; }
skip()  { printf '  %s·%s %s\n' "$C_DIM" "$C_RESET" "$*"; }
die()   { printf '%serro:%s %s\n' "$C_RED" "$C_RESET" "$*" >&2; exit 1; }

usage() {
    cat <<EOF
Instalador do WatchFlow.

Uso: scripts/install.sh [opções]

Opções:
  --prefix DIR    Onde instalar o binário (padrão: ~/.local/bin)
  --no-service    Não registra a unit do systemd --user
  --no-build      Usa o binário já existente em bin/ em vez de recompilar
  --yes, -y       Não faz perguntas; assume "sim" para tudo
  --help, -h      Exibe esta ajuda

O script NUNCA sobrescreve uma configuração existente e NUNCA remove dados.
Para desinstalar, use scripts/uninstall.sh
EOF
}

# ---------------------------------------------------------------------------
# Argumentos
# ---------------------------------------------------------------------------

while [[ $# -gt 0 ]]; do
    case "$1" in
        --prefix)     PREFIX="${2:?--prefix exige um diretório}"; shift 2 ;;
        --prefix=*)   PREFIX="${1#*=}"; shift ;;
        --no-service) INSTALL_SERVICE=0; shift ;;
        --no-build)   DO_BUILD=0; shift ;;
        -y|--yes)     ASSUME_YES=1; shift ;;
        -h|--help)    usage; exit 0 ;;
        *)            die "opção desconhecida: $1 (use --help)" ;;
    esac
done

confirm() {
    if [[ $ASSUME_YES -eq 1 ]]; then return 0; fi
    # Sem terminal interativo, o padrão é NÃO agir
    if [[ ! -t 0 ]]; then return 1; fi

    local answer
    read -r -p "  $1 [s/N] " answer
    [[ "$answer" =~ ^[SsYy]$ ]]
}

# ---------------------------------------------------------------------------
# 1. Pré-requisitos
# ---------------------------------------------------------------------------

info "Verificando pré-requisitos"

if [[ "$(id -u)" -eq 0 ]]; then
    die "não execute como root. O WatchFlow roda na sua sessão de usuário e
       instala um serviço systemd --user; instalar como root deixaria o serviço
       em um usuário que não é o dono dos arquivos monitorados."
fi

if [[ "$(uname -s)" != "Linux" ]]; then
    die "por enquanto o WatchFlow só funciona em Linux (depende de inotify,
       sockets Unix e systemd --user). Veja as limitações conhecidas no README."
fi

command -v git >/dev/null 2>&1 || die "git não encontrado no PATH. Instale-o antes de continuar."
ok "git $(git --version | awk '{print $3}')"

if [[ $DO_BUILD -eq 1 ]]; then
    command -v go >/dev/null 2>&1 || die "go não encontrado no PATH.
       Instale o Go 1.24+ ou rode com --no-build se já tiver o binário em bin/."
    ok "go $(go version | awk '{print $3}')"
fi

# ---------------------------------------------------------------------------
# 2. Compilação
# ---------------------------------------------------------------------------

BUILT_BINARY="$REPO_ROOT/bin/$BINARY_NAME"

if [[ $DO_BUILD -eq 1 ]]; then
    info "Compilando"
    ( cd "$REPO_ROOT" && make build >/dev/null ) || die "a compilação falhou. Rode 'make build' para ver o erro."
    ok "binário gerado em bin/$BINARY_NAME"
else
    [[ -x "$BUILT_BINARY" ]] || die "--no-build informado, mas não há binário em bin/$BINARY_NAME"
    skip "compilação pulada (--no-build)"
fi

# ---------------------------------------------------------------------------
# 3. Instalação do binário
# ---------------------------------------------------------------------------

info "Instalando o binário"

mkdir -p "$PREFIX"
install -m 0755 "$BUILT_BINARY" "$PREFIX/$BINARY_NAME"
ok "$PREFIX/$BINARY_NAME"

case ":$PATH:" in
    *":$PREFIX:"*) ;;
    *)
        warn "$PREFIX não está no seu PATH."
        printf '    Adicione ao seu ~/.bashrc ou ~/.zshrc:\n\n'
        printf '      export PATH="%s:$PATH"\n\n' "$PREFIX"
        ;;
esac

# ---------------------------------------------------------------------------
# 4. Configuração
# ---------------------------------------------------------------------------

info "Preparando a configuração"

mkdir -p "$CONFIG_DIR" "$STATE_DIR"
chmod 700 "$STATE_DIR"

CONFIG_IS_NEW=0
if [[ -f "$CONFIG_FILE" ]]; then
    # Sobrescrever apagaria os watchers que o usuário já configurou
    skip "configuração já existe, mantida intacta: $CONFIG_FILE"
else
    cp "$REPO_ROOT/configs/watchflow.example.yaml" "$CONFIG_FILE"
    chmod 600 "$CONFIG_FILE"
    CONFIG_IS_NEW=1
    ok "configuração inicial criada: $CONFIG_FILE"
fi

# ---------------------------------------------------------------------------
# 5. Serviço systemd --user
# ---------------------------------------------------------------------------

systemd_available() {
    command -v systemctl >/dev/null 2>&1 && \
    [[ -n "${XDG_RUNTIME_DIR:-}" ]] && \
    systemctl --user show-environment >/dev/null 2>&1
}

if [[ $INSTALL_SERVICE -eq 1 ]]; then
    info "Registrando o serviço"

    if systemd_available; then
        mkdir -p "$SYSTEMD_DIR"
        install -m 0644 "$REPO_ROOT/deploy/systemd/watchflow.service" "$SYSTEMD_DIR/watchflow.service"
        # A partir daqui o binário já está instalado: uma falha do systemctl não
        # pode abortar o script e deixar a instalação pela metade.
        systemctl --user daemon-reload || warn "falha no daemon-reload"
        ok "unit instalada: $SYSTEMD_DIR/watchflow.service"

        # Só habilita depois que houver configuração real; subir o serviço com o
        # exemplo apontando para uma pasta inexistente faria o daemon falhar em
        # laço de reinício.
        if [[ $CONFIG_IS_NEW -eq 1 ]]; then
            skip "serviço não habilitado ainda — edite a configuração primeiro"
        elif systemctl --user is-enabled watchflow.service >/dev/null 2>&1; then
            if systemctl --user restart watchflow.service; then
                ok "serviço já habilitado; reiniciado com o binário novo"
            else
                warn "falha ao reiniciar; veja 'systemctl --user status watchflow'"
            fi
        elif confirm "Habilitar e iniciar o serviço agora?"; then
            if systemctl --user enable --now watchflow.service; then
                ok "serviço habilitado e iniciado"
            else
                warn "falha ao habilitar; veja 'systemctl --user status watchflow'"
            fi
        else
            skip "serviço instalado, mas não habilitado"
        fi
    else
        warn "systemd --user indisponível nesta sessão; unit não instalada."
        printf '    Você ainda pode rodar manualmente: %s start\n' "$BINARY_NAME"
    fi
else
    skip "instalação do serviço pulada (--no-service)"
fi

# ---------------------------------------------------------------------------
# 6. Próximos passos
# ---------------------------------------------------------------------------

echo
info "Instalação concluída"
echo

if [[ $CONFIG_IS_NEW -eq 1 ]]; then
    cat <<EOF
${C_BOLD}A configuração é feita editando o arquivo YAML${C_RESET} — não há assistente
interativo. O arquivo criado é o exemplo comentado; ajuste-o antes de usar.

  1. Edite a configuração (troque 'path' pela pasta que quer sincronizar):

       \${EDITOR:-nano} $CONFIG_FILE

  2. Confira se ficou válida:

       $BINARY_NAME config validate

  3. Verifique o ambiente (limites do kernel, git, permissões):

       $BINARY_NAME doctor

  4. Habilite o serviço:

       systemctl --user enable --now watchflow

A pasta que você indicar em 'path' precisa já ser um repositório Git com um
remoto configurado.
EOF
else
    cat <<EOF
Binário atualizado. Sua configuração e seu estado não foram tocados.

  $BINARY_NAME status        estado das pastas monitoradas
  $BINARY_NAME tui           painel interativo
  $BINARY_NAME logs -f       acompanhar os eventos
EOF
fi

echo
