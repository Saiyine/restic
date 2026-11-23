# Plan: `restic backup --migrate fichero.zip` (rama `migracion-de-archivos`)

## 1. Objetivo

Añadir a `restic backup` la opción `--migrate <fichero.zip>` que mete en el repositorio
los ficheros contenidos en el zip, **conservando el path interno de cada entrada**, y
**sin descomprimir nada a disco**: cada entrada se lee del zip, se descomprime en memoria
en streaming y pasa directamente al chunker/deduplicador de restic.

### Requisitos no negociables

1. **Cero extracción a disco.** Ni directorio temporal, ni ficheros intermedios, ni
   `os.CreateTemp`, ni `os.MkdirTemp`. Solo se lee el `.zip` original.
2. **Paths preservados.** El snapshot contiene `/<ruta/dentro/del/zip>` tal cual aparece
   en el zip (tras sanear, ver §5). `restic ls <snapshot>` debe mostrar la misma jerarquía
   que `unzip -l`.
3. **Streaming real.** Memoria acotada: no se carga una entrada completa en RAM; el uso
   debe ser independiente del tamaño de los ficheros del zip.
4. **Restore idéntico.** `restic restore` del snapshot reproduce byte a byte el contenido
   del zip (y modos/fechas que el zip aporte).

### Fuera de alcance (v1)

- Otros formatos (`tar`, `tar.gz`, `7z`). Dejar el diseño preparado, no implementarlos.
- Zips cifrados (se avisa y se omite la entrada, ver §5).
- Leer el zip desde stdin (el formato zip necesita acceso aleatorio al directorio central).
- Zips multi-disco.

## 2. Diseño (decisión de arquitectura)

El `archiver` de restic ya trabaja contra la interfaz `fs.FS` (así funciona `--stdin`
con `internal/fs/fs_reader.go`). La solución es **un sistema de ficheros virtual de solo
lectura respaldado por `archive/zip`**:

```
cmd/restic/cmd_backup.go     --migrate x.zip
        │  construye
        ▼
internal/fs/fs_zip.go        type Zip struct { ... }   // implementa fs.FS
        │  usa
        ▼
archive/zip.Reader           (acceso aleatorio vía io.ReaderAt, descompresión en streaming)
```

- `Zip.OpenFile(name)` para un fichero devuelve un `fs.File` cuyo `Read` lee de
  `zip.File.Open()` (descompresión al vuelo, CRC verificado al llegar a EOF).
- `Zip.OpenFile(name)` para un directorio devuelve un `fs.File` cuyo `Readdirnames`
  sale de un índice en memoria (mapa `dir -> hijos`, ordenado).
- Todas las rutas del FS virtual usan `/` como separador, también en Windows.
- El archiver lee ficheros en paralelo: el `Zip` debe ser **seguro para concurrencia**
  (`zip.Reader` con `ReaderAt` lo permite; no compartir estado mutable entre `File`s).

### Contrato entre agentes (no cambiar sin avisar al coordinador)

```go
// internal/fs/fs_zip.go
func NewZip(zipPath string) (*Zip, error) // abre e indexa el directorio central; NO extrae
func (z *Zip) Close() error
// *Zip implementa fs.FS (mismos métodos que fs.Local / fs.Reader en esta versión)

// cmd/restic/cmd_backup.go
//   flag:  --migrate <ruta.zip>   (campo BackupOptions.Migrate string)
```

## 3. Decisiones que deben validarse en la fase 0

El agente de reconocimiento debe confirmar (y anotar en `NOTAS.md`) cada punto antes de
que nadie escriba código:

1. **Interfaz exacta de `fs.FS` y `fs.File`** en esta versión de restic
   (`internal/fs/interface.go`, y `fs_reader.go` como implementación de referencia).
   Si `fs.File` exige `ToNode(...)`, el `File` del zip debe construir el `Node` él mismo
   (nombre, tipo, modo, mtime, tamaño, `LinkTarget` para symlinks).
2. **Cómo trata el archiver el target `/`** (`cleanTargets`, `resolveRelativeTargets`,
   construcción del árbol raíz). Opción A (preferida): `targets = []string{"/"}`.
   Opción B (si A da problemas): un target por cada entrada de primer nivel del zip
   (`/docs`, `/readme.txt`, ...). Ambas preservan paths; elegir la que funcione sin
   tocar el archiver.
