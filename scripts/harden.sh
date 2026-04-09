#!/usr/bin/env bash
#
# iReparo — hardening del servidor Ubuntu.
#
# Qué hace (todo idempotente, puedes correrlo varias veces sin efectos
# raros):
#
#   1. Actualizaciones de seguridad automáticas (unattended-upgrades).
#   2. fail2ban para frenar fuerza bruta contra SSH.
#   3. Endurece sshd: sin login por contraseña, root solo por clave,
#      KeepAlive razonable. Hace copia de seguridad del sshd_config
#      antes de tocar nada.
#   4. Parámetros de kernel (sysctl) básicos: SYN cookies, ignore
#      ICMP redirects, rp_filter, ocultar punteros del kernel.
#   5. Rotación de logs de Docker (json-file max 50MB x 3 ficheros)
#      para que los contenedores no llenen el disco.
#   6. Crea un swapfile de 2 GB si no existe (CX22 tiene 4 GB RAM y
#      el build de Docker puede apurarse en RAM).
#   7. Instala utilidades básicas de diagnóstico: htop, iotop, ncdu,
#      jq, net-tools.
#   8. Asegura que UFW está en "deny incoming" por defecto.
#
# NO hace:
#   - No cambia el puerto SSH (seguridad por oscuridad, irrelevante
#     con fail2ban + claves).
#   - No deshabilita IPv6 (Hetzner lo tiene enrutado).
#   - No instala monitoring externo. Si quieres Uptime Kuma /
#     Netdata / Prometheus, lo metemos aparte.
#
# Uso en el VPS:
#
#     cd /opt/Centralita-whatomate
#     sudo ./scripts/harden.sh
#
# Avisos antes de correrlo:
#   · ASEGÚRATE de que tu clave SSH funciona ANTES de correr el script.
#     Abre una segunda sesión de PuTTY (o ssh) y verifica que entras
#     sin contraseña. Si no, no ejecutes este script — te puedes
#     quedar fuera.
#   · Si cambias de ordenador o pierdes la clave, lo único que te
#     salvará es la consola web de Hetzner (Rescue mode).

set -euo pipefail

log()  { printf "\033[1;32m[harden]\033[0m %s\n" "$*"; }
warn() { printf "\033[1;33m[harden]\033[0m %s\n" "$*"; }
err()  { printf "\033[1;31m[harden]\033[0m %s\n" "$*" >&2; exit 1; }

if [[ "$(id -u)" -ne 0 ]]; then
    err "Este script debe correrse como root (o con sudo)."
fi

# ------------------------------------------------------------------
# 0. Repos al día
# ------------------------------------------------------------------
log "Actualizando listas de paquetes..."
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq

# ------------------------------------------------------------------
# 1. unattended-upgrades: parches de seguridad automáticos
# ------------------------------------------------------------------
log "Instalando unattended-upgrades..."
apt-get install -y -qq unattended-upgrades apt-listchanges

# Configuración: solo instala parches de seguridad automáticamente.
# Los updates normales NO se aplican solos para evitar reinicios
# inesperados por cambios de versión mayor.
cat > /etc/apt/apt.conf.d/50unattended-upgrades <<'EOF'
Unattended-Upgrade::Allowed-Origins {
    "${distro_id}:${distro_codename}-security";
    "${distro_id}ESMApps:${distro_codename}-apps-security";
    "${distro_id}ESM:${distro_codename}-infra-security";
};
Unattended-Upgrade::AutoFixInterruptedDpkg "true";
Unattended-Upgrade::MinimalSteps "true";
Unattended-Upgrade::Remove-Unused-Kernel-Packages "true";
Unattended-Upgrade::Remove-New-Unused-Dependencies "true";
Unattended-Upgrade::Remove-Unused-Dependencies "true";
Unattended-Upgrade::Automatic-Reboot "false";
Unattended-Upgrade::SyslogEnable "true";
EOF

cat > /etc/apt/apt.conf.d/20auto-upgrades <<'EOF'
APT::Periodic::Update-Package-Lists "1";
APT::Periodic::Unattended-Upgrade "1";
APT::Periodic::AutocleanInterval "7";
EOF

