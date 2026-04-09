# Internacionalización (i18n)

Este proyecto utiliza [vue-i18n](https://vue-i18n.intlify.dev/) para la internacionalización y [Crowdin](https://crowdin.com/) para la gestión de traducciones.

## Añadir nuevos idiomas

Los idiomas se **detectan automáticamente** en la carpeta `locales/`. Para añadir un idioma nuevo:

1. Crea un nuevo fichero JSON: `locales/{codigo_idioma}.json` (por ejemplo, `es.json` para español)
2. Copia la estructura de `en.json`
3. Traduce todas las cadenas
4. El idioma aparecerá automáticamente en el selector de idiomas

## Usar traducciones en los componentes

```vue
<script setup>
import { useI18n } from 'vue-i18n'

const { t } = useI18n()
</script>

<template>
  <!-- Traducción simple -->
  <p>{{ $t('common.save') }}</p>

  <!-- Con interpolación -->
  <p>{{ $t('contacts.total', { count: 42 }) }}</p>

  <!-- Desde el script -->
  <button @click="alert(t('common.success'))">Click</button>
</template>
```

## Estructura de las claves de traducción

```
common.*      - Elementos comunes de la interfaz (botones, etiquetas)
auth.*        - Relacionados con la autenticación
nav.*         - Elementos del menú de navegación
chat.*        - Interfaz de chat
contacts.*    - Gestión de contactos
settings.*    - Páginas de configuración
users.*       - Gestión de usuarios
analytics.*   - Panel de analíticas
templates.*   - Plantillas de WhatsApp
errors.*      - Mensajes de error
validation.*  - Mensajes de validación de formularios
time.*        - Cadenas de tiempo relativo
```

## Para traductores (vía Crowdin)

1. Ve a [URL del proyecto en Crowdin]
2. Selecciona tu idioma
3. Traduce las cadenas desde la interfaz web
4. Las traducciones se sincronizan automáticamente con este repositorio

## Para desarrolladores

### Añadir cadenas nuevas

1. Añade la cadena en `locales/en.json`
2. Utiliza claves jerárquicas y con significado: `seccion.subseccion.accion`
3. Usa interpolación para los valores dinámicos: `"Hola, {name}!"`

### Cambiar el idioma por código

```typescript
import { setLocale, getLocale, SUPPORTED_LOCALES } from '@/i18n'

// Obtener el idioma actual
const current = getLocale()

// Cambiar el idioma
setLocale('es')

// Listar los idiomas disponibles
console.log(SUPPORTED_LOCALES)
```

## Estructura de ficheros

```
src/i18n/
├── index.ts          # Configuración de i18n
├── README.md         # Este fichero
└── locales/
    ├── en.json       # Inglés (origen)
    ├── es.json       # Español
    ├── fr.json       # Francés
    └── ...           # Otros idiomas
```

## Integración con Crowdin

### Configuración inicial (una sola vez)

1. Crea un proyecto en Crowdin
2. Define las variables de entorno:
   ```
   CROWDIN_PROJECT_ID=tu_id_de_proyecto
   CROWDIN_PERSONAL_TOKEN=tu_token
   ```

### Sincronizar traducciones

```bash
# Subir el fichero origen
npx crowdin upload sources

# Descargar traducciones
npx crowdin download
```

### Integración con GitHub

Crowdin puede configurarse para:
- Sincronizar automáticamente cuando `en.json` cambie
- Crear PRs con las nuevas traducciones
- Ver: https://support.crowdin.com/github-integration/
