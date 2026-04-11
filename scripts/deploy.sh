#!/usr/bin/env bash
#
# iReparo — despliegue en producción.
#
# Uso:
#     ./scripts/deploy.sh
#
# Primera ejecución: te pregunta el dominio, la IP pública y el email
# del administrador, genera todos los secretos de forma aleatoria,
# abre el firewall (UFW si está disponible) y arranca la stack
# completa (iReparo + Postgres + Redis + Caddy con HTTPS automático).
#
# Ejecuciones siguientes: hace git pull y reconstruye la imagen
# conservando los secretos existentes. Idempotente, puedes llamarlo
# las veces que quieras.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$REPO_ROOT"

ENV_FILE="docker/.env"
CONFIG_FILE="docker/config.toml"
CONFIG_TEMPLATE="config.prod.example.toml"
CADDYFILE="docker/Caddyfile"

COMPOSE=(docker compose -f docker/docker-compose.yml -f docker/docker-compose.prod.yml)

log()  { printf "\033[1;32m[iReparo]\033[0m %s\n" "$*"; }
warn() { printf "\033[1;33m[avis]\033[0m %s\n" "$*"; }
err()  { printf "\033[1;31m[error]\033[0m %s\n" "$*" >&2; exit 1; }

# --- sanity checks ---------------------------------------------------

if ! command -v docker >/dev/null 2>&1; then
    err "Docker no está instalado. Ejecuta: curl -fsSL https://get.docker.com | sh"
fi

if ! docker info >/dev/null 2>&1; then
    err "Docker está instalado pero no accesible. ¿Ejecutas como root o en el grupo docker?"
fi

if ! command -v openssl >/dev/null 2>&1; then
    err "openssl no está instalado. Ejecuta: apt-get install -y openssl"
fi

# --- subsequent runs: just pull + rebuild ----------------------------

if [[ -f "$ENV_FILE" && -f "$CONFIG_FILE" ]]; then
    log "Configuración existente detectada — actualización incremental."
    if [[ -d .git ]]; then
        log "Descargando últimos cambios del repositorio..."
        git pull --ff-only || warn "git pull falló. Reviso conflictos manualmente."
    fi
    log "Reconstruyendo imagen y relanzando contenedores..."
    "${COMPOSE[@]}" up -d --build
    log "Listo. Estado:"
    "${COMPOSE[@]}" ps
    exit 0
fi

# --- first run: interactive setup ------------------------------------

log "Primera ejecución. Vamos a generar la configuración de producción."
echo

read -rp "Dominio público (ej: pbx.ireparo.es): " DOMAIN
DOMAIN="${DOMAIN//[[:space:]]/}"
[[ -z "$DOMAIN" ]] && err "El dominio no puede estar vacío."