systemctl enable --now unattended-upgrades >/dev/null 2>&1 || true
log "unattended-upgrades activado."

# ------------------------------------------------------------------
# 2. fail2ban
# ------------------------------------------------------------------
log "Instalando fail2ban..."
apt-get install -y -qq fail2ban

# Jaula SSH con defaults conservadores: 5 intentos, ban 1 hora.
# Ajusta bantime si quieres algo más severo (24h, 7d, etc.).
cat > /etc/fail2ban/jail.d/ireparo.conf <<'EOF'
[DEFAULT]
bantime  = 1h
findtime = 10m
maxretry = 5
backend  = systemd

[sshd]
enabled  = true
port     = ssh
logpath  = %(sshd_log)s
EOF

systemctl enable --now fail2ban >/dev/null 2>&1 || true
systemctl restart fail2ban
log "fail2ban activado (5 intentos fallidos = ban 1 hora)."

# ------------------------------------------------------------------
# 3. SSH hardening
# ------------------------------------------------------------------
SSHD_CONFIG="/etc/ssh/sshd_config"
if [[ -f "$SSHD_CONFIG" && ! -f "${SSHD_CONFIG}.bak-harden" ]]; then
    cp "$SSHD_CONFIG" "${SSHD_CONFIG}.bak-harden"
    log "Backup de sshd_config guardado en ${SSHD_CONFIG}.bak-harden"
fi

set_sshd() {
    local key="$1" val="$2"
    if grep -qE "^[# ]*${key}\\b" "$SSHD_CONFIG"; then
        sed -i "s|^[# ]*${key}\\b.*|${key} ${val}|" "$SSHD_CONFIG"
    else
        printf '\n%s %s\n' "$key" "$val" >> "$SSHD_CONFIG"
    fi
}

log "Endureciendo sshd_config..."
set_sshd PermitRootLogin         "prohibit-password"
set_sshd PasswordAuthentication  "no"
set_sshd KbdInteractiveAuthentication "no"
set_sshd ChallengeResponseAuthentication "no"
set_sshd PubkeyAuthentication    "yes"
set_sshd X11Forwarding           "no"
set_sshd MaxAuthTries            "3"
set_sshd ClientAliveInterval     "300"
set_sshd ClientAliveCountMax     "2"
set_sshd LoginGraceTime          "30"

# Valida la sintaxis antes de recargar, por si acaso.
if sshd -t; then
    systemctl reload ssh || systemctl reload sshd || true
    log "sshd recargado. Sólo se acepta login con clave SSH."
else
    cp "${SSHD_CONFIG}.bak-harden" "$SSHD_CONFIG"
    err "La configuración nueva de sshd es inválida. Restaurada la de backup. No se ha aplicado nada."
fi

# ------------------------------------------------------------------
# 4. Kernel sysctl
# ------------------------------------------------------------------
log "Aplicando sysctl de endurecimiento..."
cat > /etc/sysctl.d/99-ireparo-hardening.conf <<'EOF'
# Red — mitigaciones básicas
net.ipv4.tcp_syncookies = 1
net.ipv4.conf.all.rp_filter = 1
net.ipv4.conf.default.rp_filter = 1
net.ipv4.conf.all.accept_redirects = 0
net.ipv4.conf.default.accept_redirects = 0
net.ipv6.conf.all.accept_redirects = 0
net.ipv6.conf.default.accept_redirects = 0
net.ipv4.conf.all.send_redirects = 0
net.ipv4.conf.default.send_redirects = 0
net.ipv4.conf.all.accept_source_route = 0
net.ipv4.conf.default.accept_source_route = 0
net.ipv4.conf.all.log_martians = 1
net.ipv4.icmp_echo_ignore_broadcasts = 1
net.ipv4.icmp_ignore_bogus_error_responses = 1

# Kernel — oculta punteros y dmesg a usuarios no root
kernel.kptr_restrict = 2
kernel.dmesg_restrict = 1
kernel.yama.ptrace_scope = 1
EOF

sysctl -q --system || warn "sysctl reload devolvió warnings, revisa dmesg."

