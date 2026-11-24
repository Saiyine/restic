# NOTAS — `--migrate` (A0: reconocimiento)

Rama: `migracion-de-archivos` (creada desde `master` @ 8f4873d5d).
Baseline: `go build ./...` OK, `go test ./internal/fs/... ./internal/archiver/...` verde (go1.26.10).

## Decisiones del §3 (validadas contra el código)

### 1. Interfaz exacta `fs.FS` / `fs.File` (`internal/fs/interface.go`)

```go
type FS interface {
    OpenFile(name string, flag int, metadataOnly bool) (File, error) // solo O_RDONLY|O_NOFOLLOW|O_DIRECTORY
    Lstat(name string) (*ExtendedFileInfo, error)
    Join(elem ...string) string
    Separator() string
    Abs(path string) (string, error)
    Clean(path string) string
    VolumeName(path string) string
    IsAbs(path string) bool
    Dir(path string) string
    Base(path string) string
}

type File interface {
    MakeReadable() error
    io.Reader
    io.Closer
    Readdirnames(n int) ([]string, error)
    Stat() (*ExtendedFileInfo, error)
    ToNode(ignoreXattrListError bool, warnf func(format string, args ...any)) (*data.Node, error)
}
```

- Confirmado: `File` **exige `ToNode`**. El `File` del zip construye el `data.Node`
  él mismo desde el header del zip (nombre, tipo, modo, mtime, tamaño, `LinkTarget`).
- Referencia de implementación: `fs_reader.go` (`reader`, `readerFile`, `fakeFile`,
  `fakeDir`, `pathError`). `ToNode` de referencia: `buildBasicNode(path, fi)` + uid/gid + ChangeTime.
- `fs.Readdirnames(filesys, dir, flags)` abre con `O_RDONLY|O_DIRECTORY|O_NOFOLLOW` y
  `metadataOnly=false`, y llama `Readdirnames(-1)`. El `Zip.OpenFile` debe aceptar esas
  flags en directorios.
- Metadatos: `data.Node` usa `ModTime/AccessTime/ChangeTime`, `Mode`, `Size` (solo ficheros
  regulares, ver `buildBasicNode`), `LinkTarget`/`LinkTargetRaw` para symlinks.

### 2. Target `/` (Opción A) — funciona, sin tocar el archiver

`resolveRelativeTargets` (archiver.go:794) expande cualquier target sin componentes de
path (p. ej. `/`) con `fs.Readdirnames(filesys, target, O_NOFOLLOW)` y crea un target por
cada entrada de primer nivel con `Explicit: false` (los excludes siguen aplicando).
Por tanto `targets = []string{"/"}` + `targetFS = zipFS` es suficiente: el snapshot queda
con los paths internos del zip tal cual (`/docs/readme.txt`, ...). Opción A y B convirgen;
no hace falta modificar `internal/archiver`.

### 3. Consistencia "fichero cambiado durante la lectura"

Flujo del archiver (`save()` → `fileSaver.saveFile`):
`OpenFile(name, O_NOFOLLOW, metadataOnly=true)` → `meta.Stat()` → (sin padre: sin
`fileChanged`) → `meta.MakeReadable()` → `meta.Stat()` otra vez → `meta.ToNode()`.
En `Zip` todas las llamadas derivan del mismo índice inmemoria del directorio central
(inmutable tras `NewZip`), así que `Lstat`/`Stat`/`ToNode` son coherentes por
construcción. No hay "cambio" posible: el `.zip` se abre una vez con `zip.OpenReader`.

### 4. `--parent` / detección de cambios

Con `--migrate` se fuerza `opts.Force = true` **antes** de `findParentSnapshot`, que con
`Force` devuelve `nil` sin consultar el repo → siempre "no parent snapshot", como pide el
§4.3. `--ignore-inode`/`--ignore-ctime` resultan irrelevantes (no hay nodos anteriores
comparables). La dedup por contenido de restic evita reescribir datos repetidos.

## Notas de implementación

- `zip.FileHeader.Mode()` (stdlib) ya decodifica los bits unix (permisos + tipo, incl.
  `ModeSymlink`) vía `unixModeToFileMode`; para creador MS-DOS devuelve 0666/0777|Dir.
  Según §4.4: modo solo si creador Unix (o macOS); si no, 0644 ficheros / 0755 dirs.
- Symlinks del zip: el target se lee al indexar (máx. 4096 B; si sobrepasa, se trata la
  entrada como fichero regular + warning). `ToNode` rellena `LinkTarget` (o
  `LinkTargetRaw` si no es UTF-8 válido).
