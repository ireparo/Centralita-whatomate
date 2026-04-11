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

> En Phase 2.1 (commit actual) la UI de "Telefonía PSTN" todavía no existe
> en el panel. Esta sección es un placeholder de cómo será cuando llegue
> Phase 2.2.

Cuando esté lista la UI, harás:

1. iReparo → **Ajustes → Telefonía PSTN → + Nueva conexión**.
2. Rellenar:
   - **Etiqueta**: `Telnyx producción`
   - **Proveedor**: `Telnyx`
   - **API Key**: pegar la del paso 4
   - **Public Key**: pegar la del paso 2
   - **Call Control App ID**: pegar la del paso 2
   - **Outbound Voice Profile ID**: pegar la del paso 1
3. **Probar conexión** (iReparo llama a `/v2/me` con la API key — debe
   devolver tus datos de cuenta).
4. **Guardar**.
5. Después: **Ajustes → Telefonía PSTN → Números → + Añadir**:
   - **Número**: `+34 873 94 07 02`
   - **País**: España
   - **Tipo**: Geográfico
   - **Etiqueta**: `Recepción Barcelona`
   - **IVR de entrada**: seleccionar el flujo que quieras que atienda
   - **Grabación**: ON / OFF
6. **Guardar**.

A partir de ese momento, las llamadas a `+34 873 94 07 02` entran a iReparo,
pasan por el IVR seleccionado y, según las opciones del menú, terminan en
el navegador de un agente (WebRTC) o en otro destino.

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
- **Hasta Phase 2.2 esto es lo esperado.** Phase 2.1 solo loga el
  evento. La lógica de IVR / dispatch llega en el siguiente commit.
- Verifica que el log muestra `Telnyx webhook event event_type=call.initiated`
  cuando llamas. Si no, el problema está en Telnyx side.

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
- **Hasta Phase 2.2 esto es lo esperado.** El bridge WebRTC entre el
  navegador del agente y la pata Telnyx llega en el commit siguiente.

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

## Siguiente fase

Phase 2.2 añadirá:
- Lógica completa del webhook handler (crear `CallLog`, ejecutar IVR, etc.)
- UI de configuración en Ajustes → Telefonía PSTN
- Bridge WebRTC entre el navegador del agente y la pata Telnyx
- Click-to-call desde la ficha de contacto
- Descarga automática de grabaciones a `/app/uploads/recordings/`