read -rp "IP pública de este VPS (ej: 142.132.169.78): " PUBLIC_IP
PUBLIC_IP="${PUBLIC_IP//[[:space:]]/}"
[[ -z "$PUBLIC_IP" ]] && err "La IP pública no puede estar vacía."
if ! [[ "$PUBLIC_IP" =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    warn "La IP '$PUBLIC_IP' no parece una IPv4 válida. Continúo, pero revísalo."
fi

read -rp "Email del administrador inicial (ej: admin@ireparo.es): " ADMIN_EMAIL
ADMIN_EMAIL="${ADMIN_EMAIL//[[:space:]]/}"
[[ -z "$ADMIN_EMAIL" ]] && err "El email no puede estar vacío."

while true; do
    read -rsp "Contraseña del administrador inicial: " ADMIN_PASSWORD
    echo
    read -rsp "Repite la contraseña: " ADMIN_PASSWORD2
    echo
    if [[ "$ADMIN_PASSWORD" == "$ADMIN_PASSWORD2" && -n "$ADMIN_PASSWORD" ]]; then
        break
    fi
    warn "Las contraseñas no coinciden o están vacías. Inténtalo de nuevo."
done

echo
log "Generando secretos criptográficos..."
ENCRYPTION_KEY="$(openssl rand -hex 32)"
JWT_SECRET="$(openssl rand -hex 32)"
DB_PASSWORD="$(openssl rand -hex 16)"
CRM_API_KEY="$(openssl rand -hex 24)"
CRM_WEBHOOK_SECRET="$(openssl rand -hex 32)"

# --- write env file --------------------------------------------------

log "Escribiendo $ENV_FILE"
umask 077
cat > "$ENV_FILE" <<EOF
# Generado automáticamente por scripts/deploy.sh el $(date -u +%Y-%m-%dT%H:%M:%SZ)
# No edites a mano. Para regenerar, borra este fichero y vuelve a correr el script.

TZ=Europe/Madrid

# Dominio público (usado por Caddy para Let's Encrypt)
DOMAIN=$DOMAIN

# PostgreSQL (coinciden con docker/config.toml)
POSTGRES_USER=ireparo
POSTGRES_PASSWORD=$DB_PASSWORD
POSTGRES_DB=ireparo
EOF
chmod 600 "$ENV_FILE"

# --- write config.toml from template ---------------------------------

log "Escribiendo $CONFIG_FILE a partir de $CONFIG_TEMPLATE"
if [[ ! -f "$CONFIG_TEMPLATE" ]]; then
    err "No encuentro $CONFIG_TEMPLATE. ¿Has hecho git pull?"
fi

# Usamos `|` como separador en sed porque los secretos pueden contener `/`.
# Escapamos `&` y `\` en los reemplazos por seguridad.
escape_sed() { printf '%s' "$1" | sed -e 's/[\&|]/\\&/g'; }

sed \
    -e "s|__ENCRYPTION_KEY__|$(escape_sed "$ENCRYPTION_KEY")|g" \
    -e "s|__DB_PASSWORD__|$(escape_sed "$DB_PASSWORD")|g" \
    -e "s|__JWT_SECRET__|$(escape_sed "$JWT_SECRET")|g" \
    -e "s|__PUBLIC_IP__|$(escape_sed "$PUBLIC_IP")|g" \
    -e "s|__DOMAIN__|$(escape_sed "$DOMAIN")|g" \
    -e "s|__ADMIN_EMAIL__|$(escape_sed "$ADMIN_EMAIL")|g" \
    -e "s|__ADMIN_PASSWORD__|$(escape_sed "$ADMIN_PASSWORD")|g" \
    -e "s|__CRM_API_KEY__|$(escape_sed "$CRM_API_KEY")|g" \
    -e "s|__CRM_WEBHOOK_SECRET__|$(escape_sed "$CRM_WEBHOOK_SECRET")|g" \
    "$CONFIG_TEMPLATE" > "$CONFIG_FILE"
chmod 600 "$CONFIG_FILE"

# --- firewall (ufw) --------------------------------------------------

if command -v ufw >/dev/null 2>&1; then
    log "Configurando firewall UFW..."
    ufw allow OpenSSH >/dev/null 2>&1 || true
    ufw allow 80/tcp comment 'HTTP (Let\''s Encrypt + redirect)' >/dev/null 2>&1 || true
    ufw allow 443/tcp comment 'HTTPS (iReparo)' >/dev/null 2>&1 || true
    ufw allow 443/udp comment 'HTTP/3' >/dev/null 2>&1 || true
    ufw allow 10000:10100/udp comment 'WebRTC media (iReparo calls)' >/dev/null 2>&1 || true
    ufw --force enable >/dev/null 2>&1 || true
    log "UFW configurado."
else
    warn "UFW no instalado — asegúrate de tener abiertos en el firewall del VPS: 22/tcp, 80/tcp, 443/tcp, 443/udp y 10000-10100/udp."
fi

# --- build and launch ------------------------------------------------

log "Construyendo imagen y arrancando la stack (la primera vez tarda ~10 min)..."
"${COMPOSE[@]}" up -d --build

echo
log "Despliegue completado."
cat <<EOF

==========================================================
  Credenciales de la integración con el CRM
==========================================================

  Pásale estos dos valores a la sesión de Claude Code que
  trabaja en el CRM Laravel (sat.ireparo.es). Tienen que
  guardarse en el .env del CRM como:

      IREPARO_PBX_API_KEY=$CRM_API_KEY
      IREPARO_PBX_WEBHOOK_SECRET=$CRM_WEBHOOK_SECRET

  Sin esto el CRM no podrá verificar las firmas HMAC de los
  webhooks que iReparo PBX le envíe.

==========================================================
  iReparo está arrancando en https://$DOMAIN
==========================================================

 · Caddy está pidiendo el certificado a Let's Encrypt. Espera 1-2 min.
 · Luego abre https://$DOMAIN en el navegador.
 · Inicia sesión con:
       Email:    $ADMIN_EMAIL
       Contraseña: (la que acabas de introducir)

 · Cambia la contraseña del admin desde la UI nada más entrar.

Comandos útiles:

 · Ver logs en vivo:
     docker compose -f docker/docker-compose.yml -f docker/docker-compose.prod.yml logs -f

 · Ver estado de los contenedores:
     docker compose -f docker/docker-compose.yml -f docker/docker-compose.prod.yml ps

 · Reiniciar tras cambios de código:
     ./scripts/deploy.sh

 · Parar todo (mantiene datos):
     docker compose -f docker/docker-compose.yml -f docker/docker-compose.prod.yml down

EOF