- Nombres no-UTF-8: sin el flag UTF-8 (bit 11), el nombre se decodifica como CP437
  y se convierte a UTF-8. Con el flag, se usa tal cual. Nota: en
  `golang.org/x/text v0.43.0` ya no existe `charmap.CP437`, así que la tabla
  de 0x80–0xFF (`cp437High`, 128 runes) va embebida en `fs_zip.go` vía
  `cp437Decode`; verificada contra `python3 bytes.decode('cp437')` y
  glibc `iconv -f CP437` (coinciden; p. ej. 0xE9→Θ U+0398, 0x82→é).
  El reader de `archive/zip` (Go 1.26) ya deduce `FileHeader.NonUTF8` del
  flag 0x800; el writer solo setea el flag UTF-8 si el nombre es UTF-8
  válido y multibyte (un byte 0x82 solo no lo setea → `NonUTF8=true` al leer).
- Warnings: desviación menor del contrato §2 — `NewZip(zipPath string, warnf func(string,
  ...any))` en lugar de `NewZip(zipPath string)`, para emitir los warnings del §5 por el
  mismo canal que el resto del comando (`printer.E`). Sin `warnf` no hay forma de
  reportar entradas omitidas.
- Concurrency: cada `OpenFile` crea un `zipFile` nuevo; `MakeReadable` abre un
  `zip.File.Open()` independiente (lectura vía `io.ReaderAt` del `*os.File` subyacente),
  sin estado mutable compartido → seguro para `ReadConcurrency` > 1.

## Resumen por fase

### A1 — ZipFS núcleo (commit `d12bf91d3`)