3. **Comprobación de "fichero cambiado durante la lectura"** en `archiver.saveFile`:
   `Lstat` y `File.Stat()` del zip deben devolver metadatos coherentes entre sí.
4. **Qué hace `backup` con `--parent`/detección de cambios** (ver §4.3).

## 4. Comportamiento de `--migrate`

### 4.1 Validación de flags

- `--migrate` es **incompatible** con: argumentos posicionales (targets), `--stdin`,
  `--files-from*`. Error claro en español/inglés según el resto de mensajes de restic.
- Debe comprobar que el fichero existe y es un zip válido **antes** de abrir el repo
  (fallar rápido).

### 4.2 Qué sigue funcionando

`--exclude*`, `--exclude-larger-than`, `--tag`, `--host`, `--time`, `--dry-run`,
`--json`, progreso/escáner (el escáner recorre el FS virtual para calcular totales).

### 4.3 Snapshot padre: **desactivado**

La detección de cambios de restic asume "mismo path + mismo mtime + mismo tamaño ⇒ mismo
contenido". En un zip eso no es fiable (dos zips distintos pueden tener la misma ruta,
mtime y tamaño con contenido diferente) y produciría **datos incorrectos en el backup**.
Por tanto `--migrate` se comporta siempre como `--force` (sin padre) e ignora
inode/ctime. La deduplicación por contenido de restic evita almacenar de nuevo lo repetido,
así que solo se pierde velocidad, no espacio.

### 4.4 Metadatos

| Dato del zip | Snapshot |
|---|---|
| nombre de entrada | path del nodo (saneado, §5) |
| `Modified` | mtime (y atime/ctime = mtime) |
| modo Unix (si `CreatorVersion` es Unix) | `Mode`; si no, `0644` ficheros / `0755` dirs |
| entrada symlink (modo Unix `ModeSymlink`) | nodo symlink; el contenido es el `LinkTarget` |
| directorios explícitos | nodo dir con su mtime |
| directorios implícitos (no listados en el zip) | nodo dir, modo `0755`, mtime = el más reciente de sus descendientes (determinista) |
| uid/gid | 0 (opcional futuro: extra field Info-ZIP `0x7875`) |

## 5. Casos límite y seguridad (obligatorio cubrirlos)

- **Zip-slip / paths peligrosos:** entradas con `..`, ruta absoluta, prefijo de unidad
  (`C:`), barras invertidas, `\0`. Se normalizan a ruta relativa limpia bajo la raíz
  virtual; si tras limpiar escapan de la raíz, se **omiten con warning** (nunca se
  escribe fuera, y además aquí nada se escribe a disco, pero el snapshot no debe contener
  `..`).
- **Entradas duplicadas** (mismo path): gana la última (como `unzip -o`) y se emite warning.
- **Fichero y directorio con el mismo nombre:** warning y se omite la entrada conflictiva.
- **Nombres no UTF-8:** respetar el flag UTF-8 del zip; si no está, tratar como CP437
  (comportamiento estándar) y convertir a UTF-8.
- **Entradas cifradas** (`Flags & 0x1`): warning y omitir; el backup termina con
  código de salida de "completado con ficheros no leídos" (como cualquier fichero no legible).
- **Error de CRC** (`zip.ErrChecksum`) o descompresión corrupta: el fichero se reporta
  como no leído (warning), no se guarda parcial silenciosamente.
- **Zip64, ficheros >4 GB, ficheros vacíos, zip con miles de entradas:** soportados.
- **Compresión no soportada por `archive/zip`:** warning y omitir.
- **Zip-bomb:** memoria acotada por streaming; no se impone límite de tamaño (el usuario
  controla con `--exclude-larger-than`). Documentarlo.

## 6. Paquetes de trabajo (varios agentes)

Regla general: **cada agente es dueño de sus ficheros** (columna "Toca"); si necesita
cambiar un fichero ajeno, lo pide al dueño. Cada agente trabaja en su propio
`git worktree` partiendo de `migracion-de-archivos`, commits pequeños, y rebasa antes de
fusionar.

