<a href="https://zerodha.tech"><img src="https://zerodha.tech/static/images/github-badge.svg" align="right" alt="Zerodha Tech Badge" /></a>

# Whatomate

Plataforma moderna y de código abierto para WhatsApp Business. Aplicación de binario único.

![Panel](docs/public/images/dashboard-light.png#gh-light-mode-only)
![Panel](docs/public/images/dashboard-dark.png#gh-dark-mode-only)

## Funcionalidades

- **Arquitectura multitenant**
  Permite varias organizaciones con datos y configuraciones aislados.

- **Roles y permisos granulares**
  Roles personalizables con permisos muy detallados. Crea roles a medida, asigna permisos específicos por recurso (usuarios, contactos, plantillas, etc.) y controla el acceso por acción (leer, crear, actualizar, eliminar). Los superadministradores pueden gestionar varias organizaciones.

- **Integración con la API de WhatsApp Cloud**
  Conecta con la API de WhatsApp Business de Meta para enviar y recibir mensajes.

- **Chat en tiempo real**
  Mensajería en directo con soporte WebSocket para una comunicación instantánea.

- **Gestión de plantillas**
  Crea y gestiona plantillas de mensajes aprobadas por Meta.

- **Campañas masivas**
  Envía campañas a múltiples contactos con reintentos automáticos para los mensajes fallidos.

- **Automatización con chatbot**
  Respuestas automáticas basadas en palabras clave, flujos de conversación con lógica de ramificación y respuestas impulsadas por IA (OpenAI, Anthropic, Google).

- **Respuestas predefinidas**
  Respuestas rápidas predefinidas con comandos de barra (`/atajo`) y marcadores dinámicos.

- **Llamadas de voz e IVR**
  Llamadas de WhatsApp entrantes y salientes con menús IVR, enrutamiento DTMF, transferencias a equipos de agentes, música de espera y grabación de llamadas. Consulta la [documentación de llamadas](https://shridarpatil.github.io/whatomate/features/calling/).

- **Panel de analíticas**
  Supervisa mensajes, interacciones y el rendimiento de las campañas.

<details>
<summary>Ver más capturas</summary>

![Panel](docs/public/images/dashboard-light.png#gh-light-mode-only)
![Panel](docs/public/images/dashboard-dark.png#gh-dark-mode-only)
![Chatbot](docs/public/images/chatbot-light.png#gh-light-mode-only)
![Chatbot](docs/public/images/chatbot-dark.png#gh-dark-mode-only)
![Analíticas de agentes](docs/public/images/agent-analytics-light.png#gh-light-mode-only)
![Analíticas de agentes](docs/public/images/agent-analytics-dark.png#gh-dark-mode-only)
![Constructor de flujos de conversación](docs/public/images/conversation-flow-light.png#gh-light-mode-only)
![Constructor de flujos de conversación](docs/public/images/conversation-flow-dark.png#gh-dark-mode-only)
![Plantillas](docs/public/images/11-templates.png)
![Campañas](docs/public/images/13-campaigns.png)

</details>

## Instalación

### Docker

La imagen más reciente está disponible en Docker Hub en [`shridh0r/whatomate:latest`](https://hub.docker.com/r/shridh0r/whatomate)

```bash
# Descarga el fichero compose, la configuración de ejemplo y el fichero env
curl -LO https://raw.githubusercontent.com/shridarpatil/whatomate/main/docker/docker-compose.yml
curl -LO https://raw.githubusercontent.com/shridarpatil/whatomate/main/config.example.toml
curl -L https://raw.githubusercontent.com/shridarpatil/whatomate/main/docker/.env.example -o .env

# Copia y edita la configuración
cp config.example.toml config.toml
# Edita .env para establecer las credenciales de PostgreSQL y la zona horaria

# Arranca los servicios
docker compose up -d
```

Abre `http://localhost:8080` e inicia sesión con `admin@admin.com` / `admin`

__________________

### Binario

Descarga la [última versión](https://github.com/shridarpatil/whatomate/releases) y extrae el binario.

```bash
# Copia y edita la configuración
cp config.example.toml config.toml

# Ejecuta aplicando las migraciones
./whatomate server -migrate
```

Abre `http://localhost:8080` e inicia sesión con `admin@admin.com` / `admin`

__________________

### Compilar desde el código fuente

```bash
git clone https://github.com/shridarpatil/whatomate.git
cd whatomate

# Compilación de producción (un único binario con el frontend embebido)
make build-prod
./whatomate server -migrate
```

Consulta la [documentación de configuración](https://shridarpatil.github.io/whatomate/getting-started/configuration/) para ver todas las opciones de instalación.

## Uso desde la CLI

```bash
./whatomate server              # API + 1 worker (por defecto)
./whatomate server -workers=0   # Solo API
./whatomate worker -workers=4   # Solo workers (para escalar)
./whatomate version             # Mostrar la versión
```

## Para desarrolladores

El backend está escrito en Go ([Fastglue](https://github.com/zerodha/fastglue)) y el frontend en Vue.js 3 con shadcn-vue.
- Si te interesa contribuir, lee primero [CONTRIBUTING.md](./CONTRIBUTING.md).

```bash
# Entorno de desarrollo
make run-migrate    # Backend (puerto 8080)
cd frontend && npm run dev   # Frontend (puerto 3000)
```

## Licencia

Consulta [LICENSE](LICENSE) para más detalles.
