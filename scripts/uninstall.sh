#!/usr/bin/env bash
#
# Desinstalador do WatchFlow.
#
# Remove o serviço e o binário. Por padrão PRESERVA sua configuração e o estado
# do daemon — use --purge para apagá-los também.
#
# Em nenhuma hipótese este script toca nas pastas que você monitorava nem nos
# repositórios Git dentro delas.

set -euo pipefail

# ---------------------------------------------------------------------------
# Padrões
# ---------------------------------------------------------------------------

PREFIX="${PREFIX:-$HOME/.local/bin}"
CONFIG_DIR="${XDG_CONFIG_HOME:-$HOME/.config}/watchflow"
STATE_DIR="${XDG_STATE_HOME:-$HOME/.local/state}/watchflow"
SYSTEMD_DIR="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user"

PURGE=0
ASSUME_YES=0
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
Desinstalador do WatchFlow.

Uso: scripts/uninstall.sh [opções]

Opções:
  --prefix DIR    Onde o binário foi instalado (padrão: ~/.local/bin)
  --purge         Também apaga a configuração e o estado do daemon
  --yes, -y       Não faz perguntas; assume "sim" para tudo
  --help, -h      Exibe esta ajuda

Sem --purge, sua configuração e o histórico do daemon são preservados, de modo
que reinstalar depois devolve tudo como estava.

As pastas que você monitorava e seus repositórios Git NUNCA são tocados por
este script, com ou sem --purge.
EOF
}

# ---------------------------------------------------------------------------
# Argumentos
# ---------------------------------------------------------------------------

while [[ $# -gt 0 ]]; do
    case "$1" in
        --prefix)   PREFIX="${2:?--prefix exige um diretório}"; shift 2 ;;
        --prefix=*) PREFIX="${1#*=}"; shift ;;
        --purge)    PURGE=1; shift ;;
        -y|--yes)   ASSUME_YES=1; shift ;;
        -h|--help)  usage; exit 0 ;;
        *)          die "opção desconhecida: $1 (use --help)" ;;
    esac
done

confirm() {
    if [[ $ASSUME_YES -eq 1 ]]; then return 0; fi
    if [[ ! -t 0 ]]; then return 1; fi

    local answer
    read -r -p "  $1 [s/N] " answer
    [[ "$answer" =~ ^[SsYy]$ ]]
}

if [[ "$(id -u)" -eq 0 ]]; then
    die "não execute como root; o WatchFlow é instalado na sua sessão de usuário."
fi

# ---------------------------------------------------------------------------
# 1. Serviço
# ---------------------------------------------------------------------------

systemd_available() {
    command -v systemctl >/dev/null 2>&1 && \
    [[ -n "${XDG_RUNTIME_DIR:-}" ]] && \
    systemctl --user show-environment >/dev/null 2>&1
}

info "Parando o serviço"

if systemd_available; then
    if systemctl --user is-active watchflow.service >/dev/null 2>&1; then
        # Encerramento ordenado: o daemon devolve à fila os jobs em voo
        systemctl --user stop watchflow.service || warn "falha ao parar o serviço"
        ok "serviço parado"
    else
        skip "serviço não estava em execução"
    fi

    if systemctl --user is-enabled watchflow.service >/dev/null 2>&1; then
        systemctl --user disable watchflow.service >/dev/null 2>&1 || true
        ok "serviço desabilitado"
    fi

    if [[ -f "$SYSTEMD_DIR/watchflow.service" ]]; then
        rm -f "$SYSTEMD_DIR/watchflow.service"
        systemctl --user daemon-reload || warn "falha no daemon-reload"
        ok "unit removida: $SYSTEMD_DIR/watchflow.service"
    else
        skip "nenhuma unit instalada"
    fi
else
    skip "systemd --user indisponível nesta sessão"

    # Sem systemd, o daemon pode ter sido iniciado à mão
    if [[ -x "$PREFIX/$BINARY_NAME" ]]; then
        if "$PREFIX/$BINARY_NAME" stop >/dev/null 2>&1; then
            ok "daemon avulso encerrado"
        fi
    fi
fi

# ---------------------------------------------------------------------------
# 2. Binário
# ---------------------------------------------------------------------------

info "Removendo o binário"

if [[ -e "$PREFIX/$BINARY_NAME" ]]; then
    rm -f "$PREFIX/$BINARY_NAME"
    ok "$PREFIX/$BINARY_NAME"
else
    skip "nada em $PREFIX/$BINARY_NAME"
fi

# Um binário fora do prefixo esperado passaria despercebido
OTHER="$(command -v "$BINARY_NAME" 2>/dev/null || true)"
if [[ -n "$OTHER" ]]; then
    warn "ainda existe um '$BINARY_NAME' em $OTHER — remova-o à mão se quiser."
fi

# O próprio desinstalador, instalado ao lado do binário. Removê-lo enquanto ele
# executa é seguro no Linux: o inode continua aberto até o processo terminar.
SELF_INSTALLED="$PREFIX/$BINARY_NAME-uninstall"
if [[ -e "$SELF_INSTALLED" ]]; then
    rm -f "$SELF_INSTALLED"
    ok "$SELF_INSTALLED"
fi

# ---------------------------------------------------------------------------
# 3. Dados do usuário
# ---------------------------------------------------------------------------

if [[ $PURGE -eq 1 ]]; then
    info "Removendo configuração e estado (--purge)"

    echo
    printf '  %sIsto apaga:%s\n' "$C_BOLD" "$C_RESET"
    if [[ -d "$CONFIG_DIR" ]]; then printf '    %s (sua configuração)\n' "$CONFIG_DIR"; fi
    if [[ -d "$STATE_DIR"  ]]; then printf '    %s (fila, histórico e logs)\n' "$STATE_DIR"; fi
    printf '  %sSuas pastas monitoradas e os repositórios Git nelas NÃO são tocados.%s\n\n' "$C_DIM" "$C_RESET"

    if confirm "Confirma a remoção?"; then
        if [[ -d "$CONFIG_DIR" ]]; then rm -rf "$CONFIG_DIR"; ok "configuração removida"; fi
        if [[ -d "$STATE_DIR"  ]]; then rm -rf "$STATE_DIR";  ok "estado removido"; fi
    else
        skip "remoção cancelada; configuração e estado preservados"
    fi
else
    info "Preservando seus dados"
    if [[ -d "$CONFIG_DIR" ]]; then skip "configuração mantida: $CONFIG_DIR"; fi
    if [[ -d "$STATE_DIR"  ]]; then skip "estado mantido: $STATE_DIR"; fi
    printf '    Use %s--purge%s para removê-los também.\n' "$C_BOLD" "$C_RESET"
fi

# ---------------------------------------------------------------------------
# 4. Encerramento
# ---------------------------------------------------------------------------

echo
info "Desinstalação concluída"
printf '  As pastas que você sincronizava seguem intactas, com todo o histórico\n'
printf '  Git preservado. Nada do seu conteúdo foi removido.\n\n'