| Agente | Fase | Toca | Depende de |
|---|---|---|---|
| **A0 – Reconocimiento y rama** | 0 | `NOTAS.md` (nuevo) | — |
| **A1 – ZipFS núcleo** | 1 | `internal/fs/fs_zip.go` | A0 |
| **A2 – Tests unitarios ZipFS** | 1 (en paralelo con A1, sobre el contrato) | `internal/fs/fs_zip_test.go` | A0, contrato §2 |
| **A3 – Integración CLI** | 2 | `cmd/restic/cmd_backup.go` | A1 (compila contra el contrato) |
| **A4 – Tests de integración** | 2 | `cmd/restic/integration_*_test.go` (o el fichero de tests de backup existente) | A3 |
| **A5 – Documentación y changelog** | 2 | `doc/040_backup.rst`, `changelog/unreleased/` | A3 (flag definitivo) |
| **A6 – Revisión final / QA** | 3 | nada (solo reporta/arregla acordado) | todos |

### A0 – Reconocimiento y rama (hacer primero, en serie)

```bash
cd <ruta-local-de-restic>
git status                      # debe estar limpio
git switch -c migracion-de-archivos
go version && go build ./... && go test ./internal/fs/... ./internal/archiver/...
```

- Confirmar los 4 puntos de §3 y escribir las conclusiones en `NOTAS.md`
  (firmas reales de `fs.FS`/`fs.File`, estrategia de target `/` A o B, ejemplo de
  cómo `--stdin` cablea `targetFS` en `cmd_backup.go`).
- Dejar el árbol compilando y los tests base en verde antes de que empiecen A1–A2.

### A1 – ZipFS núcleo (`internal/fs/fs_zip.go`)

1. `NewZip`: `zip.OpenReader`, construir índice (`map[string]*entry`, `map[string][]string`
   para hijos), aplicar saneado de §5, sintetizar directorios implícitos.
2. Implementar los métodos de `fs.FS`: `OpenFile` (solo lectura; cualquier flag de escritura
   → error), `Lstat`, `Join`, `Separator` (`"/"`), `Abs`, `Clean`, `VolumeName` (`""`),
   `IsAbs`, `Dir`, `Base`.
3. `fs.File` de fichero: `Read` sobre `zip.File.Open()`, `Stat`, `Close`, `ToNode` si aplica.
4. `fs.File` de directorio: `Readdirnames` ordenado, `Stat`.
5. Warnings de §5 vía el mecanismo de avisos que use el resto del paquete `fs`.
6. **Invariante:** ninguna llamada a `os.Create*`, `os.MkdirTemp`, `os.WriteFile`,
   `io.Copy` a fichero en este fichero. A6 lo verifica con `grep`.

### A2 – Tests unitarios (`internal/fs/fs_zip_test.go`)

Generar los zips **en el propio test** con `archive/zip` sobre `t.TempDir()` (el zip de
entrada sí puede estar en disco; lo que no se debe crear es nada extraído).

- Jerarquía con dirs explícitos e implícitos; `Lstat`/`Readdirnames` correctos.
- Contenido leído == contenido original (stored y deflate, vacío, >4 GiB opcional con `-short` saltado).
- Modos, mtime, symlink.
- Todos los casos de §5 (zip-slip, duplicados, CP437, cifrado simulado, CRC corrupto).
- Concurrencia: N goroutines leyendo entradas distintas y la misma entrada, con `-race`.
- Test de "no escribe a disco": comprobar que el directorio del zip y `os.TempDir()`
  (apuntado a un dir vacío con `t.Setenv("TMPDIR", ...)`) no ganan ficheros.

### A3 – Integración CLI (`cmd/restic/cmd_backup.go`)

1. Añadir `Migrate string` a `BackupOptions` y el flag `--migrate` con descripción clara.
2. Validación de §4.1.
3. Cableado análogo al de `--stdin`: `targetFS = zipFS`, targets según la decisión de A0;
   `defer zipFS.Close()`.
4. Forzar comportamiento sin padre (§4.3) e ignorar inode/ctime.
5. Mensajes de progreso/summary coherentes (nº de ficheros/bytes leídos del zip).
6. No modificar `internal/archiver` salvo necesidad demostrada en `NOTAS.md`
   (en ese caso, avisar al coordinador antes).

