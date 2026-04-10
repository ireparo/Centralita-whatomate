#!/usr/bin/env bash
#
# iReparo — auto-update desde GitHub.
#
# Comprueba cada ejecución si hay commits nuevos en origin/main.
# Si los hay, hace git pull + docker compose up -d --build.
# Si no, no hace nada.
#
# Diseñado para correr como cron cada 2-5 minutos.
#
# Instalación (una sola vez):
#
#     sudo ./scripts/auto-update.sh --install-cron
#
# Desinstalación:
#
#     sudo rm /etc/cron.d/ireparo-auto-update
#
# Ver historial de actualizaciones:
#
#     tail -50 /var/log/ireparo-auto-update.log

set -euo pipefail

REPO_DIR="/opt/Centralita-whatomate"
BRANCH="main"
LOG_FILE="/var/log/ireparo-auto-update.log"
LOCK_FILE="/tmp/ireparo-auto-update.lock"
SCRIPT_PATH="$(readlink -f "$0")"

COMPOSE=(docker compose -f docker/docker-compose.yml -f docker/docker-compose.prod.yml)

log() { printf "[%s] %s\n" "$(date '+%Y-%m-%d %H:%M:%S')" "$*"; }

# --- Install cron ----------------------------------------------------

if [[ "${1:-}" == "--install-cron" ]]; then
    if [[ "$(id -u)" -ne 0 ]]; then
        echo "Error: ejecuta con sudo." >&2
        exit 1
    fi
    cat > /etc/cron.d/ireparo-auto-update <<EOF
# iReparo — auto-update cada 2 minutos.
# Comprueba si hay commits nuevos en origin/main y aplica cambios.
# Generado por $SCRIPT_PATH el $(date -u +%Y-%m-%dT%H:%M:%SZ)
SHELL=/bin/bash
PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin

*/2 * * * * root $SCRIPT_PATH >> $LOG_FILE 2>&1
EOF
    chmod 644 /etc/cron.d/ireparo-auto-update
    touch "$LOG_FILE"
    chmod 640 "$LOG_FILE"
    echo "Cron instalado: cada 2 minutos comprobará actualizaciones."
    echo "Log: $LOG_FILE"
    echo "Para desactivar: sudo rm /etc/cron.d/ireparo-auto-update"
    exit 0
fi

# --- Lock (evita ejecuciones paralelas) ------------------------------

if [[ -f "$LOCK_FILE" ]]; then
    # Check if lock is stale (older than 15 min = something went wrong)
    if [[ -n "$(find "$LOCK_FILE" -mmin +15 2>/dev/null)" ]]; then
        log "WARN: Lock stale (>15 min), removing."
        rm -f "$LOCK_FILE"
    else
        exit 0  # Another instance is running, skip silently
    fi
fi
trap 'rm -f "$LOCK_FILE"' EXIT
touch "$LOCK_FILE"

# --- Check for updates -----------------------------------------------

cd "$REPO_DIR"

# Fetch latest commits from origin (silent)
if ! git fetch origin "$BRANCH" --quiet 2>/dev/null; then
    log "WARN: git fetch failed (network?). Retrying next run."
    exit 0
fi

LOCAL_HEAD="$(git rev-parse HEAD)"
REMOTE_HEAD="$(git rev-parse "origin/$BRANCH")"

if [[ "$LOCAL_HEAD" == "$REMOTE_HEAD" ]]; then
    # No changes — exit silently (don't spam the log)
    exit 0
fi

# --- There are new commits: apply ------------------------------------

NEW_COMMITS="$(git log --oneline "$LOCAL_HEAD".."$REMOTE_HEAD" | head -10)"
log "Nuevos commits detectados en origin/$BRANCH:"
echo "$NEW_COMMITS" | while read -r line; do log "  $line"; done

log "Pulling..."
if ! git pull --ff-only origin "$BRANCH"; then
    log "ERROR: git pull failed. Possible merge conflict. Manual intervention required."
    exit 1
fi

log "Rebuilding and restarting containers..."
if "${COMPOSE[@]}" up -d --build; then
    log "OK — deploy completado. Commits aplicados:"
    echo "$NEW_COMMITS" | while read -r line; do log "  $line"; done
    log "---"
else
    log "ERROR: docker compose up failed. Check docker logs."
    exit 1
fi
