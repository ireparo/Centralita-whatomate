#!/usr/bin/env bash
#
# iReparo — backup diario de Postgres.
#
# Vuelca la base de datos completa desde el contenedor `ireparo_db`
# a un fichero comprimido con fecha del día, y rota los backups
# antiguos manteniendo los últimos N días.
#
# Uso manual:
#
#     sudo ./scripts/backup.sh
#
# Instalación como tarea diaria (una sola vez):
#
#     sudo ./scripts/backup.sh --install-cron
#
# Después verás el cron en /etc/cron.d/ireparo-backup ejecutándose a
# las 04:17 AM cada día.
#
# Restaurar un backup (ejemplo):
#
#     gunzip -c /var/backups/ireparo/ireparo-2026-04-10.sql.gz \
#       | docker exec -i ireparo_db psql -U ireparo -d ireparo
#
# Variables de entorno opcionales:
#
#     BACKUP_DIR        (default: /var/backups/ireparo)
#     RETENTION_DAYS    (default: 14)
#     DB_CONTAINER      (default: ireparo_db)
#     DB_USER           (default: ireparo)
#     DB_NAME           (default: ireparo)

set -euo pipefail

BACKUP_DIR="${BACKUP_DIR:-/var/backups/ireparo}"
RETENTION_DAYS="${RETENTION_DAYS:-14}"
DB_CONTAINER="${DB_CONTAINER:-ireparo_db}"
DB_USER="${DB_USER:-ireparo}"
DB_NAME="${DB_NAME:-ireparo}"

SCRIPT_PATH="$(readlink -f "$0")"

log()  { printf "\033[1;32m[backup]\033[0m %s\n" "$*"; }
warn() { printf "\033[1;33m[backup]\033[0m %s\n" "$*"; }
err()  { printf "\033[1;31m[backup]\033[0m %s\n" "$*" >&2; exit 1; }

# --- Instalar cron diario -------------------------------------------

if [[ "${1:-}" == "--install-cron" ]]; then
    if [[ "$(id -u)" -ne 0 ]]; then
        err "Para instalar el cron hay que correr como root (sudo)."
    fi
    log "Instalando tarea diaria en /etc/cron.d/ireparo-backup..."
    cat > /etc/cron.d/ireparo-backup <<EOF
# iReparo — backup diario de Postgres a las 04:17 AM
# Generado por $SCRIPT_PATH el $(date -u +%Y-%m-%dT%H:%M:%SZ)
SHELL=/bin/bash
PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin

17 4 * * * root $SCRIPT_PATH >> /var/log/ireparo-backup.log 2>&1
EOF
    chmod 644 /etc/cron.d/ireparo-backup
    touch /var/log/ireparo-backup.log
    chmod 640 /var/log/ireparo-backup.log
    log "Cron instalado. Comprueba con: cat /etc/cron.d/ireparo-backup"
    log "Siguiente ejecución: próximas 04:17 AM (hora del servidor)."
    log "Puedes probarlo a mano ya: sudo $SCRIPT_PATH"
    exit 0
fi

# --- Ejecución del backup -------------------------------------------

if ! command -v docker >/dev/null 2>&1; then
    err "Docker no está instalado."
fi

if ! docker ps --format '{{.Names}}' | grep -q "^${DB_CONTAINER}\$"; then
    err "Contenedor ${DB_CONTAINER} no está corriendo. ¿Está iReparo levantado?"
fi

mkdir -p "$BACKUP_DIR"
chmod 700 "$BACKUP_DIR"

DATE="$(date +%Y-%m-%d_%H%M%S)"
OUT="${BACKUP_DIR}/ireparo-${DATE}.sql.gz"
LATEST_LINK="${BACKUP_DIR}/ireparo-latest.sql.gz"

log "Volcando ${DB_NAME} desde ${DB_CONTAINER}..."
# --format=plain + gzip es lo más simple y portable para restaurar.
# Usamos --clean --if-exists para que una restauración sobre una BD
# existente primero haga DROP de los objetos.
if docker exec -i "$DB_CONTAINER" \
    pg_dump \
        -U "$DB_USER" \
        -d "$DB_NAME" \
        --clean \
        --if-exists \
        --no-owner \
        --no-privileges \
    | gzip -9 > "${OUT}.tmp"; then
    mv "${OUT}.tmp" "$OUT"
    chmod 600 "$OUT"
    ln -sf "$(basename "$OUT")" "$LATEST_LINK"
    SIZE="$(du -h "$OUT" | cut -f1)"
    log "Backup OK: ${OUT} (${SIZE})"
else
    rm -f "${OUT}.tmp"
    err "pg_dump ha fallado. Mira el error arriba."
fi

# --- Rotación --------------------------------------------------------

log "Rotando backups (retención ${RETENTION_DAYS} días)..."
DELETED="$(find "$BACKUP_DIR" -maxdepth 1 -name 'ireparo-*.sql.gz' -type f -mtime +"$RETENTION_DAYS" -print -delete | wc -l)"
if [[ "$DELETED" -gt 0 ]]; then
    log "Eliminados ${DELETED} backups antiguos."
else
    log "Nada que rotar."
fi

# --- Resumen ---------------------------------------------------------

echo
log "Backups actuales en ${BACKUP_DIR}:"
ls -lh "$BACKUP_DIR"/ireparo-*.sql.gz 2>/dev/null | awk '{printf "  %s  %s  %s\n", $5, $6" "$7" "$8, $9}' || echo "  (ninguno)"

# --- Consejo off-site ------------------------------------------------

cat <<'EOF'

-----
Consejo: este backup está en el MISMO VPS. Si el servidor arde, el
backup también. Para dormir tranquilo copia los ficheros a otro sitio:

  · Hetzner Storage Box (~4 €/mes, 1 TB, rsync/sftp)
  · Backblaze B2 (~0,006 $/GB/mes, S3-compatible)
  · Un NAS en tu oficina con rsync por SSH inverso

Si quieres que te prepare la sincronización automática a alguno de
estos, dímelo.
EOF