### A4 – Tests de integración

Usar los helpers de test de backup existentes del repo (`testRunBackup`, `testRunLs`,
`testRunRestore`, etc.; confirmar nombres en esta versión).

- Crear zip con árbol conocido → `backup --migrate` → `ls` muestra los mismos paths que el zip.
- `restore` a un dir temporal → comparar byte a byte (y modos/mtime) con el contenido del zip.
- Segundo `backup --migrate` del mismo zip: no sube datos nuevos (dedup) y el snapshot es válido.
- Flags incompatibles dan error; zip inexistente/corrupto da error antes de tocar el repo.
- `--exclude` y `--exclude-larger-than` aplican sobre los paths virtuales.
- Test de memoria (se salta con `-short`): zip con un fichero muy compresible de varios
  cientos de MB; el pico de heap debe ser muy inferior al tamaño del fichero.
- `--dry-run` no escribe nada en el repo.

### A5 – Documentación y changelog

- Sección en `doc/040_backup.rst`: uso, paths resultantes, ejemplo, limitaciones
  (§1 fuera de alcance, §4.3 sin padre, §5 cifrados), nota sobre que no se extrae a disco.
- Entrada en `changelog/unreleased/` siguiendo el formato de las existentes.
- Revisar que `restic backup --help` y la generación de man/completions (`restic generate`)
  incluyen el flag sin cambios manuales.

### A6 – Revisión final / QA

- `gofmt -l .` vacío, `go vet ./...`, linter del repo (`golangci-lint run` si hay config).
- `go test -race ./internal/fs/... ./internal/archiver/... ./cmd/restic/...`
- Compilación cruzada: `GOOS=windows go build ./...`, `GOOS=darwin go build ./...`.
- `grep -nE "os\.(Create|MkdirTemp|CreateTemp|WriteFile)|ioutil\.Temp" internal/fs/fs_zip.go cmd/restic/cmd_backup.go`
  → no debe aparecer nada nuevo relacionado con `--migrate`.
- Prueba manual end-to-end con un zip real grande, vigilando que no aparecen ficheros
  temporales (`strace -f -e trace=openat,mkdir ...` en Linux o `lsof`/Process Monitor).
- Confirmar los criterios de §7.

## 7. Criterios de aceptación

- [ ] `restic backup --migrate datos.zip` crea un snapshot cuyo `restic ls` coincide con `unzip -l`.
- [ ] `restic restore` reproduce el contenido exacto del zip.
- [ ] No se crea ningún fichero/directorio temporal durante el backup.
- [ ] Memoria acotada con ficheros grandes dentro del zip.
- [ ] Todos los casos de §5 tienen test.
- [ ] Sin cambios en el comportamiento de `restic backup` cuando no se usa `--migrate`.
- [ ] Tests existentes del repo siguen en verde (`-race`), build en Linux/Windows/macOS.
- [ ] Documentación y changelog presentes.

## 8. Orden de ejecución y protocolo

1. **A0** (serie): rama, baseline, `NOTAS.md`.
2. **A1 ∥ A2** en paralelo (contrato §2 fijado).
3. **A3 ∥ A5** en cuanto A1 compile; **A4** cuando A3 esté fusionado.
4. **A6** al final; si encuentra fallos, abre issues en `NOTAS.md` y el dueño del fichero los corrige.
5. Fusión a `migracion-de-archivos` con commits pequeños y mensajes tipo
   `fs: add read-only zip filesystem` / `backup: add --migrate flag`.
   **No tocar `master`/`main` de restic.**
6. Cada agente, al terminar, deja en `NOTAS.md` un resumen de 5 líneas: qué hizo, qué
   decisiones tomó, qué queda pendiente.

## 9. Ampliaciones futuras (no hacer ahora)

- Soporte `tar`/`tar.gz`/`tar.zst` (streaming puro, sin acceso aleatorio): misma idea,
  otro `fs.FS` virtual, flag genérico `--migrate` con detección por extensión.
- uid/gid desde el extra field `0x7875`.
- Prefijo de ruta opcional (`--migrate-prefix /destino`) para colgar el contenido bajo otra raíz.