# ------------------------------------------------------------------
# 5. Docker log rotation
# ------------------------------------------------------------------
log "Configurando rotación de logs de Docker..."
mkdir -p /etc/docker
if [[ -f /etc/docker/daemon.json ]]; then
    # Hay config previa: no la pisamos, solo avisamos.
    if ! grep -q '"max-size"' /etc/docker/daemon.json; then
        warn "/etc/docker/daemon.json ya existe y no tiene max-size configurado."
        warn "Añade manualmente: \"log-driver\": \"json-file\", \"log-opts\": {\"max-size\": \"50m\", \"max-file\": \"3\"}"
    else
        log "daemon.json ya tiene rotación configurada — OK."
    fi
else
    cat > /etc/docker/daemon.json <<'EOF'
{
  "log-driver": "json-file",
  "log-opts": {
    "max-size": "50m",
    "max-file": "3"
  }
}
EOF
    log "Reiniciando Docker para aplicar la rotación de logs..."
    systemctl restart docker
    log "Docker reiniciado. Los contenedores se levantan solos con restart=unless-stopped."
fi

# ------------------------------------------------------------------
# 6. Swapfile (solo si no hay ya swap)
# ------------------------------------------------------------------
SWAP_BYTES="$(free --bytes | awk '/^Swap:/ {print $2}')"
if [[ "${SWAP_BYTES:-0}" -eq 0 ]]; then
    log "Creando swapfile de 2 GB en /swapfile..."
    fallocate -l 2G /swapfile || dd if=/dev/zero of=/swapfile bs=1M count=2048
    chmod 600 /swapfile
    mkswap /swapfile >/dev/null
    swapon /swapfile
    if ! grep -q "^/swapfile " /etc/fstab; then
        echo "/swapfile none swap sw 0 0" >> /etc/fstab
    fi
    # Swappiness bajo: priorizar RAM, usar swap solo como red de seguridad.
    sysctl -q vm.swappiness=10
    grep -q "^vm.swappiness" /etc/sysctl.d/99-ireparo-hardening.conf || echo "vm.swappiness = 10" >> /etc/sysctl.d/99-ireparo-hardening.conf
    log "Swap de 2 GB activo."
else
    log "Swap ya presente (${SWAP_BYTES} bytes), se deja como está."
fi

# ------------------------------------------------------------------
# 7. Herramientas útiles
# ------------------------------------------------------------------
log "Instalando utilidades de diagnóstico..."
apt-get install -y -qq htop iotop ncdu jq net-tools ca-certificates

# ------------------------------------------------------------------
# 8. UFW defaults
# ------------------------------------------------------------------
if command -v ufw >/dev/null 2>&1; then
    log "Verificando UFW..."
    ufw default deny incoming  >/dev/null 2>&1 || true
    ufw default allow outgoing >/dev/null 2>&1 || true
    # Asegura las reglas de iReparo (por si alguien las borró).
    ufw allow OpenSSH >/dev/null 2>&1 || true
    ufw allow 80/tcp  >/dev/null 2>&1 || true
    ufw allow 443/tcp >/dev/null 2>&1 || true
    ufw allow 443/udp >/dev/null 2>&1 || true
    ufw allow 10000:10100/udp >/dev/null 2>&1 || true
    ufw --force enable >/dev/null 2>&1 || true
    ufw status verbose | sed 's/^/  /'
else
    warn "UFW no instalado."
fi

# ------------------------------------------------------------------
# Resumen final
# ------------------------------------------------------------------
echo
log "Hardening completado."
cat <<'EOF'

========================================================
  Comprobaciones rápidas post-hardening
========================================================

  · Actualizaciones automáticas:
      systemctl status unattended-upgrades --no-pager

  · fail2ban — IPs baneadas:
      fail2ban-client status sshd

  · SSH — confirma que PUEDES entrar con clave antes de
    cerrar esta sesión. Abre OTRA ventana de PuTTY, conecta,
    y si te entra sin pedir password, ya está.

  · Rotación de logs Docker (pide a los contenedores su driver):
      docker inspect ireparo_app --format '{{.HostConfig.LogConfig}}'

  · Swap activo:
      free -h

  · UFW activo:
      ufw status

EOF