- Qué hice: `internal/fs/fs_zip.go` (589 líneas) — `Zip` con índice inmemoria del
  directorio central (`entries` + `children` ordenados, raíz `/` implícita),
  `NewZip` (saneado §5: `\`→`/`, drive letter, `..` que escapa → omitir+warning,
  `\0` → inválido; duplicados última-gana; colisión fichero/directorio → omitir+warning;
  directorios implícitos con mtime = máximo de sus descendientes, procesando por
  profundidad decreciente), `zipFile` (lazy `fh.Open()`, streaming, CRC verificado al
  EOF), `zipDir`, `Close()` idempotente.
- Decisiones: modo Unix solo si `CreatorVersion>>8 == 3` (unix) o `== 19` (macOS,
  valor que usa la stdlib de Go 1.26) vía `fh.Mode()`; si no, 0644/0755. Symlink →
  `os.ModeSymlink|0o777`, target leído al indexar (tope 4096 B). `NewZip` normaliza
  `fh.Modified.UTC()` (el reader devuelve location `""`).
- Pendiente: nada.

### A2 — Tests unitarios ZipFS (commits `b50982c5f`, `db125ca28`)

- Qué hice: `internal/fs/fs_zip_test.go` — 14 tests `TestZip*` (jerarquía, saneado,
  duplicados, colisión, symlink, CP437, cifrado, compresión no soportada, CRC corrupto,
  Zip64, concurrencia con `-race`, no-escritura a disco, zip vacío, consistencia Lstat).
  Helpers de parcheo de binario `patchZipHeaderFlags` / `patchZipEntryMethod` /
  `patchZip64Central` (offsets locales: flags 6, method 8, nlen 26, name 30;
  central: flags 8, method 10, nlen 28, name 46, xlen 30, comment 32, offset 42).
- Decisiones: el test Zip64 marca los 3 campos de 32 bits a `0xFFFFFFFF` y mueve los
  valores reales al extra field `0x0001` — el reader de Go 1.26 exige los tres valores
  en el extra (solo perdona `uncompressed` "42.zip").
- Pendiente: nada.

### A3 — Integración CLI (commit `779cff54e`)

- Qué hice: `cmd/restic/cmd_backup.go` — campo `Migrate string` en `BackupOptions`,
  flag `--migrate`, validaciones en `Check()` (incompatible con args posicionales,
  `--stdin`, `--stdin-from-command`, `--files-from*`, `--parent`),
  `collectTargets` → `[]string{"/"}`, bloque migrate en `runBackup` (`Force=true`,
  `NewZip` con `printer.E` ANTES de `openWithAppendLock` → fail-fast sin tocar el repo,
  `defer Close()`), `targetFS = zipFS` tras el bloque VSS, `RejectByDevice` desactivado
  en modo migrate (el `DeviceID` del FS virtual es 0). CRC corrupto fluye por
  `arch.Error` → exit code 3.
- Decisiones: sin cambios en `internal/archiver` (Opción A del plan, target `/`).
- Pendiente: nada.

### A4 — Tests de integración (commit `d3003c754`)

- Qué hice: `cmd/restic/cmd_backup_migrate_integration_test.go` + helpers por
  plataforma (`cmd_backup_migrate_{unix,windows,memory_linux,memory_other}_test.go`) —
  happy path (ls == zip, restore byte a byte vs. árbol fuente, dedup con 2º backup
  comprobado por packs: solo pueden aparecer packs nuevos de tree/snapshot), opciones
  incompatibles, zip inexistente con repo sin init (prueba de fail-fast), `--exclude`,
  `--dry-run` (0 snapshots), y test de memoria con zip de 512 MiB (pico RSS acotado,
  `syscall.Getrusage` solo Linux, skip en `-short`).
- Decisiones: el fixture fija los mtimes a un instante con precisión de segundo
  (`os.Chtimes` + `UtimesNanoAt(..., AT_SYMLINK_NOFOLLOW)` para el symlink), porque el
  zip solo guarda segundos; el dedup se comprueba por IDs de packs (los snapshots son
  `TreeBlob`), no por tamaño de repo (muy pequeño en tests). `zip.Writer` no es
  thread-safe: el goroutine del test de memoria escribe y el main cierra.
- Pendiente: nada.

### A5 — Documentación y changelog (commit tras A4)

- Qué hice: sección "Backup from zip archive" en `doc/040_backup.rst` (uso, paths,
  limitaciones: sin padre/`--force`, modos según creador, precisión de segundo,
  symlinks, entradas omitidas) y `changelog/unreleased/pull-migrate-zip`.
- Decisiones: la URL del changelog apunta al fork (`Saiyine/restic`) porque no hay
  issue/PR upstream; se sustituirá por el link del PR al proponerlo. `restic backup
  --help` y `restic generate` (man + completions bash/zsh/fish) incluyen el flag
  automáticamente, sin cambios manuales.
- Pendiente: sustituir la URL del changelog cuando exista el PR.

### A6 — QA final

- `gofmt -l .`: solo `internal/backend/s3/s3_test.go`, preexistente en `master`
  (diff 0 contra `master`), no tocado.
- `go vet ./...` (los paquetes tocados) y `golangci-lint run` (v2.6.0): 0 issues
  tras corregir 6 hallazgos (commit de fixes: `Close()` en defer sin chequear error,
  `SetModTime` deprecado → campo `Modified`, helper sin usar).
- `go test -race ./internal/fs/... ./internal/archiver/... ./cmd/restic/...`: verde.
- Cross-compile: `GOOS=windows` y `GOOS=darwin` `go build ./...`: OK.
- Grep anti-extracción (`os.Create*|MkdirTemp|CreateTemp|WriteFile|ioutil.Temp`)
  sobre `fs_zip.go` y `cmd_backup.go`: nada.
- End-to-end manual con zip real (129 MiB, modos 700/644, symlink, mtimes) dentro de
  `unshare -U -r -m` con tmpfs en `/tmp`: `ls` coincide con `unzip -l`; restore
  idéntico byte a byte (modos y mtimes incluidos); en `/tmp` solo existieron repo,
  cache y target de restore → cero ficheros temporales.
- Pendiente: nada. Criterios del §7 todos verificados.

### A7 — `--migrate` repetible (varios zips)

- Qué hice: `--migrate` ahora se puede especificar varias veces
  (`Migrate []string`, flag `StringArrayVar`). `fs.NewZip` delega en el nuevo
  `fs.NewZipMulti(zipPaths, warnf)`, que abre todos los archivos, concatena sus
  entradas en orden y las indexa con la MISMA lógica que ya existía (`addEntry`):
  duplicado mismo tipo → gana el último (zip posterior) + warning; colisión
  fichero/directorio → gana la primera + warning. `Zip.rzs []*zip.ReadCloser`
  (antes `rc`); `Close()` cierra todos. CLI: validación de elementos vacíos y
  de duplicados en `Check()`.
- Decisiones: semántica de "concatenación de entradas" (no hay reglas nuevas,
  reutiliza el comportamiento de duplicados/colisiones de un solo zip); sin
  env var (igual que `--exclude-if-present`, `StringArrayVar` no la soporta).
- Tests: 4 unitarios nuevos (`TestZipMulti{Disjoint,DuplicateLastWins,TypeConflict,OpenError}`)
  e integración `TestBackupMigrateMultipleArchives` (unión + último-gana,
  restore comparado con el árbol esperado fusionado).
- Docs/changelog actualizados; pendientes: QA completo y PR.
