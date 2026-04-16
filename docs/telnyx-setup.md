# iReparo + Telnyx — Guía de configuración

Esta guía cubre **todo lo que tienes que hacer en el panel de Telnyx** después de
que tu número español esté aprobado, para que iReparo pueda enviar y recibir
llamadas de fijos y móviles a través de él.

> Asume que ya tienes:
>
> - Una cuenta Telnyx con verificación KYC aprobada para España.
> - Al menos un número geográfico español (ej: `+34 873 94 07 02`) en estado
>   **Active**.
> - Saldo en la cuenta (>= 5 €).
> - iReparo desplegado en `https://pbx.ireparo.es` con HTTPS funcionando
>   (Caddy + Let's Encrypt).

---

## Resumen de los 6 pasos

```
1. Crear una Outbound Voice Profile         (define qué se puede marcar)
2. Crear una Call Control Application       (vincula número ↔ webhook iReparo)
3. Asignar el número español al Call Control App
4. Generar una API Key v2                   (credencial para el cliente Go)
5. Copiar la Public Key del Call Control App (para verificar firmas)
6. Pegar todo en iReparo → Ajustes → Telefonía PSTN
```

---

## 1. Crear una Outbound Voice Profile

La **Outbound Voice Profile** es la que determina qué destinos puedes marcar
desde tu cuenta y a qué precio. Si no la creas, las llamadas salientes
fallarán con error de billing.

1. Ve a https://portal.telnyx.com → menú lateral → **Voice → Outbound Voice
   Profiles** → **+ Add new profile**.
2. Rellena:
   - **Name**: `iReparo Outbound`
   - **Traffic Type**: `Conversational`
   - **Service Plan**: `Global`
   - **Concurrent call limit**: deja en blanco para "sin límite", o pon 10
     como precaución contra fugas.
   - **Daily spend limit**: pon `5` (5 € / día) para evitar sustos en las
     primeras pruebas. Lo subes luego.
   - **Allowed Destinations**: marca `Spain` como mínimo. Si esperas llamar
     a otros países, márcalos también — sin marcar un país, las llamadas a
     ese país se rechazan.
3. **Save**.
4. Anota el **Outbound Profile ID** (algo como `12abcdef-3456-7890-abcd-ef1234567890`).

> 💡 **Por qué un límite de gasto diario**: Telnyx factura a posteriori. Si
> alguien (o un bug) hace mil llamadas a +34 móviles en un loop, el coste se
> dispara. El daily spend limit es tu cinturón de seguridad. Para producción
> normal de iReparo (~50 llamadas/día) con 10-20 € basta.

---

## 2. Crear una Call Control Application

Esto es la "configuración" que asocia uno o varios números con un webhook
URL. iReparo escucha los eventos que Telnyx envía a esa URL para saber qué
está pasando con cada llamada.

1. https://portal.telnyx.com → menú lateral → **Voice → Programmable Voice
   → Call Control** → **+ Create Call Control App**.
2. Rellena:
   - **Name**: `iReparo PBX`
   - **Webhook URL**: `https://pbx.ireparo.es/api/webhook/telnyx`
   - **Webhook Failover URL**: deja en blanco (o pon la misma URL si quieres
     reintentos a la misma instancia).
   - **Webhook API version**: `v2` (debería estar marcada por defecto).
   - **Outbound Voice Profile**: selecciona la que creaste en el paso 1
     (`iReparo Outbound`).
   - **Outbound Settings → Outbound voice profile**: confirma la asociación.
   - **DTMF Type**: `RFC 2833` (estándar y compatible con todo).
   - **First command timeout**: `30` segundos (el tiempo que Telnyx espera
     a que iReparo responda al primer evento antes de cortar — si lo dejas
     muy bajo y el VPS está cargado, las llamadas pueden empezar mal).
3. **Save**.
4. Anota dos cosas críticas:
   - **Call Control App ID** — algo como `2a55f0c1-1234-5678-abcd-ef0123456789`. Es lo que va en
     `TelnyxConnection.CallControlAppID` en iReparo.
   - **Public Key** (justo debajo del nombre de la app, hay un botón
     **Show public key**). Es una cadena base64 de ~44 caracteres. Es la
     clave Ed25519 que iReparo usa para verificar que los webhooks vienen
     de verdad de Telnyx y no de un atacante. Cópiala entera, sin saltos
     de línea.

> 💡 La Public Key puede regenerarse en cualquier momento si la perdiste o
> si crees que se filtró. Al regenerarla tienes que actualizarla también en
> iReparo (Ajustes → Telefonía PSTN → Editar conexión).

---

## 3. Asignar el número al Call Control App

Por defecto los números nuevos no están asociados a ninguna aplicación, lo
que significa que las llamadas que entran simplemente se rechazan con
"destination not configured".

1. https://portal.telnyx.com → menú lateral → **Numbers → My Numbers**.
2. Haz click en tu número español (ej: `+34 873 94 07 02`).
3. En la pestaña **Voice** → **Connection / Call Control App**, selecciona
   `iReparo PBX` (la app que creaste en el paso 2).
4. **Translated Number Format**: deja en `+E.164`.
5. **CNAM listing**: si quieres que aparezca tu nombre comercial en el
   identificador de llamadas (cuando llames a otros), márcalo y pon `iReparo`
   o el nombre de tu empresa. **Esto es opcional y solo funciona en algunos
   destinos.**
6. **Save**.

A partir de este momento, cualquier llamada al `+34 873 94 07 02` debería
generar un evento `call.initiated` en `https://pbx.ireparo.es/api/webhook/telnyx`.

> 💡 Para verificar que el webhook llega: en el panel de iReparo (cuando
> esté la UI), o por SSH al VPS:
>
> ```bash
> docker compose -f docker/docker-compose.yml -f docker/docker-compose.prod.yml \
>   logs -f app | grep "Telnyx webhook"
> ```
>
> Llama desde tu móvil personal al +34 873 94 07 02 y deberías ver pasar
> un `Telnyx webhook event event_type=call.initiated` por la consola.
> Si no lo ves, revisa: (a) que el webhook URL en Telnyx no tenga
> espacios o sea http en vez de https; (b) que iReparo esté arriba; (c)
> que Caddy tenga certificado válido — Telnyx rechaza certificados
> autofirmados.

---

## 4. Generar una API Key v2

La API Key es la credencial que usa el código Go de iReparo para hacer
llamadas salientes, contestar llamadas entrantes desde el panel del agente,
transferir llamadas, etc.

1. https://portal.telnyx.com → tu avatar arriba a la derecha → **Account
   Settings → API Keys**.
2. **+ Create API Key**.
3. **Name**: `iReparo PBX backend`.
4. **Save**. Te muestra la key **una sola vez**, en formato `KEY01ABCDEF...`
   con 50+ caracteres. **Cópiala ahora**, no se puede recuperar después
   (solo regenerar otra nueva).
5. Pégala en un sitio temporal (no en un email, no en un Notion público).
   La vas a meter en iReparo en el paso 6 y luego la borras del sitio
   temporal.

> 💡 **Alcance**: la API Key v2 tiene acceso completo a tu cuenta Telnyx —
> puede comprar números, hacer llamadas, ver facturas, etc. Es muy potente.
> Manéjala como una contraseña de banco. iReparo la guarda **cifrada** en
> la base de datos con AES-256 usando `app.encryption_key`, así que aunque
> alguien acceda al Postgres no la puede leer en claro.

---

## 5. (Opcional pero recomendado) Habilitar grabación de llamadas

Si quieres que TODAS las llamadas se graben automáticamente en Telnyx y
luego se descarguen a iReparo:

1. En la **Call Control App** del paso 2, ve a la pestaña **Settings**.
2. Busca **Recording Settings** o configúralo por número desde **Numbers**.
3. Activa **Record on answer** = `Yes`.
4. **Channels**: `dual` (canal separado para cada lado de la conversación,
   mejor calidad para análisis posterior).
5. **Format**: `mp3` (más pequeño que WAV, calidad suficiente).
6. **Save**.

A partir de ese momento, cada llamada genera un evento
`call.recording.saved` con la URL del MP3 cuando termina. iReparo lo
descarga y lo guarda en el volumen `/app/uploads/recordings/` (o en S3 si
configuraste storage S3).

> ⚠️ **Aviso legal obligatorio**: en España estás obligado a notificar al
> llamante que se graba la llamada **antes** de que empiece. Esto se hace
> con un nodo de "Reproducir audio" en el constructor visual de IVR de
> iReparo, justo antes del menú principal. Plantilla del aviso:
>
> > "Esta llamada va a ser grabada con fines de calidad del servicio y
> > formación. Si no desea ser grabado, por favor cuelgue ahora."

---

## 6. Pegar todo en iReparo

Entra en **iReparo → Ajustes → Telefonía PSTN**. La primera vez verás el
estado vacío con un botón **Añadir conexión**.

### 6.1 Conexión Telnyx

1. Pulsa **Añadir conexión**. Rellena:
   - **Etiqueta**: `Telnyx producción`
   - **API Key**: pega la del paso 4 (`KEY...`)
   - **Public Key**: pega la del paso 2 (base64, ~44 caracteres)
   - **Call Control App ID**: pega la del paso 2 (UUID)
   - **Outbound Voice Profile ID**: pega la del paso 1 (opcional, solo si
     vas a usar llamadas salientes / click-to-call)
2. Pulsa **Probar conexión** → iReparo llama a `/v2/me` con la API key
   y debe mostrar un toast verde **"Credenciales válidas"**. Si sale
   rojo, revisa la API Key.
3. Pulsa **Crear**. La tarjeta muestra ahora el estado de la conexión,
   la API Key enmascarada (`••••••`), y la fecha de última verificación.

> Solo puede haber **una conexión Telnyx por organización**. Si quieres
> rotar la API key, pulsa **Editar** y pega solo la nueva key (los
> campos vacíos mantienen los valores actuales).

### 6.2 Números (DDI)

Con la conexión creada, aparece debajo la tarjeta de **Números**.

1. Pulsa **Añadir número**. Rellena:
   - **Etiqueta**: `Recepción Barcelona`
   - **Número de teléfono**: `+34 873 94 07 02` (se normaliza a E.164
     automáticamente, acepta espacios, paréntesis y guiones)
   - **País (ISO)**: `ES`
   - **Tipo**: `Geográfico` / `Móvil` / `Gratuito (900)` / `Nacional` /
     `Virtual` — elige el que corresponda
   - **Flujo IVR entrante**: selecciona un flujo de los que has
     construido en **Llamadas → Flujos IVR**. Déjalo en "Sin flujo
     asignado" si quieres rechazar llamadas entrantes en ese número.
   - **Activo**: ON
   - **Grabación**: ON / OFF (ver sección 5 sobre aviso legal)
2. **Crear**.

A partir de este momento, las llamadas al `+34 873 94 07 02` entran a
iReparo, pasan por el flujo IVR seleccionado y terminan donde lo
decida el flujo (transferencia a agente, hangup, menu, etc.).

> Puedes tener varios números bajo la misma conexión, cada uno con su
> propio flujo IVR. Típicamente: uno para recepción, otro para
> facturación, otro para soporte técnico.

---

## 7. Constructor de flujos IVR

Las llamadas entrantes no llegan "al panel" directamente — pasan por un
**flujo IVR** que tú construyes visualmente en
**Llamadas → Flujos IVR** (o **Calling → IVR Flows**).

Cada flujo es un grafo de **nodos** (acciones) conectados por **edges**
(condiciones). iReparo soporta los mismos 8 tipos de nodo en llamadas
WhatsApp y Telnyx, con el mismo editor visual.

### 7.1 Tipos de nodo

| Nodo | Qué hace | Salidas (edges) |
|------|----------|-----------------|
| **Greeting** | Reproduce un audio (TTS o MP3 subido) | `default` |
| **Menu** | Reproduce prompt + captura **1 dígito** con validación | `digit:1`, `digit:2`, ..., `timeout`, `max_retries` |
| **Gather** | Captura **múltiples dígitos** terminados en `#` | `default`, `timeout`, `max_retries` |
| **HTTP Callback** | Hace una petición HTTP a un servicio externo | `http:2xx`, `http:non2xx` |
| **Transfer** | Transfiere la llamada a un equipo de agentes | `completed`, `no_answer` |
| **Goto Flow** | Salta a otro flujo IVR (entry node) | (terminal en este flujo) |
| **Timing** | Evalúa horario comercial | `in_hours`, `out_of_hours` |
| **Hangup** | Cuelga la llamada | (terminal) |

### 7.2 Configuración de cada nodo

**Greeting**
```json
{
  "audio_file": "welcome_123.mp3",
  "interruptible": false
}
```
El admin sube el audio con el botón **Upload audio** del editor o usa
el generador de TTS integrado. `audio_file` es el filename local
servido por iReparo; el dispatcher Telnyx lo convierte a una URL firmada
pública automáticamente (ver sección 8).

**Menu**
```json
{
  "audio_file": "menu_main.mp3",
  "timeout_seconds": 10,
  "max_retries": 3,
  "invalid_audio_url": "https://cdn.tuempresa.es/tono-invalido.mp3",
  "options": {
    "1": { "label": "Ventas" },
    "2": { "label": "Soporte" },
    "3": { "label": "Facturación" }
  }
}
```
Cuando el caller pulsa `1`, `2` o `3`, el flujo sale por la edge
`digit:1` / `digit:2` / `digit:3`. Si no pulsa nada, sale por `timeout`.
`invalid_audio_url` (opcional, solo Telnyx) es un MP3 que Telnyx
reproduce si el caller pulsa un dígito no listado.

**Gather**
```json
{
  "audio_file": "pide_dni.mp3",
  "max_digits": 9,
  "terminator": "#",
  "timeout_seconds": 10,
  "store_as": "dni"
}
```
Guarda los dígitos en la variable `dni` y sigue por la edge `default`.
La variable está disponible para nodos posteriores (típicamente un
`http_callback`).

**HTTP Callback**
```json
{
  "url": "https://sat.ireparo.es/api/public/ticket-status?dni={{dni}}",
  "method": "GET",
  "headers": {
    "X-Api-Key": "bearer-xxx"
  },
  "body_template": "",
  "timeout_seconds": 5,
  "response_store_as": "ticket_status"
}
```
Se interpolan las variables `{{dni}}` con lo recogido en el `gather`
anterior. La respuesta se guarda (truncada a 1 KB) en `ticket_status`
para poder interpolarla en un `greeting` posterior. Sale por `http:2xx`
si el status fue 200-299, `http:non2xx` en cualquier otro caso.

**Transfer**
```json
{
  "team_id": "uuid-del-equipo",
  "timeout_secs": 120
}
```
Enruta la llamada a los agentes del equipo. El flujo continúa por
`completed` cuando el transfer acaba, o `no_answer` si nadie contestó.

**Goto Flow**
```json
{
  "flow_id": "uuid-del-flujo-destino"
}
```
Salta al entry node del flujo indicado. Útil para sub-flujos
compartidos (ej: "horario de atención" reusado en varios números).

**Timing**
```json
{
  "schedule": [
    { "day": "monday",    "enabled": true,  "start_time": "09:00", "end_time": "18:00" },
    { "day": "tuesday",   "enabled": true,  "start_time": "09:00", "end_time": "18:00" },
    { "day": "wednesday", "enabled": true,  "start_time": "09:00", "end_time": "18:00" },
    { "day": "thursday",  "enabled": true,  "start_time": "09:00", "end_time": "18:00" },
    { "day": "friday",    "enabled": true,  "start_time": "09:00", "end_time": "15:00" },
    { "day": "saturday",  "enabled": false, "start_time": "00:00", "end_time": "00:00" },
    { "day": "sunday",    "enabled": false, "start_time": "00:00", "end_time": "00:00" }
  ]
}
```
Sale por `in_hours` si la llamada llega dentro del horario del día
correspondiente; `out_of_hours` en caso contrario. El reloj usa la
zona horaria del servidor (UTC por defecto — si necesitas Europe/Madrid
lo puedes cambiar con `TZ=Europe/Madrid` en el compose).

**Hangup**
```json
{
  "audio_file": "despedida.mp3"
}
```
Reproduce el audio (opcional) y cierra la llamada.

### 7.3 Ejemplo de flujo completo

Flujo típico de recepción fuera de horario:

```
entry
  │
  ▼
[timing]──in_hours──▶ [menu: 1=ventas 2=soporte 3=hablar]
   │                     │
   └out_of_hours          ├digit:1─▶ [transfer team=ventas]
   │                     ├digit:2─▶ [transfer team=soporte]
   │                     └digit:3─▶ [gather dni] ─▶ [http_callback lookup] ─▶ [transfer team=agentes]
   ▼
[greeting: "fuera_horario.mp3"]
  │
  ▼
[hangup]
```

---

## 8. Audio de IVR: cómo funciona

iReparo almacena los audios localmente (o en S3 si lo configuras) y
los sirve con dos endpoints:

- `GET /api/ivr-flows/audio/{filename}` — requiere JWT (para preview en
  el editor).
- `GET /api/public/ivr-audio/{filename}?e=<expiry>&s=<hmac>` — público
  pero firmado con HMAC-SHA256 del JWT secret. URL firmada válida
  durante 15 minutos, generada por el dispatcher Telnyx automáticamente
  al emitir cada comando de playback / gather.

Esto hace que **no tienes que hospedar los audios tú mismo** — Telnyx
los fetchea vía la URL firmada que iReparo le pasa en cada
`gather_using_audio` / `playback_start`.

Si prefieres hospedar los audios tú (en un CDN), puedes rellenar el
campo `audio_url` del nodo (solo Telnyx lo mira) en vez de usar
`audio_file`. Por ejemplo en el campo **"Invalid Digit Audio URL"** del
nodo menu.

---

## 9. Click-to-call desde el panel

Un agente puede marcar a un contacto con un click desde la ficha del
chat. Usa el **patrón callback** de dos patas — suena primero al
teléfono del agente, y cuando éste descuelga, Telnyx transfiere la
llamada al cliente.

### 9.1 Configurar el teléfono del agente

Cada agente **tiene que configurar su teléfono personal** antes de poder
usar click-to-call:

1. Agente → **Perfil** (menú de usuario → Perfil)
2. Tarjeta **Teléfono personal (click-to-call)** → introducir en
   formato internacional: `+34 666 11 22 33`
3. **Guardar**. Se normaliza a E.164 automáticamente.

Si el teléfono está vacío, el botón "Llamar" aparece deshabilitado con
tooltip explicativo.

### 9.2 Hacer una llamada

1. En **Chat**, abre la conversación del cliente.
2. En el panel lateral derecho (ficha de contacto), verás el botón
   **Llamar** justo debajo del número.
3. Pulsa. Toast amarillo: "Llamando a tu teléfono — al descolgar,
   conectaremos con el cliente".
4. Tu móvil suena desde el número Telnyx de la organización (el
   primero activo).
5. Descuelgas → tras 1-2 segundos Telnyx marca al cliente y te conecta.

Todo queda registrado en **Llamadas → Registro de llamadas** como una
llamada `outgoing` del canal `telnyx_pstn`, con tu UUID en `agent_id`.

> ⚠️ Necesitas que la **Outbound Voice Profile** del paso 1 permita
> llamar al país del cliente. Si no, verás un error "Telnyx dial
> failed: ..." al intentar marcar.

---

## 10. Dashboard de analíticas de llamadas

En **Analíticas → Analíticas de llamadas** tienes una vista en tiempo
real (tras cada refresh) con:

- **4 KPIs principales**: total, contestadas (+%), perdidas (+%),
  duración media
- **KPIs secundarios**: entrantes/salientes, tiempo total en llamada,
  rango efectivo
- **Gráficas**:
  - Tendencia diaria (total / contestadas / perdidas)
  - Distribución horaria 24h (útil para planificar turnos)
  - Desglose por estado (donut)
  - Desglose por canal WhatsApp vs Telnyx (donut)
  - Top 10 flujos IVR por volumen
  - Top 10 agentes por llamadas atendidas

Filtros:
- **Canal**: Todos / WhatsApp / Telnyx PSTN
- **Dirección**: Todas / Entrantes / Salientes
- **Rango de fechas**: presets (hoy, 7 días, 30 días, este mes) o
  personalizado

Permiso necesario: `call_logs:read` (el mismo que para ver el listado
de llamadas).

---

## 11. Cola CRM (admin dead-letter queue)

Si tienes configurada la integración con un CRM externo, los eventos de
llamadas / mensajes se envían con reintentos exponenciales. Los que
exceden 10 intentos aterrizan en la cola dead-letter.

Para gestionarla: **Ajustes → Cola CRM**.

- **Filtros** por estado con contadores: Fallidos / Pendientes /
  Entregados / Todos
- **Ver payload** (icono del ojo) → muestra el JSON del evento y el
  último error
- **Reintentar** (icono play) → resetea intentos a 0 y hace un envío
  inmediato. Si falla, el worker lo reintenta con el backoff normal
- **Eliminar** (icono papelera) → borra permanentemente

Permiso necesario: `settings.general:read/write/delete`.

---

## Resolución de problemas

### El webhook nunca llega
- Verifica que el URL en la Call Control App es **exactamente**
  `https://pbx.ireparo.es/api/webhook/telnyx` (sin barra final, https,
  sin espacios).
- Verifica desde el VPS que el endpoint responde:
  ```bash
  curl -X POST https://pbx.ireparo.es/api/webhook/telnyx \
    -H "Content-Type: application/json" \
    -d '{"data":{"event_type":"test","id":"x","occurred_at":"2026-04-09T18:00:00Z","payload":{}}}'
  ```
  Debe devolver 200 con `{"status":"ignored"}`.
- Mira el log de Caddy: si hay 502, iReparo está caído. Si hay 404, el
  URL no está bien. Si hay 200, llega.

### Las llamadas entrantes ring pero no pasa nada en iReparo
- Verifica que el log muestra `Telnyx webhook event event_type=call.initiated`
  cuando llamas. Si no, el problema está en Telnyx side (webhook mal
  configurado).
- Verifica que el número tiene un **flujo IVR asignado** en **Ajustes →
  Telefonía PSTN → Números**. Sin flujo, la llamada se cuelga
  inmediatamente.
- Verifica que el flujo IVR está **Activo** (`is_active=true`). Flujos
  inactivos son ignorados.

### "Invalid signature" en los logs
- La Public Key copiada está mal o le faltan caracteres. Vuelve al paso 2
  y cópiala entera, sin saltos de línea.
- Reloj del VPS desincronizado. Ejecuta `timedatectl status` y verifica
  que `System clock synchronized: yes`. Si no, `apt install ntp` y
  reinicia.

### Llamadas salientes fallan con "No outbound profile assigned"
- La Call Control App no tiene asociada la Outbound Voice Profile del
  paso 1. Vuelve al paso 2 y verifica el dropdown **Outbound Voice
  Profile**.

### El agente no oye nada cuando contesta una llamada Telnyx

El patrón actual (Phase 2.6) no usa WebRTC para llamadas Telnyx; usa el
**modelo callback**:
- **Entrantes** → enrutadas por IVR, que al llegar a un nodo `transfer`
  llama al móvil del agente (si tiene teléfono configurado).
- **Salientes (click-to-call)** → Telnyx marca al agente primero,
  luego transfiere al cliente.

Si el agente no oye audio:
- Verifica que el agente tiene **teléfono personal configurado** en su
  Perfil.
- Verifica que el teléfono está encendido y no tiene el número
  bloqueado.
- Comprueba que la Outbound Voice Profile permite llamar al país del
  agente (por si el agente está en otro país que el cliente).

### "Failed to dial" al hacer click-to-call
- El agente no tiene `phone_number` configurado → mensaje explícito.
- No hay ningún `TelnyxNumber` activo en la organización → añade uno
  en **Ajustes → Telefonía PSTN**.
- Telnyx rechaza la llamada: suele ser la **Outbound Voice Profile** —
  revisa que el país del agente está marcado como permitido.
- **Daily spend limit** alcanzado: sube el límite en Telnyx o espera
  al reset a medianoche UTC.

### El audio del IVR no se escucha en llamadas Telnyx
- Verifica que el nodo tiene `audio_file` configurado (el dispatcher
  construye la URL firmada automáticamente).
- Mira el log — `Playback failed: invalid audio URL` indica que el
  firmado falla. Suele ser un reloj desincronizado o el JWT secret
  distinto entre el handler y el dispatcher (típicamente no pasa, pero
  si clonas el binario entre máquinas sin copiar `config.toml` sí).
- Prueba directamente la URL firmada con `curl` desde el VPS:
  ```bash
  curl -v "https://pbx.ireparo.es/api/public/ivr-audio/welcome.mp3?e=<exp>&s=<sig>"
  ```
  Debe devolver el MP3 con 200.

---

## Costes orientativos (España, abril 2026)

| Concepto                              | Coste              |
|---------------------------------------|--------------------|
| Llamada entrante a número geográfico  | Gratis             |
| Llamada saliente a fijo nacional      | ~0,006 €/min       |
| Llamada saliente a móvil nacional     | ~0,04 €/min        |
| Llamada saliente a fijo Europa        | 0,01-0,02 €/min    |
| Llamada saliente a móvil Europa       | 0,05-0,15 €/min    |
| Llamada saliente a USA/Canadá         | ~0,008 €/min       |
| Grabación de llamadas                 | Incluido           |
| Almacenamiento de grabaciones (Telnyx)| Primeros 5 GB free |
| Mantenimiento del número geográfico   | ~1 €/mes           |

Comprueba precios actualizados en https://telnyx.com/pricing/voice antes
de proyectar costes — Telnyx revisa tarifas un par de veces al año.

---

## Estado de la integración

| Fase | Descripción | Estado |
|------|-------------|--------|
| 2.1 | Modelos + cliente API + webhook stub | ✅ |
| 2.2 | Dispatcher + IVR + descarga de grabaciones | ✅ |
| 2.3 | Nodos IVR extendidos (`menu`, `gather`, `http_callback`, `goto_flow`, `timing`) | ✅ |
| 2.4 | UI de admin Telnyx (conexión + números) | ✅ |
| 2.5 | Bridge de audio entre canales WhatsApp y Telnyx (URL firmada) | ✅ |
| 2.6 | Click-to-call (patrón callback) | ✅ |
| 2.7 | Dashboard de analíticas de llamadas | ✅ |
| 3.1 A | PBX → CRM (lookup + eventos de llamada) | ✅ |
| 3.1 B | Spec de integración CRM + plantillas Laravel | ✅ |
| 3.2 | Eventos de mensajes + admin cola dead-letter | ✅ |

## Posibles siguientes pasos

- **Outbound con WebRTC** — reemplazar el callback por un bridge WebRTC
  directo agent-browser ↔ Telnyx (requiere infra adicional).
- **SMS via Telnyx** — el cliente y la cuenta ya están; añadir modelo
  `TelnyxMessage` + handlers + UI.
- **Campañas outbound** — bulk dial con throttling y estadísticas.
- **Transcripción automática** — Whisper local o API, integrado con el
  pipeline de grabaciones.
- **Per-user default outbound number** — actualmente el sistema usa el
  primer TelnyxNumber activo; podría elegirlo cada agente.
